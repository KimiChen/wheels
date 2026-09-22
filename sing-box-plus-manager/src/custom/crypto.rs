//! 个人自定义节点配置的**静态加密**。
//!
//! # 威胁模型：一份被复制走的账本备份
//!
//! 账本要按 R3 做一致性备份并演练恢复，而备份会被复制到别处。自定义节点里
//! 装的是**别人家的凭据**（SS 密码、VMess/VLESS 的 UUID、SOCKS5 的用户名口令），
//! 明文存进去就等于每一份账本副本都是一份用户第三方凭据的副本。
//!
//! 所以密钥**不在库里**，在配置目录下单独一个 0600 文件。拿到 db 拿不到密钥，
//! 拿到密钥拿不到 db——这是 uPSK 不进库那条纪律的同一个形状。
//!
//! # AAD 绑定的是「这条密文属于谁、属于哪张表」
//!
//! `custom-proxy:7` 与 `custom-socks5:7` 是两个不同的 AAD，`custom-proxy:8`
//! 又是第三个。于是把 A 的一行密文原样 UPDATE 到 B 名下**解不开**，
//! 把一条上游的密文塞进 socks5 表也解不开。
//!
//! 这不是锦上添花：光有写库权限（备份恢复、误操作、SQL 注入）本来足以
//! 把一条配置挪给另一个人，而挪过去之后**它会照常渲染进那个人的订阅**。
//! AAD 把「能写库」和「能搬运凭据」分开了。

use crate::error::{Error, Result};
use base64::Engine as _;
use ring::aead;

/// 密文的格式版本。换 AEAD 或换编码时加一，解密侧按它分支。
pub const CURRENT_VERSION: i64 = 1;

const PREFIX: &str = "v1:";
const NONCE_LEN: usize = 12;

/// 用途串。**同时进 AAD 与密钥派生之外的任何地方都不需要它**，
/// 所以它只在这里出现一次。
pub fn purpose(kind: Kind, user_id: i64) -> String {
    format!("custom-{}:{user_id}", kind.as_str())
}

/// 两类自定义配置。分开是为了让 AAD 不同。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Kind {
    Proxy,
    Socks5,
}

impl Kind {
    pub fn as_str(self) -> &'static str {
        match self {
            Kind::Proxy => "proxy",
            Kind::Socks5 => "socks5",
        }
    }
}

/// 一把用于自定义节点配置的密钥。
pub struct CryptoBox {
    key: aead::LessSafeKey,
}

impl std::fmt::Debug for CryptoBox {
    /// **不打印密钥。** 这个结构会进 `AppState`，而 `AppState` 在好几处
    /// 被 `{:?}` 过——派生 Debug 等于把密钥写进日志。
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("CryptoBox(<redacted>)")
    }
}

impl CryptoBox {
    /// 从 32 字节密钥建一把。入参是标准 Base64。
    pub fn from_base64(encoded: &str) -> Result<Self> {
        let raw = base64::engine::general_purpose::STANDARD
            .decode(encoded.trim())
            .map_err(|_| Error::Codec("自定义节点密钥不是合法的 Base64".into()))?;
        Self::from_bytes(&raw)
    }

    pub fn from_bytes(raw: &[u8]) -> Result<Self> {
        let unbound = aead::UnboundKey::new(&aead::CHACHA20_POLY1305, raw)
            .map_err(|_| Error::Codec("自定义节点密钥必须是 32 字节".into()))?;
        Ok(CryptoBox { key: aead::LessSafeKey::new(unbound) })
    }

    /// 生成一把新密钥的 Base64 文本。只在 `custom-key init` 里用。
    pub fn generate_base64() -> Result<String> {
        use ring::rand::SecureRandom as _;
        let mut raw = [0u8; 32];
        ring::rand::SystemRandom::new()
            .fill(&mut raw)
            .map_err(|_| Error::Codec("生成自定义节点密钥失败".into()))?;
        Ok(base64::engine::general_purpose::STANDARD.encode(raw))
    }

    pub fn encrypt(&self, plaintext: &str, purpose: &str) -> Result<String> {
        use ring::rand::SecureRandom as _;
        let mut nonce_bytes = [0u8; NONCE_LEN];
        ring::rand::SystemRandom::new()
            .fill(&mut nonce_bytes)
            .map_err(|_| Error::Codec("生成 nonce 失败".into()))?;
        let mut buffer = plaintext.as_bytes().to_vec();
        self.key
            .seal_in_place_append_tag(
                aead::Nonce::assume_unique_for_key(nonce_bytes),
                aead::Aad::from(purpose.as_bytes()),
                &mut buffer,
            )
            .map_err(|_| Error::Codec("加密自定义节点配置失败".into()))?;
        let mut payload = nonce_bytes.to_vec();
        payload.extend_from_slice(&buffer);
        Ok(format!("{PREFIX}{}", base64::engine::general_purpose::URL_SAFE_NO_PAD.encode(payload)))
    }

    pub fn decrypt(&self, encoded: &str, purpose: &str) -> Result<String> {
        let body =
            encoded.strip_prefix(PREFIX).ok_or_else(|| Error::Codec("不支持的密文版本".into()))?;
        let mut payload = base64::engine::general_purpose::URL_SAFE_NO_PAD
            .decode(body)
            .map_err(|_| Error::Codec("密文编码已损坏".into()))?;
        if payload.len() <= NONCE_LEN {
            return Err(Error::Codec("密文已损坏".into()));
        }
        let (nonce_bytes, cipher) = payload.split_at_mut(NONCE_LEN);
        let mut nonce = [0u8; NONCE_LEN];
        nonce.copy_from_slice(nonce_bytes);
        let plain = self
            .key
            .open_in_place(
                aead::Nonce::assume_unique_for_key(nonce),
                aead::Aad::from(purpose.as_bytes()),
                cipher,
            )
            // **不区分「密钥不对」与「AAD 不对」。** 区分开等于给一个能写库的人
            // 一个探测接口：他可以逐个 user_id 试，直到找到密文原本属于谁。
            .map_err(|_| Error::Codec("密文认证失败".into()))?;
        String::from_utf8(plain.to_vec())
            .map_err(|_| Error::Codec("密文解出来不是合法 UTF-8".into()))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn boxed() -> CryptoBox {
        CryptoBox::from_bytes(&[7u8; 32]).unwrap()
    }

    #[test]
    fn 往返() {
        let b = boxed();
        let sealed = b.encrypt("{\"name\":\"x\"}", &purpose(Kind::Proxy, 1)).unwrap();
        assert_eq!(b.decrypt(&sealed, &purpose(Kind::Proxy, 1)).unwrap(), "{\"name\":\"x\"}");
    }

    /// **换个用户解不开。** 这条钉住的是「能写库 ≠ 能把 A 的配置搬给 B」。
    #[test]
    fn 换用户解不开() {
        let b = boxed();
        let sealed = b.encrypt("secret", &purpose(Kind::Proxy, 1)).unwrap();
        assert!(b.decrypt(&sealed, &purpose(Kind::Proxy, 2)).is_err());
    }

    /// 换张表也解不开——两张表的 AAD 不同。
    #[test]
    fn 换类别解不开() {
        let b = boxed();
        let sealed = b.encrypt("secret", &purpose(Kind::Proxy, 1)).unwrap();
        assert!(b.decrypt(&sealed, &purpose(Kind::Socks5, 1)).is_err());
    }

    #[test]
    fn 换密钥解不开() {
        let sealed = boxed().encrypt("secret", &purpose(Kind::Proxy, 1)).unwrap();
        let other = CryptoBox::from_bytes(&[9u8; 32]).unwrap();
        assert!(other.decrypt(&sealed, &purpose(Kind::Proxy, 1)).is_err());
    }

    /// 改一个字节就认证失败——密文不是可塑的。
    #[test]
    fn 篡改被发现() {
        let b = boxed();
        let sealed = b.encrypt("secret", &purpose(Kind::Proxy, 1)).unwrap();
        let mut bytes: Vec<u8> = sealed.into_bytes();
        let last = bytes.len() - 1;
        bytes[last] = if bytes[last] == b'A' { b'B' } else { b'A' };
        let tampered = String::from_utf8(bytes).unwrap();
        assert!(b.decrypt(&tampered, &purpose(Kind::Proxy, 1)).is_err());
    }

    #[test]
    fn 密钥长度不对要报错() {
        assert!(CryptoBox::from_bytes(&[0u8; 16]).is_err());
        assert!(CryptoBox::from_base64("bm90LWJhc2U2NA").is_err());
    }

    /// Debug **不能**把密钥打出来——这个结构进 AppState，而它被 `{:?}` 过。
    #[test]
    fn debug_不泄露密钥() {
        let text = format!("{:?}", boxed());
        assert!(!text.contains('7'), "Debug 里出现了密钥字节：{text}");
        assert!(text.contains("redacted"), "{text}");
    }
}
