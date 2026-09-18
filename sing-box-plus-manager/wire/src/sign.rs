//! 应用层 HMAC（D12、`docs/threat-model.md` §3）。
//!
//! **传输层 mTLS 只提供通道，不能替代每节点独立的应用层签名。** 收益具体在两处：
//!
//! 1. 若将来在节点侧插入 C18 允许的那种反代，mTLS 会在反代处终结，
//!    那时只有应用层签名还能证明「这份快照来自这个节点」。
//! 2. 凭据映射出的节点是**授权真相**，payload 里的 `node_id` 只用于交叉校验——
//!    不能让请求自己选择归属。
//!
//! 编码纪律：**长度明确，不用字符串拼接**。拼接会产生分隔符歧义——
//! 两组不同的字段值可以拼出同一个串，于是同一个签名对两份不同的请求都成立。

use std::collections::HashMap;
use std::time::{SystemTime, UNIX_EPOCH};

use hmac::{Hmac, Mac};
use sha2::{Digest, Sha256};

type HmacSha256 = Hmac<Sha256>;

/// 时间戳偏差窗口（秒）。超出即拒绝。
pub const MAX_SKEW_SECS: u64 = 300;
/// nonce 重放缓存的保留时长（秒）。必须 ≥ 2 × [`MAX_SKEW_SECS`]，
/// 否则一个仍在偏差窗口内的请求可能因为 nonce 已被清掉而被重放成功。
pub const NONCE_TTL_SECS: u64 = 600;

const DOMAIN: &[u8] = b"proxy-manager/v1/hmac";

pub const HEADER_TIMESTAMP: &str = "x-pm-timestamp";
pub const HEADER_NONCE: &str = "x-pm-nonce";
pub const HEADER_SIGNATURE: &str = "x-pm-signature";

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Direction {
    Request,
    Response,
}

impl Direction {
    fn tag(self) -> &'static [u8] {
        match self {
            Direction::Request => b"request",
            Direction::Response => b"response",
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
pub enum SigningError {
    #[error("签名 header 缺失：{0}")]
    MissingHeader(&'static str),
    #[error("签名 header 重复：{0}（重复或畸形的 header 直接拒绝）")]
    DuplicateHeader(&'static str),
    #[error("签名 header 格式非法：{0}")]
    MalformedHeader(&'static str),
    #[error("时间戳偏差 {skew} 秒，超出 ±{MAX_SKEW_SECS} 秒窗口")]
    TimestampSkew { skew: u64 },
    #[error("nonce 已被使用过（重放）")]
    ReplayedNonce,
    #[error("签名不匹配")]
    BadSignature,
    #[error("HMAC key 必须是 32 字节，实际 {0}")]
    BadKeyLength(usize),
}

/// 每节点一把独立的 32 字节 key。
///
/// **不得复用**任何 CA 材料、发布签名密钥或数据面 PSK
/// （`docs/threat-model.md` §3）。复用的那一刻，「泄露一把 key 的影响范围」
/// 就从一个节点变成了整条信任链。
#[derive(Clone)]
pub struct HmacKey([u8; 32]);

impl std::fmt::Debug for HmacKey {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        // 绝不 Debug 出密钥本身：日志里不记密钥（threat-model §5）。
        f.write_str("HmacKey(<redacted>)")
    }
}

impl HmacKey {
    pub fn from_bytes(bytes: &[u8]) -> Result<Self, SigningError> {
        let array: [u8; 32] =
            bytes.try_into().map_err(|_| SigningError::BadKeyLength(bytes.len()))?;
        Ok(HmacKey(array))
    }

    /// 从 64 位十六进制文本解析。部署材料里以文本形式存放，便于核对与轮换。
    pub fn from_hex(text: &str) -> Result<Self, SigningError> {
        let text = text.trim();
        let bytes = hex::decode(text).map_err(|_| SigningError::MalformedHeader("hmac key"))?;
        Self::from_bytes(&bytes)
    }

    pub fn generate() -> Self {
        use rand::RngCore;
        let mut bytes = [0u8; 32];
        rand::thread_rng().fill_bytes(&mut bytes);
        HmacKey(bytes)
    }

    pub fn to_hex(&self) -> String {
        hex::encode(self.0)
    }

    /// 对任意消息算一个 HMAC-SHA256 标签（十六进制）。
    ///
    /// 给会话 ID 的 HMAC 证明用（README §4.9 加固 4）。
    /// **不要用 `SHA256(key ‖ message)` 代替**——那不是 HMAC，
    /// 在可变长消息上有长度扩展问题，而且它看起来和 HMAC 一样"像个哈希"，
    /// review 时最容易放过去。
    pub fn tag(&self, message: &[u8]) -> String {
        hex::encode(self.mac(message))
    }

    fn mac(&self, message: &[u8]) -> Vec<u8> {
        let mut mac = HmacSha256::new_from_slice(&self.0).expect("HMAC 接受任意长度 key");
        mac.update(message);
        mac.finalize().into_bytes().to_vec()
    }
}

/// 一次签名所覆盖的全部字段。
#[derive(Debug, Clone)]
pub struct SignedMessage<'a> {
    pub direction: Direction,
    pub method: &'a str,
    pub path: &'a str,
    /// 节点标识。它进签名是为了让一份请求不能被搬到另一个节点上重放。
    pub node_id: &'a str,
    pub timestamp: u64,
    pub nonce: &'a str,
    pub body: &'a [u8],
    /// 方向相关的附加字段：响应放上游状态码，请求留空。
    pub extra: &'a str,
}

/// canonical 编码：域分隔 + 方向 + 逐字段「8 字节大端长度 + 内容」。
///
/// 方向进编码，是为了让一份请求签名不能被当作响应签名使用；
/// 域分隔串进编码，是为了让这把 key 万一被用在别处也不会互相冒充。
pub fn canonical(message: &SignedMessage<'_>) -> Vec<u8> {
    let body_digest = Sha256::digest(message.body);
    let timestamp = message.timestamp.to_string();

    let mut out = Vec::with_capacity(256 + body_digest.len());
    push_field(&mut out, DOMAIN);
    push_field(&mut out, message.direction.tag());
    push_field(&mut out, message.method.as_bytes());
    push_field(&mut out, message.path.as_bytes());
    push_field(&mut out, message.node_id.as_bytes());
    push_field(&mut out, timestamp.as_bytes());
    push_field(&mut out, message.nonce.as_bytes());
    push_field(&mut out, &body_digest);
    push_field(&mut out, message.extra.as_bytes());
    out
}

fn push_field(out: &mut Vec<u8>, field: &[u8]) {
    out.extend_from_slice(&(field.len() as u64).to_be_bytes());
    out.extend_from_slice(field);
}

/// 随机 nonce。128 位足够让偶然碰撞在重放缓存的窗口内可以忽略。
pub fn new_nonce() -> String {
    use rand::RngCore;
    let mut bytes = [0u8; 16];
    rand::thread_rng().fill_bytes(&mut bytes);
    hex::encode(bytes)
}

pub fn now_secs() -> u64 {
    SystemTime::now().duration_since(UNIX_EPOCH).map(|d| d.as_secs()).unwrap_or(0)
}

/// 签出三个 header 的值。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SignatureHeaders {
    pub timestamp: u64,
    pub nonce: String,
    pub signature: String,
}

impl SignatureHeaders {
    pub fn to_pairs(&self) -> [(&'static str, String); 3] {
        [
            (HEADER_TIMESTAMP, self.timestamp.to_string()),
            (HEADER_NONCE, self.nonce.clone()),
            (HEADER_SIGNATURE, self.signature.clone()),
        ]
    }

    /// 从 header 列表里取出三个值。**重复即拒绝**——
    /// 两个同名 header 时「取第一个」还是「取最后一个」是实现细节，
    /// 而攻击者可以同时提供一个合法的和一个伪造的，赌两端取的不是同一个。
    pub fn from_headers(headers: &[(String, String)]) -> Result<Self, SigningError> {
        let pick = |name: &'static str| -> Result<&str, SigningError> {
            let mut found = headers.iter().filter(|(key, _)| key == name);
            let first = found.next().ok_or(SigningError::MissingHeader(name))?;
            if found.next().is_some() {
                return Err(SigningError::DuplicateHeader(name));
            }
            Ok(first.1.as_str())
        };
        let timestamp = pick(HEADER_TIMESTAMP)?
            .parse::<u64>()
            .map_err(|_| SigningError::MalformedHeader(HEADER_TIMESTAMP))?;
        let nonce = pick(HEADER_NONCE)?.to_string();
        if nonce.is_empty() || nonce.len() > 64 || !nonce.bytes().all(|b| b.is_ascii_hexdigit()) {
            return Err(SigningError::MalformedHeader(HEADER_NONCE));
        }
        let signature = pick(HEADER_SIGNATURE)?.to_string();
        if signature.len() != 64 || !signature.bytes().all(|b| b.is_ascii_hexdigit()) {
            return Err(SigningError::MalformedHeader(HEADER_SIGNATURE));
        }
        Ok(SignatureHeaders { timestamp, nonce, signature })
    }
}

/// 签一条消息，返回要附在 header 上的三个值。
pub fn sign(
    key: &HmacKey,
    direction: Direction,
    method: &str,
    path: &str,
    node_id: &str,
    body: &[u8],
    extra: &str,
) -> SignatureHeaders {
    let timestamp = now_secs();
    let nonce = new_nonce();
    let message =
        SignedMessage { direction, method, path, node_id, timestamp, nonce: &nonce, body, extra };
    let signature = hex::encode(key.mac(&canonical(&message)));
    SignatureHeaders { timestamp, nonce, signature }
}

/// 校验一条消息。
///
/// 顺序刻意是：**header 形状 → 时间戳窗口 → 签名 → nonce**。
///
/// 签名在 nonce 之前，是为了让伪造者**烧不掉别人的 nonce**：
/// 若先记 nonce 再验签名，任何人都能用一个随便编的签名 + 猜到的 nonce
/// 把合法请求挤成「重放」。签名先过，nonce 缓存里就只会存进真实对端发过的值。
#[allow(clippy::too_many_arguments)]
pub fn verify(
    key: &HmacKey,
    nonces: &mut NonceCache,
    headers: &SignatureHeaders,
    direction: Direction,
    method: &str,
    path: &str,
    node_id: &str,
    body: &[u8],
    extra: &str,
    now: u64,
) -> Result<(), SigningError> {
    let skew = now.abs_diff(headers.timestamp);
    if skew > MAX_SKEW_SECS {
        return Err(SigningError::TimestampSkew { skew });
    }

    let message = SignedMessage {
        direction,
        method,
        path,
        node_id,
        timestamp: headers.timestamp,
        nonce: &headers.nonce,
        body,
        extra,
    };
    let expected = key.mac(&canonical(&message));
    let provided = hex::decode(&headers.signature)
        .map_err(|_| SigningError::MalformedHeader(HEADER_SIGNATURE))?;
    // 常量时间比较：hmac 的 verify_slice 内部用的是 subtle 的等值比较。
    let mut mac = HmacSha256::new_from_slice(&key.0).expect("HMAC 接受任意长度 key");
    mac.update(&canonical(&message));
    mac.verify_slice(&provided).map_err(|_| SigningError::BadSignature)?;
    debug_assert_eq!(expected.len(), provided.len());

    if !nonces.remember(&headers.nonce, now) {
        return Err(SigningError::ReplayedNonce);
    }
    Ok(())
}

/// nonce 重放缓存。
///
/// 只在验签通过之后写入，所以它记的都是真实对端用过的值。
pub struct NonceCache {
    seen: HashMap<String, u64>,
    ttl: u64,
}

impl Default for NonceCache {
    fn default() -> Self {
        NonceCache::new(NONCE_TTL_SECS)
    }
}

impl NonceCache {
    pub fn new(ttl: u64) -> Self {
        assert!(
            ttl >= 2 * MAX_SKEW_SECS,
            "nonce 保留时长必须 ≥ 2 × 偏差窗口，否则仍在窗口内的请求可以被重放"
        );
        NonceCache { seen: HashMap::new(), ttl }
    }

    /// 记下一个 nonce。此前没见过返回 `true`；见过返回 `false`。
    pub fn remember(&mut self, nonce: &str, now: u64) -> bool {
        self.prune(now);
        if self.seen.contains_key(nonce) {
            return false;
        }
        self.seen.insert(nonce.to_string(), now + self.ttl);
        true
    }

    fn prune(&mut self, now: u64) {
        self.seen.retain(|_, expiry| *expiry > now);
    }

    pub fn len(&self) -> usize {
        self.seen.len()
    }

    pub fn is_empty(&self) -> bool {
        self.seen.is_empty()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn key() -> HmacKey {
        HmacKey::from_bytes(&[7u8; 32]).unwrap()
    }

    fn check(
        headers: &SignatureHeaders,
        body: &[u8],
        node: &str,
        now: u64,
    ) -> Result<(), SigningError> {
        let mut nonces = NonceCache::default();
        verify(
            &key(),
            &mut nonces,
            headers,
            Direction::Request,
            "POST",
            "/v1/command",
            node,
            body,
            "",
            now,
        )
    }

    #[test]
    fn 正常签名可以通过() {
        let headers =
            sign(&key(), Direction::Request, "POST", "/v1/command", "node-1", b"body", "");
        assert!(check(&headers, b"body", "node-1", now_secs()).is_ok());
    }

    #[test]
    fn 篡改任一被覆盖字段都会失败() {
        let now = now_secs();
        let headers =
            sign(&key(), Direction::Request, "POST", "/v1/command", "node-1", b"body", "");
        // body 变了
        assert_eq!(check(&headers, b"body!", "node-1", now), Err(SigningError::BadSignature));
        // node_id 变了：一份请求不能被搬到另一个节点上重放
        assert_eq!(check(&headers, b"body", "node-2", now), Err(SigningError::BadSignature));

        // 方向变了：请求签名不能当响应签名用
        let mut nonces = NonceCache::default();
        let result = verify(
            &key(),
            &mut nonces,
            &headers,
            Direction::Response,
            "POST",
            "/v1/command",
            "node-1",
            b"body",
            "",
            now,
        );
        assert_eq!(result, Err(SigningError::BadSignature));

        // 路径变了
        let mut nonces = NonceCache::default();
        let result = verify(
            &key(),
            &mut nonces,
            &headers,
            Direction::Request,
            "POST",
            "/v1/other",
            "node-1",
            b"body",
            "",
            now,
        );
        assert_eq!(result, Err(SigningError::BadSignature));
    }

    #[test]
    fn 换一把key就不通过() {
        let headers =
            sign(&key(), Direction::Request, "POST", "/v1/command", "node-1", b"body", "");
        let other = HmacKey::from_bytes(&[9u8; 32]).unwrap();
        let mut nonces = NonceCache::default();
        let result = verify(
            &other,
            &mut nonces,
            &headers,
            Direction::Request,
            "POST",
            "/v1/command",
            "node-1",
            b"body",
            "",
            now_secs(),
        );
        assert_eq!(result, Err(SigningError::BadSignature));
    }

    #[test]
    fn 超出偏差窗口被拒() {
        let now = now_secs();
        let headers =
            sign(&key(), Direction::Request, "POST", "/v1/command", "node-1", b"body", "");
        // 太旧
        assert!(matches!(
            check(&headers, b"body", "node-1", now + MAX_SKEW_SECS + 1),
            Err(SigningError::TimestampSkew { .. })
        ));
        // 太新（时钟往前跳同样要挡）
        assert!(matches!(
            check(&headers, b"body", "node-1", now.saturating_sub(MAX_SKEW_SECS + 1)),
            Err(SigningError::TimestampSkew { .. })
        ));
        // 边界上仍然通过
        assert!(check(&headers, b"body", "node-1", now + MAX_SKEW_SECS).is_ok());
    }

    #[test]
    fn 重放同一个nonce被拒() {
        let now = now_secs();
        let headers =
            sign(&key(), Direction::Request, "POST", "/v1/command", "node-1", b"body", "");
        let mut nonces = NonceCache::default();
        let args = |n: &mut NonceCache| {
            verify(
                &key(),
                n,
                &headers,
                Direction::Request,
                "POST",
                "/v1/command",
                "node-1",
                b"body",
                "",
                now,
            )
        };
        assert!(args(&mut nonces).is_ok());
        assert_eq!(args(&mut nonces), Err(SigningError::ReplayedNonce));
    }

    /// 伪造者烧不掉别人的 nonce：签名先验，nonce 后记。
    #[test]
    fn 签名失败不会消耗nonce() {
        let now = now_secs();
        let headers =
            sign(&key(), Direction::Request, "POST", "/v1/command", "node-1", b"body", "");
        let mut nonces = NonceCache::default();

        // 攻击者拿同一个 nonce 配一份错的 body 先打进来。
        let forged = verify(
            &key(),
            &mut nonces,
            &headers,
            Direction::Request,
            "POST",
            "/v1/command",
            "node-1",
            b"forged",
            "",
            now,
        );
        assert_eq!(forged, Err(SigningError::BadSignature));
        assert!(nonces.is_empty(), "验签失败不得写入 nonce 缓存");

        // 真实请求随后仍然能通过。
        let genuine = verify(
            &key(),
            &mut nonces,
            &headers,
            Direction::Request,
            "POST",
            "/v1/command",
            "node-1",
            b"body",
            "",
            now,
        );
        assert!(genuine.is_ok(), "合法请求不该被伪造者挤掉");
    }

    #[test]
    fn 重复或畸形的header直接拒绝() {
        let base = vec![
            (HEADER_TIMESTAMP.to_string(), "100".to_string()),
            (HEADER_NONCE.to_string(), "ab".repeat(8)),
            (HEADER_SIGNATURE.to_string(), "cd".repeat(32)),
        ];
        assert!(SignatureHeaders::from_headers(&base).is_ok());

        let mut duplicated = base.clone();
        duplicated.push((HEADER_SIGNATURE.to_string(), "ef".repeat(32)));
        assert_eq!(
            SignatureHeaders::from_headers(&duplicated),
            Err(SigningError::DuplicateHeader(HEADER_SIGNATURE))
        );

        for (index, broken) in [(0, "not-a-number"), (1, "zz"), (2, "tooshort")] {
            let mut headers = base.clone();
            headers[index].1 = broken.to_string();
            assert!(SignatureHeaders::from_headers(&headers).is_err(), "{broken:?} 必须被拒");
        }

        for missing in [HEADER_TIMESTAMP, HEADER_NONCE, HEADER_SIGNATURE] {
            let headers: Vec<_> = base.iter().filter(|(k, _)| k != missing).cloned().collect();
            assert_eq!(
                SignatureHeaders::from_headers(&headers),
                Err(SigningError::MissingHeader(missing))
            );
        }
    }

    /// 长度明确的编码：拼接会让两组不同的字段拼出同一个串。
    #[test]
    fn canonical编码没有分隔符歧义() {
        let make = |node: &str, nonce: &str| {
            canonical(&SignedMessage {
                direction: Direction::Request,
                method: "POST",
                path: "/v1/command",
                node_id: node,
                timestamp: 1,
                nonce,
                body: b"",
                extra: "",
            })
        };
        // 字符串拼接下 "ab"+"c" 与 "a"+"bc" 会撞；长度前缀下不会。
        assert_ne!(make("ab", "c"), make("a", "bc"));
    }

    #[test]
    fn key不会被debug打印出来() {
        assert_eq!(format!("{:?}", key()), "HmacKey(<redacted>)");
    }

    #[test]
    fn nonce缓存的保留时长必须覆盖两倍偏差窗口() {
        let result = std::panic::catch_unwind(|| NonceCache::new(MAX_SKEW_SECS));
        assert!(result.is_err(), "过短的 TTL 必须在构造时就失败");
        assert!(NonceCache::new(2 * MAX_SKEW_SECS).is_empty());
    }

    #[test]
    fn 过期的nonce会被清掉() {
        let mut nonces = NonceCache::default();
        assert!(nonces.remember("aa", 1_000));
        assert!(!nonces.remember("aa", 1_000));
        assert!(nonces.remember("aa", 1_000 + NONCE_TTL_SECS + 1), "过期后可以再次使用");
        assert_eq!(nonces.len(), 1);
    }
}
