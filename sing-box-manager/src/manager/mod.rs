//! Manager 控制面：主动编排 Agent（被动）。所有状态查询、命令派发、发布门禁均由此发起；
//! Agent 从不主动连 Manager。后台任务经连接预算限并发，避免占满 4 连接池。

pub mod agent_client;
pub mod auth;
pub mod auth_mw;
pub mod clock;
pub mod deploy;
pub mod deploy_http;
pub mod diff;
pub mod dispatcher;
pub mod gate;
pub mod http;
pub mod metering;
pub mod metrics;
pub mod observ_http;
pub mod pki_ops;
pub mod poller;
pub mod reconcile;
pub mod retention;
pub mod settlement;
pub mod topology_http;
pub mod traffic_http;
pub mod users_http;

use std::sync::Arc;
use std::time::Duration;

use sqlx::SqlitePool;
use tokio_util::sync::CancellationToken;

use crate::crypto::Cipher;
use crate::error::{AppError, ErrorCode, Result};
use crate::pki::CaRole;

pub const POLL_INTERVAL_SECS: u64 = 60;
pub const DISPATCH_INTERVAL_SECS: u64 = 5;

/// 构建生产 mTLS AgentClient（agent_ca 锚 + Manager 客户端身份）。
pub async fn build_agent_client(
    pool: &SqlitePool,
    cipher: &Cipher,
) -> Result<Arc<dyn agent_client::AgentClient>> {
    let agent_ca_pem = crate::store::pki::active_ca_cert_pem(pool, CaRole::AgentCa)
        .await?
        .ok_or_else(|| AppError::new(ErrorCode::Internal, "无 agent_ca"))?;
    let mat = crate::store::pki::load_manager_client_material(pool, cipher).await?;
    Ok(Arc::new(agent_client::RustlsAgentClient::new(
        agent_ca_pem,
        mat.cert_pem,
        mat.key_pem,
    )))
}

/// 构建生产 mTLS AgentClient 并起后台轮询 + 命令派发循环。随 `cancel` 优雅退出。
pub async fn spawn_background(
    pool: SqlitePool,
    cipher: Arc<Cipher>,
    cancel: CancellationToken,
) -> Result<()> {
    let client = build_agent_client(&pool, &cipher).await?;

    {
        let (pool, client, cancel) = (pool.clone(), client.clone(), cancel.clone());
        tokio::spawn(async move {
            poller::poll_loop(
                pool,
                client,
                Duration::from_secs(POLL_INTERVAL_SECS),
                cancel,
            )
            .await;
        });
    }
    {
        let (pool, client, cancel) = (pool.clone(), client.clone(), cancel.clone());
        tokio::spawn(async move {
            dispatch_loop(pool, client, cancel).await;
        });
    }
    // Phase 5：后台计量循环。
    {
        let (pool, cipher, client, cancel) =
            (pool.clone(), cipher.clone(), client.clone(), cancel.clone());
        tokio::spawn(async move {
            metering::tick::meter_loop(
                pool,
                cipher,
                client,
                Duration::from_secs(metering::tick::METER_INTERVAL_SECS),
                cancel,
            )
            .await;
        });
    }
    // Phase 6：会话 GC（清理过期管理员会话）。
    {
        let (pool, cancel) = (pool.clone(), cancel.clone());
        tokio::spawn(async move {
            session_gc_loop(pool, Duration::from_secs(SESSION_GC_INTERVAL_SECS), cancel).await;
        });
    }
    // Phase 6：数据保留裁剪。
    {
        let (pool, cancel) = (pool.clone(), cancel.clone());
        tokio::spawn(async move {
            retention::prune_loop(pool, cancel).await;
        });
    }
    Ok(())
}

const SESSION_GC_INTERVAL_SECS: u64 = 300;

async fn session_gc_loop(pool: SqlitePool, interval: Duration, cancel: CancellationToken) {
    let mut tick = tokio::time::interval(interval);
    loop {
        tokio::select! {
            _ = cancel.cancelled() => break,
            _ = tick.tick() => {
                match crate::store::sessions::gc_expired(&pool, crate::store::now_unix()).await {
                    Ok(n) if n > 0 => tracing::debug!(cleaned = n, "会话 GC"),
                    Ok(_) => {}
                    Err(e) => tracing::warn!(error = %e, "会话 GC 失败"),
                }
            }
        }
    }
}

async fn dispatch_loop(
    pool: SqlitePool,
    client: Arc<dyn agent_client::AgentClient>,
    cancel: CancellationToken,
) {
    let clock = clock::SystemClock;
    let mut tick = tokio::time::interval(Duration::from_secs(DISPATCH_INTERVAL_SECS));
    loop {
        tokio::select! {
            _ = cancel.cancelled() => break,
            _ = tick.tick() => {
                if let Err(e) = dispatcher::dispatch_pending(&pool, client.as_ref(), &clock).await {
                    tracing::warn!(error = %e, "dispatch_pending 失败");
                }
            }
        }
    }
}
