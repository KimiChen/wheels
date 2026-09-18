//! 登录入口：泡游 SSO 的跳转与回调（README §4.9 加固 1–5）。
//!
//! 回调的顺序是这一整页的核心，写反任何一步都会打开一个洞：
//!
//! ```text
//! 1. 清 state cookie          ——无论成败，它都只该用一次
//! 2. 拒绝未知 query 参数       ——只认 state 与 token
//! 3. 无任何写入地校验 state    ——匿名伪造的回调不得写库
//! 4. 调上游换取身份
//! 5. 成功之后才写墓碑          ——顺序反了，登录入口就是免认证写放大
//! 6. 建号/更新 + 建会话
//! 7. 303 重定向，URL 里不带 token
//! ```
//!
//! **代码里没有任何 token 泄漏点，但这挡不住反代。** token 走的是回调 URL 的
//! query（接入说明如此规定，改不了），而 nginx 的 `combined` 等默认日志格式
//! **会把完整请求行连同 query 一起落盘**。主控这边只能做到两件事：
//! 响应带 `Referrer-Policy: no-referrer`（否则回调页加载的任何外部资源都会
//! 在 `Referer` 里收到它），以及在文档里把这一条写清楚——
//! 反代那侧要对 `/auth/sso/callback` 关掉访问日志。这不是可选的加固项，
//! 而是这条链路上唯一还会明文留存 token 的地方。
//!
//! **失败一律统一跳转，不回显原因**（加固 5）。用户看到的只有「登录失败，请重试」。
//! 但日志要分得清「上游不可用」与「token 无效」——两者的处置完全不同，
//! 而生产实现把它们收敛成了同一个异常，结果是故障时看不出该找谁。
//! 日志**不记 token**，也不记上游返回的 message。

use axum::extract::State;
use axum::http::{header, HeaderMap, HeaderValue, StatusCode};
use axum::response::{IntoResponse, Response};

use crate::api::error::{ApiError, ApiResult};
use crate::api::session::{self};
use crate::api::AppState;
use crate::sso::client::CheckError;
use crate::sso::{self, state};

/// 登录失败后的去处。**带一个标记而不是一段原因**：
/// 原因对用户没有可操作性，而它对攻击者有。
const FAILED_LOCATION: &str = "/login.html?login=failed";
/// 登录成功后的去处。个人中心对两种角色都可见，管理员再自己去总览。
const SUCCESS_LOCATION: &str = "/";

/// `GET /auth/sso/login`：铸一个 state，302 到泡游。
pub async fn login(State(state_ctx): State<AppState>) -> Response {
    let Some(sso) = &state_ctx.sso else {
        return redirect(StatusCode::SEE_OTHER, FAILED_LOCATION, Vec::new());
    };
    let key = match sso::state_key(&state_ctx.store).await {
        Ok(key) => key,
        Err(error) => {
            tracing::error!(%error, "取 SSO state 密钥失败");
            return redirect(StatusCode::SEE_OTHER, FAILED_LOCATION, Vec::new());
        }
    };
    let (token, cookie) = state::issue(&key, sso.state_ttl_secs(), time::OffsetDateTime::now_utc());
    // 302 而不是 303：这一跳是「去别处登录」，不是「你刚提交的东西处理完了」。
    redirect(StatusCode::FOUND, &sso.login_location(&token), vec![cookie])
}

/// 回调只认这两个参数。
pub struct CallbackQuery {
    state: String,
    token: String,
}

/// 逐对解析回调 query，**只认 `state` 与 `token`**（加固 3）。
///
/// 手写而不是用 serde 的 `deny_unknown_fields`：这条规则是认证路径上的一道门，
/// 它该长得像一道门，而不是某个 derive 属性的副作用。手写还顺带拿到了
/// 重复键与百分号解码失败这两种情况的明确处置。
///
/// 代价要知道：泡游哪天多追加一个参数（`from`、`sign`），登录会**全线失败**
/// 且不回显原因。这是刻意选的方向——多一个没预期的参数意味着协议变了，
/// 而在认证路径上「协议变了却照常放行」是更坏的那一种。
fn parse_callback_query(raw: Option<&str>) -> Option<CallbackQuery> {
    let raw = raw.unwrap_or("");
    let mut state: Option<String> = None;
    let mut token: Option<String> = None;
    for pair in raw.split('&').filter(|p| !p.is_empty()) {
        let (key, value) = pair.split_once('=')?;
        let value = percent_decode(value)?;
        // 重复键同样拒绝：`?token=a&token=b` 在不同框架里取值不同，
        // 而「取哪一个」不该由框架的实现细节决定。
        let slot = match key {
            "state" => &mut state,
            "token" => &mut token,
            _ => return None,
        };
        if slot.is_some() {
            return None;
        }
        *slot = Some(value);
    }
    Some(CallbackQuery { state: state?, token: token? })
}

/// 百分号解码。`+` **不当空格**：那是 form 编码的约定，query 里没有这一条，
/// 而一个含 `+` 的 token 被解成空格之后会稳定地换不到身份。
fn percent_decode(value: &str) -> Option<String> {
    let bytes = value.as_bytes();
    let mut out = Vec::with_capacity(bytes.len());
    let mut index = 0;
    while index < bytes.len() {
        if bytes[index] == b'%' {
            let hex = value.get(index + 1..index + 3)?;
            out.push(u8::from_str_radix(hex, 16).ok()?);
            index += 3;
        } else {
            out.push(bytes[index]);
            index += 1;
        }
    }
    String::from_utf8(out).ok()
}

/// `GET /auth/sso/callback?state=&token=`
pub async fn callback(
    State(app): State<AppState>,
    headers: HeaderMap,
    uri: axum::http::Uri,
) -> Response {
    // 第 1 步：清 state cookie。**放在最前面且无条件**——
    // 无论后面走哪条分支，这个 cookie 都只该用一次。
    let mut cookies = vec![state::clear_cookie()];

    let Some(sso) = &app.sso else {
        return redirect(StatusCode::SEE_OTHER, FAILED_LOCATION, cookies);
    };
    // 第 2 步：拒绝未知参数。
    let Some(query) = parse_callback_query(uri.query()) else {
        tracing::warn!("SSO 回调参数不合法：含未知参数、重复键或编码错误");
        return redirect(StatusCode::SEE_OTHER, FAILED_LOCATION, cookies);
    };

    match complete(&app, sso, &headers, &query).await {
        Ok(issued) => {
            cookies.extend(issued);
            // 第 7 步：303 到一个不带 token 的地址。
            // 303 而不是 302：让浏览器把这一跳当成「处理完了，去看结果」，
            // 回退时不会重放这个带 token 的 URL。
            redirect(StatusCode::SEE_OTHER, SUCCESS_LOCATION, cookies)
        }
        Err(reason) => {
            // 日志只记类别，不记 token、不记上游 message。
            tracing::warn!(reason = reason.category(), "SSO 登录失败");
            match &reason {
                // 上游不可用要能被告警抓到，它与「token 过期」的处置完全不同。
                Failure::Upstream(detail) => {
                    tracing::error!(detail = %detail, "SSO 上游不可用（P2）")
                }
                // 按规则拒绝：记原因但只到 warn，不触发告警。
                Failure::Rejected(detail) => tracing::warn!(detail = %detail, "拒绝建立登录态"),
                _ => {}
            }
            redirect(StatusCode::SEE_OTHER, FAILED_LOCATION, cookies)
        }
    }
}

enum Failure {
    BadState,
    Replay,
    InvalidToken,
    Upstream(String),
    /// 身份换到了，但主控这边**按规则拒绝**建立登录态。
    ///
    /// 与 `Internal` 分开：账号被停用、账号名撞上本地应急账号——
    /// 这些是系统按设计工作，不是故障。混在一起的代价很具体：
    /// 一个停用账号反复尝试登录会刷出一串 ERROR 日志并触发告警，
    /// 而真正的内部错误淹没在里面。
    Rejected(String),
    Internal,
}

impl Failure {
    fn category(&self) -> &'static str {
        match self {
            Failure::BadState => "state_invalid",
            Failure::Replay => "state_replayed",
            Failure::InvalidToken => "token_invalid",
            Failure::Upstream(_) => "upstream_unavailable",
            Failure::Rejected(_) => "rejected",
            Failure::Internal => "internal",
        }
    }
}

/// 第 3–6 步。返回要下发的 `Set-Cookie` 列表。
async fn complete(
    app: &AppState,
    sso: &sso::Sso,
    headers: &HeaderMap,
    query: &CallbackQuery,
) -> std::result::Result<Vec<String>, Failure> {
    let key = sso::state_key(&app.store).await.map_err(|error| {
        tracing::error!(%error, "取 SSO state 密钥失败");
        Failure::Internal
    })?;

    // 第 3 步：**无任何写入地**校验签名、绑定与 TTL。
    let cookie_header = headers.get(header::COOKIE).and_then(|value| value.to_str().ok());
    let bound = session::cookie_value(cookie_header, state::STATE_COOKIE);
    let validated = state::validate(
        &key,
        Some(query.state.as_str()),
        bound,
        sso.state_ttl_secs(),
        time::OffsetDateTime::now_utc(),
    )
    .map_err(|_| Failure::BadState)?;

    // 第 4 步：换取身份。
    let identity =
        sso::client::check_token(sso.check_token_endpoint(), &query.token, sso.request_timeout())
            .await
            .map_err(|error| match error {
                CheckError::InvalidToken => Failure::InvalidToken,
                CheckError::Upstream(detail) => Failure::Upstream(detail),
            })?;

    // 第 5 步：**成功之后**才写墓碑。冲突即重放。
    let fresh =
        state::consume(&app.store, &validated, sso.state_ttl_secs()).await.map_err(|error| {
            tracing::error!(%error, "写 state 墓碑失败");
            Failure::Internal
        })?;
    if !fresh {
        return Err(Failure::Replay);
    }

    // 第 6 步：建号/更新 + 建会话。
    //
    // 名单在这里**重读一次**而不是复用登录起始时的那份：一次 SSO 往返可能是
    // 几十秒，名单在这中间被改过完全可能。
    let roster = app.roster().map_err(|_| Failure::Internal)?;
    let logged_in = sso::complete_login(&app.store, &roster, &identity, sso.session_ttl())
        .await
        .map_err(|error| {
            // 「账号已停用」「这是本地应急账号」这类是**被拒绝**，不是内部错误。
            // 模块文档刚刚还在批评生产实现把可区分的失败收敛成同一个异常，
            // 这里就不能自己犯同一条：两者的处置完全不同，
            // 前者不该出现在 error 级日志里、更不该触发告警。
            Failure::Rejected(error.to_string())
        })?;
    if let Some(previous) = &logged_in.quota_group_changed_from {
        // 只提示，不在登录路径上改。改额度是一次影响他人的操作（D21），
        // 它要留操作者、要预览确认、要生成逐节点下发任务——
        // 那一整套不该由某个人碰巧登录了一次来触发。
        tracing::warn!(
            login_name = %logged_in.login_name,
            from = %previous,
            to = %logged_in.quota_group,
            "名单里的档位与库里不一致：请跑 proxy-manager sso reconcile"
        );
    }
    tracing::info!(
        login_name = %logged_in.login_name,
        role = logged_in.role.as_str(),
        "SSO 登录成功"
    );
    Ok(session_cookies(&logged_in.session))
}

/// 会话与 CSRF 两个 cookie。
///
/// **CSRF cookie 刻意不带 `HttpOnly`**：双提交需要页面脚本读得到它。
/// 给它加上 HttpOnly 会让前端拿不到 token，所有写操作永久 403——
/// 而那个故障看起来完全不像是一个 cookie 属性造成的。
fn session_cookies(issued: &session::IssuedSession) -> Vec<String> {
    let max_age = (issued.expires_at - time::OffsetDateTime::now_utc()).whole_seconds().max(0);
    vec![
        format!(
            "{}={}; Path=/; Max-Age={max_age}; HttpOnly; Secure; SameSite=Lax",
            session::SESSION_COOKIE,
            issued.cookie_value
        ),
        format!(
            "{}={}; Path=/; Max-Age={max_age}; Secure; SameSite=Lax",
            session::CSRF_COOKIE,
            issued.csrf_token
        ),
    ]
}

/// `POST /api/v1/auth/logout`
///
/// 走 [`crate::api::WriteSubject`]，也就是**要过 CSRF 双提交**：
/// 不然任何第三方页面都能把人踢下线。
pub async fn logout(
    State(app): State<AppState>,
    crate::api::WriteSubject(subject): crate::api::WriteSubject,
) -> ApiResult<Response> {
    session::revoke(&app.store, subject.session_pk).await.map_err(ApiError::from)?;
    let cleared = vec![
        format!(
            "{}=; Path=/; Max-Age=0; HttpOnly; Secure; SameSite=Lax; \
             Expires=Thu, 01 Jan 1970 00:00:00 GMT",
            session::SESSION_COOKIE
        ),
        format!(
            "{}=; Path=/; Max-Age=0; Secure; SameSite=Lax; \
             Expires=Thu, 01 Jan 1970 00:00:00 GMT",
            session::CSRF_COOKIE
        ),
    ];
    let mut response = axum::Json(serde_json::json!({ "ok": true })).into_response();
    append_cookies(&mut response, cleared);
    Ok(response)
}

/// 登录相关响应的额外头。
///
/// `Referrer-Policy: no-referrer` 是必须的，不是加分项：**token 就在回调 URL 的
/// query 里**，没有这个头，回调页加载的任何外部资源都会在 `Referer` 里收到它。
/// 主控自己的 `private_cache_headers` 只加 `Cache-Control` 与 `Vary`。
fn redirect(status: StatusCode, location: &str, cookies: Vec<String>) -> Response {
    let mut response = status.into_response();
    let headers = response.headers_mut();
    if let Ok(value) = HeaderValue::from_str(location) {
        headers.insert(header::LOCATION, value);
    }
    headers.insert(header::REFERRER_POLICY, HeaderValue::from_static("no-referrer"));
    append_cookies(&mut response, cookies);
    response
}

fn append_cookies(response: &mut Response, cookies: Vec<String>) {
    let headers = response.headers_mut();
    for cookie in cookies {
        if let Ok(value) = HeaderValue::from_str(&cookie) {
            headers.append(header::SET_COOKIE, value);
        }
    }
}
