//! 下发接线：把配额挂在采集循环后面（`quota::service`）。
//!
//! 这一组测的**不是**配额算得对不对（那在 `allocation` 与 `settings`），
//! 而是这条循环在各种现场状态下**推还是不推、推什么**。
//! 每一条都配一个反向断言：不只看返回值，还要看假节点上**生效表**变没变、
//! 以及它到底收到了几个请求——`PUT` 返回 200 什么都证明不了。

use std::collections::BTreeMap;

use proxy_manager::collect::scheduler::{NodeCollector, SchedulePolicy};
use proxy_manager::quota::converge::{self, NodeContext, RoundPlan};
use proxy_manager::quota::dispatch::{self, ConflictResolution};
use proxy_manager::quota::service::{QuotaRound, QuotaService};
use proxy_manager::quota::table;
use proxy_manager::quota::wire::Quota;

use super::quota_harness::{Stack, NODE, RUNTIME};

/// `normal` 档位的月度额度（二进制 1 TiB，定案第三条；2026-09-20 由十进制改来）。
const NORMAL_BYTES: u64 = 1 << 40;
const NEW_RUNTIME: &str = "fedcba9876543210fedcba9876543210";

fn policy() -> SchedulePolicy {
    SchedulePolicy {
        target: std::time::Duration::from_secs(15),
        minimum: std::time::Duration::from_secs(5),
        max_backoff: std::time::Duration::from_secs(120),
        max_clock_skew_secs: 120,
    }
}

fn service(stack: &Stack, enabled: bool, safe_zero_on_stale: bool) -> QuotaService {
    let collector = NodeCollector::new(NODE, stack.new_client(), policy());
    QuotaService::new(collector, enabled, 300, safe_zero_on_stale)
}

// ============ 第一道闩 ============

/// 没开配额控制的节点**一个请求都不发**。
///
/// 切换时这就是第一道闩：manager 可以先只采不发跑在原型控制器旁边，
/// 而「开不开」是一个配置字段，不是两种启动代码。
#[tokio::test]
async fn 未启用配额时一个请求都不发() {
    let stack = Stack::new().await;
    stack.map_users().await;
    let before = stack.node.quota_requests().await;

    let mut service = service(&stack, false, false);
    assert_eq!(service.dispatch_round(&stack.store).await, QuotaRound::Disabled);

    assert_eq!(stack.node.quota_requests().await, before, "解除武装时不得触碰配额端点");
    assert!(
        stack.limited_set().await.is_empty(),
        "节点上不应出现任何限额——身份默认就是无限，这正是 C16 的形状"
    );
}

/// 未批准的 runtime 不得下发（C8）。
#[tokio::test]
async fn runtime未批准时不下发() {
    let stack = Stack::new().await;
    stack.map_users().await;
    let mut txn = stack.store.begin_immediate().await.unwrap();
    sqlx::query("UPDATE node_runtimes SET approval_status = 'pending' WHERE node_id = ?")
        .bind(NODE)
        .execute(txn.conn())
        .await
        .unwrap();
    txn.commit().await.unwrap();

    let before = stack.node.quota_requests().await;
    let mut service = service(&stack, true, false);
    assert_eq!(service.dispatch_round(&stack.store).await, QuotaRound::NoSettledRuntime);
    assert_eq!(stack.node.quota_requests().await, before);
}

// ============ 正常一轮 ============

/// 一轮完整下发：全量表、每个身份都在、镜像到全额剩余。
#[tokio::test]
async fn 一轮下发覆盖全部身份且是镜像额度() {
    let stack = Stack::new().await;
    stack.map_users().await;
    let identities = stack.identities().await;
    assert!(identities.len() >= 2, "样例至少要有两个身份才谈得上「全量」");

    let mut service = service(&stack, true, false);
    let round = service.dispatch_round(&stack.store).await;
    assert_eq!(round, QuotaRound::Applied { entries: identities.len() }, "实际：{round:?}");

    // C30 的现场证据：节点报的 applied 数必须等于枚举到的身份数。
    // 少一条在 wire 上等于解除限额，而 PUT 照样 200——只看状态码看不出来。
    assert_eq!(stack.node.applied_entries().await, identities.len());
    let effective = stack.effective().await;
    assert_eq!(effective.len(), identities.len());
    for (key, quota) in &effective {
        assert_eq!(
            *quota,
            Quota::Limited(NORMAL_BYTES),
            "{key:?} 应当拿到本周期全额剩余：镜像分配不做跨节点拆分"
        );
    }
}

/// 下一个采集周期结算出新序号之后，这一轮又能推。
///
/// 这条钉的是「节奏跟随采集周期」：一次采集产生一个已结算序号，
/// 一次正额度推送消耗一个，1:1。
#[tokio::test]
async fn 新序号结算之后可以再推一轮() {
    let stack = Stack::new().await;
    stack.map_users().await;
    let mut service = service(&stack, true, false);
    assert!(matches!(service.dispatch_round(&stack.store).await, QuotaRound::Applied { .. }));

    stack.settle_once().await;
    let round = service.dispatch_round(&stack.store).await;
    assert!(matches!(round, QuotaRound::Applied { .. }), "实际：{round:?}");
    assert_eq!(stack.node.quota_requests().await, 2);
}

// ============ 拿不到新鲜序号：偏离 C33 ============

/// **同一个序号不会被推第二次，而且不推零表。**
///
/// 这是对 C33 的**有意偏离**（`quota.safe_zero_on_stale = false`）。
/// 反向断言是这条的全部价值：光看返回值不推、看不出节点上发生了什么。
/// 要看的是生效表**仍然是正额度**——如果哪天有人把默认值改回去，
/// 这里会立刻变成全零，用例当场红。
#[tokio::test]
async fn 没有新鲜序号时既不重推也不推零表() {
    let stack = Stack::new().await;
    stack.map_users().await;
    let mut service = service(&stack, true, false);
    assert!(matches!(service.dispatch_round(&stack.store).await, QuotaRound::Applied { .. }));
    let after_first = stack.node.quota_requests().await;

    // 没有新的采集 → 没有新的已结算序号。
    let round = service.dispatch_round(&stack.store).await;
    assert!(matches!(round, QuotaRound::NotFresh { .. }), "实际：{round:?}");

    assert_eq!(stack.node.quota_requests().await, after_first, "一个请求都不该再发");
    let effective = stack.effective().await;
    assert!(
        effective.values().all(|q| *q == Quota::Limited(NORMAL_BYTES)),
        "节点必须保留上一次授予的余额，而不是被一份全零表切断：{effective:?}"
    );
}

/// 置 `safe_zero_on_stale = true` 一行回到 C33 的原行为。
///
/// 偏离要能一行退回去，否则它就不是一个决定，是一条单行道。
#[tokio::test]
async fn 打开开关之后回到c33的零表行为() {
    let stack = Stack::new().await;
    stack.map_users().await;
    let mut service = service(&stack, true, true);
    assert!(matches!(service.dispatch_round(&stack.store).await, QuotaRound::Applied { .. }));

    assert_eq!(service.dispatch_round(&stack.store).await, QuotaRound::SafeZero);
    let effective = stack.effective().await;
    assert!(!effective.is_empty(), "零表不是空表——空表会把所有人解除限额");
    assert!(
        effective.values().all(|q| *q == Quota::Limited(0)),
        "开关打开时应当把全部受限身份置零：{effective:?}"
    );
}

// ============ 409 的三步分支 ============

/// runtime 变了：告警、记录，但**不自动批准**（C8）。
#[tokio::test]
async fn runtime变了不自动批准也不合闩() {
    let stack = Stack::new().await;
    stack.map_users().await;
    let mut service = service(&stack, true, false);
    assert!(matches!(service.dispatch_round(&stack.store).await, QuotaRound::Applied { .. }));

    // 先攒一个新鲜序号（仍属旧 runtime），再让节点重启：
    // 这样下一轮才会真的带着旧 runtime_id 去推，从而拿到 409。
    stack.settle_once().await;
    stack.node.restart(NEW_RUNTIME).await;

    let round = service.dispatch_round(&stack.store).await;
    assert!(
        matches!(
            round,
            QuotaRound::Conflict { resolution: ConflictResolution::RuntimeChanged { .. } }
        ),
        "实际：{round:?}"
    );
    assert!(
        proxy_manager::ledger::runtime::load(&stack.store, NODE, NEW_RUNTIME)
            .await
            .unwrap()
            .is_none(),
        "批准是人的动作，下发路径不得代劳"
    );
    assert!(!service.halted(), "换 runtime 不是「连错机器」，不该合闩");
}

/// **连接目标不对：合上节点级停推闩，之后一个请求都不发。**
///
/// §4.5 第 1 步说的是「先停止推送并告警」，不是「继续往下分支」。
/// 端点指错意味着我们可能正在给**别人的节点**写表，继续推是在放大伤害。
#[tokio::test]
async fn 连接目标不对时合上停推闩() {
    let stack = Stack::new().await;
    stack.map_users().await;
    let mut service = service(&stack, true, false);
    assert!(matches!(service.dispatch_round(&stack.store).await, QuotaRound::Applied { .. }));

    stack.settle_once().await;
    stack.node.set_node_id("node-example-02").await;

    let round = service.dispatch_round(&stack.store).await;
    assert!(
        matches!(
            round,
            QuotaRound::Conflict { resolution: ConflictResolution::NodeMismatch { .. } }
        ),
        "实际：{round:?}"
    );
    assert!(service.halted());

    let before = stack.node.quota_requests().await;
    assert_eq!(service.dispatch_round(&stack.store).await, QuotaRound::Halted);
    assert_eq!(stack.node.quota_requests().await, before, "闩合上之后一个请求都不发");
}

// ============ 崩溃恢复 ============

/// 启动时把上次崩溃留下的 `prepared` 收成 `unknown`。
///
/// `run_service` 此前从不调用 `recover_prepared`：进程在发送后崩溃，
/// 那条请求会**永远**停在 `prepared`。它不能被当成「没发出去」——
/// 响应丢失不能证明旧请求没执行。
#[tokio::test]
async fn 启动时把未确认的请求收成unknown() {
    let stack = Stack::new().await;
    stack.map_users().await;
    let identities = stack.identities().await;

    // 制造现场：prepare 之后既不 send 也不 record，正是崩溃留下的形态。
    let prepared =
        dispatch::prepare(&stack.store, NODE, RUNTIME, table::zero_table(&identities), None, 1, 1)
            .await
            .unwrap();
    assert_eq!(stack.request_result(prepared.request_id).await, "prepared");

    let (shutdown_tx, shutdown_rx) = tokio::sync::watch::channel(false);
    let handle = tokio::spawn(service(&stack, true, false).run(stack.store.clone(), shutdown_rx));
    shutdown_tx.send(true).unwrap();
    handle.await.unwrap();

    assert_eq!(
        stack.request_result(prepared.request_id).await,
        "unknown",
        "不得当成「没发出去」：epoch 必须从最大已分配值继续"
    );
}

// ============ C30 不变式 ============

/// 计划里出现无限额度就地报错。
///
/// 定案第三条给 Admin 的也是有限额度，所以**没有任何人是 Unlimited**，
/// 「wire 条目数 == 表条目数 == 身份数」恒成立。哪天有人加回一档无限额度，
/// 这条会当场红——而不是悄悄下发一份漏人的表、拿回一个 200。
#[tokio::test]
async fn 计划里出现无限额度会被就地挡住() {
    let stack = Stack::new().await;
    stack.map_users().await;
    let revision =
        proxy_manager::quota::settings::load(&stack.store).await.unwrap().unwrap().revision;
    let identities = stack.identities().await;
    let entries: Vec<(String, String, Quota)> = identities
        .iter()
        .map(|i| (i.inbound_tag.clone(), i.name.clone(), Quota::Unlimited))
        .collect();
    let plan = RoundPlan { revision, tables: BTreeMap::from([(NODE.to_string(), entries)]) };
    let context = NodeContext {
        node_id: NODE.into(),
        runtime_id: RUNTIME.into(),
        weight: 1,
        fresh_sequence: Some(1),
    };

    let before = stack.node.quota_requests().await;
    let error = converge::converge_node(&stack.store, &stack.client, &context, &plan)
        .await
        .expect_err("应当在发送之前就失败");
    assert!(error.to_string().contains("无限额度"), "实际：{error}");
    assert_eq!(stack.node.quota_requests().await, before, "不得把这份表发出去");
}
