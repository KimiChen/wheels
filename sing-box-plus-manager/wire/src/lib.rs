//! 主控与 `proxy-manager-agent` 之间的共享合同。
//!
//! 三件东西必须在两个二进制里**逐字节一致**，所以它们只有一份定义：
//!
//! - [`command`]：封闭命令集。没有 `exec`、没有 shell 模板、没有任意文件路径。
//! - [`sign`]：应用层 HMAC（D12）。传输层 mTLS 只提供通道——若将来在节点侧插入
//!   C18 允许的那种反代，mTLS 会在反代处终结，那时**只有应用层签名还能证明
//!   「这份快照来自这个节点」**。
//! - [`verify`]：精确 leaf 授信。在线信任的是那一张带外核对过的 leaf 本身，
//!   **不是 intermediate**（`docs/threat-model.md` §2.2）。
//!
//! 依赖压到最小：agent 装在节点上，不该为了一个签名函数把数据库和 HTTP 框架搬过去。

pub mod audit;
pub mod command;
pub mod sign;
pub mod tls;
pub mod uds;
pub mod verify;

pub use command::{AgentCommand, AuditFetchRequest, COMMAND_METHOD, COMMAND_PATH};
pub use sign::{
    canonical, Direction, HmacKey, NonceCache, SignatureHeaders, SigningError, MAX_SKEW_SECS,
    NONCE_TTL_SECS,
};
pub use tls::{client_config, load_certs, load_hmac_key, server_config, TlsError};
pub use uds::{Failure, Response, UdsClient};
pub use verify::{ExactLeafClientVerifier, ExactLeafServerVerifier, VerifyError};
