//! 启动配置：不依赖数据库自身的最小信息，来自环境变量（`.env` / systemd）。
//! 业务配置一律入库；这里只放引导所需项。`ENCRYPTION_MASTER_KEY` 由 [`crate::crypto`] 读取。

use crate::error::{AppError, ErrorCode, Result};

pub struct StartupConfig {
    /// SQLite 数据库路径。
    pub database_path: String,
    /// Manager Web/API 监听地址（默认 `127.0.0.1:9736`）。
    pub manager_listen: String,
    /// Phase 6：管理面认证配置。
    pub auth: AuthConfig,
}

/// Phase 6 管理面认证配置（cookie / 会话 TTL / re-auth 窗口 / 登录节流），全部来自 env。
#[derive(Debug, Clone)]
pub struct AuthConfig {
    pub cookie_name: String,
    /// 生产必开（需 TLS 前置）；本地 http 调试可 `SECURE_COOKIES=false`。
    pub secure_cookie: bool,
    pub idle_ttl_secs: i64,
    pub absolute_ttl_secs: i64,
    pub reauth_window_secs: i64,
    pub lock_threshold: i64,
    pub lock_secs: i64,
}

impl Default for AuthConfig {
    fn default() -> Self {
        Self {
            cookie_name: "sbm_session".to_string(),
            secure_cookie: true,
            idle_ttl_secs: 3600,
            absolute_ttl_secs: 12 * 3600,
            reauth_window_secs: 300,
            lock_threshold: 5,
            lock_secs: 900,
        }
    }
}

impl AuthConfig {
    pub fn from_env() -> Self {
        let d = Self::default();
        let b = |k: &str, def: bool| {
            std::env::var(k)
                .ok()
                .map(|v| {
                    matches!(
                        v.trim().to_ascii_lowercase().as_str(),
                        "true" | "1" | "yes" | "on"
                    )
                })
                .unwrap_or(def)
        };
        let i = |k: &str, def: i64| {
            std::env::var(k)
                .ok()
                .and_then(|v| v.trim().parse().ok())
                .unwrap_or(def)
        };
        Self {
            cookie_name: std::env::var("SESSION_COOKIE_NAME").unwrap_or(d.cookie_name),
            secure_cookie: b("SECURE_COOKIES", d.secure_cookie),
            idle_ttl_secs: i("SESSION_IDLE_TTL_SECS", d.idle_ttl_secs),
            absolute_ttl_secs: i("SESSION_ABSOLUTE_TTL_SECS", d.absolute_ttl_secs),
            reauth_window_secs: i("REAUTH_WINDOW_SECS", d.reauth_window_secs),
            lock_threshold: i("LOGIN_LOCK_THRESHOLD", d.lock_threshold),
            lock_secs: i("LOGIN_LOCK_SECS", d.lock_secs),
        }
    }
}

impl StartupConfig {
    pub fn from_env() -> Result<Self> {
        let database_path = std::env::var("DATABASE_PATH")
            .map_err(|_| AppError::new(ErrorCode::Config, "缺少 DATABASE_PATH"))?;
        let manager_listen =
            std::env::var("MANAGER_LISTEN").unwrap_or_else(|_| "127.0.0.1:9736".to_string());
        Ok(Self {
            database_path,
            manager_listen,
            auth: AuthConfig::from_env(),
        })
    }
}
