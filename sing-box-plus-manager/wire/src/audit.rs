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

/// 轮转后缀的定宽时间戳，形如 `20260919T041530.123456789Z`。
///
/// 节点写的是 `file.path + "." + time.Format("20060102T150405.000000000Z")`
/// （`internal/userstats/audit.go:525-532`）。两件事必须记住：
///
/// * 它**自带一个点**（纳秒的小数点），所以「按最后一个点切文件名」是错的，
///   要按 `.jsonl.` 切。
/// * 它是**定宽**的，字典序因此等于时间序——节点自己也依赖这一点。
///   变长的时间戳（比如 `.1758153600`）排起来就不是时间序了，
///   而那种夹具会让「字典序即时间序」这条性质在用例里恰好不被检验。
pub const STAMP_LEN: usize = 26;

/// 文件名里把身份段与轮转时间戳分开的那一段。
pub const ROTATED_SEPARATOR: &str = ".jsonl.";

/// 这一段是不是一个合法的轮转时间戳。定宽校验，不解析成时间。
pub fn is_rotation_stamp(stamp: &str) -> bool {
    let bytes = stamp.as_bytes();
    if bytes.len() != STAMP_LEN {
        return false;
    }
    let digits = |mut range: std::ops::Range<usize>| range.all(|i| bytes[i].is_ascii_digit());
    digits(0..8)
        && bytes[8] == b'T'
        && digits(9..15)
        && bytes[15] == b'.'
        && digits(16..25)
        && bytes[25] == b'Z'
}

/// 一个审计文件名的两种形态。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum NameKind {
    /// `access-<b64>.jsonl`——正在被写。
    ///
    /// 节点以 `O_CREATE|O_RDWR|O_APPEND` 打开它，且 `repairTrailingNewline`
    /// 只**补一个换行符、从不截断**（`internal/userstats/audit.go:650-670`）。
    /// 所以它是严格只追加的：按字节偏移增量读、只取到最后一个换行符处，
    /// 读到的就永远是完整行。
    ///
    /// 它唯一会「变短」的时刻是轮转——那是一次 `mv` 加一个新建的同名空文件，
    /// 读取方必须把「大小小于我记的偏移」当成轮转信号而不是错误。
    Active,
    /// `access-<b64>.jsonl.<定宽时间戳>`——已轮转，不再变化。
    Rotated,
}

/// 认出一个审计文件名，并给出它的身份段（base64url，未解码）。
///
/// 校验的是**形状**，不是授权（C35）：文件名只用来定位，逐行的完整身份校验与
/// 归属裁剪在主控侧做。但形状本身要严——这个串会被拼进路径：
/// 身份段只允许 base64url 字母表（里面没有 `/`、没有 `.`，所以拼不出 `..`）。
pub fn name_kind(name: &str) -> Option<(NameKind, &str)> {
    let rest = name.strip_prefix(FILE_PREFIX)?;
    let (segment, kind) = match rest.split_once(ROTATED_SEPARATOR) {
        Some((segment, stamp)) => {
            // 分隔符只许出现一次，时间戳只许是定宽那一种。
            if stamp.contains(ROTATED_SEPARATOR) || !is_rotation_stamp(stamp) {
                return None;
            }
            (segment, NameKind::Rotated)
        }
        None => (rest.strip_suffix(FILE_SUFFIX)?, NameKind::Active),
    };
    if segment.is_empty() || !segment.bytes().all(|b| ALPHABET.contains(&b)) {
        return None;
    }
    Some((kind, segment))
}

/// 这个文件名是不是一个**已轮转**的审计文件——**不需要先知道它属于谁**。
///
/// 同步器要的是这一支：它按文件增量拉整个目录，一次往返拿到全部身份的文件，
/// 而不是拿着 301 个身份名各问一轮。`is_rotated_file` 那一支需要先有身份名，
/// 用在「这个文件是不是这个人的」这种判断上。
///
/// 校验的是**形状**，不是授权（C35）：文件名只用来定位，逐行的完整身份校验与
/// 归属裁剪在主控侧做。但形状本身要严——这个串会被拼进路径：
/// 身份段只允许 base64url 字母表（里面没有 `/`、没有 `.`，所以拼不出 `..`）。
pub fn is_rotated_name(name: &str) -> bool {
    rotated_identity_segment(name).is_some()
}

/// 已轮转文件名里的身份段（base64url，未解码）。不是已轮转文件则 `None`。
pub fn rotated_identity_segment(name: &str) -> Option<&str> {
    match name_kind(name) {
        Some((NameKind::Rotated, segment)) => Some(segment),
        _ => None,
    }
}

/// 这个文件名是不是该身份的**已轮转**文件。
///
/// 已轮转文件形如 `access-<b64>.jsonl.<时间戳>`；活动文件没有那个后缀。
/// C23：审计文件轮转**只许 `mv`，不许 `rm`**，而读取只许读已轮转文件——
/// 活动文件正在被写，读它拿到的可能是半行。
pub fn is_rotated_file(name: &str, identity: &str) -> bool {
    rotated_identity_segment(name)
        .is_some_and(|segment| segment == base64url_nopad(identity.as_bytes()))
}

/// 这个文件名是不是该身份的活动文件。
pub fn is_active_file(name: &str, identity: &str) -> bool {
    name == active_file_name(identity)
}

/// **只取完整行。**
///
/// 活动文件是只追加的，但最后一行可能正写到一半。截到最后一个 `\n`（含）为止，
/// 返回的字节数同时就是「下一次该从哪里接着读」的增量——所以调用方推进偏移时
/// 用的是**真正收下的字节数**，不是它请求的范围。
///
/// 一个换行符都没有时返回空：那说明整段都是半行，一个字节都不能要。
pub fn whole_lines(bytes: &[u8]) -> &[u8] {
    match bytes.iter().rposition(|&b| b == b'\n') {
        Some(index) => &bytes[..=index],
        None => &[],
    }
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

        let rotated = format!("{active}.20260919T041530.123456789Z");
        assert!(is_rotated_file(&rotated, identity));
        assert!(!is_active_file(&rotated, identity));

        // 别人的文件不算。
        assert!(!is_rotated_file(
            &format!("{}.20260919T041530.123456789Z", active_file_name("other")),
            identity
        ));
        // 只是前缀相同但没有 '.' 分隔的也不算。
        assert!(!is_rotated_file(&format!("{active}x"), identity));
    }

    #[test]
    fn 轮转时间戳是定宽的() {
        assert!(is_rotation_stamp("20260919T041530.123456789Z"));
        // 变长的整数时间戳不接受。夹具里用过 `.1758153600`，而那种串排起来
        // **不是时间序**——「字典序即时间序」只对定宽成立，节点自己也依赖这一点。
        assert!(!is_rotation_stamp("1758153600"));
        assert!(!is_rotation_stamp("20260919T041530.12345678Z"), "纳秒必须是 9 位");
        assert!(!is_rotation_stamp("20260919T041530.123456789"), "必须以 Z 结尾");
        assert!(!is_rotation_stamp("2026-09-19T04:15:30.123456789Z"), "不是 RFC 3339");
    }

    #[test]
    fn 定宽时间戳的字典序就是时间序() {
        // 这条是节点侧轮转顺序的地基。用变长时间戳做夹具会让它恰好不被检验：
        // 字符串比较下 "9" > "10"，而 9 比 10 早。
        let mut stamps = vec![
            "20260919T041530.000000010Z",
            "20260919T041530.000000009Z",
            "20260918T235959.999999999Z",
        ];
        stamps.sort();
        assert_eq!(
            stamps,
            vec![
                "20260918T235959.999999999Z",
                "20260919T041530.000000009Z",
                "20260919T041530.000000010Z",
            ]
        );
    }

    #[test]
    fn 不需要身份名也能认出已轮转文件() {
        let name = format!("{}.20260919T041530.123456789Z", active_file_name("u_example_01"));
        assert!(is_rotated_name(&name));
        assert_eq!(rotated_identity_segment(&name), Some("dV9leGFtcGxlXzAx"));
    }

    #[test]
    fn 认得出活动文件与已轮转文件() {
        let active = active_file_name("u_example_01");
        assert_eq!(name_kind(&active), Some((NameKind::Active, "dV9leGFtcGxlXzAx")));
        let rotated = format!("{active}.20260919T041530.123456789Z");
        assert_eq!(name_kind(&rotated), Some((NameKind::Rotated, "dV9leGFtcGxlXzAx")));
        // 两者的身份段相同——同步器靠这一点把一个人的活动文件与归档串起来。
        assert_eq!(name_kind(&active).unwrap().1, name_kind(&rotated).unwrap().1);
    }

    #[test]
    fn 只取完整行() {
        assert_eq!(whole_lines(b"a\nb\n"), b"a\nb\n");
        // 最后一行写到一半：丢掉它，返回的长度就是可以推进的偏移量。
        assert_eq!(whole_lines(b"a\nb\nhal"), b"a\nb\n");
        // 一个换行符都没有：整段都是半行，一个字节都不能要。
        assert_eq!(whole_lines(b"half"), b"");
        assert_eq!(whole_lines(b""), b"");
        // 推进量必须等于收下的字节数，否则下一轮会重读或漏读。
        let raw = b"one\ntwo\nthr";
        let kept = whole_lines(raw);
        assert_eq!(&raw[..kept.len()], kept);
    }

    #[test]
    fn 形状不对的名字一律拒绝() {
        let stamp = "20260919T041530.123456789Z";
        for name in [
            // 路径穿越的几种写法。身份段只许 base64url，里面没有 `/` 也没有 `.`。
            format!("../../etc/passwd.jsonl.{stamp}"),
            format!("access-../../etc/passwd.jsonl.{stamp}"),
            format!("access-a/b.jsonl.{stamp}"),
            format!("access-...jsonl.{stamp}"),
            // 活动文件是合法名字，但不是**已轮转**文件。
            // 前缀不对。
            format!("other-dV9leGFtcGxlXzAx.jsonl.{stamp}"),
            // 身份段为空。
            format!("access-.jsonl.{stamp}"),
            // 时间戳变长——`.1` 这种排起来不是时间序。
            "access-dV9leGFtcGxlXzAx.jsonl.1".to_string(),
            "access-dV9leGFtcGxlXzAx.jsonl.1758153600".to_string(),
            // 分隔符出现两次。
            format!("access-dV9leGFtcGxlXzAx.jsonl.x.jsonl.{stamp}"),
            // 时间戳后面还有东西。
            format!("access-dV9leGFtcGxlXzAx.jsonl.{stamp}.gz"),
        ] {
            assert!(!is_rotated_name(&name), "这个名字不该被接受：{name}");
            assert!(rotated_identity_segment(&name).is_none(), "{name}");
            assert!(name_kind(&name).is_none(), "连活动文件都算不上：{name}");
        }
        // 活动文件单独一条：它是合法名字，但**不是已轮转文件**。
        let active = active_file_name("u_example_01");
        assert!(!is_rotated_name(&active));
        assert_eq!(name_kind(&active).map(|k| k.0), Some(NameKind::Active));
    }
}
