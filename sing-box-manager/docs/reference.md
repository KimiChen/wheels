# 参考手册（Reference）

单一权威：固定端口、环境变量、文件布局、`.gitignore` 约定、CLI 子命令。所有其他文档以此为准。

## 架构一图

```
客户端 ──► 香港普通 VPS（Manager + Entry）──► DMIT（Node 中继）──► 家宽出口（Node/Landing）
              │  控制面(9736 Web/API)               全程走公网中继，不使用 WireGuard
              │  Entry 入站(19736 SS-2022 EIH)
              └──mTLS(各主机 Agent :39736)──► 编排 / 统计 / 部署 / reconcile
```

- **单二进制两模式**：`server`（Manager 控制面，SQLite 为真相源）/ `agent`（被动 mTLS 主机代理，只监听本机回环 SSM）。
- **Manager 主动，Agent 被动**：所有状态查询、命令派发、发布、reconcile、计量均由 Manager 发起；Agent 从不主动连 Manager。

## 固定端口

| 端口 | 角色 | 协议 | 说明 |
|---|---|---|---|
| `9736` | Manager Web/API | HTTP(S) | 管理面（会话认证）+ 公开订阅 `/sub/{token}` + `/metrics`/`/healthz`/`/readyz` |
| `19736` | Entry 入站 | TCP+UDP | 客户端 SS-2022 共享入站（EIH，managed，SSM 动态增删用户） |
| `29736` | Node 中继 | TCP+UDP | 中继/出口 sing-box 入站 |
| `39736` | Agent | TCP (mTLS) | Manager→Agent 唯一通道；双向证书 + host_id/SPKI pin |
| `49736` | SSM API | HTTP (回环) | sing-box shared-inbound 管理 API（仅本机 Agent 访问） |

防火墙：`39736` 仅放行 Manager 源 IP；`49736` 仅回环；`19736/29736` 面向数据面；`9736` 建议置于 TLS 反代之后。

## 环境变量

### Manager（server 模式）
| 变量 | 必需 | 默认 | 说明 |
|---|---|---|---|
| `DATABASE_PATH` | 是 | — | SQLite 路径（WAL）。 |
| `MANAGER_LISTEN` | 否 | `127.0.0.1:9736` | Web/API 监听。生产置于 TLS 反代之后。 |
| `ENCRYPTION_MASTER_KEY` | 是 | — | 当前主密钥，base64 的 32 字节。**不入库**，信封加密根。 |
| `ENCRYPTION_MASTER_KEY_VERSION` | 否 | `1` | 当前主密钥版本号。 |
| `ENCRYPTION_MASTER_KEY_V{n}` | 否 | — | 历史主密钥（仅解密），主密钥轮换期间提供。 |
| `ADMIN_BOOTSTRAP_USER` / `ADMIN_BOOTSTRAP_PASSWORD` | 否 | — | 首启无管理员时引导 admin（密码 ≥12）。否则用一次性 `POST /api/auth/setup`。 |
| `SECURE_COOKIES` | 否 | `true` | 会话 Cookie 是否带 `Secure`（生产必开，需 TLS）。 |
| `SESSION_IDLE_TTL_SECS` | 否 | `3600` | 会话滑动过期。 |
| `SESSION_ABSOLUTE_TTL_SECS` | 否 | `43200` | 会话硬顶（不随访问续期）。 |
| `REAUTH_WINDOW_SECS` | 否 | `300` | 敏感操作 re-auth 有效窗口。 |
| `LOGIN_LOCK_THRESHOLD` / `LOGIN_LOCK_SECS` | 否 | `5` / `900` | 登录失败锁定。 |
| `SESSION_COOKIE_NAME` | 否 | `sbm_session` | 会话 Cookie 名。 |

### Agent（agent 模式）
| 变量 | 必需 | 说明 |
|---|---|---|
| `AGENT_ENROLLMENT_PATH` | 是 | enrollment 包（含服务端证书/私钥/Manager pin），0600。 |
| `AGENT_STATE_PATH` | 是 | Agent 本地 SQLite（命令幂等/计量 outbox/本地 revision）。 |
| `AGENT_SSM_ADDRESS` | 否 | 本机 SSM/探测地址，默认回环 `49736`（Node 无 SSM 时指其 SS 端口）。 |
| `AGENT_CONFIG_DIR` | 是 | live `config.json` 与 `revisions/` 快照目录。 |
| `AGENT_BIND` | 否 | Agent 监听，默认 `0.0.0.0:39736`。 |

### 运行期可调项（settings 表，非 env）
告警阈值与保留策略经 `settings` 表读改（read-with-default）：`retention_{audit=365,health=30,snapshots=14,commands=30,batches=90}_days`、`retention_interval_secs`(21600)、`retention_batch_size`(500)、`metrics_scrape_token`(空=依赖回环)、`metrics_per_host`(false) 等。

## CLI 子命令

```
sing-box-manager server                 # Manager 控制面
sing-box-manager agent                  # 主机 Agent（被动 mTLS）
sing-box-manager key-rotation status    # 主密钥轮换：查看各表待迁移密文数与可否退休旧密钥
sing-box-manager key-rotation run       # 把库内全部信封密文 re-seal 到当前主密钥版本（幂等可续跑）
```

## 数据布局

- Manager 库：`DATABASE_PATH`（+ `-wal`/`-shm`）。9 个迁移（0001–0009）。
- Agent 库：`AGENT_STATE_PATH`（3 个 agent 迁移）。与 Manager 库物理隔离，Agent 不连 Manager 库。
- 信封密文只在三张表：`credential_versions`、`ca_keypairs`、`config_artifacts`（列 `alg,key_version,nonce,ciphertext`）。
- Agent 证书私钥只在 enrollment 文件（0600），不落 Manager 库。

## `.gitignore` 铁律（严禁提交公开 Git）

`.env`、`*.db*`、`*.pem`、`*.key`、`*.enroll.json`、`**/enrollment/`、`agent-state.db*`、`ssm-cache.json`。
主密钥（`ENCRYPTION_MASTER_KEY`）永不入库、永不入 Git、永不入日志。
