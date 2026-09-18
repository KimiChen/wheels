//! runtime 批准登记（README §4.4、`docs/data-model.md` §4）。
//!
//! 首快照策略 per-node 显式配置、**配置缺省即启动失败**（C8）。
//! 更强的一层是**批准登记**：每个 runtime 显式登记并留存**策略、原因与操作者**；
//! 未登记的 runtime（含影子 runtime）不得进入批准文件，其上的身份不可分配（D11）。
//!
//! 为什么不「首次见到即自动批准」：一次意外重启、一次误指端点，都会产生一个新的
//! `runtime_id`。自动批准意味着这些**影子 runtime** 直接获得入账资格，
//! 而它们的首快照会按策略要么吞掉已有累计（`baseline`），
//! 要么把历史流量一次性记成本周期用量（`include`）。两种都是能跑起来的错。
//!
//! M1 还没有 API 与控制台（M4），所以入口是 CLI 子命令。M4 上来之后复用同一层。

use sqlx::Row;
use time::OffsetDateTime;

use crate::error::{Error, Result};
use crate::ledger::bucket;
use crate::ledger::settle::FirstSnapshot;
use crate::store::codec::U64Text;
use crate::store::Store;

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RuntimeWindow {
    pub node_id: String,
    pub runtime_id: String,
    pub started_at_unix_ms: u64,
    pub first_snapshot: FirstSnapshot,
    pub approval_status: String,
    pub approval_reason: Option<String>,
    pub approved_by: Option<String>,
    pub last_sequence: Option<u64>,
    pub window_state: String,
}

/// 只在配置里出现过的节点才能承载 runtime。
///
/// 这一步把 `nodes.toml` 同步进库。它**不覆盖**已有节点的首快照策略——
/// 策略一旦被某个 runtime 用过，改它就等于改历史的解释方式。
pub async fn sync_nodes(store: &Store, config: &crate::config::Config) -> Result<usize> {
    let now = bucket::to_rfc3339(OffsetDateTime::now_utc());
    let mut txn = store.begin_immediate().await?;
    let mut count = 0;
    for node in &config.nodes {
        sqlx::query(
            "INSERT INTO nodes(node_id, provider, address, first_snapshot, agent_spki_sha256, \
             quota_enabled, status, created_at, updated_at) \
             VALUES (?, 'plus_v3', ?, ?, ?, ?, 'active', ?, ?) \
             ON CONFLICT(node_id) DO UPDATE SET \
               address = excluded.address, \
               agent_spki_sha256 = excluded.agent_spki_sha256, \
               quota_enabled = excluded.quota_enabled, \
               updated_at = excluded.updated_at",
        )
        .bind(&node.node_id)
        .bind(&node.address)
        .bind(match node.first_snapshot {
            crate::config::FirstSnapshotPolicy::Baseline => "baseline",
            crate::config::FirstSnapshotPolicy::Include => "include",
        })
        .bind(&node.agent_spki_sha256)
        .bind(node.quota_enabled())
        .bind(&now)
        .bind(&now)
        .execute(txn.conn())
        .await?;
        count += 1;
    }
    txn.commit().await?;
    Ok(count)
}

/// 登记并批准一个 runtime。
///
/// `started_at_unix_ms` **首次确认后固定不变**（`docs/data-model.md` §10 不变量）：
/// 同一个 `runtime_id` 报出两个不同的启动时刻，说明这不是同一个进程窗口。
#[allow(clippy::too_many_arguments)]
pub async fn approve(
    store: &Store,
    node_id: &str,
    runtime_id: &str,
    started_at_unix_ms: u64,
    policy: FirstSnapshot,
    reason: &str,
    actor: &str,
) -> Result<RuntimeWindow> {
    if reason.trim().is_empty() || actor.trim().is_empty() {
        // 留存「策略、原因与操作者」是这条纪律的全部内容。
        // 允许空原因就等于只留了策略。
        return Err(Error::Ledger("批准必须留下原因与操作者".into()));
    }
    let now = bucket::to_rfc3339(OffsetDateTime::now_utc());
    let mut txn = store.begin_immediate().await?;

    let existing = sqlx::query(
        "SELECT started_at_unix_ms, approval_status FROM node_runtimes \
         WHERE node_id = ? AND runtime_id = ?",
    )
    .bind(node_id)
    .bind(runtime_id)
    .fetch_optional(txn.conn())
    .await?;

    if let Some(row) = &existing {
        let stored = U64Text::decode(&row.get::<String, _>(0))?.get();
        if stored != started_at_unix_ms {
            txn.rollback().await?;
            return Err(Error::Ledger(format!(
                "runtime {runtime_id} 的 started_at_unix_ms 已确认为 {stored}，本次是 {started_at_unix_ms}：\
                 同一 runtime_id 报出两个启动时刻，说明它不是同一个进程窗口"
            )));
        }
        sqlx::query(
            "UPDATE node_runtimes SET approval_status = 'approved', first_snapshot = ?, \
             approval_reason = ?, approved_by = ?, approved_at = ?, last_seen_at = ? \
             WHERE node_id = ? AND runtime_id = ?",
        )
        .bind(policy.as_str())
        .bind(reason)
        .bind(actor)
        .bind(&now)
        .bind(&now)
        .bind(node_id)
        .bind(runtime_id)
        .execute(txn.conn())
        .await?;
    } else {
        sqlx::query(
            "INSERT INTO node_runtimes(node_id, runtime_id, started_at_unix_ms, first_snapshot, \
             approval_status, approval_reason, approved_by, approved_at, first_seen_at, last_seen_at) \
             VALUES (?, ?, ?, ?, 'approved', ?, ?, ?, ?, ?)",
        )
        .bind(node_id)
        .bind(runtime_id)
        .bind(U64Text::new(started_at_unix_ms).encode())
        .bind(policy.as_str())
        .bind(reason)
        .bind(actor)
        .bind(&now)
        .bind(&now)
        .bind(&now)
        .execute(txn.conn())
        .await?;
    }

    // 已经收到过的、属于这个 runtime 的批次回填 runtime_pk。
    sqlx::query(
        "UPDATE snapshot_batches SET runtime_pk = \
           (SELECT runtime_pk FROM node_runtimes WHERE node_id = ? AND runtime_id = ?) \
         WHERE node_id = ? AND runtime_id = ? AND runtime_pk IS NULL",
    )
    .bind(node_id)
    .bind(runtime_id)
    .bind(node_id)
    .bind(runtime_id)
    .execute(txn.conn())
    .await?;

    txn.commit().await?;
    load(store, node_id, runtime_id)
        .await?
        .ok_or_else(|| Error::Ledger("批准之后应当读得回来".into()))
}

pub async fn load(store: &Store, node_id: &str, runtime_id: &str) -> Result<Option<RuntimeWindow>> {
    let row = sqlx::query(
        "SELECT node_id, runtime_id, started_at_unix_ms, first_snapshot, approval_status, \
                approval_reason, approved_by, last_sequence, window_state \
         FROM node_runtimes WHERE node_id = ? AND runtime_id = ?",
    )
    .bind(node_id)
    .bind(runtime_id)
    .fetch_optional(store.readers())
    .await?;
    row.map(window_from_row).transpose()
}

/// 列出已登记的 runtime 窗口。
pub async fn list(store: &Store) -> Result<Vec<RuntimeWindow>> {
    let rows = sqlx::query(
        "SELECT node_id, runtime_id, started_at_unix_ms, first_snapshot, approval_status, \
                approval_reason, approved_by, last_sequence, window_state \
         FROM node_runtimes ORDER BY node_id, started_at_unix_ms",
    )
    .fetch_all(store.readers())
    .await?;
    rows.into_iter().map(window_from_row).collect()
}

/// 列出**收到过快照但还没登记**的 runtime。
///
/// 这是运维批准前要看的那张单子：它就是「影子 runtime」的可见形态。
pub async fn awaiting_approval(store: &Store) -> Result<Vec<(String, String, u64)>> {
    let rows = sqlx::query(
        "SELECT b.node_id, b.runtime_id, count(*) FROM snapshot_batches b \
         LEFT JOIN node_runtimes r ON r.node_id = b.node_id AND r.runtime_id = b.runtime_id \
         WHERE r.runtime_pk IS NULL GROUP BY b.node_id, b.runtime_id ORDER BY b.node_id",
    )
    .fetch_all(store.readers())
    .await?;
    Ok(rows.into_iter().map(|row| (row.get(0), row.get(1), row.get::<i64, _>(2) as u64)).collect())
}

fn window_from_row(row: sqlx::sqlite::SqliteRow) -> Result<RuntimeWindow> {
    let last_sequence = row
        .get::<Option<String>, _>(7)
        .map(|text| U64Text::decode(&text).map(|value| value.get()))
        .transpose()?;
    Ok(RuntimeWindow {
        node_id: row.get(0),
        runtime_id: row.get(1),
        started_at_unix_ms: U64Text::decode(&row.get::<String, _>(2))?.get(),
        first_snapshot: FirstSnapshot::parse(&row.get::<String, _>(3))
            .ok_or_else(|| Error::Ledger("首快照策略不合法".into()))?,
        approval_status: row.get(4),
        approval_reason: row.get(5),
        approved_by: row.get(6),
        last_sequence,
        window_state: row.get(8),
    })
}
