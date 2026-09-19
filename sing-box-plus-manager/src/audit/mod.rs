//! 出站目标审计：把节点上的访问明细同步到本机，查询时直接从文件查。
//!
//! **这条链路与计费链路完全独立**（C24）。审计数据不进数据库、也不进备份：
//! `ledger backup` 走 `VACUUM INTO`，只碰那一个 `.db` 文件，于是「用户 → 域名」
//! 的明细天然不会被复制进每一份账本备份。

pub mod filter;
pub mod health;
pub mod queries;
pub mod query;
pub mod record;
pub mod sync;
