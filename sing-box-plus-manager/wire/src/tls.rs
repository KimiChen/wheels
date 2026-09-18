//! 装载部署材料并构建 TLS 配置。
//!
//! 放在共享 crate 里，是因为**主控与 agent 必须按同一套规则判定信任**。
//! 各写一份的话，「agent 认不认这张证书」与「主控认不认」会慢慢变成两个问题。
//!
//! 这里只做装载与配置，不做签发：签发是离线 workspace 的事，
//! 它的依赖（rcgen）不该出现在装在节点上的二进制里。

use std::path::Path;
use std::sync::Arc;

use rustls::pki_types::{CertificateDer, PrivateKeyDer};
use rustls::{ClientConfig, ServerConfig};

use crate::verify::{AuthorizedLeaves, ExactLeafClientVerifier, ExactLeafServerVerifier, Usage};
use crate::HmacKey;

/// 只用 TLS 1.3。
///
/// 两端都是自己的二进制，没有任何要兼容旧客户端的理由。
/// 少一个协议版本就少一整类降级与重协商问题。
const TLS13_ONLY: &[&rustls::SupportedProtocolVersion] = &[&rustls::version::TLS13];

#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
pub enum TlsError {
    #[error("{0}")]
    Load(String),
}

type Result<T> = std::result::Result<T, TlsError>;

pub fn load_certs(path: &Path) -> Result<Vec<Vec<u8>>> {
    let data = std::fs::read(path)
        .map_err(|e| TlsError::Load(format!("读取 {} 失败：{e}", path.display())))?;
    let mut reader = std::io::BufReader::new(&data[..]);
    let certs: std::result::Result<Vec<CertificateDer<'static>>, _> =
        rustls_pemfile::certs(&mut reader).collect();
    let certs = certs.map_err(|e| TlsError::Load(format!("解析 {} 失败：{e}", path.display())))?;
    if certs.is_empty() {
        return Err(TlsError::Load(format!("{} 里没有证书", path.display())));
    }
    Ok(certs.into_iter().map(|der| der.to_vec()).collect())
}

fn load_key(path: &Path) -> Result<PrivateKeyDer<'static>> {
    let data = std::fs::read(path)
        .map_err(|e| TlsError::Load(format!("读取 {} 失败：{e}", path.display())))?;
    let mut reader = std::io::BufReader::new(&data[..]);
    rustls_pemfile::private_key(&mut reader)
        .map_err(|e| TlsError::Load(format!("解析私钥 {} 失败：{e}", path.display())))?
        .ok_or_else(|| TlsError::Load(format!("{} 里没有私钥", path.display())))
}

pub fn load_hmac_key(path: &Path) -> Result<HmacKey> {
    let text = std::fs::read_to_string(path)
        .map_err(|e| TlsError::Load(format!("读取 HMAC key {} 失败：{e}", path.display())))?;
    HmacKey::from_hex(&text).map_err(|e| TlsError::Load(format!("HMAC key 不合法：{e}")))
}

/// 主控侧的 TLS 客户端配置：信任的是 `authorized-servers.pem` 里那一张 leaf。
pub fn client_config(controller_dir: &Path) -> Result<Arc<ClientConfig>> {
    let provider = crate::verify::ring_provider();
    let authorized = AuthorizedLeaves::new(
        load_certs(&controller_dir.join("authorized-servers.pem"))?,
        Usage::ServerAuth,
    )
    .map_err(|e| TlsError::Load(format!("装配授权 bundle 失败：{e}")))?;
    let chain: Vec<CertificateDer<'static>> = load_certs(&controller_dir.join("client-chain.pem"))?
        .into_iter()
        .map(CertificateDer::from)
        .collect();
    let key = load_key(&controller_dir.join("client-leaf.key"))?;

    let config = ClientConfig::builder_with_provider(provider.clone())
        .with_protocol_versions(TLS13_ONLY)
        .map_err(|e| TlsError::Load(format!("构建客户端配置失败：{e}")))?
        .dangerous()
        .with_custom_certificate_verifier(ExactLeafServerVerifier::new(authorized, provider))
        .with_client_auth_cert(chain, key)
        .map_err(|e| TlsError::Load(format!("装载客户端证书失败：{e}")))?;
    Ok(Arc::new(config))
}

/// agent 侧的 TLS 服务端配置：信任的是 `authorized-clients.pem` 里那一张 leaf。
pub fn server_config(agent_dir: &Path) -> Result<Arc<ServerConfig>> {
    let provider = crate::verify::ring_provider();
    let authorized = AuthorizedLeaves::new(
        load_certs(&agent_dir.join("authorized-clients.pem"))?,
        Usage::ClientAuth,
    )
    .map_err(|e| TlsError::Load(format!("装配授权 bundle 失败：{e}")))?;
    let chain: Vec<CertificateDer<'static>> = load_certs(&agent_dir.join("server-chain.pem"))?
        .into_iter()
        .map(CertificateDer::from)
        .collect();
    let key = load_key(&agent_dir.join("server-leaf.key"))?;

    let config = ServerConfig::builder_with_provider(provider.clone())
        .with_protocol_versions(TLS13_ONLY)
        .map_err(|e| TlsError::Load(format!("构建服务端配置失败：{e}")))?
        .with_client_cert_verifier(ExactLeafClientVerifier::new(authorized, provider))
        .with_single_cert(chain, key)
        .map_err(|e| TlsError::Load(format!("装载服务端证书失败：{e}")))?;
    Ok(Arc::new(config))
}
