# 部署指南

以「香港 VPS = Manager + Entry 同机；DMIT = Node 中继；家宽 = 出口」为例。术语与端口见 [reference.md](reference.md)。

## 0. 构建

```
cargo build --release   # 产物 target/release/sing-box-manager，单二进制两模式
```

各主机需自行安装 sing-box（`1.13.14` 已验证），Manager 不含也不链接 sing-box。

## 1. Manager（server 模式）

`/etc/sbm/manager.env`（0600，**不入 Git**）：
```
DATABASE_PATH=/var/lib/sbm/manager.db
MANAGER_LISTEN=127.0.0.1:9736
ENCRYPTION_MASTER_KEY=<base64 32B>          # openssl rand -base64 32；异地额外保管
ENCRYPTION_MASTER_KEY_VERSION=1
ADMIN_BOOTSTRAP_USER=admin
ADMIN_BOOTSTRAP_PASSWORD=<≥12 强口令>       # 首启后登录即改密
SECURE_COOKIES=true                          # 需 TLS 反代
```

systemd `sbm-manager.service`：
```
[Service]
EnvironmentFile=/etc/sbm/manager.env
ExecStart=/usr/local/bin/sing-box-manager server
DynamicUser=yes
StateDirectory=sbm
Restart=on-failure
```

`9736` 建议置于 Caddy/Nginx TLS 反代之后（管理面走 HTTPS，`SECURE_COOKIES=true` 才有意义）。

首启：Manager 引导双 CA + Manager 客户端身份 + 从 env 引导首个管理员。登录后立刻改密。

## 2. Agent 装机（每台 Entry/Node/Landing Host）

Agent 是被动 mTLS 服务，只监听、只访问本机回环 SSM。**未装 Agent 的 Host 无法发布或激活 Route。**

enrollment 七步：
1. Manager 建 Host：登录后（admin+reauth）`POST /api/hosts`，分配能力（entry/node/landing）。
2. Manager 发牌：`POST /api/hosts/{id}/enrollment` → 一次性返回 enrollment 包（含 Agent 服务端证书+私钥、Manager pin）。**此响应是唯一含私钥的出口**，立即安全落地目标机。
3. 目标机放 enrollment 包（0600），设 `AGENT_ENROLLMENT_PATH`/`AGENT_STATE_PATH`/`AGENT_CONFIG_DIR`/`AGENT_SSM_ADDRESS`。
4. 起 Agent（`sing-box-manager agent`），监听 `39736`。
5. Manager 授信：`POST /api/hosts/{id}/trust`（admin+reauth）。
6. Manager 轮询确认在线（`/api/hosts/{id}` / `/api/hosts/{id}/readiness`）。
7. 之后所有部署/统计/reconcile 由 Manager 经 mTLS 发起。

systemd `sbm-agent.service` 同上，`ExecStart=… agent`，`EnvironmentFile=/etc/sbm/agent.env`。

## 3. 防火墙（固定端口，最小暴露）

| 主机 | 入站放行 |
|---|---|
| Manager | `9736`（或反代 443）；`39736` 出站到各 Agent |
| Entry | `19736/tcp+udp`（客户端）；`39736/tcp` 仅 Manager 源 IP；`49736` 仅回环 |
| Node | `29736/tcp+udp`；`39736/tcp` 仅 Manager 源 IP |

`49736`（SSM）与订阅 token 绝不对公网暴露。

## 4. 拓扑 → 发布 → 用户（管理面流程）

1. 建 Entry/Node/Landing/Route（可视化编辑 + 实时校验）。
2. 编译 revision → `sing-box check` 通过 → 发布（Node 批先、Entry 批后，原子应用 + 健康检查 + 失败自动回滚）。
3. 建用户、授权 Route、生成订阅；结构变更走「重编译 + 部署 + reconcile」，运行态变更（停用/超额）仅 reconcile 不重启。
4. 客户端用 `/sub/{token}` 导入（raw / Clash-mihomo / sing-box）。

## 5. 备份

- **务必**用一致快照（`VACUUM INTO` 或备份 CLI），不直接复制在写的库文件。
- 备份加密的独立密钥 `BACKUP_ENCRYPTION_KEY` 必须与主密钥**异地/独立保管**（否则偷一处即通吃）。
- 恢复演练覆盖：数据库 + 主密钥 + Agent 信任关系。见 [disaster-recovery.md](disaster-recovery.md)。
- （备份 CLI 尚在实施，当前先用 `VACUUM INTO` + 妥善保管主密钥。）

## 6. 观测

- Prometheus 抓 `GET /metrics`（非回环须设 `metrics_scrape_token` 并带 `Authorization: Bearer`）。
- 存活 `GET /healthz`、就绪 `GET /readyz`、聚合健康 `GET /api/health`（登录）。
- 数据保留后台自动裁剪历史表（保留天数经 `settings` 可调）。
