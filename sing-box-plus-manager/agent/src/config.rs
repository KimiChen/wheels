//! agent 配置。
//!
//! 解析纪律与主控一致：**对未知字段失败关闭**。这个进程能改变节点的转发行为，
//! 宽容解析等于静默降级。

use std::path::PathBuf;

use serde::Deserialize;

#[derive(Debug, Clone, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AgentConfig {
    /// 必须与进程级 registry 的 `node_id` 逐字节相同。
    /// 它进 HMAC 的 canonical 编码，所以一份请求不能被搬到另一个节点上重放。
    pub node_id: String,
    /// mTLS 监听地址。
    pub listen: String,
    /// 离线签发出来的部署材料目录（`deployment/agent/`）。
    pub materials_dir: PathBuf,
    pub sockets: Sockets,
    /// 审计文件目录。只读已轮转文件（C23）。
    #[serde(default)]
    pub audit_dir: Option<PathBuf>,
}

#[derive(Debug, Clone, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Sockets {
    /// 只读档。
    pub snapshot: PathBuf,
    /// 写档——会改变转发行为。
    ///
    /// **两个 socket 权限档次不同，不许合并**（C18）。agent 需要主组 + 补充组
    /// 才能同时穿过两档权限（C28），缺一即启动失败。
    pub quota: PathBuf,
}

#[derive(Debug, thiserror::Error)]
pub enum ConfigError {
    #[error("读取 {path} 失败：{source}")]
    Read {
        path: PathBuf,
        #[source]
        source: std::io::Error,
    },
    #[error("解析 {path} 失败：{source}")]
    Parse {
        path: PathBuf,
        #[source]
        source: toml::de::Error,
    },
    #[error("配置不合法（{constraint}）：{detail}")]
    Invalid { constraint: &'static str, detail: String },
}

impl AgentConfig {
    pub fn load(path: &std::path::Path) -> Result<Self, ConfigError> {
        let text = std::fs::read_to_string(path)
            .map_err(|source| ConfigError::Read { path: path.to_path_buf(), source })?;
        let config: AgentConfig = toml::from_str(&text)
            .map_err(|source| ConfigError::Parse { path: path.to_path_buf(), source })?;
        config.validate()?;
        Ok(config)
    }

    fn validate(&self) -> Result<(), ConfigError> {
        if self.node_id.is_empty() {
            return Err(ConfigError::Invalid {
                constraint: "§4.2",
                detail: "node_id 不能为空".into(),
            });
        }
        // C18 在 agent 侧的落点：它自己就该拒绝一份把两档权限合并掉的配置。
        if self.sockets.snapshot == self.sockets.quota {
            return Err(ConfigError::Invalid {
                constraint: "C18",
                detail: format!(
                    "snapshot 与 quota 指向同一个 socket（{}）：两个 socket 权限档次不同，不许合并",
                    self.sockets.snapshot.display()
                ),
            });
        }
        Ok(())
    }
}
