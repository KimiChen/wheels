# 发布与恢复

`python3 scripts/frp.py package` 生成 Linux amd64/arm64 发布包，包含两个二进制、
固定上游记录、构建信息、校验和、许可原文、下列运维工具、`monitor/control/schema.sql` 及 `.env.example`。
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
  --directory /var/lib/frp-monitor \
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

## Agent 接入

安全传递独立的agent token和FRP token文件到目标机器，权限0600。不要把token放入URL、
命令行参数、环境变量或公开仓库。将管理员预绑定的 `raw_client_id` 作为 `--client-id`，
`--server-id` / `--user` 必须和预绑定及FRP配置一致。

```sh
python3 scripts/ops.py agent-init \
  --directory /var/lib/frp-monitor \
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

使用专用无登录用户 `frp-monitor`，将发布包解压到root持有且运行用户不可写的
`/opt/frp-monitor`，私有运行目录归运行用户持有（0700，文件0600）。两角色一般位于不同机器；
同机部署时用不同运行目录。端口默认均大于1024，模板不授予低端口绑定特权。

```sh
python3 /opt/frp-monitor/scripts/ops.py systemd --role server \
  --directory /var/lib/frp-monitor --binary-directory /opt/frp-monitor \
  > /etc/systemd/system/frp-monitor-server.service
systemctl daemon-reload
systemctl enable --now frp-monitor-server.service
systemctl status frp-monitor-server.service
```

Agent 将两处 `server` 改为 `agent`。生成器只输出unit，安装/启用由操作者执行。
启动前运行原生 `verify`，异常退出5秒后重试、限制一分钟五次；TERM最多等待10秒，给当前计数器和历史
尾批持久化留出时间。服务端限制系统目录写入；agent保留宿主挂载/网络视图，避免沙箱改变
采集口径，仍以无特权用户运行。采集权限缺失会呈现质量状态。服务端业务配置、流量累计和可选历史只写运行目录。替换二进制前停服务，保留旧包和私有备份；不提供自动升级。

发布前分别验证目标架构的构建、配置与采样。源码 `tests/monitor_smoke.py` 验证控制库、
数字节点 ID、公开接口和 agent 接入；GitHub OAuth 与管理权限由服务端测试覆盖。
测试机地址与凭据不写入公开文档。

## 一致备份与恢复

工具只支持 `local.py` / `ops.py` 创建的 format 2 安装；开发期不兼容旧配置和备份。运行文件必须在同一私有目录，不支持外部includes。
先创建0700备份目录。输出是包含凭据和私钥的0600归档，应和运行目录一样限制访问。

```sh
python3 scripts/ops.py backup --directory /var/lib/frp-monitor \
  --output /secure/backups/frp-monitor-20260927.tar.gz
python3 scripts/ops.py restore --archive /secure/backups/frp-monitor-20260927.tar.gz \
  --directory /var/lib/frp-monitor-restored
```

备份使用SQLite在线backup API取得 `control.sqlite` 包含已提交WAL的数据库快照，并执行 `integrity_check`，
不直接拷贝活跃的 `.sqlite/-wal/-shm`。只收集白名单运行文件，不含日志、依赖或程序。
每个文件保存SHA-256用于检测损坏；校验和不是签名，应只恢复可信来源的私有备份。
默认备份时限30秒、数据库1GiB/总解包2GiB上限，超限明确失败，不生成半成品。
节点、套餐累计、计数器基线与探测配置在同一个数据库快照中。启动用TOML和密钥文件逐个读取，
备份期间不要替换这些文件；需要完整安装同一时点快照时先停服务。

可选 TSDB 的 `historyDataPath` 目录不进入此备份，恢复后可以重新积累曲线。若需要保留
指标历史，停止主控后单独复制该目录；不要在线拼接复制内部文件。恢复会同步调整配置中
的历史目录路径，空路径继续保持关闭。

恢复只写新目录，拒绝覆盖、归档链接、目录穿越、重复文件及校验失败；验证数据库后，
把生成配置中旧安装目录的文件路径替换为新目录。证书身份、网络端点和FRP绑定不会改变。
恢复后先以相同版本二进制 `verify -c`，再修改unit的运行目录并启用；保留旧目录直到验收。
当前费用、套餐用量、今日累计与差分基线会恢复，在线会话不会恢复，页面等待 agent 重新上报。
历史曲线不会从控制库恢复。恢复旧备份会恢复旧节点 token 散列，正式切换后按需轮换节点令牌；
这不会改变原生 FRP 认证。GitHub 管理员白名单及 Client Secret 随启动配置恢复，确认其仍有效后启用。

GitHub 账号权限由主控白名单控制。需要撤销管理员时编辑白名单并重启主控；更换 OAuth App
密钥时更新0600 `github.secret` 后重启。节点在管理页轮换或撤销会断开旧监控会话，
原生 FRP 隧道继续按自身凭据运行。
