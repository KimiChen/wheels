# Monitor

P1 实现独立 Listener、节点凭据握手、当前会话与内存最新状态，以及公开 JSON/SSE。
由构建工具映射到固定 FRP 源码的 `extension/frpmonitor/monitor`，不依赖上游
`server` 根包，也不改 FRP 转发协议。入口为 `Start(ctx, shared.MonitorConfig)`、
`Service.Address()`、`Service.Close()`；初始化失败由上游生命周期适配器降级。
该隔离从配置通过校验后开始；格式/范围错误的监控配置在原生 `verify` 和加载阶段报错。

## 配置与凭据

原生 frps 配置的 `[monitor]` 默认为关闭。启用后默认 `127.0.0.1:7401`；
对非 loopback 监听必须配置 `certFile` / `keyFile`。可用内置 TLS，或在本机
loopback 前放置 HTTPS/WSS 反向代理。Web 与上报共用独立监控 Listener，
不得复用原生 Dashboard 或 FRP 控制端口。

`credentialsFile` 是 UTF-8、常规、权限 `0600` 的 JSON 文件，最多 1 MiB、
1,024 个节点；凭据变更在重启监控后生效。P1 不提供热更新、管理登录或注册 API。
每条记录只包含：

```json
[
  {
    "agent_id": "example-node-0001",
    "name": "示例节点",
    "token_sha256": "替换为独立节点凭据编码文本的 SHA256 小写十六进制摘要"
  }
]
```

上例摘要是占位符，不能直接用于运行。`agent_id` 为安装时持久化的随机标识，
8–128 位 ASCII 字母、数字、`_`、`-`；展示名为 1–128 字节 UTF-8。ID 和摘要
都不能重复。token 使用至少 32 个随机字节的无填充 base64url 编码；服务端存储
该**编码文本**的 SHA256，不存明文。不得复用 FRP token/OIDC 凭据。

客户端通过 `/agent/v1/ws`，在握手时传 `Authorization: Bearer <token>`。
服务端从摘要确定节点归属，正文不能注册节点或改变归属；拒绝浏览器 Origin。
先接收 `hello`，返回接受的会话、能力与 `report_interval`，再接收 `report`。
P1 只接受 `metrics.v1` / `frp.v1`，不宣称或执行 `ping.v1` TCP 探测能力。

## 状态、边界与恢复

- 同一节点的新连接替换旧连接；连接指针、`session_id` 和严格递增的 `sequence`
  共同约束当前所有权，旧连接退出不能把新会话标为离线。重复、迟到及错误会话拒绝。
- WebSocket 文本帧最大 256 KiB；握手 10 秒、读超时 15 秒；每 5 秒独立 ping/pong，
  与 1–3,600 秒的指标间隔无关。心跳只更新会话接收时间，不伪造新指标。
- 首次无数据为 `waiting`。新鲜度使用服务端指标接收时间，超过
  `max(10 秒, 3 × reportIntervalSeconds)` 为 `stale`。监控会话与指标新鲜度、
  FRP 控制连接状态及运行隧道数分别显示。
- 上报握手全局限速 20 次/秒、突发 40；同时最多 32 个未完成握手，节点每秒
  2 个应用帧、突发 8。SSE 同时最多 128 个连接，写入超过 5 秒关闭慢消费者。
- 关闭先取消请求与连接，最多等待 2 秒回收监控 worker；监控失败不会等待网络恢复。
  同进程不保证 OOM 或所有第三方未恢复 panic 时的数据面存活。

## 公开接口

`GET /api/public/v1/nodes` 返回 `{nodes: [...], generated_at: ...}`。
`GET /events/public` 立即发送 `event: snapshot`，之后每秒发送同结构完整快照。
API/SSE 均 `Cache-Control: no-store`，页面静态资源由 `web.Handler()` 提供安全头。
没有公开原始 report / Facts 的路由。

每个节点只有 `id`、`name`、`session`、`freshness`、`last_seen`、`metrics_at`、
`interval_seconds`、`frp`、`metrics`。时间为服务端 UTC RFC3339，缺样时为 null。
`frp` 仅含 `control_state`、`proxy_total`、`proxy_running`；没有名称、关联或目标。
`metrics` 使用独立 allowlist DTO，明确排除主机名、IP、精确内核、boot ID、iface、
原始 FRP client ID、用户、代理名称/本地目标及凭据。

`metrics` 未接收时为 null；收到后包括 `scope`、CPU 百分比、三项 load、内存/
交换/磁盘总量与用量、网络 byte/s 与原始累计 bytes、uptime 秒、TCP/UDP/进程数。
每个观测值保留 `{value, quality, reason?}`；所有 uint64 值使用十进制字符串，
避免浏览器精度损失。P1 网络累计只是当前采集口径的原始计数；重启跨会话累计、
SQLite 历史、分钟聚合和趋势由 P2 实现。

## 验证

测试覆盖 TLS 信任校验、凭据权限/认证、能力限制、会话替换、旧连接退出、乱序拒绝、
状态和新鲜度分离、公开字段裁剪、大整数、SSE 首帧，以及客户端断线后新会话先 Facts
再最新样本。客户端只保存最新样本、通知队列容量 1，不回补断线期间的 missed ticks；
指数退避加抖动，401 使用约 60 秒慢重试。采集器异常局部降级，异步首样本与有限关闭
保证慢采集不阻塞 FRP 启动/退出；操作系统中不可中断的采集调用可能延后 worker 真正退出。
