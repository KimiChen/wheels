//! 离线签发器（`docs/threat-model.md` §2）。
//!
//! 结构：
//!
//! ```text
//! 离线 root CA                      只签 intermediate，私钥不出离线 workspace
//! └─ 每节点 / 每 generation
//!    ├─ server intermediate CA      该节点专属
//!    ├─ client intermediate CA      该节点专属
//!    ├─ server leaf / client leaf   固定 SAN，EKU 单一
//!    └─ deployment/                 节点只取这一层的最小材料
//! ```
//!
//! **每个节点有自己的一对 intermediate**，不是全网共用一个——
//! 这样吊销一个节点不牵动其他节点。
//!
//! 六项校验必须在**签发阶段**完成，因为运行时不一定有能力再做一次：
//! SAN 精确匹配、EKU 单一、key 与 cert 匹配、serial 唯一、部署文件集最小、权限正确。
//! [`verify_issued`] 把这六项做成可执行的断言，而不是签发流程里的口头承诺。

use std::collections::BTreeSet;
use std::path::{Path, PathBuf};

use rcgen::{
    BasicConstraints, CertificateParams, DistinguishedName, DnType, ExtendedKeyUsagePurpose,
    Ia5String, IsCa, KeyPair, KeyUsagePurpose, SanType, SerialNumber,
};

use crate::error::{Error, Result};

/// URI-SAN 前缀。节点与主控各有固定形状，签发时精确匹配。
pub const NODE_URI_PREFIX: &str = "pm://node/";
pub const CONTROLLER_URI_PREFIX: &str = "pm://controller/";

/// 部署目录里**允许存在的全部文件**。多一个少一个都算装配出错。
///
/// 「部署文件集最小」这条校验的价值在于：多出来的文件往往是私钥。
/// 一次手滑把 intermediate 的 key 拷进 deployment/，整条信任链就下放到节点上了。
pub const AGENT_DEPLOYMENT_FILES: &[&str] = &[
    "server-leaf.key",
    "server-leaf.pem",
    "server-chain.pem",
    "authorized-clients.pem",
    "hmac.key",
];
pub const CONTROLLER_DEPLOYMENT_FILES: &[&str] = &[
    "client-leaf.key",
    "client-leaf.pem",
    "client-chain.pem",
    "authorized-servers.pem",
    "hmac.key",
];

#[derive(Debug, Clone)]
pub struct Validity {
    pub root_days: i64,
    pub intermediate_days: i64,
    pub leaf_days: i64,
}

impl Default for Validity {
    fn default() -> Self {
        // leaf 短、CA 长：轮换的是叶子，root 进离线保险箱。
        Validity { root_days: 3650, intermediate_days: 1095, leaf_days: 90 }
    }
}

/// 一张签发出来的证书及其私钥。
pub struct Issued {
    pub cert: rcgen::Certificate,
    pub key: KeyPair,
    pub serial: u64,
}

impl Issued {
    pub fn cert_pem(&self) -> String {
        self.cert.pem()
    }

    pub fn key_pem(&self) -> String {
        self.key.serialize_pem()
    }

    pub fn der(&self) -> Vec<u8> {
        self.cert.der().to_vec()
    }
}

fn dn(common_name: &str) -> DistinguishedName {
    let mut name = DistinguishedName::new();
    name.push(DnType::CommonName, common_name);
    name
}

fn window(days: i64) -> (time::OffsetDateTime, time::OffsetDateTime) {
    let now = time::OffsetDateTime::now_utc();
    (now - time::Duration::hours(1), now + time::Duration::days(days))
}

fn ia5(value: &str) -> Result<Ia5String> {
    Ia5String::try_from(value.to_string())
        .map_err(|e| Error::Pki(format!("SAN {value:?} 不是合法 IA5：{e}")))
}

/// 生成离线 root CA。**私钥不出离线 workspace。**
pub fn generate_root(validity: &Validity, serial: u64) -> Result<Issued> {
    let key = KeyPair::generate().map_err(|e| Error::Pki(format!("root 密钥生成失败：{e}")))?;
    let mut params = CertificateParams::default();
    params.distinguished_name = dn("proxy-manager offline root");
    params.is_ca = IsCa::Ca(BasicConstraints::Constrained(1));
    params.key_usages = vec![
        KeyUsagePurpose::KeyCertSign,
        KeyUsagePurpose::CrlSign,
        KeyUsagePurpose::DigitalSignature,
    ];
    let (not_before, not_after) = window(validity.root_days);
    params.not_before = not_before;
    params.not_after = not_after;
    params.serial_number = Some(SerialNumber::from(serial));
    let cert = params.self_signed(&key).map_err(|e| Error::Pki(format!("root 自签失败：{e}")))?;
    Ok(Issued { cert, key, serial })
}

/// 用 root 签一张该节点专属的 intermediate。
///
/// `BasicConstraints::Constrained(0)` —— intermediate 只能签叶子，不能再签 CA。
pub fn issue_intermediate(
    root: &Issued,
    common_name: &str,
    validity: &Validity,
    serial: u64,
) -> Result<Issued> {
    let key =
        KeyPair::generate().map_err(|e| Error::Pki(format!("intermediate 密钥生成失败：{e}")))?;
    let mut params = CertificateParams::default();
    params.distinguished_name = dn(common_name);
    params.is_ca = IsCa::Ca(BasicConstraints::Constrained(0));
    params.key_usages = vec![KeyUsagePurpose::KeyCertSign, KeyUsagePurpose::DigitalSignature];
    let (not_before, not_after) = window(validity.intermediate_days);
    params.not_before = not_before;
    params.not_after = not_after;
    params.serial_number = Some(SerialNumber::from(serial));
    let cert = params
        .signed_by(&key, &root.cert, &root.key)
        .map_err(|e| Error::Pki(format!("intermediate 签发失败：{e}")))?;
    Ok(Issued { cert, key, serial })
}

/// 签发 server leaf。**EKU 单一**：只有 `serverAuth`。
pub fn issue_server_leaf(
    intermediate: &Issued,
    node_id: &str,
    dns_name: &str,
    validity: &Validity,
    serial: u64,
) -> Result<Issued> {
    let key = KeyPair::generate().map_err(|e| Error::Pki(format!("server 叶密钥生成失败：{e}")))?;
    let mut params = CertificateParams::default();
    params.distinguished_name = dn(&format!("proxy-manager-agent {node_id}"));
    params.is_ca = IsCa::ExplicitNoCa;
    params.key_usages = vec![KeyUsagePurpose::DigitalSignature, KeyUsagePurpose::KeyEncipherment];
    // 单一 EKU：一张同时带 serverAuth 与 clientAuth 的证书能在两个方向上冒充。
    params.extended_key_usages = vec![ExtendedKeyUsagePurpose::ServerAuth];
    params.subject_alt_names = vec![
        SanType::DnsName(ia5(dns_name)?),
        SanType::URI(ia5(&format!("{NODE_URI_PREFIX}{node_id}"))?),
    ];
    let (not_before, not_after) = window(validity.leaf_days);
    params.not_before = not_before;
    params.not_after = not_after;
    params.serial_number = Some(SerialNumber::from(serial));
    let cert = params
        .signed_by(&key, &intermediate.cert, &intermediate.key)
        .map_err(|e| Error::Pki(format!("server leaf 签发失败：{e}")))?;
    Ok(Issued { cert, key, serial })
}

/// 签发 client leaf。**EKU 单一**：只有 `clientAuth`。
///
/// 每节点一张独立的 client leaf：主控对每个节点用不同的客户端身份，
/// 于是「泄露一张主控证书」的影响范围是一个节点而不是全网。
pub fn issue_client_leaf(
    intermediate: &Issued,
    node_id: &str,
    validity: &Validity,
    serial: u64,
) -> Result<Issued> {
    let key = KeyPair::generate().map_err(|e| Error::Pki(format!("client 叶密钥生成失败：{e}")))?;
    let mut params = CertificateParams::default();
    params.distinguished_name = dn(&format!("proxy-manager controller for {node_id}"));
    params.is_ca = IsCa::ExplicitNoCa;
    params.key_usages = vec![KeyUsagePurpose::DigitalSignature, KeyUsagePurpose::KeyEncipherment];
    params.extended_key_usages = vec![ExtendedKeyUsagePurpose::ClientAuth];
    params.subject_alt_names =
        vec![SanType::URI(ia5(&format!("{CONTROLLER_URI_PREFIX}{node_id}"))?)];
    let (not_before, not_after) = window(validity.leaf_days);
    params.not_before = not_before;
    params.not_after = not_after;
    params.serial_number = Some(SerialNumber::from(serial));
    let cert = params
        .signed_by(&key, &intermediate.cert, &intermediate.key)
        .map_err(|e| Error::Pki(format!("client leaf 签发失败：{e}")))?;
    Ok(Issued { cert, key, serial })
}

/// 签发阶段的六项校验，逐条可执行。
///
/// 这些校验必须在这里做完，因为**运行时不一定有能力再做一次**
/// （`docs/threat-model.md` §2.1）。运行时的 verifier 只做精确 leaf 比对与有效期，
/// 它看不出「这张证书的 SAN 本来就不该是这个」。
pub struct IssuanceCheck<'a> {
    pub der: &'a [u8],
    pub key_pem: &'a str,
    pub expected_usage: proxy_manager_wire::verify::Usage,
    pub expected_sans: &'a [String],
    pub serial: u64,
    pub used_serials: &'a BTreeSet<u64>,
}

pub fn verify_issued(check: &IssuanceCheck<'_>) -> Result<()> {
    use x509_parser::prelude::*;

    // 2) EKU 单一。
    proxy_manager_wire::verify::assert_single_eku(check.der, check.expected_usage)
        .map_err(|e| Error::Pki(format!("EKU 校验失败：{e}")))?;

    let (_, cert) = X509Certificate::from_der(check.der)
        .map_err(|e| Error::Pki(format!("证书不是合法 DER：{e}")))?;

    // 1) SAN 精确匹配：集合相等，不是「包含」。
    //    「包含」会放过一张多带了一个 SAN 的证书，而多出来的那个正是冒充用的。
    let mut actual = BTreeSet::new();
    if let Ok(Some(san)) = cert.subject_alternative_name() {
        for name in &san.value.general_names {
            match name {
                GeneralName::DNSName(value) => {
                    actual.insert(format!("DNS:{value}"));
                }
                GeneralName::URI(value) => {
                    actual.insert(format!("URI:{value}"));
                }
                other => {
                    actual.insert(format!("OTHER:{other:?}"));
                }
            }
        }
    }
    let expected: BTreeSet<String> = check.expected_sans.iter().cloned().collect();
    if actual != expected {
        return Err(Error::Pki(format!("SAN 不精确匹配：期望 {expected:?}，实际 {actual:?}")));
    }

    // 3) key 与 cert 匹配：用私钥重算公钥，比对 SPKI。
    let key =
        KeyPair::from_pem(check.key_pem).map_err(|e| Error::Pki(format!("私钥解析失败：{e}")))?;
    if key.public_key_der() != cert.public_key().raw {
        return Err(Error::Pki("私钥与证书的公钥不匹配".into()));
    }

    // 4) serial 唯一。
    if check.used_serials.contains(&check.serial) {
        return Err(Error::Pki(format!("serial {} 已被使用过", check.serial)));
    }

    Ok(())
}

/// 5) 部署文件集**最小**：目录里的文件必须与清单**完全相等**。
pub fn verify_deployment_set(dir: &Path, expected: &[&str]) -> Result<()> {
    let mut actual = BTreeSet::new();
    let entries = std::fs::read_dir(dir)
        .map_err(|e| Error::Pki(format!("读取部署目录 {} 失败：{e}", dir.display())))?;
    for entry in entries {
        let entry = entry.map_err(|e| Error::Pki(format!("遍历部署目录失败：{e}")))?;
        actual.insert(entry.file_name().to_string_lossy().to_string());
    }
    let expected: BTreeSet<String> = expected.iter().map(|s| s.to_string()).collect();
    if actual != expected {
        let extra: Vec<_> = actual.difference(&expected).collect();
        let missing: Vec<_> = expected.difference(&actual).collect();
        return Err(Error::Pki(format!(
            "部署文件集不最小：{} 多出 {extra:?}，缺少 {missing:?}。\
             多出来的文件往往是私钥——一次手滑把 intermediate 的 key 拷进来，\
             整条信任链就下放到节点上了",
            dir.display()
        )));
    }
    Ok(())
}

/// 6) 权限正确：私钥 0600，目录 0700。
#[cfg(unix)]
pub fn verify_permissions(path: &Path) -> Result<()> {
    use std::os::unix::fs::PermissionsExt;
    let metadata = std::fs::metadata(path)
        .map_err(|e| Error::Pki(format!("读取 {} 的权限失败：{e}", path.display())))?;
    let mode = metadata.permissions().mode() & 0o777;
    let expected = if metadata.is_dir() { 0o700 } else { 0o600 };
    let is_key = path.extension().is_some_and(|ext| ext == "key");
    if (metadata.is_dir() || is_key) && mode != expected {
        return Err(Error::Pki(format!("{} 的权限是 {mode:o}，期望 {expected:o}", path.display())));
    }
    Ok(())
}

#[cfg(not(unix))]
pub fn verify_permissions(_path: &Path) -> Result<()> {
    Ok(())
}

/// 写一个文件并立刻设成目标权限。
///
/// 先写后 chmod 有一个窗口，所以私钥用 `OpenOptions` 在**创建时**就带上 0600，
/// 而不是写完再改——那个窗口里文件是 0644。
pub fn write_secret(path: &PathBuf, contents: &str) -> Result<()> {
    write_with_mode(path, contents, 0o600)
}

pub fn write_public(path: &PathBuf, contents: &str) -> Result<()> {
    write_with_mode(path, contents, 0o644)
}

#[cfg(unix)]
fn write_with_mode(path: &PathBuf, contents: &str, mode: u32) -> Result<()> {
    use std::io::Write;
    use std::os::unix::fs::{OpenOptionsExt, PermissionsExt};
    let mut file = std::fs::OpenOptions::new()
        .write(true)
        .create(true)
        .truncate(true)
        .mode(mode)
        .open(path)
        .map_err(|e| Error::Pki(format!("写入 {} 失败：{e}", path.display())))?;
    file.write_all(contents.as_bytes())
        .map_err(|e| Error::Pki(format!("写入 {} 失败：{e}", path.display())))?;
    // 已存在的文件不会被 .mode() 影响，补一次 chmod。
    let permissions = std::fs::Permissions::from_mode(mode);
    std::fs::set_permissions(path, permissions)
        .map_err(|e| Error::Pki(format!("设置 {} 权限失败：{e}", path.display())))?;
    Ok(())
}

#[cfg(not(unix))]
fn write_with_mode(path: &PathBuf, contents: &str, _mode: u32) -> Result<()> {
    std::fs::write(path, contents)
        .map_err(|e| Error::Pki(format!("写入 {} 失败：{e}", path.display())))
}

#[cfg(unix)]
pub fn create_private_dir(path: &Path) -> Result<()> {
    use std::os::unix::fs::DirBuilderExt;
    if path.is_dir() {
        return Ok(());
    }
    std::fs::DirBuilder::new()
        .recursive(true)
        .mode(0o700)
        .create(path)
        .map_err(|e| Error::Pki(format!("创建目录 {} 失败：{e}", path.display())))
}

#[cfg(not(unix))]
pub fn create_private_dir(path: &Path) -> Result<()> {
    std::fs::create_dir_all(path)
        .map_err(|e| Error::Pki(format!("创建目录 {} 失败：{e}", path.display())))
}
