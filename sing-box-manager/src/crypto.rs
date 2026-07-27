//! 密钥信封加密。主密钥来自环境 `ENCRYPTION_MASTER_KEY`（base64 编码的 32 字节），
//! 不落入 SQLite；业务密钥以 XChaCha20-Poly1305 AEAD 加密后按 (alg, key_version, nonce, ciphertext) 存库。
//!
//! Phase 6：**多版本 ring**。轮换时 ring 同时装历史与当前主密钥——`seal` 恒用当前版本，`open` 按密文
//! 的 `key_version` 选键（缺该版本即明确报错，绝不静默）；[`Cipher::reseal`] 把旧密文重封到当前版本。
//! ENV 约定：`ENCRYPTION_MASTER_KEY`(当前) + `ENCRYPTION_MASTER_KEY_VERSION`(当前版本号，默认 1) +
//! `ENCRYPTION_MASTER_KEY_V{n}`(历史版本，仅供解密)。备份领域另用 [`Cipher::from_raw`] 以独立 BK 实例化。
#![allow(dead_code)]

use std::collections::HashMap;

use crate::error::{AppError, ErrorCode, Result};
use base64::{engine::general_purpose::STANDARD, Engine};
use chacha20poly1305::{
    aead::{Aead, KeyInit},
    Key, XChaCha20Poly1305, XNonce,
};
use zeroize::Zeroizing;

/// AEAD 算法版本：XChaCha20-Poly1305。
pub const ALG_XCHACHA20POLY1305: i64 = 1;

/// 一段密文及其解密所需元数据（对应 credential_versions 表的列）。
#[derive(Debug, Clone)]
pub struct Sealed {
    pub alg: i64,
    pub key_version: i64,
    pub nonce: Vec<u8>,
    pub ciphertext: Vec<u8>,
}

/// 多版本主密钥 ring。`seal` 用 `current`；`open` 按密文版本选键。
#[derive(Clone)]
pub struct Cipher {
    current: i64,
    ciphers: HashMap<i64, XChaCha20Poly1305>,
    /// 原始密钥字节（备份领域需把主密钥用独立 BK 包裹进 manifest）。Zeroizing 尽早清零。
    raw: HashMap<i64, Zeroizing<Vec<u8>>>,
}

fn decode_key(raw: &str, what: &str) -> Result<Vec<u8>> {
    let bytes = STANDARD
        .decode(raw.trim())
        .map_err(|e| AppError::with(ErrorCode::Config, format!("{what} 非 base64"), e.into()))?;
    if bytes.len() != 32 {
        return Err(AppError::new(
            ErrorCode::Config,
            format!("{what} 必须为 32 字节（base64 编码）"),
        ));
    }
    Ok(bytes)
}

impl Cipher {
    /// 从原始 32 字节构建单版本 ring（备份 BK 用；不读 env）。
    pub fn from_raw(key_version: i64, key: &[u8]) -> Result<Self> {
        if key.len() != 32 {
            return Err(AppError::new(ErrorCode::Crypto, "密钥必须为 32 字节"));
        }
        let mut ciphers = HashMap::new();
        let mut raw = HashMap::new();
        ciphers.insert(key_version, XChaCha20Poly1305::new(Key::from_slice(key)));
        raw.insert(key_version, Zeroizing::new(key.to_vec()));
        Ok(Self {
            current: key_version,
            ciphers,
            raw,
        })
    }

    /// 遗留单版本构建：`ENCRYPTION_MASTER_KEY` → ring{key_version}, current=key_version。现有调用/测试不变。
    pub fn from_env(key_version: i64) -> Result<Self> {
        let raw = std::env::var("ENCRYPTION_MASTER_KEY")
            .map_err(|_| AppError::new(ErrorCode::Config, "缺少 ENCRYPTION_MASTER_KEY"))?;
        let bytes = decode_key(&raw, "ENCRYPTION_MASTER_KEY")?;
        Self::from_raw(key_version, &bytes)
    }

    /// Phase 6 多版本构建：装入 `ENCRYPTION_MASTER_KEY_V{n}` 历史版本 + `ENCRYPTION_MASTER_KEY`(当前，
    /// 版本号取 `ENCRYPTION_MASTER_KEY_VERSION`，默认 1)。current 对应密钥必须存在，否则启动失败。
    pub fn from_env_ring() -> Result<Self> {
        let current: i64 = std::env::var("ENCRYPTION_MASTER_KEY_VERSION")
            .ok()
            .and_then(|v| v.trim().parse().ok())
            .unwrap_or(1);
        let mut ciphers = HashMap::new();
        let mut raw = HashMap::new();
        // 历史版本（仅解密）。扫描 ENCRYPTION_MASTER_KEY_V{n}。
        for (k, v) in std::env::vars() {
            if let Some(nstr) = k.strip_prefix("ENCRYPTION_MASTER_KEY_V") {
                if let Ok(ver) = nstr.trim().parse::<i64>() {
                    let bytes = decode_key(&v, &k)?;
                    ciphers.insert(ver, XChaCha20Poly1305::new(Key::from_slice(&bytes)));
                    raw.insert(ver, Zeroizing::new(bytes));
                }
            }
        }
        // 当前主密钥（覆盖同号历史，以 ENCRYPTION_MASTER_KEY 为准）。
        let cur_raw = std::env::var("ENCRYPTION_MASTER_KEY")
            .map_err(|_| AppError::new(ErrorCode::Config, "缺少 ENCRYPTION_MASTER_KEY"))?;
        let cur_bytes = decode_key(&cur_raw, "ENCRYPTION_MASTER_KEY")?;
        ciphers.insert(current, XChaCha20Poly1305::new(Key::from_slice(&cur_bytes)));
        raw.insert(current, Zeroizing::new(cur_bytes));
        Ok(Self {
            current,
            ciphers,
            raw,
        })
    }

    pub fn current_version(&self) -> i64 {
        self.current
    }
    pub fn has_version(&self, v: i64) -> bool {
        self.ciphers.contains_key(&v)
    }
    /// 某版本主密钥原始字节（备份包裹用）。
    pub fn master_key_bytes(&self, version: i64) -> Option<&[u8]> {
        self.raw.get(&version).map(|z| z.as_slice())
    }
    /// ring 内全部版本号（升序）。
    pub fn versions(&self) -> Vec<i64> {
        let mut v: Vec<i64> = self.ciphers.keys().copied().collect();
        v.sort_unstable();
        v
    }

    pub fn seal(&self, plaintext: &[u8]) -> Result<Sealed> {
        use rand::RngCore;
        let cipher = self
            .ciphers
            .get(&self.current)
            .ok_or_else(|| AppError::new(ErrorCode::Crypto, "缺少当前主密钥版本"))?;
        let mut nonce = [0u8; 24];
        rand::thread_rng().fill_bytes(&mut nonce);
        let nonce = XNonce::from_slice(&nonce);
        let ciphertext = cipher
            .encrypt(nonce, plaintext)
            .map_err(|_| AppError::new(ErrorCode::Crypto, "加密失败"))?;
        Ok(Sealed {
            alg: ALG_XCHACHA20POLY1305,
            key_version: self.current,
            nonce: nonce.to_vec(),
            ciphertext,
        })
    }

    pub fn open(&self, sealed: &Sealed) -> Result<Vec<u8>> {
        if sealed.alg != ALG_XCHACHA20POLY1305 {
            return Err(AppError::new(
                ErrorCode::Crypto,
                format!("未知加密算法 {}", sealed.alg),
            ));
        }
        let cipher = self.ciphers.get(&sealed.key_version).ok_or_else(|| {
            AppError::new(
                ErrorCode::Crypto,
                format!("缺少解密所需主密钥版本 {}", sealed.key_version),
            )
        })?;
        if sealed.nonce.len() != 24 {
            return Err(AppError::new(ErrorCode::Crypto, "nonce 长度非法"));
        }
        let nonce = XNonce::from_slice(&sealed.nonce);
        cipher
            .decrypt(nonce, sealed.ciphertext.as_ref())
            .map_err(|_| AppError::new(ErrorCode::Crypto, "解密失败（密钥或密文不匹配）"))
    }

    /// 把旧密文重封到当前版本；已是当前版本返回 `None`（幂等，主密钥轮换扫描用）。
    pub fn reseal(&self, sealed: &Sealed) -> Result<Option<Sealed>> {
        if sealed.key_version == self.current {
            return Ok(None);
        }
        let plaintext = Zeroizing::new(self.open(sealed)?);
        Ok(Some(self.seal(&plaintext)?))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn test_cipher() -> Cipher {
        // 固定 32 字节主密钥，避免依赖进程环境。
        let key = STANDARD.encode([9u8; 32]);
        std::env::set_var("ENCRYPTION_MASTER_KEY", key);
        Cipher::from_env(1).unwrap()
    }

    #[test]
    fn seal_open_roundtrip() {
        let c = test_cipher();
        let s = c.seal(b"hello uPSK").unwrap();
        assert_eq!(c.open(&s).unwrap(), b"hello uPSK");
        // 每次 nonce 随机，密文不同。
        let s2 = c.seal(b"hello uPSK").unwrap();
        assert_ne!(s.ciphertext, s2.ciphertext);
    }

    #[test]
    fn tampered_ciphertext_fails() {
        let c = test_cipher();
        let mut s = c.seal(b"secret").unwrap();
        s.ciphertext[0] ^= 0xff;
        assert!(c.open(&s).is_err());
    }

    #[test]
    fn ring_opens_historical_version_and_reseals_to_current() {
        // v1 密文用 v1 key 封；ring{1,2, current=2} 应能 open v1，reseal 到 v2。
        let v1 = Cipher::from_raw(1, &[1u8; 32]).unwrap();
        let s1 = v1.seal(b"legacy secret").unwrap();
        assert_eq!(s1.key_version, 1);

        let mut ciphers = std::collections::HashMap::new();
        let mut raw = std::collections::HashMap::new();
        ciphers.insert(1, XChaCha20Poly1305::new(Key::from_slice(&[1u8; 32])));
        ciphers.insert(2, XChaCha20Poly1305::new(Key::from_slice(&[2u8; 32])));
        raw.insert(1, Zeroizing::new(vec![1u8; 32]));
        raw.insert(2, Zeroizing::new(vec![2u8; 32]));
        let ring = Cipher {
            current: 2,
            ciphers,
            raw,
        };
        // 历史版本可解。
        assert_eq!(ring.open(&s1).unwrap(), b"legacy secret");
        // reseal → 版本变 2、明文不变。
        let s2 = ring.reseal(&s1).unwrap().expect("非 current → Some");
        assert_eq!(s2.key_version, 2);
        assert_eq!(ring.open(&s2).unwrap(), b"legacy secret");
        // 已是 current → None（幂等）。
        assert!(ring.reseal(&s2).unwrap().is_none());
        // 新 seal 用 current=2。
        assert_eq!(ring.seal(b"x").unwrap().key_version, 2);
        // master_key_bytes 暴露原始字节（备份包裹用）。
        assert_eq!(ring.master_key_bytes(1).unwrap(), &[1u8; 32]);
        assert_eq!(ring.versions(), vec![1, 2]);
    }

    #[test]
    fn open_missing_version_errors_not_silent() {
        // ring 只有 v2，遇到 v1 密文 → 明确报错（不静默）。
        let v1 = Cipher::from_raw(1, &[7u8; 32]).unwrap();
        let s1 = v1.seal(b"data").unwrap();
        let v2 = Cipher::from_raw(2, &[8u8; 32]).unwrap();
        let err = v2.open(&s1).unwrap_err();
        assert!(err.message.contains("缺少解密所需主密钥版本 1"));
    }

    #[test]
    fn from_env_ring_assembles_current_and_historical() {
        std::env::set_var("ENCRYPTION_MASTER_KEY_V1", STANDARD.encode([3u8; 32]));
        std::env::set_var("ENCRYPTION_MASTER_KEY", STANDARD.encode([4u8; 32]));
        std::env::set_var("ENCRYPTION_MASTER_KEY_VERSION", "2");
        let ring = Cipher::from_env_ring().unwrap();
        assert_eq!(ring.current_version(), 2);
        assert!(ring.has_version(1) && ring.has_version(2));
        // v1 密文（用 [3;32]）可解。
        let v1 = Cipher::from_raw(1, &[3u8; 32]).unwrap();
        let s = v1.seal(b"hist").unwrap();
        assert_eq!(ring.open(&s).unwrap(), b"hist");
        std::env::remove_var("ENCRYPTION_MASTER_KEY_V1");
        std::env::remove_var("ENCRYPTION_MASTER_KEY_VERSION");
    }
}
