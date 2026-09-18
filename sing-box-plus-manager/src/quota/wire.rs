//! `PUT /v3/quota` 的 wire 形状与序列化校验。
//!
//! 节点侧对未知字段**失败关闭**（`quota_endpoint.go` 的 `DisallowUnknownFields`），
//! 与配置面同一纪律：控制面能改变转发行为，宽容解析等于静默降级。
//! 主控这一侧对应的义务是：序列化出去的表必须先在本地过一遍范围校验，
//! 让一个越界的额度在**发送前**失败，而不是换回一个 400 再去猜是哪一条。

use serde::{Deserialize, Serialize};

use crate::collect::SCHEMA_VERSION;

/// 配额路径。与 `schema_version` 成对推进。
pub const QUOTA_PATH: &str = "/v3/quota";

/// 内部额度表示。
///
/// **不用 `0`、`None` 或 `u64::MAX` 充当无限额度哨兵**（README §4.5）：
/// 这三个值都有真实含义——`0` 是零额度（必须下发），`u64::MAX` 是一个合法的大额度。
/// 用哨兵会让「零额度」和「无限额度」在某次重构里互换，而那是个静默的解限额。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Quota {
    /// 省略出表（C16）。
    Unlimited,
    /// 写进表，**包括 0**（C30）。
    Limited(u64),
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct QuotaEntry {
    pub inbound_tag: String,
    pub name: String,
    /// wire 上是 **int64**，节点拒绝负数（C31）。
    pub remaining_bytes: i64,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct QuotaRequest {
    pub schema_version: u32,
    pub node_id: String,
    pub runtime_id: String,
    /// u64 且必须 ≥ 1；`0` 节点返回 400（C31）。
    pub epoch: u64,
    pub entries: Vec<QuotaEntry>,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct QuotaResponse {
    pub schema_version: u32,
    pub epoch: u64,
    pub applied: usize,
}

#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
pub enum SerializeError {
    #[error("epoch 必须 ≥ 1（C31）：节点对 epoch = 0 返回 400")]
    EpochZero,
    #[error("{field} 超出 wire 的 int64 范围：{value}（C31：remaining_bytes 是 int64 不是 u64）")]
    ExceedsInt64 { field: String, value: u64 },
    #[error("{field} 不合法：{detail}")]
    Token { field: String, detail: String },
    #[error("身份重复：{inbound_tag}/{name}（全量表里每个身份只能出现一次）")]
    DuplicateIdentity { inbound_tag: String, name: String },
}

impl QuotaRequest {
    /// 由「该节点所有受限用户的全部当前有效身份」构造一份**完整节点表**。
    ///
    /// `identities` 必须已经是完整集合——本函数无法替调用方验证这一点，
    /// 因为「少了一个身份」在这里看起来和「那个身份本来就是 Unlimited」完全一样。
    /// 这正是 C30 要靠**全量集合断言**测的原因（README §8），不是靠这里的类型。
    pub fn build(
        node_id: impl Into<String>,
        runtime_id: impl Into<String>,
        epoch: u64,
        identities: impl IntoIterator<Item = (String, String, Quota)>,
    ) -> Result<Self, SerializeError> {
        if epoch == 0 {
            return Err(SerializeError::EpochZero);
        }
        let mut entries = Vec::new();
        let mut seen = std::collections::BTreeSet::new();
        for (inbound_tag, name, quota) in identities {
            validate_token("entries[].inbound_tag", &inbound_tag)?;
            validate_token("entries[].name", &name)?;
            if !seen.insert((inbound_tag.clone(), name.clone())) {
                return Err(SerializeError::DuplicateIdentity { inbound_tag, name });
            }
            match quota {
                // Unlimited 必须省略；写成一个很大的数不等价——
                // 那是「限额很大」，仍然会被节点闸断逻辑计数。
                Quota::Unlimited => continue,
                Quota::Limited(value) => {
                    let remaining_bytes =
                        i64::try_from(value).map_err(|_| SerializeError::ExceedsInt64 {
                            field: format!("entries[{inbound_tag}/{name}].remaining_bytes"),
                            value,
                        })?;
                    entries.push(QuotaEntry { inbound_tag, name, remaining_bytes });
                }
            }
        }
        Ok(QuotaRequest {
            schema_version: SCHEMA_VERSION,
            node_id: node_id.into(),
            runtime_id: runtime_id.into(),
            epoch,
            entries,
        })
    }

    /// 是否是纯零额度安全表。
    ///
    /// 这个区分是门禁口径而不是记账口径：正额度表要占用一个新的已结算 sequence，
    /// **纯零额度安全表不受该占用限制**（`docs/data-model.md` §11），
    /// 否则「无新鲜快照时下发全零表」这条 C33 的肯定动作会被自己的门禁挡住。
    pub fn is_zero_table(&self) -> bool {
        self.entries.iter().all(|entry| entry.remaining_bytes == 0)
    }

    pub fn to_bytes(&self) -> Vec<u8> {
        // 节点侧按 Content-Length 读体，不需要尾随换行。
        serde_json::to_vec(self).expect("QuotaRequest 是纯数据结构，序列化不会失败")
    }
}

/// 409 的成因。
///
/// **节点的 409 错误体没有 discriminator**：`runtime_id` 不符与 `epoch` 未前进
/// 返回的是同一个 `{"schema_version":3,"error":{"code":409}}`
/// （`quota_endpoint.go` 两条分支都走 `newErrorBody(statusConflict)`）。
/// 所以**必须取快照读回 `runtime_id` 才能分支**，不能从响应体猜。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ConflictCause {
    /// runtime 已变：旧窗口按闭合证据对账，新 runtime 登记批准；
    /// 立即下发零额度安全表，新 runtime 的 epoch 可从 1 开始。
    RuntimeMismatch,
    /// runtime 相同 → 是 epoch 未前进。从**发送前持久化的最大已分配 epoch** 取更大值，
    /// 不依赖最后一次 200：响应丢失不能证明旧请求没执行。
    StaleEpoch,
    /// 连快照都取不到，无法判别。不得假设成其中任何一种。
    Undetermined,
}

/// 按 §4.5 的第 2、3 步判别 409 成因。
///
/// 第 1 步（校验连接目标与 `node_id`）在调用方完成——那一步不一致时
/// 要先**停止推送并告警**，而不是继续往下分支。
pub fn classify_conflict(
    sent_runtime_id: &str,
    observed_runtime_id: Option<&str>,
) -> ConflictCause {
    match observed_runtime_id {
        None => ConflictCause::Undetermined,
        Some(observed) if observed != sent_runtime_id => ConflictCause::RuntimeMismatch,
        Some(_) => ConflictCause::StaleEpoch,
    }
}

fn validate_token(field: &str, value: &str) -> Result<(), SerializeError> {
    let fail = |detail: String| SerializeError::Token { field: field.to_string(), detail };
    if value.is_empty() {
        return Err(fail("不能为空".into()));
    }
    if value.len() > 128 {
        return Err(fail(format!("超过 128 字节：{}", value.len())));
    }
    for (offset, byte) in value.bytes().enumerate() {
        if byte <= 0x20 || byte >= 0x7f {
            return Err(fail(format!("含非 ASCII 可显示字符（偏移 {offset}）")));
        }
    }
    Ok(())
}
