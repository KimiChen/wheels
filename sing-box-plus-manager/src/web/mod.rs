//! 静态资源与页面路由（README §4.11）。
//!
//! **静态资源在构建时嵌入，随二进制一起发**（§5：单二进制 + systemd，无构建步骤）。
//! 这样部署就只有一个文件，不会出现「二进制升了、页面没升」这种半新半旧的状态。
//!
//! `web-standard-kit` 是同仓库的另一个子项目，这里按本项目自己的纪律处理：
//! **构建时嵌入 + 摘要锁定**（`tests/m4/asset_lock.rs`）——
//! 与 M0 锁节点样例是同一条（`docs/integration-contract.md` §2.3）。
//! kit 改了这边不会静默跟着变，锁会先红。
//!
//! **一页一个 URL。** 侧栏每条链接对应一个真实页面，不是 hash 路由——
//! 这让**越权在服务端就能拦住**，而不是先把整个应用发过去再藏。

use axum::http::{header, HeaderValue, StatusCode, Uri};
use axum::response::{IntoResponse, Redirect, Response};

use crate::api::session::{Role, Subject};

/// 页面对角色的可见性。
///
/// **它与页面里 `data-roles` 的那一份必须一致**，但两者职责不同：
/// 这一份是服务端的拦截，那一份只是让界面讲得通。
/// 前端隐藏不是安全边界——两份都要有，而且服务端这份说了算。
const PAGES: &[(&str, Visibility)] = &[
    ("index.html", Visibility::Authenticated),
    ("login.html", Visibility::Public),
    ("unauthorized.html", Visibility::Public),
    // 个人中心：两种角色都可见。
    ("me.html", Visibility::Authenticated),
    ("me-usage.html", Visibility::Authenticated),
    ("me-audit.html", Visibility::Authenticated),
    ("me-custom.html", Visibility::Authenticated),
    // 管理面。
    ("overview.html", Visibility::AdminOnly),
    ("nodes.html", Visibility::AdminOnly),
    ("alerts.html", Visibility::AdminOnly),
    ("usage.html", Visibility::AdminOnly),
    ("users.html", Visibility::AdminOnly),
    ("settings.html", Visibility::AdminOnly),
];

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Visibility {
    /// 未认证也能看——登录页与它的兼容跳转页。
    Public,
    Authenticated,
    AdminOnly,
}

macro_rules! page {
    ($name:literal) => {
        ($name, include_str!(concat!("../../web/", $name)))
    };
}

const PAGE_BODIES: &[(&str, &str)] = &[
    page!("index.html"),
    page!("login.html"),
    page!("unauthorized.html"),
    page!("me.html"),
    page!("me-usage.html"),
    page!("me-audit.html"),
    page!("me-custom.html"),
    page!("overview.html"),
    page!("nodes.html"),
    page!("alerts.html"),
    page!("usage.html"),
    page!("users.html"),
    page!("settings.html"),
];

/// 本项目自己的静态资源。
const ASSETS: &[(&str, &str, &str)] = &[
    (
        "assets/theme.js",
        include_str!("../../web/assets/theme.js"),
        "text/javascript; charset=utf-8",
    ),
    ("assets/app.js", include_str!("../../web/assets/app.js"), "text/javascript; charset=utf-8"),
    ("assets/api.js", include_str!("../../web/assets/api.js"), "text/javascript; charset=utf-8"),
    (
        "assets/chart.js",
        include_str!("../../web/assets/chart.js"),
        "text/javascript; charset=utf-8",
    ),
    ("assets/console.css", include_str!("../../web/assets/console.css"), "text/css; charset=utf-8"),
];

/// vendored 第三方库（§5：无构建步骤、无包管理器，随二进制发布）。
///
/// **运行时不走公网 CDN**：CDN 是一个我们控制不了的第三方，
/// 它挂了控制台就没有图，它被投毒控制台就执行别人的代码。
/// 许可证与取得方式记在 `web/vendor/README.md`，摘要由
/// `tests/m4/assets.rs::vendor_is_digest_locked` 锁住。
const VENDOR: &[(&str, &str, &str)] = &[
    (
        "vendor/uplot.iife.min.js",
        include_str!("../../web/vendor/uplot.iife.min.js"),
        "text/javascript; charset=utf-8",
    ),
    (
        "vendor/uplot.min.css",
        include_str!("../../web/vendor/uplot.min.css"),
        "text/css; charset=utf-8",
    ),
    (
        "vendor/uplot.LICENSE",
        include_str!("../../web/vendor/uplot.LICENSE"),
        "text/plain; charset=utf-8",
    ),
];

/// `web-standard-kit`。
///
/// 页面里写的是 `../../web-standard-kit/style.css`——从 `/overview.html` 解析出来
/// 正好是 `/web-standard-kit/style.css`，而直接用 `file://` 打开时它指向兄弟目录。
/// **同一个路径两边都成立**，所以页面一个字都不用改，原型预览也还能用。
const KIT: &[(&str, &str, &str)] = &[
    (
        "web-standard-kit/style.css",
        include_str!("../../../web-standard-kit/style.css"),
        "text/css; charset=utf-8",
    ),
    (
        "web-standard-kit/script.js",
        include_str!("../../../web-standard-kit/script.js"),
        "text/javascript; charset=utf-8",
    ),
];

pub fn page_body(name: &str) -> Option<&'static str> {
    PAGE_BODIES.iter().find(|(key, _)| *key == name).map(|(_, body)| *body)
}

pub fn asset(path: &str) -> Option<(&'static str, &'static str)> {
    ASSETS
        .iter()
        .chain(VENDOR.iter())
        .chain(KIT.iter())
        .find(|(key, _, _)| *key == path)
        .map(|(_, body, mime)| (*body, *mime))
}

/// 页面路由。
///
/// - 未认证 → 跳登录页。
/// - 已认证但角色不够 → **403**，正文是 `unauthorized.html`（它链回个人中心）。
///   用 403 而不是跳转：跳转会把「你没权限」和「你没登录」混成同一个观测结果。
pub async fn serve_page(
    axum::extract::State(state): axum::extract::State<crate::api::AppState>,
    uri: Uri,
    headers: axum::http::HeaderMap,
) -> Response {
    let path = uri.path().trim_start_matches('/');
    let name = if path.is_empty() { "index.html" } else { path };

    // 静态资源不需要认证：它们不含任何业务数据，而且登录页也要用。
    if let Some((body, mime)) = asset(name) {
        return static_response(body, mime);
    }

    let Some((_, visibility)) = PAGES.iter().find(|(key, _)| *key == name) else {
        return (StatusCode::NOT_FOUND, "Not Found").into_response();
    };
    if *visibility == Visibility::Public {
        let body = page_body(name).unwrap_or_default();
        // 登录页要知道服务端到底有没有挂载 SSO：没挂载时那个按钮会 404，
        // 而「点了没反应」是最难自查的一种故障。让服务端直说，不让页面去猜。
        let body = if name == "login.html" {
            stamp_sso(body, state.sso.is_some())
        } else {
            body.to_string()
        };
        return html_response(body, StatusCode::OK);
    }

    let subject = resolve_subject(&state, &headers).await;
    let Some(subject) = subject else {
        // 未认证：跳登录页。登录页的缓存头与登录态页面不同，所以它单独成页。
        return Redirect::to("/login.html").into_response();
    };
    if *visibility == Visibility::AdminOnly && !subject.role.is_admin() {
        // **越权在服务端拦住**，不是把页面发过去再藏。
        return html_response(
            page_body("unauthorized.html").unwrap_or_default().to_string(),
            StatusCode::FORBIDDEN,
        );
    }
    html_response(stamp(page_body(name).unwrap_or_default(), subject.role), StatusCode::OK)
}

/// 把**服务端这一次求值出的角色**写进 `<body>`，并标上 live 模式。
///
/// 不这样做的话，`app.js` 在 load 时会按 localStorage 里存的原型身份决定导航，
/// 而那个值可能是几天前点着玩留下的：一个管理员会在 `/api/v1/me` 还没回来之前
/// 就被自己的浏览器从 `/nodes.html` 弹到 `/me.html`。
/// 角色必须与页面同步到达，不能等一个异步探测。
///
/// 这**不是**权限边界——边界是上面那两个分支。这里只让界面别讲错话。
fn stamp(body: &'static str, role: Role) -> String {
    let Some(start) = body.find(BODY_ANCHOR) else {
        // 锚点缺失由 `tests/m4/assets.rs` 在编译期之外机械拦住；
        // 运行时退回原文，宁可界面退化成原型分流，也不发一个空页面。
        return body.to_string();
    };
    let Some(end) = body[start..].find('>').map(|offset| start + offset) else {
        return body.to_string();
    };
    format!(
        "{}<body data-principal=\"{}\" data-pm-mode=\"live\"{}",
        &body[..start],
        role.as_str(),
        &body[end..]
    )
}

/// 认证页面都必须带这个锚点，`stamp` 才有落点。
pub const BODY_ANCHOR: &str = "<body data-principal=";

/// 登录页的锚点。它是 Public 页，不经 [`stamp`]，所以单独有一个。
pub const LOGIN_ANCHOR: &str = "<body class=\"pm-login\"";

/// 把「服务端有没有配 SSO」盖进登录页。
///
/// 盖的是事实，不是样式：没配时那个 `/auth/sso/login` 链接是 404，
/// 页面据此把它换成一句说明。前端自己探测不出这件事——
/// 它只能点一次然后看见 404，而那对使用者毫无信息量。
pub fn stamp_sso(body: &'static str, enabled: bool) -> String {
    let Some(start) = body.find(LOGIN_ANCHOR) else {
        // 锚点缺失由 `tests/m4/assets.rs` 机械拦住；运行时退回原文。
        return body.to_string();
    };
    format!(
        "{}<body class=\"pm-login\" data-pm-sso=\"{}\"{}",
        &body[..start],
        if enabled { "on" } else { "off" },
        &body[start + LOGIN_ANCHOR.len()..]
    )
}

async fn resolve_subject(
    state: &crate::api::AppState,
    headers: &axum::http::HeaderMap,
) -> Option<Subject> {
    let cookies = headers.get(header::COOKIE).and_then(|value| value.to_str().ok());
    let raw = crate::api::session::cookie_value(cookies, crate::api::session::SESSION_COOKIE)?;
    // 名单读不出来时 `roster()` 是 Err——这里同样当成「没有主体」。
    // 页面层本就把一切失败折成跳登录页，而失败关闭的语义在 API 层已经强制过了。
    let roster = state.roster().ok()?;
    crate::api::session::resolve(&state.store, raw, &roster).await.ok()
}

fn html_response(body: String, status: StatusCode) -> Response {
    (status, [(header::CONTENT_TYPE, HeaderValue::from_static("text/html; charset=utf-8"))], body)
        .into_response()
}

fn static_response(body: &'static str, mime: &'static str) -> Response {
    (
        StatusCode::OK,
        [(header::CONTENT_TYPE, HeaderValue::from_str(mime).expect("mime 是合法头值"))],
        body,
    )
        .into_response()
}

/// 角色对页面的可见性。给测试与 [`serve_page`] 共用同一份表。
pub fn visible_to(name: &str, role: Option<Role>) -> bool {
    match PAGES.iter().find(|(key, _)| *key == name) {
        Some((_, Visibility::Public)) => true,
        Some((_, Visibility::Authenticated)) => role.is_some(),
        Some((_, Visibility::AdminOnly)) => role == Some(Role::Admin),
        None => false,
    }
}

/// 全部页面名，供锁定用例遍历。
/// 该页是否**不需要认证**即可取到（登录页、越权提示页）。
pub fn is_public(name: &str) -> bool {
    PAGES.iter().any(|(key, vis)| *key == name && *vis == Visibility::Public)
}

pub fn page_names() -> impl Iterator<Item = &'static str> {
    PAGES.iter().map(|(name, _)| *name)
}

/// 嵌入的 kit 文件，供摘要锁定用例核对。
pub fn kit_files() -> impl Iterator<Item = (&'static str, &'static str)> {
    KIT.iter().map(|(name, body, _)| (*name, *body))
}

/// vendored 第三方文件，供摘要锁测试遍历。
pub fn vendor_files() -> impl Iterator<Item = (&'static str, &'static str)> {
    VENDOR.iter().map(|(name, body, _)| (*name, *body))
}
