# 变更记录

本项目遵循语义化版本。由于当前仍是 `0.x` 预览阶段，小版本可能包含需要运维介入的变更。

## 未发布

### 新增

- VLESS-Reality Entry 全链路：自动 X25519/short ID、用户 UUID、编译、真实 check、Agent
  部署/回滚、进程健康检查，以及 raw 与 Clash/mihomo 混合订阅。
- Reality 私钥与 short ID 信封加密，并纳入主密钥 re-seal。

### 限制

- VLESS 暂无 SSM 式 per-user 流量计量；授权 VLESS relay 的用户必须 `quotaBytes = 0`。

## 0.1.0 - 2026-07-27

首个公开预览版本。

### 新增

- 拆分 TOML 声明式配置：servers、protocols、listeners、relays、users。
- `plan` 配置合并、未知字段、引用、端口、协议和授权校验。
- `apply` 幂等同步 Host、Entry、Node、Route、User 和 UserRoute。
- `status` 文本与 JSON 状态查看。
- 无管理 API 的后台 `controller`。
- 被动 mTLS Agent、双 CA、Host URI SAN 和 SPKI pin。
- enrollment 签发、指纹带外授信和吊销 CLI。
- Shadowsocks-2022 managed Entry、固定 Node 中继和多跳配置编译。
- 确定性 canonical JSON、revision 去重和加密 artifact。
- Controller 与 Agent 双重 `sing-box check`。
- Node→Entry 分批发布、独占租约、健康检查和失败回滚。
- Entry 与 Node 分角色健康检查；首次部署允许尚无 sing-box 进程。
- Agent 只在启动和健康检查成功后提交活动 revision，失败恢复旧版本或保持未部署状态。
- Agent 重启恢复活动配置，退出时停止受管 sing-box 子进程。
- Agent 命令幂等、超时确认和本地 revision 状态。
- Shadowsocks SSM 用户 reconcile。
- 按用户累计流量、周期配额、运行 epoch 和重启前两阶段结算屏障。
- raw `ss://`、Clash/mihomo YAML 和 HTML 订阅。
- Prometheus 指标、健康/就绪探针、审计和历史保留。
- XChaCha20-Poly1305 信封加密与多版本主密钥 re-seal。
- 中文 README、配置、架构、部署、恢复、排障、开发、贡献和安全文档。
- MIT License 和多阶段 Dockerfile。

### 安全

- 真实配置、数据库、密钥、证书、enrollment 和生成物默认 ignored。
- Agent 只接受预定义操作，不执行任意 shell。
- SSM 仅允许本机回环。
- 订阅 token 只保存 SHA-256，明文仅首次返回。
- 部署 artifact 明文只在内存、受限临时文件和 mTLS 通道中出现。
- 不支持的 VLESS listener 在远端部署前失败关闭。

### 已知限制

- VLESS-Reality 尚不能编译、订阅或部署。
- SSH 字段尚未接入自动装机。
- 不支持多 Controller 并发写同一个 SQLite。
- 没有独立备份/恢复 CLI。
- 没有独立订阅 token 轮换 CLI。
- Controller 客户端证书没有在线无中断轮换编排。
- 当前仅验证 sing-box `1.13.14` 和类 Unix 系统。
