//! `server.toml`：主控自身的配置。

use std::path::{Path, PathBuf};

use serde::Deserialize;

use crate::config::parse_u64_bytes;
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
    /// 全局月度额度，十进制字符串的字节数（D21 + C3）。
    pub monthly_bytes: String,
    /// 日桶所用的展示时区（data-model.md §6）。改它必须完整重建日缓存。
    pub display_timezone: String,
}

impl QuotaConfig {
    pub fn monthly_bytes(&self) -> Result<u64> {
        parse_u64_bytes("quota.monthly_bytes", &self.monthly_bytes)
    }
}

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
        // 额度在这里就解析一次，让一个写错的字节量在**启动时**失败，
        // 而不是等到第一次下发时才炸在配额链路上。
        self.quota.monthly_bytes()?;
        if self.quota.display_timezone.is_empty() {
            return Err(Error::invalid_config(
                "§6",
                "quota.display_timezone 不能为空：日桶按它的日历日计算，缺省会让日缓存无法重建",
            ));
        }
        Ok(())
    }
}
