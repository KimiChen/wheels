//! M1 测试的公用脚手架：起一个离线 workspace，并做一次**真实握手**。
//!
//! 「真实握手」是 `docs/threat-model.md` §2.2 那条护栏的字面要求：
//! **调整签发器或证书 Subject 后，必须重跑真实组件的正负矩阵，不能只做链验证。**
//! 链验证通过不代表那个实现的选择逻辑还成立——所以这里跑的是 rustls 本身。

#![allow(dead_code)]

use std::path::{Path, PathBuf};
use std::sync::Arc;

use proxy_manager::pki::{self, issue, Workspace};
use rustls::pki_types::ServerName;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::{TcpListener, TcpStream};
use tokio_rustls::{TlsAcceptor, TlsConnector};

pub const NODE_ID: &str = "node-example-01";
pub const GENERATION: &str = "2026-09-18-a";
pub const DNS_NAME: &str = "node-01.example.com";

pub struct Materials {
    pub dir: tempfile::TempDir,
    pub workspace: Workspace,
    pub agent_dir: PathBuf,
    pub controller_dir: PathBuf,
    pub server_leaf_spki: String,
    pub client_leaf_spki: String,
}

pub fn materials() -> Materials {
    let dir = tempfile::tempdir().expect("临时目录");
    let workspace = Workspace::new(dir.path().join("pki"));
    workspace.init(&issue::Validity::default()).expect("建 workspace");
    let generation = workspace
        .issue_node(NODE_ID, GENERATION, DNS_NAME, &issue::Validity::default())
        .expect("签发节点材料");
    Materials {
        agent_dir: generation.agent_dir.clone(),
        controller_dir: generation.controller_dir.clone(),
        server_leaf_spki: generation.server_leaf_spki.clone(),
        client_leaf_spki: generation.client_leaf_spki.clone(),
        dir,
        workspace,
    }
}

/// 一次完整的 TLS 握手 + 一来一回。
///
/// 必须真的读写一次：TLS 1.3 下客户端证书在服务端被拒时，
/// 客户端的 `connect()` 常常**先返回成功**，错误要到第一次读写才浮出来。
/// 只断言 `connect()` 成功，等于把一整类拒绝当成了通过。
pub async fn handshake(agent_dir: &Path, controller_dir: &Path) -> Result<Vec<u8>, String> {
    let server_config = pki::server_config(agent_dir).map_err(|e| format!("服务端配置：{e}"))?;
    let client_config =
        pki::client_config(controller_dir).map_err(|e| format!("客户端配置：{e}"))?;

    let listener = TcpListener::bind("127.0.0.1:0").await.map_err(|e| e.to_string())?;
    let address = listener.local_addr().map_err(|e| e.to_string())?;
    let acceptor = TlsAcceptor::from(server_config);

    let server = tokio::spawn(async move {
        let (stream, _) = listener.accept().await.map_err(|e| e.to_string())?;
        let mut tls = acceptor.accept(stream).await.map_err(|e| format!("服务端握手：{e}"))?;
        let mut buffer = [0u8; 16];
        let read = tls.read(&mut buffer).await.map_err(|e| format!("服务端读：{e}"))?;
        tls.write_all(&buffer[..read]).await.map_err(|e| format!("服务端写：{e}"))?;
        tls.flush().await.map_err(|e| e.to_string())?;
        Ok::<_, String>(())
    });

    let connector = TlsConnector::from(client_config);
    let stream = TcpStream::connect(address).await.map_err(|e| e.to_string())?;
    let name = ServerName::try_from(DNS_NAME).map_err(|e| e.to_string())?.to_owned();
    let client_result = async {
        let mut tls =
            connector.connect(name, stream).await.map_err(|e| format!("客户端握手：{e}"))?;
        tls.write_all(b"ping").await.map_err(|e| format!("客户端写：{e}"))?;
        tls.flush().await.map_err(|e| e.to_string())?;
        let mut buffer = vec![0u8; 16];
        let read = tls.read(&mut buffer).await.map_err(|e| format!("客户端读：{e}"))?;
        buffer.truncate(read);
        Ok::<_, String>(buffer)
    }
    .await;

    let server_result = tokio::time::timeout(std::time::Duration::from_secs(5), server).await;
    match (&client_result, server_result) {
        (Ok(_), Ok(Ok(Ok(())))) => {}
        (Ok(_), other) => return Err(format!("客户端成功但服务端失败：{other:?}")),
        _ => {}
    }
    client_result
}

/// 把一个 PEM 文件整体换掉，用于负向用例。
pub fn replace(path: &Path, contents: &str) {
    std::fs::write(path, contents).expect("覆盖 PEM");
}

pub fn read(path: &Path) -> String {
    std::fs::read_to_string(path).expect("读取 PEM")
}

/// 用**同一个 intermediate** 再签一张 leaf。
///
/// 这是负向矩阵最关键的入口：许多 TLS 实现只验证链，于是这张新 leaf 会被接受。
/// 精确 leaf 授信必须让它**握不上手**。
pub fn reissue_client_leaf(materials: &Materials) -> (String, String) {
    let generation_dir = materials.workspace.generation_dir(NODE_ID, GENERATION);
    let ca = pki::load_ca(
        &read(&generation_dir.join("client-intermediate.pem")),
        &read(&generation_dir.join("client-intermediate.key")),
    )
    .expect("重建 client intermediate");
    let leaf = issue::issue_client_leaf(&ca, NODE_ID, &issue::Validity::default(), 424_242)
        .expect("用同一 intermediate 再签一张 client leaf");
    (leaf.cert_pem(), leaf.key_pem())
}

pub fn reissue_server_leaf(materials: &Materials) -> (String, String) {
    let generation_dir = materials.workspace.generation_dir(NODE_ID, GENERATION);
    let ca = pki::load_ca(
        &read(&generation_dir.join("server-intermediate.pem")),
        &read(&generation_dir.join("server-intermediate.key")),
    )
    .expect("重建 server intermediate");
    let leaf =
        issue::issue_server_leaf(&ca, NODE_ID, DNS_NAME, &issue::Validity::default(), 424_243)
            .expect("用同一 intermediate 再签一张 server leaf");
    (leaf.cert_pem(), leaf.key_pem())
}

pub fn provider() -> Arc<rustls::crypto::CryptoProvider> {
    proxy_manager_wire::verify::ring_provider()
}

// ---- 裸请求：绕过 AgentClient 的类型约束，直接往 wire 上写 ----
//
// 为什么需要它：`AgentClient::send` 只接受 `AgentCommand`，于是**主控侧**的封闭性
// 让畸形命令根本发不出去。那是好事，但它也意味着**agent 侧**的封闭性没被测到。
// 两道都要有，所以这里绕开客户端，直接构造 wire 上的字节。

use proxy_manager_wire::command::{COMMAND_METHOD, COMMAND_PATH, UPSTREAM_STATUS_HEADER};
use proxy_manager_wire::sign::{self, Direction, HmacKey, SignatureHeaders};

#[derive(Debug)]
pub struct RawResponse {
    pub status: u16,
    pub upstream: Option<u16>,
    pub body: Vec<u8>,
}

/// 用给定的 key 与 body 签一份请求头。
pub fn sign_request(key: &HmacKey, node_id: &str, body: &[u8]) -> SignatureHeaders {
    sign::sign(key, Direction::Request, COMMAND_METHOD, COMMAND_PATH, node_id, body, "")
}

/// 把一份**已签名**的请求原样发给 agent，并且**不验响应签名**——
/// 我们要观察的正是 401/400 这类拒绝，验签会把它们挡在断言之前。
pub async fn raw_send(
    endpoint: &str,
    controller_dir: &Path,
    headers: &SignatureHeaders,
    body: &[u8],
) -> Result<RawResponse, String> {
    raw_send_to(endpoint, controller_dir, headers, body, COMMAND_METHOD, COMMAND_PATH).await
}

/// 同上，但方法与路径可以指定——用来验证「只有一条路由」。
pub async fn raw_send_to(
    endpoint: &str,
    controller_dir: &Path,
    headers: &SignatureHeaders,
    body: &[u8],
    method: &str,
    path: &str,
) -> Result<RawResponse, String> {
    let config =
        proxy_manager_wire::tls::client_config(controller_dir).map_err(|e| e.to_string())?;
    let connector = TlsConnector::from(config);
    let stream = TcpStream::connect(endpoint).await.map_err(|e| e.to_string())?;
    let name = ServerName::try_from(DNS_NAME).map_err(|e| e.to_string())?.to_owned();
    let mut tls = connector.connect(name, stream).await.map_err(|e| e.to_string())?;

    let mut head = format!(
        "{method} {path} HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\
         Content-Type: application/json\r\nContent-Length: {}\r\n",
        body.len()
    );
    for (name, value) in headers.to_pairs() {
        head.push_str(&format!("{name}: {value}\r\n"));
    }
    head.push_str("\r\n");
    tls.write_all(head.as_bytes()).await.map_err(|e| e.to_string())?;
    tls.write_all(body).await.map_err(|e| e.to_string())?;
    tls.flush().await.map_err(|e| e.to_string())?;

    let mut raw = Vec::new();
    tls.read_to_end(&mut raw).await.map_err(|e| e.to_string())?;

    let separator = raw.windows(4).position(|w| w == b"\r\n\r\n").ok_or("响应缺少头体分隔")?;
    let head = std::str::from_utf8(&raw[..separator]).map_err(|_| "响应头不是 ASCII")?;
    let body = raw[separator + 4..].to_vec();
    let mut lines = head.split("\r\n");
    let status_line = lines.next().ok_or("响应为空")?;
    let status: u16 =
        status_line.split(' ').nth(1).and_then(|code| code.parse().ok()).ok_or("状态码异常")?;
    let mut upstream = None;
    for line in lines {
        if let Some((name, value)) = line.split_once(':') {
            if name.trim().eq_ignore_ascii_case(UPSTREAM_STATUS_HEADER) {
                upstream = value.trim().parse::<u16>().ok();
            }
        }
    }
    Ok(RawResponse { status, upstream, body })
}
