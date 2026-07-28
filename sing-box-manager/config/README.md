# 声明式配置说明

`config/config.toml` 是唯一入口，其他文件按领域拆分并通过 `includes` 合并。真实配置只保留在
管理员本机；公开仓库只跟踪 `example.*.toml`。

## 文件布局

| 本地文件 | 公开示例 | 作用 |
|---|---|---|
| `config.toml` | `example.config.toml` | 格式版本、include 顺序、SSH 默认值、目标 sing-box 版本 |
| `servers.toml` | `example.servers.toml` | SSH 管理地址和 sing-box 数据面地址 |
| `protocols.toml` | `example.protocols.toml` | Shadowsocks-2022 与 VLESS-Reality 模板 |
| `listeners.toml` | `example.listeners.toml` | Entry 实际监听的协议、地址和固定端口 |
| `relays.toml` | `example.relays.toml` | listener 到最终出口的有序 Node 链 |
| `users.toml` | `example.users.toml` | 用户状态、配额和允许使用的转发链 |

复制示例：

```bash
for name in config servers protocols listeners relays users; do
  cp "config/example.${name}.toml" "config/${name}.toml"
done
chmod 600 config/*.toml
```

示例清单默认只启用 Shadowsocks listener；`example.protocols.toml` 同时给出可启用的
VLESS-Reality 模板。

## 入口配置

```toml
formatVersion = 1

includes = [
  "servers.toml",
  "protocols.toml",
  "listeners.toml",
  "relays.toml",
  "users.toml",
]

sshKey = "~/.ssh/id_ed25519"
knownHosts = "~/.ssh/known_hosts"
nodePort = 29736
singboxVersion = "1.13.14"
```

当前行为：

- `formatVersion` 只支持 `1`。
- `nodePort` 必须是 `29736`。
- include 必须是同目录安全相对文件名，不能绝对路径、不能含 `..`、不能重复。
- include 文件之间不能重复定义同一个顶层字段。
- `sshKey`、`knownHosts` 和 `servers.*.ssh` 已校验但尚未接入自动装机。
- `singboxVersion` 会写入编译产物元数据；真正 check 使用 `SINGBOX_BIN` 指向的本机二进制。

## 服务器

```toml
[servers.entryA]
ssh = "ssh://root@entry-a.example.com"
address = "entry-a.example.com"

[servers.relayHk]
ssh = "ssh://root@relay-hk.example.com:2222"
address = "relay-hk.example.com"
```

- server ID 只能包含 ASCII 字母、数字、`-` 和 `_`。
- `ssh` 必须以 `ssh://` 开头。
- `address` 是 sing-box 数据面地址，不自动继承 SSH 端口。
- 同一 server 可以同时具有 Entry 和 Node 能力，但一条 relay 不能再次经过自己的 Entry server。

## 协议模板

Shadowsocks-2022：

```toml
[protocols.shadowsocks]
method = "2022-blake3-aes-128-gcm"
serverKey = "auto"
managed = true
```

支持的方法：

- `2022-blake3-aes-128-gcm`
- `2022-blake3-aes-256-gcm`
- `2022-blake3-chacha20-poly1305`

当前要求 `serverKey = "auto"`、`managed = true`。自动生成的 server PSK 和用户 uPSK
只加密保存在 SQLite，不回写 TOML。

VLESS-Reality：

```toml
[protocols.vless]
flow = "xtls-rprx-vision"
privateKey = "auto"
shortId = "auto"
serverName = "itunes.apple.com"
handshakeServer = "itunes.apple.com"
handshakePort = 443
clientFingerprint = "chrome"
```

`privateKey` 和 `shortId` 只接受 `auto`。首次 `apply` 为每个 VLESS Entry 生成 Reality
X25519 密钥与 short ID，私钥和 short ID 信封加密保存并稳定复用；每个“用户 × relay”
生成独立加密 UUID。`clientFingerprint` 默认 `chrome`，也接受 `firefox`、`edge`、`safari`、
`360`、`qq`、`ios`、`android`、`random` 和 `randomized`。

## Listener

```toml
[[listeners]]
name = "entry-a-ss"
server = "entryA"
protocol = "shadowsocks"
bind = "::"
port = 19736
```

规则：

- `name` 全局唯一。
- `server` 必须引用已声明服务器。
- `protocol` 只接受 `shadowsocks` 或 `vless`，并且对应协议模板必须存在。
- `bind` 当前只接受 `::` 或 `0.0.0.0`。
- `port` 必须是固定 Entry 端口 `19736`。
- 同一 server 的同一端口只能有一个 listener。

多个用户和多条线路共享同一个 listener，通过认证身份和 `auth_user` 路由规则区分，不为每条
线路重复监听端口。

## Relay

```toml
[[relays]]
name = "entry-a-jp"
listener = "entry-a-ss"
chain = ["relayHk", "exitJp"]
```

`chain` 只列 Entry 之后的 Node：

```text
entryA:19736 → relayHk:29736 → exitJp:29736 → direct
```

规则：

- `name` 全局唯一。
- `listener` 必须存在。
- `chain` 至少包含一个 server，最后一个 server 是最终出口。
- 同一 chain 不能重复 server，也不能再次包含 Entry server。
- chain 中每个 server 都会获得 Node 能力并使用固定端口 `29736`。

## 用户与授权

```toml
[[users]]
name = "userA"
enabled = true
quotaBytes = 0
resetCycle = "monthly"
relays = ["entry-a-hk", "entry-a-jp", "entry-a-us"]
```

字段：

- `name`：全局唯一。
- `enabled`：默认 `true`。
- `quotaBytes`：默认 `0`，表示不设配额上限；不能为负数。
- `resetCycle`：`monthly`、`yearly` 或 `never`，默认 `monthly`。
- `expireAt`：可选 UTC Unix 秒。
- `relays`：该用户允许使用的 relay 白名单，不能重复且必须存在。

授权内部展开为：

```text
userA × entry-a-hk → 独立身份与凭据
userA × entry-a-jp → 独立身份与凭据
userB × entry-a-hk → 另一套独立身份与凭据
```

Shadowsocks 授权生成独立 uPSK；VLESS 授权预生成独立 UUID。所有明文凭据使用主密钥信封加密。
首次创建用户时，订阅 token 只输出一次。

VLESS 当前不提供 SSM 式 per-user 流量计量。只要用户授权了任一 VLESS relay，
`quotaBytes` 就必须为 `0`；非零值会在 `plan`/`apply` 阶段被拒绝。

## 合并、校验与应用

```bash
# 无数据库、无网络副作用
sing-box-manager plan --config config/config.toml

# 幂等同步期望状态
sing-box-manager apply --config config/config.toml

# Shadowsocks 与 VLESS-Reality 的完整发布
sing-box-manager apply --config config/config.toml --deploy
```

`apply` 的边界：

- 创建或更新清单明确声明的 Host、Entry、Node、Route、用户和授权。
- 不删除清单外对象，也不自动删除清单中已移除的服务器或线路。
- active Route 的授权撤销需要结算屏障，无法安全完成时拒绝变更。
- 第一次创建用户必须先执行普通 `apply`，保存一次性 token 后再部署。
- `--deploy` 先编译并 check 全部 artifact，再检查全体 Agent 门禁，最后按 Node→Entry 发布。

## 安全要求

- 不把真实 IP、域名、用户名、SSH 路径或拓扑提交到公开 Git。
- SSH 必须校验 `known_hosts`，不能使用 `StrictHostKeyChecking=no`。
- 文件建议权限 `0600`，父目录仅允许 Controller 系统用户读取。
- Reality 私钥、short ID、Shadowsocks PSK、用户凭据和订阅 token 不应手工写入 TOML。
- 提交前运行：

```bash
git status --short --ignored config
git diff --cached -- config
```
