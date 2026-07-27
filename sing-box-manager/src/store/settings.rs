//! settings 表 typed 访问（read-with-default）。承载**运行期可调**项：告警阈值、保留天数、metrics 开关等。
//! 启动关键项与密钥仍走 env（不入库）。沿用既有 `reset_day` 范式，无需种子迁移——缺键即取默认。

use sqlx::{Row, SqlitePool};

use crate::error::Result;
use crate::store::now_unix;

/// 读原始字符串值（缺键返回 None）。
pub async fn get_raw(pool: &SqlitePool, key: &str) -> Result<Option<String>> {
    let v: Option<Option<String>> = sqlx::query_scalar("SELECT value FROM settings WHERE key=?")
        .bind(key)
        .fetch_optional(pool)
        .await?;
    Ok(v.flatten())
}

/// 字符串，缺键取默认。
pub async fn get_str(pool: &SqlitePool, key: &str, default: &str) -> Result<String> {
    Ok(get_raw(pool, key)
        .await?
        .unwrap_or_else(|| default.to_string()))
}

/// 整数，缺键/解析失败取默认。
pub async fn get_i64(pool: &SqlitePool, key: &str, default: i64) -> Result<i64> {
    Ok(get_raw(pool, key)
        .await?
        .and_then(|s| s.trim().parse::<i64>().ok())
        .unwrap_or(default))
}

/// 布尔（`true/1/yes/on` 为真），缺键取默认。
pub async fn get_bool(pool: &SqlitePool, key: &str, default: bool) -> Result<bool> {
    Ok(match get_raw(pool, key).await? {
        Some(s) => matches!(
            s.trim().to_ascii_lowercase().as_str(),
            "true" | "1" | "yes" | "on"
        ),
        None => default,
    })
}

/// 写入（upsert）。
pub async fn set(pool: &SqlitePool, key: &str, value: &str) -> Result<()> {
    sqlx::query(
        "INSERT INTO settings(key,value,updated_at) VALUES(?,?,?)
         ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at",
    )
    .bind(key)
    .bind(value)
    .bind(now_unix())
    .execute(pool)
    .await?;
    Ok(())
}

/// 全部 settings（管理页/审计只读）。
pub async fn list(pool: &SqlitePool) -> Result<Vec<(String, String)>> {
    let rows = sqlx::query("SELECT key,value FROM settings ORDER BY key")
        .fetch_all(pool)
        .await?;
    Ok(rows
        .iter()
        .map(|r| (r.get::<String, _>("key"), r.get::<String, _>("value")))
        .collect())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::store;

    #[tokio::test]
    async fn typed_get_set_with_defaults() {
        let path = std::env::temp_dir().join(format!("sbm-settings-{}.db", uuid::Uuid::new_v4()));
        let pool = store::open(&path.to_string_lossy()).await.unwrap();
        // 缺键取默认。
        assert_eq!(
            get_i64(&pool, "retention_health_days", 30).await.unwrap(),
            30
        );
        assert!(!get_bool(&pool, "metrics_per_host", false).await.unwrap());
        assert_eq!(get_str(&pool, "x", "def").await.unwrap(), "def");
        // 写入后取到。
        set(&pool, "retention_health_days", "7").await.unwrap();
        set(&pool, "metrics_per_host", "true").await.unwrap();
        assert_eq!(
            get_i64(&pool, "retention_health_days", 30).await.unwrap(),
            7
        );
        assert!(get_bool(&pool, "metrics_per_host", false).await.unwrap());
        // 非法整数回退默认。
        set(&pool, "bad", "notint").await.unwrap();
        assert_eq!(get_i64(&pool, "bad", 5).await.unwrap(), 5);
        pool.close().await;
    }
}
