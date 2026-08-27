# sing-box-plus 多用户流量统计调研

> 调研日期：2026-08-27
>
> 上游仓库：[`SagerNet/sing-box`](https://github.com/SagerNet/sing-box)
>
> 稳定版审计基线：[`v1.13.19`](https://github.com/SagerNet/sing-box/releases/tag/v1.13.19) /
> `b5ebaa1fc0f2b94256180b95468e73ef53caa27d`（2026-08-17）
>
> 前沿功能对照：`testing` / `77bd3261367463b970648edf445c88ac541b3bf2`
>
> 当前状态：可行性与源码审计；本目录尚未包含 sing-box overlay 实现

本报告评估：能否像本仓库已经实现的 `shadowsocks-rust-plus` 一样，给 sing-box 增加可用于
多用户配额和结算的流量统计。这里的“像”不是只显示实时速率，而是要求认证后的稳定用户归属、
TCP/UDP × 上下行四个累计值、重载边界、可幂等采集以及受控的本机快照接口。

## 结论

| 目标 | 结论 |
| --- | --- |
| 查看具名用户的上下行累计流量 | **现在就能做**；启用内置 V2Ray StatsService 即可 |
| VLESS + REALITY + XTLS Vision 用户是否能归属 | **能**；VLESS 认证完成后会把非空 `users[].name` 传播为 `metadata.User` |
| 内置接口能否区分 TCP/UDP 四方向 | **不能**；TCP、UDP、XUDP 最终合并为 uplink/downlink 两项 |
| 内置接口能否直接做严格账务 | **不能**；缺少运行周期、代次、稳定快照和持久化，重载会丢未采集增量 |
| 能否开发成 `shadowsocks-rust-plus` 同等级能力 | **能，且可行性高**；协议、认证身份、统一 tracker 和 splice 计数路径都已存在 |
| 是否需要改每一种代理协议 | **通常不需要**；应扩展认证后的通用 `ConnectionTracker`，再验证特殊数据路径 |
| 推荐路线 | 先用内置统计做观测；需要配额/结算时维护一个小型、固定版本的 hardened overlay |

与 `v2ray-rust-plus` 的结论不同：sing-box 不缺 VLESS、REALITY、Vision，也不缺用户身份传播。
主要工作集中在**计数维度、生命周期和可靠导出**，因此适合作为生产扩展基线。

## 1. sing-box 现在已经有什么

### 1.1 Experimental V2Ray StatsService

sing-box 已实现与 V2Ray StatsService 兼容的 gRPC 服务。配置中可以列出需要统计的 inbound、
outbound 和 user；router 会为命中的 TCP 与 packet connection 包装原子计数器。官方文档也明确
给出了 `stats.users` 字段：见
[V2Ray API 配置文档](https://sing-box.sagernet.org/configuration/experimental/v2ray-api/)和
[稳定版实现](https://github.com/SagerNet/sing-box/blob/b5ebaa1fc0f2b94256180b95468e73ef53caa27d/experimental/v2rayapi/stats.go)。

它默认不在构建产物中，部署构建必须把 `with_v2ray_api` 加到既有 build tags；缺少该 tag 时配置
会明确报错。见[官方构建标记文档](https://sing-box.sagernet.org/installation/build-from-source/)和
[stub 实现](https://github.com/SagerNet/sing-box/blob/b5ebaa1fc0f2b94256180b95468e73ef53caa27d/include/v2rayapi_stub.go#L1-L16)。

脱敏后的最小结构如下；`identity_name` 必须同时出现在 inbound 用户和统计白名单中：

```json
{
  "inbounds": [
    {
      "type": "vless",
      "tag": "vless-entry-01",
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
- counter 在首次产生流量时才懒创建，零流量用户可能返回 not found；
- 只能使用 `reset=false` 的累计查询做外部差分，不能把破坏性 reset 当作结算协议；
- gRPC listener 使用 insecure credentials，没有内置认证或 TLS，只能绑定回环或置于独立的受控代理后。

因此，内置能力适合静态用户集的监控、用量展示和方案验证，不足以单独承担收费或硬配额。

### 1.2 VLESS、REALITY 和 Vision 的用户身份链路

VLESS 服务先解析并验证 UUID、flow 和 command，之后 sing-box handler 从认证 context 取出用户
索引，把非空 `users[].name` 写入 `metadata.User`，再交给 router。见
[VLESS 服务认证](https://github.com/SagerNet/sing-vmess/blob/3aed155119a1/vless/service.go#L55-L96)和
[VLESS inbound 用户传播](https://github.com/SagerNet/sing-box/blob/b5ebaa1fc0f2b94256180b95468e73ef53caa27d/protocol/vless/inbound.go#L167-L205)。

REALITY 握手和 Vision 解码并不会抹掉这个身份。tracker 在认证和协议解码之后、路由选出 outbound
之后附加，所以正常通用转发路径统计的是代理应用 payload：不包含 REALITY/TLS 外层、VLESS header
和 Vision padding；被代理的用户内层 TLS record 属于应用 payload，会计入。官方
[VLESS inbound 文档](https://sing-box.sagernet.org/configuration/inbound/vless/)确认每个用户都有
`name`、`uuid`、`flow` 字段，`xtls-rprx-vision` 已是现有能力。

错误 UUID、flow 不匹配和未进入 router 的连接不会形成 user counter。认证身份与路由层也已经
通过 `auth_user` 统一使用，见[路由规则文档](https://sing-box.sagernet.org/configuration/route/rule/)。

### 1.3 其他已有接口为什么不能替代账本

| 接口 | 用户维度 | TCP/UDP 字节 | 生命周期/持久化 | 判断 |
| --- | --- | --- | --- | --- |
| V2Ray StatsService | 有，静态白名单 | 合并 | 无 | 可观测，不可直接结算 |
| Clash API | 无用户累计；connection JSON 不导出 user | 仅连接 network | 关闭详情有界 | 不能可靠反推 |
| 1.14 新 API service | 每连接有 user/network | 可从事件区分 | 事件可丢、历史有界 | UI/诊断用途，不是账本 |
| common trafficcontrol | 每连接内部有 user | 每连接可辨别 | 较旧关闭连接只进全局累计 | 不能恢复完整用户累计 |
| SSM API | 有，但仅 managed Shadowsocks | 字节合并 | 可选 JSON cache | 不能覆盖 VLESS/通用协议 |
| Prometheus `/metrics` | 不存在通用用户流量 exporter | — | — | 不能依赖 |

1.14 的连接事件虽然比 Clash API 多导出了 user 和 network，但没有按用户累计、runtime ID、sequence
或可靠重放；事件总线拥塞和关闭历史淘汰都会造成不可恢复缺口。它适合界面展示，不适合账务。

### 1.4 SSM API 只能作为 Shadowsocks 的参考

SSM service 自 1.12 起可管理 Shadowsocks inbound，并记录每用户 uplink/downlink bytes、packet
和 TCP/UDP session；官方范围明确限定为 Shadowsocks，见
[SSM API 文档](https://sing-box.sagernet.org/configuration/service/ssm-api/)。字节仍把 TCP/UDP 合并，
而且不能附加到 VLESS inbound。

它的 `cache_path` 每分钟及正常关闭时保存统计和用户，但不应被当作严格账本：

- 崩溃可能丢最近一分钟，cache 错误不会让代理失败关闭；
- `clear=true` 与 V2Ray `reset=true` 一样有“已清零但响应丢失”的永久漏账窗口；
- 删除用户会移除对应 counter map，没有 tombstone 或 generation；
- cache 同时保存 Shadowsocks 用户凭据，稳定版按 `0644` 创建文件，路径必须严格保护；
- 直接覆盖文件而非事务提交，统计和用户更新也缺少账务级的一致性契约。

因此不能把 SSM cache 抽象成“sing-box 已经有可靠持久化用户统计”；它可以为 overlay 提供 tracker
与 API 设计参考，但不应直接复用其存储语义。

## 2. 与 shadowsocks-rust-plus 的差距

`shadowsocks-rust-plus` 的可结算契约不仅是四个 counter。sing-box 内置实现还缺少：

| 能力 | sing-box 内置现状 | `sing-box-plus` 应达到 |
| --- | --- | --- |
| 用户身份 | 已认证的 `metadata.User`，但可为空且内置 key 全局合并 | 非空稳定 billing name，并带 inbound 与代次 |
| 方向 | uplink/downlink | TCP/UDP × uplink/downlink 四项 |
| 数值 | `atomic.Int64`，可回绕为负 | 饱和 `u64`，overflow 进入 unhealthy |
| 查询 | map 遍历，破坏性 reset 可选 | 非破坏性、稳定排序的累计快照 |
| 运行周期 | 无 | `runtime_id`、启动时间、严格递增 `sequence` |
| 用户生命周期 | 删除即消失或重载重建 | `active`、generation、tombstone、旧连接排空 |
| 重载 | SIGHUP 关闭旧 Box 并创建新 Box，未采集量丢失 | registry/exporter 跨 Box 重载存活，或完成最终结算屏障 |
| 接口安全 | 无认证的 TCP gRPC | 本机 UDS、权限与资源上限、受监督 |
| 账务 | 无幂等协议 | 外部 collector 按完整基线键差分与幂等落库 |

内置 StatsService 的 `GetStats/QueryStats(reset=true)` 使用 `Swap(0)`；请求已经清零而响应丢失时，
这段流量无法恢复。多个 counter 也不是事务快照，map 顺序不稳定。见
[查询实现](https://github.com/SagerNet/sing-box/blob/b5ebaa1fc0f2b94256180b95468e73ef53caa27d/experimental/v2rayapi/stats.go#L121-L218)。

更重要的是，命令行收到 SIGHUP 后会关闭当前实例，再从配置创建新的 Box；StatsService 属于 Box，
没有跨实例状态交接。见
[重载循环](https://github.com/SagerNet/sing-box/blob/b5ebaa1fc0f2b94256180b95468e73ef53caa27d/cmd/sing-box/cmd_run.go#L171-L200)和
[Box 关闭路径](https://github.com/SagerNet/sing-box/blob/b5ebaa1fc0f2b94256180b95468e73ef53caa27d/box.go#L496-L535)。
上游 issue #4059 报告的正是这一丢数边界；该请求目前为 closed/not planned，见
[`SIGHUP will loss v2ray api traffic stats`](https://github.com/SagerNet/sing-box/issues/4059)。

## 3. 推荐的 sing-box-plus 架构

### 3.1 在统一 tracker 扩展，不逐协议插桩

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

建议的逻辑 key 和记录：

```go
type IdentityKey struct {
    InboundID        string
    InboundGeneration uint64
    BillingName      string
    UserGeneration   uint64
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

### 3.2 计数口径

建议沿用 `shadowsocks-rust-plus` 的应用 payload 口径：

- TCP uplink：协议解码后的用户 payload 被通用转发边界成功写入目标侧的字节；
- TCP downlink：目标侧 payload 被代理编码 writer 接受、准备回传给用户的逻辑字节；
- UDP uplink/downlink：完整逻辑数据报成功交给另一侧后累计 payload 长度；
- 不统计外层 REALITY/TLS、VLESS/VMess/Trojan/SS header、Vision padding、mux/XUDP/UoT framing、
  TCP/IP header 或重传；
- XUDP、UDP-over-TCP 和 mux 内的逻辑 UDP 仍归入 UDP，而不是因为底层 carrier 是 TCP 就归入 TCP。

sing 的标准 copy 会解包 counter wrapper，并在目标写成功后调用双方计数 callback；Linux splice
路径也显式按成功传输字节调用 counter。这比在 source `Read` 成功时提前计数可靠，可以直接复用：

- [TCP counter wrapper](https://github.com/SagerNet/sing/blob/7c349dacf402256d3a7029746073b05d2ead584a/common/bufio/counter_conn.go)
- [packet counter wrapper](https://github.com/SagerNet/sing/blob/7c349dacf402256d3a7029746073b05d2ead584a/common/bufio/counter_packet_conn.go)
- [通用 copy](https://github.com/SagerNet/sing/blob/7c349dacf402256d3a7029746073b05d2ead584a/common/bufio/copy.go)
- [Linux splice](https://github.com/SagerNet/sing/blob/7c349dacf402256d3a7029746073b05d2ead584a/common/bufio/splice_linux.go)

不过这只是核心路径结论。实现前仍需对以下特殊路径做字节对账矩阵：Vision buffered/raw/splice
切换和 early data、mux、XUDP/UoT、DNS hijack、特殊 outbound 直接读写、UDP batch、连接取消与
half-close、重载期间仍存活的长连接。任何绕过标准 router tracker 的 handler 都必须明确选择
“计数、拒绝或声明不支持”，不能静默转发但漏计。

### 3.3 进程级 registry 解决 SIGHUP

推荐把 registry 和 exporter 的生命周期提升到 sing-box 命令进程，而不是挂在每次创建的 Box 上：

1. 进程启动生成新的 `runtime_id`，创建唯一 registry/exporter；
2. 每次加载 Box 时把同一 registry 注入新的 UserStatsTracker；
3. 配置应用产生新的 inbound/user generation；旧记录标记 inactive，但仍保留在本 runtime 快照；
4. 已认证的旧连接持有旧 generation 的 record，允许按策略排空并继续计数；
5. 新 Box 就绪后才停止向旧 Box 分配连接；SIGHUP 不重置累计值；
6. 进程真正重启才产生新 `runtime_id`，由外部 collector 明确选择新周期首快照策略。

如果上游结构不便于跨 Box 注入，次选方案是重载前停止接入、排空连接、取得最终快照并等待控制面
持久化确认，再关闭旧 Box；但该方案会延长重载并复杂化失败恢复。进程级 registry 更符合 sing-box
现有 SIGHUP 循环，也能让快照接口在切换期间保持可用。

纯内存 registry 仍无法恢复进程崩溃前尚未采集的尾账，这与 `shadowsocks-rust-plus` 相同。若业务
要求“内核/进程崩溃也不丢一个字节”，需要另行加入 WAL 或持久计量数据面，复杂度会明显上升；
不能把高频轮询描述成严格保证。

### 3.4 快照与安全契约

建议复用已验证的只读 HTTP/1.1-over-Unix-stream JSON 形状，而不是扩展无认证 TCP gRPC：

```json
{
  "schema_version": 1,
  "node_id": "node-example-01",
  "runtime_id": "<随机运行周期 ID>",
  "started_at_unix_ms": 0,
  "sequence": 42,
  "health": { "ok": true, "overflow": false },
  "users": [
    {
      "inbound_id": "vless-entry-01",
      "inbound_generation": 1,
      "billing_name": "u_example_01",
      "user_generation": 1,
      "active": true,
      "tcp_uplink_bytes": 0,
      "tcp_downlink_bytes": 0,
      "udp_uplink_bytes": 0,
      "udp_downlink_bytes": 0
    }
  ]
}
```

接口必须是非破坏性累计快照，并具备稳定排序、请求/响应大小、身份数、并发数、读写超时等上限；
socket 默认 `0600`，检查父目录、符号链接、旧 socket 和 inode 替换。exporter 启动失败应阻止统计
模式启动；运行中异常退出应由主服务监督并触发整体失败或重启，不能继续转发但停止计量。

外部 collector 用以下完整维度保存 baseline：

```text
node_id + inbound_id + inbound_generation + billing_name
        + user_generation + runtime_id
```

再结合 snapshot sequence、采集批次 ID 和四项 delta 幂等入库。累计值倒退、sequence 倒退、未知
runtime、overflow 或 unhealthy 快照都应失败关闭，不能猜测并继续收费。

## 4. 协议覆盖判断

统一 tracker 的可行性来自“协议认证后都尽量投影为 `metadata.User`”，不是只适用于 VLESS。

| inbound 类别 | 当前用户身份 | overlay 判断 |
| --- | --- | --- |
| VLESS + REALITY/Vision | 具名用户认证后写入 `metadata.User` | 首要支持，风险低 |
| VMess、Trojan | 具名用户可传播到 router | 可复用通用 tracker，需集成测试 |
| Shadowsocks multi/relay | 具名用户可传播；另有 SSM tracker | 可统一，但要避免双计数 |
| Hysteria/Hysteria2、TUIC | 协议层有用户概念 | 可行，重点验证 QUIC stream/datagram 分类 |
| Naive、AnyTLS、ShadowTLS、HTTP/SOCKS/Mixed 认证 | 有认证用户名的路径可传播 | 可行，逐协议确认匿名与 fallback 行为 |
| TUN、redirect、tproxy、direct 等透明入口 | 没有认证用户 | 不能声称“按用户”；只能按 inbound/device/source 做另一类统计 |

统计模式应要求选中的 inbound 对所有允许流量都有可验证的非空 billing identity。匿名、空名、
认证 fallback 或 tracker 绕过不能默认为“unknown 后继续转发”；若它们会影响收费，启动或连接必须
失败关闭。透明入口若按源 IP/设备计费，应使用独立 schema 和身份政策，不能伪装成认证用户统计。

## 5. 与现有 sing-box-manager 的接入

本仓库 `sing-box-manager` 已经具备按 `identity_name` 的上下行 baseline、周期累计、配额评估、
runtime epoch、最终结算屏障和幂等入库框架；当前数据源是 Shadowsocks SSM，所以 README 明确要求
VLESS relay 用户的 `quotaBytes = 0`。

接入可分两层：

1. **观测 PoC**：使用固定 `with_v2ray_api` 构建，VLESS 的 `users[].name` 继续复用 Manager 由
   `(user_id, route_id)` 确定性派生的 `identity_name`；Agent 在回环读取累计 uplink/downlink。
   这一层无需改变 usage bucket 结构，但 SIGHUP、静态白名单和响应丢失边界仍在，不能解除硬配额限制。
2. **正式计费**：Agent 改读 `sing-box-plus` UDS 快照，把 `runtime_id`、inbound/user generation、
   sequence 和四项累计值传给 Manager。现有两方向账单可先把 TCP+UDP 各自求和；若产品要展示四方向，
   再扩展 raw usage schema。部署/重载必须进入最终快照与持久化屏障。

正式方案上线前，建议保留 `quotaBytes = 0` 的 VLESS 保护。只有以下条件全部通过才解除：

- VLESS Reality/Vision TCP、XUDP/UoT 与长连接重载对账无缺口/重复；
- exporter 失败、Box 重载、进程重启、Agent/Manager 重试的故障注入通过；
- 删除、停用、同名重建和凭据轮换的 generation 语义通过；
- Manager 能拒绝未知 runtime、倒退累计、重复 sequence 和 unhealthy snapshot；
- 最终结算成功后才允许旧实例退出或新部署完成。

长期可让 Shadowsocks 和 VLESS 都走同一个通用 exporter，从而删除 SSM 与 V2Ray API 的双采集逻辑；
但首期也可以只让 VLESS 使用新 exporter，保持现有 SSM 路径不变以缩小迁移面。

## 6. 实施计划与粗略工期

以下按一名熟悉 Go、sing-box 和异步代理数据面的工程师估算，不含生产灰度等待时间：

| 阶段 | 内容 | 粗估 |
| --- | --- | --- |
| PoC | 固定上游、启用 V2Ray API、VLESS 用户归属与累计采集 | 2–5 人日 |
| 观测接入 | 自定义构建、静态白名单、Agent collector、仪表与基础故障处理 | 1–2 人周 |
| 四向 registry | 通用 tracker、饱和计数、身份/代次/tombstone、配置校验 | 2–4 人周 |
| 重载生命周期 | process-scope registry、旧连接排空、runtime/sequence 契约 | 2–4 人周 |
| UDS exporter | schema、安全加固、资源上限、监督与故障注入 | 2–3 人周 |
| 协议与性能验证 | Vision/splice、mux、XUDP/UoT、QUIC、UDP batch、bench/pprof | 2–4 人周 |
| Manager/发布 | baseline 迁移、最终结算、可重放 overlay、构建与回滚 | 2–4 人周 |

部分阶段可并行。一个达到 `shadowsocks-rust-plus` 同等级可靠性的首版，合理量级约 **10–18 人周**；
仅做“能看每用户上下行”的 PoC 不应被误报成完整功能。

建议的里程碑：

1. 冻结稳定版提交，做无改动与启用统计两组构建；
2. 用现有 V2Ray API 完成 VLESS Reality/Vision 静态用户 PoC；
3. 实现四向 tracker/registry，并用字节 oracle 覆盖 TCP、UDP、splice；
4. 实现进程级 reload 和 UDS 快照；
5. 接入 Manager，但先只记账、不执行配额；
6. 故障注入、压力与性能门槛全部通过后再启用配额。

## 7. 测试与性能门槛

至少需要以下自动化测试：

- 每个协议的合法/非法认证、空 user、重复 user、跨 inbound 同名；
- TCP partial write、half-close、RST、取消、buffered copy、vectorized copy 和 Linux splice；
- UDP packet/batch、XUDP/UoT、mux、QUIC stream/datagram，不计 framing；
- Vision early data、padding/unpadding、buffered → raw/splice 切换；
- 热删、同名重加、凭据轮换、旧连接排空、tombstone 保留；
- SIGHUP 前后长连接、Box 启动失败、连续 reload、进程重启和崩溃；
- 快照响应中断、collector 重试、重复/乱序/倒退 sequence、累计溢出；
- UDS 权限、symlink/inode 替换、慢连接、超大请求和并发上限；
- `-race`、fuzz、端到端字节 oracle、Linux 真实 splice 和发布目标集成测试。

性能验收应至少比较未启用、编译但未配置、启用四向统计三组：吞吐、p50/p99 延迟、CPU、分配、
goroutine、内存随用户数/并发数增长，以及 exporter 被慢客户端占满时代理数据面的隔离。目标不是
预设“零开销”，而是给出可复现基线并设置回归阈值。

## 8. 构建验证与许可证

本次在稳定版精确提交上执行了以下定向编译检查：

```text
go test -tags with_v2ray_api \
  ./experimental/v2rayapi ./service/ssmapi ./protocol/vless
```

三个包均通过编译，但上游显示 `[no test files]`；这只能证明所审计构建标签和包在当前环境可编译，
不能替代协议、流量对账或生产测试。

sing-box 的 LICENSE 包含 GPL v3-or-later 授权正文，并附带“衍生作品未经同意不得使用该应用名称
或暗示关联”的额外文字，不能把它不加说明地视为原样的标准 SPDX 许可证。见
[稳定版 LICENSE](https://github.com/SagerNet/sing-box/blob/b5ebaa1fc0f2b94256180b95468e73ef53caa27d/LICENSE)。
发布修改版二进制前必须完成 GPL 源码/归属义务与附加命名条款的法律审查；本目录的研究 ID
`sing-box-plus` 不应直接视为可发布产品名，实际 fork 建议使用中性名称或先取得许可。

## 9. 最终 Go / No-Go

- **Go：** 使用内置 V2Ray StatsService 做具名用户的基础观测和 PoC。
- **No-Go：** 仅靠当前 V2Ray API、Clash API、新连接事件或 SSM cache 做严格用户账务。
- **Conditional Go：** 维护固定稳定版的 hardened overlay，实现四向 registry、跨 Box reload、
  非破坏快照和外部幂等结算；技术路径清晰，协议实现风险低。
- **暂不解除：** `sing-box-manager` 对 VLESS `quotaBytes = 0` 的保护，直到正式 overlay 与故障矩阵通过。

总体建议是启动 `sing-box-plus` 原型，但把第一阶段明确标记为“观测验证”，不要把现成两方向
StatsService 当作 `shadowsocks-rust-plus` 等价物。若产品确实需要多协议统一计费，sing-box 是比
`v2ray-rust` 更合适的长期基线。
