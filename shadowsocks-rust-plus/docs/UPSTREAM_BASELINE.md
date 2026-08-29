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
本项目验证脚本运行收窄的 workspace `--lib --bins` 库和二进制单元测试，并单独运行 overlay 的 EIH 及本机化
TCP/UDP 数据面集成测试替代该公网断言；所有锁定上游的 workspace integration targets 都不属于
§16 workspace 门禁的全绿判据。
上游升级时仍应再次运行完整原始命令并记录结果。
