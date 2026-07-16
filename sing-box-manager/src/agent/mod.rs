//! Agent 被动服务：加载 enrollment + 本地配置 → 建 mTLS ServerConfig → 绑 39736 → 加固 accept 循环。
//! **绝无任何主动连 Manager 的代码路径**；只监听、只访问本机回环 SSM。

pub mod accept;
pub mod api;
pub mod barrier_store;
pub mod config;
pub mod deploy;
pub mod gate;
pub mod idempotency;
pub mod reconcile;
pub mod runtime;
pub mod settle;
pub mod singbox;
pub mod ssm;
pub mod state;
pub mod stats;
pub mod tls;

use std::sync::Arc;
use std::time::Duration;

use tokio_util::sync::CancellationToken;

use self::runtime::{Health, Runtime};
use crate::pki::enrollment::EnrollmentPackage;
use crate::store::now_unix;

const MAX_CONNS: usize = 256;
const HANDSHAKE_TIMEOUT: Duration = Duration::from_secs(10);

/// Agent 主入口。
pub async fn run() -> anyhow::Result<()> {
    let cfg = config::AgentConfig::from_env()?;
    let pkg_str = std::fs::read_to_string(&cfg.enrollment_path)
        .map_err(|e| anyhow::anyhow!("读取 enrollment 包 {} 失败: {e}", cfg.enrollment_path))?;
    let pkg = EnrollmentPackage::parse(&pkg_str)?;

    // fail-fast：证书过期立即拒绝启动，不静默降级。
    if pkg.not_after <= now_unix() {
        anyhow::bail!("enrollment 证书已过期（not_after={}）", pkg.not_after);
    }

    let state_pool = state::open(&cfg.state_path).await?;
    let tls_config = Arc::new(tls::server_config(&pkg)?);
    let process_runtime = Arc::new(runtime::ProcessRuntime::new(cfg.ssm_address.clone()));
    restore_active_runtime(&state_pool, process_runtime.as_ref(), &cfg.config_dir).await?;
    let runtime: Arc<dyn runtime::Runtime> = process_runtime;
    let ssm: Arc<dyn ssm::SsmClient> = Arc::new(ssm::HttpSsmClient::new(&cfg.ssm_address));
    let gate: Arc<dyn gate::DrainWaitGate> = Arc::new(gate::SsmDrainGate::default());
    let router = api::router(api::AgentState {
        host_id: pkg.host_id.clone(),
        agent_version: env!("CARGO_PKG_VERSION").to_string(),
        state: state_pool.clone(),
        ssm_address: cfg.ssm_address.clone(),
        runtime,
        config_dir: cfg.config_dir.clone(),
        ssm,
        gate,
    });

    let cancel = CancellationToken::new();
    {
        let cancel = cancel.clone();
        tokio::spawn(async move {
            #[cfg(unix)]
            {
                use tokio::signal::unix::{signal, SignalKind};
                let mut term = signal(SignalKind::terminate()).expect("SIGTERM handler");
                tokio::select! {
                    _ = tokio::signal::ctrl_c() => {}
                    _ = term.recv() => {}
                }
            }
            #[cfg(not(unix))]
            {
                let _ = tokio::signal::ctrl_c().await;
            }
            cancel.cancel();
        });
    }

    tracing::info!(bind = %cfg.bind_address, host = %pkg.host_id, "agent 启动");
    accept::serve(
        &cfg.bind_address,
        tls_config,
        router,
        MAX_CONNS,
        HANDSHAKE_TIMEOUT,
        cancel,
    )
    .await?;
    state_pool.close().await;
    tracing::info!("agent 已停止");
    Ok(())
}

async fn restore_active_runtime(
    pool: &sqlx::SqlitePool,
    runtime: &dyn Runtime,
    config_dir: &str,
) -> anyhow::Result<()> {
    let Some(revision) = state::active_revision(pool).await? else {
        return Ok(());
    };
    let epoch = state::current_epoch(pool)
        .await?
        .ok_or_else(|| anyhow::anyhow!("active revision {revision} 缺少 runtime epoch"))?;
    let live = format!("{config_dir}/config.json");
    if !std::path::Path::new(&live).is_file() {
        anyhow::bail!("active revision {revision} 的 live 配置不存在: {live}");
    }

    runtime.restart(&live, epoch).await?;
    match runtime.health_check().await? {
        Health::Ok => {
            tracing::info!(revision, epoch, "已恢复 active sing-box 运行态");
            Ok(())
        }
        Health::Down(detail) => {
            anyhow::bail!("恢复 active revision {revision} 失败: {detail}")
        }
    }
}

#[cfg(test)]
mod startup_tests {
    use super::*;
    use crate::agent::runtime::MockRuntime;

    #[tokio::test]
    async fn restores_active_revision_before_serving() {
        let dir = std::env::temp_dir().join(format!("sbm-agent-restore-{}", uuid::Uuid::new_v4()));
        std::fs::create_dir_all(&dir).unwrap();
        let live = dir.join("config.json");
        std::fs::write(&live, b"{}").unwrap();
        let db = dir.join("agent.db");
        let pool = state::open(&db.to_string_lossy()).await.unwrap();
        state::record_applied(&pool, 7, "sha", &live.to_string_lossy(), "entry", 11)
            .await
            .unwrap();

        let runtime = MockRuntime::default();
        runtime.push_health(Health::Ok);
        restore_active_runtime(&pool, &runtime, &dir.to_string_lossy())
            .await
            .unwrap();

        assert_eq!(
            runtime.call_log(),
            vec![
                format!("restart:epoch=11:{}", live.to_string_lossy()),
                "health".into(),
            ]
        );
        pool.close().await;
        let _ = std::fs::remove_dir_all(dir);
    }
}

#[cfg(test)]
mod e2e {
    //! 真机 e2e：起 Agent + Manager mTLS 客户端做真实 loopback 握手。沙箱内 loopback TLS 不稳，
    //! 故标 `#[ignore]`；真机运行 `cargo test -- --ignored`。
    use super::*;
    use crate::manager::agent_client::build_client_config;
    use crate::pki::enrollment::ENROLLMENT_VERSION;
    use crate::pki::verify::PinnedServerVerifier;
    use crate::pki::{
        generate_ca, install_ring_default, issue_agent_server_cert, issue_manager_client_cert, Ca,
        CaRole, SanEntry,
    };
    use std::net::{IpAddr, Ipv4Addr};

    fn server_only_config(agent_ca_pem: &str, host_id: &str) -> rustls::ClientConfig {
        let der = crate::pki::one_cert_from_pem(agent_ca_pem).unwrap();
        let verifier = PinnedServerVerifier::new(der, host_id.into()).unwrap();
        rustls::ClientConfig::builder_with_provider(Arc::new(
            rustls::crypto::ring::default_provider(),
        ))
        .with_safe_default_protocol_versions()
        .unwrap()
        .dangerous()
        .with_custom_certificate_verifier(verifier)
        .with_no_client_auth()
    }

    #[tokio::test]
    #[ignore = "真机运行：沙箱 loopback TLS 握手不稳"]
    async fn loopback_mtls_status_ok_and_no_client_cert_rejected() {
        install_ring_default();
        let addr = "127.0.0.1:39811";
        let agent_ca = generate_ca(CaRole::AgentCa, 3650).unwrap();
        let client_ca = generate_ca(CaRole::ClientCa, 3650).unwrap();
        let a = Ca::from_pem(&agent_ca.cert_pem, &agent_ca.key_pem).unwrap();
        let c = Ca::from_pem(&client_ca.cert_pem, &client_ca.key_pem).unwrap();
        let leaf = issue_agent_server_cert(
            &a,
            "h1",
            &[SanEntry::Ip(IpAddr::V4(Ipv4Addr::LOCALHOST))],
            2,
            825,
        )
        .unwrap();
        let mc = issue_manager_client_cert(&c, 3, 825).unwrap();
        let pkg = EnrollmentPackage {
            version: ENROLLMENT_VERSION,
            host_id: "h1".into(),
            mgmt_bind: addr.into(),
            agent_server_cert_pem: leaf.cert_pem.clone(),
            agent_server_key_pem: leaf.key_pem.clone(),
            client_ca_cert_pem: client_ca.cert_pem.clone(),
            manager_client_spki_sha256: mc.spki_sha256.clone(),
            issued_at: 0,
            not_after: leaf.not_after,
        };
        let tls = Arc::new(tls::server_config(&pkg).unwrap());
        let statedb = std::env::temp_dir().join(format!("e2e-{}.db", uuid::Uuid::new_v4()));
        let state_pool = state::open(&statedb.to_string_lossy()).await.unwrap();
        let router = api::router(api::AgentState {
            host_id: "h1".into(),
            agent_version: "t".into(),
            state: state_pool,
            ssm_address: "127.0.0.1:1".into(),
            runtime: Arc::new(runtime::MockRuntime::default()),
            config_dir: std::env::temp_dir()
                .join(format!("e2e-cfg-{}", uuid::Uuid::new_v4()))
                .to_string_lossy()
                .into_owned(),
            ssm: Arc::new(ssm::MockSsmClient::default()),
            gate: Arc::new(gate::SsmDrainGate::default()),
        });
        let cancel = CancellationToken::new();
        {
            let (tls, router, cancel) = (tls.clone(), router.clone(), cancel.clone());
            tokio::spawn(async move {
                let _ = accept::serve(addr, tls, router, 64, Duration::from_secs(5), cancel).await;
            });
        }
        tokio::time::sleep(Duration::from_millis(300)).await;

        // 有 Manager 客户端证书 → 200。
        let cfg = build_client_config(&agent_ca.cert_pem, &mc.cert_pem, &mc.key_pem, "h1").unwrap();
        let client = reqwest::Client::builder()
            .use_preconfigured_tls(cfg)
            .build()
            .unwrap();
        let resp = client
            .get(format!("https://{addr}/v1/status"))
            .send()
            .await
            .expect("mTLS 请求应成功");
        assert_eq!(resp.status(), 200);

        // 无客户端证书 → 握手被 Agent 拒绝。
        let cfg2 = server_only_config(&agent_ca.cert_pem, "h1");
        let client2 = reqwest::Client::builder()
            .use_preconfigured_tls(cfg2)
            .build()
            .unwrap();
        let r2 = client2
            .get(format!("https://{addr}/v1/status"))
            .send()
            .await;
        assert!(r2.is_err(), "缺客户端证书应被 mTLS 拒绝");

        cancel.cancel();
        let _ = std::fs::remove_file(&statedb);
    }
}
