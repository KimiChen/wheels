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
use clap::Parser;
use cli::{Cli, Command};
use config::Config;
use service::TrafficService;
use std::sync::Arc;
use tokio::net::TcpListener;
use tracing_subscriber::EnvFilter;

#[tokio::main]
async fn main() -> Result<()> {
    init_tracing();

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

async fn run_server(config_path: &str) -> Result<()> {
    let config = Config::load(config_path)?;
    config.validate()?;

    let service = Arc::new(TrafficService::new(config.clone(), config_path));
    service.ensure_state()?;

    let listener = TcpListener::bind(&config.listen_addr)
        .await
        .with_context(|| format!("failed to bind {}", config.listen_addr))?;
    tracing::info!("vps-trafficd listening on {}", config.listen_addr);

    axum::serve(listener, api::router(service))
        .with_graceful_shutdown(shutdown_signal())
        .await
        .context("server failed")
}

async fn run_check(config_path: &str) -> Result<()> {
    let config = Config::load(config_path)?;
    config.validate()?;
    TrafficService::new(config, config_path).check()?;
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

async fn shutdown_signal() {
    if let Err(error) = tokio::signal::ctrl_c().await {
        tracing::warn!(%error, "failed to listen for shutdown signal");
    }
}
