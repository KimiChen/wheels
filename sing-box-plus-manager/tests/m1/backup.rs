//! 一致性备份与恢复演练（README §10 的 R3）。
//!
//! R3 要求「一致性备份覆盖账本、累计、游标与配额请求**并演练恢复**」。
//! 这一组钉住的是**演练的判据本身**：一个检不出坏账的核对器，
//! 比没有核对器更糟——它会让人以为演练过了。
//!
//! 所以每一条正向断言都配一条反向的：把账改坏，核对器必须红。

use proxy_manager::ledger::settle::{cycle_key_for, FirstSnapshot, CYCLE_RULE_VERSION};
use proxy_manager::store::verify::verify;
use proxy_manager::store::Store;
use sqlx::Row;

use super::ledger_harness::{Ledger, Snapshot};

/// 跑一轮真实结算，再给账本行安一个主人并写上周期总账。
///
/// 手写周期总账不是偷懒：结算侧只有在身份被认领之后才写它，
/// 而这一组要测的是**核对器**，不是认领流程。用真实结算产生的
/// `accounting_at` 去算周期键，才不会把核对器和一个手捏的时间戳一起测过去。
async fn seeded() -> (Ledger, i64) {
    let ledger = Ledger::new().await;
    ledger.approve(FirstSnapshot::Baseline).await;
    ledger.ingest(&Snapshot::new()).await;
    ledger
        .ingest(
            &Snapshot::new()
                .sequence(2)
                .counter("u_example_01", "tcp_uplink_bytes", 1_000_000)
                .counter("u_example_01", "tcp_downlink_bytes", 2_000_000),
        )
        .await;

    let mut txn = ledger.store.begin_immediate().await.unwrap();
    let user_id: i64 = sqlx::query(
        "INSERT INTO users(login_name, display_name, role, quota_group, status, created_at, updated_at) \
         VALUES ('alice', 'alice', 'user', 'normal', 'active', ?, ?) RETURNING user_id",
    )
    .bind("2026-09-18T00:00:00Z")
    .bind("2026-09-18T00:00:00Z")
    .fetch_one(txn.conn())
    .await
    .unwrap()
    .get(0);
    sqlx::query("UPDATE usage_ledger SET user_id = ?")
        .bind(user_id)
        .execute(txn.conn())
        .await
        .unwrap();
    txn.commit().await.unwrap();

    // 按账本行真实的 accounting_at 归组，算出周期总账应有的值。
    let rows = sqlx::query(
        "SELECT accounting_at, tcp_uplink_bytes, tcp_downlink_bytes, udp_uplink_bytes, \
                udp_downlink_bytes FROM usage_ledger",
    )
    .fetch_all(ledger.store.readers())
    .await
    .unwrap();
    assert!(!rows.is_empty(), "夹具没产生账本行，后面的断言会全是空转");

    let mut total: u128 = 0;
    let mut cycle = String::new();
    for row in &rows {
        for index in 1..5 {
            total +=
                row.get::<String, _>(index).trim_start_matches('0').parse::<u128>().unwrap_or(0);
        }
        let at = proxy_manager::ledger::bucket::parse_rfc3339(&row.get::<String, _>(0)).unwrap();
        cycle = cycle_key_for(at, CYCLE_RULE_VERSION).unwrap();
    }

    let mut txn = ledger.store.begin_immediate().await.unwrap();
    sqlx::query(
        "INSERT INTO usage_cycle_totals(user_id, cycle_key, rule_version, total_bytes, updated_at) \
         VALUES (?, ?, ?, ?, ?)",
    )
    .bind(user_id)
    .bind(&cycle)
    .bind(CYCLE_RULE_VERSION)
    .bind(format!("{total:0>39}"))
    .bind("2026-09-18T00:00:00Z")
    .execute(txn.conn())
    .await
    .unwrap();
    txn.commit().await.unwrap();

    (ledger, user_id)
}

/// 正向：一份自洽的库要能通过，**而且核对过的组数不为零**。
///
/// 第二句才是关键。一个什么都没核对的核对器同样会报「对得上」——
/// 那正是这套东西最容易退化成的样子。
#[tokio::test]
async fn 自洽的库能通过核对() {
    let (ledger, _) = seeded().await;
    let report = verify(&ledger.store).await.unwrap();

    assert_eq!(report.integrity, "ok");
    assert_eq!(report.foreign_key_violations, 0);
    assert!(report.missing_tables.is_empty(), "缺表：{:?}", report.missing_tables);
    assert!(report.unexpected_tables.is_empty(), "多表：{:?}", report.unexpected_tables);
    assert!(report.mismatches.is_empty(), "不该有对不上的：{:?}", report.mismatches);
    assert!(report.cycles_checked > 0, "一组周期总账都没核对，这条断言是空转的");
    assert!(report.lifetimes_checked > 0, "一个永久累计都没核对，这条断言是空转的");
    assert!(report.ok());
}

/// 反向一：周期总账被改过。
#[tokio::test]
async fn 周期总账对不上会被抓出来() {
    let (ledger, _) = seeded().await;
    let mut txn = ledger.store.begin_immediate().await.unwrap();
    sqlx::query("UPDATE usage_cycle_totals SET total_bytes = ?")
        .bind(format!("{:0>39}", 1_u128))
        .execute(txn.conn())
        .await
        .unwrap();
    txn.commit().await.unwrap();

    let report = verify(&ledger.store).await.unwrap();
    assert!(!report.ok());
    assert!(
        report.mismatches.iter().any(|m| m.what == "周期总账" && m.recorded == 1),
        "实际：{:?}",
        report.mismatches
    );
}

/// 反向二：**总账整行不见了**。
///
/// 这比「数字对不上」更隐蔽：一次只看总账表的核对会说「零处不一致」，
/// 因为它根本没去看账本里还躺着谁的字节。
#[tokio::test]
async fn 总账缺行也会被抓出来() {
    let (ledger, _) = seeded().await;
    let mut txn = ledger.store.begin_immediate().await.unwrap();
    sqlx::query("DELETE FROM usage_cycle_totals").execute(txn.conn()).await.unwrap();
    txn.commit().await.unwrap();

    let report = verify(&ledger.store).await.unwrap();
    assert!(!report.ok());
    assert!(
        report.mismatches.iter().any(|m| m.what == "周期总账缺行" && m.recomputed > 0),
        "实际：{:?}",
        report.mismatches
    );
}

/// 反向三：永久累计被改过。它是**不可从别处重建**的状态，改坏了必须看得见。
#[tokio::test]
async fn 永久累计对不上会被抓出来() {
    let (ledger, _) = seeded().await;
    let mut txn = ledger.store.begin_immediate().await.unwrap();
    sqlx::query("UPDATE usage_lifetime_totals SET tcp_uplink_bytes = ?")
        .bind(format!("{:0>39}", 999_999_999_u128))
        .execute(txn.conn())
        .await
        .unwrap();
    txn.commit().await.unwrap();

    let report = verify(&ledger.store).await.unwrap();
    assert!(!report.ok());
    assert!(
        report.mismatches.iter().any(|m| m.what == "永久累计"),
        "实际：{:?}",
        report.mismatches
    );
}

/// **备份 → 恢复 → 重算**：一次完整的演练。
///
/// 用 `VACUUM INTO` 而不是 `cp`：库是 WAL 模式，`cp` 拿到的是撕裂的快照。
/// 判据不是「文件能打开」——那句话对一个被截断到一半的文件也成立——
/// 而是恢复出来的那份**自己能把总账重算对**。
#[tokio::test]
async fn 备份出来的副本自己能重算对() {
    let (ledger, _) = seeded().await;
    let dir = tempfile::tempdir().unwrap();
    let out = dir.path().join("backup.db");

    sqlx::query("VACUUM INTO ?")
        .bind(out.to_string_lossy().as_ref())
        .execute(ledger.store.readers())
        .await
        .unwrap();

    let restored = Store::open_readonly(&out).await.unwrap();
    let report = verify(&restored).await.unwrap();
    assert!(report.ok(), "恢复出来的副本不自洽：{:?}", report.mismatches);
    assert!(report.cycles_checked > 0 && report.lifetimes_checked > 0);

    // 逐表对一遍行数：备份不是「差不多」。
    let live = verify(&ledger.store).await.unwrap();
    assert_eq!(report.counts, live.counts, "备份与源库的行数不一致");
    restored.close().await;
}

/// 只读打开**不会改动被检查的文件**。
///
/// 这一条看起来像洁癖，其实是演练的前提：如果核对本身会把备份从 delete 模式
/// 切成 WAL，那么「核对过的那份」就不再是「你存下来的那份」，
/// 而差异发生在文件头——恰好是最难察觉的地方。
#[tokio::test]
async fn 只读核对不改动备份文件() {
    let (ledger, _) = seeded().await;
    let dir = tempfile::tempdir().unwrap();
    let out = dir.path().join("backup.db");
    sqlx::query("VACUUM INTO ?")
        .bind(out.to_string_lossy().as_ref())
        .execute(ledger.store.readers())
        .await
        .unwrap();

    let before = std::fs::read(&out).unwrap();
    let restored = Store::open_readonly(&out).await.unwrap();
    verify(&restored).await.unwrap();
    restored.close().await;
    let after = std::fs::read(&out).unwrap();

    assert_eq!(before.len(), after.len(), "文件大小变了");
    assert!(before == after, "只读核对改动了备份文件的内容");
    assert!(!out.with_extension("db-wal").exists(), "核对留下了 -wal");
}

/// 路径打错要**报错**，而不是凭空造一个空库然后报告「表都不在」。
///
/// 那个结论看起来像「备份坏了」，其实是路径写错了——
/// 而这两件事的下一步完全不同。
#[tokio::test]
async fn 打不开的路径直接报错() {
    let dir = tempfile::tempdir().unwrap();
    let missing = dir.path().join("nope.db");
    assert!(Store::open_readonly(&missing).await.is_err());
    assert!(!missing.exists(), "不该顺手造出一个空库");
}

/// **月界那 8 小时的归属按 UTC+8 算，不是按 `accounting_at` 的前 7 个字符。**
///
/// 账本的时刻是 UTC，而周期按 UTC+8 切月：`2026-08-31T16:00:00Z` 在 UTC 上还是 8 月，
/// 在 UTC+8 上已经是 9 月 1 日零点，所以它属于 `2026-09`。
///
/// 这条是补上的。核对器里我写了一句注释说「在 SQL 里用 substr 拼一个出来
/// 等于写第二份实现」，却没有一条用例能证明它——把 `cycle_key_for` 换成
/// `substr(accounting_at, 1, 7)` 之后，上面那七条**全绿**。
/// 声称一个危险存在，和有一条会因为它变红的用例，是两件事。
#[tokio::test]
async fn 月界那八小时按东八区归属() {
    let (ledger, _) = seeded().await;

    // 把账本挪进那 8 小时的窗口里。
    let mut txn = ledger.store.begin_immediate().await.unwrap();
    sqlx::query("UPDATE usage_ledger SET accounting_at = '2026-08-31T16:00:00Z'")
        .execute(txn.conn())
        .await
        .unwrap();
    sqlx::query("UPDATE usage_cycle_totals SET cycle_key = '2026-09'")
        .execute(txn.conn())
        .await
        .unwrap();
    txn.commit().await.unwrap();

    let report = verify(&ledger.store).await.unwrap();
    assert!(
        report.ok(),
        "16:00Z 属于下一个自然月（UTC+8），记在 2026-09 名下应当是对的：{:?}",
        report.mismatches
    );

    // 反过来：按 UTC 的月份记成 2026-08，必须被抓出来。
    // `substr(accounting_at, 1, 7)` 那种实现在这里会说「对得上」。
    let mut txn = ledger.store.begin_immediate().await.unwrap();
    sqlx::query("UPDATE usage_cycle_totals SET cycle_key = '2026-08'")
        .execute(txn.conn())
        .await
        .unwrap();
    txn.commit().await.unwrap();

    let report = verify(&ledger.store).await.unwrap();
    assert!(!report.ok(), "按 UTC 月份归属应当对不上");
    assert!(
        report.mismatches.iter().any(|m| m.what == "周期总账缺行" && m.key.contains("2026-09")),
        "应当报 2026-09 这一组在总账里缺行：{:?}",
        report.mismatches
    );
}

/// 15:59:59Z 还在上一个月——与上一条是同一个月界的两侧。
#[tokio::test]
async fn 月界前一秒还属于上一个月() {
    let (ledger, _) = seeded().await;
    let mut txn = ledger.store.begin_immediate().await.unwrap();
    sqlx::query("UPDATE usage_ledger SET accounting_at = '2026-08-31T15:59:59Z'")
        .execute(txn.conn())
        .await
        .unwrap();
    sqlx::query("UPDATE usage_cycle_totals SET cycle_key = '2026-08'")
        .execute(txn.conn())
        .await
        .unwrap();
    txn.commit().await.unwrap();

    let report = verify(&ledger.store).await.unwrap();
    assert!(report.ok(), "15:59:59Z 仍属于 2026-08：{:?}", report.mismatches);
}

/// **epoch 水位要报得出来，而且是按 `(node_id, runtime_id)` 报的。**
///
/// 部署记录里有一段「重建库留下的一个真实缺口」：重建把 `quota_requests` 清空，
/// 主控于是从 epoch 1 重来，而节点还记着重建前的水位，四台全部 409 `stale_epoch`，
/// `resolve_conflict` 每轮只能加一。数字小的时候能自愈，**大的时候永远追不上**。
/// 恢复演练必须把这个数报出来，否则「恢复成功」之后第一件事就是全线限速失效。
///
/// 这条是补上的：第一版把 epoch 当 `i64` 读，而它是**定宽零填充的十进制文本**（C3）。
/// 夹具里从来没有 `quota_requests` 行，所以一路绿到线上才炸——
/// 而且炸的方式是 panic，恰好发生在那个「必须能在坏库上跑完」的工具里。
#[tokio::test]
async fn epoch水位按节点报得出来() {
    let (ledger, _) = seeded().await;

    let mut txn = ledger.store.begin_immediate().await.unwrap();
    for (epoch, runtime) in
        [(7_u64, "0123456789abcdef0123456789abcdef"), (3, "ffffffffffffffffffffffffffffffff")]
    {
        sqlx::query(
            "INSERT INTO quota_requests(node_id, runtime_id, epoch, table_kind, request_body, \
             body_sha256, budget_version, setting_revision, prepared_at, result) \
             VALUES (?, ?, ?, 'zero', x'00', ?, 0, 0, ?, 'applied')",
        )
        .bind(super::ledger_harness::NODE)
        .bind(runtime)
        .bind(format!("{epoch:0>20}"))
        .bind("a".repeat(64))
        .bind("2026-09-18T00:00:00Z")
        .execute(txn.conn())
        .await
        .unwrap();
    }
    txn.commit().await.unwrap();

    let report = verify(&ledger.store).await.unwrap();
    assert!(report.shape_errors.is_empty(), "列形状报错：{:?}", report.shape_errors);
    assert_eq!(
        report.epoch_high_water.len(),
        2,
        "两个 runtime 各要一行：{:?}",
        report.epoch_high_water
    );
    let epochs: Vec<u64> = report.epoch_high_water.iter().map(|(_, _, e)| *e).collect();
    assert!(epochs.contains(&7) && epochs.contains(&3), "实际：{epochs:?}");
    assert!(report.ok(), "{:?}", report.mismatches);
}

/// 一张空的 `quota_requests` 报的是「**没有水位**」，不是「水位是 0」。
///
/// 这两句话的下一步完全不同：前者要人工补水位，后者会让人以为一切正常。
#[tokio::test]
async fn 没有配额请求时说没有水位而不是零() {
    let (ledger, _) = seeded().await;
    let report = verify(&ledger.store).await.unwrap();
    assert!(report.epoch_high_water.is_empty());
}

/// 一份**坏掉的**库要报「形状不对」，而不是报「没有水位」。
///
/// 这两句话的下一步完全不同：前者说这份库不能用，后者会让人去补水位——
/// 在一个错误的诊断上继续施工。第一版把这个错误 `.ok()` 吞掉了，
/// 于是一份坏库会被报成一份「干净但需要补水位」的库。
///
/// 用 `PRAGMA ignore_check_constraints` 造出这一行。这不是钻空子：
/// **CHECK 只在写入时生效，读的时候不会重新校验**，
/// 而一个核对器存在的理由正是「这份文件可能已经不满足它自己的约束了」。
#[tokio::test]
async fn 坏掉的epoch报形状不对而不是报没有() {
    let (ledger, _) = seeded().await;

    let mut txn = ledger.store.begin_immediate().await.unwrap();
    sqlx::query("PRAGMA ignore_check_constraints = ON").execute(txn.conn()).await.unwrap();
    sqlx::query(
        "INSERT INTO quota_requests(node_id, runtime_id, epoch, table_kind, request_body, \
         body_sha256, budget_version, setting_revision, prepared_at, result) \
         VALUES (?, ?, 'not-a-number', 'zero', x'00', ?, 0, 0, ?, 'applied')",
    )
    .bind(super::ledger_harness::NODE)
    .bind("0123456789abcdef0123456789abcdef")
    .bind("a".repeat(64))
    .bind("2026-09-18T00:00:00Z")
    .execute(txn.conn())
    .await
    .unwrap();
    txn.commit().await.unwrap();

    let report = verify(&ledger.store).await.unwrap();
    assert!(
        report.shape_errors.iter().any(|e| e.contains("epoch")),
        "应当报 epoch 的形状不对：{:?} / 水位 {:?}",
        report.shape_errors,
        report.epoch_high_water
    );
    // **而且这份库要被判为不自洽。** 只把错误打出来却仍然报 ok，
    // 等于让一次演练在「有报错但通过了」的状态下收场。
    // **水位那一栏必须是空的**——这才是这条用例的重点：
    // 一个把错误吞掉的实现会在这里给出「没有水位」，
    // 而那句话的下一步是「人工补水位」，即在一个错误的诊断上继续施工。
    assert!(report.epoch_high_water.is_empty(), "坏行不该被算成一个水位");
    assert!(!report.ok(), "这份库不该判为自洽");

    // 顺带记一个在写这条用例时才量出来的事实：**`PRAGMA integrity_check`
    // 自己也会重新校验 CHECK**，所以这一行同时被两个独立的检测器抓住。
    // 也就是说 `ok()` 里那条 `shape_errors.is_empty()` 在这个场景下是冗余的——
    // 它的价值在没有 CHECK 兜底的列上。这里如实钉住实际发生的事，
    // 而不是假装那条冗余的判据是被这条用例证明的。
    assert!(report.integrity.contains("CHECK"), "integrity_check 也该抓到它：{}", report.integrity);
}

/// **翻页那段代码要真的翻过页。**
///
/// 默认翻页是 5000 行，而用例里账本只有个位数行——于是那个 `loop` 永远只转一圈，
/// 翻页的边界（`after` 推进、最后一页判空）在测试里从来不执行。
/// 而它恰好是只在大库上才走到的那一段：账本是只追加的，
/// 180-400 天之后它是这个库里行数最多的表。
#[tokio::test]
async fn 翻页边界真的被走到() {
    let (ledger, _) = seeded().await;
    // 再喂两轮，让账本至少有三行——一页一行才真的翻得动。
    // **四个计数器都得往上走**：游标是绝对值，任何一路回退都是 C 约束里的负增量，
    // 那一批会被拒绝，账本一行都不会多。
    for step in [2_u64, 3] {
        ledger
            .ingest(
                &Snapshot::new()
                    .sequence(step + 1)
                    .counter("u_example_01", "tcp_uplink_bytes", 1_000_000 * step)
                    .counter("u_example_01", "tcp_downlink_bytes", 2_000_000 * step)
                    .counter("u_example_01", "udp_uplink_bytes", 30_000 * step)
                    .counter("u_example_01", "udp_downlink_bytes", 40_000 * step),
            )
            .await;
    }
    let mut txn = ledger.store.begin_immediate().await.unwrap();
    sqlx::query("UPDATE usage_ledger SET user_id = (SELECT MIN(user_id) FROM users)")
        .execute(txn.conn())
        .await
        .unwrap();
    // 账本变了，周期总账跟着重算一遍，否则下面断言的是「对不上」而不是翻页。
    let total: u128 = {
        let rows = sqlx::query(
            "SELECT tcp_uplink_bytes, tcp_downlink_bytes, udp_uplink_bytes, udp_downlink_bytes \
               FROM usage_ledger",
        )
        .fetch_all(txn.conn())
        .await
        .unwrap();
        rows.iter()
            .flat_map(|row| (0..4).map(move |i| row.get::<String, _>(i)))
            .map(|text| text.trim_start_matches('0').parse::<u128>().unwrap_or(0))
            .sum()
    };
    sqlx::query("UPDATE usage_cycle_totals SET total_bytes = ?")
        .bind(format!("{total:0>39}"))
        .execute(txn.conn())
        .await
        .unwrap();
    txn.commit().await.unwrap();

    let whole = verify(&ledger.store).await.unwrap();
    assert!(whole.ok(), "夹具本身要先是自洽的：{:?}", whole.mismatches);

    // 一页一行：强制把每一次边界都走一遍。
    let paged = proxy_manager::store::verify::verify_with_page(&ledger.store, 1).await.unwrap();
    // 翻页翻的是**账本行**，不是身份数——夹具里只有一个身份有流量。
    let ledger_rows = ledger.ledger_rows().await;
    assert!(ledger_rows > 1, "夹具只有 {ledger_rows} 行账本，一页一行也翻不动");
    assert_eq!(paged.mismatches.len(), whole.mismatches.len(), "翻页改变了结论");
    assert!(paged.ok(), "翻页之后对不上了：{:?}", paged.mismatches);
}

/// **备份不会在源库上写一个字节，也不会凭空造库。**
///
/// 第一版的 `ledger backup` 走 `open_store()`，那条路径依次做
/// `create_if_missing(true)` → `init_schema()` → `sync_nodes()`——后两件都是写。
/// 一个备份工具在动手之前先改一遍被备份的对象，是把「备份」和「一次可能失败的写入」
/// 绑在了一起；而 `create_if_missing` 让一个打错的路径变成一个崭新的空库，
/// 然后它会被认认真真地备份、核对、并报告「一切正常」。
#[tokio::test]
async fn 备份不写源库也不凭空造库() {
    let dir = tempfile::tempdir().unwrap();

    // 路径不存在时**报错**，而且不留下任何文件。
    let missing = dir.path().join("nope.db");
    assert!(Store::open_for_backup(&missing).await.is_err());
    assert!(!missing.exists(), "不该造出一个空库");

    // 一个**缺表**的库：打开它不该把表建出来。
    let empty = dir.path().join("empty.db");
    std::fs::write(&empty, b"").unwrap();
    let store = Store::open_for_backup(&empty).await.unwrap();
    let report = verify(&store).await.unwrap();
    store.close().await;
    assert!(!report.missing_tables.is_empty(), "空库应当报缺表，而不是被悄悄建好");
    assert!(!report.ok());

    // 再打开一次，确认上一次没有顺手建表——**缺表必须还是缺表**。
    let store = Store::open_for_backup(&empty).await.unwrap();
    let again = verify(&store).await.unwrap();
    store.close().await;
    assert_eq!(again.missing_tables.len(), report.missing_tables.len(), "上一次打开建了表");
}

/// 备份前要先确认**源库自报的是 WAL**。
///
/// 这是抓 `immutable=1` 的机械 tell：加上那个参数之后 SQLite 会完全跳过 WAL，
/// 打开成功、`integrity_check` 报 ok、读到的却是 checkpoint 之前的旧数据，
/// 而它会把一个 WAL 库自报成 `delete`。直接 grep 参数名抓不住等价写法，自报模式能。
#[tokio::test]
async fn 源库必须自报wal() {
    let (ledger, _) = seeded().await;
    ledger.store.assert_backup_preconditions().await.expect("活库是 WAL，应当通过");

    // 备份产物是 delete 模式——**拿它当源去再备一次，必须被挡住**。
    let dir = tempfile::tempdir().unwrap();
    let out = dir.path().join("backup.db");
    sqlx::query("VACUUM INTO ?")
        .bind(out.to_string_lossy().as_ref())
        .execute(ledger.store.readers())
        .await
        .unwrap();
    let copy = Store::open_readonly(&out).await.unwrap();
    let error = copy.assert_backup_preconditions().await.unwrap_err().to_string();
    copy.close().await;
    assert!(error.contains("journal_mode"), "{error}");
    assert!(error.contains("immutable"), "要点名最常见的成因：{error}");
}

/// **表名对得上，不等于结构对得上。**
///
/// 原来那道一致性门禁只比表名：加一列、减一列，两个方向都静默放行。
/// 也就是说它挡得住「少一张表」，挡不住「少一列」——而后者更常见、也更难发现：
/// 库照常打开，查询照常跑，直到某个 INSERT 撞上一个不存在的列。
#[tokio::test]
async fn 多出一列会被抓出来() {
    let (ledger, _) = seeded().await;
    let clean = verify(&ledger.store).await.unwrap();
    assert!(clean.schema_drift.is_empty(), "干净的库不该有列漂移：{:?}", clean.schema_drift);
    assert!(clean.ok());

    let mut txn = ledger.store.begin_immediate().await.unwrap();
    sqlx::query("ALTER TABLE users ADD COLUMN 顺手加的 TEXT").execute(txn.conn()).await.unwrap();
    txn.commit().await.unwrap();

    let drifted = verify(&ledger.store).await.unwrap();
    // 表集合仍然完全一致——**旧门禁到这里就放行了**。
    assert!(drifted.missing_tables.is_empty() && drifted.unexpected_tables.is_empty());
    assert!(
        drifted.schema_drift.iter().any(|d| d.contains("users")),
        "应当报 users 的列不一致：{:?}",
        drifted.schema_drift
    );
    assert!(!drifted.ok(), "列漂移的库不该判为自洽");
}

/// 对照用的那份指纹是**按 DDL 现建一份空库算出来的**，不是写死的常量。
///
/// 写常量要靠人记得同步，而那正是这道门禁本身要防的那类漂移。
#[tokio::test]
async fn 对照指纹来自ddl而不是常量() {
    let expected = proxy_manager::store::schema::expected_fingerprints().await.unwrap();
    assert_eq!(
        expected.len(),
        proxy_manager::store::schema::EXPECTED_TABLES.len(),
        "现建出来的表数应当与 EXPECTED_TABLES 一致"
    );
    for table in proxy_manager::store::schema::EXPECTED_TABLES {
        let text = expected.get(*table).unwrap_or_else(|| panic!("{table} 没建出来"));
        assert!(!text.is_empty(), "{table} 的指纹是空的——PRAGMA table_info 没返回列");
    }
}

/// **启动门禁也要认列，不只是认表名。**
///
/// 上面那条核的是 `verify` 报不报；这条核的是**服务起不起得来**。
/// 两者要一致：一个「核对说不行、但照样启动」的组合，
/// 等于把一道失败关闭的门禁降级成一条日志。
#[tokio::test]
async fn 列漂移会挡住启动() {
    let (ledger, _) = seeded().await;
    // 没漂移时重复 init 是幂等的。
    assert!(ledger.store.init_schema().await.is_ok());

    let mut txn = ledger.store.begin_immediate().await.unwrap();
    sqlx::query("ALTER TABLE nodes ADD COLUMN 顺手加的 TEXT").execute(txn.conn()).await.unwrap();
    txn.commit().await.unwrap();

    let error = ledger.store.init_schema().await.unwrap_err().to_string();
    assert!(error.contains("结构与 DDL 不一致"), "{error}");
    assert!(error.contains("nodes"), "要点名是哪张表：{error}");
    // 报错里要给出下一步，而不是只说「不行」。
    assert!(error.contains("备份"), "要说清接下来该怎么做：{error}");
}

/// 拿一段 DDL 现建一份内存库，取那张表的指纹。
async fn fingerprint_of(ddl: &str) -> String {
    use sqlx::Connection;
    let mut conn = sqlx::SqliteConnection::connect("sqlite::memory:").await.unwrap();
    sqlx::raw_sql(ddl).execute(&mut conn).await.unwrap();
    let prints = proxy_manager::store::schema::table_fingerprints(&mut conn).await.unwrap();
    let _ = conn.close().await;
    prints.get("t").expect("表 t 没建出来").clone()
}

/// **换唯一键会被抓住。**
///
/// 这是 2026-09-20 那次改动（槽位键从三列改成两列）暴露出来的洞：
/// `PRAGMA table_info` 只给列名 / 类型 / 非空 / 默认值 / 主键序，
/// **看不见 UNIQUE**。三列变两列时列清单一个字都没变，旧库会顺利通过启动门禁，
/// 然后 `ON CONFLICT` 报 `no unique index matching`——更坏的一支连报都不报，
/// 只是让 `count(*)` 悄悄翻倍、池子看起来永远是空的。
///
/// D32 自己把这一条记成了「门禁不认的东西」。这条用例是把它划掉。
#[tokio::test]
async fn 换唯一键会被指纹抓住() {
    let three =
        fingerprint_of("CREATE TABLE t(a TEXT NOT NULL, b TEXT NOT NULL, c TEXT NOT NULL, UNIQUE(a, b, c)) STRICT;")
            .await;
    let two = fingerprint_of(
        "CREATE TABLE t(a TEXT NOT NULL, b TEXT NOT NULL, c TEXT NOT NULL, UNIQUE(a, b)) STRICT;",
    )
    .await;
    assert_ne!(three, two, "三列唯一键与两列唯一键的指纹必须不同");

    // 反向控制：列段完全一样——这正是旧门禁放行的原因。
    let columns = |text: &str| text.split('\u{1d}').next().unwrap().to_string();
    assert_eq!(columns(&three), columns(&two), "列清单一个字都没变，所以只比列的门禁看不见它");
}

/// **CHECK 与索引同样要被认出来。**
///
/// 索引这一条是 D32 点名过的：「索引少了一个仍然静默」。
#[tokio::test]
async fn 改check或少一条索引都会被指纹抓住() {
    let base = "CREATE TABLE t(a TEXT NOT NULL, b TEXT NOT NULL) STRICT;";
    let plain = fingerprint_of(base).await;

    let checked = fingerprint_of(
        "CREATE TABLE t(a TEXT NOT NULL CHECK(length(a) = 4), b TEXT NOT NULL) STRICT;",
    )
    .await;
    assert_ne!(plain, checked, "加一条 CHECK 必须改变指纹");

    let indexed = fingerprint_of(&format!("{base} CREATE INDEX idx_t_b ON t(b);")).await;
    assert_ne!(plain, indexed, "多一条索引必须改变指纹");

    let partial =
        fingerprint_of(&format!("{base} CREATE INDEX idx_t_b ON t(b) WHERE b <> '';")).await;
    assert_ne!(indexed, partial, "部分索引的 WHERE 是它的一部分，改了必须改变指纹");
}

/// **只改注释或排版不许误报。**
///
/// 这条是承重的反向控制：指纹里塞进 DDL 原文之后，如果规范化不剥注释、不折空白，
/// 那么**每一次给 schema 加一句注释都会变成一次「请重建数据库」**。
/// 而这个仓库的 DDL 里注释比语句多——那样的门禁会在一周内被绕过。
#[tokio::test]
async fn 改注释与排版不算漂移() {
    let tight = fingerprint_of("CREATE TABLE t(a TEXT NOT NULL, b TEXT NOT NULL) STRICT;").await;
    let loose = fingerprint_of(
        "CREATE TABLE t(\n\
         -- 一句新写的注释\n\
         a TEXT NOT NULL,\n\
         /* 换一种注释写法 */\n\
             b   TEXT   NOT NULL\n\
         ) STRICT;",
    )
    .await;
    assert_eq!(tight, loose, "剥注释 + 折空白之后两者必须同指纹");

    // 但字符串字面量里的 `--` 不是注释，剥错了会把两份不同的 DDL 抹成一个指纹。
    let dashes =
        fingerprint_of("CREATE TABLE t(a TEXT NOT NULL DEFAULT '--x', b TEXT) STRICT;").await;
    let empty = fingerprint_of("CREATE TABLE t(a TEXT NOT NULL DEFAULT '', b TEXT) STRICT;").await;
    assert_ne!(dashes, empty, "默认值里的两横不是注释");
}
