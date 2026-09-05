# sing-box-plus 实施计划

> 目标上游：[`SagerNet/sing-box`](https://github.com/SagerNet/sing-box)
>
> 实施基线：[`v1.14.0`](https://github.com/SagerNet/sing-box/releases/tag/v1.14.0) / `0b899587`
> （stable 轨道）
>
> 参考实现：本仓库 `shadowsocks-rust-plus`（可结算契约的既有落地，其 v1 快照 schema 与结算
> 参考模型在本项目中复用）
>
> 源码核对：2026-09-05。下文未标版本的 `路径:行号` 均指
> `b5ebaa1fc0f2b94256180b95468e73ef53caa27d`（`v1.13.19`，与 1.14 在该处语义一致）；
> 1.14 的差异单独标注。下次核对触发：上游发布新 minor，或本计划进入新里程碑。

## 1. 目标与范围

给 sing-box 增加可用于多用户配额和结算的流量统计：认证后的稳定用户归属、
TCP/UDP × 上下行四个累计值、明确的重启边界、可幂等采集，以及受控的本机快照接口。
能力对标本仓库已实现的 `shadowsocks-rust-plus`。

**交付范围**：一个固定上游版本的 hardened overlay，包含数据面身份归属、四向累计计数、
配置校验与失败关闭、本机只读 UDS 导出，以及可复现构建、契约测试与运维文档。

**明确不做**：订阅生成、用户与套餐管理、账单存储、管理后台、配置分发、硬配额、限速、
实时断开。这些属于下游集成方，本项目只提供 §5 的采集与结算契约以及一份参考 collector。
也不承诺“进程崩溃也不丢一个字节”——尾账按未闭合窗口审计，与 `shadowsocks-rust-plus` 一致。
宿主形态只覆盖 `sing-box run`，不支持 `daemon`、libbox 与 1.14 的 `boxdd`。

**验收定义**：在钉定的上游版本上，选中的 inbound 对所有允许流量都有可验证的非空计费身份；
四向字节 oracle 误差为 0；快照接口通过 §8 的故障矩阵；下游按 §5 差分入账时不出现漏计、
重复计费或静默降级。

本目录当前只有本计划书。第一步（里程碑 1）产出的骨架为：

```text
sing-box-plus/
├── README.md
├── upstream.lock                 # 钉死 repository / tag / commit / tree sha256 / go 最低版本
├── .env.example                  # UPSTREAM_REPOSITORY、GOMODCACHE、SING_BOX_BUILD_TAGS 占位
├── scripts/prepare-source.sh     # 按精确 commit 取源码并校验
├── scripts/verify.sh             # go vet / go test -race / lint / 敏感信息扫描
└── docs/UPSTREAM_BASELINE.md     # 基线、编译验证记录与升级规则
```

在骨架落地并冻结基线之前不写 tracker 代码——§9.2 的升级规则要求所有实现都绑定到一个已记录的
上游提交。

## 2. 前提与约束

### 2.1 可以依赖的上游能力

- **认证后的用户身份已经统一投影**。VLESS 服务验证 UUID/flow/command 后，handler 从认证 context
  取出用户索引并把非空 `users[].name` 写入 `metadata.User`（`protocol/vless/inbound.go:167-205`；
  1.14 中该方法改名为 `NewConnection`，见 `v1.14.0:protocol/vless/inbound.go:150`，逻辑不变）。
  REALITY 握手与 Vision 解码都不会抹掉该身份。
- **统一的 tracker 挂载点已经导出**。`adapter.Router.AppendTracker` 让外部在路由选出 outbound 之后
  包装连接（`route/route.go:152`、`:278`），上游自己就用它挂 clash 与 v2ray tracker
  （`box.go:354`、`:364`；1.14 移到 `v1.14.0:box.go:246-251`、`:438`）。因此**不需要逐协议插桩**。
- **计数原语与正确的计数点已经存在**。sing 的 `bufio.Copy` 会解包 counter wrapper 并在目标写成功后
  调用双方计数 callback，Linux splice 路径也按成功传输字节调用 counter。
- **进程级 registry 可注入**。`globalCtx` 已持有 service registry（`cmd/sing-box/cmd.go:70`），
  `box.New` 在 ctx 已有 registry 时原样复用（`box.go:101`），因此跨 Box 注入无需改上游签名。
- **VLESS 具备热用户更新的底层能力**（`sing-vmess vless/service.go:40 UpdateUsers`），
  为将来可能的用户管理接口留有余地（§11 D5）。

### 2.2 不能依赖的部分

内置 Experimental V2Ray StatsService 可以做观测验证，但不能承担结算：

- 只有 uplink/downlink 两项，TCP、UDP、XUDP 合并；
- key 不含 inbound tag，同名用户跨 inbound 会合并；`stats.users` 是静态白名单，新用户不自动加入；
- counter 在该用户**首次被路由**时懒创建（`experimental/v2rayapi/stats.go:214-222`），与是否已传输
  字节无关，`not found` 只意味着“本进程启动后从未路由过该用户”，采集端必须按 0 处理；
- `GetStats/QueryStats(reset=true)` 用 `Swap(0)`（`stats.go:121-218`），存在“已清零但响应丢失”的
  永久漏账窗口；多 counter 也不是事务快照，map 顺序不稳定；
- 数值是 `atomic.Int64`，可回绕为负；无运行周期、代次与持久化；
- gRPC listener 使用 insecure credentials，无认证无 TLS；
- 线路上的服务名被硬编码覆写为 `v2ray.core.app.stats.command.StatsService`（`stats.go:21`），
  不是 proto 声明的包名，用后者调用会得到 `unknown service`；
- 默认不在构建产物中，必须追加 `with_v2ray_api`（上游 `release/DEFAULT_BUILD_TAGS` 不含该 tag，
  官方与 Homebrew 二进制都不可用）。

其余接口也都不能替代账本：

| 接口 | 用户维度 | TCP/UDP 字节 | 生命周期/持久化 | 判断 |
| --- | --- | --- | --- | --- |
| V2Ray StatsService | 有，静态白名单 | 合并 | 无 | 可观测，不可结算 |
| Clash API | 无用户累计 | 仅连接 network | 关闭详情有界 | 不能可靠反推 |
| clashapi trafficontrol | 每连接内部有 user | 每连接可辨别 | 较旧关闭连接只进全局累计 | 不能恢复完整用户累计 |
| 1.14 API service | 每连接有 user/network | 可从事件区分 | 事件可丢、历史有界 | UI/诊断，不是账本 |
| SSM API | 有，仅 managed Shadowsocks | 合并 | 可选 JSON cache | 覆盖不到 VLESS |
| Prometheus `/metrics` | 无通用用户流量 exporter | — | — | 不能依赖 |

包名随版本变化：1.13.19 是 `experimental/clashapi/trafficontrol`（上游拼写少一个 c），1.14 起移到
`common/trafficcontrol`。1.14 的 API service 不是第三套账本，而是同一连接账本经 daemon gRPC
`SubscribeConnections` 的导出面。

SSM API 只作设计参考，不复用其存储语义：`cache_path` 每分钟保存但崩溃丢最近一分钟，
`clear=true` 与 V2Ray `reset=true` 同样有漏账窗口，删除用户即丢 counter map，
cache 与凭据同文件且按 `0644` 创建（`service/ssmapi/cache.go:80`），直接覆盖而非事务提交。
更重要的是 SSM tracker 在 `RouteConnection` **之前**包装
（`protocol/shadowsocks/inbound_multi.go:177-181`、`:203-206`），与 router tracker 口径不同，
两者不能互作字节 oracle。

### 2.3 结构性限制

以下限制由上游结构决定，设计必须绕开或显式声明：

- **重启没有平滑窗口**。SIGHUP 循环是先取消再关旧、然后才建新（`cmd/sing-box/cmd_run.go:188`
  `cancel()` → `:191` `Close()` → `:174` `create()`），`Box.Close()` 关闭 connection manager 后
  `CloseAll()` 遍历强杀全部在途连接（`route/conn.go:52-69`）；仓库内没有 SO_REUSEPORT 或监听 fd
  传递，端口必然有短暂无监听窗口。上游没有任何排空语义。
- **StatsService 属于 Box，重载即丢未采集增量**，见 `box.go:496-535` 与上游 issue
  [#4059](https://github.com/SagerNet/sing-box/issues/4059)（2026-04-19 开启，同日以 not planned 关闭）。
  由此推出一条对本项目同样成立的纪律：**任何“改用户就重写配置并重载”的方案都会清零内存计数**，
  用户增删因此不能与计量实现耦合（§4.3、§11 D5）。
- **部分数据面对 tracker 不可见**：
  - `hijack-dns` 与以 `reject` 结束的规则动作在 `route/route.go:126-137`（TCP）、`:256-263`（UDP）
    处理并 return，早于 `:152` / `:278` 的 tracker 循环；
  - REALITY 握手校验失败时的伪装中继在 utls 内部完成双向 `io.Copy`
    （`metacubex/utls reality.go:326,424,542-547`），不经 inbound/router；sing-box 未设
    `LimitFallback`，该中继不限速；
  - ShadowTLS 的握手/校验失败流量整条中继到 handshake 服务器，同样不经 router。
- **1.14 的 tracker 接口有三个方法**：新增 `RoutedFlow(...) tun.FlowTracker`
  （`v1.14.0:adapter/router.go:104-108`），1.13.19 只有两个（`adapter/router.go:33-36`）。
  该路径只在 TUN/L3 形态可达、口径是 IP 包全长且 `metadata.User` 恒空。

### 2.4 可计费的 inbound 范围

统计模式按 inbound 类型白名单启用。“`metadata.User` 非空即可计费”不是有效兜底——
Shadowsocks relay 就是反例。

| inbound | 身份可用性 | 处置 |
| --- | --- | --- |
| VLESS + REALITY/Vision | 认证后写入 `metadata.User` | **首期支持**；REALITY 伪装中继除外（§2.3） |
| VMess、Trojan | 具名用户可传播 | 可支持，需集成测试；fallback 路径单独确认 |
| Shadowsocks multi | 具名用户可传播 | 可支持；但 SSM tracker 在 router 之前包装，会多计 sniff 预读与随后被 reject/block 的字节，**不能与 router tracker 互作 oracle**，两者不可同时启用 |
| Shadowsocks relay | `metadata.User` 是 `destinations[].name`（`inbound_relay.go:130`） | 是中继目的地不是终端用户，**不可按用户计费** |
| Hysteria/Hysteria2、TUIC | 协议层有用户概念 | 可支持，需验证 QUIC stream/datagram 分类 |
| Naive、AnyTLS、HTTP/SOCKS/Mixed 认证 | 有认证用户名的路径可传播 | 可支持，逐协议确认匿名与 fallback |
| ShadowTLS | `users[].name` 只用于握手校验 | `InboundDetour` 跳转发生在 tracker 之前（`route/route.go:62-78`，tracker 循环在 `:152`），User 被 detour 的 `UpstreamMetadata` 丢弃；计费身份只能取 detour 末端的 SS 用户（单用户 SS 则为空，应拒绝） |
| TUN、redirect、tproxy、direct | 无认证用户 | 不支持按用户计费 |

通用规则：**带 `InboundDetour` 的 inbound 一律以 detour 末端 inbound 为计费身份**。
匿名、空名、认证 fallback 或 tracker 绕过不得默认为“unknown 后继续转发”；
若会影响收费，启动或连接必须失败关闭。

## 3. 术语与字段

| 概念 | sing-box | `shadowsocks-rust-plus` | 本项目 |
| --- | --- | --- | --- |
| 计费身份名 | `users[].name` → `metadata.User` | `users[].name` / `identity_name` | 快照字段 `name`；正文称 billing name |
| 入站标识 | inbound `tag` | `server_id` | `server_id` := inbound tag |
| 节点标识 | — | `node_id` | `node_id`，配置项，需在部署内全局唯一 |
| 运行周期 | — | `runtime_id`（32 位小写 hex，进程级随机） | 同左 |
| 快照序号 | — | `sequence`，每次 `/v1/snapshot` 严格递增 | 同左 |
| 代次 | — | `generation`，同名重激活复用、不递增 | 同左，固定输出 `1`，但不得从采集键中省略 |

计费身份名、`node_id`、`server_id` 均为非空、最多 128 字节、每字节为 ASCII 可显示非空白字符；
`server_id` 在节点内唯一，billing name 在 server 内唯一。

## 4. 设计

### 4.1 统一 tracker

新增一个 build-tag gated 的用户统计 tracker，挂在认证与路由之后：

```text
协议握手与用户认证
  -> metadata { inbound, network, user }
  -> 路由与 outbound 选择
  -> UserStatsTracker
       RoutedConnection       -> tcp_uplink / tcp_downlink
       RoutedPacketConnection -> udp_uplink / udp_downlink
       RoutedFlow             -> 固定返回 nil，不参与计费（见 §2.3）
  -> 通用 copy / packet copy / splice
  -> 进程级 registry
  -> 本机只读累计快照
  -> 外部 collector 差分与幂等入账
```

逻辑 key 与记录：

```go
type IdentityKey struct {
    ServerID    string // inbound tag
    Generation  uint64 // 固定为 1，见 §4.3
    BillingName string
}

type UserTraffic struct {
    TCPUplinkBytes   SaturatingUint64
    TCPDownlinkBytes SaturatingUint64
    UDPUplinkBytes   SaturatingUint64
    UDPDownlinkBytes SaturatingUint64
}
```

实现不变量：

- 认证凭据（UUID、密码、PSK）只用于 auth table，不得进入统计 key、快照或普通日志；
- 数据面每条连接认证成功后只持有对应 record/counter 指针，不得每次 I/O 查用户表或加全局锁；
- 计数使用饱和 `u64`，溢出经 `health` 报告，禁止回绕；
- **counter 必须可被 unwrap**：sing 的 `bufio.Copy` 只有能从连接链顶端解包出 counter 时才在
  “目标写成功后”计数，否则回落为“源读成功”计数。自研 wrapper 若包在 `CounterConn` 之外，
  必须实现 `Upstream()` 与 `Reader/WriterReplaceable()`（可参照 clashapi `tracker.go` 的做法），
  否则口径会静默退化；§8 有对应断言测试；
- 与 trafficcontrol / StatsService 的挂载顺序必须显式确定，避免双层 CounterConn 叠加；
  统计模式下应禁止同时启用会重复包装同一连接的其他 tracker。

### 4.2 计数口径

采用应用 payload 口径，与 `shadowsocks-rust-plus` 一致：

- TCP uplink：协议解码后的用户 payload 被通用转发边界成功写入目标侧的字节；
- TCP downlink：目标侧 payload 被代理编码 writer 接受、准备回传给用户的逻辑字节；
- UDP uplink/downlink：完整逻辑数据报成功交给另一侧后累计 payload 长度；
- 不统计外层 REALITY/TLS、VLESS/VMess/Trojan/SS header、Vision padding、mux/XUDP/UoT framing、
  TCP/IP header 或重传；
- XUDP、UDP-over-TCP 和 mux 内的逻辑 UDP 归入 UDP，不因底层 carrier 是 TCP 而归入 TCP。

可复用的上游计数路径：

- [TCP counter wrapper](https://github.com/SagerNet/sing/blob/7c349dacf402256d3a7029746073b05d2ead584a/common/bufio/counter_conn.go)
- [packet counter wrapper](https://github.com/SagerNet/sing/blob/7c349dacf402256d3a7029746073b05d2ead584a/common/bufio/counter_packet_conn.go)
- [通用 copy](https://github.com/SagerNet/sing/blob/7c349dacf402256d3a7029746073b05d2ead584a/common/bufio/copy.go)
- [Linux splice](https://github.com/SagerNet/sing/blob/7c349dacf402256d3a7029746073b05d2ead584a/common/bufio/splice_linux.go)

两点已核实的口径细节：

- **Vision 不进入 splice**。`VisionConn` 不实现 `Reader/WriterReplaceable`，也不暴露 `syscall.Conn`，
  sing 的 unwrap 停在该层，只走用户态 direct 读写。因此对账项写作
  “buffered → direct（用户态直读 netConn）切换”，splice 对账针对无 `flow` 的 VLESS、
  socks/http/mixed 与明文 TCP 等可解包到 `syscall.Conn` 的入口。
- **Vision padding 不计入**。padding 在 `VisionConn.Read/Write` 内剥离，counter 在其外层。

实现前需完成字节对账矩阵：Vision buffered/direct 切换与 early data、mux、XUDP/UoT、DNS hijack、
特殊 outbound 直接读写、UDP batch、连接取消与 half-close、重启期间仍存活的长连接。任何绕过标准
router tracker 的 handler 都必须明确选择“计数、拒绝或声明不支持”，不能静默转发但漏计。

计费用 inbound 不得配置 `hijack-dns` 或以 `reject` 结束的规则动作（§2.3），配置校验须拒绝。

### 4.3 身份生命周期

同一 runtime 内，inbound tag 与 billing name 构成**稳定计费身份**：

1. 删除或停用只把 `active` 切为 `false`，记录保留在快照中；
2. 同名重建复用原 generation 与原计数器，并把 `active` 切回 `true`；
3. 凭据轮换不改变 lineage；
4. `generation` 作为 schema 保留维度固定输出 `1`，但不得从采集键中省略；
5. tombstone 数达到 `max_identities` 时失败关闭或标记 unhealthy，不得丢弃 lineage；
6. 同一 runtime 内不得把已用名称重分配给不同计费用户；需要改变归属时使用新名称，
   或通过受控重启进入新 runtime。

用户集合的变更（增、删、停用、凭据轮换）在 v1 中通过修改配置并受控重启生效。由于重启会产生新
`runtime_id` 并清零内存计数，变更必须走 §5.3 的计划重启流程；这也是 §2.3 那条纪律的直接后果——
不要为了“热改用户”把计量实现与用户管理耦合。是否提供独立的热更新接口见 §11 D5。

### 4.4 运行周期与重启语义

进程启动生成 `runtime_id`，创建唯一 registry 与 exporter；同一进程内累计值单调不减；
进程重启即新 `runtime_id`、计数从零开始。采集端据此识别周期边界（§5）。

上游重启会强制关闭全部在途连接（§2.3）。由于计数按每次 copy 迭代增量累加，**强杀不丢已计字节**，
但会打断用户连接，且已计而未采集的增量会随进程消失——这正是 §5.3 的计划重启流程要消除的窗口。

**跨 Box 的进程级 registry 是可选项**，仅在需要 `kill -HUP` 热重载且要求累计值不清零的形态下才必需。
若要做：

1. 每次加载 Box 时把同一 registry 注入新的 UserStatsTracker；
2. 若要排空，必须在 `run()` 与 `Box.Close()` 之间插入 drain 阶段（停止 accept → 等待或超时 → 再
   Close）并接管 `FatalStopTimeout` 看门狗，这是新增改动而非配置策略；
3. 端口仍有短暂无监听窗口；若要求端口不中断，须单列“引入 SO_REUSEPORT 或 listener fd 传递”
   为独立工作项；
4. registry 全进程共享，`MustRegister` 会覆盖旧 Box 的同类服务，代次必须由自有 registry 维护；
5. 只覆盖 `sing-box run` 宿主。

纯内存 registry 无法恢复进程崩溃前尚未采集的尾账。若业务要求“崩溃也不丢一个字节”，
需另加 WAL 或持久计量数据面，复杂度显著上升；不得把高频轮询描述成严格保证。

### 4.5 快照接口契约

**采用 `shadowsocks-rust-plus` v1 的既有形状**（其 `docs/API.md`），使
`tests/http_unix.py`、`scripts/user-stats-client.py`、`tests/settlement_model.py` 与 mock collector
可直接作为本项目的契约测试与参考采集器。sing-box 特有信息以可选附加字段追加（现有校验器忽略
未知键），不改动 `health` 对象的字段名。

```json
{
  "schema_version": 1,
  "node_id": "node-example-01",
  "runtime_id": "0123456789abcdef0123456789abcdef",
  "started_at_unix_ms": 1787587200000,
  "sequence": 42,
  "health": { "counter_overflow": false, "sequence_overflow": false },
  "servers": [
    {
      "server_id": "vless-entry-01",
      "listen": "0.0.0.0:8443",
      "generation": 1,
      "active": true,
      "inbound_type": "vless",
      "tcp_sessions": 3,
      "udp_sessions": 1,
      "users": [
        {
          "identity_kind": "user",
          "name": "u_example_01",
          "generation": 1,
          "active": true,
          "tcp_uplink_bytes": 0,
          "tcp_downlink_bytes": 0,
          "udp_uplink_bytes": 0,
          "udp_downlink_bytes": 0
        }
      ]
    }
  ]
}
```

格式与排序：`runtime_id` 为 32 位小写 hex；`started_at_unix_ms`、`sequence` 为正整数，同一
`(node_id, runtime_id)` 内 `started_at_unix_ms` 固定；服务按 `server_id` 再按 `generation` 排序，
用户按 `name` 再按 `generation` 排序。`inbound_type`、`tcp_sessions`、`udp_sessions` 是 sing-box 的
附加字段，`*_sessions` 是**当前活跃数、不参与结算**，供计划重启时判断是否已排空（§5.3）。

传输层：HTTP/1.1-over-Unix-stream，每连接单请求单响应，禁 keep-alive / query / body；
固定两条路由——`GET /v1/snapshot`（被接受时即推进 `sequence`）与 `GET /healthz`（200/503，
不推进 `sequence`）。错误一律返回固定 `{schema_version, error:{code}}` 对象，错误码表与 v1 一致
（400/404/405/408/413/429/500/505）。

安全与资源：非破坏性累计快照；稳定排序；请求/响应大小、身份数、并发数、读写超时均有上限；
socket 默认 `0600`（可受控 `0660`），绑定前检查父目录、符号链接、旧 socket 与 inode 替换。
单个畸形、超时或超限的请求只影响该连接，不影响代理转发。
Unix socket 不得直接映射为公网监听，远程读取须经节点上的独立反向代理提供 HTTPS/mTLS/来源限制
与审计，且该代理不得缓存快照。

### 4.6 配置与失败关闭

统计是硬依赖，不是可选旁路：

1. 顶层 `user_stats` 配置对未知字段失败关闭，不静默回落默认值；
2. 启用统计时，每个被统计的 inbound 必须属于 §2.4 的白名单类型、有唯一 `server_id`、
   至少一个具名用户，且用户名在 server 内唯一；任一不满足则启动失败，不以部分覆盖或零归属模式运行；
3. 未编译统计 build tag 却出现该配置、或在非 Unix 平台使用，都必须明确报错；
4. exporter 的父目录、lockfile、遗留 socket 或 bind 检查失败时进程启动失败；
5. 启动后 exporter 与数据面同受监督：exporter 任务意外退出、panic 或连续 `accept()` 失败时，
   整个进程以失败退出，避免“代理仍在转发但统计已消失”；
6. 未配置 `user_stats` 时不创建 registry、exporter 或任何附加包装，保持上游快路径与线协议不变。

进程失败退出由 systemd `Restart=on-failure` 重启；重启产生新 `runtime_id`，
采集端按 §5 的周期规则处理，并对连续重启告警——不得通过放宽目录权限或删除活动 socket 绕过检查。

### 4.7 overlay 形态

**默认方案 A：零补丁 wrapper。** `adapter.Router.AppendTracker`、`box.New`、`Box.Router()` 与
`include.Context` 都已导出，因此一个独立 Go module（`sing-box-plus/cmd/sing-box-plus`，`go.mod` 中
`require` 钉死版本的 sing-box）在自有 main 里复刻 `cmd_run` 循环，并在 `Start()` 之前调用
`instance.Router().AppendTracker(tracker)` 即可，无需修改上游源码；registry 与 exporter 由该 main
持有，天然满足 §4.4 的进程级要求。

**方案 B：patch series。** 仅当必须改上游内部路径（vless/vision、splice、`option` schema、
需要在 tracker 之前失败关闭的位置）时使用，按 `patches/series` 顺序重放，无法零 fuzz 应用即失败。

选择标准：默认 A；A 覆盖不到的失败关闭点再以最小 patch 补，两者可共存。结论在里程碑 1 写入
`upstream.lock` 与 `docs/UPSTREAM_BASELINE.md`。

无论哪种形态，产出的二进制都应保持 `run` / `check` / `format` / `version` 的 argv 与退出码与上游
兼容，使既有部署与编排工具（包括发布前的 `check -c` 门禁）可以原样沿用。

## 5. 采集与结算契约

exporter 只输出当前进程生命周期内的累计值，不持久化账单，也不决定新运行周期的首快照是否入账。
下游集成方按本节实现差分与幂等落库；本项目提供参考 collector 与结算模型（§6 交付物）。

### 5.1 基线键与幂等

保存 baseline 的完整键：

```text
node_id + server_id + server_generation + name + identity_generation + runtime_id
```

`generation` 在 v1 中恒为 1，但不得省略，以保持 schema 与未来兼容性。幂等批次 ID 必须包含快照
`sequence` 与本次增量。差分规则：

- 同一 `(node_id, runtime_id)` 内 `started_at_unix_ms` 必须恒定，变化即视为未知 runtime；
- `sequence` ≤ 已处理值 → 丢弃整份快照且不推进基线（重复或乱序响应）；
- `sequence` 前进但累计值倒退 → 失败关闭并告警，不得猜测并继续收费；
- `health` 任一项为真、`schema_version` 不匹配、`Content-Length` 不符或 JSON 截断 → 拒绝入账；
- HTTP 429 与连接被直接关闭视为**可重试且不得入账**；其余非 200 一律拒绝入账；
- 首次看到新 `runtime_id` 时必须显式选择 `baseline`（只建基线，降低重复风险）或 `include`
  （首次累计全部计入，降低漏记风险），不得留作隐式行为。

`active=false` 的已观察 lineage 仍会出现在后续快照中，采集端必须继续保留其基线，不得因
`active=false` 而删除或归零。

### 5.2 计量口径的对外声明

账单口径是认证并解码后、成功进入转发边界的应用负载，不包括协议 header、隧道封装与重传，
也不记录目标地址、客户端地址或连接明细。该口径不能替代云厂商或 VPS 的网卡计费；
两者之间的固有差额见 §12。

### 5.3 计划重启与最终结算

因为进程重启会清零内存计数，任何计划内的重启或配置变更必须按下列顺序执行，否则会留下未闭合窗口：

1. 停止接入新连接（下线、防火墙或上游负载均衡）；
2. 轮询快照的 `tcp_sessions` / `udp_sessions` 直至为 0 或达到超时；
3. 采集最终快照并**确认已持久化入账**；
4. 停止进程、应用变更、启动新进程（新 `runtime_id`）；
5. 采集端按 §5.1 的首快照策略处理新周期。

超时强切是允许的——已计字节不会丢失，只是连接被打断——但必须在采集端标记该窗口为未排空，
以便审计。异常退出（崩溃、OOM、断电）留下的未闭合窗口必须单独审计，不得当作正常周期切换。

## 6. 工作分解、交付物与工期

按一名熟悉 Go、sing-box 和异步代理数据面的工程师估算，不含灰度等待与法务日历时间。
人周为单人工作量，多人并行只压缩日历时间、不减少人周。

| 工作项 | 内容 | 必需 | 粗估 |
| --- | --- | --- | --- |
| 骨架与基线 | §1 目录树、`upstream.lock`、prepare/verify 脚本 | 是 | 1–2 人周 |
| 观测 PoC | 在钉定构建上用最小 gRPC 客户端验证 VLESS 归属，并复现 §2.2 的各项边界 | 是 | 2–4 人日 |
| 四向 tracker / registry | 通用 tracker、饱和计数、稳定 lineage | 是 | 2–4 人周 |
| 配置与失败关闭 | §4.6 全部校验路径与错误信息 | 是 | 0.5–1 人周 |
| UDS exporter | schema、安全加固、资源上限、监督与故障注入 | 是 | 2–3 人周 |
| 协议与性能验证 | Vision direct 切换、mux、XUDP/UoT、QUIC、UDP batch、bench/pprof | 是 | 2–4 人周 |
| 参考 collector 与契约测试 | 复用并适配 `shadowsocks-rust-plus` 的 `http_unix.py`、`settlement_model.py`、`mock_collector.py` | 是 | 1–2 人周 |
| 可复现发布与签名 | 两次独立构建、manifest、detached 签名与验签 | 是 | 1–2 人周 |
| 文档与运维手册 | `docs/` 六件套 | 是 | 1–2 人周 |
| SIGHUP 跨 Box 存活 | 进程级 registry、drain 阶段、看门狗接管 | 否（§4.4） | 2–4 人周 |
| 上游 rebase 储备 | 每次 minor 升级 | 周期性 | 1–2 人周/次 |

必需项合计约 **10–19 人周**，另加 15–25% 的复核返工缓冲；含可选的 SIGHUP 存活为 12–23 人周。
“能看每用户上下行”的 PoC 不等于完整功能，不得据此宣告阶段完成。

交付物清单：

| 交付物 | 说明 |
| --- | --- |
| `upstream.lock` | repository / tag / commit / `prepared_tree_sha256` / commit_date / fetched_at / license / go 最低版本 |
| `scripts/prepare-source.sh` | 按精确 commit 取源码并校验；方案 B 时零 fuzz 重放 `patches/series` |
| `scripts/verify.sh` | `go vet`、`go test -race ./...`、lint、敏感信息扫描（私钥、access key、`PrivateKey`/`Passphrase` 赋值） |
| `scripts/build-linux-release.sh` | 两次独立路径构建逐字节一致才产出 manifest + SHA-256 |
| `scripts/sign-release.sh` / `verify-release.sh` | detached 签名与验签，私钥离线保管 |
| `scripts/user-stats-client.py` | 带 schema 与健康校验的快照读取客户端 |
| `packaging/` | 复用上游 `release/config/sing-box.service`、`sing-box.sysusers`，追加 `RuntimeDirectory=` 承载 UDS 与 `Restart=on-failure`；上游无 tmpfiles 模板，需自建 |
| `config/server.example.json` | 脱敏的最小可用配置，含 `user_stats` 全字段与默认值 |
| `docs/` | `API.md`、`ARCHITECTURE.md`、`OPERATIONS.md`、`UPSTREAM_BASELINE.md`、`PERFORMANCE.md` |
| `tests/` | 契约测试、字节 oracle、故障注入与 benchmark |
| `THIRD_PARTY_NOTICES.md` | 见 §10 |
| `.env.example` | `UPSTREAM_REPOSITORY`、`GOMODCACHE`、`SING_BOX_BUILD_TAGS` 等占位 |
| `.gitattributes` | `*.patch -whitespace` 等 |

## 7. 里程碑与完成标准

| # | 里程碑 | 完成标准 / 证据 |
| --- | --- | --- |
| 1 | 冻结基线与骨架 | `upstream.lock` 已记录 tag/commit/`prepared_tree_sha256`；overlay 形态已定（§4.7）；两次独立构建 SHA-256 相同；`sing-box version` 显示钉定版本；`docs/UPSTREAM_BASELINE.md` 落库 |
| 2 | 观测 PoC | 回环 VLESS Reality/Vision 多用户 TCP+UDP 字节 oracle 差 = 0 的报告；复现并记录 ServiceName 覆写、静态白名单与重载丢数三项边界 |
| 3 | 四向 tracker / registry | `go test -race` 全绿；四向 oracle 误差 = 0；Linux amd64 真实 splice 用例覆盖；多 tracker 叠加 unwrap 断言通过；§4.6 全部失败关闭路径有用例 |
| 4 | UDS exporter | 权限 / 符号链接 / inode 替换 / 超限 / 慢客户端故障用例通过；exporter 异常退出导致进程失败退出；契约测试与参考 collector 直接通过 |
| 5 | 长跑与结算验证 | staging 连续运行 ≥ 7 天：负增量 = 0、未知 runtime = 0、`sequence` 重复 = 0、unhealthy 快照全部被拒；§5.3 计划重启流程演练无缺口、无重复 |
| 6 | 可发布版本 | §8 故障矩阵与性能三组对照报告归档；可复现发布包与签名验签通过；`docs/OPERATIONS.md` 含部署、采集、重启屏障与回滚步骤 |

## 8. 测试与性能门槛

自动化测试（括号为归属里程碑）：

- 每个协议的合法/非法认证、空 user、重复 user、跨 inbound 同名（M3）；
- TCP partial write、half-close、RST、取消、buffered copy、vectorized copy 和 Linux splice（M3）；
- UDP packet/batch、XUDP/UoT、mux、QUIC stream/datagram，不计 framing（M3）；
- Vision early data、padding/unpadding、buffered → direct 切换（**不含 splice**，见 §4.2）（M3）；
- 多 tracker 叠加时 counter unwrap 仍生效（M3）；
- 配置校验：未知字段、非白名单 inbound、空用户集、重名、缺 build tag、非 Unix 平台（M3）；
- 热删、同名重加、凭据轮换、旧连接继续计数、tombstone 保留（M3）；
- 快照响应中断、collector 重试、重复/乱序/倒退 `sequence`、累计溢出（M4）；
- UDS 权限、symlink/inode 替换、慢连接、超大请求和并发上限（M4）；
- exporter 异常退出、连续 accept 失败、systemd 重启后的新 runtime 处理（M4/M5）；
- 计划重启流程、超时强切、崩溃后的未闭合窗口标记（M5）；
- `-race`、fuzz、端到端字节 oracle、Linux 真实 splice 和发布目标集成测试（M3/M6）。

性能验收比较三组：未启用、编译但未配置、启用四向统计。记录吞吐、p50/p99 延迟、CPU、分配、
goroutine、内存随用户数/并发数增长，以及 exporter 被慢客户端占满时代理数据面的隔离。
目标不是预设“零开销”，而是给出可复现基线并设置回归阈值。

## 9. 构建、发布与上游跟进

### 9.1 构建矩阵

上游默认 tag 集不含 `with_v2ray_api`；REALITY 服务端受 `with_utls` 门禁
（`common/tls/reality_server.go:1`），缺该 tag 时配置直接报错——**只加 `with_v2ray_api` 的构建
跑不了里程碑 2 的 Reality PoC**。

| 项 | 值 |
| --- | --- |
| Go 工具链 | 钉具体版本并设 `GOTOOLCHAIN=local`；`v1.14.0` 的 `go.mod` 为 `go 1.25.5` |
| CGO | `CGO_ENABLED=0` |
| 可复现参数 | `-trimpath -buildvcs=false -ldflags "-s -w -buildid= $(cat release/LDFLAGS)"`；`release/LDFLAGS` 内容在 1.14 已改写（`-X runtime.godebugDefault=…`），必须读取文件而非硬编码 |
| 生产 tag 集 | `with_utls,badlinkname,tfogo_checklinkname0`（按需加 `with_quic`）+ 自有 `with_user_stats` |
| PoC tag 集 | 生产集再加 `with_v2ray_api` |
| 被裁剪的默认 tag | `with_gvisor`、`with_wireguard`、`with_tailscale`、`with_dhcp`、`with_acme`、`with_ccm`、`with_ocm`、`with_clash_api`、`with_naive_outbound` 等；最终清单见 §11 D4 |

编译验证记录：2026-09-05，darwin/arm64、go1.26.5，在 `v1.14.0`（`0b899587`）上执行

```text
go build -tags with_v2ray_api ./experimental/v2rayapi ./service/ssmapi ./protocol/vless
```

exit = 0。三个包均通过编译，但上游对它们显示 `[no test files]`；这只证明所选构建标签和包可编译，
不替代协议、流量对账或生产测试。

### 9.2 上游锁定与升级规则

1. `upstream.lock` 记录 repository / tag / commit / tree sha256 / commit_date / fetched_at /
   license / go 最低版本，格式参照 `shadowsocks-rust-plus/upstream.lock`；
2. 跟随 **stable 轨道**（当前 1.14.x），只在 patch 内自动跟进；minor 升级视为新基线，
   需重跑 §8 的全部对账矩阵；
3. 每月核对上游 tag；安全修复 T+2 工作日完成评估、T+7 产出可复现构建；
4. 补丁不能零 fuzz 应用即失败，不得静默跟随其他版本；
5. 每次升级重跑 §8 全部门禁并把结果写入 `docs/UPSTREAM_BASELINE.md`；
6. 所跟随轨道转为 oldstable 或 EOL 后 30 天内必须迁移；
7. 1.14.x 依赖 `sing`、`sing-tun`、`sing-quic` 的 beta 模块，须用 `go.sum` 或 `go mod vendor` 固定。

轨道状态（2026-09-05 核对）：`origin/oldstable` = `v1.13.21-2`，`origin/stable` = `v1.14.0-16`，
`origin/testing` 已进入 `v1.15.0-alpha.1`。1.13 已是 oldstable，按上游历史节奏很快停更，
不在其上开发 overlay。

## 10. 许可与命名

sing-box 的 LICENSE 是 GPL v3-or-later 的授权声明段，并附带“衍生作品未经同意不得使用该应用名称
或暗示关联”的额外文字（该文件只有声明段，不含 GPL 正文），见
[LICENSE](https://github.com/SagerNet/sing-box/blob/b5ebaa1fc0f2b94256180b95468e73ef53caa27d/LICENSE)。

义务按 GPLv3 的 convey 分档触发：

1. **向本公开仓库提交 overlay 源码或补丁即构成 convey**：本子目录须包含 GPL-3.0-or-later 全文与
   上游附加条款原文、`THIRD_PARTY_NOTICES.md`，补丁头须注明修改内容与日期（GPLv3 §5(a)）。
   这一触发点早于二进制发布。
2. **仅在自有主机部署、不向第三方交付二进制**不产生额外源码义务；GPLv3 没有 AGPL 式的网络使用条款。
3. **向第三方交付二进制**时须随附对应完整源码或书面要约。
4. 通过快照接口消费本项目的下游系统是独立进程，不因该接口而受 GPL 传染；但不得链接或内嵌本项目的
   Go 代码。

命名：`sing-box-plus` 是内部代号，不作为可发布产品名；对外发布使用中性名称或先取得许可。
法务评审属日历项，在里程碑 6 之前启动。

## 11. 待决策

| # | 事项 | 选项 | 当前取值 | 决策时点 |
| --- | --- | --- | --- | --- |
| D1 | overlay 形态 | A 零补丁 wrapper / B patch series / A+B | A 为默认 | 里程碑 1 |
| D2 | 首期协议范围 | 只做 VLESS / 同时纳入 VMess、Trojan、Hysteria2、TUIC | 只做 VLESS，其余按 §2.4 逐个验证后追加 | 里程碑 1 |
| D3 | 快照 schema | 复用 `shadowsocks-rust-plus` v1 形状 / 另立 v2 | 复用 v1 | 里程碑 4 之前 |
| D4 | 构建 tag 裁剪清单 | 见 §9.1 | 待定 | 里程碑 1 |
| D5 | 是否提供热用户增删接口 | 不提供（v1 只读，用户变更走受控重启）/ 另开受控写入端点 | 不提供 | 里程碑 6 之后评估 |
| D6 | 是否接受“进程崩溃丢尾账” | 接受（同 `shadowsocks-rust-plus`）/ 引入 WAL | 接受 | 里程碑 5 |
| D7 | 是否支持 Shadowsocks inbound | 支持并与 SSM 互斥 / 不支持 | 暂不支持，避免双口径 | 里程碑 5 |

D5 若改为“提供”，必须另开独立端点：不得复用只读快照 socket，需要单独授权、幂等 command id
与失败关闭；底层能力已存在（`sing-vmess vless/service.go:40 UpdateUsers`）。

## 12. 已知差额与风险

以下项目会造成“节点出流量 − 用户账单”的差额，属设计内的已知缺口，须在运维文档中声明并单独告警，
不得当作计量 bug 处理：

| 来源 | 性质 | 处置 |
| --- | --- | --- |
| REALITY 握手校验失败的伪装中继 | 在 utls 内部完成，任何 tracker 不可见；未设 `LimitFallback`，不限速 | 声明不支持；对 Dest 与带宽单独设告警阈值 |
| ShadowTLS 握手/校验失败中继 | 不经 router | 声明不支持 |
| 进程崩溃前未采集的尾账 | 内存 registry 无法恢复 | 按未闭合窗口审计（D6） |
| 重启期间被强杀的连接 | 已计字节不丢，但连接中断 | 走 §5.3 计划重启流程，合并变更以降低频次 |
| 计费 inbound 误配 `hijack-dns` / `reject` | 字节不经 tracker | 配置校验失败关闭（§4.6） |
| 协议 header、隧道封装与 TCP 重传 | 不在应用 payload 口径内 | 在计费说明中声明，与网卡计费天然有差 |

主要执行风险：上游 1.14 的接口与生命周期改动较大，每次 minor 升级需预留 rebase 与全量对账
（§6、§9.2）；1.14.x 目前依赖若干 beta 模块，需锁定并跟踪其转正节奏。
