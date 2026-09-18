//! 会话、CSRF 与每请求授权求值（README §4.9）。
//!
//! **M4 只做会话机制，登录入口属于 M5。** 这里没有任何 provider 的对接代码，
//! 只有 [`create`] 这个给 provider 调的入口；`local`（break-glass）同样走它。
//!
//! 每一步都 fail closed。授权链上「看起来没问题就放行」的分支一个都不留——
//! 认证代码的失败模式是**沉默地放行**，而那种错误在日志里看不出来。

use sha2::{Digest, Sha256};
use sqlx::Row;
use time::{Duration, OffsetDateTime};

use crate::api::error::{ApiCode, ApiError, ApiResult};
use crate::error::Result;
use crate::ledger::bucket;
use crate::store::Store;

/// `__Host-` 前缀要求 `Secure` + `Path=/` + 无 `Domain`，
/// 浏览器据此保证这个 cookie 不可能由子域写入。
pub const SESSION_COOKIE: &str = "__Host-pm_session";
/// CSRF cookie **刻意不是 HttpOnly**：双提交需要页面脚本读得到它。
/// 它不是凭据——单独拿到它没用，必须配上 HttpOnly 的会话 cookie。
pub const CSRF_COOKIE: &str = "__Host-pm_csrf";
pub const CSRF_HEADER: &str = "x-csrf-token";

const SECRET_NAME: &str = "session_proof_hmac";
/// 成员事实 TTL 默认 60 秒（D13）。
pub const MEMBER_FACT_TTL_SECS: i64 = 60;
/// `last_seen_at` 的写入节流：单写者库上每请求一次写会把队列占满。
const LAST_SEEN_THROTTLE_SECS: i64 = 60;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Role {
    Admin,
    User,
}

impl Role {
    pub fn parse(text: &str) -> Option<Self> {
        match text {
            "admin" => Some(Role::Admin),
            "user" => Some(Role::User),
            _ => None,
        }
    }

    pub fn as_str(self) -> &'static str {
        match self {
            Role::Admin => "admin",
            Role::User => "user",
        }
    }

    pub fn is_admin(self) -> bool {
        self == Role::Admin
    }
}

/// 一个已通过每请求求值的主体。
///
/// **角色不由客户端选择或声明**——它只来自服务端这一次求值的结果。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Subject {
    pub session_pk: i64,
    pub user_id: i64,
    pub login_name: String,
    pub role: Role,
    pub provider: String,
}

impl Subject {
    /// 管理员闸门。**越权在服务端拦**，不是前端隐藏导航。
    pub fn require_admin(&self) -> ApiResult<()> {
        if self.role.is_admin() {
            Ok(())
        } else {
            Err(ApiError::forbidden())
        }
    }

    /// 个人端点：目标固定取服务端会话主体，**不接受客户端提供的用户 ID**。
    pub fn owns(&self, user_id: i64) -> ApiResult<()> {
        if self.user_id == user_id {
            Ok(())
        } else {
            Err(ApiError::forbidden())
        }
    }
}

/// 新签发的会话。`cookie_value` 直接放进 `__Host-pm_session`。
#[derive(Debug, Clone)]
pub struct IssuedSession {
    pub session_pk: i64,
    pub cookie_value: String,
    pub csrf_token: String,
    pub expires_at: OffsetDateTime,
}

fn random_hex_32() -> String {
    use rand::RngCore;
    let mut bytes = [0u8; 32];
    rand::thread_rng().fill_bytes(&mut bytes);
    hex::encode(bytes)
}

fn sha256_hex(value: &str) -> String {
    hex::encode(Sha256::digest(value.as_bytes()))
}

/// 会话 ID 的 HMAC 证明用的 key（§4.9 加固 4）。
async fn proof_key(store: &Store) -> Result<proxy_manager_wire::sign::HmacKey> {
    server_secret(store, SECRET_NAME).await
}

/// 预生成会话证明用的那把 key。见 `sso::warm_secrets`。
pub async fn warm_proof_key(store: &Store) -> Result<()> {
    proof_key(store).await.map(|_| ())
}

/// 按名字取一把服务端密钥，首次使用时生成并落库。
///
/// 放库里而不是配置文件：它要随库一起进一致性备份（R3），
/// 而且与会话表同生共死——单独丢一个都会让所有会话失效。
///
/// **每种用途一把 key。** 一把 key 用在两处，就意味着其中一处的分析结论
/// 不能独立成立；而域分隔串只能减轻这件事，不能消除它。
pub async fn server_secret(store: &Store, name: &str) -> Result<proxy_manager_wire::sign::HmacKey> {
    if let Some(row) = sqlx::query("SELECT secret_hex FROM server_secrets WHERE name = ?")
        .bind(name)
        .fetch_optional(store.readers())
        .await?
    {
        return proxy_manager_wire::sign::HmacKey::from_hex(&row.get::<String, _>(0))
            .map_err(|e| crate::error::Error::Quota(format!("会话密钥不合法：{e}")));
    }
    let key = proxy_manager_wire::sign::HmacKey::generate();
    let mut txn = store.begin_immediate().await?;
    // 并发首次访问时可能两边都想插；冲突了就读回已有那把。
    sqlx::query(
        "INSERT INTO server_secrets(name, secret_hex, created_at) VALUES (?, ?, ?) \
         ON CONFLICT(name) DO NOTHING",
    )
    .bind(name)
    .bind(key.to_hex())
    .bind(bucket::to_rfc3339(OffsetDateTime::now_utc()))
    .execute(txn.conn())
    .await?;
    txn.commit().await?;
    let row = sqlx::query("SELECT secret_hex FROM server_secrets WHERE name = ?")
        .bind(name)
        .fetch_one(store.readers())
        .await?;
    proxy_manager_wire::sign::HmacKey::from_hex(&row.get::<String, _>(0))
        .map_err(|e| crate::error::Error::Quota(format!("会话密钥不合法：{e}")))
}

/// 会话 ID 的 HMAC 证明。
///
/// 域分隔串进编码，让这把 key 万一被用在别处也不会互相冒充。
fn proof_of(key: &proxy_manager_wire::sign::HmacKey, session_id: &str) -> String {
    let mut message = Vec::with_capacity(64 + session_id.len());
    message.extend_from_slice(b"proxy-manager/v1/session-proof\0");
    message.extend_from_slice(session_id.as_bytes());
    key.tag(&message)
}

/// 成员事实的有效窗口。`None` 表示这个 provider 不依赖任何上游事实。
///
/// 窗口长度是**「上游撤权最迟多久被发现」**，所以它由那个 provider
/// 到底有没有可刷新的事实源决定：
///
/// - `local`：break-glass 本地账号，没有上游，也就没有事实可刷。
/// - `paoyou`：泡游只有一次性的 `check-token`，**没有按账号查状态的接口**，
///   所以窗口只能等于会话本身——续不了的东西没有 TTL 可言。
///   代价记在 D27：泡游侧停用一个人，最迟要到会话过期才生效。
///   名单内的角色与档位调整不受此限，那部分每请求从配置现解析。
/// - `feishu` / `wecom`：有 API 可查成员，按 D13 的 60 秒。
fn member_fact_window(provider: &str, session_ttl: Duration) -> Option<Duration> {
    match provider {
        "local" => None,
        "paoyou" => Some(session_ttl),
        _ => Some(Duration::seconds(MEMBER_FACT_TTL_SECS)),
    }
}

/// 签发一个会话。**给 provider 调**——泡游 SSO / break-glass 都走这里。
pub async fn create(
    store: &Store,
    user_id: i64,
    provider: &str,
    ttl: Duration,
) -> Result<IssuedSession> {
    let session_id = random_hex_32();
    let csrf_token = random_hex_32();
    let now = OffsetDateTime::now_utc();
    let expires_at = now + ttl;

    let fact = match member_fact_window(provider, ttl) {
        Some(window) => (Some(bucket::to_rfc3339(now)), Some(bucket::to_rfc3339(now + window))),
        None => (None, None),
    };

    let mut txn = store.begin_immediate().await?;
    let row = sqlx::query(
        "INSERT INTO sessions(session_id_sha256, user_id, provider, csrf_token_sha256, \
         created_at, expires_at, last_seen_at, member_fact_verified_at, member_fact_expires_at) \
         VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING session_pk",
    )
    .bind(sha256_hex(&session_id))
    .bind(user_id)
    .bind(provider)
    .bind(sha256_hex(&csrf_token))
    .bind(bucket::to_rfc3339(now))
    .bind(bucket::to_rfc3339(expires_at))
    .bind(bucket::to_rfc3339(now))
    .bind(fact.0)
    .bind(fact.1)
    .fetch_one(txn.conn())
    .await?;
    let session_pk: i64 = row.get(0);
    txn.commit().await?;

    let key = proof_key(store).await?;
    Ok(IssuedSession {
        session_pk,
        cookie_value: format!("{session_id}.{}", proof_of(&key, &session_id)),
        csrf_token,
        expires_at,
    })
}

/// 每请求求值。
///
/// 顺序固定，每一步都 fail closed：
/// **HMAC 证明 → 查表 → 吊销 → 过期 → 成员事实新鲜度 → 账号有效性 → 求值角色**。
///
/// HMAC 证明在最前面，是因为它**不碰数据库**：伪造的会话 ID 在这里就被挡掉，
/// 打不到库上去。这也是 §4.9 加固 4 的用意。
///
/// **不缓存最终授权结论**（D13）。规则文件与 TTL 内的成员事实可以缓存，
/// 结论不行——缓存结论就等于本地撤销要等缓存过期才生效。
pub async fn resolve(
    store: &Store,
    cookie_value: &str,
    roster: &crate::sso::roster::Roster,
) -> ApiResult<Subject> {
    let (session_id, proof) = cookie_value.split_once('.').ok_or_else(ApiError::unauthenticated)?;
    if session_id.len() != 64 || !session_id.bytes().all(|b| b.is_ascii_hexdigit()) {
        return Err(ApiError::unauthenticated());
    }

    let key = proof_key(store).await.map_err(ApiError::from)?;
    let expected = proof_of(&key, session_id);
    // 常量时间比较：两个都是十六进制定长串，长度已经一致。
    if !constant_time_eq(expected.as_bytes(), proof.as_bytes()) {
        return Err(ApiError::unauthenticated());
    }

    let row = sqlx::query(
        "SELECT s.session_pk, s.user_id, s.provider, s.expires_at, s.revoked_at, \
                s.member_fact_expires_at, s.last_seen_at, u.login_name, u.role, u.status, \
                u.feishu_fs_id \
         FROM sessions s JOIN users u ON u.user_id = s.user_id \
         WHERE s.session_id_sha256 = ?",
    )
    .bind(sha256_hex(session_id))
    .fetch_optional(store.readers())
    .await
    .map_err(|e| ApiError::internal(e.to_string()))?
    .ok_or_else(ApiError::unauthenticated)?;

    let session_pk: i64 = row.get(0);
    let now = OffsetDateTime::now_utc();

    // **本地撤销在每请求检查，下一次请求立即生效。**
    if row.get::<Option<String>, _>(4).is_some() {
        return Err(ApiError::new(ApiCode::SessionRevoked, "会话已被吊销"));
    }
    let expires_at = bucket::parse_rfc3339(&row.get::<String, _>(3))
        .ok_or_else(|| ApiError::internal("会话过期时间不是合法 RFC 3339"))?;
    if now >= expires_at {
        return Err(ApiError::unauthenticated());
    }

    // 成员事实新鲜度（D13）。**过期必须先刷新再决定授权**——
    // M4 没有 IM provider 可刷新，所以这里只能拒绝，并明确显示认证依赖不可用。
    let provider: String = row.get(2);
    if provider != "local" {
        let fact_expires =
            row.get::<Option<String>, _>(5).and_then(|text| bucket::parse_rfc3339(&text));
        match fact_expires {
            Some(at) if now < at => {}
            _ => {
                return Err(ApiError::new(
                    ApiCode::MemberFactStale,
                    "成员事实已过期且无法刷新：认证依赖不可用",
                ))
            }
        }
    }

    // 停用账号仍拒绝访问，**不等同于默认普通用户**。
    let status: String = row.get(9);
    if status != "active" {
        return Err(ApiError::forbidden());
    }
    // 求值角色。角色只来自服务端，客户端声明什么都不看。
    //
    // **从配置里的成员名单现解析，不读 `users.role`。** 那一列是登录时同步过去的
    // 展示缓存；把它当授权依据，效果就是「把人从名单里删掉之后，他还能一直用到
    // 下次登录」——而撤权恰恰是这条链路上最不能等的一件事（D27）。
    //
    // `local` 是唯一的例外：break-glass 账号不在名单里，它存在的理由正是
    // 名单那条路走不通的时候还能进得来（README §4.9）。
    let login_name: String = row.get(7);
    let role = if provider == "local" {
        Role::parse(&row.get::<String, _>(8)).ok_or_else(|| ApiError::internal("用户角色不合法"))?
    } else {
        roster.grade(&login_name, row.get::<Option<String>, _>(10).as_deref()).role
    };

    // last_seen_at 节流写：单写者库上每请求一次写会把队列占满。
    if let Some(last_seen) = bucket::parse_rfc3339(&row.get::<String, _>(6)) {
        if (now - last_seen).whole_seconds() >= LAST_SEEN_THROTTLE_SECS {
            let _ = touch(store, session_pk, now).await;
        }
    }

    Ok(Subject { session_pk, user_id: row.get(1), login_name, role, provider })
}

async fn touch(store: &Store, session_pk: i64, now: OffsetDateTime) -> Result<()> {
    let mut txn = store.begin_immediate().await?;
    sqlx::query("UPDATE sessions SET last_seen_at = ? WHERE session_pk = ?")
        .bind(bucket::to_rfc3339(now))
        .bind(session_pk)
        .execute(txn.conn())
        .await?;
    txn.commit().await
}

/// 校验双提交 CSRF token。
///
/// 三个都要对上：header 存在、与 cookie 相同、且哈希与库里的一致。
/// 前两者是经典的双提交（不依赖服务端状态），第三者让「拿到了 CSRF cookie
/// 但没有会话」的攻击者也过不去。
pub async fn verify_csrf(
    store: &Store,
    session_pk: i64,
    header_token: Option<&str>,
    cookie_token: Option<&str>,
) -> ApiResult<()> {
    let header = header_token.ok_or_else(|| {
        ApiError::new(ApiCode::CsrfInvalid, format!("写操作必须带 {CSRF_HEADER}"))
    })?;
    let cookie =
        cookie_token.ok_or_else(|| ApiError::new(ApiCode::CsrfInvalid, "缺少 CSRF cookie"))?;
    if !constant_time_eq(header.as_bytes(), cookie.as_bytes()) {
        return Err(ApiError::new(ApiCode::CsrfInvalid, "CSRF token 与 cookie 不一致"));
    }
    let row = sqlx::query("SELECT csrf_token_sha256 FROM sessions WHERE session_pk = ?")
        .bind(session_pk)
        .fetch_one(store.readers())
        .await
        .map_err(|e| ApiError::internal(e.to_string()))?;
    if !constant_time_eq(sha256_hex(header).as_bytes(), row.get::<String, _>(0).as_bytes()) {
        return Err(ApiError::new(ApiCode::CsrfInvalid, "CSRF token 不属于该会话"));
    }
    Ok(())
}

/// 主动吊销一个会话。下一次请求立即生效。
pub async fn revoke(store: &Store, session_pk: i64) -> Result<()> {
    let mut txn = store.begin_immediate().await?;
    sqlx::query("UPDATE sessions SET revoked_at = ? WHERE session_pk = ? AND revoked_at IS NULL")
        .bind(bucket::to_rfc3339(OffsetDateTime::now_utc()))
        .bind(session_pk)
        .execute(txn.conn())
        .await?;
    txn.commit().await
}

/// 吊销某主体的**全部**会话。
///
/// 上游返回停用/离职时要调它（§4.9）。M5 接上 IM 之后由成员刷新触发。
pub async fn revoke_all_for_user(store: &Store, user_id: i64) -> Result<u64> {
    let mut txn = store.begin_immediate().await?;
    let affected =
        sqlx::query("UPDATE sessions SET revoked_at = ? WHERE user_id = ? AND revoked_at IS NULL")
            .bind(bucket::to_rfc3339(OffsetDateTime::now_utc()))
            .bind(user_id)
            .execute(txn.conn())
            .await?
            .rows_affected();
    txn.commit().await?;
    Ok(affected)
}

/// 把成员事实标记为已过期。测试与 M5 的「上游不可达」路径都要用。
pub async fn expire_member_fact(store: &Store, session_pk: i64) -> Result<()> {
    let mut txn = store.begin_immediate().await?;
    sqlx::query("UPDATE sessions SET member_fact_expires_at = ? WHERE session_pk = ?")
        .bind(bucket::to_rfc3339(OffsetDateTime::now_utc() - Duration::seconds(1)))
        .bind(session_pk)
        .execute(txn.conn())
        .await?;
    txn.commit().await
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

/// 从 `Cookie` 头里取一个值。不引 cookie 库：我们只需要读两个固定名字。
pub fn cookie_value<'a>(header: Option<&'a str>, name: &str) -> Option<&'a str> {
    header?.split(';').find_map(|pair| {
        let (key, value) = pair.split_once('=')?;
        (key.trim() == name).then(|| value.trim())
    })
}

/// 登录态页面与 API 的缓存头（§4.11）。
///
/// `private, no-store` + `Vary: Cookie`——避免不同登录态的内容被共享缓存串台。
pub const PRIVATE_CACHE_CONTROL: &str = "private, no-store";
