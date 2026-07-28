# 架构与实现机制

本文说明 `sing-box-manager` 的组件边界、状态模型、配置编译、部署、用户同步和流量结算机制。

## 组件边界

```text
┌──────────────────────── 管理主机 ────────────────────────┐
│                                                         │
│  config/*.toml ─► CLI ─► SQLite ─► Controller           │
│                  │                 │                     │
│                  │                 ├─ 轮询 Agent          │
│                  │                 ├─ 派发预定义命令      │
│                  │                 ├─ 用户 reconcile      │
│                  │                 ├─ 流量计量            │
│                  │                 └─ 订阅/健康/指标 HTTP │
│                  │                                       │
│                  └─ 编译配置 + 本机 sing-box check       │
└──────────────────────────┬───────────────────────────────┘
                           │ mTLS
          ┌────────────────┴────────────────┐
          ▼                                 ▼
   Entry/Node Agent                  Entry/Node Agent
          │                                 │
          └─ 本机配置、进程、SSM            └─ 本机配置、进程、SSM
                           │
                           ▼
                        sing-box
```

职责分离：

- CLI 是唯一管理入口，负责 plan、apply、enrollment、status 和显式部署。
- SQLite 是 Controller 的期望状态、加密秘密、观测状态和部署历史存储。
- Controller 是单写者后台循环，不接受运行时管理请求。
- Agent 是被动执行端，只操作本机 sing-box 和本地状态。
- sing-box 承担全部代理数据面，本项目不转发代理流量。

## 声明式配置模型

清单分成五个领域：

```text
servers
  ├─ listeners（Entry）
  └─ relays.chain（Node）

listeners ─► relays ─► users.relays
```

映射到状态库：

| 清单对象 | 状态对象 | 关键不变量 |
|---|---|---|
| `servers.<id>` | Host + Agent 地址 | Host 名称唯一 |
| `listeners[]` | Entry | 每个 Host 最多一个 Entry，端口固定 `19736` |
| relay chain 中的 server | Node | 每个 Host 最多一个 Node，端口固定 `29736` |
| `relays[]` | Route + RouteHop | hop 有序、无重复、最后一个 Node 为出口 |
| `users[]` | User | 用户名唯一、配额非负 |
| `users[].relays` | UserRoute | 每个“用户 × Route”独立身份和凭据 |

`plan` 只读取文件并完成结构、未知字段、include 安全、引用和语义校验，不访问数据库或网络。

`apply` 执行幂等同步：

1. 按 server ID 查找或创建 Host。
2. 根据 listener/chain 补充 Entry、Node 能力和 Agent 管理地址。
3. 创建或更新 Entry、Node。
4. 把 relay chain 转成有序 RouteHop 和最终出口。
5. 创建或更新用户。
6. 增加缺少的授权；撤销授权时执行 active Route 保护。
7. 生成并加密保存首次凭据。

`apply` 不自动删除清单外对象。这样可以避免拼写、临时缺文件或部分清单导致破坏性删除。

## 密钥和凭据

主密钥来自环境变量：

```text
ENCRYPTION_MASTER_KEY
ENCRYPTION_MASTER_KEY_VERSION
ENCRYPTION_MASTER_KEY_V<历史版本>
```

主密钥只存在于进程环境，不写入 SQLite。以下数据使用 XChaCha20-Poly1305 信封加密：

- Entry/Node Shadowsocks PSK。
- 每个用户 Route 的 Shadowsocks uPSK 或 VLESS UUID。
- CA 私钥和 Controller 客户端身份。
- 编译后的 sing-box 配置 artifact。
- Landing 认证信息。

数据库记录密钥版本、nonce 和 ciphertext。轮换时，新写入使用当前版本，旧版本只用于解密；
`key-rotation run` 幂等地把旧密文 re-seal 到当前版本。

订阅 token 例外：明文只在创建或轮换时返回一次，数据库只保存 SHA-256。

## PKI 与 Agent 信任

Controller 首次需要时幂等创建两套 CA：

- Agent CA：签发 Agent 服务端证书。
- Client CA：签发 Controller 调用 Agent 时使用的客户端证书。

enrollment 流程：

```text
apply 建 Host
  → enrollment issue
  → 为 Host 签发 Agent 服务端证书
  → 生成含私钥、Client CA、Controller SPKI pin 的 0600 文件
  → 带外分发并核对 package fingerprint
  → enrollment trust
```

Agent 侧同时验证 Client CA 和 Controller 客户端证书 SPKI；Controller 侧验证 Agent CA、
Host URI SAN，并可核对叶证书 SPKI。证书处于 `pending`、`revoked` 或临期状态时，部署门禁失败。

## 配置编译

每个 Entry 独立形成一个编译快照：

```text
Entry
  ├─ 与它关联的全部 Route
  ├─ 每条 Route 的有序 Node
  ├─ 最终出口
  └─ 已授权身份
```

Shadowsocks Entry 编译为：

- 一个监听 `19736` 的 managed inbound。
- 每条 Route 对应的有序 outbound detour 链。
- `auth_user → outbound` 路由规则。
- 未匹配身份的 `block` 兜底。
- 回环 `49736` 的 SSM API。

Node 编译为：

- 一个监听 `29736` 的 Shadowsocks-2022 inbound。
- direct outbound。

相同 Entry 下共享的 Node 链路前缀会按确定性 tag 去重。对象和规则排序稳定，规范 JSON 用于
内容哈希和 revision 去重。

artifact 明文不会进入命令队列表。数据库保存的是信封密文和内容 SHA-256；部署时才在内存中
解封，经 mTLS 发送给目标 Agent。

## 部署状态机

显式 `apply --deploy` 的顺序：

```text
所有 Entry 编译
  → Controller 本机逐 artifact 运行 sing-box check
  → 全部目标 Agent 门禁
  → 建立 deployment 和 target
  → 获取相关 Entry 独占租约
  → Node 批次
  → Entry 批次
  → 激活 Route、分配运行 epoch
  → 释放锁
  → Shadowsocks 用户 reconcile
```

门禁要求：

- Agent 已登记。
- 证书为 trusted 且未临期。
- 最近轮询在线且状态未过期。
- 已有活动 revision 时 sing-box 当前运行正常；首次部署尚无进程时不阻断。

Agent 应用单个 artifact：

```text
校验 SHA-256
  → 本机再次 sing-box check
  → 必要时进入流量结算屏障
  → 写 revision 快照
  → 同目录临时文件 + rename 原子替换 config.json
  → 启动新 sing-box epoch
  → 健康检查（SS Entry 检查进程和 SSM；VLESS Entry、Node 检查进程）
  → 成功记账 / 失败自动回滚
```

命令使用稳定 command ID 实现幂等。Controller 超时后会先询问 Agent 是否已经完成，避免重复执行。

## Shadowsocks 用户同步

结构配置包含 Route 与 `auth_user` 规则，运行中的实际用户集合由 SSM 管理。

Controller 计算某 Entry 的期望集合：

```text
已授权
AND Route active
AND 用户 enabled
AND 未过期
AND 未超额
```

然后把完整期望集合发送给 Agent。Agent 对比 SSM 当前用户并增删差异。每次 reconcile 使用新的
command ID，确保 Agent 或 sing-box 重启导致内存用户集丢失后可以重新注入。

VLESS 用户 UUID 随 revision 静态编译进 inbound，不进入 SSM reconcile。用户停用、到期或
授权变化后，需要重新执行 `apply --deploy` 发布新 revision。

## 流量计量与结算屏障

sing-box SSM 返回用户累计流量。Controller 以以下键保存上次值：

```text
Entry + 用户身份 + Agent boot/运行 epoch
```

本次增量为：

```text
max(0, 当前累计值 - 上次累计值)
```

批次有唯一键，重复上报不会重复计费。新运行 epoch 从零开始，不会产生负增量。

重启 Entry 会清空内存累计值，因此部署采用两阶段结算屏障：

1. Agent 暂存新配置，保持旧进程运行。
2. Agent 获取最终统计批并持久化到本地 outbox。
3. Controller 幂等入账并发送 meter-ack。
4. Agent 收到确认后才切换新配置和新 epoch。

若 Controller 在中间崩溃，最终批仍保存在 Agent，本次部署可以幂等重驱动。

## 订阅生成

Controller 根据 token hash 找到用户，只输出：

- 用户当前可用。
- Route 状态为 active。
- Entry 协议为 Shadowsocks。

每条 Route 生成一个独立代理项：

```text
password = Entry server PSK : 用户 Route uPSK
```

订阅响应带 `no-store`、CSP、`noindex` 等安全头。停止、过期或超额用户得到空代理集。

## HTTP 边界

生产 Controller 只挂载：

| 路径 | 用途 |
|---|---|
| `/sub/{token}` | 用户订阅 |
| `/healthz` | 进程存活 |
| `/readyz` | SQLite 可达 |
| `/metrics` | Prometheus 指标 |

拓扑、用户、PKI、部署和密钥操作均无公开管理 API，只能通过本机 CLI 执行。

## 主要安全不变量

- Agent 不执行任意 shell，只接受固定类型命令和固定 argv。
- SSM API 只监听回环。
- 配置和凭据不写入日志、审计明文或命令队列。
- 未完成 check、门禁或结算屏障时，不激活新 Route。
- Node 先于 Entry 部署；失败时回滚已应用目标。
- 一个 SQLite 只允许一个 Controller 写入。
- VLESS Reality 私钥、short ID、用户 UUID 和含密钥 artifact 必须信封加密。
- VLESS Entry 不进入 SSM 计量与重启结算屏障；其健康检查要求受管进程存活。
- 授权 VLESS relay 的用户必须是无配额模式，避免产生无法执行的限额。
