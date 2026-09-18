//! 泡游 SSO 登录（README §4.9、D27）。
//!
//! 整条路径的顺序是固定的，它就是 §4.9 加固 3 那一条：
//!
//! ```text
//! 清 state cookie → 拒绝未知 query 参数 → 无任何写入地校验 state
//!   → 调上游换取身份 → 成功之后才写墓碑 → 建号/更新 → 建会话 → 303 清掉 URL 里的 token
//! ```
//!
//! **匿名伪造的回调不创建会话、也不写数据库。** 顺序写反，登录入口就是一个
//! 免认证的写放大入口：任何人构造一个回调 URL 就能让主控写一行。
//!
//! 模块分工：[`http`] 是那一次外呼的底座，[`client`] 是换身份的协议，
//! [`state`] 是防重放，[`roster`] 是**授权的事实来源**。
//! 本模块只做把它们串起来的那一段，以及建号事务。

pub mod client;
pub mod http;
pub mod roster;
pub mod state;

use std::sync::Arc;

use sqlx::Row;
use time::OffsetDateTime;

use crate::api::session::{self, IssuedSession, Role};
use crate::config::SsoConfig;
use crate::error::{Error, Result};
use crate::ledger::bucket;
use crate::sso::client::SsoIdentity;
use crate::sso::http::Endpoint;
use crate::sso::roster::{Grade, Roster, RosterSource};
use crate::store::Store;

/// 会话里记下的 provider。
pub const PROVIDER: &str = "paoyou";

const STATE_SECRET_NAME: &str = "sso_state_hmac";

/// 启动时解析一次的运行期句柄。
///
/// **名单不在这里定型。** `roster` 是一个按 mtime + inode 重读的热源
/// （[`RosterSource`]），每个受保护请求取一次。把它固化成一份启动快照，
/// 「改名单下一个请求即生效」这句话就是假的——撤权要重启进程，
/// 而那正是 D27 用整段篇幅论证不能接受的事。
#[derive(Debug, Clone)]
pub struct Sso {
    pub config: Arc<SsoConfig>,
    roster: Arc<RosterSource>,
    check_token: Endpoint,
}

impl Sso {
    /// 从已通过校验的配置构造，并记住授权源文件的位置。
    ///
    /// `server_toml` 就是这份配置的来源文件；名单之后每次都从它重读。
    pub fn new_with_source(config: &SsoConfig, server_toml: &std::path::Path) -> Result<Self> {
        let sso = Sso::new(config)?;
        Ok(Sso { roster: Arc::new(RosterSource::new(server_toml)), ..sso })
    }

    /// 取当前名单。**每个受保护请求调一次。**
    ///
    /// 失败关闭：文件缺失、损坏或非法时返回 `Err`，调用方必须让请求失败，
    /// 不得退回上一份好的名单（§4.9）。
    pub fn roster(&self) -> Result<Arc<Roster>> {
        self.roster.load()
    }

    /// 从已通过校验的配置构造。URL 在这里一次性拆开——
    /// 让一个写错的端点在**启动时**失败，而不是在第一个人点登录时。
    ///
    /// 这个构造出来的句柄，名单源指向的是一个**不存在**的路径，
    /// 因此 [`Self::roster`] 必然失败关闭。生产路径一律用
    /// [`Self::new_with_source`]；这一个留给只用到 URL 拼接的用例。
    pub fn new(config: &SsoConfig) -> Result<Self> {
        let check_token = Endpoint::parse(&config.check_token_url)
            .map_err(|e| Error::invalid_config("§4.9", format!("sso.check_token_url：{e}")))?;
        // login_url 与 public_base_url 只做字符串拼接，但也在这里验一遍形状，
        // 免得一个拼不出合法跳转的配置活到运行期。
        Endpoint::parse(&config.login_url)
            .map_err(|e| Error::invalid_config("§4.9", format!("sso.login_url：{e}")))?;
        Endpoint::parse(&config.public_base_url)
            .map_err(|e| Error::invalid_config("§4.9", format!("sso.public_base_url：{e}")))?;
        Ok(Sso {
            config: Arc::new(config.clone()),
            roster: Arc::new(RosterSource::new("/nonexistent/proxy-manager-server.toml")),
            check_token,
        })
    }

    pub fn check_token_endpoint(&self) -> &Endpoint {
        &self.check_token
    }

    /// 拼出要把用户送去的那个地址。
    ///
    /// `url` 参数**整体 URL 编码**：回调地址自己带着 `?state=`，
    /// 不编码的话那个 `?` 会把上游的参数结构劈开（接入说明第 5 节）。
    pub fn login_location(&self, state: &str) -> String {
        let callback = format!("{}?state={}", self.config.callback_url(), percent_encode(state));
        let separator = if self.config.login_url.contains('?') { '&' } else { '?' };
        format!("{}{separator}url={}", self.config.login_url, percent_encode(&callback))
    }

    pub fn state_ttl_secs(&self) -> u32 {
        self.config.state_ttl_secs
    }

    pub fn request_timeout(&self) -> std::time::Duration {
        std::time::Duration::from_secs(self.config.request_timeout_secs as u64)
    }

    pub fn session_ttl(&self) -> time::Duration {
        time::Duration::seconds(self.config.session_ttl_secs as i64)
    }
}

fn percent_encode(value: &str) -> String {
    let mut out = String::with_capacity(value.len() * 3);
    for byte in value.bytes() {
        if byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'.' | b'_' | b'~') {
            out.push(byte as char);
        } else {
            out.push_str(&format!("%{byte:02X}"));
        }
    }
    out
}

/// `state` 签名用的 key。与会话证明那把**分开**：
/// 一把 key 用在两处，就意味着其中一处的分析结论不能独立成立。
pub async fn state_key(store: &Store) -> Result<proxy_manager_wire::sign::HmacKey> {
    session::server_secret(store, STATE_SECRET_NAME).await
}

/// 启动时先把两把服务端密钥生成好。
///
/// 不这么做，第一次访问回调时 `state_key` 会**在校验 state 之前**往
/// `server_secrets` 插一行——而 §4.9 加固 3 的措辞是「**无任何写入地**校验」，
/// 「匿名伪造的回调不创建会话、不写数据库」。
/// 影响有限（整个进程生命周期只有一次，且内容与请求无关），
/// 但那条纪律的价值恰恰在于**没有例外**：一旦允许「这一次写入无所谓」，
/// 下一个人就会照着加第二处。
pub async fn warm_secrets(store: &Store) -> Result<()> {
    state_key(store).await?;
    session::warm_proof_key(store).await
}

/// 一次成功登录的产物。
#[derive(Debug)]
pub struct LoggedIn {
    pub user_id: i64,
    pub login_name: String,
    pub role: Role,
    pub quota_group: String,
    /// 档位与上一次不同时为 `Some(旧档位)`——调用方据此走审计路径同步。
    pub quota_group_changed_from: Option<String>,
    /// 本次登录后这个人持有的计费槽位。`None` 表示领取失败（见 `complete_login`），
    /// 那是一次**容量**问题，登录本身仍然是成功的。
    pub claim: Option<crate::identity::Claim>,
    pub session: IssuedSession,
}

/// 建号/更新 + 建会话。
///
/// **建号是幂等的**，幂等键是 `login_name`（泡游账号名，大小写敏感）。
/// 重复登录不重复建号、不重置任何用量。
///
/// **首次登录自动领取一个计费槽位**（README §4.9、D11）。
///
/// 这里曾经刻意不做，理由是「开通是一次要留操作者、要落审计、可能因池空而
/// 失败的动作」。那个理由站不住：三件事都成立，但没有一件需要一个**人**来
/// 按按钮——操作者记成 `sso:<账号>`，审计照样落，池空照样是错误。
/// 而把它拆成单独一步的代价很实在：每个新人都要等管理员跑一条命令，
/// 而在他跑之前，那个人登录进来看到的是一个什么都用不了的控制台。
///
/// **领取失败不让登录失败。** 池空、还没结算出槽位、或者节点还没就绪，
/// 都只记一条告警——登录本身是成功的，人进得来、看得见自己的状态，
/// 只是还没有身份。反过来（领取失败就拒绝登录）会把一个可恢复的容量问题
/// 变成一次彻底进不去。
pub async fn complete_login(
    store: &Store,
    roster: &Roster,
    identity: &SsoIdentity,
    session_ttl: time::Duration,
) -> Result<LoggedIn> {
    let grade = roster.grade(&identity.account, identity.feishu_fs_id.as_deref());
    let now = bucket::to_rfc3339(OffsetDateTime::now_utc());

    let mut txn = store.begin_immediate().await?;
    let existing = sqlx::query(
        "SELECT user_id, quota_group, status, auth_provider FROM users WHERE login_name = ?",
    )
    .bind(&identity.account)
    .fetch_optional(txn.conn())
    .await?;

    let (user_id, previous_group) = match existing {
        Some(row) => {
            let user_id: i64 = row.get(0);
            let previous: String = row.get(1);
            let status: String = row.get(2);
            let owner: Option<String> = row.get(3);
            // 停用的账号**不得**靠重新登录复活。停用是管理员的显式动作，
            // 而登录路径没有任何东西能判断那个动作是不是该撤销。
            if status != "active" {
                txn.rollback().await?;
                return Err(Error::Quota(format!("账号 {} 已停用", identity.account)));
            }
            // **不得吞掉 break-glass 账号。**
            //
            // `login_name` 是全表唯一的，而上游账号名是别人命名空间里的普通
            // 字符串——一个在泡游侧叫 `break-glass` 的账号，登录一次就会把本地
            // 应急管理员那一行改写成 `auth_provider = 'paoyou'`、`role` 改成名单
            // 判定的结果，并拿到指向**那个 user_id** 的会话：既继承了它名下已
            // 开通的全部节点身份，又在没有任何审计记录的情况下把唯一的逃生通道
            // 摘掉（`resolve` 对 `local` 会话读的正是 `users.role` 这一列）。
            if owner.as_deref() == Some("local") {
                txn.rollback().await?;
                return Err(Error::Quota(format!(
                    "{} 是本地应急账号，不接受 SSO 登录：请改用另一个上游账号名",
                    identity.account
                )));
            }
            sqlx::query(
                "UPDATE users SET display_name = ?, role = ?, auth_provider = ?, \
                 feishu_fs_id = coalesce(?, feishu_fs_id), updated_at = ? \
                 WHERE user_id = ?",
            )
            .bind(&identity.display_name)
            .bind(grade.role.as_str())
            .bind(PROVIDER)
            // **`coalesce` 不是可有可无的。** 接入说明写明「账号未关联用户或未设置
            // 飞书标识时返回空字符串」，而名单可以按 `fs:<标识>` 匹配管理员。
            // 无条件覆盖意味着上游某一次返回空值就把这个人的匹配键抹掉——
            // 下一个请求他就不再是管理员了，而且没有任何东西会报错。
            .bind(identity.feishu_fs_id.as_deref())
            .bind(&now)
            .bind(user_id)
            .execute(txn.conn())
            .await
            .map_err(|error| duplicate_fs_id(error, identity))?;
            (user_id, Some(previous))
        }
        None => {
            // 首次登录：按名单落档。**不走审计路径**——建号时没有「原值」，
            // 审计要记的是「谁把谁从哪一档改到哪一档」，一次建号里那三样都不存在。
            let row = sqlx::query(
                "INSERT INTO users(login_name, display_name, role, quota_group, status, \
                 auth_provider, feishu_fs_id, created_at, updated_at) \
                 VALUES (?, ?, ?, ?, 'active', ?, ?, ?, ?) RETURNING user_id",
            )
            .bind(&identity.account)
            .bind(&identity.display_name)
            .bind(grade.role.as_str())
            .bind(&grade.quota_group)
            .bind(PROVIDER)
            .bind(identity.feishu_fs_id.as_deref())
            .bind(&now)
            .bind(&now)
            .fetch_one(txn.conn())
            .await
            .map_err(|error| duplicate_fs_id(error, identity))?;
            (row.get::<i64, _>(0), None)
        }
    };
    txn.commit().await?;

    // 档位变化留给调用方走审计路径：改额度是一次影响他人的操作（D21），
    // 它要留操作者、要预览确认、要生成逐节点下发任务，
    // 那一整套不该藏在一个 INSERT 后面。
    let quota_group_changed_from = previous_group.filter(|previous| *previous != grade.quota_group);

    // 领取放在会话之外、且**不阻断登录**：它是一次容量操作，
    // 而容量问题不该表现为「登不进去」。
    //
    // 操作者记成 `sso:<账号>`，让审计上看得出这不是管理员点的按钮，
    // 而是这个人自己第一次登录触发的。
    let claim =
        match crate::identity::claim_for_user(store, user_id, &format!("sso:{}", identity.account))
            .await
        {
            Ok(claim) => Some(claim),
            Err(error) => {
                tracing::error!(
                    login_name = %identity.account, %error,
                    "自动领取计费槽位失败：登录照常成功，但这个人还没有身份可用（P2）"
                );
                None
            }
        };

    let session = session::create(store, user_id, PROVIDER, session_ttl).await?;
    Ok(LoggedIn {
        user_id,
        login_name: identity.account.clone(),
        role: grade.role,
        quota_group: grade.quota_group,
        quota_group_changed_from,
        claim,
        session,
    })
}

/// 把唯一索引冲突翻译成一句人话。
///
/// `idx_users_feishu_fs_id` 是部分唯一索引：两个人拿到同一个飞书标识时，
/// 第二个人的登录会在这里失败。接入说明声称该标识唯一，所以这**不该**发生——
/// 但它真发生时，原样往上抛只会得到一个 `UNIQUE constraint failed`，
/// 被调用方收敛成「内部错误」，而那个人从此再也登不进来，没人知道为什么。
fn duplicate_fs_id(error: sqlx::Error, identity: &SsoIdentity) -> Error {
    if error.to_string().contains("feishu_fs_id") {
        return Error::Quota(format!(
            "账号 {} 的飞书标识与库里另一个用户重复：上游声称该标识唯一，\
             出现重复说明上游数据有问题或有人改过库。处置前这个人无法登录（P1）",
            identity.account
        ));
    }
    Error::from(error)
}

/// 把库里的展示缓存与名单对齐，返回需要改档的人。
///
/// 供 `proxy-manager sso reconcile` 用：名单改了之后，已登录过的人的
/// `users.role` / `users.quota_group` 仍是上次登录时的值。
/// **授权不受影响**（那是每请求现解析的），受影响的只有控制台上显示的那一列。
pub async fn plan_reconcile(store: &Store, roster: &Roster) -> Result<Vec<Rebind>> {
    // **排除 break-glass。** 它不在名单里（名单那条路走不通正是它存在的场景），
    // 所以 `roster.grade` 对它一律返回 `User`——照单对齐会把最后一个管理员
    // 降成普通用户，而 `resolve` 对 `local` 会话读的正是这一列。
    // 那等于用一条例行的运维命令拆掉逃生通道。
    let rows = sqlx::query(
        "SELECT user_id, login_name, role, quota_group, feishu_fs_id FROM users \
         WHERE status = 'active' AND (auth_provider IS NULL OR auth_provider != 'local') \
         ORDER BY login_name",
    )
    .fetch_all(store.readers())
    .await?;
    let mut out = Vec::new();
    for row in rows {
        let login_name: String = row.get(1);
        let fs_id: Option<String> = row.get(4);
        let Grade { role, quota_group } = roster.grade(&login_name, fs_id.as_deref());
        let current_role: String = row.get(2);
        let current_group: String = row.get(3);
        if current_role != role.as_str() || current_group != quota_group {
            out.push(Rebind {
                user_id: row.get(0),
                login_name,
                from_role: current_role,
                to_role: role,
                from_group: current_group,
                to_group: quota_group,
            });
        }
    }
    Ok(out)
}

#[derive(Debug, Clone)]
pub struct Rebind {
    pub user_id: i64,
    pub login_name: String,
    pub from_role: String,
    pub to_role: Role,
    pub from_group: String,
    pub to_group: String,
}

impl Rebind {
    pub fn role_changed(&self) -> bool {
        self.from_role != self.to_role.as_str()
    }

    pub fn group_changed(&self) -> bool {
        self.from_group != self.to_group
    }
}

/// 只同步角色那一列（展示缓存）。档位要走审计路径，不在这里改。
pub async fn sync_role(store: &Store, user_id: i64, role: Role) -> Result<()> {
    let mut txn = store.begin_immediate().await?;
    sqlx::query("UPDATE users SET role = ?, updated_at = ? WHERE user_id = ?")
        .bind(role.as_str())
        .bind(bucket::to_rfc3339(OffsetDateTime::now_utc()))
        .bind(user_id)
        .execute(txn.conn())
        .await?;
    txn.commit().await
}
