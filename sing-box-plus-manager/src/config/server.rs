//! `server.toml`：主控自身的配置。

use std::path::{Path, PathBuf};

use serde::Deserialize;

use crate::error::{Error, Result};

#[derive(Debug, Clone, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ServerConfig {
    pub listen: ListenConfig,
    pub storage: StorageConfig,
    pub collect: CollectConfig,
    pub quota: QuotaConfig,
    #[serde(default)]
    pub alerts: AlertsConfig,
    #[serde(default)]
    pub im: ImConfig,
    /// 泡游 SSO 登录（M5）。**整段缺省即不启用**——那时控制台只剩 break-glass。
    ///
    /// 写了就必须写对：下面的 validate 会对它逐项下钻，不存在「反正没开所以不校验」
    /// 的中间态。一份写错了但暂时没生效的配置，会在某天有人把它接上的那一刻才炸，
    /// 而那时写配置的人已经不在现场了。
    #[serde(default)]
    pub sso: Option<crate::config::SsoConfig>,
    /// 订阅（M6）。**整段缺省即不启用**——那时 `/sub/…` 不挂载，
    /// 而 `/api/v1/me/subscription` 回 `enabled: false`，不是 404 也不是错误：
    /// 「服务端没配」是一个确定的事实，页面据此说得出话；
    /// 回错误会被读成「加载失败，刷新试试」，而刷多少次都一样。
    #[serde(default)]
    pub subscription: Option<crate::config::SubscriptionConfig>,
}

#[derive(Debug, Clone, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ListenConfig {
    /// 结构化 HTTP API 与 Web 控制台的监听地址。
    pub api: String,
}

#[derive(Debug, Clone, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct StorageConfig {
    pub path: PathBuf,
    /// `SQLITE_BUSY` 的有界退避上限（data-model.md §3.1）。
    #[serde(default = "default_busy_timeout_ms")]
    pub busy_timeout_ms: u32,
}

fn default_busy_timeout_ms() -> u32 {
    5_000
}

#[derive(Debug, Clone, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct CollectConfig {
    /// 目标采集周期。
    pub interval_secs: u32,
    /// 提频下限（README §4.5 自适应频率）。
    pub min_interval_secs: u32,
    /// 正额度请求发送前的快照新鲜度上限（README §4.5 下发纪律 3）。
    pub snapshot_max_age_secs: u32,
    /// 节点时钟偏移的容忍上限（秒）。超出即归桶回落到 `received_at` 并告警。
    ///
    /// 节点必须启用 NTP（C27），主控把**节点时钟偏移**作为一等健康项采集。
    /// 默认 120 秒不是拍的：Shadowsocks 2022 的时间戳防重放让一台慢约两分钟的节点
    /// **所有**握手失败，表现为「整节点不可用」而非「某用户不可用」。
    #[serde(default = "default_max_clock_skew_secs")]
    pub max_clock_skew_secs: i64,
}

fn default_max_clock_skew_secs() -> i64 {
    crate::ledger::bucket::DEFAULT_MAX_CLOCK_SKEW_SECS
}

#[derive(Debug, Clone, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct QuotaConfig {
    /// 日桶所用的展示时区（data-model.md §6）。改它必须完整重建日缓存。
    ///
    /// 计费周期是 UTC+8（`CYCLE_RULE_VERSION = 2`），这里要与之对齐，
    /// 否则某一天的用量会显示进「错误的月份」。
    pub display_timezone: String,

    /// 拿不到新鲜已结算 sequence 时，是否下发 C33 的全零安全表。
    ///
    /// **默认 false，这是对一条编号约束的有意偏离，不是疏漏。**
    ///
    /// C33 要求「无新鲜快照时主动把该节点全部受限身份置零」，为的是不让节点
    /// 带着旧的正额度一直跑到自己耗尽。那个推理成立的前提是额度用来强制封顶。
    /// 本系统的定位是观察（定案第四条），而且是**镜像**分配：每个节点都拿到
    /// 该用户本周期的全额剩余，没有「跑到自己耗尽」这回事——节点上那份额度
    /// 本来就等于全局剩余。
    ///
    /// 更要紧的是节点侧 `quota.go` 的判定顺序：`admit()` 先查 `exhausted()`
    /// 再查 stale，所以 `stale_action = allow` **救不了**已经被置零的身份。
    /// 一次短暂的采集失败若触发全零表，等于把唯一的生产用户硬切断，
    /// 而它恢复要等下一轮成功下发。
    ///
    /// 所以默认行为改成「拿不到新鲜序列就不推」，节点保留上一次授予的剩余额度，
    /// 并在最后一次成功下发超龄时告警。置 true 即可一行回到 C33 的原行为。
    #[serde(default)]
    pub safe_zero_on_stale: bool,
}

// 这里曾经有一个 `monthly_bytes`：D21 时代全体共用一个月度额度，配一个值。
// 分档之后（定案第三条）额度挂在 quota_groups 上，建库时按四档种下、
// 之后一律走 quota::settings 的审计路径修改，那条路径会推进 revision
// 并生成逐节点下发任务。配置里再留一个额度字段就是**设了也不生效**——
// 一个静默无操作的配置项比没有这个配置项危险。

#[derive(Debug, Clone, Default, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AlertsConfig {
    #[serde(default)]
    pub enabled: bool,
}

#[derive(Debug, Clone, Default, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ImConfig {
    #[serde(default)]
    pub enabled: bool,
}

impl ServerConfig {
    pub fn load(path: &Path) -> Result<Self> {
        let text = std::fs::read_to_string(path)
            .map_err(|source| Error::ReadFile { path: path.to_path_buf(), source })?;
        Self::from_str(&text, path)
    }

    pub fn from_str(text: &str, path: &Path) -> Result<Self> {
        toml::from_str(text).map_err(|source| Error::ParseToml { path: path.to_path_buf(), source })
    }

    pub fn validate(&self) -> Result<()> {
        if self.listen.api.is_empty() {
            return Err(Error::invalid_config("§5", "listen.api 不能为空"));
        }
        if self.storage.path.as_os_str().is_empty() {
            return Err(Error::invalid_config("D8", "storage.path 不能为空"));
        }
        if self.collect.interval_secs == 0 || self.collect.min_interval_secs == 0 {
            return Err(Error::invalid_config(
                "§4.5",
                "collect.interval_secs 与 collect.min_interval_secs 必须为正",
            ));
        }
        if self.collect.min_interval_secs > self.collect.interval_secs {
            return Err(Error::invalid_config(
                "§4.5",
                format!(
                    "collect.min_interval_secs({}) 不得大于 collect.interval_secs({})：提频只会缩短间隔",
                    self.collect.min_interval_secs, self.collect.interval_secs
                ),
            ));
        }
        if self.collect.max_clock_skew_secs <= 0 {
            return Err(Error::invalid_config(
                "C27",
                "collect.max_clock_skew_secs 必须为正：0 等于把每一份快照都判成时钟不可信",
            ));
        }
        if self.collect.snapshot_max_age_secs == 0 {
            return Err(Error::invalid_config(
                "§4.5",
                "collect.snapshot_max_age_secs 必须为正：它是正额度请求的新鲜度门禁",
            ));
        }
        // **必须显式下钻。** `ServerConfig::validate` 不会自动递归到子结构体，
        // 而 `[alerts]` / `[im]` 恰好没有任何校验，所以照抄它们的写法会得到一段
        // 「写了 validate 但从不运行」的代码。对照组是 `NodeConfig::validate`，
        // 它是显式调 `NodeQuotaControl::validate` 的。
        if let Some(sso) = &self.sso {
            sso.validate()?;
        }
        if let Some(subscription) = &self.subscription {
            subscription.validate()?;
        }
        if self.quota.display_timezone.is_empty() {
            return Err(Error::invalid_config(
                "§6",
                "quota.display_timezone 不能为空：日桶按它的日历日计算，缺省会让日缓存无法重建",
            ));
        }
        Ok(())
    }
}
