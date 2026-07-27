//! 用户与 ACL 管理 API + 变更编排：结构变更(grant/revoke/删用户)→重编译+部署+reconcile；
//! 运行态变更(disable/expire)→仅 reconcile 不重启。响应脱敏（无 uPSK/serverPSK/token 明文）。

use axum::extract::{Path, State};
use axum::routing::{get, post};
use axum::{Json, Router};
use serde::Deserialize;
use serde_json::json;

use crate::error::{AppError, ErrorCode};
use crate::manager::agent_client::AgentClient;
use crate::manager::http::AppState;
use crate::manager::{self, deploy, reconcile};
use crate::store::{agents, revisions, topology, users};

type ApiResult = std::result::Result<Json<serde_json::Value>, AppError>;

pub fn add_routes(router: Router<AppState>) -> Router<AppState> {
    router
        .route("/api/users", get(list).post(create))
        .route("/api/users/{id}", get(detail).patch(update).delete(remove))
        .route("/api/users/{id}/routes", post(grant))
        .route(
            "/api/users/{id}/routes/{route_id}",
            axum::routing::delete(revoke),
        )
        .route("/api/users/{id}/subscription/rotate", post(rotate))
        .route("/api/entries/{id}/reconcile", post(manual_reconcile))
}

#[derive(Deserialize)]
struct CreateUserReq {
    name: String,
    quota_bytes: Option<i64>,
    reset_cycle: Option<String>,
    expire_at: Option<i64>,
}

async fn create(State(st): State<AppState>, Json(r): Json<CreateUserReq>) -> ApiResult {
    let cycle = r.reset_cycle.as_deref().unwrap_or("monthly");
    if crate::domain::user::ResetCycle::parse(cycle).is_none() {
        return Err(AppError::new(ErrorCode::Validation, "非法 reset_cycle"));
    }
    let (id, token) = users::create_user(
        &st.pool,
        &r.name,
        r.quota_bytes.unwrap_or(0),
        cycle,
        r.expire_at,
    )
    .await?;
    // token 明文一次性返回（等价 enrollment 一次性密钥）。
    Ok(Json(json!({ "id": id, "subscription_token": token })))
}

async fn list(State(st): State<AppState>) -> ApiResult {
    Ok(Json(json!({ "users": users::list_users(&st.pool).await? })))
}

async fn detail(State(st): State<AppState>, Path(id): Path<String>) -> ApiResult {
    let user = users::get_user(&st.pool, &id)
        .await?
        .ok_or_else(|| AppError::new(ErrorCode::NotFound, "user 不存在"))?;
    let routes = users::user_routes(&st.pool, &id).await?;
    Ok(Json(json!({ "user": user, "routes": routes })))
}

#[derive(Deserialize)]
struct UpdateUserReq {
    quota_bytes: Option<i64>,
    reset_cycle: Option<String>,
    expire_at: Option<i64>,
    disabled: Option<bool>,
}

async fn update(
    State(st): State<AppState>,
    Path(id): Path<String>,
    Json(r): Json<UpdateUserReq>,
) -> ApiResult {
    users::update_user(
        &st.pool,
        &id,
        r.quota_bytes,
        r.reset_cycle.as_deref(),
        r.expire_at.map(Some),
        r.disabled,
    )
    .await?;
    // 运行态变更（资格翻转）→ 仅 SSM reconcile，不重启 Entry。
    if r.disabled.is_some() || r.expire_at.is_some() {
        let client = manager::build_agent_client(&st.pool, &st.cipher).await?;
        for e in reconcile::affected_entries(&st.pool, &id).await? {
            let _ = reconcile::reconcile_entry(&st.pool, &st.cipher, client.as_ref(), &e).await;
        }
    }
    Ok(Json(json!({ "updated": id })))
}

async fn remove(State(st): State<AppState>, Path(id): Path<String>) -> ApiResult {
    let affected = reconcile::affected_entries(&st.pool, &id).await?;
    users::delete_user(&st.pool, &id).await?;
    let client = manager::build_agent_client(&st.pool, &st.cipher).await?;
    for e in &affected {
        let _ = republish(&st, client.as_ref(), e).await; // 结构变更：auth_user 缩小
    }
    Ok(Json(json!({ "deleted": id })))
}

#[derive(Deserialize)]
struct GrantReq {
    route_id: String,
}

async fn grant(
    State(st): State<AppState>,
    Path(id): Path<String>,
    Json(r): Json<GrantReq>,
) -> ApiResult {
    let name = users::grant_route(&st.pool, &st.cipher, &id, &r.route_id).await?;
    let route = topology::get_route(&st.pool, &r.route_id)
        .await?
        .ok_or_else(|| AppError::new(ErrorCode::NotFound, "route 不存在"))?;
    let client = manager::build_agent_client(&st.pool, &st.cipher).await?;
    republish(&st, client.as_ref(), &route.entry_id).await?;
    Ok(Json(
        json!({ "identity_name": name, "entry_id": route.entry_id }),
    ))
}

async fn revoke(
    State(st): State<AppState>,
    Path((id, route_id)): Path<(String, String)>,
) -> ApiResult {
    users::revoke_route(&st.pool, &id, &route_id).await?;
    if let Some(route) = topology::get_route(&st.pool, &route_id).await? {
        let client = manager::build_agent_client(&st.pool, &st.cipher).await?;
        let _ = republish(&st, client.as_ref(), &route.entry_id).await;
    }
    Ok(Json(json!({ "revoked": route_id })))
}

async fn rotate(State(st): State<AppState>, Path(id): Path<String>) -> ApiResult {
    let token = users::rotate_token(&st.pool, &id).await?;
    Ok(Json(json!({ "subscription_token": token })))
}

async fn manual_reconcile(State(st): State<AppState>, Path(id): Path<String>) -> ApiResult {
    let client = manager::build_agent_client(&st.pool, &st.cipher).await?;
    let report = reconcile::reconcile_entry(&st.pool, &st.cipher, client.as_ref(), &id).await?;
    Ok(Json(json!({ "report": report })))
}

/// 结构变更编排：重编译（折入新 identities）→ 真实 check → 部署（激活 Route）→ reconcile 回填 SSM。
async fn republish(
    st: &AppState,
    client: &dyn AgentClient,
    entry_id: &str,
) -> crate::error::Result<()> {
    let entry = topology::get_entry(&st.pool, entry_id)
        .await?
        .ok_or_else(|| AppError::new(ErrorCode::NotFound, "entry 不存在"))?;
    let target = agents::get_agent(&st.pool, &entry.host_id)
        .await?
        .and_then(|a| a.singbox_version);
    let rev =
        revisions::compile_and_persist(&st.pool, &st.cipher, entry_id, target.as_deref(), None)
            .await?;
    revisions::run_check(&st.pool, &st.cipher, &rev.id).await?;
    let rev = revisions::get_revision(&st.pool, &rev.id)
        .await?
        .ok_or_else(|| AppError::new(ErrorCode::Internal, "revision 丢失"))?;
    if rev.status == "checked" {
        let dep = deploy::create_deployment(&st.pool, &rev.id, "normal", None).await?;
        deploy::drive(&st.pool, &st.cipher, client, &dep).await?;
    }
    // 部署成功后回填 SSM（新 epoch）；失败/无 Agent 隔离，不阻塞。
    let _ = reconcile::reconcile_entry(&st.pool, &st.cipher, client, entry_id).await;
    Ok(())
}
