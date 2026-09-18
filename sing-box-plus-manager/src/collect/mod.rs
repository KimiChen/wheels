//! 采集：取原始字节 → 逐字段校验 → 归一化 → 交给账本。
//!
//! M0 交付前两件（传输与校验）。归一化成 lineage 增量、交账本入账属于 M1。
//!
//! **只对接 `sing-box-plus`（D3）**：`GET /v3/snapshot`，`schema_version` 必须为 `3`。
//! 版本断言对齐**节点侧 Go 常量** `internal/userstats/snapshot.go` 的 `SchemaVersion`，
//! 不对齐它的 README——那份 README 曾有约 10 处写 `2`
//! （`docs/integration-contract.md` §4 记了这次漂移的代价）。

pub mod scheduler;
pub mod snapshot;

/// HTTP/1.1-over-UDS 传输在 `proxy-manager-wire` 里。
///
/// **生产上主控不走这条路**（README §4.1：主控绝不直连节点 UDS），它经 agent。
/// 这里 re-export 是给 M0 的假节点夹具用的——夹具刻意绕过 agent 与 mTLS。
pub use proxy_manager_wire::uds::{Failure, Response, UdsClient};
pub use snapshot::{Health, Inbound, Rejected, Snapshot, SnapshotUser, SCHEMA_VERSION};

/// 快照路径。路径版本号与 `schema_version` **成对**硬校验（C10）。
///
/// 节点已经写明 `/v1` 与 `/v2` 恒为 404——主控这一侧再加一道，是因为主控可能被指到
/// 一个根本不是 `sing-box-plus` 的端点，那时 404 不会出现。
pub const SNAPSHOT_PATH: &str = "/v3/snapshot";

pub use scheduler::{acceptance_criteria, AcceptanceCriteria, NodeCollector, SchedulePolicy, Tick};
