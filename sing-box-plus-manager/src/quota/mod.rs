//! 配额：池、分配、epoch 与下发状态机（README §4.5）。
//!
//! M0 交付 wire 类型与序列化校验，以及 409 的成因判别入口。
//! 完整的池调度、正权重/探测分配与恢复流程属于 M2。
//!
//! 三条在写第一行代码之前就必须立住的口径：
//!
//! - **未出现在 `entries[]` 中的身份 = 无限额度**（C16）。下发的是**限额清单**，不是用户清单。
//!   由此推出主控的义务 C30：每次请求必须枚举该节点**所有受限用户的全部当前有效身份**，
//!   包括零额度。缺一条不是「保持原值」，是**解除限额**。
//! - **`pool = quota − used` 用饱和减法**（C32）。`used > quota` 是预期状态（R2 声明超发必然发生），
//!   用 u64 相减会下溢成约 `2^64`，clamp 后变成 9.2 EB——**用户一超额就无限额度**。
//! - **`remaining_bytes` 在 wire 上是 `int64` 不是 u64，`epoch` 必须 ≥ 1**（C31）。
//!   C3 的快照整数规则不能被误解成「所有 wire 数字都是 u64」。

pub mod allocate;
pub mod converge;
pub mod dispatch;
pub mod pool;
pub mod settings;
pub mod table;
pub mod weight;
pub mod wire;

pub use allocate::{plan, AllocationConfig, AllocationMode, NodeWeight, Plan};
pub use pool::{budget, cycle_key};
pub use weight::{update_weight, DemandSample, SampleOutcome, SampleRejected};
pub use wire::{
    classify_conflict, ConflictCause, Quota, QuotaEntry, QuotaRequest, QuotaResponse,
    SerializeError, QUOTA_PATH,
};

/// `pool = quota − used`，饱和减法（C32）。
///
/// 周期累计在 u128 里算（跨节点、跨 runtime 相加会超出 u64），
/// 与 u64 的额度比较前先在 u128 里做饱和减法，再 checked 收窄回 u64。
/// 两步都不能省：直接在 u64 里减会下溢，直接收窄会在 `used > quota` 时panic 或截断。
pub fn pool_remaining(quota_bytes: u64, used_bytes: u128) -> u64 {
    let quota = quota_bytes as u128;
    let remaining = quota.saturating_sub(used_bytes);
    // 饱和减法的结果必然 ≤ quota ≤ u64::MAX，所以这次收窄不会失败。
    u64::try_from(remaining).unwrap_or(u64::MAX)
}

#[cfg(test)]
mod tests {
    use super::*;

    /// C32 的反向证据：如果哪天有人把它改成 u64 相减，这条会立刻红。
    #[test]
    fn 超额时池为零而不是下溢成天文数字() {
        let quota = 300u64;
        let used = 320u128;
        assert_eq!(pool_remaining(quota, used), 0);
        // 下溢的具体形态：u64 相减会得到这个值，clamp 之后就是 9.2 EB。
        assert_ne!(pool_remaining(quota, used), quota.wrapping_sub(used as u64));
    }

    #[test]
    fn 未超额时池是差值() {
        assert_eq!(pool_remaining(500, 320), 180);
        assert_eq!(pool_remaining(u64::MAX, 0), u64::MAX);
        assert_eq!(pool_remaining(0, 0), 0);
    }
}
