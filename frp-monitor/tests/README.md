# Tests

本目录预留给 Overlay 级集成测试、协议 Fixture 和公开页面安全测试。

至少需要覆盖：

- 60 秒调度、首次报告、超时、抖动、退避和 frpc 故障隔离；
- CPU、内存、Swap、磁盘、Load、进程、启动时间、网卡和连接数采集；
- 网卡计数器回绕、重启和采集字段部分失败；
- `clientID`、`bootID`、`sequence` 的重复、乱序和重放处理；
- 报告大小限制、认证失败、错误 Content-Type 和畸形 JSON；
- 在线、离线、过期、等待首报以及 frps 重启后的状态；
- 独立 Listener 不暴露 Dashboard、管理 API、Metrics、pprof 或写入凭据；
- 页面输出转义、CSP、安全响应头和 `localIP`/`localPort` 展示策略；
- Telemetry 停止或服务端不可用时，frp 数据面继续工作。
