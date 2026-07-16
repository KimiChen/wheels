//! Agent 被动 `/v1/*` 处理器。全部经 mTLS（在 [`super::accept`] 层强制）。
//! `/v1/status` 做实；命令类走 execute_once 幂等骨架（Phase 1 确认即返回，实体逻辑在后续阶段）；
//! 未实现的 GET 返回结构化 501。响应绝无密钥字段。

use std::sync::Arc;

use axum::extract::{Path, State};
use axum::http::{HeaderMap, StatusCode};
use axum::response::{AppendHeaders, IntoResponse, Response};
use axum::routing::{get, post};
use axum::{Json, Router};
use serde_json::{json, Value};
use sqlx::SqlitePool;

use crate::agent::gate::DrainWaitGate;
use crate::agent::idempotency::{self, ExecResult, Outcome};
use crate::agent::runtime::Runtime;
use crate::agent::ssm::SsmClient;
use crate::agent::{deploy, reconcile, settle, singbox, ssm, state};
use crate::domain::agent::StatusReport;
use crate::domain::deployment::DeployPush;
use crate::domain::metering::MeterAckBody;
use crate::domain::user::ReconcilePush;
use crate::store::now_unix;

const CMD_HEADER: &str = "x-sbm-command-id";

#[derive(Clone)]
pub struct AgentState {
    pub host_id: String,
    pub agent_version: String,
    pub state: SqlitePool,
    pub ssm_address: String,
    /// sing-box 进程运行时（部署/回滚受控 stop/start + 健康）。
    pub runtime: Arc<dyn Runtime>,
    /// live 配置与 revisions/ 快照所在目录。
    pub config_dir: String,
    /// 本机 SSM 客户端（用户 reconcile）。
    pub ssm: Arc<dyn SsmClient>,
    /// 结算屏障排空闸门（Phase 5）。
    pub gate: Arc<dyn DrainWaitGate>,
}

pub fn router(state: AgentState) -> Router {
    Router::new()
        .route("/v1/status", get(status))
        .route("/v1/sing-box/stats", get(get_stats))
        .route("/v1/sing-box/users", get(list_ssm_users))
        .route("/v1/sing-box/reconcile", post(reconcile_handler))
        .route("/v1/deployments", post(deploy_handler))
        .route("/v1/deployments/{command_id}", get(get_deployment))
        .route(
            "/v1/deployments/{command_id}/meter-batch",
            get(get_meter_batch),
        )
        .route("/v1/deployments/{command_id}/meter-ack", post(meter_ack))
        .route("/v1/rollback", post(rollback_handler))
        .with_state(state)
}

/// POST /v1/deployments：收 Manager 推送的完整配置（含 PSK，仅内存 + mTLS）→ execute_once 幂等
/// → 真实 sha 校验 + sing-box check + 原子替换 + restart + 健康检查 + 健康失败自动回滚。
async fn deploy_handler(
    State(st): State<AgentState>,
    headers: HeaderMap,
    body: String,
) -> Response {
    let Some(cmd_id) = command_id(&headers) else {
        return missing_id();
    };
    let push: DeployPush = match serde_json::from_str(&body) {
        Ok(p) => p,
        Err(e) => {
            return (
                StatusCode::BAD_REQUEST,
                Json(json!({"error": format!("非法 DeployPush: {e}")})),
            )
                .into_response()
        }
    };
    // 幂等键 = content_sha256（确定性，重投逐字节稳定）。
    let request_hash = push.content_sha256.clone();
    let op = async {
        let report = deploy::execute_deploy(
            &st.state,
            st.runtime.as_ref(),
            st.ssm.as_ref(),
            st.gate.as_ref(),
            &st.config_dir,
            &push,
            &cmd_id,
        )
        .await?;
        Ok::<_, crate::error::AppError>(ExecResult {
            ok: report.status == "deployed",
            http_status: 200,
            body_json: serde_json::to_string(&report).unwrap_or_default(),
        })
    };
    match idempotency::execute_once(&st.state, &cmd_id, "deploy", &request_hash, op).await {
        Ok(r) => match r.outcome {
            Outcome::Conflict => conflict(&cmd_id),
            Outcome::InProgress => in_progress(&cmd_id),
            Outcome::Executed | Outcome::Replayed => reply(&cmd_id, r.result),
        },
        Err(e) => e.into_response(),
    }
}

/// POST /v1/rollback：回滚到上一个成功 revision（幂等）。
async fn rollback_handler(
    State(st): State<AgentState>,
    headers: HeaderMap,
    body: String,
) -> Response {
    let Some(cmd_id) = command_id(&headers) else {
        return missing_id();
    };
    let request_hash = crate::pki::sha256_hex(body.as_bytes());
    let op = async {
        let report =
            deploy::rollback(&st.state, st.runtime.as_ref(), &st.config_dir, "manual").await?;
        Ok::<_, crate::error::AppError>(ExecResult {
            ok: report.status == "rolled_back",
            http_status: 200,
            body_json: serde_json::to_string(&report).unwrap_or_default(),
        })
    };
    match idempotency::execute_once(&st.state, &cmd_id, "rollback", &request_hash, op).await {
        Ok(r) => match r.outcome {
            Outcome::Conflict => conflict(&cmd_id),
            Outcome::InProgress => in_progress(&cmd_id),
            Outcome::Executed | Outcome::Replayed => reply(&cmd_id, r.result),
        },
        Err(e) => e.into_response(),
    }
}

/// POST /v1/deployments/{id}/meter-ack：Manager 确认已 ingest 最终统计（体含 boot_id+sequence 作证明）。
/// 匹配则 phase B（停旧→原子替换→复用 new_epoch 重启→健康）；序号不符则拒绝切换（约束 1）。幂等。
async fn meter_ack(
    State(st): State<AgentState>,
    Path(command_id): Path<String>,
    body: String,
) -> Response {
    let ack: MeterAckBody = match serde_json::from_str(&body) {
        Ok(a) => a,
        Err(e) => {
            return (
                StatusCode::BAD_REQUEST,
                Json(json!({"error": format!("非法 MeterAckBody: {e}")})),
            )
                .into_response()
        }
    };
    match settle::complete_barrier(
        &st.state,
        st.runtime.as_ref(),
        &st.config_dir,
        &command_id,
        ack.singbox_boot_id,
        ack.sequence,
    )
    .await
    {
        Ok(Some(report)) => {
            let val = serde_json::to_value(&report).unwrap_or_else(|_| json!({}));
            Json(val).into_response()
        }
        Ok(None) => Json(json!({"acked": true, "no_pending_barrier": true})).into_response(),
        Err(e) => e.into_response(),
    }
}

/// GET /v1/deployments/{id}/meter-batch：返回暂存的旧进程最终统计（无 uPSK；字节级稳定）。
async fn get_meter_batch(State(st): State<AgentState>, Path(command_id): Path<String>) -> Response {
    match settle::load_meter_batch(&st.state, &command_id).await {
        Ok(mb) => Json(mb).into_response(),
        Err(e) => e.into_response(),
    }
}

/// POST /v1/sing-box/reconcile：收期望身份集（含 uPSK，仅内存+mTLS）→ execute_once 幂等 → 本机 SSM 增删。
async fn reconcile_handler(
    State(st): State<AgentState>,
    headers: HeaderMap,
    body: String,
) -> Response {
    let Some(cmd_id) = command_id(&headers) else {
        return missing_id();
    };
    let push: ReconcilePush = match serde_json::from_str(&body) {
        Ok(p) => p,
        Err(e) => {
            return (
                StatusCode::BAD_REQUEST,
                Json(json!({"error": format!("非法 ReconcilePush: {e}")})),
            )
                .into_response()
        }
    };
    let request_hash = crate::pki::sha256_hex(body.as_bytes());
    let op = async {
        let report = reconcile::execute_reconcile(st.ssm.as_ref(), &push).await?;
        Ok::<_, crate::error::AppError>(ExecResult {
            ok: true,
            http_status: 200,
            body_json: serde_json::to_string(&report).unwrap_or_default(),
        })
    };
    match idempotency::execute_once(&st.state, &cmd_id, "reconcile", &request_hash, op).await {
        Ok(r) => match r.outcome {
            Outcome::Conflict => conflict(&cmd_id),
            Outcome::InProgress => in_progress(&cmd_id),
            Outcome::Executed | Outcome::Replayed => reply(&cmd_id, r.result),
        },
        Err(e) => e.into_response(),
    }
}

/// GET /v1/sing-box/users：本机 SSM 当前用户名单（无 uPSK）。
async fn list_ssm_users(State(st): State<AgentState>) -> Response {
    match st.ssm.list_users(ssm::INBOUND_TAG).await {
        Ok(users) => Json(json!({ "users": users })).into_response(),
        Err(e) => e.into_response(),
    }
}

/// GET /v1/sing-box/stats：读本机 SSM 累计统计 → StatsBatch（盖当前 boot id；无 uPSK）。只读无锁。
async fn get_stats(State(st): State<AgentState>) -> Response {
    match crate::agent::stats::read_local_stats(st.ssm.as_ref(), &st.state).await {
        Ok(batch) => Json(batch).into_response(),
        Err(e) => e.into_response(),
    }
}

async fn status(State(st): State<AgentState>) -> Response {
    let report = StatusReport {
        host_id: st.host_id.clone(),
        agent_version: st.agent_version.clone(),
        singbox_version: singbox::detect_version(),
        current_revision: state::active_revision(&st.state).await.ok().flatten(),
        singbox_running: singbox::probe_running(&st.ssm_address).await,
        os: std::env::consts::OS.to_string(),
        now_unix: now_unix(),
    };
    Json(report).into_response()
}

/// 命令幂等骨架：按 command_id 去重执行。Phase 1 为确认型 no-op（实体逻辑在 Phase 3/4/5）。
async fn command_stub(State(st): State<AgentState>, headers: HeaderMap, body: String) -> Response {
    let Some(cmd_id) = headers
        .get(CMD_HEADER)
        .and_then(|v| v.to_str().ok())
        .map(|s| s.to_string())
    else {
        return (
            StatusCode::BAD_REQUEST,
            Json(json!({"error": "missing_command_id"})),
        )
            .into_response();
    };
    let request_hash = crate::pki::sha256_hex(body.as_bytes());
    let op = async {
        Ok::<_, crate::error::AppError>(ExecResult {
            ok: true,
            http_status: 200,
            body_json: json!({"accepted": true, "implemented": false}).to_string(),
        })
    };
    match idempotency::execute_once(&st.state, &cmd_id, "command", &request_hash, op).await {
        Ok(r) => match r.outcome {
            Outcome::Conflict => (
                StatusCode::CONFLICT,
                echo(&cmd_id),
                Json(json!({"error": "command_id_hash_mismatch"})),
            )
                .into_response(),
            Outcome::InProgress => (
                StatusCode::ACCEPTED,
                echo(&cmd_id),
                Json(json!({"status": "in_progress"})),
            )
                .into_response(),
            Outcome::Executed | Outcome::Replayed => reply(&cmd_id, r.result),
        },
        Err(e) => e.into_response(),
    }
}

async fn get_deployment(State(st): State<AgentState>, Path(command_id): Path<String>) -> Response {
    match idempotency::get_executed(&st.state, &command_id).await {
        Ok(Some(res)) => reply(&command_id, Some(res)),
        Ok(None) => (
            StatusCode::NOT_FOUND,
            Json(json!({"error": "unknown_command"})),
        )
            .into_response(),
        Err(e) => e.into_response(),
    }
}

fn reply(cmd_id: &str, result: Option<ExecResult>) -> Response {
    let Some(res) = result else {
        return StatusCode::INTERNAL_SERVER_ERROR.into_response();
    };
    let val: Value = serde_json::from_str(&res.body_json).unwrap_or_else(|_| json!({}));
    let status = StatusCode::from_u16(res.http_status).unwrap_or(StatusCode::OK);
    (status, echo(cmd_id), Json(val)).into_response()
}

fn echo(cmd_id: &str) -> AppendHeaders<[(&'static str, String); 1]> {
    AppendHeaders([(CMD_HEADER, cmd_id.to_string())])
}

fn command_id(headers: &HeaderMap) -> Option<String> {
    headers
        .get(CMD_HEADER)
        .and_then(|v| v.to_str().ok())
        .map(|s| s.to_string())
}
fn missing_id() -> Response {
    (
        StatusCode::BAD_REQUEST,
        Json(json!({"error": "missing_command_id"})),
    )
        .into_response()
}
fn conflict(cmd_id: &str) -> Response {
    (
        StatusCode::CONFLICT,
        echo(cmd_id),
        Json(json!({"error": "command_id_hash_mismatch"})),
    )
        .into_response()
}
fn in_progress(cmd_id: &str) -> Response {
    (
        StatusCode::ACCEPTED,
        echo(cmd_id),
        Json(json!({"status": "in_progress"})),
    )
        .into_response()
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::agent::state;
    use axum::body::Body;
    use axum::http::Request;
    use tower::ServiceExt;

    async fn app() -> Router {
        let path = std::env::temp_dir().join(format!("sbm-api-{}.db", uuid::Uuid::new_v4()));
        let pool = state::open(&path.to_string_lossy()).await.unwrap();
        router(AgentState {
            host_id: "h1".into(),
            agent_version: "0.1.0".into(),
            state: pool,
            ssm_address: "127.0.0.1:1".into(), // 探测必失败 → singbox_running=false
            runtime: Arc::new(crate::agent::runtime::MockRuntime::default()),
            config_dir: std::env::temp_dir()
                .join(format!("sbm-cfgdir-{}", uuid::Uuid::new_v4()))
                .to_string_lossy()
                .into_owned(),
            ssm: Arc::new(crate::agent::ssm::MockSsmClient::default()),
            gate: Arc::new(crate::agent::gate::SsmDrainGate::default()),
        })
    }

    async fn body_str(resp: Response) -> String {
        let bytes = axum::body::to_bytes(resp.into_body(), usize::MAX)
            .await
            .unwrap();
        String::from_utf8(bytes.to_vec()).unwrap()
    }

    #[tokio::test]
    async fn status_returns_report_without_secrets() {
        let resp = app()
            .await
            .oneshot(
                Request::builder()
                    .uri("/v1/status")
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(resp.status(), StatusCode::OK);
        let b = body_str(resp).await;
        assert!(b.contains("\"host_id\":\"h1\""));
        assert!(!b.contains("PRIVATE KEY") && !b.contains("BEGIN") && !b.contains("psk"));
    }

    #[tokio::test]
    async fn command_is_idempotent_and_conflict_detected() {
        let app = app().await;
        let empty = r#"{"inbound_tag":"in-shared","users":[]}"#;
        let other = r#"{"inbound_tag":"in-shared","users":[{"name":"u1","upsk":"p"}]}"#;
        let mk = |body: &str| {
            Request::builder()
                .method("POST")
                .uri("/v1/sing-box/reconcile")
                .header(CMD_HEADER, "cmd-1")
                .body(Body::from(body.to_string()))
                .unwrap()
        };
        // 首次执行。
        let r1 = app.clone().oneshot(mk(empty)).await.unwrap();
        assert_eq!(r1.status(), StatusCode::OK);
        // 同 id 同体：回放，仍 200。
        let r2 = app.clone().oneshot(mk(empty)).await.unwrap();
        assert_eq!(r2.status(), StatusCode::OK);
        // 同 id 异体：409。
        let r3 = app.clone().oneshot(mk(other)).await.unwrap();
        assert_eq!(r3.status(), StatusCode::CONFLICT);
        // 缺 command_id 头：400。
        let bad = Request::builder()
            .method("POST")
            .uri("/v1/rollback")
            .body(Body::empty())
            .unwrap();
        assert_eq!(
            app.clone().oneshot(bad).await.unwrap().status(),
            StatusCode::BAD_REQUEST
        );
    }

    #[tokio::test]
    async fn get_deployment_404_then_200() {
        let app = app().await;
        let miss = app
            .clone()
            .oneshot(
                Request::builder()
                    .uri("/v1/deployments/nope")
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(miss.status(), StatusCode::NOT_FOUND);

        // 先建命令（用 sha 不匹配的 DeployPush → sha_mismatch 报告，仍被 execute_once 记录），再查。
        let push = serde_json::json!({
            "revision": 1, "content_sha256": "wrong", "config": {}, "role": "node", "barrier_required": false
        });
        app.clone()
            .oneshot(
                Request::builder()
                    .method("POST")
                    .uri("/v1/deployments")
                    .header(CMD_HEADER, "cmd-x")
                    .body(Body::from(push.to_string()))
                    .unwrap(),
            )
            .await
            .unwrap();
        let hit = app
            .oneshot(
                Request::builder()
                    .uri("/v1/deployments/cmd-x")
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(hit.status(), StatusCode::OK);
    }

    #[tokio::test]
    async fn stats_endpoint_returns_batch() {
        // MockSsmClient 无用户 → 空 StatsBatch，200。
        let resp = app()
            .await
            .oneshot(
                Request::builder()
                    .uri("/v1/sing-box/stats")
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(resp.status(), StatusCode::OK);
        let b = body_str(resp).await;
        assert!(b.contains("\"singbox_boot_id\"") && b.contains("\"inbound_tag\":\"in-shared\""));
    }
}
