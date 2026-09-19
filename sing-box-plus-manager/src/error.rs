//! 主控内部错误类型。
//!
//! 对外 API 的错误对象形状固定、`code` 是稳定枚举（README §6），那一层属于 M4；
//! 这里是内部错误，只要求**归因足够具体**：每个变体都要能指出是哪条约束拒绝了输入，
//! 否则排查会被引到错误的一侧去（`docs/integration-contract.md` §4 就是这么发生的）。

use std::path::PathBuf;

pub type Result<T> = std::result::Result<T, Error>;

#[derive(Debug, thiserror::Error)]
pub enum Error {
    #[error("读取 {path} 失败：{source}")]
    ReadFile {
        path: PathBuf,
        #[source]
        source: std::io::Error,
    },

    #[error("解析 {path} 失败：{source}")]
    ParseToml {
        path: PathBuf,
        #[source]
        source: toml::de::Error,
    },

    #[error("配置不合法（{constraint}）：{detail}")]
    InvalidConfig {
        /// 被违反的约束编号，例如 `C8`、`C12`、`C18`。写编号而不是描述，
        /// 让运维能直接回计划书查它为什么存在。
        constraint: &'static str,
        detail: String,
    },

    #[error("存储层错误：{0}")]
    Store(#[from] sqlx::Error),

    #[error("数值编码错误：{0}")]
    Codec(String),

    #[error("PKI：{0}")]
    Pki(String),

    #[error("TLS 材料：{0}")]
    Tls(#[from] proxy_manager_wire::TlsError),

    #[error("agent 通路：{0}")]
    Agent(String),

    #[error("账本：{0}")]
    Ledger(String),

    #[error("配额：{0}")]
    Quota(String),

    #[error("审计：{0}")]
    Audit(String),
}

impl Error {
    pub fn invalid_config(constraint: &'static str, detail: impl Into<String>) -> Self {
        Error::InvalidConfig { constraint, detail: detail.into() }
    }
}
