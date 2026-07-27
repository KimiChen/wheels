//! 版本化迁移。`schema_migrations` 记录已应用版本；每个迁移在单事务内应用，进程重启幂等。

use crate::error::{AppError, ErrorCode, Result};
use sqlx::{Row, SqlitePool};

pub struct Migration {
    pub version: i64,
    pub name: &'static str,
    pub sql: &'static str,
}

const MIGRATIONS: &[Migration] = &[
    Migration {
        version: 1,
        name: "init_control_plane_and_users",
        sql: include_str!("../../migrations/0001_init.sql"),
    },
    Migration {
        version: 2,
        name: "agent_pki_commands",
        sql: include_str!("../../migrations/0002_agent_pki_commands.sql"),
    },
    Migration {
        version: 3,
        name: "config_revisions_artifacts",
        sql: include_str!("../../migrations/0003_config_revisions_artifacts.sql"),
    },
    Migration {
        version: 4,
        name: "deployments",
        sql: include_str!("../../migrations/0004_deployments.sql"),
    },
    Migration {
        version: 5,
        name: "user_identities",
        sql: include_str!("../../migrations/0005_user_identities.sql"),
    },
    Migration {
        version: 6,
        name: "metering",
        sql: include_str!("../../migrations/0006_metering.sql"),
    },
    Migration {
        version: 7,
        name: "admin_auth",
        sql: include_str!("../../migrations/0007_admin_auth.sql"),
    },
    Migration {
        version: 8,
        name: "observability",
        sql: include_str!("../../migrations/0008_observability.sql"),
    },
    Migration {
        version: 9,
        name: "key_rotation",
        sql: include_str!("../../migrations/0009_key_rotation.sql"),
    },
];

/// 应用 Manager 控制面迁移集。
pub async fn run(pool: &SqlitePool) -> Result<()> {
    run_list(pool, MIGRATIONS).await
}

/// 通用版本化迁移：`schema_migrations` 记录已应用版本；每个迁移单事务、幂等。
/// Manager 库与 Agent 本地库各传自己的迁移集。
pub async fn run_list(pool: &SqlitePool, migrations: &[Migration]) -> Result<()> {
    sqlx::query(
        "CREATE TABLE IF NOT EXISTS schema_migrations(
            version    INTEGER PRIMARY KEY,
            name       TEXT    NOT NULL,
            applied_at INTEGER NOT NULL
        )",
    )
    .execute(pool)
    .await?;

    let current: i64 = sqlx::query("SELECT COALESCE(MAX(version),0) AS v FROM schema_migrations")
        .fetch_one(pool)
        .await?
        .get("v");

    for m in migrations {
        if m.version <= current {
            continue;
        }
        let mut tx = pool.begin().await?;
        sqlx::raw_sql(m.sql).execute(&mut *tx).await.map_err(|e| {
            AppError::with(
                ErrorCode::Migration,
                format!("迁移 {} ({}) 失败", m.version, m.name),
                e.into(),
            )
        })?;
        sqlx::query("INSERT INTO schema_migrations(version,name,applied_at) VALUES(?,?,?)")
            .bind(m.version)
            .bind(m.name)
            .bind(now_unix())
            .execute(&mut *tx)
            .await?;
        tx.commit().await?;
    }
    Ok(())
}

fn now_unix() -> i64 {
    time::OffsetDateTime::now_utc().unix_timestamp()
}
