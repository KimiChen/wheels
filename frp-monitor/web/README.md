# frp-monitor Web

基于本仓库 `web-standard-kit` 的监控页面，静态资源随 monitor 二进制嵌入发布。
页面与权限设计见根目录 [README.md](../README.md) §7。

## 页面结构

```text
web/
├── assets.go              # package web：//go:embed all:static，Static() fs.FS 返回 static 子树
└── static/
    ├── index.html         # / 总览 + 节点列表（公开只读）
    ├── node.html          # /node.html?id= 公开节点详情（含趋势 / 探测 / 流量 / 关联隧道）
    ├── tunnels.html       # /tunnels.html 全量隧道列表（公开只读，SSE + 10s 轮询兜底）
    ├── admin.html         # /admin.html 管理端（登录 + 完整节点详情 + 日流量表 + 凭据 / 备份设置）
    ├── assets/
    │   ├── wsk/           # web-standard-kit 锁定快照（style.css / script.js，见下）
    │   └── app.css        # 业务样式：@layer app，只用套件令牌（含 fm-chart-* 图表样式）
    └── src/               # 原生 ES Modules，无构建步骤
        ├── api.js         # fetch 封装：publicApi / adminApi，ApiError(status, code)；probeBackup 探测备份可用性
        ├── stream.js      # EventSource 封装：snapshot 全量 / node 增量 / tunnels 全量，指数退避重连
        ├── format.js      # 字节 / 速率 / 占比 / 失败率 / 延迟 / 时间格式化；null 一律「未知」
        ├── charts.js      # SVG 折线（无图表库）：null 缺口断线、孤立点成圆点、数值摘要
        └── views/
            ├── status.js  # 三状态徽章（会话 / 指标新鲜度 / FRP）、连接提示、服务端时钟
            ├── overview.js# 总览卡片渲染与按节点表重算；tunnels 事件刷新隧道卡片
            ├── nodes.js   # 节点表格：snapshot 重建 tbody；node 事件就地更新单元格
            ├── detail.js  # 详情共享渲染：状态 / 资源 / 探测 / 流量 / Proxy / 隧道 / 事件 / Facts / frp_clients
            ├── trends.js  # 趋势区块控制器：range 切换、enabled:false 空态、失败节流重试
            ├── index.js   # 总览页控制器
            ├── node.js    # 公开详情页控制器（含 404 空态）
            ├── tunnels.js # 隧道页控制器（行键调和就地更新 + 未关联节点标注）
            └── admin.js   # 管理端控制器（登录 / 退出 / 列表 / 详情联动 / 日流量表 / 凭据 / 备份）
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
| `GET /api/public/v1/overview` | `{now, nodes_total, nodes_online, nodes_offline, frp_clients_online, tunnels_total, tunnels_online, net_rx_bps, net_tx_bps}` |
| `GET /api/public/v1/nodes`、`/api/public/v1/nodes/{id}` | `{now, nodes:[PublicNode]}` / `{now, node}` |
| `GET /api/public/v1/tunnels` | `{now, tunnels:[{node_id, name, type, online, cur_conns, today_rx_bytes, today_tx_bytes}]}`；`node_id` 为 null 表示服务端未关联到监控节点 |
| `GET /api/public/v1/nodes/{id}/metrics?range=1h\|6h\|24h\|7d` | 历史趋势：`{"enabled":true,"range","t0","step","series":{cpu,load1,load5,load15,mem_used_bytes,swap_used_bytes,disk_used_bytes,net_rx_bps,net_tx_bps,tcp,udp,procs}}`；定长数组、断线缺口为 null、字节为十进制字符串；服务端未开启历史（DataDir 空）返回 `{"enabled":false}` |
| `GET /events/public` | SSE：首事件 `event: snapshot`（`{now, overview, nodes}`），之后 `event: node`（单个 PublicNode）；隧道变化时 `event: tunnels`（`{"tunnels":[...]}` 全量，公开裁剪）；心跳为 `: ping` 注释行 |
| `POST /api/admin/v1/login` | `{"password":"..."}` → 200 `{"ok":true}` 设 Cookie；401 失败 |
| `POST /api/admin/v1/logout`、`GET /api/admin/v1/session` | 会话管理；200 / 401 |
| `GET /api/admin/v1/nodes`、`/api/admin/v1/nodes/{id}`、`GET /events/admin` | 同公开路由，载荷为 AdminNode；`tunnels` 事件为管理裁剪（含 `user / client_id / local_addr`） |
| `GET /api/admin/v1/nodes/{id}/traffic/daily?days=7` | 近 N 日节点网卡流量：`{"days":[{"day":"2026-09-26","rx_bytes":"...","tx_bytes":"..."}]}`（仅管理端） |
| `GET /api/admin/v1/nodes/{id}/events?limit=50` | 最近事件倒序：`{"events":[{"ts":unix,"kind":"tunnel_online\|tunnel_offline\|client_online\|client_offline","name","detail"}]}`（仅管理端） |
| `GET /api/admin/v1/credentials` | `{"credentials":[{"id","comment","created_at"}]}`，不含 Token |
| `POST /api/admin/v1/credentials` | `{"id","comment"}` → `{"id","token"}`，Token 仅此一次明文返回；409 `{"error":"conflict"}` 表示 ID 已存在 |
| `DELETE /api/admin/v1/credentials/{id}` | 吊销 → `{"ok":true}` |
| `GET /api/admin/v1/backup` | 200 `application/octet-stream` 文件下载（Cookie 会话直接 `<a href>` 即可）；409 `{"error":"no_data_dir"}` 表示未开持久化 |
| 错误 | 401 `{"error":"unauthorized"}`；404 `{"error":"not_found"}` |

PublicNode 字段：`id, online, metrics_stale, report_interval, collected_at, last_seen,
cpu, load[3], mem_*/swap_*/disk_*（十进制字符串）, net_rx/net_tx（B/s 浮点）,
uptime/tcp/udp/procs, frp_control_connected, proxies[{name,type,enabled,status}],
probes[{id,last_latency_ms,fail_rate,samples}], traffic{today_*,total_*} | null,
tunnels[{name,type,online,cur_conns,today_rx_bytes,today_tx_bytes}]`。
AdminNode 额外含 `facts{...}`、`boot_id, iface, net_rx_total, net_tx_total,
frp_client_id, frp_clients[...], connected_at`、`proxies[].local_addr`、
`probes[].target` 及 `tunnels[].user / client_id / local_addr`。

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
- 隧道：隧道页与节点详情「关联隧道」均标注「服务端隧道收 / 发，与节点网卡流量
  口径不同，不相加」。隧道页行键为 `node_id + 名称`，SSE `tunnels` 事件按行键
  调和就地更新（保留筛选 / 排序 / 焦点），另有 10 秒 REST 轮询兜底；`node_id`
  为 null 或不在已知节点列表中时节点列显示「未关联节点」（原始 ID 在 title）。
  公开页无 `local_addr / user / client_id` 列；管理端详情隧道表含完整列。
- 事件：管理端节点详情「最近事件」表（时间 / 类型 / 名称 / 详情）仅在选择节点时
  拉取一次（`events?limit=50`，服务端倒序），不随 SSE 增量重复请求。
- 凭据与备份（管理端设置区）：凭据列表只含 id / 备注 / 创建时间；新建走真实
  POST，成功后 Token 仅一次明文展示在只读输入框，提供复制按钮（Clipboard API，
  非安全上下文回退选中复制），可见期间刷新 / 关闭页面有浏览器确认提示，「我已保存」
  后从 DOM 清除；409 冲突在表单内联提示。吊销经 `<dialog>` 二次确认后发 DELETE。
  备份为直接 `<a href download>`，进入管理端时先探测一次：409 `no_data_dir`
  时禁用并说明。所有写操作 fetch + JSON；401 一律回到登录表单。
- 公开页无 Facts / 本地目标 / 探测 target / 日流量 / 事件 / 凭据等区块（服务端
  裁剪之外，模板中也不存在这些列）。

## 本地验证

无构建步骤。任意静态服务器挂到 `static/` 即可查看外壳；实时数据需要后端
（或按上表契约的 mock）。JS 语法检查：`node --check static/src/**/*.js`。
