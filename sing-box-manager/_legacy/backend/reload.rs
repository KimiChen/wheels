//! ReloadBackend — VLESS/静态 Shadowsocks 用户路径：
//! 改用户 = 改单入口配置的 inbounds[].users + reload_cmd。
//! 统计走 v2ray_api gRPC（全局每认证身份，scope="*"）。
//! ⚠️ reload 会重建实例、断连、清零内存计数——所以每 tick 先 read_stats 再 apply（本 tick 的量已入账）。
//! 前提：sing-box 需以 -tags with_v2ray_api 构建。

use super::{Backend, Desired, UserStat};
use crate::grpc::V2RayStats;
use anyhow::{Context, Result};
use async_trait::async_trait;

pub struct ReloadBackend {
    config_out: String,
    grpc_addr: String,
    reload_cmd: Option<String>,
    vless: bool,
}

impl ReloadBackend {
    pub fn new(
        config_out: impl Into<String>,
        grpc_addr: impl Into<String>,
        reload_cmd: Option<String>,
        vless: bool,
    ) -> Self {
        Self {
            config_out: config_out.into(),
            grpc_addr: grpc_addr.into(),
            reload_cmd,
            vless,
        }
    }
}

#[async_trait]
impl Backend for ReloadBackend {
    async fn read_stats(&self) -> Result<Vec<UserStat>> {
        let mut c = V2RayStats::connect(&self.grpc_addr)
            .await
            .with_context(|| format!("连接 v2ray_api gRPC {}", self.grpc_addr))?;
        Ok(c.user_stats()
            .await?
            .into_iter()
            .map(|(name, up, down)| UserStat {
                name,
                scope: "*".into(),
                up,
                down,
            })
            .collect())
    }

    async fn apply(&self, desired: &Desired) -> Result<()> {
        let text = tokio::fs::read_to_string(&self.config_out)
            .await
            .with_context(|| format!("读取 {}", self.config_out))?;
        let mut cfg: serde_json::Value = serde_json::from_str(&text).context("解析生成配置")?;
        let mut changed = false;
        let mut matched = 0usize;

        if let Some(inbounds) = cfg.get_mut("inbounds").and_then(|v| v.as_array_mut()) {
            for ib in inbounds {
                let tag = ib
                    .get("tag")
                    .and_then(|t| t.as_str())
                    .unwrap_or("")
                    .to_string();
                if let Some(want) = desired.get(&tag) {
                    matched += 1;
                    let users: Vec<serde_json::Value> = want
                        .iter()
                        .map(|(name, credential)| {
                            if self.vless {
                                serde_json::json!({
                                    "name": name,
                                    "uuid": credential,
                                    "flow": "xtls-rprx-vision"
                                })
                            } else {
                                serde_json::json!({"name": name, "password": credential})
                            }
                        })
                        .collect();
                    let new_users = serde_json::Value::Array(users);
                    if ib.get("users") != Some(&new_users) {
                        ib["users"] = new_users;
                        changed = true;
                    }
                }
            }
        }

        if matched != desired.len() {
            anyhow::bail!(
                "运行配置的入站与期望身份集不一致：匹配 {matched}/{}，请重新生成入口配置",
                desired.len()
            );
        }

        if !changed {
            return Ok(());
        }
        let pretty = format!("{}\n", serde_json::to_string_pretty(&cfg)?);
        tokio::fs::write(&self.config_out, pretty)
            .await
            .with_context(|| format!("写回 {}", self.config_out))?;

        match &self.reload_cmd {
            Some(cmd) => {
                let status = tokio::process::Command::new("sh")
                    .arg("-c")
                    .arg(cmd)
                    .status()
                    .await
                    .with_context(|| format!("执行 reload_cmd: {cmd}"))?;
                if !status.success() {
                    anyhow::bail!("reload_cmd 退出码非零：{cmd}");
                }
                println!("[reload] 身份变更已写入配置并重载 sing-box");
            }
            None => eprintln!("[reload] 配置已更新但未设 backend.reload_cmd，sing-box 未重载"),
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::ReloadBackend;
    use crate::backend::{Backend, Desired};
    use std::collections::BTreeMap;

    async fn rewritten_user(vless: bool) -> serde_json::Value {
        let path = std::env::temp_dir().join(format!("sbm-reload-{}.json", uuid::Uuid::new_v4()));
        std::fs::write(
            &path,
            r#"{"inbounds":[{"type":"vless","tag":"in-shared","users":[]}]}"#,
        )
        .unwrap();
        let backend = ReloadBackend::new(path.to_string_lossy(), "http://127.0.0.1:1", None, vless);
        let mut users = BTreeMap::new();
        users.insert("access-id".to_string(), "credential".to_string());
        let mut desired = Desired::new();
        desired.insert("in-shared".to_string(), users);

        backend.apply(&desired).await.unwrap();
        let config: serde_json::Value =
            serde_json::from_str(&std::fs::read_to_string(&path).unwrap()).unwrap();
        let _ = std::fs::remove_file(path);
        config["inbounds"][0]["users"][0].clone()
    }

    #[tokio::test]
    async fn writes_protocol_specific_static_identity() {
        let vless = rewritten_user(true).await;
        assert_eq!(vless["name"], "access-id");
        assert_eq!(vless["uuid"], "credential");
        assert_eq!(vless["flow"], "xtls-rprx-vision");
        assert!(vless.get("password").is_none());

        let shadowsocks = rewritten_user(false).await;
        assert_eq!(shadowsocks["name"], "access-id");
        assert_eq!(shadowsocks["password"], "credential");
        assert!(shadowsocks.get("uuid").is_none());
    }
}
