# frp-plus

基于 FRP 的主机与隧道监控。节点运行 `frp-plus-agent`（frpc + 采集），主控运行
`frp-plus-server`（frps + API + 网页），各一个进程。原生穿透、Dashboard 和
wire v1/v2 保留；监控使用独立 HTTPS/WSS，不经自身隧道。

系统采用 SQLite 配置与业务存储、内存实时状态、可选内嵌 VictoriaMetrics 历史和
GitHub 管理登录。现有 v4 控制库会原位升级，保留节点、凭据和当前流量。当前文档描述实现，
未验证的实机部署、真实 GitHub 登录和持续负载范围见 [测试说明](tests/README.md)。

项目原名 `frp-monitor`，目录、发布包与程序名称现统一为 `frp-plus`；两个程序为
`frp-plus-agent` 和 `frp-plus-server`。已有安装升级时需更新启动命令中的程序名；
若同时移动安装或数据目录，还需更新配置及 systemd 单元中的绝对路径。
现有 `FRP_MONITOR_*` 环境变量、`--monitor-version`、`monitor` / `telemetry` 配置段、
内部 `extension/frpmonitor` 包路径、历史指标名、Cookie、浏览器存储键及
`.frp-monitor.json` 构建缓存标记继续沿用，以兼容已有配置、历史和本地缓存。

## 线上部署状态

2026-09-30 主控已升级至提交 `680f0bc`，包含此前首页、节点详情及管理工作台改版，
并将后台节点目录对齐参考稿，按用户要求使用 IP 地址列。Linux amd64 产物由隔离源码构建，主控发布目录为
`/opt/frp-plus/releases/680f0bc-20260930`。10 个 Agent 的现有二进制均与最新构建
逐字节一致，继续使用 `/opt/frp-plus/releases/43e3294-20260930`，无需重启。

主控与 Agent 使用 `frp-plus` 用户和同名 systemd 服务，保持开机启动。运行目录仍为
`/var/lib/frp-plus-server` 与 `/var/lib/frp-plus-agent`；节点、分组、凭据、流量账本
和历史数据完整保留。本次无协议或数据库迁移，反代与来源限流沿用现有配置。
升级前主控完整冷备位于 `/root/backup/frp-plus-before-680f0bc-20260930/`（0700），
原发布包和此前下线冷备继续保留。

验收：10/10 节点在线、采样新鲜、FRP 连接与归属正常，历史存储 ready 且无丢样，
数据库完整性、业务配置摘要及凭据哈希检查通过。8 个线上页面资源已同时通过主控本机与
公网 HTTPS 的源码哈希比对，覆盖新节点目录及现有首页、详情。浏览器检查管理入口、静态表头与加载错误；
七列目录、组合筛选、编辑入口、已到期快捷筛选、退出清理和手机布局在隔离 fixture 验收，未执行真实 GitHub 账号登录。
Go 网页测试和 67 项前端测试通过；此前 Python 工具测试 76 项，其中 4 项因本机缺少 nginx
跳过。Agent 和已有独立 FRP 进程保持运行。详细清单、校验和及操作记录仅保存在 ignored 的
`.env.serverlist` 和 `data/deploy-680f0bc/`，不进入公开 Git。

## 快速开始

在本目录执行，需要 Python 3.11+、Git、Go 1.26.6+、Node.js/npm 和 OpenSSL：

```sh
python3 scripts/frp.py build --native
python3 scripts/local.py init
python3 scripts/local.py run
```

默认访问 `https://127.0.0.1:17401/`。初始化在 ignored 的 `data/local/` 生成私有控制库、
数字 ID 节点、独立 FRP/监控凭据和 30 天回环证书；agent 显式信任该证书，不修改系统
信任。浏览器信任由使用者配置。`Ctrl-C` 结束进程，保留配置和数据。

已有目录不覆盖。可用 `--directory data/demo` 新建安装，运行时指定同一目录。
初始化加 `--http` 可选择回环明文开发；加 `--probes` 可授权本机 FRP 端口的 TCP 探测。
macOS 可验证连接和页面，Linux 专属采集字段显示未知。

`.env.example` 列出初始化和构建默认值，可复制为 ignored 的 `.env`。这些工具按
默认值 → `.env` → 进程环境变量读取；生成运行配置后不会继续联动。
例如 `FRP_MONITOR_HISTORY_DATA_PATH=history` 会在运行数据目录下启用历史，留空关闭。

## 配置与管理

两端分别共用 FRP 的单一配置文件，通过 `-c` 加载，支持 TOML/YAML/JSON。
监控使用 `monitor` / `telemetry` 段，运行时环境变量通过 FRP 模板显式引用。
二进制不自动加载 `.env`，启动配置修改后重启。规则和完整示例见
[主控配置](monitor/README.md#配置加载)、[Agent 配置](agent/README.md)。

- 主控 `databaseFile` 指向私有目录中的 SQLite 控制库。
- `historyDataPath` 留空关闭历史，非空即启用内嵌 VictoriaMetrics；`retentionDays`
  默认 7 天，允许 1–365 天，无额外进程或端口。
- `/admin/` 仅通过 github.com 登录。配置 `githubClientID`、`githubClientSecretFile`、
  `githubCallbackURL`、`githubAdminUsers` 四项；允许的用户名直接写在主控配置中。
  Secret 使用外部 0600 文件。未配置 OAuth 时，公开监控和节点上报仍可运行。
- 节点、分组、套餐、公开策略、可信 FRP 绑定和探测通过管理界面即时修改，保存在 SQLite；
  不回写启动配置，不提供本地管理员密码或令牌登录。

公开首页采用紧凑节点卡片，并保留表格视图；状态、近期到期、分组、名称搜索和排序可以
组合使用。节点详情先展示当前资源与历史趋势，再查看系统、运行状态、流量套餐和公开备注。
进度条保持单色，浅色与深色主题沿用同一套控件与数据口径。

管理页默认进入侧栏工作台，汇总全部私有节点的在线、离线、等待接入、到期和 FRP 核对
情况，提供到期日历及需要关注的节点入口。节点、分组、TCP 探测、FRP 对账与接入信息
在独立页面切换；原有配置保存、版本冲突保护与一次性凭据流程保留。页面直接读取现有
API/SSE，不包含设计预览中的示例节点，也不新增历史账单、告警或远程控制能力。

## 模块与职责

| 模块 | 职责与详细说明 |
|---|---|
| [agent](agent/README.md) | Linux `/proc`、`/sys` 和文件系统采集，WSS 上报、FRP 客户端状态；[TCP 探测](agent/probe/README.md) |
| [monitor](monitor/README.md) | 节点认证、公开/管理 API、SSE、OAuth 和服务端 FRP 对账 |
| `monitor/control` | SQLite 配置、当前流量账本、基线及串行事务 |
| [monitor/store](monitor/store/README.md) | 可选指标与 TCP 探测历史，有界异步队列和窗口聚合 |
| [shared](shared/README.md) | 帧、质量标记、会话/序号、浏览器整数表示和可信绑定契约 |
| [web](web/README.md) | 嵌入式 HTML/CSS/ES Modules 页面，复用 web-standard-kit，无 CDN 或 Node 运行服务 |
| [scripts](scripts/README.md) | 固定源码构建、本地初始化；[发布与备份](packaging/README.md) |

```mermaid
flowchart LR
    A[Agent 采集] -->|独立 WSS| M[主控实时状态]
    F[原生 FRP 状态] --> M
    M --> C[(SQLite 当前配置与流量)]
    M --> T[(可选 TSDB 历史)]
    M --> W[API / SSE / 网页]
    C --> W
    T --> W
```

原生 FRP 登录失败时，agent 仍可上报；持续诊断需原生 `loginFailExit=false`。
监控断线不应阻塞隧道转发。格式错误在原生 `verify` / 配置加载阶段报错，运行故障
隔离不代表接受非法配置。监控和原生 FRP 使用独立凭据。

可信 FRP 关联键为 `server_id + user + raw_client_id`；只有管理员绑定、节点新鲜报告、
服务端观察三方唯一一致才归属，冲突不自动绑定。实时会话与指标新鲜度分别判断，
缺失或无差分基线显示未知，不能伪造为零。

## 存储与节点数据结构

SQL 的唯一维护来源是 [monitor/control/schema.sql](monitor/control/schema.sql)，
初始化工具与 Go 主控共用此文件。业务库包含 `nodes`、`settings`、`node_groups` 和
`node_group_members` 四张表。分组沿用哪吒的分组表与成员关联表方式，避免在节点中重复保存组名。

| 位置 | 保存内容 |
|---|---|
| `nodes` 基本信息 | `INTEGER PRIMARY KEY AUTOINCREMENT` 数字 ID、名称、公开/私有备注、公开策略 |
| `nodes` 费用 | 最小货币单位价格、币种、账期、到期时间、续费说明，由管理员手工维护 |
| `nodes` 套餐 | 额度、Max/total/rx/tx 类型、每月固定日期或手动重置、套餐时区 |
| `nodes` 当前账本 | 当前周期 RX/TX 与校准差额、主控今日 RX/TX、覆盖不足标记、系统计数器基线 |
| `nodes` 接入 | 当前节点令牌的 SHA256 摘要、可信 FRP 绑定、配置修订号 |
| `settings` | 单行 `probe_json` 探测配置，带递增版本 |
| `node_groups` | 自增数字 ID、唯一分组名称、配置修订号 |
| `node_group_members` | 分组 ID 与节点 ID 联合主键；外键在删除节点或分组时自动清理关联 |
| 内存 | 连接/会话、最新 Facts/Metrics、在线与新鲜度、FRP 实时状态、管理员会话 |
| 可选 TSDB | CPU、内存、磁盘、负载、网速和 TCP 探测等历史曲线 |

API 使用数字 ID 的十进制字符串，例如 `"1"`，删除不复用。时间戳统一 UTC 毫秒；
流量与浏览器大整数使用十进制字符串。价格为非负最小货币单位，流量校准差额可以为负。
不另建账单、续费、套餐版本、每日或周期历史表；只保留当前日和当前周期。

### 节点分组

一个节点可以加入多个分组。在管理页“分组”中创建、重命名、编辑成员或删除分组；
删除分组保留节点，删除节点自动移除其所有组内成员关系。允许空组，最多 128 个分组，
名称区分大小写、唯一、最多 128 个 UTF-8 字节；前后空白去除，控制字符拒绝。
修改或删除提交 `config_revision`，过期版本返回 409，不覆盖他人编辑。

公开首页提供“All”及命名分组切换，可与名称搜索和排序组合，卡片和表格使用同一结果；无分组节点可在“All”中查看。
同一节点在列表中只出现一次。选择保存在当前浏览器会话中，实时快照和重连后保持；
组不再包含公开节点时回到“全部”。分组名可在公开卡片、表格和详情中查看。
公开数据只在可见节点上附带 `{id,name}`，不公开完整成员目录、空组或仅含隐藏节点的分组。
参考：[哪吒分组说明](https://nezha.wiki/guide/group.html)、
[分组模型](https://github.com/nezhahq/nezha/blob/master/model/server_group.go)和
[成员关联模型](https://github.com/nezhahq/nezha/blob/master/model/server_group_server.go)。

### 四种流量

| 名称 | 口径 |
|---|---|
| 系统累计 | 最新有效网卡 lifetime RX/TX，可能随主机、接口变化归零 |
| 今日流量 | 系统计数器差分，按主控机器系统时区的日历零点划日，支持夏令时 |
| 套餐周期 | 同批系统差分，按套餐独立时区和重置规则累计，再加人工校准差额 |
| FRP 隧道流量 | 原生服务端观察，保留 FRP 本地日与进程重启口径；部分协议关闭连接后计入 |

四者独立统计，FRP 流量不叠加到系统或套餐用量，仅在管理页展示。首次有效采样只建立基线；boot、
接口集合变化或计数回退时重建。基线、今日和周期增量同事务更新；失败回滚，
下次从旧基线继续。跨日/周期的增量按接收时间比例分摊并标记不完整。

### 套餐校准与重置

```text
F(RX, TX) = max(RX, TX) / RX+TX / RX / TX，按所选套餐类型
本周期已用 = F(周期累计 RX, TX) + 校准差额
管理员设置已用 U：校准差额 = U - F(当前周期累计 RX, TX)
```

Max 对整周期累计收发取较大值。手工校准不改原始 RX/TX、今日或系统累计。
切换类型保留当前已用，再按新类型继续累计：例如 RX=100、TX=80，Max 校准为 150，
改为 total 后差额为 -30，已用仍是 150。修改立即应用，过期 `config_revision` 返回冲突。

每月重置按套餐时区的配置日期零点，日期不存在时取月末，下一月仍使用原配置日期。
手动重置仅清周期 RX/TX 和差额，保留今日与基线；每月模式仍在下一配置日期自动重置。
修改重置日只调整下一边界，不清空已用。未提供套餐时默认无限流量（额度 NULL，显示 ∞），
每月 1 日按套餐时区重置；0 为实际零额度。

周期中途接入、计数器重建或采集缺口保留 `partial` 标记。TSDB 曲线使用浮点采样，
不能恢复人工校准的精确账本；历史关闭或降级时，SQLite 当前记账继续运行。
重启恢复累计，不恢复在线状态；强制终止可能丢失尚未提交的尾批。

## 页面与公开边界

首页和节点页首屏静默加载，默认隐藏顶部提示条；超过 300 毫秒才在底部固定状态栏显示同步提示。
快照到达后立即渲染，连接异常时才显示顶部警告，重试期间保留警告直到实时连接恢复。
`/` 顶部节点与网络两块总览显示全部、在线、离线、等待接入及累计流量、当前双向速率。
累计量包含离线节点最后有效的系统计数，当前速率仅包含在线且新鲜的有效样本。
状态、未来 7 天到期、分组与名称可组合筛选；排序支持默认、名称、运行时间、系统、CPU、内存、
存储、上传、下载、上传总量、下载总量及到期时间，卡片和表格共用结果，汇总保持全部公开节点口径。
名称、系统和到期时间升序，其他指标降序、缺失值最后，默认保持服务端顺序；选择在浏览器会话中记忆。
工具栏提供名称搜索；顶栏放大镜或 `⌘ / Ctrl K` 打开搜索弹窗，支持组合筛选、进入详情及键盘操作。
首页卡片的每项资源显示名称、使用率及 CPU 核心数、内存容量、存储容量或周期流量限额，
下方为细单色进度条，使用率不加粗。接收/发送仅显示速率，
流量和费用明细在节点详情页查看。
首页卡片、表格及节点页显式显示“在线 / 等待 / 离线”，分组名称独立显示，多个分组以 ` · ` 分隔。
首页卡片的节点名与状态共用一行，分组位于系统摘要旁。
卡片系统摘要仅显示系统版本和架构，例如 `Debian 12 · x86_64`；去掉 `GNU/Linux`、
括号内的版本代号和虚拟化类型，保留 `LTS`。表格和节点详情保留完整系统信息。
`/node/{id}` 标题旁展示状态徽章，首屏为 CPU、内存、存储和网络当前指标，其后为历史趋势，
底部信息面板按系统配置、运行状态、流量与套餐分区。操作系统合并显示架构与虚拟化环境，内存和存储展示“已用 / 总量”，运行状态
不显示 Agent 版本；今日收发突出显示，套餐详情合并已用 / 额度、类型、使用率与周期日期。
公开费用和备注在面板底部展示。信息面板桌面三列、平板两列、手机单列。
资源趋势展示 CPU、内存、存储、网络速率与负载曲线，卡片标题显示实时值；TCP 探测独立切换。
节点标题右侧与历史范围右侧分别显示“每 2 秒刷新一次”和“30 秒刷新”，替代手动刷新按钮。
首页与节点详情不展示 FRP 连接、隧道、核对状态或隧道流量。
`/admin/` 默认显示侧栏工作台，窄屏导航改为横向滚动。工作台提供状态汇总、需关注列表与到期日历，
节点管理按参考稿使用状态标签和七列表格：节点、分组、状态、IP 地址、套餐、到期、操作。
节点名下合并 ID、系统与隐藏标识；IP 分行展示 IPv4/IPv6，套餐显示占比，到期显示剩余天数。
点击名称或“编辑”进入原有弹窗，管理凭据轮换/撤销、费用到期、套餐校准/重置、可信绑定及探测，
并提供独立的分组与成员管理。

所有页面在上述布局和信息结构上使用 `web-standard-kit` 的组件与主题，包括
动态图表卡片、探测表单、登录和编辑对话框；业务 CSS 只补充布局与监控状态。
套件源码固定在 `6d41d588b3537b2d2460d73f999d958fd37eb00d`，来源与使用方式见
[Web 文档](web/README.md)。

费用和到期默认不公开，套餐默认公开；节点可整体隐藏。公开 API/SSE 使用字段白名单，
排除私有备注、IP、主机名、精确内核、本地目标、内部计数器标识和凭据。
保留系统/浅色/深色主题、键盘导航、减弱动画以及实时更新中的焦点和滚动位置。

TCP 探测不是 ICMP 丢包检测；默认只允许公网单播，私网/回环须由 agent 额外授权。
不提供告警、主题市场、远程命令、自动升级或远程 Proxy/Visitor CRUD。

## 构建与验证

固定源码身份见 [upstream.lock](upstream.lock)。[补丁](patches/README.md) 只修改必要
上游接入点，Overlay 映射 `agent/monitor/shared/web`；源码缓存不提交。运行程序不需要
Node.js、独立 TSDB 或外部网页资源。

```sh
python3 scripts/frp.py test
python3 -m unittest discover -s tests -p '*test*.py'
node --test web/tests/*.test.mjs
python3 tests/smoke.py --agent dist/darwin-arm64/frp-plus-agent --server dist/darwin-arm64/frp-plus-server
python3 tests/monitor_smoke.py --agent dist/darwin-arm64/frp-plus-agent --server dist/darwin-arm64/frp-plus-server --history
python3 scripts/frp.py package
```

示例二进制路径适用于 Apple Silicon；其他机器使用本机目标路径。`package` 构建
Linux amd64/arm64，并附源码身份、校验和、SQLite schema、初始化/备份工具和许可证。
FRP 互通矩阵、真实采样和故障恢复方法见 [tests/README.md](tests/README.md)。

生成物与依赖放 ignored 的 `.cache/`、`data/`、`dist/`。公开 Git 仅保存脱敏示例；
真实运行配置、密钥、地址与机器采样不提交。来源和许可统一维护于
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)，原始许可随发布包保留。

公开首页的节点卡片和表格在填写并公开到期时间后显示“N 天后到期”（不足一天按一天计），到期后显示“已到期”；未填写或未公开时隐藏，悬停可查看具体到期时间。

公开首页卡片底部显示到期提示、采样新鲜度和运行时长。运行时长显示为“在线 N 天 M 小时”，不足一天仅显示“在线 M 小时”，不足一小时显示 0 小时；离线节点保留最后上报时长并标为“运行”。该时长来自系统 uptime，不表示监控连续连接时长。
