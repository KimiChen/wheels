//! Manager 侧 SSM reconcile 编排：计算某 Entry 期望身份集 → 取 reconcile 锁（让位 deploy）→ **fresh uuid
//! command_id**（保证重启后 SSM 空时真正重放 re-add，而非回放缓存）→ mTLS 下发 → 写 entry_ssm_state。
//! 触发点：结构 republish 部署成功后、运行态变更(disable/expire)、Manager 启动声明式扫描。

use sqlx::{Row, SqlitePool};

use crate::crypto::Cipher;
use crate::domain::agent::CommandKind;
use crate::domain::user::{ReconcilePush, ReconcileReport, ReconcileUser};
use crate::error::{AppError, ErrorCode, Result};
use crate::manager::agent_client::AgentClient;
use crate::store::{agents, deployments as depl, now_unix, topology, users};

const RECONCILE_LEASE_SECS: i64 = 120;
const INBOUND_TAG: &str = "in-shared";

/// 计算某 Entry 的期望 SSM 身份集（含 uPSK；名字升序确定性）。
pub async fn compute_desired(
    pool: &SqlitePool,
    cipher: &Cipher,
    entry_id: &str,
) -> Result<ReconcilePush> {
    let desired = users::eligible_desired(pool, cipher, entry_id, now_unix()).await?;
    Ok(ReconcilePush {
        inbound_tag: INBOUND_TAG.into(),
        users: desired
            .into_iter()
            .map(|(name, upsk)| ReconcileUser { name, upsk })
            .collect(),
    })
}

/// 受影响 Entry：某用户全部授权 Route 所属的 Entry（去重）。
pub async fn affected_entries(pool: &SqlitePool, user_id: &str) -> Result<Vec<String>> {
    let rows = sqlx::query(
        "SELECT DISTINCT r.entry_id FROM user_routes ur JOIN routes r ON r.id=ur.route_id WHERE ur.user_id=?",
    )
    .bind(user_id)
    .fetch_all(pool)
    .await?;
    Ok(rows
        .iter()
        .map(|r| r.get::<String, _>("entry_id"))
        .collect())
}

/// 对某 Entry 声明式 reconcile。取 reconcile 锁保证不与 deploy 交叠；fresh command_id。
pub async fn reconcile_entry(
    pool: &SqlitePool,
    cipher: &Cipher,
    client: &dyn AgentClient,
    entry_id: &str,
) -> Result<ReconcileReport> {
    let holder = uuid::Uuid::new_v4().to_string();
    if !depl::acquire_entry_lock(pool, entry_id, "reconcile", &holder, RECONCILE_LEASE_SECS).await?
    {
        record_ssm_state(
            pool,
            entry_id,
            None,
            false,
            Some("Entry 被占用，reconcile 跳过"),
        )
        .await?;
        return Err(AppError::new(ErrorCode::Conflict, "Entry 被 deploy 占用"));
    }
    let out = reconcile_locked(pool, cipher, client, entry_id).await;
    let _ = depl::release_entry_lock(pool, entry_id, &holder).await;
    out
}

async fn reconcile_locked(
    pool: &SqlitePool,
    cipher: &Cipher,
    client: &dyn AgentClient,
    entry_id: &str,
) -> Result<ReconcileReport> {
    let push = compute_desired(pool, cipher, entry_id).await?;
    let desired_hash = crate::pki::sha256_hex(
        push.users
            .iter()
            .map(|u| u.name.as_str())
            .collect::<Vec<_>>()
            .join(",")
            .as_bytes(),
    );
    let entry = topology::get_entry(pool, entry_id)
        .await?
        .ok_or_else(|| AppError::new(ErrorCode::NotFound, "entry 不存在"))?;
    let agent = match agents::get_agent(pool, &entry.host_id).await? {
        Some(a) => a,
        None => {
            record_ssm_state(pool, entry_id, Some(&desired_hash), false, Some("无 Agent")).await?;
            return Err(AppError::new(ErrorCode::Agent, "该 Entry 无 Agent"));
        }
    };
    let body = serde_json::to_string(&push).unwrap_or_default();
    let cmd_id = uuid::Uuid::new_v4().to_string(); // FRESH per dispatch（重启后 re-add 的根）
    match client
        .post_command(
            &entry.host_id,
            &agent.mgmt_address,
            CommandKind::Reconcile,
            &cmd_id,
            &body,
        )
        .await
    {
        Ok(resp) => {
            let report: ReconcileReport =
                serde_json::from_str(&resp.body_json).unwrap_or(ReconcileReport {
                    added: vec![],
                    removed: vec![],
                    present: vec![],
                });
            record_ssm_state(pool, entry_id, Some(&desired_hash), true, None).await?;
            Ok(report)
        }
        Err(e) => {
            record_ssm_state(
                pool,
                entry_id,
                Some(&desired_hash),
                false,
                Some(&e.to_string()),
            )
            .await?;
            Err(AppError::new(
                ErrorCode::Agent,
                format!("reconcile 下发失败: {e}"),
            ))
        }
    }
}

/// Manager 启动声明式扫描：对所有含 active Route 的 Entry 回填 SSM（弥补 Phase 5 调度器缺位）。错误隔离。
pub async fn startup_sweep(
    pool: &SqlitePool,
    cipher: &Cipher,
    client: &dyn AgentClient,
) -> Result<()> {
    let rows = sqlx::query("SELECT DISTINCT entry_id FROM routes WHERE status='active'")
        .fetch_all(pool)
        .await?;
    for r in &rows {
        let eid: String = r.get("entry_id");
        if let Err(e) = reconcile_entry(pool, cipher, client, &eid).await {
            tracing::warn!(entry = %eid, error = %e, "启动 reconcile 扫描失败（隔离）");
        }
    }
    Ok(())
}

async fn record_ssm_state(
    pool: &SqlitePool,
    entry_id: &str,
    desired_hash: Option<&str>,
    reconciled: bool,
    error: Option<&str>,
) -> Result<()> {
    let now = now_unix();
    sqlx::query(
        "INSERT INTO entry_ssm_state(entry_id,last_desired_hash,last_reconciled_at,last_error,updated_at)
         VALUES(?,?,?,?,?)
         ON CONFLICT(entry_id) DO UPDATE SET
            last_desired_hash=COALESCE(excluded.last_desired_hash,last_desired_hash),
            last_reconciled_at=CASE WHEN ? THEN excluded.last_reconciled_at ELSE last_reconciled_at END,
            last_error=excluded.last_error, updated_at=excluded.updated_at",
    )
    .bind(entry_id)
    .bind(desired_hash)
    .bind(if reconciled { Some(now) } else { None })
    .bind(error)
    .bind(now)
    .bind(reconciled as i64)
    .execute(pool)
    .await?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::domain::host::Capability;
    use crate::domain::topology::{ExitKind, InboundKind, RouteDraft};
    use crate::manager::agent_client::{AgentError, AgentResponse, MockAgentClient};
    use crate::store::topology::NewEntry;
    use crate::store::{self, topology as topo};
    use base64::{engine::general_purpose::STANDARD, Engine};

    fn cipher() -> Cipher {
        std::env::set_var("ENCRYPTION_MASTER_KEY", STANDARD.encode([9u8; 32]));
        Cipher::from_env(1).unwrap()
    }

    #[tokio::test]
    async fn compute_desired_reflects_eligibility_and_reconcile_records_state() {
        let path = std::env::temp_dir().join(format!("sbm-mrec-{}.db", uuid::Uuid::new_v4()));
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
                public_address: "e.example.com",
                inbound_kind: InboundKind::Shadowsocks,
                ss_method: None,
                allow_direct: false,
            },
        )
        .await
        .unwrap();
        let n1 = topo::create_node(&pool, &c, &nh, "n.example.com", true)
            .await
            .unwrap();
        let r1 = topo::insert_route(
            &pool,
            &RouteDraft {
                id: None,
                label: "r1".into(),
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
        let (uid, _) = store::users::create_user(&pool, "alice", 0, "never", None)
            .await
            .unwrap();
        let name = store::users::grant_route(&pool, &c, &uid, &r1)
            .await
            .unwrap();

        // compute_desired 含该身份。
        let push = compute_desired(&pool, &c, &e1).await.unwrap();
        assert!(push.users.iter().any(|u| u.name == name));

        // reconcile_entry 下发（mock 回执）+ 记 ssm_state。
        let mock = MockAgentClient::default();
        mock.push_post(Ok(AgentResponse {
            http_status: 200,
            ok: true,
            body_json: serde_json::json!({"added":[name],"removed":[],"present":[name]})
                .to_string(),
            echo_command_id: None,
        }));
        let rep = reconcile_entry(&pool, &c, &mock, &e1).await.unwrap();
        assert_eq!(rep.added, vec![name]);
        let reconciled_at: Option<i64> =
            sqlx::query_scalar("SELECT last_reconciled_at FROM entry_ssm_state WHERE entry_id=?")
                .bind(&e1)
                .fetch_one(&pool)
                .await
                .unwrap();
        assert!(reconciled_at.is_some());

        // Agent 不可达 → 记 last_error，不 panic。
        let mock2 = MockAgentClient::default();
        mock2.push_post(Err(AgentError::Timeout));
        assert!(reconcile_entry(&pool, &c, &mock2, &e1).await.is_err());
        let err: Option<String> =
            sqlx::query_scalar("SELECT last_error FROM entry_ssm_state WHERE entry_id=?")
                .bind(&e1)
                .fetch_one(&pool)
                .await
                .unwrap();
        assert!(err.is_some());
        pool.close().await;
    }
}
