//! Manager 主动轮询：只读拉取 Agent `/v1/status`，把结果归约为 [`Observation`] 写库。
//! 状态转移抽成纯函数 [`reduce`] 便于矩阵测试；每 Host 错误隔离，失败记 health_event。

use std::sync::Arc;
use std::time::Duration;

use sqlx::SqlitePool;
use tokio_util::sync::CancellationToken;

use crate::domain::agent::StatusReport;
use crate::error::Result;
use crate::manager::agent_client::{AgentClient, AgentError};
use crate::store;
use crate::store::agents::Observation;

/// 纯函数：一次轮询结果 → 观测。`Timeout`/`Connect` → offline；其余错误 → error；成功 → online。
pub fn reduce(result: &std::result::Result<StatusReport, AgentError>) -> Observation {
    match result {
        Ok(r) => Observation {
            status: "online".into(),
            ok: true,
            singbox_version: r.singbox_version.clone(),
            agent_version: Some(r.agent_version.clone()),
            current_revision: r.current_revision,
            singbox_running: r.singbox_running,
            os_info: Some(r.os.clone()),
            error: None,
        },
        Err(e @ (AgentError::Timeout | AgentError::Connect)) => Observation {
            status: "offline".into(),
            ok: false,
            singbox_version: None,
            agent_version: None,
            current_revision: None,
            singbox_running: false,
            os_info: None,
            error: Some(e.to_string()),
        },
        Err(e) => Observation {
            status: "error".into(),
            ok: false,
            singbox_version: None,
            agent_version: None,
            current_revision: None,
            singbox_running: false,
            os_info: None,
            error: Some(e.to_string()),
        },
    }
}

/// 轮询单个 Host（须已有 agents 行以取得 mgmt 地址）。失败记 health_event，不向上传播（错误隔离）。
pub async fn poll_once(pool: &SqlitePool, client: &dyn AgentClient, host_id: &str) -> Result<()> {
    let Some(agent) = store::agents::get_agent(pool, host_id).await? else {
        return Ok(());
    };
    let result = client.get_status(host_id, &agent.mgmt_address).await;
    let obs = reduce(&result);
    let ok = obs.ok;
    store::agents::record_observation(pool, host_id, &obs).await?;
    if !ok {
        let _ = store::agents::insert_health_event(
            pool,
            Some(host_id),
            "poll_failure",
            obs.error.as_deref(),
        )
        .await;
    }
    Ok(())
}

/// 轮询所有已登记 Agent 的 Host（Phase 1 顺序执行；并发预算优化留待后续）。
pub async fn poll_all(pool: &SqlitePool, client: &dyn AgentClient) -> Result<()> {
    for a in store::agents::list_agents(pool).await? {
        if let Err(e) = poll_once(pool, client, &a.host_id).await {
            tracing::warn!(host = %a.host_id, error = %e, "poll_once 失败");
        }
    }
    Ok(())
}

/// 后台轮询循环。随 `cancel` 优雅退出。
pub async fn poll_loop(
    pool: SqlitePool,
    client: Arc<dyn AgentClient>,
    interval: Duration,
    cancel: CancellationToken,
) {
    let mut tick = tokio::time::interval(interval);
    loop {
        tokio::select! {
            _ = cancel.cancelled() => break,
            _ = tick.tick() => {
                if let Err(e) = poll_all(&pool, client.as_ref()).await {
                    tracing::warn!(error = %e, "poll_all 失败");
                }
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::domain::host::Capability;
    use crate::manager::agent_client::MockAgentClient;

    fn report() -> StatusReport {
        StatusReport {
            host_id: "h".into(),
            agent_version: "0.1.0".into(),
            singbox_version: Some("1.13.14".into()),
            current_revision: Some(7),
            singbox_running: true,
            os: "macos".into(),
            now_unix: 1000,
        }
    }

    #[test]
    fn reduce_maps_outcomes() {
        assert_eq!(reduce(&Ok(report())).status, "online");
        assert!(reduce(&Ok(report())).singbox_running);
        assert_eq!(reduce(&Err(AgentError::Timeout)).status, "offline");
        assert_eq!(reduce(&Err(AgentError::Connect)).status, "offline");
        assert_eq!(reduce(&Err(AgentError::Tls)).status, "error");
        assert_eq!(reduce(&Err(AgentError::Http(500))).status, "error");
    }

    #[tokio::test]
    async fn poll_once_records_online_then_offline() {
        let path = std::env::temp_dir().join(format!("sbm-poll-{}.db", uuid::Uuid::new_v4()));
        let pool = store::open(&path.to_string_lossy()).await.unwrap();
        let host = store::hosts::create_host(&pool, "h", None, &[Capability::Entry])
            .await
            .unwrap();
        store::agents::upsert_agent(&pool, &host, "127.0.0.1:39736")
            .await
            .unwrap();

        let mock = MockAgentClient::default();
        mock.push_status(Ok(report()));
        mock.push_status(Err(AgentError::Timeout));

        poll_once(&pool, &mock, &host).await.unwrap();
        assert_eq!(
            store::agents::get_agent(&pool, &host)
                .await
                .unwrap()
                .unwrap()
                .status,
            "online"
        );
        poll_once(&pool, &mock, &host).await.unwrap();
        let a = store::agents::get_agent(&pool, &host)
            .await
            .unwrap()
            .unwrap();
        assert_eq!(a.status, "offline");
        assert_eq!(a.consecutive_failures, 1);
        pool.close().await;
    }
}
