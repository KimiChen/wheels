use anyhow::{bail, Context, Result};
use chrono::{DateTime, FixedOffset};
use serde::Deserialize;
use std::{
    fs,
    io::Write,
    net::SocketAddr,
    path::{Path, PathBuf},
};

const EXAMPLE_TOKENS: &[&str] = &[
    "change-me",
    "example",
    "example-token",
    "please-change-me",
    "replace-with-a-long-random-token",
];

#[derive(Clone, Debug, Deserialize)]
pub struct Config {
    #[serde(default = "default_listen_addr")]
    pub listen_addr: SocketAddr,
    #[serde(default = "default_tls_cert_path")]
    pub tls_cert_path: PathBuf,
    #[serde(default = "default_tls_key_path")]
    pub tls_key_path: PathBuf,
    #[serde(default = "default_tls_auto_restart")]
    pub tls_auto_restart: bool,
    #[serde(default = "default_tls_watch_interval_secs")]
    pub tls_watch_interval_secs: u64,
    #[serde(default = "default_tls_restart_settle_secs")]
    pub tls_restart_settle_secs: u64,
    pub auth_token: String,
    #[serde(default = "default_interfaces")]
    pub interfaces: Vec<String>,
    #[serde(default = "default_node_id")]
    pub node_id: String,
    pub quota_bytes: u64,
    #[serde(default)]
    pub billing_mode: BillingMode,
    pub cycle_anchor: DateTime<FixedOffset>,
    #[serde(default = "default_cycle_months")]
    pub cycle_months: u32,
    #[serde(default = "default_state_path")]
    pub state_path: PathBuf,
}

#[derive(Copy, Clone, Debug, Deserialize, Eq, PartialEq)]
#[serde(rename_all = "lowercase")]
pub enum BillingMode {
    Rx,
    Tx,
    Total,
    Max,
}

impl Default for BillingMode {
    fn default() -> Self {
        Self::Total
    }
}

impl Config {
    pub fn load(path: &str) -> Result<Self> {
        let content = fs::read_to_string(path)
            .with_context(|| format!("failed to read config file {path}"))?;
        toml::from_str(&content).with_context(|| format!("failed to parse config file {path}"))
    }

    pub fn validate(&self) -> Result<()> {
        let token = self.auth_token.trim();
        if token.is_empty() {
            bail!("auth_token must not be empty");
        }
        if EXAMPLE_TOKENS
            .iter()
            .any(|example| token.eq_ignore_ascii_case(example))
        {
            bail!("auth_token must be changed from the example value");
        }
        if self.interfaces.is_empty() {
            bail!("interfaces must contain at least one network interface");
        }
        if self
            .interfaces
            .iter()
            .any(|iface| iface.trim().is_empty() || iface.contains('/') || iface.contains('\\'))
        {
            bail!("interfaces must be simple network interface names");
        }
        if self.node_id.trim().is_empty() {
            bail!("node_id must not be empty");
        }
        if self.quota_bytes == 0 {
            bail!("quota_bytes must be greater than zero");
        }
        if self.cycle_months == 0 {
            bail!("cycle_months must be greater than zero");
        }
        if self.tls_watch_interval_secs == 0 {
            bail!("tls_watch_interval_secs must be greater than zero");
        }
        self.validate_tls_paths()?;
        Ok(())
    }

    pub fn tls_enabled(&self) -> bool {
        self.tls_cert_path.exists() && self.tls_key_path.exists()
    }

    fn validate_tls_paths(&self) -> Result<()> {
        if self.tls_cert_path.as_os_str().is_empty() {
            bail!("tls_cert_path must not be empty");
        }
        if self.tls_key_path.as_os_str().is_empty() {
            bail!("tls_key_path must not be empty");
        }

        let cert_exists = self.tls_cert_path.exists();
        let key_exists = self.tls_key_path.exists();
        if cert_exists != key_exists {
            bail!(
                "tls_cert_path and tls_key_path must either both exist or both be absent: {} and {}",
                self.tls_cert_path.display(),
                self.tls_key_path.display()
            );
        }
        Ok(())
    }

    pub fn save_commented(&self, path: &Path) -> Result<()> {
        if let Some(parent) = path.parent() {
            fs::create_dir_all(parent).with_context(|| {
                format!("failed to create config directory {}", parent.display())
            })?;
        }

        let content = self.to_commented_toml();
        let tmp_path = path.with_extension("toml.tmp");
        let mut file = fs::File::create(&tmp_path)
            .with_context(|| format!("failed to create temp config file {}", tmp_path.display()))?;
        file.write_all(content.as_bytes())
            .with_context(|| format!("failed to write temp config file {}", tmp_path.display()))?;
        file.sync_all()
            .with_context(|| format!("failed to sync temp config file {}", tmp_path.display()))?;

        if cfg!(windows) && path.exists() {
            fs::remove_file(path)
                .with_context(|| format!("failed to replace config file {}", path.display()))?;
        }
        fs::rename(&tmp_path, path)
            .with_context(|| format!("failed to install config file {}", path.display()))?;
        Ok(())
    }

    pub fn to_commented_toml(&self) -> String {
        format!(
            r#"# 服务监听地址。0.0.0.0 表示监听所有公网/内网地址，9733 是默认端口。
listen_addr = {listen_addr}

# TLS 证书路径。把已有 PEM 证书放到这里，并同时提供 tls_key_path 后，服务会启用 HTTPS。
tls_cert_path = {tls_cert_path}

# TLS 私钥路径。证书和私钥两个文件必须同时存在；都不存在时服务保持 HTTP。
tls_key_path = {tls_key_path}

# 是否监控 TLS 证书/私钥文件变化。开启后，Nginx/Caddy/ip-certd 等工具更新 PEM 文件时，服务会优雅退出并交给 systemd 重启加载新证书。
tls_auto_restart = {tls_auto_restart}

# TLS 证书/私钥文件检查间隔，单位秒。值越小越快发现续期，值越大文件读取越少。
tls_watch_interval_secs = {tls_watch_interval_secs}

# 检测到证书变化后继续等待的稳定时间，单位秒。用于避开证书和私钥分步写入的短暂不一致窗口。
tls_restart_settle_secs = {tls_restart_settle_secs}

# API 鉴权 Token。必须是足够长的随机字符串；请勿公开或写入状态文件。
auth_token = {auth_token}

# 要统计的 Linux 网卡名列表。多网卡时可写成 ["eth0", "ens3"]。
interfaces = {interfaces}

# 节点标识。多台 VPS 汇总时用来区分来源，不参与鉴权。
node_id = {node_id}

# 本账期总流量额度，单位为字节。页面 JS 会显示为 K/M/G/T 两位小数。
quota_bytes = {quota_bytes}

# 计费口径，可选 total/rx/tx/max：total 表示下载+上传，rx 表示只算接收，tx 表示只算发送，max 表示取接收/发送较大值。
billing_mode = {billing_mode}

# 流量充值周期锚点，即服务商重置流量的开始时间。流量用量按这个周期计算。
cycle_anchor = {cycle_anchor}

# 流量充值周期月数。1 表示每月重置，3 表示每三个月重置。
cycle_months = {cycle_months}

# 本机状态文件路径。保存网卡上次计数、账期累计和校准偏移；不会保存 auth_token。
state_path = {state_path}
"#,
            listen_addr = toml_string(&self.listen_addr.to_string()),
            tls_cert_path = toml_string(&self.tls_cert_path.display().to_string()),
            tls_key_path = toml_string(&self.tls_key_path.display().to_string()),
            tls_auto_restart = self.tls_auto_restart,
            tls_watch_interval_secs = self.tls_watch_interval_secs,
            tls_restart_settle_secs = self.tls_restart_settle_secs,
            auth_token = toml_string(&self.auth_token),
            interfaces = toml_string_list(&self.interfaces),
            node_id = toml_string(&self.node_id),
            quota_bytes = self.quota_bytes,
            billing_mode = toml_string(self.billing_mode.as_str()),
            cycle_anchor = toml_string(&self.cycle_anchor.to_rfc3339()),
            cycle_months = self.cycle_months,
            state_path = toml_string(&self.state_path.display().to_string()),
        )
    }
}

impl BillingMode {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Rx => "rx",
            Self::Tx => "tx",
            Self::Total => "total",
            Self::Max => "max",
        }
    }
}

fn toml_string(value: &str) -> String {
    let escaped = value
        .replace('\\', "\\\\")
        .replace('"', "\\\"")
        .replace('\n', "\\n")
        .replace('\r', "\\r")
        .replace('\t', "\\t");
    format!("\"{escaped}\"")
}

fn toml_string_list(values: &[String]) -> String {
    let items = values
        .iter()
        .map(|value| toml_string(value))
        .collect::<Vec<_>>()
        .join(", ");
    format!("[{items}]")
}

fn default_listen_addr() -> SocketAddr {
    "0.0.0.0:9733"
        .parse()
        .expect("valid default listen address")
}

fn default_tls_cert_path() -> PathBuf {
    PathBuf::from("/etc/vps-trafficd/tls/fullchain.pem")
}

fn default_tls_key_path() -> PathBuf {
    PathBuf::from("/etc/vps-trafficd/tls/privkey.pem")
}

fn default_tls_auto_restart() -> bool {
    true
}

fn default_tls_watch_interval_secs() -> u64 {
    300
}

fn default_tls_restart_settle_secs() -> u64 {
    10
}

fn default_interfaces() -> Vec<String> {
    vec!["eth0".to_string()]
}

fn default_node_id() -> String {
    "vps-trafficd-01".to_string()
}

fn default_cycle_months() -> u32 {
    1
}

fn default_state_path() -> PathBuf {
    PathBuf::from("/var/lib/vps-trafficd/state.json")
}
