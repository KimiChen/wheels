//! 计量 DTO。全部无密钥（只含 identity_name + 字节/会话数），Debug 安全。

use serde::{Deserialize, Serialize};

/// Agent 读本机 SSM /stats 的一批。`singbox_boot_id` = Agent 当前 active runtime_epoch（boot id 语义）。
/// 不含 entry_id：Agent 不知平台 entry_id，由 Manager 按所轮询的 Entry 填充。
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct StatsBatch {
    pub inbound_tag: String,
    pub singbox_boot_id: i64,
    pub sequence: i64,
    pub observed_at: i64,
    pub tcp_sessions: i64,
    pub udp_sessions: i64,
    pub users: Vec<StatsUser>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct StatsUser {
    pub identity_name: String,
    pub uplink_bytes: i64,
    pub downlink_bytes: i64,
}

/// 结算屏障 phase A 回执（awaiting_meter_ack）。
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct SettleReport {
    pub singbox_boot_id: i64,
    pub sequence: i64,
    pub drained: bool,
    pub tcp_sessions: i64,
    pub udp_sessions: i64,
    pub batch: StatsBatch,
}

/// GET /v1/deployments/{id}/meter-batch 回执：待结算的最终批（无则 batch=None）。
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct MeterBatchResponse {
    pub batch: Option<StatsBatch>,
    #[serde(default)]
    pub drain_clean: bool,
}

/// meter-ack 请求体。
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct MeterAckBody {
    pub revision: i64,
    pub singbox_boot_id: i64,
    pub sequence: i64,
}

/// 每用户当前周期用量摘要。
#[derive(Debug, Clone, Serialize)]
pub struct UsageSummary {
    pub user_id: String,
    pub used_bytes: i64,
    pub quota_bytes: i64,
    pub period: String,
}
