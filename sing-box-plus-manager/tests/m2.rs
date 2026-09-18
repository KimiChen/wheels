//! M2 的集成测试入口。
//!
//! 本轮交付周期池与跨节点分配（纯计算）。epoch 状态机与 D21 的编排在后两轮。
//!
//! | 完成标准（§8 配额状态机那一节） | 模块 |
//! | --- | --- |
//! | 饱和减法、`N=0`/`B=0`/wire 截断/等权重无除零溢出 | [`allocation`] |
//! | `[64,336] MiB` 反例、最大余数法确定性、地板不因归一化失效 | [`allocation`] |
//! | 冷节点权重恒正、`a_i=0` 走探测、`B>=N` 每节点有正额 | [`allocation`] |
//! | `0<B<N` 时轮转覆盖所有节点 | [`allocation`] |
//! | 同口径多身份之和 == 节点分配额；不同口径各自成池 | [`allocation`] |
//! | 409 三步分支、unknown 恢复、C30 全量集合、C33 零表收敛 | [`dispatch`] |
//! | D21 改额度立即生效、调低先预览、审计、逐节点任务 | [`settings`] |

// 复用 M0 的假节点与 M1 的 PKI 脚手架：配额端点与生效表在 M0 就做好了。
#[path = "m0/fake_node.rs"]
mod fake_node;
#[path = "m0/fixtures.rs"]
mod fixtures;
#[path = "m1/harness.rs"]
mod harness;
#[path = "m2/quota_harness.rs"]
mod quota_harness;

#[path = "m2/allocation.rs"]
mod allocation;
#[path = "m2/dispatch.rs"]
mod dispatch;
#[path = "m2/settings.rs"]
mod settings;
