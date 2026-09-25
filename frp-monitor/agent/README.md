# frp-monitor Agent

frpc + Go 采集模块的扩展源码，构建时由 `scripts/assemble.sh` 映射到临时上游树
`extension/frpmonitor/agent/`，共享上游 go module。整体架构、字段口径与接入约束见
根目录 [README.md](../README.md)（§1、§3、§4）。

目标子包（随实施阶段落地，当前仅有契约代码在 `../shared/`）：

- `service/`：生命周期、调度、FRP 只读适配器；在 `client.Service.Run` 首次登录前启动，
  退出时取消并限时回收；任何采集或上报失败都不能中断 frpc 隧道。
- `collect/`：Linux Facts / Metrics，直读 `/proc`、`/sys` 并调用 `statvfs`，
  字段与口径对齐根 README §3。
- `probe/`：TCP 探测（握手延迟，失败 `-1`，DNS 超时缺样）。
- `transport/`：独立 WSS、认证、重连（指数退避加抖动，认证失败不高频重试）。

约束：监控挂在进程生命周期，不实现为普通 frpc Proxy 插件；不修改 FRP wire protocol；
扩展不得反向 import 上游 client/server 根包形成循环。
