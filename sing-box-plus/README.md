# sing-box-plus 实施计划

> 目标上游：[`SagerNet/sing-box`](https://github.com/SagerNet/sing-box)
>
> 实施基线：[`v1.14.0`](https://github.com/SagerNet/sing-box/releases/tag/v1.14.0) / `0b899587`
> （stable 轨道）；观测 PoC 使用 `v1.13.21`
>
> 参考实现：本仓库 `shadowsocks-rust-plus`（可结算契约的既有落地）
>
> 接入目标：本仓库 `sing-box-manager`（控制面与计量框架）
>
> 源码核对：2026-09-05。下文未标版本的 `路径:行号` 均指
> `b5ebaa1fc0f2b94256180b95468e73ef53caa27d`（`v1.13.19`）；1.14 的差异单独标注。
> 下次核对触发：上游发布新 minor、`sing-box-manager` 升级数据面版本、或本计划进入新里程碑。

## 1. 目标与范围

给 sing-box 增加可用于多用户配额和结算的流量统计：认证后的稳定用户归属、
TCP/UDP × 上下行四个累计值、明确的重载边界、可幂等采集，以及受控的本机快照接口。
能力对标本仓库已实现的 `shadowsocks-rust-plus`，并接入 `sing-box-manager` 现有的计量与结算框架，
最终解除其对 VLESS 用户 `quotaBytes = 0` 的限制。

**交付范围**：数据面身份归属、四向累计计数、本机只读导出，以及为接入 Manager 所必需的
Agent/Controller 改动。

**明确不做**：订阅生成、用户与套餐管理、账单存储、管理后台、限速、实时断开；
不承诺“进程崩溃也不丢一个字节”（尾账按未闭合窗口审计，与 `shadowsocks-rust-plus` 一致）；
不支持 `daemon`、libbox 与 1.14 的 `boxdd` 宿主形态，只覆盖 `sing-box run`。

**验收定义**：§5.4 的门槛全部通过，`sing-box-manager` 可以对 VLESS 用户设置非零配额并正确执行。

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
  为将来的配额热执行留有余地。

### 2.2 不能依赖的部分

内置 Experimental V2Ray StatsService 可以做观测，但不能承担结算：

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

以下限制不是实现难度问题，而是上游结构决定的，设计必须绕开或显式声明：

- **重载没有平滑窗口**。SIGHUP 循环是先取消再关旧、然后才建新（`cmd/sing-box/cmd_run.go:188`
  `cancel()` → `:191` `Close()` → `:174` `create()`），`Box.Close()` 关闭 connection manager 后
  `CloseAll()` 遍历强杀全部在途连接（`route/conn.go:52-69`）；仓库内没有 SO_REUSEPORT 或监听 fd
  传递，端口必然有短暂无监听窗口。上游没有任何排空语义。
- **StatsService 属于 Box，重载即丢未采集增量**，见 `box.go:496-535` 与上游 issue
  [#4059](https://github.com/SagerNet/sing-box/issues/4059)（2026-04-19 开启，同日以 not planned 关闭）。
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
| Shadowsocks multi | 具名用户可传播 | 可支持；但 SSM tracker 在 router 之前包装，会多计 sniff 预读与随后被 reject/block 的字节，**不能与 router tracker 互作 oracle** |
| Shadowsocks relay | `metadata.User` 是 `destinations[].name`（`inbound_relay.go:130`） | 是中继目的地不是终端用户，**不可按用户计费** |
| Hysteria/Hysteria2、TUIC | 协议层有用户概念 | 可支持，需验证 QUIC stream/datagram 分类 |
| Naive、AnyTLS、HTTP/SOCKS/Mixed 认证 | 有认证用户名的路径可传播 | 可支持，逐协议确认匿名与 fallback |
| ShadowTLS | `users[].name` 只用于握手校验 | `InboundDetour` 跳转发生在 tracker 之前（`route/route.go:62-78`，tracker 循环在 `:152`），User 被 detour 的 `UpstreamMetadata` 丢弃；计费身份只能取 detour 末端的 SS 用户（单用户 SS 则为空，应拒绝） |
| TUN、redirect、tproxy、direct | 无认证用户 | 不支持按用户计费 |

通用规则：**带 `InboundDetour` 的 inbound 一律以 detour 末端 inbound 为计费身份**。
匿名、空名、认证 fallback 或 tracker 绕过不得默认为“unknown 后继续转发”；
若会影响收费，启动或连接必须失败关闭。

## 3. 术语与字段对照

三个子项目对同一概念使用不同名称。实现与 collector 一律按“本项目采用”列取名。

| 概念 | sing-box | `shadowsocks-rust-plus` | `sing-box-manager` | 本项目采用 |
| --- | --- | --- | --- | --- |
| 计费身份名 | `users[].name` → `metadata.User` | `users[].name` / `identity_name` | `identity_name`，由不可变 `(user_id, route_id)` 派生（`store/users.rs:16-24`） | `name`（快照字段）；正文称 billing name |
| 入站标识 | inbound `tag` | `server_id` | `inbound_tag`，Agent 硬编码常量 `"in-shared"`（`agent/ssm.rs:14`；SS 与 VLESS Entry 共用，`compiler/entry.rs:65,96`） | `server_id` := inbound tag |
| 节点标识 | — | `node_id` | Manager 的 `node_id` 指中继出口节点，与此不同 | `node_id` := Manager 的 `entry_id`，由编译器写入 exporter 配置，Agent 拒绝不符的快照 |
| 运行周期 | — | `runtime_id`（32 位小写 hex，进程级随机） | `singbox_boot_id`（Agent 本地 `MAX+1` 整数，`agent/state.rs:90-92`）；另有 `entry_runtime_epochs.epoch` | 快照给 `runtime_id`，Agent 负责与本地 epoch 一对一绑定（§5.2） |
| 快照序号 | — | `sequence`，每次 `/v1/snapshot` 严格递增 | `StatsBatch.sequence` 已被结算屏障占用：poll 恒 0，final 由 outbox 分配并作去重键（`agent/stats.rs:17`、`agent/barrier_store.rs:44-49`） | 新字段 `exporter_sequence`，**不得**复用 `StatsBatch.sequence` |
| 代次 | — | `generation`，同名重激活复用、不递增 | 基线键无此维度（`0006_metering.sql`） | `generation`，语义同 `shadowsocks-rust-plus`，固定输出 `1`，但不得从 collector 键中省略 |

单 Host 最多一个 Entry（`sing-box-manager/docs/architecture.md:56`），因此 `node_id + server_id`
在本部署中无歧义。下文示例一律使用 `in-shared` 作为 inbound tag。

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
- 与 trafficcontrol / StatsService 的挂载顺序必须显式确定，避免双层 CounterConn 叠加。

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
特殊 outbound 直接读写、UDP batch、连接取消与 half-close、重载期间仍存活的长连接。任何绕过标准
router tracker 的 handler 都必须明确选择“计数、拒绝或声明不支持”，不能静默转发但漏计。

计费用 inbound 不得配置 `hijack-dns` 或以 `reject` 结束的规则动作（§2.3）。

### 4.3 身份生命周期

同一 runtime 内，inbound tag 与 billing name 构成**稳定计费身份**：

1. 删除或停用只把 `active` 切为 `false`，记录保留在快照中；
2. 同名重建复用原 generation 与原计数器，并把 `active` 切回 `true`；
3. 凭据轮换不改变 lineage；
4. `generation` 作为 schema 保留维度固定输出 `1`，但不得从 collector 键中省略；
5. tombstone 数达到 `max_identities` 时失败关闭或标记 unhealthy，不得丢弃 lineage；
6. 同一 runtime 内不得把已用名称重分配给不同计费用户；需要改变归属时使用新名称或进入新 runtime。

这与 `sing-box-manager` 由不可变 `(user_id, route_id)` 派生 `identity_name`（永不重分配）的做法一致，
也使 Manager 的基线表无需引入新主键维度。

### 4.4 重载与结算模型

**主路径 = `sing-box-manager` 现状的进程重启 + 已实现的两阶段结算屏障。**
Manager 的 Agent 从不发 SIGHUP，`restart` 是 kill 旧子进程再以固定 argv `run -c` spawn
（`src/agent/runtime.rs:45-71`，全仓无 reload 路径）；每次部署分配新 `runtime_epoch`，final 批经
`traffic_batches` 按 `(entry, boot_id, sequence)` 精确一次去重（`migrations/0006_metering.sql:27-36`、
`migrations/agent_0003_barrier.sql`、`src/agent/settle.rs`）。因此首期只需让 VLESS 进入这条已存在的
屏障，不必先做跨 Box registry。

进程重启会清零内存计数，屏障保证不丢账：抓取最终统计 → 写入 outbox → 等 Manager ack → 才停旧进程。
新进程产生新 `runtime_id`，采集端据此选择首快照策略。

**跨 Box 的进程级 registry 是可选项**，仅在需要 `kill -HUP` 热重载且要求累计值不清零的独立部署形态下
才必需。若要做：

1. 进程启动生成 `runtime_id`，创建唯一 registry/exporter；
2. 每次加载 Box 时把同一 registry 注入新的 UserStatsTracker；
3. 上游重载**强制关闭全部在途连接**（§2.3）。由于计数按每次 copy 迭代增量累加，强杀不丢已计字节，
   但会打断用户连接。若要排空，必须在 `run()` 与 `Box.Close()` 之间插入 drain 阶段
   （停止 accept → 等待或超时 → 再 Close）并接管 `FatalStopTimeout` 看门狗，这是新增改动而非配置策略；
4. 端口仍有短暂无监听窗口；若要求端口不中断，须单列“引入 SO_REUSEPORT 或 listener fd 传递”
   为独立工作项；
5. registry 全进程共享，`MustRegister` 会覆盖旧 Box 的同类服务，代次必须由自有 registry 维护；
6. 只覆盖 `sing-box run` 宿主。

纯内存 registry 无法恢复进程崩溃前尚未采集的尾账，这与 `shadowsocks-rust-plus` 相同。
若业务要求“崩溃也不丢一个字节”，需另加 WAL 或持久计量数据面，复杂度显著上升；
不得把高频轮询描述成严格保证。

### 4.5 快照接口契约

**采用 `shadowsocks-rust-plus` v1 的既有形状**（其 `docs/API.md`），使
`tests/http_unix.py`、`scripts/user-stats-client.py`、`tests/settlement_model.py` 与 mock collector
可直接作为本项目的契约测试。sing-box 特有信息以可选附加字段追加（现有校验器忽略未知键），
不改动 `health` 对象的字段名。

```json
{
  "schema_version": 1,
  "node_id": "entry-example-01",
  "runtime_id": "0123456789abcdef0123456789abcdef",
  "started_at_unix_ms": 1787587200000,
  "sequence": 42,
  "health": { "counter_overflow": false, "sequence_overflow": false },
  "servers": [
    {
      "server_id": "in-shared",
      "listen": "0.0.0.0:19736",
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
附加字段，`*_sessions` 是**当前活跃数、不参与结算**，专供 Agent 的排空闸门使用（§5.2）。

传输层：HTTP/1.1-over-Unix-stream，每连接单请求单响应，禁 keep-alive / query / body；
固定两条路由——`GET /v1/snapshot`（被接受时即推进 `sequence`）与 `GET /healthz`（200/503，
不推进 `sequence`）。错误一律返回固定 `{schema_version, error:{code}}` 对象，错误码表与 v1 一致
（400/404/405/408/413/429/500/505）。

采集端处置：429 与连接被直接关闭视为**可重试且不得入账**；其余非 200、`Content-Length` 不符、
JSON 截断或 `schema_version` 不匹配一律拒绝入账。

安全与资源：非破坏性累计快照；稳定排序；请求/响应大小、身份数、并发数、读写超时均有上限；
socket 默认 `0600`，绑定前检查父目录、符号链接、旧 socket 与 inode 替换。
exporter 启动失败必须阻止统计模式启动；运行中异常退出由主服务监督并触发整体失败或重启，
不得出现“继续转发但停止计量”。Unix socket 不得直接映射为公网监听，远程读取须经节点上的独立
反向代理提供 HTTPS/mTLS/来源限制与审计。

外部 collector 保存 baseline 的完整键：

```text
node_id + server_id + server_generation + name + identity_generation + runtime_id
```

再结合 snapshot sequence、采集批次 ID 和四项 delta 幂等入库。累计值倒退、sequence 倒退、
未知 runtime、overflow 或 unhealthy 快照一律失败关闭，不得猜测并继续收费。

### 4.6 overlay 形态

**默认方案 A：零补丁 wrapper。** `adapter.Router.AppendTracker`、`box.New`、`Box.Router()` 与
`include.Context` 都已导出，因此一个独立 Go module（`sing-box-plus/cmd/sing-box-plus`，`go.mod` 中
`require` 钉死版本的 sing-box）在自有 main 里复刻 `cmd_run` 循环，并在 `Start()` 之前调用
`instance.Router().AppendTracker(tracker)` 即可，无需修改上游源码；registry 与 exporter 由该 main
持有，天然满足 §4.4 的进程级要求。

**方案 B：patch series。** 仅当必须改上游内部路径（vless/vision、splice、`option` schema、
需要在 tracker 之前失败关闭的位置）时使用，按 `patches/series` 顺序重放，无法零 fuzz 应用即失败。

选择标准：默认 A；A 覆盖不到的失败关闭点再以最小 patch 补，两者可共存。结论在里程碑 1 写入
`upstream.lock` 与 `docs/UPSTREAM_BASELINE.md`。

**硬约束**：`sing-box-manager` 的 Controller 与 Agent 都会实跑 `<bin> check -c` 并用
`sing-box version` 探测版本（`src/agent/singbox.rs`、`src/agent/deploy.rs:60-71`），因此自有二进制
必须保持 `run` / `check` / `format` / `version` 的 argv 与退出码兼容，版本字符串格式也需与 Manager
的解析约定一致。

## 5. 与 sing-box-manager 的接入

`sing-box-manager` 已有按 `identity_name` 的上下行 baseline、周期累计、配额评估、runtime epoch、
最终结算屏障和幂等入库框架；当前数据源是 Shadowsocks SSM，因此它要求 VLESS relay 用户
`quotaBytes = 0`（`src/manifest/mod.rs:346-350`）。

### 5.1 版本对齐

Manager 只验证过数据面 `1.13.14`（`src/manifest/mod.rs:476`、其 README:29/79）。任何自定义构建都会
替换 Controller 与全部 Agent 上已验证的二进制，因此：

- **观测 PoC** 使用同 major 的 `v1.13.21` 加 `with_v2ray_api` 构建，先在 Manager 上重跑
  `check` / `deploy` / 屏障回归，并更新其 README 的已验证版本；
- **正式 overlay** 基于 `v1.14.x` 时，先把 `singboxVersion` 升到该版本，对全部 Manager 生成的 entry
  配置做 `sing-box check` 回归（重点关注 1.14 移除的 legacy DNS server 格式与其他弃用项），
  通过后才允许 Agent 改读新 exporter；
- Controller 本机与每台 Agent 的 `SINGBOX_BIN` 必须是同一构建，可用
  `config_artifacts.target_singbox_version` 记录。

### 5.2 分两层接入

**第 1 层：观测 PoC。** 使用固定的 `with_v2ray_api` 构建，VLESS 的 `users[].name` 复用 Manager 由
`(user_id, route_id)` 确定性派生的 `identity_name`；Agent 在回环读取累计 uplink/downlink。
这一层无需改变 usage bucket 结构，但 §2.2 的全部限制仍在，不解除硬配额保护。

该路线在 `sing-box-manager/_legacy/` 中已在 1.13.14 上实现过一次
（`_legacy/grpc.rs` 的 StatsService gRPC 只读客户端、`_legacy/backend/reload.rs`、
`_legacy/singbox.rs:205-217` 生成 `experimental.v2ray_api` 白名单），其中已记录 ServiceName 覆写的坑。
它后来被 SSM 取代——被放弃的不是 gRPC 统计客户端，而是与它绑定的**身份下发方式**。
`_legacy` 有两个并存 backend，由 `backend.mode` 选择，且 `_legacy/config.rs:247-253` 强制
`vless-reality` 入站必须用 `reload`（“VLESS 无 SSM API”）：

| | `ssm` | `reload`（VLESS 唯一可选） |
| --- | --- | --- |
| 改用户 | 内存 usersMap，不重建入站、不清零他人计数（注释标注已实测） | 重写配置 `inbounds[].users` + `reload_cmd` |
| 代价 | 无 | “reload 会重建实例、断连、清零内存计数”（`_legacy/backend/reload.rs:4`） |
| 统计源故障时 | 仍可下发身份 | **必须整轮跳过下发**（`_legacy/meter.rs:142-144, 228-231`），否则会在统计未读取时重载并永久丢数 |
| 统计维度 | 按入站 scope | `scope="*"` 全局每身份（v2ray key 不含 inbound tag） |
| 增量算法 | — | `cu >= lu ? cu-lu : cu`（`_legacy/meter.rs:157-158`），把任何回退当复位并整值计入 |

即：每次配额翻转或加删用户都会断开全部连接并清零计数；统计源一挂，配额执行随之停摆；
而“回退即复位”的增量启发式正是清零逼出来的，也正是现版本改成 `max(0, …)`（F1 回归）要消除的
重复计费风险。0.1.0 因此只保留 SSM，VLESS 随之失去计量。

由此确定两条纪律：

- PoC 复用 `_legacy/grpc.rs`（统计客户端本身没有问题），但**不复用 `ReloadBackend` 的身份下发方式**；
  身份变更仍走 Manager 现有的 revision + 屏障部署路径。
- 本项目不会重蹈覆辙的原因是屏障：`_legacy` 的“先 read_stats 再 apply”只是时序约定，失败时只能
  整轮放弃；现在停旧进程前有带回执、可重放、按 `(entry, boot_id, sequence)` 去重的协议。

PoC 前置条件：

1. 复活 `_legacy/grpc.rs` 并在新基线上回归（重点验证 ServiceName 覆写、静态白名单、SIGHUP 丢数）；
2. `src/compiler/entry.rs` 为 VLESS Entry 输出 `experimental.v2ray_api` 块，`stats.users` 等于身份投影；
3. Controller 与所有 Agent 使用同一 `with_v2ray_api` 构建（否则两侧 `check` 都会拒绝该配置）。

**第 2 层：正式计费。** Agent 改读 `sing-box-plus` 的 UDS 快照。现有两方向账单先把 TCP+UDP 各自求和；
若产品要展示四方向，再扩展 raw usage schema。Manager 侧的能力需区分已具备与需新写：

| 能力 | 现状 | 需要做什么 |
| --- | --- | --- |
| final 批精确一次 | 已具备（`traffic_batches` PK 去重） | — |
| 屏障 ack 后才停旧进程 | 已具备（`agent_0003_barrier.sql`、`settle.rs`） | — |
| 新 boot id 不产负增量 | 已具备（结构性 include） | — |
| VLESS 进入计量与屏障 | **无**：`metering/tick.rs:40-45` 对非 shadowsocks 直接返回；`manager/deploy.rs:222-232` 对 `vless-reality` 硬置 `barrier_required=false`；`agent/runtime.rs` 的 VLESS 健康检查是进程型空桩 | 取消 inbound_kind 跳过，置 `barrier_required=true`，补健康检查 |
| exporter 客户端 | **无**：Agent 只有 reqwest over TCP 的 SSM 客户端（`agent/ssm.rs`） | 新增 HTTP/1.1-over-UDS 严格客户端，按 §4.5 处置错误码 |
| 未知 runtime 拒绝 | **无**：任何 boot id 都新建基线行 | Agent 在每次 `restart()` 后把首份快照的 `runtime_id` 与本地 epoch 一对一绑定并持久化（新增 agent 迁移列），后续快照 runtime_id 不符即拒绝并告警 |
| unhealthy 拒绝 | **无**：`StatsBatch` 无 health 字段，也不读 `/healthz` | DTO 增字段，unhealthy 快照失败关闭 |
| 快照序号跟踪 | **无**：poll 批 `sequence` 恒 0（`agent/stats.rs:17`），该字段已被屏障占用 | 新增 `exporter_runtime_id` / `exporter_sequence`（**不复用** `StatsBatch.sequence`），按 `(entry, runtime_id)` 记高水位 |
| 累计倒退处理 | **相反**：`store/metering.rs:14-18` 刻意 `max(0, cur-last)`，用于吸收 final/poll 交错的陈旧读（F1 回归） | 改为两层：`exporter_sequence ≤ 高水位` → 丢弃整份快照且不推进基线；序号前进但计数倒退 → 失败关闭并标记 Entry stale |
| 基线键含 generation/runtime | **无**：PK 为 `(entry_id, inbound_tag, identity_name, singbox_boot_id)`，迁移 additive-only，SQLite 不能改 PK | generation 恒为 1 时无需改主键；以 additive 方式加 `exporter_runtime_id`、`generation` 列（默认 1），主键沿用 boot id，由上面的 runtime 绑定保证等价 |
| 排空闸门 | 绑死 SSM：`agent/gate.rs:34-46` 轮询 `tcp_sessions + udp_sessions <= 0` | 改读快照的 `*_sessions` 附加字段；若不提供该字段，必须显式声明“VLESS 不排空、`drain_clean` 恒 false 并记 `unsettled_window`”，禁止把缺失字段退化成 0 造成假性 `drain_clean` |
| 首快照策略 | 结构性 include | 保持 include 并写明理由：Agent 是唯一 spawner，崩溃尾账按未闭合窗口单独审计 |
| VLESS 非零配额校验 | 硬拒绝（`manifest/mod.rs:346-350`） | 改由能力位门控，条件满足后才放开 |

### 5.3 配额执行

超额后如何把 VLESS 用户踢下线，与 Shadowsocks 完全不同：SS 走资格翻转 → SSM reconcile 热删身份、
不重启（`metering/tick.rs:130-136`）；VLESS 用户 UUID 静态编译进 inbound，停用需要 `apply --deploy`
发布新 revision，即每次配额翻转都要走屏障 + 进程重启 + 新 `runtime_id`。

**首版采用方案 (b)：**

- **(a) 热用户增删**：另开受控写入端点（参考 SSM `/users` 语义），Manager 复用 reconcile 路径。
  底层能力已存在（`sing-vmess vless/service.go:40 UpdateUsers`），但会引入写接口、单独授权、
  幂等 command id 与失败关闭，攻击面明显扩大，且**不得复用只读快照 socket**。列为后续演进。
- **(b) 超额即重新部署**：把配额翻转改为触发 Entry 重编译 + 屏障部署，并给出翻转合批与最小间隔策略。
  周期性配额（月/年）下翻转频率低，成本可接受。

选 (b) 的前提是屏障真正生效：`_legacy` 的 reload 路线正是死在“改用户即清零计数”上（§5.2），
其缓解手段只是时序约定；现在停旧进程前有带回执、可重放的两阶段屏障，才使“重新部署”从丢数风险
变成可接受的成本。因此 §5.2 表中“VLESS 进入计量与屏障”是 (b) 的硬前置，
不得先放开配额、后补屏障。

### 5.4 解除 quotaBytes = 0 的门槛

在下列条件全部通过前，保留 Manager 对 VLESS 的 `quotaBytes = 0` 保护：

- VLESS Reality/Vision TCP、XUDP/UoT 与长连接重载对账无缺口、无重复；
- exporter 失败、进程重启、Agent/Manager 重试的故障注入通过；
- 删除、停用、同名重建和凭据轮换的 lineage 语义符合 §4.3；
- §5.2 表中“需要做什么”一栏全部落地，Manager 能拒绝未知 runtime、倒退累计、重复
  `exporter_sequence` 和 unhealthy 快照；
- §5.3 的配额执行路径已验证；
- 最终结算成功后才允许旧实例退出或新部署完成。

长期可让 Shadowsocks 和 VLESS 都走同一个 exporter，从而删除 SSM 与 V2Ray API 的双采集逻辑；
但两者口径不同（§2.2），迁移必须作为**新 runtime / 新 lineage 切换**并在文档记录口径差异，
不得跨口径差分。首期只让 VLESS 使用新 exporter，保持 SSM 路径不变以缩小迁移面。

## 6. 工作分解与工期

按一名熟悉 Go、sing-box 和异步代理数据面的工程师估算，不含灰度等待与法务日历时间。
人周为单人工作量，多人并行只压缩日历时间、不减少人周。

| 工作项 | 内容 | Manager 接入是否必需 | 粗估 |
| --- | --- | --- | --- |
| 骨架与基线 | §1 目录树、`upstream.lock`、prepare/verify 脚本、可复现发布与签名 | 必需 | 1–2 人周 |
| PoC | 复用 `_legacy/grpc.rs`，在钉定构建上验证 VLESS 归属 | 必需 | 1–2 人日 |
| 观测接入 | 自定义构建、静态白名单、Agent collector、仪表与基础故障处理 | 必需 | 1–2 人周 |
| 四向 registry | 通用 tracker、饱和计数、稳定 lineage、配置校验 | 必需 | 2–4 人周 |
| UDS exporter | schema、安全加固、资源上限、监督与故障注入 | 必需 | 2–3 人周 |
| 协议与性能验证 | Vision direct 切换、mux、XUDP/UoT、QUIC、UDP batch、bench/pprof | 必需 | 2–4 人周 |
| Agent exporter 客户端 + runtime 绑定 | UDS 客户端、runtime_id↔epoch 绑定与迁移、健康/序号校验 | 必需 | 1–2 人周 |
| Manager schema 与结算规则 | additive 迁移、倒退两层处理、VLESS 进入 tick/屏障 | 必需 | 1–2 人周 |
| VLESS 配额执行 | §5.3 方案 (b) | 必需 | 1–2 人周 |
| 文档与运维手册 | `docs/` 六件套 | 必需 | 1–2 人周 |
| SIGHUP 跨 Box 存活 | 进程级 registry、drain 阶段、看门狗接管 | **可选**（§4.4） | 2–4 人周 |
| 上游 rebase 储备 | 每次 minor 升级 | 周期性 | 1–2 人周/次 |

必需项合计约 **13–24 人周**，另加 15–25% 的复核返工缓冲；含可选的 SIGHUP 存活为 15–28 人周。
“能看每用户上下行”的 PoC 不等于完整功能，不得据此宣告阶段完成。

交付物清单（对照 `shadowsocks-rust-plus` 的既有工程要素）：

| 交付物 | 说明 |
| --- | --- |
| `upstream.lock` | repository / tag / commit / `prepared_tree_sha256` / commit_date / fetched_at / license / go 最低版本 |
| `scripts/prepare-source.sh` | 按精确 commit 取源码并校验；方案 B 时零 fuzz 重放 `patches/series` |
| `scripts/verify.sh` | `go vet`、`go test -race ./...`、lint、敏感信息扫描（私钥、access key、`PrivateKey`/`Passphrase` 赋值） |
| `scripts/build-linux-release.sh` | 两次独立路径构建逐字节一致才产出 manifest + SHA-256 |
| `scripts/sign-release.sh` / `verify-release.sh` | detached 签名与验签，私钥离线保管 |
| `packaging/` | 复用上游 `release/config/sing-box.service`、`sing-box.sysusers`，追加 `RuntimeDirectory=` 承载 UDS；上游无 tmpfiles 模板，需自建 |
| `docs/` | `API.md`、`ARCHITECTURE.md`、`OPERATIONS.md`、`UPSTREAM_BASELINE.md`、`PERFORMANCE.md` |
| `tests/` | 复用 `shadowsocks-rust-plus/tests/{http_unix,settlement_model,mock_collector}.py`，另加字节 oracle |
| `THIRD_PARTY_NOTICES.md` | 见 §10 |
| `.env.example` | `UPSTREAM_REPOSITORY`、`GOMODCACHE`、`SING_BOX_BUILD_TAGS` 等占位 |
| `.gitattributes` | `*.patch -whitespace` 等 |

## 7. 里程碑与完成标准

| # | 里程碑 | 完成标准 / 证据 |
| --- | --- | --- |
| 1 | 冻结基线与骨架 | `upstream.lock` 已记录 tag/commit/`prepared_tree_sha256`；overlay 形态已定（§4.6）；两次独立构建 SHA-256 相同；`sing-box version` 显示钉定版本；`docs/UPSTREAM_BASELINE.md` 落库 |
| 2 | 观测 PoC | 回环 VLESS Reality/Vision 多用户 TCP+UDP 字节 oracle 差 = 0 的报告；复现并记录 ServiceName 覆写与 SIGHUP 丢数；§5.2 的三项前置通过 |
| 3 | 四向 tracker / registry | `go test -race` 全绿；四向 oracle 误差 = 0；Linux amd64 真实 splice 用例覆盖；多 tracker 叠加 unwrap 断言通过 |
| 4 | UDS exporter | 权限 / 符号链接 / inode 替换 / 超限 / 慢客户端故障用例通过；exporter 异常退出导致进程失败退出；`tests/http_unix.py` 与 mock collector 直接通过 |
| 5 | 接入 Manager（只记账） | staging 只记账 ≥ 7 天：负增量 = 0、未知 runtime = 0、`exporter_sequence` 重复 = 0、unhealthy 快照全部被拒 |
| 6 | 启用配额 | §8 故障矩阵与性能三组对照报告归档；§5.3 配额执行路径验证通过；§5.4 门槛全部满足后才解除 `quotaBytes = 0` |

## 8. 测试与性能门槛

自动化测试（括号为归属里程碑）：

- 每个协议的合法/非法认证、空 user、重复 user、跨 inbound 同名（M3）；
- TCP partial write、half-close、RST、取消、buffered copy、vectorized copy 和 Linux splice（M3）；
- UDP packet/batch、XUDP/UoT、mux、QUIC stream/datagram，不计 framing（M3）；
- Vision early data、padding/unpadding、buffered → direct 切换（**不含 splice**，见 §4.2）（M3）；
- 多 tracker 叠加时 counter unwrap 仍生效（M3）；
- 热删、同名重加、凭据轮换、旧连接继续计数、tombstone 保留（M3）；
- 快照响应中断、collector 重试、重复/乱序/倒退 `exporter_sequence`、累计溢出（M4）；
- UDS 权限、symlink/inode 替换、慢连接、超大请求和并发上限（M4）；
- 进程重启前后长连接、Box 启动失败、连续部署、屏障 ack 丢失与重放、崩溃（M5）；
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
| Go 工具链 | 钉具体版本并设 `GOTOOLCHAIN=local`；1.13.19 的 `go.mod` 为 `go 1.24.7`，`v1.14.0` 升到 `go 1.25.5` |
| CGO | `CGO_ENABLED=0` |
| 可复现参数 | `-trimpath -buildvcs=false -ldflags "-s -w -buildid= $(cat release/LDFLAGS)"`；`release/LDFLAGS` 内容在 1.14 已改写（`-X runtime.godebugDefault=…`），必须读取文件而非硬编码 |
| 生产 tag 集 | `with_utls,badlinkname,tfogo_checklinkname0`（按需加 `with_quic`）+ 自有 `with_user_stats` |
| PoC tag 集 | 生产集再加 `with_v2ray_api` |
| 被裁剪的默认 tag | `with_gvisor`、`with_wireguard`、`with_tailscale`、`with_dhcp`、`with_acme`、`with_ccm`、`with_ocm`、`with_clash_api`、`with_naive_outbound` 等；最终清单见 §11 D6 |

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
`origin/testing` 已进入 `v1.15.0-alpha.1`。1.13.20/21 相对 1.13.19 在本计划涉及的文件上零差异
（另含 mux/websocket early-data 修复），可作 1.13 线的冻结点；但 1.13 已是 oldstable，
按上游历史节奏很快停更，不在其上开发 overlay。

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
4. `sing-box-manager`（MIT）必须保持进程边界，不得 import 本项目的任何 Go 代码，也不分发数据面
   二进制，以维持其许可证独立。

命名：`sing-box-plus` 是内部代号，不作为可发布产品名；对外发布使用中性名称或先取得许可。
改名时需同步确认 Manager 的 `sing-box version` 解析约定（§4.6 硬约束）。法务评审属日历项，
在里程碑 5 之前启动。

## 11. 待决策

| # | 事项 | 选项 | 当前取值 | 决策时点 |
| --- | --- | --- | --- | --- |
| D1 | 实施基线轨道 | 1.13.21（与 Manager 同 major，但 oldstable 将停更）/ 1.14.x（stable，依赖 beta 模块、Go 1.25+） | PoC 用 1.13.21，正式 overlay 用 1.14.x | 里程碑 1 |
| D2 | overlay 形态 | A 零补丁 wrapper / B patch series / A+B | A 为默认 | 里程碑 1 |
| D3 | 快照 schema | 复用 `shadowsocks-rust-plus` v1 形状 / 另立 v2 | 复用 v1 | 里程碑 4 之前 |
| D4 | Shadowsocks 是否迁到统一 exporter | 首期只做 VLESS / 一并迁移 | 首期只做 VLESS | 里程碑 5 |
| D5 | VLESS 配额执行 | (a) 热用户增删写接口 / (b) 超额即重新部署 | (b) | 里程碑 6 |
| D6 | 构建 tag 裁剪清单 | 见 §9.1 | 待定 | 里程碑 1 |
| D7 | 是否需要四方向账单展示 | 两方向求和 / 扩展 raw usage schema | 先两方向 | 里程碑 5 |
| D8 | 是否接受“进程崩溃丢尾账” | 接受（同 `shadowsocks-rust-plus`）/ 引入 WAL | 接受 | 里程碑 5 |

## 12. 已知差额与风险

以下项目会造成“节点出流量 − 用户账单”的差额，属设计内的已知缺口，须在运维文档中声明并单独告警，
不得当作计量 bug 处理：

| 来源 | 性质 | 处置 |
| --- | --- | --- |
| REALITY 握手校验失败的伪装中继 | 在 utls 内部完成，任何 tracker 不可见；未设 `LimitFallback`，不限速 | 声明不支持；对 Dest 与带宽单独设告警阈值 |
| ShadowTLS 握手/校验失败中继 | 不经 router | 声明不支持 |
| 进程崩溃前未采集的尾账 | 内存 registry 无法恢复 | 按未闭合窗口审计（D8） |
| 重载期间被强杀的连接 | 已计字节不丢，但连接中断 | 部署合批，减少翻转频率（§5.3） |
| 计费 inbound 误配 `hijack-dns` / `reject` | 字节不经 tracker | 配置校验失败关闭 |

主要执行风险：上游 1.14 的接口与生命周期改动较大，每次 minor 升级需预留 rebase 与全量对账
（§6、§9.2）；Manager 侧改动跨 Agent 与 Controller 两端，须与数据面升级同批发布，
否则会出现“新二进制 + 旧采集逻辑”的静默漏计。
