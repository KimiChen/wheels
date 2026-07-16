//! 结算屏障两阶段。**phase A**（[`prepare_barrier`]）：验证过的新配置落盘 → 排空 → 抓旧进程最终统计入
//! meter_outbox → 登记 pending_barrier，**绝不停旧进程/删用户**，返回 awaiting_meter_ack。**phase B**
//! （[`complete_barrier`]）：收到 Manager meter-ack（序号需匹配暂存 = 已 ingest 的证明）后，才停旧 →
//! 原子替换 → 复用预分配 new_epoch 重启 → 健康。
//!
//! 幂等（审查 D2/D3）：new_epoch 于 phase A 预分配并持久化，phase B 重放以 `active_revision==revision`
//! 为幂等门，绝不二次切换或膨胀 epoch；outbox/pending 于单事务原子登记，命令级去重。

use sqlx::SqlitePool;

use crate::agent::barrier_store::{self as bstore, PendingBarrier};
use crate::agent::deploy::{atomic_replace, report, rollback, write_private};
use crate::agent::gate::DrainWaitGate;
use crate::agent::runtime::{Health, Runtime};
use crate::agent::ssm::SsmClient;
use crate::agent::{state, stats};
use crate::domain::deployment::{DeployPush, DeployReport};
use crate::domain::metering::{MeterBatchResponse, StatsBatch};
use crate::error::{AppError, ErrorCode, Result};

/// phase A：暂存新配置 + 抓旧进程最终统计入 outbox + 登记 pending_barrier，不停旧进程。
#[allow(clippy::too_many_arguments)]
pub async fn prepare_barrier(
    pool: &SqlitePool,
    ssm: &dyn SsmClient,
    gate: &dyn DrainWaitGate,
    config_dir: &str,
    push: &DeployPush,
    plaintext: &[u8],
    sha: &str,
    command_id: &str,
    entry_id: &str,
    old_epoch: i64,
) -> Result<DeployReport> {
    // 新 revision 快照落盘（0600）；**不**替换 live、**不**重启——旧进程与旧用户集原样存活。
    let rev_dir = format!("{config_dir}/revisions");
    std::fs::create_dir_all(&rev_dir)
        .map_err(|e| AppError::new(ErrorCode::Deployment, format!("建 revisions 目录失败: {e}")))?;
    let rev_path = format!("{rev_dir}/{}.json", push.revision);
    write_private(&rev_path, plaintext)?;

    // 排空：礼让在途会话（有界超时）。排空非字节正确性前提，超时即 forced 放行。
    let drain_clean = gate.drain(ssm).await.unwrap_or(false);

    // 排空后抓最终统计（read_local_stats 盖当前 active boot id = old_epoch，因尚未切换）。
    let batch = stats::read_local_stats(ssm, pool).await?;
    let payload = serde_json::to_string(&batch)
        .map_err(|e| AppError::new(ErrorCode::Internal, format!("序列化最终统计失败: {e}")))?;

    // 预分配新 boot id 并持久化：phase B 重放复用同一值，杜绝 epoch 膨胀（D2）。
    let new_epoch = state::next_epoch(pool).await?;

    let pb = PendingBarrier {
        command_id: command_id.to_string(),
        revision: push.revision,
        sha256: sha.to_string(),
        config_path: rev_path,
        role: push.role.clone(),
        entry_id: entry_id.to_string(),
        old_epoch: Some(old_epoch),
        sequence: 0, // 由 stage_barrier 回填
        new_epoch,
        drain_clean,
    };
    let seq = bstore::stage_barrier(pool, &pb, &payload).await?;

    Ok(report(
        "awaiting_meter_ack",
        push.revision,
        Some(old_epoch),
        Some(format!(
            "barrier staged seq={seq} drain_clean={drain_clean}"
        )),
        None,
    ))
}

/// GET /v1/deployments/{id}/meter-batch：返回**暂存的**最终统计（字节级稳定，绝不 live 重读）（D9）。
pub async fn load_meter_batch(pool: &SqlitePool, command_id: &str) -> Result<MeterBatchResponse> {
    let Some(pb) = bstore::get_pending(pool, command_id).await? else {
        return Ok(MeterBatchResponse {
            batch: None,
            drain_clean: false,
        });
    };
    let old = pb.old_epoch.unwrap_or(-1);
    let Some(payload) = bstore::get_outbox_payload(pool, &pb.entry_id, old, pb.sequence).await?
    else {
        return Ok(MeterBatchResponse {
            batch: None,
            drain_clean: pb.drain_clean,
        });
    };
    let mut batch: StatsBatch = serde_json::from_str(&payload)
        .map_err(|e| AppError::new(ErrorCode::Internal, format!("反序列化最终统计失败: {e}")))?;
    batch.singbox_boot_id = old; // 权威 boot id
    batch.sequence = pb.sequence; // 权威序号（精确一次键）
    Ok(MeterBatchResponse {
        batch: Some(batch),
        drain_clean: pb.drain_clean,
    })
}

/// phase B：收到 meter-ack（`ack_boot_id`/`ack_sequence` 需与暂存一致 = Manager 已 ingest 的证明）后，
/// 停旧 → 原子替换 → 复用 new_epoch 重启 → 健康检查。幂等。
/// 返回 `None` = 无待结算屏障（非屏障部署或已完成 → 调用方回放既有结果）。
pub async fn complete_barrier(
    pool: &SqlitePool,
    runtime: &dyn Runtime,
    config_dir: &str,
    command_id: &str,
    ack_boot_id: i64,
    ack_sequence: i64,
) -> Result<Option<DeployReport>> {
    let Some(pb) = bstore::get_pending(pool, command_id).await? else {
        return Ok(None);
    };
    let old = pb.old_epoch.unwrap_or(-1);

    // D9：ack 序号/boot 必须与暂存一致，证明 Manager 确已 ingest 这批；否则拒绝，绝不停旧进程（约束 1）。
    if ack_sequence != pb.sequence || ack_boot_id != old {
        return Ok(Some(report(
            "awaiting_meter_ack",
            pb.revision,
            Some(old),
            Some("meter-ack (boot_id,sequence) 与暂存不符，拒绝切换".into()),
            None,
        )));
    }

    // 确认最终统计已被 Manager 收妥（幂等；停旧前置）。
    bstore::mark_outbox_acked(pool, &pb.entry_id, old, pb.sequence).await?;

    // D2 幂等门：若已切换（active_revision 已是新 revision）→ 不重复 swap/restart，清理并回放。
    if state::active_revision(pool).await? == Some(pb.revision) {
        let ep = state::current_epoch(pool).await?;
        bstore::delete_pending(pool, command_id).await?;
        return Ok(Some(report(
            "deployed",
            pb.revision,
            ep,
            Some("barrier already applied".into()),
            Some("ok".into()),
        )));
    }

    // 停旧 → 原子替换 live ← 暂存快照 → 复用预分配 new_epoch 重启。
    let live = format!("{config_dir}/config.json");
    let bytes = std::fs::read(&pb.config_path)
        .map_err(|e| AppError::new(ErrorCode::Deployment, format!("读暂存快照失败: {e}")))?;
    atomic_replace(&live, &bytes)?;
    if runtime.restart(&live, pb.new_epoch).await.is_err() {
        let r = rollback(pool, runtime, config_dir, "restart_failed").await?;
        bstore::delete_pending(pool, command_id).await?;
        return Ok(Some(r));
    }
    // 重启成功后才记账 → 此后 active_revision==revision 作为幂等门真值。
    state::record_applied(
        pool,
        pb.revision,
        &pb.sha256,
        &pb.config_path,
        &pb.role,
        pb.new_epoch,
    )
    .await?;
    let out = match runtime.health_check().await? {
        Health::Ok => report(
            "deployed",
            pb.revision,
            Some(pb.new_epoch),
            None,
            Some("ok".into()),
        ),
        Health::Down(detail) => {
            // 健康失败自动回滚（起再一个 epoch）；Manager 对任何 live-epoch 变化都会 reconcile（D10）。
            let _ = rollback(pool, runtime, config_dir, "health_failed").await;
            report(
                "health_failed",
                pb.revision,
                Some(pb.new_epoch),
                Some(detail.clone()),
                Some(detail),
            )
        }
    };
    bstore::delete_pending(pool, command_id).await?;
    Ok(Some(out))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::agent::gate::MockDrainGate;
    use crate::agent::runtime::{Health, MockRuntime};
    use crate::agent::ssm::MockSsmClient;
    use crate::agent::state;
    use crate::compiler::canonical::content_sha256;
    use crate::compiler::check;
    use base64::{engine::general_purpose::STANDARD, Engine};
    use serde_json::json;

    fn valid_config(method: &str) -> serde_json::Value {
        let psk = STANDARD.encode([7u8; 16]);
        json!({
            "log": {"level": "warn"},
            "dns": {"servers": [{"tag": "b", "type": "udp", "server": "1.1.1.1"}], "final": "b"},
            "inbounds": [{"type": "shadowsocks", "tag": "in-shared", "listen": "127.0.0.1",
                "listen_port": 19736, "method": method, "password": psk}],
            "outbounds": [{"type": "direct", "tag": "direct"}],
            "route": {"rules": [{"action": "sniff"}], "final": "direct"},
        })
    }

    async fn setup() -> (SqlitePool, String) {
        let dir = std::env::temp_dir().join(format!("sbm-settle-{}", uuid::Uuid::new_v4()));
        std::fs::create_dir_all(&dir).unwrap();
        let pool = state::open(&dir.join("agent.db").to_string_lossy())
            .await
            .unwrap();
        (pool, dir.to_string_lossy().into_owned())
    }

    fn barrier_push(cfg: &serde_json::Value, rev: i64) -> DeployPush {
        DeployPush {
            revision: rev,
            content_sha256: content_sha256(cfg),
            config: cfg.clone(),
            role: "entry".into(),
            barrier_required: true,
            entry_id: Some("e1".into()),
        }
    }

    /// 先常规部署 rev 5 建立旧 boot id，再走屏障 rev 6：phase A 暂存不重启 → meter-batch 可取 →
    /// phase B 收 ack 后才切换；且 phase B 幂等（重放不二次重启）。
    #[tokio::test]
    async fn barrier_two_phase_and_phase_b_idempotent() {
        if !check::available() {
            eprintln!("skip: sing-box 不可用");
            return;
        }
        let (pool, dir) = setup().await;
        let rt = MockRuntime::default();
        let ssm = MockSsmClient::default();
        let gate = MockDrainGate { clean: true };

        // 常规部署 rev 5（无旧 boot id → 直接切换），建立 active epoch。
        rt.push_health(Health::Ok);
        let cfg5 = valid_config("2022-blake3-aes-128-gcm");
        let p5 = DeployPush {
            revision: 5,
            content_sha256: content_sha256(&cfg5),
            config: cfg5.clone(),
            role: "entry".into(),
            barrier_required: true, // 首次无旧 boot id → 落常规切换
            entry_id: Some("e1".into()),
        };
        let r5 = crate::agent::deploy::execute_deploy(&pool, &rt, &ssm, &gate, &dir, &p5, "c5")
            .await
            .unwrap();
        assert_eq!(r5.status, "deployed");
        let old_epoch = state::current_epoch(&pool).await.unwrap().unwrap();

        // 屏障部署 rev 6：phase A 应返回 awaiting_meter_ack 且**不**重启（call_log 仍只有 rev5 的 restart+health）。
        let calls_before = rt.call_log().len();
        let cfg6 = valid_config("2022-blake3-aes-128-gcm");
        let r6 = crate::agent::deploy::execute_deploy(
            &pool,
            &rt,
            &ssm,
            &gate,
            &dir,
            &barrier_push(&cfg6, 6),
            "c6",
        )
        .await
        .unwrap();
        assert_eq!(r6.status, "awaiting_meter_ack");
        assert_eq!(r6.runtime_epoch, Some(old_epoch));
        assert_eq!(rt.call_log().len(), calls_before, "phase A 不得重启旧进程");
        assert_eq!(
            state::active_revision(&pool).await.unwrap(),
            Some(5),
            "phase A 不切换 active revision"
        );

        // meter-batch 可取暂存批（序号=0，盖旧 boot id）。
        let mb = load_meter_batch(&pool, "c6").await.unwrap();
        let batch = mb.batch.expect("有待结算批");
        assert_eq!(batch.singbox_boot_id, old_epoch);
        assert_eq!(batch.sequence, 0);
        // 再取一次字节级稳定（D9）。
        let mb2 = load_meter_batch(&pool, "c6").await.unwrap();
        assert_eq!(mb2.batch.unwrap().singbox_boot_id, old_epoch);

        // phase B：ack 序号匹配 → 切换到 rev 6，新 epoch>old。
        rt.push_health(Health::Ok);
        let done = complete_barrier(&pool, &rt, &dir, "c6", old_epoch, 0)
            .await
            .unwrap()
            .expect("有屏障");
        assert_eq!(done.status, "deployed");
        assert_eq!(done.revision, 6);
        assert_eq!(state::active_revision(&pool).await.unwrap(), Some(6));
        let new_epoch = state::current_epoch(&pool).await.unwrap().unwrap();
        assert!(new_epoch > old_epoch);

        // phase B 幂等（happy 重放）：pending 已删 → None（no-op），不再重启、不膨胀 epoch。
        let calls_after = rt.call_log().len();
        let replay = complete_barrier(&pool, &rt, &dir, "c6", old_epoch, 0)
            .await
            .unwrap();
        assert!(replay.is_none(), "pending 已删 → 幂等 no-op（None）");
        assert_eq!(rt.call_log().len(), calls_after, "重放不得二次重启");
        assert_eq!(
            state::current_epoch(&pool).await.unwrap(),
            Some(new_epoch),
            "重放不得膨胀 epoch"
        );
        pool.close().await;
    }

    /// D2 幂等门：模拟 phase B 已 record_applied（active_revision==revision）但 delete_pending 前崩溃 →
    /// 重放必须走 active_revision 门：返回 deployed、不二次重启、不膨胀 epoch、清理 pending。
    #[tokio::test]
    async fn phase_b_guard_when_pending_lingers_after_swap() {
        if !check::available() {
            eprintln!("skip");
            return;
        }
        let (pool, dir) = setup().await;
        let rt = MockRuntime::default();
        let ssm = MockSsmClient::default();
        let gate = MockDrainGate { clean: true };
        rt.push_health(Health::Ok);
        let cfg5 = valid_config("2022-blake3-aes-128-gcm");
        let p5 = DeployPush {
            revision: 5,
            content_sha256: content_sha256(&cfg5),
            config: cfg5.clone(),
            role: "entry".into(),
            barrier_required: true,
            entry_id: Some("e1".into()),
        };
        crate::agent::deploy::execute_deploy(&pool, &rt, &ssm, &gate, &dir, &p5, "c5")
            .await
            .unwrap();
        let old_epoch = state::current_epoch(&pool).await.unwrap().unwrap();
        crate::agent::deploy::execute_deploy(
            &pool,
            &rt,
            &ssm,
            &gate,
            &dir,
            &barrier_push(&valid_config("2022-blake3-aes-128-gcm"), 6),
            "c6",
        )
        .await
        .unwrap();
        // 模拟已切换（swap+restart 完成、record_applied 已落）但 delete_pending 前崩溃：pending 仍在。
        let pb = crate::agent::barrier_store::get_pending(&pool, "c6")
            .await
            .unwrap()
            .unwrap();
        state::record_applied(&pool, 6, &pb.sha256, &pb.config_path, "entry", pb.new_epoch)
            .await
            .unwrap();
        let calls = rt.call_log().len();
        let r = complete_barrier(&pool, &rt, &dir, "c6", old_epoch, 0)
            .await
            .unwrap()
            .unwrap();
        assert_eq!(r.status, "deployed");
        assert_eq!(rt.call_log().len(), calls, "幂等门不得重启");
        assert_eq!(
            state::current_epoch(&pool).await.unwrap(),
            Some(pb.new_epoch)
        );
        assert!(
            crate::agent::barrier_store::get_pending(&pool, "c6")
                .await
                .unwrap()
                .is_none(),
            "幂等门清理 pending"
        );
        pool.close().await;
    }

    /// meter-ack 序号不匹配 → 拒绝切换（约束 1：不停旧进程）。
    #[tokio::test]
    async fn meter_ack_wrong_sequence_refuses_switch() {
        if !check::available() {
            eprintln!("skip");
            return;
        }
        let (pool, dir) = setup().await;
        let rt = MockRuntime::default();
        let ssm = MockSsmClient::default();
        let gate = MockDrainGate { clean: true };
        rt.push_health(Health::Ok);
        let cfg5 = valid_config("2022-blake3-aes-128-gcm");
        let p5 = DeployPush {
            revision: 5,
            content_sha256: content_sha256(&cfg5),
            config: cfg5.clone(),
            role: "entry".into(),
            barrier_required: true,
            entry_id: Some("e1".into()),
        };
        crate::agent::deploy::execute_deploy(&pool, &rt, &ssm, &gate, &dir, &p5, "c5")
            .await
            .unwrap();
        let old_epoch = state::current_epoch(&pool).await.unwrap().unwrap();
        crate::agent::deploy::execute_deploy(
            &pool,
            &rt,
            &ssm,
            &gate,
            &dir,
            &barrier_push(&valid_config("2022-blake3-aes-128-gcm"), 6),
            "c6",
        )
        .await
        .unwrap();
        // 错误序号 99 → 拒绝，active 仍是 5，旧进程未动。
        let calls = rt.call_log().len();
        let refused = complete_barrier(&pool, &rt, &dir, "c6", old_epoch, 99)
            .await
            .unwrap()
            .unwrap();
        assert_eq!(refused.status, "awaiting_meter_ack");
        assert_eq!(state::active_revision(&pool).await.unwrap(), Some(5));
        assert_eq!(rt.call_log().len(), calls, "拒绝时不得重启");
        pool.close().await;
    }

    #[tokio::test]
    async fn complete_barrier_none_when_no_pending() {
        let (pool, dir) = setup().await;
        let rt = MockRuntime::default();
        let r = complete_barrier(&pool, &rt, &dir, "nope", 0, 0)
            .await
            .unwrap();
        assert!(r.is_none());
        pool.close().await;
    }
}
