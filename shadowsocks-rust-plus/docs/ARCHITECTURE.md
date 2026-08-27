# 架构决策与代码路径

## ADR-0001：独立 HTTP/1.1-over-Unix-stream 导出接口

状态：已接受。

用户统计不扩展现有 manager 协议。`shadowsocks-rust v1.24.0` 的 manager 使用 UDP
或 Unix datagram，单个响应受 65,536 字节数据报限制；Unix manager 客户端也按“请求不返回
响应”设计。manager 的 `list` 还具有增删实例和读取配置凭据的权限，不适合与只读结算权限
合并。固定上游 manager 的动态 `add` 还会绕过静态 `servers[]` 的统计完整性检查与 registry
接线，而原协议没有足以表达稳定计费身份的字段。因此本项目选择失败关闭：顶层 `user_stats` 与
`ConfigType::Manager` 互斥，builtin 和 standalone manager 都会在配置校验时被拒绝，并在 manager
运行入口、绑定 socket 之前再次校验。未启用统计时的 manager 协议与行为不变。

本项目增加独立的 HTTP/1.1 server-side handler，并只在 Unix stream socket 上运行。实现复用
从上游 local-http 抽到共享模块的 `TokioIo`，以 Hyper 的
`server::conn::http1::Builder + service_fn` 处理单连接；交给 Hyper 前使用已有的 `httparse` 做
有界 preflight，以限制完整请求元数据并让可安全识别的畸形请求获得固定 JSON 错误。它不复用
`sslocal` 的 HTTP 正向代理 `HttpService`、目标解析、DNS、`PingBalancer` 或监听端口，因而不会
让统计采集器同时获得正向代理能力，也不会把 local 客户端依赖带入 `ssserver`。

每条连接只处理一个请求并以 `Connection: close` 结束。handler 只接受 origin-form 的
`GET /v1/snapshot` 与 `GET /healthz`；query、任何 body framing、绝对形式 URI、`CONNECT`、
其他方法和路径均严格拒绝。preflight 已读到的尾随字节也会被拒绝；响应开始后迟到且没有 HTTP
body framing 的字节只会随连接关闭被丢弃，永远不会成为第二个请求。`/healthz` 只读取 registry
健康状态，不推进快照 `sequence`。除 HTTP 标准规定的 HEAD 空 wire body 外，完整响应带
`Content-Length`、`Cache-Control: no-store`，body 是以 LF 结尾的版本化 JSON。socket 默认
`0600`，在 bind 前临时应用 `umask(0077)`，避免先以宽松权限出现再 `chmod` 的竞态，同时不
破坏并发创建目录所需的属主执行位。接口不支持 TCP 监听。

远程访问不是 `ssserver` 的职责。节点需要远程采集时，必须由独立反向代理把本机 Unix socket
作为 HTTP upstream，并在代理边界实现 HTTPS、mTLS、来源限制与审计；不得让 exporter 自身
监听公网地址。反向代理故障不会改变 exporter 的本机协议和数据面权限模型。

## ADR-0002：累计原子计数与外部结算

状态：已接受。

数据面仅保留当前进程生命周期内的四向 `u64` 累计值。每次增加使用饱和原子更新；达到
`u64::MAX` 后保持上限并把 `health.counter_overflow` 置为 `true`。`ssserver` 不写账单、
不持久化累计值，也不猜测新运行周期的首次快照策略。

快照包含随机 `runtime_id`、`started_at_unix_ms` 和单调 `sequence`。外部采集器以
`node_id + server_id + server_generation + identity_name + identity_generation + runtime_id`
为基准键计算差值，以唯一批次 ID 实现幂等。

当前 exporter 把同一 runtime 中的 `server_id` 和用户 `name` 作为稳定逻辑身份，同名重激活会复用
`generation=1` 和原累计计数器。完整 generation 仍保留在键中，作为 v1 schema 的 lineage 维度和未来
兼容边界；控制面不得假设它们永远为 1 而从持久化键中删除。

快照并非跨所有原子的事务性视图：每个字段保证单调，但并发转发时不同字段的读取时刻可能
略有差异。构造快照时只短暂复制拓扑和计数器句柄，排序与 JSON 写入均不持有数据面锁。

## ADR-0003：认证身份与计数边界

状态：已接受。

启用统计时采用失败关闭：所有 `servers[]` 都必须使用支持 EIH 的 AEAD-2022 AES method
（`2022-blake3-aes-128-gcm` 或 `2022-blake3-aes-256-gcm`），并至少配置一个合法命名的
`users[]`。因此数据面只接受可归属到具体 EIH 用户的流量，不为主身份或不支持 EIH 的服务
生成看似健康但归属不完整的快照。底层兼容两种 AES key size；当前五节点部署 profile 固定选择
AES-128-GCM，并使用 16 字节随机 iPSK/uPSK 的标准 Base64，跨节点一致性由部署前工具校验。

### TCP 身份路径

1. `users[].name/password` 被解析为 `ServerUserManager`。
2. AEAD-2022 AES EIH 解出用户身份哈希并选择 `Arc<ServerUser>`。
3. AEAD header、时间戳、重放保护、目标地址和 padding 全部验证成功。
4. `ProxyServerStream::authenticated_user()` 才向 service 层暴露用户。
5. service 在连接建立时按用户名取得一次计数器句柄。

不能在“EIH 哈希命中”时计数，因为后续 AEAD 认证和协议解析仍可能失败。

### UDP 身份路径

UDP 解密结果已经在 `UdpSocketControlData.user` 中携带认证后的 `Arc<ServerUser>`。
启用统计时，AEAD-2022 NAT 关联键使用“客户端 session ID + EIH 身份”，避免不同用户
偶然或恶意复用 session ID 后覆盖下行加密用户或串账。关联在创建时固定用户和计数器句柄；
合法身份及 packet window 校验通过后才允许 NAT rebinding。若认证用户或活动计数器句柄缺失，
首包会在创建关联、排队和目标 socket 写入前以 `PermissionDenied` 失败；关联内的防御性检查也会
在任何计数器不变量失效时丢弃上下行数据报，不能用可选计数形成未入账转发。

统计关闭时不创建 registry/exporter，数据面保留原有快路径。

### 唯一计数点

| 字段 | 唯一计数点 | 失败语义 |
| --- | --- | --- |
| TCP 上行 | 解密数据写入目标 stream 的 `poll_write -> Ok(n)` | `Pending`、`Err`、`Ok(0)` 不增加；此前成功的局部写保留 |
| TCP 下行 | 目标数据写入加密 proxy stream 的 `poll_write -> Ok(n)` | 未形成完整可认证加密 record 时外层返回错误，不增加 |
| UDP 上行 | 明文数据报完整写入目标 socket | ACL、DNS、建 socket、short write 或 send 失败均不增加 |
| UDP 下行 | 目标数据报完整加密并写回客户端 socket | packet ID 溢出、加密、short write 或 send 失败均不增加 |

目标地址、padding、EIH、加密 tag、传输层/IP 层开销和重传不在计数内。

## ADR-0004：服务与身份生命周期

状态：已接受。

- 启用统计后要求每个 `servers[]` 项显式提供唯一 `id`。
- `user_stats.node_id`、`servers[].id` 和 `users[].name` 必须非空、不超过 128 字节，且全部为 ASCII
  可显示非空白字符；一个服务实例内用户名必须唯一。
- registry 保留已注册计数器的强引用；UDP NAT 到期和 TCP 会话关闭不会删除累计值。
- 统计项包含 `generation` 与 `active`。删除会将记录标记为 inactive 但不会从快照消失；同名重加
  复用 `generation=1` 和原计数器并重新标记为 active。未在新服务生命周期中显式注册的旧用户保持
  inactive，其累计值仍导出。
- 导出的 generation 与内部 lifecycle token 分离。每次服务重激活都换用新 token；持有旧 token 的过期句柄
  不能注册用户、取得新的活动计数器、停用用户或停用当前服务。查询和停用按 `state -> users`
  锁顺序线性化；已建立会话此前取得的计数器不会被强制撤销，排空流量继续归入同一稳定计费身份。
- 稳定名称不得在同一 runtime 内重分配给不同计费用户；改变归属必须使用新名称，或在执行最终
  快照屏障后重启为新 runtime。
- registry 对逻辑用户名和逻辑 server ID 分别使用数值相同、互相独立的上限；反复重激活不消耗
  新名额，新逻辑名称达上限时失败并记录错误。
- 固定上游 manager 的原命令和数据报协议只在统计关闭时保留；统计启用时 manager 模式整体被拒绝，
  不把 exporter 权限并入 manager，也不允许动态 `add` 绕过统计接线。

## ADR-0005：共享审计 producer 与 AuditSupervisor

状态：已接受。

`user-audit` 是 Linux-only、非默认 Cargo feature，且显式依赖 `user-stats` 的 EIH 身份路径。
配置层在 feature-off 时仍反序列化 `user_audit`，遇到该字段明确返回 unsupported-feature；只有
feature-on 且配置通过范围、路径和静态 server 校验后，才创建审计元数据。审计配置错误阻止启动，
auditd 运行故障只使 producer 进入 degraded/retry，不得影响 relay。

进程内只创建一个 `Arc<AuditEmitter>` 和一个 `AuditSupervisor`。`AuditEmitter` 统一拥有全进程
`AtomicU64` audit sequence、`ArrayQueue<EventDraft>`、`Notify`、诊断 accumulator、固定 64-shard
UDP cooldown cache 以及 close/active-observation 状态；所有静态 server 通过 `ServiceContext`
clone 同一 emitter。server、TCP tracker 和 UDP association 不得各自创建 queue、sequence、cache 或
supervisor，避免第二个 producer hello 被 auditd 拒绝。

registry 注册路径一次构造不可变的 `AuditRuntimeMetadata`、`AuditServerMetadata`、
`AuditIdentityMetadata` 和计数器组成的 `AuditIdentityHandle`。relay 只保存/clone typed `Arc`，不在
hot path 查询 snapshot、重新读配置或从 transport peer 推断身份。TCP tracker 在双向应用载荷首次
同时成功时取得 observation guard；UDP 在完整 outbound send 成功后取得 guard，guard 覆盖 cooldown
判断、sequence/draft、诊断更新和 `force_push()`，任何关闭竞态都只能漏记，不能阻断转发。

relay hook 只允许立即返回的原子操作、一次 `try_lock`、有界 draft 构造和 `try_emit`。queue 满时
淘汰最老未发送 access event 并聚合 `producer_gap/queue_overflow`；序列化、auditd 连接、ACK、
fsync、重连和 drain 全部位于后台 supervisor。`force_push()` 返回的旧 draft、永久 NACK 的 event
以及诊断快照分别按既定 bucket 处理，禁止递归产生 gap。单一 AuditClient session 失败只在同一
supervisor 内指数退避重建，并保留原始 in-flight bytes；supervisor 不加入数据面 fatal child 集合。

关闭顺序固定为：停止新数据连接、`close_emitter()`、最多 2 秒等待 active guards/queue/in-flight
排空、终止并 join relay、原子读取最终 shutdown-skipped counter、再停止 supervisor。审计 task
异常不会单独重启数据面；生产 `panic=abort` 下审计路径必须 panic-free。

## ADR-0006：协议、spool 与导出耐久性

状态：已接受。

`crates/shadowsocks-audit-protocol` 只承载强类型 DTO、严格 schema、唯一 compact canonical JSON、
4-byte big-endian ingest framing、export header/response DTO、HMAC canonicalization 和 golden
vectors，不执行 I/O。`crates/shadowsocks-auditd` 是 Linux-only binary，独占 UDS、spool、HMAC key
读取和 HTTP export；根 `ssserver` 不依赖这些 I/O 实现。

ingest 连接第一帧必须是 hello，最多 4 个连接但同一 daemon lifetime 只允许一个完成 hello 的
producer。auditd 先校验 `SO_PEERCRED`、node/runtime 和严格 JSON，再把原始 event bytes 嵌入 wrapper；
完成 open segment 写入、group `fdatasync` 与 sequence state 持久化后才回 ACK。重复 event ID 的内存
LRU（65536 条）返回原 ACK；payload 冲突永久 NACK 并断开。producer 在 retryable 错误、超时或未知
ACK 时保留 bytes，不能把“尚未放弃”计作 gap。

spool 采用 `open/`、`sealed/`、`leased/`、`acked/`、`quarantine/`、`state.json`、`tombstones.json`
布局。每个 wrapper 一行 NDJSON，保存原始 event canonical bytes 和 payload digest；segment 封口后
以 batch ID 导出，lease/ACK 通过跨目录原子 rename、双侧 fsync 和 durable receipt 保证崩溃可恢复。
容量达到 5 GiB 或文件系统低于 1 GiB 时按固定顺序回收；删除未 ACK 数据必须在后续可写 segment 生成
`spool_gap`，无法证明的数据字段使用 null。acked 副本正常保留 86400 秒，receipt ledger 最多 4096
项、最长 7 天。

export 只监听 `AF_UNIX`，精确提供 health、lease、ack 三条 HTTP/1.1 路由。每连接一个请求、严格
origin-form、有限 header/body、5 秒读写截止、`Cache-Control: no-store` 和节点 HMAC；Host 不进入
签名，nonce 仅在 timestamp/body digest/HMAC 全部验证后写入有界 replay cache。响应签名覆盖 raw body
digest 与 lease metadata，collector 必须先验签再解析并按 `(node_id,event_id)`、`(node_id,batch_id)`
幂等；auditd 错误或 health 不得输出 identity、target、event body 或 key。

## 安全边界

- `ServerConfig` 和 `ServerUser` 的 `Debug` 输出经过脱敏。
- exporter 使用专用 DTO，绝不序列化配置、密钥、identity hash、目标地址或客户端地址。
- socket 路径必须是规范绝对路径，逐级祖先都必须为真实目录且由 root 或服务最终 euid
  持有；直接父目录不得让 group/other 写入，更高层级仅允许 root 持有并带 sticky bit 的
  可写目录。路径中的 `.`、`..`、任意符号链接祖先，以及目标处的普通文件、符号链接或活动
  socket 都会被拒绝。
- exporter 先用 `O_NOFOLLOW` 打开模式 `0600` 的同路径 lockfile，取得非阻塞独占 `flock`
  后重新比较已打开 fd 与当前路径的设备号/inode，然后才探测、清理和绑定 socket。整个
  exporter 生命周期持有该锁，消除 lockfile 替换和并发实例之间的 stale-socket 清理竞态。
- Linux/Android 以 `O_PATH|O_NOFOLLOW` 打开 bind 后的 socket，核对固定 fd 的类型、设备号和
  inode，再经 `/proc/self/fd/<fd>` 修改该 inode 的 mode；其他 Unix 使用
  `fchmodat(..., AT_SYMLINK_NOFOLLOW)`。两条路径都在修改后重验 fd/path 的设备号、inode 和 mode。
  Linux/Android 的固定 fd 不会修改竞态替换对象；其他 Unix 分支保证不跟随最终符号链接，若路径
  被换成普通文件或另一 socket，则可能先修改其 mode，再由 inode 重验发现并失败关闭。该残余窗口
  受“直接父目录只有 root/服务 euid 可写”的既有可信目录边界约束。
- 清理 socket 时核对 bind 后记录的设备号与 inode，避免删除后来替换的路径。
- 原始 HTTP request-line、headers 与终止 CRLF 的总字节数、JSON 响应 body、并发数、读超时和
  写超时均有配置上限及编译期硬上限，header 数另有 64 个的固定硬上限；慢速或边界不完整的
  请求可以直接断开，错误响应不回显输入。
- 快照使用阻塞任务构造和序列化。连接写超时不能强制取消已启动的阻塞任务，因此连接许可由
  service 与阻塞任务共同持有；只有后台工作及其结果都释放后才归还。超时连接不能通过反复
  重连派生超过 `max_concurrent_clients` 的后台工作，新连接在资源仍忙时由后台 worker 返回 429。
  busy-response worker 独立限制为 32 个且写入不超过 100ms；它们全部占用时会直接关闭新连接，
  不会在 accept 循环内等待客户端 I/O 或创建无界任务。
- exporter 的认证/统计拒绝日志共享按源 IP 的 60 秒采样器，最多记录 4096 个 IP；更换源端口不能
  绕过限频，内存使用也有界。
- exporter 启动失败时 `ssserver` 启动失败；启动后 exporter task 与 relay 处于同一监督
  集合，任意异常退出、panic 或连续 3 次 `accept()` 失败都会使 `ssserver` 失败退出，并由部署服务管理器
  重启，避免
  “代理在运行但结算静默缺失”。只有单个客户端请求的协议错误、超时、超限或断开会被隔离，
  不会中断转发。
