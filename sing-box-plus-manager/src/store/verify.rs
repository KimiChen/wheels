//! 一份库的自洽性核对（README §10 的 R3）。
//!
//! R3 要求「一致性备份覆盖账本、累计、游标与配额请求**并演练恢复**」。
//! 演练的判据不能是「文件能打开」——那句话对一个被截断到一半的文件也成立。
//!
//! 这里的判据只有一条主线：**总账必须能从账本重新算出来。**
//! 这个系统其他每一条正确性性质都建立在可重算之上（计划里的原话），
//! 而一次恢复演练要证明的恰恰是「重算之后还对得上」。
//!
//! 与 `ledger stats` 的四项判据是互补的两件事：
//! 四项判据说的是「这四类错误没发生」，**它与「什么都没计费」完全兼容**；
//! 这里说的是「记下来的这些数彼此自洽」。两个都要。

use std::collections::BTreeMap;

use sqlx::Row;

use crate::error::Result;
use crate::ledger::settle::{cycle_key_for, CYCLE_RULE_VERSION};
use crate::store::codec::U128Text;
use crate::store::Store;

/// 一条对不上的账。
#[derive(Debug, Clone)]
pub struct Mismatch {
    pub what: String,
    pub key: String,
    pub recorded: u128,
    pub recomputed: u128,
}

impl std::fmt::Display for Mismatch {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(
            f,
            "{} {}：记的是 {}，从账本算出来是 {}（差 {})",
            self.what,
            self.key,
            self.recorded,
            self.recomputed,
            self.recorded.abs_diff(self.recomputed)
        )
    }
}

#[derive(Debug, Default)]
pub struct Report {
    pub integrity: String,
    pub foreign_key_violations: i64,
    pub missing_tables: Vec<String>,
    pub unexpected_tables: Vec<String>,
    /// 核对过的 `(user_id, cycle_key, rule_version)` 组数。
    pub cycles_checked: usize,
    /// 核对过的 `runtime_identity_id` 个数。
    pub lifetimes_checked: usize,
    pub mismatches: Vec<Mismatch>,
    /// 每个 `(node_id, runtime_id)` 的 epoch 水位。
    ///
    /// **epoch 是 per-(node_id, runtime_id) 的**（C31），不是全局的——
    /// 报一个全局最大值会让「这个节点的水位是多少」这个问题得到一个
    /// 看起来像答案的数字。没有这些值，一次重建会让四台节点全部 409 stale_epoch。
    pub epoch_high_water: Vec<(String, String, u64)>,
    /// 列的形状不对（类型与 DDL 的 CHECK 不符）。**不 panic，报出来。**
    ///
    /// 这是唯一一个必须能在坏库上跑完的工具：panic 掉等于「这份备份有问题」
    /// 这个结论永远打印不出来。
    pub shape_errors: Vec<String>,
    /// 载荷的编码分布：codec → (行数, 落库字节数, 原文字节数)。
    pub payload_codecs: Vec<(String, i64, i64, i64)>,
    pub counts: BTreeMap<String, i64>,
}

impl Report {
    /// **这份库自洽吗。**
    pub fn ok(&self) -> bool {
        self.integrity == "ok"
            && self.foreign_key_violations == 0
            && self.missing_tables.is_empty()
            && self.unexpected_tables.is_empty()
            && self.mismatches.is_empty()
            && self.shape_errors.is_empty()
    }
}

pub async fn verify(store: &Store) -> Result<Report> {
    let integrity =
        sqlx::query("PRAGMA integrity_check").fetch_one(store.readers()).await?.get::<String, _>(0);
    let foreign_key_violations =
        sqlx::query("PRAGMA foreign_key_check").fetch_all(store.readers()).await?.len() as i64;
    let mut report = Report { integrity, foreign_key_violations, ..Report::default() };

    // 表集合。与 `init_schema` 的一致性门禁是同一份 EXPECTED_TABLES——
    // 那道门禁只在启动时跑，而恢复出来的副本要在**启动之前**就知道它过不过得去。
    let present: Vec<String> = sqlx::query(
        "SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'",
    )
    .fetch_all(store.readers())
    .await?
    .iter()
    .map(|row| row.get::<String, _>(0))
    .collect();
    for expected in crate::store::schema::EXPECTED_TABLES {
        if !present.iter().any(|name| name == expected) {
            report.missing_tables.push((*expected).to_string());
        }
    }
    for name in &present {
        if !crate::store::schema::EXPECTED_TABLES.contains(&name.as_str()) {
            report.unexpected_tables.push(name.clone());
        }
    }

    for table in crate::store::schema::EXPECTED_TABLES {
        if report.missing_tables.iter().any(|m| m == table) {
            continue;
        }
        let n: i64 = sqlx::query(&format!("SELECT count(*) FROM \"{table}\""))
            .fetch_one(store.readers())
            .await?
            .get(0);
        report.counts.insert((*table).to_string(), n);
    }

    // epoch 是**定宽零填充的十进制文本**（C3），不是 INTEGER——
    // 定宽正是为了让 `MAX()` 在 BINARY 排序下等于数值最大值。
    for row in sqlx::query(
        "SELECT node_id, runtime_id, MAX(epoch) FROM quota_requests GROUP BY node_id, runtime_id",
    )
    .fetch_all(store.readers())
    .await?
    {
        let node: String = row.get(0);
        let runtime: String = row.get(1);
        // **类型不对要报出来，不能吞掉。** 吞掉的话这份库会被报成「没有水位」，
        // 而那句话的下一步是「人工补水位」——对着一份形状不对的库补水位，
        // 是在一个错误的诊断上继续施工。
        match row.try_get::<Option<String>, _>(2) {
            Ok(None) => {}
            Ok(Some(text)) => match crate::store::codec::U64Text::decode(&text) {
                Ok(epoch) => report.epoch_high_water.push((node, runtime, epoch.get())),
                Err(error) => report
                    .shape_errors
                    .push(format!("quota_requests({node}/{runtime}).epoch 解不出来：{error}")),
            },
            Err(error) => report.shape_errors.push(format!(
                "quota_requests({node}/{runtime}).epoch 的列类型与 DDL 不符：{error}"
            )),
        }
    }

    for row in sqlx::query(
        "SELECT codec, count(*), sum(length(payload)), sum(raw_length) \
           FROM snapshot_payloads GROUP BY codec ORDER BY codec",
    )
    .fetch_all(store.readers())
    .await?
    {
        report.payload_codecs.push((
            row.get::<String, _>(0),
            row.get::<i64, _>(1),
            row.get::<Option<i64>, _>(2).unwrap_or(0),
            row.get::<Option<i64>, _>(3).unwrap_or(0),
        ));
    }

    recompute(store, &mut report).await?;
    Ok(report)
}

/// 从 `usage_ledger` 把两份总账重算一遍。
///
/// **周期键在 Rust 里算，不在 SQL 里算。** 它是 `accounting_at` 与 `rule_version`
/// 的纯函数（UTC+8 切月），而 SQLite 没有这个口径；在 SQL 里用 `substr(accounting_at,1,7)`
/// 拼一个出来，等于写第二份实现——两份对月界的理解会在某个月初的那 8 小时里分家，
/// 而那正是它最难被发现的时候。
async fn recompute(store: &Store, report: &mut Report) -> Result<()> {
    let mut by_cycle: BTreeMap<(i64, String, i64), u128> = BTreeMap::new();
    let mut by_runtime_identity: BTreeMap<i64, u128> = BTreeMap::new();

    let rows = sqlx::query(
        "SELECT user_id, runtime_identity_id, accounting_at, \
                tcp_uplink_bytes, tcp_downlink_bytes, udp_uplink_bytes, udp_downlink_bytes \
           FROM usage_ledger",
    )
    .fetch_all(store.readers())
    .await?;

    for row in &rows {
        let mut sum: u128 = 0;
        for index in 3..7 {
            sum +=
                crate::store::codec::U64Text::decode(&row.get::<String, _>(index))?.get() as u128;
        }
        *by_runtime_identity.entry(row.get::<i64, _>(1)).or_default() += sum;

        // `user_id` 可空：未登记身份走的是失败开放，账本行照写但没有主人（C25）。
        // 那些字节不属于任何周期总账，这里也就不该把它们算进去。
        let Some(user_id) = row.get::<Option<i64>, _>(0) else { continue };
        let at =
            crate::ledger::bucket::parse_rfc3339(&row.get::<String, _>(2)).ok_or_else(|| {
                crate::error::Error::Ledger("账本行的 accounting_at 不是合法 RFC 3339".into())
            })?;
        let key = cycle_key_for(at, CYCLE_RULE_VERSION)?;
        *by_cycle.entry((user_id, key, CYCLE_RULE_VERSION)).or_default() += sum;
    }

    for row in
        sqlx::query("SELECT user_id, cycle_key, rule_version, total_bytes FROM usage_cycle_totals")
            .fetch_all(store.readers())
            .await?
    {
        let key = (row.get::<i64, _>(0), row.get::<String, _>(1), row.get::<i64, _>(2));
        let recorded = U128Text::decode(&row.get::<String, _>(3))?.get();
        let recomputed = by_cycle.remove(&key).unwrap_or(0);
        report.cycles_checked += 1;
        if recorded != recomputed {
            report.mismatches.push(Mismatch {
                what: "周期总账".into(),
                key: format!("user={} cycle={} rule={}", key.0, key.1, key.2),
                recorded,
                recomputed,
            });
        }
    }
    // 账本里有、总账里没有：**这是漏记**，比对不上更糟——它连一行都没有。
    for (key, recomputed) in by_cycle {
        report.mismatches.push(Mismatch {
            what: "周期总账缺行".into(),
            key: format!("user={} cycle={} rule={}", key.0, key.1, key.2),
            recorded: 0,
            recomputed,
        });
    }

    for row in sqlx::query(
        "SELECT runtime_identity_id, tcp_uplink_bytes, tcp_downlink_bytes, \
                udp_uplink_bytes, udp_downlink_bytes FROM usage_lifetime_totals",
    )
    .fetch_all(store.readers())
    .await?
    {
        let id: i64 = row.get(0);
        let mut recorded: u128 = 0;
        for index in 1..5 {
            recorded += U128Text::decode(&row.get::<String, _>(index))?.get();
        }
        let recomputed = by_runtime_identity.remove(&id).unwrap_or(0);
        report.lifetimes_checked += 1;
        if recorded != recomputed {
            report.mismatches.push(Mismatch {
                what: "永久累计".into(),
                key: format!("runtime_identity={id}"),
                recorded,
                recomputed,
            });
        }
    }
    for (id, recomputed) in by_runtime_identity {
        report.mismatches.push(Mismatch {
            what: "永久累计缺行".into(),
            key: format!("runtime_identity={id}"),
            recorded: 0,
            recomputed,
        });
    }

    Ok(())
}
