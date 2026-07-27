//! 统一错误码与错误类型。API 响应、结构化日志、审计日志引用同一稳定错误码。

use std::fmt;

/// 稳定错误码注册表。部分码在后续阶段（API/Agent/发布）使用。
#[allow(dead_code)]
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ErrorCode {
    Config,
    Crypto,
    Db,
    Migration,
    Validation,
    NotFound,
    Conflict,
    Unauthorized,
    Forbidden,
    Agent,
    Deployment,
    Internal,
}

impl ErrorCode {
    pub fn as_str(self) -> &'static str {
        match self {
            ErrorCode::Config => "config",
            ErrorCode::Crypto => "crypto",
            ErrorCode::Db => "db",
            ErrorCode::Migration => "migration",
            ErrorCode::Validation => "validation",
            ErrorCode::NotFound => "not_found",
            ErrorCode::Conflict => "conflict",
            ErrorCode::Unauthorized => "unauthorized",
            ErrorCode::Forbidden => "forbidden",
            ErrorCode::Agent => "agent",
            ErrorCode::Deployment => "deployment",
            ErrorCode::Internal => "internal",
        }
    }
}

#[derive(Debug)]
pub struct AppError {
    pub code: ErrorCode,
    pub message: String,
    pub source: Option<anyhow::Error>,
}

impl AppError {
    pub fn new(code: ErrorCode, message: impl Into<String>) -> Self {
        Self {
            code,
            message: message.into(),
            source: None,
        }
    }
    pub fn with(code: ErrorCode, message: impl Into<String>, source: anyhow::Error) -> Self {
        Self {
            code,
            message: message.into(),
            source: Some(source),
        }
    }
}

impl fmt::Display for AppError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "[{}] {}", self.code.as_str(), self.message)?;
        if let Some(s) = &self.source {
            write!(f, ": {s}")?;
        }
        Ok(())
    }
}

impl std::error::Error for AppError {}

impl From<sqlx::Error> for AppError {
    fn from(e: sqlx::Error) -> Self {
        AppError::with(ErrorCode::Db, "数据库错误", e.into())
    }
}

pub type Result<T> = std::result::Result<T, AppError>;
