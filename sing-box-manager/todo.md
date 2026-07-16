# sing-box-manager Web 管理平台改造计划

## 1. 已确定的设计决策

- 项目尚未上线，不保留现有 TOML 配置兼容层，按新模型直接重构。
- Manage 使用 SQLite，所有业务配置、拓扑、用户、密钥密文、发布记录和流量状态均以 SQLite 为唯一事实来源。
- 物理服务器统一建模为 Host；`manage`、`entry`、`node` 是可叠加的逻辑能力，不是互斥的机器类型。
- Manage 所在 Host 可以同时承担 Entry，提供客户端入口、直接出站和多跳 Route。
- Manager 与 sing-box 始终保持独立进程；Manage 作为 Entry 时，在同一 Host 上运行 Manager、Agent 和独立的 sing-box。
- 每台承载 Entry 或 Node 的 Host 必须运行 Agent；外部 SOCKS5 Landing 不属于 Node，不安装 Agent。
- Agent 使用被动模式，只监听配置指定的管理地址并等待 Manager 调度，不主动连接 Manager、不主动拉取任务、不主动上报状态。
- 第一阶段只支持 Shadowsocks-2022 + SSM；VLESS-Reality 放到基础控制面稳定后实现。
- 第一阶段每个 Entry 使用一个公网端口和一个 SSM managed inbound；每个“用户 × Route”身份通过 `auth_user` 规则选择线路。
- Web 增删用户或修改 Route 授权允许重新发布并重启相关 Entry，但必须先完成流量结算屏障，保证重启前后的累计统计不丢失、不重复。
- Route 明确按数据方向声明：`Entry -> Hops -> Exit`。
- 单活 Manager 负责写 SQLite 和执行控制循环；暂不支持多个 Manager 同时写同一个数据库。

## 2. 系统边界

```text
客户端 --获取订阅--> Manager Web/API
Manager --调度/轮询--> Agent --本机操作--> sing-box
Manager --读取 Agent 返回的 SSM 统计--> SQLite

客户端 --代理流量--> Entry
Entry --Route--> 0..N 个 Node --最终出口--> Entry / Node / Landing --目标地址
```

Manager 不转发代理数据。Manage Host 同时作为 Entry 时，代理流量也只进入本机 sing-box，不经过 Manager HTTP 服务。

推荐的进程布局：

```text
Manage Host
  sing-box-manager server     Web、API、订阅、控制器、计量、SQLite
  sing-box-manager agent      本机 Entry 的配置发布和状态采集
  sing-box                    客户端入口和数据转发

Entry/Node Host（必须安装 Agent）
  sing-box-manager agent
  sing-box
```

被动 Agent 要求 Manager 能直接访问所有 Entry/Node Host 的 Agent 管理地址。Agent 端口必须通过来源防火墙限制为仅 Manager 可访问。

### 2.1 固定端口规划

第一阶段统一使用以下固定端口，不为单个 Host 或 Route 分配其他端口：

| 服务 | 端口 | 监听范围 | 访问来源 |
|---|---:|---|---|
| Manager Web/API | `9736/tcp` | Manage 本机或可信管理地址 | 反向代理、管理员和订阅调用方 |
| Entry 代理入口 | `19736/tcp+udp` | Entry 公网地址 | 代理客户端 |
| Node 中继 | `29736/tcp+udp` | Node 数据面地址 | Route 中的直接上一跳 |
| Agent API | `39736/tcp` | 配置指定的管理地址；Manage 本机 Agent 可监听回环地址 | 仅 Manager |
| sing-box SSM API | `49736/tcp` | `127.0.0.1` | 仅本机 Agent |

端口安全要求：

- Entry 的 `19736/tcp+udp` 是唯一客户端入口端口，同一 Entry 的全部 Route 共用该端口。
- Node 的 `29736/tcp+udp` 只允许 Route 中的直接上一跳访问，不向任意公网来源开放。
- Agent 的 `39736/tcp` 必须使用 mTLS，并由防火墙限制为 Manager 管理地址可访问。
- SSM 的 `49736/tcp` 只能监听 `127.0.0.1`，Manager 不直接跨网络访问 SSM。
- 同一 Host 同时承担 Manage、Entry 或 Node 时，各进程仍使用上述独立端口，不得自动改为其他端口规避冲突。

## 3. SQLite 设计要求

### 3.1 运行参数

- 启用 `PRAGMA journal_mode=WAL`。
- 启用 `PRAGMA foreign_keys=ON`。
- 设置 `busy_timeout`，默认 5 秒。
- 默认使用 `synchronous=NORMAL`；涉及密钥轮换和配置发布的关键事务允许使用更严格策略。
- 连接池保持较小规模，建议最多 4 个连接。
- 所有结构变更通过版本化 migration 执行。
- 所有控制任务必须幂等，避免进程重启后重复发布或重复累计流量。
- 定期执行 checkpoint，并监控数据库、WAL 和磁盘空间。

### 3.2 最小启动配置

业务配置全部进入 SQLite，但以下启动信息不能依赖数据库自身，放在项目 `.env` 或 systemd 环境中：

```text
DATABASE_PATH
ENCRYPTION_MASTER_KEY
MANAGER_LISTEN
```

`.env`、SQLite、WAL、密钥和 Agent 证书必须保持 ignored，不得提交到公开 Git。

### 3.3 备份

- 使用 SQLite Backup API 或 `VACUUM INTO` 创建一致性备份，不直接复制正在写入的数据库文件。
- 配置发布、密钥轮换、用户批量变更前自动创建备份点。
- 备份文件加密，并设置保留数量和过期清理策略。
- 恢复测试必须覆盖数据库、加密主密钥和 Agent 信任关系。

## 4. 核心领域模型

### 4.1 Host 与能力

`hosts` 表表示物理服务器，不直接限定角色：

- Host 可以拥有 `manage`、`entry`、`node` 中的一个或多个能力。
- Manage Host 可以同时创建一个或多个 Entry 实例。
- Node 可以在一条 Route 中作为中转，在另一条 Route 中作为最终直出节点。
- Landing 可以引用受管 Node，也可以表示外部 SOCKS5 服务。

### 4.2 Entry

Entry 保存：

- 所属 Host。
- 公网连接地址。
- 固定公网入口端口 `19736/tcp+udp`。
- 入站协议和 Shadowsocks 方法。
- Agent 当前状态。
- 当前已部署 revision。
- 是否允许作为直接出口。

Manage 直出表示为一个普通 Route：

```text
entry = manage-entry
hops  = []
exit  = entry/manage-entry
```

### 4.3 Node

Node 的 sing-box 基础配置保持统一：

```text
Shadowsocks-2022 inbound :29736/tcp+udp -> direct outbound
```

当 Route 后面还有节点时，Node 的 direct 用于连接下一跳；当 Route 在该 Node 终止时，direct 用于连接真实目标。

### 4.4 Landing

第一阶段支持：

- `managed_node`：引用一个受管 Node，以 Node 作为最终直出节点。
- `socks5`：外部 SOCKS5 服务，可配置 TCP、UDP、用户名和密码。

后续可扩展 HTTP CONNECT 等类型，但不能把协议细节直接散落到 Route 表。

### 4.5 Route

Route 是用户授权、订阅节点和流量入口的基本单位：

```text
Entry -> route_hops[position] -> Exit
```

示例：

```text
manage-direct
  manage-entry -> direct

manage-homejp
  manage-entry -> dmithk -> dmitjp -> socks5/homejp

hk-dmitjp
  hk-entry -> dmithk -> dmitjp -> direct
```

Route 校验必须覆盖：

- Entry、Hop 和 Exit 必须存在且启用。
- Hop 只能引用 Node。
- 同一路径不能重复经过同一个 Node。
- 不能形成拓扑环路。
- Exit 类型与目标对象必须一致。
- Entry 直出必须得到 Entry 配置允许。
- Route 标签在同一订阅范围内必须唯一。
- Entry 必须使用 `19736/tcp+udp`，Node 中继必须使用 `29736/tcp+udp`。
- 同一 Host 上各固定端口不能被未纳入管理的其他服务占用。
- SOCKS5 的网络能力必须满足 Route 的 TCP/UDP 要求。

## 5. 入口与端口模型

第一阶段采用每个 Entry 一个固定公网端口 `19736/tcp+udp` 和一个 managed inbound：

```text
manage-entry.example.com:19736 -> in-manage
hk-entry.example.com:19736     -> in-hk
```

同一 Entry 上所有用户和 Route 共用该端口。每个“用户 × Route”生成独立内部身份和凭据，Entry 根据 `auth_user` 路由：

```text
identity(user-a, manage-direct) -> direct
identity(user-a, manage-homejp) -> out-homejp
identity(user-b, manage-homejp) -> out-homejp
未匹配身份                         -> block
```

Entry 配置需要包含：

- 唯一 managed inbound、Entry 服务端 PSK 和公网端口。
- 该 Entry 可用的全部 Route outbound chain。
- 当前用户与 Route 身份对应的 `auth_user -> outbound` 规则。
- `route.final = block` 的失败关闭兜底。

用户和授权操作规则：

- 新增用户或新增授权：生成身份和路由规则，执行受控的 Entry 配置发布与重启。
- 删除用户或删除授权：先完成最终流量结算，再从 SSM 和配置中移除身份。
- 超额、到期和临时停用：只通过 SSM 移除身份，不需要立即重启 Entry。
- 恢复用户：当配置中仍有对应路由规则时，通过 SSM 恢复身份；否则先重新发布配置。
- 订阅中的不同 Route 使用相同 Entry 地址和端口，但凭据和显示名称不同。

允许重启不代表可以在任意时间直接覆盖配置。所有会清零 sing-box 内存计数的发布都必须执行第 9 节定义的流量结算屏障。

## 6. Agent 设计

同一二进制提供两种运行模式：

```text
sing-box-manager server
sing-box-manager agent
```

Agent 需要实现：

- 从本机配置读取稳定的 Host id、Agent 服务端证书和信任的 Manager CA。
- 固定监听 `39736/tcp`，绑定配置指定的管理地址；Manage 本机 Agent 可以绑定 `127.0.0.1:39736`。
- 不监听不可信公网接口。
- 通过 mTLS 验证 Manager 客户端证书，拒绝其他调用方。
- 被动响应 Manager 的状态查询，返回系统信息、sing-box 版本、当前 revision 和运行状态。
- 接收 Manager 主动推送的配置 artifact、revision、SHA256 和签名，不自行下载配置。
- 写入临时文件并运行 `sing-box check`。
- 在 Entry 重启前执行流量结算屏障并等待 Manager 确认入账。
- 校验成功后原子替换正式配置。
- 通过受控 stop/start 启动新的 sing-box 进程 epoch，并执行本机健康检查。
- 保留当前和上一个成功 revision，支持自动回滚。
- 通过 `127.0.0.1:49736` 访问本机 SSM API。
- 收到 Manager 统计查询后，通过 `127.0.0.1:49736` 调用本机 sing-box SSM API，并返回累计流量和入站用户状态；Agent 不计算用户用量。
- 将尚未被 Manager 确认的最终统计批次持久化到本机 outbox，确认前禁止清理或继续重启。
- 对 Entry 公网端口安装和移除固定规则的维护闸门：阻止新 TCP/UDP 会话，同时允许已有会话排空。
- 任务使用唯一 command id，重复接收时返回已有结果。

Agent 被动 API 至少包含：

```text
GET  /v1/status
GET  /v1/sing-box/stats
GET  /v1/sing-box/users
POST /v1/sing-box/reconcile
POST /v1/deployments
GET  /v1/deployments/{command_id}
POST /v1/deployments/{command_id}/meter-ack
POST /v1/rollback
```

耗时任务由 Manager 创建 command 后轮询结果。Agent 不使用长轮询连接 Manager，也不自行领取数据库任务。

Agent 权限应限制为：

- 读取自身证书和下发配置。
- 写入 sing-box 配置目录。
- 执行固定的 `sing-box check` 和服务 reload/restart 命令。
- 访问本机 `127.0.0.1:49736` SSM API。
- 执行预定义的 Entry 维护闸门操作，不能接受 Manager 传入的任意防火墙命令。

禁止 Manager 下发任意 shell 命令。

## 7. 配置版本与发布

数据库中的业务对象是期望状态，Manager 主动查询 Agent 得到的是实际状态。二者必须分开保存。

发布流程：

```text
Web 保存草稿
-> 数据库事务写入期望状态
-> 拓扑和端口校验
-> 创建不可变 config_revision
-> 为受影响 Host 编译 config_artifact
-> Manager 向 Agent 调度 artifact，Agent 执行 sing-box check
-> Entry Agent 执行流量结算屏障
-> 分批应用
-> Manager 轮询 Agent revision 和健康状态
-> 所有必需目标成功后激活 Route
-> 失败时保留旧版本或自动回滚
```

发布要求：

- 草稿、已发布、已部署三个状态不能混用。
- artifact 存储完整 sing-box JSON、SHA256、目标 Host、目标 sing-box 版本和生成时间。
- Route 只有在对应 Entry 确认部署成功后才能进入订阅。
- Node 配置先部署，Entry 配置后部署。
- Entry 重启型发布必须持有该 Entry 的独占操作锁，禁止计量任务、SSM reconcile 和另一个发布并发修改运行态。
- 流量最终批次没有得到 Manager 持久化确认时，Agent 必须中止 Entry 重启。
- 删除 Node 或 Landing 前检查是否仍被 Route 引用。
- 每次发布保存结构化 diff 和操作者。
- 支持按 Host 灰度发布和一键回滚到上一个成功 revision。

## 8. 密钥设计

密钥全部以密文存入 SQLite：

- 每个 Entry 一份服务端 PSK。
- 每个受管 Node 一份中继 PSK。
- 每个用户与 Route 一份内部身份和 uPSK。
- 每个用户一个订阅 token。
- 外部 Landing 的用户名和密码。
- Agent 服务端证书、Host 身份和 Manager 信任状态。

加密要求：

- 使用不存放在 SQLite 中的主密钥进行信封加密。
- 密文保存算法版本、key version、nonce 和 ciphertext。
- Web 默认不返回密钥明文，只允许受控的复制、轮换或重新生成操作。
- API、日志、错误、审计和 Agent 状态响应不得包含明文密钥。
- 订阅 token 使用高强度随机值，数据库至少保存可查询的哈希；如需后台再次展示，则额外保存加密值。

## 9. 流量、配额与状态

Manager 定时调度每个 Entry Agent；Agent 被动调用本机 SSM API 并返回累计统计：

```text
entry_id
inbound_tag
identity
singbox_boot_id
sequence
uplink_bytes
downlink_bytes
observed_at
```

Manager 使用以下维度保存基线：

```text
entry_id + inbound_tag + identity + singbox_boot_id
```

计量流程：

- Manager 幂等处理其主动查询 Agent 后得到的统计批次。
- 根据累计值和基线计算增量。
- 将用户所有 Entry、Route 和身份的增量汇总到当前用量周期。
- 判断配额、有效期和管理员停用状态。
- 生成用户在各 Entry managed inbound 中的期望身份集。
- 下发 SSM reconcile 任务。

### 9.1 重启前流量结算屏障

新增、删除用户或修改授权导致 Entry 配置变化时，按 Entry 独立执行：

```text
锁定 Entry 运行态
-> 暂停该 Entry 的普通 SSM reconcile
-> 安装 Entry 维护闸门，拒绝新 TCP/UDP 会话但保留已有会话
-> 保持 SSM 用户和计数器不变，等待活动连接与 UDP session 全部排空
-> 读取最终累计统计
-> Agent 将最终批次写入本机持久化 outbox，并让部署命令进入 awaiting_meter_ack
-> Manager 轮询到最终批次，在 SQLite 事务中幂等计算增量
-> Manager 调用 meter-ack，Agent 收到确认后停止旧 sing-box 进程
-> 原子替换配置并启动新进程
-> 创建新的 runtime epoch，计数基线从 0 开始
-> 按数据库期望状态恢复 SSM 身份
-> 健康检查成功后移除维护闸门并释放锁
```

正确性约束：

- Agent 未得到最终统计批次确认时不得停止旧进程。
- 删除 SSM 用户可能同时删除其累计计数，因此最终统计确认前不得删除或替换旧运行态用户集。
- 默认发布必须等待活动连接和 UDP session 归零；无法证明已排空时不得宣称流量精确结算。
- 最终批次使用唯一 `(entry_id, runtime_epoch, sequence)`，Manager 重复读取或确认不会重复计量。
- 新进程必须使用新的 runtime epoch；新 epoch 第一次统计从 0 计算，不能继承旧基线。
- 强制切换只能作为管理员显式覆盖选项，并必须在审计和部署结果中记录可能存在的未结算窗口。
- 发布失败并恢复旧配置时也应启动新 epoch，避免回滚后复用旧基线。
- 多 Entry 用户变更可以并行执行，但每个 Entry 内部必须串行。
- Agent 必须能够读取 SSM 的活动 TCP 连接和 UDP session 数；若目标 sing-box 版本不能提供这些数据，第一阶段不能启用“精确结算后重启”。

故障隔离要求：

- 单个 Entry 离线不能阻塞其他 Entry 的计量。
- 单个统计批次失败不能回滚已经成功处理的其他 Entry 批次。
- boot id 变化时按 sing-box 重启处理，不能产生负增量。
- SSM 下发失败保存重试任务和最后错误，不得阻塞整个控制循环。
- Web 显示最后 Agent 轮询时间、最后统计时间、最后同步时间和数据过期状态。

## 10. SQLite 表规划

### 10.1 控制面

- [ ] `schema_migrations`
- [ ] `settings`
- [ ] `hosts`
- [ ] `host_capabilities`
- [ ] `agents`
- [ ] `agent_certificates`
- [ ] `entries`
- [ ] `nodes`
- [ ] `landings`
- [ ] `routes`
- [ ] `route_hops`

### 10.2 用户与密钥

- [ ] `users`
- [ ] `user_routes`
- [ ] `subscription_tokens`
- [ ] `credentials`
- [ ] `credential_versions`

### 10.3 发布与任务

- [ ] `config_revisions`
- [ ] `config_artifacts`
- [ ] `deployments`
- [ ] `deployment_targets`
- [ ] `agent_commands`
- [ ] `agent_command_results`

### 10.4 计量与观测

- [ ] `traffic_batches`
- [ ] `traffic_baselines`
- [ ] `entry_runtime_epochs`
- [ ] `usage_buckets`
- [ ] `user_runtime_state`
- [ ] `entry_runtime_state`
- [ ] `health_events`
- [ ] `audit_logs`

所有表必须明确外键删除策略、唯一索引、状态枚举约束和时间字段格式。数据库时间统一保存 UTC Unix 时间或严格的 UTC RFC3339，不能混用本地时间。

## 11. Web 管理台

### 11.1 页面

- [ ] 登录、会话和管理员账号管理。
- [ ] 总览：Host、Agent、Entry、Node、Route、部署和异常状态。
- [ ] Host：创建、能力分配、Agent 管理地址、版本、最后轮询时间和服务状态。
- [ ] Entry：公网端点、固定 `19736/tcp+udp`、协议、当前 revision、Route 数量和 runtime epoch。
- [ ] Node：地址、固定 `29736/tcp+udp`、是否允许直出、当前 revision。
- [ ] Landing：类型、网络能力、关联 Node 或外部服务。
- [ ] Route：可视化编辑 Entry、Hops 和 Exit，并实时校验。
- [ ] 用户：配额、周期、有效期、Route 授权、订阅、停用和恢复。
- [ ] 发布：配置 diff、受影响 Host、检查结果、部署进度和回滚。
- [ ] 流量：按用户、Entry、Route 和周期查看汇总。
- [ ] 审计：配置、密钥、用户、部署和登录记录。

### 11.2 权限与安全

- [ ] 管理页面与公开订阅路径使用不同路由边界。
- [ ] 管理员密码使用 Argon2id。
- [ ] 使用安全 Cookie、CSRF 防护和会话过期。
- [ ] 预留管理员、运维、只读审计三种角色。
- [ ] 敏感操作要求重新认证并写入审计日志。
- [ ] 所有列表和日志默认脱敏域名、IP、token 和凭据。

## 12. API 边界

- Web API：Host、Entry、Node、Landing、Route、用户、发布、流量和审计管理。
- Agent API：被动状态查询、SSM 统计、用户 reconcile、artifact 发布、结果查询、计量确认和回滚。
- Subscription API：根据 token 返回页面、原始订阅和 Clash/mihomo 配置。
- Internal Controller：配置编译、流量结算屏障、部署状态机、SSM reconcile 和配额任务。

Agent API 与 Web API 使用不同认证边界。Manager 使用 mTLS 调用 Agent；Agent 只接受受信 Manager，且只能操作本机 Host。

## 13. 分阶段实施

### Phase 0：工程基础

- [ ] 拆分 `server`、`agent`、`domain`、`db`、`compiler`、`controller`、`web` 模块。
- [ ] 接入 SQLite migration、WAL、外键和统一事务封装。
- [ ] 实现配置加密服务和主密钥版本模型。
- [ ] 建立统一错误码、结构化日志、请求 id 和审计接口。
- [ ] 确定 Web 前后端技术栈和构建方式。

### Phase 1：Host 与 Agent

- [ ] 实现 Host、能力和 Agent 数据模型。
- [ ] 实现 Host enrollment package，包含 Host id、Agent 服务端证书和 Manager CA，不要求 Agent 主动注册。
- [ ] 实现 Agent 被动 mTLS API、固定 `39736/tcp` 指定地址监听和来源防火墙限制。
- [ ] 实现 Manager 主动轮询版本、revision、服务状态和 Host 详情页。
- [ ] 实现 Manager 调度、Agent 命令幂等、超时、重试和结果查询。
- [ ] 验证 Manage Host 同时运行 server、agent 和 entry sing-box。
- [ ] 验证所有 Entry/Node Host 未安装或无法访问 Agent 时不能进入可发布状态。

### Phase 2：拓扑与配置生成

- [ ] 实现 Entry、Node、Landing、Route 和 Route Hop CRUD。
- [ ] 实现 Route 拓扑、固定端口占用、环路和能力校验。
- [ ] 实现单 Entry 单 managed inbound 和 `auth_user` Route 规则生成。
- [ ] 实现 Entry、Node 和 managed Landing 的 sing-box 配置编译器。
- [ ] 实现真实 `sing-box check` 和配置 artifact 存储。
- [ ] 实现 Manage Entry 直出和多跳 Route。

### Phase 3：版本发布

- [ ] 实现草稿、revision、artifact 和 deployment 状态机。
- [ ] 实现 Agent 原子部署、健康检查和自动回滚。
- [ ] 实现 Node 先行、Entry 后行的依赖发布。
- [ ] 实现发布 diff、灰度、重试和回滚页面。
- [ ] 只有部署成功的 Route 才进入 active 状态。

### Phase 4：用户与订阅

- [ ] 实现用户、配额、周期、有效期和 Route ACL。
- [ ] 实现每用户与 Route 的独立身份和 uPSK。
- [ ] 实现用户和 Route ACL 变更触发受影响 Entry 的配置发布。
- [ ] 实现 Agent 本机 SSM 用户 reconcile 和重启后身份恢复。
- [ ] 实现多 Entry、多 Route 原始订阅和 Clash/mihomo 订阅。
- [ ] 实现订阅页面、token 轮换和停用状态。

### Phase 5：流量与配额

- [ ] 实现 Manager 定时调度 Agent 读取 SSM 累计统计。
- [ ] 实现带 boot id 和 sequence 的幂等增量计量。
- [ ] 实现重启前最终统计、Manager 确认和 Agent 持久化 outbox。
- [ ] 实现 Entry runtime epoch 和发布期间的独占计量锁。
- [ ] 实现跨 Entry、Route 的用户全局用量汇总。
- [ ] 实现月度、年度、永不重置周期。
- [ ] 实现超额、到期、管理员停用和自动恢复。
- [ ] 实现单 Entry 故障隔离和过期数据告警。

### Phase 6：生产化

- [ ] 实现 SQLite 在线备份、恢复验证、checkpoint 和空间监控。
- [ ] 实现管理员 RBAC、CSRF、安全 Cookie 和敏感操作复核。
- [ ] 实现 Agent 服务端证书、Manager 客户端证书轮换、吊销和主密钥轮换。
- [ ] 实现系统指标、健康检查、告警和数据保留策略。
- [ ] 完成 README、部署指南、威胁模型和故障恢复文档。

### Phase 7：后续能力

- [ ] 评估每 Route 独立端口模式，作为无需重启新增用户的可选运行方式。
- [ ] 支持 VLESS-Reality Entry 和按 Entry 独立 Reality 密钥。
- [ ] 支持更多 Landing 类型。
- [ ] 支持客户端 `fallback`、`url-test` 和按 Entry 分组订阅。
- [ ] 评估只读 Manager 副本和灾备切换，不允许多个控制器同时写 SQLite。

## 14. 第一阶段验收场景

- [ ] Manage Host 同时提供 Web、订阅和 Entry 直出。
- [ ] 独立 Entry 可通过两个 Node 连接最终 Landing。
- [ ] 同一个 Node 可同时作为中转和另一条 Route 的最终出口。
- [ ] Web 新建 Route 后完成校验、发布、Agent 应用和订阅激活。
- [ ] Web 新建用户并授权 Route 时完成最终流量结算、Entry 重启、身份恢复和订阅激活。
- [ ] 重启前产生的流量全部计入旧 epoch，重启后的流量全部计入新 epoch，重复批次不重复累计。
- [ ] 用户从一个 Route 移除后，其他 Route 保持可用。
- [ ] 多 Entry 流量准确汇总进同一用户配额。
- [ ] 一个 Entry 离线时，其他 Entry 继续统计、同步和提供代理。
- [ ] Agent 不主动连接 Manager；所有状态、统计和部署行为均由 Manager 发起，Agent 只在处理请求时访问本机回环服务。
- [ ] 未安装 Agent 的 Entry/Node Host 无法发布或激活 Route。
- [ ] Entry、Node、Agent、SSM 分别只使用 `19736`、`29736`、`39736`、`49736`，防火墙来源符合固定端口规划。
- [ ] sing-box 配置校验失败时保留旧 revision。
- [ ] 发布后健康检查失败时自动回滚。
- [ ] Manager 暂时离线时，已部署 sing-box 和已有身份继续工作。
- [ ] Manager 恢复后能继续处理累计统计且不重复计量。
- [ ] SQLite 备份恢复后，配置、密钥、订阅、用量和 Agent 状态一致。
