//! Agent 读本机 SSM /stats → StatsBatch，盖当前 active boot id（local_revisions.runtime_epoch）。只读无锁、无密钥。

use sqlx::SqlitePool;

use crate::agent::ssm::{SsmClient, INBOUND_TAG};
use crate::agent::state;
use crate::domain::metering::{StatsBatch, StatsUser};
use crate::error::Result;
use crate::store::now_unix;

pub async fn read_local_stats(ssm: &dyn SsmClient, state_pool: &SqlitePool) -> Result<StatsBatch> {
    let s = ssm.read_stats(INBOUND_TAG).await?;
    let boot_id = state::current_epoch(state_pool).await?.unwrap_or(0);
    Ok(StatsBatch {
        inbound_tag: INBOUND_TAG.into(),
        singbox_boot_id: boot_id,
        sequence: 0,
        observed_at: now_unix(),
        tcp_sessions: s.tcp_sessions,
        udp_sessions: s.udp_sessions,
        users: s
            .users
            .into_iter()
            .map(|(name, up, down)| StatsUser {
                identity_name: name,
                uplink_bytes: up,
                downlink_bytes: down,
            })
            .collect(),
    })
}
