//! 公开订阅端点（§11.2 独立边界，无 admin auth）。GET /sub/{token}：按 token hash 查用户 → 该用户
//! 已授权且 active 的 Route → SS-2022 或 VLESS-Reality 代理 → Clash/raw/HTML。
//! 停用/过期用户返回空代理集。订阅内容是唯一合法出明文密钥处，靠 token 熵+hash+短路兜底。

pub mod generate;

use std::collections::HashMap;

use axum::extract::{Path, Query, State};
use axum::http::{header, HeaderMap, HeaderName, HeaderValue, StatusCode};
use axum::response::{IntoResponse, Response};
use axum::routing::get;
use axum::Router;
use sqlx::{Row, SqlitePool};

use crate::compiler::psk::NODE_SS_METHOD;
use crate::crypto::Cipher;
use crate::domain::topology::ENTRY_PORT;
use crate::domain::user::User;
use crate::error::Result;
use crate::manager::http::AppState;
use crate::store::{secrets, users};
use generate::ProxyInfo;

pub fn add_routes(router: Router<AppState>) -> Router<AppState> {
    router.route("/sub/{token}", get(handle_sub))
}

async fn handle_sub(
    State(st): State<AppState>,
    Path(token): Path<String>,
    headers: HeaderMap,
    Query(q): Query<HashMap<String, String>>,
) -> Response {
    let hash = crate::pki::sha256_hex(token.as_bytes());
    let user = match users::lookup_user_by_token_hash(&st.pool, &hash).await {
        Ok(Some(u)) => u,
        Ok(None) => return (StatusCode::NOT_FOUND, "not found").into_response(),
        Err(e) => return e.into_response(),
    };
    let eligible = user.eligible(crate::store::now_unix());
    let proxies = if eligible {
        user_proxies(&st.pool, &st.cipher, &user.id)
            .await
            .unwrap_or_default()
    } else {
        Vec::new() // 停用/过期 → 空代理集
    };
    // Phase 5：真实当前周期用量。
    let (up, down) = {
        use crate::domain::user::ResetCycle;
        use crate::manager::metering::period::period_for;
        let rd = crate::store::metering::reset_day(&st.pool)
            .await
            .unwrap_or(1);
        let cycle = ResetCycle::parse(&user.reset_cycle).unwrap_or(ResetCycle::Monthly);
        let period = period_for(crate::store::now_unix(), rd, cycle);
        crate::store::metering::period_usage(&st.pool, &user.id, &period)
            .await
            .unwrap_or((0, 0))
    };

    let target = q.get("target").map(String::as_str);
    let ua = headers
        .get(header::USER_AGENT)
        .and_then(|v| v.to_str().ok())
        .unwrap_or("");

    if generate::wants_html(target, ua) {
        return sub_response(
            "text/html; charset=utf-8",
            page_html(&user, &proxies, eligible, up + down),
            &user,
            up,
            down,
        );
    }
    if generate::wants_clash(target, ua) {
        return sub_response(
            "text/yaml; charset=utf-8",
            generate::clash_yaml(&proxies),
            &user,
            up,
            down,
        );
    }
    sub_response(
        "text/plain; charset=utf-8",
        generate::raw(&proxies),
        &user,
        up,
        down,
    )
}

/// 组装某用户的订阅代理（解封 SS 凭据或 VLESS UUID/short ID；明文仅内存）。
async fn user_proxies(pool: &SqlitePool, cipher: &Cipher, user_id: &str) -> Result<Vec<ProxyInfo>> {
    let rows = sqlx::query(
        "SELECT r.label AS label,e.public_address AS server,e.ss_method AS ss_method,
                e.inbound_kind AS inbound_kind,e.id AS entry_id,
                ur.upsk_credential_id AS credential_id
         FROM user_routes ur JOIN routes r ON r.id=ur.route_id JOIN entries e ON e.id=r.entry_id
         WHERE ur.user_id=? AND r.status='active'
         ORDER BY r.label",
    )
    .bind(user_id)
    .fetch_all(pool)
    .await?;
    let mut out = Vec::new();
    for row in &rows {
        let entry_id: String = row.get("entry_id");
        let cid: Option<String> = row.get("credential_id");
        let credential = match cid {
            Some(c) => match secrets::open_credential(pool, cipher, &c).await? {
                Some(p) => p,
                None => continue,
            },
            None => continue,
        };
        let inbound_kind: String = row.get("inbound_kind");
        if inbound_kind == "vless-reality" {
            let Some(reality) = crate::store::reality::load(pool, cipher, &entry_id).await? else {
                continue;
            };
            out.push(ProxyInfo::VlessReality {
                label: row.get("label"),
                server: row.get("server"),
                port: ENTRY_PORT,
                uuid: credential,
                flow: reality.config.flow,
                public_key: reality.config.public_key,
                short_id: reality.secret.short_id.clone(),
                server_name: reality.config.server_name,
                client_fingerprint: reality.config.client_fingerprint,
            });
        } else {
            let method: Option<String> = row.get("ss_method");
            let method = method.unwrap_or_else(|| NODE_SS_METHOD.to_string());
            let server_psk =
                match secrets::open_psk_by_scope(pool, cipher, "entry_psk", &entry_id).await? {
                    Some(p) => p,
                    None => continue,
                };
            out.push(ProxyInfo::Shadowsocks {
                label: row.get("label"),
                server: row.get("server"),
                port: ENTRY_PORT,
                method,
                password: format!("{server_psk}:{credential}"),
            });
        }
    }
    Ok(out)
}

/// 带安全响应头与 subscription-userinfo 的响应（Phase 4 用量恒 0，占位 quota）。
fn sub_response(
    content_type: &str,
    body: String,
    user: &User,
    upload: i64,
    download: i64,
) -> Response {
    let mut resp = (StatusCode::OK, body).into_response();
    let h = resp.headers_mut();
    let set = |h: &mut HeaderMap, k: HeaderName, v: &str| {
        if let Ok(val) = HeaderValue::from_str(v) {
            h.insert(k, val);
        }
    };
    set(h, header::CONTENT_TYPE, content_type);
    set(h, header::CACHE_CONTROL, "no-store");
    set(h, header::CONTENT_SECURITY_POLICY, "default-src 'none'");
    set(h, header::REFERRER_POLICY, "no-referrer");
    set(h, header::X_CONTENT_TYPE_OPTIONS, "nosniff");
    set(
        h,
        HeaderName::from_static("x-robots-tag"),
        "noindex, nofollow",
    );
    set(
        h,
        HeaderName::from_static("subscription-userinfo"),
        &format!(
            "upload={}; download={}; total={}; expire={}",
            upload.max(0),
            download.max(0),
            user.quota_bytes,
            user.expire_at.unwrap_or(0)
        ),
    );
    resp
}

fn escape_html(s: &str) -> String {
    s.replace('&', "&amp;")
        .replace('<', "&lt;")
        .replace('>', "&gt;")
        .replace('"', "&quot;")
        .replace('\'', "&#39;")
}

fn page_html(user: &User, proxies: &[ProxyInfo], eligible: bool, used_bytes: i64) -> String {
    let status = if eligible {
        format!("<span>可用 · {} 条线路</span>", proxies.len())
    } else {
        "<span style=\"color:#c00\">已停用或过期</span>".to_string()
    };
    let gib = |b: i64| b as f64 / (1u64 << 30) as f64;
    let quota_gib = gib(user.quota_bytes);
    let used_gib = gib(used_bytes.max(0));
    let usage_line = if user.quota_bytes > 0 {
        format!("<p>本周期用量：{used_gib:.2} / {quota_gib:.1} GiB</p>")
    } else {
        format!("<p>本周期用量：{used_gib:.2} GiB（无配额上限）</p>")
    };
    let route_list = if proxies.is_empty() {
        "<h2>全部线路</h2><p>暂无可用线路。</p>".to_string()
    } else {
        let items = proxies
            .iter()
            .map(|proxy| {
                let (label, protocol, server, port) = match proxy {
                    ProxyInfo::Shadowsocks {
                        label,
                        server,
                        port,
                        ..
                    } => (label, "Shadowsocks", server, port),
                    ProxyInfo::VlessReality {
                        label,
                        server,
                        port,
                        ..
                    } => (label, "VLESS-Reality", server, port),
                };
                let host = if server.contains(':') && !server.starts_with('[') {
                    format!("[{server}]")
                } else {
                    server.to_string()
                };
                format!(
                    "<li><strong>{}</strong> · {} · <code>{}:{}</code></li>",
                    escape_html(label),
                    protocol,
                    escape_html(&host),
                    port,
                )
            })
            .collect::<Vec<_>>()
            .join("");
        format!("<h2>全部线路</h2><ol>{items}</ol>")
    };
    format!(
        "<!doctype html><html lang=\"zh\"><head><meta charset=\"utf-8\">\
<meta name=\"viewport\" content=\"width=device-width,initial-scale=1\">\
<title>订阅 · {name}</title></head><body style=\"font-family:system-ui;max-width:640px;margin:2rem auto;padding:0 1rem\">\
<h1>订阅 · {name}</h1><p>状态：{status}</p>\
{usage_line}\
{route_list}\
<p>用客户端（Clash/mihomo/sing-box）打开本链接以导入。</p></body></html>",
        name = escape_html(&user.name),
        status = status,
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::domain::host::Capability;
    use crate::domain::topology::{ExitKind, InboundKind, RouteDraft};
    use crate::store::topology::{self, NewEntry};

    #[test]
    fn html_lists_all_routes_without_embedding_credentials() {
        let user = User {
            id: "u1".into(),
            name: "alice".into(),
            quota_bytes: 0,
            reset_cycle: "never".into(),
            expire_at: None,
            disabled: false,
            created_at: 0,
            updated_at: 0,
        };
        let proxies = vec![
            ProxyInfo::Shadowsocks {
                label: "hk-direct".into(),
                server: "entry.example.com".into(),
                port: 19736,
                method: "2022-blake3-aes-128-gcm".into(),
                password: "server-secret:user-secret".into(),
            },
            ProxyInfo::VlessReality {
                label: "jp-reality".into(),
                server: "2001:db8::1".into(),
                port: 19736,
                uuid: "8f13c901-99e8-43e9-ad47-1e905a8e72a6".into(),
                flow: "xtls-rprx-vision".into(),
                public_key: "public-key".into(),
                short_id: "0123456789abcdef".into(),
                server_name: "example.com".into(),
                client_fingerprint: "chrome".into(),
            },
        ];

        let html = page_html(&user, &proxies, true, 0);
        assert!(html.contains("可用 · 2 条线路"));
        assert!(html.contains("<h2>全部线路</h2>"));
        assert!(html.contains("hk-direct"));
        assert!(html.contains("Shadowsocks"));
        assert!(html.contains("entry.example.com:19736"));
        assert!(html.contains("jp-reality"));
        assert!(html.contains("VLESS-Reality"));
        assert!(html.contains("[2001:db8::1]:19736"));
        assert!(!html.contains("server-secret"));
        assert!(!html.contains("8f13c901-99e8-43e9-ad47-1e905a8e72a6"));
    }

    #[tokio::test]
    async fn loads_active_vless_proxy_from_encrypted_store() {
        let path = std::env::temp_dir().join(format!("sbm-sub-vless-{}.db", uuid::Uuid::new_v4()));
        let pool = crate::store::open(&path.to_string_lossy()).await.unwrap();
        let cipher = Cipher::from_raw(1, &[7u8; 32]).unwrap();
        let entry_host =
            crate::store::hosts::create_host(&pool, "entry", None, &[Capability::Entry])
                .await
                .unwrap();
        let node_host = crate::store::hosts::create_host(&pool, "node", None, &[Capability::Node])
            .await
            .unwrap();
        let entry = topology::create_entry(
            &pool,
            &cipher,
            &NewEntry {
                host_id: &entry_host,
                public_address: "entry.example.com",
                inbound_kind: InboundKind::VlessReality,
                ss_method: None,
                allow_direct: false,
            },
        )
        .await
        .unwrap();
        crate::store::reality::ensure(
            &pool,
            &cipher,
            &entry,
            "xtls-rprx-vision",
            "www.example.com",
            "www.example.com",
            443,
            "chrome",
        )
        .await
        .unwrap();
        let node = topology::create_node(&pool, &cipher, &node_host, "node.example.com", true)
            .await
            .unwrap();
        let route = topology::insert_route(
            &pool,
            &RouteDraft {
                id: None,
                label: "reality-route".into(),
                entry_id: entry,
                hops: vec![],
                exit_kind: ExitKind::Node,
                exit_node_id: Some(node),
                exit_landing_id: None,
            },
        )
        .await
        .unwrap();
        sqlx::query("UPDATE routes SET status='active' WHERE id=?")
            .bind(&route)
            .execute(&pool)
            .await
            .unwrap();
        let (user, _) = users::create_user(&pool, "alice", 0, "never", None)
            .await
            .unwrap();
        users::grant_route(&pool, &cipher, &user, &route)
            .await
            .unwrap();

        let proxies = user_proxies(&pool, &cipher, &user).await.unwrap();
        assert_eq!(proxies.len(), 1);
        match &proxies[0] {
            ProxyInfo::VlessReality {
                uuid,
                public_key,
                short_id,
                ..
            } => {
                assert!(uuid::Uuid::parse_str(uuid).is_ok());
                assert!(!public_key.is_empty());
                assert_eq!(short_id.len(), 16);
            }
            ProxyInfo::Shadowsocks { .. } => panic!("应生成 VLESS 代理"),
        }
        pool.close().await;
        let _ = std::fs::remove_file(path);
    }
}
