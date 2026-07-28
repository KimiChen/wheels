# 路线图

本文只描述计划，不代表已经承诺版本或交付日期。当前能力以 README 和变更记录为准。

## 当前版本：0.1.x

已完成：

- 声明式 TOML 和幂等状态同步。
- Shadowsocks-2022 多跳编译与部署。
- mTLS Agent、enrollment 和发布门禁。
- 用户授权、订阅、SSM reconcile。
- 流量计量、配额和重启结算屏障。
- 信封加密和主密钥轮换。
- VLESS-Reality 密钥、编译、部署、回滚和混合订阅。
- 中文开源文档与 systemd 部署指南。

## 近期优先级

### 运维闭环

- 独立订阅 token 轮换和吊销 CLI。
- 一致性加密备份、验证和恢复 CLI。
- Agent/Controller 证书临期检查与无中断轮换编排。
- 部署重驱动、显式回滚和失败 target 诊断 CLI。
- settings 的受控 CLI，避免直接操作 SQLite。
- 状态输出增加 readiness 原因和部署摘要。
- 调研 VLESS 可审计 per-user 计量方案；在此之前保持非零配额失败关闭。

### SSH bootstrap

- 解析 `sshKey`、`knownHosts` 和 `servers.*.ssh`。
- 强制 known_hosts 校验。
- 安装或升级 sing-box-manager 与 sing-box。
- 写入 systemd 和防火墙建议，但不执行任意远端 shell 模板。
- 支持 dry-run、逐主机确认和幂等重试。

## 中期方向

- 配置差异预览和更细粒度 plan。
- 可配置但受约束的 Entry/Node 端口迁移。
- 更丰富的 Landing 类型。
- 告警输出和监控集成。
- 发布 artifact 签名和离线验证。
- 交叉编译、校验和签名发布产物。
- 支持更多类 Unix 发行版的安装包。

## 暂不计划

- Web 管理台或公开管理 API。
- Agent 主动连接或主动拉取配置。
- 在 Agent 上执行管理员提供的任意 shell。
- 把主密钥或明文凭据存入 SQLite。
- 多 Controller 共享 SQLite。
- 内置或链接 sing-box 数据面。

若未来需要多 Controller，应先更换支持一致性协调的状态存储和锁模型，不能直接让多个进程写同一
SQLite。

## 贡献建议

优先欢迎：

- VLESS 客户端兼容性和真实连接回归测试。
- 备份/恢复与证书轮换。
- 部署和结算屏障的故障注入测试。
- 不同 Linux 发行版和 sing-box 版本的兼容报告。
- 中文文档、脱敏示例和可复现排障记录。

贡献前请阅读 [贡献指南](CONTRIBUTING.md) 和 [开发与测试](docs/development.md)。
