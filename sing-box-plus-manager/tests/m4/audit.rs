//! 出站目标审计的两条端点。
//!
//! 最要紧的一条是**管理员按用户查与本人自助查拿到的东西逐字节相同**：
//! C35 明写两者执行同一套过滤规则，而一旦写成两份，它们会在某次改动里分家，
//! 分家的方向几乎总是管理员那一侧更宽松。

use axum::http::StatusCode;
use serde_json::Value;

use proxy_manager::api::session::Role;

use crate::harness::Api;

fn error_code(body: &Value) -> &str {
    body["error"]["code"].as_str().unwrap_or_default()
}

const NODE: &str = "node-example-01";
const RUN: &str = "0123456789abcdef0123456789abcdef";
const GEN: &str = "00000000000000000001";

/// 指定节点的一行。`line` 是它在默认节点上的特例。
#[allow(clippy::too_many_arguments)]
fn line_on(
    node: &str,
    user: &str,
    inbound: &str,
    host: &str,
    seq: u64,
    ts: i64,
    up: u64,
    down: u64,
) -> String {
    format!(
        r#"{{"down":{down},"host":"{host}","host_src":"sniff","in":"{inbound}","ms":12,"net":"tcp","node":"{node}","port":443,"run":"{RUN}","seq":{seq},"ts":{ts},"up":{up},"user":"{user}"}}"#
    )
}

fn line(user: &str, inbound: &str, host: &str, seq: u64, ts: i64, up: u64, down: u64) -> String {
    format!(
        r#"{{"down":{down},"host":"{host}","host_src":"sniff","in":"{inbound}","ms":12,"net":"tcp","node":"{NODE}","port":443,"run":"{RUN}","seq":{seq},"ts":{ts},"up":{up},"user":"{user}"}}"#
    )
}

fn now_ms() -> i64 {
    (time::OffsetDateTime::now_utc().unix_timestamp_nanos() / 1_000_000) as i64
}

/// 铺一条完整的归属链：runtime → service → identity → route → assigned 事件。
async fn wire_identity(api: &Api, user_id: i64, inbound: &str, identity: &str) {
    wire_identity_on(api, NODE, user_id, inbound, identity).await;
}

/// 同上，但指定节点。按节点筛选的用例要在两台上各铺一份。
async fn wire_identity_on(api: &Api, node: &str, user_id: i64, inbound: &str, identity: &str) {
    // 持有起点放得足够早。用「昨天」当起点的话，任何一条几天前的记录都会被
    // 正确地判成「持有之前」而不返回——那时用例失败的原因与它要测的东西无关。
    let now = "2026-01-01T00:00:00Z";
    let mut txn = api.store.begin_immediate().await.unwrap();
    sqlx::query(
        "INSERT OR IGNORE INTO nodes(node_id, provider, address, first_snapshot, \
         agent_spki_sha256, quota_enabled, status, created_at, updated_at) \
         VALUES (?, 'plus_v3', 'node-01.example.com:8443', 'baseline', ?, 1, 'active', ?, ?)",
    )
    .bind(node)
    .bind("0".repeat(64))
    .bind(now)
    .bind(now)
    .execute(txn.conn())
    .await
    .unwrap();
    let runtime_pk: i64 = sqlx::query_scalar(
        "INSERT INTO node_runtimes(node_id, runtime_id, started_at_unix_ms, first_snapshot, \
         approval_status, window_state, first_seen_at, last_seen_at) \
         VALUES (?, ?, '00000000000000000001', 'baseline', 'approved', 'open', ?, ?) \
         ON CONFLICT(node_id, runtime_id) DO UPDATE SET last_seen_at = excluded.last_seen_at \
         RETURNING runtime_pk",
    )
    .bind(node)
    .bind(RUN)
    .bind(now)
    .bind(now)
    .fetch_one(txn.conn())
    .await
    .unwrap();
    let service: i64 = sqlx::query_scalar(
        "INSERT INTO runtime_services(runtime_pk, inbound_tag, generation, inbound_type, active, \
         first_seen_at) VALUES (?, ?, ?, 'shadowsocks', 1, ?) \
         ON CONFLICT(runtime_pk, inbound_tag, generation) DO UPDATE SET active = excluded.active \
         RETURNING runtime_service_id",
    )
    .bind(runtime_pk)
    .bind(inbound)
    .bind(GEN)
    .bind(now)
    .fetch_one(txn.conn())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO runtime_identities(runtime_pk, runtime_service_id, identity_name, \
         generation, active, first_seen_at, last_seen_at) VALUES (?, ?, ?, ?, 1, ?, ?)",
    )
    .bind(runtime_pk)
    .bind(service)
    .bind(identity)
    .bind(GEN)
    .bind(now)
    .bind(now)
    .execute(txn.conn())
    .await
    .unwrap();
    let route: i64 = sqlx::query_scalar(
        "INSERT INTO identity_routes(node_id, identity_name, state, user_id, \
         claimed_at, created_at) VALUES (?, ?, 'claimed', ?, ?, ?) RETURNING route_id",
    )
    .bind(node)
    .bind(identity)
    .bind(user_id)
    .bind(now)
    .bind(now)
    .fetch_one(txn.conn())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO identity_assignment_events(route_id, user_id, state, effective_from, \
         recorded_at) VALUES (?, ?, 'assigned', ?, ?)",
    )
    .bind(route)
    .bind(user_id)
    .bind(now)
    .bind(now)
    .execute(txn.conn())
    .await
    .unwrap();
    txn.commit().await.unwrap();
}

/// 把一个已有身份原样挂到另一条 inbound 上：只补节点侧的 service/identity 行，
/// **不碰 `identity_routes`**。
///
/// 槽位键是 `(node_id, identity_name)`——一个名字在一个节点上只有一行 route，
/// 它挂在几个入口上是部署细节。想再插一行 route 来表示「第二个协议」的，
/// 会被唯一键当场拒掉，那正是这次改动要立的规矩。
async fn mirror_inbound(api: &Api, inbound: &str, identity: &str) {
    let now = "2026-01-01T00:00:00Z";
    let mut txn = api.store.begin_immediate().await.unwrap();
    let runtime_pk: i64 = sqlx::query_scalar(
        "SELECT runtime_pk FROM node_runtimes WHERE node_id = ? ORDER BY runtime_pk LIMIT 1",
    )
    .bind(NODE)
    .fetch_one(txn.conn())
    .await
    .unwrap();
    let service: i64 = sqlx::query_scalar(
        "INSERT INTO runtime_services(runtime_pk, inbound_tag, generation, inbound_type, active, \
         first_seen_at) VALUES (?, ?, ?, 'vless', 1, ?) \
         ON CONFLICT(runtime_pk, inbound_tag, generation) DO UPDATE SET active = excluded.active \
         RETURNING runtime_service_id",
    )
    .bind(runtime_pk)
    .bind(inbound)
    .bind(GEN)
    .bind(now)
    .fetch_one(txn.conn())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO runtime_identities(runtime_pk, runtime_service_id, identity_name, \
         generation, active, first_seen_at, last_seen_at) VALUES (?, ?, ?, ?, 1, ?, ?)",
    )
    .bind(runtime_pk)
    .bind(service)
    .bind(identity)
    .bind(GEN)
    .bind(now)
    .bind(now)
    .execute(txn.conn())
    .await
    .unwrap();
    txn.commit().await.unwrap();
}

fn hosts(body: &Value) -> Vec<String> {
    body["rows"]
        .as_array()
        .unwrap()
        .iter()
        .map(|row| row["host"].as_str().unwrap().to_string())
        .collect()
}

/// 把一个身份从旧持有者交给新持有者，**照 `identity::claim` 的 replace 分支做**：
/// 先关上一段区间，再补 unassigned，最后给新人补 assigned。
async fn hand_over(api: &Api, identity: &str, to_user: i64, at: &str) {
    let mut txn = api.store.begin_immediate().await.unwrap();
    let route: i64 = sqlx::query_scalar(
        "SELECT route_id FROM identity_routes WHERE node_id = ? AND identity_name = ?",
    )
    .bind(NODE)
    .bind(identity)
    .fetch_one(txn.conn())
    .await
    .unwrap();
    sqlx::query(
        "UPDATE identity_assignment_events SET effective_to = ? \
          WHERE route_id = ? AND effective_to IS NULL",
    )
    .bind(at)
    .bind(route)
    .execute(txn.conn())
    .await
    .unwrap();
    for (user, state) in [(None, "unassigned"), (Some(to_user), "assigned")] {
        sqlx::query(
            "INSERT INTO identity_assignment_events(route_id, user_id, state, \
             effective_from, recorded_at) VALUES (?, ?, ?, ?, ?)",
        )
        .bind(route)
        .bind(user)
        .bind(state)
        .bind(at)
        .bind(at)
        .execute(txn.conn())
        .await
        .unwrap();
    }
    sqlx::query("UPDATE identity_routes SET user_id = ?, claimed_at = ? WHERE route_id = ?")
        .bind(to_user)
        .bind(at)
        .bind(route)
        .execute(txn.conn())
        .await
        .unwrap();
    txn.commit().await.unwrap();
}

/// 一个身份换过人之后，**新持有者看得到自己的记录，旧持有者看不到**。
///
/// 2026-09-20 线上撞见的形态：一个名字换过持有者之后，新人一条都看不到，
/// 而他那一千多条全出现在旧持有者页上。成因是换身份那条路径没关上一段持有
/// 区间，`audit::filter` 的 holdings 顺序扫、命中第一个就返回，于是开口到
/// 无穷又排在前面的旧区间把后来的记录全吃了。
///
/// 这条用例断的是**后果**而不是那一列的值：换一种方式再犯同样的错，它照样红。
#[tokio::test]
async fn 换过人的身份记录归新持有者() {
    let api = Api::new().await;
    let alice = api.user("alice", Role::User, "local").await;
    let bob = api.user("bob", Role::User, "local").await;
    wire_identity(&api, alice.user_id, "ss-entry", "id-01").await;
    hand_over(&api, "id-01", bob.user_id, "2026-06-01T00:00:00Z").await;

    // 交接之后产生的记录。
    let ts = now_ms() - 60_000;
    api.seed_audit(
        NODE,
        "id-01",
        &[
            line("id-01", "ss-entry", "bob.example.com", 1, ts, 100, 200),
            line("id-01", "ss-entry", "bob.example.com", 2, ts + 1, 100, 200),
        ],
    );

    let (status, body, _) = api.get("/api/v1/me/audit/access", Some(&bob)).await;
    assert_eq!(status, StatusCode::OK, "{body}");
    assert_eq!(hosts(&body), vec!["bob.example.com"], "新持有者该看到自己的记录：{body}");
    assert_eq!(body["rows"][0]["count"], 2);

    // 反面同样要断。少了这一半，「旧持有者仍然看得见」这个泄漏不会被发现——
    // 而那正是线上真实发生的事，比新人看不见更严重。
    let (status, body, _) = api.get("/api/v1/me/audit/access", Some(&alice)).await;
    assert_eq!(status, StatusCode::OK, "{body}");
    assert!(hosts(&body).is_empty(), "旧持有者不该看到交接之后的记录：{body}");
}

#[tokio::test]
async fn 本人查得到自己的出站目标() {
    let api = Api::new().await;
    let alice = api.user("alice", Role::User, "local").await;
    wire_identity(&api, alice.user_id, "ss-entry", "id-01").await;
    let ts = now_ms() - 60_000;
    api.seed_audit(
        NODE,
        "id-01",
        &[
            line("id-01", "ss-entry", "www.example.com", 1, ts, 100, 200),
            line("id-01", "ss-entry", "www.example.com", 2, ts + 1, 100, 200),
            line("id-01", "ss-entry", "cdn.example.net", 3, ts + 2, 50, 60),
        ],
    );

    let (status, body, _) = api.get("/api/v1/me/audit/access", Some(&alice)).await;
    assert_eq!(status, StatusCode::OK, "{body}");
    assert_eq!(body["enabled"], true);
    assert_eq!(body["range"], "7d", "缺省是 7d");
    assert_eq!(body["archive_state"], "available");
    // 按 (host, port, host_src) 聚合，次数多的在前。
    assert_eq!(hosts(&body), vec!["www.example.com", "cdn.example.net"]);
    assert_eq!(body["rows"][0]["count"], 2);
    assert_eq!(body["rows"][0]["up"], 200);
    assert_eq!(body["rows"][0]["host_src"], "sniff");
    // 节点只按大小轮转，可见延迟没有上界保证——这一条要一路说到页面上。
    assert_eq!(body["archive_delay_bounded"], false);
}

/// **同一套过滤规则。** 管理员查 alice 拿到的，与 alice 自己查到的逐字节相同。
#[tokio::test]
async fn 管理员按用户查与本人自助查结果相同() {
    let api = Api::with_admins(&["root"]).await;
    let admin = api.user("root", Role::Admin, "paoyou").await;
    let alice = api.user("alice", Role::User, "paoyou").await;
    let bob = api.user("bob", Role::User, "paoyou").await;
    // alice 的名字同时挂在 SS 与 VLESS 两个入口上，bob 是另一个名字。
    // 记录文件按身份名分，所以 bob 那条是「别人的名字落进了我的文件」——
    // C35 不信文件名、按记录自己的四元组重新定归属，正是为了这种情况。
    wire_identity(&api, alice.user_id, "ss-a", "id-01").await;
    mirror_inbound(&api, "vless-a", "id-01").await;
    wire_identity(&api, bob.user_id, "ss-a", "id-02").await;
    let ts = now_ms() - 60_000;
    api.seed_audit(
        NODE,
        "id-01",
        &[
            // SS 那个目标去过两次、VLESS 一次。**次数必须不同**：行的排序是
            // 次数降序、再按最近一次，而「最近一次」精确到秒——两条只差毫秒的
            // 记录会落进同一秒，那时先后由别的东西决定，用例就会时绿时红。
            line("id-01", "ss-a", "alice-ss.example.com", 1, ts, 100, 200),
            line("id-01", "ss-a", "alice-ss.example.com", 2, ts + 1, 100, 200),
            line("id-01", "vless-a", "alice-vless.example.com", 3, ts + 2, 100, 200),
            line("id-02", "ss-a", "bob-only.example.com", 4, ts + 3, 100, 200),
        ],
    );

    let (_, mine, _) = api.get("/api/v1/me/audit/access", Some(&alice)).await;
    let path = format!("/api/v1/users/{}/audit/access", alice.user_id);
    let (status, theirs, _) = api.get(&path, Some(&admin)).await;
    assert_eq!(status, StatusCode::OK, "{theirs}");
    assert_eq!(mine["rows"], theirs["rows"], "两条路径必须给出同一份行");
    assert_eq!(
        hosts(&mine),
        // 次数多的在前：SS 两次、VLESS 一次。
        vec!["alice-ss.example.com", "alice-vless.example.com"],
        "同一个名字的两个协议都是本人的，一条都不能丢"
    );
    assert_eq!(mine["dropped"]["dropped_other_owner"], 1, "bob 那条要被裁掉并计数");
    assert_eq!(mine["dropped"]["dropped_ambiguous"], 0, "放宽 JOIN 不得把 route 行乘出两条来");
}

#[tokio::test]
async fn 普通用户查别人被拒() {
    let api = Api::with_admins(&["root"]).await;
    let alice = api.user("alice", Role::User, "paoyou").await;
    let bob = api.user("bob", Role::User, "paoyou").await;
    let path = format!("/api/v1/users/{}/audit/access", bob.user_id);
    let (status, body, _) = api.get(&path, Some(&alice)).await;
    assert_eq!(status, StatusCode::FORBIDDEN);
    assert_eq!(error_code(&body), "forbidden");
}

/// 连**查自己**也要走 `/me`：管理面那条路由无条件要求管理员。
///
/// 放行「普通用户用管理路由查自己」看起来无害，代价是那条路由从此有两种主体来源，
/// 而下一个人改它时只会记得其中一种。
#[tokio::test]
async fn 普通用户用管理路由查自己也被拒() {
    let api = Api::with_admins(&["root"]).await;
    let alice = api.user("alice", Role::User, "paoyou").await;
    let path = format!("/api/v1/users/{}/audit/access", alice.user_id);
    let (status, _, _) = api.get(&path, Some(&alice)).await;
    assert_eq!(status, StatusCode::FORBIDDEN);
}

#[tokio::test]
async fn 时间范围是闭集() {
    let api = Api::new().await;
    let alice = api.user("alice", Role::User, "local").await;
    for range in ["24h", "7d", "30d"] {
        let path = format!("/api/v1/me/audit/access?range={range}");
        let (status, body, _) = api.get(&path, Some(&alice)).await;
        assert_eq!(status, StatusCode::OK, "{range}");
        assert_eq!(body["range"], range);
    }
    for bad in ["1h", "90d", "", "7D", "all"] {
        let path = format!("/api/v1/me/audit/access?range={bad}");
        let (status, body, _) = api.get(&path, Some(&alice)).await;
        assert_eq!(status, StatusCode::BAD_REQUEST, "{bad} 应当被拒");
        assert_eq!(error_code(&body), "invalid_request");
    }
}

#[tokio::test]
async fn 范围之外的记录不返回() {
    let api = Api::new().await;
    let alice = api.user("alice", Role::User, "local").await;
    wire_identity(&api, alice.user_id, "ss-entry", "id-01").await;
    let now = now_ms();
    api.seed_audit(
        NODE,
        "id-01",
        &[
            line("id-01", "ss-entry", "recent.example.com", 1, now - 3_600_000, 1, 1),
            line("id-01", "ss-entry", "old.example.com", 2, now - 10 * 86_400_000, 1, 1),
        ],
    );
    let (_, day, _) = api.get("/api/v1/me/audit/access?range=24h", Some(&alice)).await;
    assert_eq!(hosts(&day), vec!["recent.example.com"]);
    let (_, month, _) = api.get("/api/v1/me/audit/access?range=30d", Some(&alice)).await;
    assert_eq!(month["rows"].as_array().unwrap().len(), 2);
}

/// `no_data` 与 `unavailable` 是两回事。
///
/// 前者是「查过了，这个人名下没有记录」，后者是「同步还没跑过，我不知道」。
/// 合成一个的话，页面只能说一句「没有数据」，而那句话在第二种情况下是错的。
#[tokio::test]
async fn 没有数据与读不到分得开() {
    let api = Api::new().await;
    let alice = api.user("alice", Role::User, "local").await;
    // 审计树根还不存在：同步从没跑过。
    let (_, body, _) = api.get("/api/v1/me/audit/access", Some(&alice)).await;
    assert_eq!(body["archive_state"], "unavailable");

    // 树存在但这个人名下没有记录。
    api.seed_audit(NODE, "id-99", &[]);
    let (_, body, _) = api.get("/api/v1/me/audit/access", Some(&alice)).await;
    assert_eq!(body["archive_state"], "no_data");
    assert!(body["rows"].as_array().unwrap().is_empty());
    assert!(body["latest_available_event_at"].is_null());
}

/// 缺口要显示出来，而且**不带「丢了 N 条」的估算**，除非节点自己给了 `n`。
#[tokio::test]
async fn 缺口照原样显示不做估算() {
    let api = Api::new().await;
    let alice = api.user("alice", Role::User, "local").await;
    wire_identity(&api, alice.user_id, "ss-entry", "id-01").await;
    let ts = now_ms() - 60_000;
    api.seed_audit(
        NODE,
        "id-01",
        &[
            line("id-01", "ss-entry", "www.example.com", 1, ts, 1, 1),
            r#"{"seq":2,"ev":"gap","after":1,"n":37,"reason":"queue_full"}"#.to_string(),
            // n 可以是 null：边界不确定时节点刻意不合成一个不存在的区间。
            r#"{"seq":3,"ev":"gap","after":2,"n":null,"reason":"total_cap"}"#.to_string(),
        ],
    );
    let (_, body, _) = api.get("/api/v1/me/audit/access", Some(&alice)).await;
    let gaps = body["gaps"].as_array().unwrap();
    assert_eq!(gaps.len(), 2, "{body}");
    assert_eq!(gaps[0]["kind"], "queue_full");
    assert_eq!(gaps[0]["n"], 37);
    assert!(gaps[1]["n"].is_null(), "节点没给条数就不能编一个");
    // 诊断行绝不能进用户统计。
    assert_eq!(hosts(&body), vec!["www.example.com"]);
    assert_eq!(body["rows"][0]["count"], 1);
}

/// **每一次查询都要落日志**，包括被拒的和查不到的——
/// 「查了但没查到」与「没查过」在这份日志里必须是两回事。
#[tokio::test]
async fn 每次拉取都记进查询日志() {
    let api = Api::with_admins(&["root"]).await;
    let admin = api.user("root", Role::Admin, "paoyou").await;
    let alice = api.user("alice", Role::User, "paoyou").await;

    api.get("/api/v1/me/audit/access?range=24h", Some(&alice)).await;
    let path = format!("/api/v1/users/{}/audit/access?range=30d", alice.user_id);
    api.get(&path, Some(&admin)).await;

    let log = api.audit_queries();
    assert_eq!(log.len(), 2, "{log:?}");
    assert_eq!(log[0].actor, alice.user_id);
    assert_eq!(log[0].data_subject, alice.user_id);
    assert_eq!(log[0].range, "24h");
    // 管理员那条要同时记下**是谁**看了**谁的**明细。
    assert_eq!(log[1].actor, admin.user_id);
    assert_eq!(log[1].data_subject, alice.user_id);
    assert_eq!(log[1].range, "30d");
    assert!(!log[1].at.is_empty());
}

/// 被拒的查询**不**记进日志——它没看到任何东西。
/// 而参数错误的那次记，因为主体已经过了鉴权、确实发起了一次对某人明细的访问。
#[tokio::test]
async fn 鉴权没过的查询不记日志() {
    let api = Api::with_admins(&["root"]).await;
    let alice = api.user("alice", Role::User, "paoyou").await;
    let bob = api.user("bob", Role::User, "paoyou").await;
    let path = format!("/api/v1/users/{}/audit/access", bob.user_id);
    assert_eq!(api.get(&path, Some(&alice)).await.0, StatusCode::FORBIDDEN);
    assert!(api.audit_queries().is_empty());
}

/// **写不下查询日志就不给看。**
///
/// 这份日志是 §4.8 对「谁看过谁的明细」的全部答案。悄悄放行等于让那个答案有一个
/// 没人知道的缺口——而缺口的存在本身，正是这份日志要防的东西。
///
/// 用「把 queries.jsonl 建成一个目录」来制造写失败：确定、可复现，
/// 而且不依赖用例跑在什么身份下（改权限位对 root 无效）。
#[tokio::test]
async fn 查询日志写不下去就拒绝查询() {
    let api = Api::new().await;
    let alice = api.user("alice", Role::User, "local").await;
    wire_identity(&api, alice.user_id, "ss-entry", "id-01").await;
    api.seed_audit(
        NODE,
        "id-01",
        &[line("id-01", "ss-entry", "secret.example.com", 1, now_ms() - 1_000, 1, 1)],
    );
    std::fs::create_dir_all(api.audit_dir.join(proxy_manager::audit::queries::FILE)).unwrap();

    let (status, body, _) = api.get("/api/v1/me/audit/access", Some(&alice)).await;
    assert_eq!(status, StatusCode::INTERNAL_SERVER_ERROR, "{body}");
    assert_eq!(error_code(&body), "internal");
    // 一条明细都不能漏出去。
    assert!(body.get("rows").is_none(), "{body}");
    assert!(!body.to_string().contains("secret.example.com"), "{body}");
}

/// 响应里**只有聚合后的行**，没有逐连接的原始记录。
///
/// 这条是真跑了一遍才发现的：`dropped` 那一块是把内部的 `Filtered` 直接
/// 序列化出去的，而它带着整份原始记录（含 run / seq / ms）。
/// 页面只要聚合，多出来的那份既是冗余，也是本可以不发生的明细外泄。
#[tokio::test]
async fn 响应里没有逐连接的原始记录() {
    let api = Api::new().await;
    let alice = api.user("alice", Role::User, "local").await;
    wire_identity(&api, alice.user_id, "ss-entry", "id-01").await;
    api.seed_audit(
        NODE,
        "id-01",
        &[line("id-01", "ss-entry", "www.example.com", 4242, now_ms() - 1_000, 100, 200)],
    );

    let (_, body, _) = api.get("/api/v1/me/audit/access", Some(&alice)).await;
    let text = body.to_string();
    assert!(text.contains("www.example.com"), "聚合后的行要在");
    for leaked in ["4242", RUN, "\"ms\"", "\"seq\"", "\"user\"", "\"in\""] {
        assert!(!text.contains(leaked), "响应里不该出现 {leaked}：{text}");
    }
    // dropped 那一块只该有计数。
    let dropped = body["dropped"].as_object().unwrap();
    assert!(dropped.keys().all(|key| key.starts_with("dropped_")), "{dropped:?}");
}

/// **候选文件的前缀匹配不许多匹配到别人的文件。**
///
/// 缩小候选集用的前缀是 `access-<b64url(身份名)>.jsonl`，匹配方式是
/// `starts_with`。**那个 `.jsonl` 不是装饰**：去掉它，`user0001` 的前缀会命中
/// `user00010` 的文件（base64url 里 `dXNlcjAwMDE` 是 `dXNlcjAwMDEw` 的前缀）。
///
/// 访问行有 C35 兜底——串进来的行归属判不到本人，会被裁掉。
/// **但诊断行没有四元组、裁剪不了**，它是按文件收的。所以前缀一旦多匹配，
/// 别人文件里的缺口会原样显示给你，而那是一条关于别人的事实。
/// 这条用例挑的就是这个信号。
#[tokio::test]
async fn 前缀相近的身份文件不会被误读() {
    let api = Api::new().await;
    let alice = api.user("alice", Role::User, "local").await;
    wire_identity(&api, alice.user_id, "ss-entry", "user0001").await;
    let ts = now_ms() - 60_000;
    api.seed_audit(
        NODE,
        "user0001",
        &[line("user0001", "ss-entry", "mine.example.com", 1, ts, 1, 1)],
    );
    // 另一个身份，名字在 base64url 下与上面那个是前缀关系。它的文件里有一条缺口。
    api.seed_audit(
        NODE,
        "user00010",
        &[
            line("user00010", "ss-entry", "theirs.example.com", 1, ts, 1, 1),
            r#"{"seq":2,"ev":"gap","after":1,"n":99,"reason":"queue_full"}"#.to_string(),
        ],
    );

    let (_, body, _) = api.get("/api/v1/me/audit/access", Some(&alice)).await;
    assert_eq!(hosts(&body), vec!["mine.example.com"]);
    assert!(body["gaps"].as_array().unwrap().is_empty(), "别人文件里的缺口不能出现在这里：{body}");
    // 反向：别人的文件确实存在且里面确实有那条缺口，否则这条用例是空的。
    let other =
        api.audit_dir.join(NODE).join(proxy_manager_wire::audit::active_file_name("user00010"));
    assert!(std::fs::read_to_string(&other).unwrap().contains("queue_full"));
}

fn filter_line(seq: u64, hosts: &[&str], ips: &[&str]) -> String {
    format!(
        r#"{{"seq":{seq},"ev":"filter","hosts":{},"ips":{}}}"#,
        serde_json::to_string(hosts).unwrap(),
        serde_json::to_string(ips).unwrap()
    )
}

/// **页面必须说得出哪些目标不记录。**
///
/// 不说的话，一个看到 N 个目标的人会以为那就是他去过的全部地方——而开了排除之后
/// 那是一句假话（线上实测排掉的是 85.6%）。名单取自文件里的 `ev=filter` 行，
/// 不取自配置：配置在节点上，主控看不到；而 filter 行记的是**那些记录写下来的
/// 当时**真正生效的规则。
#[tokio::test]
async fn 排除名单从filter行取并原样返回() {
    let api = Api::new().await;
    let alice = api.user("alice", Role::User, "local").await;
    wire_identity(&api, alice.user_id, "ss-entry", "id-01").await;
    let ts = now_ms() - 60_000;
    api.seed_audit(
        NODE,
        "id-01",
        &[
            filter_line(1, &["google.com", "apple.com"], &["198.18.0.0/15"]),
            line("id-01", "ss-entry", "www.example.com", 2, ts, 1, 1),
        ],
    );

    let (_, body, _) = api.get("/api/v1/me/audit/access", Some(&alice)).await;
    let excluded = &body["excluded"];
    assert_eq!(excluded["observed"], true);
    assert_eq!(excluded["consistent"], true);
    // 排序后返回，页面不必自己再排。
    assert_eq!(excluded["hosts"], serde_json::json!(["apple.com", "google.com"]));
    assert_eq!(excluded["ips"], serde_json::json!(["198.18.0.0/15"]));
    // filter 行**不进**用户统计，也不算缺口。
    assert_eq!(hosts(&body), vec!["www.example.com"]);
    assert!(body["gaps"].as_array().unwrap().is_empty());
}

/// 名单顺序对节点没有意义（线性扫描），所以顺序不同**不算规则变过**。
#[tokio::test]
async fn 顺序不同不算规则变过() {
    let api = Api::new().await;
    let alice = api.user("alice", Role::User, "local").await;
    wire_identity(&api, alice.user_id, "ss-entry", "id-01").await;
    api.seed_audit(
        NODE,
        "id-01",
        &[
            filter_line(1, &["google.com", "apple.com"], &[]),
            filter_line(2, &["apple.com", "google.com"], &[]),
        ],
    );
    let (_, body, _) = api.get("/api/v1/me/audit/access", Some(&alice)).await;
    assert_eq!(body["excluded"]["consistent"], true, "{body}");
}

/// 同一个时间窗里读到两套**不同**的规则：报出来，并取并集。
///
/// 挑一套显示是在替读者做一个他不知道的选择；取交集会少说几个「不记录」，
/// 而少说的那几个正是他会以为「我没去过」的目标。
#[tokio::test]
async fn 规则变过时报出来并取并集() {
    let api = Api::new().await;
    let alice = api.user("alice", Role::User, "local").await;
    wire_identity(&api, alice.user_id, "ss-entry", "id-01").await;
    api.seed_audit(NODE, "id-01", &[filter_line(1, &["google.com"], &[])]);
    api.seed_audit(
        "node-example-02",
        "id-01",
        &[filter_line(1, &["apple.com"], &["17.253.0.0/16"])],
    );

    let (_, body, _) = api.get("/api/v1/me/audit/access", Some(&alice)).await;
    assert_eq!(body["excluded"]["consistent"], false, "{body}");
    assert_eq!(body["excluded"]["hosts"], serde_json::json!(["apple.com", "google.com"]));
    assert_eq!(body["excluded"]["ips"], serde_json::json!(["17.253.0.0/16"]));
}

/// 没读到 `filter` 行时 `observed` 为假，**而不是返回一个空名单**。
///
/// 空名单会被读成「什么都记录」。而真相可能是「这些记录写下来的时候文件已经开着」——
/// 节点只在**打开文件**时写这一行，所以启用排除之后、下一次轮转或重启之前，
/// 老文件里不会有它。
#[tokio::test]
async fn 没有filter行时说不知道而不是说没有排除() {
    let api = Api::new().await;
    let alice = api.user("alice", Role::User, "local").await;
    wire_identity(&api, alice.user_id, "ss-entry", "id-01").await;
    api.seed_audit(
        NODE,
        "id-01",
        &[line("id-01", "ss-entry", "www.example.com", 1, now_ms() - 1_000, 1, 1)],
    );
    let (_, body, _) = api.get("/api/v1/me/audit/access", Some(&alice)).await;
    assert_eq!(body["excluded"]["observed"], false);
    assert!(body["excluded"]["hosts"].as_array().unwrap().is_empty());

    // 同一页里读不到任何文件时也是 observed=false（archive_state 已经说了原因）。
    let bob = api.user("bob", Role::User, "local").await;
    let (_, empty, _) = api.get("/api/v1/me/audit/access", Some(&bob)).await;
    assert_eq!(empty["excluded"]["observed"], false);
}

/// 节点侧 2026-09-22 起在 `ev=filter` 行里多写一个 `ports`。
/// 主控要把它**一路带到页面上**，否则一个看到零条 NTP 的人
/// 无从知道那是被排掉了，而不是他真的没同步过时间。
#[tokio::test]
async fn 排除规则里的端口要带到页面上() {
    let api = Api::new().await;
    let alice = api.user("alice", Role::User, "local").await;
    wire_identity(&api, alice.user_id, "ss-entry", "id-01").await;
    let ts = now_ms() - 60_000;
    api.seed_audit(
        NODE,
        "id-01",
        &[
            r#"{"seq":1,"ev":"filter","hosts":["ubuntu.com"],"ips":["198.18.0.0/15"],"ports":[123,4460]}"#.to_string(),
            line("id-01", "ss-entry", "www.example.com", 2, ts, 100, 200),
        ],
    );

    let (status, body, _) = api.get("/api/v1/me/audit/access", Some(&alice)).await;
    assert_eq!(status, StatusCode::OK, "{body}");
    assert_eq!(body["excluded"]["observed"], true);
    assert_eq!(body["excluded"]["hosts"][0], "ubuntu.com");
    assert_eq!(body["excluded"]["ports"][0], 123);
    assert_eq!(body["excluded"]["ports"][1], 4460);
}

/// **只在端口上不同的两套规则，必须判成「规则变过」。**
///
/// 少把 ports 纳入比较的话，这两行会塌成一条，`consistent` 报「没变过」——
/// 一个静默的错误答案，而页面据此决定说哪句话。
#[tokio::test]
async fn 只有端口不同也算规则变过() {
    let api = Api::new().await;
    let alice = api.user("alice", Role::User, "local").await;
    wire_identity(&api, alice.user_id, "ss-entry", "id-01").await;
    let ts = now_ms() - 60_000;
    api.seed_audit(
        NODE,
        "id-01",
        &[
            r#"{"seq":1,"ev":"filter","hosts":["a.example"],"ips":[],"ports":[123]}"#.to_string(),
            line("id-01", "ss-entry", "www.example.com", 2, ts, 100, 200),
            // 同样的 hosts/ips，只有 ports 变了——重启后续写那一行。
            r#"{"seq":3,"ev":"filter","hosts":["a.example"],"ips":[],"ports":[123,4460]}"#
                .to_string(),
        ],
    );

    let (_, body, _) = api.get("/api/v1/me/audit/access", Some(&alice)).await;
    assert_eq!(body["excluded"]["consistent"], false, "只有端口变了也是变了：{body}");
    // 不一致时取并集：宁可多说几个「不记录」，也不能少说。
    let ports: Vec<i64> =
        body["excluded"]["ports"].as_array().unwrap().iter().map(|v| v.as_i64().unwrap()).collect();
    assert_eq!(ports, vec![123, 4460], "{body}");
}

/// 旧节点写的 filter 行没有 `ports` 键。**照常解析**，不能整行落进 unparsed——
/// 主控可能先于节点升级，那时全部诊断行会一起失效。
#[tokio::test]
async fn 没有ports键的旧filter行照常解析() {
    let api = Api::new().await;
    let alice = api.user("alice", Role::User, "local").await;
    wire_identity(&api, alice.user_id, "ss-entry", "id-01").await;
    api.seed_audit(
        NODE,
        "id-01",
        &[r#"{"seq":1,"ev":"filter","hosts":["old.example"],"ips":[]}"#.to_string()],
    );
    let (_, body, _) = api.get("/api/v1/me/audit/access", Some(&alice)).await;
    assert_eq!(body["unparsed"], 0, "旧格式不该解析失败：{body}");
    assert_eq!(body["excluded"]["hosts"][0], "old.example");
    assert!(body["excluded"]["ports"].as_array().unwrap().is_empty());
}

// ============ 按节点筛选 ============

const NODE_B: &str = "node-example-02";

/// 在两台节点上各给 alice 铺一份记录。
async fn seeded_two_nodes(api: &Api) -> crate::harness::Actor {
    let alice = api.user("alice", Role::User, "local").await;
    wire_identity_on(api, NODE, alice.user_id, "ss-entry", "id-01").await;
    wire_identity_on(api, NODE_B, alice.user_id, "ss-entry", "id-01").await;
    let ts = now_ms() - 60_000;
    api.seed_audit(
        NODE,
        "id-01",
        &[line_on(NODE, "id-01", "ss-entry", "only-on-a.example", 1, ts, 10, 20)],
    );
    api.seed_audit(
        NODE_B,
        "id-01",
        &[line_on(NODE_B, "id-01", "ss-entry", "only-on-b.example", 1, ts, 30, 40)],
    );
    alice
}

#[tokio::test]
async fn 按节点筛掉别台的记录() {
    let api = Api::new().await;
    let alice = seeded_two_nodes(&api).await;

    // 不筛：两台的都在。**这一半是控制组**——少了它，下面那条断言
    // 在「筛选把什么都筛没了」时同样是绿的。
    let (_, body, _) = api.get("/api/v1/me/audit/access", Some(&alice)).await;
    assert_eq!(hosts(&body).len(), 2, "{body}");

    let (status, body, _) =
        api.get(&format!("/api/v1/me/audit/access?node={NODE}"), Some(&alice)).await;
    assert_eq!(status, StatusCode::OK, "{body}");
    assert_eq!(hosts(&body), vec!["only-on-a.example"], "只该剩这一台的：{body}");

    let (_, body, _) =
        api.get(&format!("/api/v1/me/audit/access?node={NODE_B}"), Some(&alice)).await;
    assert_eq!(hosts(&body), vec!["only-on-b.example"], "{body}");
}

/// **下拉框的选项在筛选之前算。**
///
/// 用筛选之后的算，选中某一台之后选项就只剩那一台，人再也选不回「全部」——
/// 一个把自己锁死的界面，而且它不报错，只是越点越少。
#[tokio::test]
async fn 节点清单不随筛选缩水() {
    let api = Api::new().await;
    let alice = seeded_two_nodes(&api).await;

    let (_, body, _) = api.get("/api/v1/me/audit/access", Some(&alice)).await;
    let all: Vec<&str> =
        body["nodes"].as_array().unwrap().iter().map(|v| v.as_str().unwrap()).collect();
    assert_eq!(all, vec![NODE, NODE_B], "{body}");

    // 筛到一台之后，清单**仍然**是两台。
    let (_, body, _) = api.get(&format!("/api/v1/me/audit/access?node={NODE}"), Some(&alice)).await;
    let after: Vec<&str> =
        body["nodes"].as_array().unwrap().iter().map(|v| v.as_str().unwrap()).collect();
    assert_eq!(after, vec![NODE, NODE_B], "筛选后清单不该缩水：{body}");
    assert_eq!(hosts(&body), vec!["only-on-a.example"], "但行要筛：{body}");
}

/// **筛选不是绕过归属裁剪的口子。**
///
/// 指定一个自己没有记录的节点，拿到的是空，不是别人的记录。
#[tokio::test]
async fn 筛选绕不过归属裁剪() {
    let api = Api::new().await;
    let alice = api.user("alice", Role::User, "local").await;
    let bob = api.user("bob", Role::User, "local").await;
    wire_identity_on(&api, NODE, alice.user_id, "ss-entry", "id-01").await;
    wire_identity_on(&api, NODE_B, bob.user_id, "ss-entry", "id-02").await;
    let ts = now_ms() - 60_000;
    api.seed_audit(
        NODE,
        "id-01",
        &[line_on(NODE, "id-01", "ss-entry", "alice.example", 1, ts, 1, 1)],
    );
    api.seed_audit(
        NODE_B,
        "id-02",
        &[line_on(NODE_B, "id-02", "ss-entry", "bob.example", 1, ts, 1, 1)],
    );

    // alice 指名要 bob 那台。
    let (status, body, _) =
        api.get(&format!("/api/v1/me/audit/access?node={NODE_B}"), Some(&alice)).await;
    assert_eq!(status, StatusCode::OK, "{body}");
    assert!(hosts(&body).is_empty(), "不该看到别人的记录：{body}");
    // 而且那台压根不该出现在他的清单里。
    let nodes: Vec<&str> =
        body["nodes"].as_array().unwrap().iter().map(|v| v.as_str().unwrap()).collect();
    assert!(!nodes.contains(&NODE_B), "别人的节点不该列给他：{body}");
}

/// 不认识的节点名**不是错误**，是一个空结果。
///
/// 回 400 会让人以为自己填错了，而真实情况可能是这台刚加进来、他还没走过。
#[tokio::test]
async fn 不认识的节点名返回空而不是报错() {
    let api = Api::new().await;
    let alice = seeded_two_nodes(&api).await;
    let (status, body, _) =
        api.get("/api/v1/me/audit/access?node=并不存在的节点", Some(&alice)).await;
    assert_eq!(status, StatusCode::OK, "{body}");
    assert!(hosts(&body).is_empty(), "{body}");
}

/// 空串 = 不筛。下拉框的「全部节点」那一项 value 就是空串。
#[tokio::test]
async fn 空串按不筛处理() {
    let api = Api::new().await;
    let alice = seeded_two_nodes(&api).await;
    let (_, body, _) = api.get("/api/v1/me/audit/access?node=", Some(&alice)).await;
    assert_eq!(hosts(&body).len(), 2, "{body}");
}

/// 页面上的筛选控件与脚本必须对得上。
///
/// 加了下拉框却没接 `reloadOn`，症状是「选了没反应」且不报错——
/// 本项目为这一类「服务端知道，界面不说」吃过亏。
#[test]
fn 节点筛选控件已接线() {
    let page = proxy_manager::web::page_body("me-audit.html").expect("页面该在");
    let (script, _) = proxy_manager::web::asset("assets/api.js").expect("脚本该在");

    assert!(page.contains(r#"select name="audit-node""#), "页面上没有节点下拉框");
    // 换成 change 之外的事件、或者漏了这个选择器，切换就不会重新取数。
    assert!(
        script.contains("select[name='audit-node']"),
        "reloadOn 没有把下拉框算进去，选了不会重新取数"
    );
    // 取数要带上它，否则筛选永远是「全部」。
    assert!(script.contains("auditNode()"), "取数没带 node 参数");
    // 选项要由服务端给的 nodes 填，而不是前端自己编一份。
    assert!(script.contains("renderAuditNodes(data.nodes)"), "下拉框没有被填");
}
