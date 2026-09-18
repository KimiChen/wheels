//! 一个单用途的外呼 HTTPS 客户端：**一个端点、一个方法、一种 content-type、有界响应体**。
//!
//! 为什么手写而不是引一个 HTTP 客户端：需求就这么大，而 `src/agent/mod.rs`
//! 已经有一份手写 HTTP/1.1 的现成口味（含强制 `Content-Length` 校验）。
//! 引 reqwest 会连带 hyper-util / http-body-util / tower-http 一整串依赖，
//! 为一次外呼不值当，也与 Cargo.toml 顶上「不预先声明用不到的 crate」相左。
//!
//! 与 agent 那套 TLS 的差别要说清楚，它们**不能互换**：
//! agent 走的是私有 CA + 逐字节比对 leaf DER + 强制 mTLS + 不校验主机名；
//! 这里走的是公网 WebPKI 根 + 真正的主机名校验 + 无客户端证书。
//! 把前者拿来连 `account.xmpaoyou.com` 会在第一步就失败（对方 leaf 不在授信表里）。
//!
//! 硬化项逐条对应 README §4.9 加固 5：
//! **限定 HTTPS、禁跟随重定向、响应体上限、逐字段限长并拒绝控制字符**。
//! 前三条在本模块，第四条在 [`super::client`] 解析响应时做。

use std::sync::Arc;
use std::time::Duration;

use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::TcpStream;

/// 响应体上限。超过即失败，**不截断**——截断的 JSON 解析出来可能仍然合法。
pub const MAX_RESPONSE_BYTES: usize = 1024 * 1024;
/// 响应头上限。防的是一个只发 header 不发 body 的对端把内存吃光。
const MAX_HEAD_BYTES: usize = 64 * 1024;
/// 连接超时。总超时由调用方给，连接这一段取两者较小值。
const MAX_CONNECT_SECS: u64 = 3;

#[derive(Debug)]
pub struct HttpsResponse {
    pub status: u16,
    pub content_type: String,
    pub body: Vec<u8>,
}

#[derive(Debug, thiserror::Error)]
pub enum HttpsError {
    #[error("URL 不合法：{0}")]
    BadUrl(String),
    #[error("传输失败：{0}")]
    Transport(String),
    #[error("响应不合法：{0}")]
    BadResponse(String),
    #[error("响应超过 {MAX_RESPONSE_BYTES} 字节上限")]
    TooLarge,
    #[error("超时")]
    Timeout,
}

/// 拆开的 `https://host[:port]/path` 三段。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Endpoint {
    pub host: String,
    pub port: u16,
    /// 含前导 `/` 的请求目标（path + 可选 query）。
    pub target: String,
}

impl Endpoint {
    /// 解析一个 URL。**只接受 https**，且拒绝 userinfo。
    ///
    /// 不引 url crate：这里只要能拆一个我们自己配置里的固定地址。
    /// 拒绝 userinfo 是因为 `https://evil.com@real.com/` 这种写法在肉眼读配置时
    /// 极易看反，而它在这里没有任何正当用途。
    pub fn parse(url: &str) -> Result<Self, HttpsError> {
        let rest = url
            .strip_prefix("https://")
            .ok_or_else(|| HttpsError::BadUrl("必须是 https://".into()))?;
        // 先把 query 与 fragment 切掉，再找路径分隔符。
        // 不这么做，`https://host?x=1` 会把整段 `host?x=1` 当成 authority，
        // 于是 `rsplit_once(':')` 之后拿到一个荒唐的主机名——而它在启动校验时
        // 是「合法」的（`Endpoint::parse` 只要求非空），要到第一个人点登录
        // 才在连接阶段炸掉。
        let cut = rest.find(['/', '?', '#']);
        let (authority, target) = match cut {
            Some(index) if rest.as_bytes()[index] == b'/' => {
                (&rest[..index], rest[index..].to_string())
            }
            // `?`/`#` 直接跟在 authority 后面：路径是空的，补 `/`。
            Some(index) => (&rest[..index], format!("/{}", &rest[index..])),
            None => (rest, "/".to_string()),
        };
        if authority.contains('@') {
            return Err(HttpsError::BadUrl("不接受 URL 里的 userinfo".into()));
        }
        if authority.is_empty() {
            return Err(HttpsError::BadUrl("缺少主机名".into()));
        }
        // 只处理 host[:port]。IPv6 字面量在这里没有正当用途（对端是个域名），
        // 与其解析一半不如明确拒绝。
        if authority.contains('[') || authority.contains(']') {
            return Err(HttpsError::BadUrl("不接受 IPv6 字面量".into()));
        }
        let (host, port) = match authority.rsplit_once(':') {
            Some((host, port)) => {
                (host, port.parse::<u16>().map_err(|_| HttpsError::BadUrl("端口不是整数".into()))?)
            }
            None => (authority, 443u16),
        };
        if host.is_empty() || port == 0 {
            return Err(HttpsError::BadUrl("主机名或端口为空".into()));
        }
        Ok(Endpoint { host: host.to_string(), port, target })
    }
}

/// 公网 WebPKI 的 rustls 客户端配置。
///
/// 全栈单一 ring provider（D9）——这里也必须显式给 provider，
/// 否则 rustls 会去找进程级默认，而本项目从不安装它。
fn client_config() -> Arc<rustls::ClientConfig> {
    // 建一次就够：RootCertStore 要把上百张根证书解析成 TrustAnchor，
    // 而这条路径每次登录都会走。缓存也顺带保证了同一进程内的信任集合一致。
    static CONFIG: std::sync::OnceLock<Arc<rustls::ClientConfig>> = std::sync::OnceLock::new();
    CONFIG.get_or_init(build_client_config).clone()
}

fn build_client_config() -> Arc<rustls::ClientConfig> {
    let mut roots = rustls::RootCertStore::empty();
    roots.extend(webpki_roots::TLS_SERVER_ROOTS.iter().cloned());
    let config =
        rustls::ClientConfig::builder_with_provider(proxy_manager_wire::verify::ring_provider())
            .with_safe_default_protocol_versions()
            .expect("ring provider 支持默认协议版本集")
            .with_root_certificates(roots)
            // 无客户端证书：我们对上游是匿名的，token 本身就是全部凭据。
            .with_no_client_auth();
    Arc::new(config)
}

/// `POST` 一份 `application/x-www-form-urlencoded` 表单。
///
/// **不跟随重定向**：3xx 会原样返回给调用方，由它按「状态不是 200」拒绝。
/// 跟随重定向意味着把 token 发到一个配置里没写过的地址去。
pub async fn post_form(
    endpoint: &Endpoint,
    body: String,
    timeout: Duration,
) -> Result<HttpsResponse, HttpsError> {
    match tokio::time::timeout(timeout, post_form_inner(endpoint, body)).await {
        Ok(result) => result,
        Err(_) => Err(HttpsError::Timeout),
    }
}

async fn post_form_inner(endpoint: &Endpoint, body: String) -> Result<HttpsResponse, HttpsError> {
    let server_name = rustls_pki_types::ServerName::try_from(endpoint.host.clone())
        .map_err(|e| HttpsError::BadUrl(format!("{:?} 不是合法主机名：{e}", endpoint.host)))?;
    let connector = tokio_rustls::TlsConnector::from(client_config());

    let connect_timeout = Duration::from_secs(MAX_CONNECT_SECS);
    let tcp = tokio::time::timeout(
        connect_timeout,
        TcpStream::connect((endpoint.host.as_str(), endpoint.port)),
    )
    .await
    .map_err(|_| HttpsError::Timeout)?
    .map_err(|e| HttpsError::Transport(format!("连接失败：{e}")))?;
    let mut tls = connector
        .connect(server_name, tcp)
        .await
        .map_err(|e| HttpsError::Transport(format!("TLS 握手失败：{e}")))?;

    // `Connection: close` 让我们可以读到 EOF 为止；但对端仍可能用 chunked，
    // 所以下面两种编码都要认。
    let head = format!(
        "POST {} HTTP/1.1\r\nHost: {}\r\nConnection: close\r\nAccept: application/json\r\n\
         Content-Type: application/x-www-form-urlencoded\r\nContent-Length: {}\r\n\r\n",
        endpoint.target,
        host_header(endpoint),
        body.len()
    );
    tls.write_all(head.as_bytes())
        .await
        .map_err(|e| HttpsError::Transport(format!("发送失败：{e}")))?;
    tls.write_all(body.as_bytes())
        .await
        .map_err(|e| HttpsError::Transport(format!("发送失败：{e}")))?;
    tls.flush().await.map_err(|e| HttpsError::Transport(format!("发送失败：{e}")))?;

    // 有界读：上限是 head + body 两个上限之和，多一个字节都不收。
    let mut raw = Vec::new();
    let mut chunk = [0u8; 8192];
    loop {
        let read = tls
            .read(&mut chunk)
            .await
            .map_err(|e| HttpsError::Transport(format!("读取失败：{e}")))?;
        if read == 0 {
            break;
        }
        raw.extend_from_slice(&chunk[..read]);
        if raw.len() > MAX_HEAD_BYTES + MAX_RESPONSE_BYTES {
            return Err(HttpsError::TooLarge);
        }
    }
    parse_response(&raw)
}

/// `Host` 头。非 443 端口要带上端口号。
fn host_header(endpoint: &Endpoint) -> String {
    if endpoint.port == 443 {
        endpoint.host.clone()
    } else {
        format!("{}:{}", endpoint.host, endpoint.port)
    }
}

/// 解析一份 HTTP/1.1 响应。
///
/// 两种体编码都要认：`Content-Length` 与 `Transfer-Encoding: chunked`。
/// 只认前者是个生产地雷——`Connection: close` **不阻止**对端用 chunked，
/// 而那时 body 会带着十六进制长度行被当成 JSON 解析，报出来的是「JSON 不合法」，
/// 与「token 无效」在日志里长得一模一样。
pub fn parse_response(raw: &[u8]) -> Result<HttpsResponse, HttpsError> {
    let separator = raw
        .windows(4)
        .position(|window| window == b"\r\n\r\n")
        .ok_or_else(|| HttpsError::BadResponse("缺少头体分隔（连接可能被中途关闭）".into()))?;
    if separator > MAX_HEAD_BYTES {
        return Err(HttpsError::TooLarge);
    }
    let head = std::str::from_utf8(&raw[..separator])
        .map_err(|_| HttpsError::BadResponse("响应头不是 UTF-8".into()))?;
    let rest = &raw[separator + 4..];

    let mut lines = head.split("\r\n");
    let status_line = lines.next().ok_or_else(|| HttpsError::BadResponse("响应为空".into()))?;
    let mut parts = status_line.splitn(3, ' ');
    let version = parts.next().unwrap_or_default();
    if version != "HTTP/1.1" && version != "HTTP/1.0" {
        return Err(HttpsError::BadResponse(format!("状态行异常：{status_line:?}")));
    }
    let status: u16 = parts
        .next()
        .and_then(|code| code.parse().ok())
        .ok_or_else(|| HttpsError::BadResponse(format!("状态码异常：{status_line:?}")))?;

    let mut content_type = String::new();
    let mut declared_length: Option<usize> = None;
    let mut chunked = false;
    for line in lines {
        if line.is_empty() {
            continue;
        }
        let Some((name, value)) = line.split_once(':') else {
            return Err(HttpsError::BadResponse(format!("响应头异常：{line:?}")));
        };
        let name = name.trim().to_ascii_lowercase();
        let value = value.trim();
        match name.as_str() {
            "content-type" => content_type = value.to_ascii_lowercase(),
            "content-length" => {
                declared_length = Some(
                    value
                        .parse()
                        .map_err(|_| HttpsError::BadResponse("Content-Length 不是整数".into()))?,
                )
            }
            // 只认 `chunked` 本身。任何别的传输编码（gzip、compress）我们没请求过，
            // 出现即说明对端在做我们没预期的事，宁可失败也不猜。
            "transfer-encoding" => {
                if value.eq_ignore_ascii_case("chunked") {
                    chunked = true;
                } else {
                    return Err(HttpsError::BadResponse(format!("不支持的传输编码：{value:?}")));
                }
            }
            _ => {}
        }
    }

    let body = if chunked {
        decode_chunked(rest)?
    } else {
        match declared_length {
            // 长度不符必须先失败：截断的 body 有可能仍然解析成一个合法 JSON。
            Some(declared) if declared != rest.len() => {
                return Err(HttpsError::BadResponse(format!(
                    "Content-Length 与实际长度不符：{declared} != {}",
                    rest.len()
                )))
            }
            // 没有 Content-Length 也没有 chunked：靠 `Connection: close` 读到 EOF。
            // 这是 HTTP/1.1 允许的形态，但它与「连接被中途掐断」不可区分，
            // 所以只在响应确实完整落地的情况下接受，且仍受上限约束。
            _ => rest.to_vec(),
        }
    };
    if body.len() > MAX_RESPONSE_BYTES {
        return Err(HttpsError::TooLarge);
    }
    Ok(HttpsResponse { status, content_type, body })
}

/// 解 `Transfer-Encoding: chunked`。
///
/// 只做必要的那一半：长度行（允许 `;` 后的扩展）、CRLF、数据、CRLF，
/// 直到长度为 0。trailer 一律忽略——我们不读任何 trailer 头。
fn decode_chunked(mut rest: &[u8]) -> Result<Vec<u8>, HttpsError> {
    let mut body = Vec::new();
    loop {
        let line_end = rest
            .windows(2)
            .position(|w| w == b"\r\n")
            .ok_or_else(|| HttpsError::BadResponse("chunked：缺少长度行".into()))?;
        let line = std::str::from_utf8(&rest[..line_end])
            .map_err(|_| HttpsError::BadResponse("chunked：长度行不是 UTF-8".into()))?;
        // `1a;ext=1` 也是合法的长度行。
        let hex = line.split(';').next().unwrap_or("").trim();
        let size = usize::from_str_radix(hex, 16)
            .map_err(|_| HttpsError::BadResponse(format!("chunked：长度行异常 {line:?}")))?;
        rest = &rest[line_end + 2..];
        if size == 0 {
            return Ok(body);
        }
        // **先判上限，再做任何加法。** `size` 完全由对端给定，
        // 一个 `ffffffffffffffff` 的长度行就是 `usize::MAX`：
        // release 下 `body.len() + size` 与 `size + 2` 都会回绕成小数，
        // 两道长度检查一起失效，随后 `&rest[..size]` 越界 panic。
        // 那是一个约 30 字节的载荷就能触发的远端拒绝服务。
        if size > MAX_RESPONSE_BYTES || body.len() > MAX_RESPONSE_BYTES - size {
            return Err(HttpsError::TooLarge);
        }
        // 到这里 size 已经 ≤ 1 MiB，加 2 不可能回绕。
        if rest.len() < size + 2 {
            return Err(HttpsError::BadResponse("chunked：数据段被截断".into()));
        }
        body.extend_from_slice(&rest[..size]);
        if &rest[size..size + 2] != b"\r\n" {
            return Err(HttpsError::BadResponse("chunked：数据段后缺少 CRLF".into()));
        }
        rest = &rest[size + 2..];
    }
}

/// `application/x-www-form-urlencoded` 的一个字段。
///
/// 按 RFC 3986 编码（空格 → `%20`，`+` → `%2B`），与生产实现里
/// `http_build_query(..., PHP_QUERY_RFC3986)` 的行为一致。
/// **不能用 `application/x-www-form-urlencoded` 的经典 `+` 代空格**：
/// 那会让一个含 `+` 的 token 在对端被解成空格。
pub fn form_encode(name: &str, value: &str) -> String {
    let mut out = String::with_capacity(name.len() + value.len() * 3 + 1);
    percent_encode_into(&mut out, name);
    out.push('=');
    percent_encode_into(&mut out, value);
    out
}

fn percent_encode_into(out: &mut String, value: &str) {
    for byte in value.bytes() {
        // RFC 3986 的 unreserved 集合：ALPHA / DIGIT / "-" / "." / "_" / "~"
        if byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'.' | b'_' | b'~') {
            out.push(byte as char);
        } else {
            out.push('%');
            out.push_str(&format!("{byte:02X}"));
        }
    }
}
