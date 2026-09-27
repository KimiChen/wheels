# Monitor

原 `overlay/server/publicclientinfo/` 已迁到本目录。当前 P0 尚无监控 Listener、
数据库或管理 API；具体实施规格以根目录 [README.md](../README.md) 为准。

后续实现 `service/ingest/store/api/auth`，由构建工具映射到固定 FRP 源码的
`extension/frpmonitor/monitor`。FRP Registry/Stats 通过窄接口提供只读快照，
本包不得反向 import 上游 `server` 根包而造成循环依赖。

P1 使用节点独立凭据认证 WSS，维护当前会话和最新状态；正文不能决定节点归属。
P2 实现 SQLite WAL、分钟聚合和流量基线。P3 完成公开/管理 DTO 裁剪及发布验收。
监控会话、指标新鲜度、FRP 控制连接和隧道状态分别表示。
初始化、数据库及慢消费者错误应限制在监控模块的处理边界内；同进程不保证 OOM
或未恢复 panic 时的数据面存活。
