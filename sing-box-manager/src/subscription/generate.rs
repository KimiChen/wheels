//! 订阅生成纯函数：混合输出 SS-2022 与 VLESS-Reality 代理。

use base64::engine::general_purpose::{STANDARD, URL_SAFE_NO_PAD};
use base64::Engine;

#[derive(Debug, Clone)]
pub enum ProxyInfo {
    Shadowsocks {
        label: String,
        server: String,
        port: i64,
        method: String,
        password: String,
    },
    VlessReality {
        label: String,
        server: String,
        port: i64,
        uuid: String,
        flow: String,
        public_key: String,
        short_id: String,
        server_name: String,
        client_fingerprint: String,
    },
}

impl ProxyInfo {
    fn label(&self) -> &str {
        match self {
            Self::Shadowsocks { label, .. } | Self::VlessReality { label, .. } => label,
        }
    }
}

/// SIP002 SS URI 或 VLESS-Reality URI。
pub fn uri(p: &ProxyInfo) -> String {
    match p {
        ProxyInfo::Shadowsocks {
            label,
            server,
            port,
            method,
            password,
        } => {
            let userinfo = URL_SAFE_NO_PAD.encode(format!("{method}:{password}"));
            format!(
                "ss://{}@{}:{}#{}",
                userinfo,
                uri_host(server),
                port,
                pct(label)
            )
        }
        ProxyInfo::VlessReality {
            label,
            server,
            port,
            uuid,
            flow,
            public_key,
            short_id,
            server_name,
            client_fingerprint,
        } => format!(
            "vless://{}@{}:{}?encryption=none&security=reality&flow={}&pbk={}&sid={}&sni={}&fp={}&type=tcp#{}",
            uuid,
            uri_host(server),
            port,
            pct(flow),
            pct(public_key),
            pct(short_id),
            pct(server_name),
            pct(client_fingerprint),
            pct(label),
        ),
    }
}

/// raw 订阅 = base64(STANDARD, URI 列表按行拼接)。
pub fn raw(proxies: &[ProxyInfo]) -> String {
    let joined = proxies.iter().map(uri).collect::<Vec<_>>().join("\n");
    STANDARD.encode(joined)
}

/// Clash/mihomo YAML。
pub fn clash_yaml(proxies: &[ProxyInfo]) -> String {
    let mut s = String::from("proxies:\n");
    let mut names = Vec::new();
    for p in proxies {
        match p {
            ProxyInfo::Shadowsocks {
                label,
                server,
                port,
                method,
                password,
            } => s.push_str(&format!(
                "  - {{name: {:?}, type: ss, server: {:?}, port: {}, cipher: {}, password: {:?}, udp: true}}\n",
                label, server, port, method, password
            )),
            ProxyInfo::VlessReality {
                label,
                server,
                port,
                uuid,
                flow,
                public_key,
                short_id,
                server_name,
                client_fingerprint,
            } => s.push_str(&format!(
                "  - {{name: {:?}, type: vless, server: {:?}, port: {}, uuid: {:?}, network: tcp, tls: true, udp: true, flow: {:?}, servername: {:?}, client-fingerprint: {:?}, reality-opts: {{public-key: {:?}, short-id: {:?}}}}}\n",
                label,
                server,
                port,
                uuid,
                flow,
                server_name,
                client_fingerprint,
                public_key,
                short_id,
            )),
        }
        names.push(format!("{:?}", p.label()));
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

fn uri_host(s: &str) -> String {
    if s.contains(':') && !s.starts_with('[') {
        format!("[{s}]")
    } else {
        s.to_string()
    }
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

    fn ss() -> ProxyInfo {
        ProxyInfo::Shadowsocks {
            label: "hk-direct".into(),
            server: "e.example.com".into(),
            port: 19736,
            method: "2022-blake3-aes-128-gcm".into(),
            password: "SPSK:UPSK".into(),
        }
    }

    fn vless() -> ProxyInfo {
        ProxyInfo::VlessReality {
            label: "us-reality".into(),
            server: "2001:db8::1".into(),
            port: 19736,
            uuid: "8f13c901-99e8-43e9-ad47-1e905a8e72a6".into(),
            flow: "xtls-rprx-vision".into(),
            public_key: "abc_-123".into(),
            short_id: "0123456789abcdef".into(),
            server_name: "www.example.com".into(),
            client_fingerprint: "chrome".into(),
        }
    }

    #[test]
    fn clash_and_raw_mixed_golden() {
        let proxies = [ss(), vless()];
        let y = clash_yaml(&proxies);
        assert!(y.contains("type: ss"));
        assert!(y.contains("password: \"SPSK:UPSK\""));
        assert!(y.contains("type: vless"));
        assert!(y.contains("reality-opts"));
        assert!(y.contains("short-id: \"0123456789abcdef\""));
        let decoded = String::from_utf8(STANDARD.decode(raw(&proxies)).unwrap()).unwrap();
        assert!(decoded.contains("ss://"));
        assert!(
            decoded.contains("vless://8f13c901-99e8-43e9-ad47-1e905a8e72a6@[2001:db8::1]:19736")
        );
        assert!(decoded.contains("security=reality"));
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
