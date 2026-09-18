//! 幂等批次 ID 与逐行来源事件哈希的 canonical 编码（`docs/data-model.md` §4 第 8 步）。
//!
//! **长度明确，不用字符串拼接。** 分隔符歧义会让两组不同的输入算出同一个哈希：
//! 用 `\x1f` 连接时，`("a\x1fb", "c")` 与 `("a", "b\x1fc")` 拼出同一个串。
//! 逐字段写「8 字节大端长度 + 内容」就没有这个问题。
//!
//! **与节点侧参考 collector 的 `batch_id` 刻意不同。** `settlement_model.batch_id()`
//! 用的正是 `\x1f` 连接；本项目按 §4 第 8 步用定长前缀。这不是漂移——
//! 批次 ID 是**各自账本内部**的幂等键，两边不需要也不应该相等。
//! 与参考实现对账时比的是**每个桶的字节量**，不是批次 ID。

use sha2::{Digest, Sha256};

/// 哈希口径的版本。改了编码就要推进它，并且**不能**重算历史行——
/// 历史行的 `hash_version` 记的是它当时按哪套口径算的。
pub const HASH_VERSION: i64 = 1;

const BATCH_DOMAIN: &[u8] = b"proxy-manager/v1/batch";
const EVENT_DOMAIN: &[u8] = b"proxy-manager/v1/source-event";

/// 一条 lineage 的完整键与它这一批的四向增量。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct LineageDelta {
    pub inbound_tag: String,
    pub inbound_generation: u64,
    pub user_name: String,
    pub user_generation: u64,
    pub tcp_uplink_bytes: u64,
    pub tcp_downlink_bytes: u64,
    pub udp_uplink_bytes: u64,
    pub udp_downlink_bytes: u64,
}

impl LineageDelta {
    /// 自然键。排序用它而不是用 `runtime_identity_id`：
    /// 后者是数据库代理键，换一个库重算同一批输入会得到不同的哈希。
    /// 两者在实践中顺序一致（代理键按快照顺序分配，而快照本身就是按自然键升序的），
    /// 所以这个选择同时满足 §4 第 5 步「保证运算和哈希顺序确定」。
    pub fn sort_key(&self) -> (&str, u64, &str, u64) {
        (&self.inbound_tag, self.inbound_generation, &self.user_name, self.user_generation)
    }

    pub fn is_zero(&self) -> bool {
        self.tcp_uplink_bytes == 0
            && self.tcp_downlink_bytes == 0
            && self.udp_uplink_bytes == 0
            && self.udp_downlink_bytes == 0
    }

    fn encode_into(&self, out: &mut Vec<u8>) {
        push_bytes(out, self.inbound_tag.as_bytes());
        push_u64(out, self.inbound_generation);
        push_bytes(out, self.user_name.as_bytes());
        push_u64(out, self.user_generation);
        push_u64(out, self.tcp_uplink_bytes);
        push_u64(out, self.tcp_downlink_bytes);
        push_u64(out, self.udp_uplink_bytes);
        push_u64(out, self.udp_downlink_bytes);
    }
}

/// 幂等批次 ID。
///
/// **必须含 `sequence` 与本次增量**：只用 sequence 做 ID，会让「同一序号重投但增量不同」
/// 这个实现错误变成静默的重复入账。
pub fn batch_id(node_id: &str, runtime_id: &str, sequence: u64, deltas: &[LineageDelta]) -> String {
    let mut sorted: Vec<&LineageDelta> = deltas.iter().collect();
    sorted.sort_by(|a, b| a.sort_key().cmp(&b.sort_key()));

    let mut out = Vec::with_capacity(128 + sorted.len() * 96);
    push_bytes(&mut out, BATCH_DOMAIN);
    push_bytes(&mut out, node_id.as_bytes());
    push_bytes(&mut out, runtime_id.as_bytes());
    push_u64(&mut out, sequence);
    // 条目数也进编码：否则「少一条全零增量」与「本来就没有那一条」算出同一个哈希。
    push_u64(&mut out, sorted.len() as u64);
    for delta in sorted {
        delta.encode_into(&mut out);
    }
    hex::encode(Sha256::digest(&out))
}

/// 单条账本行的来源事件哈希。
pub fn source_event_hash(
    node_id: &str,
    runtime_id: &str,
    sequence: u64,
    delta: &LineageDelta,
) -> String {
    let mut out = Vec::with_capacity(160);
    push_bytes(&mut out, EVENT_DOMAIN);
    push_bytes(&mut out, node_id.as_bytes());
    push_bytes(&mut out, runtime_id.as_bytes());
    push_u64(&mut out, sequence);
    delta.encode_into(&mut out);
    hex::encode(Sha256::digest(&out))
}

fn push_bytes(out: &mut Vec<u8>, value: &[u8]) {
    out.extend_from_slice(&(value.len() as u64).to_be_bytes());
    out.extend_from_slice(value);
}

fn push_u64(out: &mut Vec<u8>, value: u64) {
    out.extend_from_slice(&value.to_be_bytes());
}

#[cfg(test)]
mod tests {
    use super::*;

    fn delta(tag: &str, name: &str, up: u64) -> LineageDelta {
        LineageDelta {
            inbound_tag: tag.into(),
            inbound_generation: 1,
            user_name: name.into(),
            user_generation: 1,
            tcp_uplink_bytes: up,
            tcp_downlink_bytes: 0,
            udp_uplink_bytes: 0,
            udp_downlink_bytes: 0,
        }
    }

    #[test]
    fn 顺序不影响批次id() {
        let a = vec![delta("in-a", "u1", 1), delta("in-b", "u2", 2)];
        let b = vec![delta("in-b", "u2", 2), delta("in-a", "u1", 1)];
        assert_eq!(batch_id("n", "r", 5, &a), batch_id("n", "r", 5, &b));
    }

    /// 定长前缀没有分隔符歧义。用 `\x1f` 连接的话这两组会撞。
    #[test]
    fn 分隔符歧义不会撞哈希() {
        let left = vec![delta("a\u{1f}b", "c", 1)];
        let right = vec![delta("a", "b\u{1f}c", 1)];
        assert_ne!(batch_id("n", "r", 5, &left), batch_id("n", "r", 5, &right));

        // node_id 与 runtime_id 之间同理。
        assert_ne!(batch_id("ab", "c", 5, &left), batch_id("a", "bc", 5, &left));
    }

    /// 只用 sequence 做 ID 会让「同号重投但增量不同」静默重复入账。
    #[test]
    fn 同号不同增量算出不同的批次id() {
        let a = vec![delta("in-a", "u1", 1)];
        let b = vec![delta("in-a", "u1", 2)];
        assert_ne!(batch_id("n", "r", 5, &a), batch_id("n", "r", 5, &b));
    }

    /// 少一条全零增量 ≠ 本来就没有那一条。
    #[test]
    fn 条目数进编码() {
        let one = vec![delta("in-a", "u1", 1)];
        let two = vec![delta("in-a", "u1", 1), delta("in-b", "u2", 0)];
        assert_ne!(batch_id("n", "r", 5, &one), batch_id("n", "r", 5, &two));
    }

    #[test]
    fn 换节点或runtime或sequence都换哈希() {
        let d = vec![delta("in-a", "u1", 1)];
        let base = batch_id("n", "r", 5, &d);
        assert_ne!(base, batch_id("n2", "r", 5, &d));
        assert_ne!(base, batch_id("n", "r2", 5, &d));
        assert_ne!(base, batch_id("n", "r", 6, &d));
    }

    /// 批次哈希与逐行哈希不同域，互相不能冒充。
    #[test]
    fn 批次与逐行哈希分域() {
        let d = delta("in-a", "u1", 1);
        assert_ne!(
            batch_id("n", "r", 5, std::slice::from_ref(&d)),
            source_event_hash("n", "r", 5, &d)
        );
    }

    #[test]
    fn 哈希是六十四位十六进制() {
        let d = vec![delta("in-a", "u1", 1)];
        let id = batch_id("n", "r", 5, &d);
        assert_eq!(id.len(), 64);
        assert!(id.bytes().all(|b| b.is_ascii_hexdigit()));
    }

    #[test]
    fn 零增量判定() {
        assert!(delta("in-a", "u1", 0).is_zero());
        assert!(!delta("in-a", "u1", 1).is_zero());
    }
}
