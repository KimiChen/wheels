//! 改额度立即生效的验收用例（D21、README §8）。

use proxy_manager::quota::allocate::AllocationConfig;
use proxy_manager::quota::converge::{self, ConvergeOutcome, NodeContext};
use proxy_manager::quota::settings::{self, TaskStatus};
use proxy_manager::quota::wire::Quota;
use proxy_manager::store::codec::U128Text;

use super::quota_harness::{Stack, NODE, RUNTIME};

const GIB: u64 = 1 << 30;
const CYCLE: &str = "2026-09";

/// 改 normal 档的字节数。这些用例测的是预览/确认/审计/任务生成这套通用机制，
/// 用哪一档不影响结论，所以统一用 normal。
fn group(new_monthly_bytes: u64) -> settings::Scope {
    settings::Scope::Group { group_name: "normal".into(), new_monthly_bytes }
}

/// 读 normal 档当前的字节数。断言改档结果要看**档位表**，
/// 不再看 quota_settings——那里已经没有额度这个数了。
/// 从 user_id 查登录名。`map_users()` 的键是**身份名**，不是登录名——
/// 换档接口按登录名定位用户，两者混用会得到「没有这个在用用户」。
async fn login_of(stack: &Stack, user_id: i64) -> String {
    use sqlx::Row;
    sqlx::query("SELECT login_name FROM users WHERE user_id = ?")
        .bind(user_id)
        .fetch_one(stack.store.readers())
        .await
        .unwrap()
        .get(0)
}

async fn normal_bytes(stack: &Stack) -> u64 {
    settings::load_groups(&stack.store)
        .await
        .unwrap()
        .into_iter()
        .find(|g| g.group_name == "normal")
        .expect("normal 档由建库种下")
        .monthly_bytes
}

/// 给某个用户塞一笔本周期已结算用量。
///
/// `rule_version` 绑常量而不是写字面量 1：读者一律按
/// `rule_version = CYCLE_RULE_VERSION` 过滤，写死版本号会让这个测试在版本一变时
/// 静默地不再测它自以为在测的东西——播进去的用量对被测代码不可见，
/// 于是「已超额的人应当被挡」变成「没有人超额」，断言照样有可能过。
async fn seed_usage(stack: &Stack, user_id: i64, bytes: u128) {
    let mut txn = stack.store.begin_immediate().await.unwrap();
    sqlx::query(
        "INSERT INTO usage_cycle_totals(user_id, cycle_key, rule_version, total_bytes, updated_at) \
         VALUES (?, ?, ?, ?, ?) \
         ON CONFLICT(user_id, cycle_key, rule_version) DO UPDATE SET total_bytes = excluded.total_bytes",
    )
    .bind(user_id)
    .bind(CYCLE)
    .bind(proxy_manager::ledger::settle::CYCLE_RULE_VERSION)
    .bind(U128Text::new(bytes).encode())
    .bind("2026-09-18T00:00:00Z")
    .execute(txn.conn())
    .await
    .unwrap();
    txn.commit().await.unwrap();
}

async fn context(stack: &Stack) -> NodeContext {
    NodeContext {
        node_id: NODE.into(),
        runtime_id: RUNTIME.into(),
        weight: 1024,
        fresh_sequence: Some(stack.settle_once().await),
    }
}

// ============ §8 的头号算例 ============

/// **改额度本周期立即生效**（D21）。
///
/// 计划书给的算例：已用 320 GiB 的用户，额度 300 → 500 后
/// 下一次 `remaining_bytes` 是 180 GiB **而不是 0**；额度 300 → 100 后为 0。
/// 两个方向都是期望行为——调高本来就该让他继续用。
#[tokio::test]
async fn 已用三百二十gib的用户在调高后拿到一百八十gib() {
    let stack = Stack::new().await;
    let users = stack.map_users().await;
    let heavy = *users.values().next().unwrap();
    seed_usage(&stack, heavy, 320 * GIB as u128).await;

    let config = AllocationConfig::default();

    // 额度 300 GiB：已用尽 → 零额度。
    settings::apply(&stack.store, &group(300 * GIB), "kimi", CYCLE, true).await.unwrap();
    let ctx = context(&stack).await;
    let plan = converge::plan_round(&stack.store, std::slice::from_ref(&ctx), &config, CYCLE)
        .await
        .unwrap();
    assert_eq!(quota_of(&plan, heavy, &stack).await, Quota::Limited(0), "已用尽");

    // 300 → 500：从「已用尽」变成「剩余 180 GiB」。
    settings::apply(&stack.store, &group(500 * GIB), "kimi", CYCLE, false).await.unwrap();
    let ctx = context(&stack).await;
    let plan = converge::plan_round(&stack.store, std::slice::from_ref(&ctx), &config, CYCLE)
        .await
        .unwrap();
    assert_eq!(
        quota_of(&plan, heavy, &stack).await,
        Quota::Limited(180 * GIB),
        "**本来就该让他继续用**——这正是调高额度的目的"
    );

    // 500 → 100：仍为已用尽，下发零额度。
    settings::apply(&stack.store, &group(100 * GIB), "kimi", CYCLE, true).await.unwrap();
    let ctx = context(&stack).await;
    let plan = converge::plan_round(&stack.store, std::slice::from_ref(&ctx), &config, CYCLE)
        .await
        .unwrap();
    assert_eq!(quota_of(&plan, heavy, &stack).await, Quota::Limited(0));
}

/// 参数变更本身**不造账**。
#[tokio::test]
async fn 改额度不写用量账本() {
    let stack = Stack::new().await;
    stack.map_users().await;
    let before: i64 = scalar(&stack, "SELECT count(*) FROM usage_ledger").await;
    let before_totals: i64 = scalar(&stack, "SELECT count(*) FROM usage_cycle_totals").await;

    settings::apply(&stack.store, &group(300 * GIB), "kimi", CYCLE, true).await.unwrap();
    settings::apply(&stack.store, &group(100 * GIB), "kimi", CYCLE, true).await.unwrap();

    assert_eq!(scalar(&stack, "SELECT count(*) FROM usage_ledger").await, before);
    assert_eq!(
        scalar(&stack, "SELECT count(*) FROM usage_cycle_totals").await,
        before_totals,
        "改参数不该产生任何用量记录"
    );
}

// ============ 调低先预览 ============

/// **调低必须先预览再确认**；调高不要求。
#[tokio::test]
async fn 调低必须先确认而调高不要求() {
    let stack = Stack::new().await;
    let users = stack.map_users().await;
    let heavy = *users.values().next().unwrap();
    seed_usage(&stack, heavy, 200 * GIB as u128).await;
    settings::apply(&stack.store, &group(300 * GIB), "kimi", CYCLE, true).await.unwrap();

    // 调低且未确认 → 拒绝。
    let error =
        settings::apply(&stack.store, &group(100 * GIB), "kimi", CYCLE, false).await.unwrap_err();
    assert!(error.to_string().contains("先预览再确认"), "实际：{error}");
    assert_eq!(normal_bytes(&stack).await, 300 * GIB);

    // 调高不要求确认。
    settings::apply(&stack.store, &group(400 * GIB), "kimi", CYCLE, false).await.unwrap();
    assert_eq!(normal_bytes(&stack).await, 400 * GIB);
}

/// 相同已结算用量下，预览名单与实际提交结果一致，且预览带**用量核验时刻**。
#[tokio::test]
async fn 预览名单与实际提交一致且带核验时刻() {
    let stack = Stack::new().await;
    let users = stack.map_users().await;
    let ids: Vec<i64> = users.values().copied().collect();
    seed_usage(&stack, ids[0], 200 * GIB as u128).await;
    seed_usage(&stack, ids[1], 50 * GIB as u128).await;
    settings::apply(&stack.store, &group(300 * GIB), "kimi", CYCLE, true).await.unwrap();

    let preview = settings::preview(&stack.store, &group(100 * GIB), CYCLE).await.unwrap();
    assert!(preview.is_reduction);
    assert!(!preview.usage_verified_at.is_empty(), "必须带用量核验时刻");
    // 只有用了 200 GiB 的那个会被闸断。
    assert_eq!(preview.newly_blocked.len(), 1);
    assert_eq!(preview.newly_blocked[0].user_id, ids[0]);
    assert_eq!(preview.affected_users(), 1);

    // 预览**不改设置、不写下发任务**。
    assert_eq!(normal_bytes(&stack).await, 300 * GIB);
    let tasks_before = settings::latest_tasks(&stack.store).await.unwrap();

    let applied =
        settings::apply(&stack.store, &group(100 * GIB), "kimi", CYCLE, true).await.unwrap();
    assert_eq!(
        applied.affected_users,
        preview.affected_users(),
        "相同已结算用量下，预览与实际提交必须一致"
    );
    let tasks_after = settings::latest_tasks(&stack.store).await.unwrap();
    assert_ne!(tasks_before, tasks_after, "提交才写任务");
}

/// 调高的收益体现在 `newly_unblocked`。
#[tokio::test]
async fn 调高把已用尽的人解出来() {
    let stack = Stack::new().await;
    let users = stack.map_users().await;
    let heavy = *users.values().next().unwrap();
    seed_usage(&stack, heavy, 320 * GIB as u128).await;
    settings::apply(&stack.store, &group(300 * GIB), "kimi", CYCLE, true).await.unwrap();

    let preview = settings::preview(&stack.store, &group(500 * GIB), CYCLE).await.unwrap();
    assert!(!preview.is_reduction);
    assert_eq!(preview.newly_unblocked.len(), 1);
    assert_eq!(preview.newly_blocked.len(), 0);
    assert_eq!(preview.affected_users(), 1);
}

// ============ 审计 ============

/// 一条记录含**操作者、时间、旧值、新值与受影响人数**。
#[tokio::test]
async fn 改额度落审计() {
    let stack = Stack::new().await;
    let users = stack.map_users().await;
    seed_usage(&stack, *users.values().next().unwrap(), 320 * GIB as u128).await;

    settings::apply(&stack.store, &group(300 * GIB), "kimi", CYCLE, true).await.unwrap();
    settings::apply(&stack.store, &group(500 * GIB), "someone-else", CYCLE, false).await.unwrap();

    let log = settings::audit_log(&stack.store).await.unwrap();
    assert_eq!(log.len(), 2);
    assert_eq!(log[1].actor, "someone-else");
    assert_eq!(log[1].old_monthly_bytes, 300 * GIB);
    assert_eq!(log[1].new_monthly_bytes, 500 * GIB);
    assert_eq!(log[1].affected_users, 1, "那个已用 320 GiB 的人被解出来了");
    assert!(!log[1].created_at.is_empty());
    assert_eq!(log[1].revision, 2);
}

#[tokio::test]
async fn 没有操作者就不许改() {
    let stack = Stack::new().await;
    let error =
        settings::apply(&stack.store, &group(300 * GIB), "  ", CYCLE, true).await.unwrap_err();
    assert!(error.to_string().contains("操作者"), "实际：{error}");
}

// ============ 逐节点任务与「待生效」 ============

/// 设置、revision、审计与待下发任务**在同一写事务提交**。
#[tokio::test]
async fn 设置与任务同事务提交() {
    let stack = Stack::new().await;
    let applied =
        settings::apply(&stack.store, &group(300 * GIB), "kimi", CYCLE, true).await.unwrap();
    assert_eq!(applied.revision, 1);
    assert_eq!(applied.nodes, vec![NODE.to_string()]);

    let tasks = settings::latest_tasks(&stack.store).await.unwrap();
    assert_eq!(tasks.len(), 1);
    assert_eq!(tasks[0].status, TaskStatus::Pending);
    assert_eq!(tasks[0].revision, 1);
    // 审计与设置也在同一事务里。
    assert_eq!(settings::audit_log(&stack.store).await.unwrap().len(), 1);
}

/// **只要还有节点没应用新配置，就不能显示「全部已生效」。**
///
/// `safe_zero` / `unknown` / `failed` 都不算。
#[tokio::test]
async fn 只有applied才算新配置已生效() {
    let stack = Stack::new().await;
    settings::apply(&stack.store, &group(300 * GIB), "kimi", CYCLE, true).await.unwrap();
    assert!(!settings::fully_applied(&stack.store).await.unwrap(), "pending 不算");

    for status in [TaskStatus::SafeZero, TaskStatus::Unknown, TaskStatus::Failed] {
        settings::set_task_status(&stack.store, 1, NODE, status, None).await.unwrap();
        assert!(
            !settings::fully_applied(&stack.store).await.unwrap(),
            "{status:?} 不能冒充新配置已生效"
        );
    }

    settings::set_task_status(&stack.store, 1, NODE, TaskStatus::Applied, None).await.unwrap();
    assert!(settings::fully_applied(&stack.store).await.unwrap());
}

// ============ 立即采集再下发 ============

/// 刚完成正额度下发就改设置：最近 sequence 已占用，**必须取到并结算新 sequence**。
#[tokio::test]
async fn 改设置后不得复用已占用的sequence() {
    let stack = Stack::new().await;
    let users = stack.map_users().await;
    seed_usage(&stack, *users.values().next().unwrap(), 10 * GIB as u128).await;
    settings::apply(&stack.store, &group(300 * GIB), "kimi", CYCLE, true).await.unwrap();

    let config = AllocationConfig::default();
    let ctx = context(&stack).await;
    let used_sequence = ctx.fresh_sequence.unwrap();
    let plan = converge::plan_round(&stack.store, std::slice::from_ref(&ctx), &config, CYCLE)
        .await
        .unwrap();
    let outcome = converge::converge_node(&stack.store, &stack.client, &ctx, &plan).await.unwrap();
    assert!(matches!(outcome, ConvergeOutcome::Applied { .. }), "实际：{outcome:?}");

    // 改设置之后拿同一个 sequence 再来一次：必须被挡住。
    settings::apply(&stack.store, &group(400 * GIB), "kimi", CYCLE, false).await.unwrap();
    let stale_ctx = NodeContext { fresh_sequence: Some(used_sequence), ..ctx.clone() };
    let plan = converge::plan_round(&stack.store, std::slice::from_ref(&stale_ctx), &config, CYCLE)
        .await
        .unwrap();
    let error =
        converge::converge_node(&stack.store, &stack.client, &stale_ctx, &plan).await.unwrap_err();
    assert!(error.to_string().contains("sequence"), "实际：{error}");

    // 主动取到并结算一个新 sequence 就可以了。
    let fresh_ctx = context(&stack).await;
    assert_ne!(fresh_ctx.fresh_sequence, Some(used_sequence));
    let plan = converge::plan_round(&stack.store, std::slice::from_ref(&fresh_ctx), &config, CYCLE)
        .await
        .unwrap();
    let outcome =
        converge::converge_node(&stack.store, &stack.client, &fresh_ctx, &plan).await.unwrap();
    assert!(matches!(outcome, ConvergeOutcome::Applied { .. }), "实际：{outcome:?}");
}

/// **发送前再次检查 revision**：过时就重算，不得把旧配置的余额推回节点。
#[tokio::test]
async fn 规划与发送之间改了设置就判过时() {
    let stack = Stack::new().await;
    stack.map_users().await;
    settings::apply(&stack.store, &group(300 * GIB), "kimi", CYCLE, true).await.unwrap();

    let config = AllocationConfig::default();
    let ctx = context(&stack).await;
    let plan = converge::plan_round(&stack.store, std::slice::from_ref(&ctx), &config, CYCLE)
        .await
        .unwrap();

    // 规划完、发送前，有人又改了一次。
    settings::apply(&stack.store, &group(400 * GIB), "kimi", CYCLE, false).await.unwrap();

    let outcome = converge::converge_node(&stack.store, &stack.client, &ctx, &plan).await.unwrap();
    match outcome {
        ConvergeOutcome::Stale { planned, current } => {
            assert_eq!((planned, current), (1, 2));
        }
        other => panic!("应当判过时，实际：{other:?}"),
    }
    // 过时的任务留在 pending 等重算。
    assert_eq!(
        converge::ConvergeOutcome::Stale { planned: 1, current: 2 }.task_status(),
        TaskStatus::Pending
    );
    // **节点上什么都没变**——旧配置的余额没有被推出去。
    assert!(!stack.node.quota_accepted().await);
}

// ============ 失败状态 ============

/// health 不可入账时**无正额度刷新**；通道可用则走 C33 的全零完整表。
#[tokio::test]
async fn 快照不健康时只做零表收敛() {
    let stack = Stack::new().await;
    stack.map_users().await;
    settings::apply(&stack.store, &group(300 * GIB), "kimi", CYCLE, true).await.unwrap();
    stack.node.set_health_bit("counter_overflow", true).await;

    // 拿不到新鲜的已结算 sequence。
    let ctx = NodeContext {
        node_id: NODE.into(),
        runtime_id: RUNTIME.into(),
        weight: 1024,
        fresh_sequence: None,
    };
    let config = AllocationConfig::default();
    let plan = converge::plan_round(&stack.store, std::slice::from_ref(&ctx), &config, CYCLE)
        .await
        .unwrap();
    // 计划出来的表必然全零——没有可更新的节点。
    let entries = &plan.tables[NODE];
    assert!(entries.iter().all(|(_, _, q)| *q == Quota::Limited(0)));

    let outcome = converge::converge_node(&stack.store, &stack.client, &ctx, &plan).await.unwrap();
    match outcome {
        // 零表确认成功记 safe_zero，**不冒充新配置生效**。
        ConvergeOutcome::SafeZero { .. } => {}
        other => panic!("应当走零表收敛，实际：{other:?}"),
    }
    converge::record_task(
        &stack.store,
        plan.revision,
        NODE,
        &ConvergeOutcome::SafeZero { request_id: 0 },
    )
    .await
    .unwrap();
    assert!(!settings::fully_applied(&stack.store).await.unwrap(), "safe_zero 不算已生效");

    // 生效表上是全零**完整表**，不是空表。
    let effective = stack.effective().await;
    assert_eq!(effective.len(), 3);
    assert!(effective.values().all(|q| *q == Quota::Limited(0)));
}

/// C33 的肯定动作单独走一遍：**主动**下发全零完整表。
#[tokio::test]
async fn 主动零表收敛拿到200() {
    let stack = Stack::new().await;
    let outcome =
        converge::converge_safe_zero(&stack.store, &stack.client, NODE, RUNTIME).await.unwrap();
    assert!(matches!(outcome, ConvergeOutcome::SafeZero { .. }), "实际：{outcome:?}");
    let effective = stack.effective().await;
    assert_eq!(effective.len(), 3, "全零**完整**表，不是空表");
    assert!(effective.values().all(|q| *q == Quota::Limited(0)));
}

/// 连续修改 revision 时，旧任务**不能恢复旧额度**。
#[tokio::test]
async fn 连续修改时只按最新revision收敛() {
    let stack = Stack::new().await;
    stack.map_users().await;
    settings::apply(&stack.store, &group(300 * GIB), "kimi", CYCLE, true).await.unwrap();
    settings::apply(&stack.store, &group(500 * GIB), "kimi", CYCLE, false).await.unwrap();
    settings::apply(&stack.store, &group(700 * GIB), "kimi", CYCLE, false).await.unwrap();

    let tasks = settings::latest_tasks(&stack.store).await.unwrap();
    assert_eq!(tasks.len(), 1);
    assert_eq!(tasks[0].revision, 3, "收敛循环只看最新 revision");

    let config = AllocationConfig::default();
    let ctx = context(&stack).await;
    let plan = converge::plan_round(&stack.store, std::slice::from_ref(&ctx), &config, CYCLE)
        .await
        .unwrap();
    assert_eq!(plan.revision, 3);
    let outcome = converge::converge_node(&stack.store, &stack.client, &ctx, &plan).await.unwrap();
    assert!(matches!(outcome, ConvergeOutcome::Applied { .. }), "实际：{outcome:?}");
}

// ---- 辅助 ----

async fn quota_of(plan: &converge::RoundPlan, user_id: i64, stack: &Stack) -> Quota {
    let identities = stack.identities().await;
    let identity =
        identities.iter().find(|i| i.user_id == Some(user_id)).expect("该用户应当有身份");
    let table = &plan.tables[NODE];
    table
        .iter()
        .find(|(tag, name, _)| *tag == identity.inbound_tag && *name == identity.name)
        .map(|(_, _, q)| *q)
        .expect("表里应当有这个身份")
}

async fn scalar(stack: &Stack, sql: &str) -> i64 {
    use sqlx::Row;
    sqlx::query(sql).fetch_one(stack.store.readers()).await.unwrap().get(0)
}

// ============ 四档额度（定案第三条） ============

/// 十进制 TB，不是 2^40。这个错不会报错，只会让每个人多拿约 10%。
#[tokio::test]
async fn 档位初值是十进制tb() {
    let stack = Stack::new().await;
    let groups = settings::load_groups(&stack.store).await.unwrap();
    let by_name: std::collections::BTreeMap<_, _> =
        groups.iter().map(|g| (g.group_name.as_str(), g.monthly_bytes)).collect();

    assert_eq!(by_name["normal"], 1_000_000_000_000);
    assert_eq!(by_name["advanced"], 2_000_000_000_000);
    assert_eq!(by_name["manage"], 5_000_000_000_000);
    assert_eq!(by_name["admin"], 10_000_000_000_000);
    // 反向：如果有人按二进制理解，1 TB 会是这个数。
    assert_ne!(by_name["normal"], 1_u64 << 40);
}

/// 预算按**用户自己的档位**取，而不是某个共同的值。
///
/// 两个用户用量相同、档位不同，拿到的剩余必须不同。用一个共同额度去算所有人，
/// 在分档之后是错的，而且错得不显眼——每个数字单看都合理。
#[tokio::test]
async fn 预算按用户档位而不是共同值() {
    let stack = Stack::new().await;
    let users = stack.map_users().await;
    let mut ids: Vec<i64> = users.values().copied().collect();
    ids.sort();
    let (low, high) = (ids[0], ids[1]);

    // 同样用掉 100 GiB。
    let used = 100 * GIB as u128;
    seed_usage(&stack, low, used).await;
    seed_usage(&stack, high, used).await;

    // 一个留在 normal（1 TB），一个换到 manage（5 TB）。
    let login_high = login_of(&stack, high).await;
    settings::apply(
        &stack.store,
        &settings::Scope::UserGroup { login_name: login_high, new_group: "manage".into() },
        "kimi",
        CYCLE,
        false,
    )
    .await
    .unwrap();

    let ctx = context(&stack).await;
    let plan = converge::plan_round(
        &stack.store,
        std::slice::from_ref(&ctx),
        &AllocationConfig::default(),
        CYCLE,
    )
    .await
    .unwrap();

    assert_eq!(
        quota_of(&plan, low, &stack).await,
        Quota::Limited(1_000_000_000_000 - used as u64),
        "normal 档按 1 TB 算剩余"
    );
    assert_eq!(
        quota_of(&plan, high, &stack).await,
        Quota::Limited(5_000_000_000_000 - used as u64),
        "manage 档按 5 TB 算剩余"
    );
}

/// 被禁用的用户拿到**零额度**，而不是从表里被省略。
///
/// 省略在 wire 上等于无限额度（C16/C30）：节点会把不在表里的身份当成不受限。
/// 这条失败时 PUT 照样返回 200，表也照样是「全量」的——除了少一行。
#[tokio::test]
async fn 禁用的用户拿零额度而不是被省略() {
    let stack = Stack::new().await;
    let users = stack.map_users().await;
    let victim = *users.values().next().unwrap();

    let before = {
        let ctx = context(&stack).await;
        converge::plan_round(
            &stack.store,
            std::slice::from_ref(&ctx),
            &AllocationConfig::default(),
            CYCLE,
        )
        .await
        .unwrap()
        .tables[NODE]
            .len()
    };

    let mut txn = stack.store.begin_immediate().await.unwrap();
    sqlx::query("UPDATE users SET status = 'disabled' WHERE user_id = ?")
        .bind(victim)
        .execute(txn.conn())
        .await
        .unwrap();
    txn.commit().await.unwrap();

    let ctx = context(&stack).await;
    let plan = converge::plan_round(
        &stack.store,
        std::slice::from_ref(&ctx),
        &AllocationConfig::default(),
        CYCLE,
    )
    .await
    .unwrap();

    assert_eq!(quota_of(&plan, victim, &stack).await, Quota::Limited(0), "禁用即零额度");
    assert_eq!(plan.tables[NODE].len(), before, "**条目数不变**：禁用不等于从表里拿掉");
}

/// 换到更低的档位也是降额，同样必须先预览再确认。
#[tokio::test]
async fn 把用户换到更低档位也要确认() {
    let stack = Stack::new().await;
    let users = stack.map_users().await;
    let user_id = *users.values().next().unwrap();
    let login = login_of(&stack, user_id).await;
    // 先换到 manage（5 TB），再往回换到 normal（1 TB）——后者是降额。
    let up = settings::Scope::UserGroup { login_name: login.clone(), new_group: "manage".into() };
    settings::apply(&stack.store, &up, "kimi", CYCLE, false).await.unwrap();
    seed_usage(&stack, user_id, 2_000_000_000_000_u128).await;

    let down = settings::Scope::UserGroup { login_name: login, new_group: "normal".into() };
    let error = settings::apply(&stack.store, &down, "kimi", CYCLE, false).await.unwrap_err();
    assert!(error.to_string().contains("先预览再确认"), "实际：{error}");

    settings::apply(&stack.store, &down, "kimi", CYCLE, true).await.unwrap();
}

/// 审计要能区分「改了哪一档」与「换了谁的档」。
#[tokio::test]
async fn 审计带scope与subject() {
    let stack = Stack::new().await;
    let users = stack.map_users().await;
    let login = login_of(&stack, *users.values().next().unwrap()).await;

    settings::apply(&stack.store, &group(3_000_000_000_000), "kimi", CYCLE, false).await.unwrap();
    settings::apply(
        &stack.store,
        &settings::Scope::UserGroup { login_name: login.clone(), new_group: "manage".into() },
        "kimi",
        CYCLE,
        false,
    )
    .await
    .unwrap();

    let rows = sqlx::query("SELECT scope, subject FROM quota_setting_audits ORDER BY audit_id")
        .fetch_all(stack.store.readers())
        .await
        .unwrap();
    use sqlx::Row;
    assert_eq!(rows.len(), 2);
    assert_eq!(rows[0].get::<String, _>(0), "group");
    assert_eq!(rows[0].get::<String, _>(1), "normal");
    assert_eq!(rows[1].get::<String, _>(0), "user_group");
    assert_eq!(rows[1].get::<String, _>(1), login);
}
