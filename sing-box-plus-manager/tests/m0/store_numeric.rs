//! 存储层：无损数值编码与事务回滚（`docs/data-model.md` §3.1；README §7 M0）。
//!
//! 覆盖 §8 那一行：`2^63−1` / `2^63` / `u64::MAX` 与跨 u64 聚合，
//! 精确解析、20/39 位 SQLite 文本往返和排序正确、checked u128 溢出回滚。
//!
//! 这里刻意**不**只测编解码函数本身：编解码在纯 Rust 里当然自洽。
//! 真正会出错的是它落到 SQLite 之后——CHECK 约束、`COLLATE BINARY` 排序、
//! 以及事务边界。所以每条都经过一张**真实的表**。

use proxy_manager::config::StorageConfig;
use proxy_manager::store::codec::{U128Text, U64Text};
use proxy_manager::store::{schema, InitOutcome, Store};

use sqlx::Row;

struct Fixture {
    store: Store,
    _dir: tempfile::TempDir,
}

async fn fresh() -> Fixture {
    let dir = tempfile::tempdir().expect("临时目录");
    let config = StorageConfig { path: dir.path().join("pm.db"), busy_timeout_ms: 5_000 };
    let store = Store::open(&config).await.expect("打开数据库");
    assert_eq!(store.init_schema().await.expect("建库"), InitOutcome::Created);
    Fixture { store, _dir: dir }
}

/// 建一个节点与一个 runtime，用来承载 20 位定宽文本的往返与排序。
async fn seed_node(store: &Store, node_id: &str) {
    let mut txn = store.begin_immediate().await.unwrap();
    sqlx::query(
        "INSERT INTO nodes(node_id, provider, address, first_snapshot, agent_spki_sha256, \
         quota_enabled, status, created_at, updated_at) \
         VALUES (?, 'plus_v3', 'node.example.com:8443', 'baseline', ?, 1, 'active', ?, ?)",
    )
    .bind(node_id)
    .bind("0".repeat(64))
    .bind("2026-09-18T00:00:00Z")
    .bind("2026-09-18T00:00:00Z")
    .execute(txn.conn())
    .await
    .unwrap();
    txn.commit().await.unwrap();
}

async fn insert_runtime(store: &Store, node_id: &str, runtime_id: &str, started: U64Text) {
    let mut txn = store.begin_immediate().await.unwrap();
    sqlx::query(
        "INSERT INTO node_runtimes(node_id, runtime_id, started_at_unix_ms, first_snapshot, \
         first_seen_at, last_seen_at) VALUES (?, ?, ?, 'baseline', ?, ?)",
    )
    .bind(node_id)
    .bind(runtime_id)
    .bind(started.encode())
    .bind("2026-09-18T00:00:00Z")
    .bind("2026-09-18T00:00:00Z")
    .execute(txn.conn())
    .await
    .unwrap();
    txn.commit().await.unwrap();
}

// ---- 建库 ----

#[tokio::test]
async fn 建库产出期望的表集合且重复调用是幂等的() {
    let fixture = fresh().await;
    let mut conn = fixture.store.readers().acquire().await.unwrap();
    let tables = schema::existing_tables(&mut conn).await.unwrap();
    assert_eq!(
        tables,
        schema::EXPECTED_TABLES.iter().map(|s| s.to_string()).collect::<Vec<_>>(),
        "EXPECTED_TABLES 与 schema/ 下的 DDL 必须一致——一份会漂移的清单比没有清单更糟"
    );
    drop(conn);

    assert_eq!(
        fixture.store.init_schema().await.unwrap(),
        InitOutcome::AlreadyInitialized,
        "已建好的库不该被重复建"
    );
}

/// 不做迁移（D22）的代价要能被观察到：表集合对不上时**失败关闭**，
/// 而不是「补一张表继续跑」。
#[tokio::test]
async fn 表集合不一致时失败关闭() {
    let fixture = fresh().await;
    {
        let mut txn = fixture.store.begin_immediate().await.unwrap();
        sqlx::query("DROP TABLE usage_ledger").execute(txn.conn()).await.unwrap();
        txn.commit().await.unwrap();
    }
    let error = fixture.store.init_schema().await.unwrap_err();
    let message = error.to_string();
    assert!(message.contains("usage_ledger"), "错误要指名缺了哪张表，实际：{message}");
    assert!(message.contains("重建"), "要给出处置方式，实际：{message}");
}

// ---- u64：20 位定宽文本 ----

#[test]
fn u64编码是二十位左补零() {
    assert_eq!(U64Text::new(0).encode(), "0".repeat(20));
    assert_eq!(U64Text::new(1).encode(), format!("{}1", "0".repeat(19)));
    assert_eq!(U64Text::MAX.encode(), "18446744073709551615");
    assert_eq!(U64Text::MAX.encode().len(), 20);
    assert_eq!(U64Text::new(1 << 63).encode(), "09223372036854775808");
}

#[tokio::test]
async fn u64边界值经真实表往返() {
    let fixture = fresh().await;
    seed_node(&fixture.store, "node-example-01").await;

    let boundaries: [u64; 6] = [0, 1, (1 << 63) - 1, 1 << 63, u64::MAX - 1, u64::MAX];
    for (index, value) in boundaries.iter().enumerate() {
        // started_at_unix_ms 有 CHECK 约束，0 也允许（下界由采集层管，不是库层）。
        insert_runtime(
            &fixture.store,
            "node-example-01",
            &format!("{:032x}", index),
            U64Text::new(*value),
        )
        .await;
    }

    let rows = sqlx::query(
        "SELECT runtime_id, started_at_unix_ms FROM node_runtimes ORDER BY started_at_unix_ms",
    )
    .fetch_all(fixture.store.readers())
    .await
    .unwrap();

    // 定宽 + COLLATE BINARY ⇒ 二进制字典序等于整数序。
    let mut expected = boundaries;
    expected.sort_unstable();
    let decoded: Vec<u64> = rows
        .iter()
        .map(|row| U64Text::decode(row.get::<String, _>(1).as_str()).unwrap().get())
        .collect();
    assert_eq!(decoded, expected.to_vec(), "SQL 排序必须与整数序一致");

    // 逐位相等，不是「看起来差不多」。
    for value in boundaries {
        let row = sqlx::query(
            "SELECT started_at_unix_ms FROM node_runtimes WHERE started_at_unix_ms = ?",
        )
        .bind(U64Text::new(value).encode())
        .fetch_one(fixture.store.readers())
        .await
        .unwrap();
        assert_eq!(U64Text::decode(row.get::<String, _>(0).as_str()).unwrap().get(), value);
    }
}

/// 库层是第二道防线：绕过包装类型直接绑一个坏值，CHECK 必须挡住。
#[tokio::test]
async fn 库层check挡住非定宽与超界文本() {
    let fixture = fresh().await;
    seed_node(&fixture.store, "node-example-01").await;

    let bad_values = [
        "1",                      // 宽度不足：字典序会排到所有 20 位值前面
        "0000000000000000000000", // 宽度超出
        "1844674407370955161x",   // 含非数字
        "99999999999999999999",   // 20 位但超过 u64::MAX
    ];
    for (index, bad) in bad_values.iter().enumerate() {
        let mut txn = fixture.store.begin_immediate().await.unwrap();
        let result = sqlx::query(
            "INSERT INTO node_runtimes(node_id, runtime_id, started_at_unix_ms, first_snapshot, \
             first_seen_at, last_seen_at) VALUES (?, ?, ?, 'baseline', ?, ?)",
        )
        .bind("node-example-01")
        .bind(format!("{:032x}", 900 + index))
        .bind(*bad)
        .bind("2026-09-18T00:00:00Z")
        .bind("2026-09-18T00:00:00Z")
        .execute(txn.conn())
        .await;
        assert!(result.is_err(), "CHECK 必须挡住 {bad:?}");
        txn.rollback().await.unwrap();
    }
}

// ---- u128：39 位定宽文本与跨 u64 聚合 ----

#[test]
fn u128编码是三十九位左补零() {
    assert_eq!(U128Text::new(0).encode().len(), 39);
    assert_eq!(U128Text::MAX.encode(), "340282366920938463463374607431768211455");
    assert_eq!(U128Text::new(u64::MAX as u128 + 1).encode().len(), 39);
}

#[tokio::test]
async fn 跨u64聚合在u128里精确保存并排序正确() {
    let fixture = fresh().await;

    // 四个方向各 u64::MAX，合计 4 × (2^64 − 1)：这个值 u64 装不下，u128 可以。
    let one = U64Text::MAX.widen();
    let mut total = U128Text::new(0);
    for _ in 0..4 {
        total = total.checked_add(one).unwrap();
    }
    assert_eq!(total.get(), 4 * (u64::MAX as u128));

    // usage_cycle_totals 用 39 位文本存周期累计。
    let mut txn = fixture.store.begin_immediate().await.unwrap();
    sqlx::query(
        "INSERT INTO users(login_name, display_name, role, status, created_at, updated_at) \
         VALUES ('u1', 'U1', 'user', 'active', ?, ?)",
    )
    .bind("2026-09-18T00:00:00Z")
    .bind("2026-09-18T00:00:00Z")
    .execute(txn.conn())
    .await
    .unwrap();
    for (cycle, value) in [
        ("2026-07", U128Text::new(0)),
        ("2026-08", U128Text::new(u64::MAX as u128)),
        ("2026-09", total),
        ("2026-10", U128Text::MAX),
    ] {
        sqlx::query(
            "INSERT INTO usage_cycle_totals(user_id, cycle_key, rule_version, total_bytes, updated_at) \
             VALUES (1, ?, 1, ?, ?)",
        )
        .bind(cycle)
        .bind(value.encode())
        .bind("2026-09-18T00:00:00Z")
        .execute(txn.conn())
        .await
        .unwrap();
    }
    txn.commit().await.unwrap();

    let rows =
        sqlx::query("SELECT cycle_key, total_bytes FROM usage_cycle_totals ORDER BY total_bytes")
            .fetch_all(fixture.store.readers())
            .await
            .unwrap();
    let order: Vec<String> = rows.iter().map(|r| r.get::<String, _>(0)).collect();
    assert_eq!(order, vec!["2026-07", "2026-08", "2026-09", "2026-10"]);

    let stored = U128Text::decode(rows[2].get::<String, _>(1).as_str()).unwrap();
    assert_eq!(stored, total, "跨 u64 的聚合值必须逐位往返");
}

#[test]
fn checked运算在溢出时报错而不是回绕() {
    assert!(U128Text::MAX.checked_add(U128Text::new(1)).is_err());
    assert!(U64Text::MAX.checked_add(U64Text::new(1)).is_err());
    // 收窄：u64 装不下就报错，不截断。
    assert!(U128Text::new(u64::MAX as u128 + 1).checked_narrow().is_err());
    assert_eq!(U128Text::new(u64::MAX as u128).checked_narrow().unwrap(), U64Text::MAX);
    // 计数器倒退在差分处就失败关闭（C5），不会产生一个巨大的正增量。
    assert!(U64Text::new(5).checked_delta_from(U64Text::new(9)).is_err());
    assert_eq!(U64Text::new(9).checked_delta_from(U64Text::new(5)).unwrap().get(), 4);
}

#[test]
fn 解码拒绝非定宽与非数字() {
    assert!(U64Text::decode("1").is_err());
    assert!(U64Text::decode(&"0".repeat(39)).is_err());
    assert!(U64Text::decode("1844674407370955161x").is_err());
    assert!(U64Text::decode("99999999999999999999").is_err());
    assert!(U128Text::decode(&"0".repeat(20)).is_err());
    assert!(U128Text::decode("999999999999999999999999999999999999999").is_err());
}

/// API 输出去掉数据库补零（README §6、C3）。
#[test]
fn api输出是去补零的十进制字符串() {
    assert_eq!(U64Text::new(0).to_api_string(), "0");
    assert_eq!(U64Text::MAX.to_api_string(), "18446744073709551615");
    assert_eq!(U128Text::MAX.to_api_string(), "340282366920938463463374607431768211455");
    assert_ne!(U64Text::new(1).to_api_string(), U64Text::new(1).encode());
}

// ---- 事务 ----

#[tokio::test]
async fn 回滚后一行都不留() {
    let fixture = fresh().await;
    seed_node(&fixture.store, "node-example-01").await;

    let mut txn = fixture.store.begin_immediate().await.unwrap();
    sqlx::query(
        "INSERT INTO node_runtimes(node_id, runtime_id, started_at_unix_ms, first_snapshot, \
         first_seen_at, last_seen_at) VALUES (?, ?, ?, 'baseline', ?, ?)",
    )
    .bind("node-example-01")
    .bind("a".repeat(32))
    .bind(U64Text::new(1_787_587_200_000).encode())
    .bind("2026-09-18T00:00:00Z")
    .bind("2026-09-18T00:00:00Z")
    .execute(txn.conn())
    .await
    .unwrap();
    txn.rollback().await.unwrap();

    let count: i64 = sqlx::query("SELECT count(*) FROM node_runtimes")
        .fetch_one(fixture.store.readers())
        .await
        .unwrap()
        .get(0);
    assert_eq!(count, 0);
}

/// 多张表要么一起提交要么一起回滚。
///
/// 这正是 D4 的形状：节点侧把「推进基线」与「产出计费记录」的先后定成一次显式取舍，
/// 主控有事务型存储，所以把两步做成一次原子提交。M0 先把这个边界证出来。
#[tokio::test]
async fn 中途失败时多张表一起回滚() {
    let fixture = fresh().await;
    seed_node(&fixture.store, "node-example-01").await;

    let mut txn = fixture.store.begin_immediate().await.unwrap();
    sqlx::query(
        "INSERT INTO node_runtimes(node_id, runtime_id, started_at_unix_ms, first_snapshot, \
         first_seen_at, last_seen_at) VALUES (?, ?, ?, 'baseline', ?, ?)",
    )
    .bind("node-example-01")
    .bind("b".repeat(32))
    .bind(U64Text::new(1).encode())
    .bind("2026-09-18T00:00:00Z")
    .bind("2026-09-18T00:00:00Z")
    .execute(txn.conn())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO runtime_services(runtime_pk, inbound_tag, generation, inbound_type, active, first_seen_at) \
         VALUES (1, 'ss-in', ?, 'shadowsocks', 1, ?)",
    )
    .bind(U64Text::new(1).encode())
    .bind("2026-09-18T00:00:00Z")
    .execute(txn.conn())
    .await
    .unwrap();

    // 第三步违反 CHECK：generation 不是定宽文本。
    let failed = sqlx::query(
        "INSERT INTO runtime_services(runtime_pk, inbound_tag, generation, inbound_type, active, first_seen_at) \
         VALUES (1, 'vless-entry-01', '1', 'vless', 1, ?)",
    )
    .bind("2026-09-18T00:00:00Z")
    .execute(txn.conn())
    .await;
    assert!(failed.is_err());
    txn.rollback().await.unwrap();

    for table in ["node_runtimes", "runtime_services"] {
        let count: i64 = sqlx::query(&format!("SELECT count(*) FROM {table}"))
            .fetch_one(fixture.store.readers())
            .await
            .unwrap()
            .get(0);
        assert_eq!(count, 0, "{table} 也必须回滚");
    }
}

/// 提交之后必须真的落盘——回滚用例的反向证据。
/// 否则一个「永远回滚」的实现能让上面两条全绿。
#[tokio::test]
async fn 提交之后数据可见() {
    let fixture = fresh().await;
    seed_node(&fixture.store, "node-example-01").await;
    insert_runtime(&fixture.store, "node-example-01", &"c".repeat(32), U64Text::new(42)).await;
    let count: i64 = sqlx::query("SELECT count(*) FROM node_runtimes")
        .fetch_one(fixture.store.readers())
        .await
        .unwrap()
        .get(0);
    assert_eq!(count, 1);
}

/// 事务 guard 没有显式结束时（调用点 panic 或提前 return），
/// 下一次取事务必须先清场，而不是让后续写入跑在别人开的事务里。
#[tokio::test]
async fn 未显式结束的事务会在下一次取事务时被回滚() {
    let fixture = fresh().await;
    seed_node(&fixture.store, "node-example-01").await;

    {
        let mut txn = fixture.store.begin_immediate().await.unwrap();
        sqlx::query(
            "INSERT INTO node_runtimes(node_id, runtime_id, started_at_unix_ms, first_snapshot, \
             first_seen_at, last_seen_at) VALUES (?, ?, ?, 'baseline', ?, ?)",
        )
        .bind("node-example-01")
        .bind("d".repeat(32))
        .bind(U64Text::new(7).encode())
        .bind("2026-09-18T00:00:00Z")
        .bind("2026-09-18T00:00:00Z")
        .execute(txn.conn())
        .await
        .unwrap();
        // 既不 commit 也不 rollback，直接丢弃。
    }

    let txn = fixture.store.begin_immediate().await.expect("下一次取事务应当成功");
    txn.rollback().await.unwrap();

    let count: i64 = sqlx::query("SELECT count(*) FROM node_runtimes")
        .fetch_one(fixture.store.readers())
        .await
        .unwrap()
        .get(0);
    assert_eq!(count, 0, "被丢弃的事务必须回滚，不能留下半截数据");
}

/// 外键必须是打开的：STRICT 表 + 外键是 schema 的一部分，不是可选项。
#[tokio::test]
async fn 外键约束生效() {
    let fixture = fresh().await;
    let mut txn = fixture.store.begin_immediate().await.unwrap();
    let result = sqlx::query(
        "INSERT INTO node_runtimes(node_id, runtime_id, started_at_unix_ms, first_snapshot, \
         first_seen_at, last_seen_at) VALUES ('不存在的节点', ?, ?, 'baseline', ?, ?)",
    )
    .bind("e".repeat(32))
    .bind(U64Text::new(1).encode())
    .bind("2026-09-18T00:00:00Z")
    .bind("2026-09-18T00:00:00Z")
    .execute(txn.conn())
    .await;
    assert!(result.is_err(), "外键必须挡住不存在的 node_id");
    txn.rollback().await.unwrap();
}
