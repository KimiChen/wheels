# frps 独立公开客户端 Telemetry 页面方案

> 文档状态：Design v2.0
> 更新日期：2026-08-22
> 目标基线：frp `v0.71.0`，固定 Tag `v0.71.0` / Commit `4a23aa181c1d7e28eecaa8216024ed753b9d27c8`
> 核心目标：frpc 每 60 秒上报主机和本地 Proxy 信息，由 frps 在独立端口提供免登录的公开状态网页；不调整其他 frps 功能。

## 1. 需求结论

独立公开网页需要显示以下客户端信息：

- CPU 使用率、逻辑核心数和物理核心数；
- 内存总量、可用量、已用量和使用率；
- Swap 总量、已用量和使用率；
- 各磁盘或挂载点的总容量、已用容量、可用容量和使用率；
- Load 1、5、15；
- 系统进程数；
- 系统启动时间和运行时间；
- 各网卡累计接收/发送流量；
- 各网卡过去一个采样周期的接收/发送速度；
- TCP 总数、ESTABLISHED、LISTEN、TIME_WAIT 和其他状态；
- UDP/UDP6 Socket 总数；
- 操作系统发行版、版本、内核和体系结构；
- frpc 版本、连接状态；
- 每条 Proxy 的 `localIP`、`localPort`、类型和运行状态。

这些字段不属于 frps 原生 `ClientRegistry` 的完整数据范围，因此不能只改 HTML 页面。必须增加一条独立的 Telemetry 数据链路：

```text
frpc Linux Collector
  → 每 60 秒生成完整快照
  → HTTPS POST 到 frps Telemetry Receiver
  → frps 内存 Telemetry Registry
  → 独立公开网页读取并展示
```

本方案仍不修改 frp wire protocol，不把监控数据塞进 `Login`、`Ping` 或其他 frp 控制消息。

## 2. 范围边界

### 2.1 必须实现

- frpc 每 60 秒采集并主动上报一次完整快照；
- frps 在独立端口接收经过认证的 Telemetry 报告；
- frps 在该独立端口的根路径公开展示客户端状态；
- 报告只保存在内存，frps 重启后等待客户端重新上报；
- 报告失败不能中断或阻塞 frpc 隧道；
- 页面失败不能影响 frps 控制连接或数据转发；
- 页面明确区分在线、离线、Telemetry 过期和等待首报；
- 所有容量统一使用字节传输，网页负责格式化为 KiB、MiB、GiB、TiB；
- 客户端采集失败的字段返回 `null` 或错误标记，不伪造为 0。

### 2.2 保持不变

- 原 Dashboard 端口、页面和 Basic Auth；
- `/api/*` 和 `/api/v2/*` 管理接口；
- frps 客户端认证；
- Proxy/Visitor 注册和生命周期；
- Store/Admin API；
- Server Plugin；
- Prometheus 指标；
- frp wire protocol v1/v2；
- TCP、UDP、HTTP、HTTPS、STCP、XTCP、SUDP、TCPMUX 数据转发；
- frps 原有配置字段的语义。

### 2.3 不实现

- 远程 Shell、文件管理和命令执行；
- 通过公开页面修改 Proxy；
- 公开 JSON 查询 API；
- 用户注册、套餐、计费或多租户后台；
- 历史时序数据库；
- 计费级流量账本；
- 进程名、命令行和连接远端 IP 明细；
- 自动升级 frpc/frps；
- 修改 frp wire protocol；
- 将现有 Dashboard 暴露为免登录页面。

## 3. 总体架构

```text
┌───────────────────────────────────────────────┐
│                   自定义 frpc                  │
│                                               │
│  原生 frpc Service / Proxy / Visitor          │
│                                               │
│  Telemetry Service                            │
│  ├─ Linux Collectors                          │
│  ├─ Proxy Local Endpoint Collector            │
│  ├─ 60 秒 Scheduler                           │
│  └─ HTTPS Reporter                            │
└───────────────────────┬───────────────────────┘
                        │ POST /api/v1/telemetry
                        │ Bearer Token + HTTPS
                        ▼
┌───────────────────────────────────────────────┐
│                     frps                      │
│                                               │
│  原生 ClientRegistry                         │
│  原生 Dashboard/API              保持不变     │
│                                               │
│  Public Client Info Server                    │
│  ├─ Telemetry Receiver（认证写入）             │
│  ├─ Telemetry Registry（内存）                 │
│  └─ Public HTML Page（免登录读取）              │
└───────────────────────┬───────────────────────┘
                        │ 独立网页端口
                        ▼
                    公开访问者
```

关键原则：

- frp 数据面和 Telemetry 完全解耦；
- frpc 只主动向外连接，不要求公网访问客户端；
- Telemetry 发送失败时 fail-open，现有隧道继续运行；
- frps 只保存最近一份报告，不承担历史指标存储；
- 公共页面只读，不提供任何控制入口。

## 4. 独立端口与路由

独立 Listener 同时承担公开 HTML 和经过认证的报告写入，但两者权限完全分离。

| 方法 | 路径 | 是否公开 | 用途 |
|---|---|---|---|
| GET | `/` | 是 | 客户端 Telemetry HTML 页面 |
| HEAD | `/` | 是 | 页面可用性检查 |
| POST | `/api/v1/telemetry` | 否 | frpc 上报完整 Telemetry 快照 |
| 其他 | 任意 | 否 | 返回 404 或 405 |

该端口不注册：

- `/api/clients`；
- `/api/v2/clients`；
- `/metrics`；
- `/healthz`；
- `/debug/pprof/*`；
- `/static/*`；
- Proxy/Visitor 管理接口。

`POST /api/v1/telemetry` 虽然位于公网可达 Listener，但必须持有正确凭据才能写入；它不是公开读 API。

## 5. frps 配置

建议新增：

```toml
[publicClientInfo]
addr = "127.0.0.1"
port = 7501
reportTokenFile = "/etc/frp/public-client-info-report-token"
staleAfterSeconds = 180
maxReportBytes = 262144
```

字段语义：

- `addr`：独立页面监听地址，默认 `127.0.0.1`；
- `port`：独立页面端口，默认 `0`，为 0 时完全关闭；
- `reportTokenFile`：frpc 上报使用的 Bearer Token 文件；
- `staleAfterSeconds`：超过该时间没有新报告时标记 Telemetry 过期，默认 180 秒；
- `maxReportBytes`：单份报告最大字节数，默认 256 KiB。

Token 文件要求：

- 使用至少 32 字节的安全随机值；
- 文件权限必须为 `0600`；
- frps 启动时读取，日志中不得输出；
- 正式环境不建议直接把 Token 写入 TOML；
- 后续可以扩展为每 Client ID 一个凭据，但不属于第一阶段。

直接监听公网：

```toml
[publicClientInfo]
addr = "0.0.0.0"
port = 7501
reportTokenFile = "/etc/frp/public-client-info-report-token"
staleAfterSeconds = 180
maxReportBytes = 262144
```

更推荐监听回环地址，由 Caddy/Nginx 发布 HTTPS。不能让 frpc 携带 Bearer Token 使用明文 HTTP 穿过公网。

## 6. frpc 配置

与 [`frpc.md`](frpc.md) 中的 Telemetry Service 对齐，增加一个只上报、不接收管理命令的 frps 目标：

```toml
[telemetry]
enabled = true
endpoint = "https://status.example.com/api/v1/telemetry"
intervalSeconds = 60
requestTimeoutSeconds = 10
tokenFile = "/etc/frp/public-client-info-report-token"
maxReportBytes = 262144
```

约束：

- `intervalSeconds` 第一阶段固定为 60；
- 首次启动在 frpc 成功登录 frps 后立即报告一次；
- 后续每 60 秒报告；
- 加入 0～10 秒随机抖动，避免大量客户端同时上报；
- 单次 HTTP 请求总超时 10 秒；
- 上报失败不得影响 frpc 登录、重连或 Proxy 转发；
- 401/403 不高频重试；
- 429、5xx 和网络错误采用带抖动的指数退避；
- 只保留最新完整快照，不无限积压历史报告。

## 7. 上报协议

请求：

```http
POST /api/v1/telemetry HTTP/1.1
Authorization: Bearer <report-token>
Content-Type: application/json
Idempotency-Key: <clientID>-<bootID>-<sequence>
```

建议报告：

```json
{
  "schema_version": "1.0",
  "client_id": "node-stable-uuid",
  "boot_id": "linux-boot-id",
  "sequence": 1024,
  "collected_at": 1787371200,
  "interval_seconds": 60,
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
      "used_bytes": 268435456,
      "used_percent": 6.25
    }
  },
  "disks": [
    {
      "device": "/dev/nvme0n1p2",
      "mount_point": "/",
      "filesystem": "ext4",
      "total_bytes": 536870912000,
      "used_bytes": 210453397504,
      "available_bytes": 326417514496,
      "used_percent": 39.19
    }
  ],
  "network": [
    {
      "name": "eth0",
      "rx_bytes_total": 1827364512,
      "tx_bytes_total": 918273645,
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
    "client_id": "node-stable-uuid",
    "online": true,
    "wire_protocol": "v2",
    "proxies": [
      {
        "name": "ssh",
        "type": "tcp",
        "enabled": true,
        "status": "running",
        "local_ip": "127.0.0.1",
        "local_port": 22
      }
    ]
  }
}
```

成功响应建议使用：

```http
HTTP/1.1 204 No Content
```

该端点只接收状态，不返回 Proxy 配置或管理命令。

## 8. 采集语义

### 8.1 CPU 和核心数

来源：

- `/proc/stat`；
- `/proc/cpuinfo`；
- `runtime.NumCPU()`；
- `/proc/loadavg`。

CPU 使用率按两个采样点之间的累计时间差计算：

```text
totalDelta = totalNow - totalPrevious
idleDelta  = idleNow  - idlePrevious
usage      = (totalDelta - idleDelta) / totalDelta * 100
```

首次没有基线时允许 `usage_percent = null`。

### 8.2 内存和 Swap

来源：`/proc/meminfo`。

```text
memoryUsed = MemTotal - MemAvailable
swapUsed   = SwapTotal - SwapFree
```

容量以字节上报。

### 8.3 磁盘

每个有效挂载点上报总量、已用量和可用量。默认过滤 proc、sysfs、devtmpfs、tmpfs、cgroup、overlay 等伪文件系统和重复 bind mount。

最多上报 64 个挂载点，防止异常系统生成超大页面。

### 8.4 系统负载、进程和启动时间

- Load：`/proc/loadavg`；
- 进程数：统计 `/proc/[0-9]+`；
- 启动时间：`/proc/stat` 的 `btime` 或 `sysinfo`；
- 运行时间：使用系统单调时钟或启动时间计算。

### 8.5 网卡累计流量和速度

来源：`/proc/net/dev`。

```text
rxBytesPerSecond = (rxNow - rxPrevious) / actualElapsedSeconds
txBytesPerSecond = (txNow - txPrevious) / actualElapsedSeconds
```

必须使用实际采样间隔，不能固定除以 60。

网页上的“实时速度”准确含义是“最近一个采样周期的平均速度”，不是瞬时速度。页面建议明确标注为“最近 60 秒平均 RX/TX”。

如果累计值回退，应设置 `counter_reset = true`，本次速度显示 `—`，然后以当前值建立新基线。

### 8.6 TCP/UDP 连接数

来源：

- `/proc/net/tcp`；
- `/proc/net/tcp6`；
- `/proc/net/udp`；
- `/proc/net/udp6`。

只上报汇总，不上报远端 IP、端口和所属进程。

### 8.7 操作系统发行版

来源：

- `/etc/os-release`；
- `uname`；
- `os.Hostname()`。

展示 Pretty Name、版本、内核和架构。

### 8.8 frpc localIP/localPort

`localIP`、`localPort` 不是 frpc 注册 `NewProxy` 时必然发送给 frps 的字段，必须由 frpc Telemetry Service 从当前合并后的配置和 Store 状态读取后上报。

对于客户端插件 Proxy：

- `localIP`、`localPort` 可以为空；
- 只显示允许公开的插件类型；
- 不显示插件密码、证书、SecretKey、文件路径和其他敏感选项。

## 9. frps Telemetry Registry

frps 新增一个与原生 `ClientRegistry` 独立的内存 Registry：

```text
map[clientID]TelemetryRecord
```

每条记录保存：

- 最近完整报告；
- frps 接收时间；
- `boot_id`；
- 最近 `sequence`；
- 最近 Idempotency Key；
- 校验状态。

接收规则：

1. 验证 HTTPS/Bearer Token；
2. 限制请求体最大 256 KiB；
3. 验证 JSON 和 `schema_version`；
4. 验证 `client_id` 非空且长度合规；
5. 确认该 Client ID 存在于原生 `ClientRegistry`；
6. 检查数组数量、端口范围、数值范围和字符串长度；
7. 按 `clientID + bootID + sequence` 幂等处理；
8. 原子替换该客户端的最近报告；
9. 使用 frps 接收时间作为页面新鲜度依据。

不依赖客户端时间判断在线或过期，避免客户端时钟错误。

frps 重启后 Registry 为空，页面显示“等待首报”；客户端最多约 60 秒重新填充。

## 10. 状态合并

页面合并两个数据源：

- 原生 `ClientRegistry`：frp 控制连接是否在线；
- Telemetry Registry：主机指标和本地 Proxy 信息。

状态规则：

| frp 状态 | Telemetry 状态 | 页面状态 |
|---|---|---|
| 在线 | 180 秒内有报告 | Online |
| 在线 | 尚未收到报告 | Waiting for telemetry |
| 在线 | 报告超过 180 秒 | Degraded / Telemetry stale |
| 离线 | 有历史报告 | Offline，展示最后报告时间 |
| 离线 | 无报告 | Offline / No telemetry |

页面必须同时展示：

- frp 在线状态；
- Telemetry 最近接收时间；
- 数据是否过期。

不能用客户端报告中的 `frpc.online` 代替服务端原生连接状态。

## 11. 公开页面布局

建议顶部显示：

- 客户端总数；
- 在线数；
- 离线数；
- Telemetry 过期数；
- 页面更新时间。

每个客户端使用独立卡片：

```text
┌──────────────────────────────────────────────────────────┐
│ home-server     Online       Telemetry: 12 秒前           │
│ Ubuntu 24.04 · Linux 6.8 · amd64 · frpc 0.71.0           │
├───────────────┬───────────────┬───────────────┬──────────┤
│ CPU 13.4%     │ 8C / 4C       │ Load .42/.51  │ 186 proc │
│ RAM 6.2/16GiB │ Swap .25/4GiB │ Uptime 4d 7h  │ TCP 93   │
├──────────────────────────────────────────────────────────┤
│ Disks                                                    │
│ /      ext4       210 / 500 GiB      39.2%               │
├──────────────────────────────────────────────────────────┤
│ Network                                                  │
│ eth0   RX 1.70 GiB · 17.8 KiB/s   TX 875 MiB · 7.1 KiB/s │
├──────────────────────────────────────────────────────────┤
│ Local Proxies                                            │
│ ssh    tcp    127.0.0.1:22    running                    │
└──────────────────────────────────────────────────────────┘
```

展示要求：

- 默认展开资源摘要；
- 磁盘、网卡和 Proxy 较多时使用折叠区域；
- 桌面端使用表格，移动端可横向滚动或转为卡片；
- 页面每 30 秒刷新；
- 不使用外部 JavaScript、字体或 CDN；
- 所有输出使用 Go `html/template` 自动转义；
- 空值显示 `—`；
- 百分比显示一位或两位小数；
- 容量自动选择 IEC 单位；
- 时间统一显示 UTC 或明确标注的服务器时区。

## 12. 隐私与安全

这是公开网页，新增字段会公开较多主机资产信息，特别是：

- 操作系统及内核版本；
- CPU/内存/磁盘容量；
- 主机名；
- 内网 `localIP`、`localPort`；
- 本地服务类型和运行状态。

这些数据可以帮助攻击者识别资产和内部网络结构。既然需求明确要求公开，实施前必须由部署者确认接受该风险。

最低安全要求：

- 正式环境只允许 HTTPS 上报；
- Bearer Token 不写日志；
- Token 使用常量时间比较；
- 报告接口限制 Body、Header、超时和每客户端速率；
- 只有已连接且具有稳定 Client ID 的 frpc 才能写入；
- 页面不得显示认证信息、User、Run ID、来源公网 IP；
- 不显示进程名、命令行和远端连接明细；
- 不显示 STCP/XTCP/SUDP SecretKey；
- 不显示 HTTP 密码、证书私钥和插件敏感字段；
- 字符串过滤控制字符并设置长度上限；
- HTML 输出必须转义；
- 页面响应设置 `Cache-Control: no-store`。

推荐安全响应头：

```http
Cache-Control: no-store
Content-Security-Policy: default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'
Referrer-Policy: no-referrer
X-Content-Type-Options: nosniff
X-Frame-Options: DENY
```

## 13. 故障与资源限制

### frpc 上报失败

- 隧道继续运行；
- 不重启 frpc；
- 本地记录最近错误摘要；
- 使用指数退避；
- 恢复后优先发送最新快照，不补传大量过期数据。

### frps Receiver 异常

- 不影响 ClientRegistry；
- 不影响 Proxy/Visitor；
- 不持有数据转发关键锁；
- 报告解析和页面渲染使用 Registry 快照。

### 页面无人访问

- 不主动渲染 HTML；
- 仅保留每客户端最近一份报告；
- 不保存 60 秒历史序列。

### 数量限制

建议：

- 单报告最大 256 KiB；
- 磁盘最多 64 个；
- 网卡最多 64 个；
- Proxy 最多 1000 条；
- 单字符串最大 128～256 字符；
- 请求 Header 最大 16 KiB；
- 报告请求超时 10 秒；
- 相同 Client ID 最快 15 秒接受一次报告。

## 14. 代码组织建议

frps：

```text
server/publicclientinfo/
├── server.go          # 独立 Listener 生命周期
├── handler.go         # GET 页面和 POST 报告路由
├── auth.go            # Token 校验
├── model.go           # Telemetry Schema
├── registry.go        # 最近报告内存存储
├── validation.go      # 报告字段校验和限制
├── view.go            # 原生 Client + Telemetry 合并
├── template.go        # HTML 模板
└── *_test.go
```

frpc 与 [`frpc.md`](frpc.md) 中的目录保持一致：

```text
client/telemetry/
├── service.go
├── scheduler.go
├── collector/
│   ├── cpu_linux.go
│   ├── memory_linux.go
│   ├── disk_linux.go
│   ├── network_linux.go
│   ├── connection_linux.go
│   ├── process_linux.go
│   ├── os_linux.go
│   └── frpc.go
└── reporter/
    ├── client.go
    └── retry.go
```

对现有代码的集成点限制为：

- `pkg/config/v1/server.go`：公开页面和 Receiver 配置；
- `pkg/config/v1/client.go`：Telemetry Reporter 配置；
- `server.Service`：创建、启动、关闭公开信息服务；
- `client.Service`：创建、启动、关闭单例 Telemetry Service；
- 不修改 Proxy 数据转发路径；
- 不修改 `pkg/msg` 和 wire protocol。

## 15. 测试计划

### 15.1 frpc Collector

- CPU 差值和首次无基线；
- CPU Counter 回退；
- 逻辑/物理核心数；
- MemAvailable 缺失；
- Swap 为 0；
- 多磁盘和伪文件系统过滤；
- 网卡累计流量差值；
- 网卡重启和 Counter Reset；
- TCP 状态分类；
- IPv4/IPv6 TCP/UDP 合并；
- `/etc/os-release` 转义和字段缺失；
- Proxy `localIP/localPort`；
- 插件 Proxy 敏感字段过滤；
- 报告最大大小。

### 15.2 frps Receiver

- Token 正确时接受；
- Token 缺失或错误时返回 401；
- 超大报告返回 413；
- 非法 JSON 返回 400；
- 未知 Client ID 被拒绝；
- 重复 sequence 幂等；
- 新 boot ID 重置 sequence 基线；
- 数组超限被拒绝；
- 端口和百分比越界被拒绝；
- frps 接收时间正确记录；
- 并发上报通过 race detector。

### 15.3 公开页面

- 无客户端时正常显示；
- 等待首报状态；
- Online、Offline、Stale 合并正确；
- CPU、内存、Swap、磁盘、负载正确格式化；
- 累计流量和速度单位正确；
- TCP/UDP 汇总正确；
- OS 发行版正确；
- `localIP/localPort` 正确；
- 恶意 Hostname、网卡名和挂载点被 HTML 转义；
- 页面不泄露 Token、User、Run ID 和公网来源 IP；
- `/api/clients` 和管理路径返回 404；
- POST 根路径返回 405；
- 安全响应头完整。

### 15.4 集成测试

1. 启动 frps 独立公开页面端口；
2. 启动一个自定义 frpc；
3. frpc 登录成功后立即报告；
4. 页面在一次刷新内显示全部指标；
5. 修改 CPU/网卡 fixture 后页面更新；
6. frpc 停止后页面显示 Offline；
7. 停止 Telemetry 但保持 frp 控制连接，180 秒后显示 Stale；
8. Receiver 返回 500 时隧道仍可用；
9. frps 重启后页面先显示等待首报，60 秒内恢复；
10. 报告和日志中没有 Token 或 Proxy Secret。

## 16. 验收标准

必须同时满足：

1. frpc 默认每 60 秒报告一次完整快照；
2. 报告失败不影响任何现有隧道；
3. frps 独立端口根路径公开展示全部指定指标；
4. CPU、网速使用实际采样时间差计算；
5. 页面把网速标注为最近采样周期平均速度；
6. 内存、Swap、磁盘容量和使用率计算一致；
7. TCP/UDP 只公开汇总；
8. 操作系统详细发行版、内核和架构可见；
9. 每条 Proxy 的 `localIP/localPort` 可见；
10. Telemetry 报告写入必须认证；
11. 页面查询不需要认证；
12. 原 Dashboard 和所有管理 API 行为不变；
13. 不修改 frp wire protocol；
14. frps 只保留最近快照，重启后 60 秒内恢复；
15. 页面明确显示 Online、Offline、Waiting 和 Stale；
16. 通过单元测试、Race、集成测试和 72 小时稳定性测试。

## 17. 实施顺序

1. 冻结 Telemetry JSON Schema v1；
2. 在 frps 实现内存 Registry、认证 Receiver 和校验；
3. 在 frpc 完成 Linux Collectors；
4. 完成 60 秒 Scheduler 和 HTTPS Reporter；
5. 在 frps 合并 ClientRegistry 与 Telemetry Registry；
6. 完成公开页面资源卡片、磁盘、网卡和 Proxy 展示；
7. 完成隐私过滤、限制和安全响应头；
8. 执行集成、Race、重启和稳定性测试；
9. 最后再制作新的 v2 补丁和部署说明。

## 18. 当前交付状态

此前的 `frps-public-client-info-v0.71.0.patch` 只展示 frps 原生客户端字段，不包含本方案新增的 CPU、内存、磁盘、网速、连接数和 `localIP/localPort`，因此不能作为 v2 需求的最终实现。

本文件已经更新为新的 v2 设计基线。后续代码实现应重新生成补丁，例如：

```text
frps-public-client-telemetry-v0.71.0.patch
```

在新补丁完成全部 Collector、Receiver、页面和测试之前，不应把旧补丁描述为满足本需求。
