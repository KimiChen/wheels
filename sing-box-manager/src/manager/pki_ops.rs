//! Manager 侧 PKI 编排：首启引导双 CA + Manager 身份；按 Host 签发并组装 enrollment 包。
//! 私钥明文只在 enrollment 包（一次性 HTTP 响应）出现；其余接口只回指纹与有效期。

use std::net::IpAddr;

use sqlx::SqlitePool;

use crate::crypto::Cipher;
use crate::error::{AppError, ErrorCode, Result};
use crate::pki::enrollment::{EnrollmentPackage, ENROLLMENT_VERSION};
use crate::pki::{self, CaRole, SanEntry};
use crate::store;

const LEAF_VALIDITY_DAYS: i64 = 825;

/// 首启幂等引导：双 CA + 唯一 Manager 客户端身份。
pub async fn bootstrap(pool: &SqlitePool, cipher: &Cipher) -> Result<()> {
    store::pki::ensure_cas(pool, cipher).await
}

pub struct IssuedEnrollment {
    pub package: EnrollmentPackage,
    pub fingerprint: String,
}

/// 为某 Host 签发 Agent 服务端证书并组装 enrollment 包（含私钥，供带外交付）。
pub async fn build_enrollment(
    pool: &SqlitePool,
    cipher: &Cipher,
    host_id: &str,
    mgmt_bind: &str,
) -> Result<IssuedEnrollment> {
    if store::hosts::get_host(pool, host_id).await?.is_none() {
        return Err(AppError::new(ErrorCode::NotFound, "host 不存在"));
    }
    let agent_ca = store::pki::load_active_ca(pool, cipher, CaRole::AgentCa).await?;
    let serial = store::pki::alloc_serial(pool, &agent_ca.ca_id).await?;
    let (sans, san_labels) = sans_from_bind(mgmt_bind, host_id)?;
    let leaf =
        pki::issue_agent_server_cert(&agent_ca.ca, host_id, &sans, serial, LEAF_VALIDITY_DAYS)?;
    let san_json = serde_json::to_string(&san_labels).unwrap_or_else(|_| "[]".into());
    store::pki::put_agent_cert(pool, cipher, host_id, &leaf, &agent_ca.ca_id, &san_json).await?;
    store::agents::upsert_agent(pool, host_id, mgmt_bind).await?;

    let client_ca_pem = store::pki::active_ca_cert_pem(pool, CaRole::ClientCa)
        .await?
        .ok_or_else(|| AppError::new(ErrorCode::Internal, "无 client_ca"))?;
    let manager_spki = store::pki::manager_client_spki(pool)
        .await?
        .ok_or_else(|| AppError::new(ErrorCode::Internal, "无 Manager 身份"))?;

    let package = EnrollmentPackage {
        version: ENROLLMENT_VERSION,
        host_id: host_id.to_string(),
        mgmt_bind: mgmt_bind.to_string(),
        agent_server_cert_pem: leaf.cert_pem.clone(),
        agent_server_key_pem: leaf.key_pem.clone(),
        client_ca_cert_pem: client_ca_pem,
        manager_client_spki_sha256: manager_spki,
        issued_at: store::now_unix(),
        not_after: leaf.not_after,
    };
    let fingerprint = package.fingerprint()?;
    store::pki::record_enrollment(
        pool,
        host_id,
        serial,
        &fingerprint,
        &leaf.spki_sha256,
        leaf.not_after,
        None,
    )
    .await?;
    Ok(IssuedEnrollment {
        package,
        fingerprint,
    })
}

/// 由 mgmt 绑定地址（`ip:port` 或 `host:port`）派生 SAN。IP→IP-SAN，否则 DNS-SAN；
/// URI host_id SAN 由 `issue_agent_server_cert` 自动追加，务必与 Manager 拨号地址匹配。
fn sans_from_bind(mgmt_bind: &str, host_id: &str) -> Result<(Vec<SanEntry>, Vec<String>)> {
    let host_part = mgmt_bind
        .rsplit_once(':')
        .map(|(h, _)| h)
        .unwrap_or(mgmt_bind);
    let host_part = host_part.trim_start_matches('[').trim_end_matches(']');
    let mut sans = Vec::new();
    let mut labels = Vec::new();
    if let Ok(ip) = host_part.parse::<IpAddr>() {
        sans.push(SanEntry::Ip(ip));
        labels.push(format!("ip:{ip}"));
    } else if !host_part.is_empty() {
        sans.push(SanEntry::Dns(host_part.to_string()));
        labels.push(format!("dns:{host_part}"));
    }
    labels.push(format!("uri:{}{}", pki::HOST_URI_PREFIX, host_id));
    if sans.is_empty() {
        return Err(AppError::new(
            ErrorCode::Validation,
            "mgmt 绑定地址无法派生 SAN",
        ));
    }
    Ok((sans, labels))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::domain::host::Capability;
    use crate::pki::install_ring_default;
    use crate::pki::verify::PinnedServerVerifier;
    use base64::{engine::general_purpose::STANDARD, Engine};
    use rustls::client::danger::ServerCertVerifier;
    use rustls::pki_types::{CertificateDer, ServerName, UnixTime};
    use std::net::{IpAddr, Ipv4Addr};
    use std::time::Duration;

    fn cipher() -> Cipher {
        std::env::set_var("ENCRYPTION_MASTER_KEY", STANDARD.encode([9u8; 32]));
        Cipher::from_env(1).unwrap()
    }
    fn der1(pem: &str) -> CertificateDer<'static> {
        rustls_pemfile::certs(&mut pem.as_bytes())
            .next()
            .unwrap()
            .unwrap()
    }

    #[tokio::test]
    async fn enrollment_issues_cert_that_passes_manager_verifier() {
        install_ring_default();
        let path = std::env::temp_dir().join(format!("sbm-enr-{}.db", uuid::Uuid::new_v4()));
        let pool = store::open(&path.to_string_lossy()).await.unwrap();
        let c = cipher();
        bootstrap(&pool, &c).await.unwrap();
        let host = store::hosts::create_host(&pool, "h", None, &[Capability::Entry])
            .await
            .unwrap();

        let issued = build_enrollment(&pool, &c, &host, "127.0.0.1:39736")
            .await
            .unwrap();
        let pkg = &issued.package;

        // 包内含 client_ca、不含 agent_ca；指纹稳定。
        let agent_ca_pem = store::pki::active_ca_cert_pem(&pool, CaRole::AgentCa)
            .await
            .unwrap()
            .unwrap();
        assert_ne!(pkg.client_ca_cert_pem, agent_ca_pem, "包不得含 agent_ca");
        assert_eq!(issued.fingerprint, pkg.fingerprint().unwrap());
        // JSON 往返。
        let back = EnrollmentPackage::parse(&pkg.to_json().unwrap()).unwrap();
        assert_eq!(back.host_id, host);

        // 端到端（脱网）：签发的 Agent 服务端证书能通过 Manager 自己的 PinnedServerVerifier。
        let v = PinnedServerVerifier::new(der1(&agent_ca_pem), host.clone()).unwrap();
        let ee = der1(&pkg.agent_server_cert_pem);
        let now = UnixTime::since_unix_epoch(Duration::from_secs((pkg.issued_at + 3600) as u64));
        let sn = ServerName::from(IpAddr::V4(Ipv4Addr::LOCALHOST));
        assert!(v.verify_server_cert(&ee, &[], &sn, &[], now).is_ok());

        // 审计行已写入。
        let n: i64 = sqlx::query_scalar("SELECT COUNT(*) FROM enrollment_packages WHERE host_id=?")
            .bind(&host)
            .fetch_one(&pool)
            .await
            .unwrap();
        assert_eq!(n, 1);
        pool.close().await;
    }
}
