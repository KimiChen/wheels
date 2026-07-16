//! Manager Web/API 路由（axum）。默认脱敏：除 enrollment 一次性响应外，绝不返回私钥明文。
//! Agent API 与 Web API 使用不同认证边界——本路由是 Web/API 面（9736），非 Agent mTLS 面。

use std::sync::Arc;

use axum::extract::{Path, State};
use axum::http::StatusCode;
use axum::response::{IntoResponse, Response};
use axum::routing::{get, post};
use axum::{Json, Router};
use serde::Deserialize;
use serde_json::json;
use sqlx::SqlitePool;

use crate::crypto::Cipher;
use crate::domain::agent::TrustStatus;
use crate::domain::host::Capability;
use crate::error::{AppError, ErrorCode};
use crate::manager::{gate, pki_ops};
use crate::store;

#[derive(Clone)]
pub struct AppState {
    pub pool: SqlitePool,
    pub cipher: Arc<Cipher>,
    pub freshness_secs: i64,
    /// Phase 6：管理面认证配置。
    pub auth: Arc<crate::config::AuthConfig>,
    /// 进程启动时刻（uptime 指标）。
    pub started_at: i64,
}

/// 统一错误响应。消息不含密钥明文（各错误构造点已保证）。
impl IntoResponse for AppError {
    fn into_response(self) -> Response {
        let status = match self.code {
            ErrorCode::Validation => StatusCode::BAD_REQUEST,
            ErrorCode::NotFound => StatusCode::NOT_FOUND,
            ErrorCode::Conflict => StatusCode::CONFLICT,
            ErrorCode::Unauthorized => StatusCode::UNAUTHORIZED,
            ErrorCode::Forbidden => StatusCode::FORBIDDEN,
            _ => StatusCode::INTERNAL_SERVER_ERROR,
        };
        if status == StatusCode::INTERNAL_SERVER_ERROR {
            tracing::error!(error = %self, "请求处理失败");
        }
        let body = Json(json!({"error": {"code": self.code.as_str(), "message": self.message}}));
        (status, body).into_response()
    }
}

type ApiResult = std::result::Result<Json<serde_json::Value>, AppError>;

pub fn router(state: AppState) -> Router {
    use crate::manager::auth;
    use crate::manager::auth::Role;
    use crate::manager::auth_mw::{csrf_mw, reauth_gate, role_gate, session_mw};
    use axum::middleware::{from_fn, from_fn_with_state};

    // ── 公开面：无任何会话认证 ──────────────────────────────
    let mut public = Router::new()
        .route("/healthz", get(healthz))
        .route("/api/auth/setup", post(auth::setup)) // 仅 admins 为空时放行
        .route("/api/auth/login", post(auth::login));
    public = crate::subscription::add_routes(public); // /sub/{token} 保持公开
    public = crate::manager::observ_http::add_public_routes(public); // /metrics /readyz 自带守卫

    // ── 自助面：任意已登录角色（会话 + CSRF，无角色/写门）──
    let self_group = Router::new()
        .route("/api/auth/whoami", get(auth::whoami))
        .route("/api/auth/logout", post(auth::logout))
        .route("/api/auth/reauth", post(auth::reauth));

    // ── 管理面：admin 角色 + re-auth（信任根/发牌/管理员/会话管理）──
    let reauth_window = state.auth.reauth_window_secs;
    let admin_group = Router::new()
        .route("/api/pki/init", post(pki_init))
        .route("/api/hosts/{id}/enrollment", post(issue_enrollment))
        .route("/api/hosts/{id}/trust", post(set_trust))
        .route(
            "/api/admins",
            get(auth::list_admins).post(auth::create_admin),
        )
        .route("/api/admins/{id}", axum::routing::patch(auth::update_admin))
        .route("/api/admins/{id}/sessions", get(auth::list_sessions))
        .route_layer(from_fn(move |req, next| {
            reauth_gate(reauth_window, req, next)
        }))
        .route_layer(from_fn(move |req, next| role_gate(Role::Admin, req, next)));

    // ── 通用业务面：readonly 可 GET，operator+ 可写（rw 门）──
    let mut general = Router::new()
        .route("/api/hosts", get(list_hosts).post(create_host))
        .route("/api/hosts/{id}", get(host_detail))
        .route("/api/hosts/{id}/readiness", get(readiness))
        .route("/api/preflight", post(preflight))
        .route("/api/audit", get(auth::list_audit));
    general = crate::manager::topology_http::add_routes(general);
    general = crate::manager::deploy_http::add_routes(general);
    general = crate::manager::users_http::add_routes(general);
    general = crate::manager::traffic_http::add_routes(general);
    general = crate::manager::observ_http::add_readonly_routes(general); // /api/metrics /api/health
    let general = general.route_layer(from_fn(rw_gate));

    // 受保护面统一挂 CSRF（写方法）+ session（最外，最先）。
    let protected = self_group
        .merge(admin_group)
        .merge(general)
        .route_layer(from_fn(csrf_mw))
        .route_layer(from_fn_with_state(state.clone(), session_mw));

    public.merge(protected).with_state(state)
}

/// readonly 只读、operator+ 可写的通用门（补 Phase 6 RBAC；敏感操作另走 admin_group）。
async fn rw_gate(req: axum::extract::Request, next: axum::middleware::Next) -> Response {
    use crate::manager::auth::{AuthCtx, Role};
    use axum::http::Method;
    let is_write = matches!(
        *req.method(),
        Method::POST | Method::PUT | Method::PATCH | Method::DELETE
    );
    match req.extensions().get::<AuthCtx>() {
        Some(ctx) if !is_write || ctx.role >= Role::Operator => next.run(req).await,
        Some(_) => (
            StatusCode::FORBIDDEN,
            Json(json!({"error": {"code": "forbidden", "message": "只读角色不可写"}})),
        )
            .into_response(),
        None => (
            StatusCode::UNAUTHORIZED,
            Json(json!({"error": {"code": "unauthorized", "message": "需要登录"}})),
        )
            .into_response(),
    }
}

async fn healthz() -> &'static str {
    "ok"
}

async fn pki_init(State(st): State<AppState>) -> ApiResult {
    pki_ops::bootstrap(&st.pool, &st.cipher).await?;
    Ok(Json(json!({"ok": true})))
}

#[derive(Deserialize)]
struct CreateHostReq {
    name: String,
    note: Option<String>,
    capabilities: Vec<String>,
}

async fn create_host(State(st): State<AppState>, Json(req): Json<CreateHostReq>) -> ApiResult {
    let caps: Vec<Capability> = req
        .capabilities
        .iter()
        .filter_map(|c| Capability::parse(c))
        .collect();
    if caps.is_empty() {
        return Err(AppError::new(ErrorCode::Validation, "至少一个有效能力"));
    }
    let id = store::hosts::create_host(&st.pool, &req.name, req.note.as_deref(), &caps).await?;
    Ok(Json(json!({"id": id})))
}

async fn list_hosts(State(st): State<AppState>) -> ApiResult {
    let hosts = store::hosts::list_hosts(&st.pool).await?;
    Ok(Json(json!({ "hosts": hosts })))
}

async fn host_detail(State(st): State<AppState>, Path(id): Path<String>) -> ApiResult {
    let host = store::hosts::get_host(&st.pool, &id)
        .await?
        .ok_or_else(|| AppError::new(ErrorCode::NotFound, "host 不存在"))?;
    let caps = store::hosts::capabilities(&st.pool, &id).await?;
    let agent = store::agents::get_agent(&st.pool, &id).await?;
    let cert = store::pki::agent_cert_info(&st.pool, &id).await?;
    Ok(Json(json!({
        "host": host,
        "capabilities": caps.iter().map(|c| c.as_str()).collect::<Vec<_>>(),
        // 去规范化 Agent 观测；无任何密钥字段。
        "agent": agent.map(|a| json!({
            "mgmt_address": a.mgmt_address,
            "status": a.status,
            "singbox_version": a.singbox_version,
            "agent_version": a.agent_version,
            "current_revision": a.current_revision,
            "singbox_running": a.singbox_running,
            "last_polled_at": a.last_polled_at,
            "last_ok_at": a.last_ok_at,
            "last_error": a.last_error,
            "consecutive_failures": a.consecutive_failures,
        })),
        // 证书只回指纹与有效期，不回 PEM。
        "certificate": cert.map(|c| json!({
            "trust_status": c.trust_status,
            "spki_sha256": c.spki_sha256,
            "not_after": c.not_after,
        })),
    })))
}

#[derive(Deserialize)]
struct EnrollReq {
    mgmt_bind: String,
}

async fn issue_enrollment(
    State(st): State<AppState>,
    Path(id): Path<String>,
    Json(req): Json<EnrollReq>,
) -> ApiResult {
    let issued = pki_ops::build_enrollment(&st.pool, &st.cipher, &id, &req.mgmt_bind).await?;
    let _ = store::agents::insert_health_event(
        &st.pool,
        Some(&id),
        "enrollment_issued",
        Some(&issued.fingerprint),
    )
    .await;
    // 唯一返回私钥明文的响应：管理员须带外核对 fingerprint 后落地为 0600 文件。
    Ok(Json(json!({
        "fingerprint": issued.fingerprint,
        "package": issued.package,
    })))
}

#[derive(Deserialize)]
struct TrustReq {
    trust: String,
}

async fn set_trust(
    State(st): State<AppState>,
    Path(id): Path<String>,
    Json(req): Json<TrustReq>,
) -> ApiResult {
    let t = TrustStatus::parse(&req.trust)
        .ok_or_else(|| AppError::new(ErrorCode::Validation, "trust 取值非法"))?;
    store::pki::set_trust(&st.pool, &id, t).await?;
    Ok(Json(json!({"host_id": id, "trust": t.as_str()})))
}

async fn readiness(State(st): State<AppState>, Path(id): Path<String>) -> ApiResult {
    let r = gate::host_readiness(&st.pool, &id, store::now_unix(), st.freshness_secs).await?;
    Ok(Json(serde_json::to_value(r).unwrap_or(json!({}))))
}

#[derive(Deserialize)]
struct PreflightReq {
    host_ids: Vec<String>,
}

async fn preflight(State(st): State<AppState>, Json(req): Json<PreflightReq>) -> ApiResult {
    let blocked = gate::preflight(
        &st.pool,
        &req.host_ids,
        store::now_unix(),
        st.freshness_secs,
    )
    .await?;
    Ok(Json(json!({
        "publishable": blocked.is_empty(),
        "blocked": blocked,
    })))
}

#[cfg(test)]
mod auth_tests {
    use super::*;
    use axum::body::Body;
    use axum::http::{header, Request, StatusCode};
    use base64::{engine::general_purpose::STANDARD, Engine};
    use tower::ServiceExt;

    async fn app() -> (Router, SqlitePool) {
        std::env::set_var("ENCRYPTION_MASTER_KEY", STANDARD.encode([9u8; 32]));
        let path = std::env::temp_dir().join(format!("sbm-authhttp-{}.db", uuid::Uuid::new_v4()));
        let pool = store::open(&path.to_string_lossy()).await.unwrap();
        let state = AppState {
            pool: pool.clone(),
            cipher: Arc::new(Cipher::from_env(1).unwrap()),
            freshness_secs: 90,
            auth: Arc::new(crate::config::AuthConfig {
                secure_cookie: false,
                ..crate::config::AuthConfig::default()
            }),
            started_at: store::now_unix(),
        };
        (router(state), pool)
    }

    async fn body_json(resp: Response) -> serde_json::Value {
        let bytes = axum::body::to_bytes(resp.into_body(), usize::MAX)
            .await
            .unwrap();
        serde_json::from_slice(&bytes).unwrap_or(json!({}))
    }

    fn session_cookie(resp: &Response) -> String {
        let sc = resp
            .headers()
            .get_all(header::SET_COOKIE)
            .iter()
            .find_map(|v| {
                let s = v.to_str().ok()?;
                s.starts_with("sbm_session=").then(|| s.to_string())
            })
            .expect("有 Set-Cookie");
        // 取 name=value 首段。
        sc.split(';').next().unwrap().to_string()
    }

    #[tokio::test]
    async fn full_auth_flow_401_login_csrf_and_rbac() {
        let (app, _pool) = app().await;

        // /healthz 公开。
        let r = app
            .clone()
            .oneshot(Request::get("/healthz").body(Body::empty()).unwrap())
            .await
            .unwrap();
        assert_eq!(r.status(), StatusCode::OK);

        // 未登录 /api/hosts → 401。
        let r = app
            .clone()
            .oneshot(Request::get("/api/hosts").body(Body::empty()).unwrap())
            .await
            .unwrap();
        assert_eq!(r.status(), StatusCode::UNAUTHORIZED);

        // setup 首个 admin。
        let r = app
            .clone()
            .oneshot(
                Request::post("/api/auth/setup")
                    .header(header::CONTENT_TYPE, "application/json")
                    .body(Body::from(
                        r#"{"username":"root","password":"supersecret123"}"#,
                    ))
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(r.status(), StatusCode::CREATED);
        // setup 第二次 → 409。
        let r = app
            .clone()
            .oneshot(
                Request::post("/api/auth/setup")
                    .header(header::CONTENT_TYPE, "application/json")
                    .body(Body::from(
                        r#"{"username":"x","password":"supersecret123"}"#,
                    ))
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(r.status(), StatusCode::CONFLICT);

        // login → Cookie + csrf_token。
        let r = app
            .clone()
            .oneshot(
                Request::post("/api/auth/login")
                    .header(header::CONTENT_TYPE, "application/json")
                    .body(Body::from(
                        r#"{"username":"root","password":"supersecret123"}"#,
                    ))
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(r.status(), StatusCode::OK);
        let cookie = session_cookie(&r);
        let jb = body_json(r).await;
        let csrf = jb["csrf_token"].as_str().unwrap().to_string();
        assert_eq!(jb["role"], "admin");

        // 错误密码 → 401。
        let r = app
            .clone()
            .oneshot(
                Request::post("/api/auth/login")
                    .header(header::CONTENT_TYPE, "application/json")
                    .body(Body::from(
                        r#"{"username":"root","password":"wrongpassword1"}"#,
                    ))
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(r.status(), StatusCode::UNAUTHORIZED);

        // 带 Cookie GET /api/hosts → 200。
        let r = app
            .clone()
            .oneshot(
                Request::get("/api/hosts")
                    .header(header::COOKIE, &cookie)
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(r.status(), StatusCode::OK);

        // 写但无 CSRF → 403。
        let r = app
            .clone()
            .oneshot(
                Request::post("/api/hosts")
                    .header(header::COOKIE, &cookie)
                    .header(header::CONTENT_TYPE, "application/json")
                    .body(Body::from(r#"{"name":"h","capabilities":["entry"]}"#))
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(r.status(), StatusCode::FORBIDDEN);

        // 写 + CSRF（admin 角色）→ 通过认证（201）。
        let r = app
            .clone()
            .oneshot(
                Request::post("/api/hosts")
                    .header(header::COOKIE, &cookie)
                    .header("x-csrf-token", &csrf)
                    .header(header::CONTENT_TYPE, "application/json")
                    .body(Body::from(r#"{"name":"h","capabilities":["entry"]}"#))
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(r.status(), StatusCode::OK);
    }

    #[tokio::test]
    async fn readonly_cannot_write_and_sub_is_public() {
        let (app, pool) = app().await;
        // 直接建 readonly 管理员。
        let hash = crate::manager::auth::hash_password("supersecret123").unwrap();
        store::admins::create(&pool, "ro", &hash, "readonly")
            .await
            .unwrap();

        let r = app
            .clone()
            .oneshot(
                Request::post("/api/auth/login")
                    .header(header::CONTENT_TYPE, "application/json")
                    .body(Body::from(
                        r#"{"username":"ro","password":"supersecret123"}"#,
                    ))
                    .unwrap(),
            )
            .await
            .unwrap();
        let cookie = session_cookie(&r);
        let csrf = body_json(r).await["csrf_token"]
            .as_str()
            .unwrap()
            .to_string();

        // readonly GET ok。
        let r = app
            .clone()
            .oneshot(
                Request::get("/api/hosts")
                    .header(header::COOKIE, &cookie)
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(r.status(), StatusCode::OK);

        // readonly 写（有 CSRF）→ 403（rw 门）。
        let r = app
            .clone()
            .oneshot(
                Request::post("/api/hosts")
                    .header(header::COOKIE, &cookie)
                    .header("x-csrf-token", &csrf)
                    .header(header::CONTENT_TYPE, "application/json")
                    .body(Body::from(r#"{"name":"h","capabilities":["entry"]}"#))
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(r.status(), StatusCode::FORBIDDEN);

        // readonly 打 admin 面（pki/init）→ 403。
        let r = app
            .clone()
            .oneshot(
                Request::post("/api/pki/init")
                    .header(header::COOKIE, &cookie)
                    .header("x-csrf-token", &csrf)
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(r.status(), StatusCode::FORBIDDEN);

        // /sub/{token} 公开（无 cookie）→ 非 401（无此 token 返回 404）。
        let r = app
            .clone()
            .oneshot(Request::get("/sub/nope").body(Body::empty()).unwrap())
            .await
            .unwrap();
        assert_ne!(r.status(), StatusCode::UNAUTHORIZED);
        pool.close().await;
    }
}
