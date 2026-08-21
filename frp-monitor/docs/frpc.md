# frpc 轻量 Telemetry / 管理模块开发计划

> 文档状态：Draft v1.1（frp v0.71 基线）
> 编写日期：2026-08-22
> 目标基线：frp v0.71.0，固定上游 Tag `v0.71.0` / Commit `4a23aa1`
> 核心方向：复用 frpc 原生 Store/Admin API 管理 Proxy/Visitor，在同一个 frpc 二进制中增加轻量、独立生命周期的 Telemetry / 管理模块。

## 1. 背景与结论

本项目只以 frp v0.71.0 作为开发、构建和发布基线，不把 v0.68/v0.69 作为客户端兼容目标。v0.71.0 继承了 v0.68.0 引入的以下能力：

- frpc Store 持久化；
- Proxy/Visitor 的运行时 CRUD；
- Store Admin API；
- 配置修改后运行时加载，不需要重启 frpc；
- Proxy/Visitor 独立的 `enabled` 状态。

因此，本项目不重复实现隧道配置引擎，也不自定义一套 Proxy 生命周期。模块只负责：

1. 每 60 秒采集并上报客户端系统状态；
2. 上报 frpc 实例和 Proxy 的实际状态；
3. 从面板取得期望配置版本或配置命令；
4. 通过本机 frpc Store/Admin API 应用 Proxy/Visitor 变更；
5. 上报应用结果，完成期望状态与实际状态的对账。

模块与 frpc 一起编译、作为单一二进制交付，但不实现为普通的“Proxy 客户端插件”。frpc 当前客户端插件接口主要围绕 `Handle(connection)` 和 `Close()`，实例生命周期与单条 Proxy 关联；如果把监控上报直接放进该接口，会出现多 Proxy 重复上报、无 Proxy 时不启动、reload 后重复创建等问题。

推荐将模块挂载在 frpc `client.Service` 生命周期上：一个 frpc 进程只有一个 Telemetry Service，隧道数据面与监控控制面代码分离。

### 1.1 v0.71 基线约束

v0.71.0 对本方案有以下直接影响：

- 当 frpc/frps 使用 wire protocol v2 并成功协商能力时，普通 UDP Proxy 和 SUDP 使用更紧凑的二进制报文编码；协商失败时仍回退到 JSON `UDPPacket`；
- v0.71.0 会拒绝负数 `pool_count`，安装器和本地配置校验仍应主动限制其范围；
- `frpc verify` 已正确读取 `featureGates`，因此生成配置的验收流程必须执行 v0.71.0 自带的 `frpc verify`；
- v0.71.0 修复了 `customDomains` 大小写校验绕过，面板和客户端仍必须在计算 Hash、冲突检查和下发前将域名统一转成小写；
- v0.70.0 起 frpc 会拒绝配置文件中重复的 Proxy/Visitor 名称，Planner 必须在调用 Store API 前执行同样的唯一性检查；
- v0.70.0 起 frps Dashboard API v2 覆盖 Clients、Proxies、Server Overview、Client/Proxy Detail 和流量历史，因此面板应把 frps API v2 作为“服务端实际状态”的数据源，把 Telemetry 作为“客户端本地状态”的数据源；
- v0.70.1 已修复半开 TCP Mux 连接下 frpc 重连导致的 control-session 资源泄漏，v0.71.0 已包含该修复。

生产推荐 frpc 和 frps 都固定为 v0.71.0，并显式设置：

```toml
transport.wireProtocol = "v2"
```

如果部署必须连接旧 frps，需单独建立兼容矩阵并决定是否回退到 v1；不能让客户端自动、静默改变 wire protocol。Telemetry 使用独立 HTTPS，不承载在 frp wire protocol 内，因此 wire protocol v1/v2 的选择不会改变 Telemetry 上报协议。

## 2. 产品目标

### 2.1 必须实现

- Linux 客户端每 60 秒主动向面板上报一次完整状态；
- 上报失败不能阻塞或中断 frpc 隧道；
- 客户端只能主动向外建立 HTTPS 连接，不要求公网访问客户端；
- 面板可以下发 Proxy/Visitor 的创建、更新、删除、启用和停用；
- 变更通过 frpc 原生 Store/Admin API 执行并持久化；
- 每次配置变更都有版本、命令 ID、应用结果和错误信息；
- 管理 API 只监听 `127.0.0.1`，不直接暴露公网；
- 不采集或上报 frpc Token、SecretKey、HTTP 密码、证书私钥等秘密；
- 客户端重启后能够恢复身份、采集基线、Store 配置版本和未完成命令状态。

### 2.2 第一阶段不实现

- 远程 Shell、任意命令执行和文件管理；
- frpc 二进制远程升级；
- 动态修改 `serverAddr`、`serverPort`、认证方法等全局配置；
- Windows/macOS 的完整系统指标采集；
- 严格计费级流量账本；
- 通过修改 frp wire protocol 携带监控指标；
- 将 frpc Admin API 映射到公网；
- 多面板控制同一个 frpc 实例。

### 2.3 后续可扩展

- Windows、macOS、OpenWrt/嵌入式 Linux Collector；
- WebSocket/gRPC 长连接，将配置生效延迟从最多 60 秒降低到秒级；
- frpc 灰度升级与失败回滚；
- 日志片段上传；
- 远程诊断，但仍不默认开放任意命令；
- 指标增量上报、压缩和时序数据离线补传。

## 3. 总体架构

```text
┌─────────────────────────────┐
│         管理面板             │
│                             │
│  Agent Enrollment API       │
│  Telemetry Report API       │
│  Desired Config/Revisions   │
│  Command Result API         │
│  PostgreSQL / TSDB          │
└──────────────┬──────────────┘
               │ 客户端主动 HTTPS
               │ 报告 + 配置版本/命令响应
               ▼
┌──────────────────────────────────────────┐
│          自定义 frpc 单一进程             │
│                                          │
│  ┌────────────────────────────────────┐  │
│  │ Telemetry / Management Service     │  │
│  │                                    │  │
│  │ Scheduler ─ Collectors ─ Reporter  │  │
│  │                │          │        │  │
│  │ State Store ─ Reconciler ─┘        │  │
│  └────────────────┬───────────────────┘  │
│                   │ localhost HTTP       │
│                   ▼                      │
│  frpc Admin API + Store                  │
│                   │                      │
│                   ▼                      │
│  原生 Proxy / Visitor / frp Control      │
└───────────────────┬──────────────────────┘
                    │ frp 隧道
                    ▼
                  frps
```

关键原则：

- 数据面：完全复用 frp；
- 隧道配置：完全复用 Store/Admin API；
- 系统监控：Telemetry Service 自己采集；
- 配置通信：客户端主动访问面板，避免 NAT 入站问题；
- 一致性：面板保存期望状态，客户端上报实际状态；
- 故障隔离：Telemetry 的所有调用必须有超时、独立 goroutine 和资源上限。

## 4. frpc 内部集成方式

### 4.1 生命周期

Telemetry Service 应与 frpc Service 同级，而不是与 Proxy 实例同级。

建议生命周期：

```text
frpc 解析配置
  → 初始化日志和本地 Admin Server
  → 初始化原生 frpc Service
  → 初始化 Telemetry Service
  → frpc 登录 frps
  → Telemetry 首次立即采集并上报
  → 每 60 秒定时采集、上报和配置对账
  → 收到 context cancel
  → 停止采集、完成有限时间内的最终状态写入
  → 关闭 Telemetry
  → 关闭 frpc
```

Telemetry 初始化失败的默认策略：记录错误并继续启动 frpc。除非配置显式设置 `telemetry.required = true`，Telemetry 不得成为隧道启动的前置条件。

### 4.2 最小侵入修改

为了降低后续合并上游 frp 的成本：

- Telemetry 独立放在一个 package 中；
- 对 `client.Service` 只增加构造、启动和关闭三个集成点；
- 不修改 Proxy 数据转发路径；
- 不修改 frp wire protocol；
- 不修改原生 Store 数据结构；
- 尽量通过 localhost Admin API 而不是直接调用 frp 内部非稳定接口；
- 可增加 `notelemetry` build tag，构建纯上游兼容版本。

建议目录：

```text
client/telemetry/
├── service.go                 # 生命周期与模块编排
├── config.go                  # Telemetry 配置
├── scheduler.go               # 60 秒调度、抖动、重试
├── model/
│   ├── report.go              # 上报协议模型
│   ├── command.go             # 管理命令模型
│   └── state.go               # 本地状态模型
├── collector/
│   ├── collector.go           # Collector 接口
│   ├── cpu_linux.go
│   ├── memory_linux.go
│   ├── disk_linux.go
│   ├── network_linux.go
│   ├── connection_linux.go
│   ├── process_linux.go
│   ├── os_linux.go
│   └── frpc.go
├── reporter/
│   ├── client.go              # HTTPS Client
│   ├── enroll.go              # 首次注册
│   └── retry.go
├── reconcile/
│   ├── reconciler.go          # desired/actual 对账
│   ├── planner.go             # 生成 CRUD 操作计划
│   ├── admin_client.go        # localhost Store API 客户端
│   └── rollback.go
└── storage/
    ├── state.go               # agentID、seq、generation
    └── journal.go             # 命令执行日志
```

## 5. 配置设计

v0.71.0 的安装器必须为每个实例生成稳定且唯一的 `clientID`，并在 frpc/frps 同为 v0.71.0 时启用 wire protocol v2：

```toml
clientID = "node-stable-uuid"
transport.wireProtocol = "v2"
```

`clientID` 在重装、重启和配置 reload 后应保持不变，用于把 frps Dashboard API v2 中的客户端记录与面板 Agent 记录关联。禁止使用 Hostname 作为唯一标识。

在 frpc TOML 中增加独立 Telemetry 配置段：

```toml
[telemetry]
enabled = true
required = false
endpoint = "https://panel.example.com"
intervalSeconds = 60
requestTimeoutSeconds = 10
initialJitterSeconds = 10
statePath = "/var/lib/frpc/telemetry-state.json"
enrollmentTokenFile = "/etc/frpc/enrollment-token"
caFile = "/etc/frpc/panel-ca.crt"
reportFullSnapshot = true
offlineBufferSize = 120
maxReportBytes = 262144
```

约束：

- 构建必须基于 Tag `v0.71.0` / Commit `4a23aa1`，不能跟随 `dev` 分支漂移；
- 正式环境默认要求 frpc/frps 版本均为 v0.71.0；
- `transport.wireProtocol = "v2"` 上线前必须完成 TCP、UDP、SUDP、HTTP、HTTPS 和重连测试；
- `clientID` 必须稳定、唯一并持久化；
- `intervalSeconds` 默认 60，服务端允许下发的最小值暂定 15、最大值 3600；
- Token 只允许通过文件加载，文件权限必须为 `0600`；
- `statePath` 必须使用原子写入并设置 `0600`；
- Admin API 保持：

```toml
webServer.addr = "127.0.0.1"
webServer.port = 7400
webServer.user = "local-telemetry"
webServer.password = "从独立权限为 0600 的文件或安全存储加载"

[store]
path = "/var/lib/frpc/store.json"
```

如果上游当前版本不支持 Web Server 密码从文件加载，则安装器生成高强度随机密码，并确保配置文件权限为 `0600`。Telemetry 不得把密码写进日志或上报内容。

## 6. 指标采集范围

### 6.1 每 60 秒上报的完整指标

#### CPU

- 使用率百分比；
- 逻辑核心数；
- 物理核心数；
- 1、5、15 分钟系统负载。

Linux 数据来源：

- `/proc/stat`：CPU 累计时间；
- `/proc/cpuinfo`：物理 CPU/核心拓扑；
- `runtime.NumCPU()`：逻辑核心数；
- `/proc/loadavg`：系统负载。

CPU 使用率计算：

```text
totalDelta = totalNow - totalPrevious
idleDelta  = idleNow  - idlePrevious
usage      = (totalDelta - idleDelta) / totalDelta * 100
```

首次启动没有前一采样点时，允许 `usagePercent` 为 `null`，或先等待一个短采样窗口后生成首个值。必须处理计数器异常、采样间隔为零和数值越界。

#### 内存与 Swap

- 内存总量；
- 可用内存；
- 已用内存；
- 使用率；
- Swap 总量；
- Swap 已用量。

来源：`/proc/meminfo`。内存“已用”统一定义为 `MemTotal - MemAvailable`，避免缓存被错误计算为不可用内存。

#### 磁盘

每个有效挂载点上报：

- 设备名；
- 挂载点；
- 文件系统类型；
- 总容量；
- 已用容量；
- 可用容量；
- 使用率。

默认排除：

- `proc`、`sysfs`、`devtmpfs`、`devpts`；
- `tmpfs`、`cgroup`、`cgroup2`；
- `overlay` 等容器临时挂载，除非明确允许；
- 同一设备的重复 bind mount。

使用 `statfs` 获取容量。Collector 设置挂载点最大数量，默认 64，避免异常系统生成超大上报。

#### 系统负载、进程与启动时间

- Load 1/5/15；
- 系统可见进程数；
- 启动 Unix 时间；
- 运行秒数。

来源：

- `/proc/loadavg`；
- `/proc/[0-9]+` 目录计数；
- `/proc/stat` 的 `btime` 或 `sysinfo`。

如果 frpc 在容器中运行，进程数、磁盘和网络可能只代表命名空间视图。第一阶段推荐以 systemd 方式在宿主机运行，并在上报中增加：

```json
"environment": {
  "containerized": false,
  "namespace_limited": false
}
```

#### 网络流量

每个有效网卡上报：

- 接收累计字节数；
- 发送累计字节数；
- 接收累计包数；
- 发送累计包数；
- 接收错误/丢包；
- 发送错误/丢包；
- 过去采样区间平均接收速度；
- 过去采样区间平均发送速度。

来源：`/proc/net/dev`。

速度计算：

```text
rxBytesPerSecond = (rxNow - rxPrevious) / actualElapsedSeconds
txBytesPerSecond = (txNow - txPrevious) / actualElapsedSeconds
```

必须使用真实经过时间，而不是固定除以 60。调度延迟、休眠恢复、网络阻塞都会让实际间隔发生变化。

计数器重置规则：

- 当前累计值小于上次累计值时，判定网卡重启、系统重启或计数器回绕；
- 本次速度返回 `null` 或 0，同时设置 `counterReset=true`；
- 以当前值作为新的基线；
- 使用系统 boot ID 区分系统重启。

默认排除 loopback；虚拟网卡是否展示通过服务端策略或本地配置控制。

说明：60 秒间隔得到的是“过去 60 秒的平均速度”，不是秒级实时速度。如果后续需要更平滑的哪吒式曲线，可本地每 5 秒采样、每 60 秒上报 `current/average/max`，或者把上报间隔降低到 5～10 秒。

#### TCP/UDP 连接总览

系统范围上报：

- TCP 总连接数；
- TCP `ESTABLISHED`；
- TCP `LISTEN`；
- TCP `TIME_WAIT`；
- TCP 其他状态；
- UDP/UDP6 Socket 总数。

Linux 来源：

- `/proc/net/tcp`；
- `/proc/net/tcp6`；
- `/proc/net/udp`；
- `/proc/net/udp6`。

第一阶段只上报汇总，不上报远端 IP、端口和进程归属，降低数据敏感度和高基数风险。

#### 操作系统发行版

- 发行版 ID；
- 展示名称；
- 版本 ID；
- Pretty Name；
- 内核版本；
- 体系结构；
- 主机名。

来源：

- `/etc/os-release`；
- `uname`；
- `os.Hostname()`。

所有字符串都应设置长度上限并过滤不可打印字符。

#### frpc 与 Proxy 信息

上报：

- frpc 版本；
- `clientID`；
- frps 地址的脱敏展示值；
- frps 登录/连接状态；
- frp wire protocol 版本，v0.71 基线期望值为 `v2`；
- 已协商能力（如果当前 v0.71.0 内部状态可安全读取），例如 UDP 二进制 Codec；
- 每条 Proxy 的名称、类型、启用状态、运行状态；
- `localIP`、`localPort`；
- `remotePort` 或域名；
- Store 来源或配置文件来源；
- 当前错误摘要；
- 当前应用的配置 generation。

注意：`localIP` 和 `localPort` 不属于 frpc 注册 `NewProxy` 时必然发送到 frps 的公开字段，必须在客户端从当前合并后的配置/Store 状态中读取。

如果 Proxy 使用客户端插件：

- `localIP`/`localPort` 可能为空；
- 只上报经过允许列表过滤的 `pluginType`；
- 不上报插件的密码、证书私钥路径、SecretKey 或其他敏感选项。

#### v0.71 的客户端/服务端状态边界

面板需要合并两个实际状态来源，不能只相信单侧：

- Telemetry：CPU、内存、磁盘、本地网卡、本地 TCP/UDP、`localIP/localPort`、本地 Store generation 和本地 Proxy 错误；
- frps Dashboard API v2：在线 Client、服务端已注册 Proxy、服务端视角流量、服务端 Proxy 状态和流量历史。

关联主键优先使用稳定 `clientID`；Proxy 使用规范化后的 `user + proxyName` 或面板分配的 Proxy ID/metadata。两侧状态不一致时展示 degraded，例如：

- 客户端 Store 中存在、frps 未注册：`LOCAL_ONLY`；
- frps 存在、客户端期望配置不存在：`SERVER_STALE`；
- 两侧都存在但 generation/hash 不一致：`CONFIG_DRIFT`；
- 客户端离线但 frps 仍保留历史记录：以实时 Client 在线状态为准，历史数据不得标记为在线。

### 6.2 静态字段与动态字段

第一阶段按照需求每 60 秒发送完整快照，简化服务端一致性处理。面板可以通过字段哈希避免重复写入静态资产信息。

后续优化可拆分为：

- Inventory：OS、核心数、磁盘、frpc 版本，启动时和变化时上报；
- Sample：CPU、内存、负载、网速、连接数，每个周期上报；
- Proxy Snapshot：配置版本变化或 Proxy 状态变化时上报。

## 7. 上报协议

### 7.1 首次注册

请求：

```http
POST /api/v1/agents/enroll
Authorization: Bearer <one-time-enrollment-token>
Content-Type: application/json
```

```json
{
  "client_id": "frpc-client-id",
  "hostname": "home-server",
  "frpc_version": "0.71.0",
  "module_version": "0.1.0",
  "public_key": "可选：客户端生成的公钥"
}
```

响应：

```json
{
  "agent_id": "agt_01J...",
  "credential": "长期凭据或短期证书",
  "credential_expires_at": 1790000000,
  "report_interval_seconds": 60,
  "desired_generation": 0
}
```

注册成功后立即删除或清空一次性 enrollment token 文件。长期凭据写入权限为 `0600` 的状态文件，后续支持轮换和吊销。

### 7.2 周期报告

请求：

```http
POST /api/v1/agents/{agentID}/reports
Authorization: Bearer <agent-credential>
Content-Type: application/json
Idempotency-Key: <agentID>-<bootID>-<sequence>
```

建议报告结构：

```json
{
  "schema_version": "1.0",
  "agent_id": "agt_01J...",
  "boot_id": "linux-boot-id",
  "sequence": 1024,
  "collected_at": 1787371200,
  "monotonic_elapsed_ms": 60012,
  "interval_seconds": 60,
  "module_version": "0.1.0",
  "applied_generation": 18,
  "last_command_id": "cmd_01J...",
  "system": {
    "hostname": "home-server",
    "boot_time": 1787000000,
    "uptime_seconds": 371200,
    "process_count": 186,
    "os": {
      "id": "ubuntu",
      "pretty_name": "Ubuntu 24.04.2 LTS",
      "version_id": "24.04",
      "kernel": "6.8.0-60-generic",
      "arch": "amd64"
    },
    "cpu": {
      "usage_percent": 13.42,
      "logical_cores": 8,
      "physical_cores": 4,
      "load_1": 0.42,
      "load_5": 0.51,
      "load_15": 0.38
    },
    "memory": {
      "total_bytes": 17179869184,
      "available_bytes": 10987654321,
      "used_bytes": 6192214847,
      "used_percent": 36.04
    },
    "swap": {
      "total_bytes": 4294967296,
      "used_bytes": 268435456
    }
  },
  "disks": [
    {
      "device": "/dev/nvme0n1p2",
      "mount_point": "/",
      "filesystem": "ext4",
      "total_bytes": 536870912000,
      "used_bytes": 210453397504,
      "available_bytes": 326417514496
    }
  ],
  "network": [
    {
      "name": "eth0",
      "rx_bytes_total": 1827364512,
      "tx_bytes_total": 918273645,
      "rx_packets_total": 1234567,
      "tx_packets_total": 765432,
      "rx_bytes_per_second": 18234,
      "tx_bytes_per_second": 7261,
      "counter_reset": false
    }
  ],
  "connections": {
    "tcp": {
      "total": 93,
      "established": 21,
      "listen": 14,
      "time_wait": 47,
      "other": 11
    },
    "udp": {
      "total": 18
    }
  },
  "frpc": {
    "version": "0.71.0",
    "client_id": "node-001",
    "online": true,
    "wire_protocol": "v2",
    "negotiated_capabilities": ["udp-binary-codec"],
    "applied_generation": 18,
    "proxies": [
      {
        "name": "ssh",
        "type": "tcp",
        "enabled": true,
        "status": "running",
        "source": "store",
        "local_ip": "127.0.0.1",
        "local_port": 22,
        "remote_port": 6001,
        "error": ""
      }
    ]
  },
  "command_results": [
    {
      "command_id": "cmd_01J...",
      "success": true,
      "finished_at": 1787371190,
      "error_code": "",
      "error_message": ""
    }
  ]
}
```

面板响应：

```json
{
  "accepted_sequence": 1024,
  "server_time": 1787371201,
  "next_report_after_seconds": 60,
  "desired_generation": 19,
  "desired_config_hash": "sha256:...",
  "commands": [
    {
      "id": "cmd_01J...",
      "generation": 19,
      "operation": "upsert_proxy",
      "issued_at": 1787371195,
      "expires_at": 1787374795,
      "payload": {
        "name": "ssh",
        "type": "tcp",
        "enabled": true,
        "localIP": "127.0.0.1",
        "localPort": 22,
        "remotePort": 6001
      }
    }
  ]
}
```

### 7.3 协议要求

- `schema_version` 必须可演进；
- 未知字段必须被忽略；
- 服务端按 `agentID + bootID + sequence` 幂等接收；
- 客户端按 `command_id` 幂等执行；
- 命令必须有过期时间；
- 响应大小必须有上限，默认 1 MiB；
- 报告大小默认限制 256 KiB；
- 客户端必须同时保存服务端时间和本地时间，用于识别严重时钟漂移；
- 第一阶段可在报告响应中直接返回少量命令；完整配置较大时只返回 revision URL 和 Hash；
- 服务端不得依赖客户端上报时间判断在线状态，应优先使用服务器接收时间。

## 8. 配置下发与状态对账

### 8.1 状态模型

面板保存：

- `desired_generation`：用户希望客户端应用的配置版本；
- `desired_config_hash`：规范化配置 Hash；
- `desired_proxies` / `desired_visitors`；
- revision 创建人、创建时间和变更说明。

客户端保存：

- `last_seen_generation`；
- `applying_generation`；
- `applied_generation`；
- `applied_config_hash`；
- 已执行的最近 Command ID；
- 应用前 Store 快照；
- 每条命令的执行结果。

### 8.2 对账流程

```text
1. Telemetry 上报 applied_generation=N
2. 面板返回 desired_generation=N+1
3. 客户端验证命令签发对象、版本、过期时间和 Payload
4. 从 localhost Store API 读取当前实际配置
5. 将当前配置规范化并生成 actual hash
6. Planner 计算 desired 与 actual 的差异
7. 保存本地执行 Journal 和回滚快照
8. 依次执行 Store CRUD
9. 查询 frpc Proxy 状态，确认配置加载结果
10. 全部成功后原子写入 applied_generation=N+1
11. 下次报告携带应用结果
12. 失败则记录错误并按策略回滚或等待人工处理
```

### 8.3 操作顺序

常规情况下：

1. 创建不会冲突的新 Proxy/Visitor；
2. 更新现有项；
3. 确认新配置运行；
4. 删除废弃项。

遇到相同名称、相同端口或相同域名冲突时，Planner 必须显式识别，不能盲目先创建。必要时：

1. 保存旧配置；
2. 停用或删除旧项；
3. 创建新项；
4. 验证；
5. 失败则恢复旧项。

Store Admin API 当前不应被假定为提供跨多个 Proxy 的数据库事务，因此客户端必须通过 Journal 和快照实现“可恢复的多步操作”，不能宣称强原子性。

### 8.4 幂等与重复命令

- 相同 `command_id` 再次收到时直接返回已有结果；
- 相同 generation 和 config hash 已应用时不再调用 Store API；
- generation 小于已应用版本时拒绝执行并返回 `STALE_GENERATION`；
- generation 跳跃时允许拉取完整 revision，但不能只执行不完整的增量命令；
- 客户端重启后从 Journal 恢复 `applying` 命令并重新核对实际 Store 状态；
- 只保证“效果上的 exactly-once”，网络协议本身采用 at-least-once。

### 8.5 支持的管理操作

第一阶段允许：

- `upsert_proxy`；
- `delete_proxy`；
- `enable_proxy`；
- `disable_proxy`；
- `upsert_visitor`；
- `delete_visitor`；
- `replace_store_revision`，通过完整 revision 对账，而不是直接覆盖文件。

第一阶段明确拒绝：

- `exec`；
- 任意文件写入；
- 修改 frpc 启动参数；
- 修改 Telemetry endpoint；
- 修改 Agent 身份文件；
- 修改 Admin API 监听地址；
- 下发未经允许的客户端插件类型；
- 直接下发包含 Token、私钥或 Shell 命令的字段。

## 9. 本地状态持久化

建议状态文件：

```json
{
  "schema_version": 1,
  "agent_id": "agt_01J...",
  "credential": "encrypted-or-protected-value",
  "credential_expires_at": 1790000000,
  "sequence": 1024,
  "last_boot_id": "...",
  "last_seen_generation": 19,
  "applied_generation": 18,
  "applied_config_hash": "sha256:...",
  "last_network_counters": {},
  "recent_command_ids": []
}
```

写入要求：

1. 写入同目录临时文件；
2. `fsync` 临时文件；
3. 原子 rename；
4. 必要时 `fsync` 父目录；
5. 权限保持 `0600`；
6. 解析失败时保留损坏文件用于诊断，不能静默覆盖；
7. Command Journal 可采用 JSON Lines 或小型嵌入式数据库，但第一阶段优先选择简单、可审计的格式。

网络累计计数可以持久化，但系统 boot ID 或网卡累计值发生回退时必须重置基线。

## 10. 调度、重试与离线行为

### 10.1 正常调度

- frpc 启动成功后立即进行一次完整采集；
- 首次网络报告增加 0～10 秒随机抖动；
- 后续默认每 60 秒调度；
- 使用单一 Scheduler，禁止重叠执行两个采集/上报周期；
- 如果一次周期超过 60 秒，跳过已错过的 tick，不并发补跑；
- 使用 monotonic clock 计算采样间隔；
- 单次采集目标超时 2 秒，单次报告 HTTP 超时 10 秒。

### 10.2 网络失败

建议重试节奏：

```text
1s → 2s → 5s → 10s → 30s → 60s
```

每次加入随机抖动。恢复后回到正常 60 秒周期。

重试原则：

- 不能在内存中无限堆积报告；
- 默认离线缓冲最近 120 个样本；
- 缓冲满后丢弃最旧样本；
- 恢复后优先发送最新快照，再限速补传历史样本；
- 401/403 不做高频重试，进入凭据刷新或重新注册流程；
- 400 类协议错误记录并丢弃对应非法报告；
- 429 和 5xx 遵守 `Retry-After`；
- DNS、TLS、超时错误不得影响 frpc 原生连接。

### 10.3 面板不可用

- 已存在的隧道继续运行；
- Store 中配置保持不变；
- 禁止因控制面离线主动删除 Proxy；
- 不更新 `applied_generation`；
- Telemetry 进入 degraded 状态并写本地日志；
- 面板恢复后通过 generation/hash 重新对账。

## 11. 安全设计

### 11.1 通信安全

- 只允许 HTTPS；
- 默认验证系统 CA，支持私有 CA 文件；
- 禁止 `insecureSkipVerify` 进入正式配置；
- 首次注册使用一次性、短有效期 enrollment token；
- 长期 Agent 凭据可以轮换和吊销；
- 第二阶段升级为 mTLS；
- 限制重定向，防止凭据被发送到其他域名；
- HTTP Client 禁止使用不受信任的环境代理，除非显式配置；
- 设置连接、TLS 握手、响应头和总请求超时。

### 11.2 Admin API 安全

- 只监听 `127.0.0.1` 或 Unix Socket；
- 不通过 TCP/HTTP Proxy 暴露到公网；
- 使用高强度随机 Basic Auth 或后续的本地 Unix Socket；
- Telemetry 只允许访问 Store CRUD 和状态查询端点；
- 请求和响应不得写入包含密码的 debug 日志；
- 如果未来使用 Unix Socket，需要确认上游 Admin Server 是否支持，否则保持 loopback HTTP。

### 11.3 下发配置验证

客户端在调用 Store API 前进行二次验证：

- Proxy/Visitor 类型允许列表；
- 名称长度和字符集；
- Proxy/Visitor 名称在完整合并配置中必须唯一，保持与 v0.71.0 的校验行为一致；
- IP、端口和域名格式；
- `customDomains`、`subdomain` 在冲突判断、Hash 和下发前统一规范化为小写；
- 端口范围 1～65535；
- `poolCount` 必须大于等于 0，并设置面板侧最大值；
- 最大 Proxy/Visitor 数量；
- 最大 JSON 深度和请求大小；
- 不允许未知插件类型；
- 不允许路径穿越和任意文件路径；
- 不允许在配置中携带认证 Token 和私钥；
- 对 `static_file`、`unix_domain_socket` 等高权限插件默认禁用，必须由本地策略明确开启。

特别注意：`unix_domain_socket` 如果指向 Docker Socket，等价于获得很高的宿主机控制权限，不能由普通面板用户直接下发。

### 11.4 隐私与脱敏

- `localIP` 会暴露内网结构，只向有权限的管理员展示；
- 第一阶段不采集进程名、命令行、远端连接地址；
- 日志中的 Token、密码、Authorization Header 必须脱敏；
- 面板必须记录谁查看、创建和修改了内网 Proxy；
- 上报 Hostname、发行版和本地地址前，应在安装说明中明确告知用户。

## 12. 面板服务端配套设计

### 12.1 最小数据表

#### agents

- `id`；
- `tenant_id`；
- `client_id`；
- `hostname`；
- `credential_hash`；
- `status`；
- `last_seen_at`；
- `last_sequence`；
- `boot_id`；
- `frpc_version`；
- `module_version`；
- `applied_generation`；
- `desired_generation`；
- `created_at`、`updated_at`。

#### agent_config_revisions

- `id`；
- `agent_id`；
- `generation`；
- `config_json`；
- `config_hash`；
- `created_by`；
- `change_summary`；
- `created_at`。

唯一约束：`(agent_id, generation)`。

#### agent_commands

- `id`；
- `agent_id`；
- `generation`；
- `operation`；
- `payload_json`；
- `status`；
- `issued_at`；
- `expires_at`；
- `started_at`；
- `finished_at`；
- `error_code`、`error_message`。

#### agent_inventory

- OS、CPU 核心数、内存、磁盘和 frpc 版本等最新资产信息；
- 可通过 Inventory Hash 避免每 60 秒重复写入相同静态字段。

#### metric_samples

建议写入 Prometheus/VictoriaMetrics/其他 TSDB，而不是长期直接堆在 PostgreSQL。指标标签必须控制高基数，Proxy 名称、网卡名、挂载点数量需要设置上限。

### 12.2 服务端一致性

- 更新期望配置时，在数据库事务中创建 revision 并增加 generation；
- 端口、域名冲突必须在服务端提前验证；
- 相同 Agent 只允许一个有效控制者；
- 面板接收报告后先幂等落库，再响应命令；
- 客户端报告 `applied_generation` 后，才能把 revision 标记为已应用；
- 超时未应用应标记 degraded 并触发告警；
- 服务端不能因为客户端短暂离线自动回收端口，除非有明确租约策略。

## 13. 可观测性与资源预算

Telemetry 自身应输出：

- 采集耗时；
- 序列化耗时；
- 报告大小；
- 上报成功/失败次数；
- 最近成功时间；
- 重试次数；
- 待发送缓冲长度；
- 配置对账成功/失败次数；
- Store API 调用延迟；
- 当前 desired/applied generation。

资源目标：

- 空闲时新增 CPU 平均占用低于单核 0.5%；
- 单次采集时间 P95 低于 500 ms；
- Telemetry 增量内存目标低于 20 MiB；
- 正常完整报告目标小于 32 KiB；
- 最大报告硬限制 256 KiB；
- 不允许 Telemetry goroutine 无界增长；
- 不允许采集或上报持有 frpc 数据转发关键锁；
- 在 10,000 Agent、每 60 秒一次报告时，服务端平均约接收 167 次请求/秒，必须加入客户端抖动并进行容量测试。

## 14. 测试计划

### 14.1 单元测试

Collector 使用固定 fixture 测试：

- `/proc/stat` CPU 差值；
- CPU Counter 回绕和无效值；
- `/proc/meminfo` 字段缺失；
- 物理/逻辑核心识别；
- 多磁盘、重复挂载和伪文件系统过滤；
- 网卡累计流量差值；
- 网卡 Counter Reset；
- TCP 状态码映射；
- IPv4/IPv6 TCP/UDP 合并；
- `/etc/os-release` 引号、转义和字段缺失；
- 超长 Hostname 和非法字符；
- Proxy 插件配置脱敏；
- 重复 Proxy/Visitor 名称被拒绝；
- `customDomains` 大小写规范化和冲突检测；
- 负数 `poolCount` 被客户端配置校验拒绝；
- 报告最大大小限制。

Reconciler 测试：

- 无变更时不调用 Store API；
- 新增、更新、删除 Proxy；
- 启用/停用；
- 端口冲突；
- 重复 command ID；
- 过期命令；
- stale generation；
- generation 跳跃；
- Store API 部分成功；
- 回滚成功/失败；
- 重启后从 Journal 恢复。

### 14.2 API 契约测试

- JSON Schema 兼容；
- 新旧客户端未知字段兼容；
- 幂等报告；
- 重复命令；
- 大响应拒绝；
- 认证过期和轮换；
- 429/5xx/Retry-After；
- 服务端时钟与客户端时钟偏差。

### 14.3 集成测试

启动：

- 一个真实 frps；
- 一个自定义 frpc；
- 一个 Mock Panel；
- 一个本地测试服务。

验证：

1. frpc 无 Proxy 时 Telemetry 仍会上报；
2. 存在 100 条 Proxy 时仍只有一个 Reporter；
3. 面板下发创建 TCP Proxy，Store 持久化且流量可通；
4. 更新 `localPort` 后配置生效；
5. 删除、禁用和重新启用均正确；
6. frpc 重启后 Store 配置恢复；
7. 面板离线时隧道不受影响；
8. 上报超时不会阻塞 Proxy Handle；
9. 部分 Store 操作失败后状态可恢复；
10. 报告和日志中没有 Token、密码、SecretKey。

### 14.4 稳定性与性能测试

- 72 小时持续运行；
- 频繁 reload/Store CRUD；
- wire protocol v2 下普通 UDP/SUDP 二进制 Codec 协商、传输和能力回退；
- frpc/frps v0.71.0 断线重连和半开 TCP Mux 场景；
- `frpc verify` 对启用/禁用 feature gates 的配置校验；
- 1,000 条 Proxy；
- 网络断开/恢复；
- DNS 故障；
- TLS 握手失败；
- 系统时间跳变；
- suspend/resume；
- 网卡重启；
- 面板返回畸形或超大 JSON；
- 使用 Go race detector 检查竞态；
- goroutine、内存和文件描述符泄漏检查。

### 14.5 兼容性测试

最低覆盖：

- 自定义 frpc（基于 Tag v0.71.0 / Commit `4a23aa1`）+ frps v0.71.0；
- wire protocol v2 的 TCP、UDP、SUDP、HTTP、HTTPS、STCP/XTCP 基础回归；
- 如产品声明支持混合版本，再额外测试 frpc v0.71.0 与目标旧 frps；未实际测试的组合不进入支持矩阵；
- Linux amd64；
- Linux arm64；
- systemd 部署；
- 常见 Ubuntu、Debian、Alpine、Rocky Linux。

如果支持 OpenWrt，需要单独确认 `/proc` 字段、musl 构建、只读文件系统、存储路径和资源预算。

## 15. 开发里程碑

### M0：技术验证与接口冻结，1～2 人日

- 固定 frp 基线为 Tag `v0.71.0` / Commit `4a23aa1`，记录 Go toolchain、依赖锁和构建产物 Hash；
- 确认 Store CRUD API 的实际路径、请求结构和错误语义；
- 验证 Store API 能完成创建、更新、删除、启停和持久化；
- 确认 Admin API 认证和 loopback 部署方式；
- 确认 Telemetry 在 `client.Service` 的挂载点；
- 确认 `clientID` 与 frps Dashboard API v2 Client 记录的关联方式；
- 在 frpc/frps v0.71.0 下启用 `transport.wireProtocol = "v2"`，验证 UDP/SUDP Codec 协商和回退；
- 验证 `frpc verify` 对 feature gates、重复名称和生成配置的行为；
- 输出 JSON Schema v1 和错误码表。

交付物：PoC、接口清单、版本兼容说明。

### M1：Linux 指标采集，3～5 人日

- Collector 接口；
- CPU、内存、Swap、负载；
- 磁盘；
- 进程和启动时间；
- 网卡累计流量和速度；
- TCP/UDP 汇总；
- OS 发行版；
- 单元测试和 fixture。

交付标准：独立命令或测试程序能稳定生成符合 Schema 的完整快照。

### M2：frpc 状态与配置采集，2～4 人日

- 读取当前 Proxy/Visitor；
- 采集 `localIP/localPort`；
- 合并配置文件和 Store 来源；
- 获取运行状态和错误摘要；
- 敏感字段过滤；
- 生成 Proxy Snapshot Hash。

交付标准：配置文件 Proxy 和 Store Proxy 均能准确展示，插件 Proxy 不泄露秘密。

### M3：注册与周期上报，3～5 人日

- Enrollment；
- 本地身份状态；
- HTTPS Reporter；
- 60 秒 Scheduler 和抖动；
- sequence/idempotency；
- 超时、退避和离线缓冲；
- 服务端 Mock 和契约测试。

交付标准：面板离线或 TLS 失败不会影响 frpc 隧道，恢复后自动继续报告。

### M4：配置对账与 Store API，5～8 人日

- Desired/Actual 模型；
- Planner；
- Admin API Client；
- Proxy/Visitor CRUD；
- generation/hash；
- Command Journal；
- 部分失败和回滚；
- 重启恢复。

交付标准：所有管理操作幂等，Store 中配置在 frpc 重启后仍存在。

### M5：面板最小服务端，5～8 人日

- Agent API；
- 注册和凭据；
- 报告接收；
- Agent/Revision/Command 数据表；
- 在线状态；
- Proxy 编辑与发布 revision；
- 应用结果和错误展示；
- 审计日志。

交付标准：从 Web/API 创建 Proxy，最多一个报告周期后在客户端生效并回报结果。

### M6：安全、性能与 Beta，5～7 人日

- 配置允许列表；
- 凭据轮换；
- 数据脱敏审计；
- 72 小时稳定性测试；
- 多 Agent 压测；
- amd64/arm64 构建；
- systemd 安装脚本；
- 灰度发布和回滚说明。

单人完整 MVP 预估约 4～6 周；如果只开发客户端模块和 Mock Panel，可先在约 2～3 周内完成可测试版本。

## 16. 验收标准

必须同时满足：

1. 一个 frpc 进程始终只有一个 Telemetry Reporter；
2. 无 Proxy 时仍能正常上报；
3. 默认每 60 秒上报需求列出的全部指标；
4. 网速按真实时间差计算，能处理重启和 Counter Reset；
5. `localIP/localPort` 能按 Proxy 正确展示；
6. 面板可下发 Proxy/Visitor CRUD 和启停；
7. 变更通过 Store API 生效并在重启后保留；
8. 相同命令重复发送不会重复产生副作用；
9. 面板不可用、上报超时或数据解析失败不影响现有隧道；
10. Admin API 不暴露公网；
11. 报告、状态文件和日志中不出现敏感凭据；
12. 失败操作有明确错误码、错误信息和可审计记录；
13. 应用配置后能报告 `desired_generation == applied_generation`；
14. 通过 race、集成、重启恢复和 72 小时稳定性测试；
15. v0.71.0 frpc/frps 在 wire protocol v2 下通过 TCP、UDP/SUDP 和重连回归；
16. 提供 Linux amd64/arm64 可复现构建及 systemd 部署文档。

## 17. 主要风险与应对

### Store/Admin API 随版本变化

应对：固定 Tag `v0.71.0` / Commit `4a23aa1`；封装 `AdminClient`；所有源码引用指向 v0.71.0 Tag；增加契约测试；升级 frp 前先建立新分支并跑完整兼容套件。

### 多步 Store 操作不是强事务

应对：使用 generation、配置 Hash、本地 Journal、操作前快照和补偿回滚；面板展示 partial failure，不能假装全部成功。

### Telemetry 影响数据面

应对：独立 goroutine、独立 HTTP Client、严格超时、禁止获取数据面关键锁、限制采集和上报资源、默认 fail-open。

### 60 秒不是严格实时

应对：产品文案称“过去 60 秒平均速度”；后续支持本地 5 秒采样或长连接上报。

### 采集值受容器和权限限制

应对：Linux 第一阶段推荐宿主机 systemd 部署；上报 `containerized/namespace_limited`；对不可用字段返回 `null` 和错误标记，不伪造 0。

### 高权限客户端插件被远程滥用

应对：客户端本地允许列表；默认禁用 `static_file`、`unix_domain_socket` 等敏感插件；服务端 RBAC 与审计；不支持任意文件和命令下发。

### 内网信息泄露

应对：传输加密、租户隔离、查看权限、日志脱敏、明确隐私告知；第一阶段不采集连接明细和进程命令行。

## 18. 实施顺序建议

最合理的落地顺序是：

1. 先固定 frp 版本并验证 Store API；
2. 完成独立 Linux Collector 和 JSON Schema；
3. 将 Telemetry Service 接入 frpc 生命周期；
4. 完成主动 HTTPS 报告与注册；
5. 只做配置版本检测，不立即做远程变更；
6. 加入 Store Reconciler 和命令 Journal；
7. 最后实现面板编辑、审计和告警；
8. 在 Beta 阶段再考虑 WebSocket、远程升级和更多平台。

这样能先证明“单进程、60 秒监控上报、不影响隧道”的基础成立，再逐步增加远程管理能力，避免同时改动数据面、控制面和面板导致问题难以定位。

## 19. 官方参考资料

- [frp v0.71.0 Release](https://github.com/fatedier/frp/releases/tag/v0.71.0)
- [frp v0.70.0 Release：Dashboard API v2 与重复名称校验](https://github.com/fatedier/frp/releases/tag/v0.70.0)
- [frp v0.70.1 Release：重连资源泄漏修复](https://github.com/fatedier/frp/releases/tag/v0.70.1)
- [frpc v0.71.0 Dynamic Proxy Management / Store](https://github.com/fatedier/frp/blob/v0.71.0/README.md#dynamic-proxy-management-store)
- [frpc v0.71.0 Client 配置与动态 reload](https://github.com/fatedier/frp/blob/v0.71.0/README.md#hot-reloading-frpc-configuration)
- [frpc v0.71.0 客户端插件接口源码](https://github.com/fatedier/frp/blob/v0.71.0/pkg/plugin/client/plugin.go)
- [frp v0.71.0 消息协议结构](https://github.com/fatedier/frp/blob/v0.71.0/pkg/msg/msg.go)
- [frp Proxy 配置参考](https://gofrp.org/en/docs/reference/proxy/)
- [frp Client Plugin 配置参考](https://gofrp.org/en/docs/reference/client-plugin/)
