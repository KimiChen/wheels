# 发布与恢复

`python3 scripts/frp.py package` 生成 Linux amd64/arm64 发布包，包含两个二进制、
固定上游记录、构建信息、校验和、许可原文、下列运维工具及 `.env.example`。
包不包含运行时目录、Token、证书、数据库或本地 `.env`。
二进制运行不需要 Node、Python 或 SQLite CLI；初始化和备份工具需要 Python 3.11+
及其标准库 sqlite3。运行前校验发布目录和包内的 `SHA256SUMS`。

## 服务端初始化

在解压目录复制 `.env.example` 为 `.env`，按需设置 `FRP_SERVER_PORT`（默认17000）、
`FRP_MONITOR_PORT`（默认17401）、采样周期和保留天数。这些只影响新生成的配置。
准备正确匹配访问主机名的 TLS 证书/私钥，将输入文件权限设为0600。

```sh
python3 scripts/ops.py server-init \
  --directory /var/lib/frp-monitor \
  --tls-cert /secure/input/tls.crt --tls-key /secure/input/tls.key \
  --server-id primary
```

目标目录必须不存在，父目录必须存在；路径不接受符号链接。工具生成0700目录和0600文件，
独立随机生成 FRP token、管理员登录 token（`admin.token`），服务器只读取管理员散列
`admin.json`。初始节点/任务列表为空。它默认绑定 `0.0.0.0`，可用 `--bind` 改为其他IP。
FRP端口和监控TLS端口各自配置防火墙；原生 Dashboard 默认关闭，监控不依赖其密码。
管理员在 `https://<monitor-host>:<monitor-port>/admin/` 登录，创建节点、指定公开名称及
可信 `server_id + user + raw_client_id` 绑定，安全保存只显示一次的agent token。
妥善保存管理员凭据后可以移走运行目录内的 `admin.token`，登录不依赖该明文文件。

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
启动前运行原生 `verify`，异常退出5秒后重试、限制一分钟五次；TERM最多等待10秒，给历史
尾批持久化留出时间。服务端限制系统目录写入；agent保留宿主挂载/网络视图，避免沙箱改变
采集口径，仍以无特权用户运行。采集权限缺失会呈现质量状态。服务端历史、凭据热更新和
探测配置只写运行目录。替换二进制前停服务，保留旧包和私有备份；不提供自动升级。

双架构包已交叉构建；Linux amd64 测试机已完成 systemd 启动、TERM、server/agent
SIGKILL自动恢复和在线备份恢复切换，并通过实机采样对照。arm64 实机与长期运行仍需补验。
完整范围见项目源码中的 `tests/DEPLOYMENT.md`；测试机地址与凭据不写入公开文档。

## 一致备份与恢复

工具支持 `local.py` / `ops.py` 创建的安装；运行文件必须在同一私有目录，不支持外部includes。
先创建0700备份目录。输出是包含凭据和私钥的0600归档，应和运行目录一样限制访问。

```sh
python3 scripts/ops.py backup --directory /var/lib/frp-monitor \
  --output /secure/backups/frp-monitor-20260927.tar.gz
python3 scripts/ops.py restore --archive /secure/backups/frp-monitor-20260927.tar.gz \
  --directory /var/lib/frp-monitor-restored
```

备份使用SQLite在线backup API取得包含已提交WAL的数据库快照，并执行 `integrity_check`，
不直接拷贝活跃的 `.sqlite/-wal/-shm`。只收集白名单运行文件，不含日志、依赖或程序。
每个文件保存SHA-256用于检测损坏；校验和不是签名，应只恢复可信来源的私有备份。
默认备份时限30秒、数据库1GiB/总解包2GiB上限，超限明确失败，不生成半成品。
配置逐个读取，不与数据库形成跨文件事务；备份窗口内暂停管理员配置修改，或停止服务
以取得完整安装的同一时点快照。只采集FRP仍可正常运行的在线数据库备份。

恢复只写新目录，拒绝覆盖、归档链接、目录穿越、重复文件及校验失败；验证数据库后，
把生成配置中旧安装目录的文件路径替换为新目录。证书身份、网络端点和FRP绑定不会改变。
恢复后先以相同版本二进制 `verify -c`，再修改unit的运行目录并启用；保留旧目录直到验收。
历史会恢复，在线会话不会恢复，页面等待agent重新上报。恢复旧备份会恢复旧token散列，
因此正式切换后应轮换管理员和agent凭据；token轮换不改变原生FRP认证。

管理员恢复/轮换：在私有文件中生成新的32随机字节base64url token，将其SHA-256写入新的
0600 `admin.json` 并原子替换，约2秒内旧会话失效。保留新明文token到安全位置；不打印到日志。
节点在管理页轮换或撤销也会断开旧监控会话，原生FRP隧道继续按自身凭据运行。
