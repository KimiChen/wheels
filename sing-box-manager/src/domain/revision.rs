//! 配置修订与编译产物元数据类型。`ArtifactMeta` **刻意不含 content 字段**——密文与明文都不出类型层，
//! 从根本上杜绝 artifact 明文/密钥经 API 泄漏。

use serde::Serialize;

/// artifact 的 sing-box check 结果（`config_artifacts.check_status`）。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum CheckStatus {
    Pending,
    Passed,
    Failed,
}

impl CheckStatus {
    pub fn as_str(self) -> &'static str {
        match self {
            CheckStatus::Pending => "pending",
            CheckStatus::Passed => "passed",
            CheckStatus::Failed => "failed",
        }
    }
}

#[derive(Debug, Clone, Serialize)]
pub struct ConfigRevision {
    pub id: String,
    pub seq: i64,
    pub status: String,
    pub topology_hash: String,
    pub summary: Option<String>,
    pub created_at: i64,
}

/// artifact 元数据（无 content / 无密钥）——API 与详情页只回这个。
#[derive(Debug, Clone, Serialize)]
pub struct ArtifactMeta {
    pub id: String,
    pub revision_id: String,
    pub host_id: String,
    pub role: String,
    pub scope_ref: String,
    pub content_sha256: String,
    pub byte_size: i64,
    pub target_singbox_version: Option<String>,
    pub check_status: String,
    pub check_output: Option<String>,
    pub generated_at: i64,
    pub checked_at: Option<i64>,
}
