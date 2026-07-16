//! deployments / deployment_targets / entry_runtime_epochs / deployment_route_activations / entry_locks 仓储。
//! 状态转移用幂等条件 UPDATE；Route 激活台账支持精确回滚；Entry 独占锁原子单飞。

use sqlx::{Row, SqlitePool};

use crate::domain::deployment::{Deployment, DeploymentTarget};
use crate::error::{AppError, ErrorCode, Result};
use crate::store::{now_unix, revisions};

/// 从某 revision 的 artifacts 创建 deployment + 目标（batch 0=node 先，1=entry 后）。
pub async fn create_deployment(
    pool: &SqlitePool,
    kind: &str,
    revision_id: &str,
    previous_revision_id: Option<&str>,
    strategy: &str,
    diff_json: &str,
    created_by: Option<&str>,
) -> Result<String> {
    let artifacts = revisions::list_artifact_meta(pool, revision_id).await?;
    if artifacts.is_empty() {
        return Err(AppError::new(
            ErrorCode::Validation,
            "revision 无 artifact，无法部署",
        ));
    }
    let id = uuid::Uuid::new_v4().to_string();
    let now = now_unix();
    let mut tx = pool.begin().await?;
    sqlx::query(
        "INSERT INTO deployments(id,kind,revision_id,previous_revision_id,status,strategy,diff_json,created_by,created_at,updated_at)
         VALUES(?,?,?,?, 'pending',?,?,?,?,?)",
    )
    .bind(&id)
    .bind(kind)
    .bind(revision_id)
    .bind(previous_revision_id)
    .bind(strategy)
    .bind(diff_json)
    .bind(created_by)
    .bind(now)
    .bind(now)
    .execute(&mut *tx)
    .await?;
    for a in &artifacts {
        let batch = if a.role == "node" { 0 } else { 1 };
        sqlx::query(
            "INSERT INTO deployment_targets(id,deployment_id,host_id,artifact_id,role,scope_ref,batch_order,content_sha256,command_id,status,created_at,updated_at)
             VALUES(?,?,?,?,?,?,?,?,?, 'pending',?,?)",
        )
        .bind(uuid::Uuid::new_v4().to_string())
        .bind(&id)
        .bind(&a.host_id)
        .bind(&a.id)
        .bind(&a.role)
        .bind(&a.scope_ref)
        .bind(batch)
        .bind(&a.content_sha256)
        .bind(uuid::Uuid::new_v4().to_string())
        .bind(now)
        .bind(now)
        .execute(&mut *tx)
        .await?;
    }
    tx.commit().await?;
    Ok(id)
}

fn row_to_deployment(r: &sqlx::sqlite::SqliteRow) -> Deployment {
    Deployment {
        id: r.get("id"),
        kind: r.get("kind"),
        revision_id: r.get("revision_id"),
        previous_revision_id: r.get("previous_revision_id"),
        status: r.get("status"),
        strategy: r.get("strategy"),
        error_summary: r.get("error_summary"),
        created_at: r.get("created_at"),
        updated_at: r.get("updated_at"),
        completed_at: r.get("completed_at"),
    }
}

pub async fn get_deployment(pool: &SqlitePool, id: &str) -> Result<Option<Deployment>> {
    let row = sqlx::query(
        "SELECT id,kind,revision_id,previous_revision_id,status,strategy,error_summary,created_at,updated_at,completed_at FROM deployments WHERE id=?",
    )
    .bind(id)
    .fetch_optional(pool)
    .await?;
    Ok(row.as_ref().map(row_to_deployment))
}

pub async fn list_deployments(pool: &SqlitePool) -> Result<Vec<Deployment>> {
    let rows = sqlx::query(
        "SELECT id,kind,revision_id,previous_revision_id,status,strategy,error_summary,created_at,updated_at,completed_at FROM deployments ORDER BY created_at DESC",
    )
    .fetch_all(pool)
    .await?;
    Ok(rows.iter().map(row_to_deployment).collect())
}

/// 最近一次成功部署的 revision_id（供 diff 与一键回滚的 previous）。
pub async fn last_succeeded_revision(pool: &SqlitePool) -> Result<Option<String>> {
    let row = sqlx::query(
        "SELECT revision_id FROM deployments WHERE status='succeeded' ORDER BY completed_at DESC LIMIT 1",
    )
    .fetch_optional(pool)
    .await?;
    Ok(row.map(|r| r.get("revision_id")))
}

fn row_to_target(r: &sqlx::sqlite::SqliteRow) -> DeploymentTarget {
    DeploymentTarget {
        id: r.get("id"),
        deployment_id: r.get("deployment_id"),
        host_id: r.get("host_id"),
        artifact_id: r.get("artifact_id"),
        role: r.get("role"),
        scope_ref: r.get("scope_ref"),
        batch_order: r.get("batch_order"),
        content_sha256: r.get("content_sha256"),
        command_id: r.get("command_id"),
        status: r.get("status"),
        applied_revision: r.get("applied_revision"),
        runtime_epoch: r.get("runtime_epoch"),
        error_summary: r.get("error_summary"),
        attempts: r.get("attempts"),
    }
}

pub async fn list_targets(pool: &SqlitePool, deployment_id: &str) -> Result<Vec<DeploymentTarget>> {
    let rows = sqlx::query(
        "SELECT id,deployment_id,host_id,artifact_id,role,scope_ref,batch_order,content_sha256,command_id,status,applied_revision,runtime_epoch,error_summary,attempts
         FROM deployment_targets WHERE deployment_id=? ORDER BY batch_order, scope_ref",
    )
    .bind(deployment_id)
    .fetch_all(pool)
    .await?;
    Ok(rows.iter().map(row_to_target).collect())
}

pub async fn set_deployment_status(
    pool: &SqlitePool,
    id: &str,
    status: &str,
    error: Option<&str>,
) -> Result<()> {
    let now = now_unix();
    let terminal = matches!(status, "succeeded" | "failed" | "rolled_back");
    sqlx::query(
        "UPDATE deployments SET status=?, error_summary=COALESCE(?,error_summary), updated_at=?, completed_at=CASE WHEN ? THEN ? ELSE completed_at END WHERE id=?",
    )
    .bind(status)
    .bind(error)
    .bind(now)
    .bind(terminal as i64)
    .bind(now)
    .bind(id)
    .execute(pool)
    .await?;
    Ok(())
}

pub async fn set_target_status(
    pool: &SqlitePool,
    id: &str,
    status: &str,
    applied_revision: Option<i64>,
    runtime_epoch: Option<i64>,
    error: Option<&str>,
) -> Result<()> {
    sqlx::query(
        "UPDATE deployment_targets SET status=?, applied_revision=COALESCE(?,applied_revision),
            runtime_epoch=COALESCE(?,runtime_epoch), error_summary=?, attempts=attempts+1, updated_at=? WHERE id=?",
    )
    .bind(status)
    .bind(applied_revision)
    .bind(runtime_epoch)
    .bind(error)
    .bind(now_unix())
    .bind(id)
    .execute(pool)
    .await?;
    Ok(())
}

// ---------- Route 激活台账 ----------

/// 激活某 Entry 的 draft Route（→active）并记台账（仅翻本次由 draft 变来的）。同事务置 entries.current_revision。
pub async fn activate_entry_routes(
    pool: &SqlitePool,
    deployment_id: &str,
    entry_id: &str,
    revision_seq: i64,
) -> Result<()> {
    let now = now_unix();
    let drafts = sqlx::query("SELECT id FROM routes WHERE entry_id=? AND status='draft'")
        .bind(entry_id)
        .fetch_all(pool)
        .await?;
    let mut tx = pool.begin().await?;
    for r in &drafts {
        let rid: String = r.get("id");
        sqlx::query("INSERT OR IGNORE INTO deployment_route_activations(deployment_id,route_id,prev_status,activated_at) VALUES(?,?, 'draft',?)")
            .bind(deployment_id)
            .bind(&rid)
            .bind(now)
            .execute(&mut *tx)
            .await?;
        sqlx::query(
            "UPDATE routes SET status='active', updated_at=? WHERE id=? AND status='draft'",
        )
        .bind(now)
        .bind(&rid)
        .execute(&mut *tx)
        .await?;
    }
    sqlx::query("UPDATE entries SET current_revision=?, updated_at=? WHERE id=?")
        .bind(revision_seq)
        .bind(now)
        .bind(entry_id)
        .execute(&mut *tx)
        .await?;
    tx.commit().await?;
    Ok(())
}

/// 回滚本次部署的 Route 激活（按台账 active→prev_status）。
pub async fn revert_route_activations(pool: &SqlitePool, deployment_id: &str) -> Result<()> {
    let now = now_unix();
    let rows = sqlx::query(
        "SELECT route_id, prev_status FROM deployment_route_activations WHERE deployment_id=?",
    )
    .bind(deployment_id)
    .fetch_all(pool)
    .await?;
    let mut tx = pool.begin().await?;
    for r in &rows {
        sqlx::query("UPDATE routes SET status=?, updated_at=? WHERE id=?")
            .bind(r.get::<String, _>("prev_status"))
            .bind(now)
            .bind(r.get::<String, _>("route_id"))
            .execute(&mut *tx)
            .await?;
    }
    tx.commit().await?;
    Ok(())
}

// ---------- Entry 独占锁 ----------

/// 原子获取 Entry 锁（INSERT ON CONFLICT DO NOTHING；租约过期可抢占）。返回是否取得。
pub async fn acquire_entry_lock(
    pool: &SqlitePool,
    entry_id: &str,
    holder_kind: &str,
    holder_id: &str,
    lease_secs: i64,
) -> Result<bool> {
    let now = now_unix();
    // 清理过期锁（单活 Manager 崩溃兜底）。
    sqlx::query("DELETE FROM entry_locks WHERE entry_id=? AND expires_at<?")
        .bind(entry_id)
        .bind(now)
        .execute(pool)
        .await?;
    let res = sqlx::query(
        "INSERT INTO entry_locks(entry_id,holder_kind,holder_id,acquired_at,expires_at) VALUES(?,?,?,?,?)
         ON CONFLICT(entry_id) DO NOTHING",
    )
    .bind(entry_id)
    .bind(holder_kind)
    .bind(holder_id)
    .bind(now)
    .bind(now + lease_secs)
    .execute(pool)
    .await?;
    Ok(res.rows_affected() == 1)
}

pub async fn release_entry_lock(pool: &SqlitePool, entry_id: &str, holder_id: &str) -> Result<()> {
    sqlx::query("DELETE FROM entry_locks WHERE entry_id=? AND holder_id=?")
        .bind(entry_id)
        .bind(holder_id)
        .execute(pool)
        .await?;
    Ok(())
}

// ---------- runtime epoch ----------

/// 分配 Entry 新 epoch（MAX+1，置 active，旧 active 归零并落 ended_at）。§9.1：新进程新 epoch。
/// MAX+1 与 INSERT 同事务原子（`INSERT..SELECT`），杜绝并发下 read-then-write 抢同一 epoch。
/// `agent_boot_epoch`=Agent 上报的新 boot id（关联 baseline/outbox/台账所在命名空间）；
/// `barrier_status`='forced' 同时标 unsettled_window=1 供审计。
pub async fn allocate_epoch(
    pool: &SqlitePool,
    entry_id: &str,
    deployment_id: &str,
    revision_id: &str,
    barrier_status: &str,
    agent_boot_epoch: Option<i64>,
) -> Result<i64> {
    let now = now_unix();
    let unsettled: i64 = (barrier_status == "forced") as i64;
    let mut tx = pool.begin().await?;
    sqlx::query(
        "UPDATE entry_runtime_epochs SET active=0, ended_at=? WHERE entry_id=? AND active=1",
    )
    .bind(now)
    .bind(entry_id)
    .execute(&mut *tx)
    .await?;
    sqlx::query(
        "INSERT INTO entry_runtime_epochs(entry_id,epoch,deployment_id,revision_id,barrier_status,agent_boot_epoch,unsettled_window,active,started_at)
         SELECT ?, COALESCE(MAX(epoch),-1)+1, ?, ?, ?, ?, ?, 1, ? FROM entry_runtime_epochs WHERE entry_id=?",
    )
    .bind(entry_id)
    .bind(deployment_id)
    .bind(revision_id)
    .bind(barrier_status)
    .bind(agent_boot_epoch)
    .bind(unsettled)
    .bind(now)
    .bind(entry_id)
    .execute(&mut *tx)
    .await?;
    let next: i64 = sqlx::query_scalar(
        "SELECT epoch FROM entry_runtime_epochs WHERE rowid=last_insert_rowid()",
    )
    .fetch_one(&mut *tx)
    .await?;
    tx.commit().await?;
    Ok(next)
}

/// 该 (entry, deployment) 是否已 ingest 过结算屏障最终批（决定 epoch 的 barrier_status）。
pub async fn has_final_batch(
    pool: &SqlitePool,
    entry_id: &str,
    deployment_id: &str,
) -> Result<bool> {
    let n: i64 = sqlx::query_scalar(
        "SELECT COUNT(*) FROM traffic_batches WHERE entry_id=? AND deployment_id=? AND kind='final'",
    )
    .bind(entry_id)
    .bind(deployment_id)
    .fetch_one(pool)
    .await?;
    Ok(n > 0)
}
