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

/// `GET /sub/<前缀><token>.yaml`。
///
/// 前缀决定这份订阅里的节点拨哪个 host（内网 / 公网），token 两种拨法共用。
///
/// **形状写死而不是宽松匹配**：订阅地址是用户会转发、会存进客户端的东西，
/// 一个宽松的匹配器意味着任何 `/sub/*` 都会打到数据库上。
/// 认不出前缀与认不出 token 一样回 404——**不给探测接口**。
pub async fn serve(State(app): State<AppState>, Path(name): Path<String>) -> Response {
    let Some(config) = &app.subscription else {
        return StatusCode::NOT_FOUND.into_response();
    };
    let Some((source, token)) = config.source_of(&name) else {
        tracing::debug!("订阅文件名不认识");
        return StatusCode::NOT_FOUND.into_response();
    };
    match subscription::resolve(&app.store, token).await {
        Ok(Some(subscriber)) => build(&app, config, source, subscriber).await,
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
    source: &crate::config::EntrySource,
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

    let profile = render::profile_name(&subscriber.login_name, source);
    if !identity.is_empty() && !credentials.upsk.contains_key(&identity) {
        tracing::error!(identity = %identity, "凭据文件里没有这个身份的 uPSK（P1）");
    }
    // 档位决定看得见哪些节点。读不出来时按最低级算——少看见几个节点是一个
    // 看得见的故障，而按最高级算是悄悄解除限制。
    let group = subscription::quota_group_of(&app.store, subscriber.user_id).await.unwrap_or_else(
        |error| {
            tracing::error!(%error, user_id = subscriber.user_id, "读不到档位：按最低级渲染");
            "normal".to_string()
        },
    );
    let visible: std::collections::BTreeSet<&str> = config
        .entries
        .iter()
        .map(|entry| entry.node_id.as_str())
        .filter(|node_id| crate::quota::level::can_see(&app.nodes, node_id, &group))
        .collect();
    // 这个人的自定义节点。**读失败不挡整份订阅**：受管节点是主业，
    // 一条解不开的自定义配置不该让人连不上网。丢了会在文件头说明——
    // 那条说明由 `render::clash_yaml` 的兜底检查写，这里只负责取不到就当没有。
    let (proxies, socks5) = match &app.custom_key {
        None => (Vec::new(), Vec::new()),
        Some(key) => {
            let proxies = crate::custom::store::list_proxies(&app.store, key, subscriber.user_id)
                .await
                .unwrap_or_else(|error| {
                    tracing::error!(?error, user_id = subscriber.user_id, "读自定义上游失败");
                    Vec::new()
                });
            let socks5 = crate::custom::store::list_socks5(&app.store, key, subscriber.user_id)
                .await
                .unwrap_or_else(|error| {
                    tracing::error!(?error, user_id = subscriber.user_id, "读自定义 SOCKS5 失败");
                    Vec::new()
                });
            (proxies, socks5)
        }
    };
    let custom = crate::subscription::custom::CustomNodes { proxies: &proxies, socks5: &socks5 };
    let yaml =
        render::clash_yaml(config, source, &credentials, &identity, &profile, &visible, custom);

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
