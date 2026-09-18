//! mTLS 服务端：唯一一条路由，先验签名再执行。
//!
//! 顺序是 **验签 → 解析命令 → 执行**。反过来等于先信任后验证：
//! 一个未经认证的请求体不该有机会决定这个进程去连哪个 socket。
//!
//! 响应也签名，**包括错误响应**。否则中间任何一跳都能凭空造一个
//! 「429」或「502」出来，而采集端对这两个码的处置是完全不同的。

use std::sync::Arc;

use proxy_manager_wire::command::{
    AgentCommand, COMMAND_METHOD, COMMAND_PATH, UPSTREAM_STATUS_HEADER,
};
use proxy_manager_wire::sign::{self, Direction, HmacKey, NonceCache, SignatureHeaders};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::TcpListener;
use tokio::sync::Mutex;
use tokio_rustls::TlsAcceptor;

use crate::commands::{Executor, Outcome};

pub struct Server {
    acceptor: TlsAcceptor,
    executor: Arc<Executor>,
    hmac: HmacKey,
    /// nonce 重放缓存。只在验签通过之后写入，所以它记的都是真实对端用过的值。
    nonces: Arc<Mutex<NonceCache>>,
}

impl Server {
    pub fn new(
        server_config: Arc<rustls::ServerConfig>,
        executor: Executor,
        hmac: HmacKey,
    ) -> Self {
        Server {
            acceptor: TlsAcceptor::from(server_config),
            executor: Arc::new(executor),
            hmac,
            nonces: Arc::new(Mutex::new(NonceCache::default())),
        }
    }

    pub async fn serve(self, listener: TcpListener) {
        let server = Arc::new(self);
        loop {
            let Ok((stream, _peer)) = listener.accept().await else { continue };
            let server = server.clone();
            tokio::spawn(async move {
                if let Err(error) = server.handle(stream).await {
                    // 日志里只记错误，不记签名 header、不记 body。
                    tracing::debug!(error = %error, "连接处理结束");
                }
            });
        }
    }

    async fn handle(&self, stream: tokio::net::TcpStream) -> Result<(), String> {
        // mTLS 握手。客户端证书由精确 leaf verifier 判定；
        // **凭据映射出的节点是授权真相**，body 里的 node_id 只用于交叉校验。
        let mut tls = self.acceptor.accept(stream).await.map_err(|e| e.to_string())?;

        let request = match read_request(&mut tls).await {
            Ok(request) => request,
            Err(error) => {
                self.respond(&mut tls, Outcome::agent_status(400)).await;
                return Err(error);
            }
        };

        let outcome = self.dispatch(&request).await;
        self.respond(&mut tls, outcome).await;
        Ok(())
    }

    async fn dispatch(&self, request: &Request) -> Outcome {
        // 封闭的路由：只有一条。
        if request.path != COMMAND_PATH {
            return Outcome::agent_status(404);
        }
        if request.method != COMMAND_METHOD {
            return Outcome::agent_status(405);
        }

        // 1) 签名 header 的形状。重复或畸形直接拒绝。
        let headers = match SignatureHeaders::from_headers(&request.headers) {
            Ok(headers) => headers,
            Err(error) => {
                tracing::warn!(error = %error, "签名 header 不合法");
                return Outcome::agent_status(401);
            }
        };

        // 2) 验签。**在解析命令之前**——未经认证的请求体不该决定这个进程去连哪个 socket。
        {
            let mut nonces = self.nonces.lock().await;
            if let Err(error) = sign::verify(
                &self.hmac,
                &mut nonces,
                &headers,
                Direction::Request,
                COMMAND_METHOD,
                COMMAND_PATH,
                self.executor.node_id(),
                &request.body,
                "",
                sign::now_secs(),
            ) {
                tracing::warn!(error = %error, "请求验签失败");
                return Outcome::agent_status(401);
            }
        }

        // 3) 解析命令。封闭枚举，不认识的在这里就失败，不进任何 match 分支。
        let command: AgentCommand = match serde_json::from_slice(&request.body) {
            Ok(command) => command,
            Err(error) => {
                tracing::warn!(error = %error, "命令不在封闭集合内");
                return Outcome::agent_status(400);
            }
        };
        tracing::info!(command = command.name(), mutates = command.mutates(), "执行命令");

        self.executor.execute(&command).await
    }

    async fn respond<S>(&self, stream: &mut S, outcome: Outcome)
    where
        S: tokio::io::AsyncWrite + Unpin,
    {
        // 响应签名覆盖上游状态码：否则中间任何一跳都能把 200 改成 429。
        let extra = outcome.upstream_status.map(|s| s.to_string()).unwrap_or_default();
        let signature = sign::sign(
            &self.hmac,
            Direction::Response,
            COMMAND_METHOD,
            COMMAND_PATH,
            self.executor.node_id(),
            &outcome.body,
            &extra,
        );

        let mut head = format!(
            "HTTP/1.1 {} {}\r\nContent-Type: application/octet-stream\r\nContent-Length: {}\r\nConnection: close\r\n",
            outcome.agent_status,
            status_text(outcome.agent_status),
            outcome.body.len()
        );
        if let Some(status) = outcome.upstream_status {
            head.push_str(&format!("{UPSTREAM_STATUS_HEADER}: {status}\r\n"));
        }
        for (name, value) in signature.to_pairs() {
            head.push_str(&format!("{name}: {value}\r\n"));
        }
        head.push_str("\r\n");

        let _ = stream.write_all(head.as_bytes()).await;
        let _ = stream.write_all(&outcome.body).await;
        let _ = stream.flush().await;
        let _ = stream.shutdown().await;
    }
}

impl Outcome {
    fn agent_status(status: u16) -> Self {
        Outcome { agent_status: status, upstream_status: None, body: Vec::new() }
    }
}

struct Request {
    method: String,
    path: String,
    headers: Vec<(String, String)>,
    body: Vec<u8>,
}

const MAX_HEAD: usize = 64 * 1024;
const MAX_BODY: usize = 8 * 1024 * 1024;

async fn read_request<S>(stream: &mut S) -> Result<Request, String>
where
    S: tokio::io::AsyncRead + Unpin,
{
    let mut raw = Vec::new();
    let mut chunk = [0u8; 4096];
    let head_end = loop {
        if let Some(index) = find(&raw, b"\r\n\r\n") {
            break index;
        }
        let read = stream.read(&mut chunk).await.map_err(|e| e.to_string())?;
        if read == 0 {
            return Err("连接在读到完整请求头前就关闭了".into());
        }
        raw.extend_from_slice(&chunk[..read]);
        if raw.len() > MAX_HEAD {
            return Err("请求头过长".into());
        }
    };

    let head = std::str::from_utf8(&raw[..head_end]).map_err(|_| "请求头不是 ASCII")?;
    let mut lines = head.split("\r\n");
    let request_line = lines.next().ok_or("请求为空")?;
    let parts: Vec<&str> = request_line.split(' ').collect();
    if parts.len() != 3 || parts[2] != "HTTP/1.1" {
        return Err(format!("请求行异常：{request_line:?}"));
    }
    let (method, path) = (parts[0].to_string(), parts[1].to_string());
    // 禁 query：与节点侧同一条纪律，不做「忽略未知参数」的宽容处理。
    if path.contains('?') || path.contains('#') {
        return Err("本协议禁 query".into());
    }

    let mut headers = Vec::new();
    let mut content_length = 0usize;
    for line in lines {
        if line.is_empty() {
            continue;
        }
        let (name, value) = line.split_once(':').ok_or("响应头异常")?;
        let name = name.trim().to_ascii_lowercase();
        let value = value.trim().to_string();
        if name == "transfer-encoding" {
            return Err("本协议不支持分块传输".into());
        }
        if name == "content-length" {
            content_length = value.parse().map_err(|_| "Content-Length 不是整数")?;
            if content_length > MAX_BODY {
                return Err("请求体过大".into());
            }
        }
        // **重复的签名 header 不在这里去重**：原样收下，交给
        // SignatureHeaders::from_headers 判重复并拒绝。在这里「取最后一个」
        // 就等于替攻击者做了选择。
        headers.push((name, value));
    }

    let mut body = raw[head_end + 4..].to_vec();
    while body.len() < content_length {
        let read = stream.read(&mut chunk).await.map_err(|e| e.to_string())?;
        if read == 0 {
            return Err("连接在读到完整请求体前就关闭了".into());
        }
        body.extend_from_slice(&chunk[..read]);
    }
    body.truncate(content_length);

    Ok(Request { method, path, headers, body })
}

fn status_text(status: u16) -> &'static str {
    match status {
        200 => "OK",
        400 => "Bad Request",
        401 => "Unauthorized",
        404 => "Not Found",
        405 => "Method Not Allowed",
        429 => "Too Many Requests",
        500 => "Internal Server Error",
        502 => "Bad Gateway",
        _ => "Unknown",
    }
}

fn find(haystack: &[u8], needle: &[u8]) -> Option<usize> {
    haystack.windows(needle.len()).position(|window| window == needle)
}
