# sing-box-manager

面向 [sing-box](https://sing-box.sagernet.org) 多跳中继的**轻量 Web 管理平台**：以 SQLite 为真相源，
在 Web 管理台声明 Host / Entry / Node / Landing / Route / 用户 / 配额，由平台编译 sing-box 配置、
经 mTLS 编排各主机 Agent 部署、动态增删用户、按用户跨 Entry 计量并执行配额，并为每个用户提供订阅。

平台本身不包含、也不链接 sing-box。sing-box 始终作为独立进程运行，二者仅通过生成的 `config.json`
与入口的 SSM 管理 API 通信。

> 已针对 sing-box `1.13.14` 完成端到端验证。中继全程走公网，**不使用 WireGuard**。

## 架构

```
客户端 ──► 香港 VPS（Manager + Entry）──► DMIT（Node 中继）──► 家宽出口
              9736 Web/API + 19736 SS-2022 入站     全程公网中继
              └── mTLS ──► 各主机 Agent :39736（编排/统计/部署/reconcile）
```

- **单二进制两模式**：`server`（Manager 控制面）/ `agent`（被动 mTLS 主机代理，只监听、只访问本机回环 SSM）。
- **Manager 主动、Agent 被动**：状态查询、命令派发、发布、reconcile、计量均由 Manager 发起。
- 固定端口：Manager `9736` / Entry `19736` / Node `29736` / Agent `39736` / SSM `49736`。

## 快速开始

```bash
cargo build --release

# Manager 控制面
DATABASE_PATH=/var/lib/sbm/m.db \
ENCRYPTION_MASTER_KEY=$(openssl rand -base64 32) \
ADMIN_BOOTSTRAP_USER=admin ADMIN_BOOTSTRAP_PASSWORD='<≥12 强口令>' \
SECURE_COOKIES=false \
  ./target/release/sing-box-manager server

# 登录（拿会话 Cookie + CSRF token）
curl -c c.txt -X POST 127.0.0.1:9736/api/auth/login \
  -H 'content-type: application/json' -d '{"username":"admin","password":"..."}'
```

各主机 Agent 装机（发牌 → 授信 → 轮询）见 [docs/deployment.md](docs/deployment.md)。

## 安全模型（要点）

- **三套认证面**：管理面（会话 Cookie + CSRF + RBAC readonly/operator/admin + 敏感操作 re-auth）；
  公开订阅面（高熵 token，库存 sha256）；Agent 面（双向 mTLS，host_id/SPKI pin）。
- **信封加密**：业务密钥（CA 私钥、uPSK、配置）以 XChaCha20-Poly1305 信封加密；主密钥来自 env、**不入库**。
- **多版本主密钥轮换**：`key-rotation run` 在线把全部密文 re-seal 到新版本，幂等可续跑，退休门禁防误删旧密钥。
- **默认脱敏**：API/日志/审计/Agent 响应不含明文密钥；Manager 绝不下发任意 shell（Agent 只执行预定义操作）。
- 管理员密码 Argon2id；会话双过期 + 改密即吊销；登录失败锁定。

## 观测

`GET /metrics`（Prometheus，非回环须设 `metrics_scrape_token`）、`/healthz`（存活）、`/readyz`（就绪）、
`GET /api/health`（聚合健康，登录）。历史表按保留策略后台自动裁剪。

## 文档

- [docs/reference.md](docs/reference.md) — 端口 / 环境变量 / 文件布局 / CLI（单一权威）
- [docs/deployment.md](docs/deployment.md) — Manager 与 Agent 部署、防火墙、发布流程
- [docs/threat-model.md](docs/threat-model.md) — 资产、信任边界、攻击面与缓解、残余风险
- [docs/disaster-recovery.md](docs/disaster-recovery.md) — 崩溃 / 离线 / 恢复 / 主密钥轮换 runbook

## 实现进度

Phase 0（工程基础）· 1（Host/Agent mTLS PKI）· 2（拓扑与配置生成）· 3（版本发布）· 4（用户与订阅）·
5（流量与配额 + 结算屏障）已完成并端到端验证。Phase 6（生产化）进行中：**管理员认证/RBAC/CSRF/审计、
系统指标/健康/数据保留、主密钥轮换**已落地；备份 CLI 与证书轮换编排为剩余项。

> 旧 CLI/TOML 版本的文档归档于 [docs/legacy/](docs/legacy/)，仅供历史参考，不适用于当前平台。
