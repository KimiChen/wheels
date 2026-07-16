//! sing-box-manager — sing-box 多跳中继的 Web 管理平台。
//!
//! 单二进制两种模式：
//!   - `server`：Manager 控制面（SQLite 为真相源 + Web/API + 编排 Agent）。
//!   - `agent`：被动模式主机代理（只监听、不主动连 Manager；Phase 1 实现）。
//!
//! 平台化重写自 Phase 0 起，旧 CLI 计量/订阅实现暂置于 `_legacy/`，按 todo.md 分阶段迁回。

mod config;
mod crypto;
mod error;
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

#[derive(Parser)]
#[command(
    name = "sing-box-manager",
    version,
    about = "sing-box 多跳中继的 Web 管理平台"
)]
struct Cli {
    #[command(subcommand)]
    mode: Mode,
}

#[derive(Subcommand)]
enum Mode {
    /// 启动 Manager 控制面（Web/API + 编排）。
    Server,
    /// 启动主机 Agent（被动 mTLS，Phase 1 实现）。
    Agent,
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
        Mode::Server => server_main().await,
        Mode::Agent => agent_main().await,
        Mode::KeyRotation { action } => key_rotation_main(action).await,
    }
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

/// Manager：加载启动配置 → 校验主密钥 → 打开库并迁移 → 引导 PKI → 起后台轮询/派发 → 服务 Web/API。
async fn server_main() -> anyhow::Result<()> {
    let cfg = config::StartupConfig::from_env()?;
    // 及早校验主密钥可用（缺失/长度错立即失败，不静默降级）。Phase 6：多版本 ring（历史+当前）。
    let cipher = std::sync::Arc::new(crypto::Cipher::from_env_ring()?);
    let pool = store::open(&cfg.database_path).await?;
    // 首启幂等引导双 CA + Manager 客户端身份。
    manager::pki_ops::bootstrap(&pool, &cipher).await?;
    // Phase 6：首启管理员引导（仅当尚无管理员且提供了 ADMIN_BOOTSTRAP_USER/PASSWORD）。
    bootstrap_admin(&pool).await?;
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
    tracing::info!(db = %cfg.database_path, listen = %cfg.manager_listen, "manager 启动");

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
    let app = manager::http::router(state);

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
    tracing::info!("manager 已停止");
    Ok(())
}

/// 首启管理员引导：仅当尚无任何管理员且提供了 `ADMIN_BOOTSTRAP_USER`/`ADMIN_BOOTSTRAP_PASSWORD` 时创建
/// role=admin。否则暴露一次性 `POST /api/auth/setup`。密码明文只在 env，不落库（只存 Argon2id）。
async fn bootstrap_admin(pool: &sqlx::SqlitePool) -> anyhow::Result<()> {
    if store::admins::count(pool).await? > 0 {
        return Ok(());
    }
    let (user, pass) = match (
        std::env::var("ADMIN_BOOTSTRAP_USER"),
        std::env::var("ADMIN_BOOTSTRAP_PASSWORD"),
    ) {
        (Ok(u), Ok(p)) if !u.is_empty() && p.chars().count() >= 12 => (u, p),
        _ => {
            tracing::warn!("尚无管理员：请用 POST /api/auth/setup 创建首个管理员，或设 ADMIN_BOOTSTRAP_USER/PASSWORD");
            return Ok(());
        }
    };
    let hash = manager::auth::hash_password(&pass)
        .map_err(|e| anyhow::anyhow!("引导管理员密码哈希失败: {e}"))?;
    // 原子首建（与 /api/auth/setup 同一防竞态路径）。
    let Some(id) = store::admins::create_if_none(pool, &user, &hash, "admin").await? else {
        return Ok(());
    };
    let _ = store::audit::record(
        pool,
        Some("system"),
        "admin.bootstrap",
        Some("admin"),
        Some(&id),
        None,
        None,
    )
    .await;
    tracing::warn!(user = %user, "已从 env 引导首个管理员，请尽快登录改密");
    Ok(())
}

/// Agent：被动 mTLS 服务（端口 39736），加载 enrollment 包后监听 Manager 调用。
async fn agent_main() -> anyhow::Result<()> {
    agent::run().await
}
