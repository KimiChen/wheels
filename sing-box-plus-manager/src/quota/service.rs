//! 配额下发循环：把 M2 的计算挂在采集循环**后面**（C13）。
//!
//! 这个模块**包裹** [`NodeCollector`] 而不是改它。`collect::scheduler` 的模块注释
//! 明说它不该知道配额的存在——顺序因此由结构保证，而不是靠谁记得先调哪一个：
//! 想推配额就必须先持有一个 collector，也就必须先走完「采集 → 入账」。
//!
//! 一轮的形状：
//!
//! ```text
//! tick()          采集 + 结算（可能一次排空多份积压）
//!   ↓
//! 取当前已批准 runtime 与它最新一份 applied 批次
//!   ↓
//! 新鲜度门禁：这个 sequence 是不是「上次正额度请求之后」新采的、且还没被占用
//!   ↓
//! plan_round      按档位算池、镜像到本节点、组装全量表
//!   ↓
//! converge_node   持久化 epoch → PUT → 记录结果
//! ```
//!
//! **节奏跟随采集周期。** 每次采集恰好产生一个已结算 sequence，每次正额度推送
//! 恰好消耗一个，1:1 天然满足 `sequence_available`，不需要另设一个下发定时器。
//!
//! **不做「表没变就不推」的优化。** diff 逻辑是 bug 工厂，而 C16 对省略条目的
//! 惩罚是静默无限：算错一次 diff 的代价远大于每轮多发一份 301 条的 JSON。

use std::sync::Arc;

use sqlx::Row;
use time::OffsetDateTime;
use tokio::sync::watch;

use crate::collect::scheduler::NodeCollector;
use crate::collect::snapshot;
use crate::config::NodeConfig;
use crate::error::Result;
use crate::ledger::bucket;
use crate::quota::allocate::AllocationConfig;
use crate::quota::converge::{self, ConvergeOutcome, NodeContext};
use crate::quota::dispatch::{self, ConflictResolution};
use crate::store::codec::U64Text;
use crate::store::Store;

/// 一轮下发的结果。只用于日志与测试断言——**它不参与退避**。
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum QuotaRound {
    /// 该节点没开配额控制。这也是切换时的第一道闩。
    Disabled,
    /// 节点级停推闩已合上（`NodeMismatch`）。要人来处理。
    Halted,
    /// 还没有已批准 runtime 的已结算批次，无从下发。
    NoSettledRuntime,
    /// 拿不到新鲜且未被占用的 sequence：**不推**，节点保留上次授予的余额。
    NotFresh { reason: &'static str },
    /// 按 `quota.safe_zero_on_stale = true` 下发了全零安全表（C33 的原行为）。
    SafeZero,
    /// 完整表已下发并被节点接受。
    Applied { entries: usize },
    /// 409，已按 §4.5 三步判别并记录。
    Conflict { resolution: ConflictResolution },
    /// 设置在规划与发送之间变了，任务留在 pending 等下一轮重算。
    Stale,
    /// 下发失败（含 400 与不可判定的 5xx）。任务已记 failed/unknown。
    Failed { detail: String },
}

pub struct QuotaService {
    collector: NodeCollector,
    /// `quota_control.enabled`。为 false 时整条下发路径不武装。
    enabled: bool,
    /// 正额度请求发送前的快照新鲜度上限（§4.5 下发纪律 3）。
    snapshot_max_age_secs: i64,
    /// 镜像分配用不到它的 floor/probe，但 `plan_round` 的签名保留了这个参数，
    /// 换回加权分配时不必再动调用方。
    allocation: AllocationConfig,
    /// 见 [`crate::config::QuotaConfig::safe_zero_on_stale`]。
    safe_zero_on_stale: bool,
    /// **节点级停推闩**。§4.5 第 1 步说的是「停止推送并告警」，不是「继续往下分支」。
    ///
    /// 合上之后本进程内这个节点不再推任何表，直到有人查清连接目标为什么不对。
    /// 它刻意只活在进程里：重启是一次人为动作，那时人应该已经看过告警了。
    halted: bool,
}

impl QuotaService {
    /// 生产路径：三个开关全部来自配置。
    pub fn from_config(
        collector: NodeCollector,
        node: &NodeConfig,
        quota: &crate::config::QuotaConfig,
        snapshot_max_age_secs: u32,
    ) -> Self {
        QuotaService::new(
            collector,
            node.quota_control.as_ref().is_some_and(|q| q.enabled),
            snapshot_max_age_secs as i64,
            quota.safe_zero_on_stale,
        )
    }

    /// 显式构造。用例用它直接给定开关，不必拼一份完整的 `NodeConfig`。
    pub fn new(
        collector: NodeCollector,
        enabled: bool,
        snapshot_max_age_secs: i64,
        safe_zero_on_stale: bool,
    ) -> Self {
        QuotaService {
            collector,
            enabled,
            snapshot_max_age_secs,
            allocation: AllocationConfig::default(),
            safe_zero_on_stale,
            halted: false,
        }
    }

    pub fn node_id(&self) -> &str {
        self.collector.node_id()
    }

    /// 节点级停推闩是否合上。
    pub fn halted(&self) -> bool {
        self.halted
    }

    /// 定时循环。
    pub async fn run(mut self, store: Arc<Store>, mut shutdown: watch::Receiver<bool>) {
        // **启动前置**：把上次进程崩溃时停在 `prepared` 的请求按 unknown 收口。
        // 不做这一步，那些请求会永远停在 `prepared`，而 `recover_prepared` 的
        // 存在理由正是「响应丢失不能证明旧请求没执行」。
        if let Err(error) = dispatch::recover_prepared(&store, self.node_id()).await {
            tracing::error!(node_id = self.node_id(), %error, "恢复未确认请求失败");
        }

        let policy = self.collector.policy().clone();
        let mut backoff = policy.target;
        loop {
            let outcome = self.collector.tick(&store).await;
            outcome.log(self.collector.node_id());

            // 配额挂在入账后面。它的结果**不喂退避**：采集是更重要的一半，
            // 配额端点抖动不该把计量的节奏也拖慢。
            let round = self.dispatch_round(&store).await;
            self.log_round(&round);

            let wait = policy.next_wait(&outcome, &mut backoff);
            tokio::select! {
                _ = tokio::time::sleep(wait) => {}
                _ = shutdown.changed() => {
                    if *shutdown.borrow() {
                        tracing::info!(node_id = self.node_id(), "收到停止信号，退出下发循环");
                        return;
                    }
                }
            }
        }
    }

    /// 入账之后的那一段。单独开出来便于测试直接驱动一轮。
    pub async fn dispatch_round(&mut self, store: &Store) -> QuotaRound {
        if !self.enabled {
            return QuotaRound::Disabled;
        }
        if self.halted {
            return QuotaRound::Halted;
        }
        match self.try_dispatch(store).await {
            Ok(round) => round,
            Err(error) => QuotaRound::Failed { detail: error.to_string() },
        }
    }

    async fn try_dispatch(&mut self, store: &Store) -> Result<QuotaRound> {
        let node_id = self.collector.node_id().to_string();
        let Some(settled) = latest_settled(store, &node_id).await? else {
            return Ok(QuotaRound::NoSettledRuntime);
        };

        // 新鲜度是两个独立条件，缺一不可：
        //  - `snapshot_is_fresh`：既没超龄，又**严格晚于**上一份正额度请求占用的序号；
        //  - `sequence_available`：这个序号还没被任何一份正额度表占用过。
        // 前者挡的是「反复刷新同一份余额」，后者挡的是同号重复占用。
        let fresh = dispatch::snapshot_is_fresh(
            store,
            &node_id,
            &settled.runtime_id,
            settled.sequence,
            settled.accounting_at,
            self.snapshot_max_age_secs,
        )
        .await?
            && dispatch::sequence_available(store, &node_id, &settled.runtime_id, settled.sequence)
                .await?;

        if !fresh {
            return self.on_not_fresh(store, &node_id, &settled).await;
        }

        let context = NodeContext {
            node_id: node_id.clone(),
            runtime_id: settled.runtime_id.clone(),
            // 镜像分配不看权重。留 1 是为了换回加权分配时这里有个合法的起点。
            weight: 1,
            fresh_sequence: Some(settled.sequence),
        };

        // 单节点规划在镜像分配下与整轮规划等价：池取自 `usage_cycle_totals`，
        // 那本来就是该用户**跨全部节点**的周期用量；镜像又不做跨节点拆分，
        // 所以另外三个节点在不在这一轮里，算出来的数一模一样。
        // 换回加权分配时这条等价性立刻失效，那时要把四个节点一起规划。
        let cycle = crate::quota::cycle_key(OffsetDateTime::now_utc());
        let plan =
            converge::plan_round(store, std::slice::from_ref(&context), &self.allocation, &cycle)
                .await?;
        let entries = plan.tables.get(&node_id).map(|t| t.len()).unwrap_or(0);

        let outcome =
            converge::converge_node(store, self.collector.client(), &context, &plan).await?;
        converge::record_task(store, plan.revision, &node_id, &outcome).await?;

        Ok(match outcome {
            ConvergeOutcome::Applied { .. } => QuotaRound::Applied { entries },
            ConvergeOutcome::SafeZero { .. } => QuotaRound::SafeZero,
            ConvergeOutcome::Stale { .. } => QuotaRound::Stale,
            ConvergeOutcome::Unknown { .. } => {
                QuotaRound::Failed { detail: "下发结果不确定，下一轮重试".into() }
            }
            ConvergeOutcome::Failed { detail } => QuotaRound::Failed { detail },
            ConvergeOutcome::Conflict { prepared } => {
                let resolution = self.resolve(store, &prepared).await?;
                QuotaRound::Conflict { resolution }
            }
        })
    }

    /// 拿不到新鲜序列时怎么办。**默认什么都不做**——理由见
    /// [`crate::config::QuotaConfig::safe_zero_on_stale`]。
    async fn on_not_fresh(
        &self,
        store: &Store,
        node_id: &str,
        settled: &Settled,
    ) -> Result<QuotaRound> {
        // 最后一次成功下发已经超龄：节点上那份余额是旧的，要让人知道。
        // 这是偏离 C33 之后**唯一**的补偿手段，所以它必须响。
        if let Some(age) = last_applied_age(store, node_id, &settled.runtime_id).await? {
            if age > self.snapshot_max_age_secs * 4 {
                tracing::error!(
                    node_id,
                    age_secs = age,
                    "配额已超龄未刷新（P1）：节点仍按上一次授予的余额放行"
                );
            }
        }
        if !self.safe_zero_on_stale {
            return Ok(QuotaRound::NotFresh {
                reason: "没有新鲜且未被占用的已结算 sequence"
            });
        }
        let outcome = converge::converge_safe_zero(
            store,
            self.collector.client(),
            node_id,
            &settled.runtime_id,
        )
        .await?;
        Ok(match outcome {
            ConvergeOutcome::SafeZero { .. } => QuotaRound::SafeZero,
            ConvergeOutcome::Failed { detail } => QuotaRound::Failed { detail },
            _ => QuotaRound::Failed { detail: "零表收敛未确认".into() },
        })
    }

    /// 409 的三步判别。取一份快照读回 envelope 才能分支——
    /// **不从响应体猜原因**，节点对两种成因返回同一个错误体。
    async fn resolve(
        &mut self,
        store: &Store,
        prepared: &dispatch::Prepared,
    ) -> Result<ConflictResolution> {
        let observed = match self.collector.client().snapshot().await {
            Ok(response) if response.upstream_ok() => snapshot::parse_envelope(&response.body),
            _ => None,
        };
        let resolution = converge::resolve_and_record(
            store,
            prepared,
            observed.as_ref().map(|e| (e.node_id.as_str(), e.runtime_id.as_str())),
        )
        .await?;

        if let ConflictResolution::NodeMismatch { .. } = resolution {
            // §4.5 第 1 步：**停止推送**。合上闩，不再往下分支。
            // 连接目标不对意味着我们可能正在给别的节点写表，继续推是在放大伤害。
            self.halted = true;
            tracing::error!(node_id = self.node_id(), "已合上节点级停推闩，需人工处理（P1）");
        }
        // RuntimeChanged 只告警，**不自动批准**：批准是人的动作（C8）。
        // StaleEpoch 无需动作，下一轮 `prepare` 会从库里的最大值 +1 取。
        // Undetermined 下一轮重试。
        Ok(resolution)
    }

    fn log_round(&self, round: &QuotaRound) {
        let node_id = self.node_id();
        match round {
            QuotaRound::Disabled | QuotaRound::NoSettledRuntime => {}
            QuotaRound::Halted => {
                tracing::error!(node_id, "节点级停推闩合上中，本轮不下发（P1）");
            }
            QuotaRound::NotFresh { reason } => tracing::debug!(node_id, reason, "本轮不下发"),
            QuotaRound::SafeZero => tracing::warn!(node_id, "下发了全零安全表"),
            QuotaRound::Applied { entries } => {
                tracing::info!(node_id, entries, "配额表已生效");
            }
            QuotaRound::Conflict { resolution } => {
                tracing::warn!(node_id, ?resolution, "409 已判别");
            }
            QuotaRound::Stale => tracing::debug!(node_id, "设置已变，下一轮重算"),
            QuotaRound::Failed { detail } => tracing::error!(node_id, detail, "下发失败"),
        }
    }
}

/// 该节点**当前已批准 runtime** 上最新一份 `applied` 批次。
struct Settled {
    runtime_id: String,
    sequence: u64,
    accounting_at: OffsetDateTime,
}

/// 取「已批准 runtime 上最新的已入账批次」。
///
/// 按 `started_at_unix_ms` 取最新的那个已批准 runtime，而不是按批次时间——
/// 一个刚重启的节点可能同时存在旧 runtime 的积压批次与新 runtime 的新批次，
/// 而额度必须推给**节点现在正在跑的那个** runtime，否则必然 409。
async fn latest_settled(store: &Store, node_id: &str) -> Result<Option<Settled>> {
    let row = sqlx::query(
        "SELECT b.runtime_id, b.sequence, b.accounting_at \
         FROM snapshot_batches b \
         JOIN node_runtimes r ON r.runtime_pk = b.runtime_pk \
         WHERE b.node_id = ? AND b.status = 'applied' AND r.approval_status = 'approved' \
           AND r.started_at_unix_ms = ( \
             SELECT MAX(started_at_unix_ms) FROM node_runtimes \
             WHERE node_id = ? AND approval_status = 'approved') \
         ORDER BY b.sequence DESC LIMIT 1",
    )
    .bind(node_id)
    .bind(node_id)
    .fetch_optional(store.readers())
    .await?;
    let Some(row) = row else { return Ok(None) };
    let accounting_at = bucket::parse_rfc3339(&row.get::<String, _>(2))
        .ok_or_else(|| crate::error::Error::Quota("accounting_at 不是合法 RFC3339".into()))?;
    Ok(Some(Settled {
        runtime_id: row.get(0),
        sequence: U64Text::decode(&row.get::<String, _>(1))?.get(),
        accounting_at,
    }))
}

/// 距上一次**成功下发的正额度表**过了多少秒。没有成功过则返回 `None`。
async fn last_applied_age(store: &Store, node_id: &str, runtime_id: &str) -> Result<Option<i64>> {
    let row = sqlx::query(
        "SELECT MAX(settled_at) FROM quota_requests \
         WHERE node_id = ? AND runtime_id = ? AND table_kind = 'positive' AND result = 'applied'",
    )
    .bind(node_id)
    .bind(runtime_id)
    .fetch_one(store.readers())
    .await?;
    Ok(row
        .get::<Option<String>, _>(0)
        .and_then(|text| bucket::parse_rfc3339(&text))
        .map(|at| (OffsetDateTime::now_utc() - at).whole_seconds()))
}
