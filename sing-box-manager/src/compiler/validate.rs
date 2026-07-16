//! Route 校验引擎（§4.5 的 11 条）。纯函数，**收集全部错误**（服务 Web 编辑器一次回显）；
//! 前置对象缺失则跳过依赖它的规则、继续收集其余。环检测严格 **intra-route**（单遍访问集）——
//! detour 图按 (node,prev) 键、via-前缀相异、均根于 direct，天然无环，故 Route entry→A→B 与
//! entry→B→A 合法共存，绝不跨 Route 判环。R9（同 Host 端口占用）在 create_entry/create_node 处强制。

use std::collections::{HashMap, HashSet};

use serde::Serialize;

use crate::domain::host::Capability;
use crate::domain::topology::{
    Entry, ExitKind, Landing, LandingKind, Node, RouteDraft, ENTRY_PORT, NODE_PORT,
};

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "lowercase")]
pub enum Severity {
    Error,
    Warning,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct Issue {
    pub rule: &'static str,
    pub severity: Severity,
    pub message: String,
}

impl Issue {
    fn err(rule: &'static str, message: impl Into<String>) -> Self {
        Issue {
            rule,
            severity: Severity::Error,
            message: message.into(),
        }
    }
    fn warn(rule: &'static str, message: impl Into<String>) -> Self {
        Issue {
            rule,
            severity: Severity::Warning,
            message: message.into(),
        }
    }
}

/// 校验所需的既有拓扑上下文（不含密钥）。
pub struct ValidationContext {
    pub entries: HashMap<String, Entry>,
    pub nodes: HashMap<String, Node>,
    pub landings: HashMap<String, Landing>,
    pub host_caps: HashMap<String, HashSet<Capability>>,
}

impl ValidationContext {
    fn host_has(&self, host_id: &str, cap: Capability) -> bool {
        self.host_caps
            .get(host_id)
            .is_some_and(|c| c.contains(&cap))
    }
}

/// 校验一条 Route 草稿。返回全部 issue（含 warning）；无 Error 即可落库。
/// `label_taken`：调用方预查 label 是否被别的 Route 占用（排除自身）。
pub fn validate_route(
    ctx: &ValidationContext,
    draft: &RouteDraft,
    label_taken: bool,
) -> Vec<Issue> {
    let mut issues: Vec<Issue> = Vec::new();
    let entry = ctx.entries.get(&draft.entry_id);

    // R1 Entry 存在。
    if entry.is_none() {
        issues.push(Issue::err(
            "R1",
            format!("entry 不存在: {}", draft.entry_id),
        ));
    }

    // R1/R2 每个 hop：须存在且为 Node。
    for h in &draft.hops {
        if ctx.nodes.contains_key(h) {
            continue;
        }
        if ctx.entries.contains_key(h) || ctx.landings.contains_key(h) {
            issues.push(Issue::err("R2", format!("hop 只能引用 Node: {h}")));
        } else {
            issues.push(Issue::err("R1", format!("hop 节点不存在: {h}")));
        }
    }

    // R5 Exit 类型与目标一致 + 收集终端节点。
    let mut terminal_node: Option<String> = None;
    match draft.exit_kind {
        ExitKind::EntryDirect => {
            if draft.exit_node_id.is_some() || draft.exit_landing_id.is_some() {
                issues.push(Issue::err("R5", "entry_direct 不应指定出口对象"));
            }
            if !draft.hops.is_empty() {
                issues.push(Issue::err("R5", "entry_direct 不应有 hops"));
            }
            // R6 直出许可。
            if let Some(e) = entry {
                if !e.allow_direct {
                    issues.push(Issue::err(
                        "R6",
                        "该 Entry 不允许直接出口（allow_direct=0）",
                    ));
                }
            }
        }
        ExitKind::Node => {
            if draft.exit_landing_id.is_some() {
                issues.push(Issue::err("R5", "node 出口不应指定 landing"));
            }
            match &draft.exit_node_id {
                None => issues.push(Issue::err("R5", "node 出口缺 exit_node_id")),
                Some(nid) => match ctx.nodes.get(nid) {
                    Some(_) => terminal_node = Some(nid.clone()),
                    None => issues.push(Issue::err("R1", format!("出口 Node 不存在: {nid}"))),
                },
            }
        }
        ExitKind::Landing => {
            if draft.exit_node_id.is_some() {
                issues.push(Issue::err("R5", "landing 出口不应指定 node"));
            }
            match &draft.exit_landing_id {
                None => issues.push(Issue::err("R5", "landing 出口缺 exit_landing_id")),
                Some(lid) => match ctx.landings.get(lid) {
                    None => issues.push(Issue::err("R1", format!("出口 Landing 不存在: {lid}"))),
                    Some(l) => {
                        validate_landing_self(l, &mut issues, &mut terminal_node, ctx, draft)
                    }
                },
            }
        }
    }

    // R3/R4 无环（intra-route）：seq = [hops by position] ++ terminal_node，单遍去重。
    let mut seq: Vec<&String> = draft.hops.iter().collect();
    if let Some(t) = &terminal_node {
        seq.push(t);
    }
    let mut seen: HashSet<&str> = HashSet::new();
    for n in &seq {
        if !seen.insert(n.as_str()) {
            issues.push(Issue::err("R4", format!("路径重复经过同一 Node: {n}")));
        }
    }

    // R7 label 唯一。
    if label_taken {
        issues.push(Issue::err(
            "R7",
            format!("Route 标签已被占用: {}", draft.label),
        ));
    }

    // R8 固定端口 + R11 能力：Entry。
    if let Some(e) = entry {
        if e.port != ENTRY_PORT {
            issues.push(Issue::err("R8", format!("Entry 端口须为 {ENTRY_PORT}")));
        }
        if !ctx.host_has(&e.host_id, Capability::Entry) {
            issues.push(Issue::err(
                "R11",
                format!("Entry 所在 Host 缺 entry 能力: {}", e.host_id),
            ));
        }
    }

    // R8/R11：全部引用到的 Node（hops + 终端 node）。
    let mut ref_nodes: Vec<&String> = draft.hops.iter().collect();
    if let Some(t) = &terminal_node {
        ref_nodes.push(t);
    }
    for nid in ref_nodes {
        if let Some(n) = ctx.nodes.get(nid) {
            if n.port != NODE_PORT {
                issues.push(Issue::err("R8", format!("Node {nid} 端口须为 {NODE_PORT}")));
            }
            if !ctx.host_has(&n.host_id, Capability::Node) {
                issues.push(Issue::err(
                    "R11",
                    format!("Node {nid} 所在 Host 缺 node 能力"),
                ));
            }
        }
    }

    // 稳定排序：先按 rule，再按 message。
    issues.sort_by(|a, b| a.rule.cmp(b.rule).then_with(|| a.message.cmp(&b.message)));
    issues.dedup();
    issues
}

fn validate_landing_self(
    l: &Landing,
    issues: &mut Vec<Issue>,
    terminal_node: &mut Option<String>,
    ctx: &ValidationContext,
    _draft: &RouteDraft,
) {
    match LandingKind::parse(&l.kind) {
        Some(LandingKind::ManagedNode) => match &l.node_id {
            Some(nid) if ctx.nodes.contains_key(nid) => *terminal_node = Some(nid.clone()),
            Some(nid) => issues.push(Issue::err(
                "R5",
                format!("managed_node landing 的 node 不存在: {nid}"),
            )),
            None => issues.push(Issue::err("R5", "managed_node landing 缺 node_id")),
        },
        Some(LandingKind::Socks5) => {
            if l.socks5_address.is_none() || l.socks5_port.is_none() {
                issues.push(Issue::err("R5", "socks5 landing 缺 address/port"));
            }
            // R10：socks5 网络能力非 both 记为 warning（透传 outbound.network；硬校验待 Phase 7）。
            if l.network != "both" {
                issues.push(Issue::warn(
                    "R10",
                    format!(
                        "socks5 landing 网络能力受限为 {}（当前为 advisory）",
                        l.network
                    ),
                ));
            }
        }
        None => issues.push(Issue::err("R5", format!("未知 landing 类型: {}", l.kind))),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::domain::topology::Network;

    fn caps(pairs: &[(&str, &[Capability])]) -> HashMap<String, HashSet<Capability>> {
        pairs
            .iter()
            .map(|(h, cs)| (h.to_string(), cs.iter().copied().collect()))
            .collect()
    }
    fn entry(id: &str, host: &str, port: i64, allow_direct: bool) -> Entry {
        Entry {
            id: id.into(),
            host_id: host.into(),
            public_address: "a".into(),
            port,
            inbound_kind: "shadowsocks".into(),
            ss_method: None,
            allow_direct,
            current_revision: None,
        }
    }
    fn node(id: &str, host: &str, port: i64) -> Node {
        Node {
            id: id.into(),
            host_id: host.into(),
            data_address: "a".into(),
            port,
            allow_direct_exit: true,
            current_revision: None,
        }
    }
    fn ctx_full() -> ValidationContext {
        ValidationContext {
            entries: [("e1".to_string(), entry("e1", "eh", ENTRY_PORT, true))].into(),
            nodes: [
                ("n1".to_string(), node("n1", "nh", NODE_PORT)),
                ("n2".to_string(), node("n2", "nh2", NODE_PORT)),
            ]
            .into(),
            landings: HashMap::new(),
            host_caps: caps(&[
                ("eh", &[Capability::Entry]),
                ("nh", &[Capability::Node]),
                ("nh2", &[Capability::Node]),
            ]),
        }
    }
    fn draft(label: &str, hops: &[&str], exit_kind: ExitKind, en: Option<&str>) -> RouteDraft {
        RouteDraft {
            id: None,
            label: label.into(),
            entry_id: "e1".into(),
            hops: hops.iter().map(|s| s.to_string()).collect(),
            exit_kind,
            exit_node_id: en.map(String::from),
            exit_landing_id: None,
        }
    }

    #[test]
    fn valid_multihop_passes() {
        let ctx = ctx_full();
        let d = draft("ok", &["n1"], ExitKind::Node, Some("n2"));
        let issues = validate_route(&ctx, &d, false);
        assert!(
            issues.iter().all(|i| i.severity == Severity::Warning),
            "{issues:?}"
        );
    }

    #[test]
    fn manage_direct_requires_allow_direct() {
        let mut ctx = ctx_full();
        ctx.entries
            .insert("e1".into(), entry("e1", "eh", ENTRY_PORT, false));
        let d = draft("d", &[], ExitKind::EntryDirect, None);
        let issues = validate_route(&ctx, &d, false);
        assert!(issues.iter().any(|i| i.rule == "R6"));
    }

    #[test]
    fn cycle_intra_route_only_no_cross_route() {
        let ctx = ctx_full();
        // A→B 与 B→A 两条 Route 各自校验都应通过（绝不跨 Route 判环）。
        let ab = draft("ab", &["n1"], ExitKind::Node, Some("n2"));
        let ba = draft("ba", &["n2"], ExitKind::Node, Some("n1"));
        assert!(!validate_route(&ctx, &ab, false)
            .iter()
            .any(|i| i.rule == "R4"));
        assert!(!validate_route(&ctx, &ba, false)
            .iter()
            .any(|i| i.rule == "R4"));
        // 同一 Route 内重复经过 n1（hop 与 exit 撞）→ R4。
        let dup = draft("dup", &["n1"], ExitKind::Node, Some("n1"));
        assert!(validate_route(&ctx, &dup, false)
            .iter()
            .any(|i| i.rule == "R4"));
    }

    #[test]
    fn collects_multiple_errors() {
        let ctx = ctx_full();
        // 不存在的 entry + label 占用 + 坏端口 node。
        let mut ctx2 = ctx;
        ctx2.nodes.insert("nbad".into(), node("nbad", "nh", 12345));
        let d = RouteDraft {
            id: None,
            label: "x".into(),
            entry_id: "missing".into(),
            hops: vec!["nbad".into()],
            exit_kind: ExitKind::Node,
            exit_node_id: Some("n1".into()),
            exit_landing_id: None,
        };
        let issues = validate_route(&ctx2, &d, true);
        let rules: HashSet<&str> = issues.iter().map(|i| i.rule).collect();
        assert!(rules.contains("R1")); // missing entry
        assert!(rules.contains("R7")); // label taken
        assert!(rules.contains("R8")); // bad node port
                                       // 排序稳定。
        let mut sorted = issues.clone();
        sorted.sort_by(|a, b| a.rule.cmp(b.rule).then_with(|| a.message.cmp(&b.message)));
        assert_eq!(issues, sorted);
    }

    #[test]
    fn socks5_restricted_network_is_warning() {
        let mut ctx = ctx_full();
        ctx.landings.insert(
            "l1".into(),
            Landing {
                id: "l1".into(),
                kind: "socks5".into(),
                node_id: None,
                socks5_address: Some("s".into()),
                socks5_port: Some(1080),
                network: Network::Tcp.as_str().into(),
                auth_credential_id: None,
            },
        );
        let d = RouteDraft {
            id: None,
            label: "s".into(),
            entry_id: "e1".into(),
            hops: vec![],
            exit_kind: ExitKind::Landing,
            exit_node_id: None,
            exit_landing_id: Some("l1".into()),
        };
        let issues = validate_route(&ctx, &d, false);
        assert!(issues
            .iter()
            .any(|i| i.rule == "R10" && i.severity == Severity::Warning));
        assert!(!issues.iter().any(|i| i.severity == Severity::Error));
    }
}
