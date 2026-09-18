//! 固定形状的错误对象（README §6）。
//!
//! ```json
//! { "error": { "code": "quota_stale_epoch", "message": "…", "detail": { "node_id": "…" } } }
//! ```
//!
//! **形状不随端点变化。** `code` 是稳定的机器可读枚举——人和 Agent 都按它分支，
//! 所以它是合同的一部分，改了等于改合同；`message` 给人看；`detail` 给排查用。
//!
//! 仓库根 README 的口径是「明确输入、结构化输出、**有用的失败**」。
//! 「有用」的判据很具体：拿到这个对象的人应当知道**下一步做什么**，
//! 而不只是知道「失败了」。

use axum::http::StatusCode;
use axum::response::{IntoResponse, Response};
use serde::Serialize;
use serde_json::{json, Value};

/// 稳定的机器可读错误码。
///
/// 新增只许追加，不许改已有的拼写或语义。
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum ApiCode {
    /// 没有有效会话。
    Unauthenticated,
    /// 有会话但角色不够。**越权在服务端拦**，不是前端隐藏。
    Forbidden,
    /// 会话已被吊销。与 `Unauthenticated` 分开：前者是「你本来有」。
    SessionRevoked,
    /// 成员事实过期且刷新失败（D13）。**不允许用过期事实继续放行。**
    MemberFactStale,
    /// 双提交 CSRF token 不匹配或缺失。
    CsrfInvalid,
    NotFound,
    InvalidRequest,
    /// `If-Match` 与当前 revision 不符（乐观并发）。
    RevisionMismatch,
    /// 同一 `Idempotency-Key` 配了不同的请求体。
    IdempotencyConflict,
    /// 调低额度未经预览确认。
    ConfirmationRequired,
    /// §4.8 的全局审计开关没开。
    ///
    /// **不是 `Forbidden`**：没有记录可看和不让你看是两回事（D18）。
    AuditNotEnabled,
    Internal,
}

impl ApiCode {
    pub fn status(self) -> StatusCode {
        match self {
            ApiCode::Unauthenticated | ApiCode::SessionRevoked | ApiCode::MemberFactStale => {
                StatusCode::UNAUTHORIZED
            }
            ApiCode::Forbidden | ApiCode::CsrfInvalid => StatusCode::FORBIDDEN,
            ApiCode::NotFound => StatusCode::NOT_FOUND,
            ApiCode::InvalidRequest => StatusCode::BAD_REQUEST,
            ApiCode::RevisionMismatch => StatusCode::PRECONDITION_FAILED,
            ApiCode::IdempotencyConflict | ApiCode::AuditNotEnabled => StatusCode::CONFLICT,
            ApiCode::ConfirmationRequired => StatusCode::UNPROCESSABLE_ENTITY,
            ApiCode::Internal => StatusCode::INTERNAL_SERVER_ERROR,
        }
    }

    pub fn as_str(self) -> &'static str {
        match self {
            ApiCode::Unauthenticated => "unauthenticated",
            ApiCode::Forbidden => "forbidden",
            ApiCode::SessionRevoked => "session_revoked",
            ApiCode::MemberFactStale => "member_fact_stale",
            ApiCode::CsrfInvalid => "csrf_invalid",
            ApiCode::NotFound => "not_found",
            ApiCode::InvalidRequest => "invalid_request",
            ApiCode::RevisionMismatch => "revision_mismatch",
            ApiCode::IdempotencyConflict => "idempotency_conflict",
            ApiCode::ConfirmationRequired => "confirmation_required",
            ApiCode::AuditNotEnabled => "audit_not_enabled",
            ApiCode::Internal => "internal",
        }
    }
}

#[derive(Debug, Clone)]
pub struct ApiError {
    pub code: ApiCode,
    pub message: String,
    pub detail: Value,
}

impl ApiError {
    pub fn new(code: ApiCode, message: impl Into<String>) -> Self {
        ApiError { code, message: message.into(), detail: json!({}) }
    }

    pub fn with_detail(mut self, detail: Value) -> Self {
        self.detail = detail;
        self
    }

    pub fn unauthenticated() -> Self {
        ApiError::new(ApiCode::Unauthenticated, "需要登录")
    }

    /// 越权。**响应里不能带任何被保护的数据**——
    /// 「拿不到的数据根本不该出现在响应里，而不是发过来再藏起来」（§4.9）。
    pub fn forbidden() -> Self {
        ApiError::new(ApiCode::Forbidden, "当前角色无权访问该资源")
    }

    pub fn not_found(what: &str) -> Self {
        ApiError::new(ApiCode::NotFound, format!("找不到{what}"))
    }

    pub fn invalid(message: impl Into<String>) -> Self {
        ApiError::new(ApiCode::InvalidRequest, message)
    }

    pub fn internal(message: impl Into<String>) -> Self {
        // 内部错误的细节不进响应体：它可能含路径、SQL 或身份名。
        tracing::error!(detail = %message.into(), "内部错误");
        ApiError::new(ApiCode::Internal, "内部错误")
    }
}

impl IntoResponse for ApiError {
    fn into_response(self) -> Response {
        let body = json!({
            "error": {
                "code": self.code.as_str(),
                "message": self.message,
                "detail": self.detail,
            }
        });
        (self.code.status(), axum::Json(body)).into_response()
    }
}

impl From<crate::error::Error> for ApiError {
    fn from(error: crate::error::Error) -> Self {
        ApiError::internal(error.to_string())
    }
}

pub type ApiResult<T> = std::result::Result<T, ApiError>;

/// 分页游标（§6：统一 `?cursor=&limit=`，返回 `next_cursor`，**不用 offset**）。
///
/// 不用 offset 的理由是它在并发写入下会漏行或重复行：
/// 第二页开始之前插入一行，`OFFSET 20` 就会把原来的第 20 行推到第 21 位，
/// 于是它既不在第一页也不在第二页。
#[derive(Debug, Clone, Copy)]
pub struct Page {
    pub after: Option<i64>,
    pub limit: i64,
}

pub const DEFAULT_LIMIT: i64 = 50;
pub const MAX_LIMIT: i64 = 500;

impl Page {
    pub fn parse(cursor: Option<&str>, limit: Option<i64>) -> ApiResult<Self> {
        let after = match cursor {
            Some(text) if !text.is_empty() => {
                Some(text.parse::<i64>().map_err(|_| ApiError::invalid("cursor 不是合法游标"))?)
            }
            _ => None,
        };
        let limit = limit.unwrap_or(DEFAULT_LIMIT);
        if limit <= 0 || limit > MAX_LIMIT {
            return Err(ApiError::invalid(format!("limit 必须在 1..={MAX_LIMIT} 之间")));
        }
        Ok(Page { after, limit })
    }
}
