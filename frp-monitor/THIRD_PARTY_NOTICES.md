# Third-Party Notices

## frp

本项目计划通过补丁和 Overlay 修改以下上游项目：

- Project: frp
- Source: https://github.com/fatedier/frp
- Version: `v0.71.0`
- Tag object: `40adeed73b51e7ee1766d7cfb15d02ba9431ba2b`
- Source commit: `4a23aa181c1d7e28eecaa8216024ed753b9d27c8`
- License: Apache-2.0

完整上游源码不保存在本仓库中。准备构建时，工具取得上述固定提交并验证身份，
然后在临时工作树中应用 `patches/`，并映射本项目扩展源码（`agent/`、`monitor/`、
`shared/`、`web/` → `extension/frpmonitor/`）。

当前实质修改（`patches/series`，均为最小补丁或包内薄胶水新文件）：

- `pkg/util/version`：增加 `Suffix`/`Base()`，用于显示自身版本与 FRP 基线版本；
- `pkg/config/v1`：增加 frpc/frps 的 `[monitor]` 配置类型与校验；
- frpc 生命周期钩子：`client.Service.Run` 首次登录前启动监控 goroutine
  （Linux 主机指标采集与独立 WSS 上报，读取本地 Proxy 只读状态）；
- frps 生命周期钩子：`server.Service.Run` 启动独立 Listener（节点认证接收、
  内存最新状态、公开/管理 API、SSE 与嵌入页面）；monitor 开启时即使 Dashboard
  关闭也强制启用内存统计 collector（全进程仅注册一次）；
- 钩子均以 build tag `frpmonitor` 接线，默认构建（空实现）保持 frp wire
  protocol、数据转发路径以及原有管理功能完全不变。

实际引入代码后，应在每个补丁或 Overlay package 中保留适用的上游版权、许可证和
来源说明，并同步更新本文件中的“实质修改”列表。

## monitor-probe 设计参考

本轮仅调研以下 MIT 项目，未复制其实现代码：

- https://github.com/monitor-probe/agent
  - Commit: `cebc5383abb5963abeb722877a2d82c1a7aea5c6`
- https://github.com/monitor-probe/monitor
  - Commit: `fb4c4a4ce0b3a665ab7a4bd491d2dd447a78e6bc`

上述源码的版权声明为 `Copyright (c) 2026 stqfdyr`。后续如移植源码、测试或其他
受版权保护材料，须随材料保留原版权声明与 MIT 许可证全文，并更新本文件。

## web-standard-kit

`web/static/assets/wsk/`（style.css、script.js）复制自本仓库
`6d41d588b3537b2d2460d73f999d958fd37eb00d` 中的 `web-standard-kit/` 目录快照，
随 monitor 二进制嵌入发布；页面结构、业务样式与 `src/` 模块为本子项目自有实现。
升级套件快照时同步更新本节与 `web/README.md` 的来源记录。

## modernc.org/sqlite（P2 历史存储）

monitor 的历史与流量持久化使用纯 Go SQLite 驱动（经 `patches/0005-deps-modernc-sqlite.patch`
加入上游 go.mod/go.sum，不复制源码进仓）：

- modernc.org/sqlite v1.34.5（BSD-3-Clause，The Sqlite Authors）
- modernc.org/libc v1.55.3、modernc.org/mathutil v1.6.0、modernc.org/memory v1.8.0
  （BSD-3-Clause）
- github.com/dustin/go-humanize v1.0.1、github.com/mattn/go-isatty v0.0.20、
  github.com/ncruces/go-strftime v0.1.9（MIT）
- github.com/remyoudompheng/bigfft（BSD-3-Clause，The Go Authors）

发布包须附带上述许可证全文（packaging 随 P3 落地）。
