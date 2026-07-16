//! 本机 SSM API 客户端（Shadowsocks Server Management，127.0.0.1:49736）。动态增删 managed inbound 用户
//! 无需重启、不清零他人计数（实测）。`reconcile` 声明式对齐期望身份集：补缺删多。
//! URL：`{base}/{inbound}/server/v1/{users,stats}`；add 体字段是 **uPSK**（不是 password）。

use std::collections::{BTreeMap, HashSet};
use std::time::Duration;

use async_trait::async_trait;

use crate::domain::user::ReconcileReport;
use crate::error::{AppError, ErrorCode, Result};

/// managed inbound 的 tag（= 编译器 entry.rs inbound tag 与 ssm servers key「/in-shared」去斜杠）。
pub const INBOUND_TAG: &str = "in-shared";

/// SSM /stats 读取结果（累计字节 + 会话数；无密钥）。
#[derive(Debug, Clone, Default)]
pub struct SsmStats {
    pub tcp_sessions: i64,
    pub udp_sessions: i64,
    pub users: Vec<(String, i64, i64)>, // (identity_name, uplink_bytes, downlink_bytes)
}

#[async_trait]
pub trait SsmClient: Send + Sync {
    async fn list_users(&self, inbound: &str) -> Result<Vec<String>>;
    async fn reconcile(
        &self,
        inbound: &str,
        desired: &BTreeMap<String, String>,
    ) -> Result<ReconcileReport>;
    /// 读本机 SSM 累计统计（Phase 5 计量源）。
    async fn read_stats(&self, inbound: &str) -> Result<SsmStats>;
}

pub struct HttpSsmClient {
    base: String,
    http: reqwest::Client,
}

impl HttpSsmClient {
    pub fn new(ssm_address: &str) -> Self {
        let base = if ssm_address.starts_with("http") {
            ssm_address.trim_end_matches('/').to_string()
        } else {
            format!("http://{ssm_address}")
        };
        let http = reqwest::Client::builder()
            .timeout(Duration::from_secs(3))
            .build()
            .unwrap_or_default();
        Self { base, http }
    }
    fn url(&self, inbound: &str, tail: &str) -> String {
        format!("{}/{}/server/v1{}", self.base, inbound, tail)
    }
    async fn add(&self, inbound: &str, name: &str, upsk: &str) -> Result<()> {
        let resp = self
            .http
            .post(self.url(inbound, "/users"))
            .json(&serde_json::json!({"username": name, "uPSK": upsk}))
            .send()
            .await
            .map_err(|e| AppError::new(ErrorCode::Agent, format!("SSM add 失败: {e}")))?;
        if resp.status().is_success() {
            Ok(())
        } else {
            Err(AppError::new(
                ErrorCode::Agent,
                format!("SSM add {} 返回 {}", name, resp.status()),
            ))
        }
    }
    async fn remove(&self, inbound: &str, name: &str) -> Result<()> {
        let resp = self
            .http
            .delete(self.url(inbound, &format!("/users/{name}")))
            .send()
            .await
            .map_err(|e| AppError::new(ErrorCode::Agent, format!("SSM remove 失败: {e}")))?;
        // 204 或 404 均视为幂等成功。
        if resp.status().is_success() || resp.status().as_u16() == 404 {
            Ok(())
        } else {
            Err(AppError::new(
                ErrorCode::Agent,
                format!("SSM remove {} 返回 {}", name, resp.status()),
            ))
        }
    }
}

#[async_trait]
impl SsmClient for HttpSsmClient {
    async fn list_users(&self, inbound: &str) -> Result<Vec<String>> {
        let resp = self
            .http
            .get(self.url(inbound, "/users"))
            .send()
            .await
            .map_err(|e| AppError::new(ErrorCode::Agent, format!("SSM list 失败: {e}")))?;
        let v: serde_json::Value = resp
            .json()
            .await
            .map_err(|e| AppError::new(ErrorCode::Agent, format!("SSM list 解析失败: {e}")))?;
        Ok(v.get("users")
            .and_then(|u| u.as_array())
            .map(|a| {
                a.iter()
                    .filter_map(|x| x.get("username").and_then(|n| n.as_str()).map(String::from))
                    .collect()
            })
            .unwrap_or_default())
    }

    async fn reconcile(
        &self,
        inbound: &str,
        desired: &BTreeMap<String, String>,
    ) -> Result<ReconcileReport> {
        let current = self.list_users(inbound).await?;
        let cur: HashSet<&String> = current.iter().collect();
        let mut added = Vec::new();
        for (name, upsk) in desired {
            if !cur.contains(name) {
                self.add(inbound, name, upsk).await?;
                added.push(name.clone());
            }
        }
        let mut removed = Vec::new();
        for name in &current {
            if !desired.contains_key(name) {
                self.remove(inbound, name).await?;
                removed.push(name.clone());
            }
        }
        Ok(ReconcileReport {
            added,
            removed,
            present: desired.keys().cloned().collect(),
        })
    }

    async fn read_stats(&self, inbound: &str) -> Result<SsmStats> {
        let resp = self
            .http
            .get(self.url(inbound, "/stats"))
            .send()
            .await
            .map_err(|e| AppError::new(ErrorCode::Agent, format!("SSM stats 失败: {e}")))?;
        let v: serde_json::Value = resp
            .json()
            .await
            .map_err(|e| AppError::new(ErrorCode::Agent, format!("SSM stats 解析失败: {e}")))?;
        let g = |o: &serde_json::Value, k: &str| o.get(k).and_then(|x| x.as_i64()).unwrap_or(0);
        let users = v
            .get("users")
            .and_then(|u| u.as_array())
            .map(|a| {
                a.iter()
                    .filter_map(|x| {
                        let name = x.get("username").and_then(|n| n.as_str())?.to_string();
                        Some((name, g(x, "uplinkBytes"), g(x, "downlinkBytes")))
                    })
                    .collect()
            })
            .unwrap_or_default();
        Ok(SsmStats {
            tcp_sessions: g(&v, "tcpSessions"),
            udp_sessions: g(&v, "udpSessions"),
            users,
        })
    }
}

/// 测试实现：内存用户表。
#[derive(Default)]
pub struct MockSsmClient {
    users: std::sync::Mutex<BTreeMap<String, String>>,
}

impl MockSsmClient {
    pub fn names(&self) -> Vec<String> {
        self.users.lock().unwrap().keys().cloned().collect()
    }
    pub fn preload(&self, name: &str, upsk: &str) {
        self.users.lock().unwrap().insert(name.into(), upsk.into());
    }
}

#[async_trait]
impl SsmClient for MockSsmClient {
    async fn list_users(&self, _inbound: &str) -> Result<Vec<String>> {
        Ok(self.names())
    }
    async fn reconcile(
        &self,
        _inbound: &str,
        desired: &BTreeMap<String, String>,
    ) -> Result<ReconcileReport> {
        let mut g = self.users.lock().unwrap();
        let mut added = Vec::new();
        for (n, u) in desired {
            if !g.contains_key(n) {
                g.insert(n.clone(), u.clone());
                added.push(n.clone());
            }
        }
        let to_remove: Vec<String> = g
            .keys()
            .filter(|k| !desired.contains_key(*k))
            .cloned()
            .collect();
        let mut removed = Vec::new();
        for n in to_remove {
            g.remove(&n);
            removed.push(n);
        }
        Ok(ReconcileReport {
            added,
            removed,
            present: desired.keys().cloned().collect(),
        })
    }
    async fn read_stats(&self, _inbound: &str) -> Result<SsmStats> {
        Ok(SsmStats {
            tcp_sessions: 0,
            udp_sessions: 0,
            users: self
                .users
                .lock()
                .unwrap()
                .keys()
                .map(|n| (n.clone(), 0, 0))
                .collect(),
        })
    }
}
