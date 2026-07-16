//! 审计日志仓储（audit_logs，0001 建表 Phase 6 启用）。统一记录「谁在何时对何目标做了什么」，
//! 供认证/备份/轮换/保留等全 Phase 6 复用。**detail 绝不含明文密钥/token**（由调用方保证，同全局脱敏约束）。

use serde::Serialize;
use sqlx::{Row, SqlitePool};

use crate::error::Result;
use crate::store::now_unix;

/// 记一条审计。`actor`=管理员用户名或系统标识；`target_*`=被操作对象；`detail`=脱敏后的补充。
pub async fn record(
    pool: &SqlitePool,
    actor: Option<&str>,
    action: &str,
    target_kind: Option<&str>,
    target_id: Option<&str>,
    request_id: Option<&str>,
    detail: Option<&str>,
) -> Result<()> {
    sqlx::query(
        "INSERT INTO audit_logs(id,actor,action,target_kind,target_id,request_id,detail,created_at)
         VALUES(?,?,?,?,?,?,?,?)",
    )
    .bind(uuid::Uuid::new_v4().to_string())
    .bind(actor)
    .bind(action)
    .bind(target_kind)
    .bind(target_id)
    .bind(request_id)
    .bind(detail)
    .bind(now_unix())
    .execute(pool)
    .await?;
    Ok(())
}

/// 审计只读视图（审计页 / API）。
#[derive(Debug, Clone, Serialize)]
pub struct AuditRow {
    pub id: String,
    pub actor: Option<String>,
    pub action: String,
    pub target_kind: Option<String>,
    pub target_id: Option<String>,
    pub request_id: Option<String>,
    pub detail: Option<String>,
    pub created_at: i64,
}

/// 最近 `limit` 条审计（倒序）。
pub async fn list_recent(pool: &SqlitePool, limit: i64) -> Result<Vec<AuditRow>> {
    // rowid 为 SQLite 隐式自增，与插入序单调一致——同秒内也稳定（created_at 仅秒级分辨率）。
    let rows = sqlx::query(
        "SELECT id,actor,action,target_kind,target_id,request_id,detail,created_at
         FROM audit_logs ORDER BY created_at DESC, rowid DESC LIMIT ?",
    )
    .bind(limit.clamp(1, 1000))
    .fetch_all(pool)
    .await?;
    Ok(rows
        .iter()
        .map(|r| AuditRow {
            id: r.get("id"),
            actor: r.get("actor"),
            action: r.get("action"),
            target_kind: r.get("target_kind"),
            target_id: r.get("target_id"),
            request_id: r.get("request_id"),
            detail: r.get("detail"),
            created_at: r.get("created_at"),
        })
        .collect())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::store;

    #[tokio::test]
    async fn record_and_list_recent_orders_desc() {
        let path = std::env::temp_dir().join(format!("sbm-audit-{}.db", uuid::Uuid::new_v4()));
        let pool = store::open(&path.to_string_lossy()).await.unwrap();
        record(&pool, Some("admin"), "login", None, None, None, None)
            .await
            .unwrap();
        record(
            &pool,
            Some("admin"),
            "user.disable",
            Some("user"),
            Some("u1"),
            Some("req-1"),
            Some("reason=quota"),
        )
        .await
        .unwrap();
        let rows = list_recent(&pool, 10).await.unwrap();
        assert_eq!(rows.len(), 2);
        // 最新在前。
        assert_eq!(rows[0].action, "user.disable");
        assert_eq!(rows[0].target_id.as_deref(), Some("u1"));
        assert_eq!(rows[1].action, "login");
        pool.close().await;
    }
}
