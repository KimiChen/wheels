//! 需求权重（README §4.5「权重、地板与冷节点恢复」、D7）。
//!
//! **消耗率只用于估计需求，不作为「已闸断」的证据。**
//! 节点在 I/O 完成后才扣额，且快照与下发时刻不重合，所以 `u_i / a_i` **可以大于 1**；
//! 恰好等于 1 也不能证明触发了闸断。把它当成闸断证据，会让一个正常跑满额度的节点
//! 被误判成「被限住了」，进而触发本不该有的提频与重分配。
//!
//! 用**持久化的正整数权重**而不是把可能为零的 `a_i` 本身当作权重：
//! `a_i = 0` 的冷节点若权重也是 0，就会永远分不到额度——一个零额度吸收态。

use crate::error::{Error, Result};

/// 初始权重。
pub const INITIAL_WEIGHT: u32 = 1024;
/// 权重范围 `[1, 2^31 − 1]`。**下界是 1 不是 0**：冷节点权重必须始终为正。
pub const MIN_WEIGHT: u32 = 1;
pub const MAX_WEIGHT: u32 = i32::MAX as u32;

/// 一轮观察出来的需求样本。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct DemandSample {
    /// `a_i`：上一轮**确认下发**的额度。
    pub allocated: u64,
    /// `u_i`：相应观察窗口内的已结算增量。
    pub used: u64,
}

/// 样本为什么不可用。沿用上一权重时要说得出原因。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum SampleRejected {
    /// 下发结果不明（`unknown`），不知道节点上到底生效了多少。
    OutcomeUnknown,
    /// 观察窗口跨越了多个补额 epoch，`u_i` 对不上任何一个 `a_i`。
    SpansMultipleEpochs,
    /// `a_i = 0`：**不做除法**，走探测分配。
    ZeroAllocation,
}

/// 一个观察窗口的结论：要么拿到可用样本，要么说得出为什么不可用。
///
/// 显式别名，不写 `Result<DemandSample, SampleRejected>`——
/// crate 里的 `Result<T>` 是单参数别名，两者放在一起看会很别扭。
pub type SampleOutcome = std::result::Result<DemandSample, SampleRejected>;

/// 按 `f(r) = (1 + 3r) / 2` 的整数实现更新权重。
///
/// ```text
/// w_i = clamp(floor(w_i × (a_i + 3 × min(u_i, a_i)) / (2 × a_i)), 1, 2^31 - 1)
/// ```
///
/// `min(u_i, a_i)` 就是把消耗率截到 `[0, 1]`——**只在权重函数内截**，
/// 原始消耗率保留给诊断（[`consumption_rate`]）。截断的理由不是「大于 1 非法」，
/// 而是「大于 1 不提供更多需求信息」：已经把额度用满了，再多用多少都只说明「还想要更多」。
///
/// 所有乘加使用 checked u128 中间值。
///
/// **这只是初始参数。** 响应速度要由流量分布测试验证，
/// 不宣称任意参数下都在固定轮次收敛。
pub fn update_weight(current: u32, sample: SampleOutcome) -> Result<u32> {
    let sample = match sample {
        Ok(sample) => sample,
        // 样本不可用时**沿用上一权重**，不衰减也不重置。
        Err(_) => return Ok(current),
    };
    if sample.allocated == 0 {
        return Ok(current);
    }

    let w = current as u128;
    let a = sample.allocated as u128;
    let capped = sample.used.min(sample.allocated) as u128;

    let numerator = w
        .checked_mul(
            a.checked_add(3u128.checked_mul(capped).ok_or_else(overflow)?).ok_or_else(overflow)?,
        )
        .ok_or_else(overflow)?;
    let denominator = 2u128.checked_mul(a).ok_or_else(overflow)?;
    let next = numerator / denominator;

    Ok(next.clamp(MIN_WEIGHT as u128, MAX_WEIGHT as u128) as u32)
}

fn overflow() -> Error {
    Error::Quota("权重更新的 u128 中间值溢出".into())
}

/// 原始消耗率，**只用于诊断**。可以大于 1。
///
/// 返回 `None` 表示 `a_i = 0`——那时不做除法。
pub fn consumption_rate(sample: DemandSample) -> Option<f64> {
    if sample.allocated == 0 {
        return None;
    }
    Some(sample.used as f64 / sample.allocated as f64)
}

/// 「需求偏高」的提频判据（§4.5 自适应频率表）。
///
/// 有效样本且 `a_i > 0` 且 `u_i / a_i ≥ 0.9`，**包括大于 1 的情形**。
/// 命中只表示**需求偏高**，标记「可能额度受限」并提频复核——
/// **不称作已证实闸断**。
pub fn demand_is_high(sample: DemandSample) -> bool {
    if sample.allocated == 0 {
        return false;
    }
    // 用整数比较避开浮点：u ≥ 0.9 a  ⇔  10u ≥ 9a
    let used = sample.used as u128;
    let allocated = sample.allocated as u128;
    used * 10 >= allocated * 9
}

#[cfg(test)]
mod tests {
    use super::*;

    fn sample(allocated: u64, used: u64) -> SampleOutcome {
        Ok(DemandSample { allocated, used })
    }

    /// `u_i / a_i` 覆盖 0.99 / 1 / 1.31，**后两者不报非法计量**。
    #[test]
    fn 消耗率可以等于一也可以大于一() {
        for (a, u, rate) in [(100u64, 99u64, 0.99), (100, 100, 1.0), (100, 131, 1.31)] {
            let s = DemandSample { allocated: a, used: u };
            let computed = consumption_rate(s).expect("a > 0 时有消耗率");
            assert!((computed - rate).abs() < 1e-9, "a={a} u={u}");
            // 都要能算出权重，不得报错。
            let w = update_weight(INITIAL_WEIGHT, Ok(s)).expect("不该报非法计量");
            assert!((MIN_WEIGHT..=MAX_WEIGHT).contains(&w));
        }
    }

    /// 大于 1 只在权重函数内被截到 1，原始值保留给诊断。
    #[test]
    fn 大于一的消耗率在权重函数内被截断() {
        let full = update_weight(1024, sample(100, 100)).unwrap();
        let over = update_weight(1024, sample(100, 131)).unwrap();
        assert_eq!(full, over, "截到 [0,1] 之后两者等价");
        // 但诊断值不同。
        assert_ne!(
            consumption_rate(DemandSample { allocated: 100, used: 100 }),
            consumption_rate(DemandSample { allocated: 100, used: 131 })
        );
    }

    /// `f(r) = (1 + 3r) / 2`：用满则涨到 2 倍，完全不用则降到一半。
    #[test]
    fn 权重按消耗率升降() {
        assert_eq!(update_weight(1024, sample(100, 100)).unwrap(), 2048, "r=1 → ×2");
        assert_eq!(update_weight(1024, sample(100, 0)).unwrap(), 512, "r=0 → ×0.5");
        assert_eq!(update_weight(1024, sample(100, 50)).unwrap(), 1280, "r=0.5 → ×1.25");
    }

    /// **冷节点权重始终为正。** 连续不用也降不到 0，否则就是零额度吸收态。
    #[test]
    fn 权重永不降到零() {
        let mut w = INITIAL_WEIGHT;
        for _ in 0..64 {
            w = update_weight(w, sample(100, 0)).unwrap();
        }
        assert_eq!(w, MIN_WEIGHT, "一直不用最终停在 1，不是 0");
        assert!(w >= MIN_WEIGHT);
    }

    #[test]
    fn 权重有上界不溢出() {
        let mut w = MAX_WEIGHT;
        for _ in 0..8 {
            w = update_weight(w, sample(100, 100)).unwrap();
            assert_eq!(w, MAX_WEIGHT, "封顶后不再增长，也不回绕");
        }
        // 极端输入也不溢出。
        assert!(update_weight(MAX_WEIGHT, sample(u64::MAX, u64::MAX)).is_ok());
        assert!(update_weight(MAX_WEIGHT, sample(1, u64::MAX)).is_ok());
    }

    /// `a_i = 0` 时**不做除法**，沿用上一权重，靠探测分配拿额度。
    #[test]
    fn 零分配不做除法() {
        assert_eq!(update_weight(777, sample(0, 0)).unwrap(), 777);
        assert_eq!(update_weight(777, sample(0, 12345)).unwrap(), 777);
        assert_eq!(consumption_rate(DemandSample { allocated: 0, used: 5 }), None);
        assert!(!demand_is_high(DemandSample { allocated: 0, used: 5 }));
    }

    /// 样本不可用时沿用上一权重——不衰减、不重置。
    #[test]
    fn 样本不可用时沿用上一权重() {
        for reason in [
            SampleRejected::OutcomeUnknown,
            SampleRejected::SpansMultipleEpochs,
            SampleRejected::ZeroAllocation,
        ] {
            let rejected: SampleOutcome = Err(reason);
            assert_eq!(update_weight(1234, rejected).unwrap(), 1234, "{reason:?}");
        }
    }

    /// 提频判据包含 `r > 1`，且命中只表示需求偏高。
    #[test]
    fn 需求偏高的判据() {
        assert!(!demand_is_high(DemandSample { allocated: 100, used: 89 }));
        assert!(demand_is_high(DemandSample { allocated: 100, used: 90 }), "边界 0.9 命中");
        assert!(demand_is_high(DemandSample { allocated: 100, used: 131 }), "大于 1 也命中");
        // 不做除法也不溢出。
        assert!(demand_is_high(DemandSample { allocated: u64::MAX, used: u64::MAX }));
    }
}
