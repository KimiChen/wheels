//! `GET /v3/snapshot` 的响应体：解析与逐字段校验。
//!
//! 版本断言的权威是节点侧 Go 常量 `internal/userstats/snapshot.go` 的
//! `const SchemaVersion = 3`，不是任何一份 README。
//!
//! 解析纪律（C3）：wire 上的 u64 用 Rust `u64` **精确**解析，全程不经
//! `serde_json::Value` / `Number` / `f64`。`serde_json` 对 `u64` 目标字段会直接拒绝
//! 浮点与布尔，`1e19` 这类值不会被当成合法计数——这正是不用 `Value` 中转的原因。

use std::collections::BTreeMap;

use serde::Deserialize;

/// 与节点侧 Go 常量一致。读到 `1` 或 `2` 整份拒绝（C10）。
pub const SCHEMA_VERSION: u32 = 3;

/// `health` 的闭集：**恰好四位**（C4）。
pub const HEALTH_KEYS: [&str; 4] =
    ["audit_dropped", "counter_overflow", "identity_limit_reached", "sequence_overflow"];

/// 前三位是「这份快照不可入账」；`audit_dropped` 是「审计有损」，照常入账。
pub const BILLING_HEALTH_KEYS: [&str; 3] =
    ["counter_overflow", "identity_limit_reached", "sequence_overflow"];

/// 整份拒绝的成因。
///
/// 成因要能被测试**逐条断言**，不能只断言「失败了」——README §8 的教训是
/// 文本断言会因为无关原因碰巧通过，所以每条关键断言都要有反向测试证明它会红。
#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
pub enum Rejected {
    #[error("快照不是合法 UTF-8：{0}")]
    NotUtf8(String),
    #[error("快照不是合法 JSON 或字段形状不符：{0}")]
    Malformed(String),
    #[error("schema_version 必须是 {expected}，实际 {actual}（C10：路径与版本号成对硬校验）")]
    SchemaVersion { expected: u32, actual: u32 },
    #[error("health 多出键 {extra:?}（C4：闭集是「不多不少」）")]
    HealthExtraKey { extra: Vec<String> },
    #[error("health 缺少键 {missing:?}（C4：闭集是「不多不少」）")]
    HealthMissingKey { missing: Vec<String> },
    #[error("runtime_id 必须是 32 位小写 hex，实际 {0:?}")]
    RuntimeId(String),
    #[error("{field} 必须是正整数，实际 0")]
    MustBePositive { field: &'static str },
    #[error("listen_port 越界：{0}")]
    ListenPort(u64),
    #[error("{field} 不合法：{detail}")]
    Token { field: &'static str, detail: String },
    #[error("{container} 未按 {key} 的 ASCII 字节升序严格排列（重复完整键也走这一支）")]
    Ordering { container: &'static str, key: &'static str },
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub struct Health {
    pub counter_overflow: bool,
    pub sequence_overflow: bool,
    pub identity_limit_reached: bool,
    pub audit_dropped: bool,
}

impl Health {
    /// 入账判据**只看前三位**（C4）。
    ///
    /// `audit_dropped` 为真意味着审计旁路丢过记录，证据链有缺口，应当告警；
    /// 但计费计数器本身没出问题。把它算进入账判据，等于让磁盘写满连带停掉计费——
    /// 这是节点侧刻意不做的，主控也不要做回来。
    pub fn billable(&self) -> bool {
        !(self.counter_overflow || self.sequence_overflow || self.identity_limit_reached)
    }

    /// 该告警但不影响入账的位。
    pub fn audit_gap(&self) -> bool {
        self.audit_dropped
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SnapshotUser {
    pub name: String,
    pub generation: u64,
    pub active: bool,
    pub tcp_uplink_bytes: u64,
    pub tcp_downlink_bytes: u64,
    pub udp_uplink_bytes: u64,
    pub udp_downlink_bytes: u64,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Inbound {
    pub tag: String,
    pub inbound_type: String,
    pub listen: String,
    pub listen_port: u16,
    pub generation: u64,
    pub active: bool,
    /// 瞬时 gauge：**不进基线、不参与差分、不触发回退告警**（C6）。
    pub tcp_sessions: i64,
    pub udp_sessions: i64,
    pub users: Vec<SnapshotUser>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Snapshot {
    pub schema_version: u32,
    pub node_id: String,
    pub runtime_id: String,
    pub started_at_unix_ms: u64,
    pub sequence: u64,
    pub health: Health,
    pub inbounds: Vec<Inbound>,
}

// ---- wire 形状。字段名与 wire 同名；归一化后的内表字段名不必与 wire 同名。----

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct WireSnapshot {
    schema_version: u32,
    node_id: String,
    runtime_id: String,
    started_at_unix_ms: u64,
    sequence: u64,
    /// 用「键 → bool」的映射接，而不是一个四字段结构体：
    /// 这样「多一个键」与「少一个键」能分别报出**是哪个键**，
    /// 而 `deny_unknown_fields` 只会给出一句通用的 serde 错误。
    /// 值类型是 bool，不经任何数值通道，C3 不受影响。
    health: BTreeMap<String, bool>,
    inbounds: Vec<WireInbound>,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct WireInbound {
    tag: String,
    #[serde(rename = "type")]
    inbound_type: String,
    listen: String,
    listen_port: u64,
    generation: u64,
    active: bool,
    tcp_sessions: i64,
    udp_sessions: i64,
    users: Vec<WireUser>,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct WireUser {
    name: String,
    generation: u64,
    active: bool,
    tcp_uplink_bytes: u64,
    tcp_downlink_bytes: u64,
    udp_uplink_bytes: u64,
    udp_downlink_bytes: u64,
}

/// 严格解析并校验一份快照。任何一处不符即**整份拒绝**。
pub fn parse(payload: &[u8]) -> Result<Snapshot, Rejected> {
    let text = std::str::from_utf8(payload).map_err(|e| Rejected::NotUtf8(e.to_string()))?;
    // serde 的 derive 会对重复字段报 "duplicate field"，落进 Malformed。
    let wire: WireSnapshot =
        serde_json::from_str(text).map_err(|e| Rejected::Malformed(e.to_string()))?;

    if wire.schema_version != SCHEMA_VERSION {
        return Err(Rejected::SchemaVersion {
            expected: SCHEMA_VERSION,
            actual: wire.schema_version,
        });
    }

    let health = build_health(&wire.health)?;

    require_token("node_id", &wire.node_id)?;
    if wire.runtime_id.len() != 32
        || !wire.runtime_id.bytes().all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
    {
        return Err(Rejected::RuntimeId(wire.runtime_id));
    }
    if wire.started_at_unix_ms == 0 {
        return Err(Rejected::MustBePositive { field: "started_at_unix_ms" });
    }
    if wire.sequence == 0 {
        return Err(Rejected::MustBePositive { field: "sequence" });
    }

    let mut inbounds = Vec::with_capacity(wire.inbounds.len());
    let mut previous_inbound: Option<(String, u64)> = None;
    for inbound in wire.inbounds {
        require_token("inbounds[].tag", &inbound.tag)?;
        require_token("inbounds[].type", &inbound.inbound_type)?;
        // listen 是纯 host，不得是 host:port 合成串。
        if inbound.listen.matches(':').count() == 1 && !inbound.listen.contains(']') {
            return Err(Rejected::Token {
                field: "inbounds[].listen",
                detail: format!("必须是纯 host，不得是 host:port 合成串：{:?}", inbound.listen),
            });
        }
        if inbound.listen_port == 0 || inbound.listen_port > 65535 {
            return Err(Rejected::ListenPort(inbound.listen_port));
        }
        if inbound.tcp_sessions < 0 || inbound.udp_sessions < 0 {
            return Err(Rejected::Token {
                field: "inbounds[].*_sessions",
                detail: "session gauge 必须非负".into(),
            });
        }
        let key = (inbound.tag.clone(), inbound.generation);
        if let Some(previous) = &previous_inbound {
            if key <= *previous {
                return Err(Rejected::Ordering { container: "inbounds", key: "(tag, generation)" });
            }
        }
        previous_inbound = Some(key);

        let mut users = Vec::with_capacity(inbound.users.len());
        let mut previous_user: Option<(String, u64)> = None;
        for user in inbound.users {
            require_token("users[].name", &user.name)?;
            let user_key = (user.name.clone(), user.generation);
            if let Some(previous) = &previous_user {
                if user_key <= *previous {
                    return Err(Rejected::Ordering {
                        container: "users",
                        key: "(name, generation)",
                    });
                }
            }
            previous_user = Some(user_key);
            users.push(SnapshotUser {
                name: user.name,
                generation: user.generation,
                active: user.active,
                tcp_uplink_bytes: user.tcp_uplink_bytes,
                tcp_downlink_bytes: user.tcp_downlink_bytes,
                udp_uplink_bytes: user.udp_uplink_bytes,
                udp_downlink_bytes: user.udp_downlink_bytes,
            });
        }

        inbounds.push(Inbound {
            tag: inbound.tag,
            inbound_type: inbound.inbound_type,
            listen: inbound.listen,
            listen_port: inbound.listen_port as u16,
            generation: inbound.generation,
            active: inbound.active,
            tcp_sessions: inbound.tcp_sessions,
            udp_sessions: inbound.udp_sessions,
            users,
        });
    }

    Ok(Snapshot {
        schema_version: wire.schema_version,
        node_id: wire.node_id,
        runtime_id: wire.runtime_id,
        started_at_unix_ms: wire.started_at_unix_ms,
        sequence: wire.sequence,
        health,
        inbounds,
    })
}

fn build_health(raw: &BTreeMap<String, bool>) -> Result<Health, Rejected> {
    let extra: Vec<String> =
        raw.keys().filter(|key| !HEALTH_KEYS.contains(&key.as_str())).cloned().collect();
    if !extra.is_empty() {
        return Err(Rejected::HealthExtraKey { extra });
    }
    let missing: Vec<String> = HEALTH_KEYS
        .iter()
        .filter(|key| !raw.contains_key(**key))
        .map(|key| (*key).to_string())
        .collect();
    if !missing.is_empty() {
        return Err(Rejected::HealthMissingKey { missing });
    }
    Ok(Health {
        counter_overflow: raw["counter_overflow"],
        sequence_overflow: raw["sequence_overflow"],
        identity_limit_reached: raw["identity_limit_reached"],
        audit_dropped: raw["audit_dropped"],
    })
}

/// 节点侧 `_require_str` / `validateToken` 的同构实现：
/// 非空、不超过 128 字节、只含 ASCII 可显示字符（`0x21..=0x7e`）。
fn require_token(field: &'static str, value: &str) -> Result<(), Rejected> {
    if value.is_empty() {
        return Err(Rejected::Token { field, detail: "不能为空".into() });
    }
    if value.len() > 128 {
        return Err(Rejected::Token {
            field, detail: format!("超过 128 字节：{}", value.len())
        });
    }
    for (offset, byte) in value.bytes().enumerate() {
        if byte <= 0x20 || byte >= 0x7f {
            return Err(Rejected::Token {
                field,
                detail: format!("含非 ASCII 可显示字符（偏移 {offset}）"),
            });
        }
    }
    Ok(())
}

/// 只从 envelope 里取出 `node_id` / `runtime_id` / `sequence`。
///
/// 用途单一：**可审计的拒绝也要留一条批次回执**（`docs/data-model.md` §4）。
/// 一份因为 health 多一个键、或版本号不对而被整份拒绝的快照，它的 envelope 往往
/// 仍然是好的——那条回执能让运维看到「这个节点这一秒确实回了东西，但我们没认」。
/// 完全认不出 envelope 的畸形输入才只进限量安全日志。
///
/// **它不是一个宽松的解析器，不得用于入账。** 这里刻意不校验 health、不校验排序、
/// 不校验计数器；拿它的结果去结算就等于绕过了整套 §2.1。
/// 三个字段仍然是强类型的：`sequence` 走 `u64` 通道，不经 f64。
pub fn parse_envelope(payload: &[u8]) -> Option<Envelope> {
    #[derive(Deserialize)]
    struct WireEnvelope {
        node_id: String,
        runtime_id: String,
        sequence: u64,
    }
    let text = std::str::from_utf8(payload).ok()?;
    // 注意没有 deny_unknown_fields：整份快照的其余字段在这里是**故意**被忽略的。
    let wire: WireEnvelope = serde_json::from_str(text).ok()?;
    if wire.node_id.is_empty()
        || wire.runtime_id.len() != 32
        || !wire.runtime_id.bytes().all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
        || wire.sequence == 0
    {
        return None;
    }
    Some(Envelope { node_id: wire.node_id, runtime_id: wire.runtime_id, sequence: wire.sequence })
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Envelope {
    pub node_id: String,
    pub runtime_id: String,
    pub sequence: u64,
}

#[cfg(test)]
mod envelope_tests {
    use super::*;

    const GOOD_ENVELOPE: &str = r#"{"schema_version":1,"node_id":"n1","runtime_id":"0123456789abcdef0123456789abcdef","sequence":7,"health":{"x":true},"inbounds":[]}"#;

    /// 整份被拒的快照，envelope 仍然认得出来——这条回执才留得下。
    #[test]
    fn 整份被拒时envelope仍可识别() {
        assert!(parse(GOOD_ENVELOPE.as_bytes()).is_err(), "版本与 health 都不对，必须整份拒绝");
        let envelope = parse_envelope(GOOD_ENVELOPE.as_bytes()).expect("envelope 应当认得出");
        assert_eq!(envelope.node_id, "n1");
        assert_eq!(envelope.sequence, 7);
    }

    #[test]
    fn 认不出的畸形输入返回none() {
        for bad in [
            &b"not json"[..],
            br#"{"node_id":"n1"}"#,
            br#"{"node_id":"","runtime_id":"0123456789abcdef0123456789abcdef","sequence":1}"#,
            br#"{"node_id":"n1","runtime_id":"TOO-SHORT","sequence":1}"#,
            br#"{"node_id":"n1","runtime_id":"0123456789abcdef0123456789abcdef","sequence":0}"#,
            // sequence 走 u64 通道，浮点冒充不了
            br#"{"node_id":"n1","runtime_id":"0123456789abcdef0123456789abcdef","sequence":1e3}"#,
        ] {
            assert!(parse_envelope(bad).is_none(), "{:?} 应当认不出", String::from_utf8_lossy(bad));
        }
    }
}
