//! SSO `state`：铸造、校验与一次性墓碑（README §4.9 加固 1、2、3）。
//!
//! 形状是 `<签发时间戳>.<随机量>.<HMAC>`，用**严格正则式的逐段解析**锁死，
//! 并允许小幅时钟前偏。同一个串同时出现在回调 query 与一个回调专用 cookie 里，
//! 两者必须逐字节相等——这是经典的双提交，不依赖任何服务端状态就能挡住
//! 「攻击者把自己的回调塞给受害者」。
//!
//! **墓碑只在上游换取身份成功之后才写**（[`consume`]）。写反顺序就等于把登录
//! 入口变成一个免认证的写放大入口：任何人构造一个回调 URL 就能让主控写一行。

use sha2::{Digest, Sha256};
use time::OffsetDateTime;

use crate::error::Result;
use crate::ledger::bucket;
use crate::store::Store;

/// 回调专用 cookie 的名字。
///
/// **必须是 `__Host-`。** 这里曾经写成 `__Secure-`，理由是「加固 2 要求把作用域
/// 收在回调路径上，而 `__Host-` 强制 `Path=/`」。那个取舍换错了东西：
///
/// - `Path` **不是写入隔离边界**。浏览器从不按 path 隔离 cookie 的写入，
///   而这个 cookie 已经是 `HttpOnly`，页面脚本本来就读不到它——
///   把 `Path` 收窄只减少了它被发送的范围，挡不住任何人。
/// - `__Host-` 才是唯一能阻止**兄弟子域写入同名 cookie** 的机制。
///   `__Secure-` 只校验 `Secure` 标志，不约束 `Domain`。
///
/// 而整条防线的理由正是「攻击者能造出 query，但造不出受害者浏览器里的那个
/// cookie」。`__Secure-` 不保证这一点：控制台域名下任意一个攻击者可写的
/// 子域（托管静态站、有 XSS 的旧后台、CNAME 悬挂）都能下发
/// `Domain=example.com` 的同名 cookie，于是双提交被架空，
/// 变成一次登录 CSRF——受害者被固定进攻击者的账号。
///
/// 会话 cookie 那边一直是 `__Host-`（`api::session`），这里是设计里唯一的例外，
/// 现已消除。
pub const STATE_COOKIE: &str = "__Host-pm_sso_state";
/// 回调路径。cookie 的 `Path` 与路由必须是同一个字面量。
pub const CALLBACK_PATH: &str = "/auth/sso/callback";

/// 允许的时钟前偏。签发与校验在同一个进程里，正常不该出现前偏，
/// 但闰秒与 NTP 步进会造成几秒的倒退，为此拒绝一次登录不值当。
const MAX_FUTURE_SKEW_SECS: i64 = 30;

const DOMAIN: &[u8] = b"proxy-manager/v1/sso-state\0";

#[derive(Debug, thiserror::Error)]
pub enum StateError {
    #[error("state 无效或已使用")]
    Invalid,
    #[error("state 已过期")]
    Expired,
}

/// 新铸一个 state 以及它的 `Set-Cookie`。
pub fn issue(
    key: &proxy_manager_wire::sign::HmacKey,
    ttl_secs: u32,
    now: OffsetDateTime,
) -> (String, String) {
    use rand::RngCore;
    let mut random = [0u8; 32];
    rand::thread_rng().fill_bytes(&mut random);
    let payload = format!("{}.{}", now.unix_timestamp(), hex::encode(random));
    let state = format!("{payload}.{}", sign(key, &payload));
    let cookie = set_cookie(&state, ttl_secs as i64);
    (state, cookie)
}

/// 校验双提交绑定、形状、签名与 TTL。**不做任何写入。**
///
/// 实际顺序是：**绑定 → 形状 → 签名 → TTL**。
/// 绑定放最前面是因为它最便宜且最先淘汰无关请求；TTL 放最后是为了让一个
/// 伪造的串在签名那一步就被拒，而不是先透露「你这个串的时间戳过期了」。
pub fn validate(
    key: &proxy_manager_wire::sign::HmacKey,
    state: Option<&str>,
    cookie: Option<&str>,
    ttl_secs: u32,
    now: OffsetDateTime,
) -> std::result::Result<String, StateError> {
    let state = state.ok_or(StateError::Invalid)?;
    let cookie = cookie.ok_or(StateError::Invalid)?;
    if !constant_time_eq(state.as_bytes(), cookie.as_bytes()) {
        return Err(StateError::Invalid);
    }

    // 逐段解析即严格格式校验：三段、时间戳是 1..=19 位十进制、
    // 随机量恰好 64 个十六进制字符、签名恰好 64 个十六进制字符。
    let mut parts = state.split('.');
    let (issued, random, mac) = match (parts.next(), parts.next(), parts.next(), parts.next()) {
        (Some(a), Some(b), Some(c), None) => (a, b, c),
        _ => return Err(StateError::Invalid),
    };
    if issued.is_empty()
        || issued.len() > 19
        || !issued.bytes().all(|b| b.is_ascii_digit())
        || random.len() != 64
        || !random.bytes().all(|b| b.is_ascii_hexdigit())
        || mac.len() != 64
        || !mac.bytes().all(|b| b.is_ascii_hexdigit())
    {
        return Err(StateError::Invalid);
    }

    let payload = format!("{issued}.{random}");
    if !constant_time_eq(sign(key, &payload).as_bytes(), mac.as_bytes()) {
        return Err(StateError::Invalid);
    }

    let issued_at: i64 = issued.parse().map_err(|_| StateError::Invalid)?;
    let now_secs = now.unix_timestamp();
    if issued_at > now_secs + MAX_FUTURE_SKEW_SECS {
        return Err(StateError::Invalid);
    }
    if now_secs - issued_at > ttl_secs as i64 {
        return Err(StateError::Expired);
    }
    Ok(state.to_string())
}

/// 原子地消费一个 state：写墓碑，冲突即重放。
///
/// **只在上游换取身份成功之后调用。** 顺带清掉已过期的墓碑——
/// 放在同一个事务里，这张表就不会无限长，也不需要另一个定时任务。
pub async fn consume(store: &Store, state: &str, ttl_secs: u32) -> Result<bool> {
    let now = OffsetDateTime::now_utc();
    let mut txn = store.begin_immediate().await?;
    sqlx::query("DELETE FROM sso_used_states WHERE expires_at < ?")
        .bind(bucket::to_rfc3339(now))
        .execute(txn.conn())
        .await?;
    let inserted = sqlx::query(
        "INSERT INTO sso_used_states(state_sha256, expires_at, consumed_at) \
         VALUES (?, ?, ?) ON CONFLICT(state_sha256) DO NOTHING",
    )
    .bind(hex::encode(Sha256::digest(state.as_bytes())))
    .bind(bucket::to_rfc3339(now + time::Duration::seconds(ttl_secs as i64)))
    .bind(bucket::to_rfc3339(now))
    .execute(txn.conn())
    .await?
    .rows_affected();
    txn.commit().await?;
    Ok(inserted == 1)
}

fn sign(key: &proxy_manager_wire::sign::HmacKey, payload: &str) -> String {
    let mut message = Vec::with_capacity(DOMAIN.len() + payload.len());
    message.extend_from_slice(DOMAIN);
    message.extend_from_slice(payload.as_bytes());
    key.tag(&message)
}

fn set_cookie(value: &str, max_age: i64) -> String {
    // `Path=/` 是 `__Host-` 前缀的硬性要求。它顺带消掉了另一个隐患：
    // 原先把 `Path` 写成回调路径的字面量，而回调路径又要与
    // `sso.public_base_url` 拼出来的地址一致——两者一旦不等，
    // cookie 根本发不出去，登录 100% 失败且没有任何诊断信息。
    let mut cookie = format!("{STATE_COOKIE}={value}; Path=/; Max-Age={max_age}");
    // HttpOnly：页面脚本没有任何读它的理由。
    // SameSite=Lax：回调是上游发起的顶层导航，Strict 会让 cookie 发不出来。
    cookie.push_str("; HttpOnly; Secure; SameSite=Lax");
    if max_age == 0 {
        cookie.push_str("; Expires=Thu, 01 Jan 1970 00:00:00 GMT");
    }
    cookie
}

/// 清掉回调 cookie 的 `Set-Cookie`。**回调处理的第一件事**（加固 3）。
pub fn clear_cookie() -> String {
    set_cookie("", 0)
}

fn constant_time_eq(a: &[u8], b: &[u8]) -> bool {
    if a.len() != b.len() {
        return false;
    }
    let mut diff = 0u8;
    for (x, y) in a.iter().zip(b) {
        diff |= x ^ y;
    }
    diff == 0
}
