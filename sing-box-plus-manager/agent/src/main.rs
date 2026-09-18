//! `proxy-manager-agent`：节点侧的薄进程（README §4.2）。
//!
//! 三条纪律（README §4.1）：
//! **主控绝不直连节点 UDS；agent 不解析快照 JSON；agent 不缓存快照。**
//!
//! 命令集是封闭枚举，共三条，没有 `exec`、没有 shell 模板、没有任意文件路径。
//! 封闭枚举是 agent 相对反代唯一增加的攻击面——反代只能解决「读快照」，
//! 配置分发、排空重启、审计拉取都做不了（D2）。
//!
//! agent 是**被动**的：不主动回连、不接受管理员提供的任意远端命令。
//! NAT 后的节点本轮不支持；若将来需要，走反向隧道而不是让 agent 主动推数据——
//! 推模式会把「谁是真相源」这件事翻过来，结算方向的幂等假设要整套重做。

use proxy_manager_agent::{commands, config, server};

use std::path::PathBuf;
use std::process::ExitCode;

#[tokio::main]
async fn main() -> ExitCode {
    tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env()
                .unwrap_or_else(|_| tracing_subscriber::EnvFilter::new("info")),
        )
        .init();

    match run().await {
        Ok(()) => ExitCode::SUCCESS,
        Err(error) => {
            // 启动失败要噪声大：C22 的对照组是「SIGHUP 超限静默置位」，
            // 那种「看起来一切正常」的失败贵得多。
            tracing::error!("启动失败：{error}");
            ExitCode::FAILURE
        }
    }
}

async fn run() -> Result<(), String> {
    let path = std::env::args_os()
        .nth(1)
        .map(PathBuf::from)
        .unwrap_or_else(|| PathBuf::from("/etc/proxy-manager-agent/agent.toml"));

    let config = config::AgentConfig::load(&path).map_err(|e| e.to_string())?;
    let materials = config.materials_dir.clone();

    let server_config =
        proxy_manager_wire::tls::server_config(&materials).map_err(|e| e.to_string())?;
    let hmac = proxy_manager_wire::tls::load_hmac_key(&materials.join("hmac.key"))
        .map_err(|e| e.to_string())?;

    let listener = tokio::net::TcpListener::bind(&config.listen)
        .await
        .map_err(|e| format!("监听 {} 失败：{e}", config.listen))?;
    let local = listener.local_addr().map_err(|e| e.to_string())?;
    // 日志里只记节点公开标识，不记密钥、不记身份明细（threat-model §5）。
    tracing::info!(node_id = %config.node_id, listen = %local, "agent 就绪");

    let executor = commands::Executor::new(config);
    let server = server::Server::new(server_config, executor, hmac);

    tokio::select! {
        _ = server.serve(listener) => {}
        _ = tokio::signal::ctrl_c() => tracing::info!("收到停止信号"),
    }
    Ok(())
}
