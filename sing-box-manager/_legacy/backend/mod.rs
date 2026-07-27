//! 后端抽象：认证身份增删 + 流量读取，两种机制。
//! - SsmBackend（ssm.rs）：SS-2022 managed 入站 + SSM API，运行态身份变化零 reload。
//! - ReloadBackend（reload.rs）：VLESS/静态 SS 身份，改配置 users[] + reload_cmd，统计走 v2ray_api gRPC。

pub mod reload;
pub mod ssm;

use anyhow::Result;
use async_trait::async_trait;
use std::collections::BTreeMap;

/// 某 scope 里一个认证身份的累计流量。SSM: scope=入站 tag；reload: scope="*"。
#[derive(Debug, Clone)]
pub struct UserStat {
    pub name: String,
    pub scope: String,
    pub up: u64,
    pub down: u64,
}

/// 期望的运行态身份集：入站 tag -> (内部认证名 -> 凭据[SS 的 uPSK / VLESS 的 UUID])。
pub type Desired = BTreeMap<String, BTreeMap<String, String>>;

#[async_trait]
pub trait Backend: Send + Sync {
    /// 每 (用户, scope) 累计流量。
    async fn read_stats(&self) -> Result<Vec<UserStat>>;
    /// 让运行态认证身份集对齐 desired。
    async fn apply(&self, desired: &Desired) -> Result<()>;
}
