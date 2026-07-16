//! Entry 配置组装：单 managed SS-2022 inbound :19736 + 各 Route 的 detour 出站链 + auth_user 规则
//! + block 兜底 + ssm-api service + DNS bootstrap（**不带 detour**）。忠实移植 legacy 结构。

use std::collections::HashMap;

use serde_json::{json, Value};

use crate::compiler::chain::build_chain;
use crate::compiler::psk::NODE_SS_METHOD;
use crate::compiler::{EntrySnapshot, SSM_CACHE_PATH, SSM_PORT};
use crate::domain::topology::ENTRY_PORT;
use crate::error::{AppError, ErrorCode, Result};
use crate::store::secrets::SecretBundle;

pub fn compile_entry(
    snap: &EntrySnapshot,
    secrets: &SecretBundle,
    identities: &HashMap<String, Vec<String>>,
) -> Result<Value> {
    if snap.entry.inbound_kind == "vless-reality" {
        return Err(AppError::new(
            ErrorCode::Validation,
            "Phase 2 不支持 vless-reality Entry（留 Phase 7）",
        ));
    }
    let entry_psk = secrets.entry_psk.get(&snap.entry.id).ok_or_else(|| {
        AppError::new(
            ErrorCode::Internal,
            format!("缺 entry_psk: {}", snap.entry.id),
        )
    })?;
    let method = snap.entry.ss_method.as_deref().unwrap_or(NODE_SS_METHOD);

    // Route 按 label 升序，确保确定性与稳定 dedup。
    let mut routes: Vec<&crate::compiler::RouteSnapshot> = snap.routes.iter().collect();
    routes.sort_by(|a, b| a.route.label.cmp(&b.route.label));

    let mut outbounds: Vec<Value> = vec![json!({"type": "direct", "tag": "direct"})];
    let mut made: HashMap<(String, String), String> = HashMap::new();
    let mut route_target: HashMap<String, String> = HashMap::new();
    for rs in &routes {
        let tag = build_chain(rs, secrets, &snap.entry.id, &mut outbounds, &mut made)?;
        route_target.insert(rs.route.id.clone(), tag);
    }
    outbounds.push(json!({"type": "block", "tag": "block"})); // fail-closed 兜底

    let inbound = json!({
        "type": "shadowsocks",
        "tag": "in-shared",
        "listen": "::",
        "listen_port": ENTRY_PORT,
        "method": method,
        "password": entry_psk,
        "managed": true,
    });

    let mut rules: Vec<Value> = vec![json!({"action": "sniff"})];
    for rs in &routes {
        let ids = match identities.get(&rs.route.id) {
            Some(v) if !v.is_empty() => v,
            _ => continue, // 空身份集：不生成规则（Phase 2 常态，实测过 check）
        };
        let mut ids = ids.clone();
        ids.sort();
        rules.push(json!({
            "inbound": ["in-shared"],
            "auth_user": ids,
            "action": "route",
            "outbound": route_target[&rs.route.id],
        }));
    }

    Ok(json!({
        "log": {"level": "info", "timestamp": true},
        // DNS bootstrap 绝不带 detour（带 detour:direct 运行期 FATAL；check 放行故靠此硬不变量 + 单测断言）。
        "dns": {
            "servers": [{"tag": "bootstrap", "type": "udp", "server": "1.1.1.1"}],
            "final": "bootstrap",
            "strategy": "prefer_ipv4",
        },
        "inbounds": [inbound],
        "outbounds": outbounds,
        "route": {"rules": rules, "final": "block"},
        "services": [{
            "type": "ssm-api",
            "listen": "127.0.0.1",
            "listen_port": SSM_PORT,
            "servers": {"/in-shared": "in-shared"},
            "cache_path": SSM_CACHE_PATH,
        }],
    }))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::compiler::{compile, RouteSnapshot, Terminal};
    use crate::domain::topology::{Entry, Landing, Node, Route};

    fn entry(id: &str, allow_direct: bool) -> Entry {
        Entry {
            id: id.into(),
            host_id: "eh".into(),
            public_address: "e.example.com".into(),
            port: ENTRY_PORT,
            inbound_kind: "shadowsocks".into(),
            ss_method: None,
            allow_direct,
            current_revision: None,
        }
    }
    fn node(id: &str) -> Node {
        Node {
            id: id.into(),
            host_id: format!("{id}-host"),
            data_address: format!("{id}.example.com"),
            port: crate::domain::topology::NODE_PORT,
            allow_direct_exit: true,
            current_revision: None,
        }
    }
    fn route(id: &str, label: &str, exit_kind: &str) -> Route {
        Route {
            id: id.into(),
            label: label.into(),
            entry_id: "e1".into(),
            exit_kind: exit_kind.into(),
            exit_node_id: None,
            exit_landing_id: None,
            status: "draft".into(),
        }
    }
    fn secrets(nodes: &[&str]) -> SecretBundle {
        let mut s = SecretBundle::default();
        s.entry_psk.insert("e1".into(), "ENTRYPSK".into());
        for n in nodes {
            s.node_psk.insert((*n).into(), format!("PSK-{n}"));
        }
        s
    }
    fn no_ids() -> HashMap<String, Vec<String>> {
        HashMap::new()
    }

    #[test]
    fn manage_direct_golden() {
        let snap = EntrySnapshot {
            entry: entry("e1", true),
            routes: vec![RouteSnapshot {
                route: route("r-direct", "manage-direct", "entry_direct"),
                hops: vec![],
                terminal: Terminal::Direct,
            }],
        };
        let cfg = compile_entry(&snap, &secrets(&[]), &no_ids()).unwrap();
        assert_eq!(cfg["inbounds"][0]["tag"], "in-shared");
        assert_eq!(cfg["inbounds"][0]["listen_port"], ENTRY_PORT);
        assert_eq!(cfg["inbounds"][0]["managed"], true);
        assert_eq!(cfg["route"]["final"], "block");
        // outbounds = [direct, block]（直出无中继）。
        let obs = cfg["outbounds"].as_array().unwrap();
        assert_eq!(obs.len(), 2);
        assert_eq!(obs[0]["tag"], "direct");
        assert_eq!(obs[1]["tag"], "block");
        // DNS bootstrap 无 detour。
        assert!(cfg["dns"]["servers"][0].get("detour").is_none());
        assert_eq!(cfg["services"][0]["type"], "ssm-api");
        assert_eq!(cfg["services"][0]["listen_port"], SSM_PORT);
    }

    #[test]
    fn multihop_socks5_chain_golden() {
        // entry -> n1 -> n2 -> socks5 landing
        let landing = Landing {
            id: "home".into(),
            kind: "socks5".into(),
            node_id: None,
            socks5_address: Some("home.example.com".into()),
            socks5_port: Some(1080),
            network: "tcp".into(),
            auth_credential_id: Some("c".into()),
        };
        let mut r = route("r1", "home-route", "landing");
        r.exit_landing_id = Some("home".into());
        let snap = EntrySnapshot {
            entry: entry("e1", false),
            routes: vec![RouteSnapshot {
                route: r,
                hops: vec![node("n1"), node("n2")],
                terminal: Terminal::Socks5(landing),
            }],
        };
        let mut sec = secrets(&["n1", "n2"]);
        sec.landing_auth
            .insert("home".into(), ("u".into(), "p".into()));
        let cfg = compile_entry(&snap, &sec, &no_ids()).unwrap();
        let obs = cfg["outbounds"].as_array().unwrap();
        // 链：out-n1-via-e1 (detour direct), out-n2-via-n1, out-socks-home-via-n2
        let find = |tag: &str| obs.iter().find(|o| o["tag"] == tag).unwrap();
        assert_eq!(find("out-n1-via-e1")["detour"], "direct");
        assert_eq!(find("out-n1-via-e1")["server"], "n1.example.com");
        assert_eq!(find("out-n2-via-n1")["detour"], "out-n1-via-e1");
        let socks = find("out-socks-home-via-n2");
        assert_eq!(socks["type"], "socks");
        assert_eq!(socks["version"], "5");
        assert_eq!(socks["detour"], "out-n2-via-n1");
        assert_eq!(socks["username"], "u");
        assert_eq!(socks["network"], "tcp"); // 受限网络透传
    }

    #[test]
    fn shared_node_relay_dedup() {
        // 两条 Route 共享前缀 entry->n1：n1 中继只建一份；一条到 n1(终端)、一条到 n2。
        let mut r1 = route("r1", "a", "node");
        r1.exit_node_id = Some("n1".into());
        let mut r2 = route("r2", "b", "node");
        r2.exit_node_id = Some("n2".into());
        let snap = EntrySnapshot {
            entry: entry("e1", false),
            routes: vec![
                RouteSnapshot {
                    route: r1,
                    hops: vec![],
                    terminal: Terminal::Node(node("n1")),
                },
                RouteSnapshot {
                    route: r2,
                    hops: vec![node("n1")],
                    terminal: Terminal::Node(node("n2")),
                },
            ],
        };
        let compiled = compile(&snap, &secrets(&["n1", "n2"]), &no_ids()).unwrap();
        let obs = compiled.entry["outbounds"].as_array().unwrap();
        // out-n1-via-e1 只出现一次（dedup）。
        let n1_count = obs.iter().filter(|o| o["tag"] == "out-n1-via-e1").count();
        assert_eq!(n1_count, 1);
        assert!(obs.iter().any(|o| o["tag"] == "out-n2-via-n1"));
        // 两个 Node 各一份配置，按 id 升序。
        assert_eq!(compiled.nodes.len(), 2);
        assert_eq!(compiled.nodes[0].0, "n1");
        assert_eq!(compiled.nodes[1].0, "n2");
        // Node 配置：SS-2022 in-relay :29736 → direct。
        assert_eq!(compiled.nodes[0].1["inbounds"][0]["tag"], "in-relay");
        assert_eq!(compiled.nodes[0].1["route"]["final"], "direct");
    }

    #[test]
    fn auth_user_rule_only_when_identities_present() {
        let mut r = route("r1", "a", "entry_direct");
        r.exit_kind = "entry_direct".into();
        let snap = EntrySnapshot {
            entry: entry("e1", true),
            routes: vec![RouteSnapshot {
                route: r,
                hops: vec![],
                terminal: Terminal::Direct,
            }],
        };
        // 无身份 → 只有 sniff 规则。
        let cfg = compile_entry(&snap, &secrets(&[]), &no_ids()).unwrap();
        assert_eq!(cfg["route"]["rules"].as_array().unwrap().len(), 1);
        // 注入两个身份（乱序）→ 生成 auth_user 规则、身份排序、指向 direct。
        let ids: HashMap<String, Vec<String>> = [(
            "r1".to_string(),
            vec!["bob".to_string(), "alice".to_string()],
        )]
        .into();
        let cfg2 = compile_entry(&snap, &secrets(&[]), &ids).unwrap();
        let rules = cfg2["route"]["rules"].as_array().unwrap();
        assert_eq!(rules.len(), 2);
        assert_eq!(rules[1]["auth_user"], serde_json::json!(["alice", "bob"]));
        assert_eq!(rules[1]["outbound"], "direct");
    }

    #[test]
    fn vless_entry_unsupported() {
        let mut e = entry("e1", true);
        e.inbound_kind = "vless-reality".into();
        let snap = EntrySnapshot {
            entry: e,
            routes: vec![],
        };
        assert!(compile_entry(&snap, &secrets(&[]), &no_ids()).is_err());
    }
}
