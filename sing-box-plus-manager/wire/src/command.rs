//! 封闭命令集（README §4.2）。
//!
//! agent 只认这三条，**没有 `exec`、没有 shell 模板、没有任意文件路径**。
//! 命令集是封闭枚举这件事是 agent 相对反代唯一增加的攻击面，所以它必须真的封闭：
//! 用 `deny_unknown_fields` + 无兜底分支的枚举，不认识的命令在反序列化时就失败，
//! 而不是进到一个 `match` 的 `_ =>` 里。
//!
//! 只有**一条**路由。命令写在签名覆盖的 body 里，不写在路径上——
//! 路径上的命令会诱导出「按前缀匹配」「忽略大小写」这类宽容处理，
//! 而那正是封闭枚举想避免的。

use serde::{Deserialize, Serialize};

/// agent 的唯一路由。方法与路径都进 HMAC 的 canonical 编码。
pub const COMMAND_PATH: &str = "/v1/command";
pub const COMMAND_METHOD: &str = "POST";

/// 上游（节点本机 UDS）响应的状态码，随 agent 响应头回传。
///
/// agent **字节透传**上游响应体，自身的 HTTP 状态只表示「这条命令有没有被执行」：
/// agent 级失败（签名不对、命令不认识、单飞被挡）用 4xx，
/// 上游的 429 / 404 则是 agent 200 + 这个头里的 429 / 404。
/// 两者对采集端的处置相同（可重试且不得入账），但归因完全不同，不能混成一个码。
pub const UPSTREAM_STATUS_HEADER: &str = "x-pm-upstream-status";

/// **`Snapshot` 必须写成 `Snapshot {}` 而不是 `Snapshot`。**
///
/// 内部 tag 下的 **unit 变体不受 `deny_unknown_fields` 约束**：
/// `{"command":"snapshot","argv":["sh"]}` 会被静默接受，多出来的键直接丢掉。
/// 空的 struct 变体才会走字段集合校验。这是实测出来的，不是从文档推的——
/// 一个「看起来已经封闭」的枚举比一个明说不封闭的枚举更危险。
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(tag = "command", rename_all = "kebab-case", deny_unknown_fields)]
pub enum AgentCommand {
    /// 打快照 socket，字节透传响应。只读档。
    ///
    /// **单飞**：同一节点同一时刻只允许一条 in-flight 采集，第二条由 agent 本地 429，
    /// 不去撞节点的并发上限。C7 的 429 是「可重试且不得入账」，本地挡掉能减少这类事件。
    Snapshot {},

    /// 把主控给的 body 原样 `PUT` 到配额 socket。**会改变转发行为**。
    ///
    /// agent 不解析这个 body，也不校验它——全量表的语义属于主控与数据面之间的合同，
    /// agent 在中间多做一次校验只会产生第三份可能漂移的定义。
    Quota {
        #[serde(with = "base64_bytes")]
        body: Vec<u8>,
    },

    /// 按身份文件与时间窗读取**已轮转**的审计片段。数据主体明细档。
    ///
    /// 硬校验放在 agent 侧，因为它是唯一知道本机真实布局的一方：
    /// 拒绝读取**活动文件**，只读已轮转文件（C23）；agent 自身从不 `rm` 活动文件。
    AuditFetch(AuditFetchRequest),
}

impl AgentCommand {
    /// 给日志与指标用的稳定名字。**不含任何参数**——
    /// body 里可能有配额表或身份名，日志里只记批次 ID、节点公开标识与错误码
    /// （`docs/threat-model.md` §5）。
    pub fn name(&self) -> &'static str {
        match self {
            AgentCommand::Snapshot {} => "snapshot",
            AgentCommand::Quota { .. } => "quota",
            AgentCommand::AuditFetch(_) => "audit-fetch",
        }
    }

    /// 该命令是否会改变节点的转发行为。用于在 agent 侧分档记录与限频。
    pub fn mutates(&self) -> bool {
        matches!(self, AgentCommand::Quota { .. })
    }
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AuditFetchRequest {
    /// 身份名。agent 按它定位候选文件，**但文件名不是授权依据**——
    /// 逐行的完整身份校验与归属裁剪在主控侧做（C35）。
    pub identity: String,
    /// 半开区间 `[from, to)`，RFC 3339 UTC。
    pub from: String,
    pub to: String,
}

/// `Vec<u8>` 的 base64 编解码。
///
/// 配额 body 要**原样**送到节点，所以它在 JSON 里必须是无损的字节串而不是字符串——
/// 后者会在非 UTF-8 输入上失败，而「失败」比「静默替换成 U+FFFD」还算好的结果。
mod base64_bytes {
    use serde::{Deserialize, Deserializer, Serializer};

    const ALPHABET: &[u8; 64] = b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";

    pub fn encode(input: &[u8]) -> String {
        let mut out = String::with_capacity(input.len().div_ceil(3) * 4);
        for chunk in input.chunks(3) {
            let b = [chunk[0], *chunk.get(1).unwrap_or(&0), *chunk.get(2).unwrap_or(&0)];
            let n = ((b[0] as u32) << 16) | ((b[1] as u32) << 8) | b[2] as u32;
            out.push(ALPHABET[(n >> 18) as usize & 63] as char);
            out.push(ALPHABET[(n >> 12) as usize & 63] as char);
            out.push(if chunk.len() > 1 { ALPHABET[(n >> 6) as usize & 63] as char } else { '=' });
            out.push(if chunk.len() > 2 { ALPHABET[n as usize & 63] as char } else { '=' });
        }
        out
    }

    pub fn decode(input: &str) -> Result<Vec<u8>, &'static str> {
        let bytes = input.as_bytes();
        if !bytes.len().is_multiple_of(4) {
            return Err("base64 长度必须是 4 的倍数");
        }
        let mut out = Vec::with_capacity(bytes.len() / 4 * 3);
        for chunk in bytes.chunks(4) {
            let mut value = 0u32;
            let mut pad = 0;
            for &byte in chunk {
                value <<= 6;
                if byte == b'=' {
                    pad += 1;
                } else {
                    let index =
                        ALPHABET.iter().position(|&c| c == byte).ok_or("base64 含非法字符")?;
                    value |= index as u32;
                }
            }
            out.push((value >> 16) as u8);
            if pad < 2 {
                out.push((value >> 8) as u8);
            }
            if pad < 1 {
                out.push(value as u8);
            }
        }
        Ok(out)
    }

    pub fn serialize<S: Serializer>(value: &[u8], serializer: S) -> Result<S::Ok, S::Error> {
        serializer.serialize_str(&encode(value))
    }

    pub fn deserialize<'de, D: Deserializer<'de>>(deserializer: D) -> Result<Vec<u8>, D::Error> {
        let text = String::deserialize(deserializer)?;
        decode(&text).map_err(serde::de::Error::custom)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn 命令集是封闭的() {
        // 认识的三条。
        for text in [
            r#"{"command":"snapshot"}"#,
            r#"{"command":"quota","body":"AAEC"}"#,
            r#"{"command":"audit-fetch","identity":"u1","from":"2026-09-01T00:00:00Z","to":"2026-09-02T00:00:00Z"}"#,
        ] {
            serde_json::from_str::<AgentCommand>(text)
                .unwrap_or_else(|e| panic!("{text} 应当被接受：{e}"));
        }

        // 不认识的一律在反序列化时失败，不进任何 match 分支。
        for text in [
            r#"{"command":"exec","argv":["sh"]}"#,
            r#"{"command":"snapshot","extra":1}"#,
            r#"{"command":"Snapshot"}"#,
            r#"{"command":"snapshot ","x":null}"#,
            r#"{"command":"quota"}"#,
            // 这一条曾经**通过**：Snapshot 当时是 unit 变体，多余字段被静默丢弃。
            r#"{"command":"snapshot","argv":["sh"]}"#,
            r#"{"command":"audit-fetch","identity":"u1","from":"a","to":"b","extra":1}"#,
        ] {
            assert!(serde_json::from_str::<AgentCommand>(text).is_err(), "{text} 必须被拒绝");
        }
    }

    #[test]
    fn 配额body原样往返() {
        // 非 UTF-8 的字节也必须无损：body 要原样送到节点。
        let raw: Vec<u8> = (0u8..=255).collect();
        let command = AgentCommand::Quota { body: raw.clone() };
        let text = serde_json::to_string(&command).unwrap();
        let back: AgentCommand = serde_json::from_str(&text).unwrap();
        assert_eq!(back, AgentCommand::Quota { body: raw });
    }

    #[test]
    fn base64边界长度往返() {
        for length in 0..=16usize {
            let raw: Vec<u8> = (0..length).map(|i| i as u8).collect();
            let encoded = base64_bytes::encode(&raw);
            assert_eq!(base64_bytes::decode(&encoded).unwrap(), raw, "长度 {length}");
        }
        assert!(base64_bytes::decode("A").is_err());
        assert!(base64_bytes::decode("!!!!").is_err());
    }

    #[test]
    fn 命令名不含参数() {
        assert_eq!(AgentCommand::Quota { body: b"secret".to_vec() }.name(), "quota");
        assert!(AgentCommand::Quota { body: vec![] }.mutates());
        assert!(!AgentCommand::Snapshot {}.mutates());
    }
}
