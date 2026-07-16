//! 发布领域类型：deployments / deployment_targets 状态与 DTO；Agent 部署回执。
//! DTO 无任何密钥/明文配置字段。枚举 `as_str`/`parse` 对齐 0004 CHECK。

use serde::{Deserialize, Serialize};

macro_rules! str_enum {
    ($(#[$m:meta])* $name:ident { $($variant:ident => $lit:literal),+ $(,)? }) => {
        $(#[$m])*
        #[derive(Debug, Clone, Copy, PartialEq, Eq)]
        pub enum $name { $($variant),+ }
        impl $name {
            pub fn as_str(self) -> &'static str { match self { $(Self::$variant => $lit),+ } }
            pub fn parse(s: &str) -> Option<Self> { match s { $($lit => Some(Self::$variant),)+ _ => None } }
        }
    };
}

str_enum!(DeployKind { Deploy => "deploy", Rollback => "rollback" });
str_enum!(Strategy { Normal => "normal", Forced => "forced" });
str_enum!(DeployRole { Entry => "entry", Node => "node" });
str_enum!(
    DeploymentStatus {
        Pending => "pending", DeployingNodes => "deploying_nodes", DeployingEntries => "deploying_entries",
        Activating => "activating", Succeeded => "succeeded", Failed => "failed",
        RollingBack => "rolling_back", RolledBack => "rolled_back",
    }
);
str_enum!(
    TargetStatus {
        Pending => "pending", Dispatched => "dispatched", AwaitingMeterAck => "awaiting_meter_ack",
        Deployed => "deployed", CheckFailed => "check_failed", HealthFailed => "health_failed",
        Failed => "failed", RolledBack => "rolled_back", Skipped => "skipped",
    }
);

impl TargetStatus {
    pub fn is_terminal_ok(self) -> bool {
        matches!(self, TargetStatus::Deployed)
    }
    pub fn is_failure(self) -> bool {
        matches!(
            self,
            TargetStatus::CheckFailed | TargetStatus::HealthFailed | TargetStatus::Failed
        )
    }
}

#[derive(Debug, Clone, Serialize)]
pub struct Deployment {
    pub id: String,
    pub kind: String,
    pub revision_id: String,
    pub previous_revision_id: Option<String>,
    pub status: String,
    pub strategy: String,
    pub error_summary: Option<String>,
    pub created_at: i64,
    pub updated_at: i64,
    pub completed_at: Option<i64>,
}

#[derive(Debug, Clone, Serialize)]
pub struct DeploymentTarget {
    pub id: String,
    pub deployment_id: String,
    pub host_id: String,
    pub artifact_id: String,
    pub role: String,
    pub scope_ref: String,
    pub batch_order: i64,
    pub content_sha256: String,
    pub command_id: Option<String>,
    pub status: String,
    pub applied_revision: Option<i64>,
    pub runtime_epoch: Option<i64>,
    pub error_summary: Option<String>,
    pub attempts: i64,
}

/// Agent 部署命令的下发体（wire；含明文配置，只活在内存 + mTLS，绝不落库）。
#[derive(Serialize, Deserialize, Clone)]
pub struct DeployPush {
    pub revision: i64,
    pub content_sha256: String,
    pub config: serde_json::Value,
    #[serde(default)]
    pub role: String,
    #[serde(default)]
    pub barrier_required: bool,
    /// 平台 entry_id（entry 角色目标下发；Agent 用于 meter_outbox 键与结算关联）。node 目标为 None。
    #[serde(default)]
    pub entry_id: Option<String>,
}

// Debug 脱敏：绝不打印 config（含 PSK）。
impl std::fmt::Debug for DeployPush {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("DeployPush")
            .field("revision", &self.revision)
            .field("content_sha256", &self.content_sha256)
            .field("config", &"<redacted>")
            .field("role", &self.role)
            .field("barrier_required", &self.barrier_required)
            .field("entry_id", &self.entry_id)
            .finish()
    }
}

/// Agent 部署回执（无密钥/无配置；Manager 据此推进 target 状态）。
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct DeployReport {
    pub status: String, // deployed / check_failed / health_failed / sha_mismatch / awaiting_meter_ack
    pub revision: i64,
    #[serde(default)]
    pub runtime_epoch: Option<i64>,
    #[serde(default)]
    pub output: Option<String>, // 脱敏后的 check/health 详情
    #[serde(default)]
    pub health: Option<String>,
}

impl DeployReport {
    /// 映射到 target 状态。
    pub fn target_status(&self) -> TargetStatus {
        match self.status.as_str() {
            "deployed" => TargetStatus::Deployed,
            "check_failed" | "sha_mismatch" => TargetStatus::CheckFailed,
            "health_failed" => TargetStatus::HealthFailed,
            "awaiting_meter_ack" => TargetStatus::AwaitingMeterAck,
            _ => TargetStatus::Failed,
        }
    }
}
