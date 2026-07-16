//! 发布 Web/API：创建/列出/查看部署、一键回滚。响应含结构化 diff 与 target 状态，无密钥/无配置明文。

use axum::extract::{Path, State};
use axum::routing::{get, post};
use axum::{Json, Router};
use serde::Deserialize;
use serde_json::json;

use crate::error::{AppError, ErrorCode};
use crate::manager::http::AppState;
use crate::manager::{self, deploy};
use crate::store::deployments as depl;

type ApiResult = std::result::Result<Json<serde_json::Value>, AppError>;

pub fn add_routes(router: Router<AppState>) -> Router<AppState> {
    router
        .route("/api/deployments", get(list).post(create))
        .route("/api/deployments/{id}", get(detail))
        .route("/api/deployments/{id}/rollback", post(rollback))
}

#[derive(Deserialize)]
struct CreateReq {
    revision_id: String,
    strategy: Option<String>,
}

async fn create(State(st): State<AppState>, Json(r): Json<CreateReq>) -> ApiResult {
    let strategy = r.strategy.as_deref().unwrap_or("normal");
    let dep_id = deploy::create_deployment(&st.pool, &r.revision_id, strategy, None).await?;
    let client = manager::build_agent_client(&st.pool, &st.cipher).await?;
    deploy::drive(&st.pool, &st.cipher, client.as_ref(), &dep_id).await?;
    detail_body(&st, &dep_id).await
}

async fn list(State(st): State<AppState>) -> ApiResult {
    Ok(Json(
        json!({ "deployments": depl::list_deployments(&st.pool).await? }),
    ))
}

async fn detail(State(st): State<AppState>, Path(id): Path<String>) -> ApiResult {
    detail_body(&st, &id).await
}

async fn detail_body(st: &AppState, id: &str) -> ApiResult {
    let dep = depl::get_deployment(&st.pool, id)
        .await?
        .ok_or_else(|| AppError::new(ErrorCode::NotFound, "deployment 不存在"))?;
    let targets = depl::list_targets(&st.pool, id).await?;
    Ok(Json(json!({ "deployment": dep, "targets": targets })))
}

async fn rollback(State(st): State<AppState>, Path(id): Path<String>) -> ApiResult {
    let dep = depl::get_deployment(&st.pool, &id)
        .await?
        .ok_or_else(|| AppError::new(ErrorCode::NotFound, "deployment 不存在"))?;
    let prev = dep
        .previous_revision_id
        .ok_or_else(|| AppError::new(ErrorCode::Validation, "无 previous revision 可回滚"))?;
    let dep_id = deploy::rollback_to_previous(&st.pool, &prev, None).await?;
    let client = manager::build_agent_client(&st.pool, &st.cipher).await?;
    deploy::drive(&st.pool, &st.cipher, client.as_ref(), &dep_id).await?;
    detail_body(&st, &dep_id).await
}
