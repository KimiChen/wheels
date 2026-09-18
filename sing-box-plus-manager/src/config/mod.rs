//! 配置解析（M0 交付物）。
//!
//! 两份文件：`server.toml`（主控自身）与 `nodes.toml`（节点清单）。
//! 解析纪律与节点侧配额端点一致——**对未知字段失败关闭**。控制面能改变转发行为，
//! 宽容解析等于静默降级：一个拼错的键被忽略之后，运维看到的是「配置改了但没生效」。
//!
//! 落地的约束：C8（首快照策略必须显式）、C12（受限节点必须 deny）、C18（两个 socket 不许合并）、
//! C3（字节量走十进制字符串，不经 TOML 有符号整数）。

mod nodes;
mod server;
mod sso;

pub use nodes::{FirstSnapshotPolicy, NodeConfig, NodeQuotaControl, QuotaAction};
pub use server::{
    AlertsConfig, CollectConfig, ImConfig, ListenConfig, QuotaConfig, ServerConfig, StorageConfig,
};
pub use sso::{SsoConfig, DEFAULT_QUOTA_GROUP, QUOTA_GROUPS};

use std::path::Path;

use crate::error::{Error, Result};

/// 解析完成并已通过全部交叉校验的配置。
#[derive(Debug, Clone)]
pub struct Config {
    pub server: ServerConfig,
    pub nodes: Vec<NodeConfig>,
}

impl Config {
    /// 从一个配置目录加载 `server.toml` 与 `nodes.toml`。
    pub fn load_dir(dir: &Path) -> Result<Self> {
        Self::load_files(&dir.join("server.toml"), &dir.join("nodes.toml"))
    }

    pub fn load_files(server_path: &Path, nodes_path: &Path) -> Result<Self> {
        let server = ServerConfig::load(server_path)?;
        let nodes = nodes::load(nodes_path)?;
        let config = Config { server, nodes };
        config.validate()?;
        Ok(config)
    }

    /// 只在内存中解析，供测试与 `--check` 使用。
    pub fn from_str(server_toml: &str, nodes_toml: &str) -> Result<Self> {
        let server = ServerConfig::from_str(server_toml, Path::new("<server.toml>"))?;
        let nodes = nodes::from_str(nodes_toml, Path::new("<nodes.toml>"))?;
        let config = Config { server, nodes };
        config.validate()?;
        Ok(config)
    }

    /// 跨文件的交叉校验。单文件内部的校验在各自的 `validate` 里。
    fn validate(&self) -> Result<()> {
        self.server.validate()?;
        if self.nodes.is_empty() {
            return Err(Error::invalid_config("§5", "nodes.toml 里没有任何 [[node]]"));
        }
        let mut seen = std::collections::BTreeSet::new();
        for node in &self.nodes {
            node.validate()?;
            if !seen.insert(node.node_id.as_str()) {
                return Err(Error::invalid_config(
                    "§4.5",
                    format!(
                        "node_id 重复：{}。node_id 是配额 PUT 的逐字节比对键，重复会让两个节点互相 409",
                        node.node_id
                    ),
                ));
            }
        }
        Ok(())
    }
}

/// 节点侧 `validateToken` 的同构实现（`internal/userstats/options.go`）。
///
/// 非空、不超过 128 字节、每个字节落在 `0x21..=0x7e`。主控这一侧必须用同一条规则：
/// `node_id` / `inbound_tag` / `name` 在配额 PUT 里会被节点逐字节比对，
/// 一个在主控侧合法、在节点侧非法的名字，表现是配额请求恒 400 而不是配置报错。
pub(crate) fn validate_token(field: &str, value: &str) -> Result<()> {
    if value.is_empty() {
        return Err(Error::invalid_config("§4.9", format!("{field} 不能为空")));
    }
    if value.len() > 128 {
        return Err(Error::invalid_config(
            "§4.9",
            format!("{field} 超过 128 字节：{}", value.len()),
        ));
    }
    for (offset, byte) in value.bytes().enumerate() {
        if byte <= 0x20 || byte >= 0x7f {
            return Err(Error::invalid_config(
                "§4.9",
                format!("{field} 含非 ASCII 可显示字符（偏移 {offset}）"),
            ));
        }
    }
    Ok(())
}

// 这里曾经有一个 parse_u64_bytes，供配置里的 quota.monthly_bytes 用。
// 分档之后（定案第三条）配置里不再有任何字节量字段，它随之没有调用方。
// C3「字节量走十进制字符串」这条纪律仍然成立，只是载体换成了 API 的
// parse_bytes 与 quota_groups 的定宽文本列，用例在 tests/m4/api.rs。
