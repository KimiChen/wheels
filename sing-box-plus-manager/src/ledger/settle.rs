//! 结算事务：11 步（`docs/data-model.md` §4）。
//!
//! 节点 README §5.1 把「推进基线」与「产出计费记录」的先后定成一次**显式取舍**：
//! 先产出记录后推进基线 = 崩溃即重复计费；反过来 = 崩溃即漏记一批。
//! 参考 collector 选了后者（宁可漏记不可重复），并写明「要两者都不发生
//! 就需要把两步做成一次原子提交」。
//!
//! **主控有事务型存储，所以把两步做成一次提交**（D4）——这是本项目相对参考实现的实质增量。
//!
//! 分成**两个**短事务而不是一个：
//!
//! - [`receive`]：原始字节先原子落盘，批次记 `pending`。
//!   它是结算输入而不是审计附件——异步结算要能从库里恢复完整快照，
//!   C5 的失败关闭要人工介入而没有原始字节无法复盘（§5）。
//! - [`settle_next`]：11 步，账本 / 累计 / 游标 / 批次状态一起提交。
//!
//! 两段之间崩溃只会留下一个 `pending` 批次，**自然保持可重试**——
//! 这正是「不持久化中间 `processing` 状态」想要的效果。

use std::collections::{BTreeMap, BTreeSet};

use sha2::{Digest, Sha256};
use sqlx::{Row, SqliteConnection};
use time::OffsetDateTime;

use crate::collect::snapshot;
use crate::error::{Error, Result};
use crate::ledger::bucket::{self, TimeQuality};
use crate::ledger::canonical::{self, LineageDelta};
use crate::store::codec::{U128Text, U64Text};
use crate::store::Store;

/// 快照里的**完整 lineage 键**：inbound tag + generation + 身份名 + generation。
///
/// `generation` 属于协议键，**不能因为当前实现里它恒为 1 就省略**
/// （`docs/data-model.md` §2）。
type LineageKey = (String, u64, String, u64);

/// 首快照策略（C8）。每个 runtime 创建时**已持久化**，缺省拒绝结算。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum FirstSnapshot {
    /// 首次 delta 记 0：把节点已有的累计当作起点。
    Baseline,
    /// 首次 delta 等于当前累计值。
    /// 选它必须能证明不存在其他数据源的重复入账，**不能因为计数从零开始就隐式批准**。
    Include,
}

impl FirstSnapshot {
    pub fn as_str(self) -> &'static str {
        match self {
            FirstSnapshot::Baseline => "baseline",
            FirstSnapshot::Include => "include",
        }
    }

    pub fn parse(text: &str) -> Option<Self> {
        match text {
            "baseline" => Some(FirstSnapshot::Baseline),
            "include" => Some(FirstSnapshot::Include),
            _ => None,
        }
    }
}

/// 整份拒绝的成因。
///
/// 前四个变体对应 README §1 验收定义里的**四项判据**，长跑时它们必须全为 0。
/// 做成枚举而不是字符串，是为了让「判据为 0」这件事可以被机械统计，
/// 而不是靠 grep 日志。
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum RejectReason {
    /// 四向任一倒退。**P0 失败关闭**——不能局部入账，也不能擅自解释成重启，
    /// 合法重启必须更换 `runtime_id`（C5）。
    CounterRegression {
        identity: String,
        field: &'static str,
        previous: u64,
        current: u64,
    },
    /// runtime 没登记，或登记了但没批准。未登记的 runtime（含影子 runtime）
    /// 不得进入批准文件，其上的身份不可分配。
    UnknownRuntime,
    RuntimeNotApproved,
    /// `sequence` ≤ 已接受值 → 整份丢弃且**不推进任何状态**（C5）。
    StaleSequence {
        sequence: u64,
        last_accepted: u64,
    },
    /// 不可入账的 health 位为真（C4 前三位）。
    Unhealthy {
        bits: Vec<&'static str>,
    },
    /// 已观察过的 lineage 在后续快照中无故消失（C25）。
    /// **不得解释成零增量或删除**——`active` 可以在快照间切换，但切换不创建新基线。
    LineageDisappeared {
        identities: Vec<String>,
    },
    /// 整份解析失败。
    Malformed(String),
}

impl RejectReason {
    /// 这条成因是**终态**，还是等主控侧补齐前置条件之后可以重试？
    ///
    /// 分界线是「不可用的是谁」：计数器倒退、health 不可入账、lineage 消失——
    /// **快照本身**不可用，重试多少次都一样，烧掉它是对的。
    /// 而 runtime 没登记/没批准是**主控侧**的前置条件没满足，快照是好的。
    ///
    /// 这个区分不是洁癖：`runtime_id` 与 `started_at_unix_ms` **就在快照里**，
    /// 所以运维根本不可能在看到第一份快照之前批准一个 runtime。
    /// 「先收到、后批准」是正常流程，把它判成终态等于让每个新 runtime 的
    /// 首快照必然丢失一份。
    pub fn is_retryable(&self) -> bool {
        matches!(self, RejectReason::UnknownRuntime | RejectReason::RuntimeNotApproved)
    }

    pub fn as_code(&self) -> &'static str {
        match self {
            RejectReason::CounterRegression { .. } => "counter_regression",
            RejectReason::UnknownRuntime => "unknown_runtime",
            RejectReason::RuntimeNotApproved => "runtime_not_approved",
            RejectReason::StaleSequence { .. } => "stale_sequence",
            RejectReason::Unhealthy { .. } => "unhealthy",
            RejectReason::LineageDisappeared { .. } => "lineage_disappeared",
            RejectReason::Malformed(_) => "malformed",
        }
    }

    /// 是否属于 README §1 验收定义里的四项判据。
    pub fn is_acceptance_criterion(&self) -> bool {
        matches!(
            self,
            RejectReason::CounterRegression { .. }
                | RejectReason::UnknownRuntime
                | RejectReason::StaleSequence { .. }
                | RejectReason::Unhealthy { .. }
        )
    }
}

impl std::fmt::Display for RejectReason {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            RejectReason::CounterRegression { identity, field, previous, current } => write!(
                f,
                "计数器倒退（{identity} 的 {field}：上次已接受 {previous}，本次 {current}）——\
                 P0 失败关闭，合法重启必须更换 runtime_id"
            ),
            RejectReason::UnknownRuntime => f.write_str("runtime 未登记"),
            RejectReason::RuntimeNotApproved => f.write_str("runtime 已登记但未批准"),
            RejectReason::StaleSequence { sequence, last_accepted } => {
                write!(
                    f,
                    "sequence {sequence} ≤ 已接受的 {last_accepted}，整份丢弃且不推进任何状态"
                )
            }
            RejectReason::Unhealthy { bits } => {
                write!(f, "不可入账的 health 位为真：{bits:?}")
            }
            RejectReason::LineageDisappeared { identities } => {
                write!(f, "已观察过的 lineage 消失：{identities:?}——不得解释成零增量或删除（C25）")
            }
            RejectReason::Malformed(detail) => write!(f, "整份解析失败：{detail}"),
        }
    }
}

/// 接收回执。停机门禁要绑定 `runtime_id / sequence / payload_sha256`（operations.md §3）。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Receipt {
    pub batch_pk: i64,
    pub node_id: String,
    pub runtime_id: String,
    pub sequence: u64,
    pub payload_sha256: String,
    pub status: String,
    /// 本次是不是新插入的。同号同摘要重投时为 `false`（幂等返回）。
    pub freshly_inserted: bool,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum SettleOutcome {
    /// 已入账。
    Applied {
        batch_pk: i64,
        sequence: u64,
        /// 实际写入的账本行数（只有非零增量才写行）。
        ledger_rows: usize,
        /// 跳号缺口。`Some(n)` 表示与上次已接受值之间跳过了 n 个序号。
        /// **跳号是合法的**（真实采集丢失，C26）：告警并记录缺口，
        /// 但照常按累计差值结算，**绝不对缺失时段插值**。
        sequence_gap: Option<u64>,
    },
    /// 整份拒绝。留了回执，**没有推进 sequence 与游标**。
    Rejected { batch_pk: i64, reason: RejectReason },
    /// 更旧且此前未处理的批次：乱序到达，已被更新的批次取代。
    Superseded { batch_pk: i64, sequence: u64 },
    /// 前置条件没满足，**批次保持 `pending` 等待重试**。
    ///
    /// 与 [`SettleOutcome::Rejected`] 的区别见 [`RejectReason::is_retryable`]。
    Deferred { reason: RejectReason },
    /// 没有待结算的批次。
    Nothing,
}

// ============================ 接收 ============================

/// 接收一份快照：原始字节先原子落盘。
///
/// 摘要对**解压后的原始响应字节**计算，**不要重新序列化对象后再算**——
/// 字段顺序与数字表示都可能变化（§5）。
#[allow(clippy::too_many_arguments)]
pub async fn receive(
    store: &Store,
    node_id: &str,
    raw: &[u8],
    collected_at: OffsetDateTime,
    received_at: OffsetDateTime,
    max_skew_secs: i64,
) -> Result<Option<Receipt>> {
    let payload_sha256 = hex::encode(Sha256::digest(raw));

    // 完整校验先走一遍：它决定这条回执是 pending 还是 rejected。
    let parsed = snapshot::parse(raw);
    let (runtime_id, sequence, reject) = match &parsed {
        Ok(snapshot) => (snapshot.runtime_id.clone(), snapshot.sequence, None),
        Err(rejected) => {
            // 只要 envelope 还能识别，就留一条可审计的回执。
            let Some(envelope) = snapshot::parse_envelope(raw) else {
                // 完全无法识别的畸形输入只进限量安全日志，不落库。
                tracing::warn!(node_id, "收到无法识别 envelope 的响应，只记安全日志");
                return Ok(None);
            };
            if envelope.node_id != node_id {
                tracing::warn!(node_id, "envelope 里的 node_id 与连接目标不符，只记安全日志");
                return Ok(None);
            }
            (
                envelope.runtime_id,
                envelope.sequence,
                Some(RejectReason::Malformed(rejected.to_string())),
            )
        }
    };

    let mut txn = store.begin_immediate().await?;

    // 同一个 (节点, runtime, sequence) 只允许一条回执。
    let existing = sqlx::query(
        "SELECT batch_pk, payload_sha256, status FROM snapshot_batches \
         WHERE node_id = ? AND runtime_id = ? AND sequence = ?",
    )
    .bind(node_id)
    .bind(&runtime_id)
    .bind(U64Text::new(sequence).encode())
    .fetch_optional(txn.conn())
    .await?;

    if let Some(row) = existing {
        let batch_pk: i64 = row.get(0);
        let stored_sha: String = row.get(1);
        let status: String = row.get(2);
        txn.rollback().await?;
        if stored_sha == payload_sha256 {
            // 幂等返回：同号同摘要，什么都不做。
            return Ok(Some(Receipt {
                batch_pk,
                node_id: node_id.to_string(),
                runtime_id,
                sequence,
                payload_sha256,
                status,
                freshly_inserted: false,
            }));
        }
        // 同号不同摘要：节点不该产生这种输入。**不改写既有那一行**——
        // 改写等于让历史随后来的输入变化，而那正是账本要防的事。
        tracing::error!(
            node_id,
            sequence,
            "同一 sequence 收到摘要不同的两份快照：疑似重放或存在第二个写入者（升 P1）"
        );
        return Err(Error::Ledger(format!(
            "sequence {sequence} 已有一条摘要不同的回执（batch_pk={batch_pk}）"
        )));
    }

    // 时间与归桶。上一次成功采集取**同一 runtime** 的最后一条已入账批次。
    let previous_collected = sqlx::query(
        "SELECT collected_at FROM snapshot_batches \
         WHERE node_id = ? AND runtime_id = ? AND status = 'applied' \
         ORDER BY sequence DESC LIMIT 1",
    )
    .bind(node_id)
    .bind(&runtime_id)
    .fetch_optional(txn.conn())
    .await?
    .and_then(|row| bucket::parse_rfc3339(&row.get::<String, _>(0)));

    let decision = bucket::decide(collected_at, received_at, previous_collected, max_skew_secs);
    if decision.quality == TimeQuality::Fallback {
        // 归桶回落要告警（C27）。节点时钟偏移是一等健康项。
        tracing::warn!(
            node_id,
            skew_secs = decision.clock_skew_secs,
            reason = ?decision.fallback_reason,
            "归桶回落到 received_at"
        );
    }

    // 排空证据取自**同一份快照**的 gauge（operations.md §3）：
    // 全零为 true，有非零为 false，gauge 缺失或读取失败为 null。
    // 这两个 gauge 只有这一个用途（C6）。
    let drained: Option<bool> = parsed.as_ref().ok().map(|snapshot| {
        snapshot
            .inbounds
            .iter()
            .all(|inbound| inbound.tcp_sessions == 0 && inbound.udp_sessions == 0)
    });

    let status = if reject.is_some() { "rejected" } else { "pending" };
    let runtime_pk = lookup_runtime_pk(txn.conn(), node_id, &runtime_id).await?;
    let now = bucket::to_rfc3339(OffsetDateTime::now_utc());

    let result = sqlx::query(
        "INSERT INTO snapshot_batches(\
            node_id, runtime_id, runtime_pk, sequence, status, reject_reason, payload_sha256, \
            collected_at, received_at, accounting_at, time_quality, observation_gap_ms, \
            drained, created_at) \
         VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING batch_pk",
    )
    .bind(node_id)
    .bind(&runtime_id)
    .bind(runtime_pk)
    .bind(U64Text::new(sequence).encode())
    .bind(status)
    .bind(reject.as_ref().map(|r| r.to_string()))
    .bind(&payload_sha256)
    .bind(bucket::to_rfc3339(decision.collected_at))
    .bind(bucket::to_rfc3339(decision.received_at))
    .bind(bucket::to_rfc3339(decision.accounting_at))
    .bind(decision.quality.as_str())
    .bind(decision.observation_gap_ms.map(|ms| U64Text::new(ms).encode()))
    .bind(drained)
    .bind(&now)
    .fetch_one(txn.conn())
    .await?;
    let batch_pk: i64 = result.get(0);

    // 原文与批次在**同一事务**保存。
    sqlx::query(
        "INSERT INTO snapshot_payloads(batch_pk, codec, raw_length, payload, created_at) \
         VALUES (?, 'identity', ?, ?, ?)",
    )
    .bind(batch_pk)
    .bind(raw.len() as i64)
    .bind(raw)
    .bind(&now)
    .execute(txn.conn())
    .await?;

    txn.commit().await?;

    Ok(Some(Receipt {
        batch_pk,
        node_id: node_id.to_string(),
        runtime_id,
        sequence,
        payload_sha256,
        status: status.to_string(),
        freshly_inserted: true,
    }))
}

// ============================ 结算：11 步 ============================

/// 结算该 `(node_id, runtime_id)` 下**最小的未处理 sequence**。
///
/// 同一 `(node_id, runtime_id)` 的批次必须**串行结算**；串行性由 SQLite 写事务保证。
pub async fn settle_next(store: &Store, node_id: &str, runtime_id: &str) -> Result<SettleOutcome> {
    // 步骤 1：经单写者队列开启 BEGIN IMMEDIATE。
    let mut txn = store.begin_immediate().await?;
    let outcome = settle_in_txn(txn.conn(), node_id, runtime_id).await;
    match outcome {
        // 步骤 11：全部成功后一次提交。
        Ok(outcome) => {
            txn.commit().await?;
            // COMMIT 已返回，此时一切都必须是持久的。
            crate::crash::checkpoint(crate::crash::points::AFTER_COMMIT);
            Ok(outcome)
        }
        // 任何一步失败整笔回滚。批次保持 pending，自然可重试。
        Err(error) => {
            txn.rollback().await?;
            Err(error)
        }
    }
}

async fn settle_in_txn(
    conn: &mut SqliteConnection,
    node_id: &str,
    runtime_id: &str,
) -> Result<SettleOutcome> {
    // 步骤 1（续）：读取 node_runtimes。
    let Some(runtime) = load_runtime(conn, node_id, runtime_id).await? else {
        // runtime 没登记。**批次保持 pending**：运维批准之后还要结算它。
        return Ok(defer(node_id, runtime_id, RejectReason::UnknownRuntime));
    };
    if runtime.approval_status != "approved" {
        return Ok(defer(node_id, runtime_id, RejectReason::RuntimeNotApproved));
    }

    let last_accepted = runtime.last_sequence.unwrap_or(0);

    // 步骤 2：**不按外部传入的批次 ID 结算**。先把「更旧且此前未处理」的标 superseded，
    // 再选最小的未处理 sequence。这条同时回答了「乱序到达怎么办」和
    // 「首快照策略会不会误落在一个已排队的较新快照上」。
    let superseded = sqlx::query(
        "UPDATE snapshot_batches SET status = 'superseded' \
         WHERE node_id = ? AND runtime_id = ? AND status = 'pending' AND sequence <= ? \
         RETURNING batch_pk, sequence",
    )
    .bind(node_id)
    .bind(runtime_id)
    .bind(U64Text::new(last_accepted).encode())
    .fetch_all(&mut *conn)
    .await?;
    if let Some(row) = superseded.first() {
        let sequence = U64Text::decode(&row.get::<String, _>(1))?.get();
        tracing::warn!(node_id, sequence, last_accepted, "批次乱序到达且已被取代，标 superseded");
        return Ok(SettleOutcome::Superseded { batch_pk: row.get(0), sequence });
    }

    let Some(batch) = sqlx::query(
        "SELECT batch_pk, sequence, payload_sha256, accounting_at FROM snapshot_batches \
         WHERE node_id = ? AND runtime_id = ? AND status = 'pending' \
         ORDER BY sequence ASC LIMIT 1",
    )
    .bind(node_id)
    .bind(runtime_id)
    .fetch_optional(&mut *conn)
    .await?
    else {
        return Ok(SettleOutcome::Nothing);
    };
    let batch_pk: i64 = batch.get(0);
    let sequence = U64Text::decode(&batch.get::<String, _>(1))?.get();
    let accounting_at = bucket::parse_rfc3339(&batch.get::<String, _>(3))
        .ok_or_else(|| Error::Ledger("批次的 accounting_at 不是合法 RFC 3339".into()))?;

    // 从库里恢复完整快照。原文是**结算输入**，不是可选的审计附件。
    let payload: Vec<u8> = sqlx::query("SELECT payload FROM snapshot_payloads WHERE batch_pk = ?")
        .bind(batch_pk)
        .fetch_one(&mut *conn)
        .await?
        .get(0);
    let snapshot = match snapshot::parse(&payload) {
        Ok(snapshot) => snapshot,
        Err(rejected) => {
            return finish_rejected(conn, batch_pk, RejectReason::Malformed(rejected.to_string()))
                .await
        }
    };

    // health：前三位任一为真即不可入账（C4）。audit_dropped 单独走告警，照常入账。
    if !snapshot.health.billable() {
        let mut bits = Vec::new();
        if snapshot.health.counter_overflow {
            bits.push("counter_overflow");
        }
        if snapshot.health.sequence_overflow {
            bits.push("sequence_overflow");
        }
        if snapshot.health.identity_limit_reached {
            bits.push("identity_limit_reached");
        }
        return finish_rejected(conn, batch_pk, RejectReason::Unhealthy { bits }).await;
    }
    if snapshot.health.audit_gap() {
        // 把审计丢弃算进入账判据，等于让磁盘写满连带停掉计费——
        // 这是节点侧刻意不做的，主控也不要做回来。
        tracing::warn!(node_id, sequence, "audit_dropped 为真：审计有损，照常入账（P2 告警）");
    }

    // 步骤 3：sequence 必须**严格大于** runtime 最后已接受值。
    if sequence <= last_accepted {
        return finish_rejected(
            conn,
            batch_pk,
            RejectReason::StaleSequence { sequence, last_accepted },
        )
        .await;
    }
    // 允许跳号（真实采集丢失，C26）：告警并记录缺口，但**绝不对缺失时段插值**。
    let sequence_gap = match last_accepted {
        0 => None,
        last if sequence > last + 1 => {
            let gap = sequence - last - 1;
            tracing::warn!(
                node_id,
                sequence,
                last_accepted,
                gap,
                "sequence 跳号：按累计差值结算，不插值"
            );
            Some(gap)
        }
        _ => None,
    };

    // 步骤 4：建立或读取快照中的 inbound generation 与身份 generation。
    // 同一快照内的重复完整键由解析器的严格升序挡住（`collect::snapshot`），这里不再重复。
    let now = bucket::to_rfc3339(OffsetDateTime::now_utc());
    let mut observed: BTreeMap<LineageKey, i64> = BTreeMap::new();
    for inbound in &snapshot.inbounds {
        let service_id = upsert_runtime_service(conn, runtime.runtime_pk, inbound, &now).await?;
        for user in &inbound.users {
            let identity_id =
                upsert_runtime_identity(conn, runtime.runtime_pk, service_id, user, &now).await?;
            // 顺带把槽位登记为 free。注册表因此永远来自节点**实际上报**的内容，
            // 不可能与节点现实漂移；而 free 不授予任何东西，assemble 会给它零额度。
            //
            // `DO NOTHING` 是承重的：结算只负责**发现**，绝不移动槽位状态。
            // 用 DO UPDATE 会让一次普通采集把 claimed 打回 free。
            // 用**结算正在处理的** node_id，不用快照体里的那个：两者不一致时
            // `receive` 只记安全日志不拒绝（settle.rs 里对 envelope 的处理），
            // 而槽位必须落在真实存在的节点上，否则外键会在这里炸。
            register_slot(conn, node_id, &inbound.tag, &user.name, &now).await?;
            observed.insert(
                (inbound.tag.clone(), inbound.generation, user.name.clone(), user.generation),
                identity_id,
            );
        }
    }

    // 步骤 4（续）：已观察过的 lineage 缺失 → 整份拒绝（C25）。
    // 这是 C9 的另一半：`active` 可在快照间切换，但切换不创建新基线、不重置累计值。
    let known = load_known_identities(conn, runtime.runtime_pk).await?;
    let observed_ids: BTreeSet<i64> = observed.values().copied().collect();
    let missing: Vec<String> = known
        .iter()
        .filter(|(id, _)| !observed_ids.contains(id))
        .map(|(_, name)| name.clone())
        .collect();
    if !missing.is_empty() {
        return finish_rejected(
            conn,
            batch_pk,
            RejectReason::LineageDisappeared { identities: missing },
        )
        .await;
    }

    // 步骤 5：按 runtime_identity_id 排序读取游标，保证处理与哈希顺序确定；不用模拟行锁。
    let cursors = load_cursors(conn, runtime.runtime_pk).await?;

    // 步骤 6 + 7 + 8（前半）：**先在内存里完成全快照校验，再插入任何一行非零 delta。**
    let mut deltas: Vec<(i64, LineageDelta)> = Vec::new();
    let mut absolutes: Vec<(i64, [u64; 4])> = Vec::new();
    let mut ordered: Vec<(&LineageKey, &i64)> = observed.iter().collect();
    ordered.sort_by_key(|(_, id)| **id);

    for (key, identity_id) in ordered {
        let user = snapshot
            .inbounds
            .iter()
            .find(|inbound| inbound.tag == key.0 && inbound.generation == key.1)
            .and_then(|inbound| {
                inbound.users.iter().find(|user| user.name == key.2 && user.generation == key.3)
            })
            .expect("observed 是从快照构建的，必然找得回去");
        let current = [
            user.tcp_uplink_bytes,
            user.tcp_downlink_bytes,
            user.udp_uplink_bytes,
            user.udp_downlink_bytes,
        ];

        let delta = match cursors.get(identity_id) {
            Some(previous) => {
                // 步骤 6：逐项检查 current >= previous。**任何一项下降都回滚整份快照。**
                const FIELDS: [&str; 4] = [
                    "tcp_uplink_bytes",
                    "tcp_downlink_bytes",
                    "udp_uplink_bytes",
                    "udp_downlink_bytes",
                ];
                let mut computed = [0u64; 4];
                for index in 0..4 {
                    if current[index] < previous[index] {
                        return finish_rejected(
                            conn,
                            batch_pk,
                            RejectReason::CounterRegression {
                                identity: format!("{}/{}", key.0, key.2),
                                field: FIELDS[index],
                                previous: previous[index],
                                current: current[index],
                            },
                        )
                        .await;
                    }
                    computed[index] = current[index] - previous[index];
                }
                computed
            }
            // 步骤 7：新游标按 runtime 创建时**已持久化**的策略处理（C8）。
            None => match runtime.first_snapshot {
                FirstSnapshot::Baseline => [0; 4],
                FirstSnapshot::Include => current,
            },
        };

        deltas.push((
            *identity_id,
            LineageDelta {
                inbound_tag: key.0.clone(),
                inbound_generation: key.1,
                user_name: key.2.clone(),
                user_generation: key.3,
                tcp_uplink_bytes: delta[0],
                tcp_downlink_bytes: delta[1],
                udp_uplink_bytes: delta[2],
                udp_downlink_bytes: delta[3],
            },
        ));
        absolutes.push((*identity_id, current));
    }

    // 步骤 8：幂等批次 ID 含四向增量，所以到这里才算得出来。
    let only_deltas: Vec<LineageDelta> = deltas.iter().map(|(_, d)| d.clone()).collect();
    let batch_id = canonical::batch_id(node_id, runtime_id, sequence, &only_deltas);
    let hour = bucket::to_rfc3339(bucket::hour_bucket(accounting_at));
    let cycle_key = cycle_key(accounting_at);

    // 归属一次查全，不在下面的循环里逐条查：301 个身份时那是每批 301 次往返，
    // 而且发生在写事务内。
    let owners = slot_owners(conn, runtime.runtime_pk).await?;

    // 步骤 8（后半）：只写**非零增量**的账本行。
    let mut ledger_rows = 0usize;
    for (identity_id, delta) in &deltas {
        if delta.is_zero() {
            continue;
        }
        let user_id = owners.get(identity_id).copied().flatten();
        let hash = canonical::source_event_hash(node_id, runtime_id, sequence, delta);
        sqlx::query(
            "INSERT INTO usage_ledger(\
                batch_pk, batch_id, runtime_identity_id, user_id, \
                tcp_uplink_bytes, tcp_downlink_bytes, udp_uplink_bytes, udp_downlink_bytes, \
                source_event_hash, hash_version, accounting_at, hour_bucket, created_at) \
             VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
        )
        .bind(batch_pk)
        .bind(&batch_id)
        .bind(identity_id)
        .bind(user_id)
        .bind(U64Text::new(delta.tcp_uplink_bytes).encode())
        .bind(U64Text::new(delta.tcp_downlink_bytes).encode())
        .bind(U64Text::new(delta.udp_uplink_bytes).encode())
        .bind(U64Text::new(delta.udp_downlink_bytes).encode())
        .bind(&hash)
        .bind(canonical::HASH_VERSION)
        .bind(bucket::to_rfc3339(accounting_at))
        .bind(&hour)
        .bind(&now)
        .execute(&mut *conn)
        .await?;
        ledger_rows += 1;
        crate::crash::checkpoint(crate::crash::points::AFTER_LEDGER_ROWS);

        // 步骤 9：同事务 checked 更新永久累计与周期累计。
        bump_lifetime(conn, *identity_id, user_id, delta, &now).await?;
        if let Some(user_id) = user_id {
            bump_cycle(conn, user_id, &cycle_key, delta, &now).await?;
        }
    }

    crate::crash::checkpoint(crate::crash::points::AFTER_TOTALS);

    // 步骤 9（续）：更新**所有**游标，**包括零增量与 `active = false` 的身份**。
    // 游标记的是「最后接受到的绝对值」，不是「最后有流量的时刻」。
    for (identity_id, current) in &absolutes {
        upsert_cursor(conn, *identity_id, current, batch_pk, &now).await?;
    }

    crate::crash::checkpoint(crate::crash::points::AFTER_CURSORS);

    // 步骤 10：更新 runtime 的最后 sequence、批次状态，并 upsert 对应小时脏桶。
    sqlx::query(
        "UPDATE node_runtimes SET last_sequence = ?, last_seen_at = ? WHERE runtime_pk = ?",
    )
    .bind(U64Text::new(sequence).encode())
    .bind(&now)
    .bind(runtime.runtime_pk)
    .execute(&mut *conn)
    .await?;
    sqlx::query(
        "UPDATE snapshot_batches SET status = 'applied', batch_id = ?, settled_at = ? \
         WHERE batch_pk = ?",
    )
    .bind(&batch_id)
    .bind(&now)
    .bind(batch_pk)
    .execute(&mut *conn)
    .await?;
    // 已存在时 version = version + 1, status = 'pending'：
    // 「算的时候又脏了」靠这个版本号检测（D15）。
    sqlx::query(
        "INSERT INTO rollup_jobs(hour_bucket, version, status, dirtied_at) VALUES (?, 1, 'pending', ?) \
         ON CONFLICT(hour_bucket) DO UPDATE SET version = version + 1, status = 'pending', dirtied_at = ?",
    )
    .bind(&hour)
    .bind(&now)
    .bind(&now)
    .execute(&mut *conn)
    .await?;

    // 全部写完，就差 COMMIT。在这里被杀，整笔必须回滚。
    crate::crash::checkpoint(crate::crash::points::BEFORE_COMMIT);

    Ok(SettleOutcome::Applied { batch_pk, sequence, ledger_rows, sequence_gap })
}

// ============================ 辅助 ============================

struct RuntimeRow {
    runtime_pk: i64,
    first_snapshot: FirstSnapshot,
    approval_status: String,
    last_sequence: Option<u64>,
}

async fn load_runtime(
    conn: &mut SqliteConnection,
    node_id: &str,
    runtime_id: &str,
) -> Result<Option<RuntimeRow>> {
    let row = sqlx::query(
        "SELECT runtime_pk, first_snapshot, approval_status, last_sequence \
         FROM node_runtimes WHERE node_id = ? AND runtime_id = ?",
    )
    .bind(node_id)
    .bind(runtime_id)
    .fetch_optional(&mut *conn)
    .await?;
    let Some(row) = row else { return Ok(None) };
    let last_sequence = row
        .get::<Option<String>, _>(3)
        .map(|text| U64Text::decode(&text).map(|value| value.get()))
        .transpose()?;
    Ok(Some(RuntimeRow {
        runtime_pk: row.get(0),
        first_snapshot: FirstSnapshot::parse(&row.get::<String, _>(1))
            .ok_or_else(|| Error::Ledger("runtime 的首快照策略不合法".into()))?,
        approval_status: row.get(2),
        last_sequence,
    }))
}

async fn lookup_runtime_pk(
    conn: &mut SqliteConnection,
    node_id: &str,
    runtime_id: &str,
) -> Result<Option<i64>> {
    Ok(sqlx::query("SELECT runtime_pk FROM node_runtimes WHERE node_id = ? AND runtime_id = ?")
        .bind(node_id)
        .bind(runtime_id)
        .fetch_optional(&mut *conn)
        .await?
        .map(|row| row.get(0)))
}

async fn upsert_runtime_service(
    conn: &mut SqliteConnection,
    runtime_pk: i64,
    inbound: &snapshot::Inbound,
    now: &str,
) -> Result<i64> {
    let generation = U64Text::new(inbound.generation).encode();
    if let Some(row) = sqlx::query(
        "SELECT runtime_service_id FROM runtime_services \
         WHERE runtime_pk = ? AND inbound_tag = ? AND generation = ?",
    )
    .bind(runtime_pk)
    .bind(&inbound.tag)
    .bind(&generation)
    .fetch_optional(&mut *conn)
    .await?
    {
        // active 可以变，但它不创建新记录。
        sqlx::query("UPDATE runtime_services SET active = ? WHERE runtime_service_id = ?")
            .bind(inbound.active)
            .bind(row.get::<i64, _>(0))
            .execute(&mut *conn)
            .await?;
        return Ok(row.get(0));
    }
    let row = sqlx::query(
        "INSERT INTO runtime_services(runtime_pk, inbound_tag, generation, inbound_type, active, first_seen_at) \
         VALUES (?, ?, ?, ?, ?, ?) RETURNING runtime_service_id",
    )
    .bind(runtime_pk)
    .bind(&inbound.tag)
    .bind(&generation)
    .bind(&inbound.inbound_type)
    .bind(inbound.active)
    .bind(now)
    .fetch_one(&mut *conn)
    .await?;
    Ok(row.get(0))
}

async fn upsert_runtime_identity(
    conn: &mut SqliteConnection,
    runtime_pk: i64,
    service_id: i64,
    user: &snapshot::SnapshotUser,
    now: &str,
) -> Result<i64> {
    let generation = U64Text::new(user.generation).encode();
    if let Some(row) = sqlx::query(
        "SELECT runtime_identity_id FROM runtime_identities \
         WHERE runtime_service_id = ? AND identity_name = ? AND generation = ?",
    )
    .bind(service_id)
    .bind(&user.name)
    .bind(&generation)
    .fetch_optional(&mut *conn)
    .await?
    {
        // 同名逻辑身份重激活**复用原记录、原累计值与原 generation**，只切换 active（C2 C9）。
        sqlx::query(
            "UPDATE runtime_identities SET active = ?, last_seen_at = ? WHERE runtime_identity_id = ?",
        )
        .bind(user.active)
        .bind(now)
        .bind(row.get::<i64, _>(0))
        .execute(&mut *conn)
        .await?;
        return Ok(row.get(0));
    }
    let row = sqlx::query(
        "INSERT INTO runtime_identities(\
            runtime_pk, runtime_service_id, identity_name, generation, active, first_seen_at, last_seen_at) \
         VALUES (?, ?, ?, ?, ?, ?, ?) RETURNING runtime_identity_id",
    )
    .bind(runtime_pk)
    .bind(service_id)
    .bind(&user.name)
    .bind(&generation)
    .bind(user.active)
    .bind(now)
    .bind(now)
    .fetch_one(&mut *conn)
    .await?;
    Ok(row.get(0))
}

async fn load_known_identities(
    conn: &mut SqliteConnection,
    runtime_pk: i64,
) -> Result<Vec<(i64, String)>> {
    Ok(sqlx::query(
        "SELECT runtime_identity_id, identity_name FROM runtime_identities WHERE runtime_pk = ?",
    )
    .bind(runtime_pk)
    .fetch_all(&mut *conn)
    .await?
    .into_iter()
    .map(|row| (row.get(0), row.get(1)))
    .collect())
}

async fn load_cursors(
    conn: &mut SqliteConnection,
    runtime_pk: i64,
) -> Result<BTreeMap<i64, [u64; 4]>> {
    let rows = sqlx::query(
        "SELECT c.runtime_identity_id, c.tcp_uplink_bytes, c.tcp_downlink_bytes, \
                c.udp_uplink_bytes, c.udp_downlink_bytes \
         FROM counter_cursors c JOIN runtime_identities i \
              ON i.runtime_identity_id = c.runtime_identity_id \
         WHERE i.runtime_pk = ? ORDER BY c.runtime_identity_id",
    )
    .bind(runtime_pk)
    .fetch_all(&mut *conn)
    .await?;
    let mut out = BTreeMap::new();
    for row in rows {
        let mut values = [0u64; 4];
        for (index, value) in values.iter_mut().enumerate() {
            *value = U64Text::decode(&row.get::<String, _>(index + 1))?.get();
        }
        out.insert(row.get::<i64, _>(0), values);
    }
    Ok(out)
}

/// 把某个 runtime 下的身份行映射到**槽位归属**。
///
/// 归属的落点是 `identity_routes`（跨重启存活），不是 `runtime_identities`
/// （每次重启重建）。退役槽位保留 user_id：退役不解绑，退役后到达的字节
/// 确实是那个人产生的，置空会把真实尾账变成无主字节。
async fn slot_owners(
    conn: &mut SqliteConnection,
    runtime_pk: i64,
) -> Result<BTreeMap<i64, Option<i64>>> {
    let rows = sqlx::query(
        "SELECT i.runtime_identity_id, rt.user_id \
         FROM runtime_identities i \
         JOIN runtime_services s ON s.runtime_service_id = i.runtime_service_id \
         JOIN node_runtimes n ON n.runtime_pk = i.runtime_pk \
         LEFT JOIN identity_routes rt \
                ON rt.node_id = n.node_id AND rt.inbound_tag = s.inbound_tag \
               AND rt.identity_name = i.identity_name \
         WHERE i.runtime_pk = ?",
    )
    .bind(runtime_pk)
    .fetch_all(&mut *conn)
    .await?;
    Ok(rows.into_iter().map(|row| (row.get(0), row.get(1))).collect())
}

/// 把观察到的身份登记为 free 槽位。已存在即原样保留。
///
/// `DO NOTHING` 不是省事：结算只负责发现，移动状态只发生在认领与退役事务里。
/// 换成 DO UPDATE，一次普通采集就会把 claimed 打回 free。
async fn register_slot(
    conn: &mut SqliteConnection,
    node_id: &str,
    inbound_tag: &str,
    identity_name: &str,
    now: &str,
) -> Result<()> {
    sqlx::query(
        "INSERT INTO identity_routes(node_id, inbound_tag, identity_name, state, created_at) \
         VALUES (?, ?, ?, 'free', ?) \
         ON CONFLICT(node_id, inbound_tag, identity_name) DO NOTHING",
    )
    .bind(node_id)
    .bind(inbound_tag)
    .bind(identity_name)
    .bind(now)
    .execute(&mut *conn)
    .await?;
    Ok(())
}

async fn bump_lifetime(
    conn: &mut SqliteConnection,
    identity_id: i64,
    user_id: Option<i64>,
    delta: &LineageDelta,
    now: &str,
) -> Result<()> {
    let current = sqlx::query(
        "SELECT tcp_uplink_bytes, tcp_downlink_bytes, udp_uplink_bytes, udp_downlink_bytes \
         FROM usage_lifetime_totals WHERE runtime_identity_id = ?",
    )
    .bind(identity_id)
    .fetch_optional(&mut *conn)
    .await?;

    let mut totals = [U128Text::new(0); 4];
    if let Some(row) = &current {
        for (index, total) in totals.iter_mut().enumerate() {
            *total = U128Text::decode(&row.get::<String, _>(index))?;
        }
    }
    let added = [
        delta.tcp_uplink_bytes,
        delta.tcp_downlink_bytes,
        delta.udp_uplink_bytes,
        delta.udp_downlink_bytes,
    ];
    // checked 累加：**溢出即回滚并告警**，不截断。
    for (index, total) in totals.iter_mut().enumerate() {
        *total = total.checked_add(U64Text::new(added[index]).widen())?;
    }

    if current.is_some() {
        sqlx::query(
            // `coalesce(user_id, ?)`：首次已知归属时补上，之后不再改动。
            // 缺了它，一条在认领**之前**就建出来的 lifetime 行会永久停在 NULL，
            // 而 `GET /users/{id}/usage` 读的正是这张表——它会一直少报，
            // 并且没有任何地方报错。
            "UPDATE usage_lifetime_totals SET tcp_uplink_bytes = ?, tcp_downlink_bytes = ?, \
             udp_uplink_bytes = ?, udp_downlink_bytes = ?, user_id = coalesce(user_id, ?), \
             updated_at = ? WHERE runtime_identity_id = ?",
        )
        .bind(totals[0].encode())
        .bind(totals[1].encode())
        .bind(totals[2].encode())
        .bind(totals[3].encode())
        .bind(user_id)
        .bind(now)
        .bind(identity_id)
        .execute(&mut *conn)
        .await?;
    } else {
        sqlx::query(
            "INSERT INTO usage_lifetime_totals(runtime_identity_id, user_id, \
             tcp_uplink_bytes, tcp_downlink_bytes, udp_uplink_bytes, udp_downlink_bytes, updated_at) \
             VALUES (?, ?, ?, ?, ?, ?, ?)",
        )
        .bind(identity_id)
        .bind(user_id)
        .bind(totals[0].encode())
        .bind(totals[1].encode())
        .bind(totals[2].encode())
        .bind(totals[3].encode())
        .bind(now)
        .execute(&mut *conn)
        .await?;
    }
    Ok(())
}

/// 周期累计口径固定为**四向之和**（C17）。按方向计费须在主控侧折算。
async fn bump_cycle(
    conn: &mut SqliteConnection,
    user_id: i64,
    cycle_key: &str,
    delta: &LineageDelta,
    now: &str,
) -> Result<()> {
    let added = U128Text::new(
        delta.tcp_uplink_bytes as u128
            + delta.tcp_downlink_bytes as u128
            + delta.udp_uplink_bytes as u128
            + delta.udp_downlink_bytes as u128,
    );
    let current = sqlx::query(
        "SELECT total_bytes FROM usage_cycle_totals \
         WHERE user_id = ? AND cycle_key = ? AND rule_version = ?",
    )
    .bind(user_id)
    .bind(cycle_key)
    .bind(CYCLE_RULE_VERSION)
    .fetch_optional(&mut *conn)
    .await?;

    let total = match &current {
        Some(row) => U128Text::decode(&row.get::<String, _>(0))?.checked_add(added)?,
        None => added,
    };
    if current.is_some() {
        sqlx::query(
            "UPDATE usage_cycle_totals SET total_bytes = ?, updated_at = ? \
             WHERE user_id = ? AND cycle_key = ? AND rule_version = ?",
        )
        .bind(total.encode())
        .bind(now)
        .bind(user_id)
        .bind(cycle_key)
        .bind(CYCLE_RULE_VERSION)
        .execute(&mut *conn)
        .await?;
    } else {
        sqlx::query(
            "INSERT INTO usage_cycle_totals(user_id, cycle_key, rule_version, total_bytes, updated_at) \
             VALUES (?, ?, ?, ?, ?)",
        )
        .bind(user_id)
        .bind(cycle_key)
        .bind(CYCLE_RULE_VERSION)
        .bind(total.encode())
        .bind(now)
        .execute(&mut *conn)
        .await?;
    }
    Ok(())
}

/// 计费规则版本。
///
/// - v1：**UTC 自然月**。已废弃，只用来解释 v1 的历史行。
/// - v2：**UTC+8 自然月**。月初 = UTC+8 当月 1 日 00:00，即前一月最后一日 16:00Z。
///
/// 改到 UTC+8 是定案第三条（周期在 UTC+8 月初重置）。当初选 UTC 的理由是
/// 「不需要引入 IANA 时区库」——这条理由对 +08:00 不成立：它是整小时偏移、
/// 无夏令时，一个编译期常量就够。
///
/// 每个读者都按 `rule_version = CYCLE_RULE_VERSION` 过滤，所以版本号一变，
/// 旧版本的行对配额立刻不可见。**不要跨版本求和**：UTC 的 "2026-09" 与
/// UTC+8 的 "2026-09" 是不同区间（`2026-08-31T16:00Z..24:00Z` 属于前者的八月、
/// 后者的九月），相加既重复计入又遗漏，得到的是一个编造的数。
/// 正确做法是重建库（决策 8 在首个真实用户跑满一个自然月前允许），
/// 于是不存在 v1 的行。
pub const CYCLE_RULE_VERSION: i64 = 2;

/// v2 的偏移量。**与版本号绑定的编译期常量，不是一条可改的设置**——
/// 做成设置意味着有人能在不推进版本号的情况下改它，
/// 那时同一个 `(cycle_key, rule_version)` 下会并存两种区间含义，
/// 而这种损坏没有任何办法被发现。
const CYCLE_OFFSET_V2: time::UtcOffset = time::macros::offset!(+8);

/// 按指定规则版本算周期键。
///
/// 带版本分支是为了让对账器能用**写入时**那套规则去核对历史行：
/// `usage_cycle_totals` 是否等于对应账本行之和，这个不变量只有在
/// 按行自己的 `rule_version` 分桶时才成立。
pub fn cycle_key_for(at: OffsetDateTime, rule_version: i64) -> crate::Result<String> {
    let offset = match rule_version {
        1 => time::UtcOffset::UTC,
        2 => CYCLE_OFFSET_V2,
        other => {
            return Err(crate::Error::Ledger(format!(
                "未知的计费规则版本 {other}：不猜测口径，宁可失败关闭"
            )))
        }
    };
    let local = at.to_offset(offset);
    Ok(format!("{:04}-{:02}", local.year(), u8::from(local.month())))
}

/// 当前口径的周期键。用编译期常量，不会失败。
pub fn cycle_key(at: OffsetDateTime) -> String {
    cycle_key_for(at, CYCLE_RULE_VERSION).expect("CYCLE_RULE_VERSION 必须是已知版本")
}

async fn upsert_cursor(
    conn: &mut SqliteConnection,
    identity_id: i64,
    absolute: &[u64; 4],
    batch_pk: i64,
    now: &str,
) -> Result<()> {
    // **禁止 `INSERT OR REPLACE`**：它是删除后插入，会破坏审计与引用语义。
    sqlx::query(
        "INSERT INTO counter_cursors(runtime_identity_id, tcp_uplink_bytes, tcp_downlink_bytes, \
         udp_uplink_bytes, udp_downlink_bytes, last_batch_pk, updated_at) \
         VALUES (?, ?, ?, ?, ?, ?, ?) \
         ON CONFLICT(runtime_identity_id) DO UPDATE SET \
           tcp_uplink_bytes = excluded.tcp_uplink_bytes, \
           tcp_downlink_bytes = excluded.tcp_downlink_bytes, \
           udp_uplink_bytes = excluded.udp_uplink_bytes, \
           udp_downlink_bytes = excluded.udp_downlink_bytes, \
           last_batch_pk = excluded.last_batch_pk, \
           updated_at = excluded.updated_at",
    )
    .bind(identity_id)
    .bind(U64Text::new(absolute[0]).encode())
    .bind(U64Text::new(absolute[1]).encode())
    .bind(U64Text::new(absolute[2]).encode())
    .bind(U64Text::new(absolute[3]).encode())
    .bind(batch_pk)
    .bind(now)
    .execute(&mut *conn)
    .await?;
    Ok(())
}

/// 留一条 `rejected` 回执，**绝不推进 sequence 与游标**。
async fn finish_rejected(
    conn: &mut SqliteConnection,
    batch_pk: i64,
    reason: RejectReason,
) -> Result<SettleOutcome> {
    sqlx::query(
        "UPDATE snapshot_batches SET status = 'rejected', reject_reason = ?, settled_at = ? \
         WHERE batch_pk = ?",
    )
    .bind(reason.to_string())
    .bind(bucket::to_rfc3339(OffsetDateTime::now_utc()))
    .bind(batch_pk)
    .execute(&mut *conn)
    .await?;
    tracing::warn!(batch_pk, code = reason.as_code(), "整份拒绝：{reason}");
    Ok(SettleOutcome::Rejected { batch_pk, reason })
}

/// 前置条件没满足：**什么都不改**，批次留在 `pending`。
fn defer(node_id: &str, runtime_id: &str, reason: RejectReason) -> SettleOutcome {
    tracing::warn!(
        node_id,
        runtime_id,
        code = reason.as_code(),
        "暂不结算，批次保持 pending：{reason}"
    );
    SettleOutcome::Deferred { reason }
}
