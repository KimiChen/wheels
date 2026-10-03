# 发布与恢复

`python3 scripts/frp.py package` 生成 Linux amd64/arm64 发布包，包含两个二进制、
固定上游记录、构建信息、校验和、许可原文、下列运维工具、两个控制库 schema SQL、组备份清单模板及 `.env.example`。
包不包含运行时目录、Token、证书、数据库或本地 `.env`。
二进制运行不需要 Node、Python 或 SQLite CLI；初始化和备份工具需要 Python 3.11+
及其标准库 sqlite3。运行前校验发布目录和包内的 `SHA256SUMS`。

## 服务端初始化

在解压目录复制 `.env.example` 为 `.env`，按需设置 `FRP_SERVER_PORT`（默认17000）、
`FRP_MONITOR_PORT`（默认17401）、采样周期和保留天数（1–365天）。这些只影响新生成的配置。
配置与当前流量写入 `control.sqlite`。`FRP_MONITOR_HISTORY_DATA_PATH` 默认留空，关闭指标历史；
填写 `history` 时，初始化工具将其转换为运行目录内的绝对路径。也可以指定其他运行目录内
的私有子目录；拒绝目录穿越、符号链接、运行文件冲突和外部路径。初始化不创建历史目录，
启动主控后才由内嵌 TSDB 建立，无需额外进程。

主控与 agent 各使用一个原生 FRP 配置文件，监控配置直接放在对应 `monitor` / `telemetry`
节中；运行时支持 TOML、YAML 和 JSON。工具生成 TOML，`historyDataPath` 非空即启用
指标历史，留空即关闭，没有独立的历史开关或额外监控配置文件。
准备正确匹配访问主机名的 TLS 证书/私钥，将输入文件权限设为0600。

```sh
python3 scripts/ops.py server-init \
  --directory /var/lib/frp-plus \
  --tls-cert /secure/input/tls.crt --tls-key /secure/input/tls.key \
  --server-id primary
```

目标目录必须不存在，父目录必须存在；路径不接受符号链接。工具生成0700目录和0600文件，
独立随机生成 FRP token，初始化空节点列表和探测配置到 `control.sqlite`。它默认绑定
`0.0.0.0`，可用 `--bind` 改为其他IP。FRP端口和监控TLS端口各自配置防火墙；
原生 Dashboard 默认关闭，监控不依赖其密码。

管理员只通过 github.com 的 OAuth 登录。初始化前在 `.env` 设置以下四项：

- `FRP_GITHUB_CLIENT_ID`：GitHub OAuth App 的 Client ID。
- `FRP_GITHUB_CLIENT_SECRET_FILE`：保存 Client Secret 的0600文件路径；去除首尾空白后须为8–256字节，不能含空格、制表符、换行或 NUL。工具复制为运行目录 `github.secret`。
- `FRP_GITHUB_CALLBACK_URL`：公开 HTTPS 地址，路径必须为 `/api/admin/v1/auth/github/callback`，与 GitHub App 设置完全一致。
- `FRP_GITHUB_ADMIN_USERS`：允许管理的 GitHub 登录名，用英文逗号分隔。

四项一起填写；全部留空时可以运行公开监控，管理页面显示登录尚未配置。没有管理员令牌
登录或备用密码。白名单写在主控 `server.toml` 的 `monitor.githubAdminUsers`，修改后重启主控。
不要把 Client Secret 内容放入 `.env`、命令行或公开仓库。管理员在 `/admin/` 登录后创建节点、
维护费用及套餐、设置可信 `server_id + user + raw_client_id` 绑定，安全保存只显示一次的 agent token。
节点 ID 为自增数字，详情地址为 `/node/1`。节点令牌与 GitHub 登录凭据独立。

## nginx 反代与来源限流

使用包内 [nginx.conf.example](nginx.conf.example) 时，将其包含在 nginx 的 `http {}` 中，
替换域名、证书路径及上游端口。模板以 HTTPS 443 对外提供网页、SSE 与 WSS，监控上游
为 `127.0.0.1:17401`。主控 `monitor.bindAddr` 应为 `127.0.0.1`，使用回环 HTTP 时清空
`monitor.certFile/keyFile`；FRP 的监听和 TLS 配置保持独立。代理保留外部 Host（含端口），
GitHub callback 使用公开 HTTPS 地址，节点使用 `wss://monitor.example.invalid/agent/v1/ws`。

模板以连接来源 `$binary_remote_addr` 计数，返回 429 表示触发限制：

| 入口 | 来源限额 |
|---|---|
| OAuth 发起 | 每 IP 每分钟 6 次，允许 1 次突发；回调不占此额度 |
| 公开 SSE | 每 IP 4 路、全站 96 路；新连接每 IP 每秒 1 次，允许 4 次突发 |
| 管理 SSE | 每 IP 8 路，独立于公开 SSE 配额 |
| 公开 API | 每 IP 每秒 10 次，允许 20 次突发 |

程序仍保留 OAuth 全局请求容量和公开/管理 SSE 的 128/64 路上限；公开 SSE 每两秒推送。
这些限制控制请求与连接数，不是固定的每秒字节带宽保证。共享 NAT 的访客共用来源额度，
部署者可按实际访问量调整。nginx 之前另有 CDN/代理时，只通过 `set_real_ip_from` 信任其
明确的地址段，再配置 `real_ip_header`；禁止信任所有地址或直接按客户端传来的 XFF 限流。
后端仅回环监听，避免绕过 nginx。详见 nginx 官方的
[请求限流](https://nginx.org/en/docs/http/ngx_http_limit_req_module.html) 和
[连接限额](https://nginx.org/en/docs/http/ngx_http_limit_conn_module.html)说明。

合并到现有站点后执行 `nginx -t`，成功后再 reload。源码测试
`python3 -m unittest discover -s tests -p nginx_test.py -v` 使用隔离 nginx 和临时证书，
验证来源限额、回调、伪造 XFF 和 WSS 升级；不会修改正在运行的 nginx。

## Agent 接入

安全传递独立的agent token和FRP token文件到目标机器，权限0600。不要把token放入URL、
命令行参数、环境变量或公开仓库。将管理员预绑定的 `raw_client_id` 作为 `--client-id`，
`--server-id` / `--user` 必须和预绑定及FRP配置一致。`--server-id`、`--client-id`、
`--user`、`--server-addr`、`--monitor-url` 等字面量参数不允许空格等空白字符。

```sh
python3 scripts/ops.py agent-init \
  --directory /var/lib/frp-plus \
  --server-addr frp.example.invalid \
  --monitor-url wss://monitor.example.invalid:17401/agent/v1/ws \
  --server-id primary --client-id node-stable-id \
  --agent-token /secure/input/agent.token --frp-token /secure/input/frp.token
```

上例域名是占位符。端口默认来自解压目录的 `.env`。私有CA增加 `--ca /secure/input/ca.crt`；
不指定则使用系统CA。工具不跳过证书验证。TCP探测通过 `--probes` 启用，确需探测私网时再
增加 `--allow-private-probes`；链路本地/元数据等保留地址仍拒绝。代理和visitor配置按原生FRP
语法追加到 `agent.toml`。接入配置使用文件引用，不在TOML写明文token。

## systemd

使用专用无登录用户 `frp-plus`，将发布包解压到root持有且运行用户不可写的
`/opt/frp-plus`，私有运行目录归运行用户持有（0700，文件0600）。两角色一般位于不同机器；
同机部署时用不同运行目录。端口默认均大于1024，模板不授予低端口绑定特权。

```sh
python3 /opt/frp-plus/scripts/ops.py systemd --role server \
  --directory /var/lib/frp-plus --binary-directory /opt/frp-plus \
  > /etc/systemd/system/frp-plus-server.service
systemctl daemon-reload
systemctl enable --now frp-plus-server.service
systemctl status frp-plus-server.service
```

Agent 将两处 `server` 改为 `agent`。生成器只输出unit，安装/启用由操作者执行。
启动前运行原生 `verify`，异常退出5秒后重试、限制一分钟五次；TERM最多等待10秒，给当前计数器和历史
尾批持久化留出时间。服务端限制系统目录写入；agent保留宿主挂载/网络视图，避免沙箱改变
采集口径，仍以无特权用户运行。采集权限缺失会呈现质量状态。服务端业务配置、流量累计和可选历史只写运行目录。替换二进制前停服务，保留旧包和私有备份；不提供自动升级。

发布前分别验证目标架构的构建、配置与采样。源码 `tests/monitor_smoke.py` 验证控制库、
数字节点 ID、公开接口和 agent 接入；GitHub OAuth 与管理权限由服务端测试覆盖。
测试机地址与凭据不写入公开文档。

## 一致备份与恢复

完整流程与恢复门禁见[根文档的备份章节](../README.md#托管-agent-的离线备份与恢复接管)，
11角色记录使用[私有组清单模板](backup-set.example.json)。发布包包含 `ops.py`、
`ops_checkpoint.py`、`ops_history.py`、`local.py` 及基础/审计保留 schema SQL；初始化与主控
一致使用 schema12，备份支持控制库4–12，未知版本拒绝。

- format2：旧生成安装的配置与SQLite；无Store的旧Agent可由新版Python直接备份，无需新helper。
- format3：原单Agent托管检查点v1，保留原读取路径。
- format4：`--dependency-graph` 或托管Agent的 `--complete` 包装检查点v2；主控的
  `--complete --offline` 同组保存SQLite与完整TSDB。普通format2不会自动包含历史目录。

先停止主控，再停止所有Agent，保留未完成事务；在11角色归档和旧程序/服务单元哈希全部
核验后直接替换。以下为占位路径，不会自动停止服务或连接远程机器：

```sh
python3 scripts/ops.py backup --directory <原安装绝对目录> \
  --output <新建私有归档路径> --complete --offline
# 托管Agent额外指定已支持检查点v2的可信helper：
python3 scripts/ops.py backup --directory <Agent原安装绝对目录> \
  --output <新建私有归档路径> --dependency-graph --offline \
  --agent-binary <可信Agent绝对路径>
```

主控TSDB保持停机并取得原 `flock.lock`，从私有DB/WAL副本生成数据库快照，检查全部来源
的路径、文件类型、摘要与修改时间，归档是最后持久化状态。主控数据上限2GiB、单文件1GiB、
文件及目录8192项；超限失败。归档含凭据和私钥，文件0600、父目录0700，仅在私有位置保存。
编号USTAR避免源文件名进入tar扩展头，拒绝链接、穿越、重复成员、压缩炸弹和配置引用遗漏。
内部摘要不是来源签名；归档SHA256须与可信私有组清单核对，不能用自改的清单证明材料完整。

主控恢复前先停服务并将原目录整体移到私有保留目录，原路径须空缺；验证staging后原子
发布回相同绝对目录。Agent托管恢复保留Root/.lock，更新身份并要求本地运行验证、Admin
接管和二次离线确认。`restore` 自动按格式分派；旧format2仍可恢复到新目录，当前部署统一
用原路径。原生 `verify` 和真实监控/业务/历史验收通过后再保留或清理旧目录。

完整主控备份以运行目录所有者执行；当前旧非托管安装恢复到 root 持有的0755父目录时，
由管理员执行并在启动前把目标所有权交回 `frp-plus`，staging/目标仍为0700。
托管Agent的helper另要求管理根由当前用户持有，且封装提取需要父目录写权限；root恢复
旧非托管数据的做法不能直接用于托管根，不要为此改写 `/var/lib` 或管理根权限。

失败时停止所有角色，用同组旧二进制和数据恢复；不得让旧frps打开新schema数据库。
程序/BUILD和unit另随组清单保存，单角色数据归档不包含程序。模板、外部映射、跨目录的
完整依赖恢复不开放，缺任何角色材料不得将组状态标为complete。这里不提供自动跨机器编排。
