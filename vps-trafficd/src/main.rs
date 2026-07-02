mod api;
mod billing;
mod cli;
mod config;
mod counters;
mod service;
mod state;

#[cfg(test)]
mod tests;

use anyhow::{Context, Result};
use axum_server::{tls_rustls::RustlsConfig, Handle};
use clap::Parser;
use cli::{Cli, Command};
use config::Config;
use service::TrafficService;
use std::{net::SocketAddr, sync::Arc, time::Duration};
use tracing_subscriber::EnvFilter;

#[tokio::main]
async fn main() -> Result<()> {
    init_tracing();
    init_tls_provider();

    let cli = Cli::parse();
    match cli.command {
        Some(Command::Check) => run_check(&cli.config).await,
        Some(Command::Calibrate { rx, tx }) => run_calibrate(&cli.config, rx, tx).await,
        None => run_server(&cli.config).await,
    }
}

fn init_tracing() {
    let filter = EnvFilter::try_from_default_env().unwrap_or_else(|_| EnvFilter::new("info"));
    tracing_subscriber::fmt().with_env_filter(filter).init();
}

fn init_tls_provider() {
    let _ = rustls::crypto::ring::default_provider().install_default();
}

async fn run_server(config_path: &str) -> Result<()> {
    let config = Config::load(config_path)?;
    config.validate()?;

    let service = Arc::new(TrafficService::new(config.clone(), config_path));
    service.ensure_state()?;

    let tls_config = tls_config_if_available(&config).await?;
    let handle = Handle::<SocketAddr>::new();
    tokio::spawn(shutdown_signal(handle.clone()));

    if let Some(tls_config) = tls_config {
        tracing::info!("vps-trafficd listening on https://{}", config.listen_addr);
        axum_server::bind_rustls(config.listen_addr, tls_config)
            .handle(handle)
            .serve(api::router(service).into_make_service())
            .await
            .context("server failed")
    } else {
        tracing::info!("vps-trafficd listening on http://{}", config.listen_addr);
        axum_server::bind(config.listen_addr)
            .handle(handle)
            .serve(api::router(service).into_make_service())
            .await
            .context("server failed")
    }
}

async fn run_check(config_path: &str) -> Result<()> {
    let config = Config::load(config_path)?;
    config.validate()?;
    TrafficService::new(config.clone(), config_path).check()?;
    let _ = tls_config_if_available(&config).await?;
    println!("configuration ok");
    Ok(())
}

async fn run_calibrate(config_path: &str, rx: u64, tx: u64) -> Result<()> {
    let config = Config::load(config_path)?;
    config.validate()?;
    let service = TrafficService::new(config, config_path);
    service.calibrate(rx, tx)?;
    println!("calibration saved");
    Ok(())
}

async fn tls_config_if_available(config: &Config) -> Result<Option<RustlsConfig>> {
    if !config.tls_enabled() {
        return Ok(None);
    }

    let tls_config = RustlsConfig::from_pem_file(&config.tls_cert_path, &config.tls_key_path)
        .await
        .with_context(|| {
            format!(
                "failed to load TLS certificate {} and key {}",
                config.tls_cert_path.display(),
                config.tls_key_path.display()
            )
        })?;
    Ok(Some(tls_config))
}

async fn shutdown_signal(handle: Handle<SocketAddr>) {
    if let Err(error) = tokio::signal::ctrl_c().await {
        tracing::warn!(%error, "failed to listen for shutdown signal");
        return;
    }
    tracing::info!("shutdown signal received");
    handle.graceful_shutdown(Some(Duration::from_secs(30)));
}
