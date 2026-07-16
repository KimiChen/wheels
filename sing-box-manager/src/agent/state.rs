//! Agent 本地库：独立 SQLite，复用通用迁移框架但用自己的迁移集。Agent 不连 Manager 库。

use crate::error::Result;
use crate::store::migrations::{run_list, Migration};
use crate::store::{self};
use sqlx::{Row, SqlitePool};

const AGENT_MIGRATIONS: &[Migration] = &[
    Migration {
        version: 1,
        name: "agent_local_init",
        sql: include_str!("../../migrations/agent_0001.sql"),
    },
    Migration {
        version: 2,
        name: "deploy_runtime",
        sql: include_str!("../../migrations/agent_0002_deploy_runtime.sql"),
    },
    Migration {
        version: 3,
        name: "barrier",
        sql: include_str!("../../migrations/agent_0003_barrier.sql"),
    },
];

/// 打开 Agent 本地库并跑本地迁移。
pub async fn open(path: &str) -> Result<SqlitePool> {
    let pool = store::connect(path).await?;
    run_list(&pool, AGENT_MIGRATIONS).await?;
    Ok(pool)
}

/// 当前 active 本地 revision（无则 None）。
pub async fn active_revision(pool: &SqlitePool) -> Result<Option<i64>> {
    let row = sqlx::query(
        "SELECT revision FROM local_revisions WHERE active=1 ORDER BY revision DESC LIMIT 1",
    )
    .fetch_optional(pool)
    .await?;
    Ok(row.map(|r| r.get("revision")))
}

/// 一条本地 revision 记录（部署/回滚用）。
#[derive(Debug, Clone)]
pub struct LocalRevision {
    pub revision: i64,
    pub sha256: String,
    pub config_path: Option<String>,
    pub role: Option<String>,
    pub runtime_epoch: Option<i64>,
}

/// 当前 active revision 的 runtime_epoch（Phase 5 stats 盖的 boot id）。
pub async fn current_epoch(pool: &SqlitePool) -> Result<Option<i64>> {
    let row = sqlx::query(
        "SELECT runtime_epoch FROM local_revisions WHERE active=1 ORDER BY revision DESC LIMIT 1",
    )
    .fetch_optional(pool)
    .await?;
    Ok(row.and_then(|r| r.get::<Option<i64>, _>("runtime_epoch")))
}

/// 下一个本地 runtime epoch（MAX+1）。新进程新 epoch。
pub async fn next_epoch(pool: &SqlitePool) -> Result<i64> {
    let m: Option<i64> = sqlx::query_scalar("SELECT MAX(runtime_epoch) FROM local_revisions")
        .fetch_one(pool)
        .await?;
    Ok(m.unwrap_or(0) + 1)
}

/// 记录一次成功应用的 revision 并置为唯一 active（旧 active 归零，保留其磁盘配置供回滚）。
pub async fn record_applied(
    pool: &SqlitePool,
    revision: i64,
    sha256: &str,
    config_path: &str,
    role: &str,
    epoch: i64,
) -> Result<()> {
    let now = crate::store::now_unix();
    let mut tx = pool.begin().await?;
    sqlx::query("UPDATE local_revisions SET active=0")
        .execute(&mut *tx)
        .await?;
    sqlx::query(
        "INSERT INTO local_revisions(revision,sha256,applied_at,active,config_path,role,runtime_epoch,succeeded)
         VALUES(?,?,?,1,?,?,?,1)
         ON CONFLICT(revision) DO UPDATE SET sha256=excluded.sha256, applied_at=excluded.applied_at,
            active=1, config_path=excluded.config_path, role=excluded.role, runtime_epoch=excluded.runtime_epoch, succeeded=1",
    )
    .bind(revision)
    .bind(sha256)
    .bind(now)
    .bind(config_path)
    .bind(role)
    .bind(epoch)
    .execute(&mut *tx)
    .await?;
    tx.commit().await?;
    Ok(())
}

/// 上一个成功且非当前 active 的 revision（自动/显式回滚目标）。
pub async fn prev_succeeded(pool: &SqlitePool) -> Result<Option<LocalRevision>> {
    let row = sqlx::query(
        "SELECT revision,sha256,config_path,role,runtime_epoch FROM local_revisions
         WHERE succeeded=1 AND active=0 ORDER BY applied_at DESC LIMIT 1",
    )
    .fetch_optional(pool)
    .await?;
    Ok(row.map(|r| LocalRevision {
        revision: r.get("revision"),
        sha256: r.get("sha256"),
        config_path: r.get("config_path"),
        role: r.get("role"),
        runtime_epoch: r.get("runtime_epoch"),
    }))
}

/// 把某已存在 revision 置为唯一 active（回滚用）。
pub async fn set_active(pool: &SqlitePool, revision: i64) -> Result<()> {
    let mut tx = pool.begin().await?;
    sqlx::query("UPDATE local_revisions SET active=0")
        .execute(&mut *tx)
        .await?;
    sqlx::query("UPDATE local_revisions SET active=1 WHERE revision=?")
        .bind(revision)
        .execute(&mut *tx)
        .await?;
    tx.commit().await?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn open_creates_local_tables_idempotently() {
        let path = std::env::temp_dir().join(format!("sbm-agentdb-{}.db", uuid::Uuid::new_v4()));
        let ps = path.to_string_lossy().to_string();
        let pool = open(&ps).await.unwrap();
        // 三张本地表可查。
        for t in ["executed_commands", "meter_outbox", "local_revisions"] {
            sqlx::query(&format!("SELECT * FROM {t} LIMIT 0"))
                .execute(&pool)
                .await
                .unwrap();
        }
        assert_eq!(active_revision(&pool).await.unwrap(), None);
        pool.close().await;
        // 再次打开幂等。
        let pool2 = open(&ps).await.unwrap();
        pool2.close().await;
        let _ = std::fs::remove_file(&path);
    }
}
