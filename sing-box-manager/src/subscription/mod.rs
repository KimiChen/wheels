//! 公开订阅端点（§11.2 独立边界，无 admin auth）。GET /sub/{token}：按 token hash 查用户 → 该用户
//! 已授权且 active 的 Route → 每条一个 SS-2022 代理(password=serverPSK:userPSK) → Clash/raw/HTML。
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
            page_html(&user, proxies.len(), eligible, up + down),
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

/// 组装某用户的订阅代理（解封 serverPSK + userPSK；明文仅内存）。
async fn user_proxies(pool: &SqlitePool, cipher: &Cipher, user_id: &str) -> Result<Vec<ProxyInfo>> {
    let rows = sqlx::query(
        "SELECT r.label AS label, e.public_address AS server, e.ss_method AS ss_method, e.id AS entry_id, ur.upsk_credential_id AS upsk_cid
         FROM user_routes ur JOIN routes r ON r.id=ur.route_id JOIN entries e ON e.id=r.entry_id
         WHERE ur.user_id=? AND r.status='active' ORDER BY r.label",
    )
    .bind(user_id)
    .fetch_all(pool)
    .await?;
    let mut out = Vec::new();
    for row in &rows {
        let entry_id: String = row.get("entry_id");
        let method: Option<String> = row.get("ss_method");
        let method = method.unwrap_or_else(|| NODE_SS_METHOD.to_string());
        let server_psk =
            match secrets::open_psk_by_scope(pool, cipher, "entry_psk", &entry_id).await? {
                Some(p) => p,
                None => continue,
            };
        let cid: Option<String> = row.get("upsk_cid");
        let user_psk = match cid {
            Some(c) => match secrets::open_credential(pool, cipher, &c).await? {
                Some(p) => p,
                None => continue,
            },
            None => continue,
        };
        out.push(ProxyInfo {
            label: row.get("label"),
            server: row.get("server"),
            port: ENTRY_PORT,
            method,
            password: format!("{server_psk}:{user_psk}"),
        });
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

fn page_html(user: &User, proxy_count: usize, eligible: bool, used_bytes: i64) -> String {
    let status = if eligible {
        format!("<span>可用 · {proxy_count} 条线路</span>")
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
    format!(
        "<!doctype html><html lang=\"zh\"><head><meta charset=\"utf-8\">\
<meta name=\"viewport\" content=\"width=device-width,initial-scale=1\">\
<title>订阅 · {name}</title></head><body style=\"font-family:system-ui;max-width:640px;margin:2rem auto;padding:0 1rem\">\
<h1>订阅 · {name}</h1><p>状态：{status}</p>\
{usage_line}\
<p>用客户端（Clash/mihomo/sing-box）打开本链接以导入。</p></body></html>",
        name = escape_html(&user.name),
        status = status,
    )
}
