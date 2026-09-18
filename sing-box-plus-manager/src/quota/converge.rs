//! 按最新 revision 重算并收敛（D21、README §4.5）。
//!
//! 顺序：**先入账 → 计算所有受限用户预算 → 持久化请求与 epoch → 推送 → 记录结果**（C13）。
//! 这个模块从第二步开始；第一步（主动采集与结算）由调用方在进同一条单飞队列时完成。
//!
//! 「改完立即触发采集、结算与下发，不等下一个定时周期」——这里的义务是
//! **不复用旧快照**：正额度表必须绑定上次正额度请求之后重新采集并结算的 sequence。
//! 仅「快照还没过 TTL」不足以授权反复刷新同一份余额。

use std::collections::{BTreeMap, BTreeSet};

use sqlx::Row;

use crate::agent::AgentClient;
use crate::error::{Error, Result};
// 镜像分配不用 allocate::plan 与 NodeWeight；两者连同 weight 模块保留不接线，
// 换回加权分配时一并恢复。AllocationConfig 仍在签名里，所以调用方无需改动。
use crate::quota::allocate::AllocationConfig;
use crate::quota::dispatch::{self, ConflictResolution, PushOutcome};
use crate::quota::pool;
use crate::quota::settings::{self, TaskStatus};
use crate::quota::table;
use crate::quota::wire::Quota;
use crate::store::codec::U128Text;
use crate::store::Store;

/// 一个参与本轮收敛的节点。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct NodeContext {
    pub node_id: String,
    pub runtime_id: String,
    /// 正整数需求权重（[`crate::quota::weight`]）。
    pub weight: u32,
    /// 本节点**最新已结算且未被占用**的 sequence。
    ///
    /// `None` 表示拿不到新鲜的健康快照——那时**禁止正额度刷新**，
    /// 只能走 C33 的零表收敛。
    pub fresh_sequence: Option<u64>,
}

/// 一轮规划的产物。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RoundPlan {
    /// 规划时读到的设置 revision。发送前要再核一次——
    /// **过时任务重新计算，不得把旧配置的余额推回节点。**
    pub revision: i64,
    pub tables: BTreeMap<String, Vec<(String, String, Quota)>>,
}

/// 按最新设置规划各节点的完整表。
///
/// **先按用户/计费池计算目标，再按节点合并全部用户，形成一次全量请求**（§4.5）。
///
/// # 镜像分配
///
/// 每个可更新的节点都收到该用户本周期的**全额剩余**，不做跨节点拆分。
/// 这是一次有意的产品选择：额度的定位是观察每个用户的用量而不是强制封顶，
/// 而按权重拆分会让把流量压在单一节点的用户在 1/4 额度处被切断——
/// 那是没人要求的强制行为，而且在下一轮重新分配之前一直存在。
///
/// 代价是短期内最多按节点数倍超发。这在当前定位下可接受：控制器每轮都从账本
/// 重算 `剩余 = 额度 − 本周期已用`，一旦用尽所有节点同时归零，
/// 因此实际超出的量是一个采集周期的流量，而不是 N 倍的月度额度。
///
/// `allocate` / `weight` 两个模块及其测试保留但不接线。换回加权分配时，
/// 改动集中在下面这一个循环，外加恢复 `quota_allocations` 表。
pub async fn plan_round(
    store: &Store,
    nodes: &[NodeContext],
    _config: &AllocationConfig,
    cycle_key: &str,
) -> Result<RoundPlan> {
    let settings =
        settings::load(store).await?.ok_or_else(|| Error::Quota("还没有全局额度设置".into()))?;

    // 每个用户在哪些节点上有身份。
    let mut identities_by_node: BTreeMap<&str, Vec<table::NodeIdentity>> = BTreeMap::new();
    for node in nodes {
        let identities = table::current_identities(store, &node.node_id, &node.runtime_id).await?;
        identities_by_node.insert(node.node_id.as_str(), identities);
    }
    let mut nodes_of_user: BTreeMap<i64, BTreeSet<&str>> = BTreeMap::new();
    for (node_id, identities) in &identities_by_node {
        for identity in identities {
            if let Some(user_id) = identity.user_id {
                nodes_of_user.entry(user_id).or_default().insert(node_id);
            }
        }
    }

    // 每个用户的池。额度按**档位**取（定案第三条），不再有全局单值。
    let used = cycle_used_by_user(store, cycle_key).await?;
    let quotas = settings::user_quota_map(store).await?;
    // 每个节点上、每个用户拿到多少。
    let mut per_node_user: BTreeMap<&str, BTreeMap<i64, Quota>> =
        nodes.iter().map(|n| (n.node_id.as_str(), BTreeMap::new())).collect();

    for (user_id, user_nodes) in &nodes_of_user {
        // 查不到额度就是零，**不是跳过**。查不到只有两种可能：用户被禁用，
        // 或者他的档位不存在。两种都该被挡住，而跳过在 wire 上等于无限额度（C16）——
        // 从额度表里省略一个身份，节点会把它当成不受限。
        let budget = match quotas.get(user_id) {
            Some(quota) => {
                pool::user_pool(*quota, used.get(user_id).copied().unwrap_or(U128Text::new(0)))
            }
            None => 0,
        };
        // 镜像：每个**本轮可更新**的节点都拿到全额剩余，不拆分。
        // 拿不到新鲜快照的节点不参与正额度刷新，它上面的身份在下面被置零。
        //
        // 钳到 i64::MAX：wire 上 remaining_bytes 是非负 int64（C31）。
        // 库层已经把档位额度限在这个范围内，这里是第二道，防的是将来
        // 有人从别的路径塞进一个更大的池。
        let mirrored = Quota::Limited(budget.min(i64::MAX as u64));
        for node_id in user_nodes {
            let fresh = nodes.iter().any(|n| n.node_id == **node_id && n.fresh_sequence.is_some());
            if let Some(entry) = per_node_user.get_mut(node_id) {
                entry.insert(*user_id, if fresh { mirrored } else { Quota::Limited(0) });
            }
        }
    }

    let mut tables = BTreeMap::new();
    for node in nodes {
        let identities = identities_by_node.remove(node.node_id.as_str()).unwrap_or_default();
        let allocations = per_node_user.remove(node.node_id.as_str()).unwrap_or_default();
        tables.insert(node.node_id.clone(), table::assemble(&identities, &allocations)?);
    }

    Ok(RoundPlan { revision: settings.revision, tables })
}

async fn cycle_used_by_user(store: &Store, cycle_key: &str) -> Result<BTreeMap<i64, U128Text>> {
    let rows = sqlx::query(
        "SELECT user_id, total_bytes FROM usage_cycle_totals \
         WHERE cycle_key = ? AND rule_version = ?",
    )
    .bind(cycle_key)
    .bind(crate::ledger::settle::CYCLE_RULE_VERSION)
    .fetch_all(store.readers())
    .await?;
    rows.into_iter()
        .map(|row| Ok((row.get::<i64, _>(0), U128Text::decode(&row.get::<String, _>(1))?)))
        .collect()
}

/// 把一个节点收敛到计划里的那张表。
///
/// **发送前再次检查设置 revision**：过时就返回 [`ConvergeOutcome::Stale`]，
/// 由调用方重算。**不得把旧配置的余额推回节点。**
pub async fn converge_node(
    store: &Store,
    client: &AgentClient,
    context: &NodeContext,
    plan: &RoundPlan,
) -> Result<ConvergeOutcome> {
    let current =
        settings::load(store).await?.ok_or_else(|| Error::Quota("还没有全局额度设置".into()))?;
    if current.revision != plan.revision {
        return Ok(ConvergeOutcome::Stale { planned: plan.revision, current: current.revision });
    }

    let entries = plan
        .tables
        .get(&context.node_id)
        .cloned()
        .ok_or_else(|| Error::Quota(format!("计划里没有节点 {}", context.node_id)))?;
    let kind = dispatch::TableKind::of(&entries);
    // 正额度表必须绑定新鲜 sequence；零表不占用。
    let source = match kind {
        dispatch::TableKind::Positive => Some(context.fresh_sequence.ok_or_else(|| {
            Error::Quota("没有新鲜的已结算 sequence，禁止正额度刷新——只能走 C33 的零表收敛".into())
        })?),
        dispatch::TableKind::Zero => None,
    };

    let prepared = dispatch::prepare(
        store,
        &context.node_id,
        &context.runtime_id,
        entries,
        source,
        plan.revision,
        plan.revision,
    )
    .await?;
    let outcome = dispatch::send(client, &prepared).await;
    dispatch::record(store, &prepared, &outcome).await?;

    Ok(match (&outcome, kind) {
        // **只有按新配置计算的完整表收到 200 才是 applied。**
        (PushOutcome::Applied { .. }, dispatch::TableKind::Positive) => {
            ConvergeOutcome::Applied { request_id: prepared.request_id }
        }
        // 零表确认成功**不冒充新配置正常生效**。
        (PushOutcome::Applied { .. }, dispatch::TableKind::Zero) => {
            ConvergeOutcome::SafeZero { request_id: prepared.request_id }
        }
        (PushOutcome::Conflict, _) => ConvergeOutcome::Conflict { prepared },
        (PushOutcome::Unknown { .. } | PushOutcome::Retryable { .. }, _) => {
            ConvergeOutcome::Unknown { request_id: prepared.request_id }
        }
        (PushOutcome::UnknownIdentity { unknown }, _) => ConvergeOutcome::Failed {
            // C15：配置与全量清单不一致。**P1 告警**，核对当前配置与有效身份后重建整表。
            detail: format!("节点不认识这些身份（P1）：{unknown:?}"),
        },
        (PushOutcome::BadRequest, _) => {
            ConvergeOutcome::Failed { detail: "节点返回 400".into() }
        }
    })
}

#[derive(Debug)]
pub enum ConvergeOutcome {
    Applied {
        request_id: i64,
    },
    /// 下发了全零安全表并确认成功（C33）。**不等于新配置已生效。**
    SafeZero {
        request_id: i64,
    },
    /// 409。调用方要取快照读回 `runtime_id` 才能分支。
    Conflict {
        prepared: dispatch::Prepared,
    },
    Unknown {
        request_id: i64,
    },
    Failed {
        detail: String,
    },
    /// 设置在规划与发送之间变了。**重算，不要把旧配置推出去。**
    Stale {
        planned: i64,
        current: i64,
    },
}

impl ConvergeOutcome {
    pub fn task_status(&self) -> TaskStatus {
        match self {
            ConvergeOutcome::Applied { .. } => TaskStatus::Applied,
            ConvergeOutcome::SafeZero { .. } => TaskStatus::SafeZero,
            ConvergeOutcome::Unknown { .. } | ConvergeOutcome::Conflict { .. } => {
                TaskStatus::Unknown
            }
            ConvergeOutcome::Failed { .. } => TaskStatus::Failed,
            // 过时的任务留在 pending，等重算。
            ConvergeOutcome::Stale { .. } => TaskStatus::Pending,
        }
    }

    pub fn detail(&self) -> Option<String> {
        match self {
            ConvergeOutcome::Failed { detail } => Some(detail.clone()),
            ConvergeOutcome::Stale { planned, current } => {
                Some(format!("规划用的 revision {planned} 已被 {current} 取代，重算"))
            }
            ConvergeOutcome::Conflict { .. } => Some("409：需取快照核对 runtime".into()),
            _ => None,
        }
    }
}

/// C33 的肯定动作：**主动下发一份全零完整节点表**。
///
/// 无新鲜、已结算的健康快照时不得新增或刷新正额度；但通道可用就要把该节点
/// **全部受限身份置零**并拿到 200。不作为会让节点带着旧的正额度一直跑到自己耗尽——
/// 两者在观测上完全不同。
pub async fn converge_safe_zero(
    store: &Store,
    client: &AgentClient,
    node_id: &str,
    runtime_id: &str,
) -> Result<ConvergeOutcome> {
    let identities = table::current_identities(store, node_id, runtime_id).await?;
    if identities.is_empty() {
        return Ok(ConvergeOutcome::Failed {
            detail: "该 runtime 上还没有已结算的身份，无法构造全零完整表".into(),
        });
    }
    let entries = table::zero_table(&identities);
    let prepared = dispatch::prepare(store, node_id, runtime_id, entries, None, 0, 0).await?;
    let outcome = dispatch::send(client, &prepared).await;
    dispatch::record(store, &prepared, &outcome).await?;
    Ok(match outcome {
        PushOutcome::Applied { .. } => {
            ConvergeOutcome::SafeZero { request_id: prepared.request_id }
        }
        PushOutcome::Conflict => ConvergeOutcome::Conflict { prepared },
        PushOutcome::Unknown { .. } | PushOutcome::Retryable { .. } => {
            ConvergeOutcome::Unknown { request_id: prepared.request_id }
        }
        other => ConvergeOutcome::Failed { detail: format!("零表收敛失败：{other:?}") },
    })
}

/// 409 之后按 §4.5 的三步分支决定下一步。
pub async fn resolve_and_record(
    store: &Store,
    prepared: &dispatch::Prepared,
    observed: Option<(&str, &str)>,
) -> Result<ConflictResolution> {
    let resolution = dispatch::resolve_conflict(store, prepared, observed).await?;
    dispatch::record_conflict_cause(store, prepared.request_id, &resolution).await?;
    match &resolution {
        ConflictResolution::NodeMismatch { sent, observed } => {
            // 第 1 步：**先停止推送并告警**，不要继续往下分支。
            tracing::error!(sent, observed, "连接目标的 node_id 与预期不符：停止推送（P1）");
        }
        ConflictResolution::RuntimeChanged { observed_runtime } => {
            tracing::warn!(observed_runtime, "runtime 已变：登记批准后先下发零额度安全表");
        }
        ConflictResolution::StaleEpoch { next_epoch } => {
            tracing::warn!(next_epoch, "epoch 未前进：从最大已分配值取更大值重试");
        }
        ConflictResolution::Undetermined => {
            tracing::warn!("取不到快照，409 成因不确定：不得假设成任何一种");
        }
    }
    Ok(resolution)
}

/// 把一轮的结果写回逐节点任务。
pub async fn record_task(
    store: &Store,
    revision: i64,
    node_id: &str,
    outcome: &ConvergeOutcome,
) -> Result<()> {
    settings::set_task_status(
        store,
        revision,
        node_id,
        outcome.task_status(),
        outcome.detail().as_deref(),
    )
    .await
}
