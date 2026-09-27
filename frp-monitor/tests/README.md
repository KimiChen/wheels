# Tests

本目录保存原生 FRP 基线回归、P1/P2/P3 闭环验收和脱敏参考数据。阶段范围以根目录
[README.md](../README.md) 为准；生产部署与容量验收仍待执行。

## P0 原生基线

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
wire v1/v2 和上述 Dashboard 页面检查，P3另行覆盖 UDP/HTTP/STCP 与混合新旧二进制；OIDC、reload、
Dashboard 管理 API和Prometheus仍未纳入此基线harness。

## P0 协议测试与采集参考向量

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
采集输入/预期向量，现全部由 P1 的真实采集函数消费。覆盖 CPU 差分、内存、磁盘、
网络计数器等边界；输入不读取当前机器状态。测试入口自动设置
`FRP_MONITOR_COLLECT_FIXTURES`，直接运行 Go 测试时需将该变量设为 cases.json 的绝对路径。

## P1 实时闭环

```sh
python3 tests/p1_smoke.py \
  --agent dist/darwin-arm64/frp-monitor-agent \
  --server dist/darwin-arm64/frp-monitor-server
node --test web/tests/*.test.mjs
```

P1 smoke 使用动态回环端口、临时独立凭据和真实 TLS 证书，运行完整二进制。
分别用两条 TCP 字节桥中断 FRP 和 WSS，观察原进程恢复，不依赖外部 VPS：

- 验证受信 TLS 成功、不受信证书与缺失/错误节点 token 拒绝。
- 初次 FRP 登录失败时监控依然 online/fresh；无 Proxy 也正常监控。
- TCP 二进制载荷一致，FRP 断连与恢复时监控持续在线。
- 监控断连后 offline/stale，原生 TCP 仍能转发；恢复后新会话继续最新样本。
- 公开 API/SSE 不含主机名、IP、关联、代理名称/本地目标和凭据，uint64 使用字符串。
- 静态 allowlist、CSP、未配置admin时API关闭、正常退出清理。

Go 测试另覆盖 17 个采集向量、质量与基线恢复、线程安全、首报、TLS 信任、会话替换、
序号/乱序/过大帧、独立心跳与指标过期、SSE 容量、慢采集有限退出和 3600→1 秒周期协商。
监控、上报和采集模块已运行 race/vet；原生集成补丁覆盖 reload 快照与 collector 单次注册。
`local_test.py` 检查本地凭据初始化、权限、路径限制和配置一致性。

网页测试覆盖大整数、质量状态、排序/搜索、API/SSE 切换及无消息超时。
人工浏览器检查覆盖浅/深色、390px 窄屏、无横向溢出和实时更新中的焦点/筛选/滚动。
开发宿主为 macOS arm64，Linux 指标在该宿主正确显示 unsupported；Linux amd64/arm64
交叉构建不等同于 Linux 实机 `free`/`df` 对照或部署验收。

## P2 探测与历史

```sh
python3 tests/p2_smoke.py \
  --agent dist/darwin-arm64/frp-monitor-agent \
  --server dist/darwin-arm64/frp-monitor-server
```

此验收运行真实 native 二进制：临时回环目标收到 TCP 建连，FRP echo 同时正常；
TERM 退出刷新不足一分钟的样本，重启后历史存在但节点仍 waiting；同 ID 换目标不会
继承旧历史，拒绝连接为失败、成功延迟均值无样本为 null；完整空列表取消任务后不再
访问目标；SQLite 无效路径为 degraded，实时报告和原生 FRP 仍工作。
临时配置、数据库与凭据均位于私有目录，结束清理；不访问实际服务器。

Go 模块测试另外验证：

- DNS 超时缺样、普通解析错误失败、最多三地址/各 900ms、私网默认拒绝、任务取消、
  固定 worker 和有界结果；report/ping.result 共享序号，任务方向独立防重放。
- 分钟加权平均、逐字段缺样与覆盖、超大整数/有限浮点、丢样状态、有界队列与退出。
- 首基线、同范围差分、回退/boot ID/iface/scope 变化、UTC 跨日、重启续算；
  使用事务故障注入验证 daily 与 baseline 一起回滚，重试不会重复加算。
- 真子进程在 WAL 已提交后不调用 Close 直接退出，重开后恢复并通过 integrity_check。
- 历史白名单、查询限额、任务版本更新/迟到结果、目标与凭据不进入公开响应。

P2 已运行完整 Go 测试、race/vet、25 项 Python 工具测试和 15 项 Node 测试，
并在真实页面验证四个历史窗口、TCP 实测曲线和平台不支持的空态。
Linux 双架构交叉编译不替代 Linux 实机部署或 100/500 节点负载验收。

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

## 后续验收

- **P1 跨架构补验**：Linux arm64 同步采样并对照 `free` / `df`，amd64 已通过。
- **P2 实机与压力**：Linux 长时采集/重启、慢盘和实际探测网络；按任务量测量队列、
  SQLite 文件增长及保留期清理成本，确定部署容量。
- **P3 生产补验**：更多Proxy/Visitor类型、reload与认证组合；
  Dashboard 关闭/开启都能取得统计且无重复 collector；监控、数据库及慢浏览器故障
  不阻塞转发；慢浏览器和资源竞争下的持续负载；
  Linux arm64 实机、更多部署环境，以及长期 TLS/探测/转发混合负载。
  amd64 systemd、恢复切换与短时 100/500 synthetic 结果见 [部署记录](DEPLOYMENT.md)。

## P3 管理、FRP 对账与运维

```sh
python3 tests/p3_smoke.py \
  --agent dist/darwin-arm64/frp-monitor-agent \
  --server dist/darwin-arm64/frp-monitor-server
python3 tests/p3_frp_smoke.py \
  --agent dist/darwin-arm64/frp-monitor-agent \
  --server dist/darwin-arm64/frp-monitor-server \
  --original-agent /private/path/to/pinned-original-frpc \
  --original-server /private/path/to/pinned-original-frps
```

原版二进制必须来自 `upstream.lock` 固定commit，未应用overlay或监控补丁；不可用
关闭监控的增强版冒充。原版构建同样保留Dashboard静态资源，不使用 `noweb`。
`p3_frp_smoke.py` 在新新、旧新、新旧三个组合分别运行wire v1/v2，检查TCP二进制echo、
UDP echo、HTTP虚拟主机路由及STCP visitor。增强server始终启用monitor；增强agent对接
原版server时，将telemetry设为另一个拒绝连接的回环端口，验证监控故障不影响转发。

`p3_smoke.py` 使用真实TLS完整二进制，验证管理员cookie属性、匿名拒绝、错误token、
CSRF和错源拒绝；检查预绑定与服务端user前缀后的隧道匹配、解绑后不自动认领；
读取完整私有facts却不会泄露到公开JSON/SSE；新增节点一次性token、任务版本冲突、
轮换断开旧监控会话、投递新token重新接入、撤销最后节点及历史404。凭据操作过程中
原生FRP echo继续工作。最后注销使旧cookie失效。

Go测试覆盖配置重复/大小写alias/非法JSON、会话期限与限额、握手窗口撤销、旧报告拒绝、
管理员文件轮换、坏配置保留最近有效值；只读Registry/Stats快照在忙锁/超过限额时降级，
可信binding与声明冲突、同clientID不同user、无稳定ID及普通FRP连接均有用例。
公开对账仅有状态和计数，不暴露服务端身份/地址/代理名。

`ops_test.py` 覆盖活跃SQLite WAL中的已提交记录、备份后继续写入的快照隔离、完整性检查、
Unicode恢复目录与配置路径重写、再次备份恢复、0600/0700、拒绝已有目标、软链接、归档
穿越/重复/校验错误、外部配置引用和systemd参数注入。运维工具不执行安装/远程命令。
Node测试新增管理表单、任务uint64版本、CSRF、会话过期与迟到一次性token响应丢弃。

上述本地回归不包含OIDC、所有Proxy/Visitor类型或生产负载，也不替代Linux实机/systemd验收。

P3本轮已完成：完整Go套件、涉及模块race/vet、36项Python工具测试、24项Node测试、
P0/P1/P2/P3真实二进制回归及6组新旧FRP协议矩阵。真实管理页面验证创建/轮换/撤销、
绑定/解绑、探测任务保存、窄屏/主题和注销；活跃SQLite在线备份与恢复后双配置verify通过。
Linux双架构发布包的SHA256与文件白名单另行校验；上述为 P3 开发阶段结果，Linux 实机补验见下。

## Linux amd64 测试部署补验（2026-09-27）

在用户授权的独立测试机上运行已发布增强版，最终 15 秒实机验收取得 10 个不同的 1 秒周期样本：
16 个公开指标全部通过，窗口外最大偏差为 0；`free` 内存/交换容量与筛选后的 `df` 容量和
used 对照全部通过。目标默认网卡规则参与汇总的是 1 个接口和 2 个本地挂载，未将这些机器
标识或采样原文提交到 Git。公开 scope 为 unknown；独立部署检查确认 agent 未启用会改变
采集范围的 systemd 文件/网络命名空间隔离，其 mount/net namespace 与 PID 1 相同。这是
该测试部署的上下文证据，不改变采集器对任意未知环境的保守 scope 语义。

Linux 真实二进制回归另完成 P0 wire v1/v2、P1 认证/WSS/独立故障恢复、P2 探测/TERM 尾批
持久化/数据库降级、P3 CSRF/绑定/凭据轮换撤销。根据 `upstream.lock` 固定 commit 独立构建
未加补丁的原版 frpc/frps，增强→增强、原版→增强、增强→原版与 wire v1/v2 的 6 组
TCP/UDP/HTTP/STCP payload 矩阵全部通过。回归置于临时 systemd service 中限制 CPU、内存
和运行时间，不改正式服务配置；它们是功能验收，不代表持续负载容量。

完整脱敏报告保存在部署工作目录内 ignored 的 `data/`，包括采样 JSON、Linux smoke 与
协议矩阵结果。Linux arm64 实机、长时运行与慢盘等尚未被这些短时 amd64 验收覆盖。

同轮补充已完成正式systemd TERM、server/agent SIGKILL自动恢复、在线备份实际切换，
以及100/500节点各60秒 synthetic容量测试；6000/30000条报告全部持久化、dropped=0。
完整数据与限制见 [部署记录](DEPLOYMENT.md)，复现命令见 [容量工具](CAPACITY.md)。
新增Linux验收工具后42项Python工具测试通过；capacity工具vet、交叉编译及真实Linux运行通过。
