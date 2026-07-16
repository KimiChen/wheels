//! 可观测只读聚合：一次查询产出 [`MetricsSnapshot`]，供 Prometheus 文本与 JSON 两种渲染共用。
//! 只含计数/状态枚举/时间戳/版本号——绝无密钥、绝无 per-user 高基数标签。

use serde::Serialize;
use sqlx::{Row, SqlitePool};

use crate::error::Result;
use crate::store::now_unix;

/// 系统指标快照（pull 模型，按需从 SQLite 计算，无内存计数器）。
#[derive(Debug, Clone, Serialize, Default)]
pub struct MetricsSnapshot {
    pub schema_version: i64,
    pub hosts_total: i64,
    pub entries_total: i64,
    pub nodes_total: i64,
    pub landings_total: i64,
    pub routes_by_status: Vec<(String, i64)>,
    pub agents_total: i64,
    pub agents_online: i64,
    pub agents_trusted: i64,
    pub users_total: i64,
    pub users_effective_disabled: i64,
    pub users_over_quota: i64,
    pub deployments_by_status: Vec<(String, i64)>,
    pub entries_stale: i64,
    pub usage_uplink_bytes: i64,
    pub usage_downlink_bytes: i64,
    pub alerts_firing: i64,
}

async fn scalar(pool: &SqlitePool, sql: &str) -> Result<i64> {
    Ok(sqlx::query_scalar::<_, i64>(sql).fetch_one(pool).await?)
}

async fn group_counts(pool: &SqlitePool, sql: &str) -> Result<Vec<(String, i64)>> {
    let rows = sqlx::query(sql).fetch_all(pool).await?;
    Ok(rows
        .iter()
        .map(|r| (r.get::<String, _>(0), r.get::<i64, _>(1)))
        .collect())
}

/// 计算一份指标快照。`agent_stale_secs`=判定 agent 在线的 last_polled_at 新鲜阈值。
pub async fn metrics_snapshot(pool: &SqlitePool, agent_stale_secs: i64) -> Result<MetricsSnapshot> {
    let now = now_unix();
    let (up, down): (i64, i64) = sqlx::query_as(
        "SELECT COALESCE(SUM(uplink_bytes),0), COALESCE(SUM(downlink_bytes),0) FROM usage_buckets",
    )
    .fetch_one(pool)
    .await?;
    Ok(MetricsSnapshot {
        schema_version: scalar(
            pool,
            "SELECT COALESCE(MAX(version),0) FROM schema_migrations",
        )
        .await?,
        hosts_total: scalar(pool, "SELECT COUNT(*) FROM hosts").await?,
        entries_total: scalar(pool, "SELECT COUNT(*) FROM entries").await?,
        nodes_total: scalar(pool, "SELECT COUNT(*) FROM nodes").await?,
        landings_total: scalar(pool, "SELECT COUNT(*) FROM landings").await?,
        routes_by_status: group_counts(
            pool,
            "SELECT status, COUNT(*) FROM routes GROUP BY status ORDER BY status",
        )
        .await?,
        agents_total: scalar(pool, "SELECT COUNT(*) FROM agents").await?,
        agents_online: sqlx::query_scalar::<_, i64>(
            "SELECT COUNT(*) FROM agents WHERE last_polled_at IS NOT NULL AND ?-last_polled_at<=?",
        )
        .bind(now)
        .bind(agent_stale_secs)
        .fetch_one(pool)
        .await?,
        agents_trusted: scalar(
            pool,
            "SELECT COUNT(*) FROM agent_certificates WHERE trust_status='trusted'",
        )
        .await?,
        users_total: scalar(pool, "SELECT COUNT(*) FROM users").await?,
        users_effective_disabled: scalar(
            pool,
            "SELECT COUNT(*) FROM user_runtime_state WHERE effective_disabled=1",
        )
        .await?,
        users_over_quota: scalar(
            pool,
            "SELECT COUNT(*) FROM user_runtime_state WHERE quota_state='over'",
        )
        .await?,
        deployments_by_status: group_counts(
            pool,
            "SELECT status, COUNT(*) FROM deployments GROUP BY status ORDER BY status",
        )
        .await?,
        entries_stale: scalar(
            pool,
            "SELECT COUNT(*) FROM entry_runtime_state WHERE stale=1",
        )
        .await?,
        usage_uplink_bytes: up,
        usage_downlink_bytes: down,
        alerts_firing: scalar(
            pool,
            "SELECT COUNT(*) FROM alert_state WHERE status='firing'",
        )
        .await?,
    })
}

// ---------- alert_state 读写（告警状态机；本轮建表 + 基础读写，规则引擎后续接入）----------

/// 置某 (rule, subject) 为 firing（新起或维持）。返回是否为**新** firing（状态跃迁，供决定是否通知）。
pub async fn upsert_firing(
    pool: &SqlitePool,
    rule_id: &str,
    subject_kind: &str,
    subject_id: &str,
    severity: &str,
    detail: Option<&str>,
) -> Result<bool> {
    let now = now_unix();
    let prev: Option<String> = sqlx::query_scalar(
        "SELECT status FROM alert_state WHERE rule_id=? AND subject_kind=? AND subject_id=?",
    )
    .bind(rule_id)
    .bind(subject_kind)
    .bind(subject_id)
    .fetch_optional(pool)
    .await?;
    let is_new = prev.as_deref() != Some("firing");
    sqlx::query(
        "INSERT INTO alert_state(rule_id,subject_kind,subject_id,severity,status,detail,since,updated_at)
         VALUES(?,?,?,?,'firing',?,?,?)
         ON CONFLICT(rule_id,subject_kind,subject_id) DO UPDATE SET
            severity=excluded.severity, status='firing', detail=excluded.detail,
            since=CASE WHEN alert_state.status='firing' THEN alert_state.since ELSE excluded.since END,
            updated_at=excluded.updated_at, resolved_at=NULL",
    )
    .bind(rule_id)
    .bind(subject_kind)
    .bind(subject_id)
    .bind(severity)
    .bind(detail)
    .bind(now)
    .bind(now)
    .execute(pool)
    .await?;
    Ok(is_new)
}

/// 置某 (rule, subject) 为 resolved（若原为 firing）。返回是否发生 firing→resolved 跃迁。
pub async fn mark_resolved(
    pool: &SqlitePool,
    rule_id: &str,
    subject_kind: &str,
    subject_id: &str,
) -> Result<bool> {
    let now = now_unix();
    let r = sqlx::query(
        "UPDATE alert_state SET status='resolved', resolved_at=?, updated_at=?
         WHERE rule_id=? AND subject_kind=? AND subject_id=? AND status='firing'",
    )
    .bind(now)
    .bind(now)
    .bind(rule_id)
    .bind(subject_kind)
    .bind(subject_id)
    .execute(pool)
    .await?;
    Ok(r.rows_affected() > 0)
}

#[derive(Debug, Clone, Serialize)]
pub struct ActiveAlert {
    pub rule_id: String,
    pub subject_kind: String,
    pub subject_id: String,
    pub severity: String,
    pub detail: Option<String>,
    pub since: i64,
}

pub async fn list_firing(pool: &SqlitePool) -> Result<Vec<ActiveAlert>> {
    let rows = sqlx::query(
        "SELECT rule_id,subject_kind,subject_id,severity,detail,since FROM alert_state
         WHERE status='firing' ORDER BY severity DESC, since",
    )
    .fetch_all(pool)
    .await?;
    Ok(rows
        .iter()
        .map(|r| ActiveAlert {
            rule_id: r.get("rule_id"),
            subject_kind: r.get("subject_kind"),
            subject_id: r.get("subject_id"),
            severity: r.get("severity"),
            detail: r.get("detail"),
            since: r.get("since"),
        })
        .collect())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::domain::host::Capability;
    use crate::store;

    #[tokio::test]
    async fn snapshot_counts_and_alert_transitions() {
        let path = std::env::temp_dir().join(format!("sbm-obs-{}.db", uuid::Uuid::new_v4()));
        let pool = store::open(&path.to_string_lossy()).await.unwrap();
        store::hosts::create_host(&pool, "h1", None, &[Capability::Entry])
            .await
            .unwrap();
        store::hosts::create_host(&pool, "h2", None, &[Capability::Node])
            .await
            .unwrap();
        let snap = metrics_snapshot(&pool, 90).await.unwrap();
        assert_eq!(snap.hosts_total, 2);
        assert_eq!(snap.schema_version, 9);
        assert_eq!(snap.agents_total, 0);
        assert_eq!(snap.alerts_firing, 0);

        // 告警跃迁：首次 firing=new；再 firing=非 new；resolved=跃迁。
        assert!(
            upsert_firing(&pool, "agent_down", "host", "h1", "warning", Some("x"))
                .await
                .unwrap()
        );
        assert!(
            !upsert_firing(&pool, "agent_down", "host", "h1", "warning", Some("x"))
                .await
                .unwrap()
        );
        assert_eq!(list_firing(&pool).await.unwrap().len(), 1);
        assert_eq!(metrics_snapshot(&pool, 90).await.unwrap().alerts_firing, 1);
        assert!(mark_resolved(&pool, "agent_down", "host", "h1")
            .await
            .unwrap());
        assert!(!mark_resolved(&pool, "agent_down", "host", "h1")
            .await
            .unwrap());
        assert_eq!(list_firing(&pool).await.unwrap().len(), 0);
        pool.close().await;
    }
}
