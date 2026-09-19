//! JSONL 访问审计的解析。
//!
//! 权威来源是**节点源码**，不是主控侧的散文：编码器是
//! `internal/userstats/audit.go` 的 `encode()`——`map[string]any` 加
//! `json.Marshal`，**没有 struct tag**，所以在二进制里 grep `json:` 找不到。
//!
//! # 第一道分支看有没有 `ev` 键
//!
//! 访问记录恰好 13 个键，诊断行有 `ev`、**没有 `ts`、没有四元组**——
//! 于是它既无法归属到用户、也无法落进时间窗。这不是实现上的将就，是规则：
//! 文件里的诊断行**不能借用邻近访问行推断用户**。
//!
//! # 几个会影响实现的陷阱
//!
//! * **`seq` 按身份计、不按文件计**，轮转时不复位，**进程重启从 1 重来**——
//!   于是同一个文件里可能有两段各自递增的 seq。查连续性必须按 `(user, run)` 分段。
//!   它与快照的 `sequence` 是两个独立命名空间。
//! * **`ts` 是墙钟、是 writer 写出时刻**，不是连接开始时刻。时钟回拨时 `seq`
//!   仍严格递增，所以**排序按 `seq` 不按 `ts`**。
//! * **没有 `generation`。** 四值 `node/run/in/user` 关联到 runtime_identity 时
//!   必须从已登记的 lineage 唯一解析，不能用当前映射或同名文件兜底（C35）。
//! * `host` 只做「小写 + 去掉一个尾点 + ToValidUTF8 + 截断 255 字节」，
//!   **不做 IDNA/UTS#46**。
//! * `host_src` 为 `sniff` 时来的可能是 ECH 伪装的 SNI——那是**错误记录**而不是缺失，
//!   所以这个字段要一路带到页面上，让人看得出这一行的可信度来自哪里。
//! * **不存在**：URL 路径、查询参数、Header、载荷、证书链、真实客户端 IP、
//!   schema_version、任何签名。
//!
//! `ev=gap` 的 `n` **可以是 null**（边界不确定时），节点刻意不合成一个不存在的区间。
//! 主控也**不得**把缺号换算成「确定丢失了 N 条访问」——那是把未知伪装成已知。

use serde::{Deserialize, Serialize};

/// 访问记录来源。
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum HostSource {
    /// 从 TLS SNI / HTTP Host 嗅探得来。**可能是 ECH 伪装的 SNI**。
    Sniff,
    /// 客户端在协议里直接给了域名。
    Fqdn,
    /// 没有域名，记的是 IP。没开 sniff 时恒为这个。
    Ip,
}

impl HostSource {
    pub fn as_str(self) -> &'static str {
        match self {
            HostSource::Sniff => "sniff",
            HostSource::Fqdn => "fqdn",
            HostSource::Ip => "ip",
        }
    }
}

/// 一条访问记录。字段名照节点写出来的键，不改名——改名会让两边对不上。
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct AccessRecord {
    pub seq: u64,
    /// 墙钟毫秒，writer 写出时刻。排序**不要**用它。
    pub ts: i64,
    pub node: String,
    pub run: String,
    /// **身份名**，不是 user_id。归属要走 C35 那套裁剪才算数。
    pub user: String,
    #[serde(rename = "in")]
    pub inbound: String,
    pub net: String,
    pub host: String,
    pub host_src: HostSource,
    pub port: u16,
    pub up: u64,
    pub down: u64,
    pub ms: u64,
}

impl AccessRecord {
    /// 成功判据的**复核**。
    ///
    /// 节点侧失败的记录压根不写文件（`audit.go:153-169`），但
    /// `docs/ACCESS_AUDIT.md:206` 写明 `up`/`down` 是原样落盘的、
    /// **成功判据交由读取方复核**。M6 准入用例直接对主控断言这一条，
    /// 所以这里必须自己再判一遍，而不是信任上游已经判过。
    pub fn successful(&self) -> bool {
        match self.net.as_str() {
            "udp" => self.up > 0,
            // TCP 以及任何别的协议：两个方向都要有字节。
            _ => self.up > 0 && self.down > 0,
        }
    }
}

/// 诊断行。**没有 `ts`、没有四元组**，所以既归属不到人，也落不进时间窗。
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(tag = "ev", rename_all = "lowercase")]
pub enum Diagnostic {
    /// 每次**打开物理文件**都写一行：新建、轮转后续写、重启续写各一份。
    ///
    /// 只在配了 `exclude_hosts` / `exclude_ips` 时才写——`stampFilter` 在排除名单
    /// 为空时直接返回。当前四台都没配排除，所以这套部署里不会出现 filter 行。
    Filter {
        seq: u64,
        #[serde(default)]
        hosts: Vec<String>,
        #[serde(default)]
        ips: Vec<String>,
    },
    /// 队列满（`reason="queue_full"`）或总量封顶删掉最老归档（`reason="total_cap"`）。
    Gap {
        seq: u64,
        after: u64,
        /// **可以是 null**：边界不确定时节点刻意不合成一个不存在的区间。
        /// 不得把它当成「确定丢失了 N 条」——那是把未知伪装成已知。
        n: Option<u64>,
        reason: String,
    },
    /// writer 关闭。
    Stop { seq: u64 },
}

impl Diagnostic {
    pub fn seq(&self) -> u64 {
        match self {
            Diagnostic::Filter { seq, .. }
            | Diagnostic::Gap { seq, .. }
            | Diagnostic::Stop { seq } => *seq,
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Line {
    Access(Box<AccessRecord>),
    Diagnostic(Diagnostic),
}

#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Parsed {
    pub access: Vec<AccessRecord>,
    pub diagnostics: Vec<Diagnostic>,
    /// 两种形状都对不上的行数。**不猜**——记一个数，不返回内容。
    ///
    /// 计数而不是丢掉：这个数字是「上游形状漂移了」的唯一信号，
    /// 而这一页的每一行都建立在那 13 个键上。
    pub unparsed: usize,
    /// 解析成功、但带了那 13 个键之外的键的访问行数。
    ///
    /// 这里刻意**不**用 `deny_unknown_fields`：上游加一个键会让全部记录变成
    /// 无法解析，页面当场空掉——那是把一次小漂移放大成一次停摆。但只是宽松接受
    /// 又会让漂移完全静默。所以宽松解析 + 计数：页面照常工作，而漂移有数字可看。
    pub unexpected_keys: usize,
}

/// 访问记录的全部键。恰好 13 个，照 `internal/userstats/audit.go:324-346`。
pub const ACCESS_KEYS: [&str; 13] = [
    "seq", "ts", "node", "run", "user", "in", "net", "host", "host_src", "port", "up", "down", "ms",
];

/// 解析一行。第一道分支看有没有 `ev` 键。
///
/// 第二个返回值：这一行是否带了 [`ACCESS_KEYS`] 之外的键。
pub fn parse_line(line: &str) -> Option<(Line, bool)> {
    let value: serde_json::Value = serde_json::from_str(line).ok()?;
    let object = value.as_object()?;
    if object.contains_key("ev") {
        return serde_json::from_value(value).ok().map(|d| (Line::Diagnostic(d), false));
    }
    let extra = object.keys().any(|key| !ACCESS_KEYS.contains(&key.as_str()));
    serde_json::from_value(value).ok().map(|r| (Line::Access(Box::new(r)), extra))
}

/// 解析一整份文件。空行跳过，坏行计数。
pub fn parse(bytes: &[u8]) -> Parsed {
    let mut parsed = Parsed::default();
    for line in String::from_utf8_lossy(bytes).lines() {
        if line.trim().is_empty() {
            continue;
        }
        match parse_line(line) {
            Some((Line::Access(record), extra)) => {
                parsed.access.push(*record);
                if extra {
                    parsed.unexpected_keys += 1;
                }
            }
            Some((Line::Diagnostic(diagnostic), _)) => parsed.diagnostics.push(diagnostic),
            None => parsed.unparsed += 1,
        }
    }
    parsed
}

#[cfg(test)]
mod tests {
    use super::*;

    /// 三条**从线上真实数据里取出来**的记录，只把 node/run/user 与那个内网地址换成了
    /// 示例值，其余字段逐字保留（键序、类型、取值范围）。自己编的样例证明不了
    /// 上游真的是这个形状。
    const SNIFF: &str = r#"{"down":3871,"host":"www.google.com","host_src":"sniff","in":"ss-entry","ms":68,"net":"tcp","node":"node-example-01","port":443,"run":"0123456789abcdef0123456789abcdef","seq":1,"ts":1789799971049,"up":1520,"user":"u_example_01"}"#;
    const FQDN: &str = r#"{"down":3501,"host":"github.com","host_src":"fqdn","in":"ss-entry","ms":3321,"net":"tcp","node":"node-example-01","port":22,"run":"0123456789abcdef0123456789abcdef","seq":1318,"ts":1789801410183,"up":3534,"user":"u_example_01"}"#;
    const IP: &str = r#"{"down":241,"host":"203.0.113.7","host_src":"ip","in":"ss-entry","ms":126,"net":"tcp","node":"node-example-01","port":9000,"run":"0123456789abcdef0123456789abcdef","seq":2,"ts":1789799972363,"up":2252,"user":"u_example_01"}"#;

    fn access(line: &str) -> AccessRecord {
        match parse_line(line).expect("应当解析成功").0 {
            Line::Access(record) => *record,
            other => panic!("应当是访问行：{other:?}"),
        }
    }

    #[test]
    fn 线上真实的三种来源都解析得了() {
        for (line, source) in
            [(SNIFF, HostSource::Sniff), (FQDN, HostSource::Fqdn), (IP, HostSource::Ip)]
        {
            let record = access(line);
            assert_eq!(record.host_src, source);
            assert_eq!(record.inbound, "ss-entry", "`in` 是关键字，要靠 rename 映射");
        }
        let record = access(SNIFF);
        assert_eq!(record.host, "www.google.com");
        assert_eq!(record.port, 443);
        assert_eq!(record.ts, 1789799971049);
        assert_eq!(record.up, 1520);
        assert_eq!(record.down, 3871);
    }

    #[test]
    fn 恰好十三个键() {
        let value: serde_json::Value = serde_json::from_str(SNIFF).unwrap();
        let keys: Vec<&str> = value.as_object().unwrap().keys().map(|k| k.as_str()).collect();
        assert_eq!(keys.len(), 13);
        for key in &keys {
            assert!(ACCESS_KEYS.contains(key), "{key} 不在已知键里");
        }
        for key in ACCESS_KEYS {
            assert!(keys.contains(&key), "{key} 在真实记录里不存在");
        }
    }

    #[test]
    fn 多一个键仍然解析得了但会被数出来() {
        // 不用 deny_unknown_fields：上游加一个键会让全部记录无法解析、页面当场空掉，
        // 那是把一次小漂移放大成一次停摆。但只是宽松接受又会让漂移完全静默。
        let drifted = SNIFF.replace(r#""seq":1,"#, r#""seq":1,"schema_version":4,"#);
        let parsed = parse(drifted.as_bytes());
        assert_eq!(parsed.access.len(), 1, "照常解析");
        assert_eq!(parsed.unexpected_keys, 1, "但要数出来");
        assert_eq!(parsed.unparsed, 0);
        // 反向：没有多余键时这个数是 0，否则这条断言是摆设。
        assert_eq!(parse(SNIFF.as_bytes()).unexpected_keys, 0);
    }

    #[test]
    fn 第一道分支看有没有ev键() {
        let gap = r#"{"seq":10242,"ev":"gap","after":10241,"n":37,"reason":"queue_full"}"#;
        let parsed = parse(format!("{SNIFF}\n{gap}\n").as_bytes());
        assert_eq!(parsed.access.len(), 1);
        assert_eq!(parsed.diagnostics.len(), 1);
        // **诊断行不进用户统计。** 它没有 ts、没有四元组，既归属不到人，
        // 也落不进时间窗；借用邻近访问行去推断用户是明确禁止的。
        assert_eq!(parsed.access[0].user, "u_example_01");
        match &parsed.diagnostics[0] {
            Diagnostic::Gap { after, n, reason, .. } => {
                assert_eq!(*after, 10241);
                assert_eq!(*n, Some(37));
                assert_eq!(reason, "queue_full");
            }
            other => panic!("{other:?}"),
        }
    }

    #[test]
    fn gap的n可以是null() {
        // 边界不确定时节点刻意不合成一个不存在的区间。主控也不得把缺号换算成
        // 「确定丢失了 N 条」——那是把未知伪装成已知。
        let line = r#"{"seq":9,"ev":"gap","after":8,"n":null,"reason":"total_cap"}"#;
        match parse_line(line).unwrap().0 {
            Line::Diagnostic(Diagnostic::Gap { n, reason, .. }) => {
                assert_eq!(n, None);
                assert_eq!(reason, "total_cap");
            }
            other => panic!("{other:?}"),
        }
    }

    /// **从线上取出来的那一行**，逐字保留（键序、35 + 5 的长度、值）。
    ///
    /// 这一行是页面上「哪些目标不记录」的唯一来源，而它的形状由节点的
    /// `encodeFilterEvent` 决定——自己编一个短的证明不了上游真的是这样。
    /// 注意 `ev` 不在第一个键：`encode()` 用的是 `map[string]any`，
    /// **键序不做任何保证**，所以解析器绝不能依赖它。
    const REAL_FILTER: &str = r#"{"ev":"filter","hosts":["google.com","googleapis.com","googleusercontent.com","gstatic.com","googlevideo.com","google.com.hk","googletagmanager.com","googleadservices.com","google-analytics.com","doubleclick.net","gvt2.com","adtrafficquality.google","microsoft.com","vscode-cdn.net","vsassets.io","exp-tas.com","trafficmanager.net","applicationinsights.azure.com","apple.com","cdn-apple.com","edge.apple","icloud.com","apple-cloudkit.com","github.com","githubusercontent.com","githubassets.com","ghcr.io","nel.cloudflare.com","cloudflareinsights.com","cp.cloudflare.com","twimg.com","pythonhosted.org","nuget.org","cursor.com","launchdarkly.com"],"ips":["198.18.0.0/15","74.125.0.0/16","142.250.0.0/15","172.217.0.0/16","17.253.0.0/16"],"seq":1}"#;

    #[test]
    fn 线上真实的filter行解析得了() {
        match parse_line(REAL_FILTER).expect("应当解析成功").0 {
            Line::Diagnostic(Diagnostic::Filter { seq, hosts, ips }) => {
                assert_eq!(seq, 1);
                assert_eq!(hosts.len(), 35);
                assert_eq!(ips.len(), 5);
                assert_eq!(hosts[0], "google.com");
                assert_eq!(ips[0], "198.18.0.0/15");
            }
            other => panic!("{other:?}"),
        }
        // 它是诊断行：**不进访问统计**，也不带 ts、不带四元组，归属不到任何人。
        let parsed = parse(format!("{REAL_FILTER}\n{SNIFF}\n").as_bytes());
        assert_eq!(parsed.access.len(), 1);
        assert_eq!(parsed.diagnostics.len(), 1);
        assert_eq!(parsed.unparsed, 0);
    }

    #[test]
    fn filter与stop两种诊断行() {
        let filter = r#"{"seq":1,"ev":"filter","hosts":["example.com"],"ips":["203.0.113.0/24"]}"#;
        match parse_line(filter).unwrap().0 {
            Line::Diagnostic(Diagnostic::Filter { hosts, ips, seq }) => {
                assert_eq!(seq, 1);
                assert_eq!(hosts, vec!["example.com"]);
                assert_eq!(ips, vec!["203.0.113.0/24"]);
            }
            other => panic!("{other:?}"),
        }
        assert!(matches!(
            parse_line(r#"{"seq":12,"ev":"stop"}"#).unwrap().0,
            Line::Diagnostic(Diagnostic::Stop { seq: 12 })
        ));
    }

    #[test]
    fn 认不出来的行计数而不是猜() {
        let parsed = parse(b"not json\n{\"ev\":\"unknown-event\"}\n{}\n");
        assert_eq!(parsed.unparsed, 3);
        assert!(parsed.access.is_empty());
        assert!(parsed.diagnostics.is_empty());
    }

    #[test]
    fn 空行跳过() {
        let parsed = parse(format!("\n{SNIFF}\n\n   \n").as_bytes());
        assert_eq!(parsed.access.len(), 1);
        assert_eq!(parsed.unparsed, 0, "空行不是坏行");
    }

    #[test]
    fn 成功判据要在主控侧再判一遍() {
        // 节点侧失败的记录压根不写文件，但 up/down 是原样落盘的、判据交由读取方复核，
        // 而 M6 准入用例是直接对**主控**断言这一条的。
        let tcp = |up: u64, down: u64| AccessRecord { up, down, ..access(SNIFF) };
        assert!(tcp(1, 1).successful());
        assert!(!tcp(1, 0).successful(), "TCP 的 up>0, down=0 不算成功");
        assert!(!tcp(0, 1).successful());

        let udp =
            |up: u64, down: u64| AccessRecord { net: "udp".into(), up, down, ..access(SNIFF) };
        assert!(udp(1, 0).successful(), "UDP 同样条件算成功");
        assert!(!udp(0, 0).successful());
    }

    #[test]
    fn seq按身份计且进程重启会从一重来() {
        // 同一个文件里可能有两段各自递增的 seq。查连续性必须按 (user, run) 分段——
        // 不分段的话一次重启会被读成一个巨大的倒退。
        let older = SNIFF.replace(r#""seq":1,"#, r#""seq":9001,"#);
        let newer = SNIFF.replace(
            r#""run":"0123456789abcdef0123456789abcdef""#,
            r#""run":"ffffffffffffffffffffffffffffffff""#,
        );
        let parsed = parse(format!("{older}\n{newer}\n").as_bytes());
        assert_eq!(parsed.access.len(), 2);
        assert!(parsed.access[0].seq > parsed.access[1].seq, "后一条的 seq 更小");
        assert_ne!(parsed.access[0].run, parsed.access[1].run, "但它们属于不同的 run");
    }
}
