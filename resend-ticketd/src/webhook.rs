use anyhow::{bail, Context, Result};
use base64::{engine::general_purpose::STANDARD as BASE64, Engine};
use hmac::{Hmac, Mac};
use sha2::Sha256;
use std::collections::HashMap;
use subtle::ConstantTimeEq;

type HmacSha256 = Hmac<Sha256>;

const SIGNATURE_TOLERANCE_SECONDS: i64 = 5 * 60;

pub fn verify_svix_signature(
    secret: &str,
    message_id: &str,
    timestamp: &str,
    signature_header: &str,
    body: &[u8],
) -> Result<()> {
    let timestamp_i64 = timestamp
        .parse::<i64>()
        .context("svix-timestamp must be a Unix timestamp")?;
    let now = chrono::Utc::now().timestamp();
    if (now - timestamp_i64).abs() > SIGNATURE_TOLERANCE_SECONDS {
        bail!("webhook timestamp is outside tolerance");
    }

    let signing_secret = secret.strip_prefix("whsec_").unwrap_or(secret);
    let key = BASE64
        .decode(signing_secret)
        .context("webhook secret is not valid base64")?;
    let payload = format!("{message_id}.{timestamp}.");
    let mut mac = HmacSha256::new_from_slice(&key).context("failed to initialize HMAC")?;
    mac.update(payload.as_bytes());
    mac.update(body);
    let expected = mac.finalize().into_bytes();

    for signature in parse_signature_header(signature_header).values() {
        if let Ok(actual) = BASE64.decode(signature) {
            if expected.as_slice().ct_eq(actual.as_slice()).into() {
                return Ok(());
            }
        }
    }

    bail!("invalid webhook signature")
}

fn parse_signature_header(header: &str) -> HashMap<String, String> {
    header
        .split_whitespace()
        .filter_map(|part| part.split_once(',').unwrap_or(("", part)).1.split_once('='))
        .map(|(version, value)| (version.to_string(), value.to_string()))
        .collect()
}
