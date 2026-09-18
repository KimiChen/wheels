//! M1 的集成测试入口。
//!
//! 本轮交付 agent + 双 CA PKI（传输与授信）。采集入账与结算事务 11 步在下一轮。
//!
//! | 完成标准 | 模块 |
//! | --- | --- |
//! | 精确 leaf 授信的正负矩阵（跑真实握手，不只做链验证） | [`pki_matrix`] |
//! | 签发阶段六项校验 | [`pki_issuance`] |
//! | 密钥不进版本库——门禁而不是嘱咐 | [`secret_gate`] |
//! | 封闭命令集、单飞、审计只读已轮转、端到端 | [`agent_contract`] |
//! | 结算事务 11 步与四项判据 | [`settlement`] |
//! | 事务中途真 `kill -9` 后的自洽 | [`crash`] |
//! | 采集调度全栈（假节点→agent→主控→账本） | [`scheduler`] |

#[path = "m1/harness.rs"]
mod harness;
#[path = "m1/ledger_harness.rs"]
mod ledger_harness;

// 复用 M0 的假节点：agent 的上游就是节点本机 UDS，没有理由再造一个。
#[path = "m0/fake_node.rs"]
mod fake_node;
#[path = "m0/fixtures.rs"]
mod fixtures;

#[path = "m1/pki_issuance.rs"]
mod pki_issuance;
#[path = "m1/pki_matrix.rs"]
mod pki_matrix;
#[path = "m1/secret_gate.rs"]
mod secret_gate;

#[path = "m1/agent_contract.rs"]
mod agent_contract;
#[path = "m1/crash.rs"]
mod crash;
#[path = "m1/scheduler.rs"]
mod scheduler;
#[path = "m1/settlement.rs"]
mod settlement;
