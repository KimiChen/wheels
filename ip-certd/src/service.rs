use crate::{
    acme::manager::AcmeManager,
    bundle::{self, CertificateBundle},
    cert_store::{CertStore, CertificateMetadata},
    cloudflare::{CloudflareDns, DnsProvider},
    config::Config,
    iplist::IpList,
    whitelist::{ipv4_is_allowed, Whitelist},
};
use anyhow::Result;
use bytes::Bytes;
use chrono::{SecondsFormat, Utc};
use std::{
    collections::HashMap,
    net::{IpAddr, Ipv4Addr},
    sync::Arc,
    time::{Duration, Instant},
};
use thiserror::Error;
use tokio::sync::Mutex;

#[derive(Clone)]
pub struct IpCertService {
    config: Config,
    whitelist: Whitelist,
    dns: Arc<dyn DnsProvider>,
    acme: AcmeManager,
    store: CertStore,
    limiter: Arc<RateLimiter>,
    ip_locks: Arc<Mutex<HashMap<Ipv4Addr, Arc<Mutex<()>>>>>,
}

#[derive(Debug)]
pub struct BundleResponse {
    pub archive: Bytes,
    pub filename: String,
    pub hostname: String,
    pub ip: Ipv4Addr,
    pub not_after: String,
}

#[derive(Debug, Error)]
pub enum ServiceError {
    #[error("{0}")]
    BadRequest(String),
    #[error("{0}")]
    Forbidden(String),
    #[error("{0}")]
    NotFound(String),
    #[error("{0}")]
    TooManyRequests(String),
    #[error("{0}")]
    NotImplemented(String),
    #[error(transparent)]
    Internal(#[from] anyhow::Error),
}

impl IpCertService {
    pub fn from_config(config: Config, iplist: IpList) -> Result<Self> {
        let token = config.cloudflare_api_token()?;
        let dns = Arc::new(CloudflareDns::new(&config.cloudflare, token)?);
        Self::new(config, iplist, dns)
    }

    pub fn new(config: Config, iplist: IpList, dns: Arc<dyn DnsProvider>) -> Result<Self> {
        let whitelist = Whitelist::new(iplist.ips().iter().copied(), config.domain.clone());
        let store = CertStore::new(config.acme.storage.clone());
        let acme = AcmeManager::new(config.acme.clone(), dns.clone());
        let limiter = Arc::new(RateLimiter::new(
            config.security.rate_limit_per_ip_per_minute,
            Duration::from_secs(60),
        ));
        Ok(Self {
            config,
            whitelist,
            dns,
            acme,
            store,
            limiter,
            ip_locks: Arc::new(Mutex::new(HashMap::new())),
        })
    }

    pub fn config(&self) -> &Config {
        &self.config
    }

    pub async fn certificate_bundle(
        &self,
        requested_ip: &str,
        source_ip: IpAddr,
    ) -> Result<BundleResponse, ServiceError> {
        let ip = parse_requested_ip(requested_ip, self.config.security.allow_private_ip)?;
        self.limiter.check(source_ip).await?;

        if !self.whitelist.contains(&ip) {
            return Err(ServiceError::NotFound(format!("{ip} is not whitelisted")));
        }
        if source_ip != IpAddr::V4(ip) {
            return Err(ServiceError::Forbidden(format!(
                "source IP must match requested IP {ip}"
            )));
        }

        let hostname = self.whitelist.hostname_for(ip);
        let ip_lock = self.lock_for_ip(ip).await;
        let _guard = ip_lock.lock().await;

        self.dns
            .upsert_a(&hostname, &ip.to_string(), self.config.ttl)
            .await?;

        let stored = self
            .ensure_certificate(ip, &hostname, &source_ip.to_string())
            .await?;
        let CertificateBundle { archive, sha256 } = bundle::create_bundle(&stored)?;
        let metadata = self
            .store
            .update_request_metadata(ip, &source_ip.to_string(), &sha256)?;

        Ok(BundleResponse {
            archive: Bytes::from(archive),
            filename: format!("{ip}.tar.gz"),
            hostname,
            ip,
            not_after: rfc3339(&metadata),
        })
    }

    async fn ensure_certificate(
        &self,
        ip: Ipv4Addr,
        hostname: &str,
        source_ip: &str,
    ) -> Result<crate::cert_store::StoredCertificate, ServiceError> {
        let stored = self.store.load(ip)?;
        if let Some(stored) = stored {
            if !stored.renewal_due(self.config.acme.renew_before_days) {
                return Ok(stored);
            }
        }

        if !self.acme.enabled() {
            return Err(ServiceError::NotImplemented(
                "certificate is missing or due for renewal, and ACME is disabled".to_string(),
            ));
        }

        let material = self
            .acme
            .issue_or_renew(ip, hostname, source_ip)
            .await
            .map_err(ServiceError::Internal)?;
        self.store.write_material(ip, material)?;
        self.store
            .load(ip)?
            .ok_or_else(|| ServiceError::Internal(anyhow::anyhow!("certificate write failed")))
    }

    async fn lock_for_ip(&self, ip: Ipv4Addr) -> Arc<Mutex<()>> {
        let mut locks = self.ip_locks.lock().await;
        locks
            .entry(ip)
            .or_insert_with(|| Arc::new(Mutex::new(())))
            .clone()
    }
}

fn parse_requested_ip(value: &str, allow_private_ip: bool) -> Result<Ipv4Addr, ServiceError> {
    let ip = value.parse::<Ipv4Addr>().map_err(|_| {
        ServiceError::BadRequest("request path must contain a valid IPv4 address".to_string())
    })?;
    if !ipv4_is_allowed(ip, allow_private_ip) {
        return Err(ServiceError::BadRequest(format!(
            "{ip} is not an allowed public IPv4 address"
        )));
    }
    Ok(ip)
}

fn rfc3339(metadata: &CertificateMetadata) -> String {
    metadata
        .not_after
        .with_timezone(&Utc)
        .to_rfc3339_opts(SecondsFormat::Secs, true)
}

struct RateLimiter {
    limit: u32,
    window: Duration,
    buckets: Mutex<HashMap<IpAddr, RateBucket>>,
}

struct RateBucket {
    started_at: Instant,
    count: u32,
}

impl RateLimiter {
    fn new(limit: u32, window: Duration) -> Self {
        Self {
            limit,
            window,
            buckets: Mutex::new(HashMap::new()),
        }
    }

    async fn check(&self, ip: IpAddr) -> Result<(), ServiceError> {
        if self.limit == 0 {
            return Ok(());
        }

        let now = Instant::now();
        let mut buckets = self.buckets.lock().await;
        let bucket = buckets.entry(ip).or_insert(RateBucket {
            started_at: now,
            count: 0,
        });

        if now.duration_since(bucket.started_at) >= self.window {
            bucket.started_at = now;
            bucket.count = 0;
        }

        if bucket.count >= self.limit {
            return Err(ServiceError::TooManyRequests(
                "rate limit exceeded".to_string(),
            ));
        }

        bucket.count += 1;
        Ok(())
    }
}
