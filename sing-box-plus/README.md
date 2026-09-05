# sing-box-plus 多用户流量统计调研

> 上游仓库：[`SagerNet/sing-box`](https://github.com/SagerNet/sing-box)
>
> 审计基线：[`v1.13.19`](https://github.com/SagerNet/sing-box/releases/tag/v1.13.19) /
> `b5ebaa1fc0f2b94256180b95468e73ef53caa27d`（2026-08-17；本文源码断言的核对对象，现已降为 oldstable）
>
> 实施基线（建议）：[`v1.14.0`](https://github.com/SagerNet/sing-box/releases/tag/v1.14.0) /
> `0b899587`（2026-08-30，stable 轨道）；观测 PoC 可用 `v1.13.21`
>
> 最近核对：2026-09-05。下次核对触发：上游发布新 minor、`sing-box-manager` 升级数据面版本、
> 或本目录进入实现阶段
>
> 调研初稿：2026-08-27

本报告评估：能否像本仓库已经实现的 `shadowsocks-rust-plus` 一样，给 sing-box 增加可用于
多用户配额和结算的流量统计。这里的“像”不是只显示实时速率，而是要求认证后的稳定用户归属、
TCP/UDP × 上下行四个累计值、重载边界、可幂等采集以及受控的本机快照接口。

## 0. 当前状态与下一步

本目录目前只有本报告，尚无 overlay 实现、脚本或测试。

下一步（里程碑 1，详见 §8.3）的具体交付物是一棵可验证的骨架：

```text
sing-box-plus/
├── README.md
├── upstream.lock                 # 钉死 repository / tag / commit / tree sha256 / go 最低版本
├── .env.example                  # UPSTREAM_REPOSITORY、GOMODCACHE、SING_BOX_BUILD_TAGS 占位
├── scripts/prepare-source.sh     # 按精确 commit 取源码并校验
├── scripts/verify.sh             # go vet / go test -race / lint / 敏感信息扫描
└── docs/UPSTREAM_BASELINE.md     # 基线、编译验证记录与升级规则
```

在骨架落地并冻结基线之前，不应开始写 tracker 代码——§10.2 的升级规则要求所有实现都绑定到
一个已记录的上游提交。

## 1. 结论

| 目标 | 结论 |
| --- | --- |
| 查看具名用户的上下行累计流量 | **能**；需自定义构建（`with_v2ray_api`）加 Manager 编译器/Agent 改动，见 §7.1 |
| VLESS + REALITY + XTLS Vision 用户是否能归属 | **能**；VLESS 认证完成后会把非空 `users[].name` 传播为 `metadata.User` |
| 内置接口能否区分 TCP/UDP 四方向 | **不能**；TCP、UDP、XUDP 最终合并为 uplink/downlink 两项 |
| 内置接口能否直接做严格账务 | **不能**；缺少运行周期、代次、稳定快照和持久化，重载会丢未采集增量 |
| 能否开发成 `shadowsocks-rust-plus` 同等级能力 | **能，且可行性高**；协议、认证身份、统一 tracker 和计数路径都已存在 |
| 是否需要改每一种代理协议 | **通常不需要**；应扩展认证后的通用 `ConnectionTracker`，再验证特殊数据路径 |
| 是否需要改上游源码 | **多半不需要**；`AppendTracker` 等接入面已导出，默认走零补丁 wrapper，见 §5.5 |
| 推荐路线 | 先用内置统计做观测；需要配额/结算时维护一个小型、固定版本的 hardened overlay |

sing-box 不缺 VLESS、REALITY、Vision，也不缺用户身份传播。主要工作集中在**计数维度、
生命周期和可靠导出**，因此适合作为生产扩展基线。

## 2. 术语与字段对照

三个子项目对同一概念使用不同名称，本报告统一如下。实现和 collector 必须按本表取名，不得混用。

| 概念 | sing-box | `shadowsocks-rust-plus` | `sing-box-manager` | 本项目采用 |
| --- | --- | --- | --- | --- |
| 计费身份名 | `users[].name` → `metadata.User` | `users[].name` / `identity_name` | `identity_name`（由不可变 `(user_id, route_id)` 派生，`store/users.rs:16-24`） | `name`（快照字段）；正文称 billing name |
| 入站标识 | inbound `tag` | `server_id` | `inbound_tag`，Agent 硬编码常量 `"in-shared"`（`agent/ssm.rs:14`；SS 与 VLESS Entry 共用，`compiler/entry.rs:65,96`） | `server_id` := inbound tag |
| 节点标识 | — | `node_id` | Manager 的 `node_id` 指中继出口节点，与此不同 | `node_id` := Manager 的 `entry_id`，由编译器写入 exporter 配置，Agent 拒绝不符的快照 |
| 运行周期 | — | `runtime_id`（32 位小写 hex，进程级随机） | `singbox_boot_id`（Agent 本地 `MAX+1` 整数，`agent/state.rs:90-92`）；另有 `entry_runtime_epochs.epoch` | 快照给 `runtime_id`；Agent 负责与本地 epoch 一对一绑定（§7.2） |
| 快照序号 | — | `sequence`（每次 `/v1/snapshot` 严格递增） | `StatsBatch.sequence` 已被结算屏障占用：poll 恒 0，final 由 outbox 分配并作去重键（`agent/stats.rs:17`、`agent/barrier_store.rs:44-49`） | 新字段 `exporter_sequence`，**不得**复用 `StatsBatch.sequence` |
| 代次 | — | `generation`（同名重激活复用，不递增） | 基线键无此维度（`0006_metering.sql`） | `generation`，语义同 ss-rust-plus，固定输出 `1`，但不得从 collector 键中省略 |

单 Host 最多一个 Entry（`sing-box-manager/docs/architecture.md:56`），因此 `node_id + server_id`
在本部署中无歧义。下文示例一律使用 `in-shared` 作为 inbound tag。

## 3. sing-box 现在已经有什么

### 3.1 Experimental V2Ray StatsService

sing-box 已实现与 V2Ray StatsService 兼容的 gRPC 服务。配置中可以列出需要统计的 inbound、
outbound 和 user；router 会为命中的 TCP 与 packet connection 包装原子计数器。官方文档也明确
给出了 `stats.users` 字段：见
[V2Ray API 配置文档](https://sing-box.sagernet.org/configuration/experimental/v2ray-api/)（访问日期
2026-09-05）和
[稳定版实现](https://github.com/SagerNet/sing-box/blob/b5ebaa1fc0f2b94256180b95468e73ef53caa27d/experimental/v2rayapi/stats.go)。

它默认不在构建产物中，部署构建必须把 `with_v2ray_api` 加到既有 build tags；缺少该 tag 时配置
会明确报错。见[官方构建标记文档](https://sing-box.sagernet.org/installation/build-from-source/)和
[stub 实现](https://github.com/SagerNet/sing-box/blob/b5ebaa1fc0f2b94256180b95468e73ef53caa27d/include/v2rayapi_stub.go#L1-L16)。
上游默认 tag 集（`release/DEFAULT_BUILD_TAGS`）不含该 tag，官方与 Homebrew 二进制都不可用于本用途。

脱敏后的最小结构如下；`identity_name` 必须同时出现在 inbound 用户和统计白名单中：

```json
{
  "inbounds": [
    {
      "type": "vless",
      "tag": "in-shared",
      "users": [
        {
          "name": "u_example_01",
          "uuid": "<运行时注入的 UUID>",
          "flow": "xtls-rprx-vision"
        }
      ]
    }
  ],
  "experimental": {
    "v2ray_api": {
      "listen": "127.0.0.1:8080",
      "stats": {
        "enabled": true,
        "users": ["u_example_01"]
      }
    }
  }
}
```

该接口当前产生以下 key：

```text
user>>>u_example_01>>>traffic>>>uplink
user>>>u_example_01>>>traffic>>>downlink
```

使用时必须满足以下约束：

- `users[].name` 必须非空；VLESS 用户名为空时只会在日志中显示数字索引，`metadata.User` 仍为空，
  不会进入 user counter；
- 用户名应稳定且全局唯一；内置 key 不含 inbound tag，同名用户跨 inbound 会合并；
- `stats.users` 是静态白名单，新用户不会自动加入；
- counter 在该用户**首次被路由**时懒创建（`stats.go:214-222`），与是否已传输字节无关；
  `not found` 只意味着“本进程启动后从未路由过该用户”，collector 必须按 0 处理，不能当作错误；
- 只能使用 `reset=false` 的累计查询做外部差分，不能把破坏性 reset 当作结算协议；
- gRPC 线路上的服务名被硬编码覆写为 `v2ray.core.app.stats.command.StatsService`
  （`stats.go:21`），不是 proto 声明的包名；用后者调用会得到 `unknown service`；
- gRPC listener 使用 insecure credentials，没有内置认证或 TLS，只能绑定回环或置于独立的受控代理后。

因此，内置能力适合静态用户集的监控、用量展示和方案验证，不足以单独承担收费或硬配额。

### 3.2 VLESS、REALITY 和 Vision 的用户身份链路

VLESS 服务先解析并验证 UUID、flow 和 command，之后 sing-box handler 从认证 context 取出用户
索引，把非空 `users[].name` 写入 `metadata.User`，再交给 router。见
[VLESS 服务认证](https://github.com/SagerNet/sing-vmess/blob/3aed155119a1/vless/service.go#L55-L96)和
[VLESS inbound 用户传播](https://github.com/SagerNet/sing-box/blob/b5ebaa1fc0f2b94256180b95468e73ef53caa27d/protocol/vless/inbound.go#L167-L205)。
在 v1.14.0 中该方法已改名（`NewConnectionEx` → `NewConnection`，`protocol/vless/inbound.go:150`），
身份传播逻辑不变。

REALITY 握手和 Vision 解码并不会抹掉这个身份。tracker 在认证和协议解码之后、路由选出 outbound
之后附加（`route/route.go:152`、`:278`），所以正常通用转发路径统计的是代理应用 payload：不包含
REALITY/TLS 外层、VLESS header 和 Vision padding；被代理的用户内层 TLS record 属于应用 payload，
会计入。Vision 的 padding 在 `VisionConn.Read/Write` 内剥离、counter 在其外层，这一点已核实。
官方 [VLESS inbound 文档](https://sing-box.sagernet.org/configuration/inbound/vless/)确认每个用户
都有 `name`、`uuid`、`flow` 字段，`xtls-rprx-vision` 已是现有能力。

错误 UUID、flow 不匹配和未进入 router 的连接不会形成 user counter。认证身份与路由层也已经
通过 `auth_user` 统一使用，见[路由规则文档](https://sing-box.sagernet.org/configuration/route/rule/)。

### 3.3 其他已有接口为什么不能替代账本

| 接口 | 用户维度 | TCP/UDP 字节 | 生命周期/持久化 | 判断 |
| --- | --- | --- | --- | --- |
| V2Ray StatsService | 有，静态白名单 | 合并 | 无 | 可观测，不可直接结算 |
| Clash API | 无用户累计；connection JSON 不导出 user | 仅连接 network | 关闭详情有界 | 不能可靠反推 |
| 1.14 API service | 每连接有 user/network | 可从事件区分 | 事件可丢、历史有界 | UI/诊断用途，不是账本 |
| clashapi trafficontrol | 每连接内部有 user | 每连接可辨别 | 较旧关闭连接只进全局累计 | 不能恢复完整用户累计 |
| SSM API | 有，但仅 managed Shadowsocks | 字节合并 | 可选 JSON cache | 不能覆盖 VLESS/通用协议 |
| Prometheus `/metrics` | 不存在通用用户流量 exporter | — | — | 不能依赖 |

包名随版本变化：1.13.19 是 `experimental/clashapi/trafficontrol`（上游拼写少一个 c），
1.14 起移到 `common/trafficcontrol` 并在配置 `clash_api` 或 API service 时挂为 tracker
（`v1.14.0:box.go:246-251`）。1.14 的“新 API service”并不是第三套账本，而是同一连接账本经
daemon gRPC `SubscribeConnections` 的导出面。它比 Clash API 多导出了 user 和 network，但没有
按用户累计、runtime ID、sequence 或可靠重放；事件总线拥塞和关闭历史淘汰都会造成不可恢复缺口。
适合界面展示，不适合账务。

### 3.4 SSM API 只能作为 Shadowsocks 的参考

SSM service 自 1.12 起可管理 Shadowsocks inbound，并记录每用户 uplink/downlink bytes、packet
和 TCP/UDP session；官方范围明确限定为 Shadowsocks，见
[SSM API 文档](https://sing-box.sagernet.org/configuration/service/ssm-api/)。字节仍把 TCP/UDP 合并，
而且不能附加到 VLESS inbound。

它的 `cache_path` 每分钟及正常关闭时保存统计和用户，但不应被当作严格账本：

- 崩溃可能丢最近一分钟，cache 错误不会让代理失败关闭；
- `clear=true` 与 V2Ray `reset=true` 一样有“已清零但响应丢失”的永久漏账窗口；
- 删除用户会移除对应 counter map，没有 tombstone 或 generation；
- cache 同时保存 Shadowsocks 用户凭据，稳定版按 `0644` 创建文件（`service/ssmapi/cache.go:80`），
  路径必须严格保护；
- 直接覆盖文件而非事务提交，统计和用户更新也缺少账务级的一致性契约。

另外 SSM tracker 是在 Shadowsocks inbound 内、`RouteConnection` **之前**包装连接
（`protocol/shadowsocks/inbound_multi.go:177-181`、`:203-206`），而 router tracker 在选出 outbound
之后包装，两者口径不同，不能互作字节 oracle（详见 §6）。

因此不能把 SSM cache 抽象成“sing-box 已经有可靠持久化用户统计”；它可以为 overlay 提供 tracker
与 API 设计参考，但不应直接复用其存储语义。

## 4. 与 shadowsocks-rust-plus 的差距

`shadowsocks-rust-plus` 的可结算契约不仅是四个 counter。sing-box 内置实现还缺少：

| 能力 | sing-box 内置现状 | `sing-box-plus` 应达到 |
| --- | --- | --- |
| 用户身份 | 已认证的 `metadata.User`，但可为空且内置 key 全局合并 | 非空稳定 billing name，并带 inbound 与代次 |
| 方向 | uplink/downlink | TCP/UDP × uplink/downlink 四项 |
| 数值 | `atomic.Int64`，可回绕为负 | 饱和 `u64`，overflow 进入 unhealthy |
| 查询 | map 遍历，破坏性 reset 可选 | 非破坏性、稳定排序的累计快照 |
| 运行周期 | 无 | `runtime_id`、启动时间、严格递增 `sequence` |
| 用户生命周期 | 删除即消失或重载重建 | `active`、稳定 lineage、tombstone 保留 |
| 重载 | SIGHUP 关闭旧 Box 并创建新 Box，未采集量丢失 | 最终结算屏障（主路径）或跨 Box 存活的 registry（可选） |
| 接口安全 | 无认证的 TCP gRPC | 本机 UDS、权限与资源上限、受监督 |
| 账务 | 无幂等协议 | 外部 collector 按完整基线键差分与幂等落库 |

内置 StatsService 的 `GetStats/QueryStats(reset=true)` 使用 `Swap(0)`；请求已经清零而响应丢失时，
这段流量无法恢复。多个 counter 也不是事务快照，map 顺序不稳定。见
[查询实现](https://github.com/SagerNet/sing-box/blob/b5ebaa1fc0f2b94256180b95468e73ef53caa27d/experimental/v2rayapi/stats.go#L121-L218)。

更重要的是，命令行收到 SIGHUP 后会关闭当前实例，再从配置创建新的 Box；StatsService 属于 Box，
没有跨实例状态交接。见
[重载循环](https://github.com/SagerNet/sing-box/blob/b5ebaa1fc0f2b94256180b95468e73ef53caa27d/cmd/sing-box/cmd_run.go#L171-L200)和
[Box 关闭路径](https://github.com/SagerNet/sing-box/blob/b5ebaa1fc0f2b94256180b95468e73ef53caa27d/box.go#L496-L535)。
上游 issue #4059（2026-04-19 开启，同日以 not planned 关闭）报告的正是这一丢数边界，见
[SagerNet/sing-box#4059](https://github.com/SagerNet/sing-box/issues/4059)。

## 5. 推荐的 sing-box-plus 架构

### 5.1 在统一 tracker 扩展，不逐协议插桩

sing-box 已经在 `adapter.InboundContext` 统一保存 inbound、network 和 user，并在 router 中调用
`adapter.ConnectionTracker`。最小正确扩展应新增一个 feature/build-tag gated 的用户统计 tracker：

```text
协议握手与用户认证
  -> metadata { inbound, network, user }
  -> 路由与 outbound 选择
  -> UserStatsTracker
       RoutedConnection       -> tcp_uplink / tcp_downlink
       RoutedPacketConnection -> udp_uplink / udp_downlink
  -> 通用 copy / packet copy / splice
  -> 进程级 registry
  -> 本机只读累计快照
  -> 外部 collector 差分与幂等入账
```

接口与注入点随版本变化，实现必须按实施基线写：

- 1.13.19：`adapter.ConnectionTracker` 只有两个方法（`adapter/router.go:33-36`），上游自己在
  `box.go:354`、`:364` 用 `AppendTracker` 挂 clash/v2ray tracker；
- 1.14.0：接口新增第三个方法 `RoutedFlow(ctx, metadata, matchedRule, matchOutbound) tun.FlowTracker`
  （`adapter/router.go:104-108`），注入点移到 `box.go:246-251` 与 `:438`。该路径只在 TUN/L3 形态可达、
  口径是 IP 包全长且 `metadata.User` 恒空，本项目实现为固定返回 `nil`，不参与计费；
  挂载顺序需与 trafficcontrol/StatsService 明确，避免双层 CounterConn 叠加。

建议的逻辑 key 和记录：

```go
type IdentityKey struct {
    ServerID    string // inbound tag
    Generation  uint64 // 固定为 1，见 §5.3
    BillingName string
}

type UserTraffic struct {
    TCPUplinkBytes   SaturatingUint64
    TCPDownlinkBytes SaturatingUint64
    UDPUplinkBytes   SaturatingUint64
    UDPDownlinkBytes SaturatingUint64
}
```

认证凭据（UUID、密码、PSK）只用于 auth table，不能进入统计 key、快照或普通日志。数据面每条连接
认证成功后只持有对应 record/counter 指针，不应每次 I/O 查用户表或加全局锁。

**实现不变量**：sing 的 `bufio.Copy` 只有能从连接链顶端解包出 counter 时才在“目标写成功后”计数，
否则回落为“源读成功”计数。因此自研 wrapper 若包在 `CounterConn` 之外，必须实现 `Upstream()` 与
`Reader/WriterReplaceable()`（可参照 clashapi `tracker.go` 的做法），否则口径会静默退化。
§9 需要一条“多 tracker 叠加下 unwrap 仍生效”的断言测试。

### 5.2 计数口径

建议沿用 `shadowsocks-rust-plus` 的应用 payload 口径：

- TCP uplink：协议解码后的用户 payload 被通用转发边界成功写入目标侧的字节；
- TCP downlink：目标侧 payload 被代理编码 writer 接受、准备回传给用户的逻辑字节；
- UDP uplink/downlink：完整逻辑数据报成功交给另一侧后累计 payload 长度；
- 不统计外层 REALITY/TLS、VLESS/VMess/Trojan/SS header、Vision padding、mux/XUDP/UoT framing、
  TCP/IP header 或重传；
- XUDP、UDP-over-TCP 和 mux 内的逻辑 UDP 仍归入 UDP，而不是因为底层 carrier 是 TCP 就归入 TCP。

sing 的标准 copy 会解包 counter wrapper，并在目标写成功后调用双方计数 callback；Linux splice
路径也显式按成功传输字节调用 counter，可以直接复用：

- [TCP counter wrapper](https://github.com/SagerNet/sing/blob/7c349dacf402256d3a7029746073b05d2ead584a/common/bufio/counter_conn.go)
- [packet counter wrapper](https://github.com/SagerNet/sing/blob/7c349dacf402256d3a7029746073b05d2ead584a/common/bufio/counter_packet_conn.go)
- [通用 copy](https://github.com/SagerNet/sing/blob/7c349dacf402256d3a7029746073b05d2ead584a/common/bufio/copy.go)
- [Linux splice](https://github.com/SagerNet/sing/blob/7c349dacf402256d3a7029746073b05d2ead584a/common/bufio/splice_linux.go)

**Vision 不进入 splice**：`VisionConn` 不实现 `Reader/WriterReplaceable`，也不暴露 `syscall.Conn`，
sing 的 unwrap 会停在该层，只走用户态 direct 读写。因此对账项应写成
“buffered → direct（用户态直读 netConn）切换”；splice 对账要针对无 `flow` 的 VLESS、
socks/http/mixed 与明文 TCP 等可解包到 `syscall.Conn` 的入口。

实现前仍需对以下特殊路径做字节对账矩阵：Vision buffered/direct 切换和 early data、mux、
XUDP/UoT、DNS hijack、特殊 outbound 直接读写、UDP batch、连接取消与 half-close、重载期间仍存活
的长连接。任何绕过标准 router tracker 的 handler 都必须明确选择“计数、拒绝或声明不支持”，
不能静默转发但漏计。

**结构性不可见的路径**（overlay 无法触及，只能声明不支持并计入“节点出流量 − 用户账单”差额）：

- REALITY 握手校验失败时的伪装中继，在 utls 内部完成双向 `io.Copy`
  （`metacubex/utls reality.go:326,424,542-547`），不经 inbound/router；sing-box 未设 `LimitFallback`，
  该中继不限速，需要对 Dest 与带宽单独设告警阈值；
- ShadowTLS 的握手/校验失败中继，同类（§6）。

此外，计费用 inbound 不得配置 `hijack-dns` 或以 `reject` 结束的规则动作：这两类动作在
`route/route.go:126-137`（TCP）与 `:256-263`（UDP）处理并 return，早于 `:152` / `:278` 的
tracker 循环，字节不会被计入。

### 5.3 重载模型：屏障优先，进程级 registry 可选

**目标重载模型 = `sing-box-manager` 现状的进程重启 + 已实现的两阶段结算屏障。**
Manager 的 Agent 从不发 SIGHUP，`restart` 是 kill 旧子进程再以固定 argv `run -c` spawn
（`src/agent/runtime.rs:45-71`，全仓无 reload 路径）；每次部署分配新 `runtime_epoch`，
final 批经 `traffic_batches` 按 `(entry, boot_id, sequence)` 精确一次去重
（`migrations/0006_metering.sql:27-36`、`migrations/agent_0003_barrier.sql`、`src/agent/settle.rs`）。
因此本项目首期只需让 VLESS 进入这条已存在的屏障，不必先做跨 Box registry。

跨 Box 的进程级 registry 仅在需要 `kill -HUP` 热重载、且要求累计值不清零的独立部署形态下才必需。
若要做，形态如下：

1. 进程启动生成新的 `runtime_id`，创建唯一 registry/exporter；
2. 每次加载 Box 时把同一 registry 注入新的 UserStatsTracker；
3. 同一 runtime 内 inbound tag 与 billing name 是稳定计费身份：删除/停用只切 `active=false` 并保留
   在快照；同名重建复用原 generation 与原计数器并切回 `active=true`；凭据轮换不改变 lineage。
   `generation` 作为 schema 保留维度固定输出 `1`，但不得从 collector 键中省略。
   tombstone 数达到 `max_identities` 时应失败关闭或标记 unhealthy，而不是丢弃 lineage；
4. 上游重载会**强制关闭全部在途连接**：`Box.Close()` 关闭 connection manager，后者 `CloseAll()`
   遍历强杀（`route/conn.go:52-69`）。由于计数按每次 copy 迭代增量累加，强杀不丢已计字节，
   但会打断用户连接。若要排空，必须在 `run()` 与 `Box.Close()` 之间插入 drain 阶段
   （停止 accept → 等待或超时 → 再 Close）并接管 `FatalStopTimeout` 看门狗，这是新增改动，
   不是配置策略；
5. SIGHUP 期间旧 Box 先完全关闭再建新 Box（`cmd/sing-box/cmd_run.go:188` `cancel()` →
   `:191` `Close()` → `:174` `create()`），端口有短暂无监听窗口；仓库内没有 SO_REUSEPORT 或监听 fd
   传递，overlay 不改变这一点。若确需端口不中断，须单列“引入 SO_REUSEPORT 或 listener fd 传递”
   为独立工作项；
6. 进程真正重启才产生新 `runtime_id`，由外部 collector 明确选择新周期首快照策略。

跨 Box 注入本身已可验证为可行：`globalCtx` 已持有 service registry（`cmd/sing-box/cmd.go:70`），
`box.New` 在 ctx 已有 registry 时原样复用（`box.go:101`），因此用 `service.ContextWith` 注入
process-scope registry 无需改上游签名。代价是 registry 全进程共享、`MustRegister` 会覆盖旧 Box 的
同类服务，代次必须由自有 registry 维护。该 registry/exporter 只覆盖 `sing-box run` 宿主；
`daemon`、libbox 与 1.14 新增的 `boxdd` 形态不在支持范围内。

纯内存 registry 仍无法恢复进程崩溃前尚未采集的尾账，这与 `shadowsocks-rust-plus` 相同。若业务
要求“内核/进程崩溃也不丢一个字节”，需要另行加入 WAL 或持久计量数据面，复杂度会明显上升；
不能把高频轮询描述成严格保证。

### 5.4 快照与安全契约

**采用 `shadowsocks-rust-plus` v1 的既有形状**（`docs/API.md`），而不是另立一套：这样
`tests/http_unix.py`、`scripts/user-stats-client.py`、`tests/settlement_model.py` 与 mock collector
可以直接作为本项目的契约测试。sing-box 特有信息作为可选附加字段追加（现有校验器忽略未知键），
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

字段与格式约束：`runtime_id` 为 32 位小写 hex；`started_at_unix_ms`、`sequence` 为正整数，
同一 `(node_id, runtime_id)` 内 `started_at_unix_ms` 固定；服务按 `server_id` 再按 `generation`
排序，用户按 `name` 再按 `generation` 排序。`inbound_type`、`tcp_sessions`、`udp_sessions` 是
sing-box 的附加字段，`*_sessions` 是**当前活跃数、不参与结算**，专供 Agent 的排空闸门使用
（见 §7.2）。

传输层与失败语义沿用 v1：HTTP/1.1-over-Unix-stream，每连接单请求单响应，禁 keep-alive/query/body，
固定两条路由——`GET /v1/snapshot`（被接受时即推进 `sequence`）与 `GET /healthz`（200/503，不推进
`sequence`）；错误一律返回固定 `{schema_version, error:{code}}` 对象，错误码表与 v1 一致
（400/404/405/408/413/429/500/505）。采集器处置：429 与连接被直接关闭视为**可重试且不得入账**，
其余非 200、`Content-Length` 不符、JSON 截断或 `schema_version` 不匹配一律拒绝入账。

接口必须是非破坏性累计快照，并具备稳定排序、请求/响应大小、身份数、并发数、读写超时等上限；
socket 默认 `0600`，检查父目录、符号链接、旧 socket 和 inode 替换。exporter 启动失败应阻止统计
模式启动；运行中异常退出应由主服务监督并触发整体失败或重启，不能继续转发但停止计量。

外部 collector 用以下完整维度保存 baseline：

```text
node_id + server_id + server_generation + name + identity_generation + runtime_id
```

再结合 snapshot sequence、采集批次 ID 和四项 delta 幂等入库。累计值倒退、sequence 倒退、未知
runtime、overflow 或 unhealthy 快照都应失败关闭，不能猜测并继续收费。Manager 侧现有的
`delta_bytes = max(0, cur-last)` 与此不同，处理办法见 §7.2。

### 5.5 overlay 形态决策

**默认方案 A：零补丁 wrapper。** 1.13.19 与 1.14.0 都已导出 `adapter.Router.AppendTracker`、
`box.New`、`Box.Router()` 与 `include.Context`，因此一个独立 Go module
（`sing-box-plus/cmd/sing-box-plus`，`go.mod` 中 `require` 钉死版本的 sing-box）在自有 main 里
复刻 `cmd_run` 循环，并在 `Start()` 之前调用 `instance.Router().AppendTracker(tracker)` 即可，
无需修改上游源码；registry 与 exporter 由该 main 持有，天然满足 §5.3 的进程级要求。

**方案 B：patch series。** 仅当必须改上游内部路径（vless/vision、splice、`option` schema、
需要在 tracker 之前失败关闭的位置）时使用，按 `patches/series` 顺序重放，无法零 fuzz 应用即失败。

选择标准：默认 A；A 覆盖不到的失败关闭点再以最小 patch 补，两者可共存。结论必须在里程碑 1
写入 `upstream.lock` 与 `docs/UPSTREAM_BASELINE.md`。

**硬约束**：`sing-box-manager` 的 Controller 与 Agent 都会实跑 `<bin> check -c` 并用
`sing-box version` 探测版本（`src/agent/singbox.rs`、`src/agent/deploy.rs:60-71`），因此自有二进制
必须保持 `run` / `check` / `format` / `version` 的 argv 与退出码兼容，版本字符串格式也需与
Manager 的解析约定一致。

## 6. 协议覆盖判断

统一 tracker 的可行性来自“协议认证后都尽量投影为 `metadata.User`”，不是只适用于 VLESS。

| inbound 类别 | 当前用户身份 | overlay 判断 |
| --- | --- | --- |
| VLESS + REALITY/Vision | 具名用户认证后写入 `metadata.User` | 首要支持，风险低；REALITY 伪装中继除外（§5.2） |
| VMess、Trojan | 具名用户可传播到 router | 可复用通用 tracker，需集成测试；fallback 路径需单独确认 |
| Shadowsocks multi | 具名用户可传播 | 可统一，但 SSM tracker 在 router 之前包装（`inbound_multi.go:177-181`），会多计 sniff 预读与随后被 reject/block 的字节，**不能与 router tracker 互作 oracle** |
| Shadowsocks relay | `metadata.User` 是 `destinations[].name`（`inbound_relay.go:130`） | 是中继目的地而非终端用户，**不能按用户计费** |
| Hysteria/Hysteria2、TUIC | 协议层有用户概念 | 可行，重点验证 QUIC stream/datagram 分类 |
| Naive、AnyTLS、HTTP/SOCKS/Mixed 认证 | 有认证用户名的路径可传播 | 可行，逐协议确认匿名与 fallback 行为 |
| ShadowTLS | `users[].name` 只用于握手校验 | `InboundDetour` 跳转发生在 tracker 之前（`route/route.go:62-78`，tracker 循环在 `:152`），User 被 detour 的 `UpstreamMetadata` 丢弃；计费身份只能取自 detour 末端的 Shadowsocks 用户（单用户 SS 则为空，统计模式应拒绝）；握手/校验失败流量整条中继到 handshake 服务器，不经 router，声明不支持 |
| TUN、redirect、tproxy、direct 等透明入口 | 没有认证用户 | 不能声称“按用户”；只能按 inbound/device/source 做另一类统计 |

通用规则：**带 `InboundDetour` 的 inbound 一律以 detour 末端 inbound 为计费身份**，因为 detour
跳转在 tracker 之前完成。

统计模式应要求选中的 inbound 对所有允许流量都有可验证的非空 billing identity，且必须按 inbound
类型白名单启用——“`metadata.User` 非空即可计费”不是有效兜底（relay 就是反例）。匿名、空名、
认证 fallback 或 tracker 绕过不能默认为“unknown 后继续转发”；若它们会影响收费，启动或连接必须
失败关闭。透明入口若按源 IP/设备计费，应使用独立 schema 和身份政策，不能伪装成认证用户统计。

## 7. 与现有 sing-box-manager 的接入

`sing-box-manager` 已经具备按 `identity_name` 的上下行 baseline、周期累计、配额评估、
runtime epoch、最终结算屏障和幂等入库框架；当前数据源是 Shadowsocks SSM，所以其 README 明确要求
VLESS relay 用户的 `quotaBytes = 0`（`src/manifest/mod.rs:346-350`）。

### 7.1 版本对齐

Manager 只验证过数据面 sing-box `1.13.14`（`src/manifest/mod.rs:476`、其 README:29/79），
本报告的审计基线是 1.13.19，上游已到 1.14.0。任何自定义构建都会替换 Controller 与全部 Agent 上
已验证的二进制，因此：

- **观测 PoC** 使用与 Manager 同 major 的 `v1.13.21` 加 `with_v2ray_api` 构建，先在 Manager 上重跑
  `check` / `deploy` / 屏障回归，并更新其 README 的已验证版本；
- **正式 overlay** 基于 `v1.14.x` 时，先把 `singboxVersion` 升到该版本，对全部 Manager 生成的
  entry 配置做 `sing-box check` 回归（重点关注 1.14 移除的 legacy DNS server 格式与其他弃用项），
  通过后才允许 Agent 改读新 exporter；
- Controller 本机与每台 Agent 的 `SINGBOX_BIN` 必须是同一构建，可用
  `config_artifacts.target_singbox_version` 记录。

### 7.2 两层接入与 Manager 侧改动清单

**第 1 层：观测 PoC。** 使用固定的 `with_v2ray_api` 构建，VLESS 的 `users[].name` 继续复用 Manager
由 `(user_id, route_id)` 确定性派生的 `identity_name`；Agent 在回环读取累计 uplink/downlink。
这一层无需改变 usage bucket 结构，但 SIGHUP、静态白名单和响应丢失边界仍在，不能解除硬配额限制。

该路线在 `sing-box-manager/_legacy/` 中已经在 1.13.14 上实现过一次并被 SSM 取代
（`_legacy/grpc.rs` 的 StatsService gRPC 只读客户端、`_legacy/backend/reload.rs`、
`_legacy/singbox.rs:205-217` 生成 `experimental.v2ray_api` 白名单），其中已记录 ServiceName 覆写的坑。
PoC 应复用这些代码而非从零重做，前置条件：

1. 复活 `_legacy/grpc.rs` 与 `_legacy/backend/reload.rs` 并在新基线上回归（重点验证 ServiceName
   覆写、静态白名单、SIGHUP 丢数）；
2. `src/compiler/entry.rs` 为 VLESS Entry 输出 `experimental.v2ray_api` 块，`stats.users` 等于身份投影；
3. Controller 与所有 Agent 使用同一 `with_v2ray_api` 构建（否则两侧 `check` 都会拒绝该配置）。

> 待确认：`_legacy` 路线当年被 SSM 取代的原因在 CHANGELOG / ROADMAP / docs 中无记载，
> 复用前需向 Manager 维护者确认，避免重蹈当时的问题。

**第 2 层：正式计费。** Agent 改读 `sing-box-plus` 的 UDS 快照。现有两方向账单可先把 TCP+UDP
各自求和；若产品要展示四方向，再扩展 raw usage schema。

Manager 侧的验收前提**不是现成的**，必须区分已具备与需新写：

| 能力 | 现状 | 需要做什么 |
| --- | --- | --- |
| final 批精确一次 | 已具备（`traffic_batches` PK 去重） | — |
| 屏障 ack 后才停旧进程 | 已具备（`agent_0003_barrier.sql`、`settle.rs`） | — |
| 新 boot id 不产负增量 | 已具备（结构性 include） | — |
| VLESS 进入计量与屏障 | **无**：`metering/tick.rs:40-45` 对非 shadowsocks 直接返回；`manager/deploy.rs:222-232` 对 `vless-reality` 硬置 `barrier_required=false`；`agent/runtime.rs` 的 VLESS 健康检查是进程型空桩 | 取消 inbound_kind 跳过，置 `barrier_required=true`，补健康检查 |
| exporter 客户端 | **无**：Agent 只有 reqwest over TCP 的 SSM 客户端（`agent/ssm.rs`） | 新增 HTTP/1.1-over-UDS 严格客户端，按 §5.4 处置错误码 |
| 未知 runtime 拒绝 | **无**：任何 boot id 都新建基线行 | Agent 在每次 `restart()` 后把首份快照的 `runtime_id` 与本地 epoch 一对一绑定并持久化（新增 agent 迁移列），后续快照 runtime_id 不符即拒绝并告警 |
| unhealthy 拒绝 | **无**：`StatsBatch` 无 health 字段，也不读 `/healthz` | DTO 增字段，unhealthy 快照失败关闭 |
| 快照序号跟踪 | **无**：poll 批 `sequence` 恒 0（`agent/stats.rs:17`），该字段已被屏障占用 | 新增 `exporter_runtime_id` / `exporter_sequence` 字段（**不复用** `StatsBatch.sequence`），按 `(entry, runtime_id)` 记高水位 |
| 累计倒退处理 | **相反**：`store/metering.rs:14-18` 刻意 `max(0, cur-last)`，用于吸收 final/poll 交错的陈旧读（F1 回归） | 改为两层：`exporter_sequence ≤ 高水位` → 丢弃整份快照且不推进基线；序号前进但计数倒退 → 失败关闭并标记 Entry stale |
| 基线键含 generation/runtime | **无**：PK 为 `(entry_id, inbound_tag, identity_name, singbox_boot_id)`，迁移 additive-only，SQLite 不能改 PK | generation 恒为 1 时无需改主键；以 additive 方式加 `exporter_runtime_id`、`generation` 列（默认 1），主键沿用 boot id，由上面的 runtime 绑定保证等价 |
| 排空闸门 | 绑死 SSM：`agent/gate.rs:34-46` 轮询 `tcp_sessions + udp_sessions <= 0` | 改读快照的 `*_sessions` 附加字段；若不提供该字段，必须显式声明“VLESS 不排空、`drain_clean` 恒 false 并记 `unsettled_window`”，禁止把缺失字段退化成 0 造成假性 `drain_clean` |
| 首快照策略 | 结构性 include | 保持 include 并写明理由：Agent 是唯一 spawner，崩溃尾账按未闭合窗口单独审计 |
| VLESS 非零配额校验 | 硬拒绝（`manifest/mod.rs:346-350`） | 改由能力位门控，条件满足后才放开 |

### 7.3 配额执行

超额之后如何把 VLESS 用户踢下线，与 Shadowsocks 完全不同：SS 走资格翻转 → SSM reconcile 热删身份、
不重启（`metering/tick.rs:130-136`）；而 VLESS 用户 UUID 静态编译进 inbound，停用需要
`apply --deploy` 发布新 revision，即每次配额翻转都要走屏障 + 进程重启 + 新 `runtime_id`。

两种方案，首版建议 (b)：

- **(a) 热用户增删**：`sing-box-plus` 另开受控写入端点（参考 SSM `/users` 语义），Manager 复用
  reconcile 路径。底层能力已存在（`sing-vmess vless/service.go:40 UpdateUsers`），但会引入写接口、
  需要单独授权、幂等 command id 与失败关闭，攻击面明显扩大，**不得复用只读快照 socket**。
- **(b) 超额即重新部署**：首版接受这一成本，把配额翻转改为触发 Entry 重编译 + 屏障部署，并给出
  翻转合批与最小间隔策略。周期性配额（月/年）下翻转频率低，可接受。

所选方案计入 §8.2 工作量与里程碑 6，并把“配额执行路径验证通过”加入解除条件。

### 7.4 解除 quotaBytes = 0 的条件

正式方案上线前，保留 `quotaBytes = 0` 的 VLESS 保护。只有以下条件全部通过才解除：

- VLESS Reality/Vision TCP、XUDP/UoT 与长连接重载对账无缺口/重复；
- exporter 失败、Box 重载、进程重启、Agent/Manager 重试的故障注入通过；
- 删除、停用、同名重建和凭据轮换的 lineage 语义通过（按 §5.3 第 3 条的稳定身份语义）；
- Manager 已实现 §7.2 表中“需要做什么”一栏的全部条目，能拒绝未知 runtime、倒退累计、重复
  `exporter_sequence` 和 unhealthy snapshot；
- §7.3 的配额执行路径已验证；
- 最终结算成功后才允许旧实例退出或新部署完成。

长期可让 Shadowsocks 和 VLESS 都走同一个通用 exporter，从而删除 SSM 与 V2Ray API 的双采集逻辑；
但 SSM 与 router tracker 口径不同（§3.4、§6），迁移时必须作为**新 runtime / 新 lineage 切换**并在
文档记录口径差异，不得跨口径差分。首期也可以只让 VLESS 使用新 exporter，保持现有 SSM 路径不变
以缩小迁移面。

## 8. 实施计划

### 8.1 交付物清单

对照 `shadowsocks-rust-plus` 已落地的工程要素，本项目至少需要：

| 交付物 | 说明 |
| --- | --- |
| `upstream.lock` | repository / tag / commit / `prepared_tree_sha256` / commit_date / fetched_at / license / go 最低版本 |
| `scripts/prepare-source.sh` | 按精确 commit 取源码并校验；方案 B 时零 fuzz 重放 `patches/series` |
| `scripts/verify.sh` | `go vet`、`go test -race ./...`、lint、敏感信息扫描（私钥、access key、`PrivateKey`/`Passphrase` 赋值） |
| `scripts/build-linux-release.sh` | 两次独立路径构建逐字节一致才产出 manifest + SHA-256 |
| `scripts/sign-release.sh` / `verify-release.sh` | detached 签名与验签，私钥离线保管 |
| `packaging/` | 复用上游 `release/config/sing-box.service`、`sing-box.sysusers`，追加 `RuntimeDirectory=` 承载 UDS；上游无 tmpfiles 模板，需自建 |
| `docs/` | `API.md`、`ARCHITECTURE.md`、`OPERATIONS.md`、`UPSTREAM_BASELINE.md`、`PERFORMANCE.md` |
| `tests/` | 复用 `shadowsocks-rust-plus/tests/{http_unix,settlement_model,mock_collector}.py` 作契约测试，另加字节 oracle |
| `THIRD_PARTY_NOTICES.md` | 见 §10.3 |
| `.env.example` | `UPSTREAM_REPOSITORY`、`GOMODCACHE`、`SING_BOX_BUILD_TAGS` 等占位（仓库约定要求子项目根目录有 `.env`） |
| `.gitattributes` | `*.patch -whitespace` 等 |

### 8.2 人力投入

按一名熟悉 Go、sing-box 和异步代理数据面的工程师估算，不含灰度等待与法务日历时间。
人周为单人工作量，多人并行只压缩日历时间、不减少人周。

| 阶段 | 内容 | Manager 接入是否必需 | 粗估 |
| --- | --- | --- | --- |
| PoC | 复用 `_legacy` 的 gRPC 客户端与白名单生成，在钉定构建上验证 VLESS 归属 | 必需 | 1–2 人日 |
| 观测接入 | 自定义构建、静态白名单、Agent collector、仪表与基础故障处理 | 必需 | 1–2 人周 |
| 四向 registry | 通用 tracker、饱和计数、稳定 lineage、配置校验 | 必需 | 2–4 人周 |
| UDS exporter | schema、安全加固、资源上限、监督与故障注入 | 必需 | 2–3 人周 |
| 协议与性能验证 | Vision direct 切换、mux、XUDP/UoT、QUIC、UDP batch、bench/pprof | 必需 | 2–4 人周 |
| Agent exporter 客户端 + runtime 绑定 | UDS 客户端、runtime_id↔epoch 绑定与迁移、健康/序号校验 | 必需 | 1–2 人周 |
| Manager schema 与结算规则 | additive 迁移、倒退两层处理、VLESS 进入 tick/屏障 | 必需 | 1–2 人周 |
| VLESS 配额执行 | §7.3 所选方案 | 必需 | 1–2 人周 |
| overlay 骨架 + 可复现发布 + 签名 | §8.1 的脚本与 packaging | 必需 | 1–2 人周 |
| 文档与运维手册 | docs 六件套 | 必需 | 1–2 人周 |
| SIGHUP 跨 Box 存活 | 进程级 registry、drain 阶段、看门狗接管 | **可选**（见 §5.3） | 2–4 人周 |
| 上游 rebase 储备 | 每次 minor 升级 | 必需（周期性） | 1–2 人周/次 |

必需项合计约 **13–24 人周**，另加 15–25% 的复核返工缓冲；含可选的 SIGHUP 存活为 15–28 人周。
仅做“能看每用户上下行”的 PoC 不应被误报成完整功能。

### 8.3 里程碑与完成标准

| # | 里程碑 | 完成标准 / 证据 |
| --- | --- | --- |
| 1 | 冻结基线与骨架 | `upstream.lock` 已记录 tag/commit/`prepared_tree_sha256`；overlay 形态已定（§5.5）；两次独立构建 SHA-256 相同；`sing-box version` 显示钉定版本；`docs/UPSTREAM_BASELINE.md` 落库 |
| 2 | 观测 PoC | 回环 VLESS Reality/Vision 多用户 TCP+UDP 字节 oracle 差 = 0 的报告；复现并记录 ServiceName 覆写与 SIGHUP 丢数；Manager 侧三项前置（§7.2）通过 |
| 3 | 四向 tracker / registry | `go test -race` 全绿；四向 oracle 误差 = 0；Linux amd64 真实 splice 用例覆盖；多 tracker 叠加 unwrap 断言通过 |
| 4 | UDS exporter | 权限/符号链接/inode 替换/超限/慢客户端故障用例通过；exporter 异常退出导致进程失败退出；`tests/http_unix.py` 与 mock collector 直接通过 |
| 5 | 接入 Manager（只记账） | staging 只记账 ≥ 7 天：负增量 = 0、未知 runtime = 0、`exporter_sequence` 重复 = 0、unhealthy 快照全部被拒 |
| 6 | 启用配额 | §9 故障矩阵与性能三组对照报告归档；§7.3 配额执行路径验证通过；§7.4 条件全部满足后才解除 `quotaBytes = 0` |

## 9. 测试与性能门槛

至少需要以下自动化测试（括号为归属里程碑）：

- 每个协议的合法/非法认证、空 user、重复 user、跨 inbound 同名（M3）；
- TCP partial write、half-close、RST、取消、buffered copy、vectorized copy 和 Linux splice（M3）；
- UDP packet/batch、XUDP/UoT、mux、QUIC stream/datagram，不计 framing（M3）；
- Vision early data、padding/unpadding、buffered → direct 切换（**不含 splice**，见 §5.2）（M3）；
- 多 tracker 叠加时 counter unwrap 仍生效（M3）；
- 热删、同名重加、凭据轮换、旧连接继续计数、tombstone 保留（M3）；
- 快照响应中断、collector 重试、重复/乱序/倒退 `exporter_sequence`、累计溢出（M4）；
- UDS 权限、symlink/inode 替换、慢连接、超大请求和并发上限（M4）；
- 进程重启前后长连接、Box 启动失败、连续部署、屏障 ack 丢失与重放、崩溃（M5）；
- `-race`、fuzz、端到端字节 oracle、Linux 真实 splice 和发布目标集成测试（M3/M6）。

性能验收应至少比较未启用、编译但未配置、启用四向统计三组：吞吐、p50/p99 延迟、CPU、分配、
goroutine、内存随用户数/并发数增长，以及 exporter 被慢客户端占满时代理数据面的隔离。目标不是
预设“零开销”，而是给出可复现基线并设置回归阈值。

## 10. 构建与许可

### 10.1 构建矩阵

上游默认 tag 集不含 `with_v2ray_api`；REALITY 服务端受 `with_utls` 门禁
（`common/tls/reality_server.go:1`），缺该 tag 时配置会直接报错——**只用 `with_v2ray_api` 的构建
跑不了里程碑 2 的 Reality PoC**。

| 项 | 值 |
| --- | --- |
| Go 工具链 | 钉具体版本并设 `GOTOOLCHAIN=local`；1.13.19 的 `go.mod` 为 `go 1.24.7`，v1.14.0 升到 `go 1.25.5` |
| CGO | `CGO_ENABLED=0` |
| 可复现参数 | `-trimpath -buildvcs=false -ldflags "-s -w -buildid= $(cat release/LDFLAGS)"`；`release/LDFLAGS` 内容在 1.14 已改写（`-X runtime.godebugDefault=…`），必须读取文件而非硬编码 |
| 生产 tag 集 | `with_utls,badlinkname,tfogo_checklinkname0`（按需加 `with_quic`）+ 自有 `with_user_stats` |
| PoC tag 集 | 生产集再加 `with_v2ray_api` |
| 被裁剪的默认 tag | `with_gvisor`、`with_wireguard`、`with_tailscale`、`with_dhcp`、`with_acme`、`with_ccm`、`with_ocm`、`with_clash_api`、`with_naive_outbound` 等；裁剪清单属待决策项（§11） |

编译验证记录：2026-09-05，darwin/arm64、go1.26.5，在 `v1.14.0`（`0b899587`）上执行

```text
go build -tags with_v2ray_api ./experimental/v2rayapi ./service/ssmapi ./protocol/vless
```

exit = 0。三个包均通过编译，但上游对它们显示 `[no test files]`；这只能证明所审计构建标签和包
可编译，不能替代协议、流量对账或生产测试。

### 10.2 上游锁定与升级规则

1. `upstream.lock` 记录 repository / tag / commit / tree sha256 / commit_date / fetched_at /
   license / go 最低版本，格式参照 `shadowsocks-rust-plus/upstream.lock`；
2. 跟随 **stable 轨道**（当前 1.14.x），只在 patch 内自动跟进；minor 升级视为新基线，需重跑
   §9 的全部对账矩阵；
3. 每月核对上游 tag；安全修复 T+2 工作日完成评估、T+7 产出可复现构建；
4. 补丁不能零 fuzz 应用即失败，不得静默跟随其他版本；
5. 每次升级都要重跑 §9 全部门禁并把结果写入 `docs/UPSTREAM_BASELINE.md`；
6. 所跟随的轨道转为 oldstable 或 EOL 后 30 天内必须迁移；
7. 1.14.x 依赖 `sing`、`sing-tun`、`sing-quic` 的 beta 模块，须用 `go.sum` 或 `go mod vendor` 固定。

当前轨道状态（2026-09-05 核对）：`origin/oldstable` = `v1.13.21-2`，`origin/stable` = `v1.14.0-16`，
`origin/testing` 已进入 `v1.15.0-alpha.1`。1.13.20/21 相对 1.13.19 在本报告审计的文件上零差异
（含 mux/websocket early-data 修复），可作 1.13 线的冻结点；但 1.13 已是 oldstable，按上游历史节奏
很快停更，不建议在其上开发 overlay。

### 10.3 GPL 与命名

sing-box 的 LICENSE 是 GPL v3-or-later 的授权声明段，并附带“衍生作品未经同意不得使用该应用名称
或暗示关联”的额外文字（注意：该文件只有声明段，不含 GPL 正文），见
[稳定版 LICENSE](https://github.com/SagerNet/sing-box/blob/b5ebaa1fc0f2b94256180b95468e73ef53caa27d/LICENSE)。

义务按 GPLv3 的 convey 分档触发：

1. **向本公开仓库提交 overlay 源码或补丁即构成 convey**：本子目录须包含 GPL-3.0-or-later 全文与
   上游附加条款原文、`THIRD_PARTY_NOTICES.md`，补丁头须注明修改内容与日期（GPLv3 §5(a)）。
   这一触发点早于二进制发布。
2. **仅在自有主机部署、不向第三方交付二进制**不产生额外源码义务；GPLv3 没有 AGPL 式的网络使用条款。
3. **向第三方交付二进制**时须随附对应完整源码或书面要约。
4. `sing-box-manager`（MIT）必须保持进程边界，不得 import 任何本项目的 Go 代码，也不分发数据面
   二进制，以维持其许可证独立。

命名：研究 ID `sing-box-plus` 不应直接作为可发布产品名；实际 fork 建议使用中性名称或先取得许可。
改名时需同步确认 Manager 的 `sing-box version` 解析约定（§5.5 硬约束）。法务评审属日历项，
应在里程碑 5 之前启动。

## 11. 待决策

以下选择会改变实现或成本，需要在对应里程碑前拍板：

| # | 事项 | 选项 | 现文默认 | 需在何时决定 |
| --- | --- | --- | --- | --- |
| D1 | 实施基线轨道 | 1.13.21（与 Manager 同 major，但 oldstable 将停更） / 1.14.x（stable，依赖 beta 模块、Go 1.25+） | PoC 用 1.13.21，正式 overlay 用 1.14.x | 里程碑 1 |
| D2 | overlay 形态 | A 零补丁 wrapper / B patch series / A+B | A 为默认 | 里程碑 1 |
| D3 | 快照 schema | 复用 ss-rust-plus v1 形状 / 另立 v2 | 复用 v1 | 里程碑 4 之前，越早越好 |
| D4 | Shadowsocks 是否迁到统一 exporter | 首期只做 VLESS / 一并迁移 | 首期只做 VLESS | 里程碑 5 |
| D5 | VLESS 配额执行 | (a) 热用户增删写接口 / (b) 超额即重新部署 | (b) | 里程碑 6 |
| D6 | 构建 tag 裁剪清单 | 见 §10.1 | 待定 | 里程碑 1 |
| D7 | 是否需要四方向账单展示 | 两方向求和 / 扩展 raw usage schema | 先两方向 | 里程碑 5 |
| D8 | 是否接受“进程崩溃丢尾账” | 接受（同 ss-rust-plus） / 引入 WAL | 接受 | 里程碑 5 |

## 12. 最终 Go / No-Go

- **Go：** 使用内置 V2Ray StatsService 做具名用户的基础观测和 PoC（需自定义构建与 §7.2 的前置改动）。
- **No-Go：** 仅靠当前 V2Ray API、Clash API、1.14 连接事件或 SSM cache 做严格用户账务。
- **Conditional Go：** 维护固定 stable 版本的 hardened overlay，实现四向 registry、非破坏快照、
  与 Manager 现有结算屏障对接的重载语义，以及外部幂等结算；技术路径清晰，协议实现风险低。
- **暂不解除：** `sing-box-manager` 对 VLESS `quotaBytes = 0` 的保护，直到 §7.4 条件全部通过。

总体建议是启动 `sing-box-plus` 原型，但把第一阶段明确标记为“观测验证”，不要把现成两方向
StatsService 当作 `shadowsocks-rust-plus` 等价物。若产品确实需要多协议统一计费，sing-box 是比
`v2ray-rust` 更合适的长期基线。

## 13. 变更记录

- **2026-09-05**：按多维度复核结果修订。更新基线元数据（1.14.0 已成为 stable，1.13 降为 oldstable，
  原 testing 引用作废）；纠正 §5.3 重载模型（上游先关旧 Box、强制关闭全部连接，无排空语义；
  Manager 从不发 SIGHUP，屏障为主路径、进程级 registry 改为可选）；lineage 语义改为与
  `shadowsocks-rust-plus` 一致；快照改用 v1 形状并补 HTTP 子集、错误码与会话数字段；
  新增术语对照、overlay 形态决策、Manager 改动清单、配额执行、交付物清单、里程碑完成标准、
  构建矩阵、上游锁定与升级规则、待决策；修正 ShadowTLS / SS relay / SS multi 的协议判断与
  Vision 不走 splice 的结论；校准 GPL 义务触发点与工期口径。
- **2026-08-27**：初稿，基于 `v1.13.19` 的可行性与源码审计。
