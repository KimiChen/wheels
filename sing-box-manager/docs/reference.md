# 参考手册

本页集中列出 CLI、环境变量、端口、文件和运行约束。配置字段详见
[声明式配置说明](../config/README.md)。

## 命令总览

```text
sing-box-manager plan [OPTIONS]
sing-box-manager apply [OPTIONS]
sing-box-manager status [OPTIONS]
sing-box-manager controller [OPTIONS]
sing-box-manager agent
sing-box-manager enrollment <COMMAND>
sing-box-manager key-rotation <COMMAND>
```

隐藏兼容命令 `server` 等同于 `controller`，新部署不要继续使用。

所有失败返回非零退出码。`RUST_LOG` 控制结构化日志过滤，默认相当于：

```text
info,sqlx=warn
```

## plan

```text
sing-box-manager plan \
  [--config <PATH>] \
  [--json]
```

默认配置：`config/config.toml`。

行为：

- 按 `includes` 合并拆分 TOML。
- 拒绝未知字段、不安全路径、重复定义和无效引用。
- 校验协议、固定端口、listener 冲突、relay chain 和用户授权。
- 不读取环境变量，不打开数据库，不连接 Agent。

`--json` 输出：

```json
{
  "config_path": "config/config.toml",
  "servers": 4,
  "listeners": 1,
  "nodes": 3,
  "relays": 3,
  "users": 2,
  "grants": 6,
  "protocols": ["shadowsocks"],
  "warnings": []
}
```

## apply

```text
sing-box-manager apply \
  [--config <PATH>] \
  [--json] \
  [--deploy]
```

不带 `--deploy`：

- 幂等同步 Host、Entry、Node、Route、User 和 UserRoute。
- 首次创建并加密保存自动凭据。
- 首次创建用户时输出一次性订阅 token。
- 不删除清单外对象。

带 `--deploy`：

1. 拒绝首次创建用户，要求先普通 `apply` 保存 token。
2. 同步期望状态和加密凭据。
3. 为全部 Shadowsocks/VLESS Entry 编译 revision 和 artifact。
4. 在 Controller 本机逐个执行真实 `sing-box check`。
5. 检查全部目标 Agent 门禁。
6. 按 Node→Entry 驱动 mTLS 部署。

`--json` 返回各类 created/updated/revoked 计数、首次 token、Entry ID、checked revision 和
succeeded deployment ID。该输出可能包含首次 token，不要写入公开 CI 日志。

## status

```text
sing-box-manager status [--json]
```

需要 `DATABASE_PATH`，不需要主密钥。文本模式输出对象数量、Route 状态和 Agent 状态；
JSON 模式输出 Host、Entry、Node、Route、User 和 Agent 完整非秘密视图。

## controller

```text
sing-box-manager controller [--config <PATH>]
```

启动顺序：

1. 加载并校验清单。
2. 读取 Controller 环境。
3. 打开 SQLite 并执行迁移。
4. 幂等引导双 CA 和 Controller 客户端身份。
5. 启动轮询、命令派发、计量、保留策略和启动 reconcile。
6. 监听订阅、健康检查和指标 HTTP。

Controller 不隐式执行 `apply`，也不监听管理 API。

## agent

```text
sing-box-manager agent
```

Agent 从环境读取 enrollment、本地数据库、配置目录和 SSM 地址，启动 mTLS 服务并等待
Controller 调用。Agent 不主动连接 Controller。

## enrollment

签发：

```text
sing-box-manager enrollment issue \
  [--config <PATH>] \
  --server <SERVER_ID> \
  --output <FILE>
```

- server 必须已通过 `apply` 写入状态库。
- output 必须不存在；命令使用 `create_new`，Unix 权限为 `0600`。
- 每次签发都会让该 Host 的当前证书回到 `pending`。
- 输出 package fingerprint，供带外核对。

授信：

```text
sing-box-manager enrollment trust \
  --server <SERVER_ID> \
  --fingerprint <SHA256>
```

只有 fingerprint 与该 Host 最近一次 enrollment 完全匹配时才标记 `trusted`。

吊销：

```text
sing-box-manager enrollment revoke --server <SERVER_ID>
```

吊销后轮询和部署门禁拒绝该 Agent，现有 sing-box 数据面不会被自动停止。

## key-rotation

状态：

```text
sing-box-manager key-rotation status
```

打印三类信封密文中不属于当前密钥版本的数量。全部为零时，才可退休历史密钥。

执行：

```text
sing-box-manager key-rotation run
```

把旧版本密文幂等 re-seal 到 `ENCRYPTION_MASTER_KEY_VERSION` 指定的当前版本。轮换期间必须同时
提供旧密钥 `ENCRYPTION_MASTER_KEY_V<n>`。

建议在 Controller 停机、确认单写者并完成一致性备份后执行。

## Controller 环境变量

| 变量 | 必需 | 默认 | 说明 |
|---|---|---|---|
| `DATABASE_PATH` | 是 | — | SQLite 状态库；父目录不存在时自动创建 |
| `ENCRYPTION_MASTER_KEY` | 是 | — | 当前 32 字节主密钥的 base64；绝不能入库或入 Git |
| `ENCRYPTION_MASTER_KEY_VERSION` | 否 | `1` | 当前写入密钥版本，正整数 |
| `ENCRYPTION_MASTER_KEY_V<n>` | 轮换期 | — | 历史版本解密密钥 |
| `MANAGER_LISTEN` | 否 | `127.0.0.1:9736` | 订阅、健康和指标 HTTP 地址 |
| `SINGBOX_BIN` | 部署时 | `sing-box` | Controller 本机 `sing-box check` 使用的可执行文件 |
| `RUST_LOG` | 否 | `info,sqlx=warn` | tracing 日志过滤表达式 |

项目不自动读取 `.env`。systemd 使用 `EnvironmentFile=`；shell 可用受控方式显式加载。

## Agent 环境变量

| 变量 | 必需 | 默认 | 说明 |
|---|---|---|---|
| `AGENT_ENROLLMENT_PATH` | 是 | — | enrollment JSON，建议 `0600` |
| `AGENT_BIND_ADDRESS` | 否 | `127.0.0.1:39736` | Agent mTLS 监听地址 |
| `AGENT_STATE_PATH` | 否 | `agent-state.db` | Agent 独立 SQLite |
| `AGENT_SSM_ADDRESS` | 否 | `127.0.0.1:49736` | 本机 SSM API 和健康探测地址 |
| `AGENT_CONFIG_DIR` | 否 | `/var/lib/sing-box-manager` | live 配置和 revision 快照目录 |
| `SINGBOX_BIN` | 否 | `sing-box` | check、run 和版本探测使用的可执行文件 |
| `RUST_LOG` | 否 | `info,sqlx=warn` | tracing 日志过滤表达式 |

当 `AGENT_BIND_ADDRESS` 设为非回环地址时，必须用主机防火墙只允许 Controller 源地址。

## 固定端口

| 端口 | 角色 | 访问来源 | 说明 |
|---:|---|---|---|
| `9736/tcp` | Controller HTTP | 本机或受控 TLS 反代 | `/sub`、`/healthz`、`/readyz`、`/metrics` |
| `19736/tcp+udp` | Entry | 代理客户端 | 所有用户和 Route 共享 |
| `29736/tcp+udp` | Node | 直接上游 Entry/Node | SS-2022 中继入站 |
| `39736/tcp` | Agent | 仅 Controller | 双向 mTLS |
| `49736/tcp` | SSM | 仅本机 Agent | 必须回环 |

## Controller HTTP

| 方法与路径 | 认证 | 响应 |
|---|---|---|
| `GET /sub/{token}` | 高熵 token | raw、Clash YAML 或 HTML |
| `GET /healthz` | 无 | `ok` |
| `GET /readyz` | 无 | SQLite 可达时 `200 ready` |
| `GET /metrics` | 默认依赖回环边界 | Prometheus 文本 |

生产 Controller 路由不包含 `/api/auth/*`、`/api/hosts/*`、`/api/users/*` 或其他管理接口。

订阅格式参数：

| 参数 | 作用 |
|---|---|
| `target=raw` | base64 的 `ss://` 行列表 |
| `target=clash` | Clash/mihomo YAML |
| 未指定且浏览器 UA | HTML 状态页 |

## 文件布局

Controller：

```text
config/*.toml                 本地真实清单
DATABASE_PATH                 SQLite + -wal + -shm
```

Agent：

```text
AGENT_ENROLLMENT_PATH         证书、私钥、Controller pin
AGENT_STATE_PATH              命令幂等、revision、计量 outbox
AGENT_CONFIG_DIR/config.json  当前 live 配置
AGENT_CONFIG_DIR/revisions/   成功 revision 快照
```

Controller 数据库中的密文主要位于：

- `credential_versions`
- `ca_keypairs`
- `config_artifacts`
- `entry_reality`

主密钥、真实配置、数据库、证书、enrollment、生成配置和 SSM cache 都不得提交公开 Git。

## 固定运行约束

- `formatVersion = 1`
- Entry 端口 `19736`
- Node 端口 `29736`
- Agent 端口 `39736`
- SSM 端口 `49736`
- Shadowsocks Entry 必须 `managed = true`
- 单 SQLite 单 Controller
- Node 先于 Entry 部署
- 未通过 check、门禁或结算屏障时不激活 Route
- VLESS 授权用户必须 `quotaBytes = 0`
