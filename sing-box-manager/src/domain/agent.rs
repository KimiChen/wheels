//! Agent 侧枚举与 DTO。严格对齐迁移的 CHECK 约束；所有对外 DTO 不含密钥字段。

use serde::{Deserialize, Serialize};

/// Agent 期望/观测状态（对齐 `agents.status` CHECK）。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum AgentStatus {
    Unknown,
    Online,
    Offline,
    Error,
}

impl AgentStatus {
    pub fn as_str(self) -> &'static str {
        match self {
            AgentStatus::Unknown => "unknown",
            AgentStatus::Online => "online",
            AgentStatus::Offline => "offline",
            AgentStatus::Error => "error",
        }
    }
}

/// Agent 证书信任状态（对齐 `agent_certificates.trust_status` CHECK）。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum TrustStatus {
    Pending,
    Trusted,
    Revoked,
}

impl TrustStatus {
    pub fn as_str(self) -> &'static str {
        match self {
            TrustStatus::Pending => "pending",
            TrustStatus::Trusted => "trusted",
            TrustStatus::Revoked => "revoked",
        }
    }
    pub fn parse(s: &str) -> Option<Self> {
        match s {
            "pending" => Some(TrustStatus::Pending),
            "trusted" => Some(TrustStatus::Trusted),
            "revoked" => Some(TrustStatus::Revoked),
            _ => None,
        }
    }
}

/// Agent 命令类型（对齐 `agent_commands.kind` CHECK）。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum CommandKind {
    Status,
    Stats,
    Users,
    Reconcile,
    Deploy,
    MeterAck,
    Rollback,
}

impl CommandKind {
    pub fn as_str(self) -> &'static str {
        match self {
            CommandKind::Status => "status",
            CommandKind::Stats => "stats",
            CommandKind::Users => "users",
            CommandKind::Reconcile => "reconcile",
            CommandKind::Deploy => "deploy",
            CommandKind::MeterAck => "meter_ack",
            CommandKind::Rollback => "rollback",
        }
    }
    pub fn parse(s: &str) -> Option<Self> {
        Some(match s {
            "status" => CommandKind::Status,
            "stats" => CommandKind::Stats,
            "users" => CommandKind::Users,
            "reconcile" => CommandKind::Reconcile,
            "deploy" => CommandKind::Deploy,
            "meter_ack" => CommandKind::MeterAck,
            "rollback" => CommandKind::Rollback,
            _ => return None,
        })
    }
}

/// Agent 命令状态机（对齐 `agent_commands.status` CHECK）。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum CommandStatus {
    Pending,
    InFlight,
    Succeeded,
    Failed,
    TimedOut,
    Canceled,
}

impl CommandStatus {
    pub fn as_str(self) -> &'static str {
        match self {
            CommandStatus::Pending => "pending",
            CommandStatus::InFlight => "in_flight",
            CommandStatus::Succeeded => "succeeded",
            CommandStatus::Failed => "failed",
            CommandStatus::TimedOut => "timed_out",
            CommandStatus::Canceled => "canceled",
        }
    }
    pub fn parse(s: &str) -> Option<Self> {
        Some(match s {
            "pending" => CommandStatus::Pending,
            "in_flight" => CommandStatus::InFlight,
            "succeeded" => CommandStatus::Succeeded,
            "failed" => CommandStatus::Failed,
            "timed_out" => CommandStatus::TimedOut,
            "canceled" => CommandStatus::Canceled,
            _ => return None,
        })
    }
    pub fn is_terminal(self) -> bool {
        matches!(
            self,
            CommandStatus::Succeeded
                | CommandStatus::Failed
                | CommandStatus::TimedOut
                | CommandStatus::Canceled
        )
    }
}

/// `GET /v1/status` 响应体：只读、无任何密钥字段。
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct StatusReport {
    pub host_id: String,
    pub agent_version: String,
    pub singbox_version: Option<String>,
    pub current_revision: Option<i64>,
    pub singbox_running: bool,
    pub os: String,
    pub now_unix: i64,
}

/// 发布门禁的阻断原因（fail-closed）。
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum ReadinessReason {
    NoAgent,
    Untrusted,
    Offline,
    Stale,
    CertExpiring,
    SingboxDown,
}

impl ReadinessReason {
    pub fn as_str(self) -> &'static str {
        match self {
            ReadinessReason::NoAgent => "no_agent",
            ReadinessReason::Untrusted => "untrusted",
            ReadinessReason::Offline => "offline",
            ReadinessReason::Stale => "stale",
            ReadinessReason::CertExpiring => "cert_expiring",
            ReadinessReason::SingboxDown => "singbox_down",
        }
    }
}
