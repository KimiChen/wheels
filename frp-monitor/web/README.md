# Web

P1 提供随 monitor 嵌入的中文公开节点页：节点在线/新鲜数、有效节点网速与 FRP 隧道
摘要，节点名称搜索、监控状态筛选和名称/状态/CPU 排序。卡片显示 CPU、内存、磁盘、
1/5/15 分钟负载、节点收发速率及累计量、运行时间、采样时间与间隔。

## 固定套件来源

`assets/style.css` 与 `assets/script.js` **完整复制且不修改**自本仓库提交
`6d41d588b3537b2d2460d73f999d958fd37eb00d` 的 `web-standard-kit/`。
不要从工作区最新版本或 CDN 隐式更新；升级须同步根 README 的锁定来源与回归结果。

```sh
git show 6d41d588b3537b2d2460d73f999d958fd37eb00d:web-standard-kit/style.css
git show 6d41d588b3537b2d2460d73f999d958fd37eb00d:web-standard-kit/script.js
```

保留 128 个设计令牌、`wsk-` 组件、`@layer wsk`、文档级委托事件和 `wsk.mount(root)`
契约。业务样式在 `assets/app.css`；原生 ES Modules 在 `src/`。主题仍使用
`web-standard-kit-theme` 存储键以及 system → light → dark 的循环；首帧恢复由独立
`assets/theme.js` 完成，不需要 inline script。按钮、输入和选择器采用原生键盘交互，
页面含跳转链接、焦点环和具名 progress；所有实时文本均使用 `textContent`。

## 实时数据契约

先 `GET /api/public/v1/nodes`，再建立 `/events/public` 的 `EventSource`，消费每秒
`event: snapshot`。两者为同一 shape：

```text
{generated_at, nodes: [{id, name, session, freshness, last_seen,
  metrics_at, interval_seconds, frp: {control_state, proxy_total, proxy_running},
  metrics: null | {scope, cpu, load, mem_total, mem_used, swap_total, swap_used,
    disk_total, disk_used, net_rx, net_tx, net_rx_total, net_tx_total,
    uptime, tcp, udp, procs}}]}
```

`id` 是安装时生成的公开随机 `agent_id`，仅用于内存中的卡片索引，不写入可见文本；
`name` 来自服务端凭据文件的公开名称，不由 hostname 推断。`metrics.scope` 是字符串；
其余字段使用 `{value, quality, reason?}`。CPU 为百分比 number，load 为三个 number，
所有 uint64 value 均为十进制 string；容量/累计量单位为 bytes，网速 bytes/s，
运行时间为秒。浏览器以 `BigInt` 格式化累计量和容量比，避免超过 `2^53` 后丢失精度。

`quality != ok` 或缺失显示 `—`，首样本提示等待差分采样；有效 0 保留为 0。
未报告 FRP 或 `control_state: unknown` 时不显示虚假的 `0 / 0` 隧道数。
监控会话、数据新鲜度、FRP 控制连接彼此独立，不从其中一个状态推断另外两个。
汇总网速与运行隧道只计入监控在线且数据新鲜的有效样本，并显示覆盖节点数。

刷新保留筛选控件和已有卡片 DOM，仅在排序或节点集合变更时移动/增删卡片。
SSE 断开或 10 秒没有任何快照显示独立连接提示，并保留明确标注的最后快照；退避重连先重新 GET，避免
将旧快照当成当前节点状态。应用不含演示节点或随机实时数据。

## 嵌入与访问边界

`Handler() http.Handler` 仅提供根页面与显式允许的静态文件；`go:embed` 只包含当前
`index.html`、`assets/`、`src/` 子树。README、Go 源码、测试、目录浏览、`.env` 与
尚未实现的 `/admin` 均返回 404。API/SSE 由 monitor 挂载，不在静态处理器中实现。

静态响应有 CSP（script/style/connect 等只允许同源）、nosniff、拒绝 iframe 与
no-referrer。页面不依赖外部字体、CDN 或运行时跨子项目资源。公开数据必须由服务端
裁剪，不能把 agent 原始报告下发后再用 CSS 隐藏；页面不展示本地目标、主机名、IP、
精确内核、FRP rawClientID、网卡/boot ID 或凭据。

当前没有管理写入、节点详情、历史趋势、探测任务与隧道管理入口；范围见根 README。

## 验证

在仓库根目录运行无需 npm 安装的纯逻辑/重连测试与静态安全边界测试：

```sh
node --test frp-monitor/web/tests/*.test.mjs
GO111MODULE=off go test ./frp-monitor/web
```

`format.test.mjs` 覆盖 uint64 极值、未知/0 区分、筛选排序以及摘要排除过期样本；
`transport.test.mjs` 覆盖先 GET 后 SSE、重连重新 GET、旧连接迟到帧和异常响应。
`handler_test.go` 验证资源 allowlist、MIME、CSP 和只读方法。构建脚本的 Go 测试还会
在固定 FRP module 中运行该包。完整双进程验收见 `tests/p1_smoke.py`。

P1 开发验证使用有明确“测试向量”名称的本地临时 fixture：浅/深色、390px 窄屏、
正常/离线/等待/过期卡片、搜索筛选和 SSE 下焦点保留；无水平溢出与 CSP 控制台错误。
fixture 不进入嵌入文件，也不会在真实服务无数据时显示。

本机 Darwin P1 二进制也已通过真实 loopback 页面复核：GET/SSE 正常、无 Proxy 的
FRP 为已连接且 `0 / 0`，未接入节点为等待且隧道数 `—`。非 Linux 主机按采集器
契约显示资源暂不支持，而不是模拟采样；浏览器控制台没有错误。
