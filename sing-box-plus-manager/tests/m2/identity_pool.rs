//! 计费槽位的认领、退役与跨重启归属（定案第一条、D11、D14）。
//!
//! 这一组盯的都是**无声**的失败：归属丢了不会报错，退役槽位拿到额度不会报错，
//! 一个身份被省略出额度表更不会报错——PUT 照样返回 200。

use proxy_manager::identity;
use proxy_manager::quota::table;
use proxy_manager::quota::wire::Quota;
use sqlx::Row;

use super::quota_harness::{Stack, NODE, RUNTIME};

const ZERO: &str = "00000000000000000000";

async fn slot_state(stack: &Stack, identity_name: &str) -> (String, Option<i64>) {
    let row = sqlx::query(
        "SELECT state, user_id FROM identity_routes WHERE node_id = ? AND identity_name = ?",
    )
    .bind(NODE)
    .bind(identity_name)
    .fetch_one(stack.store.readers())
    .await
    .unwrap();
    (row.get(0), row.get(1))
}

async fn make_user(stack: &Stack, login: &str) -> i64 {
    let mut txn = stack.store.begin_immediate().await.unwrap();
    let row = sqlx::query(
        "INSERT INTO users(login_name, display_name, role, quota_group, status, created_at, updated_at) \
         VALUES (?, ?, 'user', 'normal', 'active', ?, ?) RETURNING user_id",
    )
    .bind(login)
    .bind(login)
    .bind("2026-09-18T00:00:00Z")
    .bind("2026-09-18T00:00:00Z")
    .fetch_one(txn.conn())
    .await
    .unwrap();
    let id: i64 = row.get(0);
    txn.commit().await.unwrap();
    id
}

// ============ 结算发现槽位，但不移动状态 ============

#[tokio::test]
async fn 结算把观察到的身份登记为空闲槽位() {
    let stack = Stack::new().await;
    stack.settle_once().await;

    let identities = stack.identities().await;
    assert!(!identities.is_empty());
    for identity in &identities {
        let (state, user_id) = slot_state(&stack, &identity.name).await;
        assert_eq!(state, "free", "{} 应当被登记为 free", identity.name);
        assert_eq!(user_id, None);
    }
}

/// 结算只**发现**，绝不移动状态。
///
/// 这条如果坏了（比如把 `DO NOTHING` 写成 `DO UPDATE`），
/// 一次普通采集就会把已认领的槽位打回 free，用户随即在下一轮额度表里掉到零，
/// 而整条链路没有任何一处报错。
#[tokio::test]
async fn 再次结算不会把已认领的槽位打回空闲() {
    let stack = Stack::new().await;
    let users = stack.map_users().await;
    let (name, user_id) = users.iter().next().map(|(k, v)| (k.clone(), *v)).unwrap();
    assert_eq!(slot_state(&stack, &name).await, ("claimed".into(), Some(user_id)));

    stack.settle_once().await;
    stack.settle_once().await;

    assert_eq!(
        slot_state(&stack, &name).await,
        ("claimed".into(), Some(user_id)),
        "采集不该改变槽位状态"
    );
}

// ============ 跨 runtime 的归属（本组的头号用例） ============

/// **换 runtime 后归属不丢。**
///
/// 归属曾经写在 `runtime_identities` 上，而那张表按 `(node_id, runtime_id)` 键：
/// 节点一重启就生成全新的行，归属静默清空，`bump_cycle` 不再被调用，
/// 所有身份在下一轮推送里掉到零额度。推送失败很响，账目失败完全无声。
///
/// 这条用例的做法是直接造一个新 runtime 并批准，然后确认同名身份仍然归同一个人。
#[tokio::test]
async fn 换runtime之后归属仍然在() {
    let stack = Stack::new().await;
    let users = stack.map_users().await;
    let (name, user_id) = users.iter().next().map(|(k, v)| (k.clone(), *v)).unwrap();

    // 造一个新 runtime：新的 runtime_id、更晚的启动时间，身份集合照旧。
    const NEW_RUNTIME: &str = "ffffffffffffffff0000000000000001";
    {
        let mut txn = stack.store.begin_immediate().await.unwrap();
        let runtime_pk: i64 = sqlx::query(
            "INSERT INTO node_runtimes(node_id, runtime_id, started_at_unix_ms, first_snapshot, \
             approval_status, approved_by, approved_at, window_state, first_seen_at, last_seen_at) \
             VALUES (?, ?, ?, 'baseline', 'approved', 'test', ?, 'open', ?, ?) RETURNING runtime_pk",
        )
        .bind(NODE)
        .bind(NEW_RUNTIME)
        .bind(format!("{:020}", 1_787_587_300_000_u64))
        .bind("2026-09-18T01:00:00Z")
        .bind("2026-09-18T01:00:00Z")
        .bind("2026-09-18T01:00:00Z")
        .fetch_one(txn.conn())
        .await
        .unwrap()
        .get(0);

        // 把旧 runtime 的身份集合原样搬到新 runtime 下（重启后节点上报的就是这些）。
        let olds = sqlx::query(
            "SELECT s.inbound_tag, s.inbound_type, i.identity_name, i.generation \
               FROM runtime_identities i \
               JOIN runtime_services s ON s.runtime_service_id = i.runtime_service_id \
               JOIN node_runtimes n ON n.runtime_pk = i.runtime_pk \
              WHERE n.runtime_id = ?",
        )
        .bind(RUNTIME)
        .fetch_all(txn.conn())
        .await
        .unwrap();
        let mut services = std::collections::BTreeMap::new();
        for row in &olds {
            let tag: String = row.get(0);
            let service_id = match services.get(&tag) {
                Some(id) => *id,
                None => {
                    let id: i64 = sqlx::query(
                        "INSERT INTO runtime_services(runtime_pk, inbound_tag, generation, \
                         inbound_type, active, first_seen_at) VALUES (?, ?, ?, ?, 1, ?) \
                         RETURNING runtime_service_id",
                    )
                    .bind(runtime_pk)
                    .bind(&tag)
                    .bind(format!("{:020}", 1_u64))
                    .bind(row.get::<String, _>(1))
                    .bind("2026-09-18T01:00:00Z")
                    .fetch_one(txn.conn())
                    .await
                    .unwrap()
                    .get(0);
                    services.insert(tag.clone(), id);
                    id
                }
            };
            sqlx::query(
                "INSERT INTO runtime_identities(runtime_pk, runtime_service_id, identity_name, \
                 generation, active, first_seen_at, last_seen_at) VALUES (?, ?, ?, ?, 1, ?, ?)",
            )
            .bind(runtime_pk)
            .bind(service_id)
            .bind(row.get::<String, _>(2))
            .bind(row.get::<String, _>(3))
            .bind("2026-09-18T01:00:00Z")
            .bind("2026-09-18T01:00:00Z")
            .execute(txn.conn())
            .await
            .unwrap();
        }
        txn.commit().await.unwrap();
    }

    // 新 runtime 下读出来的身份，归属必须还在。
    let identities = table::current_identities(&stack.store, NODE, NEW_RUNTIME).await.unwrap();
    let moved = identities.iter().find(|i| i.name == name).expect("新 runtime 下应当有同名身份");
    assert_eq!(
        moved.user_id,
        Some(user_id),
        "**换 runtime 后归属不能丢**——它落在槽位上，不落在按 runtime 键的行上"
    );
}

// ============ 认领 ============

#[tokio::test]
async fn 认领是幂等的且不重复消耗名额() {
    let stack = Stack::new().await;
    stack.settle_once().await;
    let user = make_user(&stack, "alice").await;

    let first = identity::claim_for_user(&stack.store, user, "kimi").await.unwrap();
    assert!(first.newly_claimed);
    let second = identity::claim_for_user(&stack.store, user, "kimi").await.unwrap();
    assert!(!second.newly_claimed, "重复认领不该再消耗一个名额");
    assert_eq!(first.identity_name, second.identity_name);
}

/// 计数非零的槽位不可认领。
///
/// 挡的是「把一个用过的身份发给新用户，于是他一上来就欠着别人的账」。
#[tokio::test]
async fn 计数非零的槽位不可认领() {
    let stack = Stack::new().await;
    stack.settle_once().await;
    let identities = stack.identities().await;

    // 把**全部**身份的游标都弄成非零，于是没有任何一个可认领。
    let mut txn = stack.store.begin_immediate().await.unwrap();
    for identity in &identities {
        sqlx::query(
            "INSERT INTO counter_cursors(runtime_identity_id, tcp_uplink_bytes, \
             tcp_downlink_bytes, udp_uplink_bytes, udp_downlink_bytes, updated_at) \
             VALUES (?, ?, ?, ?, ?, ?) \
             ON CONFLICT(runtime_identity_id) DO UPDATE SET tcp_uplink_bytes = excluded.tcp_uplink_bytes",
        )
        .bind(identity.runtime_identity_id)
        .bind(format!("{:020}", 1_u64))
        .bind(ZERO)
        .bind(ZERO)
        .bind(ZERO)
        .bind("2026-09-18T00:00:00Z")
        .execute(txn.conn())
        .await
        .unwrap();
    }
    txn.commit().await.unwrap();

    let user = make_user(&stack, "bob").await;
    let error = identity::claim_for_user(&stack.store, user, "kimi").await.unwrap_err();
    assert!(error.to_string().contains("身份池已耗尽"), "实际：{error}");
}

/// 池耗尽要有**明确错误**，绝不静默复用。
/// 一直认领到池子见底，返回成功认领的名字。
///
/// 不按「身份总数」循环：夹具里有些身份的计数本来就非零，按零基线门禁
/// 它们从一开始就不可认领。可认领数是**观察出来的**，不是算出来的。
async fn drain_pool(stack: &Stack, prefix: &str) -> Vec<String> {
    let mut claimed = Vec::new();
    for index in 0.. {
        let user = make_user(stack, &format!("{prefix}{index}")).await;
        match identity::claim_for_user(&stack.store, user, "kimi").await {
            Ok(claim) => claimed.push(claim.identity_name),
            Err(error) => {
                assert!(error.to_string().contains("身份池已耗尽"), "实际：{error}");
                break;
            }
        }
    }
    assert!(!claimed.is_empty(), "至少要能认领一个，否则这条用例证明不了任何事");
    claimed
}

#[tokio::test]
async fn 池耗尽报错而不是复用() {
    let stack = Stack::new().await;
    stack.settle_once().await;
    let claimed = drain_pool(&stack, "user").await;

    // 反向证据：拿到的名字两两不同——耗尽时报错，而不是把某个名字发第二次。
    let unique: std::collections::BTreeSet<_> = claimed.iter().collect();
    assert_eq!(unique.len(), claimed.len(), "同一个身份不能发给两个人");
}

// ============ 退役 ============

/// 退役即永久：它不会回到空闲池，即使池子已经空了。
#[tokio::test]
async fn 退役的槽位永不回到空闲池() {
    let stack = Stack::new().await;
    stack.settle_once().await;
    let claimed = drain_pool(&stack, "u").await;

    identity::retire(&stack.store, &claimed[0], "kimi", "测试").await.unwrap();
    assert_eq!(slot_state(&stack, &claimed[0]).await.0, "retired");

    // 退了一个，但池子并没有因此多出一个名额。
    let next = make_user(&stack, "after-retire").await;
    let error = identity::claim_for_user(&stack.store, next, "kimi").await.unwrap_err();
    assert!(error.to_string().contains("身份池已耗尽"), "实际：{error}");
}

/// 退役**不解绑归属**：撤权是额度置零而不是删除凭据，
/// 退役后到达的字节确实是那个人产生的。
#[tokio::test]
async fn 退役不解绑归属() {
    let stack = Stack::new().await;
    stack.settle_once().await;
    let user = make_user(&stack, "carol").await;
    let claim = identity::claim_for_user(&stack.store, user, "kimi").await.unwrap();

    identity::retire(&stack.store, &claim.identity_name, "kimi", "离职").await.unwrap();
    let (state, owner) = slot_state(&stack, &claim.identity_name).await;
    assert_eq!(state, "retired");
    assert_eq!(owner, Some(user), "归属必须保留，否则真实尾账会变成无主字节");
}

/// 退役槽位拿零额度，**而且不稀释主人在该节点的份额**。
#[tokio::test]
async fn 退役槽位拿零额度且归属被抹掉() {
    let stack = Stack::new().await;
    stack.settle_once().await;
    let user = make_user(&stack, "dave").await;
    let claim = identity::claim_for_user(&stack.store, user, "kimi").await.unwrap();
    identity::retire(&stack.store, &claim.identity_name, "kimi", "测试").await.unwrap();

    let identities = table::current_identities(&stack.store, NODE, RUNTIME).await.unwrap();
    let retired =
        identities.iter().find(|i| i.name == claim.identity_name).expect("身份仍在节点上");
    assert_eq!(
        retired.user_id, None,
        "退役槽位在规划侧不带归属：既不拿主人的额度，也不参与他的份额切分"
    );

    // 组表时它必须**在表里**且为零，不能被省略——省略在 wire 上等于无限额度。
    let table = table::assemble(&identities, &Default::default()).unwrap();
    assert_eq!(table.len(), identities.len(), "条目数不变");
    let entry = table.iter().find(|(_, name, _)| *name == claim.identity_name).unwrap();
    assert_eq!(entry.2, Quota::Limited(0));
}

/// **尚未认领的槽位也能退役**，而且这是主要用途之一。
///
/// 这条原先断言的是相反的行为，理由是「那会让它既不可用也不可认领，
/// 是一个纯粹的坑」。那个判断错了：**「既不可用也不可认领」正是想要的结果**。
///
/// 部署侧的测试身份（`deploy-test`）、原型控制器的测试账号（`user0001`）
/// 都在节点的 inbound 里，但它们的凭据在别处流通——发给真人等于两个人
/// 共用一份凭据。在这条路存在之前，排除它们只能靠「计数非零」这个**巧合**，
/// 而一次进程重启就会让计数归零，它们悄悄变回可领取。
#[tokio::test]
async fn 未认领的槽位也能永久退出池子() {
    use sqlx::Row;
    let stack = Stack::new().await;
    stack.settle_once().await;
    let name = stack.identities().await[0].name.clone();
    let before = identity::free_slots(&stack.store).await.unwrap();

    let count = identity::retire(&stack.store, &name, "kimi", "凭据在别处流通，不得发给真人")
        .await
        .expect("从未认领的槽位也该退得掉");
    assert!(count >= 1);

    // 它真的从空闲池里少了一个，而不是只改了个状态字段。
    let after = identity::free_slots(&stack.store).await.unwrap();
    for ((node, before_n), (_, after_n)) in before.iter().zip(&after) {
        assert_eq!(*after_n, before_n - 1, "{node} 的空闲槽位应当少一个");
    }

    // 退役行没有归属，且 `claimed_at` 与 `user_id` 同进同出（schema 的 CHECK）。
    let (user_id, claimed_at): (Option<i64>, Option<String>) = sqlx::query(
        "SELECT user_id, claimed_at FROM identity_routes \
          WHERE identity_name = ? AND state = 'retired' LIMIT 1",
    )
    .bind(&name)
    .fetch_one(stack.store.readers())
    .await
    .map(|row| (row.get(0), row.get(1)))
    .unwrap();
    assert!(user_id.is_none() && claimed_at.is_none(), "从未认领的退役行两者都该为空");

    // **反向**：退役不是「删掉」——行还在，只是永远不会再被挑中。
    let still_there: i64 =
        sqlx::query("SELECT count(*) FROM identity_routes WHERE identity_name = ?")
            .bind(&name)
            .fetch_one(stack.store.readers())
            .await
            .unwrap()
            .get(0);
    assert!(still_there > 0, "退役不删行：节点上那个身份还在，账本还要靠它收尾");
}

// ============ 审计 ============

#[tokio::test]
async fn 认领与退役都落审计事件() {
    let stack = Stack::new().await;
    stack.settle_once().await;
    let user = make_user(&stack, "erin").await;
    let claim = identity::claim_for_user(&stack.store, user, "kimi").await.unwrap();
    identity::retire(&stack.store, &claim.identity_name, "kimi", "离职").await.unwrap();

    let rows = sqlx::query(
        "SELECT e.state, e.effective_to FROM identity_assignment_events e \
           JOIN identity_routes r ON r.route_id = e.route_id \
          WHERE r.identity_name = ? ORDER BY e.event_id",
    )
    .bind(&claim.identity_name)
    .fetch_all(stack.store.readers())
    .await
    .unwrap();

    assert_eq!(rows.len(), 2, "认领一条、退役一条");
    assert_eq!(rows[0].get::<String, _>(0), "assigned");
    assert!(rows[0].get::<Option<String>, _>(1).is_some(), "退役时要关掉持有区间");
    assert_eq!(rows[1].get::<String, _>(0), "unassigned");
}

// ============ 指定认领：显式旁路零基线门禁 ============

/// **零基线门禁挡的情形，在「原持有者拿回自己的身份」上不成立。**
///
/// 那道门禁防的是「把一个用过的身份发给新用户，于是他一上来就欠着别人的账」。
/// 而重建库之后原持有者重新领取自己原来那个名字，会被同一条门禁挡住——
/// 他自己转发过的字节让那个名字不再是零基线。2026-09-20 撞上过一次，
/// 当时只能手工改库，也就是把这条判断留痕在一个运维脚本里而不是代码里。
#[tokio::test]
async fn 指定认领拿得到零基线门禁挡住的身份() {
    let stack = Stack::new().await;
    let user = make_user(&stack, "kimi").await;

    // 基线夹具里 u_example_01 带着非零计数，所以它不是合法候选。
    // 先证明这一点，否则下面那条断言可能是因为别的原因绿的。
    let auto = identity::claim_for_user(&stack.store, user, "用例").await.unwrap();
    assert_eq!(auto.identity_name, "s1", "池子只会挑四向计数为零的那个");

    let other = make_user(&stack, "kimi2").await;
    let claim = identity::claim(
        &stack.store,
        other,
        identity::Pick::Named {
            identity_name: "u_example_01",
            reason: "原持有者拿回自己重建前的身份",
            replace: false,
        },
        "用例",
    )
    .await
    .expect("指定认领应当跳过零基线门禁");
    assert_eq!(claim.identity_name, "u_example_01");
    assert!(claim.newly_claimed);
    assert_eq!(slot_state(&stack, "u_example_01").await, ("claimed".into(), Some(other)));
}

/// **fence 两列必须留 NULL。**
///
/// 它们记的是「证明零基线的那份快照」，让这个敞口事后可核。
/// 本支根本没有证明零基线——填一个值就是撒谎，而那是一句没人会去复核的谎。
#[tokio::test]
async fn 指定认领不伪造零基线的证据() {
    let stack = Stack::new().await;
    let user = make_user(&stack, "kimi").await;
    identity::claim(
        &stack.store,
        user,
        identity::Pick::Named { identity_name: "u_example_01", reason: "理由", replace: false },
        "用例",
    )
    .await
    .unwrap();

    let row = sqlx::query(
        "SELECT claim_fence_runtime_id, claim_fence_sequence FROM identity_routes \
          WHERE identity_name = 'u_example_01'",
    )
    .fetch_one(stack.store.readers())
    .await
    .unwrap();
    assert_eq!(row.get::<Option<String>, _>(0), None, "没证明过零基线就不许留证据");
    assert_eq!(row.get::<Option<String>, _>(1), None);
}

/// 理由必填，而且要真的落进只追加的归属事件里。
#[tokio::test]
async fn 指定认领的理由必填且落进归属事件() {
    let stack = Stack::new().await;
    let user = make_user(&stack, "kimi").await;

    let error = identity::claim(
        &stack.store,
        user,
        identity::Pick::Named { identity_name: "u_example_01", reason: "  ", replace: false },
        "用例",
    )
    .await
    .unwrap_err()
    .to_string();
    assert!(error.contains("必须写明理由"), "{error}");

    identity::claim(
        &stack.store,
        user,
        identity::Pick::Named {
            identity_name: "u_example_01",
            reason: "重建库后拿回原身份",
            replace: false,
        },
        "kimi",
    )
    .await
    .unwrap();
    let reason: String = sqlx::query_scalar(
        "SELECT e.reason FROM identity_assignment_events e \
           JOIN identity_routes r ON r.route_id = e.route_id \
          WHERE r.identity_name = 'u_example_01' AND e.state = 'assigned'",
    )
    .fetch_one(stack.store.readers())
    .await
    .unwrap();
    assert!(reason.contains("重建库后拿回原身份"), "{reason}");
    // 操作者也要留，否则事后只知道为什么、不知道是谁。
    assert!(reason.contains("kimi"), "{reason}");
}

/// **部分认领不放行。** 同一个名字在每台上是同一份凭据，缺一台就是订阅里
/// 有一条连不上的入口，而客户端只说「握手失败」。
#[tokio::test]
async fn 指定认领仍然要求全部节点都空闲() {
    let stack = Stack::new().await;
    let user = make_user(&stack, "kimi").await;
    identity::retire(&stack.store, "u_example_01", "用例", "先退役掉").await.unwrap();

    let error = identity::claim(
        &stack.store,
        user,
        identity::Pick::Named { identity_name: "u_example_01", reason: "理由", replace: false },
        "用例",
    )
    .await
    .unwrap_err()
    .to_string();
    assert!(error.contains("必须全部空闲"), "{error}");
}

/// 已经持有别的身份时**不给换**，除非明写 `replace`。
///
/// `grant` 这个词不该悄悄退掉一个已经发出去的凭据。
#[tokio::test]
async fn 换身份必须明写replace() {
    let stack = Stack::new().await;
    let user = make_user(&stack, "kimi").await;
    identity::claim_for_user(&stack.store, user, "用例").await.unwrap();

    let error = identity::claim(
        &stack.store,
        user,
        identity::Pick::Named { identity_name: "u_example_01", reason: "理由", replace: false },
        "用例",
    )
    .await
    .unwrap_err()
    .to_string();
    assert!(error.contains("--replace"), "{error}");
    // 拒绝之后旧的必须原样还在，不能落个半截。
    assert_eq!(slot_state(&stack, "s1").await, ("claimed".into(), Some(user)));
}

/// `replace` 换身份：旧的放回池子并留下 `unassigned` 事件，新的认领成功。
///
/// 事件是**只追加**的（D14）。把 route 行改回 free 而不留事件，
/// 等于让一段持有从历史里消失。
#[tokio::test]
async fn replace换身份留下完整的归属痕迹() {
    let stack = Stack::new().await;
    let user = make_user(&stack, "kimi").await;
    identity::claim_for_user(&stack.store, user, "用例").await.unwrap();

    let claim = identity::claim(
        &stack.store,
        user,
        identity::Pick::Named {
            identity_name: "u_example_01",
            reason: "拿回重建前的身份",
            replace: true,
        },
        "kimi",
    )
    .await
    .unwrap();
    assert_eq!(claim.identity_name, "u_example_01");
    // 旧的回到池子（不是 retired——它没被用过，退役会白白少一个名额）。
    assert_eq!(slot_state(&stack, "s1").await, ("free".into(), None));
    assert_eq!(slot_state(&stack, "u_example_01").await, ("claimed".into(), Some(user)));

    let events: Vec<(String, String)> = sqlx::query_as(
        "SELECT r.identity_name, e.state FROM identity_assignment_events e \
           JOIN identity_routes r ON r.route_id = e.route_id ORDER BY e.event_id",
    )
    .fetch_all(stack.store.readers())
    .await
    .unwrap();
    assert_eq!(
        events,
        vec![
            ("s1".to_string(), "assigned".to_string()),
            ("s1".to_string(), "unassigned".to_string()),
            ("u_example_01".to_string(), "assigned".to_string()),
        ],
        "先放旧的再认领新的，三条事件一条都不能少"
    );
}

/// 指定的就是他已经持有的那个 → 幂等，不白费一个名额。
#[tokio::test]
async fn 指定已持有的身份是幂等的() {
    let stack = Stack::new().await;
    let user = make_user(&stack, "kimi").await;
    let first = identity::claim_for_user(&stack.store, user, "用例").await.unwrap();

    let again = identity::claim(
        &stack.store,
        user,
        identity::Pick::Named {
            identity_name: &first.identity_name,
            reason: "理由",
            replace: false,
        },
        "用例",
    )
    .await
    .unwrap();
    assert_eq!(again.identity_name, first.identity_name);
    assert!(!again.newly_claimed, "幂等不该再消耗一个名额");
}
