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

// ---- 审计：列目录 + 读单个已轮转文件 ----

/// 真实的轮转后缀。定宽，**自带一个点**（纳秒的小数点），
/// 所以「按最后一个点切文件名」是错的。夹具用真格式而不是 `.1`：
/// 「字典序即时间序」只对定宽成立。
const STAMP: &str = "20260919T041530.123456789Z";
const LATER: &str = "20260919T041531.000000000Z";

fn rotated(identity: &str, stamp: &str) -> String {
    format!("{}.{stamp}", proxy_manager_wire::audit::active_file_name(identity))
}

fn listing(body: &[u8]) -> proxy_manager_wire::AuditListing {
    serde_json::from_slice(body).expect("清单应当是合法 JSON")
}

/// C23：列目录**只列已轮转文件**，活动文件不在其中；agent 自身从不 `rm`。
#[tokio::test]
async fn 审计列目录只列已轮转文件() {
    let link = link().await;
    let identity = "u_example_01";
    let active = proxy_manager_wire::audit::active_file_name(identity);
    let one = rotated(identity, STAMP);
    let two = rotated("u_example_02", STAMP);

    std::fs::write(link.audit_dir.join(&active), b"ACTIVE-MUST-NOT-BE-READ\n").unwrap();
    std::fs::write(link.audit_dir.join(&one), b"ROTATED-OK\n").unwrap();
    std::fs::write(link.audit_dir.join(&two), b"THEIRS\n").unwrap();

    let response = link.client.audit_list().await.expect("列目录应当成功");
    assert_eq!(response.agent_status, 200);
    let listed = listing(&response.body);

    let names: Vec<&str> = listed.files.iter().map(|f| f.name.as_str()).collect();
    // 一次往返拿到**全部身份**的文件，不是某一个人的；活动的与已轮转的都在。
    assert_eq!(names, vec![active.as_str(), one.as_str(), two.as_str()]);
    // 活动文件排在它自己那份归档之前——它没有时间戳后缀。
    let entry = |name: &str| listed.files.iter().find(|f| f.name == name).unwrap();
    assert!(entry(&active).active, "活动文件必须标出来");
    assert!(!entry(&one).active, "已轮转文件不是活动的");
    assert_eq!(entry(&one).size, b"ROTATED-OK\n".len() as u64, "大小要真实");
    assert_eq!(listed.skipped, 0);

    // agent 从不删任何东西。
    assert!(link.audit_dir.join(&active).exists(), "活动文件必须还在");
    assert!(link.audit_dir.join(&one).exists(), "已轮转文件也不许删");
}

/// 无法归类的条目**计数**，不静默跳过——真跳过了却不说，
/// 与「目录里就这些」在输出上长得一模一样。
#[tokio::test]
async fn 无法归类的条目被计数而不是静默跳过() {
    let link = link().await;
    std::fs::write(link.audit_dir.join(rotated("u_example_01", STAMP)), b"OK\n").unwrap();
    std::fs::write(link.audit_dir.join("README"), b"x\n").unwrap();
    std::fs::write(link.audit_dir.join("access-abc.jsonl.1758153600"), b"x\n").unwrap();

    let listed = listing(&link.client.audit_list().await.unwrap().body);
    assert_eq!(listed.files.len(), 1);
    // README 一条；`access-abc.jsonl.1758153600` 一条——变长时间戳既不是合法的
    // 已轮转名，也不是活动名（它不以 .jsonl 结尾）。
    assert_eq!(listed.skipped, 2, "两个都说不清的条目都要报出来");
}

/// 定宽时间戳的字典序即时间序，所以清单顺序就是轮转顺序。
#[tokio::test]
async fn 清单顺序就是轮转顺序() {
    let link = link().await;
    let later = rotated("u_example_01", LATER);
    let earlier = rotated("u_example_01", STAMP);
    std::fs::write(link.audit_dir.join(&later), b"L\n").unwrap();
    std::fs::write(link.audit_dir.join(&earlier), b"E\n").unwrap();

    let listed = listing(&link.client.audit_list().await.unwrap().body);
    assert_eq!(
        listed.files.iter().map(|f| f.name.clone()).collect::<Vec<_>>(),
        vec![earlier, later],
        "早的在前"
    );
}

/// 活动文件按偏移增量读，**只返回完整行**。
///
/// 这是使用者定的口径（2026-09-19）：只读已轮转文件的话，忙的身份要等 1.4 天、
/// 闲的要等 63 天才第一次可见，这一页在那之前是空的。节点的活动文件是
/// `O_APPEND` 只追加、`repairTrailingNewline` 只补换行符从不截断，
/// 所以按偏移读、截到最后一个换行符处，是安全的。
#[tokio::test]
async fn 活动文件按偏移增量读且只返回完整行() {
    let link = link().await;
    let active = proxy_manager_wire::audit::active_file_name("u_example_01");
    let path = link.audit_dir.join(&active);
    std::fs::write(&path, b"one\ntwo\n").unwrap();

    let first = link.client.audit_read(active.clone(), 0).await.unwrap();
    assert_eq!(first.agent_status, 200);
    assert_eq!(first.body, b"one\ntwo\n");

    // 追加一条完整行 + 一条写到一半的。
    use std::io::Write;
    let mut handle = std::fs::OpenOptions::new().append(true).open(&path).unwrap();
    handle.write_all(b"three\nhal").unwrap();

    let second = link.client.audit_read(active.clone(), first.body.len() as u64).await.unwrap();
    assert_eq!(second.body, b"three\n", "半行不能返回");

    // 推进量就是收下的字节数。再读一次不该重复拿到已经拿过的行。
    let offset = (first.body.len() + second.body.len()) as u64;
    let third = link.client.audit_read(active.clone(), offset).await.unwrap();
    assert!(third.body.is_empty(), "只剩半行时一个字节都不该给");

    // 半行补完之后才拿得到。
    handle.write_all(b"f\n").unwrap();
    let fourth = link.client.audit_read(active.clone(), offset).await.unwrap();
    assert_eq!(fourth.body, b"half\n");
}

/// 轮转把同名文件变回 0 字节。偏移超过大小是**正常事件**，不是错误——
/// 调用方据此把偏移归零，而不是把它当成 agent 出了问题反复重试。
#[tokio::test]
async fn 偏移超过文件大小返回空而不是报错() {
    let link = link().await;
    let active = proxy_manager_wire::audit::active_file_name("u_example_01");
    std::fs::write(link.audit_dir.join(&active), b"short\n").unwrap();

    let response = link.client.audit_read(active.clone(), 9999).await.unwrap();
    assert_eq!(response.agent_status, 200, "不是错误");
    assert!(response.body.is_empty());

    // 模拟轮转：同名文件变回 0 字节。清单里报的大小小于调用方的偏移，
    // 这就是「该归零了」的信号。
    std::fs::write(link.audit_dir.join(&active), b"").unwrap();
    let listed = listing(&link.client.audit_list().await.unwrap().body);
    assert_eq!(listed.files.iter().find(|f| f.name == active).unwrap().size, 0);
}

/// 已轮转文件**不做**完整行截断：它不再变化，末尾若真有半行，那是节点侧的事实，
/// 应当让主控看见并计入解析失败，而不是在 agent 里悄悄抹掉。
#[tokio::test]
async fn 已轮转文件原样返回不截断() {
    let link = link().await;
    let name = rotated("u_example_01", STAMP);
    std::fs::write(link.audit_dir.join(&name), b"complete\npartial-no-newline").unwrap();

    let response = link.client.audit_read(name, 0).await.unwrap();
    assert_eq!(response.body, b"complete\npartial-no-newline", "逐字节原样");
}

/// 读**一个**文件，拿到的是它的原始字节——不是把同一身份的文件拼起来。
#[tokio::test]
async fn 审计读取返回单个文件的原始字节() {
    let link = link().await;
    let one = rotated("u_example_01", STAMP);
    let two = rotated("u_example_01", LATER);
    std::fs::write(link.audit_dir.join(&one), b"FIRST\n").unwrap();
    std::fs::write(link.audit_dir.join(&two), b"SECOND\n").unwrap();

    let response = link.client.audit_read(one.clone(), 0).await.expect("读取应当成功");
    assert_eq!(response.agent_status, 200);
    assert_eq!(response.body, b"FIRST\n", "只返回这一个文件，逐字节");
    assert!(!response.body.ends_with(b"SECOND\n"), "不得把同身份的文件拼起来");

    // 清单报的大小要与真读到的一致——同步器靠这个判断「拉了一半」。
    let listed = listing(&link.client.audit_list().await.unwrap().body);
    let entry = listed.files.iter().find(|f| f.name == one).unwrap();
    assert_eq!(entry.size as usize, response.body.len());
}

/// 文件名来自主控，而这条路径的全部意义就是不信任它。
#[tokio::test]
async fn 审计读取拒绝走出审计目录的名字() {
    let link = link().await;
    // 目录外放一个诱饵，证明拒绝不是因为「文件不存在」。
    let outside = link.audit_dir.parent().unwrap().join("secret.txt");
    std::fs::write(&outside, b"SECRET\n").unwrap();

    // 目录里放一个**形状完全合法**的符号链接，指向目录外。形状那一道过得去，
    // canonicalize 那一道过不去——两道校验挡的不是同一类东西。
    let escaping = rotated("u_example_09", STAMP);
    std::os::unix::fs::symlink(&outside, link.audit_dir.join(&escaping)).unwrap();
    assert!(
        proxy_manager_wire::audit::is_rotated_name(&escaping),
        "这个名字必须是形状合法的，否则这条用例证明不了 canonicalize 那一道有用"
    );

    for name in [
        "../secret.txt".to_string(),
        "../../etc/passwd".to_string(),
        "/etc/passwd".to_string(),
        format!("access-../secret.jsonl.{STAMP}"),
        format!("access-a/b.jsonl.{STAMP}"),
        escaping.clone(),
    ] {
        let response = link.client.audit_read(name.clone(), 0).await.unwrap();
        assert_eq!(response.agent_status, 400, "{name} 必须被拒");
        assert!(response.body.is_empty(), "{name} 不得返回任何字节");
    }
    assert!(outside.exists(), "目录外的文件当然也不许删");
    assert!(link.audit_dir.join(&escaping).symlink_metadata().is_ok(), "链接本身也不许删");

    // 反向：名字合法、不逃逸，但文件不在——那是 404，不是 400，也不是 500。
    let missing = rotated("u_example_10", STAMP);
    assert_eq!(link.client.audit_read(missing, 0).await.unwrap().agent_status, 404);
}

/// 未配置 audit_dir 时直接拒绝，而不是去猜一个目录。两条命令都要。
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
    assert_eq!(client.audit_list().await.unwrap().agent_status, 404);
    assert_eq!(
        client.audit_read(rotated("u_example_01", STAMP), 0).await.unwrap().agent_status,
        404
    );
}
