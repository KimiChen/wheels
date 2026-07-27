//! 订阅生成（纯函数，忠实移植 _legacy/sub.rs）：每条已授权 active Route 一个 SS-2022 代理，
//! password=serverPSK:userPSK。输出 Clash/mihomo YAML 或 raw(base64 ss:// URI 列表)。

use base64::engine::general_purpose::{STANDARD, URL_SAFE_NO_PAD};
use base64::Engine;

/// 一个订阅代理（SS-2022）。`password` = serverPSK:userPSK。
#[derive(Debug, Clone)]
pub struct ProxyInfo {
    pub label: String,
    pub server: String,
    pub port: i64,
    pub method: String,
    pub password: String,
}

/// ss:// URI（SIP002；userinfo 用 base64url-no-pad(method:password)）。
pub fn ss_uri(p: &ProxyInfo) -> String {
    let userinfo = URL_SAFE_NO_PAD.encode(format!("{}:{}", p.method, p.password));
    format!(
        "ss://{}@{}:{}#{}",
        userinfo,
        p.server,
        p.port,
        pct(&p.label)
    )
}

/// raw 订阅 = base64(STANDARD, ss:// 列表按行拼接)。
pub fn raw(proxies: &[ProxyInfo]) -> String {
    let joined = proxies.iter().map(ss_uri).collect::<Vec<_>>().join("\n");
    STANDARD.encode(joined)
}

/// Clash/mihomo YAML。
pub fn clash_yaml(proxies: &[ProxyInfo]) -> String {
    let mut s = String::from("proxies:\n");
    let mut names = Vec::new();
    for p in proxies {
        s.push_str(&format!(
            "  - {{name: {:?}, type: ss, server: {}, port: {}, cipher: {}, password: {:?}, udp: true}}\n",
            p.label, p.server, p.port, p.method, p.password
        ));
        names.push(format!("{:?}", p.label));
    }
    s.push_str("proxy-groups:\n");
    s.push_str(&format!(
        "  - {{name: PROXY, type: select, proxies: [{}]}}\n",
        names.join(", ")
    ));
    s.push_str("rules:\n  - MATCH,PROXY\n");
    s
}

/// 是否要 Clash 格式（target=clash 或 UA∈{clash,mihomo,stash}）。
pub fn wants_clash(target: Option<&str>, user_agent: &str) -> bool {
    let ua = user_agent.to_ascii_lowercase();
    target == Some("clash")
        || (target.is_none()
            && (ua.contains("clash") || ua.contains("mihomo") || ua.contains("stash")))
}

/// 是否要 HTML 页（浏览器 UA 且未显式要 raw/clash）。
pub fn wants_html(target: Option<&str>, user_agent: &str) -> bool {
    if target.is_some() {
        return false;
    }
    let ua = user_agent.to_ascii_lowercase();
    ua.contains("mozilla")
        || ua.contains("chrome")
        || ua.contains("safari")
        || ua.contains("firefox")
}

fn pct(s: &str) -> String {
    let mut out = String::new();
    for b in s.bytes() {
        if b.is_ascii_alphanumeric() || matches!(b, b'-' | b'_' | b'.' | b'~') {
            out.push(b as char);
        } else {
            out.push_str(&format!("%{b:02X}"));
        }
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    fn p() -> ProxyInfo {
        ProxyInfo {
            label: "hk-direct".into(),
            server: "e.example.com".into(),
            port: 19736,
            method: "2022-blake3-aes-128-gcm".into(),
            password: "SPSK:UPSK".into(),
        }
    }

    #[test]
    fn clash_and_raw_golden() {
        let y = clash_yaml(&[p()]);
        assert!(y.contains("type: ss"));
        assert!(y.contains("server: e.example.com"));
        assert!(y.contains("port: 19736"));
        assert!(y.contains("password: \"SPSK:UPSK\""));
        assert!(y.contains("cipher: 2022-blake3-aes-128-gcm"));
        assert!(y.contains("MATCH,PROXY"));
        // raw 可解回 ss://。
        let decoded = String::from_utf8(STANDARD.decode(raw(&[p()])).unwrap()).unwrap();
        assert!(decoded.starts_with("ss://"));
        assert!(decoded.contains("@e.example.com:19736#hk-direct"));
    }

    #[test]
    fn format_selection() {
        assert!(wants_clash(Some("clash"), "curl/8"));
        assert!(wants_clash(None, "Mihomo/1.19"));
        assert!(wants_clash(None, "Stash/2"));
        assert!(!wants_clash(Some("raw"), "Mihomo/1"));
        assert!(!wants_clash(None, "curl/8"));
        assert!(wants_html(None, "Mozilla/5.0"));
        assert!(!wants_html(Some("raw"), "Mozilla/5.0"));
    }
}
