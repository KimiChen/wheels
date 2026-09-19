//! 计费槽位的认领与退役（定案第一条、D11）。
//!
//! 开通不改节点配置、不 reload、不断任何人的连接：300 个身份早就装在每个节点的
//! inbound 里了，开通只是把其中一个槽位从 `free` 移到 `claimed`，
//! 下一轮额度表就会给它正额度。
//!
//! **退役即永久。** 复用一个槽位前必须更换 uPSK，而那要改节点配置并 reload；
//! 不换就直接复用，前一个持有者存在客户端里的凭据会去花下一个人的额度。
//! 于是池子只减不增，300 个名额用完才需要扩容——那时才需要一次 reload。
//!
//! 槽位行不是在这里创建的：**结算发现，这里只移动状态**（见 `ledger::settle`）。
//! 因此注册表的内容永远来自节点实际上报的东西。
//!
//! # 认领的目标节点是「在册的」，不是「启用了配额的」
//!
//! 这里曾经要求 `quota_enabled = 1`。那是错的耦合：认领定的是**归属**，
//! 而归属服务于**计费**——账本给谁记账，跟这个节点推不推配额没有关系。
//! 一个只计量、不限额的节点上，字节照样要有主人。
//!
//! 现实里的后果也很直接：本轮四台节点刻意不武装配额（原型控制器仍是唯一的
//! 配额写入方），于是 `quota_enabled` 全是 0——按旧口径，**没有任何人能被开通**，
//! 而那跟「要不要限额」根本是两件事。

use sqlx::Row;
use time::OffsetDateTime;

use crate::error::{Error, Result};
use crate::ledger::bucket;
use crate::store::Store;

/// 20 位定宽的零。四向计数全为它才算零基线。
const ZERO_U64: &str = "00000000000000000000";

/// 一次认领的结果。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Claim {
    pub user_id: i64,
    pub identity_name: String,
    /// 认领落在哪些节点上。**全部在册节点**——同一个身份名在每台上是同一份
    /// 凭据，缺一台，用户的订阅里就会有一条连不上的入口。
    pub nodes: Vec<String>,
    /// 是否是本次新认领。重复调用返回 `false` 且给回同一个身份。
    pub newly_claimed: bool,
}

/// 给用户认领一个身份。**幂等**：已经有的直接返回，不再消耗一个名额。
///
/// 同一个身份名在四个节点上是同一份凭据（同一个 uPSK），所以认领必须
/// 在**全部**启用配额的节点上落同一个名字——否则用户的订阅里会有几条入口
/// 连不上，而那看起来像网络问题。
pub async fn claim_for_user(store: &Store, user_id: i64, actor: &str) -> Result<Claim> {
    if actor.trim().is_empty() {
        return Err(Error::Quota("认领必须留下操作者".into()));
    }
    let now = bucket::to_rfc3339(OffsetDateTime::now_utc());
    let mut txn = store.begin_immediate().await?;

    let targets: Vec<String> = sqlx::query("SELECT node_id FROM nodes WHERE status = 'active'")
        .fetch_all(txn.conn())
        .await?
        .into_iter()
        .map(|row| row.get(0))
        .collect();
    if targets.is_empty() {
        return Err(Error::Quota("没有启用配额的在用节点，认领无处落地".into()));
    }

    // D11：重复登录不重复分配、不重置用量。
    let existing: Option<String> = sqlx::query(
        "SELECT identity_name FROM identity_routes WHERE user_id = ? AND state = 'claimed' LIMIT 1",
    )
    .bind(user_id)
    .fetch_optional(txn.conn())
    .await?
    .map(|row| row.get(0));
    if let Some(identity_name) = existing {
        txn.commit().await?;
        return Ok(Claim { user_id, identity_name, nodes: targets, newly_claimed: false });
    }

    let candidate = pick_free_slot(&mut txn, targets.len() as i64).await?.ok_or_else(|| {
        // R12：池耗尽要有明确错误码，绝不静默退化成复用一个已用过的身份。
        Error::Quota(
            "身份池已耗尽：没有在全部节点上都空闲且四向计数为零的身份。\
             扩容需要改节点配置并 reload，不能靠复用退役身份"
                .into(),
        )
    })?;

    // CAS：`WHERE state = 'free'` 让「同一个槽位被认领两次」写不进去，
    // 而不是靠上面那次 SELECT 的时效性。影响行数必须恰好等于节点数。
    let changed = sqlx::query(
        "UPDATE identity_routes \
            SET user_id = ?, state = 'claimed', claimed_at = ?, \
                claim_fence_runtime_id = ?, claim_fence_sequence = ? \
          WHERE identity_name = ? AND state = 'free' \
            AND node_id IN (SELECT node_id FROM nodes WHERE status = 'active')",
    )
    .bind(user_id)
    .bind(&now)
    .bind(&candidate.fence_runtime_id)
    .bind(&candidate.fence_sequence)
    .bind(&candidate.identity_name)
    .execute(txn.conn())
    .await?
    .rows_affected();
    if changed != targets.len() as u64 {
        return Err(Error::Quota(format!(
            "认领影响了 {changed} 行而不是 {}：槽位状态在事务内被并发改动",
            targets.len()
        )));
    }

    // 只追加的归属事件（D14）。半开区间的 effective_to 留空表示仍在持有。
    let routes: Vec<i64> =
        sqlx::query("SELECT route_id FROM identity_routes WHERE identity_name = ? AND user_id = ?")
            .bind(&candidate.identity_name)
            .bind(user_id)
            .fetch_all(txn.conn())
            .await?
            .into_iter()
            .map(|row| row.get(0))
            .collect();
    for route_id in routes {
        sqlx::query(
            "INSERT INTO identity_assignment_events(route_id, user_id, state, \
             effective_from, recorded_at, reason) VALUES (?, ?, 'assigned', ?, ?, ?)",
        )
        .bind(route_id)
        .bind(user_id)
        .bind(&now)
        .bind(&now)
        .bind(actor)
        .execute(txn.conn())
        .await?;
    }

    txn.commit().await?;
    tracing::info!(user_id, identity = %candidate.identity_name, actor, "已认领身份");
    Ok(Claim {
        user_id,
        identity_name: candidate.identity_name,
        nodes: targets,
        newly_claimed: true,
    })
}

struct Candidate {
    identity_name: String,
    fence_runtime_id: Option<String>,
    fence_sequence: Option<String>,
}

/// FIFO 挑一个在**全部**目标节点上都空闲、且四向计数全零的身份。
///
/// 零基线门禁用的是**实际观察**而不是「配置里应该是零」（D11 §4.6）：
/// 只要它在**当前** runtime 上动过哪怕一个字节，就不再可认领。
/// 这条挡住的是「把一个用过的身份发给新用户，于是他一上来就欠着别人的账」。
///
/// # 为什么只看当前 runtime，不看历史上全部已批准的
///
/// 原先看的是「任何一个已批准 runtime」。那过严，而且严得没有收益：
/// `usage_lifetime_totals` 是按 `runtime_identity_id` 键的——**每个 runtime 一行**，
/// 所以 `bump_lifetime` 的 `coalesce(user_id, ?)` 只会填当前 runtime 那一行。
/// 跨 runtime 的追溯归属在结构上不可能发生，历史字节留在历史那一行、归属为空。
///
/// 而代价是实打实的：节点一重启就换 runtime，旧窗口的非零计数会把那个身份
/// **永久**挡在池子外面——池子只减不增，等于每次重启都悄悄损耗几个名额，
/// 而且损耗的是「用过的那几个」，看起来毫无规律。
///
/// 「当前」的判据与配额下发用的是同一个（`quota::service::latest_settled`）：
/// 按 `started_at_unix_ms` 取最新的那个已批准 runtime。两处必须一致，
/// 否则会出现「能领到、但配额表里没有它」这种对不上的状态。
///
/// # 为什么数的是 `DISTINCT node_id` 而不是行数
///
/// 「每个在册节点上都有一行」才是这里要表达的意思，而 `count(*) = 节点数` 只是
/// 在「一个节点一个名字恰好一行」成立时碰巧等价于它。那个前提由
/// `identity_routes` 的唯一键给出——于是这条查询**默默依赖了唯一键里有几列**。
///
/// 依赖得不值：它的失败形态是**名字永久挑不中，而且零报错**。
/// 唯一键一旦多一列（比如把 inbound_tag 加回去），一个名字在一个节点上就有两行，
/// `count(*)` 变成 `2 × 节点数`，`HAVING` 永远不成立，池子看起来是空的——
/// 没有异常、没有日志、没有任何东西会红。
///
/// 数 `DISTINCT node_id` 让这条查询说的就是它想说的那件事，与键的形状无关。
/// 空闲判据同理：数的是**有空闲槽位的节点数**，不是空闲行数。
async fn pick_free_slot(
    txn: &mut crate::store::WriteTxn<'_>,
    node_count: i64,
) -> Result<Option<Candidate>> {
    let row = sqlx::query(
        "SELECT r.identity_name \
           FROM identity_routes r \
           JOIN nodes n ON n.node_id = r.node_id \
                       AND n.status = 'active' \
          GROUP BY r.identity_name \
         HAVING count(DISTINCT r.node_id) = ? \
            AND count(DISTINCT CASE WHEN r.state = 'free' THEN r.node_id END) = ? \
            AND NOT EXISTS ( \
                  SELECT 1 FROM counter_cursors c \
                    JOIN runtime_identities i ON i.runtime_identity_id = c.runtime_identity_id \
                    JOIN node_runtimes nr ON nr.runtime_pk = i.runtime_pk \
                   WHERE nr.approval_status = 'approved' \
                     AND nr.started_at_unix_ms = ( \
                           SELECT MAX(started_at_unix_ms) FROM node_runtimes \
                            WHERE node_id = nr.node_id AND approval_status = 'approved') \
                     AND i.identity_name = r.identity_name \
                     AND (c.tcp_uplink_bytes <> ? OR c.tcp_downlink_bytes <> ? \
                       OR c.udp_uplink_bytes <> ? OR c.udp_downlink_bytes <> ?)) \
          ORDER BY r.identity_name LIMIT 1",
    )
    .bind(node_count)
    .bind(node_count)
    .bind(ZERO_U64)
    .bind(ZERO_U64)
    .bind(ZERO_U64)
    .bind(ZERO_U64)
    .fetch_optional(txn.conn())
    .await?;
    let Some(row) = row else { return Ok(None) };
    let identity_name: String = row.get(0);

    // 记下证明零基线的那份快照，让这个敞口事后可核而不只是被断言。
    let fence = sqlx::query(
        "SELECT nr.runtime_id, nr.last_sequence FROM runtime_identities i \
           JOIN node_runtimes nr ON nr.runtime_pk = i.runtime_pk \
          WHERE i.identity_name = ? AND nr.approval_status = 'approved' \
          ORDER BY nr.started_at_unix_ms DESC LIMIT 1",
    )
    .bind(&identity_name)
    .fetch_optional(txn.conn())
    .await?;
    Ok(Some(Candidate {
        identity_name,
        fence_runtime_id: fence.as_ref().map(|row| row.get(0)),
        fence_sequence: fence.as_ref().and_then(|row| row.get(1)),
    }))
}

/// 退役一个身份。**永久**：它不会回到空闲池。
///
/// 两种用法，形状相同：
///
/// * **用过之后退役**——归属**不解绑**。撤权是额度置零而不是删除凭据
///   （定案第二条），退役后到达的字节确实是那个人产生的；置空 user_id
///   会把真实尾账变成无主字节，并让那个人的周期累计悄悄停止增长。
/// * **从未认领就退役**——把一个名字永久挡在池子外面。部署侧的测试身份
///   （`deploy-test`）、原型控制器的测试账号（`user0001`）都在节点的 inbound 里，
///   但它们的凭据在别处流通，发给真人等于两个人共用一份凭据。
///   在这条路存在之前，排除它们只能靠「计数非零」这个**巧合**，
///   而一次进程重启就会让计数归零，它们悄悄变回可领取。
pub async fn retire(
    store: &Store,
    identity_name: &str,
    actor: &str,
    reason: &str,
) -> Result<usize> {
    if actor.trim().is_empty() {
        return Err(Error::Quota("退役必须留下操作者".into()));
    }
    let now = bucket::to_rfc3339(OffsetDateTime::now_utc());
    let mut txn = store.begin_immediate().await?;

    let routes: Vec<(i64, Option<i64>)> = sqlx::query(
        "SELECT route_id, user_id FROM identity_routes \
          WHERE identity_name = ? AND state <> 'retired'",
    )
    .bind(identity_name)
    .fetch_all(txn.conn())
    .await?
    .into_iter()
    .map(|row| (row.get(0), row.get(1)))
    .collect();
    if routes.is_empty() {
        return Err(Error::Quota(format!("没有可退役的槽位：{identity_name}")));
    }

    for (route_id, user_id) in &routes {
        // 从未认领的槽位**也能退役**，而且这正是主要用途之一：
        // 把一个「永远不该发给人」的名字永久挡在池子外面。
        // schema 的 CHECK 允许 `retired` 配 NULL 归属——user_id 与 claimed_at
        // 同进同出，所以「用过之后退役」与「从未认领就退役」两种形状都自洽。
        sqlx::query(
            "UPDATE identity_routes SET state = 'retired', retired_at = ? WHERE route_id = ?",
        )
        .bind(&now)
        .bind(route_id)
        .execute(txn.conn())
        .await?;
        // 关掉持有区间，并追加一条 unassigned。
        sqlx::query(
            "UPDATE identity_assignment_events SET effective_to = ? \
              WHERE route_id = ? AND effective_to IS NULL",
        )
        .bind(&now)
        .bind(route_id)
        .execute(txn.conn())
        .await?;
        sqlx::query(
            "INSERT INTO identity_assignment_events(route_id, user_id, state, \
             effective_from, recorded_at, reason) VALUES (?, ?, 'unassigned', ?, ?, ?)",
        )
        .bind(route_id)
        .bind(user_id)
        .bind(&now)
        .bind(&now)
        .bind(reason)
        .execute(txn.conn())
        .await?;
    }

    txn.commit().await?;
    tracing::warn!(identity = %identity_name, actor, reason, "身份已退役，永不复用");
    Ok(routes.len())
}

/// 每个节点上还剩多少空闲槽位。池子只减不增，这个数要有告警线。
pub async fn free_slots(store: &Store) -> Result<Vec<(String, i64)>> {
    let rows = sqlx::query(
        "SELECT n.node_id, count(r.route_id) FROM nodes n \
           LEFT JOIN identity_routes r ON r.node_id = n.node_id AND r.state = 'free' \
          WHERE n.status = 'active' \
          GROUP BY n.node_id ORDER BY n.node_id",
    )
    .fetch_all(store.readers())
    .await?;
    Ok(rows.into_iter().map(|row| (row.get(0), row.get(1))).collect())
}
