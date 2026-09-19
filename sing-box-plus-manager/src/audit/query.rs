//! 从本机镜像树直接查，不落库。
//!
//! # 文件名只用来缩小候选集，不是授权依据
//!
//! 先按「这个人曾经持有过哪些槽位」算出候选文件名，只读那些文件——否则每次查询
//! 都要读整棵树。但**逐行的完整身份校验一条都不少**（C35 的 [`filter::for_user`]）：
//! 同一个身份名可能在两个入口上分属两人，而那两个入口**共用同一个文件**。
//! 把文件名当授权依据，正好会在那种情况下串号。
//!
//! 候选集同时取自 `identity_routes.user_id` 与 `identity_assignment_events.user_id`：
//! 前者是当前绑定（退役不解绑，所以历史槽位也在里面），后者覆盖「这个槽位换过主人」
//! 那种情况——schema 上归属是写一次不变的，但事件表能表达交接，而漏掉交接前那一段
//! 的症状是「我上个月的记录不见了」，不会有任何东西报错。
//!
//! # 时间范围用的是节点墙钟
//!
//! `ts` 是节点 writer 的写出时刻。节点与主控是两个时钟，所以范围边界上的记录可能
//! 差几秒。这条与 C35 归属时刻那条偏差同源，都记在 [`crate::audit::filter`] 里。

use std::collections::BTreeMap;
use std::path::Path;

use serde::Serialize;

use crate::audit::filter::{self, Filtered};
use crate::audit::health;
use crate::audit::record::{self, AccessRecord, Diagnostic, HostSource};
use crate::error::{Error, Result};
use crate::ledger::bucket;
use crate::store::Store;
use proxy_manager_wire::audit;

/// 允许的时间范围。闭集——任意时长会让「这一页有多贵」变得不可预期。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Range {
    Day,
    Week,
    Month,
}

impl Range {
    pub fn parse(text: &str) -> Option<Range> {
        match text {
            "24h" => Some(Range::Day),
            "7d" => Some(Range::Week),
            "30d" => Some(Range::Month),
            _ => None,
        }
    }

    pub fn as_str(self) -> &'static str {
        match self {
            Range::Day => "24h",
            Range::Week => "7d",
            Range::Month => "30d",
        }
    }

    fn millis(self) -> i64 {
        match self {
            Range::Day => 24 * 3_600_000,
            Range::Week => 7 * 24 * 3_600_000,
            Range::Month => 30 * 24 * 3_600_000,
        }
    }
}

/// 聚合后的一行。**按 (host, port, host_src) 聚合**——`host_src` 进聚合键是刻意的：
/// 同一个域名既有 `sniff` 又有 `fqdn` 时，那两行的可信度不同，合并会把区别抹掉。
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct Row {
    pub host: String,
    pub port: u16,
    pub host_src: HostSource,
    pub first_seen: String,
    pub last_seen: String,
    pub count: u64,
    pub up: u64,
    pub down: u64,
}

/// 一个缺口。**不带「丢了 N 条」的估算**，除非节点自己给了 `n`。
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct Gap {
    /// `queue_full` / `total_cap` / `audit_dropped`。
    pub kind: String,
    pub node_id: String,
    /// `ev=gap` 才有。
    #[serde(skip_serializing_if = "Option::is_none")]
    pub after_seq: Option<u64>,
    /// 节点给的条数。**可以是 null**——边界不确定时它刻意不合成一个不存在的区间，
    /// 主控也不得把缺号换算成「确定丢失了 N 条」。
    #[serde(skip_serializing_if = "Option::is_none")]
    pub n: Option<u64>,
    /// `audit_dropped` 才有：主控观察到的时刻。
    #[serde(skip_serializing_if = "Option::is_none")]
    pub at: Option<String>,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct Answer {
    pub enabled: bool,
    pub range: String,
    pub rows: Vec<Row>,
    pub gaps: Vec<Gap>,
    /// `available` / `no_data` / `unavailable`。
    ///
    /// **`no_data` 不等于「没有访问」。** 可能是这个人名下一个槽位都没有、
    /// 也可能是同步还没跑过。页面要说得出这个区别。
    pub archive_state: &'static str,
    /// 返回的记录里最新那一条的时刻。**不是完整覆盖水位**——
    /// 同步是周期性的，它之后的记录可能还在节点上。
    pub latest_available_event_at: Option<String>,
    /// 节点只按大小轮转，不支持按时间轮转，所以可见延迟没有上界保证。
    pub archive_delay_bounded: bool,
    /// 裁剪掉的条数，分门别类。「没有记录」与「有记录但都判不了归属」
    /// 在页面上必须说得出区别。
    pub dropped: Filtered,
    /// 解析不了的行数，以及带了未知键的行数。上游形状漂移的唯一信号。
    pub unparsed: usize,
    pub unexpected_keys: usize,
}

/// 这个人可能持有过的身份名。**取当前绑定与历史事件的并集。**
async fn candidate_identities(store: &Store, user_id: i64) -> Result<Vec<String>> {
    let rows: Vec<(String,)> = sqlx::query_as(
        "SELECT DISTINCT r.identity_name FROM identity_routes r \
         WHERE r.user_id = ? \
         UNION \
         SELECT DISTINCT r.identity_name FROM identity_routes r \
         JOIN identity_assignment_events e ON e.route_id = r.route_id \
         WHERE e.user_id = ? AND e.state = 'assigned'",
    )
    .bind(user_id)
    .bind(user_id)
    .fetch_all(store.readers())
    .await?;
    Ok(rows.into_iter().map(|(name,)| name).collect())
}

/// 查一个人的出站目标明细。
///
/// `subject` 是**数据主体**，不是调用者。管理员按用户查与本人自助查走同一个函数，
/// 只是这个参数的来源不同（C35 明写两者执行同一套过滤规则）。
pub async fn access(
    store: &Store,
    root: &Path,
    subject: i64,
    range: Range,
    now_ms: i64,
) -> Result<Answer> {
    let identities = candidate_identities(store, subject).await?;
    // 前缀是 `access-<b64url>.jsonl`，匹配方式是 `starts_with`——于是它同时命中
    // 活动文件与它的全部归档（`…​.jsonl.<定宽时间戳>`）。
    //
    // **那个 `.jsonl` 不是装饰。** 去掉它，`user0001` 的前缀会命中 `user00010`
    // 的文件（base64url 下 `dXNlcjAwMDE` 是 `dXNlcjAwMDEw` 的前缀）。访问行有 C35
    // 兜底，但**诊断行没有四元组、裁剪不了**——它是按文件收的，于是别人文件里的
    // 缺口会原样显示给你。
    let prefixes: Vec<String> =
        identities.iter().map(|name| audit::active_file_name(name)).collect();

    let mut records: Vec<AccessRecord> = Vec::new();
    let mut gaps: Vec<Gap> = Vec::new();
    let mut unparsed = 0usize;
    let mut unexpected_keys = 0usize;
    let mut readable_nodes = 0usize;

    let since = now_ms - range.millis();
    let entries = match std::fs::read_dir(root) {
        Ok(entries) => entries,
        // 目录不存在 = 同步从没跑过。**这与「没有访问」是两回事**，
        // 所以回 unavailable 而不是一个空的 available。
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
            return Ok(empty(range, "unavailable"))
        }
        Err(error) => return Err(Error::Audit(format!("列审计树：{error}"))),
    };

    for entry in entries {
        let entry = entry.map_err(|e| Error::Audit(format!("列审计树条目：{e}")))?;
        if !entry.path().is_dir() {
            continue;
        }
        let node_id = entry.file_name().to_string_lossy().to_string();
        readable_nodes += 1;

        for record_file in std::fs::read_dir(entry.path())
            .map_err(|e| Error::Audit(format!("列 {node_id}：{e}")))?
        {
            let record_file =
                record_file.map_err(|e| Error::Audit(format!("列 {node_id} 条目：{e}")))?;
            let name = record_file.file_name().to_string_lossy().to_string();
            if audit::name_kind(&name).is_none() {
                continue;
            }
            // 缩小候选集：只读这个人可能持有过的身份的文件。
            // **这不是授权**——下面 filter::for_user 会逐行再校验一遍。
            if !prefixes.iter().any(|prefix| name.starts_with(prefix.as_str())) {
                continue;
            }
            let bytes = std::fs::read(record_file.path())
                .map_err(|e| Error::Audit(format!("读 {name}：{e}")))?;
            let parsed = record::parse(&bytes);
            unparsed += parsed.unparsed;
            unexpected_keys += parsed.unexpected_keys;
            records.extend(parsed.access.into_iter().filter(|r| r.ts >= since));
            for diagnostic in parsed.diagnostics {
                // 诊断行**没有四元组**，所以严格说它归属不到人。但它就落在这个身份的
                // 文件里，而文件是按身份分的——把它藏起来的代价是「这段时间的记录
                // 可能不全」这件事没人看得见。显示它，并且不声称它一定是这个人的。
                if let Diagnostic::Gap { after, n, reason, .. } = diagnostic {
                    gaps.push(Gap {
                        kind: reason,
                        node_id: node_id.clone(),
                        after_seq: Some(after),
                        n,
                        at: None,
                    });
                }
            }
        }

        for dropped in health::read(&entry.path())? {
            gaps.push(Gap {
                kind: "audit_dropped".into(),
                node_id: node_id.clone(),
                after_seq: None,
                n: None,
                at: Some(dropped.at),
            });
        }
    }

    if readable_nodes == 0 {
        return Ok(empty(range, "unavailable"));
    }

    // **裁剪在聚合之前**（C35）。
    let filtered = filter::for_user(store, subject, records).await?;
    let mut buckets: BTreeMap<(String, u16, &'static str), Row> = BTreeMap::new();
    for record in &filtered.rows {
        let at = bucket::to_rfc3339(
            time::OffsetDateTime::from_unix_timestamp_nanos(record.ts as i128 * 1_000_000)
                .unwrap_or(time::OffsetDateTime::UNIX_EPOCH),
        );
        let row = buckets
            .entry((record.host.clone(), record.port, record.host_src.as_str()))
            .or_insert_with(|| Row {
                host: record.host.clone(),
                port: record.port,
                host_src: record.host_src,
                first_seen: at.clone(),
                last_seen: at.clone(),
                count: 0,
                up: 0,
                down: 0,
            });
        row.count += 1;
        row.up += record.up;
        row.down += record.down;
        if at < row.first_seen {
            row.first_seen = at.clone();
        }
        if at > row.last_seen {
            row.last_seen = at;
        }
    }

    let mut rows: Vec<Row> = buckets.into_values().collect();
    // 次数多的在前；同次数按最近一次访问排。人找「陌生的目标」时，
    // 最有用的排法是「你自己都不记得的那些」，而那通常次数少——
    // 但先给一个稳定且可预期的顺序，筛选交给页面。
    rows.sort_by(|a, b| b.count.cmp(&a.count).then(b.last_seen.cmp(&a.last_seen)));
    let latest = rows.iter().map(|r| r.last_seen.clone()).max();
    let archive_state =
        if rows.is_empty() && filtered.dropped_total() == 0 { "no_data" } else { "available" };

    Ok(Answer {
        enabled: true,
        range: range.as_str().to_string(),
        rows,
        gaps,
        archive_state,
        latest_available_event_at: latest,
        // 节点不支持按时间轮转，而活动文件是按同步周期增量拉的——
        // 所以延迟由同步周期决定，但**没有上界保证**（一次同步失败就会拖长）。
        archive_delay_bounded: false,
        dropped: filtered,
        unparsed,
        unexpected_keys,
    })
}

fn empty(range: Range, state: &'static str) -> Answer {
    Answer {
        enabled: true,
        range: range.as_str().to_string(),
        rows: Vec::new(),
        gaps: Vec::new(),
        archive_state: state,
        latest_available_event_at: None,
        archive_delay_bounded: false,
        dropped: Filtered::default(),
        unparsed: 0,
        unexpected_keys: 0,
    }
}
