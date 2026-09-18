//! 精确 leaf 授信（`docs/threat-model.md` §2.2）。
//!
//! 这是最反直觉、也最容易被简化掉的一条。
//!
//! 许多 TLS 实现**没有「期望对端 SAN 等于某字符串」的独立 matcher**：它们验证证书链，
//! 但不会按配置比对对端 SAN。如果授权 bundle 里放的是 intermediate，
//! 那么**任何由该 intermediate 签出的证书都能握手**——包括不该被信任的那些。
//!
//! 所以这里做的是：把**那一张离线校验过的 leaf 本身**当作对端的信任锚，
//! 握手时逐字节比对对端出示的 end-entity 证书。intermediate 完全不参与信任判定。
//!
//! **必须诚实地写明：这实现的是运行时精确证书授权，不是运行时逐字符串比较 SAN。**
//! SAN 与 EKU 的精确校验发生在**离线签发阶段**；这里再查一次 EKU 与有效期，
//! 挡的是另一类事故——**有人把一张不该进 bundle 的证书放进了 bundle**。
//!
//! 还有一条护栏：**调整签发器或证书 Subject 之后，必须重跑真实组件的正负矩阵**，
//! 不能只做链验证。链验证通过不代表那个实现的选择逻辑还成立。
//!
//! 任何解析失败一律 fail-closed。

use std::sync::Arc;

use rustls::client::danger::{HandshakeSignatureValid, ServerCertVerified, ServerCertVerifier};
use rustls::crypto::CryptoProvider;
use rustls::pki_types::{CertificateDer, ServerName, UnixTime};
use rustls::server::danger::{ClientCertVerified, ClientCertVerifier};
use rustls::{CertificateError, DigitallySignedStruct, DistinguishedName, Error, SignatureScheme};
use sha2::{Digest, Sha256};

#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
pub enum VerifyError {
    #[error("证书不是合法 DER：{0}")]
    BadDer(String),
    #[error("授权 bundle 是空的：一个空 bundle 会让所有对端都被拒，但更可能是装配出错")]
    EmptyBundle,
    #[error("EKU 必须**单一**（只有 {expected}），实际 {actual}")]
    EkuNotSingle { expected: &'static str, actual: String },
    #[error("证书缺少 EKU 扩展")]
    EkuMissing,
}

/// 需要的用途。server leaf 只有 `serverAuth`，client leaf 只有 `clientAuth`。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Usage {
    ServerAuth,
    ClientAuth,
}

impl Usage {
    fn name(self) -> &'static str {
        match self {
            Usage::ServerAuth => "serverAuth",
            Usage::ClientAuth => "clientAuth",
        }
    }
}

fn application_failure() -> Error {
    Error::InvalidCertificate(CertificateError::ApplicationVerificationFailure)
}

/// SPKI 的 SHA-256，十六进制。带外核对授信用的就是这个指纹（README §4.2）。
pub fn spki_sha256(der: &[u8]) -> Result<String, VerifyError> {
    use x509_parser::prelude::*;
    let (_, cert) =
        X509Certificate::from_der(der).map_err(|e| VerifyError::BadDer(e.to_string()))?;
    Ok(hex::encode(Sha256::digest(cert.public_key().raw)))
}

/// 离线签发阶段之外的第二道：EKU 必须**恰好**是要求的那一个。
///
/// 「恰好」很重要：一张同时带 `serverAuth` 与 `clientAuth` 的证书能在两个方向上冒充，
/// 而 `anyExtendedKeyUsage` 等于没有限制。
pub fn assert_single_eku(der: &[u8], usage: Usage) -> Result<(), VerifyError> {
    use x509_parser::prelude::*;
    let (_, cert) =
        X509Certificate::from_der(der).map_err(|e| VerifyError::BadDer(e.to_string()))?;
    let eku = cert
        .extended_key_usage()
        .map_err(|e| VerifyError::BadDer(e.to_string()))?
        .ok_or(VerifyError::EkuMissing)?
        .value;

    let mut present = Vec::new();
    if eku.any {
        present.push("any".to_string());
    }
    if eku.server_auth {
        present.push("serverAuth".to_string());
    }
    if eku.client_auth {
        present.push("clientAuth".to_string());
    }
    if eku.code_signing {
        present.push("codeSigning".to_string());
    }
    if eku.email_protection {
        present.push("emailProtection".to_string());
    }
    if eku.time_stamping {
        present.push("timeStamping".to_string());
    }
    if eku.ocsp_signing {
        present.push("ocspSigning".to_string());
    }
    for other in &eku.other {
        present.push(other.to_id_string());
    }

    if present.len() == 1 && present[0] == usage.name() {
        return Ok(());
    }
    Err(VerifyError::EkuNotSingle {
        expected: usage.name(),
        actual: if present.is_empty() { "（空）".into() } else { present.join(" + ") },
    })
}

/// 有效期。不走 webpki 的链校验，所以这一项必须自己查。
fn within_validity(der: &[u8], now: UnixTime) -> bool {
    use x509_parser::prelude::*;
    let Ok((_, cert)) = X509Certificate::from_der(der) else {
        return false;
    };
    let now = now.as_secs() as i64;
    let validity = cert.validity();
    validity.not_before.timestamp() <= now && now <= validity.not_after.timestamp()
}

/// 授权 bundle：一组被精确授信的 leaf。
///
/// **吊销的实质就是从这里移除旧 leaf**（`docs/threat-model.md` §2.3）。
/// 证书上的 `REVOKED` 标记只是离线审计记录，不会让任何在线组件拒绝谁。
#[derive(Debug, Clone)]
pub struct AuthorizedLeaves {
    leaves: Vec<Vec<u8>>,
    usage: Usage,
}

impl AuthorizedLeaves {
    /// 装配 bundle。每张 leaf 在**进 bundle 时**就查一次 EKU——
    /// 装配期报错比握手期报错容易归因得多。
    pub fn new(leaves: Vec<Vec<u8>>, usage: Usage) -> Result<Self, VerifyError> {
        if leaves.is_empty() {
            return Err(VerifyError::EmptyBundle);
        }
        for der in &leaves {
            assert_single_eku(der, usage)?;
        }
        Ok(AuthorizedLeaves { leaves, usage })
    }

    pub fn len(&self) -> usize {
        self.leaves.len()
    }

    pub fn is_empty(&self) -> bool {
        self.leaves.is_empty()
    }

    pub fn usage(&self) -> Usage {
        self.usage
    }

    pub fn fingerprints(&self) -> Vec<String> {
        self.leaves.iter().filter_map(|der| spki_sha256(der).ok()).collect()
    }

    /// 逐字节比对。证书是公开材料，不需要常量时间比较。
    fn authorizes(&self, presented: &CertificateDer<'_>) -> bool {
        self.leaves.iter().any(|der| der.as_slice() == presented.as_ref())
    }
}

/// 主控侧：验证 agent 出示的服务端证书就是 bundle 里那一张。
#[derive(Debug)]
pub struct ExactLeafServerVerifier {
    authorized: AuthorizedLeaves,
    provider: Arc<CryptoProvider>,
}

impl ExactLeafServerVerifier {
    pub fn new(authorized: AuthorizedLeaves, provider: Arc<CryptoProvider>) -> Arc<Self> {
        Arc::new(ExactLeafServerVerifier { authorized, provider })
    }
}

impl ServerCertVerifier for ExactLeafServerVerifier {
    fn verify_server_cert(
        &self,
        end_entity: &CertificateDer<'_>,
        _intermediates: &[CertificateDer<'_>],
        _server_name: &ServerName<'_>,
        _ocsp_response: &[u8],
        now: UnixTime,
    ) -> Result<ServerCertVerified, Error> {
        // intermediate 刻意不参与判定：由同一个 intermediate 新签的另一张 leaf
        // 必须在这里被拒，这正是本模块存在的理由。
        if !self.authorized.authorizes(end_entity) {
            return Err(application_failure());
        }
        if !within_validity(end_entity, now) {
            return Err(Error::InvalidCertificate(CertificateError::Expired));
        }
        if assert_single_eku(end_entity, Usage::ServerAuth).is_err() {
            return Err(application_failure());
        }
        Ok(ServerCertVerified::assertion())
    }

    fn verify_tls12_signature(
        &self,
        message: &[u8],
        cert: &CertificateDer<'_>,
        dss: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, Error> {
        rustls::crypto::verify_tls12_signature(
            message,
            cert,
            dss,
            &self.provider.signature_verification_algorithms,
        )
    }

    fn verify_tls13_signature(
        &self,
        message: &[u8],
        cert: &CertificateDer<'_>,
        dss: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, Error> {
        rustls::crypto::verify_tls13_signature(
            message,
            cert,
            dss,
            &self.provider.signature_verification_algorithms,
        )
    }

    fn supported_verify_schemes(&self) -> Vec<SignatureScheme> {
        self.provider.signature_verification_algorithms.supported_schemes()
    }
}

/// agent 侧：验证主控出示的客户端证书就是 bundle 里那一张。
#[derive(Debug)]
pub struct ExactLeafClientVerifier {
    authorized: AuthorizedLeaves,
    provider: Arc<CryptoProvider>,
    /// 不给 CA 提示：我们本来就不信任任何 CA，给出提示等于暗示「链到它就行」。
    no_hints: Vec<DistinguishedName>,
}

impl ExactLeafClientVerifier {
    pub fn new(authorized: AuthorizedLeaves, provider: Arc<CryptoProvider>) -> Arc<Self> {
        Arc::new(ExactLeafClientVerifier { authorized, provider, no_hints: Vec::new() })
    }
}

impl ClientCertVerifier for ExactLeafClientVerifier {
    fn root_hint_subjects(&self) -> &[DistinguishedName] {
        &self.no_hints
    }

    fn verify_client_cert(
        &self,
        end_entity: &CertificateDer<'_>,
        _intermediates: &[CertificateDer<'_>],
        now: UnixTime,
    ) -> Result<ClientCertVerified, Error> {
        if !self.authorized.authorizes(end_entity) {
            return Err(application_failure());
        }
        if !within_validity(end_entity, now) {
            return Err(Error::InvalidCertificate(CertificateError::Expired));
        }
        if assert_single_eku(end_entity, Usage::ClientAuth).is_err() {
            return Err(application_failure());
        }
        Ok(ClientCertVerified::assertion())
    }

    fn verify_tls12_signature(
        &self,
        message: &[u8],
        cert: &CertificateDer<'_>,
        dss: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, Error> {
        rustls::crypto::verify_tls12_signature(
            message,
            cert,
            dss,
            &self.provider.signature_verification_algorithms,
        )
    }

    fn verify_tls13_signature(
        &self,
        message: &[u8],
        cert: &CertificateDer<'_>,
        dss: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, Error> {
        rustls::crypto::verify_tls13_signature(
            message,
            cert,
            dss,
            &self.provider.signature_verification_algorithms,
        )
    }

    fn supported_verify_schemes(&self) -> Vec<SignatureScheme> {
        self.provider.signature_verification_algorithms.supported_schemes()
    }

    fn offer_client_auth(&self) -> bool {
        true
    }

    fn client_auth_mandatory(&self) -> bool {
        true
    }
}

/// 全栈单一 provider：ring（D9）。
///
/// 不用 `CryptoProvider::get_default()`——那取决于进程里谁先装了默认 provider，
/// 是一个会随依赖顺序变化的隐含前提。这里显式返回 ring。
pub fn ring_provider() -> Arc<CryptoProvider> {
    Arc::new(rustls::crypto::ring::default_provider())
}
