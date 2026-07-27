//! 可观测 HTTP：`/metrics`(Prometheus，自带 scrape 守卫，在会话中间件之外) + `/readyz`(就绪探针) 为公开面；
//! `/api/metrics`(JSON) + `/api/health`(聚合健康) 走管理只读面。指标/健康绝无密钥。

use axum::extract::State;
use axum::http::{header, HeaderMap, StatusCode};
use axum::response::{IntoResponse, Response};
use axum::routing::get;
use axum::{Json, Router};
use serde::Serialize;
use serde_json::json;

use crate::manager::http::AppState;
use crate::manager::metrics::render_prometheus;
use crate::store::{observability, settings};

/// 判定 agent「在线」的 last_polled_at 新鲜阈值（≈ 3×poll 周期）。
const AGENT_ONLINE_SECS: i64 = 180;

pub fn add_public_routes(router: Router<AppState>) -> Router<AppState> {
    router
        .route("/metrics", get(metrics_prom))
        .route("/readyz", get(readyz))
}

pub fn add_readonly_routes(router: Router<AppState>) -> Router<AppState> {
    router
        .route("/api/metrics", get(metrics_json))
        .route("/api/health", get(health))
}

/// GET /metrics：Prometheus 文本。scrape 守卫——设了 `metrics_scrape_token` 则要求 Bearer 匹配；
/// 未设（默认）依赖 Manager 默认回环绑定，直接放行（非回环暴露须设 token，见部署文档）。
async fn metrics_prom(State(st): State<AppState>, headers: HeaderMap) -> Response {
    let token = settings::get_str(&st.pool, "metrics_scrape_token", "")
        .await
        .unwrap_or_default();
    if !token.is_empty() {
        let ok = headers
            .get(header::AUTHORIZATION)
            .and_then(|v| v.to_str().ok())
            .and_then(|v| v.strip_prefix("Bearer "))
            .map(|t| ct_eq(t.as_bytes(), token.as_bytes()))
            .unwrap_or(false);
        if !ok {
            return (StatusCode::FORBIDDEN, "FORBIDDEN\n").into_response();
        }
    }
    match observability::metrics_snapshot(&st.pool, AGENT_ONLINE_SECS).await {
        Ok(snap) => {
            let now = crate::store::now_unix();
            let body =
                render_prometheus(&snap, env!("CARGO_PKG_VERSION"), now - st.started_at, now);
            ([(header::CONTENT_TYPE, "text/plain; version=0.0.4")], body).into_response()
        }
        Err(_) => (StatusCode::INTERNAL_SERVER_ERROR, "ERROR\n").into_response(),
    }
}

async fn metrics_json(State(st): State<AppState>) -> Response {
    match observability::metrics_snapshot(&st.pool, AGENT_ONLINE_SECS).await {
        Ok(snap) => Json(snap).into_response(),
        Err(e) => e.into_response(),
    }
}

/// GET /readyz：就绪探针（DB 可达即就绪）；区别于 /healthz 存活探针。公开。
async fn readyz(State(st): State<AppState>) -> Response {
    match sqlx::query_scalar::<_, i64>("SELECT 1")
        .fetch_one(&st.pool)
        .await
    {
        Ok(_) => (StatusCode::OK, "ready\n").into_response(),
        Err(_) => (StatusCode::SERVICE_UNAVAILABLE, "not ready\n").into_response(),
    }
}

/// 聚合健康摘要。
#[derive(Debug, Clone, Serialize)]
pub struct HealthSummary {
    pub status: &'static str, // ok / degraded / critical
    pub reasons: Vec<String>,
    pub agents_total: i64,
    pub agents_online: i64,
    pub entries_stale: i64,
    pub users_over_quota: i64,
    pub alerts_firing: i64,
    pub deployments_failed: i64,
}

pub async fn health_summary(st: &AppState) -> Result<HealthSummary, crate::error::AppError> {
    let m = observability::metrics_snapshot(&st.pool, AGENT_ONLINE_SECS).await?;
    let deployments_failed: i64 = m
        .deployments_by_status
        .iter()
        .filter(|(s, _)| s == "failed" || s == "rolled_back")
        .map(|(_, n)| n)
        .sum();
    let mut reasons = Vec::new();
    let mut critical = false;
    let mut degraded = false;
    if m.entries_stale > 0 {
        critical = true;
        reasons.push(format!("{} 个 Entry 计量过期", m.entries_stale));
    }
    if m.agents_total > 0 && m.agents_online == 0 {
        critical = true;
        reasons.push("全部 Agent 离线".into());
    } else if m.agents_online < m.agents_total {
        degraded = true;
        reasons.push(format!(
            "{} 个 Agent 离线",
            m.agents_total - m.agents_online
        ));
    }
    if m.users_over_quota > 0 {
        degraded = true;
        reasons.push(format!("{} 个用户超额", m.users_over_quota));
    }
    if m.alerts_firing > 0 {
        degraded = true;
        reasons.push(format!("{} 条告警触发中", m.alerts_firing));
    }
    let status = if critical {
        "critical"
    } else if degraded {
        "degraded"
    } else {
        "ok"
    };
    Ok(HealthSummary {
        status,
        reasons,
        agents_total: m.agents_total,
        agents_online: m.agents_online,
        entries_stale: m.entries_stale,
        users_over_quota: m.users_over_quota,
        alerts_firing: m.alerts_firing,
        deployments_failed,
    })
}

async fn health(State(st): State<AppState>) -> Response {
    match health_summary(&st).await {
        Ok(s) => Json(json!(s)).into_response(),
        Err(e) => e.into_response(),
    }
}

fn ct_eq(a: &[u8], b: &[u8]) -> bool {
    if a.len() != b.len() {
        return false;
    }
    let mut d = 0u8;
    for (x, y) in a.iter().zip(b) {
        d |= x ^ y;
    }
    d == 0
}
