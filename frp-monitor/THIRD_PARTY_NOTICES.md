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

P1 实质修改位于 `patches/0001-monitor-lifecycle.patch` 与映射到上游
`extension/frpmonitor/{agent,monitor,shared,web}` 的扩展源码：

- 增加 Telemetry/Monitor 配置和独立版本输出，异步挂接两端 Service 生命周期；
- frpc 增加 Linux 采集、只读 FRP/Proxy 状态、独立 WSS 上报和重连；
- 增加只读配置快照，保留原生启用项合并语义并展示未启用项；
- frps 增加节点凭据认证、内存最新状态和独立 Listener 的公开 API/SSE；
- 内存 metrics collector 可在监控启用时单次注册，增加注册与调用的并发保护；
- 嵌入基于 web-standard-kit 的节点网页，保留原生 Dashboard 和 wire protocol。

TCP 探测、数据库历史、累计流量及认证管理接口尚未实现。

## monitor-probe 设计参考

以下 MIT 项目用作设计和算法参考：

- https://github.com/monitor-probe/agent
  - Commit: `cebc5383abb5963abeb722877a2d82c1a7aea5c6`
- https://github.com/monitor-probe/monitor
  - Commit: `fb4c4a4ce0b3a665ab7a4bd491d2dd447a78e6bc`

上述源码的版权声明为 `Copyright (c) 2026 stqfdyr`。P1 的 `agent/collect` 使用
独立 Go 实现移植 agent 的 `collect.rs` 算法与过滤/拓扑规则，保守按算法移植保留
完整 MIT 许可于 `agent/collect/LICENSE.monitor-probe`，二进制发布包同样附带。
未复制 monitor 的服务端实现。各采集文件保留来源说明。
`tests/fixtures/collect/cases.json` 的样本输入为本项目自行构造；参考函数、
预期值单位与有意的质量语义差异见同目录 README。

## web-standard-kit

网页复用本仓库 `web-standard-kit/`，参考快照为
`6d41d588b3537b2d2460d73f999d958fd37eb00d` 中的该目录。
原始 `style.css`、`script.js` 复制至 `web/assets/`，业务样式与代码单独维护。
文件清单、源路径和摘要见 `web/README.md`，资源不在运行时跨项目读取。
