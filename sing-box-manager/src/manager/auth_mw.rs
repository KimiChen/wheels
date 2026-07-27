//! 管理面认证中间件。层序（外→内，后加的 route_layer 先执行）：session → csrf → role → reauth → handler。
//! session_mw 需 State（pool/auth），用 `from_fn_with_state`；其余读 extensions，用 `from_fn`。

use axum::extract::{Request, State};
use axum::http::{Method, StatusCode};
use axum::middleware::Next;
use axum::response::{IntoResponse, Response};
use axum::Json;
use serde_json::json;

use crate::manager::auth::{parse_cookie, AuthCtx, Role};
use crate::manager::http::AppState;
use crate::store::{admins, sessions};

/// 会话内携带的 CSRF 期望值（供 csrf_mw 读取，避免再查库）。
#[derive(Clone)]
pub struct CsrfHash(pub String);

/// 会话中间件：Cookie → 会话校验 → 重载 admin（角色以库为准）→ 滑动续期 → 放 AuthCtx。否则 401。
pub async fn session_mw(State(st): State<AppState>, mut req: Request, next: Next) -> Response {
    let Some(sid) = parse_cookie(req.headers(), &st.auth.cookie_name) else {
        return unauthorized("需要登录");
    };
    let id_hash = crate::pki::sha256_hex(sid.as_bytes());
    let now = crate::store::now_unix();
    let sess = match sessions::lookup_valid(&st.pool, &id_hash, now).await {
        Ok(Some(s)) => s,
        Ok(None) => return unauthorized("会话过期或无效"),
        Err(e) => return e.into_response(),
    };
    let admin = match admins::get_by_id(&st.pool, &sess.admin_id).await {
        Ok(Some(a)) => a,
        Ok(None) => return unauthorized("会话无效"),
        Err(e) => return e.into_response(),
    };
    // 禁用 / 改密（会话早于改密时刻）即失效。
    if admin.disabled || sess.created_at < admin.password_changed_at {
        let _ = sessions::revoke(&st.pool, &id_hash).await;
        return unauthorized("会话已失效");
    }
    let role = Role::parse(&admin.role).unwrap_or(Role::Readonly);
    let _ = sessions::touch(&st.pool, &id_hash, now, st.auth.idle_ttl_secs).await;
    req.extensions_mut()
        .insert(CsrfHash(sess.csrf_hash.clone()));
    req.extensions_mut().insert(AuthCtx {
        admin_id: admin.id,
        username: admin.username,
        role,
        session_id_hash: id_hash,
        last_reauth_at: sess.last_reauth_at,
    });
    next.run(req).await
}

/// CSRF 同步器：不安全方法要求 `X-CSRF-Token`，其 sha256 常量时间匹配会话 csrf_hash。否则 403。
pub async fn csrf_mw(req: Request, next: Next) -> Response {
    if matches!(
        *req.method(),
        Method::POST | Method::PUT | Method::PATCH | Method::DELETE
    ) {
        let provided = req
            .headers()
            .get("x-csrf-token")
            .and_then(|v| v.to_str().ok())
            .map(|s| s.to_string());
        let expected = req.extensions().get::<CsrfHash>().map(|c| c.0.clone());
        match (provided, expected) {
            (Some(tok), Some(exp)) => {
                let got = crate::pki::sha256_hex(tok.as_bytes());
                if !ct_eq(got.as_bytes(), exp.as_bytes()) {
                    return forbidden("CSRF token 不匹配");
                }
            }
            _ => return forbidden("缺少 CSRF token"),
        }
    }
    next.run(req).await
}

/// 角色门：要求 AuthCtx.role >= min。
pub async fn role_gate(min: Role, req: Request, next: Next) -> Response {
    match req.extensions().get::<AuthCtx>() {
        Some(ctx) if ctx.role >= min => next.run(req).await,
        Some(_) => forbidden("权限不足"),
        None => unauthorized("需要登录"),
    }
}

/// re-auth 门：要求 last_reauth_at 在窗口内。否则 401 reauth_required。
pub async fn reauth_gate(window_secs: i64, req: Request, next: Next) -> Response {
    let now = crate::store::now_unix();
    match req.extensions().get::<AuthCtx>() {
        Some(ctx) => {
            let ok = ctx
                .last_reauth_at
                .map(|t| now - t <= window_secs)
                .unwrap_or(false);
            if ok {
                next.run(req).await
            } else {
                (
                    StatusCode::UNAUTHORIZED,
                    Json(json!({"error": {"code": "reauth_required", "message": "敏感操作需重新认证"}})),
                )
                    .into_response()
            }
        }
        None => unauthorized("需要登录"),
    }
}

fn ct_eq(a: &[u8], b: &[u8]) -> bool {
    if a.len() != b.len() {
        return false;
    }
    let mut diff = 0u8;
    for (x, y) in a.iter().zip(b) {
        diff |= x ^ y;
    }
    diff == 0
}

fn unauthorized(msg: &str) -> Response {
    (
        StatusCode::UNAUTHORIZED,
        Json(json!({"error": {"code": "unauthorized", "message": msg}})),
    )
        .into_response()
}

fn forbidden(msg: &str) -> Response {
    (
        StatusCode::FORBIDDEN,
        Json(json!({"error": {"code": "forbidden", "message": msg}})),
    )
        .into_response()
}
