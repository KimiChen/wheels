# sing-box-plus 实施计划

> 目标上游：[`SagerNet/sing-box`](https://github.com/SagerNet/sing-box)
>
> 实施基线：[`v1.14.0`](https://github.com/SagerNet/sing-box/releases/tag/v1.14.0) / `0b899587`
> （stable 轨道）
>
> 参考实现：本仓库 `shadowsocks-rust-plus`（可结算契约的既有落地；本项目沿用其结算语义与
> 参考模型，快照 schema 按 sing-box 的字段词汇自行定义，见 §4.5）
>
> 源码核对：2026-09-05。下文未标版本的 `路径:行号` 均指 `0b899587`；涉及 1.13.x 的历史差异
> 单独标注。下次核对触发：上游发布新 minor，或本计划进入新里程碑。

## 1. 目标与范围

给 sing-box 增加可用于多用户配额和结算的流量统计：认证后的稳定用户归属、
TCP/UDP × 上下行四个累计值、明确的重启边界、可幂等采集，以及受控的本机快照接口。
能力对标本仓库已实现的 `shadowsocks-rust-plus`。

**交付范围**：一个固定上游版本的 hardened overlay，包含数据面身份归属、四向累计计数、
配置校验与失败关闭、本机只读 UDS 导出，以及可复现构建、契约测试与运维文档。
首期计费协议为 **VLESS** 与 **Shadowsocks**（仅 `2022-blake3-aes-128-gcm` / `-aes-256-gcm`
的 `users[]` 具名多用户形态）两类 inbound，范围与拒绝条件见 §2.4。

**明确不做**：订阅生成、用户与套餐管理、账单存储、管理后台、配置分发、限速。
这些属于下游集成方，本项目只提供 §5 的采集与结算契约以及一份参考 collector。
配额的**判定**同样在下游：本项目只提供 §4.9 的执行面（可选能力、默认关闭）——接收下游下发的
剩余额度、用尽时停止为该计费身份转发，而计费周期语义与持久账本仍由 collector 承担。
也不承诺“进程崩溃也不丢一个字节”——尾账按未闭合窗口审计，与 `shadowsocks-rust-plus` 一致。
宿主形态只覆盖 `run` 子命令，不支持 `daemon`、libbox 与 1.14 的 `boxdd`。

**与 `shadowsocks-rust-plus` 的分工**：同一批 Shadowsocks 流量只能由其中一个实现承载。
纯 SS 节点继续使用 `shadowsocks-rust-plus`；需要 VLESS 与 SS 混合的节点整体切到本项目，
使一个节点只产出一份快照。迁移窗口内若必须在同一主机并存，两者必须使用不同的 `node_id`
（§5.1 的基线键以 `node_id` 打头，否则 inbound tag 命名空间会跨实现串号）。两者的 SS 配置
纪律是同构的：固定 EIH method、要求具名用户、禁止与各自的动态管理面并存。

两者的快照可由三重标志区分，并列生效：本项目是 `GET /v2/snapshot` 与 `schema_version: 2`
（`shadowsocks-rust-plus` 是 `/v1/snapshot` 与 `1`），两者使用不同的 `node_id`，以及不同的 socket 路径。
误指到本项目的旧采集器会先在路由上拿到 404，即使直接打到 `/v2/snapshot` 也会在版本号处硬失败——
`shadowsocks-rust-plus` 的三个校验器都硬编码要求 `schema_version == 1`。因此下游若已对接该实现，
其 `schema_version` 约束与触发器须先放开为同时接受 `2`，这是接入本项目的硬前置。

**验收定义**：在钉定的上游版本上，选中的 inbound 对所有允许流量都有可验证的非空计费身份；
四向字节 oracle 误差为 0；快照接口通过 §8 的故障矩阵；下游按 §5 差分入账时不出现漏计、
重复计费或静默降级。

本目录当前只有本计划书。第一步（里程碑 1）产出的骨架为：

```text
sing-box-plus/
├── README.md
├── upstream.lock                 # 钉死 repository / tag / commit / tree sha256 / go 最低版本
├── .env.example                  # UPSTREAM_REPOSITORY、GOMODCACHE、SING_BOX_BUILD_TAGS 占位
├── cmd/sing-box-plus/            # 自有 main：复制的 CLI 骨架 + 最小 registry + tracker 注入
├── scripts/prepare-source.sh     # 按精确 commit 取源码并校验
├── scripts/verify.sh             # go vet / go test -race / lint / 敏感信息扫描 / 复制文件漂移门禁
└── docs/UPSTREAM_BASELINE.md     # 基线、编译验证记录与升级规则
```

在骨架落地并冻结基线之前不写 tracker 代码——§9.2 的升级规则要求所有实现都绑定到一个已记录的
上游提交。

## 2. 前提与约束

### 2.1 可以依赖的上游能力

- **认证后的用户身份已经统一投影**。VLESS 与 Shadowsocks 的具名多用户 inbound 在认证完成后
  把非空 `users[].name` 写入 `metadata.User`（`protocol/vless/inbound.go:179`、`:197`；
  `protocol/shadowsocks/inbound_multi.go:170`、`:193`）。REALITY 握手与 Vision 解码都不会抹掉该身份。
- **统一的 tracker 挂载点已经导出**。`adapter.Router.AppendTracker` 让外部在路由选出 outbound 之后
  包装连接（`route/route.go:170` TCP、`:302` UDP），上游自己就用它挂 trafficcontrol 与 v2ray tracker
  （`box.go:249`、`:438`——全树仅此两处）。因此**不需要逐协议插桩**。
- **计数原语与正确的计数点已经存在**。sing 的 `bufio.Copy` 会解包 counter wrapper 并在目标写成功后
  调用双方计数 callback，Linux splice 路径也按成功传输字节调用 counter。
- **进程级 registry 可由调用方持有**。`box.Context(...)` 与 `box.New(box.Options{...})` 均已导出，
  自有 main 可以自建并长期持有 registry，跨 Box 重载复用（§4.4、§4.7）。
- **VLESS 具备热用户更新的底层能力**（`sing-vmess vless/service.go:40 UpdateUsers`），
  为将来可能的用户管理接口留有余地（§11 D5）。

### 2.2 不能依赖的部分

内置 Experimental V2Ray StatsService 可以做观测验证，但不能承担结算：

- 只有 uplink/downlink 两项，TCP、UDP、XUDP 合并；
- key 不含 inbound tag，同名用户跨 inbound 会合并；`stats.users` 是静态白名单，新用户不自动加入；
- counter 在该用户**首次被路由**时懒创建（`experimental/v2rayapi/stats.go:214-222`），与是否已传输
  字节无关，`not found` 只意味着“本进程启动后从未路由过该用户”，采集端必须按 0 处理；
- `GetStats/QueryStats(reset=true)` 用 `Swap(0)`，存在“已清零但响应丢失”的永久漏账窗口；
  多 counter 也不是事务快照，map 顺序不稳定；
- 数值是 `atomic.Int64`，可回绕为负；无运行周期、代次与持久化；
- gRPC listener 使用 insecure credentials，无认证无 TLS；
- 线路上的服务名被硬编码覆写为 `v2ray.core.app.stats.command.StatsService`（`stats.go:21`），
  不是 proto 声明的包名，用后者调用会得到 `unknown service`；
- 默认不在构建产物中，必须追加 `with_v2ray_api`。

其余接口也都不能替代账本：

| 接口 | 用户维度 | TCP/UDP 字节 | 生命周期/持久化 | 判断 |
| --- | --- | --- | --- | --- |
| V2Ray StatsService | 有，静态白名单 | 合并 | 无 | 可观测，不可结算 |
| Clash API | 无用户累计 | 仅连接 network | 关闭详情有界 | 不能可靠反推 |
| `common/trafficcontrol` | 每连接内部有 user | 每连接可辨别 | 较旧关闭连接只进全局累计 | 不能恢复完整用户累计 |
| API service（1.14 新增） | 每连接有 user/network | 可从事件区分 | 事件可丢、历史有界 | UI/诊断，不是账本 |
| SSM API | 仅经其 HTTP API 添加的用户；配置 `users[]` 恒不出现 | 合并 | 可选 JSON cache | 覆盖不到 VLESS，且见下 |
| Prometheus `/metrics` | 无通用用户流量 exporter | — | — | 不能依赖 |

1.14 的 API service 不是第三套账本，而是同一连接账本经 daemon gRPC `SubscribeConnections`
的导出面。

**SSM API 与 router tracker 的分歧已量化**，两者不能互作字节 oracle：SSM tracker 在
`RouteConnection` **之前**包装（`protocol/shadowsocks/inbound_multi.go:177-181`、`:203-206`），
router tracker 在选出 outbound 之后。实测（v1.14.0 回环）纯 TCP ping/pong 两侧一致（各 4/4）；
一旦走 UoT，同一段 4 字节 UDP 往返 router tracker 记 `udp 4/4`，SSM `/stats` 记
`tcpSessions:1, udpSessions:0, uplinkBytes:21, downlinkBytes:13`——network 归类相反，且字节含
UoT framing。被 `reject` 的连接会在 SSM 侧再留下一个 0 字节的幽灵 `tcpSession`。
更根本的是 SSM 的用户表只装经其 HTTP API 添加的用户（`service/ssmapi/user.go`），配置 `users[]`
定义的用户在 `/users` 与 `/stats.users` 中恒为空数组；而一旦经 SSM API 增删用户，
`MultiInbound.UpdateUsers` 会用 SSM 名单**整体替换** `users[]`（`inbound_multi.go:125-138`），
配置里的用户被踢掉。因此对静态配置的多用户 SS，SSM 侧连每用户维度都给不出。

SSM 的存储语义同样不复用：`cache_path` 每分钟保存但崩溃丢最近一分钟，`clear=true` 与
V2Ray `reset=true` 同样有漏账窗口，删除用户即丢 counter map，cache 与凭据同文件且按 `0644`
创建（`service/ssmapi/cache.go:80`），直接覆盖而非事务提交。

### 2.3 结构性限制

以下限制由上游结构决定，设计必须绕开或显式声明：

- **重启没有平滑窗口**。SIGHUP 循环是先取消再关旧、然后才建新（`cmd/sing-box/cmd_run.go` 的
  `run()`：`cancel()` → `instance.Close()` → 才回到 `create()`），`Box.Close()` 关闭 connection
  manager 后 `CloseAll()` 遍历强杀全部在途连接（`route/conn.go`）；仓库内没有 SO_REUSEPORT 或监听
  fd 传递，端口必然有短暂无监听窗口。上游没有任何排空语义。
- **StatsService 属于 Box，重载即丢未采集增量**，见上游 issue
  [#4059](https://github.com/SagerNet/sing-box/issues/4059)（2026-04-19 开启，同日以 not planned 关闭）。
  由此推出一条对本项目同样成立的纪律：**任何“改用户就重写配置并重载”的方案都会清零内存计数**，
  用户增删因此不能与计量实现耦合（§4.3、§11 D5）。
- **部分数据面对 tracker 不可见**：
  - `reject` 与 `hijack-dns` 两个规则动作在 `route/route.go:140-149`（TCP）、`:276-283`（UDP）
    处理并 return，早于 `:170` / `:302` 的 tracker 循环。**绕过 tracker 的规则动作恰好只有这两个**：
    `matchRule` 只在动作为 route / reject / hijack-dns / 带 outbound 的 bypass 时选中规则，
    其中只有前述两者提前 return；`direct` 动作在 1.14.0 的路由规则里从不被消费，是静默 no-op，
    流量仍落到默认 outbound 并被计量。1.14 另有一条隐式 DNS 劫持路径，但 TCP 与 UDP 两侧都由
    `metadata.InboundType == C.TypeTun` 门禁（`route/route.go:101`、`:240`），TUN 不在计费白名单内。
  - REALITY 握手校验失败时的伪装中继在 utls 内部完成双向 `io.Copy`
    （`metacubex/utls reality.go:326,424,542-547`），不经 inbound/router；sing-box 未设
    `LimitFallback`，该中继不限速；
  - ShadowTLS 的握手/校验失败流量整条中继到 handshake 服务器，同样不经 router。
- **tracker 接口有三个方法**：`RoutedConnection`、`RoutedPacketConnection` 与
  `RoutedFlow(...) tun.FlowTracker`（`adapter/router.go:104-108`；1.13.x 只有前两个）。
  第三条路径只在 TUN/L3 形态可达、口径是 IP 包全长且 `metadata.User` 恒空。
- **失败关闭只能阻断负载，阻断不了拨号**。`ConnectionManager.NewConnection` 先拨号
  （`route/conn.go:101-105`）后 copy，tracker 位于两者之间，因此拒绝一条 TCP 连接时目的地仍会
  看到一次完成握手、0 字节的连接（UDP 无此问题）。

### 2.4 可计费的 inbound 范围

统计模式按 inbound 类型白名单启用。“`metadata.User` 非空即可计费”不是有效兜底——
Shadowsocks relay 就是反例。

| inbound | 身份可用性 | 处置 |
| --- | --- | --- |
| VLESS + REALITY/Vision | 认证后写入 `metadata.User`（`protocol/vless/inbound.go:179`、`:197`） | **首期支持**；REALITY 伪装中继除外（§2.3） |
| Shadowsocks multi（`users[]`，仅 `2022-blake3-aes-128-gcm` / `-aes-256-gcm`） | EIH 认证后写入 `metadata.User`（`inbound_multi.go:170` TCP、`:193` UDP；`name` 为空时保持空串） | **首期支持**；要求每个 `users[].name` 非空、inbound 内唯一，uPSK 归一化后两两不同，且该 inbound 的 tag 不出现在任何 `ssm-api.servers[]` 中（§4.6） |
| Shadowsocks multi（legacy AEAD：`aes-*-gcm`、`(x)chacha20-ietf-poly1305`） | TCP 可信，**UDP 不可信** | **不支持**：UDP NAT 表按客户端源 `addr:port` 建键，用户身份只在建表那一次写入，同一源端口上的后续用户全部计入第一个包的用户（实测：u1/u2 各发 4 字节，tracker 只有 `u1 udp up=8 down=8`，u2 无记录）。`ListenOptions` 无 network 选择器，无法只保留 TCP，故整类在配置校验阶段拒绝 |
| Shadowsocks 单用户（只有 `password`；含 `method: none`） | 走 `Inbound` 分支，全程不写 `metadata.User`（`protocol/shadowsocks/inbound.go`） | **不可按用户计费**，配置校验拒绝（判据：`len(users)==0 && !managed && len(destinations)==0`，三分派见 `inbound.go:33-44`） |
| Shadowsocks relay（`destinations[]` 非空） | `metadata.User` 是 `destinations[].name`（`inbound_relay.go:130`、`:150`） | 是中继目的地不是终端用户，**且字节口径不是应用 payload**——relay 只覆盖 EIH 块后整段转发密文，router tracker 计的是盐+密文+AEAD tag+padding。实测同一次 4 字节往返，末端恒为 `up=4 down=4`，relay inbound 上 `down=79`，`up` 因 SS2022 短 payload 的随机 padding 每次不同（5 次实测 127–900）。**不可按用户计费**，配置校验拒绝 |
| Shadowsocks managed（`managed: true`） | 启动时零具名用户，用户集由 SSM 运行时整体替换 `h.users`（`inbound_multi.go:125-138`） | **不支持**：与 §4.3 的稳定 lineage 及“用户变更走受控重启”直接冲突；且该替换与数据面读取无锁（写 `:132` / 读 `:170`）。配置校验拒绝 |
| VMess、Trojan | 具名用户可传播 | 后续候选，需集成测试；fallback 路径单独确认 |
| Hysteria/Hysteria2、TUIC | 协议层有用户概念 | 后续候选，需验证 QUIC stream/datagram 分类，且要加回 `with_quic`（§9.1） |
| Naive、AnyTLS、HTTP/SOCKS/Mixed 认证 | 有认证用户名的路径可传播 | 后续候选，逐协议确认匿名与 fallback |
| ShadowTLS | `users[].name` 只用于握手校验 | `InboundDetour` 跳转发生在 tracker 之前（`route/route.go:72`，tracker 循环在 `:170`），User 被 detour 的 `UpstreamMetadata` 丢弃；计费身份只能取 detour 末端的 SS 用户（单用户 SS 则为空，应拒绝） |
| TUN、redirect、tproxy、direct | 无认证用户 | 不支持按用户计费 |

计费模式的 Shadowsocks method 白名单**硬编码**为 `2022-blake3-aes-128-gcm` 与
`2022-blake3-aes-256-gcm`，不要写成“`shadowaead_2022.List` 中的 method”：
`2022-blake3-chacha20-poly1305` 虽在该 List 中，但 `NewMultiService` 的 switch 只放行两个 AES
变体，配 `users[]` 启动即 `invalid argument`；`method: none` 配 `users[]` 则直接
`unsupported method`（`inbound_multi.go:80-81`）。校验拒绝只是把上游的启动失败提前成一条明确报错。

通用规则：**带 `InboundDetour` 的 inbound 一律以 detour 末端 inbound 为计费身份**。
匿名、空名、认证 fallback 或 tracker 绕过不得默认为“unknown 后继续转发”；
若会影响收费，启动或连接必须失败关闭。首期两类 inbound 之间可互作对照，见 §4.2。

## 3. 术语与字段

| 概念 | sing-box | `shadowsocks-rust-plus` | 本项目 |
| --- | --- | --- | --- |
| 计费身份名 | `users[].name` → `metadata.User` | `users[].name` / `identity_name` | 快照字段 `users[].name`；正文称 billing name |
| 入站标识 | inbound `tag` | `server_id` | 快照字段 `inbounds[].tag`；协议类型另出 `inbounds[].type` |
| 节点标识 | — | `node_id` | `node_id`，配置项，需在部署内全局唯一 |
| 运行周期 | — | `runtime_id`（32 位小写 hex，进程级随机） | 同左 |
| 快照序号 | — | `sequence`，每次快照请求严格递增 | 同左 |
| 代次 | — | `generation`，同名重激活复用、不递增 | 同左，固定输出 `1`，但不得从采集键中省略 |

计费身份名、`node_id`、inbound `tag` 均为非空、最多 128 字节、每字节为 ASCII 可显示非空白字符；
`tag` 在节点内唯一，billing name 在该 inbound 内唯一。

上游没有“用户 id”概念：VLESS 与 Shadowsocks 的用户对象都只有 `name` 加一份凭据
（`option/vless.go:12-13`、`option/shadowsocks.go:15-16`），`metadata.User` 装的就是该 `name`。
注意上游 `user` 一词有两义，不得混用：路由规则的 `user`（`option/rule.go:167`）匹配的是**进程属主
用户名**，代理认证用户在规则侧叫 `auth_user`。本项目快照统一用 `users[].name`，与配置同名。

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
    InboundTag  string
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
  “目标写成功后”计数，否则回落为“源读成功”计数。本项目的 tracker 由自有 main 在 `box.New()`
  之后追加，因此**恒定是包装链最外层**（上游两处 `AppendTracker` 都在 `box.New` 体内），
  必须实现 `Upstream()` 与 `Reader/WriterReplaceable()`，否则口径会静默退化。实测同时启用
  clash_api 与 v2ray_api 时，本 tracker 收到的解包链为
  `*bufio.CounterConn -> *trafficcontrol.connTracker -> …`；§8 有对应断言测试。

### 4.2 计数口径

采用应用 payload 口径，与 `shadowsocks-rust-plus` 一致：

- TCP uplink：协议解码后的用户 payload 被通用转发边界成功写入目标侧的字节；
- TCP downlink：目标侧 payload 被代理编码 writer 接受、准备回传给用户的逻辑字节；
- UDP uplink/downlink：完整逻辑数据报成功交给另一侧后累计 payload 长度；
- 不统计外层 REALITY/TLS、VLESS/SS header 与 AEAD 分块、Vision padding、mux/XUDP/UoT framing、
  TCP/IP header 或重传；
- XUDP、UDP-over-TCP 和 mux 内的逻辑 UDP 归入 UDP，不因底层 carrier 是 TCP 而归入 TCP。

已实测（v1.14.0 回环，router tracker 口径）：4 字节往返 VLESS 与 SS-2022 均为 TCP 4/4、UDP 4/4；
100×64 KiB 大数据往返两协议均为 6,553,600 / 6,553,600，SS-2022 的 salt/EIH/AEAD 分块与 VLESS
header、XUDP framing 均不计入——**VLESS 可作为 SS 的口径 oracle**。`udp_over_tcp` 与 h2mux
（inbound+outbound 同开）在两档下同样精确，逻辑 UDP 仍归入 UDP、`metadata.User` 保持具名：
SS inbound 的路由链是 `mux.Router → uot.Router → router`，UoT 承载的数据报在 `common/uot/router.go`
转投 `RoutePacketConnection`，mux 子连接经 `adapter.WithContext` / `ExtendContext` 继承 metadata。
该结论仅覆盖无 TLS/REALITY/Vision 的裸链路；Vision 及 splice 分支仍按下段矩阵对账。

可复用的上游计数路径：

- [TCP counter wrapper](https://github.com/SagerNet/sing/blob/7c349dacf402256d3a7029746073b05d2ead584a/common/bufio/counter_conn.go)
- [packet counter wrapper](https://github.com/SagerNet/sing/blob/7c349dacf402256d3a7029746073b05d2ead584a/common/bufio/counter_packet_conn.go)
- [通用 copy](https://github.com/SagerNet/sing/blob/7c349dacf402256d3a7029746073b05d2ead584a/common/bufio/copy.go)
- [Linux splice](https://github.com/SagerNet/sing/blob/7c349dacf402256d3a7029746073b05d2ead584a/common/bufio/splice_linux.go)

两点已核实的口径细节：

- **Vision 不进入 splice**。`VisionConn` 不实现 `Reader/WriterReplaceable`，也不暴露 `syscall.Conn`，
  sing 的 unwrap 停在该层，只走用户态 direct 读写。因此对账项写作
  “buffered → direct（用户态直读 netConn）切换”，splice 对账针对无 `flow` 的 VLESS、
  裸 TCP 等可解包到 `syscall.Conn` 的入口。
- **Vision padding 不计入**。padding 在 `VisionConn.Read/Write` 内剥离，counter 在其外层。

实现前需完成字节对账矩阵：Vision buffered/direct 切换与 early data、DNS hijack、特殊 outbound
直接读写、UDP batch、连接取消与 half-close、重启期间仍存活的长连接（mux、XUDP/UoT 已在裸链路下
完成最小往返与 6.4 MB 两档对账，余下按本矩阵补齐），以及 **TLS 入站的 badtls read-wait 路径与
普通 `tls.Conn` 回退路径各一遍**（见 §9.1 `badlinkname`）。任何绕过标准 router tracker 的 handler
都必须明确选择“计数、拒绝或声明不支持”，不能静默转发但漏计。

计费用 inbound 不得配置 `hijack-dns` 或以 `reject` 结束的规则动作（§2.3），配置校验须拒绝。

### 4.3 身份生命周期

同一 runtime 内，inbound tag 与 billing name 构成**稳定计费身份**：

1. 删除或停用只把 `active` 切为 `false`，记录保留在快照中；
2. 同名重建复用原 generation 与原计数器，并把 `active` 切回 `true`；
3. 凭据轮换不改变 lineage；
4. `generation` 作为 schema 保留维度固定输出 `1`，但不得从采集键中省略；
5. lineage 数达到 `max_identities` 时：**启动期超限即失败关闭；SIGHUP 期超限则置
   `health.identity_limit_reached`**，两种情形都不得丢弃 lineage。该位粘滞，置位后该 runtime 余下
   时间全部不可入账（代理仍在转发但不计费），因此 SIGHUP 路径同样属于必须告警的严重状态；
6. 同一 runtime 内不得把已用名称重分配给不同计费用户；需要改变归属时使用新名称，
   或通过受控重启进入新 runtime。

用户集合的变更（增、删、停用、凭据轮换）在本项目首期通过修改配置并受控重启生效，走 §5.3 的流程。
`active` 的切换不是自动的：registry 跨重载存活时，从配置中移除的身份仍会被原样输出，
必须在每次重载时按新配置对账后才切 `active=false`（§4.4）。是否提供独立的热更新接口见 §11 D5。

### 4.4 运行周期与重启语义

registry 由自有 main 持有（§4.7），因此它的生命周期是**进程级**而非 Box 级：`runtime_id` 在进程
启动时生成，同一进程内累计值单调不减；SIGHUP 重载后计数自然延续（已实测：重载前后同一用户
`tcp_down` 从 613 累加到 1226，未清零）。进程重启才产生新 `runtime_id`、计数从零开始，
采集端据此识别周期边界（§5）。

上游重启会强制关闭全部在途连接（§2.3）。由于计数按每次 copy 迭代增量累加，**强杀不丢已计字节**，
但会打断用户连接，且已计而未采集的增量会随进程消失——这正是 §5.3 的计划重启流程要消除的窗口。

**信封字段的持有者是硬约束。** `runtime_id`、`started_at_unix_ms`、`sequence` 三者必须由自有 main
在进入 run 循环**之前**生成，并由 main 持有的进程级 registry 保存；`services[]` 中的 `user_stats`
实例只负责监听 UDS、读取 registry 并序列化，**不得自行持有或初始化这三个值**。理由：SIGHUP 在同一
进程内重建整个 Box 与全部 service 实例，挂在 service 上的状态每次重载都会重置。三种错误拆法的后果
依次递减：仅 `sequence` 归 1 → 采集端按 §5.1“`sequence` ≤ 已处理值即丢弃整份快照”**静默丢弃**，
既不报错也不告警，直到序号追平旧最高值才恢复（窗口内进程若退出即为永久丢账）；`runtime_id` 一并
重置 → 采集端转入首快照策略，静默丢失重载窗口那一段增量；仅 `started_at_unix_ms` 重置 → 采集端
会抛错，是三者中唯一可审计的一种。

仍需自行实现的部分：

1. **重载不变量校验**：在重载的配置检查阶段比对 `node_id` / `listen_path` 与进程初值，
   不一致即拒绝本次重载（§4.6 第 11 条）；
2. **重载对账**：每次重载后按新配置切换 `active` 与 tombstone（§4.3 第 1、5 条）；
3. **排空阶段**：若要在重启前排空，必须在 run 循环与 `Box.Close()` 之间插入 drain
   （停止 accept → 等待或超时 → 再 Close）并接管 `C.FatalStopTimeout` 看门狗；
4. **端口不中断**：仍需 SO_REUSEPORT 或 listener fd 传递，作为独立工作项，不在首期范围内。

纯内存 registry 无法恢复进程崩溃前尚未采集的尾账。若业务要求“崩溃也不丢一个字节”，
需另加 WAL 或持久计量数据面，复杂度显著上升；不得把高频轮询描述成严格保证。

### 4.5 快照接口契约

快照 schema 的 `schema_version` 固定为 `2`，路由为 `GET /v2/snapshot`。结算语义——基线键结构、
差分规则、health 闸门、错误码表与资源上限——与 `shadowsocks-rust-plus` 逐条同构，只是字段按
sing-box 的词汇命名。

字段命名分两类，不得混为一谈：

- **有 sing-box 上游依据的**：`inbounds`（`option/options.go:26`）、`tag` 与 `type`
  （`option/inbound.go:22-23`）、`listen` 与 `listen_port`（`:80-81`）、`users` 与 `name`
  （`option/vless.go:5,12`、`option/shadowsocks.go:8,15`），以及四向计数与会话数的词汇构件
  （`uplink`/`downlink`/`tcp`/`udp`/`sessions`，见 `service/ssmapi/cache.go:20-25`）。
  这些一律与上游同名同形。
- **本项目自有的计量概念**：`node_id`、`runtime_id`、`started_at_unix_ms`、`sequence`、
  `generation`、`active`、`health`。上游没有对应词汇（`generation` 在 `option/` 与 `adapter/` 下
  零命中），采用与 `shadowsocks-rust-plus` 相同的命名，使两套快照的结算实现可以共用同一套算法。

```json
{
  "schema_version": 2,
  "node_id": "node-example-01",
  "runtime_id": "0123456789abcdef0123456789abcdef",
  "started_at_unix_ms": 1787587200000,
  "sequence": 42,
  "health": {
    "counter_overflow": false,
    "sequence_overflow": false,
    "identity_limit_reached": false
  },
  "inbounds": [
    {
      "tag": "vless-entry-01",
      "type": "vless",
      "listen": "0.0.0.0",
      "listen_port": 8443,
      "generation": 1,
      "active": true,
      "tcp_sessions": 3,
      "udp_sessions": 1,
      "users": [
        {
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

**格式与排序。** `runtime_id` 为 32 位小写 hex；`started_at_unix_ms`、`sequence` 为正整数，同一
`(node_id, runtime_id)` 内 `started_at_unix_ms` 恒定。`inbounds` 按 `tag` 再按 `generation` 排序，
`users` 按 `name` 再按 `generation` 排序，一律按已校验的 **ASCII 原始字节升序**，不使用任何 locale
或大小写折叠。键顺序非规范性，采集端不得依赖；JSON body 以 LF 结尾。命名一律 snake_case——
上游按“面的种类”分风格，配置面 `option/` 与磁盘缓存面 `service/ssmapi/cache.go` 都是 snake_case，
camelCase 只出现在受外部规范约束的面（SSM 遵 SIP008、Clash API 遵 Clash），本快照两者皆非。

**四向计数保持扁平**，不采用 `traffic:{tcp:{…},udp:{…}}` 嵌套：计费口径下 network 维度闭合为
`{tcp, udp}`（由调用的 tracker 方法决定，第三取值只经已固定返回 nil 的 `RoutedFlow` 产生）；
嵌套会引入“键缺失 vs 0”的歧义，采集端每次差分都要先补齐。将来若真要加 network 类型，
按 `<network>_<direction>_bytes` 追加新键即可。

**u64 字段禁止经 IEEE754 double 解析**（四向计数、`sequence`、`started_at_unix_ms`、两个
`generation`）：wire 上是 JSON 非负整数，采集端必须以 64 位整数或十进制字符串解析并做整数运算。

`type` 为**必填**，取 `metadata.InboundType`（`adapter/inbound.go:49`），值直接是 `constant/proxy.go`
的常量字符串，首期取值集恰为 `{"vless", "shadowsocks"}`，无需映射表。它只是展示与运维维度，
**不进入 §5.1 的基线键**：协议类型是 inbound 的属性而非身份维度，同一 `tag` 换协议即属换 inbound、
由 tag 治理；入键反而引入一条故障路径——SIGHUP 重载保持同一 `runtime_id`，若某 tag 在重载中换了
协议，同一计费 lineage 会被劈成两条。注意 `type` **不编码 Shadowsocks 的子形态**：multi、relay 与
单用户三个构造函数都注册为同一个 `shadowsocks`（`protocol/shadowsocks/inbound.go:33-44`），
“只有 `2022-blake3-aes-*` 具名多用户形态在运行”由 §4.6 第 2 条保证，采集端不得从 `type` 反推。

`listen` / `listen_port` 取自 inbound 配置的同名字段（`option/inbound.go:80-81`），是**配置值而非
实际绑定结果**。上游 `listen` 是纯 host（`*badoption.Addr`），端口是独立键，因此 v2 拆成两个平级键
而不是合成 `host:port`——合成串属“同名异形”，拿快照对照配置必然误读，且 IPv6 还要方括号消歧。
配置省略 `listen` 时上游按 `127.0.0.1` 绑定而非 `0.0.0.0`，快照必须按同一默认补齐后输出。
上游不给回读实际绑定的路径（`listener` 字段在 `*vless.Inbound` 与 `*shadowsocks.MultiInbound` 上均
不导出，`adapter.Inbound` 只有 `Lifecycle` + `Type()` + `Tag()`），因此 §4.6 第 2 条要求显式配置非 0 的
`listen_port`，使配置值与实际绑定恒等。

`health` 是**闭集**：恰好三个 bool 键，出现额外键即整份拒绝；新增 health 键必须同时提升
`schema_version`。三位都是**粘滞**的——`counter_overflow`（任一 `*_bytes` 饱和加法发生截断）、
`sequence_overflow`（`sequence` 饱和）、`identity_limit_reached`（曾因 `max_identities` 或逻辑 inbound
上限拒绝过一次 lineage 注册，把 §4.3 第 5 条已有的“标记 unhealthy”分支变成可机器判定的位）。

`tcp_sessions` / `udp_sessions` 是当前活跃数的瞬时 gauge，**可减少**，不参与结算、不进基线、
不触发回退告警，供计划重启时判断是否已排空（§5.3）。

传输层：HTTP/1.1-over-Unix-stream，每连接单请求单响应，禁 keep-alive / query / body；
固定两条路由——`GET /v2/snapshot`（被接受时即推进 `sequence`）与 `GET /healthz`（不带版本段，
200/503，不推进 `sequence`）。**路径版本号与 `schema_version` 同步推进**：本项目不提供
`/v1/snapshot`，请求该路径返回 404，使误配的采集器立即失败，而不是读到半兼容的 body。
错误一律返回固定 `{"schema_version": 2, "error": {"code": …}}` 对象，错误码取值表与 `shadowsocks-rust-plus` 一致
（400/404/405/408/413/429/500/505）——错误码与字段命名正交，复用不产生冲突。
`/healthz`、全部错误 body 与快照必须**共用同一个 schema 版本常量**，由契约测试逐条断言，
不得出现 1/2 混用。

安全与资源：非破坏性累计快照；稳定排序；请求/响应大小、身份数、并发数、读写超时均有上限；
socket 默认 `0600`（可受控 `0660`），绑定前检查父目录、符号链接、旧 socket 与 inode 替换。
单个畸形、超时或超限的请求只影响该连接，不影响代理转发。
Unix socket 不得直接映射为公网监听，远程读取须经节点上的独立反向代理提供 HTTPS/mTLS/来源限制
与审计，且该代理不得缓存快照。

**与 `shadowsocks-rust-plus` 快照的差异**（供已对接该实现的下游评估迁移成本）：容器与标识按
sing-box 命名——`servers[]` / `server_id` / `inbound_type` 在此为 `inbounds[]` / `tag` / `type`，
`listen` 由 `host:port` 合成串拆为 `listen` + `listen_port` 两个平级键；`identity_kind` 不设——
sing-box 侧唯一身份来源是 `metadata.User`，写入点只有 `users[].name`，relay、单用户与 `managed`
三种非用户形态已由 §4.6 在配置期拒绝，跨版本的失败关闭由 `schema_version` 承担；`health` 多一位
`identity_limit_reached`。四向计数、信封字段与错误码表同名同义。

迁移成本因此集中在采集端的字段访问路径：下游服务表的列名（`exporter_server_id` 之类）与幂等
`batch_id` 的配方都不与 wire 字段名字面绑定，无需改列名，也无需重算历史批次；而
`shadowsocks-rust-plus` 的参考校验器不能原样复用——它们硬校验 `schema_version == 1`
与 `identity_kind`，并对 `health` 做整字典相等比较，三处都会整份拒绝本项目的快照。
契约测试因此须按本 schema 分叉，工作量见 §6。

### 4.6 配置与失败关闭

统计是硬依赖，不是可选旁路：

1. **统计配置以自有 service 类型承载**，对未知字段失败关闭、不静默回落默认值：
   `"services": [{ "type": "user_stats", "tag": "stats", "node_id": …, "listen_path": …, … }]`。
   上游 `option.Options` 对**顶层**未知字段硬失败（`option/options.go:41` 的
   `decoder.DisallowUnknownFields()`），因此顶层 `user_stats` 键在零补丁形态下不可行；
   而 `boxService.Register[Options](registry, "user_stats", ctor)` 已导出，自有字段自动获得
   “未知字段即失败”与 `format` 往返保真（均已实测）。
2. 启用统计时，逐 inbound 校验并在任一不满足时**启动失败**，不以部分覆盖或零归属模式运行：
   - 类型在 §2.4 白名单内（首期 `vless`、`shadowsocks`）；
   - inbound `tag` 非空且在节点内唯一；
   - 用户集非空，每个 billing name 非空且在 server 内唯一；
   - Shadowsocks 必须是 `2022-blake3-aes-128-gcm` / `-aes-256-gcm` 的多用户具名形态：
     单用户 `password` 形态、`destinations[]` relay 形态、`managed: true` 一律拒绝；
   - Shadowsocks 每一项 `users[].name` 都非空——上游在 `name == ""` 时只把索引写进日志、
     `metadata.User` 保持空串，且 `newMultiInbound` 全程不校验 `name`。“至少一个具名用户”
     覆盖不到“多用户中某一项无名”；
   - Shadowsocks `users[]` 必须按**归一化后的 uPSK 字节**两两不同，而不是按 `password` 字符串
     比较：上游 `UpdateUsers` 以 `uPSKHash[hash]=user` 覆盖建表且无冲突检测，重复 uPSK 会把两个
     计费身份静默合并到配置中靠后的那一项（已实测）。归一化须复刻上游的 `base64.StdEncoding`
     解码（非 strict）；实测 `AAAAAAAAAAAAAAAAAAAAAA==` 与 `AAAAAAAAAAAAAAAAAAAAAB==` 解码为同一
     16 字节 key，身份同样合并。超长 uPSK 也须拒绝：上游会派生密钥并启动成功，但任何标准客户端
     都连不上（outbound 报 `bad key length`），属静默不可用配置；
   - `listen_port` 必须显式配置且在 `1..=65535`：为 0 或缺省时上游把 0 交给内核分配临时端口，
     且同一 inbound 的 TCP 与 UDP 会拿到**两个不同**的端口，单一 `listen_port` 字段连表达都做不到；
     而零补丁 wrapper 无法回读实际绑定端口，快照必然与实际不符，属静默错报；
   - `listen.detour` 非空时拒绝；若确需支持，须按 §2.4 通用规则显式声明以 detour 末端 inbound
     为计费身份；
   - 顶层 `route.rules[]` 中任一 `action` 为 `hijack-dns` 或 `reject` 时拒绝。需同时读
     `DefaultRule` 与 `LogicalRule` 的 `RuleAction.Action`；**不需要递归遍历** `logical` 的
     `rules[]`——上游在反序列化期就拒绝嵌套规则携带任何 action 键，动作只可能出现在顶层；
   - `network_namespaces` 非空时拒绝：上游把 netns holder 实现为隐藏子命令，自有 main 不复刻
     该子命令（§4.7）。
3. 未编译统计 build tag 却出现该配置、或在非 Unix 平台使用，都必须明确报错。走 `services[]` 时
   这一条部分免费：未编译 `with_user_stats` 时该类型未注册，解码直接失败（上游文案把 service 误写
   成 "inbound"，须替换为可读错误）。
4. exporter 的父目录、lockfile、遗留 socket 或 bind 检查失败时进程启动失败。
5. 启动后 exporter 与数据面同受监督：exporter 任务意外退出、panic 或连续 `accept()` 失败时，
   整个进程以失败退出，避免“代理仍在转发但统计已消失”。
6. 未配置 `user_stats` 时不创建 registry、exporter 或任何附加包装，保持上游快路径与线协议不变。
7. **与上游连接账本互斥。** 启用统计时，配置中出现下列任一项即启动失败：`services[]` 中
   `"type": "api"`（`box.go:166-168` 置 `needAPIService`，`box.go:249` 会
   `router.AppendTracker(trafficManager)`，与本项目 tracker 叠加）；`experimental.clash_api`；
   PoC tag 集下 `experimental.v2ray_api.listen` 非空（`box.go:438`）。全树仅 `box.go:249` 与
   `box.go:438` 两处上游 `AppendTracker`，校验需覆盖且仅需覆盖这两条路径。
   **裁 tag 挡不住这一条**：`common/trafficcontrol` 被 `box.go:24` 无条件 import，
   `service/api` 在 `include/registry.go:146` 无 build 约束地注册，实测无任何 build tag 的二进制
   对带 `services:[{type:api}]` 的配置 `check` exit=0。
8. **与 SSM API 互斥，两层实现、有先后顺序。**
   - **第一层（registry，首选）**：`ssm-api` 在 `include/registry.go:148` 无条件注册，
     D4 的 tag 裁剪排除不掉它。自有 main 传入自建的 service registry、**不注册** ssmapi，
     使含 `ssm-api` 的配置在**解析期**即失败关闭。若要输出可读错误，须在解码前先扫描原始 JSON 的
     `services[].type`，不要依赖上游错误文本做匹配。
   - **第二层（配置校验）**：解析成功后遍历 `options.Services`，命中 SSM 类型即失败，并把其
     `Servers` 的 inbound tag 列进错误信息。**不得改用“inbound 上有 `managed: true`”作为判据**：
     `service/ssmapi/server.go:77-82` 只做 `adapter.ManagedSSMServer` 类型断言后 `SetTracker`，
     从不读 `Managed` 字段——普通 `users[]` 的 SS inbound 同样会被挂上 SSM tracker。
   - 两层是先后关系而非互为兜底：解析失败时 `options.Services` 为空，第二层对该输入不可达；
     第二层的价值在于 registry 日后若因别的需要重新纳入 ssmapi 时仍能拦截。
   - （对应 `shadowsocks-rust-plus`“manager 模式不得与 `user_stats` 同时启用”的同一条纪律。）
9. **运行期失败关闭在 tracker 内实现，不需要改上游。** TCP 与 UDP 的 tracker 包装都在
   “已选定 outbound、尚未交给 outbound handler”之处（`route/route.go:170-172` 之于 `:173-177`；
   `:302-304` 之于 `:308-312`），UDP 首包此时已搬进 `bufio.CachedPacketConn` 但一个字节都还没外发。
   因此计费 inbound 上 `metadata.User` 为空时，直接 `Close()` 并返回立即报错的包装，即可做到
   “未取得计数器就不转发”。已实测：匿名 TCP 与 UDP 均 0 字节应用负载外发、快照中无任何记录，
   同进程内带认证连接四向计数不受影响。**两点已知差异须写进 `docs/OPERATIONS.md`**：
   (a) 客户端看到的是“连接建立后被重置”而非协议层认证失败——tracker 位于握手之后；
   (b) TCP 仍会向目的地拨号（§2.3 末条）。因此与 `shadowsocks-rust-plus`“UDP 首包未取得计数器即
   失败关闭”的等价性**仅限计量口径**（0 字节、不入账），不等价于“不建立到目的地的连接”。
启用 §4.9 配额闸断时，本条的判据扩展为“`metadata.User` 为空**或**该 lineage 的剩余额度已耗尽”，
实施位置与上述两点已知差异均不变。
10. **不得把上游 `sing-box check` 当作配置门禁的唯一实现。** 实测 v1.14.0 在 `ssm-api` 的
    `servers` 键缺前导 `/` 时会 panic 退出（exit=2，`panic: chi: routing pattern must begin with '/'`），
    而非返回可读配置错误。本项目的全部校验必须在 `box.New()` 之前独立完成，并对 `check` 的
    非 0/非 1 退出码单独告警。
11. **重载不变量。** SIGHUP 会重新读取并合并配置。`user_stats` 的 `node_id` 与 `listen_path` 必须与
    **进程启动时**的取值逐字节相同，任一变化即在重载的配置检查阶段判定失败：**拒绝本次重载、
    保留旧 Box 继续服务并告警，不得以进程退出处置**——上游在 `check()` 失败时是记错误后 continue，
    此时尚未 `cancel()` 与 `Close()`；改成退出反而会丢掉未采集的尾账。理由：采集端以
    `(node_id, runtime_id)` 为 runtime 键，同一 `runtime_id` 内换 `node_id` 会一次性重置全部采集守卫，
    并按首快照策略产生两种错账（`include` 全量重复入账、`baseline` 丢一个采集区间）；换 `listen_path`
    则使采集端直接失联。需要更换这两项必须走 §5.3 的完整计划重启。启用 §4.9 时，
    `quota_control.listen_path` 同受本条约束——控制面失联的后果与换 `listen_path` 同型。

被统计的 inbound tag 名单写在 `user_stats` 服务配置里。另一条可行但**不采用**的路径是覆盖
inbound 注册表给单个 inbound 加自有字段（`adapter/inbound/registry.go` 的 register 是纯 map 赋值，
重复注册静默生效，`vless.NewInbound` / `shadowsocks.NewInbound` 均已导出；已实测可行）：
它会把上游构造函数签名变成必须逐版本跟随的接口，且覆盖后本二进制会接受上游 `check` 拒绝的配置。
两者只选其一，无论用哪种方式选中 inbound，都仍须通过本节第 2 条的全部校验。

### 4.7 overlay 形态：零补丁 wrapper

交付物是一个独立 Go module，`go.mod` 中 `require github.com/sagernet/sing-box v1.14.0`，
自有 main 位于 `cmd/sing-box-plus`，**不修改上游任何源码文件**；`go.sum` 与 `upstream.lock` 的
commit 双重固定版本。已在 v1.14.0 / go1.26.5 上实测：自有 module 能完成
“构造 ctx → 解码配置 → `box.New` → `AppendTracker` → `Start` → 信号循环 → `Close`”全链路，
tracker 在真实转发中被调用、TCP 上下行计数口径正确；darwin/arm64 本机跑通，
linux/amd64 `CGO_ENABLED=0` 交叉编译通过。

自有 main 的**必须**骨架：

```go
// 1. registry 必须先进 ctx —— option.Options 的解码依赖它（inbounds[]/services[] 都靠 registry 解 union）
//    不能用 include.Context()：它内联自建全部 registry，调用方拿不到句柄，
//    既无法注册自有 user_stats 服务类型，也无法剔除 ssmapi（§4.6 第 8 条）。
serviceRegistry := boxService.NewRegistry()          // 不调用 ssmapi.RegisterService
userstats.RegisterService(serviceRegistry)           // 自有 services[] 类型，见 §4.6 第 1 条
globalCtx := box.Context(context.Background(),
    minreg.InboundRegistry(), minreg.OutboundRegistry(), minreg.EndpointRegistry(),
    minreg.DNSTransportRegistry(), serviceRegistry, minreg.CertificateProviderRegistry())

// 2. 读配置 + §4.6 的失败关闭校验（此时尚未建 Box，拒绝启动的成本最低）
options, err := json.UnmarshalExtendedContext[option.Options](globalCtx, content)
must(validateForStats(options))

// 2'. 进程级统计 registry —— 必须在 run 循环之外创建、跨 SIGHUP 复用（§4.4）
//     runtime_id / started_at_unix_ms 在此刻同时捕获，sequence 与四向计数也由它持有
statsRegistry := userstats.NewRegistry(nodeID)
globalCtx = service.ContextWith(globalCtx, statsRegistry)   // user_stats service 从 ctx 取，禁止自建

// 3. 建 Box —— Router 在 box.New 内部创建并注册
ctx, cancel := context.WithCancel(globalCtx)
instance, err := box.New(box.Options{Context: ctx, Options: options})

// 4. 注入 tracker —— 必须在 Start() 之前
instance.Router().AppendTracker(userstats.NewTracker(statsRegistry, logger))

// 5. Start / 信号循环 / Close，并复刻 C.FatalStopTimeout 看门狗
```

还必须复刻上游 `create()` 内那段仅在 `Start()` 期间生效的临时信号处理（否则启动阶段 Ctrl-C 无响应），
以及 `closeMonitor`（`C.FatalStopTimeout` = 10s，否则 `Close()` 卡住时进程静默挂起）。
**可省**：`filemanager.WithDefault`（只服务 `SUDO_USER` 场景）、`-D` 与 `--disable-color`、
多配置 merge、`NetworkNamespaceHolderArgs` 与 `runInUserNamespaceIfNeeded`（省略等于不支持
`network_namespaces` 与 Linux rootless，校验期必须显式拒绝），以及
`merge`/`generate`/`tools`/`rule-set`/`geoip`/`geosite`/`api`/`schema` 子命令。
`deprecated.NewStderrManager` 建议保留——最小 registry 已丢掉“已移除类型”的友好报错桩，
弃用告警不宜再丢。

**注入点时序是硬约束。** `route.Router.AppendTracker` 是无锁 `append`（`route/router.go:272`，
`trackers` 字段无配套锁），而数据面在 `route/route.go:170` 与 `:302` 无锁遍历该切片；
因此“`Start()` 之前追加”不是风格偏好而是正确性约束——`Start()` 之后追加已实测触发数据竞争。
在 main 里追加（而不是在自有 service 的构造函数里）另有一个好处：上游两处 `AppendTracker` 都在
`box.New` 体内，而 services 构造夹在两者之间；只有在 main 里 New 之后追加，本项目的 tracker
才恒定是包装链最外层（§4.1）。

**CLI 兼容只能靠复制，不能靠 import。** `github.com/sagernet/sing-box/cmd/sing-box` 是
`package main`，Go 禁止导入。该目录共 100 个 `.go` 文件；`run`/`check`/`format`/`version`
的最小依赖闭包是其中 **9 个、608 行**：

| 文件 | 行数 | 说明 |
| --- | --- | --- |
| `cmd.go` | 74 | `mainCommand`、持久 flag、`globalCtx` |
| `main.go` | 11 | `mainCommand.Execute()` |
| `cmd_run.go` | 232 | run 循环与 SIGHUP，tracker 注入点 |
| `cmd_check.go` | 43 | `check -c` 门禁依赖 |
| `cmd_format.go` | 77 | |
| `cmd_version.go` | 64 | |
| `cmd_netns_holder.go` | 20 | 被 `cmd_run.go` 引用，缺则编译失败 |
| `cmd_run_userns_linux.go` | 78 | 被 `cmd_run.go` 引用，缺则编译失败 |
| `cmd_run_userns_other.go` | 9 | 非 Linux 桩 |
| **合计** | **608** | |

后三个不可省：只复制前 6 个时实测报 `undefined: commandNetnsHolder` /
`undefined: runInUserNamespaceIfNeeded`。这批文件是 GPLv3 衍生物，义务见 §10；
`scripts/verify.sh` 须增加门禁——复制文件与 `upstream.lock` 所钉版本的同名文件 diff 只含已登记
改动，避免上游升级后静默漂移。

**版本字符串。** `sing-box version` 的输出取自 `constant.Version`，默认值是 `"unknown"`，
上游由 Makefile 的 `-X` 注入、版本号来自读 sing-box 自身 git tag。wrapper module 里没有 sing-box
的 git 树，**构建脚本必须从 `upstream.lock` 读取 tag 并显式注入**，否则里程碑 1 的
“`version` 显示钉定版本”不成立。跨 module 的 `-X` 对上游包路径依然有效，且与 `-trimpath` 兼容
（已实测）。自有 `version` 固定输出四行：

```text
sing-box-plus version <本项目版本>                              # -X main.Version
Upstream: sing-box <upstream.lock 的 tag> (<commit 前 12 位>)   # -X …/constant.Version + 自有 -X
Environment: <go 版本> <GOOS>/<GOARCH>
Tags: <构建 tag 集>                                             # debug.ReadBuildInfo() 的 -tags
```

上游 commit 必须由本项目自己的 `-X` 变量携带，不能指望 `debug.ReadBuildInfo()`——`vcs.revision`
指向本仓库自身的 commit，且 `-buildvcs=false` 会把该字段整个抹掉。

产出的二进制保持 `run` / `check` / `format` / `version` 的 **argv 与退出码**与上游兼容，
使既有部署与编排工具（包括发布前的 `check -c` 门禁）可以原样沿用——注意这是本项目自己的实现义务，
不是继承来的。**但配置文件不再向上游二进制兼容**：统计配置以自有 `services[]` 类型承载，
stock sing-box 会拒绝解析（实测 exit=1），`format` 同样失败。因此 `check -c` 门禁必须使用本项目的
二进制；`docs/OPERATIONS.md` 要写明“不要用官方或 Homebrew 的 sing-box 校验本项目配置”。

**引入补丁的触发条件。** 出现下列任一情形才建立 `patches/` 并按 `patches/series` 零 fuzz 重放：
(1) 需要在协议握手层表达拒绝，即客户端必须收到协议级认证失败而不是连接重置，或需要在**拨号之前**
阻断匿名连接（§4.6 第 9 条的已知差异）；(2) 需要改 vless/vision 或 splice 内部的计数点以修正口径；
(3) 上游把 `AppendTracker`、`Box.Router()` 或 `box.Context` 从导出面移除。

### 4.8 基础访问审计（可选能力，默认关闭）

回答“哪个计费身份在什么时候访问了哪个域名或 IP 的哪个端口”。**不**回答 URL、Header、证书链，
**不**提供抗篡改与抗抵赖，**不**保证事件不丢。未配置时不创建任何 goroutine、文件或包装，
数据面与 §4.1 完全一致。

**为什么不做完整审计子系统。** `shadowsocks-rust-plus` 的同名能力是 28519 行、48 文件、
独立 auditd 进程 + ingest 协议 + spool + ACK + HMAC，其中 spool 单文件 9588 行。按新增行数拆账，
约 13–14% 是“因为拆了两个进程才需要”（wire 消息、ingest 服务端、重连帧 I/O、手写严格 HTTP/1.1、
批目录与 ACK），而**真正的提交路径只有 270 行**；UDP per-target 去重窗口 750 行只买到“同 association
同目标 60 秒不重复记”一条语义，却在历次审计中贡献 5 条缺陷、内存三轮返工。它买到的不是“不丢”，
而是“丢了能被证明、能被定位、能被去重”。本项目不需要这一层保证：审计是运营辅助而非计费依据，
计费仍走 §4.5 的快照。同时该参考实现自述“没有任何一次验证证明过『真实用户流量 → access event →
写入 spool → 被 collector 取走』这条完整链路”，其 producer 侧唯一的链路测试跑在内存管道上——
**规模不等于置信度**，本节因此把验收前置为硬门禁（见末段）。

**记录形态。** 每条成功访问一行 JSONL，追加写入独立文件；不进快照、不走 UDS：

```json
{"seq":10241,"ts":1787587200123,"node":"node-example-01","run":"0123456789abcdef0123456789abcdef","user":"u_example_01","in":"vless-entry-01","net":"tcp","host":"example.com","host_src":"sniff","port":443,"up":5120,"down":81920,"ms":2317}
```

| 字段 | 含义 |
| --- | --- |
| `seq` | writer 侧单调递增序号，进程内唯一；与 §4.5 的快照 `sequence` 是两个独立命名空间 |
| `ts` | 连接关闭时刻，Unix 毫秒，**墙钟**（见纪律 11） |
| `node` / `run` | 取 §4.4 的 `node_id` 与进程级 `runtime_id`，与快照同源 |
| `user` / `in` | 计费身份名与 inbound tag，语义同 §3 |
| `net` | `tcp` / `udp` |
| `host` | `metadata.Domain` → `Destination.Fqdn` → `Destination.Addr` 三级降级 |
| `host_src` | `sniff` / `fqdn` / `ip`，标明 `host` 的来源与可信度 |
| `port` | 恒取 `metadata.Destination.Port` |
| `up` / `down` | 该连接四向计数中对应方向的字节数，**原样落盘**，成功判据交由读取方复核 |
| `ms` | 连接时长 |

缺口用同一条流表达，不另开通道：`{"seq":10242,"ev":"gap","after":10241,"n":37,"reason":"queue_full"}`。
边界不确定时 `n` 写 `null`，**不得合成一个不存在的区间**。

**配置**（承载在 §4.6 第 1 条的自有 service 类型内，同样对未知字段失败关闭）：

```json
{
  "services": [
    {
      "type": "user_stats",
      "node_id": "node-example-01",
      "listen_path": "/run/sing-box-plus/user-stats.sock",
      "access_log": {
        "path": "/var/lib/sing-box-plus/access.jsonl",
        "max_bytes": 268435456,
        "max_total_bytes": 1073741824,
        "flush_interval_ms": 1000,
        "queue_size": 8192
      }
    }
  ]
}
```

域名维度需要在计费 inbound 上配 `action: "sniff"`；未配置时 `host_src` 恒为 `fqdn` 或 `ip`，
这是正常降级而非故障。

**挂点与写入路径。** 复用 §4.1 的 tracker wrapper：内层保持 `bufio.NewCounterConn`（字节不另行统计），
wrapper 自身只实现 `Close()` + `Upstream()` + `Reader/WriterReplaceable() = true`，以免挡住
splice/direct 快路径。`Close()` 只向有界 channel 投递，由单个 writer goroutine 序列化落盘。

**必须遵守的实现纪律。** 以下每一条都对应参考实现历次审计中的实际缺陷，或本项目已实测的上游行为：

1. **`Close()` 会重入。** `route/conn.go:376-380` 的 `common.Close(source, destination)` 在
   `if !done.Swap(true)` 守卫**之外**，UDP 上下行两个 goroutine 各调用一次；TCP 异常终止路径也可能
   多次。wrapper 必须用 `sync.Once` 包住投递。上游 `trafficcontrol/tracker.go` 敢裸写 `Close()` 是因为
   `manager.leave` 用 `LoadAndDelete` 幂等——**只抄 wrapper 形状不抄这道闸门，UDP 每次访问写两行**。
   清洁 TCP 路径只关一次，仅测正常关闭发现不了。
2. **成功判据是 AND 不是 OR。** TCP 要求 `up > 0 && down > 0`，UDP 只要求 `up > 0`。用
   `up + down > 0` 会把端口扫描（每个 accept 成功的端口写一次探测就断）和被 RST 的 ClientHello
   逐条渲染成“成功访问”，正是审计要排除的情形。`up`/`down` 仍原样落盘，供下游复核。
3. **写失败后必须复位。** `bufio.Writer` 一旦 `Flush` 出错即置粘滞 `b.err`，此后 `Write` 零系统调用
   返回、channel 永不满、丢弃计数结构性恒为 0——磁盘腾空也不自愈。失败路径必须
   `w.Reset(f)` 并累加 `dropped_write`。参考实现同型缺陷的结案方式是让 auditd 进程退出交给 systemd，
   本项目单进程且审计 fail-open，**没有这条退路**。
4. **一条记录一次写出。** `bufio` 在缓冲将满时按字节边界拆分，不是记录边界；这不是罕见竞态而是
   分片写的常态。写入前检查 `if len(line) > w.Available() { w.Flush() }`，flush 只切在 LF 上。
5. **启动先补残行。** 读文件末字节，非 `\n` 即补一个，防止崩溃留下的半行与重启后第一条粘连——
   否则两条记录会被下游一起丢弃。
6. **`host` 是攻击者可控字节串。** sniff 出的 SNI 无上限，实测 16KB SNI 经 `json.Marshal` 展开成
   96103 字节单行（非法 UTF-8 转 U+FFFD，6× 放大），16KB 上行换 96KB 写入。落盘前截 255 字节并
   `strings.ToValidUTF8`。
7. **归一化到此为止。** 归一化只做小写 + 去掉一个尾点，结果直接写进 `host` 字段，**不另开
   `host_norm` 键**——记录形态以本节的示例为准。**不要引入 `idna.Lookup`**——参考实现两轮栽在
   UTS #46 分支上。
8. **哨兵地址不是目标。** `sp.mux.sing-box.arpa`（`sing-mux protocol.go:34`）、
   `sp.v2.udp-over-tcp.arpa` 与 `sp.udp-over-tcp.arpa`（`sing common/uot/protocol.go:15-16`）、
   `sp.packet-addr.v2fly.arpa`（`sing-vmess packetaddr.go:8`）都是载体标记，真实目标在子流上。
   命中即跳过，不写记录。
9. **检测轮转与删除。** 每次 flush tick 用 `os.SameFile` 比对当前 fd 与路径上的 inode，不同则重开，
   否则轮转后写进已删除 inode 的黑洞。`docs/OPERATIONS.md` 须写明**不要给该目录配 logrotate**，
   由本进程按 `max_bytes` 自轮转。
10. **总量封顶且清理不得自指。** 超过 `max_total_bytes` 时删最老的已轮转文件并写一条 gap；
    但**无对象可删时只累加计数、不写 gap**——参考实现实测 20 条真事件被放大成 78 条自指 gap 记录。
11. **`ts` 是墙钟且非单调，不要在写入侧强行单调化。** 时钟回拨时 `seq` 仍严格递增，由下游按 `seq`
    排序；写入侧篡改时间戳会让记录与其他日志无法对齐。
12. **启动 fail-hard，运行 fail-open。** 启动期以最终 euid 实际 `OpenFile(0600)` 一次并
    `Chmod` + `Stat` 复核属主与权限，任一不符即**启动失败**（与 §4.6 同档）；进入运行期后，
    审计的任何写入错误、队列满或 panic 都**不得**阻断转发或触发进程退出——这与 §4.6 第 5 条
    exporter 的 fail-hard 监督相反，两者必须在代码里显式隔离，审计不得加入 exporter 的监督组。
    writer goroutine 与投递侧各设一个 `defer recover()`。
13. **关闭序不能丢缓冲。** `instance.Close()` 之后对 writer 做有界等待（1–2 秒）→ 排空 channel →
    `Flush` + `Sync` → 写一条 stop 行。**数据 channel 永不 `close`**，否则在途的 `Close()` 投递会
    panic 并打掉进程。

**不做 `fsync`。** 每条记录 fsync 会把吞吐钉死在千级/秒，而本能力的定位不要求崩溃零丢失；
取舍必须在 `docs/OPERATIONS.md` 写准确：**崩溃会丢缓冲区内未落盘的记录，且该丢失不产生 gap 行**。

**验收先行。** 与参考实现的顺序相反：先写门禁，再写功能。最小门禁——启动进程 → 发起一次
`curl` 与一次 `dig` → **停止进程后**断言 JSONL 恰好各一行且 `user`/`host`/`port`/`up`/`down` 正确；
再补三个反例（只完成握手、只上行、只收 banner）断言零记录；轮转一次后重复断言。
该门禁**必须是会失败的**：不得用 `|| true`、`-f` 或跳过分支把缺失依赖静默放行。功能写完后，
对纪律 1、2、4、5、9、10、12 各做一次变异检验（注释掉或取反），确认对应用例转红——
参考实现实测删掉修复的三行后 32 个用例仍全绿，加无条件 panic 也全绿。

### 4.9 配额闸断（可选能力，默认关闭）

回答“某个计费身份用满本周期额度之后，如何在不影响同节点其他用户的前提下停止为它转发”。
**不**回答额度从哪来、计费周期怎么划、超额之后怎么补账——判定始终在下游 collector，本节只定义
进程侧的执行面与下发协议。未配置时不创建控制端点、不加任何包装，数据面与 §4.1 完全一致。

**判定必须留在 collector，这是结构结论而非分工偏好。** 三条已定事实叠加，使进程无法自行知道
“本计费周期已用多少”：(1) 快照是**进程生命周期内**的累计，不是计费周期内的累计（§4.4）；
(2) §4.3 第 2 条规定同名重建复用原 lineage 与原计数器，下个周期把用户加回来时累计值不归零；
(3) D6 定了纯内存 registry、不引入 WAL，进程重启即全部清零——判定若在进程内，重启就等于给所有
超额用户解封。因此进程侧只持有**相对量**，绝对账本在 collector（下发职责见 §5.4）。

**执行手段不用 SIGHUP。** 最省事的做法是“collector 发现超额 → 从配置删掉该 `users[]` 项 → SIGHUP”，
但 §2.3 与 §4.4 已写明重载会强杀全节点在途连接：为封一个用户打断所有人，且生效延迟等于一整个
采集周期。本节的执行面不碰配置、不重载、不动 inbound。

**下发剩余额度，而不是布尔标记。** collector 每次采集后按自己的持久账本算出
`remaining = 本周期配额 − 本周期起点基线以来的累计`，把**全量**剩余额度表推给进程；
进程从收到时刻起在计数回调里扣减，扣到 ≤ 0 即闸断。

与下发 blocked 布尔相比多出来的只是一个 int64 字段，换到的是把超额窗口从**峰值带宽 × 采集周期**
压到只剩下发延迟：布尔方案下 100 Mbps 的用户在 60 秒采集周期里能多跑约 750 MB。持久化责任仍
全部在 collector，进程侧不写任何文件，D6 不受影响。

额度口径固定为**四向之和**（`tcp_uplink + tcp_downlink + udp_uplink + udp_downlink`），不设选项：
计量口径已由 §5.2 声明为应用 payload，再给配额单开一套方向选择只会制造两套口径的对账问题。
需要按方向计费时，在 collector 侧算 `remaining` 时自行折算。

**控制端点契约。** 独立 UDS，**不复用只读快照 socket**——这是 §11 D5 已经写下的纪律：
需要单独授权、幂等与失败关闭。两个 socket 的权限档次不同：快照是只读旁观者，本端点能改变转发行为。

传输层同 §4.5：HTTP/1.1-over-Unix-stream、每连接单请求单响应、禁 keep-alive 与 query；区别是本端点
**允许且只允许**请求体（`Content-Type: application/json`，大小上限与快照响应同档）。
单一路由 `PUT /v2/quota`，全量覆盖语义。**不提供增量的 block / unblock**：增量事件丢一条即永久错位，
全量覆盖天然幂等，也天然处理进程重启后的状态清零。

```json
{
  "schema_version": 2,
  "node_id": "node-example-01",
  "runtime_id": "0123456789abcdef0123456789abcdef",
  "epoch": 137,
  "entries": [
    { "inbound_tag": "vless-entry-01", "name": "u_example_01", "remaining_bytes": 5368709120 }
  ]
}
```

- `node_id` 与 `runtime_id` 必须与当前进程逐字节相同，否则 **409** 并整份丢弃。这条同时是
  “重启即解封”的自动纠正机制：进程重启后 collector 拿旧 `runtime_id` 推送会立刻拿到 409，
  据此知道必须按新 runtime 重推，而不必等到下一次采集才发现。
- `epoch` 单调递增，`≤` 已接受值即整份丢弃并返回 **409**，用于抵抗乱序重投。
- `entries[]` 出现当前配置中不存在的 `(inbound_tag, name)` 时**整份拒绝**（400）并在错误信息里列出
  前若干条：全量表本应与进程读的是同一份配置真相，对不上说明控制面与数据面已经不一致，
  静默忽略会让运维误以为额度已生效。collector 应在下一轮快照看到新 lineage 后重推。
- 未出现在 `entries[]` 中的 lineage 视为**无限额度**——全量表是“限额清单”而非“用户清单”。
- 成功返回 200 与 `{"schema_version": 2, "epoch": …, "applied": …}`；错误码复用 §4.5 的取值表，
  另加 409。响应体与快照、`/healthz` 共用同一个 schema 版本常量。
- socket 默认 `0600`，绑定前的父目录、符号链接、旧 socket 与 inode 替换检查与 §4.5 同款；
  同样不得直接映射为公网监听。

**控制面失联与首次推送前的语义。** 两个显式配置项，都必须在 `docs/OPERATIONS.md` 里写清商业
含义，不设“聪明”的默认：

- `startup_action`：进程启动后、收到第一份全量表之前的处置，`allow`（默认）或 `deny`。
  选 `deny` 意味着 collector 没起来时全节点不可用；选 `allow` 意味着重启窗口内超额用户可继续跑。
- `stale_after` / `stale_action`：距上次成功接受全量表超过 `stale_after` 后的处置，默认
  `stale_action: allow`。设成 `deny` 可以防止“collector 挂掉后所有人无限用”，代价是把 collector
  变成转发链路上的单点。

两项都**不影响已经扣减到 0 的 lineage**——它们只决定“没有额度信息时怎么办”。

**五条实现纪律。** 前两条是“只封一个人、不拖累其他人”的充要条件，后三条各封住一类会静默失效
或静默外溢的实现方式：

1. **剩余额度是逐 lineage 的无锁原子量。** 计数回调是全进程最热的路径，每次 copy 迭代与每次 splice
   循环都要跑。闸断检查只允许对挂在 lineage record 上的 `atomic.Int64` 做一次 `Add(-n)` 并判断返回值，
   **禁止在回调里查 map、取全局锁或做任何分配**——否则功能上只封一个人，吞吐上拖累所有人。
   数据结构层级与既有四向饱和计数器相同。
2. **全量表原子换页。** 应用新表时用 `atomic.Pointer` 一次性换掉整张表，**不得“先清空再逐条写入”**：
   那个窗口里其他用户会被按 `startup_action` 整体误判。逐 lineage diff 亦可，但不得出现中间态。
3. **闸断状态不得写进快照的 `active`。** `active` 在 §4.3 第 1 条里的语义是“配置中是否还存在该身份”，
   由每次重载后的配置对账切换；把闸断复用到该字段，采集端会把“超额被封”读成“用户已删除”。
   首期闸断状态**完全不进快照**——名单是 collector 自己下发的，它本就知道。这样 v2 schema 一个字节
   不动，D3 不受影响。将来若要暴露，按 §4.5 的纪律新加字段并提升 `schema_version`，
   不得塞进 `health` 的三键闭集。
4. **闸断只能实施在 CountFunc 内，不能写在包装层的 `Read` / `Write` 里。** 三条上游事实决定了这一点
   （sing `v0.9.0-beta.4`，即 `sing-box v1.14.0` 的 `go.mod:48` 所钉版本）：
   - `N.CountFunc` 的签名是 `func(n int64)`，**没有 error 返回**（`common/network/counter.go:10`），
     无法靠回调返回错误来中止 copy；
   - `bufio.Copy` 会用 `UnwrapCountReader` / `UnwrapCountWriter` 一路解包到底层 conn、**只把 CountFunc
     摘走**（`common/bufio/copy.go:36-37`，UDP 侧对称地在 `:265-266`），而 §4.1 又硬性要求本项目的
     tracker 实现 `Upstream()` 与 `Reader/WriterReplaceable()`（否则计数口径静默退化）；两者叠加的
     结果是**包装层的 I/O 方法在快路径上根本不执行**；
   - splice 路径同样只调 CountFunc（`common/bufio/splice_linux.go:88-92`）。

   因此唯一同时覆盖普通 copy、Vision direct 与 splice 的钩子就是回调本身：在回调内判负后 `Close()`
   包装层持有的那条 conn 引用，让正在跑的 copy 在下一次 I/O 上自然出错退出。好处是**计数点即执行点**，
   registry 不需要额外维护活跃连接集合。§8 为此设了变异检验：把闸断挪进 `Read`/`Write` 后，
   splice 与 Vision direct 用例必须转红。
5. **连锁范围以 lineage 为界，已核实无跨用户外溢。** mux 侧，tracker 拿到的是 `mux.serverConn` 包住的
   **单条 stream**（`sing-mux@v0.3.5 server.go:126`、`server_conn.go:16`），不是底层 session conn，
   关掉一条不影响同 session 的兄弟子流；退一步说，即便实现退化成关掉整个 session，一条 session 上的
   全部子流也都来自同一份凭据、即同一个计费身份。首期白名单内不存在跨用户共享传输层连接的形态：
   VLESS 与 SS-2022 都是每客户端独立 TCP；而“同一源端口的后续用户全部记到第一个用户名下”的 UDP
   NAT 表串号只存在于 legacy AEAD，已由 §2.4 在配置校验期拒绝。

**新连接阻断与唯一的外溢路径。** 新连接的阻断**复用 §4.6 第 9 条已有的失败关闭点**，判据从
“`metadata.User` 为空”扩为“为空**或**该 lineage 剩余额度 ≤ 0”，位置、实现与两点已知差异全部
不变：(a) 客户端看到的是连接建立后被重置而非协议层认证失败；(b) TCP 仍会向目的地拨号。

由此产生本能力唯一真正会外溢到其他用户的路径：**被闸断用户的重连风暴**。客户端通常立即重连，
每次重连都要走完握手与认证才在 tracker 处被拒，消耗的是公共的 accept 队列与握手 CPU
（REALITY/TLS 握手不便宜），并且每次都会向目的地产生一次完成握手、0 字节的真实拨号。
缓解与根治两条路：

- **首期必做**：逐 lineage 的重连节流——冷却窗口内该 lineage 的新连接在 tracker 处立即拒绝，
  省掉计数器包装、审计投递与日志。节流状态与额度同表，同样是无锁原子量。
  **它省不掉拨号**：`ConnectionManager.NewConnection` 先拨号后 copy（`route/conn.go:95-105`），
  而 tracker 只能决定交给它的是哪个 conn，无法阻止路由调用它。因此节流压低的是本机侧的
  握手后成本，不是目的地侧的连接数。这条已由 §8 的固定断言钉住（拒绝一次 = 目的地 accept 一次），
  改判必须同步改本节与 §12。
- **根治需要摘凭据**：把超额用户从 inbound 的认证表里摘掉，客户端拿到的是干净的协议层认证失败，
  既不握手成功也不拨号。VLESS 侧能力现成（`sing-vmess vless/service.go:40 UpdateUsers`），
  但 §11.2 D5 已经算过账：Shadowsocks 侧 `MultiInbound.UpdateUsers` 与数据面读取之间没有锁
  （写 `inbound_multi.go:132` / 读 `:170`），且索引即计费身份，走这条路必须自行加锁并保证索引稳定。
  留作 D5 改判后的第二步，不在本节范围内。

**配置**（承载在 §4.6 第 1 条的自有 service 类型内，同样对未知字段失败关闭）：

```json
{
  "services": [
    {
      "type": "user_stats",
      "tag": "stats",
      "node_id": "node-example-01",
      "listen_path": "/run/sing-box-plus/stats.sock",
      "quota_control": {
        "listen_path": "/run/sing-box-plus/quota.sock",
        "startup_action": "allow",
        "stale_after": "10m",
        "stale_action": "allow",
        "reconnect_throttle": "5s"
      }
    }
  ]
}
```

`quota_control` 缺省即整个能力关闭。两个 `listen_path` 必须非空且互不相同，相同即启动失败；
`quota_control.listen_path` 与 `node_id`、`listen_path` 一样进入 §4.6 第 11 条的**重载不变量**——
重载期变更即拒绝本次重载、保留旧 Box 继续服务并告警。**不新增 build tag**：闸断依赖四向计数，
与统计共用 `with_user_stats`，D4 的生产 tag 集不变。

**验收先行**，与 §4.8 同一纪律：先写会失败的门禁，再写功能。最小门禁——两个用户同时跑流量，
只给其中一个下发小于其在途增量的 `remaining_bytes`，断言 (a) 该用户的在途连接在下一次 I/O 上断开、
新连接被拒；(b) 另一用户的连接不中断、四向计数继续增长；(c) 快照中两者的 `active` 均不变；
(d) 同一用例在 Linux 真实 splice 路径下再跑一遍。全量门禁见 §8。

## 5. 采集与结算契约

exporter 只输出当前进程生命周期内的累计值，不持久化账单，也不决定新运行周期的首快照是否入账。
下游集成方按本节实现差分与幂等落库；本项目提供参考 collector 与结算模型（§6 交付物）。

### 5.1 基线键与幂等

保存 baseline 的完整键：

```text
node_id + inbounds[].tag + inbounds[].generation + users[].name + users[].generation + runtime_id
```

`generation` 恒为 1，但不得从采集键中省略，以保持 schema 与未来兼容性。采集端的内部标签与下游列名
不必与 wire 字段同名——键的语义由这个六元组确定，而非由字段拼写确定。幂等批次 ID 必须包含快照
`sequence` 与本次增量。差分规则：

- 同一 `(node_id, runtime_id)` 内 `started_at_unix_ms` 必须恒定，变化即视为未知 runtime；
- `sequence` ≤ 已处理值 → 丢弃整份快照且不推进基线（重复或乱序响应）；
- `sequence` 前进但**四向累计值**倒退 → 失败关闭并告警，不得猜测并继续收费。单调性约束**只**适用于
  四个 `*_bytes` 与 `sequence`；`tcp_sessions` / `udp_sessions` 是瞬时 gauge（正常排空时会降到 0），
  不参与差分、不进基线、不触发回退告警；
- `health` 任一项为真、`schema_version` 不为 `2`、`Content-Length` 不符或 JSON 截断 → 拒绝入账；
- HTTP 429 与连接被直接关闭视为**可重试且不得入账**；其余非 200 一律拒绝入账；
- 首次看到新 `runtime_id` 时必须显式选择 `baseline`（只建基线，降低重复风险）或 `include`
  （首次累计全部计入，降低漏记风险），不得留作隐式行为。

`active=false` 的已观察 lineage 仍会出现在后续快照中，采集端必须继续保留其基线，不得因
`active=false` 而删除或归零。

### 5.2 计量口径的对外声明

账单口径是认证并解码后、成功进入转发边界的应用负载，不包括协议 header、隧道封装与重传。
该口径不能替代云厂商或 VPS 的网卡计费；两者之间的固有差额见 §12。

**快照不含目标地址、客户端地址或连接明细**，采集与结算链路上不出现任何访问目标。§4.8 的基础访问
审计是一条完全独立、默认关闭的旁路：它写本机文件、不进快照、不走 UDS，也不参与差分与结算。
两条链路的保留期、访问控制与对外声明必须分别制定——启用 §4.8 即意味着节点上存在“用户→域名”
明细，这是产品与合规决策，不因计量已上线而自动成立。

### 5.3 计划重启与最终结算

因为进程重启会清零内存计数，任何计划内的重启或配置变更必须按下列顺序执行，否则会留下未闭合窗口：

1. 停止接入新连接（下线、防火墙或上游负载均衡）；
2. 轮询快照的 `tcp_sessions` / `udp_sessions` 直至为 0 或达到超时；
3. 采集最终快照并**确认已持久化入账**；
4. 停止进程、应用变更、启动新进程（新 `runtime_id`）；
5. 采集端按 §5.1 的首快照策略处理新周期。

仅调整用户集合时可用 SIGHUP 重载代替重启：registry 跨 Box 存活、`runtime_id` 与累计值都不变
（§4.4），代价是在途连接仍会被强制关闭。超时强切是允许的——已计字节不会丢失——但必须在采集端
标记该窗口为未排空以便审计。异常退出（崩溃、OOM、断电）留下的未闭合窗口必须单独审计，
不得当作正常周期切换。`node_id` 与 `listen_path` 不得随重载改变，重载期校验见 §4.6 第 11 条。

### 5.4 配额闸断的下发职责（启用 §4.9 时）

启用 §4.9 后，collector 在差分入账之外另担三项义务，缺一即出现“封不住”或“误封”：

1. **周期基线自持。** “本周期已用”只能由 collector 按 §5.1 的六元组基线自行累计，**不得**拿快照里的
   四向绝对值直接与配额相比——同名重建复用原计数器（§4.3 第 2 条），绝对值跨计费周期不归零。
2. **每次采集后重推，识别到新 `runtime_id` 时立即重推。** 进程侧的额度是纯内存的，重启即消失；
   `PUT /v2/quota` 返回的 409 是这条义务的兜底信号，不是可忽略的噪声。
3. **顺序是先入账、再算额度、再推送。** 反过来会在 collector 自身崩溃时下发一个基于未落库增量的
   额度，恢复后重复扣减。

配额闸断**不改变 §5.2 的计量口径**，也不改变快照内容：被闸断身份的四向累计值停止增长，但 lineage、
`active` 与基线全部照旧，§5.1 的差分与幂等规则一个字节都不用改。

## 6. 工作分解、交付物与工期

按一名熟悉 Go、sing-box 和异步代理数据面的工程师估算，不含灰度等待与法务日历时间。
人周为单人工作量，多人并行只压缩日历时间、不减少人周。

| 工作项 | 内容 | 必需 | 粗估 |
| --- | --- | --- | --- |
| 骨架与基线 | §1 目录树、`upstream.lock`、prepare/verify 脚本 | 是 | 1–2 人周 |
| wrapper main 与 CLI 复制 | 复制上游 `cmd/sing-box/` 9 文件 608 行并改造、`-X` 版本注入、最小 registry、复制文件漂移门禁（§4.7） | 是 | 1–2 人周 |
| 观测 PoC | 在钉定构建上验证 VLESS 与 SS-2022 归属，并复现 §2.2 的各项边界 | 是 | 3–5 人日 |
| 四向 tracker / registry | 通用 tracker、饱和计数、稳定 lineage | 是 | 2–4 人周 |
| 配置与失败关闭 | §4.6 全部 11 条校验路径与错误信息（含 SS 特化校验、SSM 两层互斥、上游账本互斥、重载不变量） | 是 | 1–2 人周 |
| UDS exporter | schema、安全加固、资源上限、监督与故障注入 | 是 | 2–3 人周 |
| 协议与性能验证 | Vision direct 切换、mux、XUDP/UoT、SS-2022 EIH 四向、UDP batch、bench/pprof | 是 | 3–5 人周 |
| 参考 collector 与契约测试 | 复用 `http_unix.py` 的 HTTP/UDS 传输层与 `settlement_model.py` 的周期/幂等算法骨架（两者的核心逻辑与 schema 正交），按 v2 重写字段校验与 fixture（合计约 300 行）；**参考 collector 从零实现**——`shadowsocks-rust-plus` 的 `mock_collector.py` 是审计导出协议的采集器，与快照接口无关 | 是 | 1.5–2.5 人周 |
| 可复现发布与签名 | 两次独立构建、manifest、detached 签名与验签 | 是 | 1–2 人周 |
| 文档与运维手册 | `docs/` 六件套 | 是 | 1–2 人周 |
| 重载对账与排空 | 每次重载按新配置对账 `active`/tombstone、drain 阶段、看门狗接管、排空超时策略（§4.4） | 否 | 1–2 人周 |
| 基础访问审计 | §4.8 的 13 条实现纪律、JSONL writer 与自轮转、验收门禁与 7 项变异检验（约 320 行 Go + 60 行测试脚本） | 否 | 0.5–1 人周 |
| 配额闸断 | §4.9 的控制端点与全量表原子换页、CountFunc 内闸断、重连节流，以及 §5.4 在参考 collector 侧的下发逻辑（约 350 行 Go + 参考 collector 增量） | 否 | 1–1.5 人周 |
| 上游 rebase 储备 | 每次 minor 升级 | 周期性 | 1–2 人周/次 |

必需项合计约 **14.5–25.5 人周**，另加 15–25% 的复核返工缓冲；三项可选全做为 17–30 人周。
“能看每用户上下行”的 PoC 不等于完整功能，不得据此宣告阶段完成。

**registry 裁剪比裁 tag 更能瘦身。** `include.InboundRegistry()` / `OutboundRegistry()` 无条件注册
全部协议（`include/registry.go:52-108`），`include/` 下的 build tag 只门禁
acme/ccm/clash_api/cloudflared/dhcp/naive_outbound/ocm/openconnect/openvpn/quic/tailscale/usbip/
v2ray_api/wireguard——裁 tag 不会把 VMess、Trojan、TUN、naive inbound、ssh、tor、anytls、snell
从二进制里去掉。自有 main 可以完全绕开 `include.*Registry()`，直接用
`inbound.NewRegistry()` + `vless.RegisterInbound` + `shadowsocks.RegisterInbound`、
`outbound.NewRegistry()` + direct/block、`dns.NewTransportRegistry()` + udp/tcp/local，
空 endpoint/certificate registry，再交给已导出的 `box.Context(...)`。实测最小 registry 下
`box.New` + `AppendTracker` 正常，vless + `xtls-rprx-vision` + ws transport + multiplex +
shadowsocks-2022 + dns + `hijack-dns` 路由规则的配置全部解析通过，非白名单 inbound 在配置解码期
即被拒。**两点边界不得据此放松校验**：(1) 该门禁是**类型级**的——`shadowsocks.RegisterInbound`
只注册一个 `shadowsocks` 类型，单用户 / multi / relay 三种形态在 `NewInbound` 内部按字段分派
（`protocol/shadowsocks/inbound.go:33-44`），§2.4 的 relay / 单用户拒绝**不会**被 registry 拦下
（已实测 relay 配置通过），§4.6 第 2 条仍须自行实现；(2) 代价是丢掉“已移除类型”的三个友好报错桩，
需在 `docs/OPERATIONS.md` 声明为已知行为差异。另需评估最小 DNS registry 丢掉的
`tls`/`https`/`hosts`/`mdns`/`fakeip`/`resolved` 六个 transport：若生产配置需要 DoT/DoH 或 `hosts`，
按需补注册，清单随 §9.1 一并定稿。

交付物清单：

| 交付物 | 说明 |
| --- | --- |
| `upstream.lock` | repository / tag / commit / `prepared_tree_sha256` / commit_date / fetched_at / license / go 最低版本 |
| `cmd/sing-box-plus/` | 自有 main：9 个复制文件 + 最小 registry + `user_stats` service 类型 + tracker 注入 |
| `scripts/prepare-source.sh` | 按精确 commit 取源码并校验；引入补丁后零 fuzz 重放 `patches/series` |
| `scripts/verify.sh` | `go vet`、`go test -race ./...`、lint、敏感信息扫描，以及复制文件与钉定版本的 diff 漂移门禁（§4.7） |
| `scripts/build-linux-release.sh` | 两次独立路径构建逐字节一致才产出 manifest + SHA-256 |
| `scripts/sign-release.sh` / `verify-release.sh` | detached 签名与验签，私钥离线保管 |
| `scripts/user-stats-client.py` | 带 v2 schema 与健康校验的快照读取客户端；自 `shadowsocks-rust-plus` 同名脚本适配，HTTP 解析与响应头断言原样保留，替换字段校验、默认 socket 路径与 request-line。注意该 HTTP 解析器与 `tests/http_unix.py` 是同一份代码的两处副本，v2 下两处必须同步改，`scripts/verify.sh` 应加一条一致性门禁 |
| `tests/reference_collector.py` | 参考 collector：取快照 → v2 校验 → 差分 → 幂等落地本地账本；范围**不含** outbox、mTLS 与重试（属下游控制面）。`shadowsocks-rust-plus` 无对应物可搬，须从零实现 |
| `scripts/quota-client.py` | 配额下发客户端：读取一份 `remaining_bytes` 清单后 `PUT /v2/quota`，含 `epoch` 维护与 409 重推。与 `scripts/user-stats-client.py` 共用同一份 HTTP/UDS 解析代码，受同一条一致性门禁约束 |
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
| 1 | 冻结基线与骨架 | `upstream.lock` 已记录 tag/commit/`prepared_tree_sha256`；两次独立构建 SHA-256 相同；`version` 显示钉定版本（该版本由构建脚本从 `upstream.lock` 读取后以 `-X …constant.Version` 注入，未注入时会打印 `unknown`）；tag 清单按 §9.1 定值并写入 `.env.example`；`docs/UPSTREAM_BASELINE.md` 落库 |
| 2 | 观测 PoC | 回环 VLESS Reality/Vision 与 Shadowsocks-2022 EIH 多用户 TCP+UDP 字节 oracle 差 = 0 的报告；复现并记录 ServiceName 覆写、静态白名单与重载丢数三项边界 |
| 3 | 四向 tracker / registry | `go test -race` 全绿，含 `New → AppendTracker → Start` 全路径与“`Start` 后追加必被 `-race` 报出竞争”的负向用例；四向 oracle 误差 = 0；Linux amd64 真实 splice 用例覆盖；多 tracker 叠加 unwrap 断言通过；§4.6 十一条失败关闭路径全部有用例 |
| 4 | UDS exporter | 权限 / 符号链接 / inode 替换 / 超限 / 慢客户端故障用例通过；exporter 异常退出导致进程失败退出；v2 契约测试全绿（对 §4.5 的快照与映射表逐字段断言），参考 collector 在 §8 故障矩阵下无漏计、无重复入账 |
| 5 | 长跑与结算验证 | staging 连续运行 ≥ 7 天：负增量 = 0、未知 runtime = 0、`sequence` 重复 = 0、unhealthy 快照全部被拒；§5.3 计划重启流程演练无缺口、无重复 |
| 6 | 可发布版本 | §8 故障矩阵与性能三组对照报告归档；可复现发布包与签名验签通过；`docs/OPERATIONS.md` 含部署、采集、重启屏障与回滚步骤 |

## 8. 测试与性能门槛

通用自动化测试（括号为归属里程碑）：

- 每个协议的合法/非法认证、空 user、重复 user、跨 inbound 同名（M3）；
- TCP partial write、half-close、RST、取消、buffered copy、vectorized copy 和 Linux splice（M3）；
- UDP packet/batch、XUDP/UoT、mux、逻辑 UDP 归类，不计 framing（M3）；
- Vision early data、padding/unpadding、buffered → direct 切换（**不含 splice**，见 §4.2）（M3）；
- 多 tracker 叠加时 counter unwrap 仍生效（M3）；
- 配置校验：未知字段、非白名单 inbound、空用户集、重名、缺 build tag、非 Unix 平台（M3）；
- 热删、同名重加、凭据轮换、旧连接继续计数、tombstone 保留（M3）；
- 快照响应中断、collector 重试、重复/乱序/倒退 `sequence`、累计溢出（M4）；
- UDS 权限、symlink/inode 替换、慢连接、超大请求和并发上限（M4）；
- exporter 异常退出、连续 accept 失败、systemd 重启后的新 runtime 处理（M4/M5）；
- 计划重启流程、超时强切、崩溃后的未闭合窗口标记（M5）；
- `-race`、fuzz、端到端字节 oracle、Linux 真实 splice 和发布目标集成测试（M3/M6）。

Shadowsocks 与 wrapper 形态的特化补充：

- SS-2022 EIH 多用户 TCP/UDP 四向 oracle 误差 = 0，且与同拓扑 VLESS（`packet_encoding: xudp`）
  逐项相等（M3）；
- 客户端启用 `udp_over_tcp` 或 inbound/outbound `multiplex` 时，被计数条目的 `network` 必须是
  `udp`、`metadata.User` 必须保留（M3）；
- 配置校验拒绝：单用户 SS、`method: none`、任一 `users[].name` 为空、`users[].name` 重名、
  uPSK 归一化后重复、legacy AEAD method、`destinations` 非空、`managed: true`（M3）；
- 配置校验拒绝任何 SSM service，且自有 registry 不注册 ssmapi 时解析期即失败（M3）；
- 配置校验拒绝 `services[] type:"api"`、`experimental.clash_api`、PoC 集下的
  `experimental.v2ray_api.listen`（M3）；
- 回归断言（活文档，说明“为何只支持 `2022-blake3-*`”）：legacy AEAD 多用户在 UDP 路径上，
  同一源端口的第二个用户字节会记到第一个用户名下（实测 u1 独得 up=8/down=8）；同拓扑的 SS-2022
  因按 session id 建键而正确分离（M3）；
- 回归断言：两个 `users[]` 共用同一 uPSK（含 Base64 不同但解码相同的两个串）时上游静默覆盖，
  两个身份合并到**配置中靠后**的那个（M3）；
- 回归断言（拒绝 relay 的活文档）：relay inbound 上计到的是密文流而非 payload——4 字节往返实测
  下行恒为 79 字节，上行因随机 padding 在 127–900 之间波动，故只能断言“上行远大于 payload
  且不可复现”，**不得写成固定值**（M3）；
- SS 认证失败 / 盐重放被拒 / EIH 不匹配的连接与数据报不产生任何计数、也不产生幽灵 session（M3）；
- 带 `badlinkname` 与不带两种构建各跑一遍字节对账矩阵（§9.1）（M3）；
- 匿名连接失败关闭：0 字节应用负载外发、快照无记录；同时断言 TCP 仍会向目的地拨号
  （已知差异，§4.6 第 9 条）（M3）；
- SIGHUP 信封不变性：重载前后各取一次快照，断言 `runtime_id` 与 `started_at_unix_ms` 逐字节相等、
  `sequence` 严格递增且不回退、四向累计值不清零；随后重启进程，断言 `runtime_id` 改变、
  `started_at_unix_ms` 前移、`sequence` 从头开始（M3）；
- 负向：把 `sequence` 放进 `services[]` 实例时，参考 collector 从重载到追平旧最高序号之间静默丢弃
  全部快照且不抛任何错；把 `runtime_id` 一并放进实例时，重载窗口的增量在 `baseline` 策略下被静默
  丢失（M3）；
- SIGHUP 重载时改 `node_id` 或 `listen_path`：重载被拒、旧实例继续转发、`runtime_id` 与四向累计值
  均不变、UDS 路径不变（M3）；
- 配置校验拒绝 `listen_port` 缺省或为 0（M3）；
- v2 快照与 §4.5 逐字段一致：必填键齐全、类型与 u64 边界、`inbounds` 按 `tag` 再 `generation` /
  `users` 按 `name` 再 `generation` 的稳定字节序排序、`health` 为三键闭集（M4）；
- 快照的 `listen` 必须能被 `netip.ParseAddr` 解析（即不含 `:port`），`listen_port` 在 `1..=65535`；
  配置省略 `listen` 时快照输出 `127.0.0.1`（M4）；
- `/healthz` 与全部错误响应体的 `schema_version` 与快照一致为 `2`，全套响应不出现 1/2 混用（M4）；
- `GET /v1/snapshot` 返回 404，且 body 的 `schema_version` 为 `2`（M4）；
- 共存回归：`shadowsocks-rust-plus` 的校验器对本项目快照整份拒绝且不入账，钉死并存窗口内旧
  collector 误指到本项目节点时必然失败（M4）。

启用 §4.9 配额闸断时的补充（同样必须是会失败的门禁，不得静默放行）：

- **隔离性主用例**：两个用户同时跑流量，只给其一下发小于其在途增量的 `remaining_bytes`，断言该用户
  在途连接于下一次 I/O 上断开、新连接被拒，另一用户连接不中断且四向计数继续增长（M3）；
- 同一用例在 Linux 真实 splice 路径与 Vision direct 路径下各再跑一遍（M3）；
- 变异检验：把闸断逻辑从 CountFunc 挪进包装层的 `Read` / `Write`，splice 与 Vision direct 用例必须
  转红（纪律 4）（M3）；
- 全量表换页无中间态：推送与转发并发进行时，未出现在 `entries[]` 中的 lineage 全程不被闸断
  （纪律 2）（M3）；
- 热路径无回归：闸断开启与关闭两组对照，回调内无额外分配、无锁竞争（纪律 1）（M6）；
- 快照不受污染：闸断前后两个用户的 `active` 均不变、`health` 三位不变、`schema_version` 仍为 `2`
  （纪律 3）（M4）；
- 控制端点失败关闭：`node_id` / `runtime_id` 不符、`epoch` 回退或持平、`entries[]` 含未知 lineage、
  超大请求体、非 `PUT` 方法、带 query，逐项返回对应错误码，且**不改变**任何已生效额度（M4）；
- 进程重启后旧 `runtime_id` 的推送返回 409；重启窗口内该用户按 `startup_action` 处置（M4）；
- `stale_after` 超时后按 `stale_action` 切换，且已扣减到 0 的 lineage 不因超时而解封（M4）；
- 两个 `listen_path` 相同时启动失败；SIGHUP 中变更 `quota_control.listen_path` 时拒绝本次重载、
  旧实例继续服务、`runtime_id` 与四向累计值均不变（M3）；
- 重连节流：被闸断用户在冷却窗口内的新连接不产生目的地拨号（M3）。

启用 §4.8 基础访问审计时的补充（全部归 M4，且必须是会失败的门禁，不得用 `|| true` / `-f` 静默放行）：

- 正例：一次 `curl` 与一次 `dig` 之后**停止进程**，断言 JSONL 恰好各一行，`user`/`host`/`host_src`/
  `port`/`up`/`down` 均正确；
- 反例零记录：只完成 TCP 握手、只上行无下行、只收到 banner 无上行，三种情形均不产生记录；
- UDP association 关闭后**恰好一行**（覆盖 `route/conn.go:376-380` 的双 `Close`，纪律 1）；
- 哨兵地址 `sp.mux.sing-box.arpa` / `sp.v2.udp-over-tcp.arpa` / `sp.udp-over-tcp.arpa` /
  `sp.packet-addr.v2fly.arpa` 不产生记录，其子流各自产生记录（纪律 8）；
- 16KB SNI 的连接：落盘行 `host` 截断至 255 字节且为合法 UTF-8，整行长度有界（纪律 6）；
- 注入 `ENOSPC` 后腾空磁盘：writer 自行恢复写入，期间的丢弃计入 `dropped_write` 且非 0（纪律 3）；
- 断电式 kill 后重启：不出现半行与首条粘连，末行可被 `json.Unmarshal` 解析（纪律 4、5）；
- 外部 `mv` + `rm` 日志文件后：writer 在下一个 flush tick 重开新文件，不再写入已删除 inode（纪律 9）；
- `max_total_bytes` 触顶且无可删对象时：只累加计数、**不写 gap 行**，不出现自指 gap 放大（纪律 10）；
- 审计写入失败、队列满与注入 panic 三种情形下，转发不中断、进程不退出、快照接口不受影响
  （纪律 12，与 §4.6 第 5 条的 fail-hard 形成对照用例）；
- 关闭序：`instance.Close()` 后在途记录全部落盘、stop 行存在、进程不因向已关闭 channel 投递而
  panic（纪律 13）；
- 变异检验：对纪律 1、2、4、5、9、10、12 各取反或注释一次，确认对应用例转红。

性能验收比较三组：未启用、编译但未配置、启用四向统计。记录吞吐、p50/p99 延迟、CPU、分配、
goroutine、内存随用户数/并发数增长，以及 exporter 被慢客户端占满时代理数据面的隔离。
目标不是预设“零开销”，而是给出可复现基线并设置回归阈值。

## 9. 构建、发布与上游跟进

### 9.1 构建矩阵

只保留支撑 §2.4 两类计费 inbound 所必需的 tag。实测结论是——**Shadowsocks-2022 EIH 多用户是
零 tag 的；VLESS + XTLS Vision 也是零 tag 的（`protocol/vless`、`protocol/shadowsocks` 均无 build
约束，`include/registry.go:64`、`:70` 无条件注册）；唯一门禁点是 REALITY 服务端**
（`common/tls/reality_server.go:1`）。因此生产集只需一个上游功能 tag。

| 项 | 值 |
| --- | --- |
| Go 工具链 | 钉具体版本并设 `GOTOOLCHAIN=local`；`v1.14.0` 的 `go.mod` 为 `go 1.25.5`，已验证 go1.26.5 可用 |
| CGO | `CGO_ENABLED=0` |
| 可复现参数 | `-trimpath -buildvcs=false -ldflags "-s -w -buildid= $(cat release/LDFLAGS)"`，外加从 `upstream.lock` 读取并注入的 `-X …/constant.Version=<tag>`（§4.7）；`release/LDFLAGS` 内容在 1.14 已改写，必须读取文件而非硬编码 |
| **生产 tag 集** | **`with_utls` + `badlinkname` + 自有 `with_user_stats`** |
| PoC tag 集 | 生产集再加 `with_v2ray_api`（仅里程碑 2；实测仅 +60 KiB）。PoC 集下 §4.6 第 7 条必须同时拒绝 `experimental.v2ray_api.listen` 非空 |
| 裁剪收益 | linux/amd64、`CGO_ENABLED=0`、上述可复现参数，2026-09-05 实测 `./cmd/sing-box`：上游默认集 80,728,190 B（77.0 MiB）→ 生产集 36,589,694 B（34.9 MiB），**−54.7%**；零 tag 36,225,150 B |
| registry 裁剪收益 | 同参数、tags `with_utls,with_quic,badlinkname,tfogo_checklinkname0`、linux/amd64：上游 `cmd/sing-box` 40.3 MB → 同一 wrapper main + `include.*Registry()` 38.6 MB → wrapper main + 最小 registry **20.8 MB**（§6）。**该组与上一行 tag 集不同，两组数字不可相减混算** |

`badlinkname` 保留，不进裁剪清单——**它不是纯可选项，会改数据面**。带该 tag 时 `common/badtls`
编译完整实现、Linux 上 `common/ktls` 编译 12 个文件；不带时 badtls 只剩 stub，
`common/tls/server.go` 把它返回的错误当“不支持”直接回退为普通 `tls.Conn`，**静默降级、日志零提示**
（实测以 `log.level=debug` 启动双 inbound 输出仅 6 行，无降级痕迹）。因其改变 TLS 读路径，
§4.2 与 §8 的字节对账矩阵必须覆盖两种构建。注意同一二进制里也存在两条读路径：即使带 tag，
TLS 连接类型未落入注册表时同样回退；两条路径计数都不丢，但缓冲与切分粒度不同。

被裁剪的默认 tag 与其放弃的能力（上游 `release/DEFAULT_BUILD_TAGS` 共 17 项，保留 2 项，其余全砍）：

| 被砍 tag | 放弃的能力 | 缺失时的表现 |
| --- | --- | --- |
| `with_quic` | Hysteria / Hysteria2 / TUIC 的 inbound 与 outbound、DoQ 与 DoH3 DNS transport、V2Ray QUIC transport、naive 的 HTTP/3 监听 | `box.New()` FATAL。VLESS/REALITY/Vision 与 SS-2022 的 TCP+UDP 均不依赖它。**§2.4 日后追加 Hysteria2/TUIC 须加回** |
| `with_gvisor` | TUN / WireGuard 的 gVisor 用户态网络栈 | 本项目不用 TUN，未展开验证 |
| `with_dhcp` | DHCP DNS transport | FATAL `… rebuild with -tags with_dhcp` |
| `with_wireguard` | WireGuard endpoint | FATAL |
| `with_acme` | ACME 自动证书签发 | FATAL |
| `with_clash_api` | clash 服务端与 UI | FATAL。**不移除上游连接账本**，见下注 |
| `with_tailscale` | Tailscale endpoint 与 DERP 服务 | FATAL |
| `with_ccm` / `with_ocm` | CCM / OCM 服务 | FATAL（上游 `ccm_stub.go` 的提示文案笔误为 `with_CCM`，实际 tag 是 `with_ccm`） |
| `with_cloudflared` | cloudflared outbound | FATAL |
| `with_naive_outbound` | naive outbound（cronet） | FATAL。该 tag 与 `CGO_ENABLED=0` 并非互斥，加 `with_purego` 即可，但另需运行时 `libcronet.so` |
| `with_usbip` / `with_openvpn` / `with_openconnect` | usbip 服务、openvpn / openconnect outbound | FATAL |
| `tfogo_checklinkname0` | **无。** 该 tag 只在 Windows 目标上生效 | 无任何提示 |

**`tfogo_checklinkname0` 为何删。** 它在 tfo-go 中只作为 `windows &&` 的合取项出现，
非 windows 目标上表达式取值与该 tag 无关。实测 `go list -deps` 展开的 **674 个包，在加与不加该
tag 时逐行完全相同**（linux/amd64、darwin/arm64、darwin/amd64 三个目标 diff 均为空）；
windows/amd64 则会多编入 5 个 `*_checklinkname0.go`。本项目只发布 Linux 目标，故删除无功能影响。
（不要写“两个产物字节相同”：大小相同，但 buildinfo 里 `-tags` 字串长度不同会导致其后内容位移。）

**`release/LDFLAGS` 里哪些不可省。** 实测 go1.26.5 下 `badlinkname` 构建不带 `-checklinkname=0`
也能链接成功；真正有行为意义、不可省的是 `-X runtime.godebugDefault=multipathtcp=0,tlssha1=1`。
按“读文件不硬编码”原则整段原样保留。

**`with_utls` 一次门禁三样东西。** REALITY 服务端、REALITY 客户端与 uTLS 客户端指纹三者不可分
（实测零 tag 二进制对带 `tls.utls.enabled=true` 的 outbound 报 `uTLS is not included in this build`）。
构建脚本中**禁止出现 `with_reality_server` 与 `with_ech`**：1.14 把它们改成了报错型废弃占位，
传入即编译失败。

**裁掉 `with_clash_api` 不等于隔离上游账本。** 它只移除 clash 服务端。`common/trafficcontrol`
被 `box.go:24` 无条件 import，`service/api` 在 `include/registry.go:146` 无 build 约束地注册；
`service/ssmapi` 的源文件同样无 `//go:build`。因此“不与上游账本叠加”“排除 ssm-api”都不能靠裁 tag
保证，必须由 §4.6 第 7、8 条兜底。

**裁剪的可恢复性。** 全部 `with_*` 均为编译期开关，砍掉后不能靠改配置恢复，必须重新出包。
上表中 13 个 `with_*` 缺失时都会在 `box.New()` 阶段给出
`… is not included in this build, rebuild with -tags …` 的 FATAL（运维可直接发现）；
`badlinkname` 与 `tfogo_checklinkname0` 无任何提示，需靠构建清单而非日志核对。

自有 `with_user_stats` 不可省：§8 的性能验收要比较“未启用 / 编译但未配置 / 启用”三组，
且 §4.6 第 3 条的报错路径依赖它。

编译验证记录：2026-09-05，darwin/arm64、go1.26.5，在 `v1.14.0`（`0b899587`）上
`go build -tags with_utls,badlinkname ./cmd/sing-box` 与
`go build -tags with_v2ray_api ./experimental/v2rayapi ./service/ssmapi ./protocol/vless` 均 exit=0；
最小 tag 集二进制对 VLESS+REALITY+Vision 与 SS-2022 EIH 双 inbound 配置 `check` exit=0，
零 tag 构建同配置 exit=1 并报 `uTLS, which is required by reality is not included in this build`。
这只证明所选构建标签和包可编译、配置可通过校验，不替代协议、流量对账或生产测试。

### 9.2 上游锁定与升级规则

1. `upstream.lock` 记录 repository / tag / commit / tree sha256 / commit_date / fetched_at /
   license / go 最低版本，格式参照 `shadowsocks-rust-plus/upstream.lock`；
2. 跟随 **stable 轨道**（当前 1.14.x），只在 patch 内自动跟进；minor 升级视为新基线，
   需重跑 §8 的全部对账矩阵；
3. 每月核对上游 tag；安全修复 T+2 工作日完成评估、T+7 产出可复现构建；
4. 补丁（若已引入）不能零 fuzz 应用即失败，不得静默跟随其他版本；
5. 每次升级重跑 §8 全部门禁并把结果写入 `docs/UPSTREAM_BASELINE.md`；
6. 所跟随轨道转为 oldstable 或 EOL 后 30 天内必须迁移；
7. 1.14.x 依赖 `sing`、`sing-tun`、`sing-quic` 的 beta 模块，须用 `go.sum` 或 `go mod vendor` 固定；
8. 每次升基线核对 `grep -rn "AppendTracker" --include=*.go` 的上游调用点仍全部位于 `box.New` 内且
   数量未增，否则 §4.1 的“本 tracker 恒在包装链最外层”前提失效，需重跑顺序断言；同时核对复制自
   `cmd/sing-box/` 的 9 个文件与新版本的 diff，只允许已登记改动；
9. 升级门禁必须包含**一次真实 TLS 握手冒烟**：带 `badlinkname` 时 badtls 的 reflect 字段校验失败会
   返回非 `os.ErrInvalid` 的错误并被直接抛出，Go 工具链升级若改动 `crypto/tls` 内部字段名将表现为
   TLS 连接硬失败（而非静默降级）。

轨道状态（2026-09-05 核对）：`origin/oldstable` = `v1.13.21-2`，`origin/stable` = `v1.14.0-16`，
`origin/testing` 已进入 `v1.15.0-alpha.1`。1.13 已是 oldstable，按上游历史节奏很快停更，
不在其上开发 overlay。

## 10. 许可与命名

sing-box 的 LICENSE 是 GPL v3-or-later 的授权声明段，并附带“衍生作品未经同意不得使用该应用名称
或暗示关联”的额外文字（该文件只有声明段，不含 GPL 正文），见
[LICENSE](https://github.com/SagerNet/sing-box/blob/0b899587/LICENSE)。

义务按 GPLv3 的 convey 分档触发：

1. **向本公开仓库提交 overlay 源码即构成 convey**：本子目录须包含 GPL-3.0-or-later 全文与
   上游附加条款原文、`THIRD_PARTY_NOTICES.md`，修改须注明内容与日期（GPLv3 §5(a)）。
   这一触发点早于二进制发布。`cmd/sing-box-plus/` 内复制自上游 `cmd/sing-box/` 的 9 个文件
   （608 行）是 GPLv3 衍生物；上游 `cmd/*.go` 本身不带 per-file 版权头，因此义务落在：
   每个复制文件顶部注明来源 commit、修改内容与日期，并在 `THIRD_PARTY_NOTICES.md` 中单列该批文件。
2. **仅在自有主机部署、不向第三方交付二进制**不产生额外源码义务；GPLv3 没有 AGPL 式的网络使用条款。
3. **向第三方交付二进制**时须随附对应完整源码或书面要约。
4. 通过快照接口消费本项目的下游系统是独立进程，不因该接口而受 GPL 传染；但不得链接或内嵌本项目的
   Go 代码。

命名：`sing-box-plus` 是内部代号，不作为可发布产品名；对外发布使用中性名称或先取得许可。
法务评审属日历项，在里程碑 6 之前启动。

## 11. 决策记录

### 11.1 已定决策（正文为准，本表只作索引）

| # | 事项 | 取值 | 正文位置 |
| --- | --- | --- | --- |
| D1 | overlay 形态 | **零补丁 wrapper**：独立 module + 自有 main，不改上游源码（2026-09-05 在 v1.14.0 上实证） | §4.7 |
| D2 | 首期协议范围 | **VLESS + Shadowsocks**（仅 `2022-blake3-aes-128-gcm` / `-aes-256-gcm` 的 `users[]` 具名多用户）；拒绝单用户、legacy AEAD、relay、`managed: true`，并与 SSM API 互斥。其余协议按 §2.4 逐个验证后追加 | §1、§2.4、§4.6 |
| D3 | 快照 schema | **自有 v2**：`schema_version = 2`、路由 `GET /v2/snapshot`，容器与标识按 sing-box 命名（`inbounds[]`/`tag`/`type`，`listen` + `listen_port`），不设 `identity_kind`，`health` 三位。结算语义与 `shadowsocks-rust-plus` 同构，其参考校验器须分叉重写 | §1、§4.5、§5.1、§6 |
| D4 | 构建 tag 裁剪 | **生产集 = `with_utls` + `badlinkname` + 自有 `with_user_stats`**；上游默认集其余 15 项全砍（含 `with_quic`、`tfogo_checklinkname0`）。裁 tag 不等于隔离上游账本 | §9.1、§4.6 第 7/8 条 |
| D5 | 热用户增删接口 | **不提供**：快照 socket 只读，用户变更走受控重启或 SIGHUP 重载。§4.9 的配额控制端点是另一个只写 socket，只改额度、不改用户集，不构成对本条的改判 | §4.3、§5.3、§4.9 |
| D6 | 进程崩溃丢尾账 | **接受**（与 `shadowsocks-rust-plus` 一致）：纯内存 registry，尾账按未闭合窗口审计，不引入 WAL | §1、§4.4、§5.3、§12 |
| D7 | 访问审计形态 | **同进程 JSONL 旁路，默认关闭**：不建独立进程、不做 ingest/spool/ACK/HMAC，不保证事件不丢；计费仍只走快照。放弃的是“丢失的可判定性”，换来约 320 行对 28519 行 | §4.8、§5.2 |
| D8 | 配额闸断形态 | **collector 判定 + 进程执行，下发剩余额度而非布尔标记**：独立只写 UDS、`PUT /v2/quota` 全量覆盖、闸断实施在 CountFunc 内、状态不进快照、共用 `with_user_stats`；可选能力，默认关闭 | §4.9、§5.4 |

D1 的实证覆盖：独立 module 构建（含最小 tag 集与 linux/amd64 交叉）、`Router().AppendTracker`
注入、真实转发 TCP 四向计数、SIGHUP 跨 Box 计数延续、匿名连接失败关闭（0 字节应用负载外发）、
配置期全部失败关闭判定、自有 `services[]` 配置类型与 `format` 往返、最小 registry 的体积与解码期
门禁。该形态**确定做不到**的只有一项：在 sing-box 配置里新增**顶层**键——已改走 `services[]`，
无需补丁。引入补丁的触发条件见 §4.7 末段。

### 11.2 后续评估项

当前无未决决策。以下记录已定决策的重新评估触发点：

| # | 已定取值 | 重新评估时点 | 触发条件 |
| --- | --- | --- | --- |
| D3 | 自有 v2 schema | 首个下游接入方上线后 | 出现现有字段无法表达的计量维度；届时按 §4.5 的命名纪律追加字段并提升 `schema_version` |
| D5 | 不提供热用户增删接口 | 里程碑 6 之后 | 运维反馈“每次改用户都要重载”不可接受，或 §2.4 追加的协议缺少等价的受控重启路径 |
| D6 | 接受进程崩溃丢尾账 | 里程碑 5 长跑结束 | staging ≥ 7 天实测的未闭合窗口频次与字节量超出业务容忍。改判即引入 WAL 或持久计量数据面 |
| D7 | 同进程 JSONL 旁路 | 首次因审计数据被追责或被要求举证时 | 出现“需要证明某条记录未被事后改写”的场景。同 uid 写入决定了数据面被攻破即可就地改写历史，这一点**没有廉价缓解**，改判意味着独立 uid 与独立进程，即参考实现那条路 |
| D8 | tracker 层闸断 + 重连节流 | 首个按量计费套餐上线后 | 重连风暴的实测成本超出节流可缓解的范围，或出现“必须让客户端看到协议层认证失败”的体验/合规要求。改判即走 §4.9 末段的摘凭据路线，与 D5 改判绑定，不可单独实施 |

D5 若改判为“提供”，必须另开独立端点：不得复用只读快照 socket，需要单独授权、幂等 command id
与失败关闭。VLESS 侧底层能力已存在（`sing-vmess vless/service.go:40 UpdateUsers`）；
但 Shadowsocks 侧的等价能力（`MultiInbound.UpdateUsers`）与数据面读取之间**没有锁**
（写 `inbound_multi.go:132` / 读 `:170`），且索引即计费身份，因此若开放 SS 热更新必须自行加锁并
保证索引稳定，不能直接复用上游路径。

## 12. 已知差额与风险

以下项目会造成“节点出流量 − 用户账单”的差额，属设计内的已知缺口，须在运维文档中声明并单独告警，
不得当作计量 bug 处理：

| 来源 | 性质 | 处置 |
| --- | --- | --- |
| REALITY 握手校验失败的伪装中继 | 在 utls 内部完成，任何 tracker 不可见；未设 `LimitFallback`，不限速 | 声明不支持；对 Dest 与带宽单独设告警阈值 |
| ShadowTLS 握手/校验失败中继 | 不经 router | 声明不支持 |
| Shadowsocks 认证失败、盐重放被拒、EIH 不匹配的流量 | 在 service 返回错误处即被丢弃并只记日志，不经 router、不计账 | 声明为已知差额；SS 不做伪装中继，连接直接关闭，量小，不单设带宽告警 |
| Shadowsocks relay 的 router tracker 口径 | 计到的是盐+密文+AEAD tag+随机 padding，非 payload 且不确定（同一 4 字节往返，上行 5 次实测 127–900） | 配置校验直接拒绝，不进入白名单（§2.4） |
| 匿名/无身份连接的失败关闭仍会向目的地拨号 | 先拨号后 copy，tracker 只能阻断负载；目的地看到 0 字节完成握手的 TCP 连接（UDP 无此问题） | 计量口径无差额（0 字节、不入账）；若需连接级阻断只能引入补丁（§4.7） |
| 进程崩溃前未采集的尾账 | 内存 registry 无法恢复 | 按未闭合窗口审计（D6） |
| 重启期间被强杀的连接 | 已计字节不丢，但连接中断 | 走 §5.3 计划重启流程；仅改用户集时用 SIGHUP 重载 |
| 计费 inbound 误配 `hijack-dns` / `reject` | 字节不经 tracker | 配置校验失败关闭（§4.6） |
| 协议 header、隧道封装与 TCP 重传 | 不在应用 payload 口径内 | 在计费说明中声明，与网卡计费天然有差 |

启用 §4.8 基础访问审计时，另有一组性质不同的已知缺口——它们不影响账单，只影响审计记录的完整性
与可信度，同样须在运维文档中声明：

| 来源 | 性质 | 处置 |
| --- | --- | --- |
| 崩溃丢失缓冲区内的记录 | 不做 `fsync`，且该丢失**不产生 gap 行**，事后无法与“本就没有访问”区分 | 在 `docs/OPERATIONS.md` 如实声明；需要不丢即需另建 spool（D7） |
| 审计文件与数据面同 uid | 数据面被非 root 攻破即可就地改写或删除自己的历史 | `0600` 可挡本机其他非 root 账号，**同 uid 改写无廉价缓解**；需要抗篡改即需独立 uid 与独立进程（D7） |
| `host` 来自 sniff 时可能是伪装外层 | 启用 ECH 的客户端给出的是伪装 SNI，属**错误记录**而非缺失 | `host_src` 字段标明来源，下游按可信度分级处理 |
| 未启用 sniff 或客户端直接给 IP | 只能记 IP，无域名 | 正常降级，`host_src` 为 `ip` |
| UDP 只到 association 粒度 | 记首包目标，一条 QUIC session 内的多目标不可见 | 记录中标注；需要逐包目标即需另立设计 |
| 不经 router 的伪装中继（REALITY / ShadowTLS） | 同上表，tracker 不可见 | 审计侧同样声明不支持 |

启用 §4.9 配额闸断时，另有一组缺口——它们不影响计量与账单的准确性，只影响“超额之后多久能停下来”
与“停下来的代价”，同样须在运维文档中声明：

| 来源 | 性质 | 处置 |
| --- | --- | --- |
| 下发延迟内的超额 | 进程按最近一次全量表实时扣减，误差已从“峰值带宽 × 采集周期”压到只剩下发延迟，但 collector 自身的采集与推送周期内，额度仍基于上一轮基线 | 按 §5.4 的顺序推送；窗口大小与 `stale_after` 一并定值并写进计费说明 |
| 控制面失联期间的额度失效 | `stale_after` 超时后按 `stale_action` 处置：`allow` 意味着超额可继续跑，`deny` 意味着 collector 成为转发链路的单点 | 二选一是商业决策，须在 `docs/OPERATIONS.md` 显式声明，不设“聪明”默认（§4.9） |
| 被闸断用户的重连风暴与空拨号 | 每次重连消耗公共的 accept 队列与握手 CPU，并向目的地产生一次完成握手、0 字节的拨号（与本节首表“匿名/无身份连接的失败关闭仍会向目的地拨号”同一机制） | 逐 lineage 重连节流为首期必做，但它只压低本机侧的握手后成本，**拨号仍然发生**——tracker 在拨号之前拿不到否决权；根治需摘凭据，见 §4.9 末段与 §11.2 D8 |
| 进程重启后的额度清零 | 纯内存，重启即全部解封，直到 collector 重推 | `PUT /v2/quota` 的 409 使 collector 立即察觉；重推前按 `startup_action` 处置（§4.9） |

主要执行风险：上游 1.14 的接口与生命周期改动较大，每次 minor 升级需预留 rebase 与全量对账
（§6、§9.2）；1.14.x 目前依赖若干 beta 模块，需锁定并跟踪其转正节奏；复制自上游的 9 个 CLI 文件
是持续的漂移面，须靠 `scripts/verify.sh` 的门禁而非人工记忆维护。启用 §4.8 后还须防一类特有的
自欺：审计的失败路径大多以“看起来一切正常”呈现（粘滞写错误、双写、半行、写进已删除 inode），
因此纪律 1/3/4/5/9/10/12 的变异检验是发布门禁的一部分，不是可选的加分项。
