# Third-Party Notices

## frp

本项目通过固定源码构建和 Overlay 扩展以下上游项目：

- Project: frp
- Source: https://github.com/fatedier/frp
- Version: `v0.71.0`
- Tag object: `40adeed73b51e7ee1766d7cfb15d02ba9431ba2b`
- Source commit: `4a23aa181c1d7e28eecaa8216024ed753b9d27c8`
- License: Apache-2.0

完整上游源码不保存在本仓库中。准备构建时，工具必须取得上述固定提交并验证身份，
然后在临时工作树中应用 `patches/`，并映射本项目扩展源码。当前目录迁移与构建方案
以根目录 `README.md` 为准。

实质修改位于 `patches/0001-monitor-lifecycle.patch` 与映射到上游
`extension/frpmonitor/{agent,monitor,shared,web}` 的扩展源码：

- 增加 Telemetry/Monitor 配置和独立版本输出，异步挂接两端 Service 生命周期；
- frpc 增加 Linux 采集、只读 FRP/Proxy 状态、独立 WSS 上报和重连；
- 增加只读配置快照，保留原生启用项合并语义并展示未启用项；
- frps 增加节点凭据认证、内存最新状态和独立 Listener 的公开 API/SSE；
- 内存 metrics collector 可在监控启用时单次注册，增加注册与调用的并发保护；
- 嵌入基于 web-standard-kit 的节点网页，保留原生 Dashboard 和 wire protocol。

扩展包括 TCP 探测、SQLite 节点配置与当前流量、GitHub OAuth 管理入口，以及默认关闭的
内嵌 VictoriaMetrics 历史。监控模式下接收 TERM/INT 并关闭存储。SQLite、进程退出、
FRP 对账和 TSDB 依赖变更记录在 0002～0004 补丁。

## monitor-probe 设计参考

以下 MIT 项目用作设计和算法参考：

- https://github.com/monitor-probe/agent
  - Commit: `cebc5383abb5963abeb722877a2d82c1a7aea5c6`
- https://github.com/monitor-probe/monitor
  - Commit: `fb4c4a4ce0b3a665ab7a4bd491d2dd447a78e6bc`

上述源码的版权声明为 `Copyright (c) 2026 stqfdyr`。`agent/collect` 使用
独立 Go 实现移植 agent 的 `collect.rs` 算法与过滤/拓扑规则，保守按算法移植保留
完整 MIT 许可于 `agent/collect/LICENSE.monitor-probe`，二进制发布包同样附带。
未复制 monitor 的服务端实现。各采集文件保留来源说明。
Go TCP 探测独立实现，按同一固定 agent `src/main.rs` 对齐 DNS/拨号限时及
成功/失败/缺样口径；持久化与流量事务为本项目实现，没有复制 monitor 数据库源码。
`tests/fixtures/collect/cases.json` 的样本输入为本项目自行构造；参考函数、
预期值单位与有意的质量语义差异见同目录 README。

## web-standard-kit

网页复用本仓库 `web-standard-kit/`，参考快照为
`6d41d588b3537b2d2460d73f999d958fd37eb00d` 中的该目录。
原始 `style.css`、`script.js` 复制至 `web/assets/`，业务样式与代码单独维护。
文件清单、源路径和校验命令见 `web/README.md`，资源不在运行时跨项目读取。

## SQLite 与新增依赖

配置和当前流量账本使用 `modernc.org/sqlite v1.59.0`，锁定其要求的
`modernc.org/libc v1.75.7`，保持 CGO 关闭的 Linux amd64/arm64 构建。
完整模块版本、来源、许可原文与第三方许可保存在
`monitor/store/licenses/SOURCES.md` 及相邻目录，发布包保留为 `licenses/`。
SQLite 核心的公开领域声明与 Go 封装/依赖的许可证分别保留，不将整个依赖链视作公开领域。


## VictoriaMetrics 与时序存储依赖

可选历史后端直接使用下列项目的 `lib/storage` 和 `lib/prompb`，与 frps 同进程：

- Project: VictoriaMetrics
- Source: https://github.com/VictoriaMetrics/VictoriaMetrics
- Version: `v1.151.0`
- License: Apache-2.0
- Dependency patch: `patches/0002-storage-dependencies.patch`

完整模块图和校验值由该补丁中的 `go.mod` / `go.sum` 固定，构建需要 Go 1.26.6
或更高。TSDB 新增依赖包括 VictoriaMetrics 的 easyproto、fastcache、metrics、
metricsql，以及 xxhash、snappy、klauspost/compress 和 valyala 工具库；其模块约束
也会更新 FRP 原有的 Prometheus、WebSocket、Kubernetes、YAML 等依赖。

逐项版本、官方来源及许可证原文见
`monitor/store/licenses/TSDB-SOURCES.md` 和对应 `tsdb-*` 目录。该清单同时覆盖
CGO 关闭时实际使用的存储依赖和完整 FRP 模块图的联动更新；压缩库附带的第三方
版权说明一并保留。发布包须包含这些来源清单与许可文件。

架构参考哪吒的[内嵌 TSDB 方案](https://nezha.wiki/guide/q15.html)，本项目自行实现
有界采样队列、质量过滤、缺口和字段覆盖率查询、错误状态及生命周期适配，没有
复制哪吒的服务端业务代码。SQLite 精确流量账本与 TSDB 浮点历史曲线独立维护。
