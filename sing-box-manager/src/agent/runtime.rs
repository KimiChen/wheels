//! sing-box 进程运行时抽象。生产 [`ProcessRuntime`] 受控 stop/start 子进程 + 健康探测；
//! 测试 [`MockRuntime`] 脚本化返回值。**只抽象在沙箱/CI 无法稳定跑的进程生命周期**——sha 校验、
//! 真实 sing-box check、原子文件替换、local_revisions 记账、回滚编排留在 deploy 核心用真实文件测。

use std::collections::VecDeque;
use std::sync::Mutex;

use async_trait::async_trait;

use crate::error::{AppError, ErrorCode, Result};

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Health {
    Ok,
    Down(String),
}

#[async_trait]
pub trait Runtime: Send + Sync {
    /// 受控 stop 旧 sing-box → start 新进程加载 `config_path`（新 epoch）。固定 argv，绝无任意 shell。
    async fn restart(&self, config_path: &str, epoch: i64) -> Result<()>;
    /// 本机健康探测（进程存活 + SSM 端口可达）。
    async fn health_check(&self) -> Result<Health>;
}

/// 生产实现：受控子进程 + SSM 探活。
pub struct ProcessRuntime {
    ssm_address: String,
    child: Mutex<Option<std::process::Child>>,
}

impl ProcessRuntime {
    pub fn new(ssm_address: String) -> Self {
        Self {
            ssm_address,
            child: Mutex::new(None),
        }
    }
    fn singbox_bin() -> String {
        std::env::var("SINGBOX_BIN").unwrap_or_else(|_| "/opt/homebrew/bin/sing-box".to_string())
    }
}

#[async_trait]
impl Runtime for ProcessRuntime {
    async fn restart(&self, config_path: &str, _epoch: i64) -> Result<()> {
        // 停旧进程（受控 kill）。
        if let Some(mut old) = self.child.lock().unwrap().take() {
            let _ = old.kill();
            let _ = old.wait();
        }
        // 起新进程（固定 argv：sing-box run -c <config>）。
        let child = std::process::Command::new(Self::singbox_bin())
            .arg("run")
            .arg("-c")
            .arg(config_path)
            .spawn()
            .map_err(|e| {
                AppError::new(ErrorCode::Deployment, format!("启动 sing-box 失败: {e}"))
            })?;
        *self.child.lock().unwrap() = Some(child);
        // 给进程一点启动时间。
        tokio::time::sleep(std::time::Duration::from_millis(400)).await;
        Ok(())
    }

    async fn health_check(&self) -> Result<Health> {
        // 子进程仍存活？
        if let Some(child) = self.child.lock().unwrap().as_mut() {
            if let Ok(Some(status)) = child.try_wait() {
                return Ok(Health::Down(format!("进程已退出: {status}")));
            }
        }
        if super::singbox::probe_running(&self.ssm_address).await {
            Ok(Health::Ok)
        } else {
            Ok(Health::Down("SSM 端口不可达".into()))
        }
    }
}

/// 测试实现：脚本化 restart/health，记录调用序列与 epoch。
#[derive(Default)]
pub struct MockRuntime {
    restart_ok: Mutex<VecDeque<bool>>,
    healths: Mutex<VecDeque<Health>>,
    pub calls: Mutex<Vec<String>>,
}

impl MockRuntime {
    pub fn push_restart(&self, ok: bool) {
        self.restart_ok.lock().unwrap().push_back(ok);
    }
    pub fn push_health(&self, h: Health) {
        self.healths.lock().unwrap().push_back(h);
    }
    pub fn call_log(&self) -> Vec<String> {
        self.calls.lock().unwrap().clone()
    }
}

#[async_trait]
impl Runtime for MockRuntime {
    async fn restart(&self, config_path: &str, epoch: i64) -> Result<()> {
        self.calls
            .lock()
            .unwrap()
            .push(format!("restart:epoch={epoch}:{config_path}"));
        if self.restart_ok.lock().unwrap().pop_front().unwrap_or(true) {
            Ok(())
        } else {
            Err(AppError::new(ErrorCode::Deployment, "mock restart 失败"))
        }
    }
    async fn health_check(&self) -> Result<Health> {
        self.calls.lock().unwrap().push("health".into());
        Ok(self
            .healths
            .lock()
            .unwrap()
            .pop_front()
            .unwrap_or(Health::Ok))
    }
}
