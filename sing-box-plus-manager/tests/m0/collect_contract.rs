//! 采集侧故障矩阵（README §8）。
//!
//! 每一条注入都对照 §2.1 的一个约束编号。**每条关键断言都配了正向用例**：
//! 合法样例必须被接受。只断言「畸形被拒绝」是不够的——一个把所有输入都拒绝掉的
//! 实现同样能让那一半全绿，而它在生产上表现为「一份账都记不下来」。

use std::time::Duration;

use proxy_manager::collect::snapshot::{self, Rejected};
use proxy_manager::collect::{UdsClient, SNAPSHOT_PATH};

use super::fake_node::{FakeNode, TransportFault};
use super::fixtures;

async fn fetch(
    node: &FakeNode,
) -> Result<proxy_manager::collect::Response, proxy_manager::collect::Failure> {
    UdsClient::default().get(&node.snapshot_socket, SNAPSHOT_PATH).await
}

// ---- 正向：合法样例被接受 ----

#[tokio::test]
async fn 合法样例经真实uds往返后被接受() {
    let node = FakeNode::start().await;
    let response = fetch(&node).await.expect("采集应当成功");
    assert_eq!(response.status, 200);

    let parsed = snapshot::parse(&response.body).expect("上游样例必须被接受");
    assert_eq!(parsed.schema_version, snapshot::SCHEMA_VERSION);
    assert_eq!(parsed.node_id, node.node_id().await);
    assert_eq!(parsed.runtime_id, node.runtime_id().await);
    assert!(parsed.health.billable(), "基线样例的 health 四位全假，可入账");
    assert!(!parsed.health.audit_gap());
    assert_eq!(parsed.inbounds.len(), 2);
}

/// 与节点侧 `marshalJSONLine` 一致：紧凑 JSON + 行尾 LF，且 `Content-Length` 含那个换行。
#[tokio::test]
async fn 响应是以lf结尾的紧凑json且长度自洽() {
    let node = FakeNode::start().await;
    let response = fetch(&node).await.expect("采集应当成功");
    assert_eq!(response.body.last(), Some(&b'\n'));
    let declared: usize = response
        .header("content-length")
        .expect("必须有 Content-Length")
        .parse()
        .expect("Content-Length 是整数");
    assert_eq!(declared, response.body.len());
}

/// 每次 `/v3/snapshot` 都推进 sequence；`/healthz` 不推进。
#[tokio::test]
async fn 快照推进sequence而healthz不推进() {
    let node = FakeNode::start().await;
    let first = snapshot::parse(&fetch(&node).await.unwrap().body).unwrap();
    let _ = UdsClient::default().get(&node.snapshot_socket, "/healthz").await.unwrap();
    let second = snapshot::parse(&fetch(&node).await.unwrap().body).unwrap();
    assert_eq!(second.sequence, first.sequence + 1, "healthz 不应推进 sequence");
}

// ---- C10：路径与版本号成对硬校验 ----

#[tokio::test]
async fn schema_version错配整份拒绝() {
    for wrong in [1u32, 2, 4] {
        let node = FakeNode::start().await;
        node.set_schema_version(wrong).await;
        let response = fetch(&node).await.expect("传输层仍然成功");
        let error = snapshot::parse(&response.body).expect_err("版本错配必须整份拒绝");
        assert_eq!(
            error,
            Rejected::SchemaVersion { expected: 3, actual: wrong },
            "拒绝成因必须指向版本，而不是别的字段"
        );
    }
}

/// 主控这一侧再加一道路径校验：节点已写明 `/v1`、`/v2` 恒 404，
/// 但主控可能被指到一个根本不是 `sing-box-plus` 的端点，那时 404 不会出现。
#[tokio::test]
async fn 历史路径恒404() {
    let node = FakeNode::start().await;
    for path in ["/v1/snapshot", "/v2/snapshot"] {
        let response = UdsClient::default().get(&node.snapshot_socket, path).await.unwrap();
        assert_eq!(response.status, 404, "{path} 必须 404");
    }
}

// ---- C4：health 是恰好四位的闭集 ----

#[tokio::test]
async fn health多一个键整份拒绝() {
    let node = FakeNode::start().await;
    node.add_health_key("disk_pressure", true).await;
    let response = fetch(&node).await.unwrap();
    let error = snapshot::parse(&response.body).expect_err("多一个键必须整份拒绝");
    assert_eq!(error, Rejected::HealthExtraKey { extra: vec!["disk_pressure".into()] });
}

#[tokio::test]
async fn health少一个键整份拒绝() {
    for missing in snapshot::HEALTH_KEYS {
        let node = FakeNode::start().await;
        node.remove_health_key(missing).await;
        let response = fetch(&node).await.unwrap();
        let error = snapshot::parse(&response.body).expect_err("少一个键必须整份拒绝");
        assert_eq!(
            error,
            Rejected::HealthMissingKey { missing: vec![missing.to_string()] },
            "少的是 {missing}"
        );
    }
}

/// 反向证据：上面两条不是「凡是 health 变了就拒绝」。
/// 把四位里的任意一位从 false 改成 true，仍然是合法快照。
#[tokio::test]
async fn health四位齐全时置位不影响解析() {
    for key in snapshot::HEALTH_KEYS {
        let node = FakeNode::start().await;
        node.set_health_bit(key, true).await;
        let response = fetch(&node).await.unwrap();
        snapshot::parse(&response.body)
            .unwrap_or_else(|e| panic!("置位 {key} 不该让解析失败：{e}"));
    }
}

/// 前三位任一为真即不可入账；`audit_dropped` 告警但**照常入账**。
///
/// 把审计丢弃算进入账判据，等于让磁盘写满连带停掉计费——
/// 这是节点侧刻意不做的，主控也不要做回来。
#[tokio::test]
async fn 前三位不可入账而audit_dropped照常入账() {
    for key in snapshot::BILLING_HEALTH_KEYS {
        let node = FakeNode::start().await;
        node.set_health_bit(key, true).await;
        let parsed = snapshot::parse(&fetch(&node).await.unwrap().body).unwrap();
        assert!(!parsed.health.billable(), "{key} 为真时不可入账");
    }

    let node = FakeNode::start().await;
    node.set_health_bit("audit_dropped", true).await;
    let parsed = snapshot::parse(&fetch(&node).await.unwrap().body).unwrap();
    assert!(parsed.health.billable(), "audit_dropped 为真时照常入账");
    assert!(parsed.health.audit_gap(), "但要走告警路径");
}

// ---- C7：可重试且不得入账 ----

#[tokio::test]
async fn http429可重试且不得入账() {
    let node = FakeNode::start().await;
    node.push_fault(TransportFault::Status(429)).await;
    let response = fetch(&node).await.expect("429 仍是一个完整响应");
    assert_eq!(response.status, 429);
    assert!(response.retryable(), "429 可重试");
    // 关键：不得把 429 的错误体当成快照去解析入账。
    assert!(snapshot::parse(&response.body).is_err(), "错误体不是快照");
}

#[tokio::test]
async fn 响应丢失不得当成空快照() {
    let node = FakeNode::start().await;
    node.push_fault(TransportFault::CloseWithoutResponse).await;
    let failure = fetch(&node).await.expect_err("一个字节都没收到必须失败");
    let message = failure.to_string();
    assert!(message.contains("头体分隔"), "应当指出响应被中途关闭，实际：{message}");
    // 节点侧**已经处理过**这次请求：sequence 已推进。
    // 这正是「结果不明」——不是「没发生」。
    assert_eq!(node.snapshot_requests().await, 1);
}

#[tokio::test]
async fn 延迟到达超过客户端超时时失败但节点已处理() {
    let node = FakeNode::start().await;
    node.push_fault(TransportFault::Delay(Duration::from_millis(400))).await;
    let client = UdsClient::with_timeout(Duration::from_millis(80));
    let failure = client.get(&node.snapshot_socket, SNAPSHOT_PATH).await.expect_err("超时必须失败");
    assert!(failure.retryable(), "超时可重试");
    assert_eq!(node.snapshot_requests().await, 1, "节点侧已经处理过");
}

// ---- Content-Length 与截断 ----

#[tokio::test]
async fn content_length不符拒绝入账() {
    for delta in [-1i64, 1, 64] {
        let node = FakeNode::start().await;
        node.push_fault(TransportFault::ContentLengthDelta(delta)).await;
        let failure = fetch(&node).await.expect_err("长度不符必须失败");
        assert!(
            failure.to_string().contains("Content-Length"),
            "应当指出长度不符，实际：{failure}"
        );
    }
}

/// 截断的 JSON **有可能仍然解析成功**（例如尾部对象被砍掉），
/// 所以长度校验必须在解析之前，不能指望解析器兜住。
#[tokio::test]
async fn json截断在长度校验处就被挡住() {
    let node = FakeNode::start().await;
    let full = fetch(&node).await.unwrap().body.len();
    node.push_fault(TransportFault::TruncateBody(full / 2)).await;
    let failure = fetch(&node).await.expect_err("截断必须失败");
    assert!(failure.to_string().contains("Content-Length"), "实际：{failure}");
}

// ---- C3：u64 精确解析 ----

#[tokio::test]
async fn u64边界值精确解析不经浮点() {
    let boundaries: [u64; 4] = [(1 << 63) - 1, 1 << 63, u64::MAX, u64::MAX - 1];
    for value in boundaries {
        let node = FakeNode::start().await;
        node.set_counter("vless-entry-01", "u_example_01", "tcp_uplink_bytes", value).await;
        let body = fetch(&node).await.unwrap().body;
        // wire 上必须是那串十进制数字本身，不是 9.22e18 之类的近似。
        assert!(
            String::from_utf8_lossy(&body).contains(&value.to_string()),
            "wire 上应当出现精确的 {value}"
        );
        let parsed = snapshot::parse(&body).unwrap();
        let user = &parsed.inbounds[1].users[0];
        assert_eq!(user.name, "u_example_01");
        assert_eq!(user.tcp_uplink_bytes, value, "解析后必须逐位相等");
    }
}

/// 浮点冒充计数必须被拒绝。Rust 的 `u64` 目标类型让 `1e19` 直接反序列化失败——
/// 这正是不经 `serde_json::Value` / `Number` 中转的理由。
#[test]
fn 浮点与布尔冒充计数被拒绝() {
    let mut base = fixtures::base_snapshot();
    base["inbounds"][1]["users"][0]["tcp_uplink_bytes"] = serde_json::json!(1e19);
    let body = serde_json::to_vec(&base).unwrap();
    assert!(matches!(snapshot::parse(&body), Err(Rejected::Malformed(_))));

    let mut base = fixtures::base_snapshot();
    base["inbounds"][1]["users"][0]["tcp_uplink_bytes"] = serde_json::json!(true);
    let body = serde_json::to_vec(&base).unwrap();
    assert!(matches!(snapshot::parse(&body), Err(Rejected::Malformed(_))));
}

// ---- 结构性校验 ----

#[test]
fn 未按ascii字节升序排列整份拒绝() {
    let mut base = fixtures::base_snapshot();
    let inbounds = base["inbounds"].as_array_mut().unwrap();
    inbounds.swap(0, 1);
    let error = snapshot::parse(&serde_json::to_vec(&base).unwrap()).unwrap_err();
    assert_eq!(error, Rejected::Ordering { container: "inbounds", key: "(tag, generation)" });

    let mut base = fixtures::base_snapshot();
    let users = base["inbounds"][1]["users"].as_array_mut().unwrap();
    users.swap(0, 1);
    let error = snapshot::parse(&serde_json::to_vec(&base).unwrap()).unwrap_err();
    assert_eq!(error, Rejected::Ordering { container: "users", key: "(name, generation)" });
}

/// 同一快照内出现重复完整键 → 整份拒绝（`docs/data-model.md` §4 第 4 步）。
/// 严格升序同时管住了重复：相等也不放行。
#[test]
fn 同一快照内重复完整键整份拒绝() {
    let mut base = fixtures::base_snapshot();
    let duplicate = base["inbounds"][1]["users"][0].clone();
    base["inbounds"][1]["users"].as_array_mut().unwrap().insert(1, duplicate);
    let error = snapshot::parse(&serde_json::to_vec(&base).unwrap()).unwrap_err();
    assert_eq!(error, Rejected::Ordering { container: "users", key: "(name, generation)" });
}

#[test]
fn 信封字段的下界被校验() {
    for field in ["started_at_unix_ms", "sequence"] {
        let mut base = fixtures::base_snapshot();
        base[field] = serde_json::json!(0u64);
        let error = snapshot::parse(&serde_json::to_vec(&base).unwrap()).unwrap_err();
        assert_eq!(error, Rejected::MustBePositive { field });
    }

    let mut base = fixtures::base_snapshot();
    base["runtime_id"] = serde_json::json!("NOT-HEX");
    assert!(matches!(
        snapshot::parse(&serde_json::to_vec(&base).unwrap()),
        Err(Rejected::RuntimeId(_))
    ));

    let mut base = fixtures::base_snapshot();
    base["inbounds"][0]["listen_port"] = serde_json::json!(0u64);
    assert_eq!(
        snapshot::parse(&serde_json::to_vec(&base).unwrap()).unwrap_err(),
        Rejected::ListenPort(0)
    );
}

/// C6：session gauge 是瞬时值。这里只断言它被解析出来且不参与任何累计校验——
/// 「不进基线、不参与差分」的断言属于 M1 的结算层。
#[tokio::test]
async fn session_gauge被解析但不属于计数字段() {
    let node = FakeNode::start().await;
    let parsed = snapshot::parse(&fetch(&node).await.unwrap().body).unwrap();
    assert_eq!(parsed.inbounds[1].tcp_sessions, 3);
    assert_eq!(parsed.inbounds[1].udp_sessions, 1);
    // 四向计数与 gauge 是两组字段，gauge 不在 counter_fields 里。
    assert!(!fixtures::fixtures().counter_fields.iter().any(|f| f.contains("sessions")));
}

/// C25 的注入点。
///
/// 「已观察过的 lineage 无故消失 → 整份拒绝」需要与**此前观察到的集合**比对，
/// 那是账本状态，属于 M1。M0 这一条只证明夹具造得出这个输入，
/// 并且它在**结构上**是合法的——所以 M1 的那条断言不能靠解析器顺带满足。
#[tokio::test]
async fn lineage消失在结构上合法而c25属于结算层() {
    let node = FakeNode::start().await;
    let before = snapshot::parse(&fetch(&node).await.unwrap().body).unwrap();
    assert_eq!(before.inbounds[1].users.len(), 2);

    node.remove_identity("vless-entry-01", "u_example_02").await;
    let after = snapshot::parse(&fetch(&node).await.unwrap().body)
        .expect("消失的 lineage 在结构上仍是一份合法快照");
    assert_eq!(after.inbounds[1].users.len(), 1);
}
