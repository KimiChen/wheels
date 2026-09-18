//! `proxy-manager-agent` 的实现。
//!
//! 拆成 lib + bin 是为了让集成测试能驱动**真实的服务端代码**，
//! 而不是在测试里重写一遍握手与分发——重写出来的那一份必然比真的宽容，
//! 于是「测过了」和「生产上成立」变成两件事。

pub mod commands;
pub mod config;
pub mod server;
