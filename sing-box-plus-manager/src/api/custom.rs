//! 个人自定义节点的读写端点（`/api/v1/me/custom/…`）。
//!
//! **目标固定取服务端会话主体**，不接受客户端提供的用户 ID——与 `/me` 同一条纪律。
//!
//! # 列表里**不回凭据**
//!
//! 返回 name / 协议 / 服务器 / 端口这些能认出是哪一条的字段，但**不回**密码、
//! UUID，也**不回原始分享链接**（链接里就带着密码）。理由与 `/me/subscription`
//! 不回 host 是同一条（§4.11）：页面会被截图、会被投屏，而任何一次 XSS
//! 都能把响应里的东西一起带走。
//!
//! 代价是「改一条上游」要重贴一次链接。所以改名单独放行：`share_url` 缺省
//! 就是只改名字、配置原样保留——把「重贴链接」的成本压到真正要换服务器的时候。
//! SOCKS5 的口令同理：不传就保留原来的。

use axum::extract::{Path, State};
use axum::response::IntoResponse;
use axum::Json;
use serde::Deserialize;
use serde_json::json;

use crate::api::error::{ApiCode, ApiError, ApiResult};
use crate::api::{AppState, Subject, WriteSubject};
use crate::custom::parse;
use crate::custom::store::{self, CustomError, StoredProxy, StoredSocks5};

fn map_error(error: CustomError) -> ApiError {
    match error {
        CustomError::Invalid(message) => ApiError::invalid(message),
        // 409 而不是 400：**重名不是「你填错了格式」，是「和别的东西撞了」**。
        // 前端据此分支——格式错要改这一个字段，撞名要么改名要么先删掉那一条。
        CustomError::Conflict(message) => ApiError::new(ApiCode::IdempotencyConflict, message),
        CustomError::NotFound => ApiError::not_found("自定义节点"),
        CustomError::Internal(error) => {
            tracing::error!(%error, "自定义节点操作失败");
            ApiError::internal("自定义节点操作失败")
        }
    }
}

/// 没配 `[custom_nodes]` 时的答复。**不是 404 也不是错误**：
/// 「服务端没开这个功能」是一个确定的事实，页面据此说得出话；
/// 回错误会被读成「加载失败，刷新试试」，而刷多少次都一样。
fn disabled() -> serde_json::Value {
    json!({ "enabled": false, "proxies": [], "socks5": [], "dialer_options": [] })
}

fn key(state: &AppState) -> ApiResult<&crate::custom::crypto::CryptoBox> {
    state
        .custom_key
        .as_deref()
        .ok_or_else(|| ApiError::new(ApiCode::InvalidRequest, "服务端没有启用自定义节点"))
}

/// 这个人能看见的受管入口名。拨号候选与重名判定都要用它。
///
/// **按档位过滤**，与订阅里的口径一致：级别不够看不见的入口，
/// 不该出现在拨号候选里——选了它，渲染时会因为那条入口不在订阅里而变成悬空引用。
async fn managed_names(state: &AppState, user_id: i64) -> Vec<String> {
    let Some(config) = &state.subscription else { return Vec::new() };
    let group = crate::subscription::quota_group_of(&state.store, user_id)
        .await
        .unwrap_or_else(|_| "normal".to_string());
    config
        .entries
        .iter()
        .filter(|entry| crate::quota::level::can_see(&state.nodes, &entry.node_id, &group))
        .map(|entry| entry.name.clone())
        .collect()
}

/// 受管侧占用的全部名字：入口名 + 代理组名。自定义节点不能撞上它们。
fn reserved_names(state: &AppState, managed: &[String]) -> Vec<String> {
    let mut names = managed.to_vec();
    if let Some(config) = &state.subscription {
        names.extend(config.groups.iter().map(|group| group.name.clone()));
    }
    names
}

fn proxy_view(row: &StoredProxy) -> serde_json::Value {
    json!({
        "id": row.id,
        "name": row.config.name,
        "protocol": row.protocol.as_str(),
        "server": row.config.server,
        "port": row.config.port,
        "network": row.config.network,
        "tls": row.config.tls,
        // 渲染进订阅后的名字。页面上显示它，用户在客户端里才找得到对应的那一条。
        "rendered_name": store::custom_proxy_name(&row.config.name),
        "created_at": row.created_at,
        "updated_at": row.updated_at,
    })
}

fn socks_view(row: &StoredSocks5) -> serde_json::Value {
    json!({
        "id": row.id,
        "name": row.config.name,
        "server": row.config.server,
        "port": row.config.port,
        "username": row.config.username,
        // 口令**不回**，只说有没有——页面据此显示「留空则不改」。
        "has_password": !row.config.password.is_empty(),
        "dialer_proxy": row.config.dialer_proxy,
        "rendered_name": store::socks5_node_name(&row.config.name),
        "dialer_group": store::dialer_group_name(&row.config.name),
        "created_at": row.created_at,
        "updated_at": row.updated_at,
    })
}

/// `GET /api/v1/me/custom` —— 一次取全：两个列表加拨号候选。
///
/// 做成一个端点而不是两个，是因为页面**必须**同时有这三样才能画出来：
/// 拨号候选依赖上游列表，分两次取会出现「候选里还没有刚建的那条上游」的中间态。
pub async fn list(State(state): State<AppState>, subject: Subject) -> ApiResult<impl IntoResponse> {
    let Ok(crypto) = key(&state) else {
        return Ok(Json(disabled()));
    };
    let proxies =
        store::list_proxies(&state.store, crypto, subject.user_id).await.map_err(map_error)?;
    let socks5 =
        store::list_socks5(&state.store, crypto, subject.user_id).await.map_err(map_error)?;
    let managed = managed_names(&state, subject.user_id).await;
    let managed_refs: Vec<&str> = managed.iter().map(String::as_str).collect();
    Ok(Json(json!({
        "enabled": true,
        "proxies": proxies.iter().map(proxy_view).collect::<Vec<_>>(),
        "socks5": socks5.iter().map(socks_view).collect::<Vec<_>>(),
        "dialer_options": store::upstream_names(&managed_refs, &proxies),
    })))
}

#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ProxyBody {
    pub name: String,
    /// 缺省即**只改名字**，配置原样保留。见模块文档。
    #[serde(default)]
    pub share_url: Option<String>,
}

pub async fn create_proxy(
    State(state): State<AppState>,
    WriteSubject(subject): WriteSubject,
    Json(body): Json<ProxyBody>,
) -> ApiResult<impl IntoResponse> {
    let crypto = key(&state)?;
    let share_url =
        body.share_url.ok_or_else(|| ApiError::invalid("新建自定义上游必须提供分享链接"))?;
    let config = parse::parse_share_url(&body.name, &share_url)
        .map_err(|error| ApiError::invalid(error.to_string()))?;
    let managed = managed_names(&state, subject.user_id).await;
    let reserved = reserved_names(&state, &managed);
    let reserved_refs: Vec<&str> = reserved.iter().map(String::as_str).collect();
    let id = store::create_proxy(&state.store, crypto, subject.user_id, &reserved_refs, config)
        .await
        .map_err(map_error)?;
    Ok(Json(json!({ "id": id })))
}

pub async fn update_proxy(
    State(state): State<AppState>,
    WriteSubject(subject): WriteSubject,
    Path(id): Path<i64>,
    Json(body): Json<ProxyBody>,
) -> ApiResult<impl IntoResponse> {
    let crypto = key(&state)?;
    let current = store::list_proxies(&state.store, crypto, subject.user_id)
        .await
        .map_err(map_error)?
        .into_iter()
        .find(|row| row.id == id)
        .ok_or_else(|| ApiError::not_found("自定义上游"))?;
    let config = match body.share_url {
        Some(url) => parse::parse_share_url(&body.name, &url)
            .map_err(|error| ApiError::invalid(error.to_string()))?,
        // 只改名字：配置原样保留，名字过一遍同一套校验。
        None => {
            let mut config = current.config.clone();
            config.name = parse::proxy_name(&body.name)
                .map_err(|error| ApiError::invalid(error.to_string()))?;
            config
        }
    };
    let managed = managed_names(&state, subject.user_id).await;
    let reserved = reserved_names(&state, &managed);
    let reserved_refs: Vec<&str> = reserved.iter().map(String::as_str).collect();
    store::update_proxy(&state.store, crypto, subject.user_id, &reserved_refs, id, config)
        .await
        .map_err(map_error)?;
    Ok(Json(json!({ "ok": true })))
}

pub async fn delete_proxy(
    State(state): State<AppState>,
    WriteSubject(subject): WriteSubject,
    Path(id): Path<i64>,
) -> ApiResult<impl IntoResponse> {
    let crypto = key(&state)?;
    store::delete_proxy(&state.store, crypto, subject.user_id, id).await.map_err(map_error)?;
    Ok(Json(json!({ "ok": true })))
}

#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Socks5Body {
    pub name: String,
    pub server: String,
    /// 收字符串而不是数字：表单里它就是字符串，而**在一处做完校验**
    /// 比在 serde 与业务层各做一半更容易说清错误。
    pub port: String,
    #[serde(default)]
    pub username: String,
    /// 缺省即**保留原口令**（改的时候）。新建时缺省就是不做认证。
    #[serde(default)]
    pub password: Option<String>,
    pub dialer_proxy: String,
}

pub async fn create_socks5(
    State(state): State<AppState>,
    WriteSubject(subject): WriteSubject,
    Json(body): Json<Socks5Body>,
) -> ApiResult<impl IntoResponse> {
    let crypto = key(&state)?;
    let config = parse::normalize_socks5(
        &body.name,
        &body.server,
        &body.port,
        &body.username,
        body.password.as_deref().unwrap_or(""),
        &body.dialer_proxy,
    )
    .map_err(|error| ApiError::invalid(error.to_string()))?;
    let managed = managed_names(&state, subject.user_id).await;
    let managed_refs: Vec<&str> = managed.iter().map(String::as_str).collect();
    let reserved = reserved_names(&state, &managed);
    let reserved_refs: Vec<&str> = reserved.iter().map(String::as_str).collect();
    let id = store::create_socks5(
        &state.store,
        crypto,
        subject.user_id,
        &reserved_refs,
        &managed_refs,
        config,
    )
    .await
    .map_err(map_error)?;
    Ok(Json(json!({ "id": id })))
}

pub async fn update_socks5(
    State(state): State<AppState>,
    WriteSubject(subject): WriteSubject,
    Path(id): Path<i64>,
    Json(body): Json<Socks5Body>,
) -> ApiResult<impl IntoResponse> {
    let crypto = key(&state)?;
    let current = store::list_socks5(&state.store, crypto, subject.user_id)
        .await
        .map_err(map_error)?
        .into_iter()
        .find(|row| row.id == id)
        .ok_or_else(|| ApiError::not_found("自定义 SOCKS5"))?;
    // 口令不传就沿用原来的——列表里本来就不回它，要求每次重输等于
    // 「改个前置节点也得把口令再找一遍」。
    let password = body.password.unwrap_or(current.config.password);
    let config = parse::normalize_socks5(
        &body.name,
        &body.server,
        &body.port,
        &body.username,
        &password,
        &body.dialer_proxy,
    )
    .map_err(|error| ApiError::invalid(error.to_string()))?;
    let managed = managed_names(&state, subject.user_id).await;
    let managed_refs: Vec<&str> = managed.iter().map(String::as_str).collect();
    let reserved = reserved_names(&state, &managed);
    let reserved_refs: Vec<&str> = reserved.iter().map(String::as_str).collect();
    store::update_socks5(
        &state.store,
        crypto,
        subject.user_id,
        &reserved_refs,
        &managed_refs,
        id,
        config,
    )
    .await
    .map_err(map_error)?;
    Ok(Json(json!({ "ok": true })))
}

pub async fn delete_socks5(
    State(state): State<AppState>,
    WriteSubject(subject): WriteSubject,
    Path(id): Path<i64>,
) -> ApiResult<impl IntoResponse> {
    let crypto = key(&state)?;
    store::delete_socks5(&state.store, crypto, subject.user_id, id).await.map_err(map_error)?;
    Ok(Json(json!({ "ok": true })))
}
