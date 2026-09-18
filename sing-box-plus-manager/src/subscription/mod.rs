//! 订阅（README §4.7、M6）。
//!
//! 一个用户一条 token，**首次登录时在同一个事务里创建**，之后只读。
//! 明文只在创建时返回一次；库里只存哈希。
//!
//! **没有自助轮换，这是刻意的。** 轮换会让该用户所有已导入的客户端**静默失效**，
//! 而用户通常不知道要重新导入——同类系统上线后把自助轮换整个删掉了
//! （删路由、删控制器、删仓储方法，不是 CSS 隐藏入口）。本项目沿用那个结论：
//! 吊销是运维动作，走命令行、需理由、落审计。

pub mod render;

use sha2::{Digest, Sha256};
use sqlx::Row;
use time::OffsetDateTime;

use crate::error::{Error, Result};
use crate::ledger::bucket;
use crate::store::Store;

/// 新发的一条 token。`plaintext` **只有这一次**能拿到。
#[derive(Debug, Clone)]
pub struct IssuedToken {
    pub plaintext: String,
    pub newly_created: bool,
}

fn hash(token: &str) -> String {
    hex::encode(Sha256::digest(token.as_bytes()))
}

const SECRET_NAME: &str = "subscription_token_hmac";

/// 从 `(user_id, generation)` **派生** token。
///
/// 不是随机生成后存起来：明文一个字节都不落库，而本人随时能再算一次。
/// 这解决了 §4.7 自己的一处矛盾——那一节既要求「只存哈希」，
/// 又要求 token「之后只读」，而两者只有在它可重算时才能同时成立。
///
/// 域分隔串进编码，让这把 key 万一被用在别处也不会互相冒充。
async fn derive(store: &Store, user_id: i64, generation: i64) -> Result<String> {
    let key = crate::api::session::server_secret(store, SECRET_NAME).await?;
    let mut message = Vec::with_capacity(64);
    message.extend_from_slice(b"proxy-manager/v1/subscription\0");
    message.extend_from_slice(&user_id.to_be_bytes());
    message.extend_from_slice(&generation.to_be_bytes());
    Ok(key.tag(&message))
}

/// 给用户发一条订阅 token。**幂等**：已经有未吊销的就不再发。
///
/// 幂等不是可选的：重复发会让第一条仍然有效，于是「吊销一条」不等于断掉这个人。
/// 库层的部分唯一索引是第二道，这里是第一道。
///
/// **已有 token 时重算同一个**（`newly_created = false`）。
/// token 是派生的，所以本人随时能再看一次自己的订阅地址，
/// 而明文一个字节都没落过库。
pub async fn issue_for_user(store: &Store, user_id: i64) -> Result<IssuedToken> {
    if let Some(generation) = active_generation(store, user_id).await? {
        // 已经有了：**重算**同一个，不是发一条新的。
        return Ok(IssuedToken {
            plaintext: derive(store, user_id, generation).await?,
            newly_created: false,
        });
    }
    // 下一代：吊销过 N 次就是 N+1 代。旧代的 token 算出来也查不到。
    let generation = next_generation(store, user_id).await?;
    let plaintext = derive(store, user_id, generation).await?;

    let mut txn = store.begin_immediate().await?;
    sqlx::query(
        "INSERT INTO subscription_tokens(token_sha256, user_id, generation, created_at) \
         VALUES (?, ?, ?, ?) ON CONFLICT(token_sha256) DO NOTHING",
    )
    .bind(hash(&plaintext))
    .bind(user_id)
    .bind(generation)
    .bind(bucket::to_rfc3339(OffsetDateTime::now_utc()))
    .execute(txn.conn())
    .await?;
    txn.commit().await?;
    Ok(IssuedToken { plaintext, newly_created: true })
}

async fn active_generation(store: &Store, user_id: i64) -> Result<Option<i64>> {
    Ok(sqlx::query(
        "SELECT generation FROM subscription_tokens \
          WHERE user_id = ? AND revoked_at IS NULL",
    )
    .bind(user_id)
    .fetch_optional(store.readers())
    .await?
    .map(|row| row.get(0)))
}

async fn next_generation(store: &Store, user_id: i64) -> Result<i64> {
    let row = sqlx::query("SELECT MAX(generation) FROM subscription_tokens WHERE user_id = ?")
        .bind(user_id)
        .fetch_one(store.readers())
        .await?;
    Ok(row.get::<Option<i64>, _>(0).unwrap_or(0) + 1)
}

/// 本人的订阅地址。**随时可以再看一次**——token 是派生的，不是存起来的。
pub async fn current_token(store: &Store, user_id: i64) -> Result<Option<String>> {
    match active_generation(store, user_id).await? {
        Some(generation) => Ok(Some(derive(store, user_id, generation).await?)),
        None => Ok(None),
    }
}

/// 订阅请求解出来的主体。
#[derive(Debug, Clone)]
pub struct Subscriber {
    pub user_id: i64,
    pub login_name: String,
    pub quota_group: String,
}

/// 按 token 解出用户。**每次都查吊销**，与会话吊销同一条纪律。
///
/// 返回 `None` 覆盖全部失败原因：token 不存在、已吊销、账号停用。
/// **刻意不区分**——区分开来就等于给拿着一个无效 token 的人一个探测接口。
pub async fn resolve(store: &Store, token: &str) -> Result<Option<Subscriber>> {
    // 形状先挡一道，省掉一次查库；也让明显乱来的请求不进数据库。
    if token.len() != 64 || !token.bytes().all(|b| b.is_ascii_hexdigit()) {
        return Ok(None);
    }
    let row = sqlx::query(
        "SELECT t.token_pk, t.user_id, t.revoked_at, u.login_name, u.quota_group, u.status \
           FROM subscription_tokens t JOIN users u ON u.user_id = t.user_id \
          WHERE t.token_sha256 = ?",
    )
    .bind(hash(token))
    .fetch_optional(store.readers())
    .await?;
    let Some(row) = row else { return Ok(None) };
    if row.get::<Option<String>, _>(2).is_some() || row.get::<String, _>(5) != "active" {
        return Ok(None);
    }

    // `last_used_at` 用来回答「这个人还在用吗」——吊销一个从没被用过的 token
    // 和吊销一个每天在用的，风险完全不同。写失败不影响取订阅。
    let token_pk: i64 = row.get(0);
    let _ = touch(store, token_pk).await;

    Ok(Some(Subscriber { user_id: row.get(1), login_name: row.get(3), quota_group: row.get(4) }))
}

async fn touch(store: &Store, token_pk: i64) -> Result<()> {
    let mut txn = store.begin_immediate().await?;
    sqlx::query("UPDATE subscription_tokens SET last_used_at = ? WHERE token_pk = ?")
        .bind(bucket::to_rfc3339(OffsetDateTime::now_utc()))
        .bind(token_pk)
        .execute(txn.conn())
        .await?;
    txn.commit().await
}

/// 吊销某用户的订阅。**运维动作**，不是用户自助。
///
/// 吊销之后那个人所有已导入的客户端会在下一次更新时失败，
/// 而且他不会收到任何通知——所以调用方必须先想清楚再确认。
pub async fn revoke_for_user(store: &Store, user_id: i64) -> Result<u64> {
    let mut txn = store.begin_immediate().await?;
    let affected = sqlx::query(
        "UPDATE subscription_tokens SET revoked_at = ? \
          WHERE user_id = ? AND revoked_at IS NULL",
    )
    .bind(bucket::to_rfc3339(OffsetDateTime::now_utc()))
    .bind(user_id)
    .execute(txn.conn())
    .await?
    .rows_affected();
    txn.commit().await?;
    Ok(affected)
}

/// 这个人这一周期的用量与额度——`Subscription-Userinfo` 要的那三个数。
#[derive(Debug, Clone, Copy, Default)]
pub struct CycleUsage {
    pub upload: u128,
    pub download: u128,
    pub total: u64,
}

/// 取当前周期的已结算用量与**真实**额度。
///
/// §4.7 有一条专门的警告：别照抄不做配额的系统，那种系统里 `total` 是给客户端看的
/// 假值；本项目有真实闸断，填假值会让客户端显示与实际行为对不上。
/// 所以 `total` 取的是这个人档位的真实月度额度，`upload`/`download` 取
/// **当前周期**的已结算累计——不是 lifetime。
pub async fn cycle_usage(store: &Store, user_id: i64) -> Result<CycleUsage> {
    let cycle = crate::quota::cycle_key(OffsetDateTime::now_utc());
    let row = sqlx::query(
        "SELECT sum(CAST(l.tcp_uplink_bytes AS INTEGER) \
                  + CAST(l.udp_uplink_bytes AS INTEGER)), \
                sum(CAST(l.tcp_downlink_bytes AS INTEGER) \
                  + CAST(l.udp_downlink_bytes AS INTEGER)) \
           FROM usage_ledger l \
           JOIN snapshot_batches b ON b.batch_pk = l.batch_pk \
          WHERE l.user_id = ? AND b.status = 'applied' \
            AND l.accounting_at >= ?",
    )
    .bind(user_id)
    .bind(cycle_start(&cycle)?)
    .fetch_one(store.readers())
    .await?;
    let total = crate::quota::settings::user_quota(store, user_id)
        .await?
        .map(|(_, bytes)| bytes)
        .unwrap_or(0);
    Ok(CycleUsage {
        upload: row.get::<Option<i64>, _>(0).unwrap_or(0).max(0) as u128,
        download: row.get::<Option<i64>, _>(1).unwrap_or(0).max(0) as u128,
        total,
    })
}

/// 周期键（`YYYY-MM`，UTC+8）对应的起点，RFC 3339 UTC。
///
/// 账本的 `accounting_at` 是 UTC，而周期按 UTC+8 切——所以 2026-09 这个周期
/// 的起点是 `2026-08-31T16:00:00Z`，不是 `2026-09-01T00:00:00Z`。
/// 写错的后果是每个月头 8 小时的用量算进上一个月，而那只有对账时才看得出来。
fn cycle_start(cycle_key: &str) -> Result<String> {
    let (year, month) = cycle_key
        .split_once('-')
        .ok_or_else(|| Error::Quota(format!("周期键形状不对：{cycle_key}")))?;
    let year: i32 = year.parse().map_err(|_| Error::Quota("周期键的年份不是整数".into()))?;
    let month: u8 = month.parse().map_err(|_| Error::Quota("周期键的月份不是整数".into()))?;
    let month = time::Month::try_from(month).map_err(|e| Error::Quota(e.to_string()))?;
    let date =
        time::Date::from_calendar_date(year, month, 1).map_err(|e| Error::Quota(e.to_string()))?;
    let local = date.with_hms(0, 0, 0).map_err(|e| Error::Quota(e.to_string()))?;
    let start = local.assume_offset(time::macros::offset!(+8)).to_offset(time::UtcOffset::UTC);
    Ok(bucket::to_rfc3339(start))
}

/// 周期结束时刻的 unix 秒——`Subscription-Userinfo` 的 `expire`。
pub fn cycle_expire_unix() -> Result<i64> {
    let now = OffsetDateTime::now_utc();
    let cycle = crate::quota::cycle_key(now);
    let (year, month) =
        cycle.split_once('-').ok_or_else(|| Error::Quota("周期键形状不对".into()))?;
    let year: i32 = year.parse().map_err(|_| Error::Quota("年份不是整数".into()))?;
    let month: u8 = month.parse().map_err(|_| Error::Quota("月份不是整数".into()))?;
    let (next_year, next_month) = if month == 12 { (year + 1, 1u8) } else { (year, month + 1) };
    let month = time::Month::try_from(next_month).map_err(|e| Error::Quota(e.to_string()))?;
    let date = time::Date::from_calendar_date(next_year, month, 1)
        .map_err(|e| Error::Quota(e.to_string()))?;
    let local = date.with_hms(0, 0, 0).map_err(|e| Error::Quota(e.to_string()))?;
    Ok(local.assume_offset(time::macros::offset!(+8)).unix_timestamp())
}

/// 这个人当前持有的身份名。没有就是 `None`——那时订阅是空的。
pub async fn claimed_identity(store: &Store, user_id: i64) -> Result<Option<String>> {
    let row = sqlx::query(
        "SELECT identity_name FROM identity_routes \
          WHERE user_id = ? AND state = 'claimed' LIMIT 1",
    )
    .bind(user_id)
    .fetch_optional(store.readers())
    .await?;
    Ok(row.map(|row| row.get(0)))
}

/// 凭据文件的热源：按 mtime + inode + 大小缓存，变了就重读。
///
/// 与成员名单同一条纪律，理由不同：名单放文件是因为**撤权不能等**，
/// 凭据放文件是因为**它们不该进账本的备份**——账本要按 R3 做一致性备份并演练恢复，
/// 而那份备份会被复制到别处；uPSK 进去之后，每一份账本备份都变成一份凭据副本。
///
/// **读不出来失败关闭**：`/sub/…` 返回 503 而不是空订阅。发一份空订阅的后果是
/// 客户端把「配置事故」当成「你的节点被撤了」，然后用户去找管理员要新链接——
/// 而链接是好的。
#[derive(Debug)]
pub struct CredentialSource {
    path: std::path::PathBuf,
    cached: std::sync::RwLock<Option<(Stamp, std::sync::Arc<crate::config::Credentials>)>>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
struct Stamp {
    dev: u64,
    inode: u64,
    len: u64,
    mtime: Option<std::time::SystemTime>,
}

impl CredentialSource {
    pub fn new(path: impl Into<std::path::PathBuf>) -> Self {
        CredentialSource { path: path.into(), cached: std::sync::RwLock::new(None) }
    }

    pub fn load(&self) -> Result<std::sync::Arc<crate::config::Credentials>> {
        let stamp = self.stat()?;
        if let Ok(guard) = self.cached.read() {
            if let Some((cached, credentials)) = guard.as_ref() {
                if *cached == stamp {
                    return Ok(credentials.clone());
                }
            }
        }
        let text = std::fs::read_to_string(&self.path)
            .map_err(|source| Error::ReadFile { path: self.path.clone(), source })?;
        // 读后复验：读到一半被换掉时宁可失败，也不用半份内容发订阅。
        if self.stat()? != stamp {
            return Err(Error::invalid_config(
                "§4.7",
                format!("{} 在读取过程中被改动", self.path.display()),
            ));
        }
        let credentials: crate::config::Credentials = toml::from_str(&text)
            .map_err(|source| Error::ParseToml { path: self.path.clone(), source })?;
        credentials.validate()?;
        let credentials = std::sync::Arc::new(credentials);
        if let Ok(mut guard) = self.cached.write() {
            *guard = Some((stamp, credentials.clone()));
        }
        tracing::info!(
            path = %self.path.display(), identities = credentials.upsk.len(),
            "订阅凭据已重新载入"
        );
        Ok(credentials)
    }

    fn stat(&self) -> Result<Stamp> {
        use std::os::unix::fs::MetadataExt;
        let meta = std::fs::metadata(&self.path)
            .map_err(|source| Error::ReadFile { path: self.path.clone(), source })?;
        if !meta.is_file() {
            return Err(Error::invalid_config(
                "§4.7",
                format!("{} 不是普通文件", self.path.display()),
            ));
        }
        // 凭据文件**不得对组或其他人可读**。300 条 uPSK 在里面，
        // 权限松一位就等于把它交给这台机器上的每一个进程。
        if meta.mode() & 0o077 != 0 {
            return Err(Error::invalid_config(
                "§4.7",
                format!(
                    "{} 权限是 {:o}，对组或其他人可读：凭据文件必须 0600",
                    self.path.display(),
                    meta.mode() & 0o777
                ),
            ));
        }
        Ok(Stamp {
            dev: meta.dev(),
            inode: meta.ino(),
            len: meta.len(),
            mtime: meta.modified().ok(),
        })
    }
}
