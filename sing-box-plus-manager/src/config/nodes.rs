//! `nodes.toml`：节点清单。
//!
//! 这份文件里有两条「缺省即启动失败」的键，它们是 M0 最值得先立住的门禁：
//!
//! - `first_snapshot`（C8）：首快照按 `baseline` 还是 `include` 必须**显式**选择。
//!   没有默认值——默认成 `baseline` 会静默吞掉节点已有的累计，默认成 `include`
//!   会把历史流量一次性记成本周期用量。两种都是能跑起来的错。
//! - 受限节点的 `startup_action`（C12）：额度是纯内存的，进程重启即丢失。
//!   `allow` 意味着重启后到下一次成功下发之间，所有人都是无限额度。

use std::path::{Path, PathBuf};

use serde::Deserialize;

use crate::config::validate_token;
use crate::error::{Error, Result};

#[derive(Debug, Clone, Copy, PartialEq, Eq, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum FirstSnapshotPolicy {
    /// 首次 delta 记 0：把节点已有的累计当作起点。
    Baseline,
    /// 首次 delta 等于当前累计值。选它必须能证明不存在其他数据源的重复入账。
    Include,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum QuotaAction {
    Allow,
    Deny,
}

#[derive(Debug, Clone, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct NodeConfig {
    pub node_id: String,
    pub address: String,
    /// 没有 `#[serde(default)]`——缺省即解析失败，这就是 C8 的门禁。
    pub first_snapshot: FirstSnapshotPolicy,
    pub snapshot_socket: PathBuf,
    pub quota_socket: PathBuf,
    pub agent_spki_sha256: String,
    /// 主控侧部署材料目录（离线签发出来的 `deployment/controller/`）。
    ///
    /// 必填：一个连不上的节点条目是配置错误，不是「暂时没配」。
    /// 目录里的 `authorized-servers.pem` 决定信任哪一张 leaf，
    /// 它与上面那个带外核对的指纹必须指向同一张证书。
    pub materials_dir: PathBuf,
    #[serde(default)]
    pub quota_control: Option<NodeQuotaControl>,
}

#[derive(Debug, Clone, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct NodeQuotaControl {
    pub enabled: bool,
    pub startup_action: QuotaAction,
    pub stale_after_secs: u32,
    pub stale_action: QuotaAction,
}

#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
struct NodesFile {
    #[serde(default, rename = "node")]
    nodes: Vec<NodeConfig>,
}

pub(super) fn load(path: &Path) -> Result<Vec<NodeConfig>> {
    let text = std::fs::read_to_string(path)
        .map_err(|source| Error::ReadFile { path: path.to_path_buf(), source })?;
    from_str(&text, path)
}

pub(super) fn from_str(text: &str, path: &Path) -> Result<Vec<NodeConfig>> {
    let file: NodesFile = toml::from_str(text)
        .map_err(|source| Error::ParseToml { path: path.to_path_buf(), source })?;
    Ok(file.nodes)
}

impl NodeConfig {
    pub fn validate(&self) -> Result<()> {
        // node_id 会被节点逐字节比对，用节点侧同一条 token 规则校验。
        validate_token("node.node_id", &self.node_id)?;
        if self.address.is_empty() {
            return Err(Error::invalid_config("§4.2", "node.address 不能为空"));
        }

        // C18：两个 socket 权限档次不同，不许合并。配到同一个路径上，
        // 等于把只读的快照权限和能改变转发行为的配额权限放进同一个档次。
        if self.snapshot_socket == self.quota_socket {
            return Err(Error::invalid_config(
                "C18",
                format!(
                    "节点 {} 的 snapshot_socket 与 quota_socket 相同（{}）：两个 socket 权限档次不同，不许合并",
                    self.node_id,
                    self.snapshot_socket.display()
                ),
            ));
        }
        for (field, path) in
            [("snapshot_socket", &self.snapshot_socket), ("quota_socket", &self.quota_socket)]
        {
            if !path.is_absolute() {
                return Err(Error::invalid_config(
                    "C18",
                    format!("节点 {} 的 {field} 必须是绝对路径：{}", self.node_id, path.display()),
                ));
            }
        }

        if !self.materials_dir.is_absolute() {
            return Err(Error::invalid_config(
                "§4.2",
                format!(
                    "节点 {} 的 materials_dir 必须是绝对路径：{}",
                    self.node_id,
                    self.materials_dir.display()
                ),
            ));
        }

        if self.agent_spki_sha256.len() != 64
            || !self.agent_spki_sha256.bytes().all(|b| b.is_ascii_hexdigit())
        {
            return Err(Error::invalid_config(
                "§4.2",
                format!(
                    "节点 {} 的 agent_spki_sha256 必须是 64 位十六进制：在线信任的是那一张带外核对过的精确 leaf",
                    self.node_id
                ),
            ));
        }

        if let Some(quota) = &self.quota_control {
            quota.validate(&self.node_id)?;
        }
        Ok(())
    }

    /// 拨号地址里的主机名部分，用作 TLS 的 `ServerName`。
    ///
    /// 精确 leaf 授信不看 SAN，但 rustls 仍然需要一个名字才能发起握手。
    pub fn dns_name(&self) -> &str {
        self.address.rsplit_once(':').map(|(host, _)| host).unwrap_or(&self.address)
    }

    /// 该节点是否参与配额闸断链路。
    pub fn quota_enabled(&self) -> bool {
        self.quota_control.as_ref().is_some_and(|q| q.enabled)
    }
}

impl NodeQuotaControl {
    fn validate(&self, node_id: &str) -> Result<()> {
        if !self.enabled {
            return Ok(());
        }
        // C12：受限节点必须 startup_action = deny。
        if self.startup_action != QuotaAction::Deny {
            return Err(Error::invalid_config(
                "C12",
                format!(
                    "节点 {node_id} 启用了配额但 startup_action 不是 deny：额度是纯内存的，\
                     进程重启即丢失，allow 会让重启到下一次成功下发之间所有人都是无限额度"
                ),
            ));
        }
        // C33 / README §4.5：stale_after 与 stale_action 必须显式且为 deny。
        if self.stale_after_secs == 0 {
            return Err(Error::invalid_config(
                "C12",
                format!(
                    "节点 {node_id} 启用了配额但 stale_after_secs 为 0：过期策略必须有明确期限"
                ),
            ));
        }
        if self.stale_action != QuotaAction::Deny {
            return Err(Error::invalid_config(
                "C12",
                format!("节点 {node_id} 启用了配额但 stale_action 不是 deny"),
            ));
        }
        Ok(())
    }
}
