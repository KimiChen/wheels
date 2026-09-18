//! 成员名单：**授权的事实来源**（README §4.9、D27）。
//!
//! 名单来自 `server.toml` 的 `[sso]` 段，在启动时读进内存。
//! 每个受保护请求都对它求值一次角色——**不读 `users.role`**。
//!
//! 为什么不缓存结论、也不信库里那一列：撤权是这条链路上最不能等的一件事。
//! 任何形式的库内角色缓存，效果都等于「把人从名单里删掉之后，他还能一直用到
//! 下次登录」。同一套纪律在生产实现里写得更直白：**数据库与会话里的角色缓存
//! 从不决定授权**。
//!
//! 名单能回答的和不能回答的要分清：
//!
//! - **能**：这个账号现在是不是管理员、落哪一档额度。改名单下一个请求即生效。
//! - **不能**：这个账号在泡游那边是不是已经被停用。泡游没有「按账号查状态」
//!   的接口（接入说明只有一次性的 check-token），所以那一半的生效上界是
//!   `sso.session_ttl_secs`。这是 D27 明确记下来的代价，不是这里漏了一步。

use std::collections::BTreeMap;

use crate::api::session::Role;
use crate::config::{SsoConfig, DEFAULT_QUOTA_GROUP};

/// 名单对某个账号的求值结果。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Grade {
    pub role: Role,
    pub quota_group: String,
}

/// 名单条目按飞书标识匹配时的前缀。
///
/// 两种键都支持不是为了灵活：接入说明把 `wxwork_id` 声明为**唯一**，
/// 而账号名在上游改名之后会变。「第一个管理员登不进来」是这条路径上
/// 唯一不可自愈的故障——它发生时没有任何人能进控制台去改。
pub const FS_PREFIX: &str = "fs:";

/// 解析好的名单。启动时构造一次，之后只读。
#[derive(Debug, Clone, Default)]
pub struct Roster {
    admins_by_account: std::collections::BTreeSet<String>,
    admins_by_fs: std::collections::BTreeSet<String>,
    group_by_account: BTreeMap<String, String>,
    group_by_fs: BTreeMap<String, String>,
}

impl Roster {
    /// 从已通过 [`SsoConfig::validate`] 的配置构造。
    pub fn from_config(config: &SsoConfig) -> Self {
        let mut roster = Roster::default();
        for entry in &config.admins {
            match entry.strip_prefix(FS_PREFIX) {
                Some(fs) => roster.admins_by_fs.insert(fs.to_string()),
                None => roster.admins_by_account.insert(entry.clone()),
            };
        }
        for (group, members) in &config.quota_groups {
            for entry in members {
                match entry.strip_prefix(FS_PREFIX) {
                    Some(fs) => roster.group_by_fs.insert(fs.to_string(), group.clone()),
                    None => roster.group_by_account.insert(entry.clone(), group.clone()),
                };
            }
        }
        roster
    }

    /// 没有 SSO 时的空名单：**所有人都是普通用户**。
    ///
    /// 不是「所有人都是管理员」——认证代码的失败模式是沉默地放行，
    /// 而一个空名单最容易被误当成「还没配，先都放行」。
    pub fn empty() -> Self {
        Roster::default()
    }

    /// 求值。`account` 与 `feishu_fs_id` 都参与匹配，**大小写敏感**（§4.9）。
    ///
    /// 名单里没有的人是普通用户 + `normal` 档，而不是被拒绝：
    /// 通过了泡游认证的账号默认成为普通用户（§4.9）。
    pub fn grade(&self, account: &str, feishu_fs_id: Option<&str>) -> Grade {
        let is_admin = self.admins_by_account.contains(account)
            || feishu_fs_id.is_some_and(|fs| self.admins_by_fs.contains(fs));
        // 账号名优先于飞书标识：两边都写了的时候，写得更具体的那条说了算。
        let quota_group = self
            .group_by_account
            .get(account)
            .or_else(|| feishu_fs_id.and_then(|fs| self.group_by_fs.get(fs)))
            .cloned()
            .unwrap_or_else(|| DEFAULT_QUOTA_GROUP.to_string());
        Grade { role: if is_admin { Role::Admin } else { Role::User }, quota_group }
    }

    /// 名单是不是空的。空名单意味着**没有任何管理员**，启动时要点名一次。
    pub fn is_empty(&self) -> bool {
        self.admins_by_account.is_empty()
            && self.admins_by_fs.is_empty()
            && self.group_by_account.is_empty()
            && self.group_by_fs.is_empty()
    }

    pub fn admin_count(&self) -> usize {
        self.admins_by_account.len() + self.admins_by_fs.len()
    }
}

/// 名单的**热源**：按 mtime + inode + 大小缓存，变了就重读。
///
/// 没有它，「改名单下一个请求即生效」这句话是假的——`Roster` 在启动时构造一次，
/// 之后无论怎么改配置文件都不会变，撤权要重启进程。README §4.9 明确要求
/// 「本地授权规则变更在下一个受保护请求生效」，并给了缓存口径：
/// 「授权规则小文件可按 mtime + inode 缓存内容」。
///
/// **读取做 TOCTOU 硬化**（§4.9）：`metadata` 拿 dev/inode/大小 → 读 →
/// 复验 dev/inode/大小/mtime 是否仍与读前一致。不一致说明文件在读的过程中
/// 被换掉了，那时宁可判失败也不能用半份内容去决定谁是管理员。
///
/// **失败关闭。** 文件缺失、读不动、TOML 非法、校验不过——一律让受保护请求失败，
/// 而不是退回上一份好的名单。§4.9 的原话是「授权配置缺失、损坏或非法 →
/// fail closed，整个认证请求失败」。退回旧名单看起来更友好，但它的含义是
/// 「有人把某人从名单里删掉、同时手滑写坏了文件，于是那个人继续有权限」。
pub struct RosterSource {
    path: std::path::PathBuf,
    cached: std::sync::RwLock<Option<Cached>>,
}

struct Cached {
    stamp: Stamp,
    roster: std::sync::Arc<Roster>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
struct Stamp {
    dev: u64,
    inode: u64,
    len: u64,
    mtime: Option<std::time::SystemTime>,
}

impl Stamp {
    fn of(meta: &std::fs::Metadata) -> Self {
        use std::os::unix::fs::MetadataExt;
        Stamp { dev: meta.dev(), inode: meta.ino(), len: meta.len(), mtime: meta.modified().ok() }
    }
}

/// 单个授权源文件的大小上限。名单是人手维护的小文件；
/// 一个几十 MB 的「配置」只可能是别的东西被放错了位置。
const MAX_ROSTER_BYTES: u64 = 1024 * 1024;

impl std::fmt::Debug for RosterSource {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("RosterSource").field("path", &self.path).finish_non_exhaustive()
    }
}

impl RosterSource {
    pub fn new(path: impl Into<std::path::PathBuf>) -> Self {
        RosterSource { path: path.into(), cached: std::sync::RwLock::new(None) }
    }

    /// 取当前名单。文件没变就用缓存，变了就重读。
    pub fn load(&self) -> crate::Result<std::sync::Arc<Roster>> {
        let stamp = self.stat()?;
        if let Ok(guard) = self.cached.read() {
            if let Some(cached) = guard.as_ref() {
                if cached.stamp == stamp {
                    return Ok(cached.roster.clone());
                }
            }
        }
        let (roster, verified) = self.read_verified(stamp)?;
        let roster = std::sync::Arc::new(roster);
        if let Ok(mut guard) = self.cached.write() {
            *guard = Some(Cached { stamp: verified, roster: roster.clone() });
        }
        tracing::info!(
            path = %self.path.display(),
            admins = roster.admin_count(),
            "成员名单已重新载入"
        );
        Ok(roster)
    }

    fn stat(&self) -> crate::Result<Stamp> {
        // `metadata` 而不是 `symlink_metadata`：配置目录里用软链是常见部署方式
        // （旧站就是 `current -> releases/xxx`）。真正要挡的是「读到一半被换掉」，
        // 那由下面的复验负责。
        let meta = std::fs::metadata(&self.path)
            .map_err(|source| crate::Error::ReadFile { path: self.path.clone(), source })?;
        if !meta.is_file() {
            return Err(crate::Error::invalid_config(
                "§4.9",
                format!("{} 不是普通文件：授权源必须是文件", self.path.display()),
            ));
        }
        if meta.len() > MAX_ROSTER_BYTES {
            return Err(crate::Error::invalid_config(
                "§4.9",
                format!(
                    "{} 超过 {MAX_ROSTER_BYTES} 字节：授权源应当是人手维护的小文件",
                    self.path.display()
                ),
            ));
        }
        Ok(Stamp::of(&meta))
    }

    fn read_verified(&self, before: Stamp) -> crate::Result<(Roster, Stamp)> {
        let text = std::fs::read_to_string(&self.path)
            .map_err(|source| crate::Error::ReadFile { path: self.path.clone(), source })?;
        // 读后复验：dev/inode/大小/mtime 任一变了，说明文件在读的过程中被换掉。
        let after = self.stat()?;
        if after != before || text.len() as u64 != after.len {
            return Err(crate::Error::invalid_config(
                "§4.9",
                format!("{} 在读取过程中被改动：本次授权判定作废", self.path.display()),
            ));
        }
        let server = crate::config::ServerConfig::from_str(&text, &self.path)?;
        let Some(sso) = &server.sso else {
            return Err(crate::Error::invalid_config(
                "§4.9",
                format!(
                    "{} 里没有 [sso] 段了：名单消失不等于所有人都是普通用户",
                    self.path.display()
                ),
            ));
        };
        sso.validate()?;
        Ok((Roster::from_config(sso), after))
    }
}
