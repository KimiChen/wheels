# Monitor

P3 实现独立 Listener、节点凭据握手、当前会话与内存最新状态、公开/管理 JSON 与 SSE，
以及本地探测任务、SQLite 历史、当日主机流量和 FRP 服务端注册表对账。
由构建工具映射到固定 FRP 源码的 `extension/frpmonitor/monitor`，不依赖上游
`server` 根包，也不改 FRP 转发协议。入口为 `Start(ctx, shared.MonitorConfig, ...shared.ServerProvider)`、
`Service.Address()`、`Service.Close()`；初始化失败由上游生命周期适配器降级。
该隔离从配置通过校验后开始；格式/范围错误的监控配置在原生 `verify` 和加载阶段报错。

## 配置与凭据

原生 frps 配置的 `[monitor]` 默认为关闭。启用后默认 `127.0.0.1:7401`；
对非 loopback 监听必须配置 `certFile` / `keyFile`。可用内置 TLS，或在本机
loopback 前放置 HTTPS/WSS 反向代理。管理会话只根据当前连接是否 TLS 设置 Secure，
不信任 `X-Forwarded-*`；启用管理时使用内置 TLS，或让代理向监控 Listener 继续使用 TLS
并保留原始 Host。HTTP 仅用于显式本地 loopback 开发。Web 与上报共用独立监控 Listener，
不得复用原生 Dashboard 或 FRP 控制端口。

`credentialsFile` 是 UTF-8、常规、权限 `0600` 的 JSON 文件，最多 1 MiB、
1,024 个节点。每 2 秒重新读取；有效变更立即更新允许列表，格式错误保留最近有效
版本并在管理快照标记 `credentials_state: "degraded"`。空数组 `[]` 撤销全部节点；
`null`、重复 ID/摘要/JSON 字段、未知字段、字段大小写别名均拒绝。每条记录包含：

```json
[
  {
    "agent_id": "example-node-0001",
    "name": "示例节点",
    "token_sha256": "替换为独立节点凭据编码文本的 SHA256 小写十六进制摘要",
    "frp_binding": {
      "server_id": "example-server",
      "user": "example-user",
      "raw_client_id": "example-client"
    }
  }
]
```

上例摘要是占位符，不能直接用于运行。`agent_id` 为安装时持久化的随机标识，
8–128 位 ASCII 字母、数字、`_`、`-`；展示名为 1–128 字节 UTF-8。ID 和摘要
都不能重复。token 使用至少 32 个随机字节的无填充 base64url 编码；服务端存储
该**编码文本**的 SHA256，不存明文。不得复用 FRP token/OIDC 凭据。
`frp_binding` 可省略，只接受操作员配置或管理接口写入；详细归属规则见下文。
令牌轮换关闭旧令牌的所有连接，包括已经升级但尚未完成 hello 的连接，清空当前
会话/快照至 `waiting`。撤销从公开列表立即移除节点，公开历史入口返回 404；SQLite
旧聚合按保留期存在，不等于删除历史文件。原生 FRP 凭据和控制连接不随监控撤销改变。

客户端通过 `/agent/v1/ws`，在握手时传 `Authorization: Bearer <token>`。
服务端从摘要确定节点归属，正文不能注册节点或改变归属；拒绝浏览器 Origin。
先接收 `hello`，返回接受的会话、能力与 `report_interval`，再接收 `report`。
接受 `metrics.v1` / `frp.v1`；显式启用探测的 agent 还协商 `ping.v1`。
没有协商探测的连接不会收到任务，也不能提交探测结果。

## 历史与探测配置

`[monitor] databaseFile` 为 SQLite 文件路径，直接父目录必须私有（0700），路径中不能有
符号链接，现有数据库与 WAL/SHM 必须为私有常规文件。`retentionDays` 默认 7，范围 1–31。
不配置数据库为 disabled；打开/查询/写盘错误为 degraded，监控 Listener 与原生 FRP
继续运行。内部队列/桶有限，丢样数量公开；状态与历史不伪装成完整覆盖。

`probeTasksFile` 是 0600 的 UTF-8 本地配置（最多 1 MiB），只允许凭据表中已知节点：

```json
{
  "version": 1,
  "nodes": [{
    "agent_id": "example-node-0001",
    "tasks": [{"id": "public-service", "name": "服务连通性", "target": "probe.example.invalid:443", "interval": 5}]
  }]
}
```

示例目标不可直接运行。每节点最多 64 项，间隔 5–3600 秒，名称是独立公开标签。
每 2 秒重新读取；增加 `version` 后以完整列表替换，`tasks: []` 清空该节点，省略节点
同样清空。格式错误、版本回退或同版本改内容保留上一有效列表并标记 degraded。
重连重新发送当前完整列表；旧版本在途结果丢弃，未知任务/未来版本/重复序号拒绝。
目标变化后以内部目标摘要隔离历史，不将前一个目标的数据拼接到新目标。
agent 的本地目标策略始终再次校验解析后的 IP，操作员文件不能绕过客户端的显式授权。

## 状态、边界与恢复

- 同一节点的新连接替换旧连接；连接指针、`session_id` 和严格递增的 `sequence`
  共同约束当前所有权，旧连接退出不能把新会话标为离线。重复、迟到及错误会话拒绝。
- WebSocket 文本帧最大 256 KiB；握手 10 秒、读超时 15 秒；每 5 秒独立 ping/pong，
  与 1–3,600 秒的指标间隔无关。心跳只更新会话接收时间，不伪造新指标。
- 首次无数据为 `waiting`。新鲜度使用服务端指标接收时间，超过
  `max(10 秒, 3 × reportIntervalSeconds)` 为 `stale`。监控会话与指标新鲜度、
  FRP 控制连接状态及运行隧道数分别显示。
- 上报握手全局限速 20 次/秒、突发 40；同时最多 32 个未完成握手，普通节点每秒
  2 个应用帧、突发 8；探测连接每秒 40 帧、突发 80。SSE 同时最多 128 个连接，
  写入超过 5 秒关闭慢消费者。历史查询同时最多 8 个，每请求数据库预算 2 秒。
- 关闭先取消请求与连接，最多等待 2 秒回收监控 worker；监控失败不会等待网络恢复。
  同进程不保证 OOM 或所有第三方未恢复 panic 时的数据面存活。

## 公开接口

`GET /api/public/v1/nodes` 返回 `{nodes: [...], generated_at: ...}`。
`GET /events/public` 立即发送 `event: snapshot`，之后每秒发送同结构完整快照。
API/SSE 均 `Cache-Control: no-store`，页面静态资源由 `web.Handler()` 提供安全头。
没有公开原始 report / Facts 的路由。

每个节点只有 `id`、`name`、`session`、`freshness`、`last_seen`、`metrics_at`、
`interval_seconds`、`frp`、`metrics`。时间为服务端 UTC RFC3339，缺样时为 null。
`frp` 仅含 `control_state`、`proxy_total`、`proxy_running`、`reconciliation`、
`server_online`（无可信匹配为 null）、`registered`（可信服务端在线代理数）；
没有名称、关联、目标、服务端注册表明细或隧道流量。
`metrics` 使用独立 allowlist DTO，明确排除主机名、IP、精确内核、boot ID、iface、
原始 FRP client ID、用户、代理名称/本地目标及凭据。

`metrics` 未接收时为 null；收到后包括 `scope`、CPU 百分比、三项 load、内存/
交换/磁盘总量与用量、网络 byte/s 与原始累计 bytes、uptime 秒、TCP/UDP/进程数。
每个观测值保留 `{value, quality, reason?}`；所有 uint64 值使用十进制字符串，
避免浏览器精度损失。实时快照的网络累计只是当前采集口径的原始计数；
P2 另外提供可跨会话续算、基于有效计数器差分的 UTC 当日统计，与原始累计值分开显示。

`GET /api/public/v1/nodes/{id}/history?window=1h` 提供分钟聚合；window 只允许
`1h` / `6h` / `24h` / `7d`，分别以 1/1/5/30 分钟步长返回，最多 500 点。
返回 storage 状态、每桶样本数/估计覆盖秒数、逐字段有效样本平均、当日 traffic，
以及任务公开标签、成功延迟均值、最新结果和 `0–100` 百分比 TCP 探测失败率。
没有有效样本为 null；uint 字段平均向下取整后仍用十进制字符串，不经 float64。
公开投影不含目标、主机标识、路径或凭据；公开接口不能修改任务或数据库。
管理会话可通过受 CSRF 保护的专用接口修改任务文件。
历史最多延迟约一分钟，正常 TERM/INT 退出刷新尾批；强制终止可能丢未提交窗口。
持久化数据不恢复在线状态，重启后必须重新握手。具体存储保证见 [store/README.md](store/README.md)。

## 管理认证与接口

`adminCredentialsFile` 为空时所有管理 API/SSE 返回 404。启用时提供独立 0600 常规文件：

```json
{"token_sha256": "替换为独立管理令牌编码文本的 SHA256 小写十六进制摘要"}
```

管理令牌须由 32 个随机字节进行无填充 base64url 编码（43 字符），与节点、FRP、
原生 Dashboard 凭据分别生成。服务端只保存摘要；登录不接受低熵密码代替此令牌。
初次加载文件非法时该监控实例初始化失败；已运行时每 2 秒热加载，坏文件保留最近
有效摘要并报告 `admin_credentials_state: "degraded"`。有效摘要变化撤销全部管理会话。

登录创建服务端内存会话，固定 8 小时过期，重启不会恢复。cookie 名为
`frp_monitor_admin`，Path=/、HttpOnly、SameSite=Strict，TLS 连接设置 Secure。
会话令牌与 CSRF 令牌各自随机生成，服务端只保存会话令牌摘要；最多 64 个会话。
登录全局限速为每 3 秒恢复 1 次、突发 5 次，耗尽返回 429。

所有管理响应 `Cache-Control: no-store`。管理请求检查 Origin（若携带必须精确匹配
当前 scheme/Host，拒绝跨源 Fetch Metadata）；CLI 可不发送 Origin。登录以外的
写请求必须带当前会话的 `X-CSRF-Token`。JSON 请求使用 `Content-Type: application/json`，
最大 1 MiB，拒绝重复、未知、大小写别名字段与不合法 null。错误为简短纯文本和
HTTP 状态码：未登录 401、同源/CSRF 拒绝 403、无配置/无节点 404、版本冲突 409、
服务暂不可用 503。不允许跨域 CORS，不复用原生 Dashboard 登录。

| 方法与路径 | 请求 / 结果 |
| --- | --- |
| `POST /api/admin/v1/login` | `{token}` → `{csrf_token, expires_at}`，设置会话 cookie |
| `GET /api/admin/v1/session` | 返回当前 `{csrf_token, expires_at}` |
| `POST /api/admin/v1/logout` | 使当前会话失效，204；现有管理 SSE 下一次发送前关闭 |
| `GET /api/admin/v1/nodes` | `{generated_at, nodes, credentials_state, admin_credentials_state, probes_state, frp}` |
| `GET /events/admin` | 立即及每秒发送同结构的 `event: snapshot` |
| `POST /api/admin/v1/nodes` | `{name, frp_binding?}` → 201 `{id, name, token}` |
| `POST /api/admin/v1/nodes/{id}/rotate` | 无正文 → `{id, token}`，旧令牌即刻失效 |
| `DELETE /api/admin/v1/nodes/{id}` | 撤销节点，204，允许撤销最后节点 |
| `PUT /api/admin/v1/nodes/{id}/binding` | `{server_id, user, raw_client_id}` → 204 |
| `DELETE /api/admin/v1/nodes/{id}/binding` | 解除可信绑定，204 |
| `GET /api/admin/v1/probes` | 返回当前有效 `{version, nodes:[{agent_id,tasks}]}`；未配置文件为 404 |
| `PUT /api/admin/v1/probes` | 同上完整文档，version 须大于当前版本；200 返回生效文档 |

创建/轮换的明文 token **仅在该响应返回一次**，任何读取接口不返回 token 或摘要。
配置更新以同目录 0600 临时文件、fsync、原子替换写入；不接受符号链接文件。
节点管理详情包含公开字段、`frp_summary`、浏览器精度安全的 `facts`、agent 原始 `frp`
扩展和可信 `frp_binding`；`facts` 中的 uint64 同样转为十进制字符串。
顶层 `frp` 是独立服务端对账详情，其中包含客户端身份/IP/主机名、代理名称及流量，
只在管理接口出现。管理界面不提供原生 FRP 代理 CRUD、远程 shell 或自动更新。

## FRP 对账与信任边界

关联键为 `(server_id, user, raw_client_id)`；上报中的关联只是声明。
只有操作员预设的 `frp_binding`、新鲜且在线的监控报告，以及独立服务端注册表中的
唯一稳定 client 三者一致，才产生 `matched` 与 `server_online`。不会从代理名、IP、
主机名、run ID 或代理上报自动绑定。无稳定 raw client ID 的监控声明保持 transient；
没有绑定时保持 unbound，即使文本看起来完全相同也不归属。普通原生 frpc 仍在
私有服务端客户端列表展示，未匹配监控节点时 `agent_id` 为 null。

同一可信绑定配置到多个节点、服务端键重复为 conflict；声明与自己的绑定不一致为
mismatch。无绑定的重复声明可使其自身显示 conflict；不能使合法预绑定 owner 变成 conflict，
也不能夺取其他节点的服务端归属。断线/报告过期为 stale，
服务端未登记为 missing，适配器不可用为 unavailable。状态细节见管理快照的
`frp.nodes`；公开快照只给状态、可空在线值与注册数。

服务端适配器每秒由独立 sampler 读取，使用有界快照与 TryRLock，不在 HTTP 请求中
访问原生注册表；异常单独降级，不阻塞主机指标采集或 FRP 转发。快照超过 5 秒未更新、
时间缺失或超前超过 5 秒，消费方清空服务端明细并按 unavailable 显示；异常慢 provider
不会产生不断增长的超时 goroutine。客户端原始代理名
按 FRP 规则只添加一次 `user.` 前缀，再与服务端名称、类型、user、稳定 client ID
核对；服务端独有代理保留展示，不捏造客户端配置。

FRP 隧道 RX/TX 是服务器侧转发量；主机 RX/TX、探测结果与该值的采集范围不同，
不相加，不作为计费依据。客户端与服务端可在重连/配置变更间短暂不一致，界面保留
两侧状态，不能以运行隧道数推断监控会话在线。

## 验证

测试覆盖 TLS 信任校验、凭据权限/认证、能力限制、会话替换、旧连接退出、乱序拒绝、
状态和新鲜度分离、公开字段裁剪、大整数、SSE 首帧，以及客户端断线后新会话先 Facts
再最新样本。客户端只保存最新样本、通知队列容量 1，不回补断线期间的 missed ticks；
指数退避加抖动，401 使用约 60 秒慢重试。采集器异常局部降级，异步首样本与有限关闭
保证慢采集不阻塞 FRP 启动/退出；操作系统中不可中断的采集调用可能延后 worker 真正退出。

P3 另外覆盖管理身份隔离、Secure cookie、会话过期/节流、CSRF/同源、管理 SSE 退出、
凭据创建/轮换/最后节点撤销、pending hello 撤销、热加载失败恢复、严格配置 JSON、
探测版本事务、可信绑定与公开对账裁剪；管理模块和存储模块通过 race 与 vet 检查。
