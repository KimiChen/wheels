//! HTTP/1.1-over-Unix-stream 客户端：单请求单响应、严格解析、全程有界。
//!
//! 放在共享 crate 里是因为它有两个使用者：**生产上只有 agent**（主控绝不直连节点 UDS，
//! README §4.1），而 M0 的假节点夹具刻意绕过 agent 直连 UDS，好让故障矩阵能分清
//! 「协议错了」还是「证书错了」。两处必须是同一个解析器——
//! 一个比另一个宽容的传输层会让夹具验过的东西在生产上不成立。
//!
//! 与节点侧 `scripts/http_unix.py` 同构。严格的理由是它读的是计费数据：
//! **宽容解析在这里等于「把半截响应当成一次成功采集」**，而那正是漏账的来源。
//!
//! 特别是 `Content-Length` 与实际长度必须相等。截断的 JSON 有可能仍然解析成功
//! （例如尾部对象被砍掉），那会让采集端把一份残缺快照当成完整快照入账。

use std::path::Path;
use std::time::Duration;

use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::UnixStream;

pub const DEFAULT_TIMEOUT: Duration = Duration::from_secs(10);
const MAX_STATUS_LINE: usize = 8 * 1024;
const MAX_HEADER_BYTES: usize = 64 * 1024;
const MAX_BODY_BYTES: usize = 32 * 1024 * 1024;

/// 一次采集失败。**调用方不得把任何一种失败当成空快照。**
#[derive(Debug, thiserror::Error)]
pub enum Failure {
    #[error("连接 {path} 失败：{source}")]
    Connect {
        path: String,
        #[source]
        source: std::io::Error,
    },
    #[error("读取响应超时")]
    Timeout,
    /// 连接被重置。按 C7 与 429 同等对待：**可重试且不得入账**。
    /// 节点侧对超限请求的 `lingeringDrain` 消不掉这个窗口，所以它是预期事件而非异常。
    #[error("连接被重置：{0}")]
    ConnectionReset(String),
    #[error("读写失败：{0}")]
    Io(String),
    #[error("协议错误：{0}")]
    Protocol(String),
}

impl Failure {
    /// C7：429 与连接被重置可重试且不得入账；其余非 200 一律拒绝入账。
    /// 这里只判传输层；状态码由调用方按 [`Response::status`] 判。
    pub fn retryable(&self) -> bool {
        matches!(self, Failure::ConnectionReset(_) | Failure::Timeout)
    }
}

#[derive(Debug, Clone)]
pub struct Response {
    pub status: u16,
    pub headers: Vec<(String, String)>,
    pub body: Vec<u8>,
}

impl Response {
    pub fn header(&self, name: &str) -> Option<&str> {
        self.headers.iter().find(|(key, _)| key == name).map(|(_, value)| value.as_str())
    }

    /// 429 可重试且不得入账（C7）。
    pub fn retryable(&self) -> bool {
        self.status == 429
    }
}

pub struct UdsClient {
    timeout: Duration,
}

impl Default for UdsClient {
    fn default() -> Self {
        UdsClient { timeout: DEFAULT_TIMEOUT }
    }
}

impl UdsClient {
    pub fn with_timeout(timeout: Duration) -> Self {
        UdsClient { timeout }
    }

    pub async fn get(&self, socket: &Path, target: &str) -> Result<Response, Failure> {
        self.request(socket, "GET", target, None).await
    }

    pub async fn put_json(
        &self,
        socket: &Path,
        target: &str,
        body: &[u8],
    ) -> Result<Response, Failure> {
        self.request(socket, "PUT", target, Some(body)).await
    }

    pub async fn request(
        &self,
        socket: &Path,
        method: &str,
        target: &str,
        body: Option<&[u8]>,
    ) -> Result<Response, Failure> {
        // 本协议禁 query：节点侧对带 query 的请求一律 400，不做「忽略未知参数」的宽容处理。
        if target.contains('?') || target.contains('#') {
            return Err(Failure::Protocol("本协议禁 query：请求目标不得含 ? 或 #".into()));
        }

        let fut = self.exchange(socket, method, target, body);
        match tokio::time::timeout(self.timeout, fut).await {
            Ok(result) => result,
            Err(_) => Err(Failure::Timeout),
        }
    }

    async fn exchange(
        &self,
        socket: &Path,
        method: &str,
        target: &str,
        body: Option<&[u8]>,
    ) -> Result<Response, Failure> {
        let mut stream = UnixStream::connect(socket)
            .await
            .map_err(|source| Failure::Connect { path: socket.display().to_string(), source })?;

        let mut head =
            format!("{method} {target} HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n");
        if let Some(body) = body {
            head.push_str("Content-Type: application/json\r\n");
            head.push_str(&format!("Content-Length: {}\r\n", body.len()));
        }
        head.push_str("\r\n");

        if let Err(error) = stream.write_all(head.as_bytes()).await {
            return Err(classify_io(error));
        }
        if let Some(body) = body {
            // 服务端可能已经用 413 回绝并关闭连接；此时仍应把它已写回的响应读完，
            // 所以写失败不立刻返回。
            let _ = stream.write_all(body).await;
        }
        let _ = stream.flush().await;

        let mut raw = Vec::new();
        let mut chunk = vec![0u8; 64 * 1024];
        loop {
            match stream.read(&mut chunk).await {
                Ok(0) => break,
                Ok(n) => {
                    raw.extend_from_slice(&chunk[..n]);
                    if raw.len() > MAX_BODY_BYTES + MAX_HEADER_BYTES {
                        return Err(Failure::Protocol("响应超过大小上限".into()));
                    }
                }
                Err(error) => return Err(classify_io(error)),
            }
        }
        parse_response(&raw)
    }
}

fn classify_io(error: std::io::Error) -> Failure {
    match error.kind() {
        std::io::ErrorKind::ConnectionReset | std::io::ErrorKind::BrokenPipe => {
            Failure::ConnectionReset(error.to_string())
        }
        std::io::ErrorKind::TimedOut | std::io::ErrorKind::WouldBlock => Failure::Timeout,
        _ => Failure::Io(error.to_string()),
    }
}

fn parse_response(raw: &[u8]) -> Result<Response, Failure> {
    let separator = find_subsequence(raw, b"\r\n\r\n").ok_or_else(|| {
        // 这一支就是「响应丢失」：连接被中途关闭，头体分隔都没读到。
        Failure::Protocol("响应缺少头体分隔（连接可能被中途关闭）".into())
    })?;
    let head = &raw[..separator];
    let body = &raw[separator + 4..];

    if head.len() > MAX_HEADER_BYTES {
        return Err(Failure::Protocol("响应头过长".into()));
    }
    let head =
        std::str::from_utf8(head).map_err(|_| Failure::Protocol("响应头不是 ASCII".into()))?;
    let mut lines = head.split("\r\n");
    let status_line = lines.next().ok_or_else(|| Failure::Protocol("响应为空".into()))?;
    if status_line.len() > MAX_STATUS_LINE {
        return Err(Failure::Protocol("状态行过长".into()));
    }
    let mut parts = status_line.splitn(3, ' ');
    let version = parts.next().unwrap_or_default();
    if version != "HTTP/1.1" {
        return Err(Failure::Protocol(format!("状态行异常：{status_line:?}")));
    }
    let status: u16 = parts
        .next()
        .and_then(|code| code.parse().ok())
        .ok_or_else(|| Failure::Protocol(format!("状态码异常：{status_line:?}")))?;

    let mut headers = Vec::new();
    for line in lines {
        if line.is_empty() {
            continue;
        }
        let (name, value) = line
            .split_once(':')
            .ok_or_else(|| Failure::Protocol(format!("响应头异常：{line:?}")))?;
        headers.push((name.trim().to_ascii_lowercase(), value.trim().to_string()));
    }

    if headers.iter().any(|(name, _)| name == "transfer-encoding") {
        return Err(Failure::Protocol("本协议不支持分块传输".into()));
    }
    let declared = headers
        .iter()
        .find(|(name, _)| name == "content-length")
        .map(|(_, value)| value.as_str())
        .ok_or_else(|| Failure::Protocol("响应缺少 Content-Length".into()))?;
    let declared: usize =
        declared.parse().map_err(|_| Failure::Protocol("Content-Length 不是整数".into()))?;
    if declared != body.len() {
        return Err(Failure::Protocol(format!(
            "Content-Length 与实际长度不符：{declared} != {}",
            body.len()
        )));
    }

    Ok(Response { status, headers, body: body.to_vec() })
}

fn find_subsequence(haystack: &[u8], needle: &[u8]) -> Option<usize> {
    haystack.windows(needle.len()).position(|window| window == needle)
}
