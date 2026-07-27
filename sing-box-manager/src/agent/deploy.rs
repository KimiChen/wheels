//! Agent 部署核心：sha 校验 → 真实 sing-box check → 原子替换 → restart → 健康检查 →
//! local_revisions 提交 active；失败自动恢复旧 active。进程生命周期经 [`Runtime`] trait（可 mock）；
//! 其余全用真实文件 + 真实 check。
//! 保留当前与上一个成功 revision 的磁盘配置，回滚=用旧快照原子替换 + restart（新 epoch，§9.1）。

use std::io::Write;

use sqlx::SqlitePool;

use crate::agent::gate::DrainWaitGate;
use crate::agent::runtime::{Health, Runtime};
use crate::agent::ssm::SsmClient;
use crate::agent::{settle, state};
use crate::compiler::canonical::{canonical_bytes, content_sha256};
use crate::compiler::check::{check_config, secret_values_in};
use crate::domain::deployment::{DeployPush, DeployReport};
use crate::error::{AppError, ErrorCode, Result};

pub(crate) fn report(
    status: &str,
    revision: i64,
    epoch: Option<i64>,
    output: Option<String>,
    health: Option<String>,
) -> DeployReport {
    DeployReport {
        status: status.into(),
        revision,
        runtime_epoch: epoch,
        output,
        health,
    }
}

/// 执行一次部署。任一步失败即 return，旧配置完好；health 失败自动回滚。
/// `barrier_required && 有旧 boot id && 有 entry_id` → 进结算屏障 phase A（暂存 + 抓最终统计，不停旧进程，
/// 返回 awaiting_meter_ack）；否则常规切换（首次部署无旧进程可结算，直接切）。
pub async fn execute_deploy(
    pool: &SqlitePool,
    runtime: &dyn Runtime,
    ssm: &dyn SsmClient,
    gate: &dyn DrainWaitGate,
    config_dir: &str,
    push: &DeployPush,
    command_id: &str,
) -> Result<DeployReport> {
    // 1) sha 校验（对明文规范字节现算，须等于 Manager 编译时的 content_sha256）。
    let plaintext = canonical_bytes(&push.config);
    let sha = content_sha256(&push.config);
    if sha != push.content_sha256 {
        return Ok(report(
            "sha_mismatch",
            push.revision,
            None,
            Some("content_sha256 不匹配，拒绝应用".into()),
            None,
        ));
    }

    // 2) 真实 sing-box check（纵深防御；失败保留旧 revision，不写 live、不 restart）。
    let redact = secret_values_in(&plaintext);
    let checked = check_config(&plaintext, &redact)?;
    if !checked.passed {
        return Ok(report(
            "check_failed",
            push.revision,
            None,
            Some(checked.output),
            None,
        ));
    }

    // 3) 结算屏障 phase A：仅当明确要求、存在旧 boot id（有可结算的运行态）、且携带 entry_id 时进入。
    if push.barrier_required {
        if let (Some(entry_id), Some(old_epoch)) =
            (push.entry_id.as_deref(), state::current_epoch(pool).await?)
        {
            return settle::prepare_barrier(
                pool, ssm, gate, config_dir, push, &plaintext, &sha, command_id, entry_id,
                old_epoch,
            )
            .await;
        }
        // 无旧 boot id（首次部署）或无 entry_id → 无可结算运行态，落常规切换。
    }

    // 4) 原子替换：写 revisions/<rev>.json 0600 快照 + rename 覆盖 config.json。
    let rev_dir = format!("{config_dir}/revisions");
    std::fs::create_dir_all(&rev_dir)
        .map_err(|e| AppError::new(ErrorCode::Deployment, format!("建 revisions 目录失败: {e}")))?;
    let rev_path = format!("{rev_dir}/{}.json", push.revision);
    write_private(&rev_path, &plaintext)?;
    let live = format!("{config_dir}/config.json");
    atomic_replace(&live, &plaintext)?;

    // 5) 预分配 epoch。只有启动和健康检查成功后才提交 active。
    let epoch = state::next_epoch(pool).await?;

    // 6) 受控 restart。失败 → 回滚旧版本。
    if runtime.restart(&live, epoch).await.is_err() {
        return restore_active_after_failure(pool, runtime, config_dir, "restart_failed").await;
    }

    // 7) 健康检查。成功后才提交 active；失败恢复数据库仍指向的旧 active。
    match runtime.health_check(&push.role).await {
        Ok(Health::Ok) => {
            state::record_applied(pool, push.revision, &sha, &rev_path, &push.role, epoch).await?;
            Ok(report(
                "deployed",
                push.revision,
                Some(epoch),
                None,
                Some("ok".into()),
            ))
        }
        Ok(Health::Down(detail)) => {
            let _ = restore_active_after_failure(pool, runtime, config_dir, "health_failed").await;
            Ok(report(
                "health_failed",
                push.revision,
                Some(epoch),
                Some(detail.clone()),
                Some(detail),
            ))
        }
        Err(error) => {
            let detail = error.to_string();
            let _ = restore_active_after_failure(pool, runtime, config_dir, "health_failed").await;
            Ok(report(
                "health_failed",
                push.revision,
                Some(epoch),
                Some(detail.clone()),
                Some(detail),
            ))
        }
    }
}

/// 启动指定的本地成功 revision；健康成功后才更新 active 和 epoch。
async fn start_revision(
    pool: &SqlitePool,
    runtime: &dyn Runtime,
    config_dir: &str,
    target: &state::LocalRevision,
) -> Result<std::result::Result<i64, String>> {
    let Some(path) = &target.config_path else {
        return Ok(Err(format!("revision {} 缺少配置路径", target.revision)));
    };
    let bytes = std::fs::read(path)
        .map_err(|e| AppError::new(ErrorCode::Deployment, format!("读旧快照失败: {e}")))?;
    let live = format!("{config_dir}/config.json");
    atomic_replace(&live, &bytes)?;
    let epoch = state::next_epoch(pool).await?;
    if let Err(error) = runtime.restart(&live, epoch).await {
        return Ok(Err(error.to_string()));
    }
    let role = target.role.as_deref().unwrap_or("");
    match runtime.health_check(role).await {
        Ok(Health::Ok) => {
            state::activate(pool, target.revision, epoch).await?;
            Ok(Ok(epoch))
        }
        Ok(Health::Down(detail)) => Ok(Err(detail)),
        Err(error) => Ok(Err(error.to_string())),
    }
}

/// 新配置失败后恢复数据库仍记录的 active；首次部署没有旧版本时停止失败进程。
pub(crate) async fn restore_active_after_failure(
    pool: &SqlitePool,
    runtime: &dyn Runtime,
    config_dir: &str,
    reason: &str,
) -> Result<DeployReport> {
    let Some(current) = state::active(pool).await? else {
        let _ = runtime.stop().await;
        return Ok(report(
            "rollback_no_prev",
            0,
            None,
            Some(format!("无可恢复的活动 revision ({reason})")),
            None,
        ));
    };
    match start_revision(pool, runtime, config_dir, &current).await? {
        Ok(epoch) => Ok(report(
            "rolled_back",
            current.revision,
            Some(epoch),
            Some(format!("restored {} ({reason})", current.revision)),
            Some("ok".into()),
        )),
        Err(detail) => Ok(report(
            "rollback_failed",
            current.revision,
            None,
            Some(format!("恢复 revision {} 失败: {detail}", current.revision)),
            Some(detail),
        )),
    }
}

/// 显式回滚到上一个成功 revision。失败时尽力恢复原 active。
pub async fn rollback(
    pool: &SqlitePool,
    runtime: &dyn Runtime,
    config_dir: &str,
    reason: &str,
) -> Result<DeployReport> {
    let original = state::active(pool).await?;
    let prev = match state::prev_succeeded(pool).await? {
        Some(p) => p,
        None => {
            return Ok(report(
                "rollback_no_prev",
                0,
                None,
                Some("无可回滚的历史 revision".into()),
                None,
            ))
        }
    };
    match start_revision(pool, runtime, config_dir, &prev).await? {
        Ok(epoch) => Ok(report(
            "rolled_back",
            prev.revision,
            Some(epoch),
            Some(format!("rolled back to {} ({reason})", prev.revision)),
            Some("ok".into()),
        )),
        Err(detail) => {
            if let Some(original) = original {
                let _ = start_revision(pool, runtime, config_dir, &original).await;
            } else {
                let _ = runtime.stop().await;
            }
            Ok(report(
                "rollback_failed",
                prev.revision,
                None,
                Some(format!("回滚 revision {} 失败: {detail}", prev.revision)),
                Some(detail),
            ))
        }
    }
}

/// 原子替换 live 配置：同目录写 .tmp（0600）→ rename（原子，无半写）。
pub(crate) fn atomic_replace(live: &str, bytes: &[u8]) -> Result<()> {
    let tmp = format!("{live}.tmp");
    write_private(&tmp, bytes)?;
    std::fs::rename(&tmp, live)
        .map_err(|e| AppError::new(ErrorCode::Deployment, format!("原子替换失败: {e}")))?;
    Ok(())
}

pub(crate) fn write_private(path: &str, bytes: &[u8]) -> Result<()> {
    let mut opts = std::fs::OpenOptions::new();
    opts.write(true).create(true).truncate(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        opts.mode(0o600);
    }
    let mut f = opts
        .open(path)
        .map_err(|e| AppError::new(ErrorCode::Deployment, format!("写配置失败: {e}")))?;
    f.write_all(bytes)
        .map_err(|e| AppError::new(ErrorCode::Deployment, format!("写配置失败: {e}")))?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::agent::gate::MockDrainGate;
    use crate::agent::runtime::MockRuntime;
    use crate::agent::ssm::MockSsmClient;
    use crate::agent::state as astate;
    use crate::compiler::check;
    use base64::{engine::general_purpose::STANDARD, Engine};
    use serde_json::json;

    // 常规部署测试的默认依赖（barrier_required=false 时不触达）。
    fn ssm() -> MockSsmClient {
        MockSsmClient::default()
    }
    fn gate() -> MockDrainGate {
        MockDrainGate { clean: true }
    }

    fn valid_config(method: &str) -> serde_json::Value {
        let psk = STANDARD.encode([7u8; 16]);
        json!({
            "log": {"level": "warn"},
            "dns": {"servers": [{"tag": "b", "type": "udp", "server": "1.1.1.1"}], "final": "b"},
            "inbounds": [{"type": "shadowsocks", "tag": "in-relay", "listen": "127.0.0.1",
                "listen_port": 29736, "method": method, "password": psk}],
            "outbounds": [{"type": "direct", "tag": "direct"}],
            "route": {"rules": [{"action": "sniff"}], "final": "direct"},
        })
    }
    async fn setup() -> (SqlitePool, String) {
        let dir = std::env::temp_dir().join(format!("sbm-dep-{}", uuid::Uuid::new_v4()));
        std::fs::create_dir_all(&dir).unwrap();
        let pool = astate::open(&dir.join("agent.db").to_string_lossy())
            .await
            .unwrap();
        (pool, dir.to_string_lossy().into_owned())
    }
    fn push(cfg: &serde_json::Value, rev: i64) -> DeployPush {
        DeployPush {
            revision: rev,
            content_sha256: content_sha256(cfg),
            config: cfg.clone(),
            role: "node".into(),
            barrier_required: false,
            entry_id: None,
        }
    }

    #[tokio::test]
    async fn deploy_happy_path_writes_live_and_records_revision() {
        if !check::available() {
            eprintln!("skip: sing-box 不可用");
            return;
        }
        let (pool, dir) = setup().await;
        let rt = MockRuntime::default();
        rt.push_health(Health::Ok);
        let cfg = valid_config("2022-blake3-aes-128-gcm");
        let rep = execute_deploy(&pool, &rt, &ssm(), &gate(), &dir, &push(&cfg, 5), "c1")
            .await
            .unwrap();
        assert_eq!(rep.status, "deployed");
        assert_eq!(rep.revision, 5);
        // live 配置写入，active revision=5。
        assert!(std::path::Path::new(&format!("{dir}/config.json")).exists());
        assert_eq!(astate::active_revision(&pool).await.unwrap(), Some(5));
        // restart 在 health 之前。
        let calls = rt.call_log();
        assert!(calls[0].starts_with("restart"));
        assert_eq!(calls[1], "health:node");
        pool.close().await;
    }

    #[tokio::test]
    async fn sha_mismatch_rejected_without_writing() {
        let (pool, dir) = setup().await;
        let rt = MockRuntime::default();
        let cfg = valid_config("2022-blake3-aes-128-gcm");
        let mut p = push(&cfg, 1);
        p.content_sha256 = "deadbeef".into();
        let rep = execute_deploy(&pool, &rt, &ssm(), &gate(), &dir, &p, "c1")
            .await
            .unwrap();
        assert_eq!(rep.status, "sha_mismatch");
        assert!(!std::path::Path::new(&format!("{dir}/config.json")).exists());
        assert!(rt.call_log().is_empty(), "不应 restart");
        pool.close().await;
    }

    #[tokio::test]
    async fn check_failure_preserves_old_revision() {
        if !check::available() {
            eprintln!("skip");
            return;
        }
        let (pool, dir) = setup().await;
        let rt = MockRuntime::default();
        rt.push_health(Health::Ok);
        // 先成功部署 rev 5。
        let good = valid_config("2022-blake3-aes-128-gcm");
        execute_deploy(&pool, &rt, &ssm(), &gate(), &dir, &push(&good, 5), "c1")
            .await
            .unwrap();
        // 再部署非法 method（check 失败）→ 保留 rev 5。
        let bad = valid_config("not-a-real-method");
        let rep = execute_deploy(&pool, &rt, &ssm(), &gate(), &dir, &push(&bad, 6), "c2")
            .await
            .unwrap();
        assert_eq!(rep.status, "check_failed");
        assert_eq!(
            astate::active_revision(&pool).await.unwrap(),
            Some(5),
            "保留旧 revision"
        );
        pool.close().await;
    }

    #[tokio::test]
    async fn health_failure_auto_rolls_back_to_prev() {
        if !check::available() {
            eprintln!("skip");
            return;
        }
        let (pool, dir) = setup().await;
        let rt = MockRuntime::default();
        // rev 5 成功。
        rt.push_health(Health::Ok);
        execute_deploy(
            &pool,
            &rt,
            &ssm(),
            &gate(),
            &dir,
            &push(&valid_config("2022-blake3-aes-128-gcm"), 5),
            "c1",
        )
        .await
        .unwrap();
        // rev 6 健康失败 → 自动回滚到 5。
        rt.push_health(Health::Down("ssm 不可达".into()));
        let rep = execute_deploy(
            &pool,
            &rt,
            &ssm(),
            &gate(),
            &dir,
            &push(&valid_config("2022-blake3-aes-128-gcm"), 6),
            "c2",
        )
        .await
        .unwrap();
        assert_eq!(rep.status, "health_failed");
        assert_eq!(
            astate::active_revision(&pool).await.unwrap(),
            Some(5),
            "回滚到上一个成功 revision"
        );
        assert!(
            astate::current_epoch(&pool).await.unwrap().unwrap() > 1,
            "恢复旧 revision 必须分配新 epoch"
        );
        pool.close().await;
    }

    #[tokio::test]
    async fn first_deploy_restart_failure_keeps_no_active_revision() {
        if !check::available() {
            eprintln!("skip");
            return;
        }
        let (pool, dir) = setup().await;
        let rt = MockRuntime::default();
        rt.push_restart(false);
        let rep = execute_deploy(
            &pool,
            &rt,
            &ssm(),
            &gate(),
            &dir,
            &push(&valid_config("2022-blake3-aes-128-gcm"), 1),
            "c1",
        )
        .await
        .unwrap();
        assert_eq!(rep.status, "rollback_no_prev");
        assert_eq!(astate::active_revision(&pool).await.unwrap(), None);
        assert_eq!(rt.call_log().last().map(String::as_str), Some("stop"));
        pool.close().await;
    }

    #[tokio::test]
    async fn first_deploy_health_failure_stops_and_keeps_no_active_revision() {
        if !check::available() {
            eprintln!("skip");
            return;
        }
        let (pool, dir) = setup().await;
        let rt = MockRuntime::default();
        rt.push_health(Health::Down("not ready".into()));
        let rep = execute_deploy(
            &pool,
            &rt,
            &ssm(),
            &gate(),
            &dir,
            &push(&valid_config("2022-blake3-aes-128-gcm"), 1),
            "c1",
        )
        .await
        .unwrap();
        assert_eq!(rep.status, "health_failed");
        assert_eq!(astate::active_revision(&pool).await.unwrap(), None);
        assert_eq!(rt.call_log().last().map(String::as_str), Some("stop"));
        pool.close().await;
    }
}
