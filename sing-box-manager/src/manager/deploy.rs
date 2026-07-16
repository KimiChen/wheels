//! 发布编排：校验 revision 已 checked → 算 diff → 建 deployment+targets → 取 Entry 锁 →
//! Node 批先、Entry 批后依赖发布 → 每目标解封 artifact 经 mTLS 推送（明文绝不落库）→ 映射回执 →
//! 全成功激活 Route + 分配 epoch；任一批失败回滚已应用目标 + 还原 Route 激活。

use serde_json::Value;
use sqlx::SqlitePool;

use crate::crypto::Cipher;
use crate::domain::agent::CommandKind;
use crate::domain::deployment::{DeployPush, DeployReport, DeploymentTarget, TargetStatus};
use crate::error::{AppError, ErrorCode, Result};
use crate::manager::agent_client::AgentClient;
use crate::manager::{diff, reconcile, settlement};
use crate::store::{agents, deployments as depl, revisions};

const LOCK_LEASE_SECS: i64 = 3600;

/// 创建部署：revision 必须 check 通过；算 diff、建 deployment + 目标。返回 deployment_id。
pub async fn create_deployment(
    pool: &SqlitePool,
    revision_id: &str,
    strategy: &str,
    created_by: Option<&str>,
) -> Result<String> {
    let rev = revisions::get_revision(pool, revision_id)
        .await?
        .ok_or_else(|| AppError::new(ErrorCode::NotFound, "revision 不存在"))?;
    if rev.status != "checked" {
        return Err(AppError::new(
            ErrorCode::Validation,
            format!("revision 须先 sing-box check 通过（当前 {}）", rev.status),
        ));
    }
    let prev = depl::last_succeeded_revision(pool).await?;
    let diff_json = diff::compute_diff(pool, revision_id, prev.as_deref()).await?;
    depl::create_deployment(
        pool,
        "deploy",
        revision_id,
        prev.as_deref(),
        strategy,
        &diff_json.to_string(),
        created_by,
    )
    .await
}

/// 驱动部署到终态（Phase 3 同步执行各批）。
pub async fn drive(
    pool: &SqlitePool,
    cipher: &Cipher,
    client: &dyn AgentClient,
    dep_id: &str,
) -> Result<()> {
    let dep = depl::get_deployment(pool, dep_id)
        .await?
        .ok_or_else(|| AppError::new(ErrorCode::NotFound, "deployment 不存在"))?;
    if matches!(dep.status.as_str(), "succeeded" | "failed" | "rolled_back") {
        return Ok(());
    }
    let rev = revisions::get_revision(pool, &dep.revision_id)
        .await?
        .ok_or_else(|| AppError::new(ErrorCode::NotFound, "revision 不存在"))?;
    let targets = depl::list_targets(pool, dep_id).await?;
    let entry_ids: Vec<String> = targets
        .iter()
        .filter(|t| t.role == "entry")
        .map(|t| t.scope_ref.clone())
        .collect();

    // 取所有相关 Entry 的独占锁；任一失败即整体失败（不改运行态）。
    for e in &entry_ids {
        if !depl::acquire_entry_lock(pool, e, "deploy", dep_id, LOCK_LEASE_SECS).await? {
            depl::set_deployment_status(pool, dep_id, "failed", Some("Entry 被占用，取锁失败"))
                .await?;
            release_locks(pool, &entry_ids, dep_id).await;
            return Ok(());
        }
    }

    // batch 0：Node 先。
    depl::set_deployment_status(pool, dep_id, "deploying_nodes", None).await?;
    if !run_batch(pool, cipher, client, rev.seq, 0, &targets).await? {
        rollback_deployed(pool, client, &targets).await;
        depl::set_deployment_status(pool, dep_id, "rolled_back", Some("Node 批部署失败")).await?;
        release_locks(pool, &entry_ids, dep_id).await;
        return Ok(());
    }

    // batch 1：Entry 后。
    depl::set_deployment_status(pool, dep_id, "deploying_entries", None).await?;
    if !run_batch(pool, cipher, client, rev.seq, 1, &targets).await? {
        rollback_deployed(pool, client, &targets).await;
        depl::revert_route_activations(pool, dep_id).await?;
        depl::set_deployment_status(pool, dep_id, "rolled_back", Some("Entry 批部署失败")).await?;
        release_locks(pool, &entry_ids, dep_id).await;
        return Ok(());
    }

    // 激活：仅在对应 Entry 目标 deployed 后，分配新 epoch，翻 draft Route → active。
    depl::set_deployment_status(pool, dep_id, "activating", None).await?;
    let fresh = depl::list_targets(pool, dep_id).await?;
    for e in &entry_ids {
        // barrier_status：ingest 过最终批 → settled，否则 not_required。agent_boot_epoch=该 entry 回执 boot id。
        let barrier_status = if depl::has_final_batch(pool, e, dep_id).await? {
            "settled"
        } else {
            "not_required"
        };
        let agent_boot = fresh
            .iter()
            .find(|t| t.role == "entry" && &t.scope_ref == e)
            .and_then(|t| t.runtime_epoch);
        depl::allocate_epoch(
            pool,
            e,
            dep_id,
            &dep.revision_id,
            barrier_status,
            agent_boot,
        )
        .await?;
        depl::activate_entry_routes(pool, dep_id, e, rev.seq).await?;
    }
    depl::set_deployment_status(pool, dep_id, "succeeded", None).await?;
    release_locks(pool, &entry_ids, dep_id).await;

    // D10/D11：先释放 deploy 锁再 reconcile（reconcile_entry 需另取 entry 锁）。任何 live-epoch 变化后，
    // 重启的新进程 SSM 用户集为空（Agent 无 uPSK 缓存，仅 Manager 能复原）→ 逐 Entry 补齐期望身份集。
    for e in &entry_ids {
        if let Err(err) = reconcile::reconcile_entry(pool, cipher, client, e).await {
            tracing::warn!(entry = %e, error = %err, "部署后 reconcile 失败（用户集可能待补）");
        }
    }
    Ok(())
}

/// 一键回滚：把上一个成功 revision 作为新部署重新推送（Node 先 Entry 后走同流程）。
pub async fn rollback_to_previous(
    pool: &SqlitePool,
    revision_id_to_restore: &str,
    created_by: Option<&str>,
) -> Result<String> {
    let rev = revisions::get_revision(pool, revision_id_to_restore)
        .await?
        .ok_or_else(|| AppError::new(ErrorCode::NotFound, "目标 revision 不存在"))?;
    if rev.status != "checked" {
        return Err(AppError::new(
            ErrorCode::Validation,
            "回滚目标 revision 未通过 check",
        ));
    }
    let prev = depl::last_succeeded_revision(pool).await?;
    let diff_json = diff::compute_diff(pool, revision_id_to_restore, prev.as_deref()).await?;
    depl::create_deployment(
        pool,
        "rollback",
        revision_id_to_restore,
        prev.as_deref(),
        "normal",
        &diff_json.to_string(),
        created_by,
    )
    .await
}

/// 派发一批目标。全部 deployed 返回 true；任一失败返回 false（已应用者留待回滚）。
async fn run_batch(
    pool: &SqlitePool,
    cipher: &Cipher,
    client: &dyn AgentClient,
    rev_seq: i64,
    batch: i64,
    targets: &[DeploymentTarget],
) -> Result<bool> {
    for t in targets.iter().filter(|t| t.batch_order == batch) {
        let report = dispatch_target(pool, cipher, client, rev_seq, t).await;
        let status = report.target_status();
        depl::set_target_status(
            pool,
            &t.id,
            status.as_str(),
            Some(report.revision),
            report.runtime_epoch,
            report.output.as_deref(),
        )
        .await?;
        if !status.is_terminal_ok() {
            return Ok(false);
        }
    }
    Ok(true)
}

/// 解封 artifact → 组 DeployPush → mTLS 推送 → 解析回执。明文只活在内存 + TLS 通道。
async fn dispatch_target(
    pool: &SqlitePool,
    cipher: &Cipher,
    client: &dyn AgentClient,
    rev_seq: i64,
    t: &DeploymentTarget,
) -> DeployReport {
    let fail = |msg: String| DeployReport {
        status: "failed".into(),
        revision: rev_seq,
        runtime_epoch: None,
        output: Some(msg),
        health: None,
    };
    let plaintext = match revisions::load_artifact_plaintext(pool, cipher, &t.artifact_id).await {
        Ok(p) => p,
        Err(e) => return fail(format!("解封 artifact 失败: {e}")),
    };
    let config: Value = match serde_json::from_slice(&plaintext) {
        Ok(v) => v,
        Err(e) => return fail(format!("artifact 非法 JSON: {e}")),
    };
    let mgmt = match agents::get_agent(pool, &t.host_id).await {
        Ok(Some(a)) => a.mgmt_address,
        _ => return fail(format!("Host {} 无 Agent", t.host_id)),
    };
    // barrier：Manager 据角色判定，绝不由调用方 bool 豁免（D8）。entry 目标每次都重启计量进程 → 需结算屏障；
    // entry_id 随之下发（Agent outbox 键，D11）。node 无 per-user 计量 → 无需屏障。
    let (barrier_required, entry_id) = if t.role == "entry" {
        (true, Some(t.scope_ref.clone()))
    } else {
        (false, None)
    };
    let push = DeployPush {
        revision: rev_seq,
        content_sha256: t.content_sha256.clone(),
        config,
        role: t.role.clone(),
        barrier_required,
        entry_id,
    };
    let body = serde_json::to_string(&push).unwrap_or_default();
    let cmd_id = t.command_id.clone().unwrap_or_default();
    let report = match client
        .post_command(&t.host_id, &mgmt, CommandKind::Deploy, &cmd_id, &body)
        .await
    {
        Ok(resp) => serde_json::from_str::<DeployReport>(&resp.body_json)
            .unwrap_or_else(|e| fail(format!("回执解析失败: {e}"))),
        Err(e) => return fail(format!("Agent 调用失败: {e}")),
    };
    // 结算屏障：Agent phase A 回 awaiting_meter_ack → ingest 最终批 + meter-ack → 得 phase B 终态回执。
    if report.status == "awaiting_meter_ack" {
        return match settlement::settle_and_ack(
            pool,
            client,
            &t.scope_ref,
            &t.host_id,
            &mgmt,
            &cmd_id,
            rev_seq,
            &t.deployment_id,
        )
        .await
        {
            Ok(outcome) => outcome.report,
            Err(e) => fail(format!("结算屏障失败: {e}")),
        };
    }
    report
}

/// 回滚本次已 deployed 的目标（POST /v1/rollback，Agent 用本机上一快照回滚）。best-effort。
async fn rollback_deployed(
    pool: &SqlitePool,
    client: &dyn AgentClient,
    targets: &[DeploymentTarget],
) {
    for t in targets {
        // 重新查当前状态，只回滚已 deployed 的。
        let cur = depl::list_targets(pool, &t.deployment_id).await.ok();
        let is_deployed = cur
            .as_ref()
            .and_then(|v| v.iter().find(|x| x.id == t.id))
            .map(|x| x.status == "deployed")
            .unwrap_or(false);
        if !is_deployed {
            continue;
        }
        if let Ok(Some(a)) = agents::get_agent(pool, &t.host_id).await {
            let cmd_id = uuid::Uuid::new_v4().to_string();
            let _ = client
                .post_command(
                    &t.host_id,
                    &a.mgmt_address,
                    CommandKind::Rollback,
                    &cmd_id,
                    "{}",
                )
                .await;
        }
        let _ = depl::set_target_status(
            pool,
            &t.id,
            TargetStatus::RolledBack.as_str(),
            None,
            None,
            None,
        )
        .await;
    }
}

async fn release_locks(pool: &SqlitePool, entry_ids: &[String], dep_id: &str) {
    for e in entry_ids {
        let _ = depl::release_entry_lock(pool, e, dep_id).await;
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::domain::host::Capability;
    use crate::domain::topology::{ExitKind, InboundKind, RouteDraft};
    use crate::manager::agent_client::{AgentResponse, MockAgentClient};
    use crate::store::topology::NewEntry;
    use crate::store::{self, topology as topo};
    use base64::{engine::general_purpose::STANDARD, Engine};

    fn cipher() -> Cipher {
        std::env::set_var("ENCRYPTION_MASTER_KEY", STANDARD.encode([9u8; 32]));
        Cipher::from_env(1).unwrap()
    }
    async fn pool() -> SqlitePool {
        let path = std::env::temp_dir().join(format!("sbm-mdep-{}.db", uuid::Uuid::new_v4()));
        store::open(&path.to_string_lossy()).await.unwrap()
    }
    fn deployed(
        rev: i64,
    ) -> std::result::Result<AgentResponse, crate::manager::agent_client::AgentError> {
        report_resp("deployed", rev)
    }
    fn report_resp(
        status: &str,
        rev: i64,
    ) -> std::result::Result<AgentResponse, crate::manager::agent_client::AgentError> {
        let body = serde_json::json!({"status": status, "revision": rev, "runtime_epoch": 1, "output": null, "health": "x"});
        Ok(AgentResponse {
            http_status: 200,
            ok: status == "deployed",
            body_json: body.to_string(),
            echo_command_id: None,
        })
    }

    /// 建 e1 + n1 + 一条 e1→n1 Route，编译并把 revision 标 checked；注册两台 Agent。
    async fn setup(pool: &SqlitePool, c: &Cipher) -> (String, String) {
        let eh = store::hosts::create_host(pool, "eh", None, &[Capability::Entry])
            .await
            .unwrap();
        let nh = store::hosts::create_host(pool, "nh", None, &[Capability::Node])
            .await
            .unwrap();
        store::agents::upsert_agent(pool, &eh, "127.0.0.1:39736")
            .await
            .unwrap();
        store::agents::upsert_agent(pool, &nh, "127.0.0.1:39737")
            .await
            .unwrap();
        let e1 = topo::create_entry(
            pool,
            c,
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
        let n1 = topo::create_node(pool, c, &nh, "n1.example.com", true)
            .await
            .unwrap();
        topo::insert_route(
            pool,
            &RouteDraft {
                id: None,
                label: "r1".into(),
                entry_id: e1.clone(),
                hops: vec![],
                exit_kind: ExitKind::Node,
                exit_node_id: Some(n1.clone()),
                exit_landing_id: None,
            },
        )
        .await
        .unwrap();
        let rev = revisions::compile_and_persist(pool, c, &e1, Some("1.13.14"), None)
            .await
            .unwrap();
        sqlx::query("UPDATE config_revisions SET status='checked' WHERE id=?")
            .bind(&rev.id)
            .execute(pool)
            .await
            .unwrap();
        (rev.id, e1)
    }

    #[tokio::test]
    async fn deploy_succeeds_node_first_then_entry_then_activates_routes() {
        let pool = pool().await;
        let c = cipher();
        let (rev_id, e1) = setup(&pool, &c).await;
        let mock = MockAgentClient::default();
        mock.push_post(deployed(1)); // node n1（batch 0）
        mock.push_post(deployed(1)); // entry e1（batch 1）

        let dep = create_deployment(&pool, &rev_id, "normal", None)
            .await
            .unwrap();
        drive(&pool, &c, &mock, &dep).await.unwrap();

        let d = depl::get_deployment(&pool, &dep).await.unwrap().unwrap();
        assert_eq!(d.status, "succeeded");
        // Route 激活。
        let route = topo::list_routes(&pool).await.unwrap();
        assert_eq!(route[0].status, "active");
        // entry current_revision 置位。
        assert!(topo::get_entry(&pool, &e1)
            .await
            .unwrap()
            .unwrap()
            .current_revision
            .is_some());
        // epoch 分配。
        let n: i64 = sqlx::query_scalar(
            "SELECT COUNT(*) FROM entry_runtime_epochs WHERE entry_id=? AND active=1",
        )
        .bind(&e1)
        .fetch_one(&pool)
        .await
        .unwrap();
        assert_eq!(n, 1);
        pool.close().await;
    }

    #[tokio::test]
    async fn entry_health_failure_rolls_back_and_routes_stay_draft() {
        let pool = pool().await;
        let c = cipher();
        let (rev_id, _e1) = setup(&pool, &c).await;
        let mock = MockAgentClient::default();
        mock.push_post(deployed(1)); // node deployed
        mock.push_post(report_resp("health_failed", 1)); // entry 健康失败
                                                         // rollback_deployed 会给已 deployed 的 node 发 Rollback（无脚本→mock 返回 Err，被忽略）。

        let dep = create_deployment(&pool, &rev_id, "normal", None)
            .await
            .unwrap();
        drive(&pool, &c, &mock, &dep).await.unwrap();

        let d = depl::get_deployment(&pool, &dep).await.unwrap().unwrap();
        assert_eq!(d.status, "rolled_back");
        // Route 未激活（激活在两批都成功后才发生）。
        assert_eq!(topo::list_routes(&pool).await.unwrap()[0].status, "draft");
        pool.close().await;
    }

    #[tokio::test]
    async fn reject_deploy_of_unchecked_revision() {
        let pool = pool().await;
        let c = cipher();
        let (rev_id, _) = setup(&pool, &c).await;
        sqlx::query("UPDATE config_revisions SET status='compiled' WHERE id=?")
            .bind(&rev_id)
            .execute(&pool)
            .await
            .unwrap();
        assert!(create_deployment(&pool, &rev_id, "normal", None)
            .await
            .is_err());
        pool.close().await;
    }
}
