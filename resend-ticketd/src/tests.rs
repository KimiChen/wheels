use std::{fs, path::Path};

use argon2::{
    password_hash::{rand_core::OsRng, PasswordHasher, SaltString},
    Argon2,
};
use tempfile::TempDir;

use crate::{
    config::Config,
    db::{Database, NewInboundMessage, TicketStatus},
};

#[test]
fn config_rejects_example_values() {
    let dir = TempDir::new().unwrap();
    let config_path = dir.path().join(".env");
    fs::write(
        &config_path,
        r#"RESEND_TICKETD_LISTEN_ADDR=0.0.0.0:9734
RESEND_TICKETD_PUBLIC_BASE_URL=https://tickets.example.com:9734
DATABASE_URL=sqlite:///tmp/resend-ticketd.db
RESEND_API_KEY=re_xxxxxxxxx
RESEND_WEBHOOK_SECRET=whsec_xxxxxxxxx
RESEND_FROM="Support <support@example.com>"
SUPPORT_ADDRESSES=support@example.com
TLS_CERT_PATH=/tmp/fullchain.pem
TLS_KEY_PATH=/tmp/privkey.pem
ADMIN_USERNAME=admin
ADMIN_PASSWORD_HASH=$argon2id$v=19$...
ACME_EMAIL=admin@example.com
ACME_DOMAIN=tickets.example.com
ACME_LEGO_PATH=/usr/local/bin/lego
ACME_DNS_PROVIDER=cloudflare
ACME_DNS_ENV_FILE=/etc/resend-ticketd/acme.env
ACME_CERT_DIR=/etc/resend-ticketd/tls
"#,
    )
    .unwrap();

    let config = Config::load(config_path.to_str().unwrap()).unwrap();
    assert!(config.validate_for_serve().is_err());
}

#[tokio::test]
async fn inbound_message_creates_reopens_and_matches_ticket() {
    let database = Database::open("sqlite::memory:").unwrap();
    database.migrate().await.unwrap();

    let ticket = database
        .upsert_inbound_message(NewInboundMessage {
            resend_email_id: "email_1".to_string(),
            message_id: Some("<m1@example.com>".to_string()),
            in_reply_to: None,
            references: None,
            from_email: "customer@example.com".to_string(),
            to_email: "support@example.com".to_string(),
            subject: "Help".to_string(),
            body_text: "Need help".to_string(),
            headers_json: "{}".to_string(),
            attachments: vec![],
        })
        .await
        .unwrap();

    database.close_ticket(ticket.id).await.unwrap();
    let matched = database
        .upsert_inbound_message(NewInboundMessage {
            resend_email_id: "email_2".to_string(),
            message_id: Some("<m2@example.com>".to_string()),
            in_reply_to: None,
            references: None,
            from_email: "customer@example.com".to_string(),
            to_email: "support@example.com".to_string(),
            subject: format!("Re: [{}] Help", ticket.number),
            body_text: "Still need help".to_string(),
            headers_json: "{}".to_string(),
            attachments: vec![],
        })
        .await
        .unwrap();

    assert_eq!(matched.id, ticket.id);
    assert_eq!(matched.status, TicketStatus::Open);
}

#[tokio::test]
async fn webhook_events_are_idempotent() {
    let database = Database::open("sqlite::memory:").unwrap();
    database.migrate().await.unwrap();

    assert!(database
        .try_begin_webhook_event("evt_1", "email.received")
        .await
        .unwrap());
    assert!(!database
        .try_begin_webhook_event("evt_1", "email.received")
        .await
        .unwrap());
}

#[test]
fn argon2_hash_example_for_tests_is_valid() {
    let hash = test_password_hash();
    assert!(hash.starts_with("$argon2id$"));
}

fn test_password_hash() -> String {
    let salt = SaltString::generate(&mut OsRng);
    Argon2::default()
        .hash_password(b"secret-password", &salt)
        .unwrap()
        .to_string()
}

#[allow(dead_code)]
fn touch(path: &Path) {
    fs::write(path, "").unwrap();
}
