mod api;
mod billing;
mod cli;
mod config;
mod counters;
mod service;
mod state;

#[cfg(test)]
mod tests;

use anyhow::{bail, Context, Result};
use axum_server::{tls_rustls::RustlsConfig, Handle};
use clap::Parser;
use cli::{Cli, Command};
use config::Config;
use service::TrafficService;
use std::{
    collections::hash_map::DefaultHasher,
    fs,
    hash::{Hash, Hasher},
    io::ErrorKind,
    net::SocketAddr,
    path::Path,
    sync::{
        atomic::{AtomicBool, Ordering},
        Arc,
    },
    time::Duration,
};
use tracing_subscriber::EnvFilter;

const STATE_SAMPLE_INTERVAL: Duration = Duration::from_secs(3600);
const TLS_RESTART_SHUTDOWN_TIMEOUT: Duration = Duration::from_secs(30);

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
    tokio::spawn(run_state_updater(service.clone(), STATE_SAMPLE_INTERVAL));

    let tls_config = tls_config_if_available(&config).await?;
    let handle = Handle::<SocketAddr>::new();
    let tls_restart_requested = Arc::new(AtomicBool::new(false));
    if config.tls_auto_restart {
        tokio::spawn(run_tls_auto_restart(
            config.clone(),
            handle.clone(),
            tls_restart_requested.clone(),
        ));
    }
    tokio::spawn(shutdown_signal(handle.clone()));

    let result = if let Some(tls_config) = tls_config {
        tracing::info!("vps-trafficd listening on https://{}", config.listen_addr);
        axum_server::bind_rustls(config.listen_addr, tls_config)
            .handle(handle)
            .serve(api::router(service).into_make_service())
            .await
    } else {
        tracing::info!("vps-trafficd listening on http://{}", config.listen_addr);
        axum_server::bind(config.listen_addr)
            .handle(handle)
            .serve(api::router(service).into_make_service())
            .await
    };

    if tls_restart_requested.load(Ordering::SeqCst) {
        bail!(
            "TLS certificate changed; exiting so the process supervisor can restart vps-trafficd"
        );
    }

    result.context("server failed")
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

async fn run_state_updater(service: Arc<TrafficService>, sample_interval: Duration) {
    let sample_interval = sample_interval.max(Duration::from_secs(1));

    loop {
        let delay = match service.next_state_update_delay(sample_interval) {
            Ok(delay) => delay,
            Err(error) => {
                tracing::warn!(%error, "failed to calculate next state update delay");
                sample_interval
            }
        };

        tokio::time::sleep(delay).await;

        if let Err(error) = service.refresh_state() {
            tracing::warn!(%error, "background state update failed");
        }
    }
}

fn next_state_update_delay(
    cycle_end: chrono::DateTime<chrono::FixedOffset>,
    now: chrono::DateTime<chrono::FixedOffset>,
    sample_interval: Duration,
) -> Duration {
    cycle_end
        .signed_duration_since(now)
        .to_std()
        .unwrap_or_default()
        .min(sample_interval)
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

async fn run_tls_auto_restart(
    config: Config,
    handle: Handle<SocketAddr>,
    restart_requested: Arc<AtomicBool>,
) {
    let poll_interval = Duration::from_secs(config.tls_watch_interval_secs.max(1));
    let settle_delay = Duration::from_secs(config.tls_restart_settle_secs);
    let mut current = loop {
        match tls_pair_fingerprint(&config) {
            Ok(Some(fingerprint)) => break fingerprint,
            Ok(None) => {
                tracing::warn!(
                    cert_path = %config.tls_cert_path.display(),
                    key_path = %config.tls_key_path.display(),
                    "TLS certificate pair is incomplete; waiting before watching for changes"
                );
            }
            Err(error) => {
                tracing::warn!(%error, "failed to fingerprint TLS certificate files");
            }
        }
        tokio::time::sleep(poll_interval).await;
    };

    tracing::info!(
        interval_secs = config.tls_watch_interval_secs,
        settle_secs = config.tls_restart_settle_secs,
        cert_path = %config.tls_cert_path.display(),
        key_path = %config.tls_key_path.display(),
        "watching TLS certificate files for changes"
    );

    loop {
        tokio::time::sleep(poll_interval).await;

        let changed = match tls_pair_fingerprint(&config) {
            Ok(Some(fingerprint)) if fingerprint != current => true,
            Ok(Some(fingerprint)) => {
                current = fingerprint;
                false
            }
            Ok(None) => {
                tracing::warn!(
                    cert_path = %config.tls_cert_path.display(),
                    key_path = %config.tls_key_path.display(),
                    "TLS certificate pair is incomplete; skipping restart until both files agree"
                );
                false
            }
            Err(error) => {
                tracing::warn!(%error, "failed to fingerprint TLS certificate files");
                false
            }
        };

        if !changed {
            continue;
        }

        tracing::info!(
            settle_secs = config.tls_restart_settle_secs,
            "TLS certificate files changed; waiting before restart"
        );
        if !settle_delay.is_zero() {
            tokio::time::sleep(settle_delay).await;
        }

        match tls_pair_fingerprint(&config) {
            Ok(Some(fingerprint)) if fingerprint != current => {
                restart_requested.store(true, Ordering::SeqCst);
                tracing::info!(
                    "TLS certificate change is stable; gracefully exiting for supervisor restart"
                );
                handle.graceful_shutdown(Some(TLS_RESTART_SHUTDOWN_TIMEOUT));
                return;
            }
            Ok(Some(fingerprint)) => {
                current = fingerprint;
                tracing::info!("TLS certificate files returned to the previous state");
            }
            Ok(None) => {
                tracing::warn!(
                    cert_path = %config.tls_cert_path.display(),
                    key_path = %config.tls_key_path.display(),
                    "TLS certificate pair is still incomplete after settle delay"
                );
            }
            Err(error) => {
                tracing::warn!(%error, "failed to fingerprint TLS certificate files after change");
            }
        }
    }
}

fn tls_pair_fingerprint(config: &Config) -> Result<Option<TlsPairFingerprint>> {
    let cert = tls_file_fingerprint(&config.tls_cert_path)?;
    let key = tls_file_fingerprint(&config.tls_key_path)?;

    match (&cert, &key) {
        (TlsFileFingerprint::Missing, TlsFileFingerprint::Missing)
        | (TlsFileFingerprint::Present { .. }, TlsFileFingerprint::Present { .. }) => {
            Ok(Some(TlsPairFingerprint { cert, key }))
        }
        _ => Ok(None),
    }
}

fn tls_file_fingerprint(path: &Path) -> Result<TlsFileFingerprint> {
    match fs::read(path) {
        Ok(bytes) => {
            let mut hasher = DefaultHasher::new();
            bytes.hash(&mut hasher);
            Ok(TlsFileFingerprint::Present {
                len: bytes.len(),
                hash: hasher.finish(),
            })
        }
        Err(error) if error.kind() == ErrorKind::NotFound => Ok(TlsFileFingerprint::Missing),
        Err(error) => {
            Err(error).with_context(|| format!("failed to read TLS file {}", path.display()))
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
struct TlsPairFingerprint {
    cert: TlsFileFingerprint,
    key: TlsFileFingerprint,
}

#[derive(Clone, Debug, Eq, PartialEq)]
enum TlsFileFingerprint {
    Missing,
    Present { len: usize, hash: u64 },
}

async fn shutdown_signal(handle: Handle<SocketAddr>) {
    if let Err(error) = tokio::signal::ctrl_c().await {
        tracing::warn!(%error, "failed to listen for shutdown signal");
        return;
    }
    tracing::info!("shutdown signal received");
    handle.graceful_shutdown(Some(Duration::from_secs(30)));
}
