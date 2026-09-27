# Agent

P1 由 `collect/` 采样、`service/` 独立上报，上游补丁提供只读 FRP 适配器。
原生 frpc 的 `[telemetry]` 默认为关闭；在现有 TOML 中可加入：

```toml
[telemetry]
enabled = true
endpoint = "wss://monitor.example.invalid/agent/v1/ws"
serverID = "example-server"
tokenFile = "/path/to/private/agent.token"
intervalSeconds = 1
# caFile = "/path/to/private/monitor-ca.pem"
iface = ""
```

占位地址与路径须替换；本机可直接用根 README 的 `scripts/local.py` 生成有效配置。
远程连接必须 WSS 并校验服务器证书，默认使用系统 CA，`caFile` 可附加自有 CA。
只有显式 `allowInsecureLoopback = true` 才允许指向 IP 字面量回环地址的 `ws://`。
Token 是至少 32 随机字节的无填充 base64url 编码，文件为常规文件、权限恰为 0600，
与 FRP token 分离；服务端保存编码文本的 SHA256 摘要，格式见 monitor README。

本地 `intervalSeconds` 为初始 1–3600 秒周期，握手后以服务端下发周期为准并立即调整。
只保留最新样本，不积累离线队列；心跳与采样独立。监控在首次 FRP 登录前异步启动，
若需在 FRP 登录失败后持续监控，原生配置须使用 `loginFailExit = false`。
改动 telemetry 配置或 token 后重启 agent；原生 Proxy reload/ProxyStore 变化会自动反映。
`--monitor-version` 同时输出监控版本与 FRP 基线，原生 `-v` 保持 FRP 版本输出。

本目录由 Overlay 构建映射到固定 FRP `v0.71.0` 源码的
`extension/frpmonitor/agent`，共享上游 Go module，不建立独立 `go.mod`。
具体生命周期、传输及 P1 验收规格以根目录 [README.md](../README.md) 为准。
通过窄接口读取 FRP 状态，不得反向 import 上游 `client` 根包而造成循环依赖。

## Linux 采集器

`collect.New(collect.Config{Iface: "eth0,-eth1", Version: "…"})` 返回采集实例。
`Facts() shared.Facts` 读取主机描述；`Metrics() shared.Metrics` 读取指标并推进 CPU、
网络差分基线。两方法串行化访问内部状态，Facts 不消耗差分样本；首次采样前不等待一秒。
创建实例只校验配置，不要求本机 Linux `/proc` 可读。

实现支持 Linux amd64/arm64，直接读取 `/proc`、`/sys`，Linux `statfs` 返回的
`Frsize/Blocks/Bfree` 对应协议要求的 statvfs 统计口径。不调用 shell、不访问外部地址服务。
非 Linux 平台的采集字段完整返回 `unsupported`，版本仍可报告，供本地 WSS 演示使用。

- CPU 只加前八个累计字段，idle 包含 iowait，guest 不重复加算。
- 内存按 MemAvailable 计算，仅缺失时回退；SwapCached 不抵扣 swap 使用量。
- 本地磁盘过滤伪文件系统、远程文件系统、重复设备及 ZFS pool，并识别覆盖挂载；
  使用 `f_bfree`、优先 fragment size，乘法及汇总饱和到 uint64。没有有效本地挂载，
  或任一所计挂载读取失败时，该次磁盘总量和使用量均未知，不发布部分合计。
- 网络按完整接口名纳入/排除，排除优先；显式纳入覆盖名称和内核拓扑过滤。
  默认检查设备、lower、bridge port、类型和 DEVTYPE，sysfs 不可读时保留参考名称规则。
  `boot_id` 是内核 boot ID 加排序后的实际计数接口集合 FNV-1a 摘要。
- CPU/网速无基线为 `warming_up/no_baseline`；读取错误为 `unavailable/read_error`，
  不覆盖有效基线。网络范围变化或计数器回退先重建基线，下一次有效样本才给速率；
  网速用实际单调时差，向下取整，首次仍保留有效的内核累计字节数。
- TCP 为 IPv4 inuse + tw + IPv6 inuse，UDP 为两族 inuse；不是 established 数。
- 主机地址优先公网、再优先稳定 IPv6；屏蔽容器/隧道名称、未运行、回环及链路本地地址。
  桥和 bond 可持有主机地址，地址选择不沿用流量叠加设备过滤。

已识别容器标志时 scope 为 `namespace`；其余情况为 `unknown`。与 PID 1 的 namespace
相同不能证明宿主机身份，因此当前不会自动断言 `host`。容器可见数值也不承诺是 cgroup
配额。虚拟化 `none` 只表示未发现已知标志，不是裸机证明。

采集器不创建 goroutine。单文件最多 4 MiB，目录最多 131072 个条目，网卡、地址及挂载
分别最多 4096 项；坏数据和超过上限的文件返回未知。只对已过滤的本地文件系统调用
statfs，避免远程存储阻塞；本地内核 I/O 不保证硬超时。服务应由独立采样 goroutine 调用，
不能占用 FRP 转发 goroutine，也不能每次超时都新建采样 goroutine。

## 验证与参考来源

算法对照固定的 [monitor-probe/agent collect.rs](https://github.com/monitor-probe/agent/blob/cebc5383abb5963abeb722877a2d82c1a7aea5c6/src/collect.rs)，
本目录 Go 实现和测试独立编写；过滤集合、拓扑规则及算法口径参考该实现，按参考算法
移植保留 [MIT 版权和许可](collect/LICENSE.monitor-probe)。原参考测试未复制。
参考公开算法的数值口径相同，质量状态与网络集合变更语义采用本项目协议扩展，不能称为
上游逐字节协议兼容。有效样本之外的差异见 [参考向量](../tests/fixtures/collect/README.md)。

测试通过真实生产 `Metrics` 路径及其解析/拓扑函数消费全部 17 个 P0 fixture。
唯一数据源保留在 `tests/fixtures/collect/cases.json`，Overlay 测试时设置
`FRP_MONITOR_COLLECT_FIXTURES` 为它的绝对路径，不复制第二份。补充测试覆盖错误恢复、
缺失数据、范围/时钟/重启变化、地址优先级、饱和、并发及非 Linux 合法输出。

P1 生命周期应在首次 FRP 登录前启动，每进程仅一个实例，reload 更新配置快照。
默认每秒采样，经独立 WSS 主动上报；无 Proxy 或 FRP 登录失败也能采集。
P2 增加受限 TCP 探测，本采集器尚不实现探测。协议见 [shared](../shared/README.md)。
