//! 跨节点分配（README §4.5「权重、地板与冷节点恢复」）。
//!
//! ```text
//! 若 B >= N × floor：base_i = floor
//! 否则若 B >= N：    base_i = min(probe_bytes, floor(B / N))；标记「地板已失效」
//! 否则（0 < B < N）： 按持久化轮转游标给 B 个节点各 1 字节，其余为 0；标记「地板已失效」
//!
//! extra   = B − Σ base_i
//! share_i = floor(extra × w_i / Σ w_j)
//! 余数按乘除余数从大到小补 1，平局按 node_id 字节序
//! alloc_i = min(base_i + share_i + 余数分配, i64::MAX)
//! ```
//!
//! **先保留基础额度，再分剩余预算，最后不再归一化基础额度。**
//! 最后那句是关键：如果在末尾按总额归一化，地板就会被按比例缩掉，
//! 「用户漫游到冷节点别被立刻闸断」这个地板存在的唯一理由也就没了。
//!
//! 地板的存在理由只对**节点间**成立，所以身份级（D17 情形 3）**不设地板**——
//! 同一节点内的身份切换不是用户的自主行为。

use std::collections::BTreeMap;

use crate::error::{Error, Result};

/// `floor` 默认 64 MiB；`probe_bytes` 默认 1 MiB。
pub const DEFAULT_FLOOR_BYTES: u64 = 64 * 1024 * 1024;
pub const DEFAULT_PROBE_BYTES: u64 = 1024 * 1024;

/// wire 上限：`remaining_bytes` 是 int64（C31）。
pub const WIRE_MAX: u64 = i64::MAX as u64;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct AllocationConfig {
    pub floor: u64,
    pub probe: u64,
}

impl Default for AllocationConfig {
    fn default() -> Self {
        AllocationConfig { floor: DEFAULT_FLOOR_BYTES, probe: DEFAULT_PROBE_BYTES }
    }
}

impl AllocationConfig {
    /// 必须满足 `1 ≤ probe_bytes ≤ floor ≤ i64::MAX`。
    pub fn validate(&self) -> Result<()> {
        if self.probe < 1 {
            return Err(Error::Quota("probe_bytes 必须 ≥ 1".into()));
        }
        if self.probe > self.floor {
            return Err(Error::Quota(format!(
                "probe_bytes({}) 不得大于 floor({})：探测额度本来就是地板失效时的降级值",
                self.probe, self.floor
            )));
        }
        if self.floor > WIRE_MAX {
            return Err(Error::Quota(format!(
                "floor({}) 超出 wire 的 int64 上限 {WIRE_MAX}",
                self.floor
            )));
        }
        Ok(())
    }
}

/// 这一轮走的是哪条分支。**要落库**：事后排查「为什么他只拿到 1 字节」时，
/// 分支本身就是答案。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum AllocationMode {
    /// 预算够每个节点一个地板。
    Floor,
    /// 不够地板但够每节点至少 1 字节：降级到探测额度。
    Probe,
    /// `0 < B < N`：按轮转游标给 B 个节点各 1 字节。
    Rotation,
    /// `N = 0` 或 `B = 0`：**不产生正额度**。
    NoBudget,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct NodeWeight {
    pub node_id: String,
    /// 正整数需求权重，见 [`crate::quota::weight`]。
    pub weight: u32,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Plan {
    pub allocations: BTreeMap<String, u64>,
    pub mode: AllocationMode,
    /// 地板是否已失效。UI 要据此展示阻塞原因（R2b）。
    pub floor_defeated: bool,
    /// 轮转游标的**建议**下一位置。
    ///
    /// **只有在本轮成功收敛之后才能把它写回去。** 失败就推进的话，
    /// 那些节点这一轮既没拿到额度、又被游标跳过去了，
    /// 「轮转覆盖所有节点」这条就不再成立。
    pub next_rotation_cursor: usize,
}

impl Plan {
    pub fn total(&self) -> u128 {
        self.allocations.values().map(|v| *v as u128).sum()
    }

    pub fn get(&self, node_id: &str) -> u64 {
        self.allocations.get(node_id).copied().unwrap_or(0)
    }
}

/// 按预算与权重规划各节点的分配额。
///
/// `nodes` 是**本轮可更新的节点**——健康、有新鲜已结算快照、通道可用。
/// 哪些节点可更新由调用方判定；这里只负责分钱。
pub fn plan(
    budget: u64,
    nodes: &[NodeWeight],
    config: &AllocationConfig,
    rotation_cursor: usize,
) -> Result<Plan> {
    config.validate()?;
    let n = nodes.len();

    // `N = 0` 或 `B = 0` 时不产生正额度。
    if n == 0 || budget == 0 {
        return Ok(Plan {
            allocations: nodes.iter().map(|node| (node.node_id.clone(), 0)).collect(),
            mode: AllocationMode::NoBudget,
            floor_defeated: budget == 0 && n > 0,
            next_rotation_cursor: rotation_cursor,
        });
    }

    let n_u128 = n as u128;
    let budget_u128 = budget as u128;
    // N × floor 用 u128 算：floor 可以配到很大，u64 乘法会溢出。
    let floor_demand = n_u128 * config.floor as u128;

    let (bases, mode, floor_defeated, next_cursor) = if budget_u128 >= floor_demand {
        (vec![config.floor; n], AllocationMode::Floor, false, rotation_cursor)
    } else if budget_u128 >= n_u128 {
        let per = (budget_u128 / n_u128) as u64;
        (vec![config.probe.min(per); n], AllocationMode::Probe, true, rotation_cursor)
    } else {
        // 0 < B < N：给 B 个节点各 1 字节，从游标开始。
        let mut bases = vec![0u64; n];
        let start = rotation_cursor % n;
        let lucky = budget as usize; // B < N ≤ usize::MAX
        for offset in 0..lucky {
            bases[(start + offset) % n] = 1;
        }
        (bases, AllocationMode::Rotation, true, (start + lucky) % n)
    };

    let base_sum: u128 = bases.iter().map(|b| *b as u128).sum();
    let extra = budget_u128.saturating_sub(base_sum);
    let shares = distribute(extra, nodes)?;

    let mut allocations = BTreeMap::new();
    for (index, node) in nodes.iter().enumerate() {
        let raw = bases[index] as u128 + shares[index];
        // wire 上限截断（C31）。截断只会让总额变小，不会破坏
        // 「新计划分配额之和 ≤ 额度池剩余」这条不变量。
        allocations.insert(node.node_id.clone(), raw.min(WIRE_MAX as u128) as u64);
    }

    Ok(Plan { allocations, mode, floor_defeated, next_rotation_cursor: next_cursor })
}

/// 最大余数法：按权重把 `amount` 分给各节点。
///
/// `share_i = floor(amount × w_i / Σ w_j)`，余数从大到小补 1，
/// **平局按 `node_id` 字节序**——没有这条，同一组输入在不同的 map 迭代顺序下
/// 会分出不同的结果，而「最大余数法结果确定」是 §8 要求的。
fn distribute(amount: u128, nodes: &[NodeWeight]) -> Result<Vec<u128>> {
    let n = nodes.len();
    if amount == 0 || n == 0 {
        return Ok(vec![0; n]);
    }
    let total_weight: u128 = nodes.iter().map(|node| node.weight as u128).sum();
    if total_weight == 0 {
        // 权重恒为正（[`weight::MIN_WEIGHT`] = 1），走到这里说明调用方传了 0。
        return Err(Error::Quota("权重之和为 0：需求权重必须是正整数".into()));
    }

    let mut shares = vec![0u128; n];
    let mut remainders: Vec<(u128, &str, usize)> = Vec::with_capacity(n);
    let mut distributed = 0u128;
    for (index, node) in nodes.iter().enumerate() {
        let numerator = amount
            .checked_mul(node.weight as u128)
            .ok_or_else(|| Error::Quota("分配的 u128 中间值溢出".into()))?;
        shares[index] = numerator / total_weight;
        remainders.push((numerator % total_weight, node.node_id.as_str(), index));
        distributed += shares[index];
    }

    // 余数大的先补；平局按 node_id 字节序升序。
    remainders.sort_by(|a, b| b.0.cmp(&a.0).then_with(|| a.1.as_bytes().cmp(b.1.as_bytes())));
    let mut leftover = amount - distributed;
    for (_, _, index) in &remainders {
        if leftover == 0 {
            break;
        }
        shares[*index] += 1;
        leftover -= 1;
    }
    Ok(shares)
}

/// 身份级切分（D17 情形 3：同节点、**同口径**的多个身份）。
///
/// **不设地板。** 地板的存在理由是「用户漫游到冷节点别被立刻闸断」，
/// 而同一节点内的身份切换不是用户的自主行为。
///
/// 只在情形 3 下有效。情形 2（口径不同）必须**各自独立成池**，
/// 那时它们的字节不可直接相加，`Σ 身份限额 = 节点分配额` 这条不变量本身就不成立——
/// 强行断言它反而会掩盖「两个口径被错误地相加了」这个真问题。
pub fn split_identities(amount: u64, identities: &[NodeWeight]) -> Result<BTreeMap<String, u64>> {
    let shares = distribute(amount as u128, identities)?;
    Ok(identities
        .iter()
        .zip(shares)
        .map(|(identity, share)| (identity.node_id.clone(), share.min(WIRE_MAX as u128) as u64))
        .collect())
}
