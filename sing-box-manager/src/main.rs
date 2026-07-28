//! sing-box-manager — 配置驱动的 sing-box 多跳中继控制器。
//!
//! 单二进制的主要模式：
//!   - `plan/apply/status`：校验、同步、查看声明式配置。
//!   - `controller`：无管理 API 的后台控制器。
//!   - `agent`：被动模式主机代理（只监听、不主动连 Manager；Phase 1 实现）。

mod config;
mod crypto;
mod error;
mod manifest;
// 跨阶段 API：这些模块的部分成员（命令创建、订阅、部分枚举/DTO、Phase 5 计量/epoch 预留）在后续 Phase 才接线，
// 当前已被单测覆盖但尚未被非测试代码全部调用；含 Mock/TestClock/MockRuntime 等测试替身。
#[allow(dead_code)]
mod agent;
#[allow(dead_code)]
mod compiler;
#[allow(dead_code)]
mod domain;
#[allow(dead_code)]
mod manager;
#[allow(dead_code)]
mod pki;
#[allow(dead_code)]
mod store;
#[allow(dead_code)]
mod subscription;

use clap::{Parser, Subcommand};
use std::path::PathBuf;

#[derive(Parser)]
#[command(
    name = "sing-box-manager",
    version,
    about = "配置驱动的 sing-box 多跳中继控制器"
)]
struct Cli {
    #[command(subcommand)]
    mode: Mode,
}

#[derive(Subcommand)]
enum Mode {
    /// 校验并展示声明式配置将管理的资源。
    Plan {
        #[arg(short, long, default_value = "config/config.toml")]
        config: PathBuf,
        #[arg(long)]
        json: bool,
    },
    /// 把声明式配置幂等同步到加密状态库。
    Apply {
        #[arg(short, long, default_value = "config/config.toml")]
        config: PathBuf,
        #[arg(long)]
        json: bool,
        /// 同步后编译、sing-box check 并经 mTLS 部署（当前仅 Shadowsocks）。
        #[arg(long)]
        deploy: bool,
    },
    /// 查看状态库中的拓扑、用户和 Agent 概况。
    Status {
        #[arg(long)]
        json: bool,
    },
    /// 启动无管理 API 的后台控制器。
    Controller {
        #[arg(short, long, default_value = "config/config.toml")]
        config: PathBuf,
    },
    /// 旧命令兼容别名；等同 controller。
    #[command(hide = true)]
    Server {
        #[arg(short, long, default_value = "config/config.toml")]
        config: PathBuf,
    },
    /// 启动主机 Agent（被动 mTLS）。
    Agent,
    /// Agent enrollment 文件签发与带外授信。
    Enrollment {
        #[command(subcommand)]
        action: EnrollmentAction,
    },
    /// 主密钥轮换工具（离线；需 DATABASE_PATH + ENCRYPTION_MASTER_KEY[_V*] + ENCRYPTION_MASTER_KEY_VERSION）。
    KeyRotation {
        #[command(subcommand)]
        action: KeyRotationAction,
    },
}

#[derive(Subcommand)]
enum KeyRotationAction {
    /// 打印各表待迁移密文数与是否可退休旧密钥版本。
    Status,
    /// 把库内全部信封密文 re-seal 到当前主密钥版本（幂等、可续跑）。
    Run,
}

#[derive(Subcommand)]
enum EnrollmentAction {
    /// 为清单中的服务器签发 enrollment 文件；拒绝覆盖已有文件。
    Issue {
        #[arg(short, long, default_value = "config/config.toml")]
        config: PathBuf,
        #[arg(long)]
        server: String,
        #[arg(short, long)]
        output: PathBuf,
    },
    /// 核对 issue 输出的指纹后，把 Agent 标记为 trusted。
    Trust {
        #[arg(long)]
        server: String,
        #[arg(long)]
        fingerprint: String,
    },
    /// 吊销某服务器当前 Agent 证书的信任。
    Revoke {
        #[arg(long)]
        server: String,
    },
}

fn init_tracing() {
    use tracing_subscriber::{fmt, prelude::*, EnvFilter};
    let filter =
        EnvFilter::try_from_default_env().unwrap_or_else(|_| EnvFilter::new("info,sqlx=warn"));
    tracing_subscriber::registry()
        .with(filter)
        .with(fmt::layer().with_target(false))
        .init();
}

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    init_tracing();
    // 全栈单一 ring CryptoProvider（server/agent 两模式都需要）。
    pki::install_ring_default();
    match Cli::parse().mode {
        Mode::Plan { config, json } => plan_main(config, json).await,
        Mode::Apply {
            config,
            json,
            deploy,
        } => apply_main(config, json, deploy).await,
        Mode::Status { json } => status_main(json).await,
        Mode::Controller { config } | Mode::Server { config } => controller_main(config).await,
        Mode::Agent => agent_main().await,
        Mode::Enrollment { action } => enrollment_main(action).await,
        Mode::KeyRotation { action } => key_rotation_main(action).await,
    }
}

async fn plan_main(path: PathBuf, json: bool) -> anyhow::Result<()> {
    let manifest = manifest::Manifest::load(&path)?;
    let plan = manifest.plan(&path);
    if json {
        println!("{}", serde_json::to_string_pretty(&plan)?);
    } else {
        println!("配置有效：{}", plan.config_path);
        println!(
            "  servers={} listeners={} nodes={} relays={} users={} grants={}",
            plan.servers, plan.listeners, plan.nodes, plan.relays, plan.users, plan.grants
        );
        println!("  protocols={}", plan.protocols.join(","));
        for warning in plan.warnings {
            println!("  警告：{warning}");
        }
    }
    Ok(())
}

async fn apply_main(path: PathBuf, json: bool, deploy: bool) -> anyhow::Result<()> {
    let manifest = manifest::Manifest::load(&path)?;
    let cfg = config::StartupConfig::from_env()?;
    let cipher = crypto::Cipher::from_env_ring()?;
    let pool = store::open(&cfg.database_path).await?;
    if deploy {
        let mut new_users = Vec::new();
        for user in &manifest.users {
            let exists: i64 = sqlx::query_scalar("SELECT COUNT(*) FROM users WHERE name=?")
                .bind(&user.name)
                .fetch_one(&pool)
                .await?;
            if exists == 0 {
                new_users.push(user.name.clone());
            }
        }
        if !new_users.is_empty() {
            anyhow::bail!(
                "--deploy 不用于首次建用户，否则部署门禁失败时会丢失一次性 token；请先运行普通 apply。待创建用户: {}",
                new_users.join(",")
            );
        }
    }
    let mut report = manifest::apply::apply(&pool, &cipher, &manifest).await?;
    if deploy {
        deploy_manifest(&pool, &cipher, &manifest, &mut report).await?;
    }
    pool.close().await;

    if json {
        println!("{}", serde_json::to_string_pretty(&report)?);
    } else {
        println!("声明式配置已同步：{}", path.display());
        println!(
            "  hosts +{}/~{}  entries +{}/~{}  nodes +{}/~{}",
            report.hosts_created,
            report.hosts_updated,
            report.entries_created,
            report.entries_updated,
            report.nodes_created,
            report.nodes_updated
        );
        println!(
            "  routes +{}/~{}  users +{}/~{}  grants +{}/-{}",
            report.routes_created,
            report.routes_updated,
            report.users_created,
            report.users_updated,
            report.grants_created,
            report.grants_revoked
        );
        for item in report.subscription_tokens {
            println!("  新用户 {} 的一次性订阅 token：{}", item.user, item.token);
        }
        if deploy {
            println!(
                "  checked revisions={}  succeeded deployments={}",
                report.revisions_checked.len(),
                report.deployments_succeeded.len()
            );
        } else {
            println!("注意：本次只同步期望状态；需要发布时显式增加 --deploy。");
        }
    }
    Ok(())
}

async fn deploy_manifest(
    pool: &sqlx::SqlitePool,
    cipher: &crypto::Cipher,
    desired: &manifest::Manifest,
    report: &mut manifest::apply::ApplyReport,
) -> anyhow::Result<()> {
    manager::pki_ops::bootstrap(pool, cipher).await?;
    let mut checked = Vec::new();
    let mut target_hosts = std::collections::BTreeSet::new();

    // 所有 Entry 先完成编译、真实 check 和门禁，再开始任何远端部署。
    for entry_id in &report.entry_ids {
        let revision = store::revisions::compile_and_persist(
            pool,
            cipher,
            entry_id,
            desired.singbox_version.as_deref(),
            Some("cli"),
        )
        .await?;
        let artifacts = store::revisions::run_check(pool, cipher, &revision.id).await?;
        let revision = store::revisions::get_revision(pool, &revision.id)
            .await?
            .ok_or_else(|| anyhow::anyhow!("check 后 revision 不可读"))?;
        if revision.status != "checked" {
            anyhow::bail!("revision {} 未通过 sing-box check", revision.id);
        }
        for artifact in artifacts {
            target_hosts.insert(artifact.host_id);
        }
        checked.push(revision.id);
    }

    let target_hosts = target_hosts.into_iter().collect::<Vec<_>>();
    let blocked = manager::gate::preflight(
        pool,
        &target_hosts,
        store::now_unix(),
        manager::gate::DEFAULT_FRESHNESS_SECS,
    )
    .await?;
    if !blocked.is_empty() {
        anyhow::bail!(
            "部署门禁未通过: {}",
            serde_json::to_string(&blocked).unwrap_or_default()
        );
    }

    let client = manager::build_agent_client(pool, cipher).await?;
    for revision_id in &checked {
        let deployment =
            manager::deploy::create_deployment(pool, revision_id, "normal", Some("cli")).await?;
        manager::deploy::drive(pool, cipher, client.as_ref(), &deployment).await?;
        let state = store::deployments::get_deployment(pool, &deployment)
            .await?
            .ok_or_else(|| anyhow::anyhow!("部署完成后状态不可读"))?;
        if state.status != "succeeded" {
            anyhow::bail!(
                "deployment {} 未成功（{}）：{}",
                deployment,
                state.status,
                state.error_summary.unwrap_or_default()
            );
        }
        report.deployments_succeeded.push(deployment);
    }
    report.revisions_checked = checked;
    Ok(())
}

async fn enrollment_main(action: EnrollmentAction) -> anyhow::Result<()> {
    use crate::domain::agent::TrustStatus;

    let cfg = config::StartupConfig::from_env()?;
    let cipher = crypto::Cipher::from_env_ring()?;
    let pool = store::open(&cfg.database_path).await?;
    match action {
        EnrollmentAction::Issue {
            config,
            server,
            output,
        } => {
            if output.try_exists()? {
                anyhow::bail!("输出文件已存在，拒绝覆盖：{}", output.display());
            }
            let desired = manifest::Manifest::load(config)?;
            let spec = desired
                .servers
                .get(&server)
                .ok_or_else(|| anyhow::anyhow!("清单中不存在 server {server}"))?;
            let host_id = host_id_by_name(&pool, &server).await?;
            manager::pki_ops::bootstrap(&pool, &cipher).await?;
            let mgmt = manifest::apply::host_port(&spec.address, 39736);
            let issued =
                manager::pki_ops::build_enrollment(&pool, &cipher, &host_id, &mgmt).await?;
            write_private_file(&output, issued.package.to_json()?.as_bytes())?;
            sqlx::query(
                "UPDATE enrollment_packages SET delivered=1 WHERE id=(
                    SELECT id FROM enrollment_packages WHERE host_id=? ORDER BY serial DESC LIMIT 1
                 )",
            )
            .bind(&host_id)
            .execute(&pool)
            .await?;
            println!("enrollment 已写入 {}", output.display());
            println!("请带外核对 fingerprint：{}", issued.fingerprint);
            println!(
                "核对无误后执行：sing-box-manager enrollment trust --server {} --fingerprint {}",
                server, issued.fingerprint
            );
        }
        EnrollmentAction::Trust {
            server,
            fingerprint,
        } => {
            let host_id = host_id_by_name(&pool, &server).await?;
            let expected = sqlx::query_scalar::<_, String>(
                "SELECT package_fp_sha256 FROM enrollment_packages WHERE host_id=?
                 ORDER BY serial DESC LIMIT 1",
            )
            .bind(&host_id)
            .fetch_optional(&pool)
            .await?
            .ok_or_else(|| anyhow::anyhow!("server {server} 尚未签发 enrollment"))?;
            if expected != fingerprint {
                anyhow::bail!("fingerprint 不匹配，拒绝授信");
            }
            store::pki::set_trust(&pool, &host_id, TrustStatus::Trusted).await?;
            println!("server {server} 已标记为 trusted");
        }
        EnrollmentAction::Revoke { server } => {
            let host_id = host_id_by_name(&pool, &server).await?;
            store::pki::set_trust(&pool, &host_id, TrustStatus::Revoked).await?;
            println!("server {server} 的 Agent 证书已吊销");
        }
    }
    pool.close().await;
    Ok(())
}

async fn host_id_by_name(pool: &sqlx::SqlitePool, name: &str) -> anyhow::Result<String> {
    sqlx::query_scalar::<_, String>("SELECT id FROM hosts WHERE name=?")
        .bind(name)
        .fetch_optional(pool)
        .await?
        .ok_or_else(|| anyhow::anyhow!("状态库中不存在 server {name}；请先 apply"))
}

fn write_private_file(path: &std::path::Path, bytes: &[u8]) -> anyhow::Result<()> {
    use std::io::Write;
    let mut options = std::fs::OpenOptions::new();
    options.write(true).create_new(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        options.mode(0o600);
    }
    let mut file = options.open(path)?;
    file.write_all(bytes)?;
    file.sync_all()?;
    Ok(())
}

async fn status_main(json: bool) -> anyhow::Result<()> {
    let db = std::env::var("DATABASE_PATH").map_err(|_| anyhow::anyhow!("缺少 DATABASE_PATH"))?;
    let pool = store::open(&db).await?;
    let hosts = store::hosts::list_hosts(&pool).await?;
    let entries = store::topology::list_entries(&pool).await?;
    let nodes = store::topology::list_nodes(&pool).await?;
    let routes = store::topology::list_routes(&pool).await?;
    let users = store::users::list_users(&pool).await?;
    let agents = store::agents::list_agents(&pool).await?;
    if json {
        println!(
            "{}",
            serde_json::to_string_pretty(&serde_json::json!({
                "hosts": hosts,
                "entries": entries,
                "nodes": nodes,
                "routes": routes,
                "users": users,
                "agents": agents,
            }))?
        );
    } else {
        println!(
            "hosts={} entries={} nodes={} routes={} users={} agents={}",
            hosts.len(),
            entries.len(),
            nodes.len(),
            routes.len(),
            users.len(),
            agents.len()
        );
        for route in routes {
            println!("  route {:<24} {}", route.label, route.status);
        }
        for agent in agents {
            println!("  agent {:<36} {}", agent.host_id, agent.status);
        }
    }
    pool.close().await;
    Ok(())
}

/// 主密钥轮换 CLI：离线运行（Manager 停机以保证单写者）。
async fn key_rotation_main(action: KeyRotationAction) -> anyhow::Result<()> {
    let db = std::env::var("DATABASE_PATH").map_err(|_| anyhow::anyhow!("缺少 DATABASE_PATH"))?;
    let cipher = crypto::Cipher::from_env_ring()?;
    let current = cipher.current_version();
    let pool = store::open(&db).await?;
    match action {
        KeyRotationAction::Status => {
            let pc = store::reencrypt::pending_counts(&pool, current).await?;
            println!("当前主密钥版本: {current}");
            let mut total = 0i64;
            for p in &pc {
                println!("  {:<22} 待迁移 {}", p.table, p.pending);
                total += p.pending;
            }
            if total == 0 {
                println!(
                    "全部密文已在版本 {current}：可安全从 env 退休旧 ENCRYPTION_MASTER_KEY_V*。"
                );
            } else {
                println!("尚有 {total} 条旧密文：请先运行 `key-rotation run`，勿删旧密钥。");
            }
        }
        KeyRotationAction::Run => {
            let rep = store::reencrypt::reseal_all(&pool, &cipher, None).await?;
            for (t, n) in &rep.per_table {
                println!("  {t:<22} re-seal {n}");
            }
            let _ = store::audit::record(
                &pool,
                Some("system"),
                "key.rotate.reseal",
                None,
                None,
                None,
                Some(&format!("target_version={current} total={}", rep.total)),
            )
            .await;
            let migrated = store::reencrypt::all_migrated(&pool, current).await?;
            println!(
                "完成：本次 re-seal {} 条；{}",
                rep.total,
                if migrated {
                    format!("全部已在版本 {current}，可退休旧密钥")
                } else {
                    "仍有残留，请重跑".into()
                }
            );
        }
    }
    pool.close().await;
    Ok(())
}

/// Controller：校验清单 → 打开状态库 → 起编排循环 → 只提供订阅/健康/指标 HTTP。
async fn controller_main(manifest_path: PathBuf) -> anyhow::Result<()> {
    let desired = manifest::Manifest::load(&manifest_path)?;
    let cfg = config::StartupConfig::from_env()?;
    // 及早校验主密钥可用（缺失/长度错立即失败，不静默降级）。Phase 6：多版本 ring（历史+当前）。
    let cipher = std::sync::Arc::new(crypto::Cipher::from_env_ring()?);
    let pool = store::open(&cfg.database_path).await?;
    // 首启幂等引导双 CA + Manager 客户端身份。
    manager::pki_ops::bootstrap(&pool, &cipher).await?;
    // Phase 6：/metrics 非回环暴露且未设 scrape token → 告警（审查 C）。
    let loopback = cfg.manager_listen.starts_with("127.")
        || cfg.manager_listen.starts_with("localhost")
        || cfg.manager_listen.starts_with("[::1]");
    if !loopback
        && store::settings::get_str(&pool, "metrics_scrape_token", "")
            .await?
            .is_empty()
    {
        tracing::warn!(listen = %cfg.manager_listen, "MANAGER_LISTEN 非回环且未设 metrics_scrape_token：/metrics 将无认证暴露，请设置 token 或置于反代之后");
    }
    tracing::info!(
        db = %cfg.database_path,
        listen = %cfg.manager_listen,
        config = %manifest_path.display(),
        relays = desired.relays.len(),
        users = desired.users.len(),
        "controller 启动"
    );

    let cancel = tokio_util::sync::CancellationToken::new();
    manager::spawn_background(pool.clone(), cipher.clone(), cancel.clone()).await?;

    // Phase 4：启动时声明式 SSM reconcile 扫描（重启后回填 active Entry 的用户身份）。
    {
        let (pool, cipher) = (pool.clone(), cipher.clone());
        tokio::spawn(async move {
            match manager::build_agent_client(&pool, &cipher).await {
                Ok(client) => {
                    if let Err(e) =
                        manager::reconcile::startup_sweep(&pool, &cipher, client.as_ref()).await
                    {
                        tracing::warn!(error = %e, "启动 reconcile 扫描失败");
                    }
                }
                Err(e) => tracing::warn!(error = %e, "构建 Agent 客户端失败，跳过启动扫描"),
            }
        });
    }

    let state = manager::http::AppState {
        pool: pool.clone(),
        cipher: cipher.clone(),
        freshness_secs: manager::gate::DEFAULT_FRESHNESS_SECS,
        auth: std::sync::Arc::new(cfg.auth.clone()),
        started_at: store::now_unix(),
    };
    let app = manager::http::controller_router(state);

    let listener = tokio::net::TcpListener::bind(&cfg.manager_listen).await?;
    let shutdown = {
        let cancel = cancel.clone();
        async move {
            let _ = tokio::signal::ctrl_c().await;
            tracing::info!("收到停机信号");
            cancel.cancel();
        }
    };
    axum::serve(listener, app)
        .with_graceful_shutdown(shutdown)
        .await?;
    cancel.cancel();
    pool.close().await;
    tracing::info!("controller 已停止");
    Ok(())
}

/// Agent：被动 mTLS 服务（端口 39736），加载 enrollment 包后监听 Manager 调用。
async fn agent_main() -> anyhow::Result<()> {
    agent::run().await
}
