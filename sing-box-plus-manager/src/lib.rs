//! proxy-manager：`sing-box-plus` 节点的采集、结算、配额与控制台主控。
//!
//! 正文即规范的计划书是 [`README.md`](../README.md)，约束 C1–C35、决策 D1–D30、
//! 风险 R1–R13 都在里面，每条都有编号。本 crate 的每个模块只标注它落地了哪几条，
//! **不重述约束内容**——重述出来的第二份定义总会与计划书漂移。
//!
//! 里程碑边界（README §7）：
//!
//! | 模块 | 里程碑 | 状态 |
//! | --- | --- | --- |
//! | [`config`] | M0 | 已实现 |
//! | [`store`] | M0 | 已实现（数值编码、单写者事务、一致性门禁）。**不做迁移**（D22） |
//! | [`collect`] | M0 / M1 | 已实现：传输、快照校验、周期调度 |
//! | [`quota`] | M0 / M2 | 已实现：wire 类型、池与分配、epoch 状态机、下发接线 |
//! | [`ledger`] | M1 | 已实现：入账与基线同事务（D4）、runtime 批准登记 |
//! | [`agent`] | M1 | 已实现：封闭命令集 + 应用层 HMAC 客户端 |
//! | [`pki`] | M1 | 已实现：双 CA 签发与检视 |
//! | [`identity`] | M2 | 已实现：D11 账号池的领取与退役 |
//! | [`api`] / [`web`] | M4 | 已实现：结构化 API、会话/CSRF、12 页控制台 |
//! | [`sso`] | M5 | 已实现：泡游 SSO 登录与成员名单（D27–D30） |
//! | [`im`] | M5 | **未实现**：告警与日报（§4.10）仍是设计文档 |
//! | [`subscription`] | M6 | 已实现：token 与 `ss://` 订阅；Clash/sing-box 产物未做 |
//!
//! 这张表曾经整张落后于代码——五个模块都标着「未实现」而它们早已各有上千行。
//! 它没有任何自动校验，所以改模块时要顺手改它。

pub mod agent;
pub mod api;
pub mod audit;
pub mod collect;
pub mod config;
pub mod crash;
pub mod error;
pub mod identity;
pub mod im;
pub mod ledger;
pub mod pki;
pub mod quota;
pub mod sso;
pub mod store;
pub mod subscription;
pub mod web;

pub use error::{Error, Result};
