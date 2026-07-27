//! Manager 计量：定时读各 Entry 的 SSM 累计统计 → 单事务增量入账 → 配额评估 → 资格翻转触发 reconcile。
//! 周期纯函数在 [`period`]；仓储在 `store::metering`；运行态在 `store::runtime_state`。

pub mod period;
pub mod tick;
