# frp-monitor Web

基于本仓库 `web-standard-kit` 的监控页面，静态资源随 monitor 二进制嵌入发布。
页面与权限设计见根目录 [README.md](../README.md) §7。

## 页面结构

```text
web/
├── assets.go              # package web：//go:embed all:static，Static() fs.FS 返回 static 子树
└── static/
    ├── index.html         # / 总览 + 节点列表（公开只读）
    ├── node.html          # /node.html?id= 公开节点详情（含趋势 / 探测 / 流量）
    ├── admin.html         # /admin.html 管理端（登录 + 完整节点详情 + 日流量表）
    ├── assets/
    │   ├── wsk/           # web-standard-kit 锁定快照（style.css / script.js，见下）
    │   └── app.css        # 业务样式：@layer app，只用套件令牌（含 fm-chart-* 图表样式）
    └── src/               # 原生 ES Modules，无构建步骤
        ├── api.js         # fetch 封装：publicApi / adminApi，ApiError(status, code)
        ├── stream.js      # EventSource 封装：snapshot 全量 / node 增量，指数退避重连
        ├── format.js      # 字节 / 速率 / 占比 / 失败率 / 延迟 / 时间格式化；null 一律「未知」
        ├── charts.js      # SVG 折线（无图表库）：null 缺口断线、孤立点成圆点、数值摘要
        └── views/
            ├── status.js  # 三状态徽章（会话 / 指标新鲜度 / FRP）、连接提示、服务端时钟
            ├── overview.js# 总览卡片渲染与按节点表重算
            ├── nodes.js   # 节点表格：snapshot 重建 tbody；node 事件就地更新单元格
            ├── detail.js  # 详情共享渲染：状态 / 资源 / 探测 / 流量 / Proxy / Facts / frp_clients
            ├── trends.js  # 趋势区块控制器：range 切换、enabled:false 空态、失败节流重试
            ├── index.js   # 总览页控制器
            ├── node.js    # 公开详情页控制器（含 404 空态）
            └── admin.js   # 管理端控制器（登录 / 退出 / 列表 / 详情联动 / 日流量表）
```

## 套件快照

`static/assets/wsk/` 复制自本仓库 `web-standard-kit@6d41d588b3537b2d2460d73f999d958fd37eb00d`
（`style.css` 与 `script.js` 原样拷贝，未修改）。主题三态（跟随系统 / 浅色 / 深色）、
`@layer wsk`、`light-dark()` 令牌、键盘导航与 `prefers-reduced-motion` 均来自套件；
集合（筛选 / 排序 / 分页）使用套件 `data-table` 契约，新行片段插入后调用
`wsk.mount(fragment)` 完成初始渲染。登录等真实表单不使用 `data-demo-submit`。

## 与后端的接口契约（摘要）

页面按 `/` 为根挂载，引用均为根相对路径（`/assets/...`、`/src/...`）。

| 路由 | 说明 |
| --- | --- |
| `GET /api/public/v1/overview` | `{now, nodes_total, nodes_online, nodes_offline, frp_clients_online, net_rx_bps, net_tx_bps}` |
| `GET /api/public/v1/nodes`、`/api/public/v1/nodes/{id}` | `{now, nodes:[PublicNode]}` / `{now, node}` |
| `GET /api/public/v1/nodes/{id}/metrics?range=1h\|6h\|24h\|7d` | 历史趋势：`{"enabled":true,"range","t0","step","series":{cpu,load1,load5,load15,mem_used_bytes,swap_used_bytes,disk_used_bytes,net_rx_bps,net_tx_bps,tcp,udp,procs}}`；定长数组、断线缺口为 null、字节为十进制字符串；服务端未开启历史（DataDir 空）返回 `{"enabled":false}` |
| `GET /events/public` | SSE：首事件 `event: snapshot`（`{now, overview, nodes}`），之后 `event: node`（单个 PublicNode），心跳为 `: ping` 注释行 |
| `POST /api/admin/v1/login` | `{"password":"..."}` → 200 `{"ok":true}` 设 Cookie；401 失败 |
| `POST /api/admin/v1/logout`、`GET /api/admin/v1/session` | 会话管理；200 / 401 |
| `GET /api/admin/v1/nodes`、`/api/admin/v1/nodes/{id}`、`GET /events/admin` | 同公开路由，载荷为 AdminNode |
| `GET /api/admin/v1/nodes/{id}/traffic/daily?days=7` | 近 N 日节点网卡流量：`{"days":[{"day":"2026-09-26","rx_bytes":"...","tx_bytes":"..."}]}`（仅管理端） |
| 错误 | 401 `{"error":"unauthorized"}`；404 `{"error":"not_found"}` |

PublicNode 字段：`id, online, metrics_stale, report_interval, collected_at, last_seen,
cpu, load[3], mem_*/swap_*/disk_*（十进制字符串）, net_rx/net_tx（B/s 浮点）,
uptime/tcp/udp/procs, frp_control_connected, proxies[{name,type,enabled,status}],
probes[{id,last_latency_ms,fail_rate,samples}], traffic{today_*,total_*} | null`。
AdminNode 额外含 `facts{...}`、`boot_id, iface, net_rx_total, net_tx_total,
frp_client_id, frp_clients[...], connected_at`、`proxies[].local_addr` 及
`probes[].target`。

展示约定：

- 三种状态分列展示：监控会话（`online`）、指标新鲜度（`metrics_stale=true` →
  「数据过期」）、FRP 控制连接（`frp_control_connected`）。任何 null 显示「未知」，
  不显示 0。
- 大整数字段为十进制字符串：`format.js` 在 Number 安全范围内解析格式化；超出时用
  BigInt 换算并显示「原始字符串（约 X）」。
- 实时更新：`snapshot` 只重建 `<tbody>`（保留筛选、排序状态、页码与滚动位置）；
  `node` 事件就地更新单元格文本与徽章，不重排整表；断线时顶栏提示并由
  `stream.js` 指数退避重连，重连成功服务端重发 `snapshot`。
- 趋势：数据来自 metrics 历史接口（range 切换 1h/6h/24h/7d），SVG 折线按
  CPU / 内存 / 磁盘 / 网速 / 负载 / 连接与进程分组，附「当前 / 均值 / 峰值」摘要；
  null 缺口不连线。`enabled:false` 显示「历史未开启」说明而非报错。实时当前值仍由
  SSE 负责，趋势不轮询；同节点同范围不重复请求，失败后 30 秒内不自动重试。
- TCP 探测：措辞统一为「TCP 探测失败率」（近 15 分钟窗口握手失败占比），不代表
  真实 IP 丢包率；无样本时延迟与失败率显示「未知」；无任务空态「未配置探测任务」。
  节点列表列显示各任务失败率最差值，无任务显示「—」。
- 流量：仅展示主机网卡侧（标注「节点网卡流量」「节点收 / 发」），与 FRP 隧道流量
  口径不同、不相加；`traffic` 为 null 时各项显示「未知」。节点列表「今日流量」为
  今日收 + 发合计；管理端另有近 7 日流量表（仅选择节点时拉取一次）。
- 公开页无 Facts / 本地目标 / 探测 target / 日流量等区块（服务端裁剪之外，模板中
  也不存在这些列）。

## 本地验证

无构建步骤。任意静态服务器挂到 `static/` 即可查看外壳；实时数据需要后端
（或按上表契约的 mock）。JS 语法检查：`node --check static/src/**/*.js`。
