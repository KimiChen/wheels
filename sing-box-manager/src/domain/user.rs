//! 用户与订阅领域类型。管理 API DTO 无 uPSK/serverPSK/token 明文；reconcile wire 体含 uPSK 但 Debug 脱敏。

use serde::{Deserialize, Serialize};

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ResetCycle {
    Monthly,
    Yearly,
    Never,
}

impl ResetCycle {
    pub fn as_str(self) -> &'static str {
        match self {
            ResetCycle::Monthly => "monthly",
            ResetCycle::Yearly => "yearly",
            ResetCycle::Never => "never",
        }
    }
    pub fn parse(s: &str) -> Option<Self> {
        match s {
            "monthly" => Some(ResetCycle::Monthly),
            "yearly" => Some(ResetCycle::Yearly),
            "never" => Some(ResetCycle::Never),
            _ => None,
        }
    }
}

#[derive(Debug, Clone, Serialize)]
pub struct User {
    pub id: String,
    pub name: String,
    pub quota_bytes: i64,
    pub reset_cycle: String,
    pub expire_at: Option<i64>,
    pub disabled: bool,
    pub created_at: i64,
    pub updated_at: i64,
}

impl User {
    /// 是否可用（未停用且未过期）。用量配额检查在 Phase 5。
    pub fn eligible(&self, now: i64) -> bool {
        !self.disabled && self.expire_at.map(|e| e > now).unwrap_or(true)
    }
}

/// 用户的一条 Route 授权（详情页；无密钥）。
#[derive(Debug, Clone, Serialize)]
pub struct UserRouteRow {
    pub route_id: String,
    pub route_label: String,
    pub route_status: String,
    pub identity_name: Option<String>,
    pub identity_label: Option<String>,
}

/// Agent reconcile 下发体（wire；含 uPSK，仅内存 + mTLS，绝不落库/日志）。
#[derive(Serialize, Deserialize, Clone)]
pub struct ReconcilePush {
    pub inbound_tag: String,
    pub users: Vec<ReconcileUser>,
}

#[derive(Serialize, Deserialize, Clone)]
pub struct ReconcileUser {
    pub name: String,
    pub upsk: String,
}

impl std::fmt::Debug for ReconcileUser {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("ReconcileUser")
            .field("name", &self.name)
            .field("upsk", &"<redacted>")
            .finish()
    }
}
impl std::fmt::Debug for ReconcilePush {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("ReconcilePush")
            .field("inbound_tag", &self.inbound_tag)
            .field("users", &self.users)
            .finish()
    }
}

/// Agent reconcile 回执（无密钥；仅名字与计数）。
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ReconcileReport {
    pub added: Vec<String>,
    pub removed: Vec<String>,
    pub present: Vec<String>,
}
