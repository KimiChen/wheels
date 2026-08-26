# 完整功能补丁

`series` 只列出一个相对于锁定 `shadowsocks-rust v1.24.0` 提交生成的最终态补丁：
`0001-eih-user-stats-http-unix-exporter.patch`。它作为不可拆分的完整 overlay，包含：

- AEAD-2022 EIH 认证用户句柄与日志脱敏；
- `user-stats` feature、严格配置校验及 manager 互斥；
- 稳定身份 registry、四向饱和计数与生命周期监督；
- TCP/TFO 和按认证身份隔离的 UDP 计数及失败关闭；
- 严格 HTTP/1.1-over-Unix-stream exporter、资源上限、超时和 socket/lockfile 安全边界；
- 对应 Rust 单元与集成测试。

该补丁必须整体应用，不支持选择性应用。维护时应在锁定上游的独立工作树中形成完整源码，再以
一条提交重新生成补丁：

```bash
git format-patch -1 --stdout --full-index --binary --no-renames --zero-commit --no-signature
```

`--no-renames` 用于把移动文件展开为明确的删除与新增，保证无 `.git` 源码树中的通用 `patch`
可以重放。不得拼接补丁、手工忽略冲突或允许 fuzz；更新后必须运行 `scripts/verify.sh`。
