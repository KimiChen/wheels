//! 订阅 HTTP API（axum）。
//!   GET /sub/{token} → 浏览器显示双语设备导入页；命令行/客户端按 UA 返回 base64 或 Clash YAML。
//!   GET /status       → 只读 JSON：每用户本期用量/配额/有效期/是否停用。
//! 读取 ArcSwap 快照，支持热重载与优雅停机。

use crate::app::{AppData, Shared};
use crate::db;
use crate::meter::{current_period, usage_period};
use crate::sub_page::{preferred_language, render_page, wants_html, PageData};
use anyhow::Result;
use axum::{
    extract::{Path as AxPath, Query, State},
    http::{header, HeaderMap, StatusCode},
    response::{IntoResponse, Response},
    routing::get,
    Json, Router,
};
use base64::engine::general_purpose::{STANDARD, URL_SAFE_NO_PAD};
use base64::Engine;
use percent_encoding::{utf8_percent_encode, NON_ALPHANUMERIC};
use sqlx::SqlitePool;
use std::collections::HashMap;
use std::sync::Arc;
use time::OffsetDateTime;
use tokio_util::sync::CancellationToken;

pub struct SubState {
    pub shared: Shared,
    pub pool: SqlitePool,
}

pub async fn serve(
    shared: Shared,
    pool: SqlitePool,
    listen: String,
    cancel: CancellationToken,
) -> Result<()> {
    let state = Arc::new(SubState { shared, pool });
    let app = Router::new()
        .route("/sub/{token}", get(handle_sub))
        .route("/status", get(handle_status))
        .with_state(state);
    let listener = tokio::net::TcpListener::bind(&listen).await?;
    println!("subscription API: http://{listen}/sub/{{token}}  · status: http://{listen}/status");
    axum::serve(listener, app)
        .with_graceful_shutdown(async move { cancel.cancelled().await })
        .await?;
    Ok(())
}

/// 出口 -> 中文名。文本后缀始终保留；数字后缀仅在同地区多节点时用于消歧。
pub fn label(exit: &str, dup: bool) -> String {
    if let Some((region, suffix)) = home_parts(exit) {
        let base = format!("{}家宽", region_name(region));
        let suffix = suffix.trim_start_matches(['-', '_', '.']);
        if suffix.is_empty() || (suffix.chars().all(|c| c.is_ascii_digit()) && !dup) {
            return base;
        }
        return if suffix.chars().all(|c| c.is_ascii_digit()) {
            format!("{base}{suffix}")
        } else {
            format!("{base}·{suffix}")
        };
    }

    let region = ["hk", "jp", "us"].into_iter().find(|r| exit.contains(r));
    let cn = match region {
        Some("hk") => "香港",
        Some("jp") => "日本",
        Some("us") => "美国",
        _ => "",
    };
    if exit.starts_with("default") {
        "默认·香港VPS".into()
    } else if exit.starts_with("dmit") {
        format!("{cn}·DMIT")
    } else {
        exit.to_string()
    }
}

fn home_parts(exit: &str) -> Option<(&'static str, &str)> {
    if let Some(suffix) = exit.strip_prefix("homehk") {
        Some(("hk", suffix))
    } else if let Some(suffix) = exit.strip_prefix("homejp") {
        Some(("jp", suffix))
    } else {
        exit.strip_prefix("homeus").map(|suffix| ("us", suffix))
    }
}

fn region_name(region: &str) -> &'static str {
    match region {
        "hk" => "香港",
        "jp" => "日本",
        "us" => "美国",
        _ => "",
    }
}

fn ss_uri(method: &str, spsk: &str, upsk: &str, host: &str, port: u16, label: &str) -> String {
    let userinfo = URL_SAFE_NO_PAD.encode(format!("{method}:{spsk}:{upsk}"));
    let tag = utf8_percent_encode(label, NON_ALPHANUMERIC);
    format!("ss://{userinfo}@{host}:{port}#{tag}")
}

fn reality_params(data: &AppData) -> (&str, &str, &str, &str) {
    let sni = data
        .cfg
        .singbox
        .inbound
        .reality_handshake
        .as_deref()
        .unwrap_or("www.microsoft.com");
    let fp = data
        .cfg
        .singbox
        .inbound
        .utls_fingerprint
        .as_deref()
        .unwrap_or("chrome");
    (
        &data.sec.reality.public_key,
        &data.sec.reality.short_id,
        sni,
        fp,
    )
}

fn vless_uri(data: &AppData, user: &str, exit: &str, label: &str) -> String {
    let uuid = &data.sec.access(user, exit).uuid;
    let port = data.cfg.singbox.entry_port;
    let (pbk, sid, sni, fp) = reality_params(data);
    let tag = utf8_percent_encode(label, NON_ALPHANUMERIC);
    format!(
        "vless://{uuid}@{}:{port}?encryption=none&security=reality&flow=xtls-rprx-vision&pbk={pbk}&sid={sid}&sni={sni}&fp={fp}&type=tcp#{tag}",
        data.host
    )
}

fn clash_vless(nodes: &[(String, String)], data: &AppData, user: &str) -> String {
    let (pbk, sid, sni, fp) = reality_params(data);
    let port = data.cfg.singbox.entry_port;
    let mut s = String::from("proxies:\n");
    let mut names = Vec::new();
    for (exit, label) in nodes {
        let uuid = &data.sec.access(user, exit).uuid;
        s.push_str(&format!(
            "  - {{name: {label:?}, type: vless, server: {}, port: {port}, uuid: {uuid}, flow: xtls-rprx-vision, network: tcp, tls: true, servername: {sni}, reality-opts: {{public-key: {pbk}, short-id: {sid}}}, client-fingerprint: {fp}}}\n",
            data.host
        ));
        names.push(format!("{label:?}"));
    }
    s.push_str("proxy-groups:\n");
    s.push_str(&format!(
        "  - {{name: PROXY, type: select, proxies: [{}]}}\n",
        names.join(", ")
    ));
    s.push_str("rules:\n  - MATCH,PROXY\n");
    s
}

fn clash_yaml(nodes: &[(String, String)], data: &AppData, user: &str) -> String {
    let method = &data.method;
    let host = &data.host;
    let port = data.cfg.singbox.entry_port;
    let mut s = String::from("proxies:\n");
    let mut names = Vec::new();
    for (exit, label) in nodes {
        let pw = format!(
            "{}:{}",
            data.sec.server_psk,
            data.sec.access(user, exit).upsk
        );
        s.push_str(&format!(
            "  - {{name: {label:?}, type: ss, server: {host}, port: {port}, cipher: {method}, password: {pw:?}, udp: true}}\n"
        ));
        names.push(format!("{label:?}"));
    }
    s.push_str("proxy-groups:\n");
    s.push_str(&format!(
        "  - {{name: PROXY, type: select, proxies: [{}]}}\n",
        names.join(", ")
    ));
    s.push_str("rules:\n  - MATCH,PROXY\n");
    s
}

fn wants_clash(target: Option<&str>, user_agent: &str) -> bool {
    let ua = user_agent.to_ascii_lowercase();
    target == Some("clash")
        || (target.is_none()
            && (ua.contains("clash") || ua.contains("mihomo") || ua.contains("stash")))
}

async fn handle_sub(
    State(st): State<Arc<SubState>>,
    AxPath(token): AxPath<String>,
    headers: HeaderMap,
    Query(q): Query<HashMap<String, String>>,
) -> Response {
    let data = st.shared.load_full();
    let Some(user) = data.tokens.get(&token) else {
        return (StatusCode::NOT_FOUND, "not found").into_response();
    };
    let Some(ucfg) = data.cfg.users.get(user) else {
        return (StatusCode::NOT_FOUND, "not found").into_response();
    };

    let now = OffsetDateTime::now_utc();
    let period = usage_period(now, data.cfg.service.reset_day, ucfg.reset);
    let (up, down) = db::period_usage(&st.pool, user, &period)
        .await
        .unwrap_or((0, 0));
    let (quota, expire) = db::user_limits(&st.pool, user).await.unwrap_or((0, None));
    let disabled = db::get_disabled(&st.pool, user).await.unwrap_or(false);
    let subscription_url = format!(
        "{}/{}",
        data.cfg.service.sub_base_url.trim_end_matches('/'),
        token
    );
    let target = q.get("target").map(String::as_str);
    let user_agent = headers
        .get(header::USER_AGENT)
        .and_then(|value| value.to_str().ok())
        .unwrap_or("");

    if wants_html(user_agent, target) {
        let accept_language = headers
            .get(header::ACCEPT_LANGUAGE)
            .and_then(|value| value.to_str().ok())
            .unwrap_or("");
        let language = preferred_language(accept_language, q.get("lang").map(String::as_str));
        let page = render_page(
            &PageData {
                username: user,
                subscription_url: &subscription_url,
                upload: up.max(0) as u64,
                download: down.max(0) as u64,
                quota: quota.max(0) as u64,
                expire,
                reset: ucfg.reset.as_str(),
                period: &period,
                disabled,
            },
            user_agent,
            language,
        );
        let mut response_headers = HeaderMap::new();
        response_headers.insert(
            header::CONTENT_TYPE,
            "text/html; charset=utf-8".parse().unwrap(),
        );
        response_headers.insert(header::CACHE_CONTROL, "no-store".parse().unwrap());
        response_headers.insert(
            "x-robots-tag",
            "noindex, nofollow, noarchive".parse().unwrap(),
        );
        response_headers.insert("referrer-policy", "no-referrer".parse().unwrap());
        response_headers.insert("x-content-type-options", "nosniff".parse().unwrap());
        response_headers.insert("x-frame-options", "DENY".parse().unwrap());
        response_headers.insert(
            "content-security-policy",
            "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; img-src data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
                .parse()
                .unwrap(),
        );
        return (response_headers, page).into_response();
    }

    let mine: Vec<String> = data
        .cfg
        .all_exits()
        .into_iter()
        .filter(|exit| ucfg.exits.contains(exit))
        .collect();

    // 同地区家宽计数（标签消歧）
    let mut region_home: HashMap<&str, u32> = HashMap::new();
    for e in &mine {
        if let Some((region, _)) = home_parts(e) {
            *region_home.entry(region).or_default() += 1;
        }
    }
    let lbl = |e: &str| {
        let dup = home_parts(e)
            .map(|(region, _)| region_home.get(region).copied().unwrap_or(0) > 1)
            .unwrap_or(false);
        label(e, dup)
    };

    let spsk = &data.sec.server_psk;

    let want_clash = wants_clash(target, user_agent);

    let vless = data.cfg.singbox.inbound.kind == "vless-reality";
    let (body, ctype) = if want_clash {
        let nodes: Vec<(String, String)> =
            mine.iter().map(|exit| (exit.clone(), lbl(exit))).collect();
        let yaml = if vless {
            clash_vless(&nodes, &data, user)
        } else {
            clash_yaml(&nodes, &data, user)
        };
        (yaml, "text/yaml; charset=utf-8")
    } else {
        let uris: Vec<String> = if vless {
            mine.iter()
                .map(|exit| vless_uri(&data, user, exit, &lbl(exit)))
                .collect()
        } else {
            mine.iter()
                .map(|exit| {
                    let upsk = &data.sec.access(user, exit).upsk;
                    ss_uri(
                        &data.method,
                        spsk,
                        upsk,
                        &data.host,
                        data.cfg.singbox.entry_port,
                        &lbl(exit),
                    )
                })
                .collect()
        };
        (
            STANDARD.encode(uris.join("\n")),
            "text/plain; charset=utf-8",
        )
    };

    let mut ui = format!(
        "upload={}; download={}; total={}",
        up.max(0),
        down.max(0),
        quota.max(0)
    );
    if let Some(e) = expire {
        ui.push_str(&format!("; expire={e}"));
    }
    let mut h = HeaderMap::new();
    h.insert(header::CONTENT_TYPE, ctype.parse().unwrap());
    h.insert(header::CACHE_CONTROL, "no-store".parse().unwrap());
    h.insert("access-control-allow-origin", "*".parse().unwrap());
    h.insert(
        "x-robots-tag",
        "noindex, nofollow, noarchive".parse().unwrap(),
    );
    h.insert("x-content-type-options", "nosniff".parse().unwrap());
    h.insert("subscription-userinfo", ui.parse().unwrap());
    h.insert("profile-update-interval", "24".parse().unwrap());
    if let Ok(value) = subscription_url.parse() {
        h.insert("profile-web-page-url", value);
    }
    h.insert(
        "profile-title",
        format!("base64:{}", STANDARD.encode(user)).parse().unwrap(),
    );
    h.insert(
        header::CONTENT_DISPOSITION,
        "attachment; filename=\"subscription.txt\"".parse().unwrap(),
    );
    (h, body).into_response()
}

async fn handle_status(State(st): State<Arc<SubState>>) -> Response {
    let data = st.shared.load_full();
    let now = OffsetDateTime::now_utc();
    // 保留顶层月度 period 字段兼容旧调用方；混合周期以每个用户的 period/reset 为准。
    let period = current_period(now, data.cfg.service.reset_day);
    let mut out = Vec::new();
    for (name, user) in &data.cfg.users {
        let user_period = usage_period(now, data.cfg.service.reset_day, user.reset);
        let (up, down) = db::period_usage(&st.pool, name, &user_period)
            .await
            .unwrap_or((0, 0));
        let (quota, expire) = db::user_limits(&st.pool, name).await.unwrap_or((0, None));
        let disabled = db::get_disabled(&st.pool, name).await.unwrap_or(false);
        out.push(serde_json::json!({
            "name": name,
            "reset": user.reset.as_str(),
            "period": user_period,
            "used": (up + down).max(0),
            "up": up.max(0),
            "down": down.max(0),
            "total": quota.max(0),
            "expire": expire,
            "disabled": disabled,
        }));
    }
    Json(serde_json::json!({ "period": period, "users": out })).into_response()
}

#[cfg(test)]
mod tests {
    use super::{clash_yaml, label, wants_clash};
    use crate::app::AppData;
    use crate::config::Config;
    use crate::secrets::Secrets;
    use std::collections::HashMap;

    #[test]
    fn labels_numeric_and_named_home_exits() {
        assert_eq!(label("homeus1", false), "美国家宽");
        assert_eq!(label("homeus1", true), "美国家宽1");
        assert_eq!(label("homeusOracle", false), "美国家宽·Oracle");
        assert_eq!(label("homeusVircs", true), "美国家宽·Vircs");
        assert_eq!(label("homejp-Sakura", false), "日本家宽·Sakura");
    }

    #[test]
    fn selects_clash_format_by_target_or_client_user_agent() {
        assert!(wants_clash(Some("clash"), "curl/8.7.1"));
        assert!(wants_clash(None, "Mihomo/1.19"));
        assert!(wants_clash(None, "Stash/2.7"));
        assert!(!wants_clash(Some("raw"), "Mihomo/1.19"));
        assert!(!wants_clash(None, "curl/8.7.1"));
    }

    #[test]
    fn shared_port_subscription_uses_distinct_credential_per_exit() {
        let cfg: Config = toml::from_str(
            r#"
[service]
listen = "127.0.0.1:9736"
public_host = "entry.example.com"
sub_base_url = "https://sub.example.com/sub"
poll_interval = "30s"
reset_day = 1
db_path = "/tmp/sbm.db"

[singbox]
config_out = "/tmp/config.json"
entry_port = 19736
relay_method = "2022-blake3-aes-128-gcm"

[singbox.inbound]
type = "shadowsocks"
method = "2022-blake3-aes-128-gcm"

[backend]
mode = "ssm"
ssm_base = "http://127.0.0.1:8081"

[nodes]
entry = "192.0.2.1"
home = "192.0.2.2"

[exits]
entry = []
home = ["entry"]

[users.alice]
quota = "10G"
expire = "2030-01-01"
exits = ["entry", "home"]
"#,
        )
        .unwrap();
        let sec: Secrets = toml::from_str(
            r#"
server_psk = "entry-key"

[user.alice]
token = "token"

[user.alice.access.entry]
name = "alice-entry"
upsk = "alice-entry-key"
uuid = "00000000-0000-4000-8000-000000000001"

[user.alice.access.home]
name = "alice-home"
upsk = "alice-home-key"
uuid = "00000000-0000-4000-8000-000000000002"
"#,
        )
        .unwrap();
        let data = AppData {
            cfg,
            sec,
            tokens: HashMap::new(),
            method: "2022-blake3-aes-128-gcm".to_string(),
            host: "entry.example.com".to_string(),
        };
        let nodes = vec![
            ("entry".to_string(), "Entry".to_string()),
            ("home".to_string(), "Home".to_string()),
        ];

        let yaml = clash_yaml(&nodes, &data, "alice");
        assert_eq!(yaml.matches("port: 19736").count(), 2);
        assert!(yaml.contains("entry-key:alice-entry-key"));
        assert!(yaml.contains("entry-key:alice-home-key"));
    }
}
