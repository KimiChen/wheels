//! 配置编译器：纯函数把拓扑快照 + 解封密钥编译为 sing-box JSON；11 条 Route 校验；确定性序列化；
//! 真实 `sing-box check`。忠实移植已验证的 `_legacy/singbox.rs` 结构，适配平台 DB 拓扑模型。

pub mod canonical;
pub mod chain;
pub mod check;
pub mod entry;
pub mod node;
pub mod psk;
pub mod validate;

use std::collections::BTreeSet;

use serde_json::Value;

use crate::domain::topology::{Landing, Node, Route};
use crate::error::Result;
use crate::store::secrets::SecretBundle;

/// Agent 本机 SSM API 端口（§2.1）。
pub const SSM_PORT: i64 = 49736;
/// Agent 本机 SSM 缓存路径（部署主机上；Phase 3 可配置化）。
pub const SSM_CACHE_PATH: &str = "/var/lib/sing-box-manager/ssm-cache.json";

/// 一条 Route 的解析终端。
#[derive(Debug, Clone)]
pub enum Terminal {
    /// entry_direct：入口自身出口。
    Direct,
    /// node 出口 或 managed_node landing：经 SS-2022 中继到该 Node（其 direct 直出真实目标）。
    Node(Node),
    /// socks5 landing：外部 SOCKS5 终端出站。
    Socks5(Landing),
}

/// 一条 Route 的编译快照（hops 已按 position 解析为 Node）。
#[derive(Debug, Clone)]
pub struct RouteSnapshot {
    pub route: Route,
    pub hops: Vec<Node>,
    pub terminal: Terminal,
}

/// 一个 Entry 的编译快照（含其服务的全部 Route）。不含任何密钥。
#[derive(Debug, Clone)]
pub struct EntrySnapshot {
    pub entry: crate::domain::topology::Entry,
    pub routes: Vec<RouteSnapshot>,
}

/// 编译产物：Entry 配置 + 链中出现的各受管 Node 配置（按 node_id 升序去重）。
pub struct Compiled {
    pub entry: Value,
    pub nodes: Vec<(String, Value)>,
}

/// 编译一个 Entry 及其链中受管 Node。`identities`：route_id → 认证身份集合（Phase 2 通常为空）。
pub fn compile(
    snap: &EntrySnapshot,
    secrets: &SecretBundle,
    identities: &std::collections::HashMap<String, Vec<String>>,
) -> Result<Compiled> {
    let entry_cfg = entry::compile_entry(snap, secrets, identities)?;

    // 收集链中出现的受管 Node（hops + Node 型终端），按 id 去重升序。
    let mut node_ids: BTreeSet<&str> = BTreeSet::new();
    for rs in &snap.routes {
        for h in &rs.hops {
            node_ids.insert(&h.id);
        }
        if let Terminal::Node(n) = &rs.terminal {
            node_ids.insert(&n.id);
        }
    }
    let mut nodes = Vec::new();
    for nid in node_ids {
        let node = find_node(snap, nid).expect("node 已在快照内");
        let psk = secrets.node_psk.get(nid).ok_or_else(|| {
            crate::error::AppError::new(
                crate::error::ErrorCode::Internal,
                format!("缺 node_psk: {nid}"),
            )
        })?;
        nodes.push((nid.to_string(), node::compile_node(node, psk)));
    }
    Ok(Compiled {
        entry: entry_cfg,
        nodes,
    })
}

fn find_node<'a>(snap: &'a EntrySnapshot, node_id: &str) -> Option<&'a Node> {
    for rs in &snap.routes {
        if let Some(n) = rs.hops.iter().find(|n| n.id == node_id) {
            return Some(n);
        }
        if let Terminal::Node(n) = &rs.terminal {
            if n.id == node_id {
                return Some(n);
            }
        }
    }
    None
}
