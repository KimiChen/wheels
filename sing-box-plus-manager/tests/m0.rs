//! M0 的集成测试入口。
//!
//! 单一测试二进制：假节点夹具只编译一次，各故障矩阵以模块形式挂在它下面。
//!
//! 覆盖对应 README §7 的 M0 完成标准：
//!
//! | 完成标准 | 模块 |
//! | --- | --- |
//! | 输入摘要锁定 | [`fixture_lock`] |
//! | 版本错配与 health 多/少字段各覆盖一次 | [`collect_contract`] |
//! | u64/u128 存储往返与事务回滚 | [`store_numeric`] |
//! | 409 两种成因、响应丢失与延迟到达、零表收敛、epoch 顺序、新 sequence 门禁 | [`quota_contract`] |
//! | 配置侧的缺省即启动失败 | [`config_gate`] |

// 测试二进制的 crate 根在 tests/m0.rs，因此子模块默认会去 tests/ 下找，
// 而不是 tests/m0/。用 #[path] 显式指过去，把这个二进制的文件收在一个目录里。
#[path = "m0/fake_node.rs"]
mod fake_node;
#[path = "m0/fixtures.rs"]
mod fixtures;

#[path = "m0/collect_contract.rs"]
mod collect_contract;
#[path = "m0/config_gate.rs"]
mod config_gate;
#[path = "m0/fixture_lock.rs"]
mod fixture_lock;
#[path = "m0/quota_contract.rs"]
mod quota_contract;
#[path = "m0/store_numeric.rs"]
mod store_numeric;
