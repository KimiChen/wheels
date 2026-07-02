use crate::{cert_store::CertificateMaterial, cloudflare::DnsProvider, config::AcmeConfig};
use anyhow::{bail, Result};
use std::{net::Ipv4Addr, sync::Arc};

#[derive(Clone)]
pub struct AcmeManager {
    config: AcmeConfig,
    #[allow(dead_code)]
    dns: Arc<dyn DnsProvider>,
}

impl AcmeManager {
    pub fn new(config: AcmeConfig, dns: Arc<dyn DnsProvider>) -> Self {
        Self { config, dns }
    }

    pub fn enabled(&self) -> bool {
        self.config.enabled
    }

    pub async fn issue_or_renew(
        &self,
        _ip: Ipv4Addr,
        _hostname: &str,
        _source_ip: &str,
    ) -> Result<CertificateMaterial> {
        bail!("ACME DNS-01 issuance is not implemented yet")
    }
}
