use crate::config::CloudflareConfig;
use anyhow::{bail, Context, Result};
use async_trait::async_trait;
use reqwest::{Client, StatusCode};
use serde::{Deserialize, Serialize};

const API_BASE: &str = "https://api.cloudflare.com/client/v4";

#[allow(dead_code)]
#[async_trait]
pub trait DnsProvider: Send + Sync {
    async fn upsert_a(&self, name: &str, ip: &str, ttl: u32) -> Result<()>;
    async fn upsert_txt(&self, name: &str, value: &str, ttl: u32) -> Result<()>;
    async fn delete_txt(&self, name: &str, value: &str) -> Result<()>;
    async fn upsert_caa(&self, name: &str, value: &str, ttl: u32) -> Result<()>;
}

#[derive(Clone)]
pub struct CloudflareDns {
    zone_id: String,
    api_token: String,
    client: Client,
}

impl CloudflareDns {
    pub fn new(config: &CloudflareConfig, api_token: String) -> Result<Self> {
        if api_token.trim().is_empty() {
            bail!("Cloudflare API token must not be empty");
        }
        Ok(Self {
            zone_id: config.zone_id.trim().to_string(),
            api_token,
            client: Client::new(),
        })
    }

    async fn upsert_record(&self, payload: DnsRecordPayload) -> Result<()> {
        let records = self
            .list_records(&payload.record_type, &payload.name)
            .await
            .with_context(|| {
                format!(
                    "failed to find existing Cloudflare {} record for {}",
                    payload.record_type, payload.name
                )
            })?;

        if let Some(record) = records
            .iter()
            .find(|record| record.content == payload.content)
            .or_else(|| records.first())
        {
            let _: CloudflareEnvelope<serde_json::Value> = self
                .request(
                    self.client
                        .put(self.record_url(Some(&record.id)))
                        .json(&payload),
                )
                .await
                .with_context(|| {
                    format!(
                        "failed to update Cloudflare {} record for {}",
                        payload.record_type, payload.name
                    )
                })?;
        } else {
            let _: CloudflareEnvelope<serde_json::Value> = self
                .request(self.client.post(self.record_url(None)).json(&payload))
                .await
                .with_context(|| {
                    format!(
                        "failed to create Cloudflare {} record for {}",
                        payload.record_type, payload.name
                    )
                })?;
        }

        Ok(())
    }

    async fn list_records(&self, record_type: &str, name: &str) -> Result<Vec<DnsRecord>> {
        let envelope: CloudflareEnvelope<Vec<DnsRecord>> = self
            .request(
                self.client
                    .get(self.record_url(None))
                    .query(&[("type", record_type), ("name", name)]),
            )
            .await?;
        Ok(envelope.result)
    }

    async fn request<T>(&self, builder: reqwest::RequestBuilder) -> Result<CloudflareEnvelope<T>>
    where
        T: for<'de> Deserialize<'de>,
    {
        let response = builder.bearer_auth(&self.api_token).send().await?;
        let status = response.status();
        let text = response.text().await?;

        if !status.is_success() {
            bail!(
                "Cloudflare API returned HTTP {status}: {}",
                trim_body(&text)
            );
        }

        let envelope: CloudflareEnvelope<T> =
            serde_json::from_str(&text).context("failed to parse Cloudflare API response")?;
        if !envelope.success {
            bail!("Cloudflare API error: {}", envelope.error_summary());
        }
        Ok(envelope)
    }

    fn record_url(&self, id: Option<&str>) -> String {
        match id {
            Some(id) => format!("{API_BASE}/zones/{}/dns_records/{id}", self.zone_id),
            None => format!("{API_BASE}/zones/{}/dns_records", self.zone_id),
        }
    }
}

#[async_trait]
impl DnsProvider for CloudflareDns {
    async fn upsert_a(&self, name: &str, ip: &str, ttl: u32) -> Result<()> {
        self.upsert_record(DnsRecordPayload {
            record_type: "A".to_string(),
            name: name.to_string(),
            content: ip.to_string(),
            ttl,
            proxied: Some(false),
        })
        .await
    }

    async fn upsert_txt(&self, name: &str, value: &str, ttl: u32) -> Result<()> {
        self.upsert_record(DnsRecordPayload {
            record_type: "TXT".to_string(),
            name: name.to_string(),
            content: value.to_string(),
            ttl,
            proxied: None,
        })
        .await
    }

    async fn delete_txt(&self, name: &str, value: &str) -> Result<()> {
        let records = self.list_records("TXT", name).await?;
        for record in records.into_iter().filter(|record| record.content == value) {
            let _: CloudflareEnvelope<serde_json::Value> = self
                .request(self.client.delete(self.record_url(Some(&record.id))))
                .await
                .with_context(|| format!("failed to delete Cloudflare TXT record for {name}"))?;
        }
        Ok(())
    }

    async fn upsert_caa(&self, name: &str, value: &str, ttl: u32) -> Result<()> {
        self.upsert_record(DnsRecordPayload {
            record_type: "CAA".to_string(),
            name: name.to_string(),
            content: value.to_string(),
            ttl,
            proxied: None,
        })
        .await
    }
}

#[derive(Debug, Deserialize)]
struct CloudflareEnvelope<T> {
    success: bool,
    #[serde(default)]
    errors: Vec<CloudflareApiError>,
    result: T,
}

impl<T> CloudflareEnvelope<T> {
    fn error_summary(&self) -> String {
        if self.errors.is_empty() {
            return "unknown error".to_string();
        }
        self.errors
            .iter()
            .map(|error| match error.code {
                Some(code) => format!("{code}: {}", error.message),
                None => error.message.clone(),
            })
            .collect::<Vec<_>>()
            .join("; ")
    }
}

#[derive(Debug, Deserialize)]
struct CloudflareApiError {
    code: Option<u64>,
    message: String,
}

#[derive(Debug, Deserialize)]
struct DnsRecord {
    id: String,
    content: String,
}

#[derive(Debug, Serialize)]
struct DnsRecordPayload {
    #[serde(rename = "type")]
    record_type: String,
    name: String,
    content: String,
    ttl: u32,
    #[serde(skip_serializing_if = "Option::is_none")]
    proxied: Option<bool>,
}

fn trim_body(body: &str) -> String {
    const LIMIT: usize = 500;
    if body.chars().count() <= LIMIT {
        body.to_string()
    } else {
        let prefix = body.chars().take(LIMIT).collect::<String>();
        format!("{prefix}...")
    }
}

#[allow(dead_code)]
fn _status_is_rate_limit(status: StatusCode) -> bool {
    status == StatusCode::TOO_MANY_REQUESTS
}
