# Web

公开监控采用顶部导航、紧凑节点栏、卡片/表格视图和底部实时状态栏。
节点栏显示在线、离线、等待接入及总数，提供名称搜索与多分组切换，按服务端顺序展示；
不提供地球、可用性、标签、状态或排序筛选。

分组栏提供“全部”和包含公开节点的分组，数量只统计公开节点。一个节点可以
属于多个组，分组筛选与名称搜索取交集，卡片/表格显示同一结果，汇总保持全部公开节点口径。
选中分组在当前浏览器会话中记忆，SSE 和重连不会清除有效选择；组消失时回到“全部”。
真实分组“全部”“未分组”用其数字 ID 区分，不与内置筛选冲突。卡片、表格及详情仅展示已配置的组名；未分组节点不显示分组文字，仍在“全部”中展示。

卡片显示系统摘要、资源使用率和双向实时速率。每项资源第一行左侧显示 CPU 逻辑核心数、
内存总容量、硬盘总容量或周期流量限额，右侧显示百分比，第二行显示整宽进度条。
浅色主题采用黑色数值、50% 透明度的黑色填充、浅灰底轨、灰色名称；百分比不加粗，
深色主题随正文色适配。进度条高度为套件默认值的三分之一（`0.5rem / 3`）。
不显示第三行用量数值。累计、今日、套餐用量/时间、费用和备注在详情页查看。
名称和备注来自管理员配置，不从 hostname、IP 或其他采集信息推断。
CPU 负载仍可在表格和详情页查看；状态旁的时长是系统 uptime，
不代表连续监控在线时间。底栏的 CPU 均值与网速只汇总在线且新鲜的有效样本。

卡片与表格链接到 `/node/{id}`，例如 `/node/1`，支持直接访问和刷新。
详情页顶部以紧凑信息栏展示硬件、系统累计、运行时间与采样状态；下方分为“详情”
和“网络”。详情采用 CPU、内存、硬盘三列曲线卡片，随后显示网络速率和系统负载。
卡片标题显示实时值，内存和硬盘同时标注容量；网络页显示 TCP 探测。主控时区今日
流量、套餐用量与费用在资源采样信息上方使用与系统信息一致的紧凑信息栏直接展示。节点不存在、取消公开或被撤销时，
清空旧详情并停止历史请求；浏览器断流时保留且标注最后快照。

卡片/表格视图可以本地记忆，存储不可用时仍能使用。窄屏卡片单列，表格只在自身
容器横向滚动，详情纵向排列。入口和表单支持键盘操作。

## 固定套件与业务文件

`assets/style.css` 与 `assets/script.js` 完整复制且不修改，来源为本仓库提交
`6d41d588b3537b2d2460d73f999d958fd37eb00d` 的 `web-standard-kit/`。
不得从工作区最新版本或 CDN 隐式更新；升级时同步项目 README 的锁定来源与验证结果。

```sh
git show 6d41d588b3537b2d2460d73f999d958fd37eb00d:web-standard-kit/style.css
git show 6d41d588b3537b2d2460d73f999d958fd37eb00d:web-standard-kit/script.js
```

保留 128 个设计令牌、`wsk-` 组件、`@layer wsk`、文档级委托事件和 `wsk.mount(root)`
契约。首页、节点详情、GitHub 登录和管理工作台统一使用套件的顶栏、卡片、徽章、
分段切换、进度条、按钮、表格、表单、提示和原生对话框；动态创建的历史卡片、
探测表单与 FRP 表格也使用相同组件。保留现有布局、字段和操作，业务样式
`assets/app.css`、`assets/node.css`、`assets/admin.css` 只补充布局、图表及监控状态，
不重新定义全局主题或基础控件外观。
业务代码使用 `src/` 下的原生 ES Modules。主题使用 `web-standard-kit-theme`
存储键，按 system → light → dark 循环；首帧恢复由 `assets/theme.js` 完成。
页面包含跳转链接、焦点环、具名 progress 和图表文字说明；实时文本使用 `textContent`。

## 实时接口与精度

页面先请求 `GET /api/public/v1/nodes`，再通过 `/events/public` 的 `EventSource`
消费 `event: snapshot`。JSON 与 SSE 使用同一服务端裁剪结构：

```text
{generated_at, nodes: [{
  id, name, groups: [{id, name}], public_note, session, freshness, last_seen, metrics_at,
  interval_seconds, accounting_state,
  hardware: null | {os, arch, virt, cpu_name, cpu_cores, agent_version},
  metrics: null | {cpu, load, mem_total, mem_used, swap_total, swap_used,
    disk_total, disk_used, net_rx, net_tx, net_rx_total, net_tx_total,
    uptime, tcp, udp, procs},
  frp: {control_state, proxy_total, proxy_running, reconciliation,
    server_online, registered, today_rx_bytes, today_tx_bytes, traffic_scope},
  traffic_today: {day, timezone, rx_bytes, tx_bytes, partial},
  traffic_plan?: {quota_bytes, mode, reset_mode, reset_day, reset_timezone,
    period_start_at_ms, period_end_at_ms, rx_bytes, tx_bytes, used_bytes, partial},
  billing?: {price_minor, currency, billing_cycle, expires_at_ms, renewal_note}
}]}
```

`id` 是 SQLite 自增正整数，以十进制字符串传输，范围不超过有符号 64 位整数。
节点路由拒绝 0、前导零、负数、溢出值和额外路径片段。
`name` 是管理员保存的名称。
`accounting_state` 由服务端随快照下发（`ready` / `degraded`），当前页面不直接读取：
记账降级时服务端已把 `traffic_today` 与 `traffic_plan` 的 `partial` 置真；
管理页仍可查看完整性提示，公开首页和节点详情不显示「统计不完整」。保留该字段供诊断使用。
硬件与其他实时指标使用 `{value, quality, reason?}`：CPU、load 和逻辑核心数为
number，硬件文本为 string，uint64 指标为十进制 string。容量和流量单位为 bytes，
网速为 bytes/s，运行时间为秒。浏览器使用 `BigInt` 处理字节值，避免超过 `2^53`
后的精度损失。累计量允许超过单个系统 uint64 计数器的范围。

首页（卡片和表格）与节点详情不展示 FRP 连接、隧道数量、核对状态或隧道流量。
公开接口保留 `frp` 字段，供现有 API 客户端使用。
无效质量或缺失指标显示 `—`，有效 0 仍显示 0。管理页未报告 FRP 或控制状态未知时，
不显示虚假的 `0 / 0` 隧道数。监控会话、数据新鲜度、FRP 控制连接各自独立。
服务端登记数量与节点自报运行数量分别展示；未绑定、冲突、过期、未登记及不可用
保持不同状态。仅可信核对通过时展示服务端归属观测。

列表刷新保留搜索框和已有卡片/表格行 DOM，按节点集合变化增删或移动元素。
分组目录只从公开节点的 `groups` 派生，不请求管理目录；空组和隐藏节点专属组不可见。
SSE 断开或 10 秒未收到快照时提示浏览器连接异常，并保留最后快照；退避重连前
重新 GET，迟到响应不能覆盖新连接。应用不生成演示节点或随机实时数据。

## 四种流量与当前套餐

| 统计项目 | 数据来源 | 时间边界 |
|---|---|---|
| 系统累计 | 最新有效 Metrics 的网卡 RX/TX 累计值 | 可随主机重启、接口变化归零 |
| 今日流量 | SQLite 保存的有效系统计数器增量，随实时快照下发 | 主控机器系统时区当天零点至次日零点 |
| 套餐用量 | SQLite 当前周期 RX/TX 与管理员校准差额 | 套餐重置方式、日期和时区 |
| FRP 隧道流量（仅管理页展示） | 原生 FRP 服务端的可信节点观测 | FRP 服务端本地日及进程重启口径 |

今日和套餐数据直接读取实时快照，不依赖历史接口或 TSDB。
`partial` 表示统计不完整，例如中途接入、计数器重置、观察缺口或记账降级；
初始累计为 0 不代表接入前用量已知。系统累计、今日、套餐和 FRP 流量不会相加。
FRP 接收方向为公网访客 → frpc，发送为 frpc → 公网访客，部分协议在连接关闭时计入。
公开接口不包含代理名、私有目标或 FRP 原始身份；管理页无法可信核对的流量显示未知。

套餐类型支持 `max`、`total`、`rx`、`tx`：分别对整个周期累计收发取较大值、相加、
仅接收或仅发送，再加校准差额。公开页使用服务端 `used_bytes`，不自行推断账本。
未设置额度（`quota_bytes: null`）表示无限流量，显示 `∞`，有效已用量对应使用率为
0%；未公开套餐或数据缺失显示 `—`，不当作无限流量。零额度是实际 0，百分比留空；
非零额度允许显示超过 100% 和超出量，进度条最多填满 100%。

套餐默认无限流量、每月 1 日按套餐时区重置。支持每月固定日期和手动重置；
月内没有指定日期时取月末。
修改套餐立即生效，切换计费类型保留当前显示已用量并继续累计。
今日统计、当前套餐与基线合并在节点行中，不提供历史每日或历史套餐账本。
费用、到期和续费说明由管理员维护，不自动扣款或延期。

业务时间戳为 UTC 毫秒；快照、到期和周期时间在浏览器按本地时区显示。
今日统计的日期和时区直接采用主控返回值，套餐重置时区独立显示。

## 可选指标历史与 TCP 探测

指标历史使用服务进程内嵌的 VictoriaMetrics TSDB，不需要额外进程。原生 FRP 配置的
`monitor.historyDataPath` 默认为空，表示关闭；配置非空目录即启用，没有独立开关。
只有存在且当前可见的独立节点详情页请求
`GET /api/public/v1/nodes/{id}/history?window=1h`，之后每 30 秒刷新，单次请求
10 秒超时。支持 1 小时、6 小时、24 小时和 7 天范围；首页不轮询历史。
离开页面、进入后台或节点消失时取消请求；切换范围拒绝旧响应。
刷新失败保留上次成功结果，并标注范围、更新时间和失败提示。
实时连接与历史请求分别维护状态。

```text
{node_id, generated_at, window, step_seconds,
 storage: {state: ready|disabled|degraded, dropped},
 probes_state: disabled|waiting|ready|degraded,
 points: [{at, samples, coverage_seconds, cpu, mem_used, mem_total,
   net_rx, net_tx, load, fields}],
 probes: [{id, name, interval_seconds, latest_at, latency_ms,
   samples, failures, failure_rate,
   points: [{at, latency_ms, samples, failures}]}]}
```

关闭历史时保留资源卡片的实时值并提示暂无曲线，今日与套餐仍独立累计；后端降级时标注记录可能
不完整；后端正常但没有样本时显示空状态。TSDB 不能恢复人工校准后的套餐用量。

CPU、内存、硬盘、负载和收发速率使用 SVG 折线。图表只读取相应指标的有效采样，
显式 null、无样本时段和时间断层均留出缺口，真实 0 保留；单点仍可见。
图表提供纵轴刻度和时间边界，最近有效历史值及样本摘要放在“采样详情”中，
与卡片标题的实时值区分，并用 SVG title/desc 支持
辅助技术。绘图只将归一化后的有界比例转换为 number，字节格式化保持整数精度。

公开探测只包含任务 ID 和公开名称，不含目标地址。TCP 建连失败率是所选范围的
失败次数占比，不是 IP 丢包率。`latency_ms: -1` 表示建连失败，null 表示未知；
耗时曲线只统计成功建连。未配置、等待节点启用、可执行和配置降级分别提示。

## 管理工作台

后台保留本站顶栏，其下为横向导航和整宽工作区，使用 Web Standard Kit 组件与主题。
导航通过 `#nodes`、`#probes`、`#frp` 切换节点、网络探测和 FRP 注册表。
标题与搜索、刷新、添加操作并排，窄屏操作栏换到下一行；表格只在自身容器内滚动。
节点表按独立 ID、名称、IP、状态、套餐流量、价格、到期和操作排列，支持名称/ID 搜索。
添加节点和编辑采用原生对话框，节点编辑分为设置、资源、FRP 绑定和接入凭据；
设置按基本信息、流量套餐、费用与续费组织，关闭窗口后再次打开同一节点保留未保存的设置。

`/admin/` 只提供“使用 GitHub 登录”。浏览器通过匿名
`GET /api/admin/v1/auth` 获取 `{provider: "github", enabled}`，未配置时说明原因。
点击 `/api/admin/v1/auth/github` 发起 OAuth，服务端处理回调后重定向管理页。
没有管理员令牌登录或备用密码；GitHub Client Secret、OAuth access token 和
管理员白名单均由服务端处理，不进入公开 API 或页面存储。
登录失败参数只映射为固定提示，不直接显示外部错误文本。

`GET /api/admin/v1/session` 返回 CSRF 与会话期限，浏览器仅在内存保存 CSRF，
写请求发送 `X-CSRF-Token`，认证由服务端 cookie 维护。401/403 清除私有快照、
表单和一次性节点令牌，关闭对话框、取消进行中的请求并返回登录页。退出、页面离开或会话
失效后的迟到响应不能恢复私有数据。退出请求失败时提示服务端会话可能仍有效。

管理快照每 10 秒刷新。刷新失败保留并标注上次成功快照，不覆盖编辑中的节点设置、
FRP 绑定和探测表单。授权详情包含主机事实、地址、代理本地目标和独立 FRP 核对；
服务端注册表可以包含普通 frpc，节点归属必须通过可信绑定确认。

节点设置编辑名称、公开/私有备注、公开策略、费用、币种、付费周期、到期、续费说明
以及流量额度、类型、重置方式、日期、时区和当前已用量。费用输入按币种最小单位
精确转换为 `price_minor`；流量输入采用 GiB，以整数运算转换成字节。

- `PATCH /api/admin/v1/nodes/{id}/settings`：提交完整可编辑配置及 `config_revision`。
  校准输入留空时不发送 `traffic_used_bytes`，保留期间新增流量；填写 0 是明确校准为 0。
- `POST /api/admin/v1/nodes/{id}/reset-traffic`：提交 `config_revision`，结束当前周期并清零
  套餐用量，今日和系统累计保留。有未保存设置时要求先保存或重新读取。
- 配置版本冲突提示重新读取，不自动重试覆盖其他管理员的修改。

创建节点和轮换凭据后，节点令牌只在成功响应的独立窗口中显示一次，可复制或清除，关闭窗口、刷新和退出
都会清除；不写 localStorage、sessionStorage、URL 或日志。轮换和撤销先展示节点名称
与影响，成功后旧监控会话失效，原生 FRP 隧道仍遵循自身认证和配置。
页面可设置或解除可信绑定，不提供代理配置增删、远程命令或 shell。

探测编辑器加载完整任务清单，可编辑执行节点、稳定任务 ID、公开名称、私有目标与
间隔，保存时递增版本并整体替换。删除行后保存停止对应任务，清空所有行可清空任务。
读取失败保留编辑内容，并发冲突要求重新读取；超过浏览器安全整数范围的版本会拒绝编辑。

管理页“分组”提供独立列表和成员多选，支持创建、重命名、保存成员及删除空组或已有组。
同一节点可勾选进多个分组，删除组不删除节点。名称最多 128 个 UTF-8 字节，不能包含
控制字符或与其他组同名；最多 128 组。编辑请求携带修订号，后台更新或 409 冲突不会
覆盖未保存草稿，可明确重新读取最新状态。登出或会话失效时清空分组与成员信息。

## 嵌入与隐私边界

`Handler() http.Handler` 只提供首页、数字节点详情壳、管理登录壳和显式允许的
静态文件。`go:embed` 嵌入页面、`assets/` 和 `src/`；README、Go 源码、测试、
目录浏览、`.env`、`/admin.html` 和 `/node.html` 不直接公开，其他路径不作 SPA 回退。
`/admin/` 返回 no-store，源文件没有任何私有节点值；API/SSE 鉴权由 monitor 负责。

静态响应设置 CSP（脚本、样式、连接等只允许同源）、nosniff、拒绝 iframe 和
no-referrer，并携带内容哈希 ETag；`If-None-Match` 命中（含弱比较）时返回 304。
页面不依赖外部字体、CDN 或跨子项目运行时资源；GitHub 登录通过顶层跳转完成。

公开字段必须由服务端白名单裁剪，不能将私有数据下发后再用 CSS 隐藏。
`is_public=false` 的节点不进入公开快照或历史接口；费用默认不公开、套餐默认公开，
对应对象未获许可时不返回。私有备注、完整 Facts、主机名、IP、精确内核、探测目标、
代理本地目标、FRP raw client ID、网卡/boot ID、差分基线及凭据不进入公开数据。

## 验证

在仓库根目录运行前端逻辑和静态边界检查：

```sh
node --test frp-monitor/web/tests/*.test.mjs
GO111MODULE=off go test ./frp-monitor/web
```

JavaScript 测试覆盖数字节点 ID、uint64/大整数精度、未知与零、金额与
GiB 换算、校准留空及显式零、额度超用、快照状态、同源 CSRF、会话失效清理、
迟到响应、探测版本、重连、历史范围和缺样图表。静态 Go 测试覆盖资源 allowlist、
MIME、CSP、ETag 与 304 校验、数字详情路由、只读方法及不含私有值的 GitHub 登录壳。

本地临时 fixture 已用真实 Chrome 验证卡片与详情、设置保存、切换计费类型不额外
提交校准值、150 GiB 手工校准、带版本的套餐重置、GitHub 拒绝访问提示；历史面板
覆盖关闭、正常、降级和无样本、资源/探测切换、时间范围与刷新。后台验证节点表格与
弹窗编辑、创建/轮换后令牌展示与关闭清除、探测保存、导航刷新和会话失效时清空私有数据。
分组已通过隔离 fixture 验证创建、空组、多组成员、改名、删除保留节点、409 草稿保留、
轮询不覆盖编辑、降级恢复与退出清空；公开页验证组合筛选、视图切换、实时更新及组消失。
390px 详情和后台无整体横向溢出，检查未发现 JavaScript 页面异常；fixture 与截图不嵌入发布资源。

项目 Go 全套测试及 race 检查通过；Darwin native 双进程监控验收在历史关闭和
启用两种配置下通过，覆盖新控制库、数字节点、实时 JSON/SSE 隐私裁剪、agent 接入、
持久用量与重启恢复，启用时也验证内嵌 TSDB 样本。执行方式：

```sh
python3 frp-monitor/tests/monitor_smoke.py --agent <native-agent> --server <native-server>
python3 frp-monitor/tests/monitor_smoke.py --agent <native-agent> --server <native-server> --history
```

GitHub OAuth 流程和管理员权限由服务端模拟测试覆盖；部署环境已完成真实 GitHub
登录及节点表格、编辑窗口的只读验收。Linux 主控发布后已验证 HTTPS 静态资源、
实时 JSON/SSE、全部接入节点在线，以及保留 90 天的 TSDB 历史持续写入。
服务器地址、发布版本、备份和回滚信息只记录在被忽略的 `.env.serverlist` 中。

今日接收、今日发送、套餐已用 / 额度、套餐详情共用一行紧凑信息栏，窄屏自动换行；套餐未公开时仅隐藏套餐字段，保留今日流量。节点流量信息不展示统计口径；套餐详情合并大写类型、使用率与日期范围，例如 `MAX · 0.0% · 2026/9/1 - 2026/10/1`，未设置的日期显示 `—`。
