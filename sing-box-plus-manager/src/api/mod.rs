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

pub mod auth;
pub mod custom;
pub mod error;
pub mod routes;
pub mod session;
pub mod sub;
pub mod trend;

use std::sync::Arc;

use axum::extract::FromRequestParts;
use axum::http::request::Parts;
use axum::http::{header, HeaderValue};
use axum::routing::get;
use axum::Router;

use crate::api::error::{ApiError, ApiResult};
use crate::api::session::Subject;
use crate::config::NodeConfig;
use crate::store::Store;

#[derive(Clone)]
pub struct AppState {
    pub store: Arc<Store>,
    /// 节点配置。**不从库里读**：失败开放的签字（C12）的事实来源是配置文件，
    /// 复制进库就会多出一处可以与配置漂移的副本。
    pub nodes: Arc<Vec<NodeConfig>>,
    /// 泡游 SSO。`None` 表示没配 `[sso]` 段——那时登录端点不挂载。
    /// 同样是配置即事实来源：成员名单在这里面，每请求求值一次（D27）。
    pub sso: Option<crate::sso::Sso>,
    /// 订阅配置。`None` 表示没配 `[subscription]`——那时 `/sub/…` 不挂载。
    pub subscription: Option<Arc<crate::config::SubscriptionConfig>>,
    /// 凭据文件的热源。与名单同一条纪律：按 mtime 重读，读不出来失败关闭。
    credentials: Option<Arc<crate::subscription::CredentialSource>>,
    /// 审计镜像树。`None` 表示没配 `[audit]`——那时审计端点回 `audit_not_enabled`，
    /// **不是 403 也不是 404**：没有记录可看和不让你看是两回事（D18）。
    pub audit: Option<Arc<crate::config::AuditConfig>>,
    /// 自定义节点的密钥。`None` 表示没配 `[custom_nodes]`——那时相关端点不挂载，
    /// 订阅里也不会出现自定义节点。
    pub custom_key: Option<Arc<crate::custom::crypto::CryptoBox>>,
    /// 没配 SSO 时求值用的空名单。**所有人都是普通用户**，不是所有人都是管理员——
    /// 认证代码的失败模式是沉默地放行，空名单最容易被误当成「还没配，先都放行」。
    empty_roster: Arc<crate::sso::roster::Roster>,
}

impl AppState {
    pub fn node(&self, node_id: &str) -> Option<&NodeConfig> {
        self.nodes.iter().find(|n| n.node_id == node_id)
    }

    /// 订阅凭据。**读不出来就失败关闭**——调用方返回 503 而不是空订阅。
    pub fn credentials(&self) -> crate::Result<Arc<crate::config::Credentials>> {
        let Some(source) = &self.credentials else {
            return Err(crate::Error::invalid_config("§4.7", "没有配置 [subscription]"));
        };
        source.load()
    }

    /// 本次求值要用的成员名单。**每请求取一次，不缓存结论。**
    ///
    /// 配了 SSO 时它从磁盘重读（按 mtime + inode 缓存内容），于是改名单
    /// 下一个请求即生效；**读不出来就失败关闭**——授权源缺失、损坏或非法时
    /// 整个认证请求失败，不退回上一份好的名单（§4.9）。
    /// 退回看起来更友好，含义却是「有人把某人从名单里删掉、同时手滑写坏了
    /// 文件，于是那个人继续有权限」。
    pub fn roster(&self) -> ApiResult<std::sync::Arc<crate::sso::roster::Roster>> {
        match &self.sso {
            Some(sso) => sso.roster().map_err(|error| {
                tracing::error!(%error, "读取成员名单失败：本次授权判定失败关闭（P1）");
                ApiError::new(
                    crate::api::error::ApiCode::MemberFactStale,
                    "授权源不可读：认证依赖不可用",
                )
            }),
            // 没配 SSO 时是空名单：所有人都是普通用户，break-glass 走 users.role。
            None => Ok(self.empty_roster.clone()),
        }
    }
}

/// 从 cookie 解出主体。**每请求求值**（D13），不缓存结论。
impl FromRequestParts<AppState> for Subject {
    type Rejection = ApiError;

    async fn from_request_parts(parts: &mut Parts, state: &AppState) -> ApiResult<Self> {
        let cookies = parts.headers.get(header::COOKIE).and_then(|value| value.to_str().ok());
        let raw = session::cookie_value(cookies, session::SESSION_COOKIE)
            .ok_or_else(ApiError::unauthenticated)?;
        let roster = state.roster()?;
        session::resolve(&state.store, raw, &roster).await
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

pub fn router(
    store: Arc<Store>,
    nodes: Arc<Vec<NodeConfig>>,
    sso: Option<crate::sso::Sso>,
    subscription: Option<crate::config::SubscriptionConfig>,
    audit: Option<crate::config::AuditConfig>,
    custom_key: Option<Arc<crate::custom::crypto::CryptoBox>>,
) -> Router {
    let sso_enabled = sso.is_some();
    let credentials = subscription
        .as_ref()
        .map(|c| Arc::new(crate::subscription::CredentialSource::new(&c.credentials_path)));
    let subscription_enabled = subscription.is_some();
    let subscription = subscription.map(Arc::new);
    let state = AppState {
        store,
        nodes,
        sso,
        subscription,
        credentials,
        audit: audit.map(Arc::new),
        custom_key,
        empty_roster: Arc::new(crate::sso::roster::Roster::empty()),
    };
    let router = Router::new()
        // 自身健康与指标。**不需要认证**——它们不返回任何业务数据。
        .route("/healthz", get(routes::healthz))
        .route("/readyz", get(routes::readyz))
        // 个人中心：目标固定取服务端会话主体，**不接受客户端提供的用户 ID**。
        .route("/api/v1/me", get(routes::me))
        .route("/api/v1/me/usage", get(routes::me_usage))
        // 订阅地址是**本人的凭据**，所以单独一个端点而不是塞进 `/me`：
        // `/me` 每页都会取一次（前端靠它判定 live 模式），塞进去等于把 token
        // 复制进每一个页面的内存与每一条响应里。这里只有 me.html 会取。
        .route("/api/v1/me/subscription", get(routes::me_subscription))
        // 个人自定义节点。**一次取全**：拨号候选依赖上游列表，
        // 分两次取会出现「候选里还没有刚建的那条上游」的中间态。
        .route("/api/v1/me/custom", get(custom::list))
        .route("/api/v1/me/custom/proxies", axum::routing::post(custom::create_proxy))
        .route(
            "/api/v1/me/custom/proxies/{id}",
            axum::routing::put(custom::update_proxy).delete(custom::delete_proxy),
        )
        .route("/api/v1/me/custom/socks5", axum::routing::post(custom::create_socks5))
        .route(
            "/api/v1/me/custom/socks5/{id}",
            axum::routing::put(custom::update_socks5).delete(custom::delete_socks5),
        )
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
        .route("/api/v1/subscriptions/{user_id}", get(routes::user_subscription))
        // 出站目标审计。没配 `[audit]` 时回 `audit_not_enabled` 而不是 404——
        // 404 会被读成「打错了」，而真相是「这个能力还没开」。
        //
        // 两条路由走**同一个函数**，只是数据主体的来源不同：`/me` 取会话主体，
        // `/users/{id}` 取路径参数并要求管理员。C35 明写两者执行同一套过滤规则，
        // 而写成两份的话它们会在某次改动里分家——分家的方向几乎总是管理员
        // 那一侧更宽松。
        .route("/api/v1/me/audit/access", get(routes::me_audit_access))
        .route("/api/v1/users/{user_id}/audit/access", get(routes::user_audit_access))
        // 退出。**是写操作**，所以走 CSRF 双提交——否则第三方页面可以把人踢下线。
        .route("/api/v1/auth/logout", axum::routing::post(auth::logout));

    // 登录入口只在配好 `[sso]` 时才存在。没配就是 404 而不是一个会 500 的按钮：
    // SSO 依赖对外域名、回调地址与出口 IP 这些主控仓库之外的事实。
    let router = if sso_enabled {
        router
            .route("/auth/sso/login", get(auth::login))
            .route(crate::sso::state::CALLBACK_PATH, get(auth::callback))
    } else {
        router
    };

    // 订阅端点**不在 `/api/v1` 下**，不认会话、只认 token（§4.7）。
    // 客户端是定时拉的，带不了 cookie，也不该带。
    // 文件名的前缀决定这份订阅里的节点拨哪个 host（内网 / 公网），token 两种共用。
    let router =
        if subscription_enabled { router.route("/sub/{name}", get(sub::serve)) } else { router };

    router
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
// 八个参数都是**可选段的开关兼配置**（sso / subscription / audit / custom_nodes）
// 加上 store、nodes、listener、shutdown。打包成结构体只会多一个没有含义的类型。
#[allow(clippy::too_many_arguments)]
pub async fn serve(
    store: Arc<Store>,
    nodes: Arc<Vec<NodeConfig>>,
    sso: Option<crate::sso::Sso>,
    subscription: Option<crate::config::SubscriptionConfig>,
    audit: Option<crate::config::AuditConfig>,
    custom_key: Option<Arc<crate::custom::crypto::CryptoBox>>,
    listener: tokio::net::TcpListener,
    shutdown: tokio::sync::watch::Receiver<bool>,
) -> std::io::Result<()> {
    let app = router(store, nodes, sso, subscription, audit, custom_key);
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
