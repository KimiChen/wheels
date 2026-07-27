//! 从 enrollment 包组装 Agent 侧 rustls ServerConfig：出示 Agent 服务端证书 +
//! 用 [`PinnedClientVerifier`] 强制 mTLS（只信 client_ca 且 pin Manager SPKI）。

use std::sync::Arc;

use rustls::server::danger::ClientCertVerifier;

use crate::error::{AppError, ErrorCode, Result};
use crate::pki::enrollment::EnrollmentPackage;
use crate::pki::verify::PinnedClientVerifier;
use crate::pki::{certs_from_pem, one_cert_from_pem, private_key_from_pem};

pub fn server_config(pkg: &EnrollmentPackage) -> Result<rustls::ServerConfig> {
    let chain = certs_from_pem(&pkg.agent_server_cert_pem)?;
    let key = private_key_from_pem(&pkg.agent_server_key_pem)?;
    let client_ca = one_cert_from_pem(&pkg.client_ca_cert_pem)?;
    let verifier: Arc<dyn ClientCertVerifier> =
        PinnedClientVerifier::new(client_ca, pkg.manager_client_spki_sha256.clone())?;

    let cfg = rustls::ServerConfig::builder_with_provider(Arc::new(
        rustls::crypto::ring::default_provider(),
    ))
    .with_safe_default_protocol_versions()
    .map_err(|e| AppError::new(ErrorCode::Crypto, format!("协议版本失败: {e}")))?
    .with_client_cert_verifier(verifier)
    .with_single_cert(chain, key)
    .map_err(|e| AppError::new(ErrorCode::Crypto, format!("装载服务端证书失败: {e}")))?;
    Ok(cfg)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::pki::enrollment::ENROLLMENT_VERSION;
    use crate::pki::{
        generate_ca, install_ring_default, issue_agent_server_cert, Ca, CaRole, SanEntry,
    };
    use std::net::{IpAddr, Ipv4Addr};

    #[test]
    fn builds_server_config_from_enrollment() {
        install_ring_default();
        let agent_ca = generate_ca(CaRole::AgentCa, 3650).unwrap();
        let client_ca = generate_ca(CaRole::ClientCa, 3650).unwrap();
        let signer = Ca::from_pem(&agent_ca.cert_pem, &agent_ca.key_pem).unwrap();
        let leaf = issue_agent_server_cert(
            &signer,
            "h1",
            &[SanEntry::Ip(IpAddr::V4(Ipv4Addr::LOCALHOST))],
            2,
            825,
        )
        .unwrap();
        let pkg = EnrollmentPackage {
            version: ENROLLMENT_VERSION,
            host_id: "h1".into(),
            mgmt_bind: "127.0.0.1:39736".into(),
            agent_server_cert_pem: leaf.cert_pem,
            agent_server_key_pem: leaf.key_pem,
            client_ca_cert_pem: client_ca.cert_pem,
            manager_client_spki_sha256: "ab".repeat(32),
            issued_at: 0,
            not_after: leaf.not_after,
        };
        // 仅构建，不握手（沙箱 loopback TLS 不稳）。
        assert!(server_config(&pkg).is_ok());
    }
}
