use anyhow::{Context, Result};
use argon2::{Argon2, PasswordHash, PasswordVerifier};
use base64::{engine::general_purpose::URL_SAFE_NO_PAD, Engine};
use hmac::{Hmac, Mac};
use rand::{rngs::OsRng, RngCore};
use sha2::{Digest, Sha256};
use subtle::ConstantTimeEq;

type HmacSha256 = Hmac<Sha256>;

pub const SESSION_COOKIE: &str = "resend_ticketd_session";

pub fn verify_password(hash: &str, password: &str) -> Result<bool> {
    let parsed = PasswordHash::new(hash).context("failed to parse admin password hash")?;
    Ok(Argon2::default()
        .verify_password(password.as_bytes(), &parsed)
        .is_ok())
}

pub fn generate_token() -> String {
    let mut bytes = [0_u8; 32];
    OsRng.fill_bytes(&mut bytes);
    URL_SAFE_NO_PAD.encode(bytes)
}

pub fn hash_token(token: &str) -> String {
    let digest = Sha256::digest(token.as_bytes());
    hex_encode(&digest)
}

pub fn csrf_token(session_token: &str) -> Result<String> {
    let mut mac = HmacSha256::new_from_slice(session_token.as_bytes())
        .context("failed to initialize CSRF token HMAC")?;
    mac.update(b"resend-ticketd-csrf");
    Ok(URL_SAFE_NO_PAD.encode(mac.finalize().into_bytes()))
}

pub fn verify_csrf(session_token: &str, candidate: &str) -> Result<bool> {
    let expected = csrf_token(session_token)?;
    Ok(expected.as_bytes().ct_eq(candidate.as_bytes()).into())
}

fn hex_encode(bytes: &[u8]) -> String {
    const HEX: &[u8; 16] = b"0123456789abcdef";
    let mut out = String::with_capacity(bytes.len() * 2);
    for byte in bytes {
        out.push(HEX[(byte >> 4) as usize] as char);
        out.push(HEX[(byte & 0x0f) as usize] as char);
    }
    out
}
