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
    // 持有起点放得足够早。用「昨天」当起点的话，任何一条几天前的记录都会被
    // 正确地判成「持有之前」而不返回——那时用例失败的原因与它要测的东西无关。
    let now = "2026-01-01T00:00:00Z";
    let mut txn = api.store.begin_immediate().await.unwrap();
    sqlx::query(
        "INSERT OR IGNORE INTO nodes(node_id, provider, address, first_snapshot, \
         agent_spki_sha256, quota_enabled, status, created_at, updated_at) \
         VALUES (?, 'plus_v3', 'node-01.example.com:8443', 'baseline', ?, 1, 'active', ?, ?)",
    )
    .bind(NODE)
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
    .bind(NODE)
    .bind(RUN)
    .bind(now)
    .bind(now)
    .fetch_one(txn.conn())
    .await
    .unwrap();
    let service: i64 = sqlx::query_scalar(
        "INSERT INTO runtime_services(runtime_pk, inbound_tag, generation, inbound_type, active, \
         first_seen_at) VALUES (?, ?, ?, 'shadowsocks', 1, ?) RETURNING runtime_service_id",
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
        "INSERT INTO identity_routes(node_id, inbound_tag, identity_name, state, user_id, \
         claimed_at, created_at) VALUES (?, ?, ?, 'claimed', ?, ?, ?) RETURNING route_id",
    )
    .bind(NODE)
    .bind(inbound)
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

fn hosts(body: &Value) -> Vec<String> {
    body["rows"]
        .as_array()
        .unwrap()
        .iter()
        .map(|row| row["host"].as_str().unwrap().to_string())
        .collect()
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
    // 同一个身份名在两个入口上分属两人——最容易串号的那种状态。
    wire_identity(&api, alice.user_id, "ss-a", "id-01").await;
    wire_identity(&api, bob.user_id, "ss-b", "id-01").await;
    let ts = now_ms() - 60_000;
    api.seed_audit(
        NODE,
        "id-01",
        &[
            line("id-01", "ss-a", "alice-only.example.com", 1, ts, 100, 200),
            line("id-01", "ss-b", "bob-only.example.com", 2, ts + 1, 100, 200),
        ],
    );

    let (_, mine, _) = api.get("/api/v1/me/audit/access", Some(&alice)).await;
    let path = format!("/api/v1/users/{}/audit/access", alice.user_id);
    let (status, theirs, _) = api.get(&path, Some(&admin)).await;
    assert_eq!(status, StatusCode::OK, "{theirs}");
    assert_eq!(mine["rows"], theirs["rows"], "两条路径必须给出同一份行");
    assert_eq!(hosts(&mine), vec!["alice-only.example.com"]);
    assert_eq!(mine["dropped"]["dropped_other_owner"], 1, "bob 那条要被裁掉并计数");
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
