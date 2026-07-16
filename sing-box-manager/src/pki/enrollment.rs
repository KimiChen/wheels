//! EnrollmentPackage：Manager 带外交付给主机的信任引导包。
//!
//! 恰含 §13 三要素——Host id、Agent 服务端证书（+私钥）、Manager 信任锚（client_ca）——外加 Manager
//! 客户端 SPKI pin。**刻意不含** agent_ca（Agent 无需验证其他 Agent）与任何 CA / Manager 私钥。
//! 包本身不签名：完整性由 Manager 打印的指纹 + 管理员带外核对保证（人工 OOB 信道即信任根）。

use crate::error::{AppError, ErrorCode, Result};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

/// enrollment 包格式版本。
pub const ENROLLMENT_VERSION: u32 = 1;

#[derive(Serialize, Deserialize, Clone)]
pub struct EnrollmentPackage {
    pub version: u32,
    pub host_id: String,
    /// Agent 监听绑定地址，如 `127.0.0.1:39736` 或 `<LAN_IP>:39736`。
    pub mgmt_bind: String,
    pub agent_server_cert_pem: String,
    /// PKCS#8 PEM，敏感——仅本文件明文承载，落地须 0600 且 gitignore。
    pub agent_server_key_pem: String,
    /// Agent 用作客户端信任锚（验证 Manager）。
    pub client_ca_cert_pem: String,
    /// Manager 客户端证书 SPKI 指纹，Agent 在握手层强制。
    pub manager_client_spki_sha256: String,
    pub issued_at: i64,
    pub not_after: i64,
}

impl EnrollmentPackage {
    /// 规范字节（用于指纹）。serde_json 按字段声明顺序序列化，结果确定。
    pub fn canonical_bytes(&self) -> Result<Vec<u8>> {
        serde_json::to_vec(self)
            .map_err(|e| AppError::new(ErrorCode::Internal, format!("enrollment 序列化失败: {e}")))
    }

    /// 整包指纹（sha256 hex 小写）。Manager 打印、Agent 复算，供管理员带外核对。
    pub fn fingerprint(&self) -> Result<String> {
        let bytes = self.canonical_bytes()?;
        let mut h = Sha256::new();
        h.update(&bytes);
        Ok(hex_lower(&h.finalize()))
    }

    pub fn to_json(&self) -> Result<String> {
        serde_json::to_string_pretty(self)
            .map_err(|e| AppError::new(ErrorCode::Internal, format!("enrollment 序列化失败: {e}")))
    }

    pub fn parse(s: &str) -> Result<Self> {
        let pkg: Self = serde_json::from_str(s).map_err(|e| {
            AppError::new(ErrorCode::Validation, format!("enrollment 解析失败: {e}"))
        })?;
        if pkg.version != ENROLLMENT_VERSION {
            return Err(AppError::new(
                ErrorCode::Validation,
                format!("enrollment 版本不支持: {}", pkg.version),
            ));
        }
        Ok(pkg)
    }
}

// Debug 脱敏：绝不打印证书/私钥字节（避免误入日志/审计）。
impl std::fmt::Debug for EnrollmentPackage {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("EnrollmentPackage")
            .field("version", &self.version)
            .field("host_id", &self.host_id)
            .field("mgmt_bind", &self.mgmt_bind)
            .field("agent_server_cert_pem", &"<redacted>")
            .field("agent_server_key_pem", &"<redacted>")
            .field("client_ca_cert_pem", &"<redacted>")
            .field(
                "manager_client_spki_sha256",
                &self.manager_client_spki_sha256,
            )
            .field("issued_at", &self.issued_at)
            .field("not_after", &self.not_after)
            .finish()
    }
}

fn hex_lower(bytes: &[u8]) -> String {
    use std::fmt::Write;
    let mut s = String::with_capacity(bytes.len() * 2);
    for b in bytes {
        let _ = write!(s, "{b:02x}");
    }
    s
}

#[cfg(test)]
mod tests {
    use super::*;

    fn sample() -> EnrollmentPackage {
        EnrollmentPackage {
            version: ENROLLMENT_VERSION,
            host_id: "host-1".into(),
            mgmt_bind: "127.0.0.1:39736".into(),
            agent_server_cert_pem: "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"
                .into(),
            agent_server_key_pem:
                "-----BEGIN PRIVATE KEY-----\nSECRET\n-----END PRIVATE KEY-----\n".into(),
            client_ca_cert_pem: "-----BEGIN CERTIFICATE-----\nBBBB\n-----END CERTIFICATE-----\n"
                .into(),
            manager_client_spki_sha256: "ab".repeat(32),
            issued_at: 1000,
            not_after: 2000,
        }
    }

    #[test]
    fn roundtrip_and_stable_fingerprint() {
        let p = sample();
        let json = p.to_json().unwrap();
        let back = EnrollmentPackage::parse(&json).unwrap();
        assert_eq!(back.host_id, "host-1");
        // 指纹在 parse 往返后保持一致。
        assert_eq!(p.fingerprint().unwrap(), back.fingerprint().unwrap());
    }

    #[test]
    fn debug_redacts_key_material() {
        let dbg = format!("{:?}", sample());
        assert!(!dbg.contains("SECRET"), "Debug 不得泄露私钥: {dbg}");
        assert!(!dbg.contains("PRIVATE KEY"));
        assert!(dbg.contains("<redacted>"));
    }

    #[test]
    fn rejects_unknown_version() {
        let mut p = sample();
        p.version = 99;
        let json = serde_json::to_string(&p).unwrap();
        assert!(EnrollmentPackage::parse(&json).is_err());
    }
}
