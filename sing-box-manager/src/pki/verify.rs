//! 自定义 rustls verifier：在 webpki 链/名称/有效期/EKU 校验之上，做结构化 pin。
//!
//! - [`PinnedServerVerifier`]（Manager 侧）：链校验后要求叶证书 URI-SAN 含 `sbm://host/<host_id>`，
//!   使泄露的其他 Host 证书无法冒充本次要连接的 Host。
//! - [`PinnedClientVerifier`]（Agent 侧）：链校验后要求叶证书 SPKI 指纹等于 enrollment 内的
//!   Manager pin，使 client_ca 下误签的合法 clientAuth 证书仍被拒。
//!
//! 任何解析失败一律 fail-closed（拒绝）。

use std::sync::Arc;

use rustls::client::danger::{HandshakeSignatureValid, ServerCertVerified, ServerCertVerifier};
use rustls::client::WebPkiServerVerifier;
use rustls::pki_types::{CertificateDer, ServerName, UnixTime};
use rustls::server::danger::{ClientCertVerified, ClientCertVerifier};
use rustls::server::WebPkiClientVerifier;
use rustls::{
    CertificateError, DigitallySignedStruct, DistinguishedName, Error, RootCertStore,
    SignatureScheme,
};

use crate::error::{AppError, ErrorCode, Result as AppResult};
use crate::pki::{spki_sha256_from_der, HOST_URI_PREFIX};

fn tls_err<E: std::fmt::Display>(what: &str, e: E) -> AppError {
    AppError::new(ErrorCode::Crypto, format!("{what}失败: {e}"))
}

fn app_verification_failure() -> Error {
    Error::InvalidCertificate(CertificateError::ApplicationVerificationFailure)
}

/// Manager 侧：验证 Agent 服务端证书链到 agent_ca 并 pin 其 host_id URI-SAN。
#[derive(Debug)]
pub struct PinnedServerVerifier {
    inner: Arc<WebPkiServerVerifier>,
    expected_host_id: String,
}

impl PinnedServerVerifier {
    /// `agent_ca_der`：唯一信任锚（agent_ca 公证书 DER）。
    pub fn new(
        agent_ca_der: CertificateDer<'static>,
        expected_host_id: String,
    ) -> AppResult<Arc<Self>> {
        let mut roots = RootCertStore::empty();
        roots
            .add(agent_ca_der)
            .map_err(|e| tls_err("agent_ca 信任锚", e))?;
        let provider = Arc::new(rustls::crypto::ring::default_provider());
        let inner = WebPkiServerVerifier::builder_with_provider(Arc::new(roots), provider)
            .build()
            .map_err(|e| tls_err("构建服务端 verifier", e))?;
        Ok(Arc::new(Self {
            inner,
            expected_host_id,
        }))
    }
}

impl ServerCertVerifier for PinnedServerVerifier {
    fn verify_server_cert(
        &self,
        end_entity: &CertificateDer<'_>,
        intermediates: &[CertificateDer<'_>],
        server_name: &ServerName<'_>,
        ocsp_response: &[u8],
        now: UnixTime,
    ) -> Result<ServerCertVerified, Error> {
        // 1) webpki：链、名称（IP/DNS SAN 对齐拨号地址）、有效期、serverAuth EKU。
        self.inner.verify_server_cert(
            end_entity,
            intermediates,
            server_name,
            ocsp_response,
            now,
        )?;
        // 2) 结构化 pin：URI-SAN 必须含本次期望的 host_id。
        if !cert_has_host_uri(end_entity, &self.expected_host_id) {
            return Err(app_verification_failure());
        }
        Ok(ServerCertVerified::assertion())
    }

    fn verify_tls12_signature(
        &self,
        message: &[u8],
        cert: &CertificateDer<'_>,
        dss: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, Error> {
        self.inner.verify_tls12_signature(message, cert, dss)
    }

    fn verify_tls13_signature(
        &self,
        message: &[u8],
        cert: &CertificateDer<'_>,
        dss: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, Error> {
        self.inner.verify_tls13_signature(message, cert, dss)
    }

    fn supported_verify_schemes(&self) -> Vec<SignatureScheme> {
        self.inner.supported_verify_schemes()
    }
}

/// Agent 侧：验证 Manager 客户端证书链到 client_ca 并 pin 其 SPKI 指纹。
#[derive(Debug)]
pub struct PinnedClientVerifier {
    inner: Arc<dyn ClientCertVerifier>,
    pin_spki_sha256: String,
}

impl PinnedClientVerifier {
    /// `client_ca_der`：唯一信任锚（client_ca 公证书 DER）；`pin_spki_sha256`：enrollment 内 Manager pin。
    pub fn new(
        client_ca_der: CertificateDer<'static>,
        pin_spki_sha256: String,
    ) -> AppResult<Arc<Self>> {
        let mut roots = RootCertStore::empty();
        roots
            .add(client_ca_der)
            .map_err(|e| tls_err("client_ca 信任锚", e))?;
        let provider = Arc::new(rustls::crypto::ring::default_provider());
        let inner = WebPkiClientVerifier::builder_with_provider(Arc::new(roots), provider)
            .build()
            .map_err(|e| tls_err("构建客户端 verifier", e))?;
        Ok(Arc::new(Self {
            inner,
            pin_spki_sha256,
        }))
    }
}

impl ClientCertVerifier for PinnedClientVerifier {
    fn root_hint_subjects(&self) -> &[DistinguishedName] {
        self.inner.root_hint_subjects()
    }

    fn verify_client_cert(
        &self,
        end_entity: &CertificateDer<'_>,
        intermediates: &[CertificateDer<'_>],
        now: UnixTime,
    ) -> Result<ClientCertVerified, Error> {
        // 1) webpki：链到 client_ca、有效期、clientAuth EKU。
        self.inner
            .verify_client_cert(end_entity, intermediates, now)?;
        // 2) 结构化 pin：SPKI 指纹必须等于唯一 Manager 身份。
        match cert_spki_sha256(end_entity) {
            Some(fp) if fp == self.pin_spki_sha256 => Ok(ClientCertVerified::assertion()),
            _ => Err(app_verification_failure()),
        }
    }

    fn verify_tls12_signature(
        &self,
        message: &[u8],
        cert: &CertificateDer<'_>,
        dss: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, Error> {
        self.inner.verify_tls12_signature(message, cert, dss)
    }

    fn verify_tls13_signature(
        &self,
        message: &[u8],
        cert: &CertificateDer<'_>,
        dss: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, Error> {
        self.inner.verify_tls13_signature(message, cert, dss)
    }

    fn supported_verify_schemes(&self) -> Vec<SignatureScheme> {
        self.inner.supported_verify_schemes()
    }

    fn client_auth_mandatory(&self) -> bool {
        true
    }

    fn offer_client_auth(&self) -> bool {
        true
    }
}

fn cert_has_host_uri(der: &CertificateDer<'_>, host_id: &str) -> bool {
    use x509_parser::prelude::FromDer;
    let want = format!("{HOST_URI_PREFIX}{host_id}");
    let Ok((_, cert)) = x509_parser::certificate::X509Certificate::from_der(der.as_ref()) else {
        return false;
    };
    let Ok(Some(san)) = cert.subject_alternative_name() else {
        return false;
    };
    san.value.general_names.iter().any(|g| match g {
        x509_parser::extensions::GeneralName::URI(u) => *u == want.as_str(),
        _ => false,
    })
}

fn cert_spki_sha256(der: &CertificateDer<'_>) -> Option<String> {
    use x509_parser::prelude::FromDer;
    let (_, cert) = x509_parser::certificate::X509Certificate::from_der(der.as_ref()).ok()?;
    Some(spki_sha256_from_der(cert.public_key().raw))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::pki::{
        generate_ca, install_ring_default, issue_agent_server_cert, issue_manager_client_cert, Ca,
        CaRole, SanEntry,
    };
    use std::net::{IpAddr, Ipv4Addr};
    use std::time::Duration;

    const LOCAL: IpAddr = IpAddr::V4(Ipv4Addr::LOCALHOST);

    fn der_of(pem: &str) -> CertificateDer<'static> {
        rustls_pemfile::certs(&mut pem.as_bytes())
            .next()
            .unwrap()
            .unwrap()
    }
    fn at(ts: i64) -> UnixTime {
        UnixTime::since_unix_epoch(Duration::from_secs(ts as u64))
    }
    fn sni() -> ServerName<'static> {
        ServerName::from(LOCAL)
    }

    #[test]
    fn pinned_server_accepts_matching_host_id() {
        install_ring_default();
        let ca_gen = generate_ca(CaRole::AgentCa, 3650).unwrap();
        let ca = Ca::from_pem(&ca_gen.cert_pem, &ca_gen.key_pem).unwrap();
        let leaf = issue_agent_server_cert(&ca, "h1", &[SanEntry::Ip(LOCAL)], 5, 825).unwrap();
        let v = PinnedServerVerifier::new(der_of(&ca_gen.cert_pem), "h1".into()).unwrap();
        let ee = der_of(&leaf.cert_pem);
        assert!(v
            .verify_server_cert(&ee, &[], &sni(), &[], at(leaf.not_before + 3600))
            .is_ok());
    }

    #[test]
    fn pinned_server_rejects_wrong_host_id_san() {
        install_ring_default();
        let ca_gen = generate_ca(CaRole::AgentCa, 3650).unwrap();
        let ca = Ca::from_pem(&ca_gen.cert_pem, &ca_gen.key_pem).unwrap();
        // 证书链有效、IP-SAN 匹配拨号地址，但 host_id 为 h2。
        let leaf = issue_agent_server_cert(&ca, "h2", &[SanEntry::Ip(LOCAL)], 5, 825).unwrap();
        let v = PinnedServerVerifier::new(der_of(&ca_gen.cert_pem), "h1".into()).unwrap();
        let ee = der_of(&leaf.cert_pem);
        assert!(v
            .verify_server_cert(&ee, &[], &sni(), &[], at(leaf.not_before + 3600))
            .is_err());
    }

    #[test]
    fn pinned_server_rejects_wrong_ca() {
        install_ring_default();
        let ca1 = generate_ca(CaRole::AgentCa, 3650).unwrap();
        let ca2 = generate_ca(CaRole::AgentCa, 3650).unwrap();
        let signer = Ca::from_pem(&ca2.cert_pem, &ca2.key_pem).unwrap();
        let leaf = issue_agent_server_cert(&signer, "h1", &[SanEntry::Ip(LOCAL)], 5, 825).unwrap();
        // 信任锚是 ca1，但证书由 ca2 签发。
        let v = PinnedServerVerifier::new(der_of(&ca1.cert_pem), "h1".into()).unwrap();
        let ee = der_of(&leaf.cert_pem);
        assert!(v
            .verify_server_cert(&ee, &[], &sni(), &[], at(leaf.not_before + 3600))
            .is_err());
    }

    #[test]
    fn pinned_server_rejects_expired() {
        install_ring_default();
        let ca_gen = generate_ca(CaRole::AgentCa, 3650).unwrap();
        let ca = Ca::from_pem(&ca_gen.cert_pem, &ca_gen.key_pem).unwrap();
        let leaf = issue_agent_server_cert(&ca, "h1", &[SanEntry::Ip(LOCAL)], 5, 1).unwrap();
        let v = PinnedServerVerifier::new(der_of(&ca_gen.cert_pem), "h1".into()).unwrap();
        let ee = der_of(&leaf.cert_pem);
        // now 在 not_after 之后。
        assert!(v
            .verify_server_cert(&ee, &[], &sni(), &[], at(leaf.not_after + 3600))
            .is_err());
    }

    #[test]
    fn pinned_client_pin_enforced() {
        install_ring_default();
        let ca_gen = generate_ca(CaRole::ClientCa, 3650).unwrap();
        let ca = Ca::from_pem(&ca_gen.cert_pem, &ca_gen.key_pem).unwrap();
        let mc = issue_manager_client_cert(&ca, 2, 825).unwrap();
        let ee = der_of(&mc.cert_pem);
        let now = at(mc.not_before + 3600);

        // 错 pin → 拒绝。
        let bad = PinnedClientVerifier::new(der_of(&ca_gen.cert_pem), "00".repeat(32)).unwrap();
        assert!(bad.verify_client_cert(&ee, &[], now).is_err());
        // 对 pin → 通过。
        let good =
            PinnedClientVerifier::new(der_of(&ca_gen.cert_pem), mc.spki_sha256.clone()).unwrap();
        assert!(good.verify_client_cert(&ee, &[], now).is_ok());
    }

    #[test]
    fn role_confusion_server_cert_as_client_rejected() {
        install_ring_default();
        let agent_ca = generate_ca(CaRole::AgentCa, 3650).unwrap();
        let client_ca = generate_ca(CaRole::ClientCa, 3650).unwrap();
        let signer = Ca::from_pem(&agent_ca.cert_pem, &agent_ca.key_pem).unwrap();
        let server_leaf =
            issue_agent_server_cert(&signer, "h1", &[SanEntry::Ip(LOCAL)], 5, 825).unwrap();
        // 客户端 verifier 锚定 client_ca；pin 设为服务端证书自身指纹（证明是链/EKU 而非 pin 拒绝）。
        let v =
            PinnedClientVerifier::new(der_of(&client_ca.cert_pem), server_leaf.spki_sha256.clone())
                .unwrap();
        let ee = der_of(&server_leaf.cert_pem);
        assert!(v
            .verify_client_cert(&ee, &[], at(server_leaf.not_before + 3600))
            .is_err());
    }
}
