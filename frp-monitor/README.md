# frp-monitor

`frp-monitor` 是基于上游 frp 的轻量监控 Overlay 子项目。项目不保存完整的 frp
源码，只保存设计文档、固定的上游版本、必要补丁、新增文件和后续构建工具。

> 当前状态：文档与目录骨架；尚未包含实现代码、构建脚本或可发布二进制。

## 当前目标

基于 frp `v0.71.0`：

- frpc 每 60 秒采集并上报 Linux 主机指标和本地 Proxy 信息；
- frps 在独立端口接收经过认证的报告；
- frps 在该端口提供免登录、只读的公开客户端信息页面；
- Telemetry 故障不影响 frp 登录、重连、Proxy/Visitor 生命周期或数据转发；
- 不修改 frp wire protocol，也不改变原 Dashboard、API 和认证行为。

页面需要展示 CPU、核心数、内存、Swap、磁盘、系统负载、进程数、启动时间、
网卡累计流量和实时速度、TCP/UDP 连接总览、操作系统发行版，以及 frpc Proxy
的 `localIP`、`localPort`、类型和运行状态。

## 本阶段范围

当前实施范围以 [`docs/frps.md`](docs/frps.md) 的“独立公开客户端 Telemetry
页面方案”为准。第一阶段只实现采集、上报、内存中最新快照和公开只读页面。

[`docs/frpc.md`](docs/frpc.md) 同时保留了更完整的客户端 Telemetry/管理模块设计。
其中远程配置下发、Proxy/Visitor CRUD 和状态对账属于后续候选能力，不属于当前
公开页面 MVP，不能在没有单独决策的情况下顺带实现。

## 上游基线

| 项目 | 值 |
|---|---|
| 上游仓库 | `https://github.com/fatedier/frp.git` |
| Tag | `v0.71.0` |
| Tag 对象 | `40adeed73b51e7ee1766d7cfb15d02ba9431ba2b` |
| 源码 Commit | `4a23aa181c1d7e28eecaa8216024ed753b9d27c8` |
| 上游许可证 | Apache-2.0 |

机器可读的固定值以 [`upstream.lock`](upstream.lock) 为准。升级上游时必须在同一
次变更中更新该文件、重新生成补丁、执行完整测试，并在 README 中记录兼容性结论。

## Overlay 组织方式

仓库只跟踪：

- `patches/`：对上游既有文件的最小补丁；
- `overlay/`：新增的独立 package、网页资源和嵌入资源；
- `scripts/`：后续用于准备源码、校验、构建和打包的脚本；
- `tests/`：Overlay 特有的测试说明和测试资源；
- `packaging/`：后续发布所需的服务和容器配置；
- 文档、许可证、第三方声明和上游锁定信息。

完整上游源码应在构建时进入被忽略的 `.cache/` 或 `tmp/`，构建产物进入被忽略的
`dist/`。不要在本目录创建嵌套 `.git`、Git submodule，或提交一份完整 frp 源码。

## 目录

| 路径 | 用途 |
|---|---|
| `docs/` | frpc/frps 详细设计和文档索引 |
| `patches/` | 修改上游既有文件的顺序补丁 |
| `overlay/client/telemetry/` | frpc Telemetry 新增文件的预留位置 |
| `overlay/server/publicclientinfo/` | frps 接收器、内存 Registry 和独立 Listener 的预留位置 |
| `overlay/web/` | 公开只读页面资源的预留位置 |
| `scripts/` | 后续构建与上游升级脚本；当前没有可执行脚本 |
| `tests/` | 后续 Overlay 专项测试和测试资源 |
| `packaging/` | 后续 systemd、容器和发布配置 |

## 计划中的构建流程

1. 读取并校验 `upstream.lock`。
2. 下载固定 Tag，并确认解析后的源码 Commit 完全一致。
3. 将上游源码放到 `.cache/` 或 `tmp/`。
4. 按 `patches/series` 的顺序应用补丁。
5. 把 `overlay/` 中的新增文件合入临时工作树。
6. 执行上游测试、Overlay 专项测试以及 frpc/frps 构建。
7. 将可发布文件输出到 `dist/`，不提交上游工作树和构建产物。

构建入口和脚本会与第一批实现代码一起增加；当前骨架不提供会产生虚假成功结果的
占位构建命令。

## 配置与公开仓库安全

- 本地默认配置放在项目根目录 `.env`，该文件不得提交；可提交字段说明放在
  [`.env.example`](.env.example)。
- 文档、Fixture、截图和日志中不得出现真实域名、IP、Token、证书或密钥。
- `localIP`、`localPort` 会暴露内部服务拓扑。部署者必须明确接受该风险，必要时在
  页面层隐藏字段或增加访问控制。
- 独立 Listener 默认只监听 `127.0.0.1`，推荐通过 Caddy/Nginx 发布 HTTPS。
- Telemetry 写入接口必须认证；公开只读页面不得复用写入凭据，也不得暴露原
  Dashboard、管理 API、Metrics 或调试路由。

## 许可证与来源

本子项目采用 Apache-2.0。上游来源、固定版本和预计修改范围记录在
[`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md)，完整许可证文本位于
[`LICENSE`](LICENSE)。
