//! hosts / host_capabilities 仓储。

use crate::domain::host::{Capability, Host};
use crate::error::Result;
use crate::store::now_unix;
use sqlx::{Row, SqlitePool};

fn row_to_host(row: &sqlx::sqlite::SqliteRow) -> Host {
    Host {
        id: row.get("id"),
        name: row.get("name"),
        note: row.get("note"),
        created_at: row.get("created_at"),
        updated_at: row.get("updated_at"),
    }
}

/// 创建 Host 并写入能力（单事务）。返回新 host id。
pub async fn create_host(
    pool: &SqlitePool,
    name: &str,
    note: Option<&str>,
    capabilities: &[Capability],
) -> Result<String> {
    let id = uuid::Uuid::new_v4().to_string();
    let now = now_unix();
    let mut tx = pool.begin().await?;
    sqlx::query("INSERT INTO hosts(id,name,note,created_at,updated_at) VALUES(?,?,?,?,?)")
        .bind(&id)
        .bind(name)
        .bind(note)
        .bind(now)
        .bind(now)
        .execute(&mut *tx)
        .await?;
    for cap in capabilities {
        sqlx::query("INSERT OR IGNORE INTO host_capabilities(host_id,capability) VALUES(?,?)")
            .bind(&id)
            .bind(cap.as_str())
            .execute(&mut *tx)
            .await?;
    }
    tx.commit().await?;
    Ok(id)
}

pub async fn get_host(pool: &SqlitePool, id: &str) -> Result<Option<Host>> {
    let row = sqlx::query("SELECT id,name,note,created_at,updated_at FROM hosts WHERE id=?")
        .bind(id)
        .fetch_optional(pool)
        .await?;
    Ok(row.as_ref().map(row_to_host))
}

pub async fn list_hosts(pool: &SqlitePool) -> Result<Vec<Host>> {
    let rows = sqlx::query("SELECT id,name,note,created_at,updated_at FROM hosts ORDER BY name")
        .fetch_all(pool)
        .await?;
    Ok(rows.iter().map(row_to_host).collect())
}

pub async fn capabilities(pool: &SqlitePool, host_id: &str) -> Result<Vec<Capability>> {
    let rows = sqlx::query("SELECT capability FROM host_capabilities WHERE host_id=?")
        .bind(host_id)
        .fetch_all(pool)
        .await?;
    Ok(rows
        .iter()
        .filter_map(|r| Capability::parse(r.get::<String, _>("capability").as_str()))
        .collect())
}

/// 列出拥有某能力的所有 host id（发布门禁与轮询选取目标用）。
pub async fn list_host_ids_with_capability(
    pool: &SqlitePool,
    cap: Capability,
) -> Result<Vec<String>> {
    let rows =
        sqlx::query("SELECT host_id FROM host_capabilities WHERE capability=? ORDER BY host_id")
            .bind(cap.as_str())
            .fetch_all(pool)
            .await?;
    Ok(rows.iter().map(|r| r.get::<String, _>("host_id")).collect())
}
