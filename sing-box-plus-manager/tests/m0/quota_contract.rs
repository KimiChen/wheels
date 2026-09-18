//! 配额端点故障矩阵（README §7 M0、§8）。
//!
//! M0 的完成标准这么写：「复现 409 两种成因、响应丢失与延迟到达，
//! **通过生效表验证**完整零表收敛、epoch 顺序及新 sequence 门禁」。
//!
//! 「通过生效表验证」这五个字是整组用例的关键。`PUT` 返回 200 什么都证明不了：
//! 一张**空表**同样返回 200，而它的效果是把节点上所有人解除限额。
//! 所以每条断言落在 [`FakeNode::effective_quota`] 上，不落在状态码上。

use std::collections::{BTreeMap, BTreeSet};
use std::time::Duration;

use proxy_manager::collect::{snapshot, UdsClient, SNAPSHOT_PATH};
use proxy_manager::config::StorageConfig;
use proxy_manager::quota::{
    classify_conflict, ConflictCause, Quota, QuotaRequest, SerializeError, QUOTA_PATH,
};
use proxy_manager::store::{codec::U64Text, Store};

use super::fake_node::{FakeNode, TransportFault};

type Identity = (String, String);

async fn all_identities(node: &FakeNode) -> Vec<Identity> {
    node.identities().await.into_iter().collect()
}

/// 构造该节点**所有受限身份**的完整表（C30）。
async fn full_table(node: &FakeNode, epoch: u64, bytes: u64) -> QuotaRequest {
    let identities = all_identities(node).await;
    QuotaRequest::build(
        node.node_id().await,
        node.runtime_id().await,
        epoch,
        identities.into_iter().map(|(tag, name)| (tag, name, Quota::Limited(bytes))),
    )
    .expect("完整表应当可构造")
}

async fn put(node: &FakeNode, request: &QuotaRequest) -> Result<(u16, Vec<u8>), String> {
    let response = UdsClient::default()
        .put_json(&node.quota_socket, QUOTA_PATH, &request.to_bytes())
        .await
        .map_err(|e| e.to_string())?;
    Ok((response.status, response.body))
}

fn expect_all(table: &BTreeMap<Identity, Quota>, quota: Quota) {
    for (identity, actual) in table {
        assert_eq!(*actual, quota, "{identity:?} 的生效额度不符");
    }
}

// ---- 正向：完整表被应用 ----

#[tokio::test]
async fn 完整表应用后每个身份都在生效表里() {
    let node = FakeNode::start().await;
    expect_all(&node.effective_quota().await, Quota::Unlimited);

    let request = full_table(&node, 1, 1_000).await;
    let (status, body) = put(&node, &request).await.unwrap();
    assert_eq!(status, 200, "响应体：{}", String::from_utf8_lossy(&body));

    let effective = node.effective_quota().await;
    assert_eq!(effective.len(), 3, "夹具样例里有三个计费身份");
    expect_all(&effective, Quota::Limited(1_000));
    assert_eq!(node.quota_epoch().await, 1);
    assert!(node.quota_accepted().await);
}

// ---- C33：完整零表收敛 ----

/// C33 的**肯定动作**：无新鲜快照但通道可用时，主控必须主动下发一份
/// **全零完整节点表**并拿到 200，而不是什么都不做。
///
/// 两者在观测上完全不同——不作为会让节点带着旧的正额度一直跑到自己耗尽。
#[tokio::test]
async fn 全零完整表把每个身份都置零() {
    let node = FakeNode::start().await;
    put(&node, &full_table(&node, 1, 1_000).await).await.unwrap();
    expect_all(&node.effective_quota().await, Quota::Limited(1_000));

    let zero = full_table(&node, 2, 0).await;
    assert!(zero.is_zero_table());
    assert_eq!(zero.entries.len(), 3, "零额度条目必须在表里，不能省略");
    let (status, _) = put(&node, &zero).await.unwrap();
    assert_eq!(status, 200);

    expect_all(&node.effective_quota().await, Quota::Limited(0));
}

/// 反向证据，也是这一整组用例存在的理由：
/// **空表同样返回 200**，但它把所有人解除限额。
///
/// 现有的否定面断言（「不许刷新正额度」）对一个「什么都不做」或「发空表」的实现
/// 同样是绿的，只有生效表能把两者区分开。
#[tokio::test]
async fn 空表也返回200但会解除所有限额() {
    let node = FakeNode::start().await;
    put(&node, &full_table(&node, 1, 1_000).await).await.unwrap();

    let empty = QuotaRequest::build(
        node.node_id().await,
        node.runtime_id().await,
        2,
        Vec::<(String, String, Quota)>::new(),
    )
    .unwrap();
    let (status, _) = put(&node, &empty).await.unwrap();
    assert_eq!(status, 200, "空表在 wire 上完全合法");

    expect_all(&node.effective_quota().await, Quota::Unlimited);
}

/// C16 / C30：**缺条目 = 解除限额**，不是保持原值。
#[tokio::test]
async fn 表里少一个身份就等于给他解除限额() {
    let node = FakeNode::start().await;
    put(&node, &full_table(&node, 1, 1_000).await).await.unwrap();

    let identities = all_identities(&node).await;
    let omitted = identities[0].clone();
    let partial = QuotaRequest::build(
        node.node_id().await,
        node.runtime_id().await,
        2,
        identities.iter().skip(1).cloned().map(|(tag, name)| (tag, name, Quota::Limited(500))),
    )
    .unwrap();
    put(&node, &partial).await.unwrap();

    let effective = node.effective_quota().await;
    assert_eq!(
        effective[&omitted],
        Quota::Unlimited,
        "被省略的 {omitted:?} 在节点上已经是无限额度——这正是 C30 要求枚举全部身份的原因"
    );
    for identity in identities.iter().skip(1) {
        assert_eq!(effective[identity], Quota::Limited(500));
    }
}

/// `Unlimited` 必须省略、`Limited(0)` 必须保留，两者同时出现时各走各的。
#[tokio::test]
async fn unlimited省略而limited零保留() {
    let node = FakeNode::start().await;
    let identities = all_identities(&node).await;
    let request = QuotaRequest::build(
        node.node_id().await,
        node.runtime_id().await,
        1,
        vec![
            (identities[0].0.clone(), identities[0].1.clone(), Quota::Unlimited),
            (identities[1].0.clone(), identities[1].1.clone(), Quota::Limited(0)),
            (identities[2].0.clone(), identities[2].1.clone(), Quota::Limited(7)),
        ],
    )
    .unwrap();
    assert_eq!(request.entries.len(), 2, "Unlimited 不进表");
    put(&node, &request).await.unwrap();

    let effective = node.effective_quota().await;
    assert_eq!(effective[&identities[0]], Quota::Unlimited);
    assert_eq!(effective[&identities[1]], Quota::Limited(0));
    assert_eq!(effective[&identities[2]], Quota::Limited(7));
}

// ---- epoch 顺序 ----

#[tokio::test]
async fn epoch必须严格前进否则409且生效表不变() {
    let node = FakeNode::start().await;
    put(&node, &full_table(&node, 5, 1_000).await).await.unwrap();
    let before = node.effective_quota().await;

    for stale in [1u64, 4, 5] {
        let request = full_table(&node, stale, 42).await;
        let (status, _) = put(&node, &request).await.unwrap();
        assert_eq!(status, 409, "epoch {stale} ≤ 已接受的 5，必须 409");
        assert_eq!(node.effective_quota().await, before, "被拒的请求不得改变生效表");
        assert_eq!(node.quota_epoch().await, 5);
    }

    let request = full_table(&node, 6, 42).await;
    assert_eq!(put(&node, &request).await.unwrap().0, 200);
    expect_all(&node.effective_quota().await, Quota::Limited(42));
}

/// C31：`epoch` 必须 ≥ 1。主控这一侧在**构造时**就拦住，不把 0 发出去。
#[tokio::test]
async fn epoch为零在构造时就被拦住() {
    let node = FakeNode::start().await;
    let error = QuotaRequest::build(
        node.node_id().await,
        node.runtime_id().await,
        0,
        Vec::<(String, String, Quota)>::new(),
    )
    .unwrap_err();
    assert_eq!(error, SerializeError::EpochZero);

    // 反向证据：真发出去节点也会 400。两道防线都要在。
    let mut request = full_table(&node, 1, 10).await;
    request.epoch = 0;
    let (status, _) = put(&node, &request).await.unwrap();
    assert_eq!(status, 400);
    expect_all(&node.effective_quota().await, Quota::Unlimited);
}

// ---- 409 的两种成因 ----

/// **409 没有 discriminator**：`runtime_id` 不符与 `epoch` 未前进返回同一个响应体。
///
/// 这条用例的价值就在于把「两个响应体逐字节相同」钉死。
/// 一旦有人试图从响应体里读出成因，它会立刻红。
#[tokio::test]
async fn 两种409的响应体逐字节相同() {
    let node = FakeNode::start().await;
    put(&node, &full_table(&node, 5, 1_000).await).await.unwrap();

    // 成因一：epoch 未前进。
    let stale = full_table(&node, 5, 1).await;
    let (status_epoch, body_epoch) = put(&node, &stale).await.unwrap();

    // 成因二：runtime 不符。
    let mut wrong_runtime = full_table(&node, 99, 1).await;
    wrong_runtime.runtime_id = "f".repeat(32);
    let (status_runtime, body_runtime) = put(&node, &wrong_runtime).await.unwrap();

    assert_eq!(status_epoch, 409);
    assert_eq!(status_runtime, 409);
    assert_eq!(
        body_epoch, body_runtime,
        "节点对两种成因返回同一个错误体，因此必须取快照读回 runtime_id 才能分支"
    );

    let counters = node.counters().await;
    assert_eq!(counters.rejected_by_epoch, 1);
    assert_eq!(counters.rejected_by_runtime, 1);
}

/// 409 之后的正确动作：取快照读回 `runtime_id`，按它分支。
#[tokio::test]
async fn 取快照读回runtime_id才能判别409成因() {
    let node = FakeNode::start().await;
    let original_runtime = node.runtime_id().await;
    put(&node, &full_table(&node, 5, 1_000).await).await.unwrap();

    // 场景一：runtime 没变，那就是 epoch 的问题。
    let observed = observe_runtime(&node).await;
    assert_eq!(
        classify_conflict(&original_runtime, observed.as_deref()),
        ConflictCause::StaleEpoch
    );

    // 场景二：节点重启换了 runtime。
    let new_runtime = "a".repeat(32);
    node.restart(&new_runtime).await;
    let stale = QuotaRequest::build(
        node.node_id().await,
        original_runtime.clone(),
        6,
        all_identities(&node).await.into_iter().map(|(t, n)| (t, n, Quota::Limited(1))),
    )
    .unwrap();
    let (status, _) = put(&node, &stale).await.unwrap();
    assert_eq!(status, 409, "拿旧 runtime_id 推送必然 409——这是节点侧的「重启即解封」纠正机制");

    let observed = observe_runtime(&node).await;
    assert_eq!(observed.as_deref(), Some(new_runtime.as_str()));
    assert_eq!(
        classify_conflict(&original_runtime, observed.as_deref()),
        ConflictCause::RuntimeMismatch
    );

    // 重启后额度状态全部丢失（C12：额度是纯内存的）。
    expect_all(&node.effective_quota().await, Quota::Unlimited);

    // 新 runtime 的 epoch 可以从 1 开始，先下发零额度安全表。
    let zero = QuotaRequest::build(
        node.node_id().await,
        new_runtime.clone(),
        1,
        all_identities(&node).await.into_iter().map(|(t, n)| (t, n, Quota::Limited(0))),
    )
    .unwrap();
    assert_eq!(put(&node, &zero).await.unwrap().0, 200);
    expect_all(&node.effective_quota().await, Quota::Limited(0));
}

/// 取不到快照时**不得**假设成其中任何一种。存疑时不猜。
#[tokio::test]
async fn 取不到快照时409成因判为不确定() {
    let node = FakeNode::start().await;
    node.push_fault(TransportFault::CloseWithoutResponse).await;
    let observed = observe_runtime(&node).await;
    assert_eq!(observed, None);
    assert_eq!(classify_conflict("whatever", None), ConflictCause::Undetermined);
}

async fn observe_runtime(node: &FakeNode) -> Option<String> {
    let response = UdsClient::default().get(&node.snapshot_socket, SNAPSHOT_PATH).await.ok()?;
    let parsed = snapshot::parse(&response.body).ok()?;
    Some(parsed.runtime_id)
}

/// `node_id` 配错要单独拒绝：这是 §4.5 的第 1 步，
/// 发现不一致要**先停止推送并告警**，而不是继续往下按 runtime/epoch 分支。
#[tokio::test]
async fn node_id配错也返回409且不改变生效表() {
    let node = FakeNode::start().await;
    put(&node, &full_table(&node, 1, 1_000).await).await.unwrap();
    let before = node.effective_quota().await;

    let mut wrong = full_table(&node, 2, 1).await;
    wrong.node_id = "node-example-02".into();
    let (status, _) = put(&node, &wrong).await.unwrap();
    assert_eq!(status, 409);
    assert_eq!(node.effective_quota().await, before);
    assert_eq!(node.counters().await.rejected_by_runtime, 1);
}

// ---- 响应丢失与延迟到达 ----

/// 「节点已应用但 200 丢失」：主控这一侧是 `unknown`，不是失败。
///
/// **响应丢失不能证明旧请求没执行**——所以恢复时不能用「最后一次 200 的 epoch」，
/// 必须用**发送前持久化的最大已分配 epoch**。
#[tokio::test]
async fn 响应丢失时节点已应用而主控只知道结果不明() {
    let node = FakeNode::start().await;
    node.push_fault(TransportFault::CloseWithoutResponse).await;

    let request = full_table(&node, 7, 500).await;
    let outcome = put(&node, &request).await;
    assert!(outcome.is_err(), "主控收不到可信响应");

    // 节点侧**已经应用了**。
    expect_all(&node.effective_quota().await, Quota::Limited(500));
    assert_eq!(node.quota_epoch().await, 7);

    // 于是「按最后一次 200 恢复」是错的：那会重发 epoch 7 并拿到 409。
    let replay = full_table(&node, 7, 500).await;
    assert_eq!(put(&node, &replay).await.unwrap().0, 409);

    // 正确做法：从最大已分配 epoch 取更大值收敛。
    let converge = full_table(&node, 8, 0).await;
    assert_eq!(put(&node, &converge).await.unwrap().0, 200);
    expect_all(&node.effective_quota().await, Quota::Limited(0));
}

#[tokio::test]
async fn 延迟到达时主控超时而节点已应用() {
    let node = FakeNode::start().await;
    node.push_fault(TransportFault::Delay(Duration::from_millis(400))).await;

    let request = full_table(&node, 3, 250).await;
    let client = UdsClient::with_timeout(Duration::from_millis(80));
    let outcome = client.put_json(&node.quota_socket, QUOTA_PATH, &request.to_bytes()).await;
    assert!(outcome.is_err(), "主控这一侧超时");

    // 等节点把事情做完再看：它早就应用了。
    tokio::time::sleep(Duration::from_millis(500)).await;
    expect_all(&node.effective_quota().await, Quota::Limited(250));
    assert_eq!(node.quota_epoch().await, 3);
}

#[tokio::test]
async fn 配额链路上的429可重试且不改变生效表() {
    let node = FakeNode::start().await;
    node.push_fault(TransportFault::Status(429)).await;
    let request = full_table(&node, 1, 100).await;
    let (status, _) = put(&node, &request).await.unwrap();
    assert_eq!(status, 429);
    expect_all(&node.effective_quota().await, Quota::Unlimited);
    assert!(!node.quota_accepted().await, "429 没有进入处理逻辑");

    // 退避后重试同一 epoch 必须成功——它确实没被应用过。
    let (status, _) = put(&node, &request).await.unwrap();
    assert_eq!(status, 200);
    expect_all(&node.effective_quota().await, Quota::Limited(100));
}

// ---- C15：未知身份整份 400 ----

#[tokio::test]
async fn 未知身份整份400且一个cell都不写() {
    let node = FakeNode::start().await;
    put(&node, &full_table(&node, 1, 1_000).await).await.unwrap();
    let before = node.effective_quota().await;

    let mut identities: Vec<(String, String, Quota)> =
        all_identities(&node).await.into_iter().map(|(t, n)| (t, n, Quota::Limited(1))).collect();
    identities.push(("vless-entry-01".into(), "不存在的身份".into(), Quota::Limited(1)));
    // 名字含非 ASCII，主控这一侧在构造时就会拦下——先证明这一道。
    assert!(QuotaRequest::build(
        node.node_id().await,
        node.runtime_id().await,
        2,
        identities.clone()
    )
    .is_err());

    // 换一个合法 token 但节点配置里不存在的身份，让它真的发出去。
    identities.pop();
    identities.push(("vless-entry-01".into(), "u_example_99".into(), Quota::Limited(1)));
    let request =
        QuotaRequest::build(node.node_id().await, node.runtime_id().await, 2, identities).unwrap();
    let (status, body) = put(&node, &request).await.unwrap();
    assert_eq!(status, 400);
    let text = String::from_utf8_lossy(&body);
    assert!(text.contains("u_example_99"), "错误体要回未知项，实际：{text}");

    assert_eq!(node.effective_quota().await, before, "整份拒绝，不得局部应用");
    assert_eq!(node.counters().await.rejected_by_unknown, 1);
    assert_eq!(node.quota_epoch().await, 1, "被拒的请求不推进 epoch");
}

// ---- C31：remaining_bytes 是 int64 ----

#[tokio::test]
async fn 超出int64的额度在序列化时就失败() {
    let node = FakeNode::start().await;
    let error = QuotaRequest::build(
        node.node_id().await,
        node.runtime_id().await,
        1,
        vec![("ss-in".into(), "s1".into(), Quota::Limited(u64::MAX))],
    )
    .unwrap_err();
    assert!(matches!(error, SerializeError::ExceedsInt64 { .. }), "实际：{error}");

    // 边界上恰好能过。
    let ok = QuotaRequest::build(
        node.node_id().await,
        node.runtime_id().await,
        1,
        vec![("ss-in".into(), "s1".into(), Quota::Limited(i64::MAX as u64))],
    )
    .unwrap();
    assert_eq!(ok.entries[0].remaining_bytes, i64::MAX);
    assert_eq!(put(&node, &ok).await.unwrap().0, 200);
    assert_eq!(
        node.effective_quota().await[&("ss-in".to_string(), "s1".to_string())],
        Quota::Limited(i64::MAX as u64)
    );
}

#[tokio::test]
async fn 负额度被节点拒绝() {
    let node = FakeNode::start().await;
    let mut request = full_table(&node, 1, 10).await;
    request.entries[0].remaining_bytes = -1;
    let (status, _) = put(&node, &request).await.unwrap();
    assert_eq!(status, 400);
    expect_all(&node.effective_quota().await, Quota::Unlimited);
}

// ---- 新 sequence 门禁（库层） ----

/// 每份**正额度**请求占用一个新的已结算 `(node_id, runtime_id, sequence)`，
/// 数据库唯一约束防止重复使用（`docs/data-model.md` §11）。
///
/// 仅「快照还没过 TTL」不足以授权反复刷新同一份余额——
/// 否则同一份健康快照可以被无限次用来把余额刷回去。
#[tokio::test]
async fn 同一sequence最多支撑一份正额度表() {
    let (store, _dir) = quota_store().await;

    assert!(insert_request(&store, "positive", Some(100), 1).await.is_ok());
    let second = insert_request(&store, "positive", Some(100), 2).await;
    assert!(second.is_err(), "同一 sequence 不得支撑第二份正额度表");

    // 换一个新结算的 sequence 就可以。
    assert!(insert_request(&store, "positive", Some(101), 3).await.is_ok());
}

/// **纯零额度安全表不受该占用限制**，否则 C33 的肯定动作会被自己的门禁挡住：
/// 无新鲜快照时正是要反复收敛零表。
#[tokio::test]
async fn 零额度安全表可以重复收敛() {
    let (store, _dir) = quota_store().await;
    for epoch in 1..=3 {
        assert!(
            insert_request(&store, "zero", None, epoch).await.is_ok(),
            "第 {epoch} 次零表收敛应当被允许"
        );
    }
    // 零表与正额度表共存也不冲突。
    assert!(insert_request(&store, "positive", Some(100), 4).await.is_ok());
}

/// epoch 在 `(node, runtime)` 内单调，同一个值不得分配两次。
#[tokio::test]
async fn 同一epoch不得分配两次() {
    let (store, _dir) = quota_store().await;
    assert!(insert_request(&store, "positive", Some(100), 9).await.is_ok());
    assert!(insert_request(&store, "zero", None, 9).await.is_err(), "epoch 唯一性与表类型无关");
}

/// 正额度表必须有来源 sequence，否则新鲜度门禁形同虚设。
#[tokio::test]
async fn 正额度表缺来源sequence时库层拒绝() {
    let (store, _dir) = quota_store().await;
    assert!(insert_request(&store, "positive", None, 1).await.is_err());
}

const TEST_RUNTIME: &str = "0123456789abcdef0123456789abcdef";

async fn quota_store() -> (Store, tempfile::TempDir) {
    let dir = tempfile::tempdir().unwrap();
    let config = StorageConfig { path: dir.path().join("pm.db"), busy_timeout_ms: 5_000 };
    let store = Store::open(&config).await.unwrap();
    store.init_schema().await.unwrap();

    let mut txn = store.begin_immediate().await.unwrap();
    sqlx::query(
        "INSERT INTO nodes(node_id, provider, address, first_snapshot, agent_spki_sha256, \
         quota_enabled, status, created_at, updated_at) \
         VALUES ('node-example-01', 'plus_v3', 'node.example.com:8443', 'baseline', ?, 1, 'active', ?, ?)",
    )
    .bind("0".repeat(64))
    .bind("2026-09-18T00:00:00Z")
    .bind("2026-09-18T00:00:00Z")
    .execute(txn.conn())
    .await
    .unwrap();
    txn.commit().await.unwrap();
    (store, dir)
}

async fn insert_request(
    store: &Store,
    table_kind: &str,
    source_sequence: Option<u64>,
    epoch: u64,
) -> Result<(), sqlx::Error> {
    let mut txn = store.begin_immediate().await.unwrap();
    let result = sqlx::query(
        "INSERT INTO quota_requests(node_id, runtime_id, epoch, table_kind, source_sequence, \
         request_body, body_sha256, budget_version, setting_revision, prepared_at, result) \
         VALUES ('node-example-01', ?, ?, ?, ?, ?, ?, 1, 1, ?, 'prepared')",
    )
    .bind(TEST_RUNTIME)
    .bind(U64Text::new(epoch).encode())
    .bind(table_kind)
    .bind(source_sequence.map(|s| U64Text::new(s).encode()))
    .bind(Vec::<u8>::new())
    .bind("0".repeat(64))
    .bind("2026-09-18T00:00:00Z")
    .execute(txn.conn())
    .await
    .map(|_| ());
    if result.is_ok() {
        txn.commit().await.unwrap();
    } else {
        txn.rollback().await.unwrap();
    }
    result
}

/// 夹具的生效表必须覆盖节点上**全部**身份，而不只是最后一份表里的那些。
/// 这条是对夹具自身的断言：如果它只回表里的身份，
/// 「少一个人就解除限额」那条用例会因为看不见那个人而碰巧变绿。
#[tokio::test]
async fn 生效表覆盖节点全部身份() {
    let node = FakeNode::start().await;
    let identities: BTreeSet<Identity> = node.identities().await;
    let effective: BTreeSet<Identity> = node.effective_quota().await.into_keys().collect();
    assert_eq!(effective, identities);

    let partial = QuotaRequest::build(
        node.node_id().await,
        node.runtime_id().await,
        1,
        vec![("ss-in".into(), "s1".into(), Quota::Limited(1))],
    )
    .unwrap();
    put(&node, &partial).await.unwrap();
    let effective: BTreeSet<Identity> = node.effective_quota().await.into_keys().collect();
    assert_eq!(effective, identities, "应用一份只含一个身份的表之后，生效表仍应覆盖全部身份");
}
