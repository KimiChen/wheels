//! 加固 mTLS accept 循环：连接上限（背压）+ 每连接握手超时（抗 slowloris）+ per-conn 错误隔离
//! （单连接失败不影响 listener）+ 优雅停机。承载 [`axum::Router`] 于 tokio-rustls TLS 流之上。

use std::sync::Arc;
use std::time::Duration;

use axum::Router;
use hyper_util::rt::{TokioExecutor, TokioIo};
use hyper_util::server::conn::auto;
use hyper_util::service::TowerToHyperService;
use tokio::net::TcpListener;
use tokio::sync::Semaphore;
use tokio_rustls::TlsAcceptor;
use tokio_util::sync::CancellationToken;

use crate::error::{AppError, ErrorCode, Result};

pub async fn serve(
    bind_address: &str,
    tls_config: Arc<rustls::ServerConfig>,
    router: Router,
    max_conns: usize,
    handshake_timeout: Duration,
    cancel: CancellationToken,
) -> Result<()> {
    let listener = TcpListener::bind(bind_address)
        .await
        .map_err(|e| AppError::new(ErrorCode::Agent, format!("绑定 {bind_address} 失败: {e}")))?;
    let acceptor = TlsAcceptor::from(tls_config);
    let sem = Arc::new(Semaphore::new(max_conns));
    tracing::info!(bind = %bind_address, "agent 开始监听");

    loop {
        tokio::select! {
            _ = cancel.cancelled() => break,
            accepted = listener.accept() => {
                let (stream, _peer) = match accepted {
                    Ok(v) => v,
                    Err(e) => { tracing::warn!(error = %e, "accept 失败"); continue; }
                };
                // 连接上限：permit 满则暂缓 accept（背压）。
                let permit = match sem.clone().acquire_owned().await {
                    Ok(p) => p,
                    Err(_) => continue,
                };
                let acceptor = acceptor.clone();
                let router = router.clone();
                let ht = handshake_timeout;
                tokio::spawn(async move {
                    let _permit = permit; // 持有至连接结束
                    let tls = match tokio::time::timeout(ht, acceptor.accept(stream)).await {
                        Ok(Ok(s)) => s,
                        Ok(Err(e)) => { tracing::debug!(error = %e, "TLS 握手失败"); return; }
                        Err(_) => { tracing::debug!("TLS 握手超时"); return; }
                    };
                    let io = TokioIo::new(tls);
                    let svc = TowerToHyperService::new(router);
                    if let Err(e) = auto::Builder::new(TokioExecutor::new())
                        .serve_connection_with_upgrades(io, svc)
                        .await
                    {
                        tracing::debug!(error = %e, "连接结束");
                    }
                });
            }
        }
    }
    tracing::info!("agent 停止监听");
    Ok(())
}
