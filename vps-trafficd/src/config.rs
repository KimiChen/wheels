use anyhow::{bail, Context, Result};
use chrono::{DateTime, FixedOffset};
use serde::Deserialize;
use std::{fs, net::SocketAddr, path::PathBuf};

const EXAMPLE_TOKENS: &[&str] = &[
    "change-me",
    "example",
    "example-token",
    "please-change-me",
    "replace-with-a-long-random-token",
];

#[derive(Clone, Debug, Deserialize)]
pub struct Config {
    #[serde(default = "default_listen_addr")]
    pub listen_addr: SocketAddr,
    pub auth_token: String,
    #[serde(default = "default_interfaces")]
    pub interfaces: Vec<String>,
    #[serde(default = "default_node_id")]
    pub node_id: String,
    pub quota_bytes: u64,
    #[serde(default)]
    pub billing_mode: BillingMode,
    pub cycle_anchor: DateTime<FixedOffset>,
    #[serde(default = "default_cycle_months")]
    pub cycle_months: u32,
    #[serde(default = "default_state_path")]
    pub state_path: PathBuf,
}

#[derive(Copy, Clone, Debug, Deserialize, Eq, PartialEq)]
#[serde(rename_all = "lowercase")]
pub enum BillingMode {
    Rx,
    Tx,
    Total,
}

impl Default for BillingMode {
    fn default() -> Self {
        Self::Total
    }
}

impl Config {
    pub fn load(path: &str) -> Result<Self> {
        let content = fs::read_to_string(path)
            .with_context(|| format!("failed to read config file {path}"))?;
        toml::from_str(&content).with_context(|| format!("failed to parse config file {path}"))
    }

    pub fn validate(&self) -> Result<()> {
        let token = self.auth_token.trim();
        if token.is_empty() {
            bail!("auth_token must not be empty");
        }
        if EXAMPLE_TOKENS
            .iter()
            .any(|example| token.eq_ignore_ascii_case(example))
        {
            bail!("auth_token must be changed from the example value");
        }
        if self.interfaces.is_empty() {
            bail!("interfaces must contain at least one network interface");
        }
        if self
            .interfaces
            .iter()
            .any(|iface| iface.trim().is_empty() || iface.contains('/') || iface.contains('\\'))
        {
            bail!("interfaces must be simple network interface names");
        }
        if self.node_id.trim().is_empty() {
            bail!("node_id must not be empty");
        }
        if self.quota_bytes == 0 {
            bail!("quota_bytes must be greater than zero");
        }
        if self.cycle_months == 0 {
            bail!("cycle_months must be greater than zero");
        }
        Ok(())
    }
}

fn default_listen_addr() -> SocketAddr {
    "0.0.0.0:9733"
        .parse()
        .expect("valid default listen address")
}

fn default_interfaces() -> Vec<String> {
    vec!["eth0".to_string()]
}

fn default_node_id() -> String {
    "vps-trafficd-01".to_string()
}

fn default_cycle_months() -> u32 {
    1
}

fn default_state_path() -> PathBuf {
    PathBuf::from("/var/lib/vps-trafficd/state.json")
}
