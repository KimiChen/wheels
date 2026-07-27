//! 结算屏障排空闸门。停旧进程前礼让在途会话（有界超时）。**排空非字节正确性前提**——
//! 最终统计取的是累计计数器，与会话是否活跃无关；排空仅为优雅收尾。故超时即放行（forced）。

use std::time::Duration;

use async_trait::async_trait;

use crate::agent::ssm::{SsmClient, INBOUND_TAG};
use crate::error::Result;

#[async_trait]
pub trait DrainWaitGate: Send + Sync {
    /// 轮询直到无活跃会话或超时。返回是否干净排空（false=超时强切）。
    async fn drain(&self, ssm: &dyn SsmClient) -> Result<bool>;
}

/// 轮询 SSM 会话数直到归零或超时。
pub struct SsmDrainGate {
    pub timeout: Duration,
    pub poll: Duration,
}

impl Default for SsmDrainGate {
    fn default() -> Self {
        Self {
            timeout: Duration::from_secs(5),
            poll: Duration::from_millis(500),
        }
    }
}

#[async_trait]
impl DrainWaitGate for SsmDrainGate {
    async fn drain(&self, ssm: &dyn SsmClient) -> Result<bool> {
        let deadline = tokio::time::Instant::now() + self.timeout;
        loop {
            let s = ssm.read_stats(INBOUND_TAG).await?;
            if s.tcp_sessions + s.udp_sessions <= 0 {
                return Ok(true);
            }
            if tokio::time::Instant::now() >= deadline {
                return Ok(false); // 强切：仍有会话，但最终统计仍精确，不丢字节。
            }
            tokio::time::sleep(self.poll).await;
        }
    }
}

/// 测试用：立即返回预设 drain_clean，不轮询不睡眠。
#[cfg(test)]
pub struct MockDrainGate {
    pub clean: bool,
}

#[cfg(test)]
#[async_trait]
impl DrainWaitGate for MockDrainGate {
    async fn drain(&self, _ssm: &dyn SsmClient) -> Result<bool> {
        Ok(self.clean)
    }
}
