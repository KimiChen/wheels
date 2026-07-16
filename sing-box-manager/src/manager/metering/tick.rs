//! 每 Entry 计量 tick + 后台 meter_loop。取 metering 锁 → get_stats → 单事务 ingest → 配额评估；
//! 资格翻转的用户在锁外触发 reconcile（SSM 删/加身份，不重启）。每 Entry 隔离，单 Entry 失败不阻塞他者。

use std::collections::BTreeSet;
use std::sync::Arc;
use std::time::Duration;

use sqlx::{Row, SqlitePool};
use tokio_util::sync::CancellationToken;

use crate::crypto::Cipher;
use crate::domain::user::ResetCycle;
use crate::error::Result;
use crate::manager::agent_client::AgentClient;
use crate::manager::metering::period::period_for;
use crate::manager::reconcile;
use crate::store::{agents, deployments as depl, metering, runtime_state, topology, users};

pub const METER_INTERVAL_SECS: u64 = 60;
pub const STALE_SECS: i64 = 180;
const METER_LEASE_SECS: i64 = 15;

/// 有 ≥1 active Route 且 host 有 Agent 的 Entry。
async fn active_entries(pool: &SqlitePool) -> Result<Vec<String>> {
    let rows = sqlx::query("SELECT DISTINCT entry_id FROM routes WHERE status='active'")
        .fetch_all(pool)
        .await?;
    Ok(rows
        .iter()
        .map(|r| r.get::<String, _>("entry_id"))
        .collect())
}

/// 对单个 Entry 跑一次计量 tick。返回资格翻转的 user_id（供锁外 reconcile）。
pub async fn meter_tick_entry(
    pool: &SqlitePool,
    client: &dyn AgentClient,
    entry_id: &str,
) -> Result<Vec<String>> {
    let holder = uuid::Uuid::new_v4().to_string();
    if !depl::acquire_entry_lock(pool, entry_id, "metering", &holder, METER_LEASE_SECS).await? {
        return Ok(Vec::new()); // deploy/reconcile 持锁 → 本 tick 跳过（他 Entry 照常）
    }
    let out = meter_tick_locked(pool, client, entry_id).await;
    let _ = depl::release_entry_lock(pool, entry_id, &holder).await;
    out
}

async fn meter_tick_locked(
    pool: &SqlitePool,
    client: &dyn AgentClient,
    entry_id: &str,
) -> Result<Vec<String>> {
    let entry = match topology::get_entry(pool, entry_id).await? {
        Some(e) => e,
        None => return Ok(Vec::new()),
    };
    let agent = match agents::get_agent(pool, &entry.host_id).await? {
        Some(a) => a,
        None => {
            runtime_state::record_stats_err(pool, entry_id, "无 Agent").await?;
            return Ok(Vec::new());
        }
    };
    let batch = match client.get_stats(&entry.host_id, &agent.mgmt_address).await {
        Ok(b) => b,
        Err(e) => {
            runtime_state::record_stats_err(pool, entry_id, &e.to_string()).await?;
            return Ok(Vec::new()); // 故障隔离
        }
    };
    let rd = metering::reset_day(pool).await?;
    let affected = metering::ingest_batch(pool, entry_id, &batch, "poll", None, rd).await?;
    runtime_state::record_stats_ok(pool, entry_id, batch.singbox_boot_id).await?;

    // 配额评估在 ingest 之后（先落最后字节）。仅记翻转。
    let now = crate::store::now_unix();
    let mut flipped = Vec::new();
    for uid in &affected {
        let Some(user) = users::get_user(pool, uid).await? else {
            continue;
        };
        let cycle = ResetCycle::parse(&user.reset_cycle).unwrap_or(ResetCycle::Monthly);
        let period = period_for(now, rd, cycle);
        let (up, down) = metering::period_usage(pool, uid, &period).await?;
        let ev = runtime_state::evaluate_user(
            pool,
            uid,
            &period,
            up + down,
            user.quota_bytes,
            user.expire_at,
            now,
        )
        .await?;
        if ev.flipped {
            flipped.push(uid.clone());
        }
    }
    Ok(flipped)
}

/// 后台计量循环。随 `cancel` 优雅退出。
pub async fn meter_loop(
    pool: SqlitePool,
    cipher: Arc<Cipher>,
    client: Arc<dyn AgentClient>,
    interval: Duration,
    cancel: CancellationToken,
) {
    let mut tick = tokio::time::interval(interval);
    loop {
        tokio::select! {
            _ = cancel.cancelled() => break,
            _ = tick.tick() => {
                let entries = active_entries(&pool).await.unwrap_or_default();
                let mut flipped: BTreeSet<String> = BTreeSet::new();
                for e in &entries {
                    match meter_tick_entry(&pool, client.as_ref(), e).await {
                        Ok(f) => flipped.extend(f),
                        Err(err) => tracing::warn!(entry = %e, error = %err, "meter tick 失败"),
                    }
                }
                // 资格翻转 → 在 metering 锁外 reconcile 受影响 Entry（SSM 删/加身份，不重启）。
                for uid in &flipped {
                    if let Ok(affected) = reconcile::affected_entries(&pool, uid).await {
                        for e in affected {
                            let _ = reconcile::reconcile_entry(&pool, &cipher, client.as_ref(), &e).await;
                        }
                    }
                    let _ = agents::insert_health_event(&pool, None, "quota_eligibility_flip", Some(uid)).await;
                }
                // 过期告警。
                for e in runtime_state::mark_stale(&pool, STALE_SECS).await.unwrap_or_default() {
                    let _ = agents::insert_health_event(&pool, Some(&e), "stats_stale", None).await;
                }
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::domain::host::Capability;
    use crate::domain::metering::{StatsBatch, StatsUser};
    use crate::domain::topology::{ExitKind, InboundKind, RouteDraft};
    use crate::manager::agent_client::{AgentError, MockAgentClient};
    use crate::store::topology::NewEntry;
    use crate::store::{self, topology as topo};
    use base64::{engine::general_purpose::STANDARD, Engine};

    fn cipher() -> Cipher {
        std::env::set_var("ENCRYPTION_MASTER_KEY", STANDARD.encode([9u8; 32]));
        Cipher::from_env(1).unwrap()
    }

    #[tokio::test]
    async fn tick_meters_usage_and_flips_over_quota() {
        let path = std::env::temp_dir().join(format!("sbm-tick-{}.db", uuid::Uuid::new_v4()));
        let pool = store::open(&path.to_string_lossy()).await.unwrap();
        let c = cipher();
        let eh = store::hosts::create_host(&pool, "eh", None, &[Capability::Entry])
            .await
            .unwrap();
        let nh = store::hosts::create_host(&pool, "nh", None, &[Capability::Node])
            .await
            .unwrap();
        store::agents::upsert_agent(&pool, &eh, "127.0.0.1:39736")
            .await
            .unwrap();
        let e1 = topo::create_entry(
            &pool,
            &c,
            &NewEntry {
                host_id: &eh,
                public_address: "e",
                inbound_kind: InboundKind::Shadowsocks,
                ss_method: None,
                allow_direct: false,
            },
        )
        .await
        .unwrap();
        let n1 = topo::create_node(&pool, &c, &nh, "n", true).await.unwrap();
        let r1 = topo::insert_route(
            &pool,
            &RouteDraft {
                id: None,
                label: "r".into(),
                entry_id: e1.clone(),
                hops: vec![],
                exit_kind: ExitKind::Node,
                exit_node_id: Some(n1),
                exit_landing_id: None,
            },
        )
        .await
        .unwrap();
        sqlx::query("UPDATE routes SET status='active' WHERE id=?")
            .bind(&r1)
            .execute(&pool)
            .await
            .unwrap();
        // 配额 100 字节。
        let (uid, _) = store::users::create_user(&pool, "u", 100, "monthly", None)
            .await
            .unwrap();
        let name = store::users::grant_route(&pool, &c, &uid, &r1)
            .await
            .unwrap();

        let mock = MockAgentClient::default();
        // 首个 stats：60 字节 (up 30 + down 30) < 100 → 不翻转。
        mock.push_stats(Ok(batch(&name, 1, 30, 30)));
        let f1 = meter_tick_entry(&pool, &mock, &e1).await.unwrap();
        assert!(f1.is_empty());
        assert!(
            store::users::eligible_desired(&pool, &c, &e1, crate::store::now_unix())
                .await
                .unwrap()
                .contains_key(&name)
        );

        // 第二个 stats：cumulative 涨到 80+80=160 ≥ 100 → 翻转超额。
        mock.push_stats(Ok(batch(&name, 1, 80, 80)));
        let f2 = meter_tick_entry(&pool, &mock, &e1).await.unwrap();
        assert_eq!(f2, vec![uid.clone()]);
        // 超额后 eligible_desired 排除该身份。
        assert!(
            !store::users::eligible_desired(&pool, &c, &e1, crate::store::now_unix())
                .await
                .unwrap()
                .contains_key(&name)
        );
        pool.close().await;
    }

    fn batch(name: &str, boot: i64, up: i64, down: i64) -> StatsBatch {
        StatsBatch {
            inbound_tag: "in-shared".into(),
            singbox_boot_id: boot,
            sequence: 0,
            observed_at: crate::store::now_unix(),
            tcp_sessions: 0,
            udp_sessions: 0,
            users: vec![StatsUser {
                identity_name: name.into(),
                uplink_bytes: up,
                downlink_bytes: down,
            }],
        }
    }

    #[tokio::test]
    async fn tick_isolates_agent_failure() {
        let path = std::env::temp_dir().join(format!("sbm-tickf-{}.db", uuid::Uuid::new_v4()));
        let pool = store::open(&path.to_string_lossy()).await.unwrap();
        let c = cipher();
        let eh = store::hosts::create_host(&pool, "eh", None, &[Capability::Entry])
            .await
            .unwrap();
        store::agents::upsert_agent(&pool, &eh, "127.0.0.1:1")
            .await
            .unwrap();
        let e1 = topo::create_entry(
            &pool,
            &c,
            &NewEntry {
                host_id: &eh,
                public_address: "e",
                inbound_kind: InboundKind::Shadowsocks,
                ss_method: None,
                allow_direct: false,
            },
        )
        .await
        .unwrap();
        let mock = MockAgentClient::default();
        mock.push_stats(Err(AgentError::Timeout));
        // 失败不 panic，记 last_error。
        assert!(meter_tick_entry(&pool, &mock, &e1)
            .await
            .unwrap()
            .is_empty());
        let states = runtime_state::list_entry_states(&pool).await.unwrap();
        assert_eq!(states[0].last_error.as_deref(), Some("timeout"));
        pool.close().await;
    }
}
