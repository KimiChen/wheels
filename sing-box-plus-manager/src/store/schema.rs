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
    ("08_custom_nodes", include_str!("../../schema/08_custom_nodes.sql")),
];

/// 期望的表集合。与 `schema/` 下的 DDL 手工对齐，
/// 由 `tests/m0/store_numeric.rs` 的用例断言两者一致——
/// 一份会漂移的清单比没有清单更糟。
pub const EXPECTED_TABLES: &[&str] = &[
    "counter_cursors",
    "custom_proxies",
    "custom_socks5",
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

/// 指纹的两段之间的分隔符：GS(0x1d)。列段在前，DDL 段在后。
const SECTION: char = '\u{1d}';

/// 把一段 DDL 规范化：剥掉注释、把连续空白折成一个空格、去掉标点周围的空白。
///
/// **只做这三件保守的事。** 不做关键字大小写归一——那会连带改到标识符与字符串
/// 字面量，而「`DEFAULT 'Active'` 变成 `DEFAULT 'active'`」是一处真实的漂移。
///
/// 三件事都必须**认字符串字面量**：`DEFAULT '--'` 里那两横不是注释，
/// `DEFAULT 'a, b'` 里那个空格也不是排版。不认的话，规范化本身就会把两份不同的
/// DDL 抹成同一个指纹——一道门禁最不该有的毛病。
///
/// 去标点空白这一条是必须的而不是锦上添花：只折空白的话，`t(a TEXT` 与
/// `t(\n    a TEXT` 会得出两个指纹，于是**在 schema 里加一句注释就等于「请重建
/// 数据库」**。这个仓库的 DDL 里注释比语句多，那样的门禁会在一周内被人绕过。
/// 标点周围的空白在 SQL 里不承载任何语义，去掉是安全的。
fn normalize_ddl(sql: &str) -> String {
    // 第一趟：剥注释，同时记下每个字符是不是在字符串字面量里。
    let mut body: Vec<(char, bool)> = Vec::with_capacity(sql.len());
    let mut chars = sql.chars().peekable();
    let mut in_string = false;
    while let Some(ch) = chars.next() {
        if in_string {
            body.push((ch, true));
            if ch == '\'' {
                in_string = false;
            }
            continue;
        }
        match ch {
            '\'' => {
                in_string = true;
                body.push((ch, true));
            }
            '-' if chars.peek() == Some(&'-') => {
                for next in chars.by_ref() {
                    if next == '\n' {
                        break;
                    }
                }
                body.push((' ', false));
            }
            '/' if chars.peek() == Some(&'*') => {
                chars.next();
                let mut prev = '\0';
                for next in chars.by_ref() {
                    if prev == '*' && next == '/' {
                        break;
                    }
                    prev = next;
                }
                body.push((' ', false));
            }
            _ => body.push((ch, false)),
        }
    }

    // 第二趟：串外的连续空白折成一个空格，串内原样。
    let mut folded: Vec<(char, bool)> = Vec::with_capacity(body.len());
    for (ch, quoted) in body {
        if !quoted && ch.is_whitespace() {
            if !matches!(folded.last(), Some((' ', false))) {
                folded.push((' ', false));
            }
            continue;
        }
        folded.push((ch, quoted));
    }

    // 第三趟：串外紧挨标点的空格去掉。
    const PUNCT: [char; 3] = ['(', ')', ','];
    let mut out = String::with_capacity(folded.len());
    for (index, (ch, quoted)) in folded.iter().enumerate() {
        if !quoted && *ch == ' ' {
            let before = index.checked_sub(1).and_then(|i| folded.get(i));
            let after = folded.get(index + 1);
            let touches =
                |slot: Option<&(char, bool)>| matches!(slot, Some((c, false)) if PUNCT.contains(c));
            if touches(before) || touches(after) {
                continue;
            }
        }
        out.push(*ch);
    }
    out.trim().to_string()
}

/// 每张表的结构指纹：**列段**（列名、类型、非空、默认值、主键序，按 `cid` 排列）
/// 加 **DDL 段**（表自身的 `sqlite_master.sql`，以及 `tbl_name` 指向它的全部索引）。
///
/// **门禁原来只比表名。** 加一列、减一列、改一个默认值，两个方向都静默放行——
/// 也就是说那道「要么空、要么完全一致，介于两者之间失败关闭」的门禁，
/// 挡得住「少一张表」，挡不住「少一列」。而后者恰恰是更常见、也更难发现的那种：
/// 库照常打开，查询照常跑，直到某个 INSERT 撞上一个不存在的列。
///
/// # 为什么还要 DDL 段（2026-09-20）
///
/// `PRAGMA table_info` 只给列名 / 类型 / 非空 / 默认值 / 主键序——**看不见
/// UNIQUE、CHECK 与索引**。于是「把唯一键从三列改成两列」这种改动对门禁完全隐形：
/// 旧库顺利通过启动检查，然后 `ON CONFLICT` 报 `no unique index matching`，
/// 而更坏的一支是 `pick_free_slot` 在旧库上把行数数成两倍——**池子永远空、零报错**。
/// D32 自己也把这一条记成了「门禁不认的东西」。
///
/// 代价：非注释的排版变动会误报。但空白已经被折平、注释已经剥掉，剩下的误报
/// （比如把 `CHECK` 的两个条件调换顺序）触发的动作是「重建库」，
/// 恰好就是这个项目本来的规则（D32 不做通用迁移引擎），所以代价接近零。
///
/// 已知陷阱：`ALTER TABLE … RENAME` 会重写 `sqlite_master.sql` 的文本。本仓不做
/// ALTER——`tests/m1/backup.rs` 里那条造漂移的用例是故意的，它正该被抓住。
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
                "{}\u{1f}{}\u{1f}{}\u{1f}{}\u{1f}{}\u{1f}{}\u{1e}",
                row.get::<i64, _>(0),
                row.get::<String, _>(1),
                row.get::<String, _>(2),
                row.get::<i64, _>(3),
                row.get::<Option<String>, _>(4).unwrap_or_default(),
                row.get::<i64, _>(5),
            ));
        }
        text.push(SECTION);
        // 表自身与指向它的索引。**按 name 排序**，不靠 rowid——建表顺序不是结构。
        // `sql IS NULL` 的是 UNIQUE 自动生成的索引，它的内容已经在表的 DDL 文本里，
        // 而它的名字（`sqlite_autoindex_<表>_<序号>`）会随无关的增删而改号。
        let ddl = sqlx::query(
            "SELECT sql FROM sqlite_master \
             WHERE tbl_name = ? AND sql IS NOT NULL AND type IN ('table', 'index') \
             ORDER BY type, name",
        )
        .bind(&table)
        .fetch_all(&mut *conn)
        .await?;
        for row in &ddl {
            text.push_str(&normalize_ddl(&row.get::<String, _>(0)));
            text.push('\u{1e}');
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

/// 结构层面的漂移：列、约束与索引。空表示一致。
///
/// 报错要分得开「列不一样」与「列一样但约束/索引不一样」：前者的症状是
/// INSERT 撞上不存在的列，后者的症状是**没有症状**——库照常跑，
/// 直到某个 `ON CONFLICT` 找不到匹配的唯一索引，或者某条 `count(*)` 悄悄翻倍。
pub async fn schema_drift(conn: &mut SqliteConnection) -> Result<Vec<String>> {
    let actual = table_fingerprints(conn).await?;
    let expected = expected_fingerprints().await?;
    let mut drift = Vec::new();
    let columns_of = |text: &str| text.split(SECTION).next().unwrap_or_default().to_string();
    for (table, want) in &expected {
        match actual.get(table) {
            Some(have) if have == want => {}
            Some(have) => {
                let (have_cols, want_cols) = (columns_of(have), columns_of(want));
                if have_cols == want_cols {
                    drift.push(format!(
                        "{table} 的列与 DDL 一致，但**约束或索引**不一致（UNIQUE / CHECK / CREATE INDEX）"
                    ));
                } else {
                    drift.push(format!(
                        "{table} 的列与 DDL 不一致：库里 {} 列，DDL {} 列",
                        have_cols.matches('\u{1e}').count(),
                        want_cols.matches('\u{1e}').count()
                    ));
                }
            }
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
            //
            // 2026-09-20 起还认约束与索引。换唯一键这类改动比少一列更隐蔽：
            // 它连 INSERT 都撞不上，只会让 `ON CONFLICT` 找不到匹配的唯一索引，
            // 或者让某条 `count(*)` 悄悄翻倍。
            let drift = schema_drift(conn).await?;
            if !drift.is_empty() {
                return Err(crate::error::Error::Codec(format!(
                    "数据库的表名对得上，但**结构与 DDL 不一致**：{drift:?}。\
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

/// 四个档位的初值（定案第三条）。**二进制 TiB**，1 TiB = 2^40 = 1 099 511 627 776 字节。
///
/// # 为什么从十进制改成二进制（2026-09-20）
///
/// 原本是十进制 TB（`10^12`），理由是「TB 在计费语境里是十进制」。
/// 实际用下来它每天都在说错话：**客户端按 GiB 显示**，于是一个叫「1 TB」的档
/// 在用户屏幕上是 `931 G`。档位名与用户看到的数字对不上，而对不上的那一方
/// 是用户天天看的那一方。
///
/// 改成二进制之后两者一致：`1 TiB / 2^30 = 1024 G`，客户端显示 1 T。
/// 代价是「TB」这个词在本系统里从此指 TiB——写在这里、写在 README 的 D23、
/// 也写在 schema 的注释里，三处一致。
///
/// `(group_name, display_name, monthly_bytes, sort_order)`
pub const SEED_QUOTA_GROUPS: &[(&str, &str, u64, i64)] = &[
    ("normal", "普通用户", 1 << 40, 0),
    ("advanced", "Advanced", 2 << 40, 1),
    ("manage", "Manage", 5 << 40, 2),
    ("admin", "Admin", 10 << 40, 3),
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
