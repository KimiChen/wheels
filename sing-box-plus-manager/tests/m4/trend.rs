//! 趋势查询的验收用例（README §6、§4.11）。
//!
//! 账本行由**真实结算**产生，不在用例里直接 INSERT——
//! 否则用例和被测代码会共用同一个键格式化函数，
//! 「结算写的键」与「查询找的键」不一致这件事就永远测不出来。

// `seed_ledger` 住在这里而不是 harness 里：它依赖 `crate::ledger_harness` 与
// `crate::fixtures`，而那两个模块只在 M4 的入口里声明。留在 harness 里，
// M5 就复用不了同一个 harness——共用的东西不该带着只有一个用户的依赖。

use axum::http::StatusCode;
use proxy_manager::api::session::Role;

use crate::harness::Api;

/// 桶键必须与结算侧逐字一致，而且**真的能匹配上**。
///
/// 写第一版时我把 join 写成了不存在的表，又把桶键另写了一份格式化。
/// 两处都编译通过——sqlx 不做编译期 SQL 检查，键的格式更没人管。
/// 表现是图上一条全零的线，看起来像「这段时间没有流量」。
#[tokio::test]
async fn buckets_line_up_with_what_settlement_wrote() {
    let api = Api::new().await;
    let admin = api.user("boss", Role::Admin, "local").await;
    // 1 小时前 4096 字节，3 小时前 1024 字节。
    api.seed_ledger("node-a", &[(3, 1024), (1, 4096)]).await;

    let (status, body, _) = api.get("/api/v1/usage/trend?range=24h", Some(&admin)).await;
    assert_eq!(status, StatusCode::OK, "{body}");
    let points = body["points"].as_array().unwrap();
    assert_eq!(points.len(), 24, "24h 应当有 24 个桶");

    let total: u128 =
        points.iter().map(|p| p["total_bytes"].as_str().unwrap().parse::<u128>().unwrap()).sum();
    assert_eq!(total, 5120, "两笔增量都要落进窗口内的桶里，一个都不能丢");

    let non_empty: Vec<&str> = points
        .iter()
        .filter(|p| p["total_bytes"].as_str().unwrap() != "0")
        .map(|p| p["bucket"].as_str().unwrap())
        .collect();
    assert_eq!(non_empty.len(), 2, "两笔增量应当落在两个不同的小时桶：{non_empty:?}");
}

/// 空桶要**显式出现**在结果里。
///
/// 只回传有数据的桶，图表会把 12 点和 15 点画成相邻两根，
/// 中间那段「没有流量」被压没了——而那正是最该看见的东西。
#[tokio::test]
async fn empty_buckets_are_present_not_skipped() {
    let api = Api::new().await;
    let admin = api.user("boss", Role::Admin, "local").await;
    api.seed_ledger("node-a", &[(5, 2048)]).await;

    let (_, body, _) = api.get("/api/v1/usage/trend?range=24h", Some(&admin)).await;
    let points = body["points"].as_array().unwrap();
    assert_eq!(points.len(), 24);
    let zeros = points.iter().filter(|p| p["total_bytes"].as_str().unwrap() == "0").count();
    assert_eq!(zeros, 23, "除了有数据的那个桶，其余 23 个都要以 0 出现");

    // 桶必须按时间**严格递增**且无重复：图表按下标取 x 轴，顺序错了就是画错。
    let keys: Vec<&str> = points.iter().map(|p| p["bucket"].as_str().unwrap()).collect();
    let mut sorted = keys.clone();
    sorted.sort_unstable();
    sorted.dedup();
    assert_eq!(keys, sorted, "桶必须严格递增且不重复");
}

/// 字节字段一律十进制字符串（C3），趋势端点也不例外。
#[tokio::test]
async fn trend_bytes_are_decimal_strings() {
    let api = Api::new().await;
    let admin = api.user("boss", Role::Admin, "local").await;
    api.seed_ledger("node-a", &[(2, 12345)]).await;

    let (_, body, _) = api.get("/api/v1/usage/trend?range=24h", Some(&admin)).await;
    for point in body["points"].as_array().unwrap() {
        for (key, value) in point.as_object().unwrap() {
            if key.ends_with("_bytes") {
                assert!(value.is_string(), "{key} 回了裸数字：前端拿到就会喂进 Number");
            }
        }
    }
    // 口径三元组随事实一起回传：图上的数字是什么口径，不靠口头解释。
    assert!(body["metric_scope"].is_string());
    assert_eq!(body["traffic_estimated"], false);
    assert_eq!(body["time_bucket_estimated"], true);
}

/// 范围上限**在 API 层强制**。前端的下拉框只有三个选项，但下拉框不是边界。
#[tokio::test]
async fn range_cap_is_enforced_server_side() {
    let api = Api::new().await;
    let admin = api.user("boss", Role::Admin, "local").await;

    let (status, body, _) = api.get("/api/v1/usage/trend?range=9000h", Some(&admin)).await;
    assert_eq!(status, StatusCode::BAD_REQUEST);
    assert_eq!(body["error"]["code"], "invalid_request");
    assert_eq!(body["error"]["detail"]["max_hours"], 744);

    let (status, _, _) = api.get("/api/v1/usage/trend?range=nonsense", Some(&admin)).await;
    assert_eq!(status, StatusCode::BAD_REQUEST, "认不出的 range 要拒，不能悄悄回默认值");
}

/// 节点筛选走的是真的 join。
///
/// 这条用例的价值在于：SQL 里把表名写错编译器完全不管，
/// 只有真跑一次才知道。第一版我写的 `runtime_windows` 根本不存在。
#[tokio::test]
async fn node_filter_actually_joins() {
    let api = Api::new().await;
    let admin = api.user("boss", Role::Admin, "local").await;
    api.seed_ledger("node-a", &[(1, 777)]).await;

    let (status, body, _) =
        api.get("/api/v1/usage/trend?range=24h&node_id=node-a", Some(&admin)).await;
    assert_eq!(status, StatusCode::OK, "{body}");
    let hit: u128 = body["points"]
        .as_array()
        .unwrap()
        .iter()
        .map(|p| p["total_bytes"].as_str().unwrap().parse::<u128>().unwrap())
        .sum();
    assert_eq!(hit, 777);

    let (_, body, _) =
        api.get("/api/v1/usage/trend?range=24h&node_id=node-zzz", Some(&admin)).await;
    let miss: u128 = body["points"]
        .as_array()
        .unwrap()
        .iter()
        .map(|p| p["total_bytes"].as_str().unwrap().parse::<u128>().unwrap())
        .sum();
    assert_eq!(miss, 0, "筛别的节点应当是 0，不是「筛选条件被忽略」");
}

/// 日粒度回结构化的「未启用」，而不是 404，也不是悄悄按小时返回。
#[tokio::test]
async fn day_grain_says_not_enabled_instead_of_pretending() {
    let api = Api::new().await;
    let admin = api.user("boss", Role::Admin, "local").await;
    let (status, body, _) = api.get("/api/v1/usage/trend?grain=day", Some(&admin)).await;
    assert_eq!(status, StatusCode::CONFLICT);
    assert_eq!(body["error"]["code"], "audit_not_enabled");
    assert!(body["error"]["detail"]["reason"].as_str().unwrap().contains("时区"));
}

/// 趋势是管理面数据，普通用户拿不到。
#[tokio::test]
async fn trend_is_admin_only() {
    let api = Api::new().await;
    let user = api.user("member", Role::User, "local").await;
    let (status, _, _) = api.get("/api/v1/usage/trend", Some(&user)).await;
    assert_eq!(status, StatusCode::FORBIDDEN);
}

/// 让账本行由**真实结算**产生。
///
/// 直接 `INSERT INTO usage_ledger` 会让用例用上与被测代码同一个格式化函数，
/// 于是「结算写的键」和「查询找的键」不一致这件事永远测不出来。
/// 这里走 `runtime::approve` → `settle::receive` → `settle::settle_next`，
/// 与生产路径完全一致，只是把 `collected_at` 拨到指定的小时。
impl Api {
    pub async fn seed_ledger(&self, node_id: &str, points: &[(i64, u64)]) {
        use proxy_manager::ledger::runtime;
        use proxy_manager::ledger::settle::{self, FirstSnapshot};

        const RUNTIME: &str = "a1b2c3d4e5f60718293a4b5c6d7e8f90";
        let mut txn = self.store.begin_immediate().await.unwrap();
        sqlx::query(
            "INSERT INTO nodes(node_id, provider, address, first_snapshot, agent_spki_sha256, \
             quota_enabled, status, created_at, updated_at) \
             VALUES (?, 'plus_v3', '203.0.113.9:8443', 'baseline', ?, 1, 'active', ?, ?)",
        )
        .bind(node_id)
        .bind("0".repeat(64))
        .bind("2026-09-18T00:00:00Z")
        .bind("2026-09-18T00:00:00Z")
        .execute(txn.conn())
        .await
        .unwrap();
        txn.commit().await.unwrap();

        runtime::approve(
            &self.store,
            node_id,
            RUNTIME,
            1_700_000_000_000,
            FirstSnapshot::Baseline,
            "用例：已确认是预期的进程窗口",
            "test",
        )
        .await
        .expect("批准 runtime");

        // baseline 策略：首快照只建基线，所以计数器要从 0 起一路累加，
        // 之后每一发的**增量**才是该小时的流量。
        let skew = 5 * 60 * 1000; // 允许的时钟偏差（毫秒）
        let mut cumulative = 0_u64;
        let base = crate::ledger_harness::Snapshot::new().runtime(RUNTIME);
        let schedule = [&[(0_i64, 0_u64)][..], points].concat();
        for (sequence, (hours_ago, delta)) in (1_u64..).zip(schedule) {
            cumulative += delta;
            let at = proxy_manager::ledger::bucket::hour_bucket(time::OffsetDateTime::now_utc())
                - time::Duration::hours(hours_ago)
                + time::Duration::minutes(30);
            let snapshot = base.clone().sequence(sequence).counter(
                "u_example_01",
                "tcp_uplink_bytes",
                cumulative,
            );
            settle::receive(&self.store, node_id, &snapshot.bytes(), at, at, skew)
                .await
                .expect("接收不该失败");
            settle::settle_next(&self.store, node_id, RUNTIME).await.expect("结算不该 Err");
        }
    }
}
