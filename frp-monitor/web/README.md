# Web

P3 在公开节点页之外增加独立的 `/admin/` 管理工作台。公开页继续提供：节点在线/新鲜数、有效节点网速与 FRP 隧道
摘要，节点名称搜索、监控状态筛选和名称/状态/CPU 排序。卡片显示 CPU、内存、磁盘、
1/5/15 分钟负载、节点收发速率及累计量、运行时间、采样时间与间隔。
展开节点的「趋势与探测」可查看 1 小时、6 小时、24 小时或 7 天资源趋势、UTC
当日主机累计流量和 TCP 探测。宽屏展开卡片使用整行，窄屏保留单列与原生键盘操作。

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
  metrics_at, interval_seconds, frp: {control_state, proxy_total, proxy_running, reconciliation, server_online, registered},
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
P3 卡片增加服务端核对行：明确显示未绑定、冲突、过期等状态；仅核对通过才显示
独立的服务端在线状态与已登记隧道数。服务端登记数量不替代节点自报运行数量，
未知不视为零；公开摘要不包含代理名、身份或本地目标。
汇总网速与运行隧道只计入监控在线且数据新鲜的有效样本，并显示覆盖节点数。

刷新保留筛选控件和已有卡片 DOM，仅在排序或节点集合变更时移动/增删卡片。
SSE 断开或 10 秒没有任何快照显示独立连接提示，并保留明确标注的最后快照；退避重连先重新 GET，避免
将旧快照当成当前节点状态。应用不含演示节点或随机实时数据。

## 历史、流量与 TCP 探测

只有展开且未被搜索/筛选隐藏的卡片请求
`GET /api/public/v1/nodes/{id}/history?window=1h`，之后每 30 秒更新，单次请求
10 秒超时。关闭、隐藏或移除节点会取消请求与轮询；切换范围会取消旧请求并拒绝
迟到响应覆盖新范围。刷新失败保留上次成功结果，同时明确标注旧范围、更新时间与
失败提示。实时 SSE 与历史请求各自维护状态。

```text
{node_id, generated_at, window, step_seconds,
 storage: {state: ready|disabled|degraded, dropped},
 probes_state: disabled|waiting|ready|degraded,
 points: [{at, samples, coverage_seconds, cpu, mem_used, mem_total,
   net_rx, net_tx, load, fields?}],
 traffic: {day, rx_bytes, tx_bytes, coverage_seconds, resets},
 probes: [{id, name, interval_seconds, latest_at, latency_ms,
   samples, failures, failure_rate,
   points: [{at, latency_ms, samples, failures}]}]}
```

历史的内存与收发字节字段为十进制字符串或 null；CPU 与 load 为 number 或 null。
`fields` 可提供逐指标样本与覆盖信息，图表仅使用对应有效值。CPU、内存、负载和
收发速率以 SVG 折线展示，显式 null、0 样本和时间断层都会断开路径，真实 0 保留。
单点仍画圆点，不把缺样连成趋势或补零。每张图提供可见坐标范围、时间边界、最近
有效值及其时间、范围与有效时间段数，另有 SVG title/desc 供辅助技术读取。字节
使用 BigInt 格式化；绘图只把已归一化的有界比例转成 number。

今日流量按 UTC 日期统计服务端收到的有效网卡计数器差分，并展示覆盖秒数与重置
次数；null 是无有效数据，`"0"` 是真实 0。覆盖时间只计有效观察；同一计数范围可
补回断线增量，跨日按接收日归属可能有误差。它不等于节点卡片的系统计数器累计量、
FRP 隧道流量或流量账单。每日累计允许超过 uint64，以最多 80 位十进制 BigInt
处理；资源计数器仍限 uint64。精确字节数保留在累计值的 title 中，不经过 Number。

探测名称仅使用服务端公开标签；页面没有目标地址字段。失败率是所选范围的 TCP
建连失败次数百分比（0–100），不是 IP 丢包率。`latency_ms: -1` 的最近样本显示
「建连失败」，null 显示未知；耗时趋势只画成功样本的平均耗时。任务未配置、等待
节点启用、可执行、配置降级彼此区分。历史存储未启用时仍显示已配置的任务标签；
存储降级标记可能丢失的记录，正常但无采样时显示空状态。

## 嵌入与访问边界

`Handler() http.Handler` 提供根页面、`/admin/` 登录壳和显式允许的静态文件；
`go:embed` 包含 `index.html`、`admin.html`、`assets/`、`src/` 子树。README、Go
源码、测试、目录浏览、`.env`、`/admin.html` 均返回 404。`/admin/` 返回 no-store，
页面源不含任何私有节点数据。API/SSE 由 monitor 鉴权和挂载，不在静态处理器中实现。

静态响应有 CSP（script/style/connect 等只允许同源）、nosniff、拒绝 iframe 与
no-referrer。页面不依赖外部字体、CDN 或运行时跨子项目资源。公开数据必须由服务端
裁剪，不能把 agent 原始报告下发后再用 CSS 隐藏；页面不展示本地目标、主机名、IP、
精确内核、FRP rawClientID、网卡/boot ID 或凭据。

## 管理工作台

`/admin/` 使用独立管理令牌登录。`POST /api/admin/v1/login` 与
`GET /api/admin/v1/session` 返回 `{csrf_token, expires_at}`；浏览器只在内存保留
CSRF，后续写请求发送 `X-CSRF-Token`，会话 cookie 由服务端维护。401/403 会立即
清除当前私有快照、表单与一次性令牌，取消进行中的请求，返回登录页。管理未配置
时显示未启用。请求有 10 秒超时；退出、刷新、返回页面或过期会话后的迟到响应不
能重新展示旧数据。退出请求失败会明确说明服务端 cookie 可能仍有效。

初版每 10 秒读取管理快照，失败保留并标注上次成功快照；节点按钮 DOM 保持稳定，
刷新不覆盖正在编辑的绑定或任务表单。详情包含经过授权的主机事实、地址、代理本地
目标、节点自行报告的控制状态以及独立的 FRP 服务端核对结果。服务端注册表包含
普通 frpc；只有人工设置可信绑定并核对通过才显示监控节点归属。未绑定、冲突、
不匹配、过期、未登记与服务端不可用分别显示，不把未知状态变成离线。

FRP 连接数与累计字节使用十进制字符串；字节用 BigInt 展示。FRP 流量为服务端本地
日范围，接收方向为公网访客到 frpc，发送为 frpc 到公网访客，页面与主机网卡
速率及流量明确分开。管理页面不提供原生代理配置增删、远程命令或 shell。

可创建公开名称的节点、轮换或撤销凭据，并设置或解除可信 FRP 绑定。轮换和撤销
先展示具体节点与影响的内联确认。创建/轮换的令牌仅在本次成功响应后展示，可手动
复制或清除，刷新与退出立即清除；不写 localStorage、sessionStorage、URL 或日志。
主题设置沿用固定套件的存储键，与凭据隔离。

探测编辑器一次加载完整任务清单，保留所有节点的任务；用户修改执行节点、稳定
任务 ID、公开名称、私有目标及间隔，保存时递增版本并提交完整替换。移除行再保存
可停止任务；清空所有行表示完整清空。并发版本冲突提示重新读取，不自动重试覆盖。
任务存储未配置、配置降级、读取失败各有独立说明。浏览器拒绝不安全整数版本，
避免大于 `2^53-1` 时丢失版本精度；此极端版本需使用服务端配置工具维护。

## 验证

在仓库根目录运行无需 npm 安装的纯逻辑/重连测试与静态安全边界测试：

```sh
node --test frp-monitor/web/tests/*.test.mjs
GO111MODULE=off go test ./frp-monitor/web
```

`format.test.mjs` 覆盖 uint64 极值、未知/0 区分、筛选排序以及摘要排除过期样本；
`transport.test.mjs` 覆盖先 GET 后 SSE、重连重新 GET、旧连接迟到帧和异常响应。
`history.test.mjs` 覆盖 BigInt、缺样断线、单点与真实零、响应身份/范围校验、展开
才请求、关闭取消、切换范围拒绝旧响应及失败保留成功数据。
`handler_test.go` 验证资源 allowlist、MIME、CSP 和只读方法。构建脚本的 Go 测试还会
在固定 FRP module 中运行该包。完整双进程验收见 `tests/p1_smoke.py`。

P1 开发验证使用有明确“测试向量”名称的本地临时 fixture：浅/深色、390px 窄屏、
正常/离线/等待/过期卡片、搜索筛选和 SSE 下焦点保留；无水平溢出与 CSP 控制台错误。
fixture 不进入嵌入文件，也不会在真实服务无数据时显示。

本机 Darwin P1 二进制也已通过真实 loopback 页面复核：GET/SSE 正常、无 Proxy 的
FRP 为已连接且 `0 / 0`，未接入节点为等待且隧道数 `—`。非 Linux 主机按采集器
契约显示资源暂不支持，而不是模拟采样；浏览器控制台没有错误。

P2 在未嵌入项目的临时测试向量上检查了浅/深色、390px、键盘展开/收起与控件焦点、
7 天范围、SVG 缺样断点、存储未启用仍显示任务、请求失败保留旧图并标注范围。
窄屏无水平溢出；正常响应时控制台无 CSP 或 JavaScript 错误。测试向量和截图不提交。

真实 Darwin P2 双进程也已复核：四个时间范围均通过前端契约校验，SQLite 首次归档
后显示真实 loopback TCP 探测的 12 次成功结果、0.0% 失败率与两个有效时间段。
macOS 资源保持 unsupported/空图，当日流量为未知而非 0；公开响应不含探测目标。
真实页面无横向溢出、控制台错误或警告，验收临时浏览器页已关闭。


P3 增加 8 项 Node 测试：CSRF/同源 cookie、401/403 清除、迟到凭证响应、超时、
退出失败清除本页内存、快照身份校验、探测完整替换/版本冲突边界、FRP 精度与未知值。
静态测试验证 `/admin/` 空登录壳、no-store、CSP 和显式静态 allowlist。
在未嵌入的本地测试向量中验证了登录、模拟创建/轮换后一次性显示、刷新清除、
探测保存再读取、主机/FRP 字段、390px 深色布局无横向溢出与空控制台错误。
这些模拟凭据不能用于真实节点，测试向量与截图不提交。


真实 Darwin P3 双进程也通过管理页面验收：独立令牌登录、创建临时节点、43 字符
一次性令牌展示、轮换、可信绑定保存/解除、撤销恢复原节点集合，以及探测公开标签
保存/重读/恢复。原节点保持监控在线和可信对账匹配；非 Linux 主机资源仍如实显示
暂不支持。公开/管理页面未混入测试向量，管理令牌、返回凭据未进入截图或文档。

真实管理会话在服务端热轮换管理令牌后自动退回登录，绑定草稿、主机详情和一次性
令牌均清空；原测试令牌随后恢复。390px 深色无横向溢出，跨多次自动快照更新后
表单焦点与未保存输入保留，正常页面控制台没有 JavaScript 或 CSP 错误。

最终重建二进制再次验证公开卡片显示「已核对 · 服务端在线 · 已登记 0 条」，
390px 保持无横向溢出。管理页重登录、正常注销和刷新后保持未登录均通过；浏览器
验收页与临时模拟服务已关闭。
