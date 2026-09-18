//! 结构化 HTTP API，统一前缀 `/api/v1`（README §6）。
//!
//! **API-first**：Web 是 API 的一个客户端，页面上能做的每件事都有对应的结构化端点。
//! 这不只是架构洁癖——**越权必须在服务端被拒绝**，拿不到的数据根本不该出现在响应里，
//! 而不是「发过来再藏起来」（§4.9）。前端隐藏导航只是让界面讲得通。
//!
//! 两条贯穿全局的口径：
//!
//! - **所有字节字段是十进制字符串**（C3）。前端只做格式化展示；
//!   确需运算时用 `BigInt`，禁止 `Number` 参与字节运算。
//! - **时间是 RFC 3339（UTC）**。

pub mod error;
pub mod routes;
pub mod session;
pub mod trend;

use std::sync::Arc;

use axum::extract::FromRequestParts;
use axum::http::request::Parts;
use axum::http::{header, HeaderValue};
use axum::routing::get;
use axum::Router;

use crate::api::error::{ApiError, ApiResult};
use crate::api::session::Subject;
use crate::store::Store;

#[derive(Clone)]
pub struct AppState {
    pub store: Arc<Store>,
}

/// 从 cookie 解出主体。**每请求求值**（D13），不缓存结论。
impl FromRequestParts<AppState> for Subject {
    type Rejection = ApiError;

    async fn from_request_parts(parts: &mut Parts, state: &AppState) -> ApiResult<Self> {
        let cookies = parts.headers.get(header::COOKIE).and_then(|value| value.to_str().ok());
        let raw = session::cookie_value(cookies, session::SESSION_COOKIE)
            .ok_or_else(ApiError::unauthenticated)?;
        session::resolve(&state.store, raw).await
    }
}

/// 写操作的守卫：解出主体**并**校验双提交 CSRF。
///
/// 做成独立的提取器而不是在每个 handler 里手写，是为了让「忘了校验 CSRF」
/// 变成一个类型错误而不是一个安静的漏洞。
pub struct WriteSubject(pub Subject);

impl FromRequestParts<AppState> for WriteSubject {
    type Rejection = ApiError;

    async fn from_request_parts(parts: &mut Parts, state: &AppState) -> ApiResult<Self> {
        let subject = Subject::from_request_parts(parts, state).await?;
        let cookies = parts.headers.get(header::COOKIE).and_then(|value| value.to_str().ok());
        let header_token =
            parts.headers.get(session::CSRF_HEADER).and_then(|value| value.to_str().ok());
        let cookie_token = session::cookie_value(cookies, session::CSRF_COOKIE);
        session::verify_csrf(&state.store, subject.session_pk, header_token, cookie_token).await?;
        Ok(WriteSubject(subject))
    }
}

pub fn router(store: Arc<Store>) -> Router {
    let state = AppState { store };
    Router::new()
        // 自身健康与指标。**不需要认证**——它们不返回任何业务数据。
        .route("/healthz", get(routes::healthz))
        .route("/readyz", get(routes::readyz))
        // 个人中心：目标固定取服务端会话主体，**不接受客户端提供的用户 ID**。
        .route("/api/v1/me", get(routes::me))
        .route("/api/v1/me/usage", get(routes::me_usage))
        // 管理面。每个 handler 第一件事就是 require_admin()。
        .route("/api/v1/nodes", get(routes::list_nodes))
        .route("/api/v1/nodes/{node_id}", get(routes::get_node))
        .route("/api/v1/identities", get(routes::list_identities))
        .route("/api/v1/users", get(routes::list_users))
        .route("/api/v1/users/{user_id}/usage", get(routes::user_usage))
        .route(
            "/api/v1/settings/quota",
            get(routes::get_quota_settings).put(routes::put_quota_settings),
        )
        .route("/api/v1/usage/trend", get(trend::usage_trend))
        .route("/api/v1/alerts", get(routes::list_alerts))
        // 这两页的后端属于 M6/M5，现在明确回「未启用」而不是 404——
        // 404 会被读成「打错了」，而真相是「这个能力还没开」。
        .route("/api/v1/subscriptions/{user_id}", get(routes::subscriptions_not_enabled))
        .route("/api/v1/me/audit/access", get(routes::audit_not_enabled))
        // 页面与静态资源。放 fallback 上：API 路由优先，剩下的交给 web 层。
        // **一页一个 URL**，所以这里没有 hash 路由，越权在服务端就能拦住。
        .fallback(crate::web::serve_page)
        .layer(axum::middleware::from_fn(private_cache_headers))
        .with_state(state)
}

/// 登录态响应一律 `private, no-store` + `Vary: Cookie`（§4.11）。
///
/// 没有这一对，共享缓存会把 A 的响应发给 B——而这两个头是**默认就该有**的，
/// 不是某几个端点的特殊要求，所以挂在中间件上而不是逐个 handler 写。
async fn private_cache_headers(
    request: axum::extract::Request,
    next: axum::middleware::Next,
) -> axum::response::Response {
    let mut response = next.run(request).await;
    let headers = response.headers_mut();
    headers.insert(header::CACHE_CONTROL, HeaderValue::from_static(session::PRIVATE_CACHE_CONTROL));
    headers.insert(header::VARY, HeaderValue::from_static("Cookie"));
    response
}

/// 起 HTTP 服务。绑回环；公网访问经本机反代（§5）。
pub async fn serve(
    store: Arc<Store>,
    listener: tokio::net::TcpListener,
    shutdown: tokio::sync::watch::Receiver<bool>,
) -> std::io::Result<()> {
    let app = router(store);
    let mut shutdown = shutdown;
    axum::serve(listener, app)
        .with_graceful_shutdown(async move {
            while shutdown.changed().await.is_ok() {
                if *shutdown.borrow() {
                    return;
                }
            }
        })
        .await
}
