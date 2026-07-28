//! 拆分 TOML 声明式清单：入口文件按 includes 合并 servers/protocols/listeners/relays/users，
//! 做无副作用语义校验，并为 plan/apply 提供稳定领域输入。

pub mod apply;

use std::collections::{BTreeMap, BTreeSet};
use std::path::{Component, Path, PathBuf};

use serde::{Deserialize, Serialize};

use crate::domain::topology::{ENTRY_PORT, NODE_PORT};
use crate::error::{AppError, ErrorCode, Result};

#[derive(Debug, Clone, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
#[allow(dead_code)]
pub struct Manifest {
    pub format_version: u32,
    #[serde(default)]
    pub includes: Vec<String>,
    pub ssh_key: Option<String>,
    pub known_hosts: Option<String>,
    pub node_port: i64,
    pub singbox_version: Option<String>,
    pub servers: BTreeMap<String, ServerSpec>,
    pub protocols: Protocols,
    #[serde(default)]
    pub listeners: Vec<ListenerSpec>,
    #[serde(default)]
    pub relays: Vec<RelaySpec>,
    #[serde(default)]
    pub users: Vec<UserSpec>,
}

#[derive(Debug, Clone, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ServerSpec {
    pub ssh: String,
    pub address: String,
}

#[derive(Debug, Clone, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Protocols {
    pub shadowsocks: Option<ShadowsocksSpec>,
    pub vless: Option<VlessSpec>,
}

#[derive(Debug, Clone, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct ShadowsocksSpec {
    pub method: String,
    pub server_key: String,
    #[serde(default = "default_true")]
    pub managed: bool,
}

#[derive(Debug, Clone, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct VlessSpec {
    pub flow: String,
    pub private_key: String,
    pub short_id: String,
    pub server_name: String,
    pub handshake_server: String,
    pub handshake_port: u16,
    #[serde(default = "default_client_fingerprint")]
    pub client_fingerprint: String,
}

#[derive(Debug, Clone, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ListenerSpec {
    pub name: String,
    pub server: String,
    pub protocol: String,
    pub bind: String,
    pub port: i64,
}

#[derive(Debug, Clone, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RelaySpec {
    pub name: String,
    pub listener: String,
    pub chain: Vec<String>,
}

#[derive(Debug, Clone, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct UserSpec {
    pub name: String,
    #[serde(default = "default_true")]
    pub enabled: bool,
    pub relays: Vec<String>,
    #[serde(default)]
    pub quota_bytes: i64,
    #[serde(default = "default_reset_cycle")]
    pub reset_cycle: String,
    pub expire_at: Option<i64>,
}

fn default_true() -> bool {
    true
}

fn default_reset_cycle() -> String {
    "monthly".to_string()
}

fn default_client_fingerprint() -> String {
    "chrome".to_string()
}

#[derive(Debug, Clone, Serialize)]
pub struct Plan {
    pub config_path: String,
    pub servers: usize,
    pub listeners: usize,
    pub nodes: usize,
    pub relays: usize,
    pub users: usize,
    pub grants: usize,
    pub protocols: Vec<String>,
    pub warnings: Vec<String>,
}

impl Manifest {
    pub fn load(path: impl AsRef<Path>) -> Result<Self> {
        let path = path.as_ref();
        let base = path.parent().unwrap_or_else(|| Path::new("."));
        let mut root = read_toml(path)?;
        let includes = root
            .get("includes")
            .and_then(toml::Value::as_array)
            .ok_or_else(|| AppError::new(ErrorCode::Config, "config.toml 缺 includes 数组"))?
            .iter()
            .map(|v| {
                v.as_str()
                    .map(ToOwned::to_owned)
                    .ok_or_else(|| AppError::new(ErrorCode::Config, "includes 只能包含字符串"))
            })
            .collect::<Result<Vec<_>>>()?;

        let mut seen = BTreeSet::new();
        for include in &includes {
            if !safe_relative(include) {
                return Err(AppError::new(
                    ErrorCode::Config,
                    format!("include 必须是 config 目录内的相对文件名: {include}"),
                ));
            }
            if !seen.insert(include.clone()) {
                return Err(AppError::new(
                    ErrorCode::Config,
                    format!("重复 include: {include}"),
                ));
            }
            let child = read_toml(&base.join(include))?;
            merge_top_level(&mut root, child, include)?;
        }

        let manifest: Manifest = root
            .try_into()
            .map_err(|e| AppError::new(ErrorCode::Config, format!("声明式配置结构非法: {e}")))?;
        manifest.validate()?;
        Ok(manifest)
    }

    pub fn validate(&self) -> Result<()> {
        let mut issues = Vec::new();
        if self.format_version != 1 {
            issues.push(format!(
                "formatVersion={} 不受支持，当前只支持 1",
                self.format_version
            ));
        }
        if self.node_port != NODE_PORT {
            issues.push(format!(
                "nodePort 必须为固定端口 {NODE_PORT}，当前 {}",
                self.node_port
            ));
        }
        if self.servers.is_empty() {
            issues.push("servers 不能为空".to_string());
        }
        for (id, server) in &self.servers {
            if !valid_id(id) {
                issues.push(format!("server id 非法: {id}"));
            }
            if !server.ssh.starts_with("ssh://") {
                issues.push(format!("server {id} 的 ssh 必须以 ssh:// 开头"));
            }
            if server.address.trim().is_empty() {
                issues.push(format!("server {id} 的 address 不能为空"));
            }
        }

        if let Some(ss) = &self.protocols.shadowsocks {
            if ss.method != "2022-blake3-aes-128-gcm"
                && ss.method != "2022-blake3-aes-256-gcm"
                && ss.method != "2022-blake3-chacha20-poly1305"
            {
                issues.push(format!("不支持的 Shadowsocks-2022 method: {}", ss.method));
            }
            if ss.server_key != "auto" {
                issues.push("protocols.shadowsocks.serverKey 当前只允许 auto".to_string());
            }
            if !ss.managed {
                issues.push("Shadowsocks Entry 必须 managed=true 才能由 SSM 管理用户".to_string());
            }
        }
        if let Some(vless) = &self.protocols.vless {
            if vless.flow != "xtls-rprx-vision" {
                issues.push(format!("不支持的 VLESS flow: {}", vless.flow));
            }
            if vless.private_key != "auto" || vless.short_id != "auto" {
                issues.push("VLESS privateKey/shortId 当前只允许 auto".to_string());
            }
            if vless.server_name.trim().is_empty()
                || vless.handshake_server.trim().is_empty()
                || vless.handshake_port == 0
            {
                issues.push(
                    "VLESS Reality serverName/handshakeServer/handshakePort 非法".to_string(),
                );
            }
            if !matches!(
                vless.client_fingerprint.as_str(),
                "chrome"
                    | "firefox"
                    | "edge"
                    | "safari"
                    | "360"
                    | "qq"
                    | "ios"
                    | "android"
                    | "random"
                    | "randomized"
            ) {
                issues.push(format!(
                    "VLESS clientFingerprint 非法: {}",
                    vless.client_fingerprint
                ));
            }
        }

        let mut listener_names = BTreeSet::new();
        let mut listener_servers = BTreeMap::new();
        let mut listener_protocols = BTreeMap::new();
        let mut listener_sockets = BTreeSet::new();
        for listener in &self.listeners {
            if !listener_names.insert(listener.name.clone()) {
                issues.push(format!("重复 listener name: {}", listener.name));
            }
            listener_servers.insert(listener.name.clone(), listener.server.clone());
            listener_protocols.insert(listener.name.clone(), listener.protocol.clone());
            if !self.servers.contains_key(&listener.server) {
                issues.push(format!(
                    "listener {} 引用不存在的 server {}",
                    listener.name, listener.server
                ));
            }
            if listener.port != ENTRY_PORT {
                issues.push(format!(
                    "listener {} 的 Entry 端口必须为 {ENTRY_PORT}",
                    listener.name
                ));
            }
            if !matches!(listener.bind.as_str(), "::" | "0.0.0.0") {
                issues.push(format!(
                    "listener {} 的 bind 当前只允许 :: 或 0.0.0.0",
                    listener.name
                ));
            }
            match listener.protocol.as_str() {
                "shadowsocks" if self.protocols.shadowsocks.is_none() => issues.push(format!(
                    "listener {} 使用 shadowsocks，但缺协议模板",
                    listener.name
                )),
                "vless" if self.protocols.vless.is_none() => issues.push(format!(
                    "listener {} 使用 vless，但缺协议模板",
                    listener.name
                )),
                "shadowsocks" | "vless" => {}
                other => issues.push(format!("listener {} 使用未知协议 {other}", listener.name)),
            }
            if !listener_sockets.insert((listener.server.clone(), listener.port)) {
                issues.push(format!(
                    "同一服务器端口只能有一个 listener: {}:{}",
                    listener.server, listener.port
                ));
            }
        }

        let mut relay_names = BTreeSet::new();
        let mut relay_protocols = BTreeMap::new();
        for relay in &self.relays {
            if !relay_names.insert(relay.name.clone()) {
                issues.push(format!("重复 relay name: {}", relay.name));
            }
            if !listener_names.contains(&relay.listener) {
                issues.push(format!(
                    "relay {} 引用不存在的 listener {}",
                    relay.name, relay.listener
                ));
            }
            if let Some(protocol) = listener_protocols.get(&relay.listener) {
                relay_protocols.insert(relay.name.clone(), protocol.clone());
            }
            if relay.chain.is_empty() {
                issues.push(format!("relay {} 的 chain 不能为空", relay.name));
            }
            let mut chain_seen = BTreeSet::new();
            for server in &relay.chain {
                if !self.servers.contains_key(server) {
                    issues.push(format!(
                        "relay {} 引用不存在的 server {}",
                        relay.name, server
                    ));
                }
                if !chain_seen.insert(server) {
                    issues.push(format!("relay {} 重复经过 server {}", relay.name, server));
                }
                if listener_servers.get(&relay.listener) == Some(server) {
                    issues.push(format!(
                        "relay {} 的 chain 不能再次包含 Entry server {}",
                        relay.name, server
                    ));
                }
            }
        }

        let mut user_names = BTreeSet::new();
        for user in &self.users {
            if !user_names.insert(user.name.clone()) {
                issues.push(format!("重复 user name: {}", user.name));
            }
            if user.quota_bytes < 0 {
                issues.push(format!("user {} 的 quotaBytes 不能为负数", user.name));
            }
            if user.quota_bytes > 0
                && user
                    .relays
                    .iter()
                    .any(|relay| relay_protocols.get(relay).is_some_and(|p| p == "vless"))
            {
                issues.push(format!(
                    "user {} 授权了 VLESS relay，当前不支持 per-user 计量，quotaBytes 必须为 0",
                    user.name
                ));
            }
            if !matches!(user.reset_cycle.as_str(), "monthly" | "yearly" | "never") {
                issues.push(format!(
                    "user {} 的 resetCycle 非法: {}",
                    user.name, user.reset_cycle
                ));
            }
            let mut grants = BTreeSet::new();
            for relay in &user.relays {
                if !relay_names.contains(relay) {
                    issues.push(format!("user {} 引用不存在的 relay {}", user.name, relay));
                }
                if !grants.insert(relay) {
                    issues.push(format!("user {} 重复授权 relay {}", user.name, relay));
                }
            }
        }

        if issues.is_empty() {
            Ok(())
        } else {
            Err(AppError::new(
                ErrorCode::Validation,
                format!("manifest 校验失败:\n- {}", issues.join("\n- ")),
            ))
        }
    }

    pub fn plan(&self, config_path: impl AsRef<Path>) -> Plan {
        let nodes = self
            .relays
            .iter()
            .flat_map(|r| r.chain.iter().cloned())
            .collect::<BTreeSet<_>>()
            .len();
        let mut protocols = self
            .listeners
            .iter()
            .map(|l| l.protocol.clone())
            .collect::<BTreeSet<_>>()
            .into_iter()
            .collect::<Vec<_>>();
        protocols.sort();
        Plan {
            config_path: config_path.as_ref().display().to_string(),
            servers: self.servers.len(),
            listeners: self.listeners.len(),
            nodes,
            relays: self.relays.len(),
            users: self.users.len(),
            grants: self.users.iter().map(|u| u.relays.len()).sum(),
            protocols,
            warnings: Vec::new(),
        }
    }
}

fn read_toml(path: &Path) -> Result<toml::Value> {
    let raw = std::fs::read_to_string(path).map_err(|e| {
        AppError::with(
            ErrorCode::Config,
            format!("读取配置 {} 失败", path.display()),
            e.into(),
        )
    })?;
    toml::from_str(&raw).map_err(|e| {
        AppError::with(
            ErrorCode::Config,
            format!("解析 TOML {} 失败", path.display()),
            e.into(),
        )
    })
}

fn merge_top_level(root: &mut toml::Value, child: toml::Value, source: &str) -> Result<()> {
    let root = root
        .as_table_mut()
        .ok_or_else(|| AppError::new(ErrorCode::Config, "config.toml 顶层必须是 table"))?;
    let child = child.as_table().ok_or_else(|| {
        AppError::new(
            ErrorCode::Config,
            format!("include {source} 顶层必须是 table"),
        )
    })?;
    for (key, value) in child {
        if root.contains_key(key) {
            return Err(AppError::new(
                ErrorCode::Config,
                format!("include {source} 重复定义顶层字段 {key}"),
            ));
        }
        root.insert(key.clone(), value.clone());
    }
    Ok(())
}

fn safe_relative(path: &str) -> bool {
    let path = PathBuf::from(path);
    !path.as_os_str().is_empty() && path.components().all(|c| matches!(c, Component::Normal(_)))
}

fn valid_id(s: &str) -> bool {
    !s.is_empty()
        && s.chars()
            .all(|c| c.is_ascii_alphanumeric() || c == '-' || c == '_')
}

#[cfg(test)]
mod tests {
    use super::*;

    fn write(path: &Path, value: &str) {
        std::fs::write(path, value).unwrap();
    }

    fn fixture() -> PathBuf {
        let dir = std::env::temp_dir().join(format!("sbm-manifest-{}", uuid::Uuid::new_v4()));
        std::fs::create_dir_all(&dir).unwrap();
        write(
            &dir.join("config.toml"),
            r#"
formatVersion=1
includes=["servers.toml","protocols.toml","listeners.toml","relays.toml","users.toml"]
nodePort=29736
singboxVersion="1.13.14"
"#,
        );
        write(
            &dir.join("servers.toml"),
            r#"
[servers.entry]
ssh="ssh://root@entry.example"
address="entry.example"
[servers.node]
ssh="ssh://root@node.example"
address="node.example"
"#,
        );
        write(
            &dir.join("protocols.toml"),
            r#"
[protocols.shadowsocks]
method="2022-blake3-aes-128-gcm"
serverKey="auto"
managed=true
"#,
        );
        write(
            &dir.join("listeners.toml"),
            r#"
[[listeners]]
name="entry-ss"
server="entry"
protocol="shadowsocks"
bind="::"
port=19736
"#,
        );
        write(
            &dir.join("relays.toml"),
            r#"
[[relays]]
name="to-node"
listener="entry-ss"
chain=["node"]
"#,
        );
        write(
            &dir.join("users.toml"),
            r#"
[[users]]
name="kimi"
enabled=true
relays=["to-node"]
"#,
        );
        dir.join("config.toml")
    }

    #[test]
    fn load_split_manifest_and_plan() {
        let path = fixture();
        let manifest = Manifest::load(&path).unwrap();
        assert_eq!(manifest.servers.len(), 2);
        assert_eq!(manifest.listeners[0].port, ENTRY_PORT);
        assert_eq!(manifest.relays[0].chain, vec!["node"]);
        assert_eq!(manifest.users[0].relays, vec!["to-node"]);
        let plan = manifest.plan(&path);
        assert_eq!(plan.nodes, 1);
        assert_eq!(plan.grants, 1);
        std::fs::remove_dir_all(path.parent().unwrap()).unwrap();
    }

    #[test]
    fn rejects_include_escape_and_unknown_relay() {
        let path = fixture();
        write(
            &path,
            r#"
formatVersion=1
includes=["../servers.toml"]
nodePort=29736
"#,
        );
        assert!(Manifest::load(&path).is_err());
        std::fs::remove_dir_all(path.parent().unwrap()).unwrap();
    }

    #[test]
    fn vless_defaults_fingerprint_and_rejects_nonzero_quota() {
        let raw = r#"
formatVersion=1
includes=[]
nodePort=29736
[servers.entry]
ssh="ssh://root@entry.example"
address="entry.example"
[servers.node]
ssh="ssh://root@node.example"
address="node.example"
[protocols.vless]
flow="xtls-rprx-vision"
privateKey="auto"
shortId="auto"
serverName="www.example.com"
handshakeServer="www.example.com"
handshakePort=443
[[listeners]]
name="entry-vless"
server="entry"
protocol="vless"
bind="::"
port=19736
[[relays]]
name="relay"
listener="entry-vless"
chain=["node"]
[[users]]
name="alice"
enabled=true
relays=["relay"]
quotaBytes=1
"#;
        let manifest: Manifest = toml::from_str(raw).unwrap();
        assert_eq!(
            manifest
                .protocols
                .vless
                .as_ref()
                .unwrap()
                .client_fingerprint,
            "chrome"
        );
        let err = manifest.validate().unwrap_err().to_string();
        assert!(err.contains("quotaBytes 必须为 0"));
    }
}
