//! 建库：一次性把 `schema/` 下的 DDL 建出来。
//!
//! **本项目不做迁移功能**（用户决定，见 README §9 D22）：库是新搭建的，
//! schema 变更靠重建库而不是增量升级。因此这里没有版本表、没有升级路径、没有版本门禁。
//!
//! 代价写明：这意味着**任何 schema 变更都要求丢弃并重建数据库**。
//! 一旦有了需要保留的生产数据，要重估的是这条决策本身，而不是在这里补一个半吊子的升级器。
//!
//! 取而代之的是一道**一致性门禁**：库要么是空的（建全套），要么表集合与期望**完全一致**（放行）。
//! 介于两者之间——少了几张表、或多出不认识的表——一律失败关闭。
//! 「差不多能跑」在结算链路上是最贵的那种状态。

use sqlx::{Row, SqliteConnection};

use crate::error::Result;

/// 按加载顺序排列。顺序有意义：外键引用的表必须先建出来。
const PARTS: &[(&str, &str)] = &[
    ("01_config_dimensions", include_str!("../../schema/01_config_dimensions.sql")),
    ("02_runtime_lifecycle", include_str!("../../schema/02_runtime_lifecycle.sql")),
    ("03_settlement", include_str!("../../schema/03_settlement.sql")),
    ("04_quota", include_str!("../../schema/04_quota.sql")),
    ("05_rollup", include_str!("../../schema/05_rollup.sql")),
    ("06_session", include_str!("../../schema/06_session.sql")),
];

/// 期望的表集合。与 `schema/` 下的 DDL 手工对齐，
/// 由 `tests/m0/store_numeric.rs` 的用例断言两者一致——
/// 一份会漂移的清单比没有清单更糟。
pub const EXPECTED_TABLES: &[&str] = &[
    "counter_cursors",
    "identity_assignment_events",
    "identity_routes",
    "node_runtimes",
    "nodes",
    "quota_requests",
    "quota_setting_audits",
    "quota_settings",
    "quota_update_tasks",
    "rollup_jobs",
    "runtime_identities",
    "runtime_services",
    "server_secrets",
    "sessions",
    "snapshot_batches",
    "snapshot_payloads",
    "usage_cycle_totals",
    "usage_ledger",
    "usage_lifetime_totals",
    "users",
];

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum InitOutcome {
    /// 库是空的，本次建全套。
    Created,
    /// 库已存在且表集合与期望一致，什么都没做。
    AlreadyInitialized,
}

/// 读出当前库里的用户表名（排除 SQLite 自身的 `sqlite_*`）。
pub async fn existing_tables(conn: &mut SqliteConnection) -> Result<Vec<String>> {
    let rows = sqlx::query(
        "SELECT name FROM sqlite_master \
         WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name",
    )
    .fetch_all(&mut *conn)
    .await?;
    Ok(rows.into_iter().map(|row| row.get::<String, _>(0)).collect())
}

/// 建库或确认已建好。整套 DDL 在**一个事务**里提交：
/// 建到一半失败不能留下一个「部分建好」的库，那正是上面那道门禁最难判断的状态。
pub async fn initialize(conn: &mut SqliteConnection) -> Result<InitOutcome> {
    let present = existing_tables(conn).await?;
    if !present.is_empty() {
        let expected: Vec<String> = EXPECTED_TABLES.iter().map(|s| s.to_string()).collect();
        if present == expected {
            return Ok(InitOutcome::AlreadyInitialized);
        }
        let missing: Vec<&str> = EXPECTED_TABLES
            .iter()
            .copied()
            .filter(|name| !present.iter().any(|p| p == name))
            .collect();
        let unexpected: Vec<&str> = present
            .iter()
            .map(String::as_str)
            .filter(|name| !EXPECTED_TABLES.contains(name))
            .collect();
        return Err(crate::error::Error::Codec(format!(
            "数据库已存在但表集合与期望不一致：缺少 {missing:?}，多出 {unexpected:?}。\
             本项目不做迁移（D22），请重建数据库文件"
        )));
    }

    sqlx::query("BEGIN IMMEDIATE").execute(&mut *conn).await?;
    for (name, sql) in PARTS {
        if let Err(error) = sqlx::raw_sql(sql).execute(&mut *conn).await {
            let _ = sqlx::query("ROLLBACK").execute(&mut *conn).await;
            return Err(crate::error::Error::Codec(format!("建表 {name} 失败：{error}")));
        }
    }
    sqlx::query("COMMIT").execute(&mut *conn).await?;
    Ok(InitOutcome::Created)
}
