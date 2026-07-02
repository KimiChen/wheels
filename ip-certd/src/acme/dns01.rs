use crate::cloudflare::DnsProvider;
use anyhow::{bail, Result};
use serde::Deserialize;
use std::{sync::Arc, time::Duration};
use tokio::time::{sleep, Instant};

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

    pub async fn wait_for_propagation(&self, name: &str, value: &str) -> Result<()> {
        let client = reqwest::Client::new();
        let deadline = Instant::now() + self.propagation_timeout;
        let mut delay = Duration::from_secs(2);

        loop {
            if txt_visible(&client, name, value).await.unwrap_or(false) {
                return Ok(());
            }
            if Instant::now() + delay > deadline {
                bail!("DNS-01 TXT record did not propagate before timeout");
            }
            sleep(delay).await;
            delay = Duration::from_secs(5);
        }
    }
}

async fn txt_visible(client: &reqwest::Client, name: &str, value: &str) -> Result<bool> {
    let response = client
        .get("https://cloudflare-dns.com/dns-query")
        .header(reqwest::header::ACCEPT, "application/dns-json")
        .query(&[("name", name), ("type", "TXT")])
        .send()
        .await?;
    if !response.status().is_success() {
        return Ok(false);
    }

    let body: DnsJsonResponse = response.json().await?;
    Ok(body
        .answer
        .unwrap_or_default()
        .iter()
        .any(|answer| txt_answer_matches(&answer.data, value)))
}

fn txt_answer_matches(answer: &str, value: &str) -> bool {
    answer == value || answer.contains(value)
}

#[derive(Debug, Deserialize)]
struct DnsJsonResponse {
    #[serde(rename = "Answer")]
    answer: Option<Vec<DnsJsonAnswer>>,
}

#[derive(Debug, Deserialize)]
struct DnsJsonAnswer {
    data: String,
}
