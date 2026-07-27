//! 计量存储（SQLite / sqlx，运行时查询，无编译期 DATABASE_URL 依赖）。
//! 表：users（配额/有效期/重置策略/停用）· usage（每周期累计）· baseline（增量基线）。

use anyhow::Result;
use sqlx::sqlite::{SqliteConnectOptions, SqliteJournalMode, SqlitePoolOptions};
use sqlx::{Row, SqlitePool};
use std::time::Duration;

const DDL: &str = "
CREATE TABLE IF NOT EXISTS users(
  name TEXT PRIMARY KEY,
  quota_bytes INTEGER NOT NULL,
  expire_at INTEGER,
  reset_cycle TEXT NOT NULL DEFAULT 'monthly',
  active_period TEXT NOT NULL,
  disabled INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS usage(
  name TEXT NOT NULL, period TEXT NOT NULL,
  up INTEGER NOT NULL DEFAULT 0, down INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(name, period)
);
CREATE TABLE IF NOT EXISTS baseline(
  account TEXT NOT NULL, identity TEXT NOT NULL, inbound TEXT NOT NULL,
  last_up INTEGER NOT NULL DEFAULT 0, last_down INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(identity, inbound)
);
";

pub async fn open(path: &str) -> Result<SqlitePool> {
    if let Some(dir) = std::path::Path::new(path).parent() {
        if !dir.as_os_str().is_empty() {
            std::fs::create_dir_all(dir).ok();
        }
    }
    let opts = SqliteConnectOptions::new()
        .filename(path)
        .create_if_missing(true)
        .journal_mode(SqliteJournalMode::Wal)
        .busy_timeout(Duration::from_secs(5));
    let pool = SqlitePoolOptions::new()
        .max_connections(4)
        .connect_with(opts)
        .await?;
    for stmt in DDL.split(';') {
        let s = stmt.trim();
        if !s.is_empty() {
            sqlx::query(s).execute(&pool).await?;
        }
    }
    Ok(pool)
}

pub async fn sync_user(
    pool: &SqlitePool,
    name: &str,
    quota: i64,
    expire: Option<i64>,
    reset_cycle: &str,
    current_period: &str,
) -> Result<()> {
    let mut tx = pool.begin().await?;
    let existing = sqlx::query("SELECT reset_cycle,active_period FROM users WHERE name=?")
        .bind(name)
        .fetch_optional(&mut *tx)
        .await?;

    if let Some(row) = existing {
        let old_cycle = row.get::<String, _>("reset_cycle");
        let old_period = row.get::<String, _>("active_period");

        if old_cycle != reset_cycle {
            // 策略切换不是一次免费重置：把旧策略当前桶的余额「移动」到新策略当前桶
            // ——累加进目标桶（而非覆盖）再删除源桶，保证来回切换既不丢失也不重复计量。
            // （旧策略当前桶取自 active_period，每 tick 更新，至多滞后一个轮询间隔。）
            let (up, down) = sqlx::query("SELECT up,down FROM usage WHERE name=? AND period=?")
                .bind(name)
                .bind(&old_period)
                .fetch_optional(&mut *tx)
                .await?
                .map(|r| (r.get::<i64, _>("up"), r.get::<i64, _>("down")))
                .unwrap_or((0, 0));
            if (up != 0 || down != 0) && old_period != current_period {
                sqlx::query(
                    "INSERT INTO usage(name,period,up,down) VALUES(?,?,?,?)
                     ON CONFLICT(name,period) DO UPDATE SET up=up+excluded.up, down=down+excluded.down",
                )
                .bind(name)
                .bind(current_period)
                .bind(up)
                .bind(down)
                .execute(&mut *tx)
                .await?;
                sqlx::query("DELETE FROM usage WHERE name=? AND period=?")
                    .bind(name)
                    .bind(&old_period)
                    .execute(&mut *tx)
                    .await?;
            }
        }

        sqlx::query(
            "UPDATE users SET quota_bytes=?,expire_at=?,reset_cycle=?,active_period=? WHERE name=?",
        )
        .bind(quota)
        .bind(expire)
        .bind(reset_cycle)
        .bind(current_period)
        .bind(name)
        .execute(&mut *tx)
        .await?;
    } else {
        sqlx::query(
            "INSERT INTO users(name,quota_bytes,expire_at,reset_cycle,active_period) VALUES(?,?,?,?,?)",
        )
        .bind(name)
        .bind(quota)
        .bind(expire)
        .bind(reset_cycle)
        .bind(current_period)
        .execute(&mut *tx)
        .await?;
    }
    tx.commit().await?;
    Ok(())
}

pub async fn all_user_names(pool: &SqlitePool) -> Result<Vec<String>> {
    let rows = sqlx::query("SELECT name FROM users")
        .fetch_all(pool)
        .await?;
    Ok(rows.iter().map(|r| r.get::<String, _>("name")).collect())
}

pub async fn delete_user(pool: &SqlitePool, name: &str) -> Result<()> {
    for t in [
        "DELETE FROM users WHERE name=?",
        "DELETE FROM usage WHERE name=?",
    ] {
        sqlx::query(t).bind(name).execute(pool).await?;
    }
    sqlx::query("DELETE FROM baseline WHERE account=?")
        .bind(name)
        .execute(pool)
        .await?;
    Ok(())
}

pub async fn add_usage(
    pool: &SqlitePool,
    name: &str,
    period: &str,
    dup: i64,
    ddown: i64,
) -> Result<()> {
    sqlx::query(
        "INSERT INTO usage(name,period,up,down) VALUES(?,?,?,?)
         ON CONFLICT(name,period) DO UPDATE SET up=up+excluded.up, down=down+excluded.down",
    )
    .bind(name)
    .bind(period)
    .bind(dup)
    .bind(ddown)
    .execute(pool)
    .await?;
    Ok(())
}

pub async fn period_usage(pool: &SqlitePool, name: &str, period: &str) -> Result<(i64, i64)> {
    let row = sqlx::query("SELECT up,down FROM usage WHERE name=? AND period=?")
        .bind(name)
        .bind(period)
        .fetch_optional(pool)
        .await?;
    Ok(row.map(|r| (r.get("up"), r.get("down"))).unwrap_or((0, 0)))
}

pub async fn get_baseline(pool: &SqlitePool, identity: &str, inbound: &str) -> Result<(i64, i64)> {
    let row = sqlx::query("SELECT last_up,last_down FROM baseline WHERE identity=? AND inbound=?")
        .bind(identity)
        .bind(inbound)
        .fetch_optional(pool)
        .await?;
    Ok(row
        .map(|r| (r.get("last_up"), r.get("last_down")))
        .unwrap_or((0, 0)))
}

pub async fn set_baseline(
    pool: &SqlitePool,
    account: &str,
    identity: &str,
    inbound: &str,
    up: i64,
    down: i64,
) -> Result<()> {
    sqlx::query(
        "INSERT INTO baseline(account,identity,inbound,last_up,last_down) VALUES(?,?,?,?,?)
         ON CONFLICT(identity,inbound) DO UPDATE SET account=excluded.account, last_up=excluded.last_up, last_down=excluded.last_down",
    )
    .bind(account)
    .bind(identity)
    .bind(inbound)
    .bind(up)
    .bind(down)
    .execute(pool)
    .await?;
    Ok(())
}

pub async fn set_disabled(pool: &SqlitePool, name: &str, disabled: bool) -> Result<()> {
    sqlx::query("UPDATE users SET disabled=? WHERE name=?")
        .bind(disabled as i64)
        .bind(name)
        .execute(pool)
        .await?;
    Ok(())
}

/// (quota_bytes, expire_at)
pub async fn user_limits(pool: &SqlitePool, name: &str) -> Result<(i64, Option<i64>)> {
    let row = sqlx::query("SELECT quota_bytes,expire_at FROM users WHERE name=?")
        .bind(name)
        .fetch_optional(pool)
        .await?;
    Ok(row
        .map(|r| (r.get("quota_bytes"), r.get::<Option<i64>, _>("expire_at")))
        .unwrap_or((0, None)))
}

pub async fn get_disabled(pool: &SqlitePool, name: &str) -> Result<bool> {
    let row = sqlx::query("SELECT disabled FROM users WHERE name=?")
        .bind(name)
        .fetch_optional(pool)
        .await?;
    Ok(row
        .map(|r| r.get::<i64, _>("disabled") != 0)
        .unwrap_or(false))
}

#[cfg(test)]
mod tests {
    use super::{
        add_usage, delete_user, get_baseline, open, period_usage, set_baseline, sync_user,
    };

    fn temp_db() -> std::path::PathBuf {
        std::env::temp_dir().join(format!("sbm-{}.db", uuid::Uuid::new_v4()))
    }

    fn cleanup(path: &std::path::Path) {
        for suffix in ["", "-wal", "-shm"] {
            let _ = std::fs::remove_file(format!("{}{suffix}", path.display()));
        }
    }

    #[tokio::test]
    async fn policy_change_carries_usage_but_natural_boundary_resets() {
        let path = temp_db();
        let path_str = path.to_string_lossy();
        let pool = open(&path_str).await.unwrap();

        sync_user(&pool, "alice", 1_000, None, "monthly", "2027-03")
            .await
            .unwrap();
        add_usage(&pool, "alice", "2027-03", 7, 11).await.unwrap();

        sync_user(&pool, "alice", 1_000, None, "yearly", "2027")
            .await
            .unwrap();
        assert_eq!(period_usage(&pool, "alice", "2027").await.unwrap(), (7, 11));

        // 同一策略自然跨年时进入空桶，不继承上一年的用量。
        sync_user(&pool, "alice", 1_000, None, "yearly", "2028")
            .await
            .unwrap();
        assert_eq!(period_usage(&pool, "alice", "2028").await.unwrap(), (0, 0));

        pool.close().await;
        cleanup(&path);
    }

    #[tokio::test]
    async fn policy_toggle_back_neither_loses_nor_double_counts() {
        let path = temp_db();
        let path_str = path.to_string_lossy();
        let pool = open(&path_str).await.unwrap();

        sync_user(&pool, "alice", 1_000, None, "yearly", "2027")
            .await
            .unwrap();
        add_usage(&pool, "alice", "2027", 100, 200).await.unwrap();

        // 年度 -> 月度：余额「移动」到当前月桶，源桶清空。
        sync_user(&pool, "alice", 1_000, None, "monthly", "2027-07")
            .await
            .unwrap();
        assert_eq!(
            period_usage(&pool, "alice", "2027-07").await.unwrap(),
            (100, 200)
        );
        assert_eq!(period_usage(&pool, "alice", "2027").await.unwrap(), (0, 0));

        // 月度 -> 年度（同年回切）：既不丢失也不重复（不是 200/400）。
        sync_user(&pool, "alice", 1_000, None, "yearly", "2027")
            .await
            .unwrap();
        assert_eq!(
            period_usage(&pool, "alice", "2027").await.unwrap(),
            (100, 200)
        );
        assert_eq!(
            period_usage(&pool, "alice", "2027-07").await.unwrap(),
            (0, 0)
        );

        pool.close().await;
        cleanup(&path);
    }

    #[tokio::test]
    async fn access_identities_keep_independent_baselines_but_share_account_cleanup() {
        let path = temp_db();
        let path_str = path.to_string_lossy();
        let pool = open(&path_str).await.unwrap();

        sync_user(&pool, "alice", 1_000, None, "monthly", "2027-03")
            .await
            .unwrap();
        set_baseline(&pool, "alice", "alice-entry", "in-shared", 10, 20)
            .await
            .unwrap();
        set_baseline(&pool, "alice", "alice-home", "in-shared", 30, 40)
            .await
            .unwrap();

        assert_eq!(
            get_baseline(&pool, "alice-entry", "in-shared")
                .await
                .unwrap(),
            (10, 20)
        );
        assert_eq!(
            get_baseline(&pool, "alice-home", "in-shared")
                .await
                .unwrap(),
            (30, 40)
        );

        delete_user(&pool, "alice").await.unwrap();
        assert_eq!(
            get_baseline(&pool, "alice-entry", "in-shared")
                .await
                .unwrap(),
            (0, 0)
        );

        pool.close().await;
        cleanup(&path);
    }
}
