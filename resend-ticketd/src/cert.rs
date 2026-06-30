use anyhow::{bail, Context, Result};
use std::{
    path::{Path, PathBuf},
    time::{SystemTime, UNIX_EPOCH},
};
use tokio::{fs, process::Command};

use crate::config::Config;

pub async fn issue(config: &Config) -> Result<()> {
    run_lego(config, false).await?;
    install_lego_certificate(config).await
}

pub async fn renew(config: &Config) -> Result<()> {
    run_lego(config, true).await?;
    install_lego_certificate(config).await?;
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

    let dns_env = load_dns_env(&config.acme_dns_env_file)?;
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

    for (key, value) in dns_env {
        command.env(key, value);
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

async fn install_lego_certificate(config: &Config) -> Result<()> {
    let (source_cert, source_key) = lego_certificate_paths(config);
    if !source_cert.is_file() {
        bail!(
            "lego certificate output does not exist: {}",
            source_cert.display()
        );
    }
    if !source_key.is_file() {
        bail!(
            "lego private key output does not exist: {}",
            source_key.display()
        );
    }

    copy_file_with_mode(&source_cert, &config.tls_cert_path, 0o644).await?;
    copy_file_with_mode(&source_key, &config.tls_key_path, 0o640).await?;
    Ok(())
}

fn load_dns_env(path: &Path) -> Result<Vec<(String, String)>> {
    let mut values = Vec::new();
    for item in dotenvy::from_path_iter(path)
        .with_context(|| format!("failed to read {}", path.display()))?
    {
        values.push(item.with_context(|| format!("failed to parse {}", path.display()))?);
    }
    Ok(values)
}

fn lego_certificate_paths(config: &Config) -> (PathBuf, PathBuf) {
    let stem = lego_certificate_stem(&config.acme_domain);
    let certificates_dir = config.acme_cert_dir.join("certificates");
    (
        certificates_dir.join(format!("{stem}.crt")),
        certificates_dir.join(format!("{stem}.key")),
    )
}

fn lego_certificate_stem(domain: &str) -> String {
    domain.replace('*', "_")
}

async fn copy_file_with_mode(source: &Path, destination: &Path, mode: u32) -> Result<()> {
    if let Some(parent) = destination.parent() {
        fs::create_dir_all(parent)
            .await
            .with_context(|| format!("failed to create {}", parent.display()))?;
    }

    let temp_destination = temp_destination_path(destination);
    fs::copy(source, &temp_destination).await.with_context(|| {
        format!(
            "failed to copy {} to {}",
            source.display(),
            temp_destination.display()
        )
    })?;
    set_file_mode(&temp_destination, mode).await?;
    fs::rename(&temp_destination, destination)
        .await
        .with_context(|| format!("failed to install {}", destination.display()))?;
    Ok(())
}

fn temp_destination_path(destination: &Path) -> PathBuf {
    let nonce = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|duration| duration.as_nanos())
        .unwrap_or_default();
    let file_name = destination
        .file_name()
        .and_then(|name| name.to_str())
        .unwrap_or("certificate");
    destination.with_file_name(format!(".{file_name}.{nonce}.tmp"))
}

#[cfg(unix)]
async fn set_file_mode(path: &Path, mode: u32) -> Result<()> {
    use std::os::unix::fs::PermissionsExt;

    fs::set_permissions(path, std::fs::Permissions::from_mode(mode))
        .await
        .with_context(|| format!("failed to set permissions on {}", path.display()))?;
    Ok(())
}

#[cfg(not(unix))]
async fn set_file_mode(_path: &Path, _mode: u32) -> Result<()> {
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::lego_certificate_stem;

    #[test]
    fn wildcard_domains_match_lego_certificate_file_names() {
        assert_eq!(lego_certificate_stem("*.example.com"), "_.example.com");
        assert_eq!(
            lego_certificate_stem("tickets.example.com"),
            "tickets.example.com"
        );
    }
}
