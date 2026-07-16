//! 配置模型 + 校验（含 `vless ⇒ reload` 约束）。
//! 链路以 `[exits]` 显式声明为唯一权威——空数组=入口节点自身；其余为 [上一跳,…,入口节点]。

use anyhow::{bail, Context, Result};
use indexmap::IndexMap;
use serde::Deserialize;
use std::path::Path;

#[derive(Debug, Deserialize)]
pub struct Config {
    pub service: Service,
    pub singbox: Singbox,
    pub backend: Backend,
    /// 节点名 -> 地址（IP 或域名；中继密钥由工具托管）
    pub nodes: IndexMap<String, String>,
    /// 出口 -> 链路上游（nearest-first，最后一个是入口节点；空=入口节点自身）
    pub exits: IndexMap<String, Vec<String>>,
    /// 使用外部 SOCKS5 服务的终端出口；未声明的节点继续使用 Shadowsocks-2022 中继。
    #[serde(default)]
    pub socks5_exits: IndexMap<String, Socks5Exit>,
    pub users: IndexMap<String, User>,
}

#[derive(Debug, Deserialize)]
pub struct Service {
    pub listen: String,
    pub public_host: String,
    pub sub_base_url: String,
    pub poll_interval: String,
    /// 月度周期的每月重置日、年度周期的一月重置日（UTC）。
    pub reset_day: u8,
    pub db_path: String,
}

#[derive(Debug, Deserialize)]
pub struct Singbox {
    pub config_out: String,
    /// 所有客户端身份共用的唯一入口端口。
    pub entry_port: u16,
    #[serde(default = "default_relay_port")]
    pub relay_port: u16,
    pub relay_method: String,
    pub inbound: Inbound,
}

fn default_relay_port() -> u16 {
    9736
}

#[derive(Debug, Deserialize)]
pub struct Inbound {
    #[serde(rename = "type")]
    pub kind: String,
    pub method: Option<String>,
    pub reality_handshake: Option<String>,
    pub reality_handshake_port: Option<u16>,
    pub utls_fingerprint: Option<String>,
}

#[derive(Debug, Deserialize)]
pub struct Backend {
    pub mode: String,
    pub ssm_base: String,
    pub stats_grpc: Option<String>,
    /// reload 模式：改用户后重载 sing-box 的命令（如 "systemctl reload sing-box"）。
    pub reload_cmd: Option<String>,
}

#[derive(Debug, Deserialize)]
pub struct Socks5Exit {
    pub port: u16,
    pub username: String,
    pub password: String,
    /// 可选：只允许 `tcp` 或 `udp`；省略时由 sing-box 同时启用两者。
    pub network: Option<String>,
}

#[derive(Debug, Clone, Copy, Default, Deserialize, Eq, PartialEq)]
#[serde(rename_all = "lowercase")]
pub enum ResetCycle {
    #[default]
    Monthly,
    Yearly,
    Never,
}

impl ResetCycle {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Monthly => "monthly",
            Self::Yearly => "yearly",
            Self::Never => "never",
        }
    }
}

#[derive(Debug, Deserialize)]
pub struct User {
    pub quota: String,
    pub expire: String,
    /// 流量配额重置周期；缺省为按月。
    #[serde(default)]
    pub reset: ResetCycle,
    pub exits: Vec<String>,
}

impl Config {
    pub fn load(path: &Path) -> Result<Self> {
        let text =
            std::fs::read_to_string(path).with_context(|| format!("读取 {}", path.display()))?;
        toml::from_str(&text).context("解析 TOML")
    }

    /// 入口节点 = 唯一一个链路为空的出口。
    pub fn entry(&self) -> Result<&str> {
        let mut it = self.exits.iter().filter(|(_, up)| up.is_empty());
        match (it.next(), it.next()) {
            (Some((e, _)), None) => Ok(e.as_str()),
            (None, _) => bail!("需要一个空链路的出口作为入口节点（如 entry = []）"),
            (Some(_), Some(_)) => bail!("多个空链路出口；只能有一个入口节点"),
        }
    }

    /// 所有已声明出口，按 [nodes] 顺序稳定返回。
    ///
    /// 单入口模式会始终生成完整出口链和用户×出口路由；运行时只下发用户已获授权的身份。
    pub fn all_exits(&self) -> Vec<String> {
        self.nodes
            .keys()
            .filter(|n| self.exits.contains_key(*n))
            .cloned()
            .collect()
    }

    /// 需要下发配置的终端节点 = 出现在任一出口链路里的非入口节点，按 [nodes] 顺序。
    pub fn terminal_nodes(&self) -> Vec<String> {
        let entry = self.entry().unwrap_or("");
        let mut seen: Vec<String> = Vec::new();
        for s in self.all_exits() {
            if let Some(ups) = self.exits.get(&s) {
                for n in std::iter::once(s.clone()).chain(ups.iter().cloned()) {
                    if n != entry && !seen.contains(&n) {
                        seen.push(n);
                    }
                }
            }
        }
        self.nodes
            .keys()
            .filter(|n| seen.contains(n) && !self.socks5_exits.contains_key(n.as_str()))
            .cloned()
            .collect()
    }

    pub fn validate(&self) -> Result<()> {
        let mut errs: Vec<String> = Vec::new();

        if let Err(e) = self.entry() {
            errs.push(e.to_string());
        }

        if !(1..=31).contains(&self.service.reset_day) {
            errs.push(format!(
                "service.reset_day 非法：{}（应为 1..=31）",
                self.service.reset_day
            ));
        }

        // 每个出口及其链路上游都必须是已声明节点
        for (name, ups) in &self.exits {
            if !self.nodes.contains_key(name) {
                errs.push(format!("出口 {name} 不在 [nodes]"));
            }
            for h in ups {
                if !self.nodes.contains_key(h) {
                    errs.push(format!("链路 {name} 的上游 {h} 不在 [nodes]"));
                }
                if self.socks5_exits.contains_key(h) {
                    errs.push(format!(
                        "SOCKS5 出口 {h} 只能作为终端，不能作为链路 {name} 的中间节点"
                    ));
                }
            }
        }

        for (name, socks) in &self.socks5_exits {
            if !self.nodes.contains_key(name) {
                errs.push(format!("SOCKS5 出口 {name} 不在 [nodes]"));
            }
            match self.exits.get(name) {
                None => errs.push(format!("SOCKS5 出口 {name} 未在 [exits] 声明")),
                Some(chain) if chain.is_empty() => errs.push(format!(
                    "SOCKS5 出口 {name} 不能作为入口节点，必须作为终端节点"
                )),
                Some(_) => {}
            }
            if socks.port == 0 {
                errs.push(format!("SOCKS5 出口 {name} 的 port 不能为 0"));
            }
            if socks.username.is_empty() {
                errs.push(format!("SOCKS5 出口 {name} 的 username 不能为空"));
            }
            if socks.password.is_empty() {
                errs.push(format!("SOCKS5 出口 {name} 的 password 不能为空"));
            }
            if let Some(network) = &socks.network {
                if !matches!(network.as_str(), "tcp" | "udp") {
                    errs.push(format!(
                        "SOCKS5 出口 {name} 的 network 非法：{network:?}（tcp | udp）"
                    ));
                }
            }
        }

        // 用户出口须已声明；配额/有效期须可解析
        for (u, user) in &self.users {
            let mut seen = std::collections::HashSet::new();
            for e in &user.exits {
                if !self.exits.contains_key(e) {
                    errs.push(format!("用户 {u} 的出口 {e} 未在 [exits] 声明"));
                }
                if !seen.insert(e) {
                    errs.push(format!("用户 {u} 的出口 {e} 重复"));
                }
            }
            if let Err(e) = crate::parse_quota(&user.quota) {
                errs.push(format!("用户 {u} 配额 {:?}：{e}", user.quota));
            }
            if let Err(e) = crate::parse_expire(&user.expire) {
                errs.push(format!("用户 {u} 有效期 {:?}：{e}", user.expire));
            }
        }

        // 入口协议 ↔ 后端模式约束
        match self.singbox.inbound.kind.as_str() {
            "shadowsocks" => {
                if self.singbox.inbound.method.is_none() {
                    errs.push("shadowsocks 入站需要 [singbox.inbound].method".into());
                }
                if !matches!(self.backend.mode.as_str(), "ssm" | "reload") {
                    errs.push(format!(
                        "backend.mode 非法：{}（ssm | reload）",
                        self.backend.mode
                    ));
                }
            }
            "vless-reality" => {
                if self.backend.mode != "reload" {
                    errs.push(
                        "vless-reality 入站必须 backend.mode = \"reload\"（VLESS 无 SSM API）"
                            .into(),
                    );
                }
            }
            other => errs.push(format!(
                "未知入站类型 {other:?}（shadowsocks | vless-reality）"
            )),
        }

        if self.singbox.entry_port == 0 {
            errs.push("singbox.entry_port 不能为 0".into());
        }
        if self.singbox.relay_port == 0 {
            errs.push("singbox.relay_port 不能为 0".into());
        }

        if errs.is_empty() {
            Ok(())
        } else {
            bail!("配置校验失败：\n  - {}", errs.join("\n  - "))
        }
    }
}

#[cfg(test)]
mod tests {
    use super::{ResetCycle, User};

    #[test]
    fn user_reset_defaults_to_monthly_and_accepts_all_cycles() {
        let parse = |reset: Option<&str>| {
            let reset = reset
                .map(|value| format!("reset = \"{value}\"\n"))
                .unwrap_or_default();
            toml::from_str::<User>(&format!(
                "quota = \"10G\"\nexpire = \"2030-01-01\"\n{reset}exits = []\n"
            ))
            .unwrap()
            .reset
        };

        assert_eq!(parse(None), ResetCycle::Monthly);
        assert_eq!(parse(Some("monthly")), ResetCycle::Monthly);
        assert_eq!(parse(Some("yearly")), ResetCycle::Yearly);
        assert_eq!(parse(Some("never")), ResetCycle::Never);
    }
}
