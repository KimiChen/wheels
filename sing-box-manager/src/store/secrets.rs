//! 业务 PSK / socks 凭据的信封读写（复用 credentials + credential_versions）。
//! 编译器只吃解封后的 [`SecretBundle`]，绝不接触 Cipher/DB——保编译器纯度。

use std::collections::HashMap;

use sqlx::{Row, Sqlite, SqlitePool, Transaction};
use zeroize::Zeroize;

use crate::crypto::{Cipher, Sealed};
use crate::error::{AppError, ErrorCode, Result};
use crate::store::now_unix;

/// 事务内封存一段明文为 credential(kind,scope) + credential_versions v1，返回 credential_id。
pub async fn put_psk_tx(
    tx: &mut Transaction<'_, Sqlite>,
    cipher: &Cipher,
    kind: &str,
    scope: &str,
    plaintext: &str,
) -> Result<String> {
    let sealed = cipher.seal(plaintext.as_bytes())?;
    let cred_id = uuid::Uuid::new_v4().to_string();
    let now = now_unix();
    sqlx::query("INSERT INTO credentials(id,kind,scope,created_at) VALUES(?,?,?,?)")
        .bind(&cred_id)
        .bind(kind)
        .bind(scope)
        .bind(now)
        .execute(&mut **tx)
        .await?;
    sqlx::query(
        "INSERT INTO credential_versions(credential_id,version,alg,key_version,nonce,ciphertext,active,created_at)
         VALUES(?,1,?,?,?,?,1,?)",
    )
    .bind(&cred_id)
    .bind(sealed.alg)
    .bind(sealed.key_version)
    .bind(&sealed.nonce)
    .bind(&sealed.ciphertext)
    .bind(now)
    .execute(&mut **tx)
    .await?;
    Ok(cred_id)
}

/// 删除某 (kind, scope) 的凭据（credential_versions 经 0001 CASCADE 连带）。对象删除清理用。
pub async fn delete_psk_tx(
    tx: &mut Transaction<'_, Sqlite>,
    kind: &str,
    scope: &str,
) -> Result<()> {
    sqlx::query("DELETE FROM credentials WHERE kind=? AND scope=?")
        .bind(kind)
        .bind(scope)
        .execute(&mut **tx)
        .await?;
    Ok(())
}

/// 按 (kind, scope) 读取并解封 active 凭据明文。
pub async fn open_psk_by_scope(
    pool: &SqlitePool,
    cipher: &Cipher,
    kind: &str,
    scope: &str,
) -> Result<Option<String>> {
    let row = sqlx::query(
        "SELECT cv.alg, cv.key_version, cv.nonce, cv.ciphertext
         FROM credentials c JOIN credential_versions cv ON cv.credential_id = c.id AND cv.active = 1
         WHERE c.kind = ? AND c.scope = ? ORDER BY cv.version DESC LIMIT 1",
    )
    .bind(kind)
    .bind(scope)
    .fetch_optional(pool)
    .await?;
    match row {
        None => Ok(None),
        Some(r) => Ok(Some(open_row(cipher, &r)?)),
    }
}

/// 按 credential_id 读取并解封 active 明文（socks landing_auth 存的是 "user\npass"）。
pub async fn open_credential(
    pool: &SqlitePool,
    cipher: &Cipher,
    credential_id: &str,
) -> Result<Option<String>> {
    let row = sqlx::query(
        "SELECT alg, key_version, nonce, ciphertext FROM credential_versions
         WHERE credential_id = ? AND active = 1 ORDER BY version DESC LIMIT 1",
    )
    .bind(credential_id)
    .fetch_optional(pool)
    .await?;
    match row {
        None => Ok(None),
        Some(r) => Ok(Some(open_row(cipher, &r)?)),
    }
}

fn open_row(cipher: &Cipher, row: &sqlx::sqlite::SqliteRow) -> Result<String> {
    let sealed = Sealed {
        alg: row.get("alg"),
        key_version: row.get("key_version"),
        nonce: row.get("nonce"),
        ciphertext: row.get("ciphertext"),
    };
    let bytes = cipher.open(&sealed)?;
    String::from_utf8(bytes).map_err(|_| AppError::new(ErrorCode::Crypto, "解封结果非 UTF-8"))
}

/// socks landing 凭据的编码：`username\npassword`（换行分隔）。
pub fn encode_socks_auth(user: &str, pass: &str) -> String {
    format!("{user}\n{pass}")
}
pub fn decode_socks_auth(s: &str) -> (String, String) {
    match s.split_once('\n') {
        Some((u, p)) => (u.to_string(), p.to_string()),
        None => (s.to_string(), String::new()),
    }
}

/// 编译器输入：全部解封后的业务密钥。Drop 时 best-effort 清零。
#[derive(Default)]
pub struct SecretBundle {
    pub entry_psk: HashMap<String, String>,
    pub node_psk: HashMap<String, String>,
    pub landing_auth: HashMap<String, (String, String)>,
}

impl Drop for SecretBundle {
    fn drop(&mut self) {
        for v in self.entry_psk.values_mut() {
            v.zeroize();
        }
        for v in self.node_psk.values_mut() {
            v.zeroize();
        }
        for (u, p) in self.landing_auth.values_mut() {
            u.zeroize();
            p.zeroize();
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::store;
    use base64::{engine::general_purpose::STANDARD, Engine};

    fn cipher() -> Cipher {
        std::env::set_var("ENCRYPTION_MASTER_KEY", STANDARD.encode([9u8; 32]));
        Cipher::from_env(1).unwrap()
    }

    #[tokio::test]
    async fn psk_seal_open_roundtrip_by_scope() {
        let path = std::env::temp_dir().join(format!("sbm-sec-{}.db", uuid::Uuid::new_v4()));
        let pool = store::open(&path.to_string_lossy()).await.unwrap();
        let c = cipher();
        let mut tx = pool.begin().await.unwrap();
        put_psk_tx(&mut tx, &c, "entry_psk", "entry-1", "SECRETPSK==")
            .await
            .unwrap();
        tx.commit().await.unwrap();

        let got = open_psk_by_scope(&pool, &c, "entry_psk", "entry-1")
            .await
            .unwrap();
        assert_eq!(got.as_deref(), Some("SECRETPSK=="));
        assert_eq!(
            open_psk_by_scope(&pool, &c, "node_psk", "entry-1")
                .await
                .unwrap(),
            None
        );

        // 删除后不可再读。
        let mut tx = pool.begin().await.unwrap();
        delete_psk_tx(&mut tx, "entry_psk", "entry-1")
            .await
            .unwrap();
        tx.commit().await.unwrap();
        assert_eq!(
            open_psk_by_scope(&pool, &c, "entry_psk", "entry-1")
                .await
                .unwrap(),
            None
        );
        pool.close().await;
    }

    #[test]
    fn socks_auth_codec() {
        let e = encode_socks_auth("u", "p:w\\d");
        assert_eq!(decode_socks_auth(&e), ("u".into(), "p:w\\d".into()));
    }
}
