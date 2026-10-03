# shared：schema v1 契约

本目录实现独立监控通道的方法帧、字段类型、验证和浏览器整数转换；使用标准库，
在 Overlay 中映射为 `github.com/fatedier/frp/extension/frpmonitor/shared`，共享
固定上游 Go module，不单独创建 `go.mod`。

运行时由 agent/service、agent/probe、monitor、monitor/control 和 monitor/store 分别负责
握手凭据、会话所有权、重连、采样、探测、SQLite 当前业务与可选 TSDB 历史。验证器不做 DNS、
网络请求或节点身份绑定，也不等于完成资源配额、限速、目标授权与会话防重放。

## 启动配置与存储身份

`MonitorConfig` 对应 frps 的 `[monitor]`，启用后必须设置 SQLite `databaseFile`。
节点主键是 SQLite 自增整数，API 中使用十进制字符串；它与 JSON-RPC 的请求 id、
每次连接随机生成的 session_id 是不同概念。认证只认数据库中当前节点令牌摘要。
节点费用、套餐、今日统计及计数器基线保存到同一节点行，探测文档保存到 settings 单行。
实时 Facts/Metrics 与在线状态不进入业务表。

`historyDataPath` 默认空，即关闭；填入目录即启用。保留期 `retentionDays`
默认 7 天，范围 1–365。历史后端内嵌主控进程，使用 VictoriaMetrics 保存曲线，
不参与 SQLite 精确流量记账。今日日期按主控系统时区划分，套餐有独立重置时区。
时间戳均使用 UTC 毫秒或 API 规定的 RFC3339 格式。

管理员只通过 github.com OAuth 登录。`githubClientID`、`githubClientSecretFile`、
`githubCallbackURL`、`githubAdminUsers` 必须成组配置；允许列表最多 32 个用户名。
密钥由外部 0600 文件提供，不写入业务表。未配置时不开放管理功能；部分配置或非法
callback 会被配置校验拒绝。callback 路径固定为 `/api/admin/v1/auth/github/callback`，
要求 HTTPS，字面量 loopback 的 HTTP 仅用于本机开发。具体 API 见
[monitor/README.md](../monitor/README.md)。

## 帧与会话

独立 WSS 路径 `/agent/v1/ws`，节点凭据通过 Authorization 握手交付，禁止写入正文。
`DecodeFrame` 只解码下表中的 JSON-RPC 2.0 **方法帧**，不解码响应帧。批量请求、
数字 JSON-RPC 请求 id、未知方法和未知字段不属于 v1 profile。`hello` 必须有非空字符串 `id`；
其余三个方法都是通知，不能出现 `id`（包括 `null`）。

| 方法 | 方向 | params | 行为 |
|---|---|---|---|
| `hello` | agent → monitor | `Hello` | 每次连接第一个方法，sequence=1，先提供 Facts |
| `report` | agent → monitor | `Report` | sequence≥2，至少提供 Facts、Metrics、FRP 之一 |
| `ping.tasks` | monitor → agent | `PingTasks` | 版本递增的完整任务列表；`[]` 清空，不能省略或为 null |
| `ping.result` | agent → monitor | `PingResult` | sequence≥2，标明任务列表版本和 task_id |

每个 params 都包含 `schema:1`、`session_id`、`sequence`、`collected_at`：

- `session_id` 为每次 WSS 连接新生成的不透明随机标识，最多 128 UTF-8 字节。
  每个方向独立维护严格递增的正 `uint64` sequence；agent 的所有方法共用一个序列。
  序号到达 uint64 上界前须新建连接，不回绕。
- `collected_at` 为 RFC3339 时间（允许纳秒及显式时区偏移，年份≥1970），表示快照
  采集或任务生成时间。客户端时钟不决定在线、新鲜度、累计日期或认证有效期；服务端
  另记接收时间。差分 CPU/网速使用采集器本地单调时间。
- 每帧校验只检查字段范围。当前会话替换、旧会话迟到、序号倒退/重复、旧连接退出
  不能覆盖新连接，均由接收器保证；不能因为 `DecodeFrame` 返回成功就覆盖节点。
- 不提供可由正文指定的 `agent_id`。服务端凭据映射出的节点才是权威身份；
  `session_id`、FRP 关联和主机名都不是认证依据。

hello 的 capabilities 为不重复字符串列表，例如 `metrics.v1`、`frp.v1`、`ping.v1`。
服务端只接受自身支持且客户端声明的能力子集，未知能力不等于授权。
`HelloResult` 约束成功响应的 result：

```json
{"jsonrpc":"2.0","id":"hello-1","result":{"schema":1,"session_id":"example-session-1","capabilities":["metrics.v1"],"report_interval":1}}
```

`report_interval` 为 1–3600 秒。`HelloResult.Validate` 验证结果字段，调用方负责保证
id 对应请求、会话和能力子集匹配。协商拒绝时返回 JSON-RPC error，不发送成功响应。
建议不支持 schema 使用应用错误 `-32001`；`ErrUnsupportedSchema` 可供接收器映射。
鉴权失败发生于握手阶段。响应编码、协商流程和错误到断线的处理由接收器实现。

## 字段、单位和质量

保留参考 Facts/Metrics 字段名，值统一为 `Field[T]`：

```json
{"value":null,"quality":"warming_up","reason":"no_baseline"}
```

`value` 和 `quality` 都是必填字段，`reason` 可省略。`ok` 必须有非 null 值；其他
质量必须为 null。真实 0 必须用 `{"value":0,"quality":"ok"}`，不可代替读取失败。

| quality | 含义 | 允许 reason |
|---|---|---|
| `ok` | 有效读数 | 无 |
| `warming_up` | 尚无可用差分基线 | 可省略，或 `no_baseline` / `counter_reset` |
| `unavailable` | 本次读取缺失/失败 | 可省略，或 `read_error` |
| `unsupported` | 当前平台/范围不支持 | 无 |

每次完整 Facts/Metrics 对象都包含全部规定字段；
局部读取失败用质量表示，不通过遗漏字段或沿用旧值伪装成新读数。

| 对象 | 字段 | Go 值与单位 |
|---|---|---|
| Facts | `hostname, os, kernel, arch, virt, cpu_name, agent_version` | UTF-8 文本；有效文本非空，virt 无虚拟化可用 `none` |
| Facts | `cpu_cores` | uint32，逻辑处理器数，有效值≥1 |
| Facts | `ipv4, ipv6` | 对应地址族的单个本机地址；没有地址用 unavailable，不写空串 |
| Facts | `mem_total, swap_total, disk_total` | uint64，字节 |
| Metrics | `cpu` | float64，整机百分比，0–100；无差分基线为 warming_up |
| Metrics | `load` | 恰好三个有限非负 float64，依次为 1/5/15 分钟负载 |
| Metrics | `mem_total, mem_used, swap_total, swap_used, disk_total, disk_used` | uint64，字节；两者有效时 used≤total |
| Metrics | `net_rx, net_tx` | uint64，B/s，除以实际单调时差后向下取整；无基线为 warming_up |
| Metrics | `net_rx_total, net_tx_total` | uint64，所计网卡内核 lifetime 字节计数器，不能当今日新增 |
| Metrics | `boot_id` | 参考范围标识：内核 boot ID 加所计网卡集合摘要；字符串视为不透明值 |
| Metrics | `iface` | 原始接口筛选 spec；空串表示默认筛选，空串+ok 是有效状态 |
| Metrics | `uptime` | uint64，秒 |
| Metrics | `tcp, udp, procs` | uint64，socket 汇总/进程数；TCP 包括 TIME_WAIT，不是 established 数 |

参考 boot_id 的网卡集合摘要使用 FNV-1a；具体确定性集合构造由采集器按固定
参考源码实现。boot_id 的变化包含主机重启/所计集合改变，不能据此判定 agent 进程
或监控会话是否重启。有效 lifetime 计数器必须同时有有效 boot_id 和 iface；缺失读数
不得用 0 改写流量基线。相同范围的差分、计数器回退、同事务累计由 monitor/control 实现。

本目录只验证表示与范围，不实现 CPU、内存、磁盘、网卡算法。算法与覆盖范围以
根 README 及 `tests/fixtures/collect/` 为准。

## FRP 扩展与 TCP 探测

`extensions.frp` 可附在 hello/report 上。每次是完整 FRP 快照，包含：

- `association {server_id,user,raw_client_id}`：三元关联键，原生默认 user 允许空串。
  没有稳定 client ID 时 `raw_client_id:null`，不能临时 ID 冒充稳定绑定。
- `version` 为独立 FRP 版本；`control_state` 为 `unknown/connecting/connected/disconnected/error`。
- `proxies` 为完整数组（允许 `[]`），每项含 `name/type/local_target/enabled/status`。
  `local_target:null` 表示不可用；状态为 `unknown/disabled/starting/running/error/closed`，
  由运行时适配器归一化原生状态。类型支持 tcp/udp/http/https/tcpmux/stcp/sudp/xtcp。

关联用于对账，不能自行重新绑定凭据节点；monitor 根据自己的 Registry/Stats 添加
注册状态、连接数和隧道流量，agent 不能在此伪报服务端数据。冲突由后续接入器显式报告。

`ping.tasks` 附 `version`（正 uint64）和完整 `tasks` 列表，每项 `id/target/interval`。
`target` 是 `host:port`（IPv6 用方括号），port=1–65535，interval=5–3600 秒。
验证器只校验文本形状，服务端还须验证操作者、目标策略，agent 按获准能力执行。
`ping.result` 附 `task_version/task_id/latency_ms`；必须属于当前任务版本的匹配检查由
接收器负责。成功 latency 为有限 0–900 毫秒，失败为 -1；DNS 超时不发送结果，普通
解析错误发送失败。调度器每地址最多 900ms、最多三个地址，延迟不包含 DNS 及此前
地址失败耗时。指标应命名“TCP 探测失败率”，不是 IP 丢包率。

## JSON、大小限制与浏览器边界

### 私有 FRP 观察详情

`frp.detail.v1` 与原生 wire v1/v2 无关。新版 monitor 在已认证的 WebSocket 升级响应中
发送 `X-Frp-Plus-Capabilities: frp.detail.v1`；仅看到该预告且有适配器的 Agent 才在
旧结构 hello 的 capabilities 中声明它，收到接受后才发送 `frp.detail` 通知。
没有响应头的旧服务端完全收不到新增能力或字段；新版服务端只授予客户端声明且自身
支持的能力。首个 hello 和 `extensions.frp` 保持旧契约。

详情复用连接 session/递增 sequence（与指标、探测共用序号），但有独立采样和接收时间。
内容是允许字段组成的私有 DTO：Proxy、Visitor、配置来源与观察修订、最近来源读取时间、
Dashboard 是否配置监听、传输摘要、入口、固定错误码及恢复时间。秘密、原始配置、
插件选项、原始错误和文件路径不进入 DTO。锁忙、采集失败和超限分别退化为 busy、unavailable
或 truncated，非 ready 不携带部分列表；最多 512 个 Proxy、512 个 Visitor、每对象
32 个入口，详情序列化上限 192 KiB。采样最多一个异步任务、等待 50ms，不积压采样。

管理列表/SSE 只包含 `frp_detail_state` 与 `frp_detail_at`，不广播整份详情；
`GET /api/admin/v1/nodes/{id}/frp-detail` 使用同源管理员会话鉴权，返回
`{node_id,state,received_at,detail}` 并禁止缓存。未协商、等待、过期、不可用、超限均
返回 `detail:null`。公开 API/SSE 保持原有汇总字段，不包含新增详情或管理员公布链接。

### 通用边界

| 边界 | 限制 |
|---|---|
| 单帧原始 UTF-8 字节（含空白） | 256 KiB |
| JSON 嵌套层数 | 16（根值深度为 0） |
| 任意 JSON 字符串/对象键 | 解码后最多 1024 UTF-8 字节 |
| hello 请求 id | 1–64 字节 |
| session / task id / server_id / user / raw_client_id | 最多 128 字节（仅 user 可为空） |
| capability / FRP version | 最多 64 字节；capabilities 最多 32 项，不重复 |
| Proxy name / probe target | 最多 256 字节 |
| Proxies / probe tasks | 最多 512 / 64 项，名称/id 不重复 |

对象键采用非空 ASCII `[a-z0-9_]` 的精确 snake_case，禁止 Unicode 大小写折叠别名。
拒绝重复键、大小写别名、未知字段、遗漏必填字段、
必填标量 null、非法 UTF-8、尾随第二个 JSON 值、非有限 float、负整数/容量、uint 溢出
及不合范围值。可空指针仍支持 null；空数组必须使用 `[]`。总帧限制可能早于单项上限
生效。实际 WSS 读取器必须在分配完整载荷前也设置读限制；本函数检查不替代传输限额。

agent ↔ monitor 的 uint64 使用 JSON 整数，由 Go 精确解析；浏览器不得直接消费这些
原始帧。`Facts.Browser()` / `Metrics.Browser()` 验证后把**所有 uint64 读数**转成
十进制 JSON 字符串（仍保留质量和 null），包括容量、速率、计数器、uptime 和 socket/
进程数。uint32 CPU 核数、CPU/load 等有限浮点仍为 JSON number。对其他 uint64（如任务 version），
前端解析和提交必须保持整数精度，不能先经过 JavaScript Number。控制库中的字节数、
API 流量与价格最小单位同样使用十进制字符串；TSDB 曲线样本的浮点精度不等于业务账本精度。

**BrowserFacts/BrowserMetrics 只解决整数精度，不是公开视图 DTO。** 它们仍含 IP、
主机名、内核、范围标识等私有内容，不能直接挂公开路由；公开裁剪与管理鉴权由 monitor
在服务端完成。FRP 本地目标和节点/FRP/Dashboard 凭据不进入公开 DTO。

## 测试与 fixture

`testdata/` 的四个 JSON 为自行构造的脱敏协议样例，使用文档保留地址与
`example.invalid`；没有参考项目实现代码或生产信息。首报 fixture 故意包含 CPU/网速
未知、MemAvailable=0 对应的 mem_used=mem_total，以及超过 JS 安全整数范围、达到 uint64
上界的计数器读数。

上游 Overlay 测试运行 `go test ./extension/frpmonitor/shared`。本目录也可在仓库根
用 `GO111MODULE=off go test ./frp-plus/shared` 独立验证标准库契约。
表驱动测试覆盖正确帧、畸形 JSON/身份冒充字段、限制边界、未知/零值、数值范围、
任务与 FRP 数组及浏览器精度，并提供 `FuzzDecodeFrame` 入口。测试不声称会话所有权、
凭据轮换、真实采集和网络探测已通过。
