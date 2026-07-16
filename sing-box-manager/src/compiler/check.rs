//! 真实 `sing-box check`：明文写 0700 临时目录内的 0600 文件（权限先于内容）→ 固定 argv 执行 →
//! 退出码判定（0=通过；实测坏 detour 不失败故失败路径须用 unsupported method）。RAII 清理临时文件，
//! stderr 入库前脱敏本次涉及的 PSK 明文。

use std::io::Write;
use std::path::{Path, PathBuf};
use std::process::Command;

use crate::error::{AppError, ErrorCode, Result};

/// sing-box 二进制路径（env `SINGBOX_BIN` 覆盖；Phase 3 随 Agent 主机配置化）。
fn singbox_bin() -> String {
    std::env::var("SINGBOX_BIN").unwrap_or_else(|_| "/opt/homebrew/bin/sing-box".to_string())
}

pub struct CheckResult {
    pub passed: bool,
    pub output: String,
}

/// 对一段 sing-box JSON 明文跑 check。`redact`：本次涉及的明文密钥集，出现在 stderr 即替换。
pub fn check_config(plaintext: &[u8], redact: &[String]) -> Result<CheckResult> {
    let dir = TempDir::new()?;
    let path = dir.path().join("cfg.json");
    write_private(&path, plaintext)?;

    let out = Command::new(singbox_bin())
        .arg("check")
        .arg("-c")
        .arg(&path)
        .output()
        .map_err(|e| AppError::new(ErrorCode::Internal, format!("执行 sing-box 失败: {e}")))?;

    let passed = out.status.success();
    let mut stderr = String::from_utf8_lossy(&out.stderr).into_owned();
    for secret in redact {
        if !secret.is_empty() {
            stderr = stderr.replace(secret.as_str(), "***REDACTED***");
        }
    }
    Ok(CheckResult {
        passed,
        output: stderr,
    })
    // dir Drop 在此删除临时目录与文件。
}

fn write_private(path: &Path, bytes: &[u8]) -> Result<()> {
    let mut opts = std::fs::OpenOptions::new();
    opts.write(true).create_new(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        opts.mode(0o600); // 权限先于内容，防竞态读取
    }
    let mut f = opts
        .open(path)
        .map_err(|e| AppError::new(ErrorCode::Internal, format!("建临时配置失败: {e}")))?;
    f.write_all(bytes)
        .map_err(|e| AppError::new(ErrorCode::Internal, format!("写临时配置失败: {e}")))?;
    Ok(())
}

/// 0700 临时目录，Drop 时递归删除（panic 也清）。
struct TempDir {
    path: PathBuf,
}

impl TempDir {
    fn new() -> Result<Self> {
        let p = std::env::temp_dir().join(format!("sbm-check-{}", uuid::Uuid::new_v4()));
        std::fs::create_dir(&p)
            .map_err(|e| AppError::new(ErrorCode::Internal, format!("建临时目录失败: {e}")))?;
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            let _ = std::fs::set_permissions(&p, std::fs::Permissions::from_mode(0o700));
        }
        Ok(Self { path: p })
    }
    fn path(&self) -> &Path {
        &self.path
    }
}

impl Drop for TempDir {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.path);
    }
}

/// 从配置 JSON 收集 password/username 值，供 check/日志脱敏（无需接触 SecretBundle）。
pub fn secret_values_in(plaintext: &[u8]) -> Vec<String> {
    fn collect(v: &serde_json::Value, out: &mut Vec<String>) {
        match v {
            serde_json::Value::Object(m) => {
                for (k, val) in m {
                    if (k == "password" || k == "username") && val.is_string() {
                        out.push(val.as_str().unwrap().to_string());
                    } else {
                        collect(val, out);
                    }
                }
            }
            serde_json::Value::Array(a) => a.iter().for_each(|e| collect(e, out)),
            _ => {}
        }
    }
    let mut out = Vec::new();
    if let Ok(v) = serde_json::from_slice::<serde_json::Value>(plaintext) {
        collect(&v, &mut out);
    }
    out
}

/// sing-box 二进制是否可用（测试用于在缺二进制的机器上跳过真实 check）。
pub fn available() -> bool {
    Command::new(singbox_bin())
        .arg("version")
        .output()
        .map(|o| o.status.success())
        .unwrap_or(false)
}

#[cfg(test)]
mod tests {
    use super::*;
    use base64::{engine::general_purpose::STANDARD, Engine};
    use serde_json::json;

    fn minimal_entry(method: &str) -> Vec<u8> {
        let psk = STANDARD.encode([1u8; 16]);
        serde_json::to_vec(&json!({
            "log": {"level": "warn"},
            "dns": {"servers": [{"tag": "b", "type": "udp", "server": "1.1.1.1"}], "final": "b"},
            "inbounds": [{"type": "shadowsocks", "tag": "in-shared", "listen": "::",
                "listen_port": 19736, "method": method, "password": psk, "managed": true}],
            "outbounds": [{"type": "direct", "tag": "direct"}, {"type": "block", "tag": "block"}],
            "route": {"rules": [{"action": "sniff"}], "final": "block"},
            "services": [{"type": "ssm-api", "listen": "127.0.0.1", "listen_port": 49736,
                "servers": {"/in-shared": "in-shared"}, "cache_path": "/var/lib/sing-box-manager/ssm-cache.json"}],
        }))
        .unwrap()
    }

    #[test]
    fn real_check_passes_managed_ssm_empty_auth_user() {
        if !available() {
            eprintln!("skip: sing-box 不可用");
            return;
        }
        let r = check_config(&minimal_entry("2022-blake3-aes-128-gcm"), &[]).unwrap();
        assert!(
            r.passed,
            "managed+ssm-api+空 auth_user 应过 check: {}",
            r.output
        );
    }

    #[test]
    fn real_check_fails_unsupported_method_with_redaction() {
        if !available() {
            eprintln!("skip: sing-box 不可用");
            return;
        }
        // 坏 detour 不会失败（实测），故失败路径用 unsupported method。
        let r = check_config(
            &minimal_entry("not-a-real-method"),
            &["hunter2".to_string()],
        )
        .unwrap();
        assert!(!r.passed);
        assert!(
            r.output.contains("method") || r.output.contains("FATAL"),
            "应捕获 stderr: {}",
            r.output
        );
        assert!(!r.output.contains("hunter2")); // 脱敏（此处虽不含，兜底断言）
    }
}
