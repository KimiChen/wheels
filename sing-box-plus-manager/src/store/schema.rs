//! 建库：一次性把 `schema/` 下的 DDL 建出来。
//!
//! **本项目不做迁移功能**（用户决定，见 README §9 D22）：库是新搭建的，
//! schema 变更靠重建库而不是增量升级。因此这里没有版本表、没有升级路径、没有版本门禁。
//!
//! 代价写明：这意味着**任何 schema 变更都要求丢弃并重建数据库**。
//! 一旦有了需要保留的生产数据，要重估的是这条决策本身，而不是在这里补一个半吊子的升级器。
//!
//! 取而代之的是一道**一致性门禁**：库要么是空的（建全套），要么表集合与期望**完全一致**（放行）。
//! 介于两者之间——少了几张表、或多出不认识的表——一律失败关闭。
//! 「差不多能跑」在结算链路上是最贵的那种状态。

use sqlx::{Row, SqliteConnection};

use crate::error::Result;

/// 按加载顺序排列。顺序有意义：外键引用的表必须先建出来。
const PARTS: &[(&str, &str)] = &[
    ("01_config_dimensions", include_str!("../../schema/01_config_dimensions.sql")),
    ("02_runtime_lifecycle", include_str!("../../schema/02_runtime_lifecycle.sql")),
    ("03_settlement", include_str!("../../schema/03_settlement.sql")),
    ("04_quota", include_str!("../../schema/04_quota.sql")),
    ("05_rollup", include_str!("../../schema/05_rollup.sql")),
    ("06_session", include_str!("../../schema/06_session.sql")),
    ("07_subscription", include_str!("../../schema/07_subscription.sql")),
];

/// 期望的表集合。与 `schema/` 下的 DDL 手工对齐，
/// 由 `tests/m0/store_numeric.rs` 的用例断言两者一致——
/// 一份会漂移的清单比没有清单更糟。
pub const EXPECTED_TABLES: &[&str] = &[
    "counter_cursors",
    "identity_assignment_events",
    "identity_routes",
    "node_runtimes",
    "nodes",
    "quota_groups",
    "quota_requests",
    "quota_setting_audits",
    "quota_settings",
    "quota_update_tasks",
    "rollup_jobs",
    "runtime_identities",
    "runtime_services",
    "server_secrets",
    "sessions",
    "snapshot_batches",
    "snapshot_payloads",
    "sso_used_states",
    "subscription_tokens",
    "usage_cycle_totals",
    "usage_ledger",
    "usage_lifetime_totals",
    "users",
];

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum InitOutcome {
    /// 库是空的，本次建全套。
    Created,
    /// 库已存在且表集合与期望一致，什么都没做。
    AlreadyInitialized,
}

/// 读出当前库里的用户表名（排除 SQLite 自身的 `sqlite_*`）。
pub async fn existing_tables(conn: &mut SqliteConnection) -> Result<Vec<String>> {
    let rows = sqlx::query(
        "SELECT name FROM sqlite_master \
         WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name",
    )
    .fetch_all(&mut *conn)
    .await?;
    Ok(rows.into_iter().map(|row| row.get::<String, _>(0)).collect())
}

/// 每张表的结构指纹：列名、类型、非空、默认值、主键序，按 `cid` 排列。
///
/// **门禁原来只比表名。** 加一列、减一列、改一个默认值，两个方向都静默放行——
/// 也就是说那道「要么空、要么完全一致，介于两者之间失败关闭」的门禁，
/// 挡得住「少一张表」，挡不住「少一列」。而后者恰恰是更常见、也更难发现的那种：
/// 库照常打开，查询照常跑，直到某个 INSERT 撞上一个不存在的列。
pub async fn table_fingerprints(
    conn: &mut SqliteConnection,
) -> Result<std::collections::BTreeMap<String, String>> {
    let mut out = std::collections::BTreeMap::new();
    for table in existing_tables(conn).await? {
        // 表名来自 sqlite_master，不是外部输入；PRAGMA 也不接受绑定参数。
        let rows =
            sqlx::query(&format!("PRAGMA table_info(\"{table}\")")).fetch_all(&mut *conn).await?;
        let mut text = String::new();
        for row in &rows {
            // 分隔符用 US(0x1f) 与 RS(0x1e)：列名与默认值里可能有任何可打印字符，
            // 用逗号拼会让「默认值是 'a,b'」和「两列 a 与 b」撞成同一个指纹。
            text.push_str(&format!(
                "{}\u{1f}{}\u{1f}{}\u{1f}{}\u{1f}{}\u{1e}",
                row.get::<i64, _>(0),
                row.get::<String, _>(1),
                row.get::<String, _>(2),
                row.get::<i64, _>(3),
                row.get::<Option<String>, _>(4).unwrap_or_default(),
            ));
        }
        out.insert(table, text);
    }
    Ok(out)
}

/// 按 `schema/` 下的 DDL 现建一份空库，取它的指纹。
///
/// **不把指纹写成常量。** 写常量要靠人记得同步，而那正是门禁本身要防的那类漂移；
/// 拿 DDL 现算，比的就是「代码里的 DDL」与「磁盘上的库」，中间没有第三份真相。
pub async fn expected_fingerprints() -> Result<std::collections::BTreeMap<String, String>> {
    use sqlx::Connection;
    let mut conn = SqliteConnection::connect("sqlite::memory:").await?;
    for (name, sql) in PARTS {
        sqlx::raw_sql(sql)
            .execute(&mut conn)
            .await
            .map_err(|e| crate::error::Error::Codec(format!("建表 {name} 失败：{e}")))?;
    }
    let out = table_fingerprints(&mut conn).await?;
    let _ = conn.close().await;
    Ok(out)
}

/// 列层面的漂移。空表示一致。
pub async fn column_drift(conn: &mut SqliteConnection) -> Result<Vec<String>> {
    let actual = table_fingerprints(conn).await?;
    let expected = expected_fingerprints().await?;
    let mut drift = Vec::new();
    for (table, want) in &expected {
        match actual.get(table) {
            Some(have) if have == want => {}
            Some(have) => drift.push(format!(
                "{table} 的列与 DDL 不一致：库里 {} 列，DDL {} 列",
                have.matches('\u{1e}').count(),
                want.matches('\u{1e}').count()
            )),
            None => drift.push(format!("{table} 不在库里")),
        }
    }
    Ok(drift)
}

/// 建库或确认已建好。整套 DDL 在**一个事务**里提交：
/// 建到一半失败不能留下一个「部分建好」的库，那正是上面那道门禁最难判断的状态。
pub async fn initialize(conn: &mut SqliteConnection) -> Result<InitOutcome> {
    let present = existing_tables(conn).await?;
    if !present.is_empty() {
        let expected: Vec<String> = EXPECTED_TABLES.iter().map(|s| s.to_string()).collect();
        if present == expected {
            // **表名一致还不够。** 原来这里就返回了，于是加一列、减一列、
            // 改一个默认值，两个方向都静默放行——门禁挡得住「少一张表」，
            // 挡不住「少一列」，而后者更常见也更难发现：库照常打开、
            // 查询照常跑，直到某个 INSERT 撞上一个不存在的列。
            let drift = column_drift(conn).await?;
            if !drift.is_empty() {
                return Err(crate::error::Error::Codec(format!(
                    "数据库的表名对得上，但**列与 DDL 不一致**：{drift:?}。\
                     本项目不做通用迁移引擎（D32）：请按「备份 → 在恢复出来的副本上\
                     跑通导入 → 重算核对 → 才对生产执行」这条路径处理"
                )));
            }
            return Ok(InitOutcome::AlreadyInitialized);
        }
        let missing: Vec<&str> = EXPECTED_TABLES
            .iter()
            .copied()
            .filter(|name| !present.iter().any(|p| p == name))
            .collect();
        let unexpected: Vec<&str> = present
            .iter()
            .map(String::as_str)
            .filter(|name| !EXPECTED_TABLES.contains(name))
            .collect();
        return Err(crate::error::Error::Codec(format!(
            "数据库已存在但表集合与期望不一致：缺少 {missing:?}，多出 {unexpected:?}。\
             本项目不做通用迁移引擎（D32），请按备份 → 副本上导入 → 重算核对的路径处理"
        )));
    }

    sqlx::query("BEGIN IMMEDIATE").execute(&mut *conn).await?;
    for (name, sql) in PARTS {
        if let Err(error) = sqlx::raw_sql(sql).execute(&mut *conn).await {
            let _ = sqlx::query("ROLLBACK").execute(&mut *conn).await;
            return Err(crate::error::Error::Codec(format!("建表 {name} 失败：{error}")));
        }
    }
    if let Err(error) = seed_quota_groups(&mut *conn).await {
        let _ = sqlx::query("ROLLBACK").execute(&mut *conn).await;
        return Err(error);
    }
    sqlx::query("COMMIT").execute(&mut *conn).await?;
    Ok(InitOutcome::Created)
}

/// 四个档位的初值（定案第三条）。**十进制 TB**，1 TB = 1 000 000 000 000 字节。
///
/// `(group_name, display_name, monthly_bytes, sort_order)`
pub const SEED_QUOTA_GROUPS: &[(&str, &str, u64, i64)] = &[
    ("normal", "普通用户", 1_000_000_000_000, 0),
    ("advanced", "Advanced", 2_000_000_000_000, 1),
    ("manage", "Manage", 5_000_000_000_000, 2),
    ("admin", "Admin", 10_000_000_000_000, 3),
];

/// 在建库同一个事务里把四档写进去。
///
/// 放在建库事务内而不是留给首次启动：`users.quota_group` 是 NOT NULL 外键，
/// 库一旦存在就必须有可指向的行，否则第一个建号事务会失败在一个
/// 与它自己无关的地方。四档是被明确批准的闭集，不是运行期配置。
/// 之后改额度一律走 `quota::settings` 的审计路径，那里会推进 revision 并生成下发任务。
async fn seed_quota_groups(conn: &mut SqliteConnection) -> Result<()> {
    let now = crate::ledger::bucket::to_rfc3339(time::OffsetDateTime::now_utc());
    for (name, display, bytes, order) in SEED_QUOTA_GROUPS {
        sqlx::query(
            "INSERT INTO quota_groups(group_name, display_name, monthly_bytes, sort_order, \
             updated_by, updated_at) VALUES (?, ?, ?, ?, 'schema', ?)",
        )
        .bind(name)
        .bind(display)
        .bind(crate::store::codec::U64Text::new(*bytes).encode())
        .bind(order)
        .bind(&now)
        .execute(&mut *conn)
        .await?;
    }
    Ok(())
}
