//! 拓扑 Web/API：Entry/Node/Landing/Route CRUD + 实时校验 + 编译 + 修订/artifact 查询。
//! 挂到 [`crate::manager::http`] 的 `AppState` 路由树；默认脱敏：绝不返回 PSK / socks 凭据 / artifact content。

use axum::extract::{Path, State};
use axum::routing::{get, post};
use axum::{Json, Router};
use serde::Deserialize;
use serde_json::json;

use crate::compiler::validate::validate_route;
use crate::domain::topology::{ExitKind, InboundKind, LandingKind, Network, RouteDraft};
use crate::error::{AppError, ErrorCode};
use crate::manager::http::AppState;
use crate::store::{self, revisions, snapshot, topology};

type ApiResult = std::result::Result<Json<serde_json::Value>, AppError>;

/// 把拓扑路由加入 `AppState` 路由树（with_state 由调用方统一施加）。
pub fn add_routes(router: Router<AppState>) -> Router<AppState> {
    router
        .route("/api/entries", get(list_entries).post(create_entry))
        .route("/api/entries/{id}", get(get_entry).delete(delete_entry))
        .route("/api/nodes", get(list_nodes).post(create_node))
        .route("/api/nodes/{id}", get(get_node).delete(delete_node))
        .route("/api/landings", get(list_landings).post(create_landing))
        .route("/api/landings/{id}", axum::routing::delete(delete_landing))
        .route("/api/routes", get(list_routes).post(create_route))
        .route("/api/routes/{id}", get(get_route).delete(delete_route))
        .route("/api/routes/validate", post(validate_route_ep))
        .route("/api/compile", post(compile))
        .route("/api/revisions", get(list_revisions))
        .route("/api/revisions/{id}", get(get_revision))
        .route("/api/revisions/{id}/check", post(check_revision))
}

fn parse_enum<T>(v: Option<T>, what: &str) -> Result<T, AppError> {
    v.ok_or_else(|| AppError::new(ErrorCode::Validation, format!("非法{what}")))
}

// ---------- Entry ----------

#[derive(Deserialize)]
struct CreateEntryReq {
    host_id: String,
    public_address: String,
    inbound_kind: String,
    ss_method: Option<String>,
    allow_direct: bool,
}

async fn create_entry(State(st): State<AppState>, Json(r): Json<CreateEntryReq>) -> ApiResult {
    let kind = parse_enum(InboundKind::parse(&r.inbound_kind), "inbound_kind")?;
    let id = topology::create_entry(
        &st.pool,
        &st.cipher,
        &topology::NewEntry {
            host_id: &r.host_id,
            public_address: &r.public_address,
            inbound_kind: kind,
            ss_method: r.ss_method.as_deref(),
            allow_direct: r.allow_direct,
        },
    )
    .await?;
    Ok(Json(json!({ "id": id })))
}

async fn list_entries(State(st): State<AppState>) -> ApiResult {
    Ok(Json(
        json!({ "entries": topology::list_entries(&st.pool).await? }),
    ))
}

async fn get_entry(State(st): State<AppState>, Path(id): Path<String>) -> ApiResult {
    let entry = topology::get_entry(&st.pool, &id)
        .await?
        .ok_or_else(|| AppError::new(ErrorCode::NotFound, "entry 不存在"))?;
    Ok(Json(json!({ "entry": entry })))
}

async fn delete_entry(State(st): State<AppState>, Path(id): Path<String>) -> ApiResult {
    topology::delete_entry(&st.pool, &id).await?;
    Ok(Json(json!({ "deleted": id })))
}

// ---------- Node ----------

#[derive(Deserialize)]
struct CreateNodeReq {
    host_id: String,
    data_address: String,
    allow_direct_exit: bool,
}

async fn create_node(State(st): State<AppState>, Json(r): Json<CreateNodeReq>) -> ApiResult {
    let id = topology::create_node(
        &st.pool,
        &st.cipher,
        &r.host_id,
        &r.data_address,
        r.allow_direct_exit,
    )
    .await?;
    Ok(Json(json!({ "id": id })))
}

async fn list_nodes(State(st): State<AppState>) -> ApiResult {
    Ok(Json(
        json!({ "nodes": topology::list_nodes(&st.pool).await? }),
    ))
}

async fn get_node(State(st): State<AppState>, Path(id): Path<String>) -> ApiResult {
    let node = topology::get_node(&st.pool, &id)
        .await?
        .ok_or_else(|| AppError::new(ErrorCode::NotFound, "node 不存在"))?;
    Ok(Json(json!({ "node": node })))
}

async fn delete_node(State(st): State<AppState>, Path(id): Path<String>) -> ApiResult {
    topology::delete_node(&st.pool, &id).await?;
    Ok(Json(json!({ "deleted": id })))
}

// ---------- Landing ----------

#[derive(Deserialize)]
struct CreateLandingReq {
    kind: String,
    node_id: Option<String>,
    socks5_address: Option<String>,
    socks5_port: Option<i64>,
    network: Option<String>,
    socks_user: Option<String>,
    socks_pass: Option<String>,
}

async fn create_landing(State(st): State<AppState>, Json(r): Json<CreateLandingReq>) -> ApiResult {
    let kind = parse_enum(LandingKind::parse(&r.kind), "landing kind")?;
    let network = parse_enum(
        Network::parse(r.network.as_deref().unwrap_or("both")),
        "network",
    )?;
    let id = topology::create_landing(
        &st.pool,
        &st.cipher,
        &topology::NewLanding {
            kind,
            node_id: r.node_id.as_deref(),
            socks5_address: r.socks5_address.as_deref(),
            socks5_port: r.socks5_port,
            network,
            socks_user: r.socks_user.as_deref(),
            socks_pass: r.socks_pass.as_deref(),
        },
    )
    .await?;
    Ok(Json(json!({ "id": id })))
}

async fn list_landings(State(st): State<AppState>) -> ApiResult {
    Ok(Json(
        json!({ "landings": topology::list_landings(&st.pool).await? }),
    ))
}

async fn delete_landing(State(st): State<AppState>, Path(id): Path<String>) -> ApiResult {
    topology::delete_landing(&st.pool, &id).await?;
    Ok(Json(json!({ "deleted": id })))
}

// ---------- Route ----------

#[derive(Deserialize)]
struct RouteReq {
    label: String,
    entry_id: String,
    #[serde(default)]
    hops: Vec<String>,
    exit_kind: String,
    exit_node_id: Option<String>,
    exit_landing_id: Option<String>,
}

impl RouteReq {
    fn into_draft(self, id: Option<String>) -> Result<RouteDraft, AppError> {
        let exit_kind = parse_enum(ExitKind::parse(&self.exit_kind), "exit_kind")?;
        Ok(RouteDraft {
            id,
            label: self.label,
            entry_id: self.entry_id,
            hops: self.hops,
            exit_kind,
            exit_node_id: self.exit_node_id,
            exit_landing_id: self.exit_landing_id,
        })
    }
}

async fn validate_draft(
    st: &AppState,
    draft: &RouteDraft,
) -> Result<Vec<crate::compiler::validate::Issue>, AppError> {
    let ctx = snapshot::load_validation_context(&st.pool).await?;
    let taken = topology::label_taken(&st.pool, &draft.label, draft.id.as_deref()).await?;
    Ok(validate_route(&ctx, draft, taken))
}

async fn create_route(State(st): State<AppState>, Json(r): Json<RouteReq>) -> ApiResult {
    let draft = r.into_draft(None)?;
    let issues = validate_draft(&st, &draft).await?;
    if issues
        .iter()
        .any(|i| i.severity == crate::compiler::validate::Severity::Error)
    {
        return Err(AppError::new(
            ErrorCode::Validation,
            format!(
                "Route 校验未通过: {}",
                serde_json::to_string(&issues).unwrap_or_default()
            ),
        ));
    }
    let id = topology::insert_route(&st.pool, &draft).await?;
    Ok(Json(json!({ "id": id, "issues": issues })))
}

async fn validate_route_ep(State(st): State<AppState>, Json(r): Json<RouteReq>) -> ApiResult {
    let draft = r.into_draft(None)?;
    let issues = validate_draft(&st, &draft).await?;
    let ok = !issues
        .iter()
        .any(|i| i.severity == crate::compiler::validate::Severity::Error);
    Ok(Json(json!({ "ok": ok, "issues": issues })))
}

async fn list_routes(State(st): State<AppState>) -> ApiResult {
    Ok(Json(
        json!({ "routes": topology::list_routes(&st.pool).await? }),
    ))
}

async fn get_route(State(st): State<AppState>, Path(id): Path<String>) -> ApiResult {
    let route = topology::get_route(&st.pool, &id)
        .await?
        .ok_or_else(|| AppError::new(ErrorCode::NotFound, "route 不存在"))?;
    let hops = topology::route_hops(&st.pool, &id).await?;
    Ok(Json(json!({ "route": route, "hops": hops })))
}

async fn delete_route(State(st): State<AppState>, Path(id): Path<String>) -> ApiResult {
    topology::delete_route(&st.pool, &id).await?;
    Ok(Json(json!({ "deleted": id })))
}

// ---------- 编译 / 修订 ----------

#[derive(Deserialize)]
struct CompileReq {
    entry_id: String,
}

async fn compile(State(st): State<AppState>, Json(r): Json<CompileReq>) -> ApiResult {
    // 目标 sing-box 版本 = 该 Entry Host 的 Agent 观测版本（可空）。
    let entry = topology::get_entry(&st.pool, &r.entry_id)
        .await?
        .ok_or_else(|| AppError::new(ErrorCode::NotFound, "entry 不存在"))?;
    let target = store::agents::get_agent(&st.pool, &entry.host_id)
        .await?
        .and_then(|a| a.singbox_version);
    let rev =
        revisions::compile_and_persist(&st.pool, &st.cipher, &r.entry_id, target.as_deref(), None)
            .await?;
    let metas = revisions::list_artifact_meta(&st.pool, &rev.id).await?;
    Ok(Json(json!({ "revision": rev, "artifacts": metas })))
}

async fn list_revisions(State(st): State<AppState>) -> ApiResult {
    Ok(Json(
        json!({ "revisions": revisions::list_revisions(&st.pool).await? }),
    ))
}

async fn get_revision(State(st): State<AppState>, Path(id): Path<String>) -> ApiResult {
    let rev = revisions::get_revision(&st.pool, &id)
        .await?
        .ok_or_else(|| AppError::new(ErrorCode::NotFound, "revision 不存在"))?;
    let artifacts = revisions::list_artifact_meta(&st.pool, &id).await?;
    Ok(Json(json!({ "revision": rev, "artifacts": artifacts })))
}

async fn check_revision(State(st): State<AppState>, Path(id): Path<String>) -> ApiResult {
    let metas = revisions::run_check(&st.pool, &st.cipher, &id).await?;
    Ok(Json(json!({ "artifacts": metas })))
}
