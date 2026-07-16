//! SsmBackend — sing-box SSM API 客户端（SS-2022 managed 入站）。
//! 端点（实测 1.13.14）：<base>/<inbound>/server/v1/{users,users/:name,stats}
//!   POST /users {username,uPSK}->201；DELETE /users/:name->204(404 幂等)；
//!   GET /users->{users:[...]}；GET /stats->{..,users:[{username,uplinkBytes,downlinkBytes,..}]}
//! 加/删用户走内存 usersMap，不重建入站、不清零他人计数（已实测）。

use super::{Backend, Desired, UserStat};
use anyhow::{bail, Result};
use async_trait::async_trait;
use serde::Deserialize;
use std::collections::BTreeSet;
use std::time::Duration;

#[derive(Debug, Clone)]
pub struct UserTraffic {
    pub name: String,
    pub up: u64,
    pub down: u64,
}

pub struct SsmBackend {
    client: reqwest::Client,
    base: String,
    inbounds: Vec<String>, // 由 meter 填入（trait read_stats/apply 遍历它）
}

impl SsmBackend {
    pub fn new(base: impl Into<String>) -> Result<Self> {
        let client = reqwest::Client::builder()
            .timeout(Duration::from_secs(10))
            .build()?;
        Ok(Self {
            client,
            base: base.into().trim_end_matches('/').to_string(),
            inbounds: Vec::new(),
        })
    }

    pub fn with_inbounds(mut self, inbounds: Vec<String>) -> Self {
        self.inbounds = inbounds;
        self
    }

    fn url(&self, inbound: &str, tail: &str) -> String {
        format!("{}/{}/server/v1{}", self.base, inbound, tail)
    }

    pub async fn list_users(&self, inbound: &str) -> Result<Vec<String>> {
        let r: UsersResp = self
            .client
            .get(self.url(inbound, "/users"))
            .send()
            .await?
            .error_for_status()?
            .json()
            .await?;
        Ok(r.users.into_iter().map(|u| u.username).collect())
    }

    pub async fn add_user(&self, inbound: &str, name: &str, upsk: &str) -> Result<()> {
        let resp = self
            .client
            .post(self.url(inbound, "/users"))
            .json(&serde_json::json!({ "username": name, "uPSK": upsk }))
            .send()
            .await?;
        match resp.status().as_u16() {
            201 => Ok(()),
            code => bail!(
                "add_user {name} 失败 [{code}]: {}",
                resp.text().await.unwrap_or_default()
            ),
        }
    }

    pub async fn remove_user(&self, inbound: &str, name: &str) -> Result<()> {
        let resp = self
            .client
            .delete(self.url(inbound, &format!("/users/{name}")))
            .send()
            .await?;
        match resp.status().as_u16() {
            204 | 404 => Ok(()),
            code => bail!(
                "remove_user {name} 失败 [{code}]: {}",
                resp.text().await.unwrap_or_default()
            ),
        }
    }

    pub async fn stats_of(&self, inbound: &str) -> Result<Vec<UserTraffic>> {
        let r: UsersResp = self
            .client
            .get(self.url(inbound, "/stats"))
            .send()
            .await?
            .error_for_status()?
            .json()
            .await?;
        Ok(r.users
            .into_iter()
            .map(|u| UserTraffic {
                name: u.username,
                up: u.up.max(0) as u64,
                down: u.down.max(0) as u64,
            })
            .collect())
    }

    /// 让某入站的用户集对齐 desired（name->upsk）：补缺删多。
    async fn reconcile_inbound(
        &self,
        inbound: &str,
        desired: &std::collections::BTreeMap<String, String>,
    ) -> Result<()> {
        let existing: BTreeSet<String> = self.list_users(inbound).await?.into_iter().collect();
        for (name, upsk) in desired {
            if !existing.contains(name) {
                self.add_user(inbound, name, upsk).await?;
            }
        }
        for name in existing.iter().filter(|n| !desired.contains_key(*n)) {
            self.remove_user(inbound, name).await?;
        }
        Ok(())
    }
}

#[derive(Deserialize)]
struct UserRow {
    username: String,
    #[serde(rename = "uplinkBytes", default)]
    up: i64,
    #[serde(rename = "downlinkBytes", default)]
    down: i64,
}

#[derive(Deserialize)]
struct UsersResp {
    #[serde(default)]
    users: Vec<UserRow>,
}

#[async_trait]
impl Backend for SsmBackend {
    async fn read_stats(&self) -> Result<Vec<UserStat>> {
        let mut out = Vec::new();
        for inbound in &self.inbounds {
            for t in self.stats_of(inbound).await? {
                out.push(UserStat {
                    name: t.name,
                    scope: inbound.clone(),
                    up: t.up,
                    down: t.down,
                });
            }
        }
        Ok(out)
    }

    async fn apply(&self, desired: &Desired) -> Result<()> {
        for inbound in &self.inbounds {
            let empty = std::collections::BTreeMap::new();
            let want = desired.get(inbound).unwrap_or(&empty);
            self.reconcile_inbound(inbound, want).await?;
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn ssm_roundtrip() {
        use base64::{engine::general_purpose::STANDARD, Engine};
        let (base, inbound) = match (
            std::env::var("SBM_SSM_BASE"),
            std::env::var("SBM_SSM_INBOUND"),
        ) {
            (Ok(b), Ok(i)) => (b, i),
            _ => {
                eprintln!("skip ssm_roundtrip：设 SBM_SSM_BASE 与 SBM_SSM_INBOUND 以运行");
                return;
            }
        };
        let be = SsmBackend::new(base).unwrap();
        let name = "sbm_it_user";
        let upsk = STANDARD.encode([7u8; 16]);
        let _ = be.remove_user(&inbound, name).await;
        be.add_user(&inbound, name, &upsk).await.unwrap();
        assert!(be
            .list_users(&inbound)
            .await
            .unwrap()
            .iter()
            .any(|u| u == name));
        let _ = be.stats_of(&inbound).await.unwrap();
        be.remove_user(&inbound, name).await.unwrap();
        assert!(!be
            .list_users(&inbound)
            .await
            .unwrap()
            .iter()
            .any(|u| u == name));
        be.remove_user(&inbound, name).await.unwrap();
    }
}
