//! 分享链接解析。把一条 `ss://` / `trojan://` / `vmess://` / `vless://`
//! 变成一份 Clash 代理配置，或者拒绝它。
//!
//! # 这是一个解析不可信输入的地方，所以规则是**白名单**
//!
//! 每个协议都写死了：允许哪些查询参数、允许哪些传输、允许哪些加密方式。
//! 遇到不认识的字段**报错而不是忽略**——忽略一个不认识的参数，
//! 意味着用户以为生效的设置（比如某个 plugin、某个 headerType）其实没生效，
//! 而这类静默降级在代理配置里恰好是「看起来能连、实际暴露」的那一类。
//!
//! 规则逐条照搬旧站 `ShareLinkParser.php`：那份清单是踩出来的，
//! 自己重写一份「先能用」的等于把同样的坑再踩一遍。
//!
//! # 解析出来的东西会原样进别人的客户端
//!
//! 所以 `skip-cert-verify` 一律写死 `false`，不接受链接里传进来的值。
//! 分享链接是从聊天窗口复制粘贴来的，让它能关掉证书校验，
//! 等于让任何能改那条链接的人拿到中间人位置。

use crate::error::{Error, Result};
use serde::{Deserialize, Serialize};

/// 名称上限。与旧站一致（80 字符，按字符不按字节）。
const NAME_MAX_CHARS: usize = 80;
/// 分享链接长度上限。
const URL_MAX_BYTES: usize = 16384;
/// 单个凭据字段的长度上限。
const CREDENTIAL_MAX_BYTES: usize = 1024;

/// 系统保留的节点名。旧站用它表示「套餐」那一条 direct 节点。
const RESERVED_NAME: &str = "套餐：无限制";

const SS_CIPHERS: &[&str] = &[
    "aes-128-gcm",
    "aes-256-gcm",
    "chacha20-ietf-poly1305",
    "2022-blake3-aes-128-gcm",
    "2022-blake3-aes-256-gcm",
    "2022-blake3-chacha20-poly1305",
];

const FINGERPRINTS: &[&str] =
    &["chrome", "firefox", "safari", "ios", "android", "edge", "360", "qq", "random", "randomized"];

const NETWORKS: &[&str] = &["tcp", "ws", "grpc"];

fn invalid(message: impl Into<String>) -> Error {
    Error::Codec(message.into())
}

/// 支持的协议。**闭集**，与 `schema/08_custom_nodes.sql` 的 CHECK 一起改。
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Protocol {
    Ss,
    Trojan,
    Vmess,
    Vless,
}

impl Protocol {
    pub fn as_str(self) -> &'static str {
        match self {
            Protocol::Ss => "ss",
            Protocol::Trojan => "trojan",
            Protocol::Vmess => "vmess",
            Protocol::Vless => "vless",
        }
    }

    pub fn parse(text: &str) -> Result<Self> {
        match text {
            "ss" => Ok(Protocol::Ss),
            "trojan" => Ok(Protocol::Trojan),
            "vmess" => Ok(Protocol::Vmess),
            "vless" => Ok(Protocol::Vless),
            other => Err(invalid(format!("不支持的协议：{other}"))),
        }
    }
}

/// 一条解析好的自定义上游。**这就是落库的明文形状**（JSON），
/// 也是渲染进订阅时的输入。
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct CustomProxy {
    pub name: String,
    pub protocol: Protocol,
    pub server: String,
    pub port: u16,
    /// 原始分享链接。**留着是为了让人能在页面上核对自己贴的是哪一条**，
    /// 渲染进订阅前会被剥掉（它不是 Clash 字段）。
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub share_url: Option<String>,

    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub cipher: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub password: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub uuid: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub alter_id: Option<u32>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub flow: Option<String>,

    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub network: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub ws_path: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub ws_host: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub grpc_service_name: Option<String>,

    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub tls: Option<bool>,
    /// Trojan 用 `sni`，VMess/VLESS 用 `servername`——**Clash 的字段名不同**，
    /// 合成一个字段会在渲染时又要分支，不如在这里就分开。
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub sni: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub servername: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub client_fingerprint: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub alpn: Option<Vec<String>>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub reality_public_key: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub reality_short_id: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub reality_spider_x: Option<String>,
}

/// 一条 SOCKS5 落地。
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct CustomSocks5 {
    pub name: String,
    pub server: String,
    pub port: u16,
    /// 空串表示不做认证。**空用户名配非空口令是错的**，建的时候就拒。
    #[serde(default)]
    pub username: String,
    #[serde(default)]
    pub password: String,
    /// 从哪条上游拨出去：`DIRECT`、某条受管入口名，或这个人自己的某条自定义上游名。
    pub dialer_proxy: String,
}

// ============ 名称与基本类型 ============

/// 节点名。**按字符数限长**，中文名不该只能写 26 个字。
pub fn proxy_name(value: &str) -> Result<String> {
    let name = value.trim();
    if name.is_empty() {
        return Err(invalid("名称不能为空"));
    }
    if name.chars().count() > NAME_MAX_CHARS {
        return Err(invalid(format!("名称超过 {NAME_MAX_CHARS} 个字符")));
    }
    if name.chars().any(is_control) {
        return Err(invalid("名称包含控制字符"));
    }
    if name == RESERVED_NAME {
        return Err(invalid("该名称由系统保留"));
    }
    Ok(name.to_string())
}

fn is_control(ch: char) -> bool {
    (ch as u32) < 0x20 || ch as u32 == 0x7f
}

/// 服务器地址：IP 字面量或域名。域名统一小写。
///
/// **不做 IDN 转换**（旧站用 PHP 的 `idn_to_ascii`）：本仓没有 IDNA 依赖，
/// 而为它引一个 punycode crate 只为支持中文域名的代理服务器不值。
/// 非 ASCII 直接拒绝，错误信息里说清楚要填 punycode——
/// 这是一个看得见、说得出口的限制，而不是一个悄悄接受再悄悄连错的行为。
pub fn host(value: &str) -> Result<String> {
    if value.is_empty() || value.len() > 253 {
        return Err(invalid("服务器地址为空或过长"));
    }
    if value.parse::<std::net::IpAddr>().is_ok() {
        return Ok(value.to_string());
    }
    if !value.is_ascii() {
        return Err(invalid("服务器地址含非 ASCII 字符，请填 punycode（xn-- 开头）形式"));
    }
    let lowered = value.to_ascii_lowercase();
    let labels: Vec<&str> = lowered.split('.').collect();
    if labels.iter().any(|label| {
        label.is_empty()
            || label.len() > 63
            || label.starts_with('-')
            || label.ends_with('-')
            || !label.bytes().all(|b| b.is_ascii_alphanumeric() || b == b'-')
    }) {
        return Err(invalid("服务器地址不是合法域名"));
    }
    Ok(lowered)
}

pub fn port(value: &str) -> Result<u16> {
    let parsed: u32 = value.parse().map_err(|_| invalid("端口必须是数字"))?;
    if !(1..=65535).contains(&parsed) {
        return Err(invalid("端口必须在 1 到 65535 之间"));
    }
    Ok(parsed as u16)
}

fn credential(value: &str, label: &str, allow_empty: bool) -> Result<()> {
    if (!allow_empty && value.is_empty())
        || value.len() > CREDENTIAL_MAX_BYTES
        || value.chars().any(is_control)
    {
        return Err(invalid(format!("{label}无效")));
    }
    Ok(())
}

fn uuid(value: &str, label: &str) -> Result<()> {
    let bytes = value.as_bytes();
    let shape_ok = bytes.len() == 36
        && [8, 13, 18, 23].iter().all(|&i| bytes[i] == b'-')
        && bytes
            .iter()
            .enumerate()
            .all(|(i, b)| [8, 13, 18, 23].contains(&i) || b.is_ascii_hexdigit());
    // 版本位 1-5、变体位 8/9/a/b。照抄旧站那条正则的约束，
    // 它挡掉的是全零 UUID 这类「形状对但不是真 UUID」的值。
    let version_ok = shape_ok && (b'1'..=b'5').contains(&bytes[14]);
    let variant_ok =
        shape_ok && matches!(bytes[19].to_ascii_lowercase(), b'8' | b'9' | b'a' | b'b');
    if !(shape_ok && version_ok && variant_ok) {
        return Err(invalid(format!("{label}无效")));
    }
    Ok(())
}

fn transport_path(value: &str) -> Result<String> {
    if value.is_empty()
        || !value.starts_with('/')
        || value.len() > 1024
        || value.chars().any(is_control)
    {
        return Err(invalid("传输路径无效"));
    }
    Ok(value.to_string())
}

fn header_value(value: &str) -> Result<String> {
    if value.is_empty() || value.len() > 512 || value.chars().any(is_control) {
        return Err(invalid("WS Host 无效"));
    }
    Ok(value.to_string())
}

// ============ URL 与查询串 ============

/// 一条分享链接拆开之后的样子。本仓没有 `url` crate，而这些链接的形状很窄，
/// 手写一个反而能把「不允许 path」这类规则写在同一处。
#[derive(Debug, Default)]
struct Parts {
    scheme: String,
    user: Option<String>,
    pass: Option<String>,
    host: String,
    port: String,
    path: Option<String>,
    query: Option<String>,
}

fn split_url(url: &str) -> Result<Parts> {
    let (scheme, rest) = url.split_once("://").ok_or_else(|| invalid("分享链接缺少协议头"))?;
    let mut parts = Parts { scheme: scheme.to_ascii_lowercase(), ..Default::default() };
    // fragment 是节点备注，一律丢掉——名称由用户在表单里另填。
    let rest = rest.split('#').next().unwrap_or("");
    let (rest, query) = match rest.split_once('?') {
        Some((left, right)) => (left, Some(right.to_string())),
        None => (rest, None),
    };
    parts.query = query;
    let (authority, path) = match rest.split_once('/') {
        Some((left, right)) => (left, Some(format!("/{right}"))),
        None => (rest, None),
    };
    parts.path = path;
    // **从右边找 `@`**：userinfo 里可以有 `@`（Base64 里不会，但密码里会）。
    let (userinfo, hostport) = match authority.rsplit_once('@') {
        Some((left, right)) => (Some(left), right),
        None => (None, authority),
    };
    if let Some(userinfo) = userinfo {
        match userinfo.split_once(':') {
            Some((user, pass)) => {
                parts.user = Some(user.to_string());
                parts.pass = Some(pass.to_string());
            }
            None => parts.user = Some(userinfo.to_string()),
        }
    }
    // IPv6 字面量写成 `[::1]:443`。
    if let Some(end) = hostport.strip_prefix('[').and_then(|rest| rest.find(']').map(|i| i + 1)) {
        parts.host = hostport[1..end].to_string();
        parts.port = hostport[end + 1..].trim_start_matches(':').to_string();
    } else {
        match hostport.rsplit_once(':') {
            Some((h, p)) => {
                parts.host = h.to_string();
                parts.port = p.to_string();
            }
            None => parts.host = hostport.to_string(),
        }
    }
    Ok(parts)
}

/// 查询串。**重复键报错**而不是后者覆盖前者——「写了两个 sni」是一个
/// 说不清作者意图的输入，猜一个等于替他做决定。
fn query_map(query: &str) -> Result<std::collections::BTreeMap<String, String>> {
    let mut out = std::collections::BTreeMap::new();
    if query.is_empty() {
        return Ok(out);
    }
    for pair in query.split('&') {
        if pair.is_empty() {
            return Err(invalid("分享链接包含空查询参数"));
        }
        let (raw_key, raw_value) = match pair.split_once('=') {
            Some((k, v)) => (k, v),
            None => (pair, ""),
        };
        let key = percent_decode(raw_key)?;
        let value = percent_decode(raw_value)?;
        if key.is_empty() || key.contains('[') {
            return Err(invalid("分享链接包含非法查询参数"));
        }
        if out.insert(key, value).is_some() {
            return Err(invalid("分享链接包含重复查询参数"));
        }
    }
    Ok(out)
}

fn percent_decode(value: &str) -> Result<String> {
    let bytes = value.as_bytes();
    let mut out = Vec::with_capacity(bytes.len());
    let mut i = 0;
    while i < bytes.len() {
        if bytes[i] == b'%' {
            if i + 2 >= bytes.len() {
                return Err(invalid("百分号编码不完整"));
            }
            let hex =
                std::str::from_utf8(&bytes[i + 1..i + 3]).map_err(|_| invalid("百分号编码无效"))?;
            out.push(u8::from_str_radix(hex, 16).map_err(|_| invalid("百分号编码无效"))?);
            i += 3;
        } else {
            out.push(bytes[i]);
            i += 1;
        }
    }
    String::from_utf8(out).map_err(|_| invalid("百分号编码解出来不是合法 UTF-8"))
}

fn assert_allowed_keys(
    values: &std::collections::BTreeMap<String, String>,
    allowed: &[&str],
) -> Result<()> {
    let unknown: Vec<&str> =
        values.keys().map(String::as_str).filter(|key| !allowed.contains(key)).collect();
    if !unknown.is_empty() {
        return Err(invalid(format!("包含未知字段：{}", unknown.join(", "))));
    }
    Ok(())
}

/// Base64，标准与 URL-safe 都收，填不填 `=` 都收。
/// 分享链接在野外就是这几种写法混着来的。
fn flexible_base64(value: &str, label: &str) -> Result<Vec<u8>> {
    use base64::Engine as _;
    if value.is_empty()
        || !value.bytes().all(|b| b.is_ascii_alphanumeric() || b"+/_-=".contains(&b))
    {
        return Err(invalid(format!("{label} Base64 格式无效")));
    }
    let normalized: String = value
        .chars()
        .map(|c| match c {
            '-' => '+',
            '_' => '/',
            other => other,
        })
        .filter(|c| *c != '=')
        .collect();
    base64::engine::general_purpose::STANDARD_NO_PAD
        .decode(normalized)
        .map_err(|_| invalid(format!("{label} Base64 格式无效")))
}

// ============ 入口 ============

/// 解析一条分享链接。`name` 由用户另填，链接里的 fragment 备注一律丢弃。
pub fn parse_share_url(name: &str, url: &str) -> Result<CustomProxy> {
    let name = proxy_name(name)?;
    if url.is_empty() || url.len() > URL_MAX_BYTES || url.chars().any(is_control) {
        return Err(invalid("分享链接为空、过长或包含控制字符"));
    }
    let scheme = url.split("://").next().unwrap_or("").to_ascii_lowercase();
    let mut proxy = match scheme.as_str() {
        "ss" => parse_ss(url)?,
        "trojan" => parse_trojan(url)?,
        "vmess" => parse_vmess(url)?,
        "vless" => parse_vless(url)?,
        _ => return Err(invalid("仅支持 SS、Trojan、VMess 和 VLESS 分享链接")),
    };
    proxy.name = name;
    proxy.share_url = Some(url.to_string());
    Ok(proxy)
}

fn blank(protocol: Protocol, server: String, port: u16) -> CustomProxy {
    CustomProxy {
        name: String::new(),
        protocol,
        server,
        port,
        share_url: None,
        cipher: None,
        password: None,
        uuid: None,
        alter_id: None,
        flow: None,
        network: None,
        ws_path: None,
        ws_host: None,
        grpc_service_name: None,
        tls: None,
        sni: None,
        servername: None,
        client_fingerprint: None,
        alpn: None,
        reality_public_key: None,
        reality_short_id: None,
        reality_spider_x: None,
    }
}

// ============ 四个协议 ============

fn parse_ss(url: &str) -> Result<CustomProxy> {
    let body = url.trim_start_matches("ss://").split('#').next().unwrap_or("");
    // **插件一律不收。** `?plugin=...` 的语义由插件自己定义，
    // 白名单校验不到它里面去，而它能把流量改道到任意地方。
    if body.contains('?') {
        return Err(invalid("SS 仅允许无插件的 SIP002 链接"));
    }
    // 整条 Base64 的老写法（SIP002 之前）。
    let body = if body.contains('@') {
        body.to_string()
    } else {
        let decoded = flexible_base64(body, "SS SIP002")?;
        let text =
            String::from_utf8(decoded).map_err(|_| invalid("SS SIP002 解出来不是合法 UTF-8"))?;
        if !text.contains('@') {
            return Err(invalid("SS SIP002 链接缺少服务器"));
        }
        text
    };
    let (userinfo, endpoint) = body.rsplit_once('@').ok_or_else(|| invalid("SS 链接缺少服务器"))?;
    let decoded_userinfo = percent_decode(userinfo)?;
    let decoded_userinfo = if decoded_userinfo.contains(':') {
        decoded_userinfo
    } else {
        String::from_utf8(flexible_base64(&decoded_userinfo, "SS userinfo")?)
            .map_err(|_| invalid("SS userinfo 解出来不是合法 UTF-8"))?
    };
    let (cipher, password) = decoded_userinfo
        .split_once(':')
        .ok_or_else(|| invalid("SS userinfo 缺少加密方式或密码"))?;
    if !SS_CIPHERS.contains(&cipher) {
        return Err(invalid("SS 加密方式不在允许列表中"));
    }
    credential(password, "SS 密码", false)?;
    let target = split_url(&format!("tcp://{endpoint}"))?;
    if target.user.is_some() || target.path.is_some() || target.query.is_some() {
        return Err(invalid("SS 服务器地址无效"));
    }
    let mut proxy = blank(Protocol::Ss, host(&target.host)?, port(&target.port)?);
    proxy.cipher = Some(cipher.to_string());
    proxy.password = Some(password.to_string());
    Ok(proxy)
}

fn parse_trojan(url: &str) -> Result<CustomProxy> {
    let parts = network_url(url, "trojan")?;
    let mut password = percent_decode(parts.user.as_deref().unwrap_or(""))?;
    if let Some(extra) = &parts.pass {
        password.push(':');
        password.push_str(&percent_decode(extra)?);
    }
    credential(&password, "Trojan 密码", false)?;
    let query = query_map(parts.query.as_deref().unwrap_or(""))?;
    let network = query.get("type").map(|s| s.to_ascii_lowercase()).unwrap_or_else(|| "tcp".into());
    if !NETWORKS.contains(&network.as_str()) {
        return Err(invalid("Trojan 传输只支持 TCP、WS 或 gRPC"));
    }
    if query.get("security").map(|s| s.to_ascii_lowercase()).unwrap_or_else(|| "tls".into())
        != "tls"
    {
        return Err(invalid("Trojan 必须启用 TLS"));
    }
    assert_allowed_keys(
        &query,
        &allowed_keys(&network, &["type", "security", "sni", "servername", "alpn", "fp"], false),
    )?;

    let mut proxy = blank(Protocol::Trojan, host(&parts.host)?, port(&parts.port)?);
    proxy.password = Some(password);
    proxy.network = Some(network.clone());
    proxy.sni = optional_host(query.get("sni").or_else(|| query.get("servername")))?;
    apply_tls(&mut proxy, &query)?;
    apply_network(&mut proxy, &network, &query)?;
    Ok(proxy)
}

fn parse_vmess(url: &str) -> Result<CustomProxy> {
    let raw = url.trim_start_matches("vmess://");
    if raw.is_empty() || raw.contains('?') || raw.contains('#') {
        return Err(invalid("VMess 必须使用 Base64 JSON 分享格式"));
    }
    let decoded = flexible_base64(raw, "VMess")?;
    if decoded.len() > 65536 {
        return Err(invalid("VMess JSON 超过大小限制"));
    }
    let data: serde_json::Map<String, serde_json::Value> =
        serde_json::from_slice(&decoded).map_err(|_| invalid("VMess JSON 无法解析"))?;
    let allowed = [
        "v", "ps", "add", "port", "id", "aid", "scy", "net", "type", "host", "path", "tls", "sni",
        "alpn", "fp",
    ];
    let unknown: Vec<&str> =
        data.keys().map(String::as_str).filter(|k| !allowed.contains(k)).collect();
    if !unknown.is_empty() {
        return Err(invalid(format!("VMess 包含未知字段：{}", unknown.join(", "))));
    }
    let scalar = |key: &str| -> String {
        match data.get(key) {
            Some(serde_json::Value::String(s)) => s.clone(),
            Some(serde_json::Value::Number(n)) => n.to_string(),
            _ => String::new(),
        }
    };
    let version = scalar("v");
    if !version.is_empty() && version != "2" {
        return Err(invalid("VMess 配置版本必须为 2"));
    }
    let alter_id = scalar("aid");
    if !alter_id.is_empty() && alter_id != "0" {
        return Err(invalid("VMess 仅允许 AEAD（alterId=0）"));
    }
    let id = scalar("id").to_ascii_lowercase();
    uuid(&id, "VMess UUID")?;
    let network = {
        let net = scalar("net");
        if net.is_empty() {
            "tcp".to_string()
        } else {
            net.to_ascii_lowercase()
        }
    };
    if !NETWORKS.contains(&network.as_str()) {
        return Err(invalid("VMess 传输只支持 TCP、WS 或 gRPC"));
    }
    let cipher = {
        let scy = scalar("scy");
        if scy.is_empty() {
            "auto".to_string()
        } else {
            scy.to_ascii_lowercase()
        }
    };
    if !["auto", "none", "zero"].contains(&cipher.as_str()) {
        return Err(invalid("VMess cipher 无效"));
    }
    let tls_mode = scalar("tls").to_ascii_lowercase();
    if !tls_mode.is_empty() && tls_mode != "tls" {
        return Err(invalid("VMess 安全层只能为 none 或 TLS"));
    }
    // VMess 的 JSON 把传输参数放在固定键上，而下面两个 apply_* 读的是查询串的
    // 键名。在这里做一次映射，两条路径就只有一份实现。
    let mut query = std::collections::BTreeMap::new();
    for (from, to) in [
        ("path", "path"),
        ("host", "host"),
        ("path", "serviceName"),
        ("alpn", "alpn"),
        ("fp", "fp"),
    ] {
        let value = scalar(from);
        if !value.is_empty() {
            query.insert(to.to_string(), value);
        }
    }

    let mut proxy = blank(Protocol::Vmess, host(&scalar("add"))?, port(&scalar("port"))?);
    proxy.uuid = Some(id);
    proxy.alter_id = Some(0);
    proxy.cipher = Some(cipher);
    proxy.network = Some(network.clone());
    proxy.tls = Some(tls_mode == "tls");
    if tls_mode == "tls" {
        proxy.servername =
            optional_host(data.get("sni").and_then(|v| v.as_str()).map(String::from).as_ref())?;
        apply_tls(&mut proxy, &query)?;
    }
    apply_network(&mut proxy, &network, &query)?;
    Ok(proxy)
}

fn parse_vless(url: &str) -> Result<CustomProxy> {
    let parts = network_url(url, "vless")?;
    if parts.pass.is_some() {
        return Err(invalid("VLESS userinfo 格式无效"));
    }
    let id = percent_decode(parts.user.as_deref().unwrap_or(""))?.to_ascii_lowercase();
    uuid(&id, "VLESS UUID")?;
    let query = query_map(parts.query.as_deref().unwrap_or(""))?;
    let network = query.get("type").map(|s| s.to_ascii_lowercase()).unwrap_or_else(|| "tcp".into());
    if !NETWORKS.contains(&network.as_str()) {
        return Err(invalid("VLESS 传输只支持 TCP、WS 或 gRPC"));
    }
    let security = query.get("security").map(|s| s.to_ascii_lowercase()).unwrap_or_default();
    if security != "tls" && security != "reality" {
        return Err(invalid("VLESS 必须使用 TLS 或 REALITY"));
    }
    let flow = query.get("flow").cloned().unwrap_or_default();
    if (!flow.is_empty() && flow != "xtls-rprx-vision") || (!flow.is_empty() && network != "tcp") {
        return Err(invalid("VLESS flow 只能为空或 TCP 上的 xtls-rprx-vision"));
    }
    let mut allowed = allowed_keys(
        &network,
        &["type", "security", "flow", "sni", "servername", "alpn", "fp", "encryption"],
        network == "tcp",
    );
    if security == "reality" {
        allowed.extend_from_slice(&["pbk", "sid", "spx"]);
    }
    assert_allowed_keys(&query, &allowed)?;
    if query.get("encryption").map(String::as_str).unwrap_or("none") != "none" {
        return Err(invalid("VLESS encryption 目前只支持 none"));
    }
    if query.get("headerType").map(String::as_str).unwrap_or("none") != "none" {
        return Err(invalid("VLESS TCP headerType 目前只支持 none"));
    }
    let servername = optional_host(query.get("sni").or_else(|| query.get("servername")))?
        .ok_or_else(|| invalid("VLESS 的 TLS/REALITY 必须提供 SNI"))?;

    let mut proxy = blank(Protocol::Vless, host(&parts.host)?, port(&parts.port)?);
    proxy.uuid = Some(id);
    proxy.network = Some(network.clone());
    proxy.tls = Some(true);
    proxy.servername = Some(servername);
    if !flow.is_empty() {
        proxy.flow = Some(flow);
    }
    if security == "reality" {
        let public_key = query.get("pbk").cloned().unwrap_or_default();
        if public_key.len() != 43
            || !public_key.bytes().all(|b| b.is_ascii_alphanumeric() || b == b'-' || b == b'_')
        {
            return Err(invalid("REALITY 公钥必须是 43 字符的 Base64url"));
        }
        let short_id = query.get("sid").cloned().unwrap_or_default();
        if short_id.len() > 16
            || short_id.len() % 2 != 0
            || !short_id.bytes().all(|b| b.is_ascii_hexdigit())
        {
            return Err(invalid("REALITY short-id 必须是最多 8 字节的十六进制"));
        }
        proxy.reality_public_key = Some(public_key);
        proxy.reality_short_id = Some(short_id.to_ascii_lowercase());
        if let Some(spx) = query.get("spx").filter(|s| !s.is_empty()) {
            proxy.reality_spider_x = Some(transport_path(spx)?);
        }
    }
    apply_tls(&mut proxy, &query)?;
    apply_network(&mut proxy, &network, &query)?;
    Ok(proxy)
}

// ============ 共用片段 ============

fn allowed_keys<'a>(network: &str, base: &[&'a str], tcp_header: bool) -> Vec<&'a str> {
    let mut allowed = base.to_vec();
    match network {
        "ws" => allowed.extend_from_slice(&["path", "host"]),
        "grpc" => allowed.push("serviceName"),
        _ => {}
    }
    if tcp_header && network == "tcp" {
        allowed.push("headerType");
    }
    allowed
}

fn network_url(url: &str, scheme: &str) -> Result<Parts> {
    let parts = split_url(url)?;
    if parts.scheme != scheme || parts.path.is_some() {
        return Err(invalid(format!("{} 分享链接无效", scheme.to_uppercase())));
    }
    host(&parts.host)?;
    port(&parts.port)?;
    Ok(parts)
}

fn optional_host(value: Option<&String>) -> Result<Option<String>> {
    match value {
        None => Ok(None),
        Some(text) if text.is_empty() => Ok(None),
        Some(text) => Ok(Some(host(text)?)),
    }
}

fn apply_tls(
    proxy: &mut CustomProxy,
    query: &std::collections::BTreeMap<String, String>,
) -> Result<()> {
    if let Some(fingerprint) = query.get("fp").filter(|s| !s.is_empty()) {
        let lowered = fingerprint.to_ascii_lowercase();
        if !FINGERPRINTS.contains(&lowered.as_str()) {
            return Err(invalid("TLS client fingerprint 无效"));
        }
        proxy.client_fingerprint = Some(lowered);
    }
    if let Some(alpn) = query.get("alpn").filter(|s| !s.is_empty()) {
        let mut values: Vec<String> = Vec::new();
        for item in alpn.split(',') {
            let trimmed = item.trim().to_string();
            if !["h2", "http/1.1"].contains(&trimmed.as_str()) {
                return Err(invalid("TLS ALPN 只允许 h2 和 http/1.1"));
            }
            if !values.contains(&trimmed) {
                values.push(trimmed);
            }
        }
        if values.is_empty() {
            return Err(invalid("TLS ALPN 不能为空"));
        }
        proxy.alpn = Some(values);
    }
    Ok(())
}

fn apply_network(
    proxy: &mut CustomProxy,
    network: &str,
    query: &std::collections::BTreeMap<String, String>,
) -> Result<()> {
    match network {
        "ws" => {
            let path = query.get("path").map(String::as_str).unwrap_or("/");
            proxy.ws_path = Some(transport_path(path)?);
            if let Some(host_header) = query.get("host").filter(|s| !s.is_empty()) {
                proxy.ws_host = Some(header_value(host_header)?);
            }
        }
        "grpc" => {
            let service = query.get("serviceName").map(String::as_str).unwrap_or("");
            if service.is_empty() || service.len() > 256 || service.chars().any(is_control) {
                return Err(invalid("gRPC serviceName 无效"));
            }
            proxy.grpc_service_name = Some(service.to_string());
        }
        _ => {}
    }
    Ok(())
}

// ============ SOCKS5 ============

/// 表单提交的 SOCKS5 配置。链接形式由 `parse_socks5_url` 处理。
pub fn normalize_socks5(
    name: &str,
    server: &str,
    port_text: &str,
    username: &str,
    password: &str,
    dialer: &str,
) -> Result<CustomSocks5> {
    credential(username, "SOCKS5 用户名", true)?;
    credential(password, "SOCKS5 密码", true)?;
    // 空用户名配非空口令是一份**半配置**：客户端会不带认证去连，
    // 而作者以为自己配了认证。这种「以为配了其实没配」正是要在入口挡掉的。
    if username.is_empty() && !password.is_empty() {
        return Err(invalid("SOCKS5 密码不能脱离用户名单独配置"));
    }
    Ok(CustomSocks5 {
        name: proxy_name(name)?,
        server: host(server)?,
        port: port(port_text)?,
        username: username.to_string(),
        password: password.to_string(),
        dialer_proxy: dialer_name(dialer)?,
    })
}

/// `socks5://[user:pass@]host:port`。**不接受查询串与 fragment**——
/// SOCKS5 没有需要它们表达的东西，收下只会让人以为某个参数生效了。
pub fn parse_socks5_url(name: &str, url: &str, dialer: &str) -> Result<CustomSocks5> {
    if url.len() > URL_MAX_BYTES || url.chars().any(is_control) {
        return Err(invalid("SOCKS5 链接过长或包含控制字符"));
    }
    if url.contains('?') || url.contains('#') {
        return Err(invalid("SOCKS5 链接不允许查询参数或片段"));
    }
    let parts = split_url(url)?;
    if parts.scheme != "socks5" || parts.path.is_some() {
        return Err(invalid("SOCKS5 链接无效"));
    }
    let username = percent_decode(parts.user.as_deref().unwrap_or(""))?;
    let password = percent_decode(parts.pass.as_deref().unwrap_or(""))?;
    normalize_socks5(name, &parts.host, &parts.port, &username, &password, dialer)
}

fn dialer_name(value: &str) -> Result<String> {
    if value == "DIRECT" {
        return Ok("DIRECT".to_string());
    }
    proxy_name(value)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn ss_sip002() {
        let proxy =
            parse_share_url("家里", "ss://YWVzLTI1Ni1nY206cGFzc3dvcmQ@example.com:8388#备注")
                .unwrap();
        assert_eq!(proxy.protocol, Protocol::Ss);
        assert_eq!(proxy.server, "example.com");
        assert_eq!(proxy.port, 8388);
        assert_eq!(proxy.cipher.as_deref(), Some("aes-256-gcm"));
        assert_eq!(proxy.password.as_deref(), Some("password"));
        // fragment 是链接自带的备注，**不能**变成节点名——名字由用户另填。
        assert_eq!(proxy.name, "家里");
    }

    #[test]
    fn ss_拒绝插件() {
        let error =
            parse_share_url("x", "ss://YWVzLTI1Ni1nY206cA@h.example:1?plugin=obfs").unwrap_err();
        assert!(format!("{error}").contains("插件"), "{error}");
    }

    #[test]
    fn ss_加密方式白名单() {
        // rc4-md5 是被淘汰的算法，不在白名单里。
        let url = format!("ss://{}@h.example:1", b64("rc4-md5:pw"));
        assert!(parse_share_url("x", &url).is_err());
    }

    fn b64(text: &str) -> String {
        use base64::Engine as _;
        base64::engine::general_purpose::STANDARD_NO_PAD.encode(text)
    }

    #[test]
    fn trojan_ws() {
        let proxy = parse_share_url(
            "t",
            "trojan://pw@h.example:443?type=ws&security=tls&path=%2Fws&host=cdn.example&fp=chrome",
        )
        .unwrap();
        assert_eq!(proxy.protocol, Protocol::Trojan);
        assert_eq!(proxy.network.as_deref(), Some("ws"));
        assert_eq!(proxy.ws_path.as_deref(), Some("/ws"));
        assert_eq!(proxy.ws_host.as_deref(), Some("cdn.example"));
        assert_eq!(proxy.client_fingerprint.as_deref(), Some("chrome"));
    }

    #[test]
    fn trojan_必须开tls() {
        assert!(parse_share_url("t", "trojan://pw@h.example:443?security=none").is_err());
    }

    #[test]
    fn vmess_base64_json() {
        let json = r#"{"v":"2","ps":"备注","add":"h.example","port":"443","id":"b831381d-6324-4d53-ad4f-8cda48b30811","aid":"0","net":"ws","path":"/p","tls":"tls","sni":"s.example"}"#;
        let proxy = parse_share_url("v", &format!("vmess://{}", b64(json))).unwrap();
        assert_eq!(proxy.protocol, Protocol::Vmess);
        assert_eq!(proxy.uuid.as_deref(), Some("b831381d-6324-4d53-ad4f-8cda48b30811"));
        assert_eq!(proxy.alter_id, Some(0));
        assert_eq!(proxy.tls, Some(true));
        assert_eq!(proxy.servername.as_deref(), Some("s.example"));
        assert_eq!(proxy.ws_path.as_deref(), Some("/p"));
    }

    /// alterId 非 0 是老式非 AEAD VMess，拒掉。
    #[test]
    fn vmess_拒绝非aead() {
        let json = r#"{"v":"2","add":"h.example","port":"443","id":"b831381d-6324-4d53-ad4f-8cda48b30811","aid":"64"}"#;
        assert!(parse_share_url("v", &format!("vmess://{}", b64(json))).is_err());
    }

    #[test]
    fn vless_reality() {
        let url = "vless://b831381d-6324-4d53-ad4f-8cda48b30811@h.example:443\
                   ?security=reality&sni=www.apple.com&pbk=mF4TAe0k9XmXcQ1LQGvCGH2OxRYsWLnLDgLLmVoSFXQ&sid=01234567&flow=xtls-rprx-vision";
        let proxy = parse_share_url("r", url).unwrap();
        assert_eq!(proxy.protocol, Protocol::Vless);
        assert_eq!(proxy.flow.as_deref(), Some("xtls-rprx-vision"));
        assert_eq!(proxy.reality_short_id.as_deref(), Some("01234567"));
        assert_eq!(proxy.servername.as_deref(), Some("www.apple.com"));
    }

    /// REALITY 的 short-id 是十六进制，奇数位或非十六进制都该拒。
    #[test]
    fn vless_reality_short_id_形状() {
        let base = "vless://b831381d-6324-4d53-ad4f-8cda48b30811@h.example:443\
                    ?security=reality&sni=a.example&pbk=mF4TAe0k9XmXcQ1LQGvCGH2OxRYsWLnLDgLLmVoSFXQ";
        assert!(parse_share_url("r", &format!("{base}&sid=012")).is_err(), "奇数位该拒");
        assert!(parse_share_url("r", &format!("{base}&sid=zz")).is_err(), "非十六进制该拒");
        assert!(parse_share_url("r", &format!("{base}&sid=")).is_ok(), "空 short-id 是合法的");
    }

    /// **flow 只在 TCP 上成立。** ws + vision 是一个连不通的组合，
    /// 收下它等于发一份注定失败的订阅。
    #[test]
    fn vless_flow_只能配tcp() {
        let url = "vless://b831381d-6324-4d53-ad4f-8cda48b30811@h.example:443\
                   ?security=tls&sni=a.example&type=ws&flow=xtls-rprx-vision";
        assert!(parse_share_url("r", url).is_err());
    }

    #[test]
    fn vless_必须有sni() {
        let url = "vless://b831381d-6324-4d53-ad4f-8cda48b30811@h.example:443?security=tls";
        assert!(parse_share_url("r", url).is_err());
    }

    /// 不认识的查询参数**报错而不是忽略**：忽略等于让人以为设置生效了。
    #[test]
    fn 未知查询参数要报错() {
        let url = "trojan://pw@h.example:443?security=tls&someUnknown=1";
        let error = parse_share_url("t", url).unwrap_err();
        assert!(format!("{error}").contains("未知字段"), "{error}");
    }

    #[test]
    fn 重复查询参数要报错() {
        let url = "trojan://pw@h.example:443?security=tls&sni=a.example&sni=b.example";
        assert!(parse_share_url("t", url).is_err());
    }

    #[test]
    fn 保留名被拒() {
        assert!(proxy_name("套餐：无限制").is_err());
    }

    #[test]
    fn 名称按字符数限长() {
        // 80 个汉字应当通过——按字节算的话这里是 240 字节，会被误拒。
        assert!(proxy_name(&"节".repeat(80)).is_ok());
        assert!(proxy_name(&"节".repeat(81)).is_err());
    }

    #[test]
    fn 域名与端口校验() {
        assert_eq!(host("EXAMPLE.COM").unwrap(), "example.com");
        assert_eq!(host("2001:db8::1").unwrap(), "2001:db8::1");
        assert!(host("-bad.example").is_err());
        assert!(host("中文.example").is_err(), "非 ASCII 要明确拒绝而不是悄悄接受");
        assert!(port("0").is_err());
        assert!(port("65536").is_err());
        assert_eq!(port("443").unwrap(), 443);
    }

    #[test]
    fn socks5_链接() {
        let socks = parse_socks5_url("落地", "socks5://u:p%40ss@h.example:1080", "DIRECT").unwrap();
        assert_eq!(socks.server, "h.example");
        assert_eq!(socks.port, 1080);
        assert_eq!(socks.username, "u");
        assert_eq!(socks.password, "p@ss");
        assert_eq!(socks.dialer_proxy, "DIRECT");
    }

    #[test]
    fn socks5_口令不能脱离用户名() {
        assert!(normalize_socks5("x", "h.example", "1080", "", "pw", "DIRECT").is_err());
    }

    #[test]
    fn socks5_拒绝查询串() {
        assert!(parse_socks5_url("x", "socks5://h.example:1080?a=1", "DIRECT").is_err());
    }

    /// 配置要能原样往返 JSON——它就是落库的形状。
    #[test]
    fn 配置往返json() {
        let proxy = parse_share_url(
            "t",
            "trojan://pw@h.example:443?type=grpc&security=tls&serviceName=svc&alpn=h2",
        )
        .unwrap();
        let text = serde_json::to_string(&proxy).unwrap();
        let back: CustomProxy = serde_json::from_str(&text).unwrap();
        assert_eq!(proxy, back);
    }
}
