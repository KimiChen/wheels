//! `server.toml` 的 `[sso]` 段：泡游 SSO 登录与成员名单（README §4.9、D27）。
//!
//! **整段缺省即不启用 SSO**，没有第二个开关。一个 `enabled = false` 配上三条
//! 写好的 URL，会让「这套配置到底生不生效」变成要读两处才答得出的问题；
//! 而把整段注释掉表达同一件事，且不可能读错。
//!
//! **成员名单的事实来源是这个文件，不是数据库。** `users.role` 与
//! `users.quota_group` 只是登录时同步过去的展示缓存。这条纪律抄自同一套
//! 在生产跑着的实现，它的理由用一句话就能说清：**撤权是最不能等的那件事**，
//! 而任何形式的库内角色缓存都意味着「改了名单要等下次登录才生效」。

use std::collections::BTreeSet;

use serde::Deserialize;

use crate::error::{Error, Result};

/// 四档额度的闭集（D23）。与 `schema/01_config_dimensions.sql` 的 `quota_groups`
/// 以及 `store::schema::SEED_QUOTA_GROUPS` 是同一份取值域。
pub const QUOTA_GROUPS: &[&str] = &["normal", "advanced", "manage", "admin"];
/// 未在名单里出现的人落在这一档。
pub const DEFAULT_QUOTA_GROUP: &str = "normal";

#[derive(Debug, Clone, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct SsoConfig {
    /// 泡游 SSO 登录页。用户被 302 到这里。
    pub login_url: String,

    /// 服务端换取身份的端点。
    ///
    /// 接入说明只写了路径 `POST /api/sso/check-token`，base 是「双方约定」，
    /// 所以它必须是一个完整 URL 而不是拼出来的——拼接会让改 base 变成改代码。
    pub check_token_url: String,

    /// 主控**对外**的基址，回调地址由它拼出来。
    ///
    /// 主控自己绑回环（§5），对外域名由反代决定，所以这个值代码推不出来。
    /// 强制 `https://`：会话 cookie 带 `__Host-` 前缀，而该前缀要求 `Secure`，
    /// 在明文源上浏览器会直接丢弃——表现是「登录成功但立刻又没登录」。
    pub public_base_url: String,

    /// `state` 的有效期。
    #[serde(default = "default_state_ttl_secs")]
    pub state_ttl_secs: u32,

    /// 换取身份这一次外呼的总超时。连接超时取 `min(3, 它)`。
    #[serde(default = "default_request_timeout_secs")]
    pub request_timeout_secs: u32,

    /// 会话有效期。
    ///
    /// **它同时是上游撤权的生效上界**（D27）：泡游没有「按账号查状态」的接口，
    /// 一个人在泡游侧被停用之后，主控要到会话过期才会发现。组内调整不受此限——
    /// 那部分每请求从本文件现解析。
    #[serde(default = "default_session_ttl_secs")]
    pub session_ttl_secs: u32,

    /// 控制台管理员的名单。
    ///
    /// **与额度档位是两件事**：档位管的是流量多少，这里管的是能不能改别人的配置。
    /// 两份名单允许完全不重叠。
    #[serde(default)]
    pub admins: Vec<String>,

    /// 额度档位名单。键是档位名，值是账号列表；未列出者落 `normal`。
    #[serde(default)]
    pub quota_groups: std::collections::BTreeMap<String, Vec<String>>,
}

fn default_state_ttl_secs() -> u32 {
    300
}

fn default_request_timeout_secs() -> u32 {
    8
}

fn default_session_ttl_secs() -> u32 {
    8 * 3600
}

impl SsoConfig {
    /// 回调地址（不含 `?state=`）。
    pub fn callback_url(&self) -> String {
        format!("{}/auth/sso/callback", self.public_base_url.trim_end_matches('/'))
    }

    pub fn validate(&self) -> Result<()> {
        require_https(&self.login_url, "sso.login_url")?;
        require_https(&self.check_token_url, "sso.check_token_url")?;
        require_https(&self.public_base_url, "sso.public_base_url")?;
        if self.public_base_url.contains('?') || self.public_base_url.contains('#') {
            return Err(Error::invalid_config(
                "§4.9",
                "sso.public_base_url 不能带 query 或 fragment：回调地址由它拼接",
            ));
        }
        // 也不能带路径段。回调路由挂在**绝对路径** `/auth/sso/callback` 上，
        // 而回调地址是 `<基址>/auth/sso/callback`——基址带了前缀，
        // 泡游跳回来的地址就路由不到，表现是登录 100% 失败且没有任何诊断：
        // 主控这边连一条日志都不会有，因为请求根本没进到回调 handler。
        if self.public_base_url.trim_end_matches('/')["https://".len()..].contains('/') {
            return Err(Error::invalid_config(
                "§4.9",
                format!(
                    "sso.public_base_url 只能是协议加主机名，不能带路径段：{:?}。\
                     回调路由挂在绝对路径 {} 上",
                    self.public_base_url,
                    crate::sso::state::CALLBACK_PATH
                ),
            ));
        }
        if self.state_ttl_secs == 0 {
            return Err(Error::invalid_config("§4.9", "sso.state_ttl_secs 必须为正"));
        }
        if self.request_timeout_secs == 0 {
            return Err(Error::invalid_config("§4.9", "sso.request_timeout_secs 必须为正"));
        }
        if self.session_ttl_secs == 0 {
            return Err(Error::invalid_config("§4.9", "sso.session_ttl_secs 必须为正"));
        }
        // 档位名是闭集。写错一个档位名的后果是**那一组人静默落回 normal**——
        // 没有任何报错，只有额度对不上，而那要一个月才看得出来。
        for name in self.quota_groups.keys() {
            if !QUOTA_GROUPS.contains(&name.as_str()) {
                return Err(Error::invalid_config(
                    "D23",
                    format!(
                        "sso.quota_groups 里的 {name:?} 不是合法档位；合法值：{QUOTA_GROUPS:?}"
                    ),
                ));
            }
        }
        // 同一个人不得同时出现在两档：两份都成立时按哪一份算是没有答案的，
        // 而「按额度高的算」和「按声明顺序算」都是悄悄替使用者做决定。
        let mut seen: BTreeSet<&str> = BTreeSet::new();
        for (group, members) in &self.quota_groups {
            for member in members {
                check_identifier(member, "sso.quota_groups")?;
                if !seen.insert(member.as_str()) {
                    return Err(Error::invalid_config(
                        "§4.9",
                        format!("{member:?} 同时出现在多个额度档位（本次在 {group}）：请只留一处"),
                    ));
                }
            }
        }
        let mut admins: BTreeSet<&str> = BTreeSet::new();
        for admin in &self.admins {
            check_identifier(admin, "sso.admins")?;
            if !admins.insert(admin.as_str()) {
                return Err(Error::invalid_config("§4.9", format!("sso.admins 里 {admin:?} 重复")));
            }
        }
        Ok(())
    }
}

fn require_https(value: &str, field: &str) -> Result<()> {
    if !value.starts_with("https://") || value.len() <= "https://".len() {
        return Err(Error::invalid_config(
            "§4.9",
            format!("{field} 必须是 https:// 开头的完整 URL：token 只走 HTTPS"),
        ));
    }
    if value.bytes().any(|b| b <= 0x20 || b == 0x7f) {
        return Err(Error::invalid_config("§4.9", format!("{field} 含空白或控制字符")));
    }
    Ok(())
}

/// 名单条目的形状。
///
/// 条目可以是泡游账号名，也可以写成 `fs:<飞书标识>` 来按飞书标识匹配——
/// 接入说明把 `wxwork_id` 声明为唯一，而账号名在改名后会变。两种都支持是因为
/// 「第一个管理员登不进来」是这条路径上唯一不可自愈的故障。
fn check_identifier(value: &str, field: &str) -> Result<()> {
    if value.is_empty() {
        return Err(Error::invalid_config("§4.9", format!("{field} 里有空条目")));
    }
    if value.len() > 128 {
        return Err(Error::invalid_config("§4.9", format!("{field} 里的条目超过 128 字节")));
    }
    if value.bytes().any(|b| b <= 0x20 || b == 0x7f) {
        return Err(Error::invalid_config(
            "§4.9",
            format!(
                "{field} 里的 {value:?} 含空白或控制字符：名单是逐字节比较的，肉眼看不出这种差别"
            ),
        ));
    }
    Ok(())
}
