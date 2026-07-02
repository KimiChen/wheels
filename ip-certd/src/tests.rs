use crate::{
    api,
    cert_store::{CertStore, CertificateMaterial, CertificateMetadata},
    cloudflare::DnsProvider,
    config::{AcmeConfig, CloudflareConfig, Config, SecurityConfig, ServerConfig},
    iplist::IpList,
    real_ip,
    service::IpCertService,
    whitelist::{ipv4_is_allowed, Whitelist},
};
use anyhow::Result;
use async_trait::async_trait;
use axum::{
    body::{to_bytes, Body},
    extract::connect_info::ConnectInfo,
    http::{header, Request, StatusCode},
};
use chrono::{Duration, Utc};
use std::{
    net::{IpAddr, Ipv4Addr, SocketAddr},
    path::PathBuf,
    sync::{Arc, Mutex},
};
use tempfile::TempDir;
use tower::ServiceExt;

#[test]
fn whitelist_generates_ip_hostname() {
    let ip = Ipv4Addr::new(52, 0, 56, 137);
    let whitelist = Whitelist::new([ip], "ip.example.com.");

    assert!(whitelist.contains(&ip));
    assert_eq!(whitelist.hostname_for(ip), "52.0.56.137.ip.example.com");
}

#[test]
fn private_ips_are_rejected_by_default() {
    assert!(ipv4_is_allowed(Ipv4Addr::new(52, 0, 56, 137), false));
    assert!(!ipv4_is_allowed(Ipv4Addr::new(10, 0, 0, 1), false));
    assert!(ipv4_is_allowed(Ipv4Addr::new(10, 0, 0, 1), true));
}

#[test]
fn real_ip_header_is_only_trusted_from_configured_proxy() {
    let server = ServerConfig {
        trusted_proxies: vec![IpAddr::V4(Ipv4Addr::LOCALHOST)],
        ..ServerConfig::default()
    };
    let mut headers = axum::http::HeaderMap::new();
    headers.insert("x-real-ip", "52.0.56.137".parse().unwrap());

    let resolved = real_ip::resolve(&headers, IpAddr::V4(Ipv4Addr::LOCALHOST), &server).unwrap();
    assert_eq!(resolved, IpAddr::V4(Ipv4Addr::new(52, 0, 56, 137)));

    let direct =
        real_ip::resolve(&headers, IpAddr::V4(Ipv4Addr::new(8, 8, 8, 8)), &server).unwrap();
    assert_eq!(direct, IpAddr::V4(Ipv4Addr::new(8, 8, 8, 8)));
}

#[tokio::test]
async fn bundle_endpoint_returns_existing_certificate_archive() {
    let temp = TempDir::new().unwrap();
    let ip = Ipv4Addr::new(52, 0, 56, 137);
    let config = test_config(temp.path().join("certs"));
    let store = CertStore::new(config.acme.storage.clone());
    write_test_certificate(&store, ip, "52.0.56.137.ip.example.com");

    let dns = Arc::new(MockDns::default());
    let service =
        Arc::new(IpCertService::new(config, IpList::from_ips(vec![ip]), dns.clone()).unwrap());
    let app = api::router(service);
    let mut request = Request::builder()
        .method("POST")
        .uri("/api/v1/certificates/52.0.56.137/bundle")
        .header("x-real-ip", "52.0.56.137")
        .body(Body::empty())
        .unwrap();
    request
        .extensions_mut()
        .insert(ConnectInfo(SocketAddr::from(([127, 0, 0, 1], 40000))));

    let response = app.oneshot(request).await.unwrap();

    assert_eq!(response.status(), StatusCode::OK);
    assert_eq!(
        response.headers().get(header::CONTENT_TYPE).unwrap(),
        "application/gzip"
    );
    assert_eq!(
        response.headers().get("x-certificate-hostname").unwrap(),
        "52.0.56.137.ip.example.com"
    );
    assert_eq!(dns.a_records.lock().unwrap().len(), 1);

    let body = to_bytes(response.into_body(), usize::MAX).await.unwrap();
    assert!(!body.is_empty());
}

#[tokio::test]
async fn bundle_endpoint_rejects_source_ip_mismatch() {
    let temp = TempDir::new().unwrap();
    let ip = Ipv4Addr::new(52, 0, 56, 137);
    let config = test_config(temp.path().join("certs"));
    let service = Arc::new(
        IpCertService::new(
            config,
            IpList::from_ips(vec![ip]),
            Arc::new(MockDns::default()),
        )
        .unwrap(),
    );
    let app = api::router(service);
    let mut request = Request::builder()
        .method("POST")
        .uri("/api/v1/certificates/52.0.56.137/bundle")
        .header("x-real-ip", "8.8.8.8")
        .body(Body::empty())
        .unwrap();
    request
        .extensions_mut()
        .insert(ConnectInfo(SocketAddr::from(([127, 0, 0, 1], 40000))));

    let response = app.oneshot(request).await.unwrap();

    assert_eq!(response.status(), StatusCode::FORBIDDEN);
}

fn test_config(storage: PathBuf) -> Config {
    Config {
        domain: "ip.example.com".to_string(),
        ttl: 60,
        server: ServerConfig {
            listen: "127.0.0.1:9735".parse().unwrap(),
            public_base_url: "https://api.ip.example.com".to_string(),
            storage: storage.parent().unwrap().to_path_buf(),
            real_ip_header: "x-real-ip".to_string(),
            trusted_proxies: vec![IpAddr::V4(Ipv4Addr::LOCALHOST)],
        },
        cloudflare: CloudflareConfig {
            zone_id: "zone".to_string(),
            api_token_env: "CLOUDFLARE_API_TOKEN".to_string(),
        },
        acme: AcmeConfig {
            enabled: false,
            email: "admin@example.com".to_string(),
            storage,
            ..AcmeConfig::default()
        },
        security: SecurityConfig {
            allow_private_ip: false,
            rate_limit_per_ip_per_minute: 0,
        },
    }
}

fn write_test_certificate(store: &CertStore, ip: Ipv4Addr, hostname: &str) {
    let now = Utc::now();
    let material = CertificateMaterial {
        fullchain_pem: b"fullchain".to_vec(),
        privkey_pem: b"privkey".to_vec(),
        cert_pem: b"cert".to_vec(),
        chain_pem: b"chain".to_vec(),
        metadata: CertificateMetadata {
            ip: ip.to_string(),
            hostname: hostname.to_string(),
            certificate_path: None,
            not_before: now - Duration::days(1),
            not_after: now + Duration::days(60),
            issued_at: now,
            renewed_at: now,
            last_requested_at: None,
            last_source_ip: None,
            last_bundle_sha256: None,
        },
    };
    store.write_material(ip, material).unwrap();
}

#[derive(Default)]
struct MockDns {
    a_records: Mutex<Vec<(String, String, u32)>>,
}

#[async_trait]
impl DnsProvider for MockDns {
    async fn upsert_a(&self, name: &str, ip: &str, ttl: u32) -> Result<()> {
        self.a_records
            .lock()
            .unwrap()
            .push((name.to_string(), ip.to_string(), ttl));
        Ok(())
    }

    async fn upsert_txt(&self, _name: &str, _value: &str, _ttl: u32) -> Result<()> {
        Ok(())
    }

    async fn delete_txt(&self, _name: &str, _value: &str) -> Result<()> {
        Ok(())
    }

    async fn upsert_caa(&self, _name: &str, _value: &str, _ttl: u32) -> Result<()> {
        Ok(())
    }
}
