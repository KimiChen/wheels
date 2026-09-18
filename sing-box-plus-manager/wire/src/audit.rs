//! 审计文件的命名规则（镜像节点侧 `internal/userstats/audit_file.go`）。
//!
//! 节点**不能直接用身份名当文件名**：计费身份名只要求「每字节为 ASCII 可显示非空白字符」，
//! 于是 `../../tmp/x` 是一个完全合法的身份名，直接拼进路径就是路径穿越。
//! 节点用 base64url（RFC 4648 §5，无填充）做映射——标准 base64 的字母表里有 `/`，
//! 正好是路径分隔符。
//!
//! 主控与 agent 都要按这套规则定位文件，所以它在共享 crate 里。
//! **但文件名不是授权依据**（C35）：逐行的完整身份校验与归属裁剪在主控侧做。

pub const FILE_PREFIX: &str = "access-";
pub const FILE_SUFFIX: &str = ".jsonl";

const ALPHABET: &[u8; 64] = b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_";

/// 身份名 → 活动文件名。
pub fn active_file_name(identity: &str) -> String {
    format!("{FILE_PREFIX}{}{FILE_SUFFIX}", base64url_nopad(identity.as_bytes()))
}

/// 这个文件名是不是该身份的**已轮转**文件。
///
/// 已轮转文件形如 `access-<b64>.jsonl.<时间戳>`；活动文件没有那个后缀。
/// C23：审计文件轮转**只许 `mv`，不许 `rm`**，而读取只许读已轮转文件——
/// 活动文件正在被写，读它拿到的可能是半行。
pub fn is_rotated_file(name: &str, identity: &str) -> bool {
    let active = active_file_name(identity);
    name.starts_with(&active) && name.len() > active.len() && name.as_bytes()[active.len()] == b'.'
}

/// 这个文件名是不是该身份的活动文件。
pub fn is_active_file(name: &str, identity: &str) -> bool {
    name == active_file_name(identity)
}

fn base64url_nopad(input: &[u8]) -> String {
    let mut out = String::with_capacity(input.len().div_ceil(3) * 4);
    for chunk in input.chunks(3) {
        let b = [chunk[0], *chunk.get(1).unwrap_or(&0), *chunk.get(2).unwrap_or(&0)];
        let n = ((b[0] as u32) << 16) | ((b[1] as u32) << 8) | b[2] as u32;
        out.push(ALPHABET[(n >> 18) as usize & 63] as char);
        out.push(ALPHABET[(n >> 12) as usize & 63] as char);
        if chunk.len() > 1 {
            out.push(ALPHABET[(n >> 6) as usize & 63] as char);
        }
        if chunk.len() > 2 {
            out.push(ALPHABET[n as usize & 63] as char);
        }
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn 路径穿越的身份名编码后穿越不了() {
        let name = active_file_name("../../tmp/x");
        assert!(!name.contains('/'), "编码后不得含路径分隔符：{name}");
        assert!(!name.contains(".."), "编码后不得拼出 ..：{name}");
    }

    #[test]
    fn 区分活动文件与已轮转文件() {
        let identity = "u_example_01";
        let active = active_file_name(identity);
        assert!(is_active_file(&active, identity));
        assert!(!is_rotated_file(&active, identity), "活动文件不是已轮转文件");

        let rotated = format!("{active}.1758153600");
        assert!(is_rotated_file(&rotated, identity));
        assert!(!is_active_file(&rotated, identity));

        // 别人的文件不算。
        assert!(!is_rotated_file(&format!("{}.1", active_file_name("other")), identity));
        // 只是前缀相同但没有 '.' 分隔的也不算。
        assert!(!is_rotated_file(&format!("{active}x"), identity));
    }
}
