//! 周期池（README §4.5）。
//!
//! ```text
//! remaining_pool = 全局月度额度 − Σ(本周期起点以来各节点的增量)
//! ```
//!
//! **绝对值一次都不参与比较**（C11）。「本周期已用」只能由主控按六元组基线自持，
//! 不得拿快照绝对值与配额相比——快照是**进程生命周期**内的累计（C1），
//! 同名重建还会复用原计数器、绝对值跨周期不归零（C2）。
//!
//! 三个口径并存，不要混用：
//!
//! | 口径 | 用在哪 |
//! | --- | --- |
//! | 本次增量 | 账本行、小时/日汇总 |
//! | **周期累计** | **配额判定、额度池** |
//! | 永久累计 | 用户总览与长期报表，**不用来比较周期配额** |

use crate::store::codec::U128Text;

/// 本轮可分配预算 `B`。
///
/// ```text
/// B = u64::try_from((quota as u128).saturating_sub(cycle_used_u128))
/// ```
///
/// **饱和减法保证 `used > quota` 时 `B = 0` 而不是下溢**（C32）。
/// R2 已声明超发必然发生，所以 `used > quota` 是**预期状态**而不是异常：
/// 用 u64 相减会下溢成约 `2^64`，clamp 之后变成 9.2 EB——
/// 用户一超额就变成无限额度，这是最坏的一种静默失败。
///
/// 周期累计在 u128 里算（跨节点、跨 runtime 相加会超出 u64），
/// 与 u64 的额度比较前先在 u128 里做饱和减法，再收窄回 u64。
/// 两步都不能省。
pub fn budget(quota_bytes: u64, cycle_used: U128Text) -> u64 {
    let quota = quota_bytes as u128;
    let remaining = quota.saturating_sub(cycle_used.get());
    // 饱和减法的结果必然 ≤ quota ≤ u64::MAX，所以这次收窄不会失败。
    u64::try_from(remaining).unwrap_or(u64::MAX)
}

/// 某个用户在本周期的剩余池。与 [`budget`] 同义，命名区分调用语境。
pub fn user_pool(quota_bytes: u64, cycle_used: U128Text) -> u64 {
    budget(quota_bytes, cycle_used)
}

/// 自然月周期键。
///
/// 当前口径是 **UTC 自然月**（与 `ledger::settle::CYCLE_RULE_VERSION` 同一套）。
/// D21 只写了「同一个自然月周期」，没有指定时区；展示时区只约束日桶。
/// 改口径要推进 `CYCLE_RULE_VERSION`，历史行留在旧版本下不重算。
pub fn cycle_key(at: time::OffsetDateTime) -> String {
    let utc = at.to_offset(time::UtcOffset::UTC);
    format!("{:04}-{:02}", utc.year(), u8::from(utc.month()))
}

#[cfg(test)]
mod tests {
    use super::*;

    const GIB: u64 = 1 << 30;

    /// C32：`used > quota` 是**预期状态**，必须得到 0 而不是下溢。
    #[test]
    fn 超额时预算为零而不是下溢() {
        let quota = 300 * GIB;
        let used = U128Text::new(320 * GIB as u128);
        assert_eq!(budget(quota, used), 0);
        // 下溢的具体形态：u64 相减会得到这个天文数字。
        assert_ne!(budget(quota, used), quota.wrapping_sub(320 * GIB));
    }

    /// D21 的两个方向都是期望行为。
    #[test]
    fn 改额度对本周期立即生效的两个方向() {
        let used = U128Text::new(320 * GIB as u128);
        // 300 → 500：从「已用尽」变成「剩余 180 GiB」。本来就该让他继续用。
        assert_eq!(budget(500 * GIB, used), 180 * GIB);
        // 300 → 100：仍为「已用尽」，下发零额度。
        assert_eq!(budget(100 * GIB, used), 0);
    }

    #[test]
    fn 边界不溢出() {
        assert_eq!(budget(u64::MAX, U128Text::new(0)), u64::MAX);
        assert_eq!(budget(0, U128Text::new(0)), 0);
        // 周期累计跨过 u64 也不能把结果算错。
        assert_eq!(budget(u64::MAX, U128Text::new(u64::MAX as u128 + 1)), 0);
        assert_eq!(budget(u64::MAX, U128Text::MAX), 0);
    }

    #[test]
    fn 周期键是utc自然月() {
        use time::macros::datetime;
        assert_eq!(cycle_key(datetime!(2026-09-18 10:00:00 UTC)), "2026-09");
        assert_eq!(cycle_key(datetime!(2026-01-01 00:00:00 UTC)), "2026-01");
        // 跨时区输入先归一到 UTC：北京时间 9 月 1 日 07:00 还是 UTC 的 8 月。
        assert_eq!(cycle_key(datetime!(2026-09-01 07:00:00 +08:00)), "2026-08");
    }
}
