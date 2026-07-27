//! PKI 原语：双 CA 生成、叶证书签发、SPKI 指纹、ring provider 安装。
//!
//! 全栈单一 ring provider，纯内存无 I/O。双 CA 角色分离（结构性杜绝角色混淆）：
//! `agent_ca` 只签 Agent 服务端证书（serverAuth），`client_ca` 只签唯一 Manager 客户端证书（clientAuth）。
//! Agent 服务端证书带 URI-SAN `sbm://host/<host_id>` 把主机身份绑进证书，供 [`verify`] 做结构化 pin。

use crate::error::{AppError, ErrorCode, Result};
use rcgen::{
    BasicConstraints, CertificateParams, DistinguishedName, DnType, ExtendedKeyUsagePurpose,
    Ia5String, IsCa, KeyPair, KeyUsagePurpose, SanType, SerialNumber,
};
use sha2::{Digest, Sha256};
use std::net::IpAddr;
use std::sync::Once;
use time::{Duration, OffsetDateTime};

pub mod enrollment;
pub mod verify;

/// URI-SAN 前缀，用于把 host_id 绑进 Agent 服务端证书。
pub const HOST_URI_PREFIX: &str = "sbm://host/";

/// 进程级安装 ring 为 rustls 默认 CryptoProvider（幂等；重复安装忽略）。
pub fn install_ring_default() {
    static ONCE: Once = Once::new();
    ONCE.call_once(|| {
        let _ = rustls::crypto::ring::default_provider().install_default();
    });
}

/// CA 角色。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum CaRole {
    AgentCa,
    ClientCa,
}

impl CaRole {
    pub fn as_str(self) -> &'static str {
        match self {
            CaRole::AgentCa => "agent_ca",
            CaRole::ClientCa => "client_ca",
        }
    }
    fn common_name(self) -> &'static str {
        match self {
            CaRole::AgentCa => "sing-box-manager agent CA",
            CaRole::ClientCa => "sing-box-manager client CA",
        }
    }
}

/// 叶证书 SAN 条目。
#[derive(Debug, Clone)]
pub enum SanEntry {
    Dns(String),
    Ip(IpAddr),
    Uri(String),
}

/// 一张生成的证书及其私钥（PEM）与元数据。`key_pem` 为敏感数据，调用方须尽快信封封存。
pub struct GeneratedCert {
    pub cert_pem: String,
    pub key_pem: String, // PKCS#8 PEM，敏感
    pub spki_sha256: String,
    pub not_before: i64,
    pub not_after: i64,
    pub serial: u64,
}

/// 从存库 PEM 重建的签发者（CA 公证书 + 私钥），用于签发叶证书。
pub struct Ca {
    cert: rcgen::Certificate,
    key: KeyPair,
}

impl Ca {
    /// 从存库的 CA 公证书 PEM 与（信封解密后的）私钥 PEM 重建签发者。
    pub fn from_pem(cert_pem: &str, key_pem: &str) -> Result<Self> {
        let key = KeyPair::from_pem(key_pem).map_err(|e| crypto_err("CA 私钥解析", e))?;
        let params = CertificateParams::from_ca_cert_pem(cert_pem)
            .map_err(|e| crypto_err("CA 证书解析", e))?;
        let cert = params
            .self_signed(&key)
            .map_err(|e| crypto_err("CA 重建", e))?;
        Ok(Self { cert, key })
    }
}

/// 生成一个自签根 CA。
pub fn generate_ca(role: CaRole, validity_days: i64) -> Result<GeneratedCert> {
    let key = KeyPair::generate().map_err(|e| crypto_err("CA 密钥生成", e))?;
    let mut p = CertificateParams::default();
    p.distinguished_name = dn(role.common_name());
    p.is_ca = IsCa::Ca(BasicConstraints::Unconstrained);
    p.key_usages = vec![
        KeyUsagePurpose::KeyCertSign,
        KeyUsagePurpose::CrlSign,
        KeyUsagePurpose::DigitalSignature,
    ];
    let (nb, na) = validity_window(validity_days);
    p.not_before = nb;
    p.not_after = na;
    p.serial_number = Some(SerialNumber::from(1u64));
    let cert = p.self_signed(&key).map_err(|e| crypto_err("CA 自签", e))?;
    Ok(assemble(cert, &key, nb, na, 1))
}

/// 用 agent_ca 签发某 Host 的 Agent 服务端证书。始终追加 URI-SAN `sbm://host/<host_id>`。
pub fn issue_agent_server_cert(
    ca: &Ca,
    host_id: &str,
    sans: &[SanEntry],
    serial: u64,
    validity_days: i64,
) -> Result<GeneratedCert> {
    let key = KeyPair::generate().map_err(|e| crypto_err("叶密钥生成", e))?;
    let mut p = CertificateParams::default();
    p.distinguished_name = dn(&format!("sbm agent {host_id}"));
    p.is_ca = IsCa::ExplicitNoCa;
    p.key_usages = vec![
        KeyUsagePurpose::DigitalSignature,
        KeyUsagePurpose::KeyEncipherment,
    ];
    p.extended_key_usages = vec![ExtendedKeyUsagePurpose::ServerAuth];
    let mut san_types = map_sans(sans)?;
    san_types.push(SanType::URI(ia5(&format!("{HOST_URI_PREFIX}{host_id}"))?));
    p.subject_alt_names = san_types;
    let (nb, na) = validity_window(validity_days);
    p.not_before = nb;
    p.not_after = na;
    p.serial_number = Some(SerialNumber::from(serial));
    let cert = p
        .signed_by(&key, &ca.cert, &ca.key)
        .map_err(|e| crypto_err("Agent 服务端证书签发", e))?;
    Ok(assemble(cert, &key, nb, na, serial))
}

/// 用 client_ca 签发唯一 Manager 客户端证书（clientAuth；身份由 SPKI pin 锁定，无需 SAN）。
pub fn issue_manager_client_cert(
    ca: &Ca,
    serial: u64,
    validity_days: i64,
) -> Result<GeneratedCert> {
    let key = KeyPair::generate().map_err(|e| crypto_err("客户端密钥生成", e))?;
    let mut p = CertificateParams::default();
    p.distinguished_name = dn("sing-box-manager controller");
    p.is_ca = IsCa::ExplicitNoCa;
    p.key_usages = vec![KeyUsagePurpose::DigitalSignature];
    p.extended_key_usages = vec![ExtendedKeyUsagePurpose::ClientAuth];
    let (nb, na) = validity_window(validity_days);
    p.not_before = nb;
    p.not_after = na;
    p.serial_number = Some(SerialNumber::from(serial));
    let cert = p
        .signed_by(&key, &ca.cert, &ca.key)
        .map_err(|e| crypto_err("Manager 客户端证书签发", e))?;
    Ok(assemble(cert, &key, nb, na, serial))
}

/// PEM → 证书链 DER。
pub fn certs_from_pem(pem: &str) -> Result<Vec<rustls_pki_types::CertificateDer<'static>>> {
    rustls_pemfile::certs(&mut pem.as_bytes())
        .collect::<std::result::Result<Vec<_>, _>>()
        .map_err(|e| AppError::new(ErrorCode::Crypto, format!("解析证书链失败: {e}")))
}

/// PEM → 单张证书 DER（取第一张）。
pub fn one_cert_from_pem(pem: &str) -> Result<rustls_pki_types::CertificateDer<'static>> {
    certs_from_pem(pem)?
        .into_iter()
        .next()
        .ok_or_else(|| AppError::new(ErrorCode::Crypto, "PEM 无证书"))
}

/// PEM → PKCS#8 私钥 DER。
pub fn private_key_from_pem(pem: &str) -> Result<rustls_pki_types::PrivateKeyDer<'static>> {
    rustls_pemfile::private_key(&mut pem.as_bytes())
        .map_err(|e| AppError::new(ErrorCode::Crypto, format!("解析私钥失败: {e}")))?
        .ok_or_else(|| AppError::new(ErrorCode::Crypto, "PEM 无私钥"))
}

/// SubjectPublicKeyInfo DER 的 sha256（hex 小写）。签发侧与校验侧用同一字节表示，指纹可比。
pub fn spki_sha256_from_der(spki_der: &[u8]) -> String {
    let mut h = Sha256::new();
    h.update(spki_der);
    hex_lower(&h.finalize())
}

/// 任意字节的 sha256（hex 小写）。请求体哈希、指纹等通用。
pub fn sha256_hex(bytes: &[u8]) -> String {
    let mut h = Sha256::new();
    h.update(bytes);
    hex_lower(&h.finalize())
}

fn assemble(
    cert: rcgen::Certificate,
    key: &KeyPair,
    nb: OffsetDateTime,
    na: OffsetDateTime,
    serial: u64,
) -> GeneratedCert {
    let spki = key.public_key_der();
    GeneratedCert {
        cert_pem: cert.pem(),
        key_pem: key.serialize_pem(),
        spki_sha256: spki_sha256_from_der(&spki),
        not_before: nb.unix_timestamp(),
        not_after: na.unix_timestamp(),
        serial,
    }
}

fn dn(cn: &str) -> DistinguishedName {
    let mut d = DistinguishedName::new();
    d.push(DnType::CommonName, cn);
    d
}

/// 回退 5 分钟容忍时钟偏移；`validity_days` 决定 not_after。
fn validity_window(days: i64) -> (OffsetDateTime, OffsetDateTime) {
    let now = OffsetDateTime::now_utc();
    (now - Duration::minutes(5), now + Duration::days(days))
}

fn map_sans(sans: &[SanEntry]) -> Result<Vec<SanType>> {
    sans.iter()
        .map(|e| {
            Ok(match e {
                SanEntry::Dns(s) => SanType::DnsName(ia5(s)?),
                SanEntry::Ip(ip) => SanType::IpAddress(*ip),
                SanEntry::Uri(s) => SanType::URI(ia5(s)?),
            })
        })
        .collect()
}

fn ia5(s: &str) -> Result<Ia5String> {
    Ia5String::try_from(s).map_err(|_| AppError::new(ErrorCode::Crypto, "非法 IA5 SAN 字符串"))
}

fn hex_lower(bytes: &[u8]) -> String {
    use std::fmt::Write;
    let mut s = String::with_capacity(bytes.len() * 2);
    for b in bytes {
        let _ = write!(s, "{b:02x}");
    }
    s
}

/// 把证书/密钥库的错误折叠为统一错误码。Display 不含密钥材料，可安全入日志。
fn crypto_err<E: std::fmt::Display>(what: &str, e: E) -> AppError {
    AppError::new(ErrorCode::Crypto, format!("{what}失败: {e}"))
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::net::Ipv4Addr;
    use x509_parser::prelude::FromDer;

    fn parse(pem: &str) -> Vec<u8> {
        // PEM -> 单张证书 DER
        let der = rustls_pemfile::certs(&mut pem.as_bytes())
            .next()
            .unwrap()
            .unwrap();
        der.as_ref().to_vec()
    }

    #[test]
    fn generate_cas_and_issue_leaves_with_expected_extensions() {
        install_ring_default();
        let agent_ca = generate_ca(CaRole::AgentCa, 3650).unwrap();
        let client_ca = generate_ca(CaRole::ClientCa, 3650).unwrap();

        let ca = Ca::from_pem(&agent_ca.cert_pem, &agent_ca.key_pem).unwrap();
        let leaf = issue_agent_server_cert(
            &ca,
            "host-abc",
            &[SanEntry::Ip(IpAddr::V4(Ipv4Addr::LOCALHOST))],
            7,
            825,
        )
        .unwrap();

        let der = parse(&leaf.cert_pem);
        let (_, cert) = x509_parser::certificate::X509Certificate::from_der(&der).unwrap();

        // basicConstraints CA:FALSE
        assert!(!cert.basic_constraints().unwrap().unwrap().value.ca);
        // EKU serverAuth（非 clientAuth）
        let eku = cert.extended_key_usage().unwrap().unwrap().value;
        assert!(eku.server_auth && !eku.client_auth);
        // SAN 含 URI host_id 与 IP
        let san = cert.subject_alternative_name().unwrap().unwrap().value;
        let uris: Vec<&str> = san
            .general_names
            .iter()
            .filter_map(|g| match g {
                x509_parser::extensions::GeneralName::URI(u) => Some(*u),
                _ => None,
            })
            .collect();
        assert!(uris.contains(&"sbm://host/host-abc"));
        assert!(san
            .general_names
            .iter()
            .any(|g| matches!(g, x509_parser::extensions::GeneralName::IPAddress(_))));
        // 序列自增（issue 传入 7）
        assert_eq!(leaf.serial, 7);

        // 客户端证书：clientAuth，非 serverAuth
        let cca = Ca::from_pem(&client_ca.cert_pem, &client_ca.key_pem).unwrap();
        let mc = issue_manager_client_cert(&cca, 2, 825).unwrap();
        let mder = parse(&mc.cert_pem);
        let (_, mcert) = x509_parser::certificate::X509Certificate::from_der(&mder).unwrap();
        let meku = mcert.extended_key_usage().unwrap().unwrap().value;
        assert!(meku.client_auth && !meku.server_auth);
        // SPKI 指纹与 x509 解析一致
        assert_eq!(mc.spki_sha256, spki_sha256_from_der(mcert.public_key().raw));
    }
}
