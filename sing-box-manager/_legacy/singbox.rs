//! 由 config + secrets 生成 sing-box 1.13.x 配置。
//! 所有用户和出口共用一个入口端口，按独立认证身份路由到显式 detour 出站链。
//! 终端节点可为 SS-2022 中继或带认证的 SOCKS5。

use crate::config::Config;
use crate::secrets::Secrets;
use anyhow::{bail, Result};
use serde_json::{json, Value};
use std::collections::{HashMap, HashSet};

pub struct Generated {
    pub entry: Value,
    /// (终端节点名, 该节点的 sing-box 配置)
    pub nodes: Vec<(String, Value)>,
}

/// 沿某出口的显式链路建 detour 出站链，返回路由目标（出口终端 tag；入口自身出口返回 "direct"）。
fn build_chain(
    cfg: &Config,
    sec: &Secrets,
    exit: &str,
    entry: &str,
    outbounds: &mut Vec<Value>,
    made: &mut HashMap<(String, String), String>,
) -> String {
    let ups = &cfg.exits[exit]; // nearest-first, entry last
    let mut seq: Vec<&str> = ups.iter().rev().map(String::as_str).collect();
    seq.push(exit); // entry .. exit

    let mut prev_tag = String::from("direct");
    let mut prev_node = entry.to_string();
    for &node in seq.iter().skip(1) {
        let key = (node.to_string(), prev_node.clone());
        let tag = match made.get(&key) {
            Some(t) => t.clone(),
            None => {
                let t = format!("out-{node}-via-{prev_node}");
                let outbound = if let Some(socks) = cfg.socks5_exits.get(node) {
                    let mut outbound = json!({
                        "type": "socks",
                        "tag": t,
                        "server": cfg.nodes[node],
                        "server_port": socks.port,
                        "version": "5",
                        "username": socks.username,
                        "password": socks.password,
                        "detour": prev_tag,
                    });
                    if let Some(network) = &socks.network {
                        outbound["network"] = json!(network);
                    }
                    outbound
                } else {
                    json!({
                        "type": "shadowsocks",
                        "tag": t,
                        "server": cfg.nodes[node],
                        "server_port": cfg.singbox.relay_port,
                        "method": cfg.singbox.relay_method,
                        "password": sec.node[node],
                        "detour": prev_tag,
                    })
                };
                outbounds.push(outbound);
                made.insert(key, t.clone());
                t
            }
        };
        prev_tag = tag;
        prev_node = node.to_string();
    }
    prev_tag // 无跳（exit==entry）时保持 "direct"
}

pub fn generate(cfg: &Config, sec: &Secrets) -> Result<Generated> {
    let entry = cfg.entry()?.to_string();
    let exits = cfg.all_exits();
    let in_method = cfg
        .singbox
        .inbound
        .method
        .as_deref()
        .unwrap_or(&cfg.singbox.relay_method);

    // 出站链
    let mut outbounds: Vec<Value> = vec![json!({"type": "direct", "tag": "direct"})];
    let mut made: HashMap<(String, String), String> = HashMap::new();
    let mut target: HashMap<&str, String> = HashMap::new();
    for s in &exits {
        let t = build_chain(cfg, sec, s, &entry, &mut outbounds, &mut made);
        target.insert(s.as_str(), t);
    }
    outbounds.push(json!({"type": "block", "tag": "block"})); // fail-closed 兜底

    let ssm = cfg.backend.mode == "ssm";
    let vless = cfg.singbox.inbound.kind == "vless-reality";
    let handshake = cfg
        .singbox
        .inbound
        .reality_handshake
        .clone()
        .unwrap_or_else(|| "www.microsoft.com".to_string());
    let hs_port = cfg.singbox.inbound.reality_handshake_port.unwrap_or(443);

    // 验证认证身份全局唯一；否则不同出口的 auth_user 规则会产生歧义。
    let mut identity_set = HashSet::new();
    for user in cfg.users.keys() {
        for exit in &exits {
            let identity = &sec.access(user, exit).name;
            if !identity_set.insert(identity) {
                bail!("认证身份重复：{identity}");
            }
        }
    }

    // 运行态只认证用户当前获权的“用户 × 出口”身份；路由表预生成当前用户与全部出口的组合，
    // 因此 SSM 模式下已有用户调整出口权限只需动态增删身份，无需改路由。
    let authorized: Vec<(&str, &str)> = cfg
        .users
        .iter()
        .flat_map(|(user, u)| {
            u.exits
                .iter()
                .map(move |exit| (user.as_str(), exit.as_str()))
        })
        .collect();

    let inbound = if vless {
        let users: Vec<Value> = authorized
            .iter()
            .map(|(user, exit)| {
                let access = sec.access(user, exit);
                json!({"name": access.name, "uuid": access.uuid, "flow": "xtls-rprx-vision"})
            })
            .collect();
        json!({
            "type": "vless", "tag": "in-shared", "listen": "::",
            "listen_port": cfg.singbox.entry_port, "users": users,
            "tls": {"enabled": true, "server_name": handshake,
                "reality": {"enabled": true,
                    "handshake": {"server": handshake, "server_port": hs_port},
                    "private_key": sec.reality.private_key,
                    "short_id": [sec.reality.short_id]}}
        })
    } else {
        let mut inbound = json!({
            "type": "shadowsocks", "tag": "in-shared", "listen": "::",
            "listen_port": cfg.singbox.entry_port, "method": in_method,
            "password": sec.server_psk,
        });
        if ssm {
            inbound["managed"] = json!(true);
        } else {
            inbound["users"] = json!(authorized
                .iter()
                .map(|(user, exit)| {
                    let access = sec.access(user, exit);
                    json!({"name": access.name, "password": access.upsk})
                })
                .collect::<Vec<_>>());
        }
        inbound
    };

    let mut rules: Vec<Value> = vec![json!({"action": "sniff"})];
    for exit in &exits {
        let auth_users: Vec<&str> = cfg
            .users
            .keys()
            .map(|user| sec.access(user, exit).name.as_str())
            .collect();
        if !auth_users.is_empty() {
            rules.push(json!({
                "inbound": ["in-shared"],
                "auth_user": auth_users,
                "action": "route",
                "outbound": target[exit.as_str()],
            }));
        }
    }

    let mut entry_cfg = json!({
        "log": {"level": "info", "timestamp": true},
        "dns": {
            "servers": [{"tag": "bootstrap", "type": "udp", "server": "1.1.1.1"}],
            "final": "bootstrap",
            "strategy": "prefer_ipv4",
        },
        "inbounds": [inbound],
        "outbounds": outbounds,
        "route": {"rules": rules, "final": "block"},
    });

    if ssm {
        // SSM API：单一 managed 入站负责全部身份的动态管理与统计。
        let (host, port) = host_port(&cfg.backend.ssm_base)?;
        entry_cfg["services"] = json!([{
            "type": "ssm-api",
            "listen": host,
            "listen_port": port,
            "servers": {"/in-shared": "in-shared"},
            "cache_path": ssm_cache_path(&cfg.service.db_path),
        }]);
    } else {
        // reload 模式：v2ray_api 每认证身份统计（gRPC 由 ReloadBackend 读）。
        // ⚠️ 需要 sing-box 以 -tags with_v2ray_api 构建（官方/homebrew 默认不带）。
        let (gh, gp) = match cfg.backend.stats_grpc.as_deref() {
            Some(a) => host_port(a)?,
            None => ("127.0.0.1".to_string(), 8080),
        };
        entry_cfg["experimental"] = json!({
            "v2ray_api": {
                "listen": format!("{gh}:{gp}"),
                "stats": {"enabled": true, "users": authorized
                    .iter()
                    .map(|(user, exit)| sec.access(user, exit).name.as_str())
                    .collect::<Vec<_>>()},
            }
        });
    }

    // 终端 sing-box 节点配置（SOCKS5 终端由外部服务提供，不生成节点配置）
    let mut nodes: Vec<(String, Value)> = Vec::new();
    for s in cfg.terminal_nodes() {
        let v = json!({
            "log": {"level": "warn"},
            "dns": {"servers": [{"tag": "local", "type": "local"}], "final": "local"},
            "inbounds": [{
                "type": "shadowsocks", "tag": "in-relay", "listen": "::",
                "listen_port": cfg.singbox.relay_port,
                "method": cfg.singbox.relay_method, "password": sec.node[&s],
            }],
            "outbounds": [{"type": "direct", "tag": "direct"}],
            "route": {"rules": [{"action": "sniff"}], "final": "direct"},
        });
        nodes.push((s, v));
    }

    Ok(Generated {
        entry: entry_cfg,
        nodes,
    })
}

/// "http://127.0.0.1:8081" -> ("127.0.0.1", 8081)
fn host_port(url: &str) -> Result<(String, u16)> {
    let s = url
        .trim()
        .trim_start_matches("http://")
        .trim_start_matches("https://")
        .trim_end_matches('/');
    let (h, p) = s
        .rsplit_once(':')
        .ok_or_else(|| anyhow::anyhow!("ssm_base 缺少端口：{url}"))?;
    let port = p
        .parse()
        .map_err(|_| anyhow::anyhow!("ssm_base 端口非法：{url}"))?;
    Ok((h.to_string(), port))
}

/// SSM 缓存文件放在 db_path 同目录。
fn ssm_cache_path(db_path: &str) -> String {
    match std::path::Path::new(db_path).parent() {
        Some(d) if !d.as_os_str().is_empty() => {
            d.join("ssm-cache.json").to_string_lossy().into_owned()
        }
        _ => "ssm-cache.json".to_string(),
    }
}

#[cfg(test)]
mod tests {
    use super::generate;
    use crate::config::Config;
    use crate::secrets::Secrets;

    #[test]
    fn authenticated_socks5_is_terminal_outbound_without_node_config() {
        let cfg: Config = toml::from_str(
            r#"
[service]
listen = "127.0.0.1:9736"
public_host = "sub.example.com"
sub_base_url = "https://sub.example.com/sub"
poll_interval = "30s"
reset_day = 1
db_path = "/tmp/sbm.db"

[singbox]
config_out = "/tmp/config.json"
entry_port = 19736
relay_method = "2022-blake3-aes-128-gcm"

[singbox.inbound]
type = "shadowsocks"
method = "2022-blake3-aes-128-gcm"

[backend]
mode = "ssm"
ssm_base = "http://127.0.0.1:8081"

[nodes]
entry = "192.0.2.1"
relay = "192.0.2.2"
home = "socks.example.com"

[exits]
entry = []
home = ["relay", "entry"]

[socks5_exits.home]
port = 1080
username = "home-user"
password = "home-pass"
network = "tcp"

[users.alice]
quota = "10G"
expire = "2030-01-01"
exits = ["home"]
"#,
        )
        .unwrap();
        assert_eq!(cfg.singbox.relay_port, 9736);
        cfg.validate().unwrap();
        assert_eq!(cfg.terminal_nodes(), ["relay"]);

        let sec: Secrets = toml::from_str(
            r#"
server_psk = "entry-key"

[node]
relay = "relay-key"

[user.alice]
token = "token"

[user.alice.access.entry]
name = "alice-entry"
upsk = "entry-user-key"
uuid = "00000000-0000-4000-8000-000000000001"

[user.alice.access.home]
name = "alice-home"
upsk = "home-user-key"
uuid = "00000000-0000-4000-8000-000000000002"
"#,
        )
        .unwrap();
        let generated = generate(&cfg, &sec).unwrap();

        assert_eq!(generated.nodes.len(), 1);
        assert_eq!(generated.nodes[0].0, "relay");
        let outbounds = generated.entry["outbounds"].as_array().unwrap();
        let socks = outbounds
            .iter()
            .find(|outbound| outbound["type"] == "socks")
            .unwrap();
        assert_eq!(socks["server"], "socks.example.com");
        assert_eq!(socks["server_port"], 1080);
        assert_eq!(socks["version"], "5");
        assert_eq!(socks["username"], "home-user");
        assert_eq!(socks["password"], "home-pass");
        assert_eq!(socks["network"], "tcp");
        assert_eq!(socks["detour"], "out-relay-via-entry");

        let inbounds = generated.entry["inbounds"].as_array().unwrap();
        assert_eq!(inbounds.len(), 1);
        assert_eq!(inbounds[0]["tag"], "in-shared");
        assert_eq!(inbounds[0]["listen_port"], 19736);
        assert_eq!(inbounds[0]["managed"], true);
        assert_eq!(
            generated.entry["services"][0]["servers"]["/in-shared"],
            "in-shared"
        );

        let rules = generated.entry["route"]["rules"].as_array().unwrap();
        let home_rule = rules
            .iter()
            .find(|rule| {
                rule["auth_user"]
                    .as_array()
                    .is_some_and(|users| users.iter().any(|user| user == "alice-home"))
            })
            .unwrap();
        assert_eq!(home_rule["action"], "route");
        assert_eq!(home_rule["outbound"], "out-home-via-relay");
    }

    #[test]
    fn vless_shared_inbound_uses_authorized_pairs_and_prebuilds_all_routes() {
        let cfg: Config = toml::from_str(
            r#"
[service]
listen = "127.0.0.1:9736"
public_host = "sub.example.com"
sub_base_url = "https://sub.example.com/sub"
poll_interval = "30s"
reset_day = 1
db_path = "/tmp/sbm.db"

[singbox]
config_out = "/tmp/config.json"
entry_port = 19736
relay_method = "2022-blake3-aes-128-gcm"

[singbox.inbound]
type = "vless-reality"
reality_handshake = "www.microsoft.com"

[backend]
mode = "reload"
ssm_base = "http://127.0.0.1:8081"
stats_grpc = "http://127.0.0.1:8082"
reload_cmd = "true"

[nodes]
entry = "192.0.2.1"
home = "192.0.2.2"

[exits]
entry = []
home = ["entry"]

[users.alice]
quota = "10G"
expire = "2030-01-01"
exits = ["entry", "home"]

[users.bob]
quota = "10G"
expire = "2030-01-01"
exits = ["home"]
"#,
        )
        .unwrap();
        let sec: Secrets = toml::from_str(
            r#"
server_psk = "entry-key"

[node]
home = "home-node-key"

[reality]
private_key = "private"
public_key = "public"
short_id = "0011223344556677"

[user.alice]
token = "alice-token"
[user.alice.access.entry]
name = "alice-entry"
upsk = "alice-entry-key"
uuid = "00000000-0000-4000-8000-000000000001"
[user.alice.access.home]
name = "alice-home"
upsk = "alice-home-key"
uuid = "00000000-0000-4000-8000-000000000002"

[user.bob]
token = "bob-token"
[user.bob.access.entry]
name = "bob-entry"
upsk = "bob-entry-key"
uuid = "00000000-0000-4000-8000-000000000003"
[user.bob.access.home]
name = "bob-home"
upsk = "bob-home-key"
uuid = "00000000-0000-4000-8000-000000000004"
"#,
        )
        .unwrap();

        let generated = generate(&cfg, &sec).unwrap();
        let inbound = &generated.entry["inbounds"][0];
        assert_eq!(inbound["tag"], "in-shared");
        assert_eq!(inbound["listen_port"], 19736);
        let users = inbound["users"].as_array().unwrap();
        assert_eq!(users.len(), 3);
        assert!(users.iter().any(|user| user["name"] == "alice-entry"));
        assert!(users.iter().any(|user| user["name"] == "alice-home"));
        assert!(users.iter().any(|user| user["name"] == "bob-home"));
        assert!(!users.iter().any(|user| user["name"] == "bob-entry"));

        let rules = generated.entry["route"]["rules"].as_array().unwrap();
        let entry_rule = rules
            .iter()
            .find(|rule| {
                rule["auth_user"]
                    .as_array()
                    .is_some_and(|users| users.iter().any(|user| user == "bob-entry"))
            })
            .unwrap();
        assert_eq!(entry_rule["outbound"], "direct");
        assert_eq!(
            generated.entry["experimental"]["v2ray_api"]["stats"]["users"]
                .as_array()
                .unwrap()
                .len(),
            3
        );
    }
}
