//! 发布门禁：fail-closed。所有判据肯定为真才 `Publishable`，任一未知/不满足即 `Blocked{reason}`。
//! 保证「未安装或不可达 Agent 的 Entry/Node 不能进入可发布状态」（todo §13/§14）。

use serde::Serialize;
use sqlx::SqlitePool;

use crate::domain::agent::ReadinessReason;
use crate::error::Result;
use crate::store;

/// 证书临期安全余量：not_after 须晚于 now + 7 天。
pub const CERT_MARGIN_SECS: i64 = 7 * 24 * 3600;
/// 新鲜度窗口默认值：3 × 60s 轮询周期。
pub const DEFAULT_FRESHNESS_SECS: i64 = 3 * 60;

/// 门禁判据输入（从 DB 快照收集）。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct GateInput {
    pub has_agent_row: bool,
    pub trust_status: Option<String>,
    pub cert_not_after: Option<i64>,
    pub agent_status: Option<String>,
    pub last_ok_at: Option<i64>,
    pub singbox_running: bool,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
#[serde(tag = "state", content = "reason", rename_all = "snake_case")]
pub enum Readiness {
    Publishable,
    Blocked(ReadinessReason),
}

/// 纯函数门禁。判据顺序即诊断优先级。
pub fn host_publishable(input: &GateInput, now: i64, freshness_secs: i64) -> Readiness {
    use ReadinessReason::*;
    if !input.has_agent_row {
        return Readiness::Blocked(NoAgent);
    }
    if input.trust_status.as_deref() != Some("trusted") {
        return Readiness::Blocked(Untrusted);
    }
    match input.cert_not_after {
        Some(na) if na > now + CERT_MARGIN_SECS => {}
        _ => return Readiness::Blocked(CertExpiring),
    }
    if input.agent_status.as_deref() != Some("online") {
        return Readiness::Blocked(Offline);
    }
    match input.last_ok_at {
        Some(t) if now - t <= freshness_secs => {}
        _ => return Readiness::Blocked(Stale),
    }
    if !input.singbox_running {
        return Readiness::Blocked(SingboxDown);
    }
    Readiness::Publishable
}

/// 从 DB 收集某 Host 的门禁输入快照。
pub async fn gather(pool: &SqlitePool, host_id: &str) -> Result<GateInput> {
    let agent = store::agents::get_agent(pool, host_id).await?;
    let cert = store::pki::agent_cert_info(pool, host_id).await?;
    Ok(GateInput {
        has_agent_row: agent.is_some(),
        trust_status: cert.as_ref().map(|c| c.trust_status.clone()),
        cert_not_after: cert.as_ref().and_then(|c| c.not_after),
        agent_status: agent.as_ref().map(|a| a.status.clone()),
        last_ok_at: agent.as_ref().and_then(|a| a.last_ok_at),
        singbox_running: agent.as_ref().map(|a| a.singbox_running).unwrap_or(false),
    })
}

pub async fn host_readiness(
    pool: &SqlitePool,
    host_id: &str,
    now: i64,
    freshness_secs: i64,
) -> Result<Readiness> {
    Ok(host_publishable(
        &gather(pool, host_id).await?,
        now,
        freshness_secs,
    ))
}

#[derive(Debug, Clone, Serialize)]
pub struct Blocked {
    pub host_id: String,
    pub reason: ReadinessReason,
}

/// 对一批目标 Host 逐一门禁；返回所有被阻断项（空 = 全部可发布）。
pub async fn preflight(
    pool: &SqlitePool,
    host_ids: &[String],
    now: i64,
    freshness_secs: i64,
) -> Result<Vec<Blocked>> {
    let mut blocked = Vec::new();
    for h in host_ids {
        if let Readiness::Blocked(reason) = host_readiness(pool, h, now, freshness_secs).await? {
            blocked.push(Blocked {
                host_id: h.clone(),
                reason,
            });
        }
    }
    Ok(blocked)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn green(now: i64) -> GateInput {
        GateInput {
            has_agent_row: true,
            trust_status: Some("trusted".into()),
            cert_not_after: Some(now + CERT_MARGIN_SECS + 100),
            agent_status: Some("online".into()),
            last_ok_at: Some(now - 10),
            singbox_running: true,
        }
    }

    #[test]
    fn green_is_publishable() {
        let now = 1_000_000;
        assert_eq!(
            host_publishable(&green(now), now, DEFAULT_FRESHNESS_SECS),
            Readiness::Publishable
        );
    }

    #[test]
    fn each_missing_condition_blocks_with_reason() {
        use ReadinessReason::*;
        let now = 1_000_000;
        let f = DEFAULT_FRESHNESS_SECS;

        let mut i = green(now);
        i.has_agent_row = false;
        assert_eq!(host_publishable(&i, now, f), Readiness::Blocked(NoAgent));

        let mut i = green(now);
        i.trust_status = Some("pending".into());
        assert_eq!(host_publishable(&i, now, f), Readiness::Blocked(Untrusted));

        let mut i = green(now);
        i.cert_not_after = Some(now + 100); // 临期
        assert_eq!(
            host_publishable(&i, now, f),
            Readiness::Blocked(CertExpiring)
        );

        let mut i = green(now);
        i.agent_status = Some("offline".into());
        assert_eq!(host_publishable(&i, now, f), Readiness::Blocked(Offline));

        let mut i = green(now);
        i.last_ok_at = Some(now - f - 1); // 陈旧
        assert_eq!(host_publishable(&i, now, f), Readiness::Blocked(Stale));

        let mut i = green(now);
        i.singbox_running = false;
        assert_eq!(
            host_publishable(&i, now, f),
            Readiness::Blocked(SingboxDown)
        );
    }

    #[test]
    fn never_polled_host_is_not_publishable() {
        // 新 Host：无 agent 行 → 天然不可发布（默认拒绝）。
        let now = 1_000_000;
        let fresh = GateInput {
            has_agent_row: false,
            trust_status: None,
            cert_not_after: None,
            agent_status: None,
            last_ok_at: None,
            singbox_running: false,
        };
        assert!(matches!(
            host_publishable(&fresh, now, DEFAULT_FRESHNESS_SECS),
            Readiness::Blocked(_)
        ));
    }
}
