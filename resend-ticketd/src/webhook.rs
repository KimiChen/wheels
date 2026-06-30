use anyhow::{bail, Context, Result};
use base64::{engine::general_purpose::STANDARD as BASE64, Engine};
use hmac::{Hmac, Mac};
use sha2::Sha256;
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

    for signature in parse_signature_header(signature_header) {
        if let Ok(actual) = BASE64.decode(signature) {
            if expected.as_slice().ct_eq(actual.as_slice()).into() {
                return Ok(());
            }
        }
    }

    bail!("invalid webhook signature")
}

fn parse_signature_header(header: &str) -> Vec<&str> {
    header
        .split_whitespace()
        .filter_map(|part| {
            part.split_once(',')
                .or_else(|| part.split_once('='))
                .map(|(_, signature)| signature)
        })
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn verifies_svix_v1_signature_with_base64_padding() {
        let key = b"0123456789abcdef01234567";
        let secret = format!("whsec_{}", BASE64.encode(key));
        let message_id = "msg_test";
        let timestamp = chrono::Utc::now().timestamp().to_string();
        let body = br#"{"event_type":"email.received"}"#;

        let mut mac = HmacSha256::new_from_slice(key).unwrap();
        mac.update(format!("{message_id}.{timestamp}.").as_bytes());
        mac.update(body);
        let signature = BASE64.encode(mac.finalize().into_bytes());

        verify_svix_signature(
            &secret,
            message_id,
            &timestamp,
            &format!("v1,{signature}"),
            body,
        )
        .unwrap();
    }
}
