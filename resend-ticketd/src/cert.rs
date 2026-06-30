use anyhow::{bail, Context, Result};
use tokio::{fs, process::Command};

use crate::config::Config;

pub async fn issue(config: &Config) -> Result<()> {
    run_lego(config, false).await
}

pub async fn renew(config: &Config) -> Result<()> {
    run_lego(config, true).await?;
    let status = Command::new("systemctl")
        .args(["reload-or-restart", "resend-ticketd"])
        .status()
        .await
        .context("failed to execute systemctl reload-or-restart resend-ticketd")?;
    if !status.success() {
        bail!("systemctl reload-or-restart resend-ticketd failed");
    }
    Ok(())
}

async fn run_lego(config: &Config, renew: bool) -> Result<()> {
    fs::create_dir_all(&config.acme_cert_dir)
        .await
        .with_context(|| format!("failed to create {}", config.acme_cert_dir.display()))?;

    let env_content = fs::read_to_string(&config.acme_dns_env_file)
        .await
        .with_context(|| format!("failed to read {}", config.acme_dns_env_file.display()))?;
    let mut command = Command::new(&config.acme_lego_path);
    command
        .arg("--accept-tos")
        .arg("--email")
        .arg(&config.acme_email)
        .arg("--dns")
        .arg(&config.acme_dns_provider)
        .arg("--domains")
        .arg(&config.acme_domain)
        .arg("--path")
        .arg(&config.acme_cert_dir);

    for line in env_content.lines() {
        let line = line.trim();
        if line.is_empty() || line.starts_with('#') {
            continue;
        }
        if let Some((key, value)) = line.split_once('=') {
            command.env(key.trim(), value.trim().trim_matches('"'));
        }
    }

    if renew {
        command.arg("renew").arg("--days").arg("30");
    } else {
        command.arg("run");
    }

    let status = command.status().await.context("failed to execute lego")?;
    if !status.success() {
        bail!("lego exited with a failure status");
    }
    Ok(())
}
