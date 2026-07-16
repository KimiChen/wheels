//! Agent 启动配置（env）。`AGENT_ENROLLMENT_PATH` 必填；绑定默认回环，SSM 固定回环。

use crate::error::{AppError, ErrorCode, Result};

pub struct AgentConfig {
    /// enrollment 包文件路径（Manager 带外交付；含 Agent 服务端证书与私钥）。
    pub enrollment_path: String,
    /// Agent mTLS 监听地址（默认 `127.0.0.1:39736`；绑非回环需显式设置且受防火墙限制）。
    pub bind_address: String,
    /// Agent 本地库路径。
    pub state_path: String,
    /// 本机 SSM API 地址（固定回环；Agent 不接受 Manager 覆盖）。
    pub ssm_address: String,
    /// live 配置与 revisions/ 快照目录（部署原子替换目标）。
    pub config_dir: String,
}

impl AgentConfig {
    pub fn from_env() -> Result<Self> {
        let enrollment_path = std::env::var("AGENT_ENROLLMENT_PATH")
            .map_err(|_| AppError::new(ErrorCode::Config, "缺少 AGENT_ENROLLMENT_PATH"))?;
        let bind_address =
            std::env::var("AGENT_BIND_ADDRESS").unwrap_or_else(|_| "127.0.0.1:39736".to_string());
        let state_path =
            std::env::var("AGENT_STATE_PATH").unwrap_or_else(|_| "agent-state.db".to_string());
        let ssm_address =
            std::env::var("AGENT_SSM_ADDRESS").unwrap_or_else(|_| "127.0.0.1:49736".to_string());
        let config_dir = std::env::var("AGENT_CONFIG_DIR")
            .unwrap_or_else(|_| "/var/lib/sing-box-manager".to_string());
        Ok(Self {
            enrollment_path,
            bind_address,
            state_path,
            ssm_address,
            config_dir,
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn defaults_apply_when_optional_unset() {
        std::env::set_var("AGENT_ENROLLMENT_PATH", "/tmp/e.json");
        std::env::remove_var("AGENT_BIND_ADDRESS");
        std::env::remove_var("AGENT_SSM_ADDRESS");
        let c = AgentConfig::from_env().unwrap();
        assert_eq!(c.bind_address, "127.0.0.1:39736");
        assert_eq!(c.ssm_address, "127.0.0.1:49736");
        assert_eq!(c.enrollment_path, "/tmp/e.json");
    }
}
