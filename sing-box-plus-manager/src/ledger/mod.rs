//! 账本：入账与基线在同一个事务里（README §4.4、`docs/data-model.md` §4）。
//!
//! 入口是 `docs/data-model.md` §4 的 11 步，顺序不能改：
//! 节点 README 把「推进基线」与「产出计费记录」的先后定成一次显式取舍
//! （先产出记录 = 崩溃即重复计费，反过来 = 崩溃即漏记），参考 collector 选了后者。
//! 主控有事务型存储，所以把两步做成一次原子提交（D4）——这是本项目相对参考实现的实质增量。

pub mod bucket;
pub mod canonical;
pub mod runtime;
pub mod settle;

pub use bucket::{hour_bucket, TimeDecision, TimeQuality};
pub use canonical::{batch_id, source_event_hash, LineageDelta, HASH_VERSION};
pub use settle::{receive, settle_next, FirstSnapshot, Receipt, RejectReason, SettleOutcome};
