pub mod certificates;
pub mod errors;

use crate::service::IpCertService;
use axum::{routing::get, Router};
use serde::Serialize;
use std::sync::Arc;

pub fn router(service: Arc<IpCertService>) -> Router {
    Router::new()
        .route("/health", get(health))
        .route("/api/health", get(health))
        .route(
            "/api/v1/certificates/:ip/bundle",
            axum::routing::post(certificates::bundle),
        )
        .route(
            "/v1/certificates/:ip/bundle",
            axum::routing::post(certificates::bundle),
        )
        .with_state(service)
}

async fn health() -> axum::Json<HealthResponse> {
    axum::Json(HealthResponse { status: "ok" })
}

#[derive(Serialize)]
struct HealthResponse {
    status: &'static str,
}
