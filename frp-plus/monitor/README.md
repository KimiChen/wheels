# Monitor

主控提供独立 Listener、节点认证、内存实时状态、公开/管理 JSON 与 SSE、当前流量统计、
可选指标历史和 FRP 服务端对账。测试与实机验收方法见 [测试说明](../tests/README.md)。
构建时映射到固定 FRP 源码的 `extension/frpmonitor/monitor`，不依赖上游 `server` 根包。
入口为 `Start(ctx, shared.MonitorConfig, ...shared.ServerProvider)`、`Service.Address()`、
`Service.Close()`。监控与原生 FRP 转发的监听、认证和生命周期相互隔离。

## 启动配置

原生 frps 的 `[monitor]` 默认关闭。启用后默认监听 `127.0.0.1:7401`，需要指定
`databaseFile`。非 loopback 监听必须配置 `certFile` / `keyFile`；可以使用内置 TLS，
或在回环 Listener 前放置 HTTPS/WSS 反向代理。不要复用 Dashboard 或 FRP 控制端口。

```toml
[monitor]
enabled = true
bindAddr = "127.0.0.1"
bindPort = 7401
databaseFile = "/private/frp-plus/control.sqlite"
reportIntervalSeconds = 1
historyDataPath = "" # 留空关闭；填写私有目录即启用
retentionDays = 7

# 以下四项一起配置后开放 GitHub 管理登录。
githubClientID = "OAUTH_APP_CLIENT_ID"
githubClientSecretFile = "/private/frp-plus/github.secret"
githubCallbackURL = "https://monitor.example.invalid/api/admin/v1/auth/github/callback"
githubAdminUsers = ["example-admin"]
```

以上地址和身份仅为占位示例。OAuth callback 必须是实际管理入口的 HTTPS URL，
本机开发可显式使用字面量 loopback 的 HTTP 地址；路径固定为
`/api/admin/v1/auth/github/callback`。代理应保留外部 Host。安全 Cookie 根据 TLS
连接或配置的 HTTPS callback 设置，不信任客户端提供的转发头。
启动配置改变后重启，密钥必须是外部 0600 常规文件，不写入 SQLite、页面或公开 API。

## 配置加载

借鉴哪吒的单文件配置方式，复用 FRP 原生加载器，不增加监控专用配置文件或加载库。
服务端与客户端各使用自己的单一配置文件；分别将 `monitor` / `telemetry` 放入原生
FRP 配置中。TOML、YAML、JSON 使用相同的 camelCase 字段。

```sh
frp-plus-server verify -c /private/frp-plus/server.yaml
frp-plus-server -c /private/frp-plus/server.yaml
frp-plus-agent -c /private/frp-plus/agent.toml
```

启动顺序为：读取 `-c` 指定文件 → 展开 FRP 的 `{{ .Envs.NAME }}` 环境变量模板 →
按格式解析 → 补默认值 → 校验配置 → 初始化 SQLite、管理登录与可选 TSDB。
保留原生严格模式，未知字段报错。运行二进制不自动读取 `.env`、不扫描其他目录，
不合并第二份监控配置，也不自动回写文件。

环境变量只影响显式引用它的字段，不自动覆盖同名配置。例如以下 YAML 片段：

```yaml
monitor:
  enabled: true
  databaseFile: /private/frp-plus/control.sqlite
  historyDataPath: {{ printf "%q" (or .Envs.FRP_MONITOR_HISTORY_DATA_PATH "") }}
  retentionDays: 7
  githubClientID: OAUTH_APP_CLIENT_ID
  githubClientSecretFile: /private/frp-plus/github.secret
  githubCallbackURL: https://monitor.example.invalid/api/admin/v1/auth/github/callback
  githubAdminUsers: [example-admin]
```

未提供环境变量时，例中的历史目录为空；设置变量后启用。管理员名单仍直接写在主控
配置里。密钥用外部私有文件，配置模板只引用路径。相对运行路径按进程工作目录解析，
部署建议使用绝对路径或固定 systemd `WorkingDirectory`。今日统计取主控系统时区，
也可由启动进程的标准 `TZ` 环境变量确定；不增加第二套今日时区配置。

`.env` 只供 `local.py` / `ops.py` 初始化和构建工具使用；这些工具读取顺序是默认值 →
子项目 `.env` → 进程环境变量，生成 TOML 后不会继续联动。已有安装修改实际运行配置并
重启。节点、套餐与探测属于 SQLite 业务数据，通过管理界面即时修改，不回写启动配置。

参考：[哪吒配置加载源码](https://github.com/nezhahq/nezha/blob/master/model/config.go)、
[哪吒 TSDB 目录开关](https://nezha.wiki/guide/q15.html)。哪吒的 `NZ_` 环境变量映射和配置
回写属于其自身实现，本项目沿用 FRP 的显式模板机制，避免出现两套覆盖规则。

## SQLite 控制库与节点凭据

`control` 包保存四张业务表：

| 表 | 内容 |
|---|---|
| `nodes` | 自增数字 ID、节点名称/公开策略、费用到期、套餐、当前周期和今日流量、计数器基线、当前令牌摘要及可信 FRP 绑定 |
| `settings` | `id=1` 的单行，`probe_json` 保存带版本的探测文档 |
| `node_groups` | 自增 ID、唯一分组名和配置修订号 |
| `node_group_members` | 分组/节点多对多关系，联合主键去重，外键级联清理关联 |

精确结构见 [control/schema.sql](control/schema.sql) 和[项目数据结构](../README.md#存储与节点数据结构)。
新库直接创建 v5 结构；本项目 v4 库在一个事务内补充分组表，保留原有配置与账本，
其他身份或版本仍拒绝。`nodes.id` 使用 `INTEGER PRIMARY KEY AUTOINCREMENT`，
删除后不复用；API 使用其十进制字符串，例如 `"1"`。最多 1,024 个节点。

数据库直接父目录必须是 0700，数据库及 WAL/SHM 必须为私有常规文件，路径拒绝符号链接。
使用 WAL 和 FULL 同步；配置更新与当前流量更新由同一串行 worker 处理。
`config_revision` 只随管理员修改递增，采样不修改版本。
控制库是认证和记账的权威来源，失败必须报告不可用，不能作为可选历史后端处理。

节点令牌由至少 32 个随机字节进行无填充 base64url 编码；`token_sha256` 只保存该
编码文本的 SHA256 小写摘要。创建和轮换响应仅返回一次明文，读取接口不返回令牌或摘要。
不复用 FRP/OIDC/Dashboard 凭据。轮换关闭旧监控连接，包括尚未完成 hello 的连接；
删除节点同时清理其探测配置，公开列表和历史入口立即不再提供该节点。
原生 FRP 连接与凭据不随监控凭据操作改变。

客户端连接 `/agent/v1/ws`，通过 `Authorization: Bearer <token>` 握手，禁止浏览器 Origin。
节点归属由数据库凭据确定，正文不允许注册或指定节点。先完成 `hello`，再发送 `report`；
能力为 `metrics.v1`、`frp.v1` 和 agent 显式开启的 `ping.v1`。

## 当前流量与校准

系统累计、主控时区今日、套餐周期、FRP 隧道流量分别展示，不相加。
SQLite 只保留当前日和当前套餐周期；跨边界覆盖旧值，不保留历史账本。

- 首个有效样本只建立基线。boot、接口变化或累计回退时重建基线并标记不完整。
- 同一事务保存基线、今日 RX/TX 和周期 RX/TX；失败整体回滚，下一样本从旧基线继续。
- 今日按主控 `time.Local` 的日历零点划分，支持夏令时，不固定按 86400 秒划日。
- 套餐按独立时区每月固定日期重置，日期不存在时取月末；也支持仅手动重置。
  重置清空周期 RX/TX 和校准差额，保留今日与系统基线。
- Max 使用整周期累计 RX/TX 的较大值；total 为两者之和，rx/tx 为对应方向。
  已用量等于原始计费用量加校准差额。管理员输入非负已用值时只重算差额。
- 套餐修改立即生效；切换计费类型时重算差额以保持已用值，再按新类型继续累计。
- 跨边界增量按接收时间比例分摊。中途接入、缺口、基线重建或跨界估算产生的
  `partial` 保留在当前日或套餐周期内，后续正常采样和已用量校准不会补齐此前缺失。
  新统计范围从零开始，边界上有精确计数器基线时可标为完整；今日与套餐周期分别判定。

队列有界，批量写入不阻塞 FRP 转发线程。`Healthy()` 在队列满或写事务失败时返回 false，
下一次成功的计数器批次可以从持久基线恢复。重启恢复累计，不恢复在线状态。
流量使用精确整数和十进制字符串，不能把未知数据当成零或服务商账单。

## 可选历史与探测

`historyDataPath` 默认空，历史关闭；填写目录后在主控进程内运行
VictoriaMetrics `lib/storage`，不启动额外服务或端口。主控配置 `retentionDays` 默认 7，
允许 1–365；历史目录独立于控制库。历史不可用时实时上报与当前流量记账继续工作。
历史状态区分 `disabled`、`ready`、`degraded`，无样本保持空值，不补零。
TSDB 以浮点数保存曲线指标，不能替代 SQLite 的精确套餐账本。实现细节见
[store/README.md](store/README.md)。

探测完整文档通过管理接口读写，保存到 `settings.probe_json`：

```json
{
  "version": 1,
  "nodes": [{
    "agent_id": "1",
    "tasks": [{"id": "public-service", "name": "服务连通性", "target": "probe.example.invalid:443", "interval": 5}]
  }]
}
```

示例目标不可直接运行。只允许已知节点；每节点最多 64 项，间隔 5–3600 秒。
修改时递增版本并提交完整列表，空列表清空。旧版本在途结果丢弃，未知任务、未来版本和
重复序号拒绝。目标变化使用新的内部任务摘要，避免串接不同目标的历史。
agent 仍会校验解析后的 IP；主控任务配置不能绕过 agent 的目标授权策略。

## 实时与历史 API

`GET /api/public/v1/nodes` 返回 `{nodes, generated_at}`；`GET /events/public` 立即及每两秒
发送同结构 `event: snapshot`。两者复用发布点编码好的同一字节串，不逐请求重新编码。
API/SSE 使用 `Cache-Control: no-store`。公开 SSE 最多 128 并发，管理 SSE 独立最多 64 并发，
互不挤占；管理 SSE 每秒重验会话。管理快照每秒统一投影和编码，GET/SSE 共享缓存；
配置修改使旧缓存立即失效，JSON 编码及网络读写不占用配置锁。后台配置刷新在锁外读取
数据库，提交期间发生配置修改时丢弃旧读结果，避免恢复已撤销的凭据或旧探测任务。
隐藏节点不进入公开列表或公开历史接口。

公网部署的来源限额由 nginx 执行，见 [反代模板](../packaging/nginx.conf.example) 与
[部署说明](../packaging/README.md#nginx-反代与来源限流)。程序不信任转发头，不增加另一套
代理配置；保留全局 OAuth 发起容量和 SSE 总并发限制。模板未安装时，程序总量限制不能
防止单一来源占满名额；多来源请求仍受全站容量上限约束。

公开节点包括会话/新鲜度、硬件摘要、实时指标、`public_note`、`traffic_today` 和按公开
策略裁剪的 `billing` / `traffic_plan`。费用默认不公开，套餐默认公开。`traffic_today`
包含 `day/timezone/rx_bytes/tx_bytes/partial`；`traffic_plan` 包含额度、类型、重置设置、
周期起止、RX/TX、已用与不完整标记。系统累计仍在实时 metrics 中，FRP 流量保留自己的口径。
`hardware` 只含 `os/arch/virt/cpu_name/cpu_cores/agent_version`，保留质量标记。
`groups` 为当前节点所属分组的 `{id,name}` 数组，未分组返回 `[]`。不提供公开的全库分组目录，
隐藏节点专属组及空组不会进入公开快照。组内成员变化不改变节点接入凭据或 FRP 连接。

公开投影不含私有备注、主机名、IP、精确内核、boot/interface 标识、FRP 身份/目标和凭据。
实时 uint64 与费用最小单位、流量字节值使用十进制字符串，缺样为 null。

`GET /api/public/v1/nodes/{id}/history?window=1h` 的窗口支持 `1h/6h/24h/7d`，
对应 1/1/5/30 分钟步长，最多 500 点。返回 storage 状态、样本数、估计覆盖秒数、字段均值，
以及探测公开标签、延迟、失败率和最新结果；不再从历史接口提供今日流量。
历史查询最多 8 个并发、请求预算 2 秒，匿名访问仍受节点公开策略约束。

## GitHub 管理登录与 API

只接受 github.com OAuth；不提供管理员密码或令牌登录，也没有本地管理员表。
未配置 OAuth 时 `GET /api/admin/v1/auth` 返回 `{provider:"github",enabled:false}`，
管理读写和 SSE 不开放。配置四项 OAuth 参数后，使用 state、短期 cookie 和 PKCE
完成授权码交换；只有 `githubAdminUsers` 中的个人账号可创建管理会话。
未完成授权最多保留 64 条、单一连接来源 IP 最多 8 条；反代后的访客会共用代理地址。
同一浏览器重试先替换自己的旧请求；来源名额满时淘汰该来源最旧请求，否则全局满时
淘汰全局最旧请求，允许新登录继续发起。被淘汰的授权需要重新发起，登录发起频率仍单独限流。
GitHub access token 只用于本次身份查询，不持久化，也不发送给浏览器。

管理会话保存在内存，固定 8 小时过期，最多 64 个；重启需重新登录。
会话 Cookie 为 `frp_monitor_admin`，Path=/、HttpOnly、SameSite=Strict；OAuth 临时
Cookie 为 SameSite=Lax，便于回调。写请求检查同源和 `X-CSRF-Token`，JSON 最大 1 MiB，
拒绝重复、未知、大小写别名字段和非法 null。所有管理响应禁止缓存，不开放 CORS。
同源检查将 HTTPS 443、HTTP 80 与省略默认端口视为相同来源，仍校验协议、Host 和实际端口。

| 方法与路径 | 请求 / 结果 |
|---|---|
| `GET /api/admin/v1/auth` | 查询 GitHub 登录是否可用 |
| `GET /api/admin/v1/auth/github` | 跳转 github.com 授权 |
| `GET /api/admin/v1/auth/github/callback` | 校验回调并建立会话 |
| `GET /api/admin/v1/session` | 当前登录名、CSRF 与过期时间 |
| `POST /api/admin/v1/logout` | 注销当前会话 |
| `GET /api/admin/v1/nodes`、`GET /events/admin` | 节点配置/实时状态、存储状态与私有 FRP 对账 |
| `GET /api/admin/v1/groups` | `{groups:[{id,name,node_ids,config_revision}]}`，包括空组和隐藏节点成员 |
| `POST /api/admin/v1/groups` | `{name,node_ids}`，创建并返回分组 |
| `PATCH /api/admin/v1/groups/{id}` | `{name,node_ids,config_revision}`，完整替换名称与成员，返回更新分组 |
| `DELETE /api/admin/v1/groups/{id}` | `{config_revision}`，只删除分组及其关联 |
| `POST /api/admin/v1/nodes` | `{name,frp_binding?}`，返回数字 ID 字符串及一次性节点 token |
| `PATCH /api/admin/v1/nodes/{id}/settings` | 完整可编辑配置、`config_revision`、可选 `traffic_used_bytes` |
| `POST /api/admin/v1/nodes/{id}/reset-traffic` | `{config_revision}`，重置当前套餐周期 |
| `POST /api/admin/v1/nodes/{id}/rotate` | 返回新的一次性节点 token |
| `DELETE /api/admin/v1/nodes/{id}` | 删除节点及其探测配置 |
| `PUT /api/admin/v1/nodes/{id}/binding` | `{server_id,user,raw_client_id}` |
| `DELETE /api/admin/v1/nodes/{id}/binding` | 解除可信绑定 |
| `GET /api/admin/v1/probes` | 当前完整探测文档 |
| `PUT /api/admin/v1/probes` | 提交版本递增的完整探测文档 |

配置和计费修改写入 SQLite，不改外部 JSON 文件。修订冲突返回 409，未知节点 404，
未登录 401，同源/CSRF 拒绝 403，非法输入 400，存储不可用 503。
管理 DTO 包含 `settings`、费用/流量和私有 Facts/FRP；公开 API 始终使用独立白名单。
管理快照还包含 `groups` 和 `groups_state`。组名去除首尾空白后唯一、非空、最多 128 个
UTF-8 字节且不能包含控制字符；最多 128 组，每组最多 1024 个不同的已存在节点，允许空组。
删除节点自动递增受影响分组的修订号并移除成员；旧版本编辑会被拒绝。后台刷新使用不可变
分组缓存，并核对节点、探测和分组三份快照，避免旧读结果恢复已删除或已修改的分组。

## FRP 对账与信任边界

关联键为 `(server_id, user, raw_client_id)`；上报中的关联只是声明。
只有操作员预设的 `frp_binding`、新鲜且在线的监控报告，以及独立服务端注册表中的
唯一稳定 client 三者一致，才产生 `matched` 与 `server_online`。不会从代理名、IP、
主机名、run ID 或代理上报自动绑定。无稳定 raw client ID 的监控声明保持 transient；
没有绑定时保持 unbound，即使文本看起来完全相同也不归属。普通原生 frpc 仍在
私有服务端客户端列表展示，未匹配监控节点时 `agent_id` 为 null。

同一可信绑定配置到多个节点、服务端键重复为 conflict；声明与自己的绑定不一致为
mismatch。无绑定的重复声明可使其自身显示 conflict；不能使合法预绑定 owner 变成 conflict，
也不能夺取其他节点的服务端归属。断线/报告过期为 stale，
服务端未登记为 missing，适配器不可用为 unavailable。状态细节见管理快照的
`frp.nodes`；公开快照只给状态、可空在线值与注册数。

服务端适配器每秒由独立 sampler 读取，使用有界快照与 TryRLock，不在 HTTP 请求中
访问原生注册表；异常单独降级，不阻塞主机指标采集或 FRP 转发。快照超过 5 秒未更新、
时间缺失或超前超过 5 秒，消费方清空服务端明细并按 unavailable 显示；异常慢 provider
不会产生不断增长的超时 goroutine。客户端原始代理名
按 FRP 规则只添加一次 `user.` 前缀，再与服务端名称、类型、user、稳定 client ID
核对；服务端独有代理保留展示，不捏造客户端配置。

FRP 隧道 RX/TX 是服务器侧转发量；主机 RX/TX、探测结果与该值的采集范围不同，
不相加，不作为计费依据。客户端与服务端可在重连/配置变更间短暂不一致，界面保留
两侧状态，不能以运行隧道数推断监控会话在线。

## 验证

当前验收命令见 [tests/README.md](../tests/README.md)。核心控制库测试覆盖自增 ID、
凭据/绑定唯一性、精确大整数、主控时区与 DST、月末/手动重置、校准/计费类型切换、
基线重建、事务失败恢复和重启；整体验收还需结合 OAuth、Web、历史后端、真实二进制
与原生 FRP 回归，不能将单个包测试通过视为部署完成。
