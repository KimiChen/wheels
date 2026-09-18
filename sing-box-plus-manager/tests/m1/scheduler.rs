//! 采集调度的全栈用例。
//!
//! 链路是真的：**假节点 UDS → 真实 agent 服务端 → mTLS → 主控客户端 → 调度器 → 账本**。
//! 这是 M1 唯一一条把所有部件串起来的用例——前面那些各自只验一段。

use std::sync::Arc;
use std::time::Duration;

use proxy_manager::agent::AgentClient;
use proxy_manager::collect::{acceptance_criteria, NodeCollector, SchedulePolicy, Tick};
use proxy_manager::config::StorageConfig;
use proxy_manager::ledger::runtime;
use proxy_manager::ledger::settle::FirstSnapshot;
use proxy_manager::store::Store;
use proxy_manager_agent::{commands::Executor, config::AgentConfig, server::Server};

use super::fake_node::{FakeNode, TransportFault};
use super::harness::{self, DNS_NAME};
use super::ledger_harness::{NODE, RUNTIME, STARTED_AT_MS};

struct Stack {
    _materials: harness::Materials,
    _dir: tempfile::TempDir,
    node: FakeNode,
    store: Arc<Store>,
    collector: NodeCollector,
}

async fn stack() -> Stack {
    let materials = harness::materials();
    let node = FakeNode::start().await;
    let dir = tempfile::tempdir().unwrap();

    // agent
    let config_toml = format!(
        "node_id = \"{NODE}\"\nlisten = \"127.0.0.1:0\"\nmaterials_dir = \"{}\"\n\n\
         [sockets]\nsnapshot = \"{}\"\nquota = \"{}\"\n",
        materials.agent_dir.display(),
        node.snapshot_socket.display(),
        node.quota_socket.display(),
    );
    let config_path = dir.path().join("agent.toml");
    std::fs::write(&config_path, config_toml).unwrap();
    let agent_config = AgentConfig::load(&config_path).unwrap();
    let server_config = proxy_manager_wire::tls::server_config(&materials.agent_dir).unwrap();
    let hmac =
        proxy_manager_wire::tls::load_hmac_key(&materials.agent_dir.join("hmac.key")).unwrap();
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let endpoint = listener.local_addr().unwrap().to_string();
    tokio::spawn(Server::new(server_config, Executor::new(agent_config), hmac).serve(listener));

    // 主控
    let storage = StorageConfig { path: dir.path().join("pm.db"), busy_timeout_ms: 5_000 };
    let store = Store::open(&storage).await.unwrap();
    store.init_schema().await.unwrap();
    let mut txn = store.begin_immediate().await.unwrap();
    sqlx::query(
        "INSERT INTO nodes(node_id, provider, address, first_snapshot, agent_spki_sha256, \
         quota_enabled, status, created_at, updated_at) \
         VALUES (?, 'plus_v3', 'node-01.example.com:8443', 'baseline', ?, 1, 'active', ?, ?)",
    )
    .bind(NODE)
    .bind("0".repeat(64))
    .bind("2026-09-18T00:00:00Z")
    .bind("2026-09-18T00:00:00Z")
    .execute(txn.conn())
    .await
    .unwrap();
    txn.commit().await.unwrap();

    let client = AgentClient::new(NODE, &endpoint, DNS_NAME, &materials.controller_dir).unwrap();
    let policy = SchedulePolicy {
        target: Duration::from_millis(30),
        minimum: Duration::from_millis(10),
        max_backoff: Duration::from_millis(200),
        max_clock_skew_secs: 120,
    };

    Stack {
        collector: NodeCollector::new(NODE, client, policy),
        store: Arc::new(store),
        node,
        _materials: materials,
        _dir: dir,
    }
}

impl Stack {
    async fn approve(&self, policy: FirstSnapshot) {
        runtime::approve(&self.store, NODE, RUNTIME, STARTED_AT_MS, policy, "用例", "test")
            .await
            .unwrap();
    }

    async fn scalar(&self, sql: &str) -> i64 {
        use sqlx::Row;
        sqlx::query(sql).fetch_one(self.store.readers()).await.unwrap().get(0)
    }

    async fn lifetime_sum(&self) -> u128 {
        use sqlx::Row;
        sqlx::query(
            "SELECT tcp_uplink_bytes, tcp_downlink_bytes, udp_uplink_bytes, udp_downlink_bytes \
             FROM usage_lifetime_totals",
        )
        .fetch_all(self.store.readers())
        .await
        .unwrap()
        .iter()
        .flat_map(|row| {
            (0..4).map(move |i| {
                row.get::<String, _>(i).trim_start_matches('0').parse::<u128>().unwrap_or(0)
            })
        })
        .sum()
    }
}

// ============ 正向全栈 ============

#[tokio::test]
async fn 一次tick走完整条链路() {
    let stack = stack().await;
    stack.approve(FirstSnapshot::Include).await;

    let tick = stack.collector.tick(&stack.store).await;
    assert!(matches!(tick, Tick::Applied { settled: 1, ledger_rows: 2 }), "实际：{tick:?}");
    assert_eq!(stack.lifetime_sum().await, 312, "样例里两个非零身份合计 300 + 12");
    assert_eq!(stack.node.snapshot_requests().await, 1);
}

/// 连续多轮：sequence 递增，差分正确。
#[tokio::test]
async fn 连续多轮采集按差值累计() {
    let stack = stack().await;
    stack.approve(FirstSnapshot::Baseline).await;

    // 第一轮：baseline，不入账。
    assert!(matches!(
        stack.collector.tick(&stack.store).await,
        Tick::Applied { ledger_rows: 0, .. }
    ));

    // 之后每轮给一个身份加 1000。
    for round in 1..=5u64 {
        stack
            .node
            .set_counter("vless-entry-01", "u_example_01", "tcp_uplink_bytes", 100 + round * 1_000)
            .await;
        let tick = stack.collector.tick(&stack.store).await;
        assert!(matches!(tick, Tick::Applied { ledger_rows: 1, .. }), "第 {round} 轮：{tick:?}");
    }
    assert_eq!(stack.lifetime_sum().await, 5_000);
    assert_eq!(stack.scalar("SELECT count(*) FROM usage_ledger").await, 5);
}

/// **先收到、后批准**：批准之前攒下的批次要在一次 tick 里被排空。
#[tokio::test]
async fn 批准之后一次tick排空积压() {
    let stack = stack().await;

    // 批准之前采三轮：每轮都 Deferred，批次留在 pending。
    for round in 1..=3u64 {
        stack
            .node
            .set_counter("vless-entry-01", "u_example_01", "tcp_uplink_bytes", 100 + round * 1_000)
            .await;
        let tick = stack.collector.tick(&stack.store).await;
        assert!(matches!(tick, Tick::Deferred(_)), "第 {round} 轮应当 Deferred：{tick:?}");
    }
    assert_eq!(
        stack.scalar("SELECT count(*) FROM snapshot_batches WHERE status = 'pending'").await,
        3,
        "三份都留着"
    );
    assert_eq!(stack.scalar("SELECT count(*) FROM usage_ledger").await, 0);

    // 批准。下一次 tick 要把积压一次排空：
    // 3 份旧的 + 这一次新采的 = 4 份。
    stack.approve(FirstSnapshot::Baseline).await;
    let tick = stack.collector.tick(&stack.store).await;
    match tick {
        Tick::Applied { settled, .. } => assert_eq!(settled, 4, "积压必须被排空"),
        other => panic!("实际：{other:?}"),
    }
    assert_eq!(
        stack.scalar("SELECT count(*) FROM snapshot_batches WHERE status = 'pending'").await,
        0
    );
    // 首份按 baseline 记 0，之后按差值：1000 + 1000 + 0（第四次没加流量）。
    assert_eq!(stack.lifetime_sum().await, 2_000);
}

// ============ C7：可重试且不得入账 ============

#[tokio::test]
async fn 上游429可重试且不入账() {
    let stack = stack().await;
    stack.approve(FirstSnapshot::Include).await;
    stack.node.push_fault(TransportFault::Status(429)).await;

    let tick = stack.collector.tick(&stack.store).await;
    assert!(matches!(tick, Tick::Retryable(_)), "实际：{tick:?}");
    assert_eq!(stack.scalar("SELECT count(*) FROM usage_ledger").await, 0);
    assert_eq!(stack.scalar("SELECT count(*) FROM snapshot_batches").await, 0, "429 不落回执");

    // 退避之后重试必须成功。
    let tick = stack.collector.tick(&stack.store).await;
    assert!(matches!(tick, Tick::Applied { .. }), "实际：{tick:?}");
}

#[tokio::test]
async fn 响应丢失可重试且不当成空快照() {
    let stack = stack().await;
    stack.approve(FirstSnapshot::Include).await;
    stack.node.push_fault(TransportFault::CloseWithoutResponse).await;

    let tick = stack.collector.tick(&stack.store).await;
    assert!(matches!(tick, Tick::Retryable(_)), "实际：{tick:?}");
    assert_eq!(stack.scalar("SELECT count(*) FROM usage_ledger").await, 0);
    // 节点侧**已经处理过**了——这正是「结果不明」，不是「没发生」。
    assert_eq!(stack.node.snapshot_requests().await, 1);
}

/// 整份拒绝不退避：节点在正常回应，只是内容不可入账。
#[tokio::test]
async fn health不可入账时拒绝而不是重试() {
    let stack = stack().await;
    stack.approve(FirstSnapshot::Include).await;
    stack.node.set_health_bit("counter_overflow", true).await;

    let tick = stack.collector.tick(&stack.store).await;
    assert!(matches!(tick, Tick::Rejected(_)), "实际：{tick:?}");
    assert_eq!(stack.scalar("SELECT count(*) FROM usage_ledger").await, 0);
    // 但回执要留——可审计的拒绝。
    assert_eq!(
        stack.scalar("SELECT count(*) FROM snapshot_batches WHERE status = 'rejected'").await,
        1
    );
}

/// 版本错配在**接收**阶段就被整份拒绝，回执仍然留。
#[tokio::test]
async fn 版本错配留回执但不入账() {
    let stack = stack().await;
    stack.approve(FirstSnapshot::Include).await;
    stack.node.set_schema_version(2).await;

    let tick = stack.collector.tick(&stack.store).await;
    // envelope 仍认得出，所以留了 rejected 回执，settle 时没有 pending 可结算。
    assert!(matches!(tick, Tick::Applied { settled: 0, ledger_rows: 0 }), "实际：{tick:?}");
    assert_eq!(
        stack.scalar("SELECT count(*) FROM snapshot_batches WHERE status = 'rejected'").await,
        1
    );
    assert_eq!(stack.scalar("SELECT count(*) FROM usage_ledger").await, 0);
}

// ============ collected_at 来自签名 ============

/// `collected_at` 取 agent 签名里的时刻，所以 C27 的时钟偏移才看得见。
#[tokio::test]
async fn collected_at来自agent的签名时刻() {
    use sqlx::Row;
    let stack = stack().await;
    stack.approve(FirstSnapshot::Include).await;
    stack.collector.tick(&stack.store).await;

    let row =
        sqlx::query("SELECT collected_at, received_at, time_quality FROM snapshot_batches LIMIT 1")
            .fetch_one(stack.store.readers())
            .await
            .unwrap();
    let collected: String = row.get(0);
    let received: String = row.get(1);
    assert_eq!(row.get::<String, _>(2), "trusted", "本机两端时钟一致，不该回落");
    // 两个时刻都记了，而且不是同一个字段抄两遍。
    assert!(!collected.is_empty() && !received.is_empty());
    let collected =
        time::OffsetDateTime::parse(&collected, &time::format_description::well_known::Rfc3339)
            .unwrap();
    let received =
        time::OffsetDateTime::parse(&received, &time::format_description::well_known::Rfc3339)
            .unwrap();
    assert!((received - collected).abs() < time::Duration::seconds(5));
}

// ============ 四项判据 ============

#[tokio::test]
async fn 连续采集后四项判据全为零() {
    let stack = stack().await;
    stack.approve(FirstSnapshot::Baseline).await;
    for round in 1..=10u64 {
        stack
            .node
            .set_counter("vless-entry-01", "u_example_01", "tcp_uplink_bytes", 100 + round * 500)
            .await;
        stack.collector.tick(&stack.store).await;
    }
    let criteria = acceptance_criteria(&stack.store).await.unwrap();
    assert!(criteria.all_zero(), "实际：{criteria:?}");
}

/// 反向证据：判据统计不是恒为 0。
///
/// 手工往库里塞一条「被拒但标成 applied」的回执——那正是 unhealthy 入账的形态。
/// 没有这一条，上面那条对一个「永远返回全零」的实现同样是绿的。
#[tokio::test]
async fn 判据统计能发现被污染的账本() {
    let stack = stack().await;
    stack.approve(FirstSnapshot::Include).await;
    stack.collector.tick(&stack.store).await;
    assert!(acceptance_criteria(&stack.store).await.unwrap().all_zero());

    let mut txn = stack.store.begin_immediate().await.unwrap();
    sqlx::query(
        "UPDATE snapshot_batches SET reject_reason = '不可入账的 health 位为真' \
         WHERE status = 'applied'",
    )
    .execute(txn.conn())
    .await
    .unwrap();
    txn.commit().await.unwrap();

    let criteria = acceptance_criteria(&stack.store).await.unwrap();
    assert!(!criteria.all_zero(), "被污染的账本必须被发现");
    assert_eq!(criteria.unhealthy_accounted, 1);
}

// ============ 循环与停机 ============

/// 定时循环能跑起来，也能被停下来。
#[tokio::test]
async fn 循环可以被停止信号停下() {
    let stack = stack().await;
    stack.approve(FirstSnapshot::Baseline).await;

    let (tx, rx) = tokio::sync::watch::channel(false);
    let store = stack.store.clone();
    let handle = tokio::spawn(stack.collector.run(store, rx));

    // 等它跑几轮。
    tokio::time::sleep(Duration::from_millis(150)).await;
    assert!(stack.node.snapshot_requests().await >= 2, "循环应当已经采过几轮");

    tx.send(true).unwrap();
    let stopped = tokio::time::timeout(Duration::from_secs(5), handle).await;
    assert!(stopped.is_ok(), "收到停止信号后必须退出循环");
}
