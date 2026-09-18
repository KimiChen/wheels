//! proxy-manager：`sing-box-plus` 节点的采集、结算、配额与控制台主控。
//!
//! 正文即规范的计划书是 [`README.md`](../README.md)，约束 C1–C35、决策 D1–D22、
//! 风险 R1–R13 都在里面，每条都有编号。本 crate 的每个模块只标注它落地了哪几条，
//! **不重述约束内容**——重述出来的第二份定义总会与计划书漂移。
//!
//! 里程碑边界（README §7）：
//!
//! | 模块 | 里程碑 | 状态 |
//! | --- | --- | --- |
//! | [`config`] | M0 | 已实现 |
//! | [`store`] | M0 | 已实现（迁移、数值编码、单写者事务） |
//! | [`collect`] | M0 / M1 | M0 实现传输与快照校验；入账属于 M1 |
//! | [`quota`] | M0 / M2 | M0 实现 wire 类型与序列化校验；状态机属于 M2 |
//! | [`ledger`] | M1 | 未实现 |
//! | [`agent`] | M1 | 未实现 |
//! | [`pki`] | M1 | 未实现 |
//! | [`api`] / [`web`] | M4 | 未实现 |
//! | [`im`] | M5 | 未实现 |

pub mod agent;
pub mod api;
pub mod collect;
pub mod config;
pub mod crash;
pub mod error;
pub mod im;
pub mod ledger;
pub mod pki;
pub mod quota;
pub mod store;
pub mod web;

pub use error::{Error, Result};
