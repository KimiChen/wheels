use anyhow::{bail, Context, Result};
use serde::Deserialize;
use std::{
    env, fs,
    net::{IpAddr, Ipv4Addr, Ipv6Addr, SocketAddr},
    path::{Path, PathBuf},
};

#[derive(Clone, Debug, Deserialize)]
pub struct Config {
    #[serde(default = "default_domain")]
    pub domain: String,
    #[serde(default = "default_ttl")]
    pub ttl: u32,
    #[serde(default)]
    pub server: ServerConfig,
    #[serde(default)]
    pub cloudflare: CloudflareConfig,
    #[serde(default)]
    pub acme: AcmeConfig,
    #[serde(default)]
    pub security: SecurityConfig,
}

#[derive(Clone, Debug, Deserialize)]
pub struct ServerConfig {
    #[serde(default = "default_listen")]
    pub listen: SocketAddr,
    #[serde(default)]
    pub public_base_url: String,
    #[serde(default = "default_server_storage")]
    pub storage: PathBuf,
    #[serde(default = "default_real_ip_header")]
    pub real_ip_header: String,
    #[serde(default = "default_trusted_proxies")]
    pub trusted_proxies: Vec<IpAddr>,
}

#[derive(Clone, Debug, Deserialize)]
pub struct CloudflareConfig {
    #[serde(default)]
    pub zone_id: String,
    #[serde(default = "default_cloudflare_token_env")]
    pub api_token_env: String,
}

#[derive(Clone, Debug, Deserialize)]
pub struct AcmeConfig {
    #[serde(default = "default_true")]
    pub enabled: bool,
    #[serde(default)]
    pub email: String,
    #[serde(default = "default_acme_directory")]
    pub directory: String,
    #[serde(default = "default_acme_staging_directory")]
    pub staging_directory: String,
    #[serde(default)]
    pub use_staging: bool,
    #[serde(default = "default_cert_storage")]
    pub storage: PathBuf,
    #[serde(default = "default_renew_before_days")]
    pub renew_before_days: i64,
    #[serde(default = "default_dns_timeout_seconds")]
    pub dns_propagation_timeout_seconds: u64,
}

#[derive(Clone, Debug, Deserialize)]
pub struct SecurityConfig {
    #[serde(default)]
    pub allow_private_ip: bool,
    #[serde(default = "default_rate_limit")]
    pub rate_limit_per_ip_per_minute: u32,
}

impl Default for ServerConfig {
    fn default() -> Self {
        Self {
            listen: default_listen(),
            public_base_url: String::new(),
            storage: default_server_storage(),
            real_ip_header: default_real_ip_header(),
            trusted_proxies: default_trusted_proxies(),
        }
    }
}

impl Default for CloudflareConfig {
    fn default() -> Self {
        Self {
            zone_id: String::new(),
            api_token_env: default_cloudflare_token_env(),
        }
    }
}

impl Default for AcmeConfig {
    fn default() -> Self {
        Self {
            enabled: true,
            email: String::new(),
            directory: default_acme_directory(),
            staging_directory: default_acme_staging_directory(),
            use_staging: false,
            storage: default_cert_storage(),
            renew_before_days: default_renew_before_days(),
            dns_propagation_timeout_seconds: default_dns_timeout_seconds(),
        }
    }
}

impl Default for SecurityConfig {
    fn default() -> Self {
        Self {
            allow_private_ip: false,
            rate_limit_per_ip_per_minute: default_rate_limit(),
        }
    }
}

impl Config {
    pub fn load(path: &Path) -> Result<Self> {
        let content = fs::read_to_string(path)
            .with_context(|| format!("failed to read config file {}", path.display()))?;
        toml::from_str(&content)
            .with_context(|| format!("failed to parse config file {}", path.display()))
    }

    pub fn validate(&self) -> Result<()> {
        let domain = self.domain.trim().trim_end_matches('.');
        if !is_valid_domain(domain) {
            bail!("domain must be a valid DNS name");
        }
        if !(self.ttl == 1 || (60..=86_400).contains(&self.ttl)) {
            bail!("ttl must be 1 or between 60 and 86400 seconds");
        }
        if !is_safe_listen_ip(self.server.listen.ip()) {
            bail!("server.listen must be loopback or private; keep public HTTPS on Nginx");
        }
        if !self.server.public_base_url.trim().is_empty()
            && !self.server.public_base_url.starts_with("https://")
        {
            bail!("server.public_base_url must start with https:// when set");
        }
        if self.server.storage.as_os_str().is_empty() {
            bail!("server.storage must not be empty");
        }
        if self.server.real_ip_header.trim().is_empty() {
            bail!("server.real_ip_header must not be empty");
        }
        if self.server.trusted_proxies.is_empty() {
            bail!("server.trusted_proxies must contain at least one proxy address");
        }
        if self.cloudflare.zone_id.trim().is_empty() {
            bail!("cloudflare.zone_id must not be empty");
        }
        if self.cloudflare.api_token_env.trim().is_empty() {
            bail!("cloudflare.api_token_env must not be empty");
        }
        if self.acme.enabled && self.acme.email.trim().is_empty() {
            bail!("acme.email must not be empty when ACME is enabled");
        }
        if self.acme.enabled && self.acme_directory().trim().is_empty() {
            bail!("acme directory URL must not be empty");
        }
        if self.acme.storage.as_os_str().is_empty() {
            bail!("acme.storage must not be empty");
        }
        if self.acme.renew_before_days <= 0 {
            bail!("acme.renew_before_days must be greater than zero");
        }
        if self.acme.dns_propagation_timeout_seconds == 0 {
            bail!("acme.dns_propagation_timeout_seconds must be greater than zero");
        }
        Ok(())
    }

    pub fn cloudflare_api_token(&self) -> Result<String> {
        let name = self.cloudflare.api_token_env.trim();
        env::var(name).with_context(|| format!("environment variable {name} is required"))
    }

    pub fn acme_directory(&self) -> &str {
        if self.acme.use_staging {
            &self.acme.staging_directory
        } else {
            &self.acme.directory
        }
    }
}

fn is_valid_domain(value: &str) -> bool {
    if value.is_empty() || value.len() > 253 || value.contains("://") || value.contains('/') {
        return false;
    }

    value.split('.').all(|label| {
        let bytes = label.as_bytes();
        !bytes.is_empty()
            && bytes.len() <= 63
            && bytes[0].is_ascii_alphanumeric()
            && bytes[bytes.len() - 1].is_ascii_alphanumeric()
            && bytes
                .iter()
                .all(|byte| byte.is_ascii_alphanumeric() || *byte == b'-')
    })
}

fn is_safe_listen_ip(ip: IpAddr) -> bool {
    match ip {
        IpAddr::V4(ip) => ip.is_loopback() || ip.is_private() || ip.is_link_local(),
        IpAddr::V6(ip) => {
            ip.is_loopback() || ip.is_unicast_link_local() || is_unique_local_ipv6(ip)
        }
    }
}

fn is_unique_local_ipv6(ip: Ipv6Addr) -> bool {
    (ip.segments()[0] & 0xfe00) == 0xfc00
}

fn default_domain() -> String {
    "ip.example.com".to_string()
}

fn default_ttl() -> u32 {
    60
}

fn default_listen() -> SocketAddr {
    "127.0.0.1:9735"
        .parse()
        .expect("valid default listen address")
}

fn default_server_storage() -> PathBuf {
    PathBuf::from("/var/lib/ip-certd")
}

fn default_real_ip_header() -> String {
    "x-real-ip".to_string()
}

fn default_trusted_proxies() -> Vec<IpAddr> {
    vec![
        IpAddr::V4(Ipv4Addr::LOCALHOST),
        IpAddr::V6(Ipv6Addr::LOCALHOST),
    ]
}

fn default_cloudflare_token_env() -> String {
    "CLOUDFLARE_API_TOKEN".to_string()
}

fn default_true() -> bool {
    true
}

fn default_acme_directory() -> String {
    "https://acme-v02.api.letsencrypt.org/directory".to_string()
}

fn default_acme_staging_directory() -> String {
    "https://acme-staging-v02.api.letsencrypt.org/directory".to_string()
}

fn default_cert_storage() -> PathBuf {
    PathBuf::from("/var/lib/ip-certd/certs")
}

fn default_renew_before_days() -> i64 {
    30
}

fn default_dns_timeout_seconds() -> u64 {
    120
}

fn default_rate_limit() -> u32 {
    6
}
