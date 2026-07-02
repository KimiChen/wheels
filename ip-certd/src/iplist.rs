use crate::whitelist::ipv4_is_allowed;
use anyhow::{bail, Context, Result};
use serde::Deserialize;
use std::{collections::HashSet, fs, net::Ipv4Addr, path::Path};

#[derive(Clone, Debug)]
pub struct IpList {
    ips: Vec<Ipv4Addr>,
}

#[derive(Debug, Deserialize)]
struct RawIpList {
    #[serde(default)]
    ips: Vec<String>,
}

impl IpList {
    pub fn load(path: &Path) -> Result<Self> {
        let content = fs::read_to_string(path)
            .with_context(|| format!("failed to read iplist file {}", path.display()))?;
        let raw: RawIpList = toml::from_str(&content)
            .with_context(|| format!("failed to parse iplist file {}", path.display()))?;
        let mut ips = Vec::with_capacity(raw.ips.len());
        for value in raw.ips {
            let ip = value
                .parse::<Ipv4Addr>()
                .with_context(|| format!("invalid IPv4 address in iplist: {value}"))?;
            ips.push(ip);
        }
        Ok(Self { ips })
    }

    pub fn from_ips(ips: Vec<Ipv4Addr>) -> Self {
        Self { ips }
    }

    pub fn validate(&self, allow_private_ip: bool) -> Result<()> {
        if self.ips.is_empty() {
            bail!("iplist.ips must contain at least one IPv4 address");
        }

        let mut seen = HashSet::new();
        for ip in &self.ips {
            if !seen.insert(*ip) {
                bail!("duplicate IP in iplist: {ip}");
            }
            if !ipv4_is_allowed(*ip, allow_private_ip) {
                bail!("{ip} is not a public IPv4 address; set security.allow_private_ip=true for private test networks");
            }
        }
        Ok(())
    }

    pub fn ips(&self) -> &[Ipv4Addr] {
        &self.ips
    }
}
