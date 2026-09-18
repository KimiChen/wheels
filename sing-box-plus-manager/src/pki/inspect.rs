//! 证书到期巡检（`docs/threat-model.md` §2.4）。
//!
//! 三条纪律：
//!
//! - **只读公钥叶子。** 为此把祖先目录设成可穿越但不可列举、被巡检的公钥设成可读，
//!   **私钥权限不变**。
//! - **绝不为了让巡检能读而把采集身份加进持有私钥的组**——那等于抹掉 C18 想保住的分界线。
//! - **输出不得包含 PEM、私钥或证书主题。** 巡检结果会进日志与告警，
//!   而告警是最容易被转发到一个权限比它低的地方去的东西。

use std::path::{Path, PathBuf};

use crate::error::{Error, Result};

/// 两级阈值。缺失 / 损坏 / 未生效 / 已过期一律 critical。
#[derive(Debug, Clone, Copy)]
pub struct Thresholds {
    pub warning_days: i64,
    pub critical_days: i64,
}

impl Default for Thresholds {
    fn default() -> Self {
        Thresholds { warning_days: 30, critical_days: 7 }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord)]
pub enum Level {
    Ok,
    Warning,
    Critical,
}

#[derive(Debug, Clone)]
pub struct Finding {
    /// 只记路径，不记证书主题。
    pub path: PathBuf,
    pub level: Level,
    pub days_left: Option<i64>,
    /// 给人看的原因。**不含 PEM、私钥或主题**。
    pub reason: String,
}

/// 巡检一批公钥叶子。
///
/// 传进来的必须是**公证书**路径。函数本身不会去读同目录下的私钥——
/// 这一点由调用方保证，而调用方就是 CLI，它只接受 `*.pem`。
pub fn inspect(paths: &[PathBuf], thresholds: Thresholds, now_unix: i64) -> Vec<Finding> {
    paths.iter().map(|path| inspect_one(path, thresholds, now_unix)).collect()
}

fn inspect_one(path: &Path, thresholds: Thresholds, now_unix: i64) -> Finding {
    let critical = |reason: &str| Finding {
        path: path.to_path_buf(),
        level: Level::Critical,
        days_left: None,
        reason: reason.to_string(),
    };

    let Ok(certs) = super::load_certs(path) else {
        // 缺失或损坏一律 critical，不降级成 warning——
        // 「读不到」和「还早着呢」是完全不同的两件事。
        return critical("缺失或无法解析");
    };
    let Some(der) = certs.first() else {
        return critical("文件里没有证书");
    };

    use x509_parser::prelude::*;
    let Ok((_, cert)) = X509Certificate::from_der(der) else {
        return critical("不是合法 DER");
    };
    let not_before = cert.validity().not_before.timestamp();
    let not_after = cert.validity().not_after.timestamp();

    if now_unix < not_before {
        return critical("尚未生效");
    }
    if now_unix > not_after {
        return critical("已过期");
    }
    let days_left = (not_after - now_unix) / 86_400;
    let level = if days_left <= thresholds.critical_days {
        Level::Critical
    } else if days_left <= thresholds.warning_days {
        Level::Warning
    } else {
        Level::Ok
    };
    Finding {
        path: path.to_path_buf(),
        level,
        days_left: Some(days_left),
        reason: format!("剩余 {days_left} 天"),
    }
}

/// 巡检结果的最高级别。给 CLI 的退出码用。
///
/// 注意 C22：**退出码不是健康证据**。这个退出码只说明「这次巡检看到了什么」，
/// 不说明证书链在真实握手里还能不能用——那要靠真实组件的正负矩阵。
pub fn worst(findings: &[Finding]) -> Level {
    findings.iter().map(|f| f.level).max().unwrap_or(Level::Ok)
}

/// 巡检输出不得包含 PEM、私钥或证书主题——这条做成可执行的断言。
pub fn assert_output_is_safe(rendered: &str) -> Result<()> {
    for needle in ["-----BEGIN", "PRIVATE KEY", "CN=", "subject="] {
        if rendered.contains(needle) {
            return Err(Error::Pki(format!("巡检输出含敏感片段 {needle:?}")));
        }
    }
    Ok(())
}
