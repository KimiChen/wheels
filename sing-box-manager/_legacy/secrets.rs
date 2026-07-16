//! 工具托管的密钥：一个共用 server_psk、每终端节点一把 relay psk、
//! 每个“用户 × 出口”一套认证身份/uPSK/UUID，每用户一个订阅 token。
//! 已存在的密钥会复用，只补缺失项，保证订阅稳定。

use anyhow::{bail, Context, Result};
use base64::engine::general_purpose::{STANDARD, URL_SAFE_NO_PAD};
use base64::Engine;
use indexmap::IndexMap;
use serde::{Deserialize, Serialize};
use std::collections::HashSet;
use std::path::Path;

#[derive(Debug, Default, Serialize, Deserialize)]
pub struct Secrets {
    pub server_psk: String,
    #[serde(default)]
    pub node: IndexMap<String, String>,
    #[serde(default)]
    pub user: IndexMap<String, UserSecret>,
    /// VLESS-Reality 用（vless-reality 入站时生成）。
    #[serde(default)]
    pub reality: RealitySecret,
}

#[derive(Debug, Default, Clone, Serialize, Deserialize)]
pub struct RealitySecret {
    pub private_key: String,
    pub public_key: String,
    pub short_id: String,
}

#[derive(Debug, Default, Clone, Serialize, Deserialize)]
pub struct UserSecret {
    /// 订阅链接 token（URL 安全）。
    #[serde(default)]
    pub token: String,
    /// 出口名 -> 该用户选择该出口时使用的独立认证身份。
    #[serde(default)]
    pub access: IndexMap<String, AccessSecret>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct AccessSecret {
    /// sing-box 内部认证用户名；使用随机 URL-safe 值，避免用户/出口名进入 SSM URL。
    pub name: String,
    pub upsk: String,
    pub uuid: String,
}

fn rand16() -> [u8; 16] {
    use rand::RngCore;
    let mut b = [0u8; 16];
    rand::thread_rng().fill_bytes(&mut b);
    b
}
fn key16() -> String {
    STANDARD.encode(rand16())
}
fn token16() -> String {
    URL_SAFE_NO_PAD.encode(rand16())
}
fn access_name() -> String {
    format!("a_{}", URL_SAFE_NO_PAD.encode(rand16()))
}
fn short_id8() -> String {
    rand16()[..8].iter().map(|b| format!("{b:02x}")).collect()
}

/// 通过 `sing-box generate reality-keypair` 生成（需 sing-box 在 PATH）。
fn gen_reality_keypair() -> Result<(String, String)> {
    let out = std::process::Command::new("sing-box")
        .args(["generate", "reality-keypair"])
        .output()
        .context("运行 sing-box generate reality-keypair（需 sing-box 在 PATH）")?;
    if !out.status.success() {
        bail!("sing-box generate reality-keypair 失败");
    }
    let s = String::from_utf8_lossy(&out.stdout);
    let mut priv_k = String::new();
    let mut pub_k = String::new();
    for line in s.lines() {
        if let Some(v) = line.strip_prefix("PrivateKey:") {
            priv_k = v.trim().to_string();
        } else if let Some(v) = line.strip_prefix("PublicKey:") {
            pub_k = v.trim().to_string();
        }
    }
    if priv_k.is_empty() || pub_k.is_empty() {
        bail!("解析 reality-keypair 输出失败");
    }
    Ok((priv_k, pub_k))
}

impl Secrets {
    pub fn load_or_make(
        path: &Path,
        nodes: &[String],
        users: &[String],
        exits: &[String],
        need_reality: bool,
    ) -> Result<Self> {
        let mut s: Secrets = if path.exists() {
            toml::from_str(&std::fs::read_to_string(path)?).context("解析 secrets")?
        } else {
            Secrets::default()
        };

        if s.server_psk.is_empty() {
            s.server_psk = key16();
        }
        for n in nodes {
            s.node.entry(n.clone()).or_insert_with(key16);
        }
        for u in users {
            let e = s.user.entry(u.clone()).or_default();
            if e.token.is_empty() {
                e.token = token16();
            }
            for exit in exits {
                e.access
                    .entry(exit.clone())
                    .or_insert_with(|| AccessSecret {
                        name: access_name(),
                        upsk: key16(),
                        uuid: uuid::Uuid::new_v4().to_string(),
                    });
            }
        }
        if need_reality {
            if s.reality.private_key.is_empty() || s.reality.public_key.is_empty() {
                let (priv_k, pub_k) = gen_reality_keypair()?;
                s.reality.private_key = priv_k;
                s.reality.public_key = pub_k;
            }
            if s.reality.short_id.is_empty() {
                s.reality.short_id = short_id8();
            }
        }

        s.validate_current(users, exits)?;

        if let Some(dir) = path.parent() {
            std::fs::create_dir_all(dir).ok();
        }
        std::fs::write(path, toml::to_string_pretty(&s)?).context("写入 secrets")?;
        Ok(s)
    }

    pub fn access(&self, user: &str, exit: &str) -> &AccessSecret {
        &self.user[user].access[exit]
    }

    /// 根据 sing-box/SSM 返回的内部认证名定位主用户和出口。
    pub fn access_owner(&self, identity: &str) -> Option<(&str, &str)> {
        self.user.iter().find_map(|(user, secrets)| {
            secrets.access.iter().find_map(|(exit, access)| {
                (access.name == identity).then_some((user.as_str(), exit.as_str()))
            })
        })
    }

    fn validate_current(&self, users: &[String], exits: &[String]) -> Result<()> {
        let mut tokens = HashSet::new();
        let mut identities = HashSet::new();
        let mut upsks = HashSet::new();
        let mut uuids = HashSet::new();
        for user in users {
            let secret = self
                .user
                .get(user)
                .ok_or_else(|| anyhow::anyhow!("用户 {user} 缺少密钥"))?;
            if secret.token.is_empty() || !tokens.insert(secret.token.as_str()) {
                bail!("用户 {user} 的订阅 token 为空或重复");
            }
            for exit in exits {
                let access = secret
                    .access
                    .get(exit)
                    .ok_or_else(|| anyhow::anyhow!("用户 {user} 的出口 {exit} 缺少认证密钥"))?;
                if access.name.is_empty() || !identities.insert(access.name.as_str()) {
                    bail!("用户 {user} 的出口 {exit} 认证名为空或重复");
                }
                // uPSK / UUID 也须全局唯一，否则 sing-box 认证歧义可能路由到错误出口。
                if access.upsk.is_empty() || !upsks.insert(access.upsk.as_str()) {
                    bail!("用户 {user} 的出口 {exit} uPSK 为空或重复");
                }
                if uuid::Uuid::parse_str(&access.uuid).is_err() {
                    bail!("用户 {user} 的出口 {exit} UUID 非法");
                }
                if !uuids.insert(access.uuid.as_str()) {
                    bail!("用户 {user} 的出口 {exit} UUID 重复");
                }
            }
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::Secrets;

    #[test]
    fn creates_independent_access_identity_for_every_user_and_exit() {
        let path = std::env::temp_dir().join(format!("sbm-secrets-{}.toml", uuid::Uuid::new_v4()));
        let users = vec!["alice".to_string(), "bob".to_string()];
        let exits = vec!["entry".to_string(), "home".to_string()];
        let secrets = Secrets::load_or_make(&path, &[], &users, &exits, false).unwrap();

        let alice_entry = secrets.access("alice", "entry");
        let alice_home = secrets.access("alice", "home");
        assert_ne!(alice_entry.name, alice_home.name);
        assert_ne!(alice_entry.upsk, alice_home.upsk);
        assert_ne!(alice_entry.uuid, alice_home.uuid);
        assert_eq!(
            secrets.access_owner(&alice_home.name),
            Some(("alice", "home"))
        );

        let _ = std::fs::remove_file(path);
    }
}
