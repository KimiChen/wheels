//! 本机 sing-box 只读探测：版本解析、运行态探测。均为预定义操作，绝不接受 Manager 传入的任意命令。

use std::time::Duration;

/// 解析 `sing-box version` 输出的版本号。形如 `sing-box version 1.13.14 ...`。
pub fn parse_version(output: &str) -> Option<String> {
    for line in output.lines() {
        if let Some(rest) = line.trim().strip_prefix("sing-box version ") {
            return rest.split_whitespace().next().map(|s| s.to_string());
        }
    }
    None
}

/// best-effort 执行 `sing-box version`（固定 argv，无任意参数）。未安装返回 None。
pub fn detect_version() -> Option<String> {
    let out = std::process::Command::new("sing-box")
        .arg("version")
        .output()
        .ok()?;
    parse_version(&String::from_utf8_lossy(&out.stdout))
}

/// 探测 sing-box 运行态：能否连上本机 SSM 端口（best-effort）。
pub async fn probe_running(ssm_address: &str) -> bool {
    matches!(
        tokio::time::timeout(
            Duration::from_millis(300),
            tokio::net::TcpStream::connect(ssm_address),
        )
        .await,
        Ok(Ok(_))
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_version_line() {
        assert_eq!(
            parse_version("sing-box version 1.13.14\n\nEnvironment: go1.22"),
            Some("1.13.14".to_string())
        );
        assert_eq!(
            parse_version("  sing-box version 1.12.0-beta.1 (extra)"),
            Some("1.12.0-beta.1".to_string())
        );
        assert_eq!(parse_version("no version here"), None);
    }
}
