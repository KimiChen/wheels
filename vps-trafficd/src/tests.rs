use crate::{
    api,
    billing::current_cycle,
    config::{BillingMode, Config},
    next_state_update_delay,
    service::{ConfigUpdate, TrafficService},
};
use axum::{
    body::{to_bytes, Body},
    http::{header, Request, StatusCode},
};
use chrono::{DateTime, FixedOffset};
use std::{fs, path::Path, time::Duration};
use tempfile::TempDir;
use tower::ServiceExt;

fn dt(value: &str) -> DateTime<FixedOffset> {
    DateTime::parse_from_rfc3339(value).unwrap()
}

fn base_config(temp: &TempDir) -> Config {
    Config {
        listen_addr: "0.0.0.0:9733".parse().unwrap(),
        tls_cert_path: temp.path().join("tls/fullchain.pem"),
        tls_key_path: temp.path().join("tls/privkey.pem"),
        tls_auto_restart: true,
        tls_watch_interval_secs: 300,
        tls_restart_settle_secs: 10,
        auth_token: "unit-test-secret".to_string(),
        interfaces: vec!["eth0".to_string()],
        node_id: "node-a".to_string(),
        quota_bytes: 1_000,
        billing_mode: BillingMode::Total,
        cycle_anchor: dt("2026-01-31T08:00:00+08:00"),
        cycle_months: 1,
        state_path: temp.path().join("state.json"),
    }
}

fn write_iface(root: &Path, iface: &str, rx: u64, tx: u64) {
    let stats = root.join(iface).join("statistics");
    fs::create_dir_all(&stats).unwrap();
    fs::write(stats.join("rx_bytes"), rx.to_string()).unwrap();
    fs::write(stats.join("tx_bytes"), tx.to_string()).unwrap();
}

#[test]
fn traffic_cycle_uses_month_end_when_anchor_day_is_missing() {
    let anchor = dt("2026-01-31T08:00:00+08:00");
    let cycle = current_cycle(anchor, 1, dt("2026-02-28T09:00:00+08:00")).unwrap();

    assert_eq!(cycle.start, dt("2026-02-28T08:00:00+08:00"));
    assert_eq!(cycle.end, dt("2026-03-31T08:00:00+08:00"));
}

#[test]
fn background_state_update_uses_earlier_sample_interval_or_cycle_boundary() {
    let now = dt("2026-07-02T13:00:00+08:00");
    let sample_interval = Duration::from_secs(3600);

    assert_eq!(
        next_state_update_delay(dt("2026-07-02T15:00:00+08:00"), now, sample_interval),
        Duration::from_secs(3600)
    );
    assert_eq!(
        next_state_update_delay(dt("2026-07-02T13:10:00+08:00"), now, sample_interval),
        Duration::from_secs(600)
    );
    assert_eq!(
        next_state_update_delay(dt("2026-07-02T12:59:59+08:00"), now, sample_interval),
        Duration::ZERO
    );
}

#[test]
fn tls_is_disabled_when_default_certificate_pair_is_absent() {
    let temp = TempDir::new().unwrap();
    let config = base_config(&temp);

    assert!(!config.tls_enabled());
    config.validate().unwrap();
    let rendered = config.to_commented_toml();
    assert!(rendered.contains("tls_cert_path"));
    assert!(rendered.contains("tls_key_path"));
    assert!(rendered.contains("tls_auto_restart = true"));
    assert!(rendered.contains("tls_watch_interval_secs = 300"));
    assert!(rendered.contains("tls_restart_settle_secs = 10"));
    assert!(rendered.contains("服务会启用 HTTPS"));
    assert!(rendered.contains("Nginx/Caddy/ip-certd"));
}

#[test]
fn tls_certificate_and_key_must_exist_together() {
    let temp = TempDir::new().unwrap();
    let config = base_config(&temp);
    fs::create_dir_all(config.tls_cert_path.parent().unwrap()).unwrap();
    fs::write(&config.tls_cert_path, "not a real certificate").unwrap();

    let error = config.validate().unwrap_err().to_string();
    assert!(error.contains("tls_cert_path and tls_key_path"));
}

#[test]
fn tls_watch_interval_must_be_positive() {
    let temp = TempDir::new().unwrap();
    let mut config = base_config(&temp);
    config.tls_watch_interval_secs = 0;

    let error = config.validate().unwrap_err().to_string();
    assert!(error.contains("tls_watch_interval_secs"));
}

#[test]
fn tls_pair_fingerprint_changes_when_certificate_bundle_is_replaced() {
    let temp = TempDir::new().unwrap();
    let config = base_config(&temp);
    fs::create_dir_all(config.tls_cert_path.parent().unwrap()).unwrap();
    fs::write(&config.tls_cert_path, "fullchain version 1").unwrap();
    fs::write(&config.tls_key_path, "privkey version 1").unwrap();

    let before = crate::tls_pair_fingerprint(&config).unwrap().unwrap();
    fs::write(&config.tls_cert_path, "fullchain version 2").unwrap();
    fs::write(&config.tls_key_path, "privkey version 2").unwrap();
    let after = crate::tls_pair_fingerprint(&config).unwrap().unwrap();

    assert_ne!(before, after);
}

#[test]
fn service_accumulates_growth_and_ignores_counter_reset() {
    let temp = TempDir::new().unwrap();
    let sysfs = temp.path().join("sys");
    let config = base_config(&temp);
    let service =
        TrafficService::with_sysfs_root(config, temp.path().join("config.toml"), sysfs.clone());

    write_iface(&sysfs, "eth0", 100, 200);
    let initial = service.snapshot().unwrap();
    assert_eq!(initial.rx_bytes, 0);
    assert_eq!(initial.tx_bytes, 0);

    write_iface(&sysfs, "eth0", 180, 260);
    let grown = service.snapshot().unwrap();
    assert_eq!(grown.rx_bytes, 80);
    assert_eq!(grown.tx_bytes, 60);
    assert_eq!(grown.used_bytes, 140);
    assert_eq!(grown.remaining_bytes, 860);

    write_iface(&sysfs, "eth0", 10, 20);
    let reset = service.snapshot().unwrap();
    assert_eq!(reset.rx_bytes, 80);
    assert_eq!(reset.tx_bytes, 60);

    write_iface(&sysfs, "eth0", 25, 45);
    let after_reset = service.snapshot().unwrap();
    assert_eq!(after_reset.rx_bytes, 95);
    assert_eq!(after_reset.tx_bytes, 85);
}

#[test]
fn service_uses_larger_direction_for_max_billing_mode() {
    let temp = TempDir::new().unwrap();
    let sysfs = temp.path().join("sys");
    let mut config = base_config(&temp);
    config.billing_mode = BillingMode::Max;
    let service =
        TrafficService::with_sysfs_root(config, temp.path().join("config.toml"), sysfs.clone());

    write_iface(&sysfs, "eth0", 100, 200);
    let initial = service.snapshot().unwrap();
    assert_eq!(initial.used_bytes, 0);
    assert_eq!(initial.billing_mode, "max");

    write_iface(&sysfs, "eth0", 180, 260);
    let grown = service.snapshot().unwrap();
    assert_eq!(grown.rx_bytes, 80);
    assert_eq!(grown.tx_bytes, 60);
    assert_eq!(grown.used_bytes, 80);
    assert_eq!(grown.remaining_bytes, 920);
}

#[test]
fn used_calibration_does_not_change_reported_rx_or_tx_bytes() {
    let temp = TempDir::new().unwrap();
    let sysfs = temp.path().join("sys");
    let config = base_config(&temp);
    let service =
        TrafficService::with_sysfs_root(config, temp.path().join("config.toml"), sysfs.clone());

    write_iface(&sysfs, "eth0", 100, 200);
    service.snapshot().unwrap();
    write_iface(&sysfs, "eth0", 180, 260);
    let before = service.snapshot().unwrap();
    assert_eq!(before.rx_bytes, 80);
    assert_eq!(before.tx_bytes, 60);
    assert_eq!(before.used_bytes, 140);

    service
        .update_config(ConfigUpdate {
            traffic_cycle_anchor: dt("2026-01-31T08:00:00+08:00"),
            traffic_cycle_months: 1,
            quota_bytes: 1_000,
            billing_mode: BillingMode::Total,
            current_cycle_used_bytes: Some(512),
        })
        .unwrap();

    let calibrated = service.snapshot().unwrap();
    assert_eq!(calibrated.rx_bytes, 80);
    assert_eq!(calibrated.tx_bytes, 60);
    assert_eq!(calibrated.used_bytes, 512);
    assert_eq!(calibrated.remaining_bytes, 488);
}

#[tokio::test]
async fn index_page_prompts_for_token() {
    let temp = TempDir::new().unwrap();
    let sysfs = temp.path().join("sys");
    let config = base_config(&temp);
    write_iface(&sysfs, "eth0", 100, 200);

    let service = std::sync::Arc::new(TrafficService::with_sysfs_root(
        config,
        temp.path().join("config.toml"),
        sysfs,
    ));
    let app = api::router(service);

    let response = app
        .oneshot(Request::builder().uri("/").body(Body::empty()).unwrap())
        .await
        .unwrap();

    assert_eq!(response.status(), StatusCode::OK);
    let content_type = response
        .headers()
        .get(header::CONTENT_TYPE)
        .and_then(|value| value.to_str().ok())
        .unwrap();
    assert!(content_type.starts_with("text/html"));

    let body = to_bytes(response.into_body(), usize::MAX).await.unwrap();
    let body = String::from_utf8(body.to_vec()).unwrap();
    assert!(body.contains("window.prompt"));
    assert!(body.contains("/api/v1/traffic"));
    assert!(body.contains("/api/v1/config"));
    assert!(!body.contains("Billing start"));
    assert!(!body.contains("billing_cycle_anchor"));
    assert!(body.contains("Billing mode"));
    assert!(body.contains("billing-mode"));
    assert!(body.contains("billing_mode"));
    assert!(body.contains("Max of RX/TX"));
    assert!(body.contains("Current cycle used"));
    assert!(body.contains("current_cycle_used_bytes"));
    assert!(body.contains("normalizeLocalDateTimeInput"));
    assert!(body.contains("return `${trimmed}:00`;"));
    assert!(body.contains("const units = [\"B\", \"K\", \"M\", \"G\", \"T\", \"P\"]"));
    assert!(body.contains("size.toFixed(2)"));
    assert!(!body.contains("data.usage_ratio"));
    assert!(!body.contains("[\"Usage\""));
    assert!(body.contains("setByteInput(config.quota_bytes, quotaValueEl, quotaUnitEl, \"G\")"));
    assert!(body
        .contains("setByteInput(data.used_bytes, currentUsedValueEl, currentUsedUnitEl, \"G\")"));
}

#[tokio::test]
async fn config_endpoint_accepts_minute_precision_datetime_local_value() {
    let temp = TempDir::new().unwrap();
    let sysfs = temp.path().join("sys");
    let config_path = temp.path().join("config.toml");
    let config = base_config(&temp);
    write_iface(&sysfs, "eth0", 100, 200);

    let service = std::sync::Arc::new(TrafficService::with_sysfs_root(
        config,
        config_path.clone(),
        sysfs,
    ));
    let app = api::router(service);

    let payload = r#"{
        "traffic_cycle_anchor":"2026-06-21T00:00+08:00",
        "traffic_cycle_months":1,
        "quota_bytes":6597069766656,
        "billing_mode":"total",
        "current_cycle_used_bytes":54567559496
    }"#;
    let response = app
        .oneshot(
            Request::builder()
                .method("PUT")
                .uri("/api/v1/config")
                .header(header::AUTHORIZATION, "Bearer unit-test-secret")
                .header(header::CONTENT_TYPE, "application/json")
                .body(Body::from(payload))
                .unwrap(),
        )
        .await
        .unwrap();

    assert_eq!(response.status(), StatusCode::OK);
    let saved = fs::read_to_string(&config_path).unwrap();
    assert!(saved.contains("cycle_anchor = \"2026-06-21T00:00:00+08:00\""));
    assert!(saved.contains("quota_bytes = 6597069766656"));
}

#[tokio::test]
async fn config_endpoint_updates_config_file_runtime_quota_and_used_traffic() {
    let temp = TempDir::new().unwrap();
    let sysfs = temp.path().join("sys");
    let config_path = temp.path().join("config.toml");
    let config = base_config(&temp);
    write_iface(&sysfs, "eth0", 100, 200);

    let service = std::sync::Arc::new(TrafficService::with_sysfs_root(
        config,
        config_path.clone(),
        sysfs,
    ));
    let app = api::router(service);

    let payload = r#"{
        "traffic_cycle_anchor":"2026-02-01T08:00:00+08:00",
        "traffic_cycle_months":1,
        "quota_bytes":2048,
        "billing_mode":"max",
        "current_cycle_used_bytes":512
    }"#;
    let response = app
        .clone()
        .oneshot(
            Request::builder()
                .method("PUT")
                .uri("/api/v1/config")
                .header(header::AUTHORIZATION, "Bearer unit-test-secret")
                .header(header::CONTENT_TYPE, "application/json")
                .body(Body::from(payload))
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::OK);

    let saved = fs::read_to_string(&config_path).unwrap();
    assert!(!saved.contains("billing_cycle"));
    assert!(saved.contains("流量充值周期锚点"));
    assert!(saved.contains("quota_bytes = 2048"));
    assert!(saved.contains("billing_mode = \"max\""));
    assert!(saved.contains("max 表示取接收/发送较大值"));

    let response = app
        .oneshot(
            Request::builder()
                .uri("/api/v1/traffic")
                .header(header::AUTHORIZATION, "Bearer unit-test-secret")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::OK);
    let body = to_bytes(response.into_body(), usize::MAX).await.unwrap();
    let body: serde_json::Value = serde_json::from_slice(&body).unwrap();
    assert_eq!(body["quota_bytes"], 2048);
    assert_eq!(body["billing_mode"], "max");
    assert_eq!(body["rx_bytes"], 0);
    assert_eq!(body["tx_bytes"], 0);
    assert_eq!(body["used_bytes"], 512);
    assert_eq!(body["remaining_bytes"], 1536);
    assert!(body.get("usage_ratio").is_none());
}

#[tokio::test]
async fn traffic_endpoint_requires_bearer_token() {
    let temp = TempDir::new().unwrap();
    let sysfs = temp.path().join("sys");
    let config = base_config(&temp);
    write_iface(&sysfs, "eth0", 100, 200);

    let service = std::sync::Arc::new(TrafficService::with_sysfs_root(
        config,
        temp.path().join("config.toml"),
        sysfs,
    ));
    let app = api::router(service);

    let response = app
        .clone()
        .oneshot(
            Request::builder()
                .uri("/api/v1/traffic")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::UNAUTHORIZED);

    let response = app
        .oneshot(
            Request::builder()
                .uri("/api/v1/traffic")
                .header(header::AUTHORIZATION, "Bearer unit-test-secret")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::OK);
}
