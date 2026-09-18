//! 下发状态机（README §4.5「下发纪律」与「下发状态机」）。
//!
//! 顺序固定：**先入账 → 计算所有受限用户预算 → 持久化请求与 epoch → 推送 → 记录结果**。
//! 前两步是调用方的事（[`crate::ledger`] 与 [`crate::quota::allocate`]），
//! 这里从第三步开始。
//!
//! **持久化在发送之前。** 这不是稳妥起见：epoch 必须在请求出网之前就被占用，
//! 否则「发送后崩溃」会留下一个我们不知道分配过的 epoch，
//! 恢复时按「最后一次 200」去推下一个值就会撞上 409。
//!
//! `unknown` **不做特别记账**（D20）。早先版本为「结果不明的在途授信」维护一笔预留，
//! 冻结预算并要求人工核销才能释放。那一层连同 C34 一起去掉了：配额是内部限额，
//! 几 GiB 的重复授信不构成问题，而预留带来的冻结、核销与 `H > pool` 对账
//! 是一整套没有对应收益的机制。**代价是超发没有有限上界**，写在 §10 R2。

use sha2::{Digest, Sha256};
use sqlx::Row;
use time::OffsetDateTime;

use crate::agent::AgentClient;
use crate::error::{Error, Result};
use crate::ledger::bucket;
use crate::quota::wire::{Quota, QuotaRequest, QUOTA_PATH};
use crate::store::codec::U64Text;
use crate::store::Store;

/// 正额度表要占用一个新的已结算 sequence；纯零额度安全表不受该占用限制。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum TableKind {
    Positive,
    Zero,
}

impl TableKind {
    pub fn as_str(self) -> &'static str {
        match self {
            TableKind::Positive => "positive",
            TableKind::Zero => "zero",
        }
    }

    /// 从表内容判断。全零即零表——**空表不算**，空表是解除限额。
    pub fn of(entries: &[(String, String, Quota)]) -> Self {
        let all_zero = entries.iter().all(|(_, _, q)| matches!(q, Quota::Limited(0)));
        if all_zero && !entries.is_empty() {
            TableKind::Zero
        } else {
            TableKind::Positive
        }
    }
}

/// 一份**已持久化、尚未发送**的请求。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Prepared {
    pub request_id: i64,
    pub node_id: String,
    pub runtime_id: String,
    pub epoch: u64,
    pub kind: TableKind,
    pub source_sequence: Option<u64>,
    pub body: Vec<u8>,
    pub body_sha256: String,
}

/// 一次推送的结果。
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum PushOutcome {
    /// 200：本请求已生效。epoch 在发送前已经占用。
    Applied { applied: usize },
    /// 409：node/runtime 不符或 epoch 未前进，**错误体不区分**。
    Conflict,
    /// 400 且回了未知身份：配置与全量清单不一致，请求未应用。**P1 告警。**
    UnknownIdentity { unknown: Vec<String> },
    /// 其他 400：序列化或范围出了问题。
    BadRequest,
    /// 已确认未执行的 429（或 agent 级可重试）。退避后重新核验账本与预算。
    Retryable { status: u16 },
    /// 超时 / 连接重置 / 无可信响应：**是否应用未知**。
    Unknown { detail: String },
}

impl PushOutcome {
    /// 落库用的结果枚举（`docs/data-model.md` §11）。
    pub fn result(&self) -> &'static str {
        match self {
            PushOutcome::Applied { .. } => "applied",
            PushOutcome::Conflict
            | PushOutcome::UnknownIdentity { .. }
            | PushOutcome::BadRequest => "rejected",
            // **可重试与结果不明都记 unknown。** 429 虽然「已确认未执行」，
            // 但把它记成 rejected 会让恢复流程以为节点明确拒绝过，
            // 而那两种情形的处置不同。
            PushOutcome::Retryable { .. } | PushOutcome::Unknown { .. } => "unknown",
        }
    }
}

/// 409 的三步分支（README §4.5）。
///
/// **不从响应体猜原因**——节点对两种成因返回同一个错误体。
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum ConflictResolution {
    /// 第 1 步：连接目标或 `node_id` 不一致。**先停止推送并告警**，不要继续往下分支。
    NodeMismatch { sent: String, observed: String },
    /// 第 2 步：`runtime_id` 已变。旧窗口按闭合证据对账，新 runtime 登记批准；
    /// 立即下发零额度安全表，新 runtime 的 epoch **可从 1 开始**。
    RuntimeChanged { observed_runtime: String },
    /// 第 3 步：runtime 相同 → 是 epoch 未前进。
    /// 从**发送前持久化的最大已分配 epoch** 取更大值，**不依赖最后一次 200**。
    StaleEpoch { next_epoch: u64 },
    /// 取不到快照，无法判别。**不得假设成其中任何一种。**
    Undetermined,
}

/// 下一个可用的 epoch。
///
/// **取最大已分配值 + 1，不是「最后一次 200」+ 1**（`docs/data-command.md` §11）。
/// 响应丢失不能证明旧请求没执行；按最后一次 200 恢复会重发一个已被节点接受的 epoch，
/// 稳定地撞 409。
pub async fn next_epoch(store: &Store, node_id: &str, runtime_id: &str) -> Result<u64> {
    let row =
        sqlx::query("SELECT MAX(epoch) FROM quota_requests WHERE node_id = ? AND runtime_id = ?")
            .bind(node_id)
            .bind(runtime_id)
            .fetch_one(store.readers())
            .await?;
    // 定宽文本，字典序即数值序，所以 SQL 的 MAX 在这里是安全的。
    let max: Option<String> = row.get(0);
    Ok(match max {
        Some(text) => U64Text::decode(&text)?.get().checked_add(1).ok_or_else(|| {
            // epoch 耗尽时失败关闭，不回绕（C31）。
            Error::Quota("epoch 已耗尽：必须换 runtime，不得回绕".into())
        })?,
        // 新 runtime 从 1 开始。`epoch = 0` 节点返回 400。
        None => 1,
    })
}

/// 在单写者事务里持久化一份请求，**然后才允许发送**。
pub async fn prepare(
    store: &Store,
    node_id: &str,
    runtime_id: &str,
    entries: Vec<(String, String, Quota)>,
    source_sequence: Option<u64>,
    setting_revision: i64,
    budget_version: i64,
) -> Result<Prepared> {
    let kind = TableKind::of(&entries);
    if kind == TableKind::Positive && source_sequence.is_none() {
        return Err(Error::Quota(
            "正额度表必须绑定一个已结算的 sequence：\
             仅「快照还没过 TTL」不足以授权反复刷新同一份余额"
                .into(),
        ));
    }

    let mut txn = store.begin_immediate().await?;
    // epoch 在同一事务里取，避免两条并发的下发命令拿到同一个值。
    // 每节点额度命令本来就单飞，这里是第二道。
    let row =
        sqlx::query("SELECT MAX(epoch) FROM quota_requests WHERE node_id = ? AND runtime_id = ?")
            .bind(node_id)
            .bind(runtime_id)
            .fetch_one(txn.conn())
            .await?;
    let epoch = match row.get::<Option<String>, _>(0) {
        Some(text) => U64Text::decode(&text)?
            .get()
            .checked_add(1)
            .ok_or_else(|| Error::Quota("epoch 已耗尽：必须换 runtime，不得回绕".into()))?,
        None => 1,
    };

    let request = QuotaRequest::build(node_id, runtime_id, epoch, entries)
        .map_err(|e| Error::Quota(format!("构造完整表失败：{e}")))?;
    let body = request.to_bytes();
    let body_sha256 = hex::encode(Sha256::digest(&body));
    let now = bucket::to_rfc3339(OffsetDateTime::now_utc());

    let inserted = sqlx::query(
        "INSERT INTO quota_requests(node_id, runtime_id, epoch, table_kind, source_sequence, \
         request_body, body_sha256, budget_version, setting_revision, prepared_at, result) \
         VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'prepared') RETURNING request_id",
    )
    .bind(node_id)
    .bind(runtime_id)
    .bind(U64Text::new(epoch).encode())
    .bind(kind.as_str())
    .bind(source_sequence.map(|s| U64Text::new(s).encode()))
    .bind(&body)
    .bind(&body_sha256)
    .bind(budget_version)
    .bind(setting_revision)
    .bind(&now)
    .fetch_one(txn.conn())
    .await;

    let request_id = match inserted {
        Ok(row) => row.get::<i64, _>(0),
        Err(error) => {
            txn.rollback().await?;
            // 部分唯一索引挡住「同一 sequence 支撑第二份正额度表」。
            let message = error.to_string();
            if message.contains("UNIQUE") || message.contains("constraint") {
                return Err(Error::Quota(format!(
                    "该 sequence 已支撑过一份正额度表（或 epoch 已被分配）：{message}。\
                     手动重算遇到这种情况要主动排队采集新快照，不能复用旧余额"
                )));
            }
            return Err(Error::Store(error));
        }
    };
    txn.commit().await?;

    Ok(Prepared {
        request_id,
        node_id: node_id.to_string(),
        runtime_id: runtime_id.to_string(),
        epoch,
        kind,
        source_sequence,
        body,
        body_sha256,
    })
}

/// 把已持久化的请求推给节点。
pub async fn send(client: &AgentClient, prepared: &Prepared) -> PushOutcome {
    let response = match client.quota(prepared.body.clone()).await {
        Ok(response) => response,
        // 传输层失败：**是否应用未知**。不得当成「没发生」。
        Err(error) => return PushOutcome::Unknown { detail: error.to_string() },
    };
    // **只有 429 算「已确认未执行」。**
    //
    // 这里刻意**不**复用 `AgentResponse::retryable()`——那个判据是给采集路径用的，
    // 它把 agent 的 502（连不上本机 UDS）也算成可重试。在采集路径上那是对的：
    // 一次失败的 `GET` 没有副作用，重试是免费的。
    //
    // 配额路径上不成立：**`PUT` 可能已经被节点应用了，只是响应丢了**——
    // 那正是「节点已应用但 200 丢失」，按 §4.5 必须记 `unknown` 并用更高 epoch 收敛。
    // 把它当成「可重试」会让恢复流程以为节点没收到，于是重发同一个 epoch 撞 409。
    //
    // 两条路径必须不对称，因为一边的失败没有副作用、另一边有。
    let confirmed_not_executed =
        response.agent_status == 429 || response.upstream_status == Some(429);
    if confirmed_not_executed {
        return PushOutcome::Retryable {
            status: response.upstream_status.unwrap_or(response.agent_status),
        };
    }
    match response.upstream_status {
        Some(200) => {
            let applied =
                serde_json::from_slice::<crate::quota::wire::QuotaResponse>(&response.body)
                    .map(|parsed| parsed.applied)
                    .unwrap_or(0);
            PushOutcome::Applied { applied }
        }
        Some(409) => PushOutcome::Conflict,
        Some(400) => {
            // 节点在未知身份的 400 里回前若干条。**不要把它当成普通 400**：
            // 它说明控制面与数据面读的不是同一份配置真相（C15）。
            let unknown = serde_json::from_slice::<serde_json::Value>(&response.body)
                .ok()
                .and_then(|v| v["error"]["unknown"].as_array().cloned())
                .map(|items| {
                    items.iter().filter_map(|i| i.as_str().map(str::to_string)).collect::<Vec<_>>()
                })
                .unwrap_or_default();
            if unknown.is_empty() {
                PushOutcome::BadRequest
            } else {
                PushOutcome::UnknownIdentity { unknown }
            }
        }
        Some(status) => PushOutcome::Unknown { detail: format!("上游状态 {status}") },
        None => {
            PushOutcome::Unknown { detail: format!("agent 级状态 {}", response.agent_status) }
        }
    }
}

/// 记录结果。**epoch 与 sequence 占用不得回滚复用。**
pub async fn record(store: &Store, prepared: &Prepared, outcome: &PushOutcome) -> Result<()> {
    let now = bucket::to_rfc3339(OffsetDateTime::now_utc());
    let conflict_cause = match outcome {
        PushOutcome::Conflict => Some("undetermined"),
        _ => None,
    };
    let mut txn = store.begin_immediate().await?;
    sqlx::query(
        "UPDATE quota_requests SET result = ?, sent_at = ?, settled_at = ?, \
         response_status = ?, conflict_cause = ? WHERE request_id = ?",
    )
    .bind(outcome.result())
    .bind(&now)
    .bind(&now)
    .bind(match outcome {
        PushOutcome::Applied { .. } => Some(200i64),
        PushOutcome::Conflict => Some(409),
        PushOutcome::UnknownIdentity { .. } | PushOutcome::BadRequest => Some(400),
        PushOutcome::Retryable { status } => Some(*status as i64),
        PushOutcome::Unknown { .. } => None,
    })
    .bind(conflict_cause)
    .bind(prepared.request_id)
    .execute(txn.conn())
    .await?;
    txn.commit().await?;
    Ok(())
}

/// 记录 409 判别之后的成因。**取快照读回 `runtime_id` 之后才能写这一列。**
pub async fn record_conflict_cause(
    store: &Store,
    request_id: i64,
    resolution: &ConflictResolution,
) -> Result<()> {
    let cause = match resolution {
        ConflictResolution::RuntimeChanged { .. } => "runtime_mismatch",
        ConflictResolution::StaleEpoch { .. } => "stale_epoch",
        ConflictResolution::NodeMismatch { .. } | ConflictResolution::Undetermined => {
            "undetermined"
        }
    };
    let mut txn = store.begin_immediate().await?;
    sqlx::query("UPDATE quota_requests SET conflict_cause = ? WHERE request_id = ?")
        .bind(cause)
        .bind(request_id)
        .execute(txn.conn())
        .await?;
    txn.commit().await?;
    Ok(())
}

/// 按 §4.5 的三步判别 409。
///
/// `observed` 是**取快照读回来**的 `(node_id, runtime_id)`，`None` 表示取不到。
pub async fn resolve_conflict(
    store: &Store,
    prepared: &Prepared,
    observed: Option<(&str, &str)>,
) -> Result<ConflictResolution> {
    let Some((observed_node, observed_runtime)) = observed else {
        return Ok(ConflictResolution::Undetermined);
    };
    // 第 1 步：校验连接目标和 node_id。不一致先停止推送并告警。
    if observed_node != prepared.node_id {
        return Ok(ConflictResolution::NodeMismatch {
            sent: prepared.node_id.clone(),
            observed: observed_node.to_string(),
        });
    }
    // 第 2 步：runtime 已变。
    if observed_runtime != prepared.runtime_id {
        return Ok(ConflictResolution::RuntimeChanged {
            observed_runtime: observed_runtime.to_string(),
        });
    }
    // 第 3 步：runtime 相同 → epoch 未前进。
    Ok(ConflictResolution::StaleEpoch {
        next_epoch: next_epoch(store, &prepared.node_id, &prepared.runtime_id).await?,
    })
}

/// 恢复：把「已准备却无法确认是否发送」的请求按 `unknown` 对待。
///
/// 主控在发送后崩溃时，请求会停在 `prepared`。**它不能被当成「没发出去」**——
/// 响应丢失不能证明旧请求没执行。
pub async fn recover_prepared(store: &Store, node_id: &str) -> Result<usize> {
    let now = bucket::to_rfc3339(OffsetDateTime::now_utc());
    let mut txn = store.begin_immediate().await?;
    let affected = sqlx::query(
        "UPDATE quota_requests SET result = 'unknown', settled_at = ? \
         WHERE node_id = ? AND result = 'prepared'",
    )
    .bind(&now)
    .bind(node_id)
    .execute(txn.conn())
    .await?
    .rows_affected();
    txn.commit().await?;
    if affected > 0 {
        tracing::warn!(node_id, affected, "恢复时发现未确认的请求，按 unknown 对待");
    }
    Ok(affected as usize)
}

/// 该 sequence 是否还能支撑一份**正额度**表。
///
/// 仅「快照还没过 TTL」不足以授权反复刷新同一份余额（§4.5 下发纪律 3）。
pub async fn sequence_available(
    store: &Store,
    node_id: &str,
    runtime_id: &str,
    sequence: u64,
) -> Result<bool> {
    let row = sqlx::query(
        "SELECT count(*) FROM quota_requests \
         WHERE node_id = ? AND runtime_id = ? AND source_sequence = ? AND table_kind = 'positive'",
    )
    .bind(node_id)
    .bind(runtime_id)
    .bind(U64Text::new(sequence).encode())
    .fetch_one(store.readers())
    .await?;
    Ok(row.get::<i64, _>(0) == 0)
}

/// 发送前的新鲜度门禁：这份快照是不是**上次正额度请求之后**重新采集并结算的。
pub async fn snapshot_is_fresh(
    store: &Store,
    node_id: &str,
    runtime_id: &str,
    sequence: u64,
    accounting_at: OffsetDateTime,
    max_age_secs: i64,
) -> Result<bool> {
    if (OffsetDateTime::now_utc() - accounting_at).whole_seconds() > max_age_secs {
        return Ok(false);
    }
    // 必须严格晚于上一份正额度请求占用的 sequence。
    let row = sqlx::query(
        "SELECT MAX(source_sequence) FROM quota_requests \
         WHERE node_id = ? AND runtime_id = ? AND table_kind = 'positive'",
    )
    .bind(node_id)
    .bind(runtime_id)
    .fetch_one(store.readers())
    .await?;
    Ok(match row.get::<Option<String>, _>(0) {
        Some(text) => sequence > U64Text::decode(&text)?.get(),
        None => true,
    })
}

/// 下发路径用的路径常量，便于调用方在日志里对齐。
pub const PATH: &str = QUOTA_PATH;
