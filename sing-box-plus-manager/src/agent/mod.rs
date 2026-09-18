//! 主控侧的 agent 客户端：经 mTLS + 应用层 HMAC 连到节点上的 `proxy-manager-agent`。
//!
//! **主控绝不直连节点 UDS**（README §4.1）。这个模块是主控到数据面的唯一通路。
//!
//! 响应的处理顺序是 **先验签名，再解析内容**（`docs/threat-model.md` §3）。
//! 顺序反了等于先信任后验证。这里把它做成结构性的：[`AgentClient::send`] 只有在
//! 验签通过之后才返回 body，调用方拿不到未经验证的字节。

use std::path::Path;

use proxy_manager_wire::command::{
    AgentCommand, AuditFetchRequest, COMMAND_METHOD, COMMAND_PATH, UPSTREAM_STATUS_HEADER,
};
use proxy_manager_wire::sign::{self, Direction, HmacKey, NonceCache, SignatureHeaders};
use rustls::pki_types::ServerName;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::TcpStream;
use tokio::sync::Mutex;
use tokio_rustls::TlsConnector;

use crate::error::{Error, Result};

/// 一次命令的结果。
#[derive(Debug, Clone)]
pub struct AgentResponse {
    /// agent 自身的状态：这条命令**有没有被执行**。
    pub agent_status: u16,
    /// 上游（节点本机 UDS）的状态码。agent 级失败时为 `None`。
    ///
    /// 两者不能混：agent 的 429（单飞挡掉）与上游的 429（节点并发上限）
    /// 对采集端的处置相同——可重试且不得入账——但归因完全不同。
    pub upstream_status: Option<u16>,
    /// **已验签**的响应体，字节透传自上游。
    pub body: Vec<u8>,
    /// agent 签这份响应的时刻（Unix 秒）。
    ///
    /// 用它当 `collected_at`（`docs/data-model.md` §6）。它进 HMAC 的 canonical 编码，
    /// 所以**链路上改不动**——这比让主控拿自己的墙钟当采集时刻强得多，
    /// 后者把「节点采到的时刻」和「主控收到的时刻」混成了同一个数，
    /// 于是 C27 要查的节点时钟偏移永远是 0。
    pub agent_timestamp: u64,
}

impl AgentResponse {
    /// C7：429 与连接被重置**可重试且不得入账**。
    ///
    /// 三种来源都算：
    /// - agent 自己的 429（单飞挡掉）；
    /// - 上游的 429（节点并发上限）；
    /// - agent 的 502——它没能连上本机 UDS。节点可能正在重启或 socket 一时不可达，
    ///   这与「连接被重置」是同一类事件。
    ///
    /// **不包括** 401 / 400 / 404 / 405：那些是配置或授信不对，重试多少次都一样，
    /// 退避只会让告警来得更晚。
    pub fn retryable(&self) -> bool {
        matches!(self.agent_status, 429 | 502 | 503) || self.upstream_status == Some(429)
    }

    /// 上游返回 200 才算拿到了可入账的输入。
    pub fn upstream_ok(&self) -> bool {
        self.agent_status == 200 && self.upstream_status == Some(200)
    }
}

pub struct AgentClient {
    node_id: String,
    endpoint: String,
    server_name: ServerName<'static>,
    connector: TlsConnector,
    hmac: HmacKey,
    nonces: Mutex<NonceCache>,
}

impl AgentClient {
    /// `materials` 是离线签发出来的 `deployment/controller/` 目录。
    pub fn new(
        node_id: impl Into<String>,
        endpoint: impl Into<String>,
        dns_name: &str,
        materials: &Path,
    ) -> Result<Self> {
        let config = proxy_manager_wire::tls::client_config(materials)?;
        let hmac = proxy_manager_wire::tls::load_hmac_key(&materials.join("hmac.key"))?;
        let server_name = ServerName::try_from(dns_name.to_string())
            .map_err(|e| Error::Pki(format!("{dns_name:?} 不是合法的服务器名：{e}")))?;
        Ok(AgentClient {
            node_id: node_id.into(),
            endpoint: endpoint.into(),
            server_name,
            connector: TlsConnector::from(config),
            hmac,
            nonces: Mutex::new(NonceCache::default()),
        })
    }

    pub async fn snapshot(&self) -> Result<AgentResponse> {
        self.send(&AgentCommand::Snapshot {}).await
    }

    pub async fn quota(&self, body: Vec<u8>) -> Result<AgentResponse> {
        self.send(&AgentCommand::Quota { body }).await
    }

    pub async fn audit_fetch(&self, request: AuditFetchRequest) -> Result<AgentResponse> {
        self.send(&AgentCommand::AuditFetch(request)).await
    }

    pub async fn send(&self, command: &AgentCommand) -> Result<AgentResponse> {
        let body = serde_json::to_vec(command)
            .map_err(|e| Error::Agent(format!("序列化命令失败：{e}")))?;
        let signature = sign::sign(
            &self.hmac,
            Direction::Request,
            COMMAND_METHOD,
            COMMAND_PATH,
            &self.node_id,
            &body,
            "",
        );

        let mut head = format!(
            "{COMMAND_METHOD} {COMMAND_PATH} HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\
             Content-Type: application/json\r\nContent-Length: {}\r\n",
            body.len()
        );
        for (name, value) in signature.to_pairs() {
            head.push_str(&format!("{name}: {value}\r\n"));
        }
        head.push_str("\r\n");

        let stream = TcpStream::connect(&self.endpoint)
            .await
            .map_err(|e| Error::Agent(format!("连接 {} 失败：{e}", self.endpoint)))?;
        let mut tls = self
            .connector
            .connect(self.server_name.clone(), stream)
            .await
            .map_err(|e| Error::Agent(format!("mTLS 握手失败：{e}")))?;

        tls.write_all(head.as_bytes()).await.map_err(|e| Error::Agent(format!("发送失败：{e}")))?;
        tls.write_all(&body).await.map_err(|e| Error::Agent(format!("发送失败：{e}")))?;
        tls.flush().await.map_err(|e| Error::Agent(format!("发送失败：{e}")))?;

        let mut raw = Vec::new();
        tls.read_to_end(&mut raw).await.map_err(|e| Error::Agent(format!("读取响应失败：{e}")))?;

        self.parse_verified(&raw).await
    }

    /// 解析响应。**验签在返回 body 之前**。
    async fn parse_verified(&self, raw: &[u8]) -> Result<AgentResponse> {
        let separator = raw
            .windows(4)
            .position(|window| window == b"\r\n\r\n")
            .ok_or_else(|| Error::Agent("响应缺少头体分隔（连接可能被中途关闭）".into()))?;
        let head = std::str::from_utf8(&raw[..separator])
            .map_err(|_| Error::Agent("响应头不是 ASCII".into()))?;
        let body = raw[separator + 4..].to_vec();

        let mut lines = head.split("\r\n");
        let status_line = lines.next().ok_or_else(|| Error::Agent("响应为空".into()))?;
        let mut parts = status_line.splitn(3, ' ');
        if parts.next() != Some("HTTP/1.1") {
            return Err(Error::Agent(format!("状态行异常：{status_line:?}")));
        }
        let agent_status: u16 = parts
            .next()
            .and_then(|code| code.parse().ok())
            .ok_or_else(|| Error::Agent(format!("状态码异常：{status_line:?}")))?;

        let mut headers = Vec::new();
        let mut declared_length: Option<usize> = None;
        let mut upstream_status = None;
        for line in lines {
            if line.is_empty() {
                continue;
            }
            let (name, value) = line
                .split_once(':')
                .ok_or_else(|| Error::Agent(format!("响应头异常：{line:?}")))?;
            let name = name.trim().to_ascii_lowercase();
            let value = value.trim().to_string();
            if name == "content-length" {
                declared_length = Some(
                    value.parse().map_err(|_| Error::Agent("Content-Length 不是整数".into()))?,
                );
            }
            if name == UPSTREAM_STATUS_HEADER {
                upstream_status = Some(
                    value.parse::<u16>().map_err(|_| Error::Agent("上游状态码不是整数".into()))?,
                );
            }
            headers.push((name, value));
        }

        // 长度不符必须先失败：截断的 body 有可能仍然验签失败，
        // 但也有可能（长度为 0 时）意外通过，届时得到的是一份空快照。
        match declared_length {
            Some(declared) if declared != body.len() => {
                return Err(Error::Agent(format!(
                    "Content-Length 与实际长度不符：{declared} != {}",
                    body.len()
                )))
            }
            None => return Err(Error::Agent("响应缺少 Content-Length".into())),
            _ => {}
        }

        let signature = SignatureHeaders::from_headers(&headers)
            .map_err(|e| Error::Agent(format!("响应签名 header 不合法：{e}")))?;
        let extra = upstream_status.map(|s| s.to_string()).unwrap_or_default();
        {
            let mut nonces = self.nonces.lock().await;
            sign::verify(
                &self.hmac,
                &mut nonces,
                &signature,
                Direction::Response,
                COMMAND_METHOD,
                COMMAND_PATH,
                &self.node_id,
                &body,
                &extra,
                sign::now_secs(),
            )
            .map_err(|e| Error::Agent(format!("响应验签失败：{e}")))?;
        }

        // 到这里才把 body 交出去。
        Ok(AgentResponse {
            agent_status,
            upstream_status,
            body,
            agent_timestamp: signature.timestamp,
        })
    }
}
