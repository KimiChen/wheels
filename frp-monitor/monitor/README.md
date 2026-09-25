# frp-monitor Monitor

frps + Go 监控模块的扩展源码，构建时由 `scripts/assemble.sh` 映射到临时上游树
`extension/frpmonitor/monitor/`，共享上游 go module。整体架构、身份规则与存储设计见
根目录 [README.md](../README.md)（§1、§4、§5、§6）。

目标子包（随实施阶段落地，当前仅有契约代码在 `../shared/`）：

- `service/`：生命周期、FRP Registry / Stats 只读适配器；Monitor 开启后即使 Dashboard
  关闭也显式启用内存统计 collector，且统一入口保证只注册一次。
- `ingest/`：独立 Listener `/agent/v1/ws` 的认证、接收、会话管理；
  只接受当前会话，迟到报告与旧连接退出不得覆盖新连接。
- `store/`：SQLite WAL、迁移、分钟聚合、流量基线与累计（同事务更新）。
- `api/`：公开/管理 DTO、查询、SSE；公开 DTO 服务端裁剪，不含本地目标、IP、主机名。
- `auth/`：管理会话（安全 Cookie、写操作 CSRF）与节点独立凭据（保存验证摘要，支持轮换/吊销）。

约束：监控初始化、磁盘或查询故障应使监控降级，不能拖停隧道；与原 Dashboard/API 使用
独立路由与认证，不向网页提供 Dashboard 凭据。
