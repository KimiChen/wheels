mod auth;
mod cert;
mod cli;
mod config;
mod db;
mod http;
mod resend;
mod webhook;

#[cfg(test)]
mod tests;

use anyhow::{Context, Result};
use axum_server::tls_rustls::RustlsConfig;
use clap::Parser;
use cli::{CertCommand, Cli, Command};
use config::Config;
use db::Database;
use http::AppState;
use resend::ResendClient;
use tracing_subscriber::EnvFilter;

#[tokio::main]
async fn main() -> Result<()> {
    init_tracing();

    let cli = Cli::parse();
    match cli.command.unwrap_or(Command::Serve) {
        Command::Serve => run_server(&cli.config).await,
        Command::Check => run_check(&cli.config).await,
        Command::Cert { command } => run_cert(&cli.config, command).await,
    }
}

fn init_tracing() {
    let filter = EnvFilter::try_from_default_env().unwrap_or_else(|_| EnvFilter::new("info"));
    tracing_subscriber::fmt().with_env_filter(filter).init();
}

async fn run_server(config_path: &str) -> Result<()> {
    let config = Config::load(config_path)?;
    config.validate_for_serve()?;

    let database = Database::open(&config.database_url)?;
    database.migrate().await?;

    let tls = RustlsConfig::from_pem_file(&config.tls_cert_path, &config.tls_key_path)
        .await
        .context("failed to load TLS certificate and private key")?;
    let listen_addr = config.listen_addr;
    let state = AppState::new(config, database, ResendClient::new());

    tracing::info!("resend-ticketd listening on https://{}", listen_addr);
    let server =
        axum_server::bind_rustls(listen_addr, tls).serve(http::router(state).into_make_service());

    tokio::select! {
        result = server => result.context("server failed"),
        _ = shutdown_signal() => Ok(()),
    }
}

async fn run_check(config_path: &str) -> Result<()> {
    let config = Config::load(config_path)?;
    config.validate_for_serve()?;
    let database = Database::open(&config.database_url)?;
    database.migrate().await?;
    println!("configuration ok");
    Ok(())
}

async fn run_cert(config_path: &str, command: CertCommand) -> Result<()> {
    let config = Config::load(config_path)?;
    config.validate_for_cert()?;
    match command {
        CertCommand::Issue => cert::issue(&config).await,
        CertCommand::Renew => cert::renew(&config).await,
    }
}

async fn shutdown_signal() {
    if let Err(error) = tokio::signal::ctrl_c().await {
        tracing::warn!(%error, "failed to listen for shutdown signal");
    }
}
