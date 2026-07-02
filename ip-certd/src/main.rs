mod acme;
mod api;
mod bundle;
mod cert_store;
mod cli;
mod cloudflare;
mod config;
mod iplist;
mod real_ip;
mod service;
mod whitelist;

#[cfg(test)]
mod tests;

use anyhow::{Context, Result};
use clap::Parser;
use cli::{Cli, Command};
use config::Config;
use iplist::IpList;
use service::IpCertService;
use std::{net::SocketAddr, sync::Arc};
use tokio::net::TcpListener;
use tracing_subscriber::EnvFilter;

#[tokio::main]
async fn main() -> Result<()> {
    let _ = dotenvy::dotenv();
    init_tracing();

    let cli = Cli::parse();
    match cli.command.clone().unwrap_or(Command::Serve) {
        Command::Serve => run_server(&cli).await,
        Command::Check => run_check(&cli),
    }
}

fn init_tracing() {
    let filter = EnvFilter::try_from_default_env().unwrap_or_else(|_| EnvFilter::new("info"));
    tracing_subscriber::fmt().with_env_filter(filter).init();
}

async fn run_server(cli: &Cli) -> Result<()> {
    let config = Config::load(&cli.config)?;
    config.validate()?;
    let iplist = IpList::load(&cli.iplist)?;
    iplist.validate(config.security.allow_private_ip)?;

    let listen_addr = config.server.listen;
    let service = Arc::new(IpCertService::from_config(config, iplist)?);
    let listener = TcpListener::bind(listen_addr)
        .await
        .with_context(|| format!("failed to bind {listen_addr}"))?;

    tracing::info!("ip-certd listening on http://{}", listen_addr);
    axum::serve(
        listener,
        api::router(service).into_make_service_with_connect_info::<SocketAddr>(),
    )
    .with_graceful_shutdown(shutdown_signal())
    .await
    .context("server failed")
}

fn run_check(cli: &Cli) -> Result<()> {
    let config = Config::load(&cli.config)?;
    config.validate()?;
    let iplist = IpList::load(&cli.iplist)?;
    iplist.validate(config.security.allow_private_ip)?;
    println!("configuration ok");
    Ok(())
}

async fn shutdown_signal() {
    if let Err(error) = tokio::signal::ctrl_c().await {
        tracing::warn!(%error, "failed to listen for shutdown signal");
    }
}
