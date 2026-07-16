//! agents / agent_status_snapshots / health_events 仓储。写入 Manager 主动轮询得到的**实际状态**
//! （与数据库中的期望状态分离），并提供发布门禁所需读取。

use crate::error::Result;
use crate::store::now_unix;
use sqlx::{Row, SqlitePool};

/// 一次轮询归约后的观测（由 `manager::poller::reduce` 产出）。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Observation {
    pub status: String, // agents.status 枚举字符串
    pub ok: bool,
    pub singbox_version: Option<String>,
    pub agent_version: Option<String>,
    pub current_revision: Option<i64>,
    pub singbox_running: bool,
    pub os_info: Option<String>,
    pub error: Option<String>,
}

/// agents 行读视图。
#[derive(Debug, Clone)]
pub struct AgentRow {
    pub host_id: String,
    pub mgmt_address: String,
    pub status: String,
    pub singbox_version: Option<String>,
    pub agent_version: Option<String>,
    pub current_revision: Option<i64>,
    pub singbox_running: bool,
    pub last_polled_at: Option<i64>,
    pub last_ok_at: Option<i64>,
    pub last_error: Option<String>,
    pub consecutive_failures: i64,
}

fn row_to_agent(r: &sqlx::sqlite::SqliteRow) -> AgentRow {
    AgentRow {
        host_id: r.get("host_id"),
        mgmt_address: r.get("mgmt_address"),
        status: r.get("status"),
        singbox_version: r.get("singbox_version"),
        agent_version: r.get("agent_version"),
        current_revision: r.get("current_revision"),
        singbox_running: r.get::<i64, _>("singbox_running") != 0,
        last_polled_at: r.get("last_polled_at"),
        last_ok_at: r.get("last_ok_at"),
        last_error: r.get("last_error"),
        consecutive_failures: r.get("consecutive_failures"),
    }
}

/// 登记（或更新）某 Host 的 Agent 管理地址；enrollment 时调用。
pub async fn upsert_agent(pool: &SqlitePool, host_id: &str, mgmt_address: &str) -> Result<()> {
    let now = now_unix();
    sqlx::query(
        "INSERT INTO agents(host_id,mgmt_address,status,created_at,updated_at) VALUES(?,?,'unknown',?,?)
         ON CONFLICT(host_id) DO UPDATE SET mgmt_address=excluded.mgmt_address, updated_at=excluded.updated_at",
    )
    .bind(host_id)
    .bind(mgmt_address)
    .bind(now)
    .bind(now)
    .execute(pool)
    .await?;
    Ok(())
}

/// 写入一次观测：更新 agents 去规范化最新值 + 追加快照。单事务、短小，避免占满连接池。
pub async fn record_observation(pool: &SqlitePool, host_id: &str, obs: &Observation) -> Result<()> {
    let now = now_unix();
    let mut tx = pool.begin().await?;
    if obs.ok {
        sqlx::query(
            "UPDATE agents SET status=?, singbox_version=?, agent_version=?, current_revision=?,
                singbox_running=?, os_info=?, last_polled_at=?, last_ok_at=?, last_error=NULL,
                consecutive_failures=0, updated_at=? WHERE host_id=?",
        )
        .bind(&obs.status)
        .bind(&obs.singbox_version)
        .bind(&obs.agent_version)
        .bind(obs.current_revision)
        .bind(obs.singbox_running as i64)
        .bind(&obs.os_info)
        .bind(now)
        .bind(now)
        .bind(now)
        .bind(host_id)
        .execute(&mut *tx)
        .await?;
    } else {
        sqlx::query(
            "UPDATE agents SET status=?, last_polled_at=?, last_error=?,
                consecutive_failures=consecutive_failures+1, updated_at=? WHERE host_id=?",
        )
        .bind(&obs.status)
        .bind(now)
        .bind(&obs.error)
        .bind(now)
        .bind(host_id)
        .execute(&mut *tx)
        .await?;
    }
    sqlx::query(
        "INSERT INTO agent_status_snapshots(id,host_id,ok,singbox_version,agent_version,current_revision,singbox_running,sys_info_json,error_code,polled_at)
         VALUES(?,?,?,?,?,?,?,?,?,?)",
    )
    .bind(uuid::Uuid::new_v4().to_string())
    .bind(host_id)
    .bind(obs.ok as i64)
    .bind(&obs.singbox_version)
    .bind(&obs.agent_version)
    .bind(obs.current_revision)
    .bind(obs.singbox_running as i64)
    .bind(&obs.os_info)
    .bind(&obs.error)
    .bind(now)
    .execute(&mut *tx)
    .await?;
    tx.commit().await?;
    Ok(())
}

pub async fn get_agent(pool: &SqlitePool, host_id: &str) -> Result<Option<AgentRow>> {
    let row = sqlx::query(
        "SELECT host_id,mgmt_address,status,singbox_version,agent_version,current_revision,singbox_running,
                last_polled_at,last_ok_at,last_error,consecutive_failures FROM agents WHERE host_id=?",
    )
    .bind(host_id)
    .fetch_optional(pool)
    .await?;
    Ok(row.as_ref().map(row_to_agent))
}

pub async fn list_agents(pool: &SqlitePool) -> Result<Vec<AgentRow>> {
    let rows = sqlx::query(
        "SELECT host_id,mgmt_address,status,singbox_version,agent_version,current_revision,singbox_running,
                last_polled_at,last_ok_at,last_error,consecutive_failures FROM agents ORDER BY host_id",
    )
    .fetch_all(pool)
    .await?;
    Ok(rows.iter().map(row_to_agent).collect())
}

/// 记录一条健康事件（观测/告警；发布门禁与详情页用）。
pub async fn insert_health_event(
    pool: &SqlitePool,
    host_id: Option<&str>,
    kind: &str,
    detail: Option<&str>,
) -> Result<()> {
    sqlx::query("INSERT INTO health_events(id,host_id,kind,detail,created_at) VALUES(?,?,?,?,?)")
        .bind(uuid::Uuid::new_v4().to_string())
        .bind(host_id)
        .bind(kind)
        .bind(detail)
        .bind(now_unix())
        .execute(pool)
        .await?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::domain::host::Capability;
    use crate::store;

    async fn pool_host() -> (sqlx::SqlitePool, String) {
        let path = std::env::temp_dir().join(format!("sbm-agents-{}.db", uuid::Uuid::new_v4()));
        let pool = store::open(&path.to_string_lossy()).await.unwrap();
        let host = store::hosts::create_host(&pool, "h", None, &[Capability::Entry])
            .await
            .unwrap();
        (pool, host)
    }

    #[tokio::test]
    async fn observation_ok_then_fail_tracks_state() {
        let (pool, host) = pool_host().await;
        upsert_agent(&pool, &host, "127.0.0.1:39736").await.unwrap();

        record_observation(
            &pool,
            &host,
            &Observation {
                status: "online".into(),
                ok: true,
                singbox_version: Some("1.13.14".into()),
                agent_version: Some("0.1.0".into()),
                current_revision: Some(5),
                singbox_running: true,
                os_info: Some("macos".into()),
                error: None,
            },
        )
        .await
        .unwrap();
        let a = get_agent(&pool, &host).await.unwrap().unwrap();
        assert_eq!(a.status, "online");
        assert!(a.singbox_running && a.last_ok_at.is_some());
        assert_eq!(a.current_revision, Some(5));
        assert_eq!(a.consecutive_failures, 0);

        // 失败观测递增 consecutive_failures，保留上次 last_ok_at。
        record_observation(
            &pool,
            &host,
            &Observation {
                status: "offline".into(),
                ok: false,
                singbox_version: None,
                agent_version: None,
                current_revision: None,
                singbox_running: false,
                os_info: None,
                error: Some("timeout".into()),
            },
        )
        .await
        .unwrap();
        let a2 = get_agent(&pool, &host).await.unwrap().unwrap();
        assert_eq!(a2.status, "offline");
        assert_eq!(a2.consecutive_failures, 1);
        assert!(a2.last_ok_at.is_some(), "失败不清空 last_ok_at");
        assert_eq!(a2.last_error.as_deref(), Some("timeout"));
        pool.close().await;
    }
}
