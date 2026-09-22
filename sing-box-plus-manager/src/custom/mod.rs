//! 个人自定义节点：每个人自己贴的上游与 SOCKS5 落地。
//!
//! 数据形状见 `schema/08_custom_nodes.sql`，解析规则见 `parse`，
//! 静态加密见 `crypto`。

pub mod crypto;
pub mod parse;
