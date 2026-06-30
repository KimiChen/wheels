use anyhow::{bail, Context, Result};
use std::{
    collections::HashMap,
    fs::{self, OpenOptions},
    net::SocketAddr,
    path::{Path, PathBuf},
};

const PLACEHOLDERS: &[&str] = &[
    "change-me",
    "example.com",
    "re_xxxxxxxxx",
    "whsec_xxxxxxxxx",
    "$argon2id$v=19$...",
    "xxxxxxxxx",
];

#[derive(Clone, Debug)]
pub struct Config {
    pub listen_addr: SocketAddr,
    pub public_base_url: String,
    pub database_url: String,
    pub resend_api_key: String,
    pub resend_webhook_secret: String,
    pub resend_from: String,
    pub support_addresses: Vec<String>,
    pub tls_cert_path: PathBuf,
    pub tls_key_path: PathBuf,
    pub admin_username: String,
    pub admin_password_hash: String,
    pub acme_email: String,
    pub acme_domain: String,
    pub acme_lego_path: PathBuf,
    pub acme_dns_provider: String,
    pub acme_dns_env_file: PathBuf,
    pub acme_cert_dir: PathBuf,
}

impl Config {
    pub fn load(path: &str) -> Result<Self> {
        let mut values = HashMap::new();
        for item in dotenvy::from_path_iter(path)
            .with_context(|| format!("failed to read config file {path}"))?
        {
            let (key, value) =
                item.with_context(|| format!("failed to parse config file {path}"))?;
            values.insert(key, value);
        }

        let listen_addr = value_or(&values, "RESEND_TICKETD_LISTEN_ADDR", "0.0.0.0:9734")
            .parse()
            .context("RESEND_TICKETD_LISTEN_ADDR must be a valid socket address")?;

        Ok(Self {
            listen_addr,
            public_base_url: required(&values, "RESEND_TICKETD_PUBLIC_BASE_URL")?,
            database_url: required(&values, "DATABASE_URL")?,
            resend_api_key: required(&values, "RESEND_API_KEY")?,
            resend_webhook_secret: required(&values, "RESEND_WEBHOOK_SECRET")?,
            resend_from: required(&values, "RESEND_FROM")?,
            support_addresses: split_csv(&required(&values, "SUPPORT_ADDRESSES")?),
            tls_cert_path: PathBuf::from(required(&values, "TLS_CERT_PATH")?),
            tls_key_path: PathBuf::from(required(&values, "TLS_KEY_PATH")?),
            admin_username: required(&values, "ADMIN_USERNAME")?,
            admin_password_hash: required(&values, "ADMIN_PASSWORD_HASH")?,
            acme_email: required(&values, "ACME_EMAIL")?,
            acme_domain: required(&values, "ACME_DOMAIN")?,
            acme_lego_path: PathBuf::from(value_or(
                &values,
                "ACME_LEGO_PATH",
                "/usr/local/bin/lego",
            )),
            acme_dns_provider: required(&values, "ACME_DNS_PROVIDER")?,
            acme_dns_env_file: PathBuf::from(required(&values, "ACME_DNS_ENV_FILE")?),
            acme_cert_dir: PathBuf::from(required(&values, "ACME_CERT_DIR")?),
        })
    }

    pub fn validate_for_serve(&self) -> Result<()> {
        self.validate_common()?;
        self.validate_tls_files()?;
        self.validate_database_parent()?;
        self.validate_privileged_port()?;
        Ok(())
    }

    pub fn validate_for_cert(&self) -> Result<()> {
        self.validate_common()?;
        if self.acme_email.trim().is_empty() || contains_placeholder(&self.acme_email) {
            bail!("ACME_EMAIL must be set to a real email address");
        }
        if self.acme_domain.trim().is_empty() || contains_placeholder(&self.acme_domain) {
            bail!("ACME_DOMAIN must be set to a real domain");
        }
        if self.acme_dns_provider.trim().is_empty() {
            bail!("ACME_DNS_PROVIDER must not be empty");
        }
        if !self.acme_lego_path.is_file() {
            bail!("ACME_LEGO_PATH does not exist or is not a file");
        }
        if !self.acme_dns_env_file.is_file() {
            bail!("ACME_DNS_ENV_FILE does not exist or is not a file");
        }
        validate_secret_file_mode(&self.acme_dns_env_file)?;
        Ok(())
    }

    pub fn sqlite_path(&self) -> Result<Option<PathBuf>> {
        sqlite_path_from_url(&self.database_url)
    }

    fn validate_common(&self) -> Result<()> {
        if !self.public_base_url.starts_with("https://")
            || contains_placeholder(&self.public_base_url)
        {
            bail!("RESEND_TICKETD_PUBLIC_BASE_URL must be a real https URL");
        }
        if self.resend_api_key.trim().is_empty()
            || !self.resend_api_key.starts_with("re_")
            || contains_placeholder(&self.resend_api_key)
        {
            bail!("RESEND_API_KEY must be a real Resend API key");
        }
        if self.resend_webhook_secret.trim().is_empty()
            || !self.resend_webhook_secret.starts_with("whsec_")
            || contains_placeholder(&self.resend_webhook_secret)
        {
            bail!("RESEND_WEBHOOK_SECRET must be a real webhook signing secret");
        }
        if self.resend_from.trim().is_empty() || contains_placeholder(&self.resend_from) {
            bail!("RESEND_FROM must be changed from the example value");
        }
        if self.support_addresses.is_empty()
            || self
                .support_addresses
                .iter()
                .any(|addr| !addr.contains('@') || contains_placeholder(addr))
        {
            bail!("SUPPORT_ADDRESSES must contain at least one real email address");
        }
        if self.admin_username.trim().is_empty() {
            bail!("ADMIN_USERNAME must not be empty");
        }
        if !self.admin_password_hash.starts_with("$argon2id$")
            || contains_placeholder(&self.admin_password_hash)
            || self.admin_password_hash.len() < 40
        {
            bail!("ADMIN_PASSWORD_HASH must be a real Argon2id password hash");
        }
        self.sqlite_path()?;
        Ok(())
    }

    fn validate_tls_files(&self) -> Result<()> {
        if !self.tls_cert_path.is_file() {
            bail!("TLS_CERT_PATH does not exist or is not a file");
        }
        if !self.tls_key_path.is_file() {
            bail!("TLS_KEY_PATH does not exist or is not a file");
        }
        validate_secret_file_mode(&self.tls_key_path)?;
        Ok(())
    }

    fn validate_database_parent(&self) -> Result<()> {
        let Some(path) = self.sqlite_path()? else {
            return Ok(());
        };
        let parent = path
            .parent()
            .filter(|parent| !parent.as_os_str().is_empty())
            .unwrap_or_else(|| Path::new("."));
        if !parent.is_dir() {
            bail!("database directory does not exist: {}", parent.display());
        }
        let probe = parent.join(format!(
            ".resend-ticketd-write-test-{}",
            uuid::Uuid::new_v4()
        ));
        OpenOptions::new()
            .create_new(true)
            .write(true)
            .open(&probe)
            .with_context(|| format!("database directory is not writable: {}", parent.display()))?;
        let _ = fs::remove_file(probe);
        Ok(())
    }

    fn validate_privileged_port(&self) -> Result<()> {
        if self.listen_addr.port() >= 1024 {
            return Ok(());
        }
        if process_can_bind_low_ports() {
            return Ok(());
        }
        bail!(
            "listening on privileged port {} requires root or CAP_NET_BIND_SERVICE",
            self.listen_addr.port()
        )
    }
}

fn required(values: &HashMap<String, String>, key: &str) -> Result<String> {
    values
        .get(key)
        .map(|value| value.trim().to_string())
        .filter(|value| !value.is_empty())
        .with_context(|| format!("{key} must be set"))
}

fn value_or(values: &HashMap<String, String>, key: &str, default: &str) -> String {
    values
        .get(key)
        .map(|value| value.trim().to_string())
        .filter(|value| !value.is_empty())
        .unwrap_or_else(|| default.to_string())
}

fn split_csv(value: &str) -> Vec<String> {
    value
        .split(',')
        .map(str::trim)
        .filter(|item| !item.is_empty())
        .map(ToOwned::to_owned)
        .collect()
}

fn contains_placeholder(value: &str) -> bool {
    let lower = value.to_ascii_lowercase();
    PLACEHOLDERS
        .iter()
        .any(|placeholder| lower.contains(&placeholder.to_ascii_lowercase()))
}

fn sqlite_path_from_url(url: &str) -> Result<Option<PathBuf>> {
    let trimmed = url.trim();
    if matches!(trimmed, "sqlite::memory:" | "sqlite://:memory:") {
        return Ok(None);
    }
    if let Some(path) = trimmed.strip_prefix("sqlite://") {
        if path.is_empty() {
            bail!("DATABASE_URL sqlite path must not be empty");
        }
        return Ok(Some(PathBuf::from(path)));
    }
    if let Some(path) = trimmed.strip_prefix("sqlite:") {
        if path.is_empty() {
            bail!("DATABASE_URL sqlite path must not be empty");
        }
        return Ok(Some(PathBuf::from(path)));
    }
    bail!("DATABASE_URL must use sqlite:// or sqlite: format")
}

#[cfg(unix)]
fn validate_secret_file_mode(path: &Path) -> Result<()> {
    use std::os::unix::fs::PermissionsExt;

    let mode = fs::metadata(path)
        .with_context(|| format!("failed to inspect {}", path.display()))?
        .permissions()
        .mode()
        & 0o777;
    if mode & 0o077 != 0 {
        bail!("{} must not be readable by group or others", path.display());
    }
    Ok(())
}

#[cfg(not(unix))]
fn validate_secret_file_mode(_path: &Path) -> Result<()> {
    Ok(())
}

#[cfg(target_os = "linux")]
fn process_can_bind_low_ports() -> bool {
    if let Ok(status) = fs::read_to_string("/proc/self/status") {
        if status.lines().any(|line| line == "Uid:\t0\t0\t0\t0") {
            return true;
        }
        if let Some(cap_eff) = status
            .lines()
            .find_map(|line| line.strip_prefix("CapEff:\t"))
            .and_then(|hex| u64::from_str_radix(hex.trim(), 16).ok())
        {
            return cap_eff & (1 << 10) != 0;
        }
    }
    false
}

#[cfg(not(target_os = "linux"))]
fn process_can_bind_low_ports() -> bool {
    true
}
