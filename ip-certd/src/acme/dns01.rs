use crate::cloudflare::DnsProvider;
use anyhow::Result;
use std::{sync::Arc, time::Duration};

#[derive(Clone)]
pub struct Dns01Challenge {
    dns: Arc<dyn DnsProvider>,
    ttl: u32,
    propagation_timeout: Duration,
}

impl Dns01Challenge {
    pub fn new(dns: Arc<dyn DnsProvider>, ttl: u32, propagation_timeout: Duration) -> Self {
        Self {
            dns,
            ttl,
            propagation_timeout,
        }
    }

    pub async fn present(&self, name: &str, value: &str) -> Result<()> {
        self.dns.upsert_txt(name, value, self.ttl).await
    }

    pub async fn cleanup(&self, name: &str, value: &str) -> Result<()> {
        self.dns.delete_txt(name, value).await
    }

    pub async fn wait_for_propagation(&self) {
        let wait = self.propagation_timeout.min(Duration::from_secs(5));
        tokio::time::sleep(wait).await;
    }
}
