# 完整功能补丁

`series` 按顺序列出相对于锁定 `shadowsocks-rust v1.24.0` 提交生成的最终态补丁。补丁必须按
`0001`、`0002`、`0003` 的顺序应用；后续补丁依赖前一个补丁的源码状态：

- `0001-eih-user-stats-http-unix-exporter.patch`：EIH 用户统计与 Unix HTTP exporter；
- `0002-reproducible-build-time.patch`：从 `SOURCE_DATE_EPOCH` 派生可复现 build time；
- `0003-user-audit.patch`：用户成功访问审计、ingest/export 协议、auditd spool 与 producer。

`0001` 与 `0003` 各自作为不可拆分的完整功能补丁；`0002` 是独立的构建确定性补丁。完整 overlay
包含：

- AEAD-2022 EIH 认证用户句柄与日志脱敏；
- `user-stats` feature、严格配置校验及 manager 互斥；
- 稳定身份 registry、四向饱和计数与生命周期监督；
- TCP/TFO 和按认证身份隔离的 UDP 计数及失败关闭；
- 严格 HTTP/1.1-over-Unix-stream exporter、资源上限、超时和 socket/lockfile 安全边界；
- 对应 Rust 单元与集成测试。
- `shadowsocks-audit-protocol` 强类型事件、诊断、ingest framing、export DTO、HMAC canonicalization
  与 golden vectors；
- `shadowsocks-auditd` Linux-only daemon、NDJSON spool、lease/ack、health、容量回收与恢复；
- ssserver 的 TCP/UDP 成功事件 producer、有限 queue、诊断 gap 和共享 AuditSupervisor；
- auditd systemd/sysusers/tmpfiles 模板、双二进制 release manifest、mock collector 与故障测试。

不得只应用 `0001` 并声称启用了审计；`user-audit` feature 需要 `0003` 提供的 crate、binary 和
接线。维护时应在锁定上游的独立工作树中形成完整源码，再为每个逻辑变更生成补丁，并用
`patches/series` 顺序重放：

```bash
git format-patch -1 --stdout --full-index --binary --no-renames --zero-commit --no-signature \
  > patches/0003-user-audit.patch
printf '%s\n' 0001-eih-user-stats-http-unix-exporter.patch 0002-reproducible-build-time.patch \
  0003-user-audit.patch > patches/series
```

`--no-renames` 用于把移动文件展开为明确的删除与新增，保证无 `.git` 源码树中的通用 `patch`
可以重放。不得拼接补丁、手工忽略冲突或允许 fuzz；更新后必须运行 `scripts/verify.sh`。非 Linux
主机必须先安装 `SHADOWSOCKS_AUDIT_CHECK_TARGET`（默认 `x86_64-unknown-linux-gnu`），脚本会在
feature-off/protocol/mock 测试之外对 auditd 执行交叉 `cargo check --all-targets`，并在 target 缺失
时失败。Linux 主机还必须运行 `cargo test --workspace --features user-audit` 和 auditd 原生测试；
Linux runtime 路径不能由非 Linux 的交叉检查替代。
