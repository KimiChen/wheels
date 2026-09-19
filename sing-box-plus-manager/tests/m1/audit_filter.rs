//! C35 归属裁剪的正反矩阵。
//!
//! 夹具用直接 SQL 铺，不复用别的 harness：这几条用例要证明的东西全在**前置状态**
//! 里（同名跨入口、退役之后、多 generation），而一个把前置状态藏在 helper 里的用例，
//! 读起来像在测别的东西。

use proxy_manager::audit::filter::{self, Attribution, Resolver};
use proxy_manager::audit::record::{AccessRecord, HostSource};
use proxy_manager::config::StorageConfig;
use proxy_manager::store::Store;

const NODE: &str = "node-example-01";
const RUN: &str = "0123456789abcdef0123456789abcdef";
const GEN: &str = "00000000000000000001";
/// 2026-09-19T00:00:00Z
const T0: i64 = 1_789_776_000_000;

struct Fixture {
    store: Store,
    _dir: tempfile::TempDir,
}

async fn fixture() -> Fixture {
    let dir = tempfile::tempdir().unwrap();
    let storage = StorageConfig { path: dir.path().join("pm.db"), busy_timeout_ms: 5_000 };
    let store = Store::open(&storage).await.unwrap();
    store.init_schema().await.unwrap();
    let f = Fixture { store, _dir: dir };
    f.exec(
        "INSERT INTO nodes(node_id, provider, address, first_snapshot, agent_spki_sha256, \
         quota_enabled, status, created_at, updated_at) \
         VALUES (?, 'plus_v3', 'node-01.example.com:8443', 'baseline', ?, 1, 'active', ?, ?)",
        vec![NODE.into(), "0".repeat(64).into(), iso(T0).into(), iso(T0).into()],
    )
    .await;
    f
}

fn iso(ms: i64) -> String {
    let at = time::OffsetDateTime::from_unix_timestamp_nanos(ms as i128 * 1_000_000).unwrap();
    at.format(&time::format_description::well_known::Rfc3339).unwrap()
}

/// 绑定值。`user_id` 是 INTEGER 且可空，而 STRICT 表不会把空字符串当成 NULL——
/// 用一个枚举把三种情况分开，比在调用处拼字符串更不容易出错。
#[derive(Clone)]
enum Bind {
    Text(String),
    Int(i64),
    Null,
}

impl From<&str> for Bind {
    fn from(value: &str) -> Self {
        Bind::Text(value.to_string())
    }
}

impl From<String> for Bind {
    fn from(value: String) -> Self {
        Bind::Text(value)
    }
}

impl From<i64> for Bind {
    fn from(value: i64) -> Self {
        Bind::Int(value)
    }
}

impl From<Option<i64>> for Bind {
    fn from(value: Option<i64>) -> Self {
        value.map(Bind::Int).unwrap_or(Bind::Null)
    }
}

impl From<Option<String>> for Bind {
    fn from(value: Option<String>) -> Self {
        value.map(Bind::Text).unwrap_or(Bind::Null)
    }
}

impl Fixture {
    async fn exec(&self, sql: &str, binds: Vec<Bind>) -> i64 {
        let mut query = sqlx::query(sql);
        for bind in binds {
            query = match bind {
                Bind::Text(text) => query.bind(text),
                Bind::Int(value) => query.bind(value),
                Bind::Null => query.bind(None::<i64>),
            };
        }
        let mut txn = self.store.begin_immediate().await.unwrap();
        let out = query.execute(txn.conn()).await.unwrap().last_insert_rowid();
        txn.commit().await.unwrap();
        out
    }

    async fn user(&self, login: &str) -> i64 {
        self.exec(
            "INSERT INTO users(login_name, display_name, role, quota_group, created_at, \
             updated_at) VALUES (?, ?, 'user', 'normal', ?, ?)",
            vec![login.into(), login.into(), iso(T0).into(), iso(T0).into()],
        )
        .await
    }

    /// 一个已批准的 runtime。
    async fn runtime(&self, runtime_id: &str, approval: &str) -> i64 {
        self.exec(
            "INSERT INTO node_runtimes(node_id, runtime_id, started_at_unix_ms, first_snapshot, \
             approval_status, window_state, first_seen_at, last_seen_at) \
             VALUES (?, ?, ?, 'baseline', ?, 'open', ?, ?)",
            vec![
                NODE.into(),
                runtime_id.into(),
                format!("{:020}", T0).into(),
                approval.into(),
                iso(T0).into(),
                iso(T0).into(),
            ],
        )
        .await
    }

    /// runtime 里的一个 inbound + 一个身份。`generation` 可变，用来造多 generation。
    async fn identity(&self, runtime_pk: i64, inbound: &str, name: &str, generation: &str) {
        let service = self
            .exec(
                "INSERT INTO runtime_services(runtime_pk, inbound_tag, generation, inbound_type, \
                 active, first_seen_at) VALUES (?, ?, ?, 'shadowsocks', 1, ?)",
                vec![runtime_pk.into(), inbound.into(), generation.into(), iso(T0).into()],
            )
            .await;
        self.exec(
            "INSERT INTO runtime_identities(runtime_pk, runtime_service_id, identity_name, \
             generation, active, first_seen_at, last_seen_at) VALUES (?, ?, ?, ?, 1, ?, ?)",
            vec![
                runtime_pk.into(),
                service.into(),
                name.into(),
                generation.into(),
                iso(T0).into(),
                iso(T0).into(),
            ],
        )
        .await;
    }

    /// 一个槽位。归属落在这里，跨重启存活。
    async fn route(&self, inbound: &str, name: &str, user_id: Option<i64>, state: &str) -> i64 {
        self.exec(
            // CHECK 要求 state 与 (user_id, claimed_at) 三者自洽：claimed 必须两者都有，
            // free 必须两者都空，retired 要么都有要么都空。
            "INSERT INTO identity_routes(node_id, inbound_tag, identity_name, state, user_id, \
             claimed_at, retired_at, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
            vec![
                NODE.into(),
                inbound.into(),
                name.into(),
                state.into(),
                user_id.into(),
                user_id.map(|_| iso(T0)).into(),
                (state == "retired").then(|| iso(T0)).into(),
                iso(T0).into(),
            ],
        )
        .await
    }

    async fn event(
        &self,
        route_id: i64,
        user_id: Option<i64>,
        state: &str,
        from_ms: i64,
        to_ms: Option<i64>,
    ) {
        let sql = match to_ms {
            Some(_) => {
                "INSERT INTO identity_assignment_events(route_id, user_id, state, \
                 effective_from, effective_to, recorded_at) VALUES (?, ?, ?, ?, ?, ?)"
            }
            None => {
                "INSERT INTO identity_assignment_events(route_id, user_id, state, \
                 effective_from, recorded_at) VALUES (?, ?, ?, ?, ?)"
            }
        };
        let mut binds: Vec<Bind> =
            vec![route_id.into(), user_id.into(), state.into(), iso(from_ms).into()];
        if let Some(to) = to_ms {
            binds.push(iso(to).into());
        }
        binds.push(iso(from_ms).into());
        self.exec(sql, binds).await;
    }
}

fn record(inbound: &str, name: &str, ts: i64) -> AccessRecord {
    AccessRecord {
        seq: 1,
        ts,
        node: NODE.into(),
        run: RUN.into(),
        user: name.into(),
        inbound: inbound.into(),
        net: "tcp".into(),
        host: "www.example.com".into(),
        host_src: HostSource::Sniff,
        port: 443,
        up: 100,
        down: 200,
        ms: 10,
    }
}

// ---- 四值关联 ----

#[tokio::test]
async fn 正常情况下归属解析得出来() {
    let f = fixture().await;
    let alice = f.user("alice").await;
    let pk = f.runtime(RUN, "approved").await;
    f.identity(pk, "ss-entry", "id-01", GEN).await;
    let route = f.route("ss-entry", "id-01", Some(alice), "claimed").await;
    f.event(route, Some(alice), "assigned", T0, None).await;

    let mut resolver = Resolver::new(&f.store);
    let found = resolver.attribute(&record("ss-entry", "id-01", T0 + 1_000)).await.unwrap();
    assert_eq!(found, Attribution::Resolved(alice));
}

/// **最容易串号的一种。**
///
/// `identity_routes` 的唯一键刻意带 `inbound_tag`，所以「同一个身份名在两个入口上
/// 分属两人」是**合法状态**。而审计文件名只由身份名编码而来，**两个入口共用同一个
/// 文件**，文件里 A 和 B 的行是混在一起的——关联不带 `in` 的话，一个人能查到
/// 另一个人的全部记录。
#[tokio::test]
async fn 同节点跨入口同名分属两人时互相查不到() {
    let f = fixture().await;
    let alice = f.user("alice").await;
    let bob = f.user("bob").await;
    let pk = f.runtime(RUN, "approved").await;
    f.identity(pk, "ss-a", "id-01", GEN).await;
    f.identity(pk, "ss-b", "id-01", GEN).await;
    let route_a = f.route("ss-a", "id-01", Some(alice), "claimed").await;
    let route_b = f.route("ss-b", "id-01", Some(bob), "claimed").await;
    f.event(route_a, Some(alice), "assigned", T0, None).await;
    f.event(route_b, Some(bob), "assigned", T0, None).await;

    // 同一份文件里的两行，只有 `in` 不同。
    let mixed = vec![
        AccessRecord { host: "alice-only.example.com".into(), ..record("ss-a", "id-01", T0 + 1) },
        AccessRecord { host: "bob-only.example.com".into(), ..record("ss-b", "id-01", T0 + 2) },
    ];

    let mine = filter::for_user(&f.store, alice, mixed.clone()).await.unwrap();
    assert_eq!(mine.rows.len(), 1);
    assert_eq!(mine.rows[0].host, "alice-only.example.com");
    assert_eq!(mine.dropped_other_owner, 1, "另一条要被算进「不是你的」");

    let theirs = filter::for_user(&f.store, bob, mixed).await.unwrap();
    assert_eq!(theirs.rows.len(), 1);
    assert_eq!(theirs.rows[0].host, "bob-only.example.com");
}

/// 四值关联到多行 → **不返回**，而不是取第一行。
///
/// schema 的三个 UNIQUE 约束都带 generation，而 JSONL 不带，所以这是 schema
/// 允许而协议无法区分的状态。取第一行会把两个 generation 的记录混成一个人的。
#[tokio::test]
async fn 关联不唯一时不返回而不是取第一行() {
    let f = fixture().await;
    let alice = f.user("alice").await;
    let pk = f.runtime(RUN, "approved").await;
    // 同一个 runtime、同一个 inbound tag、同一个身份名，两个 generation。
    f.identity(pk, "ss-entry", "id-01", "00000000000000000001").await;
    f.identity(pk, "ss-entry", "id-01", "00000000000000000002").await;
    let route = f.route("ss-entry", "id-01", Some(alice), "claimed").await;
    f.event(route, Some(alice), "assigned", T0, None).await;

    let mut resolver = Resolver::new(&f.store);
    assert_eq!(
        resolver.attribute(&record("ss-entry", "id-01", T0 + 1)).await.unwrap(),
        Attribution::Ambiguous
    );

    let out =
        filter::for_user(&f.store, alice, vec![record("ss-entry", "id-01", T0 + 1)]).await.unwrap();
    assert!(out.rows.is_empty(), "归属不明就不返回");
    assert_eq!(out.dropped_ambiguous, 1);
}

#[tokio::test]
async fn 关联不到已登记lineage时不兜底() {
    let f = fixture().await;
    let alice = f.user("alice").await;
    f.runtime(RUN, "approved").await;
    // 槽位有，但这个 runtime 下没有登记过这个身份——不能用当前映射兜底。
    f.route("ss-entry", "id-01", Some(alice), "claimed").await;

    let out =
        filter::for_user(&f.store, alice, vec![record("ss-entry", "id-01", T0 + 1)]).await.unwrap();
    assert!(out.rows.is_empty());
    assert_eq!(out.dropped_unknown, 1);
}

#[tokio::test]
async fn 未批准的runtime不出现在任何人的明细里() {
    let f = fixture().await;
    let alice = f.user("alice").await;
    let pk = f.runtime(RUN, "pending").await;
    f.identity(pk, "ss-entry", "id-01", GEN).await;
    let route = f.route("ss-entry", "id-01", Some(alice), "claimed").await;
    f.event(route, Some(alice), "assigned", T0, None).await;

    let out =
        filter::for_user(&f.store, alice, vec![record("ss-entry", "id-01", T0 + 1)]).await.unwrap();
    assert!(out.rows.is_empty());
    assert_eq!(out.dropped_unapproved, 1);
}

// ---- 历史持有区间 ----

/// **这条专门抓 `unassigned` 那个开口到无穷的区间。**
///
/// 退役那条路径会先关掉上一段，再插一条 `unassigned` 事件——而那条新事件的
/// `effective_to` 永远是 NULL，之后没有任何代码去关它，而且它**带着 user_id**。
/// 只按 `user_id` 取区间（不加 `state = 'assigned'`）就会得到一条开口到无穷的区间，
/// 把退役之后到达的记录全判给原持有人。
#[tokio::test]
async fn 退役之后产生的记录不归给原持有人() {
    let f = fixture().await;
    let alice = f.user("alice").await;
    let pk = f.runtime(RUN, "approved").await;
    f.identity(pk, "ss-entry", "id-01", GEN).await;
    let route = f.route("ss-entry", "id-01", Some(alice), "retired").await;

    let retired_at = T0 + 60_000;
    // 持有区间 [T0, retired_at)，已关闭。
    f.event(route, Some(alice), "assigned", T0, Some(retired_at)).await;
    // 退役事件：带着同一个 user_id，effective_to 永远为 NULL。
    f.event(route, Some(alice), "unassigned", retired_at, None).await;

    let before = record("ss-entry", "id-01", retired_at - 1);
    let boundary = record("ss-entry", "id-01", retired_at);
    let after = record("ss-entry", "id-01", retired_at + 60_000);

    let out = filter::for_user(&f.store, alice, vec![before, boundary, after]).await.unwrap();
    assert_eq!(out.rows.len(), 1, "只有退役之前那一条：{out:?}");
    assert_eq!(out.rows[0].ts, retired_at - 1);
    // 半开区间 [from, to)：右端点那一刻**不**属于原持有人。
    assert_eq!(out.dropped_other_owner, 2);
}

#[tokio::test]
async fn 持有之前产生的记录也不归给他() {
    let f = fixture().await;
    let alice = f.user("alice").await;
    let pk = f.runtime(RUN, "approved").await;
    f.identity(pk, "ss-entry", "id-01", GEN).await;
    let route = f.route("ss-entry", "id-01", Some(alice), "claimed").await;
    let claimed_at = T0 + 60_000;
    f.event(route, Some(alice), "assigned", claimed_at, None).await;

    let out = filter::for_user(
        &f.store,
        alice,
        vec![record("ss-entry", "id-01", claimed_at - 1), record("ss-entry", "id-01", claimed_at)],
    )
    .await
    .unwrap();
    assert_eq!(out.rows.len(), 1, "左端点那一刻算在区间内");
    assert_eq!(out.rows[0].ts, claimed_at);
}

#[tokio::test]
async fn 换过主人的槽位按时刻各归各的() {
    let f = fixture().await;
    let alice = f.user("alice").await;
    let bob = f.user("bob").await;
    let pk = f.runtime(RUN, "approved").await;
    f.identity(pk, "ss-entry", "id-01", GEN).await;
    let route = f.route("ss-entry", "id-01", Some(bob), "claimed").await;
    let handover = T0 + 60_000;
    f.event(route, Some(alice), "assigned", T0, Some(handover)).await;
    f.event(route, Some(bob), "assigned", handover, None).await;

    let records =
        vec![record("ss-entry", "id-01", handover - 1), record("ss-entry", "id-01", handover)];
    // 归属是**不可变**的（D14）：槽位现在归 bob，不代表交接前那条也归 bob。
    let hers = filter::for_user(&f.store, alice, records.clone()).await.unwrap();
    assert_eq!(hers.rows.len(), 1);
    assert_eq!(hers.rows[0].ts, handover - 1);
    let his = filter::for_user(&f.store, bob, records).await.unwrap();
    assert_eq!(his.rows.len(), 1);
    assert_eq!(his.rows[0].ts, handover);
}

// ---- 成功判据复核 ----

/// 节点侧失败的记录压根不写文件，但 up/down 是原样落盘的、判据交由读取方复核，
/// 而 M6 准入用例是直接对**主控**断言这一条的。
#[tokio::test]
async fn 成功判据在主控侧再判一遍() {
    let f = fixture().await;
    let alice = f.user("alice").await;
    let pk = f.runtime(RUN, "approved").await;
    f.identity(pk, "ss-entry", "id-01", GEN).await;
    let route = f.route("ss-entry", "id-01", Some(alice), "claimed").await;
    f.event(route, Some(alice), "assigned", T0, None).await;

    let base = record("ss-entry", "id-01", T0 + 1);
    let out = filter::for_user(
        &f.store,
        alice,
        vec![
            AccessRecord { up: 100, down: 0, ..base.clone() },
            AccessRecord { net: "udp".into(), up: 100, down: 0, ..base.clone() },
            AccessRecord { up: 100, down: 200, ..base.clone() },
            AccessRecord { net: "udp".into(), up: 0, down: 200, ..base },
        ],
    )
    .await
    .unwrap();
    assert_eq!(out.rows.len(), 2, "TCP 的 up>0,down=0 不返回；UDP 同条件返回：{out:?}");
    assert_eq!(out.dropped_unsuccessful, 2);
    assert!(out.rows.iter().any(|r| r.net == "udp" && r.down == 0));
}

/// 裁剪要在**聚合、计数、分页之前**。这里的反向证据是：丢掉的条数分门别类，
/// 而返回的行里一条不属于本人的都没有。
#[tokio::test]
async fn 裁剪在统计之前且丢弃原因分得开() {
    let f = fixture().await;
    let alice = f.user("alice").await;
    let bob = f.user("bob").await;
    let pk = f.runtime(RUN, "approved").await;
    f.identity(pk, "ss-a", "id-01", GEN).await;
    f.identity(pk, "ss-b", "id-01", GEN).await;
    let route_a = f.route("ss-a", "id-01", Some(alice), "claimed").await;
    let route_b = f.route("ss-b", "id-01", Some(bob), "claimed").await;
    f.event(route_a, Some(alice), "assigned", T0, None).await;
    f.event(route_b, Some(bob), "assigned", T0, None).await;

    let out = filter::for_user(
        &f.store,
        alice,
        vec![
            record("ss-a", "id-01", T0 + 1),
            record("ss-b", "id-01", T0 + 2),
            AccessRecord { up: 1, down: 0, ..record("ss-a", "id-01", T0 + 3) },
            record("ss-a", "id-99", T0 + 4),
        ],
    )
    .await
    .unwrap();
    assert_eq!(out.rows.len(), 1);
    assert_eq!(out.dropped_other_owner, 1);
    assert_eq!(out.dropped_unsuccessful, 1);
    assert_eq!(out.dropped_unknown, 1);
    assert_eq!(out.dropped_total(), 3);
}

/// 排序按 seq 不按 ts——ts 是墙钟，时钟回拨时 seq 仍严格递增。
/// 但 seq 只在 `(user, run)` 内单调，所以先按 run 分组。
#[tokio::test]
async fn 排序按run分段再按seq() {
    let f = fixture().await;
    let alice = f.user("alice").await;
    for run in [RUN, "ffffffffffffffffffffffffffffffff"] {
        let pk = f.runtime(run, "approved").await;
        f.identity(pk, "ss-entry", "id-01", GEN).await;
    }
    let route = f.route("ss-entry", "id-01", Some(alice), "claimed").await;
    f.event(route, Some(alice), "assigned", T0, None).await;

    let other_run = "ffffffffffffffffffffffffffffffff";
    let out = filter::for_user(
        &f.store,
        alice,
        vec![
            // ts 递减但 seq 递增：时钟回拨的样子。
            AccessRecord { seq: 2, ts: T0 + 10, ..record("ss-entry", "id-01", T0) },
            AccessRecord { seq: 1, ts: T0 + 20, ..record("ss-entry", "id-01", T0) },
            AccessRecord {
                seq: 1,
                ts: T0 + 5,
                run: other_run.into(),
                ..record("ss-entry", "id-01", T0)
            },
        ],
    )
    .await
    .unwrap();
    assert_eq!(out.rows.len(), 3);
    assert_eq!(
        out.rows.iter().map(|r| (r.run.as_str(), r.seq)).collect::<Vec<_>>(),
        vec![(RUN, 1), (RUN, 2), (other_run, 1)],
        "同一 run 内按 seq，跨 run 不混排"
    );
}
