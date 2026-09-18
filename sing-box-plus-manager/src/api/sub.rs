//! 订阅端点 `GET /sub/{token}`（README §4.7）。
//!
//! **不在 `/api/v1` 下，不认会话，只认 token。** 它是客户端定时拉的，
//! 带不了 cookie，也不该带——订阅链接本身就是凭据。
//!
//! 失败一律 404，**不区分**「token 不存在」「已吊销」「账号停用」：
//! 区分开来就等于给拿着一个无效 token 的人一个探测接口。

use axum::extract::{Path, State};
use axum::http::{header, HeaderValue, StatusCode};
use axum::response::{IntoResponse, Response};

use crate::api::AppState;
use crate::subscription::{self, render};

/// 从 `Proxy-<64位十六进制>.yaml` 里取出 token。
///
/// 形状写死而不是宽松匹配：订阅地址是**用户会转发、会存进客户端**的东西，
/// 一个宽松的匹配器意味着任何 `/sub/*` 都会打到数据库上。
fn token_from(name: &str) -> Option<&str> {
    let token = name.strip_prefix(render::PROFILE_PREFIX)?.strip_suffix(".yaml")?;
    (token.len() == 64 && token.bytes().all(|b| b.is_ascii_hexdigit())).then_some(token)
}

pub async fn serve(State(app): State<AppState>, Path(name): Path<String>) -> Response {
    let Some(config) = &app.subscription else {
        return StatusCode::NOT_FOUND.into_response();
    };
    let Some(token) = token_from(&name) else {
        return StatusCode::NOT_FOUND.into_response();
    };
    match subscription::resolve(&app.store, token).await {
        Ok(Some(subscriber)) => build(&app, config, subscriber).await,
        Ok(None) => {
            // 不记 token。§4.7：响应头与日志都不得含它。
            tracing::debug!("订阅 token 无效");
            StatusCode::NOT_FOUND.into_response()
        }
        Err(error) => {
            tracing::error!(%error, "取订阅失败");
            StatusCode::INTERNAL_SERVER_ERROR.into_response()
        }
    }
}

async fn build(
    app: &AppState,
    config: &crate::config::SubscriptionConfig,
    subscriber: subscription::Subscriber,
) -> Response {
    let credentials = match app.credentials() {
        Ok(credentials) => credentials,
        Err(error) => {
            // 凭据读不出来**不是 404**：那会让客户端把一次配置事故当成
            // 「订阅被吊销了」，然后用户去找管理员要新链接——而链接是好的。
            tracing::error!(%error, "读凭据失败：订阅暂时不可用（P1）");
            return StatusCode::SERVICE_UNAVAILABLE.into_response();
        }
    };

    let identity = match subscription::claimed_identity(&app.store, subscriber.user_id).await {
        Ok(Some(identity)) => identity,
        Ok(None) => {
            // 有 token 但没身份：登录时领取失败过。返回空订阅而不是 404——
            // 404 会被读成「你的订阅没了」，而真相是「还没给你分到节点」。
            tracing::warn!(login_name = %subscriber.login_name, "取订阅时这个人还没有身份");
            String::new()
        }
        Err(error) => {
            tracing::error!(%error, "查身份失败");
            return StatusCode::INTERNAL_SERVER_ERROR.into_response();
        }
    };

    let profile = render::profile_name(&subscriber.login_name);
    if !identity.is_empty() && !credentials.upsk.contains_key(&identity) {
        tracing::error!(identity = %identity, "凭据文件里没有这个身份的 uPSK（P1）");
    }
    let yaml =
        render::clash_yaml(&config.entries, &config.groups, &credentials, &identity, &profile);

    let usage = subscription::cycle_usage(&app.store, subscriber.user_id).await.unwrap_or_default();
    let expire = subscription::cycle_expire_unix().unwrap_or(0);
    let (ascii, encoded) = render::disposition(&profile);

    let mut response = yaml.into_response();
    let headers = response.headers_mut();
    // `text/yaml` 而不是 `application/octet-stream`：后者会让部分客户端
    // 直接下载文件而不是把它当订阅导入。
    headers.insert(header::CONTENT_TYPE, HeaderValue::from_static("text/yaml; charset=utf-8"));
    if let Ok(value) =
        HeaderValue::from_str(&render::userinfo(usage.upload, usage.download, usage.total, expire))
    {
        headers.insert("subscription-userinfo", value);
    }
    if let Ok(value) =
        HeaderValue::from_str(&format!("attachment; filename=\"{ascii}\"; filename*={encoded}"))
    {
        headers.insert(header::CONTENT_DISPOSITION, value);
    }
    // 订阅是凭据，不进任何缓存。
    headers.insert(header::CACHE_CONTROL, HeaderValue::from_static("private, no-store"));
    headers.insert(header::REFERRER_POLICY, HeaderValue::from_static("no-referrer"));
    response
}
