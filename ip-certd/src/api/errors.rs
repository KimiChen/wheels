use crate::{real_ip::RealIpError, service::ServiceError};
use axum::{
    http::StatusCode,
    response::{IntoResponse, Response},
    Json,
};
use serde::Serialize;

#[derive(Debug)]
pub struct ApiError {
    status: StatusCode,
    message: String,
}

impl ApiError {
    pub fn from_real_ip(error: RealIpError) -> Self {
        tracing::warn!(%error, "failed to resolve client real IP");
        Self {
            status: StatusCode::FORBIDDEN,
            message: "could not verify client source IP".to_string(),
        }
    }
}

impl From<ServiceError> for ApiError {
    fn from(error: ServiceError) -> Self {
        match error {
            ServiceError::BadRequest(message) => Self {
                status: StatusCode::BAD_REQUEST,
                message,
            },
            ServiceError::Forbidden(message) => Self {
                status: StatusCode::FORBIDDEN,
                message,
            },
            ServiceError::NotFound(message) => Self {
                status: StatusCode::NOT_FOUND,
                message,
            },
            ServiceError::TooManyRequests(message) => Self {
                status: StatusCode::TOO_MANY_REQUESTS,
                message,
            },
            ServiceError::NotImplemented(message) => Self {
                status: StatusCode::NOT_IMPLEMENTED,
                message,
            },
            ServiceError::Internal(error) => {
                tracing::error!(%error, "request failed");
                Self {
                    status: StatusCode::INTERNAL_SERVER_ERROR,
                    message: "internal error".to_string(),
                }
            }
        }
    }
}

impl IntoResponse for ApiError {
    fn into_response(self) -> Response {
        (
            self.status,
            Json(ErrorBody {
                error: self.message,
            }),
        )
            .into_response()
    }
}

#[derive(Serialize)]
struct ErrorBody {
    error: String,
}
