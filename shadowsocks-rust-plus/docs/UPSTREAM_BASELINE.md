# 上游基线验证

## 固定版本

- repository：`https://github.com/shadowsocks/shadowsocks-rust.git`
- tag：`v1.24.0`（lightweight tag）
- commit：`7ee1aa9223ed8f4d34734aac919036c8ad4502c2`
- commit date：`2025-12-11T07:38:59+08:00`
- Rust MSRV：`1.88`
- license：MIT

`scripts/prepare-source.sh` 以 shallow fetch 取得 tag，验证解析后的 commit，再用
`git archive` 生成不含嵌套 `.git` 的源码树；任一补丁不能精确应用时立即失败。

## 2026-08-25 原始上游测试记录

环境：Apple M4、16 GiB、Darwin arm64，`rustc 1.97.0`。

命令：

```bash
cargo test --workspace --all-targets
```

结果：编译成功；`shadowsocks` 库单元测试 2/2 通过；随后五个
`crates/shadowsocks/tests/tcp.rs` 用例失败。五个用例均成功通过代理收到
`HTTP/1.1 200 OK`，但固定上游测试在第 144 行只接受 `HTTP/1.0 200 OK`。

这是依赖公网 `www.example.com` 响应格式的上游基线问题，不由本项目补丁修改或隐藏。

本项目验证脚本运行收窄的 workspace `--lib --bins` 库和二进制单元测试。workspace 命令因此不选择
任何 integration target，但被排除的目标并非一律豁免——§16 按三类逐一处置：overlay 自有的
`tcp_eih_user` 与三个纯 loopback 目标（`crates/shadowsocks/tests/udp.rs`、`tests/udp.rs`、
`tests/tunnel.rs::udp_tunnel`）由 `scripts/test.sh` 显式单独运行并计入全绿判据；只有依赖公网的
`crates/shadowsocks/tests/{tcp,tcp_tfo}.rs` 与 `tests/{socks4,socks5,http,dns}.rs`、
`tests/tunnel.rs::tcp_tunnel` 被豁免，作为本文的基线诊断保留。

上游升级时仍应再次运行完整原始命令并记录结果。
