use crate::service::TrafficService;
use axum::{
    extract::State,
    http::{header, HeaderMap, StatusCode},
    response::{IntoResponse, Response},
    routing::get,
    Json, Router,
};
use serde::Serialize;
use std::sync::Arc;

pub fn router(service: Arc<TrafficService>) -> Router {
    Router::new()
        .route("/health", get(health))
        .route("/api/v1/traffic", get(traffic))
        .with_state(service)
}

async fn health() -> Json<HealthResponse> {
    Json(HealthResponse { status: "ok" })
}

async fn traffic(
    State(service): State<Arc<TrafficService>>,
    headers: HeaderMap,
) -> Result<Json<impl Serialize>, ApiError> {
    authorize(&headers, service_auth_token(&service))?;
    let snapshot = service.snapshot().map_err(ApiError::internal)?;
    Ok(Json(snapshot))
}

fn authorize(headers: &HeaderMap, token: &str) -> Result<(), ApiError> {
    let Some(value) = headers.get(header::AUTHORIZATION) else {
        return Err(ApiError::unauthorized());
    };
    let Ok(value) = value.to_str() else {
        return Err(ApiError::unauthorized());
    };
    let Some(received) = value.strip_prefix("Bearer ") else {
        return Err(ApiError::unauthorized());
    };
    if !constant_time_eq(received.as_bytes(), token.as_bytes()) {
        return Err(ApiError::unauthorized());
    }
    Ok(())
}

fn constant_time_eq(left: &[u8], right: &[u8]) -> bool {
    if left.len() != right.len() {
        return false;
    }

    let mut diff = 0_u8;
    for (a, b) in left.iter().zip(right) {
        diff |= a ^ b;
    }
    diff == 0
}

fn service_auth_token(service: &TrafficService) -> &str {
    service.auth_token()
}

#[derive(Serialize)]
struct HealthResponse {
    status: &'static str,
}

struct ApiError {
    status: StatusCode,
}

impl ApiError {
    fn unauthorized() -> Self {
        Self {
            status: StatusCode::UNAUTHORIZED,
        }
    }

    fn internal(error: anyhow::Error) -> Self {
        tracing::error!(%error, "request failed");
        Self {
            status: StatusCode::INTERNAL_SERVER_ERROR,
        }
    }
}

impl IntoResponse for ApiError {
    fn into_response(self) -> Response {
        self.status.into_response()
    }
}
