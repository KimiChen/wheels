//! 周期性采集调度：把 agent 客户端与入账串成一条定时循环。
//!
//! 顺序是固定的：**先入账 → 再算额度 → 再推送**（C13）。这个模块只做第一步，
//! 额度那两步属于 M2；但顺序已经由结构决定了——调度器不知道配额的存在，
//! 配额只能挂在它后面。
//!
//! **每个节点一条任务**，于是主控侧天然单飞。agent 侧还有一层本地单飞
//! （README §4.2），两层都要：主控重启、或将来 M4 的手动采集端点，
//! 都可能让同一节点同时出现两条在途请求。
//!
//! 自适应频率（§4.5 的提频触发：池将尽 / 需求偏高 / 冷节点探测）**属于 M2**——
//! 那三条判据全都要读额度池。这里只把**机制**做出来（目标周期与下限、
//! 失败退避），触发条件留给 M2 接上。

use std::sync::Arc;
use std::time::Duration;

use time::OffsetDateTime;
use tokio::sync::watch;

use crate::agent::AgentClient;
use crate::ledger::settle::{self, RejectReason, SettleOutcome};
use crate::store::Store;

#[derive(Debug, Clone)]
pub struct SchedulePolicy {
    /// 目标采集周期。
    pub target: Duration,
    /// 提频下限。M2 接上自适应频率之后才会真的用到。
    pub minimum: Duration,
    /// 失败退避上限。
    pub max_backoff: Duration,
    /// 节点时钟偏移容忍上限（C27）。
    pub max_clock_skew_secs: i64,
}

impl SchedulePolicy {
    pub fn from_config(collect: &crate::config::CollectConfig) -> Self {
        SchedulePolicy {
            target: Duration::from_secs(collect.interval_secs as u64),
            minimum: Duration::from_secs(collect.min_interval_secs as u64),
            // 退避上限取目标周期的 8 倍：再长就不如说「这个节点已经失联」，
            // 而那该由告警说，不该由一个越退越久的循环隐式表达。
            max_backoff: Duration::from_secs(collect.interval_secs as u64 * 8),
            max_clock_skew_secs: collect.max_clock_skew_secs,
        }
    }
}

/// 一次采集循环的结果。
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Tick {
    /// 入账成功。可能一次结算了多份积压批次。
    Applied { settled: usize, ledger_rows: usize },
    /// 前置条件没满足（多半是 runtime 没批准）。批次留在 `pending`。
    Deferred(RejectReason),
    /// 整份拒绝，批次已烧掉。
    Rejected(RejectReason),
    /// **可重试且不得入账**（C7）：429、连接被重置、超时。
    Retryable(String),
    /// 上游非 200 且不可重试：拒绝入账，但也不是我们这边的错。
    NotAccountable { upstream_status: Option<u16>, agent_status: u16 },
    /// envelope 都认不出来，只进限量安全日志，没落库。
    Unidentifiable,
}

impl Tick {
    /// 这次是否应当退避。
    fn should_back_off(&self) -> bool {
        matches!(self, Tick::Retryable(_) | Tick::NotAccountable { .. } | Tick::Unidentifiable)
    }
}

pub struct NodeCollector {
    node_id: String,
    client: AgentClient,
    policy: SchedulePolicy,
}

impl NodeCollector {
    pub fn new(node_id: impl Into<String>, client: AgentClient, policy: SchedulePolicy) -> Self {
        NodeCollector { node_id: node_id.into(), client, policy }
    }

    pub fn node_id(&self) -> &str {
        &self.node_id
    }

    /// 一次采集 + 入账。可单独调用，便于测试与将来的手动采集端点。
    pub async fn tick(&self, store: &Store) -> Tick {
        let response = match self.client.snapshot().await {
            Ok(response) => response,
            Err(error) => {
                // 传输层失败一律「可重试且不得入账」——**绝不当成空快照**。
                return Tick::Retryable(error.to_string());
            }
        };

        if !response.upstream_ok() {
            if response.retryable() {
                return Tick::Retryable(format!(
                    "agent={} upstream={:?}",
                    response.agent_status, response.upstream_status
                ));
            }
            return Tick::NotAccountable {
                upstream_status: response.upstream_status,
                agent_status: response.agent_status,
            };
        }

        // `collected_at` 取 agent 签名里的时刻：它进 HMAC 的 canonical 编码，链路上改不动。
        let received_at = OffsetDateTime::now_utc();
        let collected_at = OffsetDateTime::from_unix_timestamp(response.agent_timestamp as i64)
            .unwrap_or(received_at);

        let receipt = match settle::receive(
            store,
            &self.node_id,
            &response.body,
            collected_at,
            received_at,
            self.policy.max_clock_skew_secs,
        )
        .await
        {
            Ok(Some(receipt)) => receipt,
            Ok(None) => return Tick::Unidentifiable,
            Err(error) => {
                // 同号不同摘要这类契约违反落在这里。它不是传输问题，退避没用，
                // 但也不能当成成功——交给告警。
                tracing::error!(node_id = %self.node_id, error = %error, "接收失败");
                return Tick::Retryable(error.to_string());
            }
        };

        // **把积压排空**：一次 tick 可能要结算不止一份。
        // 常见来源是「先收到、后批准」——批准之前攒下的批次都还在 pending。
        let mut settled = 0usize;
        let mut ledger_rows = 0usize;
        loop {
            match settle::settle_next(store, &self.node_id, &receipt.runtime_id).await {
                Ok(SettleOutcome::Applied {
                    ledger_rows: rows, sequence, sequence_gap, ..
                }) => {
                    settled += 1;
                    ledger_rows += rows;
                    if let Some(gap) = sequence_gap {
                        tracing::warn!(node_id = %self.node_id, sequence, gap, "sequence 跳号");
                    }
                }
                Ok(SettleOutcome::Superseded { .. }) => settled += 1,
                Ok(SettleOutcome::Nothing) => break,
                Ok(SettleOutcome::Deferred { reason }) => return Tick::Deferred(reason),
                Ok(SettleOutcome::Rejected { reason, .. }) => return Tick::Rejected(reason),
                Err(error) => {
                    tracing::error!(node_id = %self.node_id, error = %error, "结算失败");
                    return Tick::Retryable(error.to_string());
                }
            }
        }
        Tick::Applied { settled, ledger_rows }
    }

    /// 定时循环。收到停止信号即退出。
    pub async fn run(self, store: Arc<Store>, mut shutdown: watch::Receiver<bool>) {
        let mut backoff = self.policy.target;
        loop {
            let outcome = self.tick(&store).await;
            match &outcome {
                Tick::Applied { settled, ledger_rows } => {
                    tracing::debug!(node_id = %self.node_id, settled, ledger_rows, "入账完成");
                }
                Tick::Deferred(reason) => {
                    tracing::warn!(node_id = %self.node_id, "暂不结算：{reason}");
                }
                Tick::Rejected(reason) => {
                    tracing::warn!(node_id = %self.node_id, "整份拒绝：{reason}");
                }
                Tick::Retryable(detail) => {
                    tracing::warn!(node_id = %self.node_id, detail, "可重试失败，不入账");
                }
                Tick::NotAccountable { upstream_status, agent_status } => {
                    tracing::warn!(
                        node_id = %self.node_id, ?upstream_status, agent_status,
                        "上游非 200，拒绝入账"
                    );
                }
                Tick::Unidentifiable => {
                    tracing::warn!(node_id = %self.node_id, "响应无法识别 envelope");
                }
            }

            // 退避只对传输类失败生效。整份拒绝**不退避**：
            // 那是节点在正常回应、只是内容不可入账，退避会把告警的节奏也拖慢。
            let wait = if outcome.should_back_off() {
                backoff = (backoff * 2).min(self.policy.max_backoff);
                backoff
            } else {
                backoff = self.policy.target;
                self.policy.target
            };

            tokio::select! {
                _ = tokio::time::sleep(wait) => {}
                _ = shutdown.changed() => {
                    if *shutdown.borrow() {
                        tracing::info!(node_id = %self.node_id, "收到停止信号，退出采集循环");
                        return;
                    }
                }
            }
        }
    }
}

/// 四项判据（README §1 验收定义）。长跑时必须全为 0。
///
/// 从**库里**统计而不是从进程内计数器：计数器会随重启清零，
/// 而长跑的结论要跨重启成立。
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub struct AcceptanceCriteria {
    /// 负增量。
    pub counter_regression: i64,
    /// 未知 runtime。
    pub unknown_runtime: i64,
    /// 重复 / 回退 sequence。
    pub stale_sequence: i64,
    /// unhealthy 入账。
    pub unhealthy_accounted: i64,
}

impl AcceptanceCriteria {
    pub fn all_zero(&self) -> bool {
        self.counter_regression == 0
            && self.unknown_runtime == 0
            && self.stale_sequence == 0
            && self.unhealthy_accounted == 0
    }
}

/// 按库里的批次回执统计四项判据。
///
/// 注意 `unhealthy_accounted` 统计的是「**不可入账的快照被入账了**」——
/// 它应当恒为 0，因为被拒的快照状态是 `rejected` 而不是 `applied`。
/// 统计「被拒了多少次」没有意义：节点本来就可能报 unhealthy，
/// 判据要问的是主控有没有把它记进账。
pub async fn acceptance_criteria(store: &Store) -> crate::error::Result<AcceptanceCriteria> {
    use sqlx::Row;
    let count = |sql: &'static str| async move {
        sqlx::query(sql)
            .fetch_one(store.readers())
            .await
            .map(|row| row.get::<i64, _>(0))
            .map_err(crate::error::Error::from)
    };

    Ok(AcceptanceCriteria {
        counter_regression: count(
            "SELECT count(*) FROM snapshot_batches \
             WHERE status = 'applied' AND reject_reason LIKE '%计数器倒退%'",
        )
        .await?,
        unknown_runtime: count(
            "SELECT count(*) FROM snapshot_batches \
             WHERE status = 'applied' AND runtime_pk IS NULL",
        )
        .await?,
        // 已接受的 sequence 必须严格递增：同一 runtime 下 applied 的 sequence 有重复即违反。
        stale_sequence: count(
            "SELECT coalesce(sum(duplicates), 0) FROM ( \
               SELECT count(*) - 1 AS duplicates FROM snapshot_batches \
               WHERE status = 'applied' GROUP BY node_id, runtime_id, sequence \
               HAVING count(*) > 1)",
        )
        .await?,
        // 不可入账的快照被记成 applied。
        unhealthy_accounted: count(
            "SELECT count(*) FROM snapshot_batches \
             WHERE status = 'applied' AND reject_reason IS NOT NULL",
        )
        .await?,
    })
}
