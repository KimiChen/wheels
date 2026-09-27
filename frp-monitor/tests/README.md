# Tests

本目录保存原生 FRP 基线、当前 P4 闭环验收和脱敏参考数据。当前 P4 已通过完整 Go 测试、
关键包 race、扩展包 vet、41 项 Python、36 项 Node 测试，以及真实 Darwin 二进制的
P4 历史开关/重启 smoke 与原生 FRP wire v1/v2 TCP 转发和重连。
Linux amd64/arm64 发布包已构建并验证包内容及校验和；本轮尚未部署。
既有 Linux 测试记录见 [DEPLOYMENT.md](DEPLOYMENT.md)，不能替代新数据结构的验收。

## 原生 FRP 基线

在子项目根目录执行，Python 仅使用标准库：

```sh
python3 scripts/frp.py build --native
python3 tests/smoke.py \
  --agent dist/darwin-arm64/frp-monitor-agent \
  --server dist/darwin-arm64/frp-monitor-server
python3 -m unittest discover -s tests -p '*test*.py' -v
```

上例是 macOS arm64；Linux 或其他架构使用本机构建输出的
`dist/<GOOS>-<GOARCH>/`。参数必须指向当前机器能执行的两个文件，不能直接运行异构
Linux 发布包。可加 `--wire v1` 或 `--wire v2` 单独定位；默认依次运行两者。
`--timeout 30` 为每项 verify / 就绪等待的秒数，允许 1–300 秒。

每个 wire 版本实际覆盖：

- 用原生 `verify -c` 校验 frps、无 Proxy frpc 和 TCP Proxy frpc 的 TOML；
- 原生 frps Dashboard 匿名首页返回 401，临时 Basic 认证后 `/static/` 返回 HTML 200，
  且至少一个首页实际引用的同源 JavaScript 资源可读，避免误用 `noweb` 隐去原生界面；
- 无 Proxy 配置完成 token 认证登录，并在登录后保持运行；
- FRP TCP 隧道把包含全部字节值的二进制载荷原样传到本地 echo 服务；
- 终止 frps、确认旧隧道端口关闭，再启动 frps；同一个 frpc 进程恢复转发；
- 退出后回收进程和临时文件，正常终止超时则强制清理并判失败。

`transport.wireProtocol = "v1" / "v2"` 来自固定上游
[frpc 配置](https://github.com/fatedier/frp/blob/4a23aa181c1d7e28eecaa8216024ed753b9d27c8/conf/frpc_full_example.toml)；
frps 自动接收两种 wire。无 Proxy 登录事件按同一 commit 的
[client/service.go](https://github.com/fatedier/frp/blob/4a23aa181c1d7e28eecaa8216024ed753b9d27c8/client/service.go)
识别，升级基线时应复核该事件。测试使用默认 TLS、token 认证与 TCP transport。

所有监听、FRP 连接和 echo 目标均为 `127.0.0.1`，动态分配端口，不访问外部服务，
不读取 `.env` 或已有 FRP 配置。父进程的代理和凭据环境变量不传给测试进程。
临时目录权限为 `0700`、配置文件为 `0600`，每轮生成独立随机测试 token；
退出时删除。进程日志持续读取但不保存、不原样打印，只保留固定登录事件和脱敏的
失败阶段/退出码；失败时可用 `--wire` 缩小范围。端口预留在启动前才释放，
操作系统不支持将预留 socket 移交给 FRP，因此极少数抢占会表现为明确失败，可重跑。

Dashboard 在 v1、v2 两轮均验证，使用独立动态回环端口和临时凭据；不跟随外部资源，
不读取或调用管理 API。这是原生 FRP 界面资源与认证回归，不是新的监控网页验收。

`test_smoke.py` 验证测试工具自身的环境隔离、临时配置权限、子进程提前退出、
超时、强制清理和二进制 echo；它不替代真实 FRP smoke。当前基线只含 TCP、token、
wire v1/v2 和上述 Dashboard 页面检查，混合协议回归另行覆盖 UDP/HTTP/STCP 与混合新旧二进制；OIDC、reload、
Dashboard 管理 API和Prometheus仍未纳入此基线harness。

## 协议测试与采集参考向量

`shared/testdata/` 保存 `hello`、首报、TCP 探测任务和结果的协议黄金帧，
由 `shared/` 的 Go 测试直接消费，检查协议解析与验证。将扩展映射到固定上游
Go module 后，可用以下入口运行协议测试及所选原生 FRP 单元测试：

```sh
python3 scripts/frp.py test
```

`pipeline_test.py` 检查源码准备/构建脚本的本地边界与失败行为；上方
`python3 -m unittest discover -s tests -p '*test*.py' -v` 会同时运行它和
smoke 工具自测，无需把测试文件名约定限定为 `test_` 前缀。

[`fixtures/collect/`](fixtures/collect/README.md) 包含 17 个自行构造的 Linux
采集输入/预期向量，现全部由真实采集函数消费。覆盖 CPU 差分、内存、磁盘、
网络计数器等边界；输入不读取当前机器状态。测试入口自动设置
`FRP_MONITOR_COLLECT_FIXTURES`，直接运行 Go 测试时需将该变量设为 cases.json 的绝对路径。

## 当前 P4 验收入口

在子项目根目录使用 Go 1.26.6+ 构建，再运行当前完整二进制：

```sh
python3 scripts/frp.py test
python3 -m unittest discover -s tests -p '*test*.py' -v
node --test web/tests/*.test.mjs
python3 scripts/frp.py build --native
python3 tests/p4_smoke.py \
  --agent dist/darwin-arm64/frp-monitor-agent \
  --server dist/darwin-arm64/frp-monitor-server
# 同一命令增加 --history，验证非空目录启用内嵌历史。
```

`p4_smoke.py` 是本轮集成入口；按当前数字节点 ID、SQLite 控制库、GitHub 管理认证与
可选 TSDB 配置验收，不把旧数据迁移视为前提。真实 GitHub 授权需要有效 OAuth App
及允许账号；离线 OAuth 测试使用受控 HTTP 客户端替身，不能当作外部账号登录验收。

模块测试和整体验收关注以下边界：

| 范围 | 验证内容 |
|---|---|
| 协议/采集/会话 | TLS 信任、hello 能力、当前连接所有权、严格序号、过期与心跳分离、质量状态、慢采集退出 |
| SQLite 控制库 | 自增 ID 不复用、节点凭据/绑定唯一性、探测删除事务、修订冲突、权限与新库初始化 |
| 当前流量 | 整周期 Max、大整数、人工校准、切换类型保留已用、主控时区/DST、月末/手动重置 |
| 恢复与故障 | 无效计数器不覆盖基线、scope/boot/接口变化、事务回滚后不重计、重启不恢复在线 |
| 可选 TSDB | 默认关闭、启用写查、保留期、重启、缺样、失败探测、队列背压、目录/查询失败隔离 |
| GitHub 管理 | state/PKCE、允许列表、授权失败、会话期限、Cookie、同源/CSRF、注销和无配置时禁用 |
| Web/API | 费用与套餐表单、人工校准、公开策略、私有字段裁剪、十进制字符串、今日/套餐/FRP 口径 |
| 工具与打包 | 私有初始化、schema 随包、控制库备份恢复、路径重写、权限、白名单与校验和 |

`monitor/control` 测试直接使用临时 SQLite，包含故障触发器验证累计值与基线原子回滚。
`monitor/store` 测试使用真实内嵌 VictoriaMetrics，包含重启与保留期清理。
测试数据和凭据仅写临时私有目录，不读取真实机器配置或提交样本。

准备源码后，可在生成的上游目录对修改模块执行 `go test -race` 和 `go vet`；
完整采集测试需要设置 `FRP_MONITOR_COLLECT_FIXTURES`，或使用 `scripts/frp.py test`
统一配置。网页人工检查包括浅/深主题、窄屏、键盘、无横向溢出及实时刷新中的焦点保持。

## FRP 对账与协议矩阵

此矩阵仍需适配 P4 的 SQLite 初始化后重新执行；`p3_frp_smoke.py` 依赖旧配置，
不作为当前可运行入口。当前可执行的集成入口使用上面的 `p4_smoke.py` 与原生 `smoke.py`。

原版二进制必须来自 `upstream.lock` 固定 commit，未应用 Overlay 或监控补丁；
不能用关闭监控的增强版冒充原版，Dashboard 资源同样需要保留。
矩阵在增强/增强、原版/增强、增强/原版组合中分别使用 wire v1/v2，验证
TCP/UDP echo、HTTP 虚拟主机及 STCP visitor。监控不可用时，原生 FRP 转发仍须工作。

可信绑定与声明冲突、同 clientID 不同 user、无稳定 ID、普通 FRP 连接，以及适配器
忙锁/快照过期都需要测试。公开对账只提供允许字段，不暴露服务端身份、地址或本地目标。

## Linux 同机采样验收

`linux_acceptance.py` 只使用 Python 标准库，在运行 agent 的同一 Linux 主机和命名空间
执行，读取公开 API，不需要管理员或节点 token，不修改服务、配置或凭据。例如：

```sh
python3 tests/linux_acceptance.py \
  --url https://127.0.0.1:17401 --ca /private/test/tls.crt --seconds 15
```

默认要求公开列表仅有一个节点；多节点用 `--node-id` 选择本机 agent。若 agent 配置了
显式网卡规则，需要传相同的 `--iface`。不要用远端机器的 `/proc` 对照另一个节点。
HTTPS 校验证书和名称；不支持跳过 TLS 校验。HTTP 仅允许字面回环地址，不使用环境代理，
不跟随重定向。采样周期 1–10 秒，观察时长至少 `4 × 周期 + 4` 秒，需取得至少 3 个不同
API 样本；默认 15 秒适用于 1 秒上报。

验收读取 `/proc/stat` 的两次差分（iowait 计 idle、不重加 guest）、`/proc/meminfo`、
`/proc/net/dev`、socket/进程/uptime/load；按采集器相同挂载与网卡筛选规则汇总。
内存、交换和磁盘容量要求精确相等，并以 `free --bytes`、筛选后的 `df -B1` 验证容量；
内存 used 使用 MemTotal−MemAvailable，不能直接拿不同版本 `free` 的 used 列等同。
磁盘 used 使用 blocks−bfree，排除远程/伪文件系统，并按设备及 ZFS 池去重。

API 的时间是服务端收报时间，与内核读数没有严格同步；工具保留 `3 × 周期 + 2` 秒的
本地窗口。网络累计计数必须落在该窗口精确上下界内，静态容量没有误差额度；CPU 使用窗口
包络另加 5 个百分点，内存 used 加 0.5% 总量或 8 MiB，其他动态字段采用输出所列容差。
因此这是实机取值、质量标志、类型与量级验收，不替代 fixture 的算法精确性证明，也不证明
容器指标属于宿主机。`scope=unknown` 是采集器的保守表示，允许通过实机数值验收；宿主机
部署、命名空间与 PID 1 的对照是额外部署上下文，不能由该字段推断。输出仅含固定字段 JSON，列出样本数、各指标失败数、窗口外最大偏差和
容差；不含 IP、主机名、网卡名、节点标识、路径或原始错误。退出码 0 表示全部通过。
如需留存，将 stdout 重定向到 ignored 的 `.cache/` 或 `data/`，不要提交机器样本。

`linux_acceptance_test.py` 覆盖 MemAvailable=0 与缺失回退、CPU guest/iowait、过滤去重与
overmount、uint64/quality、计数器包络边界和错误脱敏。

## 工具与运维测试

`local_test.py` 验证新控制库、可选历史、OAuth 配置、0600/0700 和已有目录保护。
`ops_test.py` 验证控制库在线快照、完整性、恢复后的路径、校验和及已有目标/软链接/
归档穿越拒绝。TSDB 历史不混入 SQLite 事务或恢复账本。
`pipeline_test.py` 验证固定源码、补丁、构建和发布包边界；Node 测试验证页面状态与表单。
这些检查不执行远程安装，也不替代真实 systemd 和业务负载验证。

## 部署与持续负载待验收

浏览器 fixture 已覆盖节点卡片、详情、管理编辑和 390px 窄屏，无页面错误及横向溢出。
- 有效 GitHub OAuth App 和允许账号的实际登录、拒绝与注销。
- 本轮 Linux 二进制的实机采样对照、systemd TERM/SIGKILL 恢复和备份切换。
- 长时 TSDB 保留期、慢盘、队列背压、历史关闭/故障时的实时与 FRP 故障隔离。
- 按 100/500 节点测量持续 CPU、内存、磁盘和浏览器开销；测试档位不等于容量承诺。

既有部署与短时容量结果集中在 [部署记录](DEPLOYMENT.md) 和 [容量工具](CAPACITY.md)。
报告只写 ignored 的 `.cache/` 或 `data/`；公开 Git 不提交真实节点身份、地址、配置与凭据。
