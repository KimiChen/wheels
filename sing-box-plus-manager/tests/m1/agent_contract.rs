//! agent 的端到端与故障矩阵。
//!
//! 链路是真的：**主控客户端 → mTLS → 真实 agent 服务端 → 本机 UDS → M0 假节点**。
//! 测试里不重写握手与分发——重写出来的那一份必然比真的宽容，
//! 于是「测过了」和「生产上成立」会变成两件事。

use std::sync::Arc;
use std::time::Duration;

use proxy_manager::agent::AgentClient;
use proxy_manager::collect::snapshot;
use proxy_manager::quota::{Quota, QuotaRequest};
use proxy_manager_agent::{commands::Executor, config::AgentConfig, server::Server};
use proxy_manager_wire::command::AuditFetchRequest;

use super::fake_node::{FakeNode, TransportFault};
use super::harness::{self, DNS_NAME, NODE_ID};

struct Link {
    materials: harness::Materials,
    node: FakeNode,
    client: AgentClient,
    endpoint: String,
    _dir: tempfile::TempDir,
    audit_dir: std::path::PathBuf,
}

impl Link {
    fn controller_dir(&self) -> std::path::PathBuf {
        self.materials.controller_dir.clone()
    }

    fn hmac(&self) -> proxy_manager_wire::sign::HmacKey {
        proxy_manager_wire::tls::load_hmac_key(&self.materials.controller_dir.join("hmac.key"))
            .expect("HMAC key")
    }
}

/// 起一条完整链路。agent 监听回环随机端口。
async fn link() -> Link {
    let materials = harness::materials();
    let node = FakeNode::start().await;
    let dir = tempfile::tempdir().expect("临时目录");
    let audit_dir = dir.path().join("audit");
    std::fs::create_dir_all(&audit_dir).unwrap();

    let config_toml = format!(
        r#"
node_id = "{NODE_ID}"
listen = "127.0.0.1:0"
materials_dir = "{}"
audit_dir = "{}"

[sockets]
snapshot = "{}"
quota = "{}"
"#,
        materials.agent_dir.display(),
        audit_dir.display(),
        node.snapshot_socket.display(),
        node.quota_socket.display(),
    );
    let config_path = dir.path().join("agent.toml");
    std::fs::write(&config_path, config_toml).unwrap();
    let config = AgentConfig::load(&config_path).expect("agent 配置必须能解析");

    let server_config =
        proxy_manager_wire::tls::server_config(&materials.agent_dir).expect("服务端 TLS 配置");
    let hmac = proxy_manager_wire::tls::load_hmac_key(&materials.agent_dir.join("hmac.key"))
        .expect("HMAC key");

    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let endpoint = listener.local_addr().unwrap().to_string();
    let server = Server::new(server_config, Executor::new(config), hmac);
    tokio::spawn(server.serve(listener));

    let client =
        AgentClient::new(NODE_ID, &endpoint, DNS_NAME, &materials.controller_dir).expect("客户端");

    Link { materials, node, client, endpoint, _dir: dir, audit_dir }
}

// ---- 正向：整条链路通 ----

#[tokio::test]
async fn 经agent取到的快照与直连uds逐字节相同() {
    let link = link().await;
    let response = link.client.snapshot().await.expect("采集应当成功");
    assert!(response.upstream_ok());
    assert_eq!(response.upstream_status, Some(200));

    // 字节透传：agent 不解析、不重序列化。主控要对**解压后的原始响应字节**算摘要，
    // 重序列化会改变字段顺序与数字表示。
    let parsed = snapshot::parse(&response.body).expect("必须是一份合法快照");
    assert_eq!(parsed.node_id, NODE_ID);
    assert_eq!(response.body.last(), Some(&b'\n'), "行尾 LF 也要原样带过来");
    assert!(parsed.health.billable());
}

#[tokio::test]
async fn 经agent下发的配额真的改变了节点生效表() {
    let link = link().await;
    let identities: Vec<_> = link.node.identities().await.into_iter().collect();
    let request = QuotaRequest::build(
        link.node.node_id().await,
        link.node.runtime_id().await,
        1,
        identities.iter().cloned().map(|(t, n)| (t, n, Quota::Limited(4_096))),
    )
    .unwrap();

    let response = link.client.quota(request.to_bytes()).await.expect("下发应当成功");
    assert!(response.upstream_ok(), "实际：{response:?}");

    let effective = link.node.effective_quota().await;
    assert_eq!(effective.len(), 3);
    for (identity, quota) in &effective {
        assert_eq!(*quota, Quota::Limited(4_096), "{identity:?}");
    }
}

/// 上游的非 200 必须原样回传，而不是被 agent 折叠成自己的错误。
#[tokio::test]
async fn 上游状态码原样回传且与agent自身状态可区分() {
    let link = link().await;
    link.node.push_fault(TransportFault::Status(429)).await;

    let response = link.client.snapshot().await.expect("这仍是一次成功的往返");
    assert_eq!(response.agent_status, 200, "agent 执行了这条命令");
    assert_eq!(response.upstream_status, Some(429), "上游的 429 要原样带回");
    assert!(response.retryable());
    assert!(!response.upstream_ok(), "429 不得入账");
}

// ---- 单飞 ----

/// 同一节点同一时刻只允许一条 in-flight 采集，第二条直接本地 429。
///
/// agent 的 429 与上游的 429 在 wire 上可区分：**前者没有上游状态码**。
/// 两者对采集端的处置相同（可重试且不得入账），但归因完全不同。
#[tokio::test]
async fn 快照单飞第二条本地429() {
    let link = Arc::new(link().await);
    // 让第一条在途停一会儿。
    link.node.push_fault(TransportFault::Delay(Duration::from_millis(500))).await;

    let first = {
        let link = link.clone();
        tokio::spawn(async move { link.client.snapshot().await })
    };
    // 等第一条确实进到 agent 里再打第二条。
    tokio::time::sleep(Duration::from_millis(120)).await;
    let second = link.client.snapshot().await.expect("第二条也该拿到一个完整响应");

    assert_eq!(second.agent_status, 429, "第二条必须被本地挡掉");
    assert_eq!(second.upstream_status, None, "本地 429 没有上游状态码——这是它与上游 429 的区别");
    assert!(second.retryable());
    assert!(second.body.is_empty());

    let first = first.await.unwrap().expect("第一条应当正常完成");
    assert!(first.upstream_ok());

    // 在途那条结束之后，槽位要放回来。
    let third = link.client.snapshot().await.expect("单飞结束后应当恢复");
    assert!(third.upstream_ok(), "单飞不是一次性的闸门");
}

// ---- 封闭命令集与验签 ----

#[tokio::test]
async fn 不在封闭集合内的命令被拒() {
    let link = link().await;
    let key = link.hmac();

    // 每一条都用**正确的签名**发出去：我们要测的是命令集的封闭性，
    // 不是签名。签名不对的话，agent 会在 401 就停住，永远走不到命令解析。
    for body in [
        &br#"{"command":"exec","argv":["sh"]}"#[..],
        // 这一条曾经**通过**：Snapshot 当时是 unit 变体，多余字段被静默丢弃。
        &br#"{"command":"snapshot","argv":["sh"]}"#[..],
        &br#"{"command":"Snapshot"}"#[..],
        &br#"{"command":"quota"}"#[..],
        &br#"not json at all"#[..],
    ] {
        let headers = harness::sign_request(&key, NODE_ID, body);
        let response = harness::raw_send(&link.endpoint, &link.controller_dir(), &headers, body)
            .await
            .expect("往返本身应当成功");
        assert_eq!(response.status, 400, "{} 必须被 agent 拒绝", String::from_utf8_lossy(body));
        assert_eq!(response.upstream, None, "被拒的命令不得到达上游");
    }

    // 反向证据：正常的 snapshot 走同一条裸路径必须成功。
    // 没有这一条，上面那一组对一个「什么都返回 400」的实现同样是绿的。
    let body = br#"{"command":"snapshot"}"#;
    let headers = harness::sign_request(&key, NODE_ID, body);
    let response =
        harness::raw_send(&link.endpoint, &link.controller_dir(), &headers, body).await.unwrap();
    assert_eq!(response.status, 200);
    assert_eq!(response.upstream, Some(200));
}

/// **传输层 mTLS 只提供通道**（D12）：证书对、HMAC 不对，照样拒绝。
///
/// 这是应用层签名存在的理由之一——若将来在节点侧插入 C18 允许的那种反代，
/// mTLS 会在反代处终结，那时只有应用层签名还能证明这份请求来自主控。
#[tokio::test]
async fn mtls通了但hmac不对仍然被拒() {
    let link = link().await;
    let body = br#"{"command":"snapshot"}"#;

    // 证书仍用本节点的（所以 mTLS 一定握得上），只换一把 HMAC key。
    let wrong = proxy_manager_wire::sign::HmacKey::from_bytes(&[0x5a; 32]).unwrap();
    let headers = harness::sign_request(&wrong, NODE_ID, body);
    let response = harness::raw_send(&link.endpoint, &link.controller_dir(), &headers, body)
        .await
        .expect("mTLS 握手应当成功——被拒的是应用层");
    assert_eq!(response.status, 401, "HMAC 不对必须 401");
    assert_eq!(response.upstream, None, "没有到达上游");
    assert_eq!(link.node.snapshot_requests().await, 0, "节点侧一次都没被碰到");
}

/// 重放同一份签名头必须被拒。
#[tokio::test]
async fn 重放同一份签名被拒() {
    let link = link().await;
    let key = link.hmac();
    let body = br#"{"command":"snapshot"}"#;
    let headers = harness::sign_request(&key, NODE_ID, body);

    let first =
        harness::raw_send(&link.endpoint, &link.controller_dir(), &headers, body).await.unwrap();
    assert_eq!(first.status, 200);

    let replay =
        harness::raw_send(&link.endpoint, &link.controller_dir(), &headers, body).await.unwrap();
    assert_eq!(replay.status, 401, "同一个 nonce 不得被用第二次");
    assert_eq!(link.node.snapshot_requests().await, 1, "重放的那条没有到达上游");
}

/// 改 body 不改签名：签名覆盖 body 摘要，必须被拒。
#[tokio::test]
async fn 签名不覆盖的body改动会被发现() {
    let link = link().await;
    let key = link.hmac();
    let signed_body = br#"{"command":"snapshot"}"#;
    let headers = harness::sign_request(&key, NODE_ID, signed_body);

    // 拿 snapshot 的签名去发一条 quota：这是最值钱的一种伪造，
    // 因为 quota 会改变节点的转发行为。
    let tampered = br#"{"command":"quota","body":"AAAA"}"#;
    let response = harness::raw_send(&link.endpoint, &link.controller_dir(), &headers, tampered)
        .await
        .unwrap();
    assert_eq!(response.status, 401);
    assert_eq!(link.node.quota_requests().await, 0, "伪造的配额命令没有到达上游");
}

/// 封闭的路由：只有一条。
#[tokio::test]
async fn 只有一条路由() {
    let link = link().await;
    let key = link.hmac();
    let body = br#"{"command":"snapshot"}"#;
    let headers = harness::sign_request(&key, NODE_ID, body);
    let response = harness::raw_send_to(
        &link.endpoint,
        &link.controller_dir(),
        &headers,
        body,
        "GET",
        "/v1/command",
    )
    .await
    .unwrap();
    assert_eq!(response.status, 405, "非 POST 必须 405");

    let response = harness::raw_send_to(
        &link.endpoint,
        &link.controller_dir(),
        &headers,
        body,
        "POST",
        "/v1/other",
    )
    .await
    .unwrap();
    assert_eq!(response.status, 404, "别的路径必须 404");
}

// ---- 审计：只读已轮转文件 ----

/// C23：**拒绝读取活动文件**，只读已轮转文件；agent 自身从不 `rm`。
#[tokio::test]
async fn 审计只读已轮转文件() {
    let link = link().await;
    let identity = "u_example_01";
    let active = proxy_manager_wire::audit::active_file_name(identity);
    let rotated = format!("{active}.1758153600");

    std::fs::write(link.audit_dir.join(&active), b"ACTIVE-MUST-NOT-BE-READ\n").unwrap();
    std::fs::write(link.audit_dir.join(&rotated), b"ROTATED-OK\n").unwrap();

    let response = link
        .client
        .audit_fetch(AuditFetchRequest {
            identity: identity.to_string(),
            from: "2026-09-01T00:00:00Z".into(),
            to: "2026-10-01T00:00:00Z".into(),
        })
        .await
        .expect("检索应当成功");
    assert_eq!(response.agent_status, 200);

    let text = String::from_utf8_lossy(&response.body);
    assert!(text.contains("ROTATED-OK"), "已轮转文件要被读到");
    assert!(
        !text.contains("ACTIVE-MUST-NOT-BE-READ"),
        "活动文件绝不能被读：它正在被写，读到的可能是半行"
    );

    // agent 从不删任何东西。
    assert!(link.audit_dir.join(&active).exists(), "活动文件必须还在");
    assert!(link.audit_dir.join(&rotated).exists(), "已轮转文件也不许删");
}

/// 反向证据：换一个身份就读不到别人的文件。
#[tokio::test]
async fn 审计不会读到别的身份的文件() {
    let link = link().await;
    let mine = proxy_manager_wire::audit::active_file_name("u_example_01");
    let theirs = proxy_manager_wire::audit::active_file_name("u_example_02");
    std::fs::write(link.audit_dir.join(format!("{mine}.1")), b"MINE\n").unwrap();
    std::fs::write(link.audit_dir.join(format!("{theirs}.1")), b"THEIRS\n").unwrap();

    let response = link
        .client
        .audit_fetch(AuditFetchRequest {
            identity: "u_example_01".into(),
            from: "2026-09-01T00:00:00Z".into(),
            to: "2026-10-01T00:00:00Z".into(),
        })
        .await
        .unwrap();
    let text = String::from_utf8_lossy(&response.body);
    assert!(text.contains("MINE"));
    assert!(!text.contains("THEIRS"), "不得跨身份返回");
}

/// 路径穿越的身份名编码后穿越不了。
#[tokio::test]
async fn 路径穿越的身份名读不到任何东西() {
    let link = link().await;
    let response = link
        .client
        .audit_fetch(AuditFetchRequest {
            identity: "../../etc/passwd".into(),
            from: "2026-09-01T00:00:00Z".into(),
            to: "2026-10-01T00:00:00Z".into(),
        })
        .await
        .unwrap();
    assert_eq!(response.agent_status, 200);
    assert!(response.body.is_empty(), "编码之后它只是一个普通的文件名，什么都匹配不到");
}

/// 未配置 audit_dir 时直接拒绝，而不是去猜一个目录。
#[tokio::test]
async fn 未配置审计目录时拒绝检索() {
    let materials = harness::materials();
    let node = FakeNode::start().await;
    let dir = tempfile::tempdir().unwrap();
    let config_toml = format!(
        "node_id = \"{NODE_ID}\"\nlisten = \"127.0.0.1:0\"\nmaterials_dir = \"{}\"\n\n[sockets]\nsnapshot = \"{}\"\nquota = \"{}\"\n",
        materials.agent_dir.display(),
        node.snapshot_socket.display(),
        node.quota_socket.display(),
    );
    let config_path = dir.path().join("agent.toml");
    std::fs::write(&config_path, config_toml).unwrap();
    let config = AgentConfig::load(&config_path).unwrap();

    let server_config = proxy_manager_wire::tls::server_config(&materials.agent_dir).unwrap();
    let hmac =
        proxy_manager_wire::tls::load_hmac_key(&materials.agent_dir.join("hmac.key")).unwrap();
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let endpoint = listener.local_addr().unwrap().to_string();
    tokio::spawn(Server::new(server_config, Executor::new(config), hmac).serve(listener));

    let client = AgentClient::new(NODE_ID, &endpoint, DNS_NAME, &materials.controller_dir).unwrap();
    let response = client
        .audit_fetch(AuditFetchRequest {
            identity: "u_example_01".into(),
            from: "a".into(),
            to: "b".into(),
        })
        .await
        .unwrap();
    assert_eq!(response.agent_status, 404);
}

// ---- 配置门禁 ----

/// C18 在 agent 侧的落点：它自己就该拒绝一份把两档权限合并掉的配置。
#[test]
fn agent拒绝把两个socket合并的配置() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("agent.toml");
    std::fs::write(
        &path,
        "node_id = \"n\"\nlisten = \"127.0.0.1:0\"\nmaterials_dir = \"/tmp\"\n\n[sockets]\nsnapshot = \"/run/a.sock\"\nquota = \"/run/a.sock\"\n",
    )
    .unwrap();
    let error = AgentConfig::load(&path).unwrap_err();
    assert!(error.to_string().contains("C18"), "实际：{error}");
}

#[test]
fn agent对未知字段失败关闭() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("agent.toml");
    std::fs::write(
        &path,
        "node_id = \"n\"\nlisten = \"127.0.0.1:0\"\nmaterials_dir = \"/tmp\"\nunknown = 1\n\n[sockets]\nsnapshot = \"/run/a.sock\"\nquota = \"/run/b.sock\"\n",
    )
    .unwrap();
    assert!(AgentConfig::load(&path).is_err());
}
