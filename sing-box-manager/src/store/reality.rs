//! VLESS-Reality listener 材料：X25519 密钥对与 short ID 首次生成后稳定复用。
//! 私钥和 short ID 合并信封加密；公钥及握手参数可用于编译/订阅，明文保存。

use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use base64::Engine;
use rand::rngs::OsRng;
use rand::RngCore;
use serde::{Deserialize, Serialize};
use sqlx::{Row, SqlitePool};
use x25519_dalek::{PublicKey, StaticSecret};
use zeroize::Zeroize;

use crate::crypto::{Cipher, Sealed};
use crate::error::{AppError, ErrorCode, Result};
use crate::store::now_unix;

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RealityConfig {
    pub flow: String,
    pub public_key: String,
    pub server_name: String,
    pub handshake_server: String,
    pub handshake_port: i64,
    pub client_fingerprint: String,
}

#[derive(Debug, Serialize, Deserialize)]
pub struct RealitySecret {
    pub private_key: String,
    pub short_id: String,
}

impl Drop for RealitySecret {
    fn drop(&mut self) {
        self.private_key.zeroize();
        self.short_id.zeroize();
    }
}

pub struct RealityMaterial {
    pub config: RealityConfig,
    pub secret: RealitySecret,
}

fn generate_secret() -> (String, String, String) {
    let private = StaticSecret::random_from_rng(OsRng);
    let public = PublicKey::from(&private);
    let mut short = [0u8; 8];
    let mut rng = OsRng;
    rng.fill_bytes(&mut short);
    let private_key = URL_SAFE_NO_PAD.encode(private.to_bytes());
    let public_key = URL_SAFE_NO_PAD.encode(public.as_bytes());
    let short_id = short.iter().map(|b| format!("{b:02x}")).collect();
    (private_key, public_key, short_id)
}

pub async fn load_config(pool: &SqlitePool, entry_id: &str) -> Result<Option<RealityConfig>> {
    let row = sqlx::query(
        "SELECT flow,public_key,server_name,handshake_server,handshake_port,client_fingerprint
         FROM entry_reality WHERE entry_id=?",
    )
    .bind(entry_id)
    .fetch_optional(pool)
    .await?;
    Ok(row.map(|row| RealityConfig {
        flow: row.get("flow"),
        public_key: row.get("public_key"),
        server_name: row.get("server_name"),
        handshake_server: row.get("handshake_server"),
        handshake_port: row.get("handshake_port"),
        client_fingerprint: row.get("client_fingerprint"),
    }))
}

#[allow(clippy::too_many_arguments)]
pub async fn ensure(
    pool: &SqlitePool,
    cipher: &Cipher,
    entry_id: &str,
    flow: &str,
    server_name: &str,
    handshake_server: &str,
    handshake_port: i64,
    client_fingerprint: &str,
) -> Result<()> {
    let now = now_unix();
    let exists: i64 = sqlx::query_scalar("SELECT COUNT(*) FROM entry_reality WHERE entry_id=?")
        .bind(entry_id)
        .fetch_one(pool)
        .await?;
    if exists > 0 {
        sqlx::query(
            "UPDATE entry_reality SET flow=?,server_name=?,handshake_server=?,handshake_port=?,
                client_fingerprint=?,updated_at=? WHERE entry_id=?",
        )
        .bind(flow)
        .bind(server_name)
        .bind(handshake_server)
        .bind(handshake_port)
        .bind(client_fingerprint)
        .bind(now)
        .bind(entry_id)
        .execute(pool)
        .await?;
        return Ok(());
    }

    let (private_key, public_key, short_id) = generate_secret();
    let plaintext = serde_json::to_vec(&RealitySecret {
        private_key,
        short_id,
    })
    .map_err(|e| AppError::new(ErrorCode::Internal, format!("编码 Reality 密钥失败: {e}")))?;
    let sealed = cipher.seal(&plaintext)?;
    sqlx::query(
        "INSERT INTO entry_reality(
            entry_id,flow,public_key,server_name,handshake_server,handshake_port,
            client_fingerprint,alg,key_version,nonce,ciphertext,created_at,updated_at
         ) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)",
    )
    .bind(entry_id)
    .bind(flow)
    .bind(public_key)
    .bind(server_name)
    .bind(handshake_server)
    .bind(handshake_port)
    .bind(client_fingerprint)
    .bind(sealed.alg)
    .bind(sealed.key_version)
    .bind(sealed.nonce)
    .bind(sealed.ciphertext)
    .bind(now)
    .bind(now)
    .execute(pool)
    .await?;
    Ok(())
}

pub async fn load(
    pool: &SqlitePool,
    cipher: &Cipher,
    entry_id: &str,
) -> Result<Option<RealityMaterial>> {
    let Some(row) = sqlx::query(
        "SELECT flow,public_key,server_name,handshake_server,handshake_port,
                client_fingerprint,alg,key_version,nonce,ciphertext
         FROM entry_reality WHERE entry_id=?",
    )
    .bind(entry_id)
    .fetch_optional(pool)
    .await?
    else {
        return Ok(None);
    };
    let sealed = Sealed {
        alg: row.get("alg"),
        key_version: row.get("key_version"),
        nonce: row.get("nonce"),
        ciphertext: row.get("ciphertext"),
    };
    let plaintext = cipher.open(&sealed)?;
    let secret = serde_json::from_slice(&plaintext)
        .map_err(|e| AppError::new(ErrorCode::Crypto, format!("解码 Reality 密钥失败: {e}")))?;
    Ok(Some(RealityMaterial {
        config: RealityConfig {
            flow: row.get("flow"),
            public_key: row.get("public_key"),
            server_name: row.get("server_name"),
            handshake_server: row.get("handshake_server"),
            handshake_port: row.get("handshake_port"),
            client_fingerprint: row.get("client_fingerprint"),
        },
        secret,
    }))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::domain::host::Capability;
    use crate::domain::topology::InboundKind;
    use crate::store;
    use crate::store::topology::{self, NewEntry};

    fn cipher() -> Cipher {
        Cipher::from_raw(1, &[7u8; 32]).unwrap()
    }

    #[tokio::test]
    async fn generated_material_is_stable_and_well_formed() {
        let path = std::env::temp_dir().join(format!("sbm-reality-{}.db", uuid::Uuid::new_v4()));
        let pool = store::open(&path.to_string_lossy()).await.unwrap();
        let c = cipher();
        let host = store::hosts::create_host(&pool, "entry", None, &[Capability::Entry])
            .await
            .unwrap();
        let entry = topology::create_entry(
            &pool,
            &c,
            &NewEntry {
                host_id: &host,
                public_address: "entry.example.com",
                inbound_kind: InboundKind::VlessReality,
                ss_method: None,
                allow_direct: false,
            },
        )
        .await
        .unwrap();
        ensure(
            &pool,
            &c,
            &entry,
            "xtls-rprx-vision",
            "www.example.com",
            "www.example.com",
            443,
            "chrome",
        )
        .await
        .unwrap();
        let first = load(&pool, &c, &entry).await.unwrap().unwrap();
        assert_eq!(
            URL_SAFE_NO_PAD
                .decode(first.secret.private_key.as_bytes())
                .unwrap()
                .len(),
            32
        );
        assert_eq!(
            URL_SAFE_NO_PAD
                .decode(first.config.public_key.as_bytes())
                .unwrap()
                .len(),
            32
        );
        assert_eq!(first.secret.short_id.len(), 16);

        ensure(
            &pool,
            &c,
            &entry,
            "xtls-rprx-vision",
            "changed.example.com",
            "changed.example.com",
            8443,
            "firefox",
        )
        .await
        .unwrap();
        let second = load(&pool, &c, &entry).await.unwrap().unwrap();
        assert_eq!(
            first.secret.private_key.as_str(),
            second.secret.private_key.as_str()
        );
        assert_eq!(
            first.secret.short_id.as_str(),
            second.secret.short_id.as_str()
        );
        assert_eq!(second.config.server_name, "changed.example.com");
        assert_eq!(second.config.handshake_port, 8443);
        pool.close().await;
    }
}
