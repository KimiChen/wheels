//! 一个人同时持有 SS 与 VLESS：一个名字、一个槽位、两条入口。
//!
//! 槽位键是 `(node_id, identity_name)`。同一个名字挂在几条 inbound 上是**部署细节**，
//! 不改变「一个计费身份是一个人的一个名额」这件事。
//!
//! 这一组盯的全是**零报错**的失败形态：
//!
//! * 键里多一列 → 一个名字在一个节点上两行 → `pick_free_slot` 的
//!   `count(DISTINCT node_id)` 退化 → 池子看起来是空的，没有任何东西会红
//! * `assemble` 退回等权平分 → 用户在一半额度处被切断，而 PUT 照样 200、表照样全量
//! * 入账按入口分行 → 两个协议各记各的，月度用量少算一半

use std::collections::BTreeMap;

use proxy_manager::identity;
use proxy_manager::quota::table;
use proxy_manager::quota::wire::Quota;
use proxy_manager::store::codec::U128Text;
use sqlx::Row;

use super::quota_harness::{Stack, NODE, RUNTIME};

/// 基线里唯一四向计数为零的身份——也就是 `pick_free_slot` 唯一的合法候选。
const NAME: &str = "s1";
const SS: &str = "ss-in";
const VLESS: &str = "vless-entry-01";

/// 把 `s1` 原样挂到 VLESS 入口上，再结算一轮让主控看见它。
async fn mirror(stack: &Stack) {
    stack.node.mirror_identity(SS, VLESS, "vless", NAME).await;
    stack.settle_once().await;
}

async fn scalar<T>(stack: &Stack, sql: &str) -> T
where
    T: for<'r> sqlx::Decode<'r, sqlx::Sqlite> + sqlx::Type<sqlx::Sqlite> + Send + Unpin,
{
    sqlx::query_scalar(sql).fetch_one(stack.store.readers()).await.unwrap()
}

async fn make_user(stack: &Stack, login: &str) -> i64 {
    let mut txn = stack.store.begin_immediate().await.unwrap();
    let row = sqlx::query(
        "INSERT INTO users(login_name, display_name, role, quota_group, status, created_at, updated_at) \
         VALUES (?, ?, 'user', 'normal', 'active', ?, ?) RETURNING user_id",
    )
    .bind(login)
    .bind(login)
    .bind("2026-09-18T00:00:00Z")
    .bind("2026-09-18T00:00:00Z")
    .fetch_one(txn.conn())
    .await
    .unwrap();
    let id: i64 = row.get(0);
    txn.commit().await.unwrap();
    id
}

// ============ 1. 结算只登记一个槽位 ============

#[tokio::test]
async fn 同名跨两条入口只登记一个槽位() {
    let stack = Stack::new().await;
    mirror(&stack).await;

    // 节点侧确实是两条记录——否则下面那条断言是空的。
    let seen: Vec<String> = stack
        .identities()
        .await
        .into_iter()
        .filter(|identity| identity.name == NAME)
        .map(|identity| identity.inbound_tag)
        .collect();
    assert_eq!(seen, vec![SS.to_string(), VLESS.to_string()], "节点侧应当有两条入口记录");

    let routes: i64 = scalar(
        &stack,
        &format!(
            "SELECT count(*) FROM identity_routes \
              WHERE node_id = '{NODE}' AND identity_name = '{NAME}'"
        ),
    )
    .await;
    assert_eq!(routes, 1, "一个名字在一个节点上只有一个名额，不随入口条数增长");
}

// ============ 2. 额度对两条入口各发全额 ============

#[tokio::test]
async fn 两条入口各拿全额而不是各拿一半() {
    let stack = Stack::new().await;
    mirror(&stack).await;
    let user = make_user(&stack, "kimi").await;
    identity::claim_for_user(&stack.store, user, "用例").await.unwrap();

    let identities = stack.identities().await;
    let allocations: BTreeMap<i64, Quota> = [(user, Quota::Limited(4_096))].into_iter().collect();
    let entries = table::assemble(&identities, &allocations).unwrap();

    let sequence = stack.settle_once().await;
    let (_, outcome) = stack.push(entries, Some(sequence)).await;
    assert!(
        matches!(outcome, proxy_manager::quota::dispatch::PushOutcome::Applied { applied: 4 }),
        "实际：{outcome:?}"
    );

    // 端到端看节点侧的生效表，而不是只看 assemble 的返回值：
    // 节点的 quotaCell 挂在 (inbound_tag, name) 上，是两个互不相干的计数器，
    // 「各发全额」要在那一层成立才算数。
    let effective = stack.effective().await;
    let ss = effective.get(&(SS.to_string(), NAME.to_string())).copied();
    let vless = effective.get(&(VLESS.to_string(), NAME.to_string())).copied();
    assert_eq!(ss, Some(Quota::Limited(4_096)), "SS 入口应当拿到全额");
    assert_eq!(vless, ss, "VLESS 入口与 SS 入口拿到同一个数——退回平分的话这里是 2048");
}

// ============ 3. 池子不因为双入口而翻倍 ============

#[tokio::test]
async fn 双入口不会让身份池看起来是空的() {
    let stack = Stack::new().await;
    mirror(&stack).await;
    let user = make_user(&stack, "kimi").await;

    // 基线里另外两个身份带着非零计数，不是合法候选。于是 s1 是唯一的候选，
    // 这条用例才真的在考 `pick_free_slot` 怎么数行。
    let claim = identity::claim_for_user(&stack.store, user, "用例")
        .await
        .expect("s1 四向计数为零且在唯一的节点上空闲，必须认领得到");
    assert_eq!(claim.identity_name, NAME);
    assert!(claim.newly_claimed);
    assert_eq!(claim.nodes, vec![NODE.to_string()]);
}

// ============ 4. 两个协议的流量并进同一行月度用量 ============

#[tokio::test]
async fn 跨协议流量合并进同一行月度用量() {
    let stack = Stack::new().await;
    mirror(&stack).await;
    let user = make_user(&stack, "kimi").await;
    identity::claim_for_user(&stack.store, user, "用例").await.unwrap();

    // 认领之后才跑的流量才归这个人（D14：归属在结算那一刻定死）。
    stack.node.set_counter(SS, NAME, "tcp_uplink_bytes", 1_000).await;
    stack.node.set_counter(SS, NAME, "tcp_downlink_bytes", 2_000).await;
    stack.node.set_counter(VLESS, NAME, "tcp_uplink_bytes", 400).await;
    stack.node.set_counter(VLESS, NAME, "udp_downlink_bytes", 30).await;
    stack.settle_once().await;

    let rows: i64 =
        scalar(&stack, &format!("SELECT count(*) FROM usage_cycle_totals WHERE user_id = {user}"))
            .await;
    assert_eq!(rows, 1, "月度用量按人记，不按入口分行");

    let total: String = scalar(
        &stack,
        &format!("SELECT total_bytes FROM usage_cycle_totals WHERE user_id = {user}"),
    )
    .await;
    assert_eq!(
        U128Text::decode(&total).unwrap().get(),
        1_000 + 2_000 + 400 + 30,
        "两个协议的字节要相加——分行的话这里只会看到其中一半"
    );
}

// ============ 5. 一人一节点只能有一个在用槽位 ============

#[tokio::test]
async fn 同一节点上第二个在用槽位被索引拒掉() {
    let stack = Stack::new().await;
    mirror(&stack).await;
    let user = make_user(&stack, "kimi").await;
    identity::claim_for_user(&stack.store, user, "用例").await.unwrap();

    // 绕过 `claim_for_user` 的幂等短路直接写库——短路让它永远碰不到这条索引，
    // 于是索引本身从来没被任何用例触发过。而「各发全额」之后，
    // 一个人在一个节点上拿两个名字就是**两份全额**，这条索引是唯一挡着它的东西。
    let mut txn = stack.store.begin_immediate().await.unwrap();
    let error = sqlx::query(
        "UPDATE identity_routes SET user_id = ?, state = 'claimed', claimed_at = ? \
          WHERE node_id = ? AND identity_name <> ? AND state = 'free' \
          AND route_id = (SELECT min(route_id) FROM identity_routes \
                           WHERE node_id = ? AND identity_name <> ? AND state = 'free')",
    )
    .bind(user)
    .bind("2026-09-18T00:00:00Z")
    .bind(NODE)
    .bind(NAME)
    .bind(NODE)
    .bind(NAME)
    .execute(txn.conn())
    .await
    .expect_err("一人一节点一个在用槽位，第二个必须被 idx_identity_routes_active_per_user 拒掉");
    // SQLite 报的是列名不是索引名。表上的 UNIQUE 是 (node_id, identity_name)，
    // 所以 (node_id, user_id) 这一对只可能来自那条部分索引——认准它就够了。
    assert!(
        error.to_string().contains("identity_routes.node_id, identity_routes.user_id"),
        "拒绝的理由要是那条索引，而不是别的什么：{error}"
    );
}

#[allow(dead_code)]
fn _runtime_is_used() -> &'static str {
    RUNTIME
}
