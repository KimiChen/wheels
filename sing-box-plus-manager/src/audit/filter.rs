//! C35：归属裁剪。**在聚合、计数、分页之前。**
//!
//! 「先查出来再按用户过滤显示」与「先裁剪再统计」在页面上看不出区别，直到某一次
//! 分页把别人的记录算进了总数。所以这一层返回的是**已经裁剪过的**记录，
//! 调用方拿到什么就统计什么。
//!
//! # 顺序是固定的
//!
//! 1. 四值 `node/run/in/user` → runtime_identity，**必须唯一**；
//! 2. runtime 已批准；
//! 3. 记录时刻落在该槽位的**历史持有区间**内（半开 `[from, to)`）；
//! 4. 成功判据复核（TCP 双向有字节，UDP 只要上行）。
//!
//! # 为什么第 1 步要在应用层判唯一
//!
//! schema 不保证那条查询只返回一行：三个 UNIQUE 约束**都带 generation**
//! （`UNIQUE(runtime_pk, inbound_tag, generation)`、
//! `UNIQUE(runtime_service_id, identity_name, generation)`），而 JSONL **不带
//! generation**。当前节点实现里 generation 恒为 1，但协议允许它变。
//! 所以关联到多行时**不返回**，而不是取第一行——那正是 C35 那句
//! 「关联不唯一则不返回」的技术成因。
//!
//! # 三类同名二义
//!
//! * **同节点跨入口同名**：`identity_routes` 的唯一键刻意带 `inbound_tag`，
//!   `(node, inA, X)` 归 A、`(node, inB, X)` 归 B 是**合法状态**；而审计文件名只由
//!   身份名编码而来，**两个入口共用同一个文件**，文件里 A 和 B 的行是混在一起的。
//!   这是最容易串号的一种——所以关联必须带上 `in`。
//! * **跨 runtime 名称复用**：两个 runtime 的墙钟区间可以重叠，单看 `ts` 分不开，
//!   所以关联必须带上 `run`。
//! * **同 runtime 内多 generation**：见上，判唯一。
//!
//! # 两处已知偏差，不假装实现
//!
//! * **`clock_ambiguous` 没有实现。** 全仓只有 `schema/02_runtime_lifecycle.sql:93`
//!   那个 CHECK 枚举值，**没有任何写入方、没有推导代码、没有阈值常量**。
//!   最接近的是 `snapshot_batches.time_quality = 'fallback'`，但它粒度是快照批次、
//!   服务于结算归桶，与审计 `ts` 之间没有任何关联字段。不拿它凑一个近似。
//! * **归属时刻用的是主控墙钟**（`identity/mod.rs:55,248`），而
//!   `docs/data-model.md:234` 要求的是「已批准 runtime 的 `started_at_unix_ms`
//!   加单调增量」。那套机制没有实现。而记录的 `ts` 是**节点**的墙钟，
//!   两者是两个时钟——区间边界上的记录因此可能判错。沿用现状并记下这条。

use std::collections::HashMap;

use time::OffsetDateTime;

use crate::audit::record::AccessRecord;
use crate::error::Result;
use crate::ledger::bucket;
use crate::store::Store;

/// 一条记录的归属判定。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Attribution {
    /// 唯一解析到这个 user_id。
    Resolved(i64),
    /// 四值关联到了多行。归属不明——**不返回**。
    Ambiguous,
    /// 关联不到任何已登记的 lineage。不能用当前映射或同名文件兜底。
    Unknown,
    /// runtime 没批准。未登记的 runtime 不可入账，也不该出现在任何人的明细里。
    Unapproved,
    /// 关联到了槽位，但在这条记录的时刻它不属于任何人。
    Unheld,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
struct Key<'a> {
    node: &'a str,
    run: &'a str,
    inbound: &'a str,
    identity: &'a str,
}

#[derive(Debug, Clone)]
struct Lineage {
    route_id: Option<i64>,
    approved: bool,
    ambiguous: bool,
}

#[derive(Debug, Clone)]
struct Holding {
    user_id: i64,
    from: OffsetDateTime,
    to: Option<OffsetDateTime>,
}

/// 裁剪结果。丢掉的分门别类计数——「没有记录」与「有记录但都判不了归属」
/// 在页面上必须说得出区别。
#[derive(Debug, Clone, Default, PartialEq, Eq, serde::Serialize)]
pub struct Filtered {
    /// **不序列化。** 这个结构体整体会作为响应里的 `dropped` 出现，而那一处只该
    /// 有计数。跟着 derive 走的话，每次查询都会把**整份逐连接原始记录**
    /// （含 run / seq / ms）塞进响应——页面只要聚合后的行，多出来的那份既是冗余，
    /// 也是本可以不发生的明细外泄。
    ///
    /// 这个字段本身是给调用方消费的，只是不该出现在 JSON 里。
    #[serde(skip)]
    pub rows: Vec<AccessRecord>,
    /// 关联不到已登记 lineage。
    pub dropped_unknown: usize,
    /// 四值关联到多行。
    pub dropped_ambiguous: usize,
    /// runtime 未批准。
    pub dropped_unapproved: usize,
    /// 那个时刻槽位没有主人，或主人不是本人。
    pub dropped_other_owner: usize,
    /// 没过成功判据。
    pub dropped_unsuccessful: usize,
    /// `ts` 解析不出时间。
    pub dropped_bad_time: usize,
}

impl Filtered {
    pub fn dropped_total(&self) -> usize {
        self.dropped_unknown
            + self.dropped_ambiguous
            + self.dropped_unapproved
            + self.dropped_other_owner
            + self.dropped_unsuccessful
            + self.dropped_bad_time
    }
}

/// 逐条判归属。按四值缓存，因为一份文件里同一个四值会重复成千上万次。
pub struct Resolver<'a> {
    store: &'a Store,
    lineages: HashMap<(String, String, String, String), Lineage>,
    holdings: HashMap<i64, Vec<Holding>>,
}

impl<'a> Resolver<'a> {
    pub fn new(store: &'a Store) -> Self {
        Resolver { store, lineages: HashMap::new(), holdings: HashMap::new() }
    }

    async fn lineage(&mut self, key: Key<'_>) -> Result<Lineage> {
        let cache_key = (
            key.node.to_string(),
            key.run.to_string(),
            key.inbound.to_string(),
            key.identity.to_string(),
        );
        if let Some(found) = self.lineages.get(&cache_key) {
            return Ok(found.clone());
        }
        // 四值关联。**不带 generation**——JSONL 里没有它，所以唯一性要在这里判。
        let rows: Vec<(Option<i64>, String)> = sqlx::query_as(
            "SELECT rt.route_id, r.approval_status \
             FROM node_runtimes r \
             JOIN runtime_services s ON s.runtime_pk = r.runtime_pk \
             JOIN runtime_identities i ON i.runtime_service_id = s.runtime_service_id \
             LEFT JOIN identity_routes rt \
                    ON rt.node_id = r.node_id AND rt.inbound_tag = s.inbound_tag \
                   AND rt.identity_name = i.identity_name \
             WHERE r.node_id = ? AND r.runtime_id = ? \
               AND s.inbound_tag = ? AND i.identity_name = ?",
        )
        .bind(key.node)
        .bind(key.run)
        .bind(key.inbound)
        .bind(key.identity)
        .fetch_all(self.store.readers())
        .await?;

        let lineage = match rows.len() {
            0 => Lineage { route_id: None, approved: false, ambiguous: false },
            1 => {
                Lineage { route_id: rows[0].0, approved: rows[0].1 == "approved", ambiguous: false }
            }
            // 关联不唯一 -> 不返回。取第一行会把两个 generation 的记录混成一个人的。
            _ => Lineage { route_id: None, approved: false, ambiguous: true },
        };
        self.lineages.insert(cache_key, lineage.clone());
        Ok(lineage)
    }

    async fn holdings(&mut self, route_id: i64) -> Result<Vec<Holding>> {
        if let Some(found) = self.holdings.get(&route_id) {
            return Ok(found.clone());
        }
        // **`state = 'assigned'` 是必需的，不是保险。**
        //
        // 退役那条路径会先把上一段区间关掉（`SET effective_to = now`），然后再插一条
        // `unassigned` 事件——而**那条新插的事件 `effective_to` 永远是 NULL**，
        // 之后没有任何代码去关它。它还带着 `user_id`（`identity/mod.rs:290` 绑了）。
        // 于是只按 `user_id` 取区间会得到一条**开口到无穷**的区间，
        // 把退役之后到达的记录全判给这个人。
        let rows: Vec<(Option<i64>, String, Option<String>)> = sqlx::query_as(
            "SELECT user_id, effective_from, effective_to \
             FROM identity_assignment_events \
             WHERE route_id = ? AND state = 'assigned' \
             ORDER BY effective_from",
        )
        .bind(route_id)
        .fetch_all(self.store.readers())
        .await?;

        let holdings: Vec<Holding> = rows
            .into_iter()
            .filter_map(|(user_id, from, to)| {
                Some(Holding {
                    user_id: user_id?,
                    from: bucket::parse_rfc3339(&from)?,
                    // 解析不出来的 effective_to 当成**仍未结束**是错的方向——
                    // 那会把区间开到无穷。当成「这条区间不可用」才是失败关闭。
                    to: match to {
                        None => None,
                        Some(text) => Some(bucket::parse_rfc3339(&text)?),
                    },
                })
            })
            .collect();
        self.holdings.insert(route_id, holdings.clone());
        Ok(holdings)
    }

    /// 判一条记录归谁。
    pub async fn attribute(&mut self, record: &AccessRecord) -> Result<Attribution> {
        let lineage = self
            .lineage(Key {
                node: &record.node,
                run: &record.run,
                inbound: &record.inbound,
                identity: &record.user,
            })
            .await?;
        if lineage.ambiguous {
            return Ok(Attribution::Ambiguous);
        }
        let Some(route_id) = lineage.route_id else {
            return Ok(Attribution::Unknown);
        };
        // 显式写出来，尽管现实里 node_runtimes 只有 'approved' 一个值
        // （唯一的 INSERT/UPDATE 都写死它，'pending'/'rejected' 是死值）。
        // 它现在挡不住任何东西，但它挡的那件事仍然是真的要挡的。
        if !lineage.approved {
            return Ok(Attribution::Unapproved);
        }
        let Some(at) =
            OffsetDateTime::from_unix_timestamp_nanos(record.ts as i128 * 1_000_000).ok()
        else {
            return Ok(Attribution::Unknown);
        };
        for holding in self.holdings(route_id).await? {
            // 半开区间 [from, to)。右端点那一刻属于**下一个**持有者。
            if at >= holding.from && holding.to.is_none_or(|to| at < to) {
                return Ok(Attribution::Resolved(holding.user_id));
            }
        }
        Ok(Attribution::Unheld)
    }
}

/// 裁剪出属于 `subject` 的记录。**管理员按用户查与本人自助查走同一个函数**
/// （C35 明写两者执行同一套过滤规则）——写成两份的话，它们会在某一次改动里分家，
/// 而分家的方向几乎总是管理员那一侧更宽松。
pub async fn for_user(store: &Store, subject: i64, records: Vec<AccessRecord>) -> Result<Filtered> {
    let mut resolver = Resolver::new(store);
    let mut out = Filtered::default();
    for record in records {
        // 成功判据放在归属之后：先判归属能让「不是你的」与「没成功」两类
        // 分得开，而这两个数在页面上的含义完全不同。
        match resolver.attribute(&record).await? {
            Attribution::Resolved(user_id) if user_id == subject => {
                if record.successful() {
                    out.rows.push(record);
                } else {
                    out.dropped_unsuccessful += 1;
                }
            }
            Attribution::Resolved(_) => out.dropped_other_owner += 1,
            Attribution::Unheld => out.dropped_other_owner += 1,
            Attribution::Ambiguous => out.dropped_ambiguous += 1,
            Attribution::Unapproved => out.dropped_unapproved += 1,
            Attribution::Unknown => out.dropped_unknown += 1,
        }
    }
    // 排序按 seq 不按 ts：ts 是墙钟，时钟回拨时 seq 仍严格递增。
    // 但 seq 只在 (user, run) 内单调，所以先按 run 分组再按 seq。
    out.rows.sort_by(|a, b| a.run.cmp(&b.run).then(a.seq.cmp(&b.seq)));
    Ok(out)
}
