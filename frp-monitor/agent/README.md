# frp-monitor Agent

frpc + Go 采集模块的扩展源码，构建时由 `scripts/assemble.sh` 映射到临时上游树
`extension/frpmonitor/agent/`，共享上游 go module。整体架构、字段口径与接入约束见
根目录 [README.md](../README.md)（§1、§3、§4）。

子包现状：

- `collect/`：Linux Facts / Metrics 采集，直读 `/proc`、`/sys` 并调用 `statfs`，
  字段与口径对齐根 README §3；以 `tests/fixtures/` 驱动测试。
- `transport/`：独立 WSS 客户端（Authorization 握手认证；wss 强制，ws 仅回环）。
- `service/`：生命周期与调度——hello/report 会话、Facts 变化重报、FRP 扩展区
  变化上报加 60 秒兜底、指数退避加抖动、认证失败长退避、ping.tasks 分发。
- `probe/`：TCP 探测调度（版本化任务整体替换、每地址 900ms、最多 3 地址、
  失败 -1、DNS 超时缺样、并发上限 16）。

接入方式：`patches/0003-client-monitor-hook.patch` 在 `client.Service.Run`
首次登录前调用钩子（build tag `frpmonitor`；不带标签为空实现），监控 goroutine
独立于转发路径，任何采集或上报失败都不能中断 frpc 隧道。扩展不反向 import 上游
client/server 根包；钩子胶水在 client 包内把 `Service` 适配为窄接口
`FRPStatusSource`。
