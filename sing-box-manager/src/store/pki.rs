//! ca_keypairs / manager_identity / agent_certificates / credentials 持久化。
//!
//! CA 私钥内联信封存 `ca_keypairs`；Manager 客户端与 Agent 服务端私钥经 `credentials` + `credential_versions`
//! 信封存储。对外只出公开 PEM 与元数据；私钥仅在签发/构建 TLS 配置时于内存解封。

use crate::crypto::{Cipher, Sealed};
use crate::domain::agent::TrustStatus;
use crate::error::{AppError, ErrorCode, Result};
use crate::pki::{self, CaRole, GeneratedCert};
use crate::store::now_unix;
use sqlx::{Row, SqlitePool};

const CA_VALIDITY_DAYS: i64 = 3650;
const LEAF_VALIDITY_DAYS: i64 = 825;

/// 首启幂等：确保双 CA 与唯一 Manager 客户端身份存在。
pub async fn ensure_cas(pool: &SqlitePool, cipher: &Cipher) -> Result<()> {
    ensure_one_ca(pool, cipher, CaRole::AgentCa).await?;
    let client_ca_id = ensure_one_ca(pool, cipher, CaRole::ClientCa).await?;
    ensure_manager_identity(pool, cipher, &client_ca_id).await?;
    Ok(())
}

async fn ensure_one_ca(pool: &SqlitePool, cipher: &Cipher, role: CaRole) -> Result<String> {
    if let Some(id) = active_ca_id(pool, role).await? {
        return Ok(id);
    }
    let gen = pki::generate_ca(role, CA_VALIDITY_DAYS)?;
    let sealed = cipher.seal(gen.key_pem.as_bytes())?;
    let id = uuid::Uuid::new_v4().to_string();
    let now = now_unix();
    // partial UNIQUE(role) WHERE active=1 保证并发 bootstrap 只有一个成功；失败者 IGNORE 后重读。
    sqlx::query(
        "INSERT OR IGNORE INTO ca_keypairs(id,role,cert_pem,spki_sha256,alg,key_version,nonce,ciphertext,not_before,not_after,next_serial,active,created_at)
         VALUES(?,?,?,?,?,?,?,?,?,?,2,1,?)",
    )
    .bind(&id)
    .bind(role.as_str())
    .bind(&gen.cert_pem)
    .bind(&gen.spki_sha256)
    .bind(sealed.alg)
    .bind(sealed.key_version)
    .bind(&sealed.nonce)
    .bind(&sealed.ciphertext)
    .bind(gen.not_before)
    .bind(gen.not_after)
    .bind(now)
    .execute(pool)
    .await?;
    active_ca_id(pool, role)
        .await?
        .ok_or_else(|| AppError::new(ErrorCode::Internal, "CA 引导后仍无 active 行"))
}

async fn ensure_manager_identity(
    pool: &SqlitePool,
    cipher: &Cipher,
    client_ca_id: &str,
) -> Result<()> {
    if manager_client_spki(pool).await?.is_some() {
        return Ok(());
    }
    let ca = load_active_ca(pool, cipher, CaRole::ClientCa).await?;
    let serial = alloc_serial(pool, client_ca_id).await?;
    let mc = pki::issue_manager_client_cert(&ca.ca, serial, LEAF_VALIDITY_DAYS)?;
    let sealed = cipher.seal(mc.key_pem.as_bytes())?;
    let now = now_unix();

    let mut tx = pool.begin().await?;
    let cred_id = uuid::Uuid::new_v4().to_string();
    insert_sealed_credential(
        &mut tx,
        &cred_id,
        "manager_client_cert",
        "manager",
        &sealed,
        now,
    )
    .await?;
    let res = sqlx::query(
        "INSERT OR IGNORE INTO manager_identity(id,credential_id,ca_keypair_id,cert_pem,spki_sha256,not_before,not_after,active,created_at)
         VALUES(?,?,?,?,?,?,?,1,?)",
    )
    .bind(uuid::Uuid::new_v4().to_string())
    .bind(&cred_id)
    .bind(client_ca_id)
    .bind(&mc.cert_pem)
    .bind(&mc.spki_sha256)
    .bind(mc.not_before)
    .bind(mc.not_after)
    .bind(now)
    .execute(&mut *tx)
    .await?;
    if res.rows_affected() == 0 {
        // 并发 bootstrap 抢先建立身份：回滚，丢弃本次多余 credential。
        tx.rollback().await?;
        return Ok(());
    }
    tx.commit().await?;
    Ok(())
}

/// 原子分配某 CA 下一叶证书序列号（自增，返回分配到的值）。
pub async fn alloc_serial(pool: &SqlitePool, ca_id: &str) -> Result<u64> {
    let row = sqlx::query(
        "UPDATE ca_keypairs SET next_serial = next_serial + 1 WHERE id=? RETURNING next_serial - 1 AS serial",
    )
    .bind(ca_id)
    .fetch_optional(pool)
    .await?
    .ok_or_else(|| AppError::new(ErrorCode::NotFound, "CA 不存在，无法分配序列"))?;
    let s: i64 = row.get("serial");
    Ok(s as u64)
}

/// 从存库 PEM + 信封私钥重建的签发者。
pub struct LoadedCa {
    pub ca_id: String,
    pub ca: pki::Ca,
    pub cert_pem: String,
}

pub async fn load_active_ca(pool: &SqlitePool, cipher: &Cipher, role: CaRole) -> Result<LoadedCa> {
    let row = sqlx::query(
        "SELECT id,cert_pem,alg,key_version,nonce,ciphertext FROM ca_keypairs WHERE role=? AND active=1",
    )
    .bind(role.as_str())
    .fetch_optional(pool)
    .await?
    .ok_or_else(|| AppError::new(ErrorCode::NotFound, format!("无 active {} CA", role.as_str())))?;
    let ca_id: String = row.get("id");
    let cert_pem: String = row.get("cert_pem");
    let key_pem = open_sealed_row(cipher, &row)?;
    let ca = pki::Ca::from_pem(&cert_pem, &key_pem)?;
    Ok(LoadedCa {
        ca_id,
        ca,
        cert_pem,
    })
}

/// 某角色 active CA 公证书 PEM（仅公开数据，不解密）。
pub async fn active_ca_cert_pem(pool: &SqlitePool, role: CaRole) -> Result<Option<String>> {
    let row = sqlx::query("SELECT cert_pem FROM ca_keypairs WHERE role=? AND active=1")
        .bind(role.as_str())
        .fetch_optional(pool)
        .await?;
    Ok(row.map(|r| r.get("cert_pem")))
}

async fn active_ca_id(pool: &SqlitePool, role: CaRole) -> Result<Option<String>> {
    let row = sqlx::query("SELECT id FROM ca_keypairs WHERE role=? AND active=1")
        .bind(role.as_str())
        .fetch_optional(pool)
        .await?;
    Ok(row.map(|r| r.get("id")))
}

/// Manager 客户端证书 SPKI 指纹（公开；打入 enrollment 包作为 pin）。
pub async fn manager_client_spki(pool: &SqlitePool) -> Result<Option<String>> {
    let row = sqlx::query("SELECT spki_sha256 FROM manager_identity WHERE active=1")
        .fetch_optional(pool)
        .await?;
    Ok(row.map(|r| r.get("spki_sha256")))
}

/// Manager 客户端 TLS 材料（cert + 解封私钥 + spki），供 Manager 拨号 Agent 时出示。
pub struct ManagerClientMaterial {
    pub cert_pem: String,
    pub key_pem: String, // 敏感
    pub spki_sha256: String,
}

pub async fn load_manager_client_material(
    pool: &SqlitePool,
    cipher: &Cipher,
) -> Result<ManagerClientMaterial> {
    let row = sqlx::query(
        "SELECT mi.cert_pem AS cert_pem, mi.spki_sha256 AS spki, cv.alg AS alg, cv.key_version AS key_version, cv.nonce AS nonce, cv.ciphertext AS ciphertext
         FROM manager_identity mi
         JOIN credential_versions cv ON cv.credential_id = mi.credential_id AND cv.active = 1
         WHERE mi.active = 1",
    )
    .fetch_optional(pool)
    .await?
    .ok_or_else(|| AppError::new(ErrorCode::NotFound, "无 active Manager 身份"))?;
    let key_pem = open_sealed_row(cipher, &row)?;
    Ok(ManagerClientMaterial {
        cert_pem: row.get("cert_pem"),
        key_pem,
        spki_sha256: row.get("spki"),
    })
}

/// 签发并持久化某 Host 的 Agent 服务端证书：私钥信封入 credentials，公开元数据入 agent_certificates。
/// 重新签发（轮换）会更新公开元数据并把 trust 重置为 pending（需重新带外确认）。
pub async fn put_agent_cert(
    pool: &SqlitePool,
    cipher: &Cipher,
    host_id: &str,
    gen: &GeneratedCert,
    ca_id: &str,
    san_json: &str,
) -> Result<()> {
    let sealed = cipher.seal(gen.key_pem.as_bytes())?;
    let now = now_unix();
    let mut tx = pool.begin().await?;
    let cred_id = uuid::Uuid::new_v4().to_string();
    insert_sealed_credential(
        &mut tx,
        &cred_id,
        "agent_server_cert",
        host_id,
        &sealed,
        now,
    )
    .await?;
    sqlx::query(
        "INSERT INTO agent_certificates(host_id,credential_id,trust_status,cert_pem,spki_sha256,san_json,serial,ca_keypair_id,not_before,not_after,created_at)
         VALUES(?,?,'pending',?,?,?,?,?,?,?,?)
         ON CONFLICT(host_id) DO UPDATE SET
            credential_id=excluded.credential_id, trust_status='pending', cert_pem=excluded.cert_pem,
            spki_sha256=excluded.spki_sha256, san_json=excluded.san_json, serial=excluded.serial,
            ca_keypair_id=excluded.ca_keypair_id, not_before=excluded.not_before, not_after=excluded.not_after",
    )
    .bind(host_id)
    .bind(&cred_id)
    .bind(&gen.cert_pem)
    .bind(&gen.spki_sha256)
    .bind(san_json)
    .bind(gen.serial as i64)
    .bind(ca_id)
    .bind(gen.not_before)
    .bind(gen.not_after)
    .bind(now)
    .execute(&mut *tx)
    .await?;
    tx.commit().await?;
    Ok(())
}

/// 管理员带外核对指纹后，将某 Host 的 Agent 证书标记为 trusted（或吊销）。
pub async fn set_trust(pool: &SqlitePool, host_id: &str, trust: TrustStatus) -> Result<()> {
    let res = sqlx::query("UPDATE agent_certificates SET trust_status=? WHERE host_id=?")
        .bind(trust.as_str())
        .bind(host_id)
        .execute(pool)
        .await?;
    if res.rows_affected() == 0 {
        return Err(AppError::new(
            ErrorCode::NotFound,
            "该 Host 尚无 Agent 证书",
        ));
    }
    Ok(())
}

/// Agent 证书公开视图（gate / 详情页用；无私钥）。
#[derive(Debug, Clone)]
pub struct AgentCertInfo {
    pub trust_status: String,
    pub spki_sha256: Option<String>,
    pub not_after: Option<i64>,
}

pub async fn agent_cert_info(pool: &SqlitePool, host_id: &str) -> Result<Option<AgentCertInfo>> {
    let row = sqlx::query(
        "SELECT trust_status, spki_sha256, not_after FROM agent_certificates WHERE host_id=?",
    )
    .bind(host_id)
    .fetch_optional(pool)
    .await?;
    Ok(row.map(|r| AgentCertInfo {
        trust_status: r.get("trust_status"),
        spki_sha256: r.get("spki_sha256"),
        not_after: r.get("not_after"),
    }))
}

/// 记录一次 enrollment 签发（审计；不存任何私钥）。供轮换与带外交付追踪。
pub async fn record_enrollment(
    pool: &SqlitePool,
    host_id: &str,
    serial: u64,
    package_fp_sha256: &str,
    cert_spki_sha256: &str,
    not_after: i64,
    issued_by: Option<&str>,
) -> Result<()> {
    sqlx::query(
        "INSERT INTO enrollment_packages(id,host_id,serial,package_fp_sha256,cert_spki_sha256,not_after,issued_by,delivered,created_at)
         VALUES(?,?,?,?,?,?,?,0,?)",
    )
    .bind(uuid::Uuid::new_v4().to_string())
    .bind(host_id)
    .bind(serial as i64)
    .bind(package_fp_sha256)
    .bind(cert_spki_sha256)
    .bind(not_after)
    .bind(issued_by)
    .bind(now_unix())
    .execute(pool)
    .await?;
    Ok(())
}

async fn insert_sealed_credential(
    tx: &mut sqlx::Transaction<'_, sqlx::Sqlite>,
    cred_id: &str,
    kind: &str,
    scope: &str,
    sealed: &Sealed,
    now: i64,
) -> Result<()> {
    sqlx::query("INSERT INTO credentials(id,kind,scope,created_at) VALUES(?,?,?,?)")
        .bind(cred_id)
        .bind(kind)
        .bind(scope)
        .bind(now)
        .execute(&mut **tx)
        .await?;
    sqlx::query(
        "INSERT INTO credential_versions(credential_id,version,alg,key_version,nonce,ciphertext,active,created_at)
         VALUES(?,1,?,?,?,?,1,?)",
    )
    .bind(cred_id)
    .bind(sealed.alg)
    .bind(sealed.key_version)
    .bind(&sealed.nonce)
    .bind(&sealed.ciphertext)
    .bind(now)
    .execute(&mut **tx)
    .await?;
    Ok(())
}

/// 从含 (alg,key_version,nonce,ciphertext) 列的行解封出明文字符串（私钥 PEM）。
fn open_sealed_row(cipher: &Cipher, row: &sqlx::sqlite::SqliteRow) -> Result<String> {
    let sealed = Sealed {
        alg: row.get("alg"),
        key_version: row.get("key_version"),
        nonce: row.get("nonce"),
        ciphertext: row.get("ciphertext"),
    };
    let bytes = cipher.open(&sealed)?;
    String::from_utf8(bytes).map_err(|_| AppError::new(ErrorCode::Crypto, "解封结果非 UTF-8"))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::pki::SanEntry;
    use crate::store;
    use base64::{engine::general_purpose::STANDARD, Engine};
    use std::net::{IpAddr, Ipv4Addr};

    fn cipher() -> Cipher {
        // 与其它测试统一使用固定主密钥，避免并发 env 竞态。
        std::env::set_var("ENCRYPTION_MASTER_KEY", STANDARD.encode([9u8; 32]));
        Cipher::from_env(1).unwrap()
    }

    async fn fresh_pool() -> sqlx::SqlitePool {
        crate::pki::install_ring_default();
        let path = std::env::temp_dir().join(format!("sbm-pki-{}.db", uuid::Uuid::new_v4()));
        store::open(&path.to_string_lossy()).await.unwrap()
    }

    #[tokio::test]
    async fn bootstrap_is_idempotent_and_ca_keys_roundtrip() {
        let pool = fresh_pool().await;
        let c = cipher();
        ensure_cas(&pool, &c).await.unwrap();
        // 幂等：再次 ensure 不新增 CA。
        ensure_cas(&pool, &c).await.unwrap();
        let n: i64 = sqlx::query("SELECT COUNT(*) AS n FROM ca_keypairs")
            .fetch_one(&pool)
            .await
            .unwrap()
            .get("n");
        assert_eq!(n, 2, "恰好双 CA");

        // 解封 CA 私钥并用其签发叶证书（证明信封往返 + 可签名）。
        let agent_ca = load_active_ca(&pool, &c, CaRole::AgentCa).await.unwrap();
        let leaf = pki::issue_agent_server_cert(
            &agent_ca.ca,
            "host-1",
            &[SanEntry::Ip(IpAddr::V4(Ipv4Addr::LOCALHOST))],
            alloc_serial(&pool, &agent_ca.ca_id).await.unwrap(),
            825,
        )
        .unwrap();
        assert!(leaf.cert_pem.contains("BEGIN CERTIFICATE"));

        // Manager 客户端材料可解封，spki 与公开值一致。
        let mat = load_manager_client_material(&pool, &c).await.unwrap();
        assert_eq!(
            Some(mat.spki_sha256),
            manager_client_spki(&pool).await.unwrap()
        );
        pool.close().await;
    }

    #[tokio::test]
    async fn alloc_serial_increments() {
        let pool = fresh_pool().await;
        let c = cipher();
        ensure_cas(&pool, &c).await.unwrap();
        let ca = load_active_ca(&pool, &c, CaRole::AgentCa).await.unwrap();
        let s1 = alloc_serial(&pool, &ca.ca_id).await.unwrap();
        let s2 = alloc_serial(&pool, &ca.ca_id).await.unwrap();
        assert_eq!(s2, s1 + 1);
        pool.close().await;
    }

    #[tokio::test]
    async fn put_agent_cert_and_trust_flip() {
        let pool = fresh_pool().await;
        let c = cipher();
        ensure_cas(&pool, &c).await.unwrap();
        let host =
            store::hosts::create_host(&pool, "h", None, &[crate::domain::host::Capability::Entry])
                .await
                .unwrap();
        let ca = load_active_ca(&pool, &c, CaRole::AgentCa).await.unwrap();
        let serial = alloc_serial(&pool, &ca.ca_id).await.unwrap();
        let leaf = pki::issue_agent_server_cert(
            &ca.ca,
            &host,
            &[SanEntry::Ip(IpAddr::V4(Ipv4Addr::LOCALHOST))],
            serial,
            825,
        )
        .unwrap();
        put_agent_cert(&pool, &c, &host, &leaf, &ca.ca_id, "[]")
            .await
            .unwrap();

        let info = agent_cert_info(&pool, &host).await.unwrap().unwrap();
        assert_eq!(info.trust_status, "pending");
        assert_eq!(info.spki_sha256.as_deref(), Some(leaf.spki_sha256.as_str()));

        set_trust(&pool, &host, TrustStatus::Trusted).await.unwrap();
        let info2 = agent_cert_info(&pool, &host).await.unwrap().unwrap();
        assert_eq!(info2.trust_status, "trusted");
        pool.close().await;
    }
}
