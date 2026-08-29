# shadowsocks-rust-plus 用户成功访问审计实现规格

> 文档状态：实现就绪（implementation-ready）
>
> 目标读者：独立负责 `shadowsocks-rust-plus` 节点侧审计功能的开发同事
>
> 规范版本：4
>
> 最后决策日期：2026-08-28
>
> 版本沿革：
>
> - v2（2026-08-27）：UDP 去重窗口固定 60 秒、acked 保留固定 86400 秒、auditd 配置改为逐字段
>   校验表、producer ACK 超时与 auditd 写截止对齐，并明确 UDP shard/固定常量与 runtime 起点依赖；
> - v3（2026-08-28）：锁定首次审计 §17.6 第 1-4 条——重连总等待含 jitter 硬顶 5 秒；
>   `auditd_user` 解析失败按运行故障重试而不阻断 ssserver；ASCII domain 空 label 规范化失败但保留
>   访问事件；根 `user-audit` feature 显式包含 `dep:shadowsocks-auditd`；
> - v4（2026-08-28）：锁定第二轮审计决策——逐条 durability 提交、已认证空 UDP payload 在
>   feature-on/off 下保持相同数据面语义、`min_free_bytes` 仅作为运行期清理水位、未 ACK spool gap
>   使 health 为 `degraded`、producer diagnostic 按 bucket 独立重试并 round-robin，以及对
>   `spool_dir/.lock` 持有排他锁。同步记录 macOS 可执行验证与 Linux runtime、fuzz、性能及完整
>   capacity/crash 矩阵的环境限制。

本文是节点侧成功访问审计的规范性合同。实现者不需要再决定产品语义、组件边界、失败策略、
存储上限或协议形态。文中的“必须”“不得”“应当”分别对应 MUST、MUST NOT、SHOULD。

本文是本功能唯一权威实现规格。负责实现的工程师只修改本 `shadowsocks-rust-plus` 仓库；外部
controller、部署编排、反向代理和业务管理系统只是协议兼容方，不得要求实现者到其他仓库补写业务代码。
若下游集成文档与本文冲突，以本文的节点行为、wire schema 和 golden vectors 为准；确需改变合同必须先
在本文件中升版，再由外部兼容方跟进。

## 1. 已锁定的产品决策

| 项目 | 决策 |
| --- | --- |
| 审计范围 | 只记录已经满足成功条件的目标访问 |
| TCP 成功条件 | 同一 TCP relay 已分别成功转发至少一个上行应用字节和一个下行应用字节 |
| UDP 成功条件 | ssserver 在现有唯一成功计数点完整发送一个 outbound 数据报 |
| UDP 重复控制 | 缓存 key 未被淘汰或重置时，同一 association、同一规范目标 60 秒内最多一条；持续活跃每 60 秒可再记一条，LRU 淘汰或恢复可能产生提前重复 |
| 事件生命周期 | 只记录 success 事件，不记录 start/end 对、时长、结果或流量 |
| 代理可用性 | 合法配置下的运行时审计不得阻断、延迟等待、终止或拒绝代理流量 |
| 审计故障 | 允许漏记；能分配 sequence/写入存储时必须产生诊断记录，否则至少保留有界 health counter 与限频 journald |
| 证据强度 | 普通结构化日志，不提供防 root 篡改、hash chain 或数字签名 |
| 本机静态保护 | 明文文件，独立账号和 `0600` 权限 |
| 本机容量 | 每节点 5 GiB，达到上限循环保留最新数据 |
| 已上传副本 | 正常保留 24 小时，容量或磁盘水位优先 |
| 同事交付边界 | ssserver producer、`shadowsocks-auditd`、节点侧协议、存储、导出、打包和测试 |

以上决策优先于本文其余描述。若实现细节与表中决策冲突，以本表为准。

## 2. 目标、能力边界与非目标

### 2.1 目标

在一个或多个运行 ssserver 的物理节点上，以已经完成 EIH 认证的用户身份为归属，记录：

- 物理节点和 ssserver instance；
- 认证 `identity_name`；
- TCP 或 UDP；
- Shadowsocks 地址头携带的目标域名或 IP 与端口；
- 实际连接或发送使用的远端 IP；
- 发生成功证据的节点墙钟和 runtime 单调时间；
- TCP 双向载荷成功或 UDP 本机发送成功这一证据类型。

实现必须复用 ssserver 已认证的数据路径。不得通过旁路抓包猜测身份，不得解析 payload。

### 2.2 能可靠表达的事实

- 某个 EIH 认证身份通过某一物理 ssserver 成功访问了目标 `domain/IP:port`；
- TCP 至少有一个应用字节成功发往目标，且至少有一个应用字节成功返回客户端；
- UDP 数据报已被节点内核完整接受发送；这不等于远端已收到或响应；
- 当原始目标是域名时，能够同时保留原始域名、规范化搜索域名和本次实际使用的 IP。

### 2.3 不得声称能够表达的事实

- 完整 URL、HTTP path、query、Header、Cookie、请求体或响应内容；
- TLS SNI、ECH 内部名称、证书、明文协议内容或 DNS payload；
- 用户访问公共 DNS resolver 后在 DNS payload 中查询的域名；
- 上游转发层之前的真实客户端 IP；ssserver 看到的 transport peer 不是可靠用户来源；
- 客户端的 DIRECT、其他代理或未经过受审计 ssserver 的流量；
- UDP 远端已实际收到数据；
- 节点 root 未删除、伪造或修改尚未被 controller 接收的日志。

“访问网址审计”在本文中只表示目标域名或 IP 与端口，绝不表示完整网址。

### 2.4 明确不做

- 不增加 Zeek、Suricata、PCAP、eBPF 抓包或 TLS MITM；
- 不记录失败的认证、ACL 拒绝、DNS 失败、connect 失败、单向 TCP 或 UDP send 失败；
- 不记录连接结束、duration、outcome、包数和每连接字节数；
- 不更改 Shadowsocks wire protocol、iPSK、uPSK 或订阅格式；
- 不在第一版实现 TPM/HSM、hash chain、Ed25519 manifest 或日志静态加密；
- 不把 controller、数据库 DDL、管理后台或案件管理纳入本同事的代码交付。

## 3. 交付边界和代码基线

### 3.1 上游与 overlay

固定基线：

- upstream：`shadowsocks-rust v1.24.0`；
- commit：`7ee1aa9223ed8f4d34734aac919036c8ad4502c2`；
- 交付仓库：本 `shadowsocks-rust-plus` 仓库；这是实现者唯一可写的仓库；
- 发布目标：Linux x86_64 musl；
- 现有补丁：`0001-eih-user-stats-http-unix-exporter.patch`、
  `0002-reproducible-build-time.patch`。

审计功能必须作为新的 `0003-user-audit.patch` 交付，不得重写已经验收的统计补丁。实现完成后同步更新
`patches/series`、`patches/README.md`、架构/API/运维文档、构建脚本和发布 manifest。

### 3.2 本同事必须交付

- ssserver 成功访问事件生成与异步 producer；
- 共享的强类型 event、ingest protocol 和 export protocol；
- 新二进制 `shadowsocks-auditd`；
- auditd 配置解析、UDS、NDJSON spool、lease/ack export 和 health；
- systemd、sysusers、tmpfiles 与配置样例；
- `ssserver` 和 `shadowsocks-auditd` 两个 Linux release artifact；
- 两个 artifact 的 SHA-256、detached signature 和 release manifest；
- Rust 单元/集成测试、协议 golden vectors、fuzz target、故障与性能测试；
- 可供外部 collector 开发使用的 mock collector/client。

workspace 布局固定为：

- 新建 `crates/shadowsocks-audit-protocol`：不执行 I/O，承载严格 event/diagnostic DTO、ingest framing DTO、
  export DTO、HMAC canonicalization 和 golden vectors；
- 新建 `crates/shadowsocks-auditd`：Linux-only binary，依赖 protocol crate；
- `shadowsocks-service` 的可选 `user-audit` feature 依赖 protocol crate并实现 producer；
- 根 `ssserver` 不直接包含 spool、HTTP export 或 HMAC key 读取代码。

### 3.3 外部依赖，不属于本同事交付

- 外部 controller collector；
- 外部数据库 migration、分区和保留期清理任务；
- Admin 查询、导出、权限和管理员行为审计；
- 任意反向代理、传输隧道配置和实际节点部署。

上述外部依赖不得在本仓库实现。本同事仍必须实现本文件定义的 export 合同，并通过仓库内 mock collector
验证互通；外部兼容方负责适配本合同，而不是反向把其私有拓扑、账号或存储实现固化进本仓库。

## 4. 现有接线点与必须新增的内部类型

现有 `user-stats` 已提供以下可信接线：

- TCP 的 `ProxyServerStream::authenticated_user()`；
- UDP 的 `UdpSocketControlData.user`；
- server/user registry、generation 和流量 counter；
- 16 字节随机 runtime ID 的 32 个小写十六进制字符表示；
- runtime 的 `started_at_unix_ms`；
- TCP/UDP 数据路径中唯一的应用载荷计数位置。

现有实现没有公开 runtime/server/identity 的组合元数据 handle，也没有保存 runtime 起点的 `Instant`。
审计实现必须新增以下不可变类型和 API；这些都是本次 patch 的新增项，不得把它们误认为现有 API，
也不得为取得元数据调用统计 `snapshot()`，因为 snapshot 会推进统计 sequence。

```rust
struct AuditRuntimeMetadata {
    node_id: Arc<str>,
    runtime_id: Arc<str>,
    started_at_unix_ms: u64,
    runtime_started_at: Instant,
}

struct AuditServerMetadata {
    server_id: Arc<str>,
    server_generation: u64,
}

struct AuditIdentityMetadata {
    identity_name: Arc<str>,
    identity_generation: u64,
}

struct AuditIdentityHandle {
    runtime: Arc<AuditRuntimeMetadata>,
    server: Arc<AuditServerMetadata>,
    identity: Arc<AuditIdentityMetadata>,
    counters: Arc<UserTrafficCounters>,
}

struct AuditEmitter {
    audit_sequence: AtomicU64,
    queue: Arc<ArrayQueue<EventDraft>>,
    notify: Arc<Notify>,
    diagnostics: Arc<DiagnosticAccumulators>,
    udp_windows: Arc<UdpAuditWindowCache>,
    lifecycle_state: AtomicU64, // high bit CLOSED, low 63 bits active observation guards
}

#[cfg(feature = "user-audit")]
impl UserStatsRegistry {
    fn new_with_audit(node_id: impl Into<String>, max_identities: usize)
        -> Result<Arc<Self>, AuditInitError>;
}

impl ServerStatsHandle {
    fn audit_identity_handle(&self, name: &str) -> Option<Arc<AuditIdentityHandle>>;
}
```

feature-off 和 feature-on 但未配置 `user_audit` 时，现有 `UserStatsRegistry::new()` 路径保持原样，不构造
任何审计元数据。`user-audit` feature 下新增 `UserStatsRegistry::new_with_audit()`（可共享 private
`new_inner`）；只有配置存在且通过验证时，调用它在读取现有 `started_at_unix_ms` 的同时捕获一次
`Instant::now()`，并构造每个 process runtime 唯一的 `Arc<AuditRuntimeMetadata>`。不得用墙钟差值生成
`runtime_monotonic_ms`。`new_with_audit()` 不执行任何 I/O，失败返回专用的 `AuditInitError`（属于
配置错误，必须阻止启动），不得复用 `io::Result`。

audit-enabled registry 的 `register_server()` 构造与该 stable server record/generation 绑定的 server
metadata；`register_user()` 构造与 active user record/generation 绑定的 identity metadata 和
`AuditIdentityHandle`。`audit_identity_handle()` 必须复用 `user_stats_counter()` 相同的 registry/server
lifecycle 校验与锁顺序，一次返回属于同一 active generation 的 runtime、server、identity 和 counter，
不得分别查找后拼装；statistics-only registry 固定返回 `None`。原有 `register_user()`、
`user_stats_counter()` 和 counter 语义保持兼容。

typed handle 在 registry 注册路径构造并注入 `ServiceContext`；TCP/UDP relay 只能持有或克隆该 `Arc`，
不得在 hot path 查询 snapshot 或重新读取配置。事件只允许使用认证得到的 `ServerUser.name()`，不得从
请求、自定义 header 或 transport peer 覆盖身份。

每个 ssserver process/runtime 只能构造一个 `Arc<AuditEmitter>` 和一个 AuditSupervisor；emitter 拥有全
进程唯一 audit sequence、queue、Notify、diagnostic accumulators、UDP audit window cache 和
closed/active lifecycle state。server builder 通过新增 `ServiceContext::set_user_audit_emitter()` 把同一个
emitter clone 给所有 static server；TCP tracker 和 UDP association 只能继续 clone 这个 handle，禁止每个
server 各建 sequence、queue、cache 或 supervisor，否则第二个 ingest hello 会被 `producer_busy` 拒绝。
术语约定：AuditEmitter 是进程内共享状态句柄，AuditSupervisor 是消费 queue 的唯一 task，AuditClient
只是 AuditSupervisor 每次连接 session 的内部实现；relay 只持有 emitter。

每次审计 observation 必须先调用 `begin_observation()`：用 CAS 在 CLOSED bit 未设置时增加 active count，
返回的 RAII guard 在所有路径减一。TCP 在双向条件刚满足时取得 guard；UDP 在完整 send 成功后、访问 shard
前取得，guard 必须覆盖 cache 判断、contention accumulator 更新、sequence/draft 和 `force_push()` 全过程。
active count 达上限时按关闭处理。

`close_emitter()` 原子设置 CLOSED 后禁止取得新 guard；此后的 relay hook 仅以非阻塞的原子饱和递增增加本机
`shutdown_skipped_observations` 并立即返回。`drain()` 必须先等 active count 归零，再冻结 diagnostic
accumulators、尝试最终 snapshot，并把 pending diagnostics、queue 和 in-flight 全部纳入判空，避免 close
与并发 force_push/contention update 的竞态；最后一个 guard 释放时唤醒 drain。整个过程不能触碰或阻塞
relay，2 秒总 timeout 仍由根 launcher 控制。该 skipped counter 不属于 drain 判空条件，也不得在 relay
仍可能运行时称为最终值；根 launcher 终止并 await/join 全部 relay task 后，以 `swap(0, AcqRel)` 取得稳定
快照，在停止 AuditSupervisor/runtime 前最多写一条 final journald。小于 `u64::MAX` 时为精确值，等于上限
表示至少该值；若进程在 join 稳定点前被 SIGKILL，则允许没有 final journald。

主要接线位置固定为：配置和 feature 在 `crates/shadowsocks-service/src/config.rs`，不可变元数据与
`Arc<AuditEmitter>` handle 在 `crates/shadowsocks-service/src/server/context.rs`，成功条件分别在
`server/tcprelay.rs` 和 `server/udprelay.rs`。不得通过用户累计计数器前后相减推算单连接状态。

## 5. Cargo feature 与配置合同

### 5.1 Cargo feature

根 crate 和 `shadowsocks-service` crate 同时增加非默认 feature，传播关系固定为：

```toml
# root Cargo.toml
user-audit = ["user-stats", "shadowsocks-service/user-audit", "dep:shadowsocks-auditd"]

# crates/shadowsocks-service/Cargo.toml
user-audit = ["user-stats", "dep:shadowsocks-audit-protocol", "dep:crossbeam-queue"]

[dependencies]
shadowsocks-audit-protocol = { path = "../shadowsocks-audit-protocol", optional = true }
crossbeam-queue = { version = "0.3.13", optional = true }
```

约束：

- 只支持 Linux；不是泛 Unix feature；
- 必须依赖 `user-stats` 的 EIH 身份接线；
- 只支持静态 server 配置和支持 EIH 的 AEAD-2022 method；
- 与 built-in/standalone manager 模式互斥；
- feature 未编译或未配置时，不创建 queue、task、callback 或 audit metadata；
- 配置包含 `user_audit` 但二进制未编译 feature 时，配置校验必须明确失败；
- 顶层 wire `SSConfig` 现有实现没有 `deny_unknown_fields`，所以
  `user_audit: Option<SSUserAuditConfig>` 及其纯配置 DTO 必须在 feature-off build 也参与反序列化；
  `load/check_integrity` 遇到 `Some` 时显式返回 unsupported-feature。只有 runtime emitter/handle/task 接线
  使用 `#[cfg(feature = "user-audit")]`，禁止靠 unknown field 或 `cfg` 掉 wire 字段来实现失败；
- `user_audit` 结构错误属于配置错误，必须阻止启动；auditd 运行故障不是配置错误。

### 5.2 ssserver 配置

顶层增加：

```json
{
  "user_stats": {
    "node_id": "node-example-01",
    "socket_path": "/run/shadowsocks-rust-plus/user-stats.sock"
  },
  "user_audit": {
    "ingest_socket_path": "/run/shadowsocks-audit/ingest/ingest.sock",
    "auditd_user": "shadowsocks-audit",
    "queue_capacity": 4096,
    "max_udp_targets_per_association": 256,
    "max_udp_target_windows": 65536
  }
}
```

| 字段 | 默认值 | 可配置范围 | 编译期硬上限 |
| --- | ---: | ---: | ---: |
| `queue_capacity` | 4096 | 256–4096 | 4096 |
| `max_udp_targets_per_association` | 256 | 1–256 | 256 |
| `max_udp_target_windows` | 65536 | 16384–65536 | 65536 |
| 单事件 JSON | 不可配置 | 不适用 | 8192 bytes |

仓库发布验收使用表中的默认值；部署方只能在给定范围内调整，不能越过编译期硬上限。

配置验证规则：

- `user_audit` 使用 `deny_unknown_fields`；
- node ID 只复用 `user_stats.node_id`，不得重复配置第二份 node ID；
- `ingest_socket_path` 必须是规范绝对路径，不含 `.`、`..`、空组件或 symlink parent；symlink
  parent 在启动解析配置时校验一次，运行期由 `SO_PEERCRED`/socket inode owner 校验和 root 管理的
  父目录（第 11 节）兜底，路径组件不得可被非特权账号替换；
- `auditd_user` 是部署时配置的专用 daemon 账号，示例值为 `shadowsocks-audit`；ssserver 启动时解析为
  UID，连接后同时校验
  `SO_PEERCRED.uid` 和 socket inode owner UID；校验失败视为连接失败：按第 7.3 节退避重连、
  置 producer health degraded、写限频 journald，不得影响代理，也不得使 ssserver 退出；
- 审计不得要求、设置或改写现有顶层 `udp_max_associations`，也不得改变数据面既有 TTL 或淘汰行为；
  UDP NAT association 的容量和生命周期仍完全由原数据面配置决定；
- UDP 去重窗口固定为 60 秒（第 1 节决策），不提供配置项；
- 审计窗口固定分为 64 个 shard；使用 checked arithmetic 验证 `max_udp_target_windows` 能被 64 整除，
  且每 shard capacity 不小于 `max_udp_targets_per_association`。默认值为
  `65536 / 64 = 1024` 项/shard；
- node/server/identity identifier 沿用现有规则：非空、最多 128 bytes、仅可打印非空白 ASCII；
- 启用审计时，每个静态 server 必须具有非空、唯一 `server.id`；
- 任何范围错误必须在配置加载和 service 运行入口各校验一次；
- 不提供 `failure_mode` 和 `peer_address_mode`，因为第一版行为已经固定。

### 5.3 auditd 配置

CLI 固定为 `shadowsocks-auditd [--config <path>]`：省略参数时读取
`/etc/shadowsocks-audit/auditd.json`；使用 `--config` 时，`path` 必须是 UTF-8、规范绝对路径。未知参数、重复
`--config`、缺失值、相对路径、`.`/`..`、空组件或 symlink parent 都必须在任何 socket/文件创建前失败。
不提供环境变量或搜索多个候选位置的 fallback，systemd unit 使用非默认生产路径时必须显式传入唯一的
`--config`。默认配置内容为：

```json
{
  "schema_version": 1,
  "node_id": "node-example-01",
  "producer_user": "shadowsocks",
  "export_peer_user": "audit-exporter",
  "ingest_socket_path": "/run/shadowsocks-audit/ingest/ingest.sock",
  "export_socket_path": "/run/shadowsocks-audit/export/export.sock",
  "spool_dir": "/var/lib/shadowsocks-audit",
  "max_spool_bytes": 5368709120,
  "min_free_bytes": 1073741824,
  "segment_max_bytes": 4194304,
  "segment_max_age_seconds": 60,
  "export_max_response_bytes": 8388608,
  "export_hmac_key_file": "/etc/shadowsocks-audit/export-hmac"
}
```

这些默认值构成发布验收 profile。部署方只能在下列范围内调整（自动化测试可以使用更小值）：

| 字段 | 默认值 | 可配置范围 | 编译期硬上限 |
| --- | ---: | ---: | ---: |
| `max_spool_bytes` | 5368709120 | 67108864–5368709120 | 5368709120 |
| `min_free_bytes` | 1073741824 | 268435456–5368709120 | 5368709120 |
| `segment_max_bytes` | 4194304 | 16384–4194304 | 4194304 |
| `segment_max_age_seconds` | 60 | 1–3600 | 3600 |
| `export_max_response_bytes` | 8388608 | 4194304–8388608 | 8388608 |

另需满足：

- `max_spool_bytes` 必须大于 `2 × segment_max_bytes`；
- `min_free_bytes` 必须至少 256 MiB 且小于所在文件系统总容量；它只作为运行期清理触发水位，
  不得阻止 auditd 启动；低于水位时按第 9.5 节清理并在 health 中反映 degraded；
- `segment_max_bytes` 必须不小于协议 crate 计算的最大单条 record 字节数（8192 字节事件 +
  wrapper 开销 + LF），否则空 segment 可能被单条超限 record 静默写超；
- `export_max_response_bytes` 必须不小于 `segment_max_bytes`；默认 8 MiB 是 segment 硬上限的
  两倍余量，用于容纳恢复路径中因单 record 超过配置上限而略超的历史 segment；
- `producer_user` 与 `export_peer_user` 必须解析为两个不同的 UID。

auditd 配置同样拒绝未知字段、symlink parent、错误 owner/mode 和相对路径；symlink parent 在启动时、
任何 socket/文件创建前校验一次，父目录权限要求见第 11 节。

HMAC key 文件必须恰好为 64 个小写十六进制字符加一个可选末尾 LF，解码后为 32 个随机字节。
每节点使用不同 key，不得复用 iPSK、uPSK、stats CA、release signing key 或其他节点 key。

### 5.4 固定常量（不可配置）

以下常量第一版固定，不进入任何配置文件；改动必须先对本文件升版：

- UDP 去重窗口 60 秒；审计窗口 64 shard（第 5.2/6.4 节）；
- acked 副本正常保留 86400 秒（24 小时），容量/磁盘水位优先（第 1/9.5/10.1 节）；
- 每条已接受 record 独立完成 `write_all`、open segment `fdatasync` 和 `state.json` durability
  barrier 后才 ACK；当前 producer/ingest lock-step 不启用 group commit，因此 auditd 配置不得包含
  `group_commit_max_events` 或 `group_commit_max_delay_ms`；
- in-flight 硬上限 256 条；producer 读 ACK 超时 3 秒（第 7.2/7.3 节）；
- ingest 最多 4 连接、hello/partial frame/response 截止 2 秒；export 最多 4 并发连接、
  请求/响应截止 5 秒（第 8.1/10.1 节）；
- 单事件 JSON 与 ingest frame 上限 8192 bytes（第 5.2/8.1 节）；
- UDP contention snapshot 与同类 journald 错误各 60 秒限频（第 6.5/7.2 节）；
- HMAC timestamp 偏差 300 秒；nonce cache 10 分钟 / 4096 条（第 10.2 节）；
- dedup LRU 65536 条（第 8.3 节）；tombstone ledger 4096 项、receipt 保留 7 天（第 9.5 节）。

## 6. 成功事件定义

### 6.1 公共 JSON 规则

- UTF-8 JSON object；禁止顶层数组、重复 key、NaN、Infinity 和尾随数据；
- schema 未声明的字段拒绝；
- `schema_version` 是 JSON integer，第一版固定为 `1`；
- 所有可能达到 `u64` 的值均编码为十进制 JSON string；允许 0 的字段必须匹配
  `0|[1-9][0-9]*`，要求正数的 sequence/count/byte-size 字段必须匹配 `[1-9][0-9]*`；一律拒绝正号、
  负号、前导零、指数、小数、空白和超出 `u64` 的值；
- generation 是有界 JSON integer，第一版现有 registry 值为 `1`；
- 128-bit 随机 ID 编码为 32 个小写十六进制字符；
- runtime ID 沿用现有 32 个小写十六进制字符；
- `event_id = runtime_id + ":" + audit_sequence`；
- `audit_sequence` 在一个 runtime 内使用 `AtomicU64` 从 1 递增，不回绕；
- sequence 表示事件分配顺序，不保证送达顺序；队列丢失时允许缺号；
- 达到 `u64::MAX` 后停止生成审计事件并告警，代理继续服务。

公共字段：

| 字段 | 类型 | 约束 |
| --- | --- | --- |
| `schema_version` | integer | 固定 1 |
| `record_type` | string | 访问事件固定 `access` |
| `event_type` | string | `tcp_target_success` 或 `udp_target_success` |
| `event_id` | string | runtime 内唯一，最多 64 bytes |
| `audit_sequence` | decimal string | 1–`u64::MAX` |
| `occurred_at_unix_ms` | decimal string | 成功条件首次满足时的墙钟 |
| `runtime_monotonic_ms` | decimal string | 从 user-stats runtime `Instant` 起算 |
| `node_id` | string | 复用 `user_stats.node_id` |
| `runtime_id` | string | 现有 32 字符小写十六进制 runtime ID |
| `server_id` | string | 静态 server instance ID |
| `server_generation` | integer | 当前固定 1 |
| `identity_kind` | string | 固定 `user` |
| `identity_name` | string | 已认证 `ServerUser.name()` |
| `identity_generation` | integer | 当前固定 1 |
| `transport` | string | `tcp` 或 `udp` |
| `target` | object | 原始目标、规范目标、端口和实际 IP |
| `success_evidence` | string | 固定枚举，见下文 |

### 6.2 目标对象与规范化

```json
{
  "kind": "domain",
  "host": "Example.COM.",
  "normalized_host": "example.com",
  "port": 443,
  "remote_ip": "192.0.2.10"
}
```

规则：

- `host` 是 Shadowsocks 地址头解析后的目标，不是 DNS payload 或 TLS SNI；
- domain 最多 255 UTF-8 bytes；不得因为审计规范化而改变实际路由目标；
- ASCII domain：转换为小写并移除一个末尾点；任何空 label（包括连续点或仅由点组成的输入）均视为
  规范化失败，保留原始 `host` 并将 `normalized_host` 置为 `null`；
- Unicode domain：使用 UTS #46 non-transitional + STD3 规则转 ASCII、小写并移除一个末尾点；
- 规范化失败时保留 `host`，`normalized_host` 为 `null`，不得丢弃访问事件；
- IP 使用 `std::net::IpAddr::to_string()` 的规范文本；IPv4-mapped IPv6 保持 IPv6，不折叠；
- IP 目标的 `host` 与 `normalized_host` 均为相同规范 IP；
- `remote_ip` 是本次实际连接或发送使用的 IP；domain 目标不得只记录 DNS 返回列表；
- port 是 1–65535 的 JSON integer。

不记录 transport peer address。

### 6.3 TCP：`tcp_target_success`

TCP 只有在同一个 relay 已经满足以下两个条件后才生成一条事件：

1. 至少一个应用载荷字节已成功写入目标连接；
2. 至少一个目标返回的应用载荷字节已成功写回客户端连接。

仅 TCP handshake 成功不算；仅上行、仅 banner 下行、DNS/connect 成功但无双向载荷均不记录。如果目标先发
banner，必须等首个客户端载荷也成功写入目标后才满足条件。

不得重写底层 bidirectional copy helper。现有 user-stats 已在 TFO first write 之前用两个
`TcpTrafficStream` writer wrapper 覆盖普通与 TFO 路径；在 `user-audit` 下给两个 wrapper 共享一个
`Arc<TcpSuccessTracker>`，其中 `AtomicU8` 的 uplink/downlink bit 初始为 0。每个 wrapper 另有 task-local
`seen_positive: bool`；`poll_write()` 和 `poll_write_vectored()` 只有在 inner 首次返回 `Ok(n)` 且 `n > 0`
时才把本地 bool 置 true，并对共享 tracker 执行一次 `fetch_or`。仅观察到状态从非 `0b11` 转为 `0b11` 的
调用执行一次非阻塞 `try_emit()`；因此每连接共享 RMW 最多两次。重复 positive write、`Pending`、`Ok(0)`
和 `Err` 均不得再改变 bit。wrapper 仍在相同位置更新原有 user-stats counter。

远端连接成功后、构造 wrapper 前读取 `remote_stream.peer_addr()` 并固化为事件的 `remote_ip`。读取失败
不能让已建立的代理连接失败；仍创建缺少 remote IP 的 tracker 并继续观察两个 direction bit，若之后达到
双向成功条件，则分配 audit sequence 并把这条确定无法构造的 access 累计为 `encode_error` gap。tracker
持有本连接已解析的 target、typed identity handle、可选 remote IP 和一次性状态；callback 不得执行 JSON
序列化、socket I/O、await 或磁盘操作。

```json
{
  "schema_version": 1,
  "record_type": "access",
  "event_type": "tcp_target_success",
  "event_id": "0123456789abcdef0123456789abcdef:42",
  "audit_sequence": "42",
  "occurred_at_unix_ms": "1787587200000",
  "runtime_monotonic_ms": "123456",
  "node_id": "node-example-01",
  "runtime_id": "0123456789abcdef0123456789abcdef",
  "server_id": "ss-entry-01",
  "server_generation": 1,
  "identity_kind": "user",
  "identity_name": "user-example-01",
  "identity_generation": 1,
  "transport": "tcp",
  "target": {
    "kind": "domain",
    "host": "Example.COM.",
    "normalized_host": "example.com",
    "port": 443,
    "remote_ip": "192.0.2.10"
  },
  "success_evidence": "tcp_bidirectional_payload"
}
```

一个 TCP connection 最多生成一条。TFO 和普通 connect 必须使用相同成功定义。

### 6.4 UDP：`udp_target_success`

UDP 在现有 user-stats uplink 唯一成功计数点判断：outbound send 返回 `Ok(bytes_sent)`，且
`payload.len() > 0`、`bytes_sent == payload.len()`。short datagram write 视为失败。这个条件只证明节点
内核完整接受了本次发送，不证明远端接收或响应，也不要求上游内部 API 必须恰好命名为 `send_to()`。

已认证的空 UDP payload 与非空 payload 采用相同的数据面处理：feature-on/off 都放行成功的完整发送，
但空 payload 不生成 `udp_target_success`（审计事件的成功条件仍要求 `payload.len() > 0`）。

现有 UDP helper 在成功后只返回 `()`，domain lookup 路径也会丢弃最终成功的 `SocketAddr`；本次必须把
send helper 改为仅在完整 write 后返回 `io::Result<SocketAddr>`。返回值是经过 IP stack capability 映射、
实际传给 socket send 的最终地址；domain lookup 必须保留多个候选中真正发送成功的那个地址，不能再
`.map(|_| ())`。outer dispatch 在调用 helper 前保留原始 Shadowsocks `Address`，成功后同时用原始
Address 构造 `host`/`normalized_host`，用返回的最终 `SocketAddr` 构造 `remote_ip`/实际端口，然后才执行
cooldown 和 emit。send 失败或 short write 不得返回地址、更新 cooldown 或生成事件。

每个 UDP association 创建随机 128-bit `association_id`。去重 key 为：

```text
(server_id, association_id, target.kind, target.normalized_host-or-host, target.port)
```

使用 runtime 单调时间实现 60 秒冷却：

- 审计使用独立于 UDP NAT map 的 process-wide `UdpAuditWindowCache`，固定 64 个
  `std::sync::Mutex<AuditWindowShard>`；不得改动或借用现有 association LRU capacity；
- shard index 固定取 `association_id` 128-bit 随机值的前 8 个 byte（非 hex 文本）按 big-endian
  组成的 `u64 & 63`；一个 association 的所有
  target 必在同一 shard。`shard_capacity = max_udp_target_windows / 64`，默认 profile 为 1024；LRU 与 association
  count map 都以 `(server_id, association_id)` 区分 lineage，总窗口严格不超过配置值；
- 每个 association 最多保留配置的 `max_udp_targets_per_association`（默认 profile 为 256）个 key；插入下一项时，
  在持有该 shard lock 的情况下从最多 `shard_capacity` 项中有界扫描并淘汰该 association 最老 key。
  shard 满时淘汰 shard 最老 key并同步递减 count；count 归零立即删除 map entry，因此
  `count_map.len() <= lru.len()`，association ID churn 不能造成无界增长；
- 每个完整 UDP send 成功后只调用一次 `try_lock()`，不得等待 mutex。拿到 lock 时完成 lookup/LRU 更新后
  立即释放，再分配 sequence、构造 draft 和 `try_emit()`；不得持锁执行序列化、queue 或 socket 操作；
- `try_lock()` 返回 `WouldBlock` 时立即放行数据报，不分配 audit sequence、不更新 cooldown、不猜测该
  datagram 原本是否应生成 access event；只累计严格定义的 `udp_window_contention` 诊断。后续成功
  datagram 会重新尝试；
- 生产 `panic=abort` 下不会观察到 poisoned mutex；测试/unwind build 的 `Poisoned` 是唯一恢复例外：
  `into_inner()` 取得现成 guard、清空该 shard并累计 saturating reset health counter，再按空 cache 处理；
  guard 仍持有时调用 `Mutex::clear_poison()`，避免每包重复 reset。它不是 `WouldBlock` contention，禁止
  `unwrap()` 或影响数据报；
- key 不存在或距离 `last_audit_attempt_at` 已满 60 秒：在 lock 内先把 attempt time 更新为当前 monotonic
  time，再释放 lock并调用非阻塞 `try_emit()`；因此后续 sequence/build/enqueue 失败也不会让每个 packet
  重试审计；
- 未满 60 秒：不重复记录；
- 无论是否满足 60 秒，成功取得 lock 的 lookup 都刷新 LRU recency；只有资格判断为“本次应尝试 access
  event”时才更新 `last_audit_attempt_at`，持续活跃目标因此每 60 秒仍可再尝试记录；
- outbound send 失败或 short write：不建立或刷新窗口；
- 无论 enqueue 成功与否，都刷新窗口，避免故障时每个数据报反复冲击 queue；
- association 自然过期或重建时使用新 ID；旧审计 key 仅占用有界 shard 空间并由 LRU 回收，不需要在
  association drop path 获取锁；
- per-association 或 shard LRU 淘汰、poison recovery 都可能导致 60 秒内提前再记一条，属于允许的重复，
  不得阻断流量。mutex contention 的语义是明确漏观察而不是重复，必须按第 6.5 节披露。

```json
{
  "schema_version": 1,
  "record_type": "access",
  "event_type": "udp_target_success",
  "event_id": "0123456789abcdef0123456789abcdef:43",
  "audit_sequence": "43",
  "occurred_at_unix_ms": "1787587200500",
  "runtime_monotonic_ms": "123956",
  "node_id": "node-example-01",
  "runtime_id": "0123456789abcdef0123456789abcdef",
  "server_id": "ss-entry-01",
  "server_generation": 1,
  "identity_kind": "user",
  "identity_name": "user-example-01",
  "identity_generation": 1,
  "transport": "udp",
  "association_id": "89abcdef0123456789abcdef01234567",
  "target": {
    "kind": "ip",
    "host": "192.0.2.53",
    "normalized_host": "192.0.2.53",
    "port": 53,
    "remote_ip": "192.0.2.53"
  },
  "success_evidence": "udp_send_ok"
}
```

### 6.5 诊断记录

诊断记录用于披露已知日志缺口，不代表用户访问。它们与访问事件使用相同 ingest、spool 和 export
通道，并且同样必须是严格 schema。

`producer_gap` 由 ssserver 生成：

```json
{
  "schema_version": 1,
  "record_type": "diagnostic",
  "event_type": "producer_gap",
  "event_id": "0123456789abcdef0123456789abcdef:99",
  "audit_sequence": "99",
  "occurred_at_unix_ms": "1787587210000",
  "node_id": "node-example-01",
  "runtime_id": "0123456789abcdef0123456789abcdef",
  "reason": "queue_overflow",
  "permanent_nack_code": null,
  "dropped_events": "55",
  "first_dropped_sequence": "44",
  "last_dropped_sequence": "98",
  "first_seen_unix_ms": "1787587201000",
  "last_seen_unix_ms": "1787587209000"
}
```

- reason 固定为 `queue_overflow`、`encode_error` 或 `permanent_nack`；
- reason 为 `permanent_nack` 时，`permanent_nack_code` 必须是本版非 retryable event NACK code
  `invalid_schema`、`event_id_conflict` 或 `runtime_mismatch`；另外两个 reason 时该字段必须为 null；
- `dropped_events` 是 producer 已明确放弃、不再保证送达的 event 精确累计值；连接错误或 ACK 状态未知
  不计入。它只披露 producer 观察到的缺口，不证明 controller 最终缺少多少行；并发或非连续缺口使
  边界无法确定时，first/last sequence 可以为 null；
- producer gap 自己取得新的 audit sequence，但不进入 access queue。它构造、序列化或被永久 NACK 时，
  只把所携带的原始 dropped count 合并回 queue 外 accumulator，不能把该 diagnostic 再计为一个新的
  access drop，避免递归生成 gap；
- 不得包含被丢事件的 identity 或 target。

`sequence_exhausted` 是 producer 内部状态而非 wire diagnostic：无法再分配 producer gap 的 audit
sequence，因此只进入本机 health counter 和限频 journald；代理继续服务，禁止回绕。

`udp_window_contention` 由 ssserver 生成，用于披露已经成功发送、但因审计 shard lock 正忙而没有执行
cooldown/access-event 判断的数据报；它不等于确定丢失了相同数量的 access event：

```json
{
  "schema_version": 1,
  "record_type": "diagnostic",
  "event_type": "udp_window_contention",
  "event_id": "0123456789abcdef0123456789abcdef:100",
  "audit_sequence": "100",
  "occurred_at_unix_ms": "1787587211000",
  "node_id": "node-example-01",
  "runtime_id": "0123456789abcdef0123456789abcdef",
  "skipped_successful_datagrams": "17",
  "first_seen_unix_ms": "1787587201000",
  "last_seen_unix_ms": "1787587210000"
}
```

- `skipped_successful_datagrams` 统计 `try_lock()` 返回 `WouldBlock` 的完整成功 UDP send；小于
  `u64::MAX` 时精确，等于 `u64::MAX` 时只表示“至少该值”，并把 producer health 置为 degraded。controller
  不得把它并入 `dropped_events` 或 access-loss 数；
- 不得填 identity、target、association ID 或猜测本来应产生的 access event 数；
- counter 和 first/last time 位于 queue 外；AuditSupervisor 最多每 60 秒 snapshot 一次并直接放入
  in-flight。counter 从 0 变为非 0 时必须 `notify_one()`；supervisor 另持有下一次允许 snapshot 的 timer，
  即使 access queue 没有流量也会主动醒来。诊断构造、序列化或永久 NACK 时只合并回自身 accumulator，
  不递归生成 producer gap；
- 同时最多一个 contention snapshot/in-flight；shutdown drain 忽略 60 秒 rate limit 尝试最后一次
  snapshot，若 2 秒内仍无法持久化，退出前至少输出最终聚合 journald；
- 时间必须满足 `first_seen_unix_ms <= last_seen_unix_ms <= occurred_at_unix_ms`；snapshot 时
  `occurred_at=max(当前合法墙钟,last_seen)`，若 snapshot 墙钟非法则使用已有 last_seen。某次 contention
  的墙钟读取/转换失败时，不伪造时间，也不
  混入 on-wire accumulator，而是累计独立 `udp_window_contention_time_unknown` health counter；
- 同时写入最多每 60 秒一条的聚合 journald；若 sequence 不可用，至少保留饱和 health counter。

`spool_gap` 由 auditd 生成：

```json
{
  "schema_version": 1,
  "record_type": "diagnostic",
  "event_type": "spool_gap",
  "event_id": "spool:0123456789abcdef0123456789abcdef",
  "occurred_at_unix_ms": "1787587220000",
  "node_id": "node-example-01",
  "spool_epoch": "89abcdef0123456789abcdef01234567",
  "lost_spool_epoch": "89abcdef0123456789abcdef01234567",
  "reason": "capacity_eviction",
  "first_lost_spool_sequence": "1",
  "last_lost_spool_sequence": "1000",
  "lost_events": "1000",
  "lost_bytes": "4194304",
  "lost_batch_id": "fedcba9876543210fedcba9876543210"
}
```

- `event_id` 使用 `spool:` 加随机 128-bit gap ID，在节点内唯一；
- `spool_epoch` 是 gap record 自身所属 epoch，`lost_spool_epoch` 是被描述数据所属 epoch；
- reason 固定为 `capacity_eviction`、`min_free_eviction`、`quarantine_eviction`、`tail_truncation`、
  `segment_corruption` 或 `state_reset`；`storage_unavailable` 只表示 record 尚未被接受，不能伪装成
  已持久化后又丢失的 spool gap；
- 无法确定的 sequence、event count、bytes 或 batch ID 字段使用 null，不得猜测；
- gap 记录不得写入它所描述的已损坏或即将删除 segment，而应进入下一个可写 segment。

variant 字段集合固定如下；“禁止”意味着字段必须不存在，不能用 null 代替。所有 object 仍拒绝未知字段：

| variant | 必填字段 | nullable 字段 | 禁止字段 |
| --- | --- | --- | --- |
| `tcp_target_success` | 第 6.1 节全部 access 公共字段；target 的 `kind/host/port/remote_ip` | `target.normalized_host` | `association_id`、所有 gap 字段 |
| `udp_target_success` | TCP 的字段集合加 `association_id` | `target.normalized_host` | 所有 gap 字段 |
| `producer_gap` | 示例中的全部顶层字段 | `permanent_nack_code`；`first_dropped_sequence` 与 `last_dropped_sequence` 必须同时为值或同时为 null | access、target、association、spool/contention 字段、`runtime_monotonic_ms` |
| `udp_window_contention` | 示例中的全部字段 | 无 | access、target、association、producer/spool gap 字段、`runtime_monotonic_ms` |
| `spool_gap` | `schema_version/record_type/event_type/event_id/occurred_at_unix_ms/node_id/spool_epoch/reason` | `lost_spool_epoch/first_lost_spool_sequence/last_lost_spool_sequence/lost_events/lost_bytes/lost_batch_id` | `runtime_id/audit_sequence`、access、target、association、producer/contention 字段 |

`record_type/event_type/transport/success_evidence` 必须按 variant 交叉校验：TCP 固定
`access/tcp_target_success/tcp/tcp_bidirectional_payload`，UDP 固定
`access/udp_target_success/udp/udp_send_ok`，三个 diagnostic 固定 `record_type=diagnostic` 且不得出现
transport/success_evidence。nullable decimal 字段非 null 时仍按无前导零十进制字符串校验；所有时间
范围必须满足 first ≤ last。

## 7. ssserver producer 行为

### 7.1 不阻断原则

relay task 只能做以下有界操作：

1. 读取已构造的 `Arc` 元数据；
2. 取得一个立即返回的 observation guard；
3. UDP 成功后对一个审计 shard 执行一次 `try_lock()` 和有界 cache 操作；
4. 仅在需要 access event 时分配 audit sequence；
5. 构造不超过硬上限的 event draft；
6. 向有界 queue 执行立即返回的 `try_emit()`。

relay task 不得：

- 等待 auditd、ACK、重连、序列化、fsync 或 shutdown drain；
- 因 queue 满、auditd 离线、磁盘满、协议错误或 HMAC 错误终止流量；
- 把审计错误转换为 Shadowsocks/ACL 错误；
- 为审计创建每连接 background task。

读取墙钟早于 Unix epoch、毫秒转换溢出、target 规范化以外的 event draft 构造失败，以及 supervisor
序列化 access draft 失败，统一按 `encode_error` 放弃该 access event。UDP 仍按“成功发送后无论 enqueue
结果都刷新 cooldown”的规则更新窗口；所有这些错误只更新 gap accumulator、health 和限频 journald，
不能返回到 relay 数据路径。

### 7.2 queue 与丢失

- `queue_capacity` 只限制尚未发送的 queue，默认且最多 4096；AuditClient 另有固定 256 条 in-flight
  硬上限，因此总内存中最多存在 4352 条待处理事件；
- queue 固定使用 `crossbeam_queue::ArrayQueue<EventDraft>`；`try_emit()` 调用 `force_push()` 后用
  `tokio::sync::Notify::notify_one()` 唤醒 consumer，全程无 await。`force_push()` 返回的旧 draft 就是
  本次被淘汰的 access event，必须按 `queue_overflow` 和其 sequence 更新 atomic gap accumulator；
  consumer 每次注册 notified future 前后都检查 queue，避免 lost wakeup；
- queue 由单一 AuditSupervisor task 消费；该 task 在自己的外层状态中持有最多 256 条已经序列化的
  `VecDeque<SerializedEvent>` in-flight；每项除原始 JSON bytes/event ID 外还保存 access variant 或原始
  diagnostic accumulator snapshot，供永久 NACK 时采取非递归动作。连接 session 只能可变借用它，不能
  取得所有权；
- 未发送 queue 满时淘汰最老未发送事件并插入最新事件；已经发送、尚未 ACK 的 in-flight 事件不得
  淘汰，避免把可能已经 stored 的事件误报为确定丢失；
- in-flight 在连接 session 返回和重连后仍由 supervisor 保留，并优先以原 bytes 重放；收到合法 ACK 或
  非 retryable NACK 后释放槽位，再从保留最新数据的 queue 取值；
- AuditClient 只把确实从 producer 生命周期中放弃的 event 汇总为 `queue_overflow`、`encode_error` 或
  `permanent_nack`；其中 dropped count 表示不再保证送达的 event 数，不等于已经证明 controller 缺失的
  事件数；不同 reason 以及不同 `permanent_nack_code` 必须分别聚合；
- auditd 不可用、连接/ACK 超时、retryable NACK、未知或非法 ACK/NACK 只触发保留原始 in-flight bytes、
  重连、health counter 和限频 journald，不得单独生成 gap，因为此时 event 尚未被 producer 放弃；
- 收到合法的非 retryable event NACK 时才释放对应 in-flight：access event 累计 `permanent_nack` gap，
  producer gap 或 UDP contention diagnostic 则把其原始 snapshot 合并回原 accumulator，不新增
  `permanent_nack` bucket；
- `sequence_exhausted` 只进入 health counter 和限频 journald，因为已经无法为 gap 分配 sequence；
- pending producer-gap buckets 和 UDP contention accumulator 都位于 queue 外。in-flight 有空位时，
  supervisor 先原子 snapshot 一个非空 accumulator、给诊断分配新 sequence、直接强类型序列化并插入
  in-flight，然后才消费 access queue；两类 producer diagnostic 都不调用 `force_push()`；
- 每个 `(reason, permanent_nack_code)` 同时最多存在一个 producer gap snapshot/in-flight；当前最多 5 个
  bucket（queue、encode 和 3 个永久 NACK code），UDP contention 同时最多一个 snapshot/in-flight；因此
  256 个 in-flight 槽中至少 250 个可供 access。同一 accumulator 的下一次 snapshot 必须等待上一诊断
  ACK 或永久失败处理完（期间新计数继续原子累加），禁止诊断正反馈饿死 access queue；
- 任一 producer diagnostic 构造/序列化失败或收到合法永久 NACK 时，把 snapshot 的原始计数和边界合并
  回原 accumulator，增加 health counter 并最多每 60 秒重试一次；本轮允许继续处理 access，禁止 tight
  loop 或递归 gap；
- journald 只记录聚合计数、时间范围和错误类别，不记录 identity、target、event body 或密钥；
- 相同错误日志最多每 60 秒一条。

producer diagnostics 不是访问事件，字段集合严格以第 6.5 节为准；`producer_gap` 不要求精确列出每个
被丢弃的 event ID，`udp_window_contention` 也不得声称跳过的数据报必然对应缺失 access event。

### 7.3 AuditSupervisor 与 AuditClient session

- 单一长寿命 AuditSupervisor task 持有 queue consumer、atomic diagnostic counters 和最多 256 条
  in-flight 原始 JSON bytes；它循环调用返回 `Result` 的 AuditClient connection session，session 负责
  UDS、hello、ACK/NACK 和一次连接内的读写，不能拥有或在返回时 drop in-flight；
- 强类型序列化发生在 supervisor 从 queue 取出 draft 后、放入 in-flight 前；序列化失败累计
  `encode_error`，不得在 relay task 序列化；
- 首次重连等待 100 ms，指数退避并加入 0–20% 正抖动，但总等待时间硬顶 5 秒（含 jitter）；成功收到
  任一合法 ACK 后立即重置；非 retryable hello NACK 使用精确固定 5 秒重试；
- 每次发送和读取 ACK 超时 3 秒，不短于第 8.1 节 auditd 响应 2 秒写截止；超时只触发后台重连；
- ACK 丢失时以完全相同的 event ID 和 JSON bytes 重试；
- 256 条 in-flight 在重连时优先重试；未发送 queue 独立使用淘汰最老策略保留最新事件；
- auditd 在 ssserver 启动时不可用，ssserver 仍正常监听并代理；
- auditd 运行中断开，已有和新建代理流量均不受影响；
- 必须把 server 启动 API 固定为等价于
  `build_server(config) -> ServerRuntime { run, data_shutdown, audit_shutdown }`：构建阶段创建唯一 emitter、
  AuditSupervisor 和两个 shutdown handle，根 CLI `src/service/server.rs` pin `run` 后再等待 signal；
- SIGTERM 顺序固定为：`data_shutdown.stop_accepting()`；
  `audit_shutdown.close_emitter()`；在 Tokio runtime 仍存活时 `timeout(2s, audit_shutdown.drain())`；随后
  终止其余 server/relay tasks 并 await/join 全部 JoinHandle；join 完成后才原子 swap
  `shutdown_skipped_observations` 并最多写一条 final journald，最后停止 AuditSupervisor 并退出。drain 只尝试
  清空已有 queue/in-flight，到期立即 drop 内存状态，不能无限等待；
- AuditSupervisor 不得加入现有“任一 child task 返回就终止 ssserver”的 fatal `supervise_tasks` 集合；
  AuditClient connection session 的错误全部在同一 AuditSupervisor 内处理；
- SIGKILL 或主机断电允许丢失内存 queue 中所有事件。

AuditSupervisor 必须监督 AuditClient session 的 `Result` 返回：返回 `Err` 或没有收到 shutdown 却意外
返回 `Ok` 时，只按相同 100 ms–5 秒退避重建 connection session，不得主动退出或重启 ssserver。queue、
in-flight 和 atomic diagnostic counters 的所有权必须位于 connection session 之外，保证 session 重建后
仍能重放原 bytes并报告缺口；supervisor 自身检测到异常至少进入限频 journald。

生产 release profile 使用 `panic = "abort"`，因此 panic 不可被 launcher 捕获，也不能承诺“只重建
审计 task”：任意 panic 都会终止进程并由 systemd 重启整个 ssserver。审计路径必须设计为 panic-free，
不得对运行时数据使用 `unwrap()`、`expect()`、越界索引或依赖 debug assertion；锁中毒、序列化、协议和
时钟错误必须转换为有界 `Result`/counter。审计代码 panic 属于阻断数据面的发布缺陷，而不是允许的
auditd 运行故障，必须通过单元测试、故障注入、fuzz 和 canary 阶段拦截并修复。

## 8. ssserver 到 auditd 的 ingest 协议

### 8.1 transport 与 framing

- Linux Unix stream socket；
- 每帧为 4-byte big-endian unsigned length 加 JSON payload；
- request 和 response 使用相同 framing；
- 单帧 payload 最大 8192 bytes；长度 0、超长、partial frame 超时或尾随 JSON 数据均断开；
- 长连接第一帧必须是 hello；hello 前的 event 永久拒绝；
- auditd 最多同时接受 4 个 ingest 连接，但最多一个完成 hello；
- hello 必须在 accept 后 2 秒内完整读完；hello ACK 后允许连接在 frame 边界无限 idle，不对等待下一帧
  第一个 byte 设置 read timeout；一旦读到 length prefix 的第一个 byte，其余 3-byte length 和完整 payload
  必须在同一个 2 秒 deadline 内读完；所有 response 必须在 2 秒内完整写完；
- partial-frame 或 write timeout 只关闭审计连接，producer 保留 in-flight 并正常重连；frame 边界 idle
  不得占用“第二 producer”身份之外的额外资源；
- v1 不使用压缩、二进制 event、JSON batch 或 socket ancillary payload。

### 8.2 hello

```json
{
  "protocol_version": 1,
  "frame_type": "hello",
  "node_id": "node-example-01",
  "runtime_id": "0123456789abcdef0123456789abcdef"
}
```

auditd 必须同时验证：

- `SO_PEERCRED.uid` 等于配置解析后的 `producer_user` UID；
- hello node ID 等于 auditd 配置 node ID；
- runtime ID 格式合法；
- 当前没有另一个已完成 hello 的活跃 producer。

第二个 producer 返回 `producer_busy` 后断开。旧连接已经 EOF 时允许新 runtime 立即连接。

hello ACK：

```json
{
  "protocol_version": 1,
  "frame_type": "hello_ack",
  "status": "ready"
}
```

hello 拒绝使用不含 event ID 的独立 frame：

```json
{
  "protocol_version": 1,
  "frame_type": "hello_nack",
  "error_code": "producer_busy",
  "retryable": true
}
```

hello error code 固定为 `unsupported_version`、`unauthorized_peer`、`invalid_hello`、
`node_mismatch` 或 `producer_busy`。只有完整、合法 JSON hello 才返回 hello_nack；非法 framing、超长
frame 或无法解析的 JSON 直接关闭连接，不尝试构造响应。

`producer_busy` 的 retryable 固定为 true；其余 hello error 固定为 false。所有 hello_nack 都立即关闭当前
审计连接且保留 queue/in-flight：`producer_busy` 使用正常 100 ms–5 秒指数退避；false 类把 producer
health 置为 sticky degraded、写限频 journald，并固定每 5 秒重试 hello，直到收到合法 hello ACK 或进程
退出。false 不得使 ssserver 退出，也不得丢弃事件。

### 8.3 event ACK/NACK

producer 只允许发送完整 access event、`producer_gap` 或 `udp_window_contention`；`spool_gap` 只能由
auditd 内部生成。auditd
必须校验 event 的 node/runtime 与 hello 完全一致，再在强类型解析、写入 open segment并完成本组
`fdatasync()` 后返回：

- `event.node_id == hello.node_id`；
- `event.runtime_id == hello.runtime_id`；
- `event.event_id == event.runtime_id + ":" + event.audit_sequence`；
- audit sequence 是合法 1–`u64::MAX` 十进制字符串；允许乱序、重复和缺号；
- access 与两类 producer diagnostic 分别满足第 6 节的 variant 必填、nullable 和禁止字段。

```json
{
  "protocol_version": 1,
  "frame_type": "ack",
  "event_id": "0123456789abcdef0123456789abcdef:42",
  "status": "stored",
  "spool_epoch": "89abcdef0123456789abcdef01234567",
  "spool_sequence": "1024"
}
```

NACK：

```json
{
  "protocol_version": 1,
  "frame_type": "event_nack",
  "event_id": "0123456789abcdef0123456789abcdef:42",
  "error_code": "invalid_schema",
  "retryable": false
}
```

固定错误码：

| code | retryable | 行为 |
| --- | --- | --- |
| `invalid_schema` | false | 丢弃该 event，累计 `permanent_nack/invalid_schema` |
| `event_id_conflict` | false | 相同 event ID 携带不同 payload，累计 `permanent_nack/event_id_conflict`，丢弃并断开 |
| `runtime_mismatch` | false | event node/runtime 与 hello 不同，累计 `permanent_nack/runtime_mismatch`，丢弃并断开 |
| `storage_unavailable` | true | 保留对应 in-flight 原始 bytes 并重连；只允许未发送 queue 按既定容量策略淘汰 |
| `internal_error` | true | 保留原始 bytes，断开并退避 |

event_nack 只在 event ID 已成功解析时返回。event ID 缺失、非法 JSON、0-length 或超长 frame 均直接
断开。producer 收到 unknown error code、错误 event ID 或不符合 schema 的 ACK/NACK 时增加本机
`invalid_ack` health counter、保留全部 in-flight 原始 bytes并重连，不生成 producer gap。收到任何合法
retryable event_nack 也保留全部 in-flight 并断开重连；重放时仍以原顺序和原 bytes 发送。

同一 daemon lifetime 内，auditd 维护最近 65536 个 `event_id → payload SHA-256 + ACK` 的 LRU：

- 相同 ID、相同 payload 返回原 ACK，不重复写入；
- 相同 ID、不同 payload 返回 `event_id_conflict` 并断开；
- LRU 淘汰或 auditd 重启后，允许同一 event ID 被再次写入并分配新的 spool sequence；auditd 不做
  持久去重，controller 必须按 event ID 去重。

ACK 只表示节点日志已同步到本机文件，不是允许代理流量的凭证。

## 9. auditd spool

### 9.1 文件布局

```text
/var/lib/shadowsocks-audit/
  state.json
  tombstones.json
  open/
    current.ndjson
  sealed/
    <epoch>-<first>-<last>-<batch>/
      segment.ndjson
      meta.json
  acked/
    <acked-unix>-<epoch>-<first>-<last>-<batch>/
      segment.ndjson
      meta.json
  quarantine/
```

目录 `0700 shadowsocks-audit:shadowsocks-audit`，文件 `0600`。不得把 spool、HMAC key 或含真实身份/目标的
生产事件样本提交 Git；本文件中的保留地址示例不受此限制。

### 9.2 NDJSON record

每行是独立 JSON object，末尾必须有 LF：

```json
{
  "spool_schema_version": 1,
  "spool_epoch": "89abcdef0123456789abcdef01234567",
  "spool_sequence": "1024",
  "received_at_unix_ms": "1787587200100",
  "event_payload_sha256": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
  "event": {"schema_version": 1, "record_type": "access"}
}
```

真实 `event` 必须是原始强类型事件的完整 object。producer event 的 `event_payload_sha256` 覆盖 ingest
frame 内原始 event JSON bytes；auditd 内部诊断覆盖其确定性强类型 JSON bytes。auditd 不修改 identity、
target 或事件时间。

`shadowsocks-audit-protocol` 必须提供唯一的 compact deterministic serializer（固定 struct 字段顺序、无非必要
空白、末尾无 LF）；producer 只能发送该输出。auditd 强类型解析后重新生成 canonical bytes 并要求与
ingest raw bytes 逐字相等，同时拒绝 raw CR/LF，再用 `serde_json::value::RawValue` 或等价 raw embedding
把同一 bytes 逐字放入 wrapper 的 `event` value，禁止 parse 后普通 reserialize。collector 必须从 wrapper
保留的 raw event value 复算 `event_payload_sha256`；协议 crate 与 mock collector 对 escaping、Unicode、
nullable 字段和所有 variant 提供 golden vectors。

### 9.3 sequence、segment 与同步

- 首次初始化生成随机 128-bit `spool_epoch`，`spool_sequence` 从 1 开始；
- 每次成功接受 record 分配严格递增 spool sequence；不回绕；
- `state.json` 使用严格 schema，字段固定为 `state_schema_version=1`、`spool_epoch`、
  `next_spool_sequence` 和 `pending_state_reset`；正常时 pending 为 null，reset 时为包含固定
  `gap_event_id`、occurred time、nullable lost epoch/sequence 范围的 object。文件通过同目录 temp file 的
  write、`fdatasync`、atomic rename 和目录 `fsync` 更新；
- 当前 producer、ingest 和 spool 采用 lock-step 逐条提交：每条 record 依次完成 `write_all`、open
  file `fdatasync`、把 `next_spool_sequence` 原子持久化到 `state.json` 后才 ACK；任一步失败均不 ACK。
  `group_commit_max_events` 与 `group_commit_max_delay_ms` 不属于配置 schema。未来若引入批量提交，
  必须先升版并同时改造 producer、ingest 和 spool 三侧；不得以未生效的配置字段冒充 durability；
- append 前先序列化完整 wrapper 加 LF；若非空 open 加上该 record 将超过配置的 `segment_max_bytes`
  （默认 4194304 bytes），则先 seal，再写入新 open；等于上限允许；
- 非空 open segment 达到配置的 `segment_max_bytes` 或年龄达到 `segment_max_age_seconds`（默认 60
  秒）时封口，以先到者为准；空 segment 永不 seal/export；
- 单条 event 不得跨 segment；
- seal 顺序固定为：在 sealed 同级创建临时 batch 目录；同步并关闭 open file；把
  `open/current.ndjson` rename 为临时目录内 `segment.ndjson` 后同时 `fsync(open/)` 和临时目录；计算 raw
  NDJSON SHA-256；写入并同步 `meta.json`；再次同步临时目录；把整个临时目录原子 rename 为最终 batch
  目录并 `fsync(sealed/)`；最后创建、同步新 `open/current.ndjson` 并 `fsync(open/)`。不得声称两个独立
  文件能够原子 rename；
- `batch_id` 是 seal 时生成的随机 128-bit ID；一个 sealed segment 就是一个 export batch；
- ACK 通过跨目录原子 rename 整个 batch 目录进入 `acked/`，随后必须同时 `fsync(sealed/)` 和
  `fsync(acked/)`；完成第 10.1 节 receipt durability barrier 后才返回 200；
- meta 使用下列完整 schema，未知字段拒绝，所有 `u64` 使用十进制字符串：

```json
{
  "meta_schema_version": 1,
  "node_id": "node-example-01",
  "spool_epoch": "89abcdef0123456789abcdef01234567",
  "batch_id": "0123456789abcdef0123456789abcdef",
  "first_spool_sequence": "1",
  "last_spool_sequence": "1000",
  "event_count": "1000",
  "raw_bytes": "4190000",
  "body_sha256": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
  "first_received_at_unix_ms": "1787587140000",
  "last_received_at_unix_ms": "1787587200000"
}
```

- `body_sha256` 精确覆盖 `segment.ndjson` 从第一个 byte 到最后一个 LF，不包含 meta 或 filename；
- SHA-256 只用于传输错误和 ACK 绑定，不得描述为防篡改签名。

### 9.4 crash recovery

- 已删除的历史 segment 不再能证明 epoch high-water mark，因此不得只从现存文件最大值重建并沿用旧
  epoch。只有 `state.json` schema 完整、epoch 格式合法、`next_spool_sequence`
  大于同 epoch 所有现存 record sequence时，才允许沿用该 epoch。空 `open/current.ndjson` 不需要内含
  epoch；非空 open 的每条 record epoch 必须等于 state epoch 且最后 sequence 小于 next；
- `state.json` 缺失、非法、next 小于等于扫描最大值，或无法证明其属于当前 open epoch 时，必须封口或
  quarantine 可恢复的旧 open，并原子创建新的随机 epoch、sequence 1 和非空 `pending_state_reset`；新
  epoch 第一个可写 record 必须使用 pending 中固定 ID 的 `state_reset` spool gap，旧 epoch 和无法确定的
  sequence 范围允许为 null；该 gap 与 next sequence durable 后才把 pending 原子清为 null；group writer
  在 record `fdatasync` 与 `state.json` 持久化之间崩溃也会落入该分支，此时换 epoch 并生成
  `state_reset` gap 是有意的保守行为，不代表确认丢失，无法确定的 lost 字段必须为 null，controller
  不得把单独的 `state_reset` 等同于确认丢失；
- 启动发现 pending 非空时先按固定 gap ID 扫描：若 gap 已 durable，则把 next 提升到同 epoch 扫描最大值
  加一并清 pending；若不存在则用 state 中同一 ID 补写后再清。仅这个可验证的 pending crash window 允许
  修复 `next <= scanned_max` 而不再次换 epoch；
- 启动时扫描未完成的临时 batch 目录：两文件和 meta digest 完整则完成 rename，否则移入 quarantine
  并生成 gap；
- open 文件只有最后一行不完整时，允许截断该行并生成 `spool_gap`；
- 中间 JSON 行损坏、sequence 倒退或 meta 与文件不一致时，把相关 segment 移入 quarantine；
- 任何 open/sealed/acked 与 quarantine 之间的 rename 都必须在 rename 后同步 source 和 target 两侧目录；
- quarantine 后生成新的 epoch 并继续接收最新日志，不得要求 ssserver 停止代理；
- state 丢失或 epoch 改变是 degraded 健康告警，不需要人工批准才能继续，但绝不能在同一 epoch 复用
  `(spool_epoch, spool_sequence)`；
- recovery、quarantine 和新 epoch 不能伪装为无数据丢失。

### 9.5 容量和循环覆盖

容量计算包括 open、sealed、acked、quarantine、state、tombstone ledger 和临时文件。

触发条件：总量即将超过配置的 `max_spool_bytes`（默认 5368709120 bytes），或文件系统可用空间低于
配置的 `min_free_bytes`（默认 1073741824 bytes）。

清理顺序：

1. 删除超过 86400 秒（24 小时，固定）的 acked segment；
2. 若仍超限，提前删除最老 acked segment；
3. 删除最老 quarantine batch；删除前必须按下文同步 `quarantine_pending`，不得只在内存中“安排” gap；
4. 删除最老 sealed、尚未 ACK 的 batch；删除前必须先把 batch ID、digest、epoch、sequence 范围、
   event count、bytes、eviction reason 和时间同步写入有界 tombstone ledger 的 pending entry；
5. 不删除正在写的 open segment；必要时先 seal；
6. 若仍无法写入，不分配 spool sequence、不接受或丢弃 producer record；返回 retryable
   `storage_unavailable`，累计 `storage_rejected_attempts` health counter。producer 保留 in-flight 并重试，
   因而 auditd 不得为拒绝尝试生成 loss gap。

删除未 ACK segment 后必须在下一可写 segment 中写入 `spool_gap`，记录被删除的 batch、sequence
范围、event count、bytes、原因和时间。每次删除目录后必须同步其 parent directory；循环覆盖永远不影响
ssserver 流量。

`tombstones.json` 是未知字段拒绝、顶层仅含 `tombstone_schema_version=1` 和最多 4096 项 `entries` 的
单一严格 JSON document，使用与 `state.json` 相同的 temp file + `fdatasync` + rename + directory
`fsync` 更新。每项用 `entry_type` 判别，u64 仍为规范十进制 string，共四种严格 variant：

- `acked_receipt`：`entry_type/batch_id/body_sha256/recorded_at_unix_ms/status=acked`；
- `eviction_pending`：acked 的 batch/digest/time 公共字段，加 `entry_type=eviction_pending`、
  `status=evicted`、预先生成且固定的 `gap_event_id`、reason、spool epoch、first/last sequence、event count、
  raw bytes 和 `gap_persisted=false`；
- `evicted_receipt`：gap 已同步后只保留 acked 的公共字段、`entry_type=evicted_receipt`、
  `status=evicted` 和 `gap_persisted=true`；
- `quarantine_pending`：`entry_type=quarantine_pending`、预先生成且固定的 `gap_event_id`、quarantine 相对 basename、
  `reason=quarantine_eviction`、recorded time，以及能够从损坏对象可靠取得的 nullable batch/digest/epoch/
  sequence/count/bytes；不得保存任意绝对路径。

删除未 ACK batch 的事务顺序固定为：先同步 `eviction_pending`，再删除 batch 并同步原目录，再把对应
`spool_gap` 写入 durable group，最后把 pending entry 压缩成 `evicted` receipt。进程崩溃后，若 pending
对应 batch 仍完整存在，则撤销 pending 或重新执行删除；只有 batch 已不存在时才根据 pending 补写 gap；
恢复必须先扫描固定 `gap_event_id`：已存在于 durable spool 时只完成 pending→evicted，未存在时才用同一
ID 补写，禁止生成新 ID 造成重复 gap。状态无法判定时 quarantine 并置 degraded。在 pending entry 成功
同步前绝不能删除 batch。

quarantine eviction 使用同一事务模型：先同步 `quarantine_pending`，再删除 quarantine entry 并同步
`quarantine/`，再用固定 gap ID 写入 durable `quarantine_eviction` spool gap，最后删除 pending。恢复时
entry 仍存在则撤销或重做删除；entry 已不存在则扫描固定 gap ID 后幂等补写。pending 无法同步时不得
删除 quarantine entry。

ledger 先删除超过 7 天的 receipt；达到 4096 时再淘汰最老 `acked/evicted` receipt，淘汰后相关 ACK
返回 404。两类 pending 在 gap durable 前不得因时间或容量淘汰。若 ledger 已满且只有 pending entry，
或 ledger 更新无法同步，auditd 不得删除新的未 ACK batch，只能停止接收新 record、累计
`storage_rejected_attempts` health counter，ssserver 仍继续代理。

## 10. auditd export API

### 10.1 transport 与路由

auditd 在 `/run/shadowsocks-audit/export/export.sock` 提供严格 HTTP/1.1：

| 请求 | 成功响应 | 含义 |
| --- | --- | --- |
| `GET /v1/audit/healthz` | `200` 或 `503` JSON | auditd、ingest、spool、丢失计数和最老 batch 状态 |
| `POST /v1/audit/lease` | `200` NDJSON 或 `204` | 返回最老 sealed、未确认 segment |
| `POST /v1/audit/ack` | `200` JSON | 按 batch ID 和 segment digest 幂等确认 |

要求：

- request-target 只接受 origin-form 的精确 path 和 method；拒绝 absolute-form、authority-form、asterisk-form、
  query、重定向、百分号编码变体和尾斜杠；
- HTTP/1.1 必须恰好包含一个非空、语法合法且不超过 255 bytes 的 `Host`；拒绝控制字符、空白和重复
  Host。Host 不进入 HMAC，也不得参与 auditd 路由或节点选择；
- GET health 不得有 body；允许省略 `Content-Length` 或使用精确 `Content-Length: 0`，且不带
  `Content-Type`；两个 POST 必须有准确 `Content-Length` 并使用精确
  `Content-Type: application/json`；
- security headers 的逻辑名为 `Host`、`Content-Length`、`Content-Type`、`Authorization` 和全部
  `X-Shadowsocks-Audit-*`。header name 按 HTTP ASCII case-insensitive 识别，兼容常见 HTTP 库把 `SHA256/MAC`
  输出为 `Sha256/Mac`；不得要求示例中的 casing。对 case-fold 后的逻辑名拒绝任何重复。每行 wire grammar
  必须是合法 token name、冒号、恰一个 ASCII SP、非空 field-value、CRLF；该分隔 SP 不属于 field-value。
  拒绝 HTAB、额外 leading/trailing OWS、obs-fold 和控制字符；
- 所有请求拒绝 `Transfer-Encoding` 与 `Content-Encoding`；非 security header 不得影响认证、body
  framing、路由或节点选择，仍受 header 数量/总量和控制字符限制；
- request-line 最大 1024 bytes、header 总量最大 16 KiB、header 最多 32 个、POST body 最大 4096
  bytes；export 最多 4 个并发连接；完整请求读取和响应写入 timeout 均为 5 秒；
- 每个连接只处理一个请求并返回 `Connection: close`；除 204 外所有响应必须给出准确
  `Content-Length`；所有响应都拒绝/不使用 Transfer-Encoding 与 Content-Encoding，并携带
  `Cache-Control: no-store`；
- lease 请求 body 固定为 raw bytes `{"schema_version":1}`；ACK 请求也使用 compact UTF-8 canonical raw
  bytes，字段顺序固定为 `schema_version,batch_id,body_sha256`，冒号和逗号前后均无空白，无 BOM 或末尾
  LF。lease/ACK 都必须是单个严格 JSON object，拒绝重复/未知字段、字段乱序、token 间空白和尾随数据。
  ACK 的 `schema_version` 是 integer 1，`batch_id` 是 32 位小写 hex，`body_sha256` 是 64 位小写 hex；
- 同一未 ACK batch 重复 lease 返回相同 raw NDJSON 和 metadata；
- v1 同时只存在一个逻辑 leased batch；
- 环形清理可以淘汰 leased batch，之后 ACK 返回 `410 batch_evicted`；
- 200 lease body 不超过配置的 `export_max_response_bytes`（默认 8 MiB）；正常 segment 不超过配置的
  `segment_max_bytes`（硬上限 4 MiB）；
- 不在任何 HTTP intermediary access log、错误响应或 health 中输出 identity、target 或 event body。

204 lease 响应按 HTTP/1.1 明确省略 Content-Length、Content-Type、Transfer-Encoding 和
Content-Encoding，body 为零 bytes，但仍携带 node、SHA-256(empty) 的 response body digest 和 response
MAC；batch/epoch/sequence/count headers 省略并在 canonical response 中使用空值。

ACK 首次成功和幂等成功均返回完全相同的 raw JSON body（无末尾 LF）：

```json
{"schema_version":1,"status":"acked"}
```

200 lease 响应必须包含：

```text
Content-Type: application/x-ndjson
X-Shadowsocks-Audit-Schema: 1
X-Shadowsocks-Audit-Node: node-example-01
X-Shadowsocks-Audit-Batch-Id: <32 lowercase hex>
X-Shadowsocks-Audit-Spool-Epoch: <32 lowercase hex>
X-Shadowsocks-Audit-First-Sequence: <decimal>
X-Shadowsocks-Audit-Last-Sequence: <decimal>
X-Shadowsocks-Audit-Event-Count: <decimal>
X-Shadowsocks-Audit-Body-SHA256: <64 lowercase hex>
```

200 lease 的 `X-Shadowsocks-Audit-Schema` 必须是规范十进制 `1`；
`X-Shadowsocks-Audit-Body-SHA256` 必须逐字等于通用响应 header
`X-Shadowsocks-Audit-Response-SHA256`，并等于 collector 对实际 raw body 计算的 digest。

ACK body：

```json
{"schema_version":1,"batch_id":"89abcdef0123456789abcdef01234567","body_sha256":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}
```

ACK 行为：

- batch 和 digest 匹配：原子移动到 acked、同步 sealed/acked 两侧目录、原子写入并同步 `acked` receipt，
  然后才返回 200；
- 已 ACK 的相同 batch/digest：幂等返回 200；
- batch 未知：404 `unknown_batch`；
- 已被循环覆盖：410 `batch_evicted`；
- digest 不同：409 `digest_mismatch`；
- ACK 成功不得立即删除文件，正常保留 86400 秒（24 小时，固定）。

若 rename 已完成但 receipt barrier 失败，不得返回 200；重试或 crash recovery 必须从仍存在的 acked
batch/meta 重建 receipt 后再幂等成功。任何因 5 GiB/1 GiB 水位提前删除的 acked batch，在删除和同步
`acked/` 之前必须已有 durable receipt；否则不能删除，只能进入 storage unavailable 路径。

auditd 按第 9.5 节在独立 `tombstones.json` 中保留最多 4096 个、最长 7 天的 ACK/eviction receipt。
receipt 存续期间重复 ACK 分别返回幂等 200 或 410；receipt 淘汰后返回 404。已 ACK receipt 可从仍保留
的 acked batch 重建；已删除 batch 的 evicted receipt 不可重建，ledger 损坏时返回 404 并置 degraded，
但不得阻止 auditd 或 ssserver 继续服务。任何尚未完成 gap 的 `eviction_pending` 都按第 9.5 节恢复。

health response 固定为：

```json
{
  "schema_version": 1,
  "node_id": "node-example-01",
  "status": "ok",
  "producer_connected": true,
  "producer_runtime_id": "0123456789abcdef0123456789abcdef",
  "last_ingest_at_unix_ms": "1787587200100",
  "spool_epoch": "89abcdef0123456789abcdef01234567",
  "spool_bytes": "1048576",
  "max_spool_bytes": "5368709120",
  "sealed_batches": "2",
  "oldest_unacked_at_unix_ms": "1787587140000",
  "stored_records": "1000",
  "storage_rejected_attempts": "0",
  "evicted_unacked_records": "0"
}
```

- `status` 只接受 `ok` 或 `degraded`；存储不可写、producer 断开超过 5 秒、recovery/quarantine
  未处置或最新 gap 所在 segment 尚未被 controller ACK 时，返回 `degraded` 和 HTTP 503；gap 被
  ACK 后可以恢复 `ok`，但累计计数不得清零；
- producer 尚未连接时，`producer_runtime_id` 和 `last_ingest_at_unix_ms` 为 null；没有 sealed/leased
  unacked batch 时，`oldest_unacked_at_unix_ms` 为 null；health 不输出身份或目标；
- 所有计数饱和在 `u64::MAX` 并把 status 置为 degraded，不得回绕。

除已经通过 HMAC 认证的 `/v1/audit/healthz` degraded 503 外，所有非 2xx response 使用固定 error body，
不能回显请求内容。degraded 503 仍返回上面的完整 health object，并按普通 health response 携带 body digest
和 response MAC：

```json
{"schema_version":1,"error_code":"digest_mismatch"}
```

HTTP error code 固定为 `unauthorized`、`replayed_nonce`、`rate_limited`、`invalid_request`、
`unknown_batch`、`batch_evicted`、`digest_mismatch` 或 `internal_error`。

状态码固定映射为：`invalid_request=400`、`unauthorized=401`、`unknown_batch=404`、
`replayed_nonce/digest_mismatch=409`、`batch_evicted=410`、`rate_limited=429`、
`internal_error=500`。JSON success/error response 使用精确 `Content-Type: application/json`；204 不带
Content-Type 和 body。

### 10.2 端到端 HMAC

外层传输安全通常终止在 HTTP intermediary，不能单独提供 auditd 所需的应用级调用者身份，因此三个 API
全部必须使用每节点独立 HMAC。仅有 mTLS 或 bearer token 不合格；是否部署额外传输保护属于外部系统责任。

请求 headers：

```text
X-Shadowsocks-Audit-Node: <node_id>
X-Shadowsocks-Audit-Timestamp: <unix seconds>
X-Shadowsocks-Audit-Nonce: <32 lowercase hex>
X-Shadowsocks-Audit-Content-SHA256: <64 lowercase hex>
Authorization: Shadowsocks-Audit-HMAC-SHA256 <64 lowercase hex>
```

canonical request UTF-8 bytes：

```text
SHADOWSOCKS-AUDIT-V1<LF>
<METHOD><LF>
<exact-path><LF>
<node-id><LF>
<timestamp><LF>
<nonce><LF>
<lowercase-body-sha256>
```

`<LF>` 表示单个 `0x0a` byte；字段不得含 CR/LF；最后 digest 后没有末尾 LF 或其他空白。

Authorization 值为 `HMAC-SHA256(node_key, canonical_request)` 的小写十六进制。

验证规则：

- node 必须等于 auditd 配置；
- timestamp 与 auditd 墙钟偏差不超过 300 秒；
- nonce 在同一 node 的 10 分钟、4096 条有界 cache 中不得重复；未过期 nonce 不得为新请求提前
  淘汰，cache 满时新请求返回 429 `rate_limited`；
- timestamp 只接受无符号、无前导零的十进制；nonce 和 digest 只接受固定长度小写十六进制；
- 必须先完成 timestamp、body digest 和 constant-time HMAC 验证，成功后才把 nonce 写入 replay
  cache，防止无 key 请求污染 cache；
- body digest 使用实际原始 body bytes；空 body 使用 SHA-256 空值；
- HMAC 使用 constant-time 比较；
- 认证失败返回 401；nonce 重放返回 409；不得区分 key 是否存在；
- auditd 还必须验证 export UDS `SO_PEERCRED.uid` 等于 `export_peer_user` 解析得到的 UID。

响应 headers：

```text
X-Shadowsocks-Audit-Response-SHA256: <body sha256>
X-Shadowsocks-Audit-Response-MAC: <hmac>
```

response canonical bytes：

```text
SHADOWSOCKS-AUDIT-RESPONSE-V1<LF>
<request-nonce><LF>
<http-status><LF>
<content-type-or-empty><LF>
<audit-schema-or-empty><LF>
<lease-body-sha256-or-empty><LF>
<node-id><LF>
<batch-id-or-empty><LF>
<spool-epoch-or-empty><LF>
<first-sequence-or-empty><LF>
<last-sequence-or-empty><LF>
<event-count-or-empty><LF>
<lowercase-response-body-sha256>
```

响应 canonical bytes 同样不带末尾 LF。所有响应都必须携带 `X-Shadowsocks-Audit-Node`；只有 200 lease 的
audit schema、lease body digest、batch/epoch/sequence/count 有值，其他响应对应字段使用空字符串。
collector 必须从实际响应 header 构造 canonical bytes并先核对两个 body digest 与实际 body，因此 HTTP
intermediary 或其他本机进程不能在不破坏 MAC 的情况下替换 schema、digest 或 batch metadata。

只有请求中 node、nonce 均能够按严格格式解析时，错误响应才携带 response MAC；缺失或格式非法时
可以返回未签名 400/401。collector 对任何未签名响应一律视为采集失败，不解析 body，也不发送 ACK。

collector 必须验证 response MAC 后才解析或 ACK batch。协议文档和测试必须提供固定 key、请求、响应
以及期望 HMAC 的 golden vector。

### 10.3 外部暴露边界

本仓库只实现 export UDS，不规定公网域名、TCP 端口、隧道产品或 controller 拓扑，也不交付这些配置。
外部兼容方若通过 HTTP intermediary 暴露该 UDS，必须满足：

- 每个逻辑 endpoint 只映射一个配置的 `node_id`，不同节点使用不同 HMAC key；
- stats 与 audit 使用不同 UDS、handler 和 key；
- intermediary 只能访问 export socket，不能访问 ingest socket、spool 或 HMAC key；
- 精确保留 method、origin-form path、Authorization、全部 `X-Shadowsocks-Audit-*` headers、raw request/response body、
  status 和 body length；不得解压、压缩、重写 JSON 或合并重复 security header；
- 在转发前拒绝 query、百分号编码路径变体、非预期 method 和超过 4096 bytes 的 request body；
- access/error log 不记录 Authorization、MAC、request/response body、identity 或 target；
- 外层来源限制、mTLS、防火墙和隧道只作为纵深防御，不能替代第 10.2 节 HMAC。

## 11. 本机账号、目录和 systemd

新增：

- system user：`shadowsocks-audit`；
- group：`shadowsocks-audit-ingest`，成员仅 `shadowsocks-audit`、配置中 `producer_user` 解析出的 ssserver 用户；
- group：`shadowsocks-audit-export`，成员仅 `shadowsocks-audit`、配置的 export peer 用户；
- ingest 目录：`0750 shadowsocks-audit:shadowsocks-audit-ingest`；
- export 目录：`0750 shadowsocks-audit:shadowsocks-audit-export`；
- 两个 socket：`0660 shadowsocks-audit:<对应组>`；
- spool：`0700 shadowsocks-audit:shadowsocks-audit`；
- 默认配置目录 `/etc/shadowsocks-audit`，以及通过 `--config`/`export_hmac_key_file` 选择的配置文件和 HMAC
  key 的直接父目录：`0750 root:shadowsocks-audit`，不得与 ssserver 密钥和用户配置共用文件或放宽权限；
- auditd config：`0640 root:shadowsocks-audit`；
- HMAC key：`0600 shadowsocks-audit:shadowsocks-audit`；root 仍可通过系统特权读取。

`producer_user` 对应的 ssserver unit 必须显式加入 `shadowsocks-audit-ingest`，配置的
`export_peer_user`/HTTP intermediary unit 必须显式加入 `shadowsocks-audit-export`。不得复用 stats socket 的
访问组代替两个审计组，也不得让 export peer 进入 ingest 组。

`shadowsocks-auditd.service` 必须：

- `User=shadowsocks-audit`、`Group=shadowsocks-audit`；
- `RestrictAddressFamilies=AF_UNIX`；
- `NoNewPrivileges=true`、`PrivateTmp=true`、`ProtectSystem=strict`、`ProtectHome=true`；
- 只允许写 spool 和两个 runtime socket 目录；
- 不具备读取 ssserver config、iPSK、uPSK、users.json、stats UDS 或网站配置的权限；
- 在 ssserver 前启动，但 ssserver 只使用 `Wants`/`After`，不得使用 `Requires`；
- auditd 失败不得触发 ssserver stop/restart。

ssserver 必须验证 auditd peer UID；仅依靠 socket pathname 或 mode 不够。

## 12. controller 外部合同

本节只定义外部 collector 为正确消费节点协议必须满足的兼容行为，不规定其语言、框架、数据库产品、
表名、采集频率、网络拓扑、业务账号模型或数据保留期，也不要求本功能实现者修改 controller 仓库。

collector 必须：

1. 对每个节点使用独立 key 构造第 10 节 HMAC health/lease 请求；
2. 在解析 body 前验证 response MAC、node、status、Content-Type、长度、signed batch headers 和 raw body
   SHA-256；
3. 强类型逐行解析 NDJSON，验证每个 wrapper 的 epoch 与 signed header 一致、spool sequence 严格连续、
   行数与 event count 一致、首末 sequence 与 headers 一致，并复算 raw event payload hash；
4. durable 保存 batch 接收状态、幂等键、可接受 record、冲突证据和告警状态后才发送 HMAC ACK；任一步
   未完成都不得 ACK；
5. 单节点失败不得阻断其他节点；对 `producer_gap`、`udp_window_contention`、`spool_gap`、sequence 缺号
   和 `batch_evicted` 产生明确告警，且不得把 contention datagram count 冒充 access-loss count。

外部持久层无论如何实现，都必须遵守下列幂等语义：

- `(node_id,event_id)` 相同且 event payload SHA-256 相同是幂等重放；hash 不同是
  `event_payload_conflict`，保留原记录并 durable 隔离 incoming wrapper，禁止覆盖或静默忽略；
- `(node_id,batch_id)` 只有 body digest、spool epoch、first/last sequence 和 count 全部相同时才幂等；任一
  字段不同是 `batch_id_conflict`，整批 durable 隔离；
- `(node_id,spool_epoch,spool_sequence)` 相同且 raw wrapper hash 相同是幂等；hash 不同是
  `spool_sequence_conflict`，保留原记录并 durable 隔离 incoming wrapper；
- 冲突证据和同批可接受 record 必须作为一个不可分割的 durable commit；提交成功后 ACK 当前 raw batch，
  避免 poison batch 永久占据 lease，提交失败则不得 ACK；
- 去重状态的生命周期必须覆盖协议允许的最迟重放窗口。v1 没有最大迟到时间，因此删除去重状态前必须
  先升版并定义超龄 batch 的确定拒绝规则。

`identity_name` 到业务账号的映射完全属于外部系统。无法映射时仍必须保存 access record，不能拒绝整个
batch。若外部系统需要历史归属，应使用 append-only assignment history。runtime 起点不随审计事件、
ingest hello 或 export health 携带，collector 必须另行通过 user-stats exporter 管线按 `runtime_id`
关联获得 `started_at_unix_ms`；以已批准 runtime 的
`runtime_started_at_unix_ms + runtime_monotonic_ms`（checked arithmetic）作为归属时间；runtime 不可信、
起点缺失或该结果与 `occurred_at_unix_ms` 相差超过 300000 ms 时必须标记 clock ambiguous，不得只用
采集时的当前映射覆盖历史身份。

## 13. 安全与隐私

- spool 是敏感明文日志，文件和目录权限是第一版静态保护边界；
- HMAC 解决 export 调用者认证、重放和传输 body 绑定，不提供离线不可篡改证据；
- 取得节点 root 的攻击者可以读取 key、修改 spool、删除尾部或伪造新记录；文档和 UI 不得否认该限制；
- 事件、错误日志、Debug、panic、metrics 和 health 不得包含密码、iPSK、uPSK、订阅 token 或 payload；
- 所有 HTTP intermediary 和 transport 日志都不得记录 request/response body 或 HMAC Authorization；
- 外部查询、导出和权限控制不属于本交付；
- 外部数据保留策略不改变节点按 5 GiB/24 小时执行的独立清理合同。

## 14. 测试规格

### 14.1 配置与 feature

- feature dependency、feature-off fast path 和 Linux-only compile gate；
- feature-off wire parser 仍识别 `user_audit` 并明确报 unsupported-feature，不能静默忽略；
- statistics-only `new()` 不构造 audit metadata，`new_with_audit()` 的 runtime/server/identity typed handle
  在同一 generation 内一致且不调用 snapshot；
- 配置 round-trip、未知字段、所有边界值、symlink parent 和错误 socket owner；
- auditd CLI 默认路径、唯一 `--config` 覆盖、未知/重复/缺值/相对路径参数失败，以及非默认配置父目录
  owner/mode；
- 审计启用前后均不设置/改写顶层 `udp_max_associations`；64 shard、总窗口整除、每 shard/association
  上限的边界和 checked arithmetic；默认 profile `64 × 1024 = 65536` 通过；
- 缺 user-stats、非 EIH、无 users、重复 server/identity、manager 模式拒绝；
- auditd 不存在时 ssserver 仍启动、监听并完成 TCP/UDP 代理；
- audit 配置非法时 ssserver 明确启动失败。

### 14.2 TCP

- 认证失败、ACL/DNS/connect 失败：零访问事件；
- 只有 TCP handshake：零事件；
- 只有上行：零事件；
- 只有目标 banner 下行：零事件；
- 先上行后下行、先下行后上行：在第二个方向首次成功写入后恰好一条；
- 后续任意双向载荷不重复；
- `poll_write`/vectored write 的 0/partial/error、两个方向并发 bit transition 和 exactly-once；
- client/remote reset、TFO first write、普通 connect 和并发用户；
- 远端 `peer_addr()` 失败且随后双向成功时累计一条 `encode_error` gap，代理连接仍工作；
- auditd 离线、ACK 超时、queue 满时流量行为与未启用审计一致。

### 14.3 UDP

- send 失败或 short datagram write：不产生事件、不刷新窗口；
- 首次 send 成功：一条；
- 60 秒内重复成功：零新增；
- 60 秒后继续成功：再一条；
- 相同目标不同 association、不同端口、domain 与 IP 分别建 key；
- domain 多候选解析时记录真正完整发送成功的最终 mapped/unmapped `SocketAddr`，同时保留原始目标；
- 64 shard 分布、每 shard 1024、per-association 256、shard/association oldest eviction 与总窗口 65536；
- shard `try_lock` contention 不等待/不刷新 cooldown/不分配 access sequence，按第 6.5 节饱和语义聚合
  `udp_window_contention`；poison recovery 清 shard 但不拒绝数据报；
- 人为持有一个 shard lock 时，N 个完整成功包产生 0 access sequence、0 producer gap、contention count=N；
  解锁后下一成功包恢复正常 cooldown 判断；
- contention strict schema 拒绝 dropped count/sequence、identity、target、association 和 access 字段；
- association ID churn 始终满足 `count_map.len() <= lru.len()`，不同 server ID 的相同 association/target
  key 相互隔离；
- 现有 NAT association 数超过 256 仍不因审计被淘汰或 abort；
- queue 满仍刷新 cooldown，避免每包重试。

### 14.4 ingest、spool 与 export

- request/response framing、partial frame、0/8192/8193 bytes、重复 JSON key 和尾随数据；
- hello 缺失、错误 node、伪 UID、第二 producer 和 runtime 切换；
- hello 总超时、frame 边界长时间 idle、首 byte 后 partial-frame 2 秒超时；
- retryable hello NACK 指数退避、non-retryable hello NACK sticky degraded/5 秒自愈重试；
- 4096 未发送 queue 与 256 in-flight 分离、ACK 乱序/丢失/迟到、重复 event ID 和不同 payload；
- ArrayQueue 满时恰好淘汰 oldest access，connection session 返回后所有 in-flight 原 bytes仍可重放；
- producer gap 绕过 queue；其构造/encode/permanent NACK 把原计数合并回 accumulator 且不递归/tight loop；
- UDP contention diagnostic 同样绕过 queue、最多一个 in-flight、失败回并自身 accumulator；
- contention ACK 丢失时重放相同 event ID/raw bytes，encode 或永久 NACK 只回并自身 accumulator；
- contention counter 0→1 wakeup、无 access 流量的 60 秒 timer，以及 shutdown final snapshot/journald；
- contention u64 saturation、合法/非法墙钟和 first≤last≤occurred 边界；
- 墙钟和 event build/encode 错误生成 `encode_error` 缺口，UDP cooldown 仍更新；
- AuditClient connection session 返回 `Err` 或意外正常退出时只重建 session，不影响 ssserver；
- SIGTERM cooperative shutdown 在 2 秒内 flush 或按时退出；SIGKILL 允许内存事件丢失；
- close/observation 并发模型检查证明 guard 覆盖 UDP cache/accumulator，drain 冻结 pending diagnostics 后
  才判空；测试强制 relay 在 close 后继续触发 hook，证明 launcher 必须按 terminate→await/join→atomic swap
  顺序取得稳定的 shutdown skipped observations，且仅写一条 final journald；
- 审计路径无 `unwrap()`/`expect()`/越界索引，panic fuzz target 与 release-profile 子进程测试证明
  panic 会 abort，因此不能把 panic 当作 task 内可恢复错误；
- 每条 record 只在 open segment 与 `state.json` 的 durability barrier 均完成后 ACK；
- open 尾部截断、中间损坏、state 丢失/倒退、已删历史 high-water、pending state-reset 各崩溃点、
  quarantine 和新 epoch 不复用 sequence/不重复 gap；
- tombstone 4096 上限、pending 各崩溃点、ledger 满/同步失败不删除 unacked、receipt 淘汰后 ACK 404；
- seal、ACK、quarantine 跨目录 rename 的 source/target fsync，以及 ACK rename 后 receipt 失败的恢复；
- append 前 4 MiB 预判、60 秒 seal、空 segment 不导出、batch 目录原子 rename、meta schema、lease
  重取和 ACK 幂等；
- 错 digest、未知、已覆盖 batch；
- 5 GiB 和 1 GiB 水位、acked 优先删除、未 ACK 循环覆盖和 `spool_gap`；
- HMAC 正确、错误 key、body/header metadata 篡改、path 变体、过期时间、nonce 重放/cache 污染、
  response MAC、GET 无 Content-Length、Host/request-target 变体、重复 header、degraded 503 health object 和
  canonical `Name: SP value`/额外 OWS/HTAB、204 无 Content-Length/Content-Type/TE、HTTP timeout/并发上限；
- mock collector 的持久层覆盖跨时间边界相同 event ID 同 payload 幂等、不同 payload 冲突；
- batch ID/digest/range 冲突、spool key 同 wrapper hash 幂等/不同 hash 冲突、batch 内 epoch/连续性/count；
- event/batch/cursor 冲突证据事务失败不 ACK，成功隔离后 ACK 且不会形成 poison retry；
- assignment event history、runtime monotonic attribution、300 秒边界与 clock ambiguous；
- producer canonical event raw bytes 经 RawValue wrapper 后逐字不变，controller 可复算 event payload hash；
- protocol JSON/NDJSON/HMAC golden vectors 在 Rust 与 mock collector 间逐字一致。

### 14.5 可用性与性能

在相同 host、配置、连接模型和测试数据下比较 feature-off 与 feature-on：

- 长连接吞吐下降不得超过 5%；
- ssserver CPU 增幅不得超过 10%；
- ssserver RSS 增量不得超过 64 MiB；
- auditd RSS 不得超过 128 MiB；
- auditd healthy、离线、慢 ACK、queue 满和 spool 满均分别压测；
- relay hot path 的 `try_emit()` 必须证明无 await、无 socket I/O、无文件 I/O 和有界分配；
- ArrayQueue 在最大生产者并发和持续 full/`force_push()` 下单独 benchmark，验证不会把上述性能门槛转化
  为明显 tail-latency 回退；
- UDP 64 shard 在均匀、单 shard 倾斜和锁争用压测下验证 critical section 不含序列化/queue/socket，
  `WouldBlock` 路径立即返回且不会改变现有 NAT association 生命周期；
- `queue_capacity`、UDP window cache、nonce cache、dedup LRU 和 spool 必须证明有界。

## 15. 构建、发布与回滚合同

### 15.1 构建产物

release 必须同时包含：

```text
ssserver
ssserver.sha256
shadowsocks-auditd
shadowsocks-auditd.sha256
release-manifest.json
release-manifest.sig
```

执行两次独立 Linux x86_64 musl 构建，两个二进制分别满足可复现 SHA。发布验证必须覆盖来源 commit、
补丁 series、toolchain lock、artifact SHA 和 detached signature。

### 15.2 外部集成建议（非本仓库交付）

本文不执行部署，也不授权实现者修改外部 operations/controller 仓库。下游部署方可采用允许短暂中断的
逐节点轮换：

1. controller mock 与真实 collector 先验证协议；
2. 节点先安装用户、组、目录、auditd、HMAC key 和符合第 10.3 节的 HTTP intermediary 路由；
3. auditd 运行但 ssserver 尚未启用 producer，验证 health/空 lease；
4. 临时端口验证 TCP/UDP 成功条件和故障不阻断；
5. 每次只切换一个物理节点，完成 smoke 后按下游变更策略观察；
6. 再切换下一节点，直到所有节点完成；
7. 核对 access、producer gap、UDP window contention/skipped datagrams、spool gap、queue、spool 和
   controller lag。

### 15.3 回滚

- auditd、collector 或审计协议故障：ssserver 继续代理，修复审计组件，不需要回滚数据面；
- 首个 audited ssserver release 自身造成代理功能故障：该节点退出轮换并暂时离线，不恢复无审计旧版；
- 后续 release 只允许回滚到最后一个已经通过本规格验收的 audited release；
- 回滚不得删除 spool、中心明细或诊断缺口；
- 记录故障时间、runtime ID、最后 event/spool sequence、queue drops、UDP skipped datagrams 和 spool gaps。

## 16. 最终验收清单

只有同时满足以下条件，节点侧交付才算完成：

- feature-off 的现有 Shadowsocks 和 user-stats 全量回归通过；
- 两类成功事件的生成时机与本文件逐字一致；
- 合法配置下所有可处理的运行时审计故障均不会拒绝或等待代理流量；审计路径 panic 视为发布缺陷；
- queue、UDP cache、dedup、nonce cache、segment 和 spool 均有硬上限；
- 审计不设置 UDP NAT association hard capacity；shard contention 只漏观察并产生可区分诊断；
- ingest/export schema、framing、HMAC、错误码和 golden vectors 完整；
- UDS 双向 `SO_PEERCRED`、分组权限和 systemd hardening 通过；
- 5 GiB 循环覆盖和 24 小时已确认保留合同通过；
- 不存在 hash chain、审计日志数字签名、防 root 篡改或完整 URL 的错误能力声明；
- `ssserver` 与 `shadowsocks-auditd` 均可复现构建、校验和签名；
- 完整测试矩阵和性能门槛通过；
- 文档、配置样例、mock collector 和运维说明足以让另一位工程师独立对接。

## 17. 代码审计记录（2026-08-28）

> 本节是审计记录，不改变合同条文。所列问题以代码修复为主；标注"需规格决策"的条目必须先按约定
> 对本文件升版。行号基于 `.cache/audit-work-source`（upstream v1.24.0 + 0001/0002/0003 应用树），
> 与 `patches/0003-user-audit.patch` 内容一致。

### 17.1 审计范围与方法

- 对象：`patches/0003-user-audit.patch`（16291 行）、`crates/shadowsocks-audit-protocol`、
  `crates/shadowsocks-auditd`、`shadowsocks-service` 的 user-audit 接线、packaging、tests、scripts、
  docs。
- 方法：对照本文件 v2 逐节静态审查；HMAC golden 值用独立实现重算；补丁↔源码树用
  `patch --dry-run -R --fuzz=0` 全量反向校验（逐字一致）；未执行编译与测试，feature-off/feature-on
  编译矩阵和测试通过情况未在本次验证范围内。

### 17.2 总体结论

核心合同点——事件语义（TCP 双向/UDP 完整发送）、持久化顺序、HMAC、权限模型、配置/feature 合同、
panic-free——落实到位，未发现数据面阻断或 wire 不兼容。但存在 1 个 critical 与 11 个 major，
集中在 shutdown 竞态、session 模型、producer 诊断义务、group commit、崩溃窗口、mock 幂等合同与
测试欠账。建议 17.3/17.4 全部修复后再按第 16 节验收。

### 17.3 Critical

- **C-1 relay task 在首次 poll 前被 abort 泄漏 `active_tasks`，`terminate_and_join` 永久等待。**
  `context.rs:120-152`：`active_tasks` 在 spawn 前递增，`RelayTaskGuard` 却在 task 内部首次 poll
  时才构造（`context.rs:133-137`）；tokio 对未 poll 即 abort 的 task 直接 drop future，guard 从不
  存在，计数永不递减。`terminate_and_join`（`context.rs:166-175`）无超时自旋等待，且经
  `stop_data_tasks`（`server/mod.rs:399-404`）被两条 shutdown 分支（`server/mod.rs:459,477`）
  调用，外层均无超时兜底（2 秒 timeout 只包 `emitter.drain()`）。触发路径：spawn 后
  `!is_accepting` 分支的 abort（`context.rs:144-148`）与 terminate 时对未 poll task 的 abort。
  命中即 ssserver 关机永久挂起，只能靠 systemd SIGKILL 收场。修复：guard 在 spawn 前构造好再
  move 进 task（或计数与 JoinHandle 完成绑定），并给 `terminate_and_join` 加有界超时。
  （§7.3 SIGTERM 顺序）

### 17.4 Major

- **M-1 AuditClient session 每批 flush 完主动断开，下一批重新 hello。** `user_audit.rs:1969-2014`
  清空 pending 即 `Ok(())` 返回并 drop stream；`run_supervisor`（`user_audit.rs:1897-1899`）把该
  Ok 当正常路径。后果：突发流量每 256 条一次 connect+hello；空闲期连接断开使 auditd health 的
  `producer_connected` 在健康节点上超过 5 秒即 flap 为 degraded（§10.1）。违反 §7.3 session
  模型（"意外返回 Ok"应按异常处理）与 §8.1 长连接语义。修复：session 内等待新入队事件或
  shutdown 信号，保持长连接，仅在 shutdown 时正常返回。
- **M-2 feature-on 改变 0 字节 UDP 数据报接收行为。** `udprelay.rs:435-453`：audit build 不再按
  上游 `return None` 早退，空数据报继续进入 association 创建与解密失败路径。注释理由不成立：
  合法 SS UDP 包必含加密地址头，wire 上不可能为 0 字节；空包只会是伪造包或 Windows ICMP 假包。
  与 feature-off 产生数据面行为差异，并可被伪造空包造成 association churn。违反 §1"审计不得
  改变代理行为"。修复：还原上游早退；emit 点的 `!data.is_empty()` 守卫（`udprelay.rs:993-997`）
  已正确处理解密后空载荷。
- **M-3 缺少 Linux-only 编译门。** `server/mod.rs:32` 仅按 feature 门控 `user_audit` 模块；
  `user_audit.rs:1919-1943` 的全部 peer 校验（auditd UID、socket inode、SO_PEERCRED）都在
  `#[cfg(target_os = "linux")]` 内，非 Linux 下 producer 不校验对端身份直接连接，目前只靠
  `check_integrity` 的运行期检查兜底。违反 §5.1"只支持 Linux"。修复：模块加
  `target_os = "linux"` 门，或对 `all(feature = "user-audit", not(target_os = "linux"))` 出
  `compile_error!`。
- **M-4 producer 侧 journald 与 health 义务大面积缺失。** `user_audit.rs` 全文仅一处日志
  （sequence exhausted，`user_audit.rs:1312`）：`run_supervisor` Err 分支（`user_audit.rs:1901-1914`）
  对所有连接失败静默 sleep 重试；SO_PEERCRED/inode 校验失败无 journald（§5.2）；非 retryable
  hello NACK 无 sticky degraded、无 journald（§8.2）；无 60 秒限频聚合 journald 机制
  （§6.5/§7.2）；supervisor 异常无 journald（§7.3）；`auditd_user` 解析失败表现为无限静默重试，
  启动期不可见。
- **M-5 shutdown final journald 不含遗留诊断聚合。** `server/mod.rs:390-397` 只输出 skipped
  observations 与 diagnostic_drops；drain 超时后仍滞留在 contention/queue_overflow/encode_error/
  permanent_nack accumulator、queue 与 256 条 in-flight 中的聚合计数被静默丢弃，违反 §6.5"退出前
  至少输出最终聚合 journald"。
- **M-6 auditd group commit 未实现，两个配置项成死配置。** `group_commit_max_events`/
  `group_commit_max_delay_ms` 在 `shadowsocks-auditd/src/config.rs:205-206` 定义并校验，但 spool
  无任何引用；实际写路径是每事件 `write_all→sync_data→persist_state→ACK`
  （`spool.rs:894-910`）。耐久性顺序正确、不丢数据，但 §9.3 的 group writer 语义不存在，部署方
  调整配置无任何效果，且每事件两次 fsync 的吞吐代价与设计意图不符。修复：实现 §9.3 group
  writer，或按"逐条提交"升版改写条文（需规格决策）。
- **M-7 corruption quarantine 的 spool_gap 存在崩溃丢失窗口。** `spool.rs:872-885`
  `quarantine_batch_locked` 先 durable rename（`quarantine_path`），再把 gap 推入仅内存的
  `recovery_gaps`，随后才 flush；崩溃发生在 rename 后、gap durable 前时，重启后 `recover_layout`
  不再扫描 quarantine 内对象，该 `segment_corruption` gap 永久丢失。`recover_layout` 自产的
  recovery gaps 同理。§9.5 为两类 eviction 设计了 pending ledger 事务解决同一问题，corruption
  类缺等价物，违反 §9.4"recovery、quarantine 和新 epoch 不能伪装为无数据丢失"。
- **M-8 mock collector 幂等/冲突语义违反 §12。** `tests/mock_collector.py:489-513`：
  (a) 无 `(node_id, spool_epoch, spool_sequence)` 维度，`spool_sequence_conflict` 未实现；
  (b) `event_payload_conflict`/`batch_id_conflict` 时只追加一条 conflict 记录即 raise，同批可接受
  record 被丢弃、incoming wrapper 未 durable 隔离、随后不发送 ACK——正是 §12 禁止的 poison
  batch 永久占据 lease；(c) batch 幂等只比较 body digest，未比较 epoch/first/last/count 全字段。
  mock 是外部 collector 的参照实现（§3.2/§12），必须按"冲突证据+可接受 record 原子 durable
  commit 后 ACK"改写。
- **M-9 测试关键缺口。** §14.2 TCP 成功矩阵仅 3 个测试（缺仅握手/仅上行/仅 banner、双向顺序
  exactly-once、TFO、reset、peer_addr 失败 encode_error、故障不阻断）；§14.3 UDP 窗口/contention
  核心语义无测试（60 秒冷却、LRU 边界、WouldBlock 计数、poison recovery、queue 满刷新窗口）；
  §3.2/§14.4 fuzz target 完全未交付；§14.5 性能门槛无 user-audit feature-off/on 对照
  （`tests/benchmark_data_path.py`、`docs/PERFORMANCE.md` 零提及）；`tests/integration_audit.py`
  不存在，`scripts/test.sh:122` 以 `-f` 判断静默跳过，而 `tests/README.md` 声称提供真实
  TCP/UDP 与 auditd 集成测试。
- **M-10 事件 JSON golden vectors 缺失。** protocol crate 测试（`shadowsocks-audit-protocol/src/
  lib.rs:3060-3349`）只有自生成 round-trip，无钉死期望字节串的向量；escaping/Unicode/nullable/
  全部 5 个 variant 的 golden vectors 未交付（§9.2/§3.2）。HMAC 请求/响应两条向量存在且经
  独立重算正确。
- **M-11 release manifest 未覆盖补丁 series。** `scripts/release-artifact.py:563-571` 的
  exact-keys 不含 series；§15.1 要求发布验证覆盖来源 commit、补丁 series、toolchain lock、
  artifact SHA 和 detached signature。

### 17.5 Minor

| 编号 | 位置 | 问题 |
| --- | --- | --- |
| m-1 | `user_audit.rs:1435-1452` | queue_overflow gap 的 first/last_seen 用被淘汰事件的 `occurred_at`，而非淘汰观测墙钟（encode_error 路径正确用 `wall_now()`），两处时间语义不一致（§6.5） |
| m-2 | `user_audit.rs:82-91` | 重连 jitter 叠加在 5 秒 cap 之上，最大 6 秒；§7.3 措辞歧义（需规格决策） |
| m-3 | `udprelay.rs:863-864,988-992` | 热路径每包可避免的 Address/Arc 克隆；有界合规，可在 association 上缓存 |
| m-4 | `user_audit.rs:1454-1457` | 带 guard 的公有 `record_contention` 无调用方，死代码 |
| m-5 | `user_stats.rs:228-231` | `UserStatsRegistry::new()` 新增 node_id 校验改变 0001 已验收共享路径行为，应在补丁说明中声明 |
| m-6 | 0003 对 `local/*`、`manager/*`、`net/sys/*`、`acl` 的改动 | 绝大多数为 let-chain 格式化 churn，与本功能无关，应剥离或单独成补丁；唯一实质改动是 `local/http/http_stream.rs:23` 双 TLS 后端编译修复 |
| m-7 | `src/service/server.rs:574-598`、`server/mod.rs:483-494` | feature-off 的 SIGTERM 从立即 drop 变为最长 5 秒优雅停机；语义有界但需在发布说明中注明 |
| m-8 | 根 `Cargo.toml` | `user-audit` 含 §5.1 公式之外的 `dep:shadowsocks-auditd`（根 crate 产出 auditd bin 所需），合理但需规格补记（需规格决策） |
| m-9 | `shadowsocks-audit-protocol/src/lib.rs:565-567` | ASCII domain 含空 label 判规范化失败，超出 §6.2 字面规则（需规格决策：补条文或放开） |
| m-10 | `shadowsocks-auditd/src/ingest.rs:446-448` | `protocol_version` 超 u8 的合法 JSON hello 被静默断开，未回 `unsupported_version` hello_nack（§8.2） |
| m-11 | `spool.rs:2696,749-759` | health 计数饱和 u64::MAX 时不置 degraded；§10.1 要求"饱和并置 degraded" |
| m-12 | `ingest.rs:291` | hello 完成即更新 `last_ingest_at_unix_ms`；该字段语义应为最后事件接收时间 |
| m-13 | `spool.rs:423-426`、`lib.rs:76-77` | `segment_max_age_seconds` 无后台 seal timer，只在 append/lease 顺带检查；producer 静默时超龄 segment 不按时封口（lease 强制 seal 使可见行为等价） |
| m-14 | `spool.rs:706-748`、`ingest.rs:124-131` | health/ACK 对每个 sealed batch 全量读取+逐行校验+SHA-256；dedup LRU 用 VecDeque `position()` 线性扫描；性能层面 minor |
| m-15 | `shadowsocks-auditd/Cargo.toml:22`、`spool.rs:2521` | `rand` 依赖未使用；`write_atomic_file` 临时文件崩溃残留无清理 |
| m-16 | `spool.rs` 全模块、`main.rs:33` | spool 同步 I/O 持 std::Mutex 跑在 current_thread runtime，慢盘下事件循环秒级 stall；§14.5 验收须覆盖 |
| m-17 | 根 `Cargo.toml:27-30` vs `shadowsocks-auditd/Cargo.toml:13-15` | 两个同名 `shadowsocks-auditd` bin、两份 CLI 解析，存在发散风险，应只留一份 |
| m-18 | `tests/mock_collector.py`、`test_mock_collector.py:151-153` | mock 无诊断/gap 告警行为；冲突路径测试构造了冲突事件但从未提交断言，测试名不副实 |
| m-19 | `docs/PERFORMANCE.md` | 未同步 §14.5 user-audit 性能门槛（§3.1 文档同步义务） |
| m-20 | `packaging/shadowsocks-auditd.service:34` | `ReadWritePaths` 写父目录而非 ingest/export 两个子目录；实际影响为零，字面与 §11 有出入 |

### 17.6 需规格决策的条目

以下条目代码与规格存在双向出入或规格措辞歧义，须先对本文件升版再由代码跟进：

1. §7.3"指数退避到 5 秒并加入 0–20% jitter"：5 秒是 base cap 还是总延迟硬顶（代码为前者，
   最大 6 秒）。
2. §5.2 `auditd_user` 启动解析失败是否阻断启动（代码降级为无限静默重试的运行故障，且无日志，
   见 M-4）。
3. §6.2 ASCII domain 含空 label 是否视为规范化失败（代码判失败、`normalized_host=null`）。
4. §5.1 根 crate `user-audit` 是否补记 `dep:shadowsocks-auditd`。
5. §9.3 group commit：若采纳逐条提交实现（M-6），须升版改写 group writer 条文并删除两个
   配置项；否则代码补齐 group writer。

### 17.7 已验证无问题（摘要）

- protocol crate：十进制字符串规则、严格 schema 与 variant 交叉校验、target 规范化（UTS #46、
  IPv4-mapped 不折叠）、canonical serializer + RawValue 逐字嵌入 + payload digest 复算、HMAC
  canonical bytes（两条 golden MAC 独立重算吻合）、panic-free、无 I/O。
- producer：§4 类型/API 与锁序、TCP 双向 exactly-once 与 `peer_addr()` 失败 encode_error 路径、
  UDP send helper/64 shard/窗口/poison recovery、guard 覆盖范围、queue/gap 会计与诊断旁路、
  SIGTERM 顺序骨架（除 C-1/M-5）、配置与 feature 合同（除 M-3）、热路径有界无 await、
  非测试代码零 unwrap/expect/panic。
- auditd：CLI 与逐字段配置校验、ingest 帧/hello/ACK/NACK/65536 dedup LRU、spool 持久化顺序与
  crash recovery 主分支、tombstone 两类 pending 事务、export HTTP 严格语法/HMAC/health/204 语义、
  §11 权限模型、panic-free 与资源有界。
- 交付：补丁↔源码树逐字一致、§11 打包（sysusers/tmpfiles/unit/组）、§15 发布链路（除 M-11）、
  文档已同步 v2 常量（无 v1 残留）、无敏感信息泄露、`.gitignore` 覆盖生成物。

## 18. 第二轮代码审计记录（2026-08-28）

> 本节是第二轮审计记录，不改变合同条文。编号接续第 17 节：新增 critical 为 `C-2`/`C-3`，major 为
> `M-12` 起，minor 为 `m-21` 起。行号基准与第 17 节相同（`upstream v1.24.0 + 0001/0002/0003` 应用树）。
> 与第 17 节的差异：本轮**实际执行了编译与测试矩阵**，第 17 节明确未做这两项。

### 18.1 审计范围与方法

- 对象：与第 17 节相同的全部交付物，另加第 17 节自身的结论正确性。
- 方法：
  1. 13 个维度并行逐节静态审查（producer 核心/TCP/UDP、protocol schema、HMAC、auditd 配置/ingest/
     spool 写入/spool 容量/export、打包发布、测试与 mock、文档一致性）；
  2. 每条候选发现交由独立复核者**对抗性反驳**（默认怀疑，须逐行复核代码与规格才可确认）；共产生
     115 条候选，驳回 18 条，保留 97 条，其中多条被复核者下调严重度或修正表述；
  3. 实跑编译矩阵：`cargo check` feature-off 通过（2 条警告）；`cargo check --features user-audit`
     在非 Linux 上按 `crates/shadowsocks-auditd/src/lib.rs:3-4` 的 `compile_error!` 及 `libc::ucred`
     缺失而失败（符合 Linux-only 预期）；
  4. 实跑测试矩阵（全部通过）：`shadowsocks-audit-protocol` 17 项、`shadowsocks-service --features
     user-audit --lib` 71 项、`--workspace --features user-stats` 298 项、`test_mock_collector.py` 5 项、
     `test_audit_packaging.py` 4 项、`test_release_artifact.py` 6 项、`test_http_unix.py` 13 项、
     `check-sensitive.sh`；`shadowsocks-auditd` 的 32 项测试因 Linux-only 在本机未执行；
  5. HMAC golden vector 用独立 Python 实现重算；systemd 行为按 `systemd.exec` 的
     `RuntimeDirectory`/`RuntimeDirectoryPreserve`/`ReadWritePaths` 契约核对。
- 未做：Linux 主机上的真实运行、`shadowsocks-auditd` 测试执行、性能压测。

### 18.2 总体结论

第 17 节对**事件语义、持久化顺序、HMAC canonicalization、权限模型、panic-free** 的正面结论在本轮
复核中继续成立。但本轮新发现 2 个 critical 与 17 个 major，其中两类问题最需要在验收前处理：

1. **打包层把审计故障放大成数据面故障**（`C-2`）——这是本次交付唯一会导致代理中断的缺陷，且触发
   动作正是 `docs/OPERATIONS.md` 明文规定的常规运维步骤。它同时推翻第 17.7 节“§11 打包已验证无
   问题”的结论。
2. **spool 的容量/驱逐路径存在两处永久自锁**（`C-3`）——一旦命中，auditd 的 ingest/lease/ack 全部
   永久返回失败且跨进程重启不自愈，§9.5 的循环覆盖合同事实上只能执行一次。

此外，第 17 节的 `C-1` 严重度定级有误（实际有 5 秒硬顶，不会永久挂起），`M-2` 的事实认定有误，
`M-6` 给出的修复方案在当前架构下不成立，`m-10` 为误报。详见 18.4。

测试欠账比第 17 节 `M-9` 描述的更严重：`ingest.rs`（583 行，承载整个 §8）只有 1 个内存单测，
`spool.rs`（2999 行）10 个，`export.rs`（1365 行）10 个且**没有一个能通过 `authenticate()`**。
本轮 5 个 critical/major 恰好全部落在零测试区域。

### 18.3 Critical

- **C-2 auditd unit 的 `RuntimeDirectory` 删除 socket 父目录，配合 ssserver 无 `-` 前缀的
  `ReadWritePaths`，使一次 auditd 重启后 ssserver 无法启动。**
  `packaging/shadowsocks-auditd.service:29-30` 声明 `RuntimeDirectory=shadowsocks-audit` 且全仓库未
  出现 `RuntimeDirectoryPreserve=`（systemd 默认 `no`），systemd 在 unit **stop（含 restart 的停止
  阶段、`Restart=on-failure` 的每次重启）** 时以 root 递归删除整棵 `/run/shadowsocks-audit`，再次
  start 只重建顶层目录。而 §11 要求的两个带专用组子目录
  `/run/shadowsocks-audit/{ingest,export}`（`0750 shadowsocks-audit:shadowsocks-audit-ingest`/`-export`）
  只由 `packaging/shadowsocks-auditd.tmpfiles:3-4` 在 boot/安装时创建，systemd 无法重建（它给不出这
  两个组），auditd 代码也从不创建（`config.rs` 只对 `spool_dir` 调 `validate_or_create_dir`）。

  两条后果：
  1. **auditd 自身再也起不来**：`AuditDaemon::new`（`lib.rs:34-35`）→ `validate_runtime`
     （`config.rs:376,381-382`）→ `validate_socket_parent_for`（`config.rs:698-702`）首句
     `fs::symlink_metadata(parent)?` 对缺失的 `ingest/` 返回 ENOENT，进程退出（`main.rs` 路径为
     ExitCode 78，`lib.rs::cli_main` 路径为 1），`Restart=on-failure` 进入 3 秒一次的失败循环。
     journald 只有 `configuration file I/O failed: No such file or directory (os error 2)`，不含路径。
  2. **数据面被阻断**：`packaging/shadowsocks-rust-plus.service:34`
     `ReadWritePaths=/run/shadowsocks-rust-plus /run/shadowsocks-audit/ingest` 未加 systemd 的 `-`
     可选前缀，配合 `:25 ProtectSystem=strict`，路径缺失时 ssserver 在 mount namespace 建立阶段以
     `226/NAMESPACE` 失败，`ExecStart` 根本不执行。已在运行的 ssserver 不受影响，但**此后任何一次
     `systemctl start/restart shadowsocks-rust-plus`（配置热更、内核升级、紧急回滚）都会失败**，
     `Restart=on-failure` 反复失败。该条目在功能上还是多余的：`user_audit.rs` 对 ingest socket 只做
     `UnixStream::connect()`，AF_UNIX 的 connect(2) 在只读挂载上不受 EROFS 限制。

  文档明示的必然触发路径：`docs/OPERATIONS.md:388-400`（计划升级屏障要求先停两个服务再先起
  auditd）、`:414-424`（禁用/回滚审计时停 auditd 后重启 ssserver）、`:124-125`（改 spool 上限/
  segment/HMAC 后须重启 auditd）。此外 `docs/OPERATIONS.md:97-107` 的安装 bash 块只对 unit 用
  `install`，对 sysusers/tmpfiles 片段只跑一次 `systemd-tmpfiles --create` 而未装入
  `/usr/lib/tmpfiles.d`，照该块字面部署的节点每次主机重启（`/run` 为 tmpfs）也会落入同一状态。

  违反 §1「合法配置下的运行时审计不得阻断代理流量」、§7.3「auditd 在 ssserver 启动时不可用，
  ssserver 仍正常监听并代理」、§15.3「auditd 故障不需要回滚数据面」，以及 §11 对两个 socket 父目录
  属组的要求（该属性无法在一次 auditd 重启后存活）。注意 §11 字面条款「auditd 失败不得触发
  ssserver stop/restart」并未被违反——本缺陷不触发 stop，而是让下一次 start 失败。

  修复：auditd unit 加 `RuntimeDirectoryPreserve=yes`（或移除 `RuntimeDirectory=` 全部交给 tmpfiles）；
  ssserver unit 的审计路径改为 `-/run/shadowsocks-audit/ingest` 或整条删除；`docs/OPERATIONS.md` 与
  `packaging/README.md` 的安装步骤补上把 sysusers/tmpfiles 片段装入系统目录；
  `tests/test_audit_packaging.py` 增加「auditd unit 不得声明会删除 socket 父目录的 RuntimeDirectory」
  与「ssserver 不得对审计路径形成启动硬依赖」两条断言。

- **C-3 `write_gap_locked` 被 `capacity_ok` 前置拦截，使容量到顶后的 quarantine/驱逐事务把 spool
  永久自锁，§9.5 的循环覆盖只能执行一次。**
  `spool.rs:995-997` 在写 durable gap 之前做 `if !self.capacity_ok(wrapper.len() as u64) { return
  Err(StorageUnavailable) }`，而 `capacity_ok`（`spool.rs:1005-1011`）同时含 `max_spool_bytes` 与
  `min_free_bytes` 两个水位。两条互相独立的触发路径：

  1. **quarantine 路径**：`quarantine_batch_locked`（`spool.rs:872-885`）先把损坏 batch **rename 进
     `quarantine/`**（仍在 root 下，`directory_size` 一字节不减），随后立刻要求把
     `segment_corruption` gap 落盘；此时 `capacity_ok` 必然仍为 false（它刚刚对更小的事件 wrapper
     判过 false），gap 写入失败。`flush_recovery_gaps_locked`（`spool.rs:947-957`）只有写成功才
     `pop_front`，该 spec 永久留在队首。
  2. **驱逐路径**：`evict_sealed_locked`（`1341` 写 durable `eviction_pending` → `1342`
     `remove_dir_all` → `1366` 写 gap）与 `evict_quarantine_locked`（`1400/1401/1419`）同构。只要
     「溢出量大于本次被删对象」（运维下调 `max_spool_bytes`、最老可删对象只有几 KB 的 quarantine
     条目）或「文件系统可用空间低于 `min_free_bytes`」，删除**已经完成**之后 gap 写入仍失败，
     ledger 中留下 `gap_persisted=false` 的 durable pending。

  自锁机制相同：`append` 的固定顺序是 `flush_state_reset`(350) → `reconcile_tombstones_locked`(357)
  → `flush_recovery_gaps_locked`(364) → **之后才是** `ensure_capacity_locked`(399)，而
  `cleanup_locked` 的唯一入口就是 `ensure_capacity_locked`（全文件仅 `1017` 一处调用），
  `ensure_capacity_locked` 又只有 `append:399` 一个调用点。因此一旦 pending gap 写不下去，
  `append`/`lease`(451-455)/`ack`(573-577) 每次都在 reconcile/flush 阶段返回，**容量回收（含
  `remove_aged_acked_locked` 的 24 小时 acked 清理）从此永远不再执行**，`directory_size` 再也不会
  减小，判定恒为 false。

  外部表现：ingest 对每条事件回 retryable `storage_unavailable`（`ingest.rs:359-362`），producer
  无限重试并最终 queue 溢出丢事件；`POST /v1/audit/lease` 与 `/v1/audit/ack` 全部 500
  （`export.rs:1013-1024`）——collector 既取不走也 ACK 不了，于是也无法通过 ACK+24h 清理释放空间；
  `healthz` 仍可用（`health()` 不走 reconcile）并报 degraded，是唯一可观测信号。
  **重启不自愈**：`Spool::open` 的 `298/308` 只 `.is_err()` 吞掉 reconcile/flush 失败并置 degraded，
  重启后 append/lease/ack 仍全部 `StorageUnavailable`；quarantine 路径下 `recovery_gaps` 是纯内存
  队列，重启会丢掉该 gap（与 `M-7` 后果叠加），但 quarantine 目录仍占着容量，下一轮驱逐会把它变成
  durable 的 `quarantine_pending`，自锁重新形成且跨进程持久。
  `recover_layout:1740-1758` 在启动时对损坏 sealed/acked 的 quarantine 同样会塞入 `recovery_gaps`，
  因此首次 append 起就可能落入闭锁，触发面比单纯的“运行期损坏”更宽。

  违反 §1/§9.5「达到上限循环保留最新数据」、§9.5「删除未 ACK segment 后必须在下一可写 segment 中
  写入 spool_gap」与清理顺序 1–6 的逐级推进。

  修复：gap 记录是驱逐事务的收尾，体积（<1 KiB）远小于刚释放的空间，不应受 `capacity_ok` 门控——
  给 `write_gap_locked` 增加 `bypass_capacity` 参数（仅保留 `segment_max_bytes` 与真实 ENOSPC 约束），
  或为 diagnostic 预留固定配额；同时把 `cleanup_locked` 提到 `reconcile_tombstones_locked` 之前，或
  在 reconcile 失败时仍继续尝试 cleanup，打破「要腾空间必须先写 gap、写 gap 又必须先有空间」的循环。

### 18.4 对第 17 节结论的更正

- **`C-1` 严重度与后果更正（应降为 major）。** guard 泄漏机制真实存在，本轮用 multi_thread tokio
  最小程序复现（spawn 后未 poll 即 abort，`active_tasks` 停在 1）。但 C-1 结论「关机永久挂起，只能
  靠 systemd SIGKILL 收场」不成立：`src/service/server.rs:586,591` 用
  `time::timeout(Duration::from_secs(5), server)` 包住**整个 server future**。实际后果上界是关机被
  拖满 5 秒后强制 drop，于是 `log_final_shutdown_skipped`（`mod.rs:461/479`）与
  `stop_audit_supervisor`（`mod.rs:462/480`）被跳过——§7.3 明文要求的 final journald 静默丢失、
  supervisor 的最多 256 条 in-flight 无记录消失——进程以 `ServerAborted`→ExitCode 70 退出，
  `systemctl stop/restart` 把 unit 标为 failed。命中概率约为「泄漏窗口宽度 × accept 速率」，
  1000 conn/s 下每次关机约 0.1% 量级，不是「几乎必然」。修复方案与 C-1 原文一致。
- **`M-2` 事实认定有误，需改写。** `udprelay.rs:435` 处的 `n` 是**解密后 payload 长度**
  （`proxy_socket.rs:534-537`），不是 wire 字节数。wire 上的 0 字节数据报在 `decrypt_client_payload`
  阶段就返回 Err，在 `udprelay.rs:425-428` 早退，根本到不了 435。因此 M-2 的两条论断——「继续进入
  association 创建与解密失败路径」「可被伪造空包造成 association churn」——都不成立，其修复建议
  「还原上游早退」会重新丢弃**合法的、已认证的空载荷**。真实差异应改写为：feature-on 与 feature-off
  在已认证空载荷 UDP 数据报上行为不同（feature-off 丢弃，feature-on 转发），仍是 §1 禁止的 feature
  数据面差异，但方向是「放行上游误丢的合法流量」，不是安全退化；若要求严格等价，应把早退条件改为
  按 wire 长度判断，使两个 build 都不丢弃合法空载荷。emit 侧 `udprelay.rs:993-997` 的
  `!data.is_empty()` 守卫本就保证空载荷不产生审计事件。
- **`M-3` 部分更正。** 根 crate 的 `user-audit` 含 `dep:shadowsocks-auditd`，而
  `crates/shadowsocks-auditd/src/lib.rs:3-4` 与 `main.rs:1-2` 都有
  `compile_error!("shadowsocks-auditd is supported on Linux only")`，因此**任何发布产物都不可能是
  非 Linux 的 audit build**，M-3 描述的「非 Linux 下 producer 不校验对端身份直接连接」不会出现在
  release artifact 中。M-3 的实质仍成立并需修复：`shadowsocks-service` 自己的 `user-audit` feature
  没有任何 Linux 门，`scripts/test.sh:96-98` 正是用
  `cargo test -p shadowsocks-service --features user-audit --lib` 在非 Linux 上编译并运行**跳过了全部
  peer 校验**的 producer，测试因此无法覆盖 §5.2 的身份校验路径。另：`scripts/test.sh:57` 的注释
  「The auditd crate has an intentional Linux-only compile gate」描述的是 auditd crate 而非 service
  crate，容易被误读为 service 侧也有门。
- **`M-6` 的修复方案不成立（结构性），见 `M-16`。** producer 的发送循环是严格 lock-step
  （`user_audit.rs:1969-1971`：write_frame → read_frame → 处理 → pop_front），任一时刻至多 1 条未
  ACK；auditd 侧 `ingest.rs:301-397` 亦逐帧串行 append+ACK。因此**仅在 spool 层补 group writer 的组
  大小恒为 1**，`group_commit_max_events=256` 永远触发不了，`group_commit_max_delay_ms=100` 只会给
  每条事件凭空增加延迟。17.6 第 5 条的两个选项必须改写为：要么同时改造 producer 流水线、ingest 批量
  收帧与 spool 组提交三处，要么按「逐条提交」升版并删除两个配置项。附带说明：producer 的 lock-step
  本身**不违背 §7.2**——规格把 in-flight 定义为「最多 256 条**已经序列化**的
  `VecDeque<SerializedEvent>`」，实现逐字符合。
- **`m-10` 是误报。** `ingest.rs:425-448` 对 `protocol_version` 超 u8 的合法 JSON hello 会正确返回
  `unsupported_version` hello_nack，不是静默断开。应从记录中删除。
- **`m-11` 定位偏差。** health 中可饱和的计数里，`spool_bytes` 走
  `directory_size().unwrap_or(u64::MAX)` 反而必然触发 degraded；真正不驱动 degraded 的是
  `evicted_unacked_records`（`spool.rs:1376`），且它在**任何取值**下都不驱动 degraded——这与
  `M-21` 是同一个缺陷，应合并表述。
- **`m-1` 描述不准确。** `record_encode_error`（`user_audit.rs:1413-1431`）是在 `self.now()` 已失败
  之后再读一次 `wall_now()`，两个主调用点（`1355-1361`、`1387-1393`）在结构上必然拿不到时间，
  因此并非「encode_error 路径正确用 `wall_now()`」；两处时间语义不一致的结论仍成立，但成因需改写。
- **`m-20` 的修复建议无效。** `packaging/shadowsocks-auditd.service:34`
  `ReadWritePaths=/run/shadowsocks-audit /var/lib/shadowsocks-audit` 两行在 `RuntimeDirectory=` 与
  `StateDirectory=` 已隐含授予写权限的前提下**完全冗余**，把它改成两个子目录并不能收紧任何权限；
  真正需要改的是 `C-2` 指出的 ssserver 侧那一行。
- **推翻 17.7 的两条「已验证无问题」**：
  - 「§11 打包（sysusers/tmpfiles/unit/组）」——见 `C-2`、`m-45`、`m-46`、`m-50`；
  - 「auditd：spool 持久化顺序与 crash recovery 主分支、tombstone 两类 pending 事务、export HTTP
    严格语法/HMAC/health」——见 `C-3`、`M-17`、`M-19`、`M-20`、`M-21`。

### 18.5 Major

- **M-12 6 个诊断 bucket 共用一个全局 60 秒闸门且遍历顺序固定，持续过载时 `udp_window_contention`
  与 3 个 `permanent_nack` 被无限期饿死。** `run_supervisor` 的 `diagnostic_retry_after`
  （`user_audit.rs:1765`）是**所有 bucket 共用的单个局部变量**，任一 bucket 被取走（`1804`）或编码
  失败（`1811`）后即整体推后 60 秒；`take_diagnostic_excluding`（`1509-1516`、`1611-1615`）按固定
  顺序 `[QueueOverflow, EncodeError, PermanentNack×3, Contention]` 遍历，命中第一个即返回。于是
  全进程每 60 秒最多发出 1 条 producer diagnostic（而不是每 bucket 1 条）。当 access 产出速率持续高于
  auditd 消费速率时，`try_push`（`1436-1447`）每次 `force_push` 都把 `queue_overflow` 补满、其
  in-flight 位又在同批 ACK 时由 `resolve_pending`（`1645`）释放，因此每个 60 秒边界上它都排在首位并
  重新占用闸门——排在末位的 contention 与 3 个 permanent_nack **在整个过载期一条也发不出去**。计数
  本身不丢（u64 饱和累加），过载缓解后或退出时的 `force_final` 会补发，但 §6.5 明确要求独立披露、
  且禁止并入 `dropped_events` 的 contention 缺口在最需要它的时段对 controller 完全不可见。另：§7.2
  只对 contention snapshot 与**失败重试**规定 60 秒限频，对 `queue_overflow`/`encode_error`/
  `permanent_nack` 的正常 snapshot 并无节流要求，实现把它们一并降到全局 1 条/60 秒，缺口披露延迟
  最坏放大到 6 分钟。修复：`diagnostic_retry_after` 拆成 per-bucket，正常 snapshot 只受
  `diagnostic_inflight` 位门控，遍历起点做 round-robin。（§7.2 §6.5）
- **M-13 重连退避 `sleep` 不感知 shutdown，可独占整个 2 秒 drain 预算，auditd 健康时仍全量丢弃
  queue 与 in-flight。** `user_audit.rs:1900-1912` 的连接失败分支使用裸
  `time::sleep(delay).await`（`1909`），未与 `emitter.notify` / `CLOSED_BIT` /
  `diagnostic_shutdown_requested` 组成 `select!`；`delay` 由 `reconnect_delay_with_jitter`
  （`82-91`）产生，上限 5 秒 + 20% jitter = 6 秒。`diagnostic_shutdown_requested` 只在外层循环顶部
  （`1778`）被读取，sleep 期间无法生效。场景：auditd 曾不可用 3 秒以上（例如
  `systemctl restart shadowsocks-auditd`），backoff 已抬到 3.2–5 秒，auditd 恢复后 producer 仍在一次
  3.2–6 秒的 sleep 中；此时按常规部署顺序重启 ssserver，`mod.rs:456-457/474-475` 的
  `close_emitter()` + `timeout(2s, drain())` 在整个 2 秒内**一次连接都没尝试**，随后
  `stop_audit_supervisor` abort supervisor，queue 中最多 4096 条 access、`pending` 中最多 256 条
  已序列化 in-flight 与全部聚合计数一并随内存丢弃，而 `log_final_shutdown_skipped` 打印的
  `skipped=0 diagnostic_drops=0` 完全掩盖了这次丢失。修复：把 sleep 改为可被 `notify` /
  `CLOSED_BIT` 打断的 `select!`，close/drain 时把 backoff 压回 `INITIAL_RECONNECT_BACKOFF`。
  （§7.3 §6.5）
- **M-14 `check_free_space` 把 `min_free_bytes` 变成启动前置条件，磁盘低水位时 auditd 直接拒绝启动
  而不是按 §9.5 淘汰并置 degraded。** `config.rs:396` 在 `validate_runtime` 中调用
  `check_free_space`（`563-588`），可用空间低于 `min_free_bytes`（默认 1 GiB）即返回
  `ConfigError::Invalid`，进程退出并进入 `Restart=on-failure` 循环。但 §9.5 恰恰把「可用空间低于
  `min_free_bytes`」定义为**常态清理触发条件**，`spool.rs:1329-1333` 甚至为它专门定义了
  `min_free_eviction` reason 并在 health 里置 degraded——即 daemon 被设计为在该水位线上持续工作。
  后果：一次处于 `min_free_eviction` 稳态下的例行重启就是永久性 fatal；而磁盘占用者往往正是 auditd
  自己的 5 GiB spool，本应「重启→清理→自愈」，却被这个前置门禁挡死，acked 与超龄 segment 一个都
  不会被回收。修复：删除 `config.rs:582-587` 的可用空间前置检查（保留容量上界检查），把低水位交给
  §9.5 的驱逐路径与 §10.1 的 degraded health；确需启动期提示则写 journald warning。（§5.3 §9.5）
- **M-15 进程级互斥锁绑定在 `export_socket_path` 而非 `spool_dir`，两个 auditd 可并发写同一 spool。**
  `AuditDaemon::new`（`lib.rs:34-46`）唯一的排他手段是
  `SocketLock::acquire(&config.export_socket_path)`（`lib.rs:39`，锁文件 `<export.sock>.lock`，
  flock 在 `config.rs:108`），`Spool::open`（`spool.rs:217-314`）除进程内 `Mutex` 外**无任何文件系统
  锁**，`validate`（`config.rs:281-315`）也不把 `spool_dir` 纳入唯一性约束，而 `lib.rs:35-38` 的注释
  明确宣称提供了互斥。只需在原有 `/run/shadowsocks-audit/{ingest,export}/` 下换用不同 socket 文件名
  （父目录属主/组/0750 校验照样通过）跑第二份配置，两个实例即可各自完成 `recover_layout`、各自持有
  独立 `next_sequence`、各自 rename `open/current.ndjson` 并原子覆盖同一个 `state.json`/
  `tombstones.json`。结果：同一 `spool_epoch` 下出现内容不同的重复 `spool_sequence`（collector 按
  §12 判 `spool_sequence_conflict` 并整批隔离）、seal 互相抢走对方的 open segment、tombstone 事务被
  覆盖，已 durable 的事件可被静默丢失或重号。违反 §9.3「不回绕、严格递增」与 §9.4「绝不能在同一
  epoch 复用 `(spool_epoch, spool_sequence)`」。修复：改为在 `spool_dir/.lock` 上
  `flock(LOCK_EX|LOCK_NB)`，在 `Spool::open` 之前获取并持有到进程结束。（§9.3 §9.4 §5.3）
- **M-16 §9.3 的 group commit 在当前两侧实现下结构上不可达（`M-6` 的修订）。** 见 18.4 对 `M-6` 的
  更正。补充后果：稳定产生 1500 条事件/秒的节点，因每条事件都要等 ACK、每条 ACK 前要完成约 3 次
  `fdatasync`，在典型云盘（`fdatasync` ≈ 0.3–1 ms）上 ingest 吞吐被钉在 300–1000 events/s，producer
  的 4096 条 `ArrayQueue` 数秒内填满并开始 `force_push` 淘汰**真实的成功访问事件**。这不是「配置项
  失效」而是合同层面的自相矛盾。（§9.3 §8.3 §7.2）
- **M-17 `write_record_locked` 在写入/同步失败后不截断，残留未记账字节使整段 segment 在 seal 后校验
  失败并被整批丢弃。** `spool.rs:887-912` 在 `write_all`(894) 或 `sync_data`(896) 失败时直接返回，
  `append` 的错误分支（`412-422`）只置 degraded，磁盘上留下一条（或半条）未被 `open_meta` 记账的
  记录。后果链：(a) 无需 producer 重试，下一条 append 就会复用未推进的 `next_sequence`，使磁盘上出现
  两行同一 `(spool_epoch, spool_sequence)`，违反 §9.4「绝不能复用」；(b) `seal_locked`（`1517`）写
  meta 时 `first/last/event_count` 取自 `open_meta`（`1524-1531,1551`）而 `raw_bytes/body_sha256`
  取自真实文件（`1543,1552`），于是 `read_batch` 的 digest 检查通过、`validate_segment_body`
  （`2065-2096`）在多出来的那一行上失败；(c) `lease` 随即把整个 batch quarantine（`473-512`），
  最多一个 `segment_max_bytes`（4 MiB，数千条）**已经 durable 并已向 producer ACK** 的审计记录被整段
  丢弃，只留一条 `segment_corruption` gap。典型触发：ext4 延迟分配下 `write_all` 进页缓存成功、
  `fdatasync` 才报 ENOSPC/EDQUOT，或坏扇区/网络块设备抖动导致的瞬时 EIO。修复：任一步失败必须先把
  open 文件 `ftruncate` 回写入前的 `open_meta.bytes` 并重新 `sync_data` + `fsync(open/)`，截断失败则
  立即 seal 或 quarantine 当前 open 并生成 gap。（§9.3 §9.4）
- **M-18 每条事件都对整棵 spool 树做递归 `directory_size`，且 24 小时 acked 保留期只挂在容量清理下，
  最终使单次 append 超过 producer 3 秒 ACK 超时并造成真实事件丢失。** `capacity_ok`
  （`spool.rs:1005-1011`）每次调用 `directory_size(&self.root)`（`2612-2625`）——对 open/sealed/
  acked/quarantine 整棵树递归 `read_dir` + `symlink_metadata`，无缓存；它在 `append` 路径上至少被调用
  两次（`399` 的 `ensure_capacity_locked` 与可能的 `995`）。同时 `remove_aged_acked_locked`
  （`1111`）的唯一调用点是 `cleanup_locked:1027`，而 `cleanup_locked` 只在容量超限时才执行，因此
  collector 正常工作（每秒 lease 一次、及时 ACK）的节点**永远不会触发清理**，24 小时后 `acked/` 下
  约有 86400 个 batch 目录。此后每接收一条事件要 opendir 约 8.6 万次、stat 约 17 万个文件，普通 SSD
  上耗时数百毫秒到数秒，全程持 spool 全局 `Mutex` 并阻塞 current_thread runtime（`main.rs:33`），
  ingest 与 export 同时停摆。一旦单次 append 超过 §5.4 规定的 producer 3 秒 ACK 超时，producer 判超时
  断链重连、in-flight 反复重投、queue 溢出并按 §6.5 生成 `queue_overflow` gap——纯粹因为容量检查的
  实现方式造成审计事件丢失。这是对 `m-16` 的加深与升级。修复：spool 占用量改为增量维护的计数器
  （open 时全量扫描一次，之后在 append/seal/ack/evict/quarantine 各处增减），并把 24 小时 acked 清理
  改为独立于容量触发的周期任务。（§9.5 §8.1 §7.3）
- **M-19 tombstone ledger 的 `batch_id` 唯一性检查跨 variant 生效，使「被隔离且仍持有 receipt 的
  batch」永久阻断 §9.5 第 3/4 步清理。** `add_tombstone_locked`（`spool.rs:1183-1196`）只按
  `batch_id()`/`gap_event_id()` 判身份冲突而**不区分 `entry_type`**，且
  `TombstoneEntry::batch_id()`（protocol `lib.rs:1825-1832`）对 `QuarantinePending` 返回其 nullable
  hint。当一个已 ACK batch 的 `segment.ndjson` 被破坏而 `meta.json` 仍合法时（§13 承认的 root/磁盘
  威胁），`read_batch` 返回 `Ok(None)`，`cleanup_locked:1053-1057` 或
  `remove_aged_acked_locked:1136-1138` 用 `quarantine_batch_locked(..., "acked-corrupt")` 整目录
  rename 进 `quarantine/`（meta.json 随之保留），而它在 ledger 中的 `AckedReceipt(batch_id=B)` 原样
  存在。此后 `evict_quarantine_locked`（`1380-1400`）用 `read_meta_hint` 取到 `batch_id=B` 构造
  `QuarantinePending`，在 `1184` 命中 `same_batch` 但两条 entry 类型不同 → 返回
  `Err(InvalidState)`，经 `?` 一路抛到 `append` 失败。由于 quarantine 列表按 mtime 排序、这个损坏
  对象始终是最老的一条（`1067-1073` 只取 `quarantine.first()`），**每一次清理都会撞在同一条上**，
  容量回收永久停止。修复：`quarantine_pending` 的 `batch_id` 只是尽力而为的证据 hint，不应参与
  ledger 身份唯一性判定；`quarantine_batch_locked` 在把 acked 目录移入 quarantine 时应同时移除或
  改写其 `AckedReceipt`；`cleanup_locked` 各驱逐分支应改为「失败则跳过该对象、记 degraded、继续下
  一个」。（§9.5）
- **M-20 `recover_seal_failure` 的失败分支保留指向已封口 segment inode 的 `open_file`，后续事件被
  ACK 却写进 sealed batch 并整批丢失。** `spool.rs:1613-1628` 的「final rename 已完成」分支在
  `1622-1625` 任一步（`file.sync_all()` 或 `sync_dir(open/)`）失败时提前返回，跳过 `1626/1627`，
  使 `inner.open_file` 继续指向已被 rename 进
  `sealed/<epoch>-<first>-<last>-<batch>/segment.ndjson` 的 inode，且 `open_meta` 保持旧值；
  `seal_locked:1592` 用 `?` 把它抛出后，`append` 的 best-effort seal 分支（`423-435`）静默吞掉并只置
  degraded。此后每一条事件都被 `write_all` 追加进这个**已封口且 meta.json 已固定
  `raw_bytes`/`body_sha256`** 的 segment，`sync_data` 与 `persist_state` 成功，`ingest.rs:357` 照常
  返回 ACK，producer 按 §7.2 释放 in-flight 不再重发；下一次 `lease()` 的 `read_batch`（`813`）因
  body 与 meta 不符返回 `Ok(None)`，整批被 quarantine，原批 N 条已 ACK 事件连同全部误写入事件一次性
  丢失。触发条件限定：由 `segment_max_bytes` 触发的 seal 不会命中（下次 append 的 `390-392` 前置检查
  必然为真，会先重试 seal 并在失败时直接返回 Err、不 ACK）；真正可触发的是 open 段远低于
  `segment_max_bytes` 时的封口（`423-425` 的 `segment_max_age_seconds` 到期封口、lease 强制封口）。
  修复：`recover_seal_failure` 无论成败都必须保证 `inner.open_file` 不再指向被 rename 走的 inode
  （把字段改成 `Option<File>` 并先置 `None`），或在 `write_record_locked` 入口用 `fstat` 校验描述符的
  dev/ino 仍等于 `open/current.ndjson`。（§9.3 §7.2）
- **M-21 health `status` 完全不反映未 ACK 的 `spool_gap`，5 GiB 循环覆盖丢数据后仍返回 200 `ok`。**
  `spool.rs:749-759` 的 status 表达式只含
  `inner.degraded || spool_bytes >= max_spool_bytes || !free_space_ok || producer 断开 >5s`，
  **不含 `gap_degraded`**；而 `evict_sealed_locked`（`1365`）与 `evict_quarantine_locked`（`1418`）
  在成功路径上**只置 `gap_degraded`**（`degraded` 仅在 write_gap 失败时于 `1367` 被置）。于是：
  spool 达到 `max_spool_bytes` → 淘汰最老的未 ACK sealed batch（真实审计记录被永久删除）→ 写入
  `capacity_eviction` gap、`evicted_unacked_records += N` → 淘汰后 `spool_bytes` 已回落到上限以下 →
  随后的 `GET /v1/audit/healthz` 返回 **HTTP 200 且 `"status":"ok"`**。§10.1 明确要求「最新 gap 所在
  segment 尚未被 controller ACK 时返回 degraded 和 HTTP 503」。这也是 `m-11` 真正指向的缺陷。
  反向证据：`refresh_degraded_after_ack_locked`（`695-703`）在清除时**同时**清 `gap_degraded` 与
  `degraded`，说明设计意图本是两者成对置位，置位侧漏了一半。注意
  `flush_state_reset_locked`（`918`）不受影响——其唯一来源 `recover_layout` 的 `force_new_epoch`
  分支已在 `1930-1931` 置 `degraded = true`。修复：把 `gap_degraded`（或
  `has_unacked_spool_gap_locked()`）并入 `749` 的 status 表达式，或在两条驱逐路径上同步置
  `degraded`。（§10.1 §9.5）
- **M-22 `healthz` 与 ACK 恢复路径对整个 sealed 集合做全量重读 + SHA-256 + 逐行双解析，持锁阻塞
  current_thread runtime，放大为审计事件丢失。** `Spool::health`（`spool.rs:727-748`）持锁后
  `729` 全树递归 stat，并在 `733-734` 对**每个** sealed batch 调 `read_batch`（`808-817`）——读整个
  `segment.ndjson`、算全量 `digest_hex`、再 `validate_segment_body`（`2065-2096`）对每行做
  `parse_spool_line` 与 `parse_canonical_record`，后者内部（protocol `lib.rs:1170-1172`）还要 canonical
  重序列化并 memcmp。调用点 `export.rs:444` 位于 async fn 内且无 `spawn_blocking`，runtime 为
  current_thread（`main.rs:33`），ingest 与 export 是同线程两 task。collector 中断导致积压到接近上限
  时（5 GiB / 4 MiB ≈ 1200+ 个 sealed batch），一次例行 `healthz` 需读取并逐行解析约 5 GiB，耗时数十
  秒，期间 ingest 无法 poll producer 帧 → 3 秒 ACK 超时 → 断链重连（与 `M-1` 每批断开叠加形成重连
  风暴）→ queue 溢出丢事件。**健康探针本身导致审计数据丢失，且在最需要健康信号时最严重。**
  同一路径也出现在 gap 未 ACK 期间的每次 ACK（`695-725`）。这是对 `m-14`/`m-16` 的加深与升级。
  修复：health 快照改为增量维护的内存计数；确需校验时只读 `meta.json`；至少把文件 I/O 移出 spool
  `Mutex` 与 runtime 线程。（§10.1 §14.5）
- **M-23 mock collector 对所有 200 响应强制要求 lease 专属的 Body-SHA256 头，导致 ACK 验签必然失败，
  `collect_once` 端到端从未走通。** `tests/mock_collector.py:236-238` 的 `verify_response` 把
  「必须携带 `X-Shadowsocks-Audit-Body-SHA256`」加在**所有 HTTP 200** 上，而 §10.1/§10.2 只对
  **200 lease** 有此要求，其他响应该字段应为空。auditd 的 ACK 成功响应走
  `ResponseParts::json`（`export.rs:476,524-537`），因 `build_wire_response` 的
  `lease_metadata = parts.schema.is_some()`（`624,653`）为 false 而不发该 header，属于合规行为。
  于是 `collect_once`（`mock_collector.py:530-560`）在 `552-559` 对 ACK 200 调用 `verify_response`
  时必然抛 `CollectorError("lease response lacks body digest")`：lease 与 durable 接受都已成功，只在
  最后一步失败，`main()` 打印 `collector failed: ...` 并 exit 1。也就是说 §3.2 要求交付的、
  `tests/README.md:154-161` 与 `docs/OPERATIONS.md:377` 文档化的「可供外部 collector 开发使用的
  mock collector/client」**在真实 auditd 上从未成功完成过一次采集循环**，并会把一个错误的 §10.1
  约束传播给外部实现者。未被发现的直接原因是 `collect_once` 零测试覆盖（全仓库除定义处外零调用），
  而 `scripts/test.sh:122` 又用 `-f` 静默跳过不存在的 `tests/integration_audit.py`。修复：把 lease
  body digest 检查限定到 lease 路由；补一个跑通 lease→accept→ACK→health 的端到端测试并纳入
  `scripts/test.sh`（不得静默跳过）。（§3.2 §12 §10.1）
- **M-24 export 的 HMAC 认证与响应签名路径零测试覆盖。** `export.rs` 的 `mod tests`（`1204-1365`）
  共 10 个测试，全部只覆盖 HTTP 语法层（canonical decimal、token、Host 变体、重复 header、
  partial/malformed 请求的 signable 前缀）与一个 `SocketPathGuard` 用例；**没有一个测试构造过能通过
  `authenticate()`（`343-418`）的请求**，全树无任何测试构造 `ExportServer`。因此 §14.4 的这一整行
  验收项全部零覆盖：正确 HMAC 200/204、错误 key 401、body/header metadata 篡改、timestamp ±299/±301
  边界（`381`）、nonce 重放 409 与 cache 满 429（`404-417`）、「未验签请求不得污染 nonce cache」
  （§10.2 明文 MUST）、response MAC 值正确性、GET 无 Content-Length、degraded 503 health object
  （`445`）、204 无 Content-Length/Content-Type/TE（`455,646`）、`MAX_EXPORT_CONNECTIONS=4` 与
  5 秒超时、`SO_PEERCRED` 401（`287-292`）。验证性实验：把 `check_and_insert`（`66-71`）里 Replay 与
  Full 两个判断对调、把 `383` 的 `abs_diff` 写成 `now - timestamp_value`（u64 下溢，任何未来 1 秒的
  合法请求都被判 401）、把 `387` 的 `ct_eq` 换成 `==`、把 `404` 的 nonce 写入移到 MAC 校验之前——
  以上四种改法 `cargo test -p shadowsocks-auditd` 全部照常通过。本轮的 `M-21`/`M-23` 恰好全部落在
  这片无测试区域。（§14.4 §10.2）
- **M-25 ingest 服务端零测试覆盖。** `ingest.rs:180-399` 的 `run()`/`handle_connection()` 没有任何
  测试触达：crate 无 `tests/` 目录，全树 `IngestServer` 只在 `ingest.rs:147/155` 与 `lib.rs:14/72`
  出现；`ingest.rs:567-583` 的 `#[cfg(test)] mod tests` 只有一个 12 行的纯内存 `DedupCache` 单测。
  对比之下 export 服务端尚有 4 个 async 测试，ingest 为 0。因此 §14.4 的下列条目零覆盖且无法由
  producer 侧测试替代：伪 UID（`SO_PEERCRED`）、错误 node、第二 producer 的 `producer_busy` 与旧
  连接 EOF 后接管、runtime 切换、hello 总超时、frame 边界长时间 idle、首字节后 partial-frame 2 秒
  截止、0/8192/8193 bytes、重复 JSON key 与尾随数据、相同 event ID 同/异 payload 的 dedup 行为、
  「非法 framing 直接关闭不构造响应」。任何回归（例如把 `read_frame_boundary` 的首字节读改回
  `read_exact(&mut header)`，从而给 frame 边界 idle 错误地加上 2 秒超时）都会在 CI 中静默通过，上线
  后表现为空闲期 producer 每 2 秒被踢一次、`producer_connected` 持续 flap。（§14.4 §8）
- **M-26 spool 崩溃恢复与容量回收测试矩阵大面积缺失。** `spool.rs` 共 2999 行，测试只有
  `2754-2999` 的 10 个 `#[test]`；fixture（`2762-2763`）用
  `max_spool_bytes: 67_108_864, min_free_bytes: 1`，配合 `capacity_ok`/`ensure_capacity_locked` 的
  短路，使 `cleanup_locked`(1025)、`remove_aged_acked_locked`(1111)、`evict_sealed_locked`(1314)、
  `evict_quarantine_locked`(1380) 在全部 10 个测试中**均不可达**。据此以下 §14.4/§9.5/§16 条目零
  断言：open 尾部截断与中间损坏的恢复、5 GiB/1 GiB 水位下「acked 优先删除、未 ACK 循环覆盖并写
  `spool_gap`」、「ledger 满或同步失败不得删除 unacked」、24 小时 acked 保留合同、tombstone 4096
  上限与 receipt 淘汰后 ACK 404、已覆盖 batch 410、digest 不同 409、以及各 crash point 的
  pending 事务恢复。`M-7`（quarantine gap 崩溃丢失窗口）与本轮 `C-3`/`M-19`/`M-20` 之所以能长期
  存在，正是因为这片区域没有任何测试。（§14.4 §9.4 §9.5）
- **M-27 §14.1「配置与 feature」矩阵中 `user_audit` 相关条目基本未测。** `config.rs` 的测试模块中
  只有 3 个 user_audit 测试（`4133-4165`），全部带
  `#[cfg(all(feature = "user-audit", target_os = "linux"))]` 且只覆盖
  `canonical_user_audit_socket_path` 的路径规范化；全仓库**没有任何测试构造过含 `user_audit` 的
  wire JSON**。零覆盖项包括：feature-off 下 wire parser 仍识别 `user_audit` 并返回 unsupported-feature
  （`config.rs:3332-3339`）——§14.1 明令「不能静默忽略」，而 `SSUserAuditConfig`(`456-467`) 与
  `user_audit`(`500-501`) 均无 cfg 门，任何人给它们加上 `#[cfg(feature = "user-audit")]`（“清理未
  使用类型”的常见改法）即可让 feature-off 二进制静默丢弃整个 `user_audit` 块而 CI 全绿；
  feature-on 非 Linux 的 Linux-only 报错（`3341-3348`）；`check_user_audit_integrity`
  （`3257-3319`）的全部边界（`queue_capacity` 上下界、64 整除、per-shard/per-association 上限、
  checked arithmetic）；`user_audit` 的 round-trip 与 `deny_unknown_fields`；
  `UserStatsRegistry::new_with_audit`（`user_stats.rs:219`）在该文件 31 个测试中零引用。
  `scripts/test.sh:76` 的 feature-off 检查只是 `cargo check`，捕获不了任何语义回归。（§14.1 §5.1）
- **M-28 Rust 与 mock collector 的 golden vectors 是两组互不相交的硬编码常量，§14.4「逐字一致」
  无任何机制保证。** `crates/shadowsocks-audit-protocol/src/lib.rs:3121-3200` 与
  `tests/test_mock_collector.py:29-69` 各自硬编码一组 HMAC 向量，key/path/nonce/body 四项**全部
  不同**；仓库内不存在共享向量文件，也没有任何测试用同一输入同时驱动两侧。本轮用 mock collector 的
  `canonical_request`/`canonical_response` 复算 Rust 侧输入，得到相同的 MAC，说明两侧**当前**一致，
  但该一致性未被任何测试固定。漂移路径示例：把 `lib.rs:2771` 的
  `value.unwrap_or_default()` 空串缺省改为 `"0"`（为迁就某个 header 变体），Rust 侧只需改自己那条
  硬编码期望值即可全绿，Python 侧 `optional()` 仍返回空串 → 所有 lease 响应 MAC 在外部 collector 上
  失败，而 `cargo test` 与 `python3 tests/test_mock_collector.py` 都通过。§9.2 要求的 event JSON/
  NDJSON golden vectors 则两侧同时缺失（Rust 侧见 `M-10`；Python 侧只有为 parser 行为手工拼的
  wrapper 字面量，digest 动态计算，不是钉死字节的 golden）。修复：把三类 golden vectors 抽成仓库内
  单一数据文件，两侧都从该文件读取并逐字断言。（§14.4 §3.2 §9.2）

### 18.6 Minor

| 编号 | 位置 | 问题 |
| --- | --- | --- |
| m-21 | `user_audit.rs:1212` vs `1219,1224` | `is_drained()` 把 `diagnostics_empty` 提前求值为局部量，再读 `pending_count`/`diagnostic_inflight`，与 supervisor 侧“先 merge 回 accumulator、后 decrement”（`1639/1641/1808`、`1668/1672`、`1848/1850`）读序相反，`&&` 短路保护不了已求值的局部量；多线程 runtime 下存在约 10 条指令的 TOCTOU 窗口，drain 可能提前返回并放弃剩余 best-effort 冲刷。把 `diagnostics_empty()` 移到链尾即可 |
| m-22 | `user_audit.rs:1764,1897-1913` vs `1979-1983` | 重连退避只在整个 session 返回 `Ok` 时重置，§7.3 明文要求「成功收到合法 ACK 后重置」 |
| m-23 | `user_audit.rs:1243-1245,1547,1563,1637,1653`、`mod.rs:389-397` | `udp_window_contention` 的 skipped datagram 计数在 shutdown 冻结后被并入 `shutdown_diagnostic_drops`，与真实丢事件数混在同一条 final journald；§6.5 明令 controller 不得把两者合并 |
| m-24 | `user_audit.rs:1632-1645,1646-1658,1806-1813` | producer diagnostic 构造/序列化失败或被永久 NACK 时只做 merge-back，缺 §7.2 要求的「增加 health counter」 |
| m-25 | `mod.rs:69,168,317,324-327,369,450-484`、`lib.rs:61`、`src/service/server.rs` | §7.3 强制的 `build_server(config) -> ServerRuntime { run, data_shutdown, audit_shutdown }` API 不存在，两个 shutdown 句柄被埋在 `run` future 内部；语义等价但违反明文 API 合同 |
| m-26 | `server/server.rs:224-230,274-276`、`utils.rs:19-24` | `Server::run` 用 `select_all` 只 abort 不 join 子 listener/plugin/manager task，§7.3「await/join 全部 JoinHandle」在代码层面并未成立 |
| m-27 | `user_audit.rs:989,1235,1251,1259,1265,1269,1278,1284,1454`、`2146` | feature-on 编译产生 9 条 dead-code 警告，证明 §1/§6.5 要求的 producer health counter（poison reset、contention/encode-error time-unknown、invalid ACK、sequence exhausted）在进程内**没有任何读取方**，既不进 journald 也不进任何接口——`M-4` 的机器可证版本 |
| m-28 | `user_audit.rs:1873-1877` | supervisor 在 GapSlot seqlock 处于奇数（生产者正在合并计数）时进入**无 await 的紧循环自旋**，违反 §7.2「禁止 tight loop」 |
| m-29 | `user_audit.rs:907-913,924,963-974`、`udprelay.rs:1000-1011` | UDP 窗口查找是 O(shard_capacity) 线性扫描且扫描方向与 LRU 顺序相反，最热的 key 命中代价恒为最坏值，全程持分片锁；且每包在冷却判定**之前**就构造完整 target draft（含域名规范化）。有界合规，实测 miss 路径约 3 µs/包，但与 §7.1「有界」的设计意图有距离——`m-3` 的加深 |
| m-30 | `user_audit.rs:278-465,148-153,1675-1692` | producer 绕过协议 crate 维护了第二套事件序列化器，违反 §9.2「协议 crate 必须提供**唯一**的 compact deterministic serializer」；两套实现漂移不会被任何测试发现 |
| m-31 | `shadowsocks-audit-protocol/src/lib.rs:549-584` | `normalize_domain` 的 Unicode 分支额外强制 UTS #46 的 `VerifyDnsLength` 与 `CheckHyphens`（比 §6.2 严），ASCII 分支则完全不跑 UTS #46（比 §6.2 松），两条路径宽严相反——`m-9` 的加深 |
| m-32 | `shadowsocks-audit-protocol/src/lib.rs:1142-1157,211-227,232-235` | `parse_record`/`parse_json_exact` 接受 JSON 前后的空格与 TAB，与 §6.1「拒绝空白和尾随数据」字面冲突 |
| m-33 | `protocol/lib.rs:24-27`、`user_audit.rs:51,53`、`auditd/src/ingest.rs` | 8192 字节上限在三处独立定义而非引用协议 crate 导出的常量，三者可静默漂移 |
| m-34 | `shadowsocks-audit-protocol/src/lib.rs:3125-3134` | Rust 请求 golden vector 使用 `POST /v1/audit/healthz`——服务端（`export.rs:828-838`）必然拒绝的 method/path 组合，向量本身不代表任何合法请求 |
| m-35 | `auditd/src/main.rs:8-31`、`lib.rs:121-142` | auditd CLI 解析器零测试且结构上不可测（解析逻辑内联在 `main`）；§5.3/§14.1 要求的 7 项 CLI 失败路径与 6 个数值范围边界值均无用例 |
| m-36 | `auditd/src/config.rs:1080-1096` | `config_rejects_unknown_fields_and_trailing_json` 的 `load_from_file` 分支是空断言，从未走到 JSON 解析器 |
| m-37 | `crates/shadowsocks-service/src/config.rs:3277-3282` | ssserver 侧缺少 auditd 已有的 `sun_path` 107 字节长度校验，超长 `ingest_socket_path` 通过配置校验后永久连不上，且因 `M-4` 无任何日志 |
| m-38 | `auditd/src/main.rs:42-61` vs `lib.rs:151-174`、根 `Cargo.toml:27-30` | 两份 `shadowsocks-auditd` bin 的具体发散：`validate_runtime` 失败时退出码 78 vs 1；且两个同名 bin 在 workspace 构建中产生同名输出文件冲突，`scripts/test.sh` 在 Linux 上测到的是不确定的那一个——`m-17` 的加深 |
| m-39 | `auditd/src/ingest.rs:117-144,323-344` | dedup LRU 用 `VecDeque::position()` 线性扫描，缓存满后每事件固定付出 O(65536) 次字符串比较（实测约 153 µs/事件）——`m-14` 的量化 |
| m-40 | `auditd/src/ingest.rs:219-222,379` | spool 写入的 I/O 错误（如 ENOSPC）不返回 retryable `storage_unavailable` 而是静默关闭连接，producer 侧因此走连接失败退避而非 §8.3 定义的 NACK 路径 |
| m-41 | `auditd/src/ingest.rs:206-210`、`lib.rs:96-113` | `accept()` 的任一瞬时错误（EMFILE、ECONNABORTED）终止整个 auditd 进程，ingest 与 export 同时停止服务 |
| m-42 | `auditd/src/ingest.rs:306`、`spool.rs:347` | 每条 ingest 事件被完整强类型解析两次（ingest 一次、`spool.append` 再一次），合计每事件 6 遍 JSON 遍历加 2 次 canonical 重序列化 |
| m-43 | `auditd/src/spool.rs:1848-1860,1918-1938` | 全新节点首次启动即伪造 `state_reset` spool_gap 并进入 degraded/503，与 §9.3「首次初始化生成随机 epoch、sequence 从 1 开始」的正常路径冲突；运维会在全新节点上看到一条不存在的数据丢失告警 |
| m-44 | `auditd/src/spool.rs:1635-1650` | `recover_seal_failure` 回滚 rename 后只同步了 tmp 的父目录而非 tmp 本身，且 `remove_dir_all` 失败被静默忽略，可留下不可回收的临时目录 |
| m-45 | `auditd/src/spool.rs:772,1919-1922` | health 的 `stored_records` 取 `next_sequence - 1`，每次换 epoch 后归零，且不反映旧 epoch 仍在盘上的记录，语义与字段名不符 |
| m-46 | `auditd/src/spool.rs:695-704` | `refresh_degraded_after_ack_locked` 用一次**无关 batch** 的 ACK 无条件把 `degraded` 清为 false，抹掉 tombstone ledger 损坏、seal 失败等与 gap 无关的降级原因；§10.1 要求这些状态保守保留 |
| m-47 | `auditd/src/spool.rs:1025-1035` | `cleanup_locked` 在回收任何空间前先 seal open segment（把 §9.5 清理顺序的第 5 步提到第 2 步之前），且 seal 失败即中止整个清理 |
| m-48 | `auditd/src/spool.rs:2707-2745,1476-1477,1504-1505` | `event_id_exists_in_epoch` 按**当前** epoch 过滤，pending 跨 epoch 恢复时会用同一固定 `gap_event_id` 再写一条内容不同的 spool_gap，违反 §9.5「禁止生成新 ID 造成重复 gap」的等价意图，collector 侧表现为 `event_payload_conflict` |
| m-49 | `auditd/src/spool.rs:247-255,2299-2338` | `tombstones.json` 解析失败时整体丢弃 ledger，两类 pending 一并消失，已删除 batch 的 gap 永久丢失且无法补写 |
| m-50 | `auditd/src/spool.rs:2223-2233,1036-1097` | 隐藏临时目录与临时文件计入 `directory_size` 但清理路径永不回收，可使容量预算被不可回收对象长期占用（与 `C-3` 叠加会加速自锁） |
| m-51 | `auditd/src/spool.rs:350-370,1211-1215` | reconcile/flush 路径的非 `StorageUnavailable` 失败不计入 `storage_rejected_attempts`，§9.5 点名的两种情形（ledger 满、ledger 无法同步）漏计 |
| m-52 | `packaging/shadowsocks-auditd.sysusers:11-12`、`config/auditd.example.json:5`、`docs/OPERATIONS.md:98-106` | 出厂 sysusers 不创建 `export_peer_user`（示例值 `audit-exporter`），而 `validate_runtime` 要求它必须能解析为 UID；照文档逐条安装后 auditd 启动即失败 |
| m-53 | `packaging/shadowsocks-auditd.service:16-34`、`packaging/README.md:18` | §11「auditd 不得具备读取 ssserver config、iPSK、uPSK、users.json、stats UDS 的权限」在 unit 中没有任何结构性实现（无 `InaccessiblePaths=`/`ProtectProc=` 等），仅靠 `ProtectSystem=strict` 的只读语义，且模板未规定 ssserver 配置目录的 owner:group |
| m-54 | `scripts/release-artifact.py:563-605`、`scripts/verify-release.sh:68-89`、`scripts/sign-release.sh` | 发布验证既不覆盖补丁 series（`M-11`），也不要求验签机工作树干净，且 §15.1 列出的 6 个产物名从未被任何脚本强制存在性检查 |
| m-55 | `tests/test_audit_packaging.py:44-76` | 打包回归未覆盖 §11 的关键权限条款（两个组的成员集合、socket 0660、spool 0700、config 0640、HMAC key 0600、`/etc/shadowsocks-audit` 0750 root:shadowsocks-audit），只断言了少数字符串 |
| m-56 | `scripts/check-sensitive.sh:21-23`、`.gitignore` | 敏感信息扫描识别不了本功能引入的两类明文密钥形态（64 位小写 hex 的 HMAC key、Base64 uPSK），`.gitignore` 也未覆盖 `export-hmac` 类文件名 |
| m-57 | `patches/README.md`（末段维护流程） | 要求执行 `cargo test --workspace --features user-audit`，但该命令在非 Linux 上因 `compile_error!` 必然失败，与实现的 Linux-only 门矛盾 |
| m-58 | `tests/mock_collector.py`（全文件） | mock collector 完全没有 health 采集与 §12.5 的告警路径，`degraded`/`spool_gap`/`producer_gap`/`udp_window_contention`/`batch_evicted` 语义在参照实现中不存在 |
| m-59 | `tests/mock_collector.py:401-418,437-477` | mock collector 的 durable state 写入非原子（`O_TRUNC` 原地覆盖、无 temp+rename、无父目录 fsync），崩溃后去重状态整体损坏且无法重启，与 §12.4「durable 保存后才 ACK」的示范意图相反 |
| m-60 | `tests/mock_collector.py:496-511` | 同一 batch 内重复 `event_id` 不同 payload 时静默覆盖，且覆盖方向与 §12 相反（保留新记录而非原记录） |
| m-61 | `crates/`（全树）、`scripts/test.sh`、`Cargo.toml:60-65` | §3.2/§14.4 要求的 fuzz target、release-profile `panic = "abort"` 子进程测试，以及「审计路径无 `unwrap()`/`expect()`/越界索引」的自动化检查全部未交付（本轮人工扫描确认非测试代码的 6 处裸索引均有长度或类型守卫，但没有任何机制防止回归） |
| m-62 | `tests/benchmark_data_path.py`、`docs/PERFORMANCE.md`、`scripts/test.sh:110-127` | §14.5 的五项性能门槛与四项专项压测无任何对应实现，`test.sh`/`verify.sh` 也不含性能步骤——`M-9`/`m-19` 的量化 |
| m-63 | `udprelay.rs:1458-1497` | 测试把 `M-2`（feature-on 改变空载荷 UDP 接收行为）**写成期望行为并加以断言**，其 feature-off 分支在 Linux 上从不执行；green test 反向锁死了一个已知的 feature 行为差异 |
| m-64 | `tests/test_audit_packaging.py:38-40,57` | 同样把 `m-20` 的非规格 `ReadWritePaths` 写法与 `M-6` 的两个死配置项固化为期望值 |
| m-65 | `docs/OPERATIONS.md:96-106,117-122`、`README.md:86-102,92-93`、`docs/API.md:293`、`docs/ARCHITECTURE.md:159-160`、`docs/PERFORMANCE.md:9-14` | 文档与实现不符的五处：安装步骤原样安装示例配置（缺 `export_peer_user` 账号与 `node_id` 定制，必然启动失败）；把直接运行 `shadowsocks-auditd` 描述为“启动前配置检查”，但二进制没有 check-only 模式且以 root 运行必然失败；声称 `scripts/build.sh` 在当前宿主平台（含 macOS）构建 auditd，实际非 Linux 无法编译；声称 `verify.sh` 会运行 auditd 集成测试，实际 `tests/integration_audit.py` 不存在；`API.md`/`ARCHITECTURE.md` 明文承诺 group `fdatasync` 提交语义，与逐条提交实现不符（见 `M-16`） |

### 18.7 v4 已锁定的审计决策

本节所列 18.7 条目已在 v4 正式决策，并已纳入当前实现与第 19 节结论；不再是待选择的方案：

6. §9.3 采用逐条提交。每条 record 必须依次完成 `write_all`、open segment `fdatasync` 和
   `state.json` durability barrier，完成后才发送 ACK；`group_commit_max_events` 与
   `group_commit_max_delay_ms` 从配置 schema 删除。未来若改为批量提交，必须先升版并同时改造
   producer、ingest、spool 三侧，不能以未生效字段宣称 durability。
7. §1 的“审计不得改变代理行为”适用于已认证空 UDP payload：feature-on 与 feature-off 都放行
   完整成功发送，空 payload 不生成 audit event。该行为由 wire 长度/发送结果决定，而不是由审计
   feature 决定。
8. §5.3 的 `min_free_bytes` 只是运行期清理触发水位，不是启动前置条件。auditd 在低于水位时仍
   启动并按 §9.5 尝试清理；无法恢复时通过 `degraded` health 和有界计数表达。
9. §10.1 的 health `status` 在存在未 ACK 的 `spool_gap` 时必须为 `degraded`；相关 gap ACK
   后可恢复为 `ok`，但 `evicted_unacked_records` 等累计计数不得清零，其他降级原因也不得被无关
   ACK 抹掉。
10. §7.2 的 producer diagnostic 按 bucket 独立维护 retry/deadline，并以 round-robin 选择待发
    bucket。contention 及其 journald 错误各自 60 秒限频；正常 producer-gap snapshot 只受自身
    in-flight 门控，不得被另一个 bucket 的退避饿死。
11. §9.3/§9.4 要求 auditd 在 `spool_dir/.lock` 上持有文件系统级 `flock` 排他锁；同一 spool
    目录即使使用不同 socket 配置，也不得由两个 auditd 并发写入。

### 18.8 本轮驳回的候选问题（记录以免重复排查）

以下 18 条候选经逐行复核不成立，主要驳回理由如下（同类问题日后无需重复分析）：
`GapFallback` count-only 溢出并未作废已记录的 first/last（元数据与计数分别原子维护）；
「C-1 主触发点在 `mod.rs:453/471` 的常规 `stop_accepting`」不成立（泄漏窗口是 `spawn_relay` 内部
无 await 段，与翻转点距 `terminate_and_join` 多远无关，且规格强制的 stop→drain 顺序反而给了 runtime
poll 机会）；TCP 两次 registry 查找不违反 §4（规格未禁止，且两次查找取的是不同对象）；
send helper 的空载荷判断下沉到 emit 点不违反 §6.4（`bytes_sent == payload.len()` 已覆盖）；
`AuditTargetDraft::ip()` 的 `port=0` 不是死代码（`ip_with_port` 复用它）；canonical response 中
`Some("")` 与 `None` 同签名不构成歧义（两者在协议上都表示“无 Content-Type”）；
`validate_origin_path` 不拒尾斜杠与 dot-segment 由精确 path 白名单兜底；未验签的 400/401 携带
response MAC 不构成认证边界问题（`request_is_signable` 只在 node/nonce 均合法解析时才签名，且
不泄露 key 存在性）；export 出口不经 protocol crate 的 `serialize_*` 不构成 wire 偏差；
hello 要求逐字等于 canonical 序列化未超出 §9.2（producer「只能发送该输出」是明文要求）；
dedup 命中不更新 `last_ingest_at_unix_ms` 与 `m-12` 是同一问题；运行期 quarantine 不换 epoch 符合
§9.4（换 epoch 只对 open/state 分支要求）；24 小时 acked 保留只在容量触发时执行虽是事实，但按 §9.5
字面「容量或磁盘水位优先」并非违背（其真实危害已并入 `M-18`）；「producer 从未连接过时 health 返回
ok」符合 §10.1 字面（degraded 条件是“断开超过 5 秒”，未连接不是断开）；
`OPERATIONS.md` 的 0600 socket 模式描述指的是 stats socket 而非审计 socket。

## 19. 修复结论（2026-08-28）

> 本节记录按第 17、18 节审计结果在当前工作树中实施的修复，不改变前述规范条文。
> 结论对应当前未提交的开发工作树；代码、准备源码树和补丁 series 已按本节的 clean replay
> 流程校验。凡标注“环境限制”或“未完成”的项目，不得解释为已通过完整发布门槛。

### 19.1 Critical 修复

- **C-1 已修复。** `server/context.rs::spawn_relay` 在 task 首次 poll 之前即建立并移入
  `RelayTaskGuard`，并用 start gate 覆盖尚未开始执行时的 abort 路径；无论 task 是否被
  首次 poll，`active_tasks` 注册都会释放。回归用例
  `abort_before_first_poll_releases_task_registration` 固定了该关机竞态。
- **C-2 已修复。** auditd unit 增加 `RuntimeDirectoryPreserve=yes`，并明确隔离
  `/etc/shadowsocks-rust-plus` 与 `/run/shadowsocks-rust-plus`；ssserver 删除对 audit ingest
  目录的强制 `ReadWritePaths`，只保留 `Wants=`/`After=`，auditd 离线不会阻断数据面。补齐
  `audit-exporter` sysuser、成员组、sysusers/tmpfiles 安装步骤和权限/打包断言，重启后
  ingest/export socket 父目录可由 tmpfiles 恢复。
- **C-3 已修复。** `spool.rs::write_gap_locked` 不再受普通容量水位短路；quarantine 和
  eviction 先写入并同步 marker/tombstone，再执行 rename/remove 等破坏性操作。单个候选
  清理失败会记录并继续尝试其他候选，避免一个坏对象永久锁死整个 spool；失败状态仍进入
  degraded/计数路径。

### 19.2 Major 修复

以下项目已按第二轮审计意见落地：

| 编号 | 修复结论 |
| --- | --- |
| M-12 | 诊断 bucket 使用独立 deadline、round-robin cursor 和逐 bucket retry；contention 或永久 NACK 不再让固定遍历顺序饿死其他诊断。 |
| M-13 | 重连等待改为 shutdown-aware `select!`，关闭时立即退出并重置退避；sticky hello NACK 使用固定 5 秒间隔，收到合法 ACK 后恢复正常退避。 |
| M-14 | `min_free_bytes` 仅是运行期清理触发水位，不再作为启动前置条件；无法满足水位时由清理和 degraded health 表达，而不是拒绝启动。 |
| M-15 | spool 目录增加 `.lock`/`flock` 排他锁，同一目录即使 socket 配置不同也不能由两个 auditd 并发写入。 |
| M-16 | 移除未生效的 group-commit 配置；逐条 `write_all`、open segment `fdatasync`、state durability barrier 完成后才 ACK。若将来引入批量提交，必须先升版并同时改造 producer、ingest、spool。 |
| M-17 | 写入或同步任一步失败时回滚文件长度、metadata、sequence、计数和 oldest timestamp，避免内存索引领先于磁盘。 |
| M-18 | spool bytes/records/batches 改由启动扫描后的增量索引维护；acked retention 由周期 sweep 触发，不在每次 health/ACK 下全量重读。 |
| M-19 | tombstone 身份按 variant 区分；quarantine marker 先 durable；corruption quarantine 与 quarantine eviction 分开处理，并维护对应 receipt/index。 |
| M-20 | seal 失败会解除旧 descriptor 与已封口 inode 的绑定，重新打开 `open/current.ndjson`，再重建运行时索引，避免继续写入已封口对象。 |
| M-21 | health 纳入 `gap_degraded`、未 ACK gap 和饱和计数；ACK 只清除已恢复的 gap 条件，不抹掉其他 degraded 原因。 |
| M-22 | health/ACK 使用增量快照，避免在 current-thread runtime 和 spool mutex 下递归扫描、全量 hash 或逐行解析 sealed 集合。 |
| M-23 | mock collector 仅对 200 lease 响应要求 Body-SHA256；状态写入采用 temp + fsync + rename + 目录 fsync，并提供可复用的 `collect_once` 流程。 |
| M-24 | 增加签名 lease、ACK、health 的集成路径和协议验证；export 原生认证 runtime 矩阵仍受本机 macOS 限制，见 19.4。 |
| M-25 | `integration_audit.py` 覆盖 ingest hello、事件、ACK 和 peer 身份；原生 ingest runtime 测试需 Linux auditd 环境，未在本机执行。 |
| M-26 | 增加 spool lock、state reset、seal failure、health gap 等回归用例；完整 crash-point、5 GiB/1 GiB 容量回收矩阵仍未完全覆盖，不能据此宣称第 14.4 节全通过。 |
| M-27 | 增加 Linux-only `compile_error!` 和 socket/path/config 静态检查；Linux feature-on 测试需 Linux 主机，本机只完成交叉编译检查。 |
| M-28 | Rust protocol 与 Python mock collector 共用 `tests/golden_vectors.json`；`scripts/test.sh` 逐字 `cmp`，两侧同时验证 canonical record 与 HMAC vectors。 |

第 17 节遗留的 session、UDP 空 payload、producer shutdown 诊断、quarantine gap、mock 幂等、
事件 golden vector 和 release manifest 问题也随上述改动一并收敛：session 保持长连接，
feature-on/off 对已认证空 payload 采用相同数据面处理，最终诊断和 health counter 有独立汇总，
quarantine gap 先落 durable 证据，mock collector 执行冲突隔离后再 ACK，事件向量和补丁 series
进入自动校验。未把旧的“group commit”死配置重新带回 schema。

### 19.3 关键 minor 与交付护栏

- drain 判断调整为先观察 pending/in-flight，再读取 diagnostics，修复 TOCTOU；合法 ACK 后立即
  重置退避；shutdown contention drops 与普通 diagnostic drops 分开计数并进入最终报告。
- ingest hello 只更新 producer 连接状态，不伪造 `last_ingest_at_unix_ms`；新事件和去重重放在成功
  ACK 后按实际接收时间刷新该字段，断开时清理 runtime/timestamp。
- 审计 ingest socket 的现有父目录只信任 root、当前 producer 或已解析的 `auditd_user` UID；root
  启动且不降权时不再接受任意非特权属主目录。ssserver 根 launcher 在构建 listener 前同步首次
  poll signal monitor，消除 `spawn + yield_now` 的未注册窗口。
- producer health counters 接入 snapshot/final reporting；诊断 accumulator 使用有界扫描和
  fallback，避免 seqlock 紧循环；严格 JSON 边界拒绝前后空白，payload 上限改用共享常量，并补充
  合法 lease golden vector。
- CLI/config parser、Linux `sun_path` 长度、dedup LRU 的 HashMap/token、transient `accept()`
  错误恢复和 ingest I/O 的 retryable NACK 均加入检查；spool 新节点不再伪造 state-reset，跨
  epoch event ID 与 tombstone salvage 路径有明确处理。
- packaging 补齐 exporter 账号、manifest patch series、敏感信息扫描和权限断言；mock collector
  增加 health/gap 记录与原子状态写入；README、API、ARCHITECTURE、OPERATIONS、PERFORMANCE
  及 patches 文档同步当前逐条 durability 和 Linux-only 边界。

这些改动解决了本轮已确认的行为缺陷，但不把“有界设计取舍”写成性能保证：UDP 热路径、诊断
扫描等仍须以第 14.5 节目标机数据复测；fuzz、release-profile `panic = "abort"` 子进程门和
完整容量/crash 矩阵仍属于未完成验收项。

### 19.4 已执行验证与环境限制

本机为 **macOS**。本节按第四轮审计证据更正 `4a6348e` 当时的验证记录；以下命令在当时的
工作树/准备源码树上实际执行并通过：

```text
cargo test --locked -p shadowsocks-audit-protocol                                      # 20 passed
cargo test --locked -p shadowsocks-service --no-default-features --features server --lib # 8 passed
cargo test --locked --workspace --features user-stats                                   # 302 passed
python3 tests/check_audit_static.py --source .cache/audit-work-source
python3 tests/test_check_audit_static.py
python3 tests/test_mock_collector.py
python3 tests/test_audit_packaging.py
python3 tests/test_release_artifact.py
python3 tests/test_fuzz_target.py
python3 tests/test_panic_abort.py --source .cache/audit-work-source
python3 tests/test_benchmark_audit.py
bash scripts/check-sensitive.sh
git diff --check
```

原记录中的 `python3 tests/test_check_audit_static.py --source ...` 是不可执行的命令（该 unittest
文件没有该参数），已按上面拆成实际的静态检查器与 unittest 两条命令。当时 `rg` 已安装，
`bash scripts/check-sensitive.sh` 实跑通过，不存在“本机缺 `rg`”的例外。

以下项目在该次记录时**未执行**：`cargo check --locked --target x86_64-unknown-linux-gnu -p
shadowsocks-auditd --all-targets` 和 `bash scripts/test.sh ...`，因为当时未安装该 Rust target；Linux
auditd runtime（包括 ingest/export 原生认证、peer/UDS、真实 Linux SIGTERM 与 crash/capacity
场景）、完整 Linux feature-on workspace、fuzz 实际运行及第 14.5 节性能压测也均未执行。非 Linux
运行完整脚本的显式前置条件是先安装 `SHADOWSOCKS_AUDIT_CHECK_TARGET`；交叉 `cargo check` 仍不能
替代 Linux 原生测试。因此该次结论只覆盖静态/可移植测试，不是最终发布签字。

## 20. 第三轮代码审计记录（2026-08-28）

> 本节是对第 19 节修复结论（commit `4a6348e`）的对抗性验证记录，不改变合同条文。新增问题编号
> 接续前序：major 自 `M-29` 起，minor 自 `m-66` 起；本轮无新 critical。行号基于整改后的
> `.cache/audit-work-source`（与 `patches/0003-user-audit.patch` 逐字一致）。

### 20.1 审计范围与方法

- 对象：整改后全部交付物；重点是 §19 每条"已修复"声明的代码证据，以及整改 diff 引入的回归。
- 方法：四路并行对抗性验证（producer / auditd / 协议与 mock / 打包脚本文档），关键发现由
  编排者亲自复核源码确认；本机实跑可移植验证项（protocol 20 项、service server --lib 8 项、
  workspace 302 项、mock/packaging/release/fuzz/panic 各 Python 套件、golden vectors 独立重算、
  feature-off `cargo check` 零警告、user-audit 在非 Linux 被 `compile_error!` 拦截）；补丁↔源码树
  `patch --dry-run -R -p1 --fuzz=0` 48 个文件干净反向应用；对合同条文（§1–16）逐 hunk 核对
  两个整改 commit，确认无夹带变更。
- 未做：Linux runtime 实跑、fuzz 实跑、性能压测（同 §19.4 披露）。

### 20.2 总体结论

§19 的修复声明**大体属实**：三个 critical（C-1/C-2/C-3）与绝大多数 major/minor 经当前代码逐条
证实已修复并带有回归测试；两项核心交付核查（补丁与源码树逐字一致、合同文本无夹带）明确通过。
但有 3 条 major 级新问题/残留需要在验收前处理：**M-29**（集成测试必然 ImportError，§19 声称的
Linux 端到端路径按现状跑不过）、**M-30**（整改把 M-22 消灭的"锁内全量读+逐行解析"又在 gap
写入路径请回了 §9.5 过载场景）、**M-31**（seal 持续失败时每条 append 触发全量索引重建）。
另有 m-1 证伪未修复、M-17 兜底缺失等部分修复项，见 20.3/20.4。

### 20.3 §17/§18 条目验证结果汇总

已修复（本轮逐条证实，含回归测试）：C-1、C-2、C-3；M-1、M-2、M-3、M-5、M-8、M-11、M-12、
M-13、M-14、M-15、M-16、M-18、M-19、M-20、M-21、M-23、M-27、M-28；m-21、m-22、m-23、m-24、
m-25、m-26、m-28、m-29、m-30、m-31、m-32、m-33、m-34、m-35、m-36、m-37、m-38、m-39、m-40、
m-41、m-42、m-43、m-44、m-45、m-46、m-47、m-48、m-49、m-50、m-51、m-52、m-53、m-57、m-59、
m-60、m-63、m-64、m-65。

部分修复：

| 编号 | 状态 |
| --- | --- |
| M-4 | 连接失败/NACK/解析失败已接入 60 秒限频 journald 与 sticky degraded；§6.5 要求的 contention 运行期聚合 journald 仍缺（仅 wire + final） |
| M-17 | 写入/同步失败的回滚（ftruncate+同步+索引恢复）已实现；回滚失败后的 seal/quarantine open + gap 兜底未实现（残留见 m-72），外部表现为 retryable NACK + degraded 直到重启 |
| M-22 | health/ACK 已改增量快照；但 gap 写入路径新引入全量扫描，见 M-30 |
| M-24/M-25/M-26 | export 认证矩阵、ingest 服务端、spool 容量/crash 矩阵的 Rust 测试仍大面积缺失；新增覆盖与 §19.2/§19.4 披露口径相符，不夸大 |
| m-55 | 组成员/spool 0700//etc 0750 已有断言；socket 0660 为名义覆盖（m-78） |
| m-56 | 敏感扫描新增两类形态，仍有逃逸形态（m-77） |
| m-58 | mock 已有 health/gap primitives 与测试；未接入 collect_once/ACK-410/跨批缺号检测（m-76） |
| m-61 | 静态护栏存在且接入 test.sh；protocol crate 与 config/mod 接线不在扫描范围（m-82） |
| m-62 | §14.5 仅合成 preflight；真实 feature-off/on 对照构建、auditd RSS 测量仍不可执行，文档披露诚实 |

未修复：

- **m-1 仍未修复。** `user_audit.rs:2007-2008` 的 queue_overflow gap 仍用被淘汰事件的
  `occurred_at` 作为 first/last_seen，未统一为丢弃观测墙钟（permanent_nack 与 supervisor 侧
  encode_error 同样用事件原时间，relay 侧用 `wall_now()`，语义不一致依旧）。

### 20.4 新发现问题

Major：

- **M-29 `tests/integration_audit.py:104` 存在必然 ImportError，Linux 端到端验证路径按现状不可
  通过。** 该文件从 `http_unix` 导入不存在的 `unix_http_request`（`http_unix.py` 只有 `request()`，
  签名与返回类型均不同；正确来源是同文件已导入的 `mock_collector.unix_http_request`，
  `mock_collector.py:845`）。collector 角色在 lease/ACK 全部成功后才执行到该 import，随后整个
  角色非零退出。即 M-23/M-25 交付的端到端验证（含 §12 health 步骤）在任何机器上都跑不过，
  只是从未在 Linux 执行而未暴露。修复：改从 `mock_collector` 导入，并在 Linux 环境实跑一次。
- **M-30 gap 写入在 §9.5 驱逐热路径上每次全量扫描并逐行解析整个 spool。** `spool.rs:2154`
  `write_gap_locked` 每次写 gap 前调用 `event_id_exists_in_epoch`（`spool.rs:4881-4926`），后者
  读取 open + 全部 sealed + 全部 acked 的 segment 正文并逐行 `parse_spool_line` + canonical 复验；
  对刚用随机 ID 生成的新 gap 也做该扫描（碰撞概率为零，纯属浪费）。容量到顶触发驱逐时，每驱逐
  一个 batch 都在全局 Mutex + current_thread runtime 下读完并解析整个 spool（极限约 5 GiB），
  必然超过 producer 3 秒 ACK 超时——M-18/M-22 消灭的丢失机制在过载场景重现。
  `find_spool_gap_reason`（`spool.rs:4830-4875`，reconcile QuarantinePending 每次调用）同构。
  修复：为已写 gap ID 维护内存集合（`unacked_gap_batches` 已有类似机制），或对新生成随机 ID
  跳过存在性扫描（固定 ID 重放路径才需要扫描）。
- **M-31 seal 持续失败时，每条 append 触发全量索引重建。** `seal_locked` 错误分支无条件
  `rebuild_runtime_indexes_locked`（`spool.rs:3265`），后者对整树 `directory_size` 并读取每个
  sealed batch 正文做 gap 检测；seal 持久失败（如 `sealed/` 被替换为普通文件）时，open 段超龄后
  每条 append 都走 best-effort seal → 失败 → 全量 rebuild，degraded 状态下 ingest 吞吐崩塌。
  修复：rebuild 结果做脏标记缓存，连续失败时降频（如 60 秒一次）。

Minor：

| 编号 | 位置 | 问题 |
| --- | --- | --- |
| m-66 | docs §19.4 | 验证清单中 `python3 tests/test_check_audit_static.py --source ...` 字面不可通过（exit 2，该文件无 argparse）；可跑的是 `tests/check_audit_static.py --source`。验证记录失真，需更正 |
| m-67 | `tests/README.md:46-48` | 仍描述整改 commit 已删除的"非 Linux 主机运行 user-audit 单元测试"路径；同文件 :35 的 workspace 表述也只在 Linux 成立 |
| m-68 | `user_audit.rs:2541-2563、2727-2740` | shutdown 残留窗口：backoff 睡前未预检 CLOSED_BIT 且 `notify_waiters` 无 permit 记忆；session 空闲等待在 `diagnostic_shutdown_requested` 消费前计算 due。close 落在帧 IO 窗口时可耗尽 2 秒 drain 预算；聚合计数仍进 final journald，不静默 |
| m-69 | `user_audit.rs:2508-2517` | session 意外 `Ok` 的兜底无退避无日志（当前不可达）；§7.3 要求意外 Ok 与 Err 同构（相同退避 + 限频 journald） |
| m-70 | `user_audit.rs` 多处 | 整改新增约 10 个无调用方 accessor 与一个只写字段（`supervisor_stopped`）；§17 m-4 点名的 `record_contention` 死代码原样保留；Linux feature-on `cargo check` 受环境阻断未验证警告清零 |
| m-71 | `spool.rs:1396-1414` | `ack()` 三个错误分支漏设 `non_gap_degraded`（append/lease 均有）：ack 路径真实故障可被无关 gap ACK 经 `refresh_degraded_after_ack_locked` 抹掉 degraded，与 m-46/M-21 的防护意图不一致 |
| m-72 | `spool.rs:2027-2035` | M-17 残留细节：persist_state 失败且 ftruncate 失败时仍扣减增量索引，磁盘字节领先内存索引直到下次 rebuild |
| m-73 | `spool.rs:1947-1957、3006-3008` | 元数据不可读对象的 quarantine 不维护 `sealed_batches`/`stored_records` 增量索引（两处），health 字段高估直到下次 rebuild |
| m-74 | `spool.rs:2991-3113` | `reconcile_tombstones_locked` 对单条目 I/O 错误 fail-fast（多处 `?`）：一个永久失败对象可 wedge 全部 append/lease/ack；C-3 的"单候选失败继续"未覆盖 reconcile |
| m-75 | `tests/golden_vectors.json`、`lib.rs:3455-3478` | golden vectors 残留：无 escaping 向量、无 NDJSON wrapper 行向量、nullable 仅一例、Rust 侧只断言数量不钉 key 集合 |
| m-76 | `mock_collector.py:512-514、832、128、601` | `_load_state` 缺 key 抛裸 KeyError；Content-Length 查找大小写敏感（与同函数查重逻辑不一致）；`canonical_request` 拒绝 timestamp "0"（略严于 §10.2）；`record_diagnostic` 的 CR/LF 检查未覆盖 `kind` |
| m-77 | `scripts/check-sensitive.sh:27` | 三类真实凭据形态不命中：`shared_i_psk`（cluster-users.py 源字段）、users[] 的 `password`、合并名 `export_hmac_key`；缓解：相关路径已 gitignore 且扫描跳过 ignored 文件 |
| m-78 | `tests/test_audit_packaging.py:63、100` | C-2 回归断言为整行字面匹配，等价重排写法可绕过；socket 0660 断言锚在 OPERATIONS.md 的 user-stats 段落，审计 socket 0660（auditd 代码常量）实际无测试锚定 |
| m-79 | `tests/test_fuzz_target.py:12-13`、`scripts/test-fuzz.sh:53-55` | fuzz 检查硬编码 `.cache/audit-work-source` 与被测树脱钩（test.sh 不传 `--source`，默认流程下可能检查陈旧树或 skip）；runner 未按 `fuzz/README.md` 要求使用 release+sanitizer |
| m-80 | docs §头部/§14.4 | 版本记录链断档："版本 2 变更"说明被整体替换、无 v3 记录、§17.6 第 1-4 条决策落点未记录；§14.4 仍残留"group sync"失效术语（v4 已删 group commit） |
| m-81 | `auditd/Cargo.toml:18`、`spool.rs:1123-1139`、`export.rs:132-135` | `rand` 依赖仍未使用未清；append 错误分支 `non_gap_degraded` 重复赋值两行（笔误）；export 注释与实际取锁路径不符 |
| m-82 | `tests/check_audit_static.py:16` | unwrap/越界静态护栏只覆盖 auditd crate 与 user_audit.rs；protocol crate（全部 wire parser/HMAC）与 config/mod 接线无护栏 |
| m-83 | `tests/integration_audit.py:85-93、128-156` | producer 角色只读帧不解析 ACK 内容（NACK 也判通过）；Linux 非 root/缺账号时打印后 exit 0，"强制"仅保证文件存在；无伪 UID/错误 node 负例 |
| m-84 | `spool.rs:1710-1726` | 观察项：周期 sweep 在全局锁内做目录列举+逐候选 fsync，collector 长期离线后恢复时单次 sweep 可阻塞 runtime；§14.5 复测应覆盖 |

### 20.5 交付一致性核查结论

- 补丁↔源码树：`patch --dry-run -R -p1 --fuzz=0` 对 0003 全部 48 个文件干净反向应用，逐字一致。
- 合同文本：`07a3b83` 对规格为纯插入（仅 §18）；`4a6348e` 对 §1–16 共 9 处 hunk，全部落在
  v4 决策 6-11 与 §17.6 第 1/3/4 条对应范围，无夹带变更；示例配置与 v4 文本逐字一致，
  `group_commit_*`/`acked_retention_seconds`/`udp_target_window_seconds` 全仓库零残留。
- §19.4 已执行清单已按实际证据更正：可移植 cargo/Python 项和 `check-sensitive.sh`（本机已有 `rg`）
  复跑一致；Linux-target auditd check、完整 `scripts/test.sh`、Linux runtime/fuzz/性能/容量矩阵不在
  该次通过清单中，不能用可移植结果替代。
- golden vectors 双端消费链路（仓库文件 ↔ 补丁内嵌副本 ↔ Rust `include_str!` ↔ Python 读取 ↔
  test.sh 逐字 cmp）闭合，向量内容经独立重算正确。
- 敏感信息：tracked 文件零命中；`.gitignore` 覆盖生成物与 key 文件名；git 工作树干净。

### 20.6 验收建议

1. 修复 M-29（一行导入错误）后在 Linux 环境实跑 `integration_audit.py`，坐实 §12 端到端路径；
2. 修复 M-30/M-31（gap ID 内存索引/随机 ID 免扫描；rebuild 降频）后再做 §14.5 目标机压测；
3. m-1 按 §18.4 判定统一为观测墙钟；M-4 的 contention 运行期聚合 journald 补齐；
4. 更正 §19.4 失真命令（m-66）与 tests/README.md 过时描述（m-67）；
5. m-68/m-69 的 shutdown 窗口建议同批收紧（睡前预检 CLOSED_BIT/freeze 标志）；
6. 其余 minor 可排期；Linux runtime、fuzz 实跑与完整容量/crash 矩阵仍为发布前置项。

## 21. 第四轮代码审计记录（2026-08-28）

> 本节是对整改 commit `4a6348e` 的第四轮审计，不改变合同条文。编号接续：critical 自 `C-4` 起，
> major 自 `M-32` 起，minor 自 `m-85` 起。行号基于当前 `patches/0003-user-audit.patch` 应用树
> （已用 `patch --dry-run -R -p1 --fuzz=0` 对全部 48 个文件确认逐字一致）。

### 21.1 审计范围与方法

- 对象：整改后全部交付物。重点是**整改自身引入的回归**——`spool.rs` 2999→5406 行、
  `user_audit.rs` 2654→3616 行、`protocol/lib.rs` 3349→3516 行，以及 6 个新增测试/工具文件。
- 方法：12 维度并行审查（producer 诊断/shutdown/热路径、protocol 与 golden vectors、auditd
  配置与生命周期/ingest/spool 索引/spool 事务/spool 恢复/export 与 health、测试工具、打包文档），
  每条候选交独立复核者对抗性反驳（73 条候选 → 驳回 8 条、保留 65 条）；两个 critical 由编排者
  亲自对照 `0f52bd9` 与当前补丁逐行复核；多条发现由复核者在私有副本树上**实测复现**（实验后
  经反向 patch 确认源码树逐字还原）。
- 本机实跑（macOS）：`cargo check` feature-off 通过（2 条警告）、`cargo test -p
  shadowsocks-audit-protocol` 20 项通过、`cargo test --workspace --features user-stats` 302 项通过、
  `test_mock_collector` 10 项、`test_audit_packaging` 6 项、`test_release_artifact` 7 项、
  `test_http_unix` 14 项、`test_check_audit_static` 5 项、`check_audit_static.py`、
  `test_benchmark_audit` 2 项、`test_panic_abort`、`check-sensitive.sh` 均通过；
  `test_fuzz_target` 2 项中 1 项 skip、`integration_audit.py` 整体 skip。
- 本机失败/不可执行：`cargo test -p shadowsocks-service --features user-audit --lib`
  （被 `crates/shadowsocks-service/src/lib.rs:56` 的新 `compile_error!` 拦截）、
  `cargo check --target x86_64-unknown-linux-gnu -p shadowsocks-auditd`（该 target 未安装）、
  因而 `scripts/test.sh` 在本机也会在 `test.sh:100` 硬 `die`。
- 未做：Linux runtime 实跑、fuzz 实跑、性能压测（与 §19.4/§20.1 的披露一致）。

### 21.2 总体结论

§19 的修复方向正确，§20 对其的核查也基本准确；但本轮发现**整改质量存在系统性问题**：

1. **两个 critical**。`C-4` 是三轮审计全部漏检的既存缺陷——auditd 在真实 Linux 上**根本无法
   启动**，整个功能零可用；`C-5` 是整改引入的新回归——容量清理在正常水位场景下变成永不退出的
   销毁性死循环，比它所修复的 `C-3`（立即报错自锁）后果严重得多。
2. **回归比例高**。65 条保留发现中 20 条标记为 regression、5 条为 incomplete_fix。整改在修好
   一个问题的同时引入了新问题的模式反复出现：`C-3` 解除容量短路 + 把 seal 挪进循环 → `C-5`；
   `C-2` 加 `InaccessiblePaths` → `M-40`；`M-18` 改增量索引 → `M-36`/多条索引漂移；
   `M-22` 改增量 health → 损坏检测能力丢失；`M-12` 改 round-robin → 空转与死代码。
3. **两条交付的回归测试本身必然失败**（`M-35`、`M-39`），加上 §20 已记的 `M-29`，现在共有
   3 条“已交付的验证”在任何机器上都跑不过。根因相同：Linux-only 代码从未在 Linux 上执行过，
   而本机被 `compile_error!` 拦截，`cargo check` 只做类型检查。
4. **验证声明失真扩大**。§19.4 清单中的两条 cargo 命令在本机不可能通过（缺 Linux target），
   §20.5“cargo 侧均复跑一致”因此不成立；§20.5 关于“本机缺 `rg`”的说明也与事实不符
   （`rg` 已安装，`check-sensitive.sh` 实跑通过）。

**当前状态不适合进入第 16 节验收。** `C-4` 未修复前，任何 Linux 验收都无法开始。

### 21.3 Critical

- **C-4 两个 UDS 的属组设置使用 `fchownat(fd, NULL, …, AT_EMPTY_PATH)`，在 Linux 上必然失败，
  auditd 永远无法启动，审计功能在真实目标平台零可用。**
  `crates/shadowsocks-auditd/src/config.rs:876-928` 的 `secure_bound_socket_with_gid` 在 bind 后
  以 `O_PATH|O_NOFOLLOW|O_CLOEXEC` 打开 socket，然后执行：

  ```rust
  libc::fchownat(file.as_raw_fd(), std::ptr::null(), u32::MAX, expected_gid, libc::AT_EMPTY_PATH)
  // result != 0 -> return Err(io::Error::last_os_error());   // 无任何兜底
  ```

  `AT_EMPTY_PATH` 的 Linux 语义是“pathname 为**空字符串** `""` 时对 dfd 本身操作”，NULL 指针
  不是空字符串：内核 `do_fchownat → user_path_at → getname_flags → strncpy_from_user(NULL)`
  返回 `-EFAULT`（允许 NULL 的 `getname_maybe_null()` 是 6.13 才引入且未用于 `fchownat`，
  Debian 12 / Ubuntu 22.04·24.04 / RHEL 9 全部早于该版本）；glibc 与 musl 的 `fchownat` 都只是
  原样转发指针，不做 NULL→“” 归一化。该分支**没有兜底**，直接 `return Err`。

  退一步说，即使 `fchownat` 未报错也没有真正改到 gid，紧随其后的
  `validate_socket_metadata(&opened, expected_gid)`（`config.rs:917-920`）同样必然失败——
  `packaging/shadowsocks-auditd.tmpfiles:3-4` 给 ingest/export 目录的是 `0750` 且**未设 setgid**，
  unit 以 `Group=shadowsocks-audit` 运行，因此 `bind()` 产生的 inode 主组绝不可能自然等于
  `shadowsocks-audit-ingest`/`-export`，这次 chgrp 不可省略。两条路都是 `Err`。

  旁证：同一函数紧接着的 `fchmodat(..., AT_EMPTY_PATH)`（`config.rs:905-916`）同样非法
  （`fchmodat(2)` 从不支持 `AT_EMPTY_PATH`），作者为它写了 `/proc/self/fd/N` 兜底，却唯独
  没给 `fchownat` 写。同仓库已在生产运行的 `user_stats.rs:949-994`
  （`set_socket_permissions_nofollow`）采用的正是 `O_PATH` + `/proc/self/fd/N` 方案，并明确注释了
  内核/glibc 版本限制——auditd 这段没有沿用。

  后果：Linux 节点按 `docs/OPERATIONS.md:96-111` 安装后 `systemctl start shadowsocks-auditd`，
  `IngestServer::run`（`ingest.rs:199`）与 `ExportServer::run`（`export.rs:175`）在 bind 后立即
  失败 → `AuditDaemon::run` 返回 Err → 进程退出 → `Restart=on-failure` 死循环，
  `ingest.sock`/`export.sock` 从不出现，ssserver producer 永远连不上，§6 的两类成功访问事件
  一条也进不了 spool。§11「两个 socket `0660 shadowsocks-audit:<对应组>`」与 §16「UDS 双向
  `SO_PEERCRED`、分组权限通过」在真实 Linux 上无法达成。

  **这不是整改引入的回归**：`git show 0f52bd9:…/patches/0003-user-audit.patch:4473-4507` 显示
  该写法自首次交付起就存在。三轮审计全部漏检，原因是该路径零单元测试覆盖
  （`config.rs` 的 13 个测试均不调用 `secure_bound_socket*`），而非 Linux 主机只做
  `cargo check`，类型检查不会暴露运行期 `EFAULT`。

  修复：pathname 改用空字符串常量（`c""`），或直接照搬 `user_stats.rs:949-994` 的
  `/proc/self/fd/N` 方案同时完成 chown 与 chmod；并补一条 Linux 运行期回归断言
  「bind 后 socket 为 `0660` 且 gid 等于父目录组」。

- **C-5 整改把 seal 移进容量清理的 while 循环，与“gap 写入不再受容量水位约束”叠加，形成
  seal→evict→写 gap 的永不退出死循环：auditd 全局锁被永久占用、三条服务路径全部挂死，同时以
  fsync 风暴持续销毁真实审计数据。**
  整改前（`0f52bd9` 版补丁第 8169-8171 行）“必要时先 seal open”在 `while` **之外**只执行一次，
  候选耗尽后必然 `break` 并返回 `StorageUnavailable`。整改后（`spool.rs:2386-2402`）变成循环体
  末尾的 `if inner.open_meta.event_count > 0 { seal_locked(...); continue; }`；而
  `evict_sealed_locked`（`spool.rs:2851-2911`）在删除 sealed batch 后**必然**经
  `write_gap_locked`（`spool.rs:2905`）向 open 段写一条 gap——按 `C-3` 的整改该函数刻意不再受容量
  水位约束（`spool.rs:2190-2194`）。于是每轮：seal 上一轮的 gap → 它成为新的 sealed 候选 →
  驱逐它 → 再写一条新 gap 回 open → 再 seal……`progressed` 每轮为真，`spool.rs:2402` 的 `break`
  永不可达。

  `capacity_ok`（`spool.rs:2201-2207`）= `size_ok && free_space_ok`，而
  `free_space_ok`（`spool.rs:4721-4736`）只读 statvfs 的 `f_bavail`：**spool 自己删空也不会变
  true**（空间被同一文件系统上的其它文件占用），statvfs 失败时同样恒 false。tombstone ledger
  满不是出口（`add_tombstone_locked` 淘汰最老 receipt 后继续）；真实 ENOSPC 也不是出口（1 GiB
  水位远早于写失败触发，且循环净字节持平）。

  两条独立复现（均在私有副本树，实验后源码树经反向 patch 确认逐字还原）：
  - `min_free_bytes=u64::MAX`、先写 3 条事件后重开再 append 第 4 条 → 45 秒未返回，
    `next_spool_sequence` 从 17 涨到 498，sealed 目录反复出现/消失，tombstone receipt 累积到 1532 条；
  - 79 条事件 / 3 个 sealed batch 场景 → 单次 `append()` 持锁运行 4 分 38 秒未返回，CPU 40%，
    spool 目录大小恒定（纯空转）；同配置下**空 spool** 对照组立即返回 `Err(StorageUnavailable)`，
    证明分支判定无误。

  触发条件是 §9.5 明确列为正常运行场景的水位：节点 `/var` 所在文件系统被其它服务（journald、
  镜像层、core dump）占到可用空间低于 `min_free_bytes`（默认 1 GiB）——**此时写入本身仍成功**，
  故不会被 ENOSPC 打断。另有两条同源触发路径：statvfs 失败，以及 `M-36` 把 `spool_bytes` 永久
  锁存为 `u64::MAX`（只需 spool 树里出现一个符号链接或一次瞬时 `read_dir` 错误）。

  后果：`append_parsed`（`spool.rs:1171`）持有全局 `Mutex<SpoolInner>` 永不释放，`lease`(1220)、
  `ack`(1391)、`health`(1597) 全部阻塞在同一把锁上——**`healthz` 连 degraded 503 都返回不了**
  （比 `C-3` 时代“healthz 仍可用”更差，唯一的可观测信号也没了）；期间每秒十余轮 seal/evict/
  fsync，把 spool 内全部 batch 连同记录其丢失的 gap 反复销毁，`evicted_unacked_records` 无界
  增长；§9.5 第 6 条要求的“不分配 spool sequence、返回 retryable `storage_unavailable`、累计
  `storage_rejected_attempts`”永远无法执行；producer 侧 3 秒 ACK 超时全部落空，队列溢出丢事件。
  需人工 kill 并扩容磁盘才能恢复。ssserver 代理流量本身不中断。

  修复：把“seal 活跃 open”恢复为每次 cleanup 至多一次（局部 `sealed_once` 守卫），并给 while
  增加显式进度判据（一轮内没有净减少字节即 `break`）与迭代上限，超限按 §9.5 第 6 条返回
  `StorageUnavailable`；`spool.rs:2389` 的分支还应排除“open 段只含本次 cleanup 自己写入的
  诊断记录”这一自激情形。

### 21.4 Major

- **M-32 `saturating_add_atomic` 在 4 次 CAS 失败后无条件写入 `u64::MAX`，把并发累加伪造成
  “至少 1.8e19”的灾难性缺口。** `user_audit.rs:673-689`：

  ```rust
  let mut current = atom.load(Ordering::Acquire);
  for _ in 0..GAP_FALLBACK_CAS_ATTEMPTS {          // user_audit.rs:572，= 4
      if current == u64::MAX { return; }
      let next = current.checked_add(amount).unwrap_or(u64::MAX);
      match atom.compare_exchange_weak(...) { Ok(_) => return, Err(actual) => current = actual }
  }
  atom.store(u64::MAX, Ordering::Release);          // 与真实计数无关
  ```

  4 次 CAS 失败只证明存在并发争用，与计数是否接近上限无关。饱和后循环入口的
  `if current == u64::MAX { return; }` 还会让该 atomic 在下次 `take()` 清零前静默丢弃所有后续
  真实计数。可达调用点为 `GapFallback::merge` 的 count-only 分支（`user_audit.rs:594`）与
  `merge_locked`（`601`）：当 8 个 `GapSlot` 的 `try_merge` 全部失败（`GapAccumulator::merge`，
  `718-732`）时，多个 relay 线程一起对同一个 `count` 做 CAS。复核者用独立程序复刻该函数实测：
  2 个线程即在极短时间内读出 `18446744073709551615`，真实计数仅数十；单线程 100 万次从不饱和
  （故根因是真实争用，不是 weak CAS 伪失败）。
  后果：`take_diagnostic_round_robin` 取出该 count 后直接构造
  `"skipped_successful_datagrams": "18446744073709551615"` 的 `udp_window_contention`，或
  `"dropped_events": "18446744073709551615"` 的 `producer_gap`；protocol crate
  （`lib.rs:836,900`）只拒绝 0、不设上限，原样上线。§6.5 定义 `dropped_events` 为“精确累计值”，
  §12 第 5 条要求 collector 对其明确告警——真实跳过量只有数十时，controller 会把它升级为节点级
  灾难告警。§7.2 要求永久 NACK 时把 snapshot 的“原始计数”合并回 accumulator，经该函数同样可能被
  改写为 `u64::MAX`。
  **既存缺陷**（`0f52bd9` 版补丁 13263-13281 行与当前逐字相同），三轮审计均漏检。
  修复：改用无重试预算的 `fetch_update`（本文件其它计数器已这样用，见 `1983-1987`、`2419-2423`），
  使 `u64::MAX` 只在真实溢出时出现；若必须保留有界重试，超预算的余量应记入独立的
  `merge_lost` health counter 并置 degraded，而不是伪造 on-wire 计数。（§6.5 §7.2 §12）

- **M-33 session 空闲等待缺少 `emitter.queue` 复检，drain 抢走唯一 `notify` permit 后 supervisor
  睡满 60 秒，2 秒 drain 必然失败并丢弃整条 access queue。** `connect_and_send_session`
  （`user_audit.rs:2723-2740`）在注册 `emitter.notify.notified()` 后只复检本地 `state.pending`，
  **从不复检 `emitter.queue`**；而 `run_supervisor`（`2486-2489`）保留了该复检，整改前的单循环
  实现也有——是 `4a6348e` 复制空闲块时漏掉的一行。tokio `Notify` 只保存单个 NOTIFIED permit：
  relay 在 session 阻塞于 `read_frame_with_deadline` 期间 `try_push(E1)` + `notify_one()` 留下
  permit，`close_emitter()` 与 drain 首轮的 `notify_waiters()` 在无 waiter 时保留/消耗该 permit，
  等 session 回到空闲块注册 waiter 时 permit 已被抢走，于是睡满 60 秒空闲超时。
  §7.3 的 `timeout(2s, drain())` 到期，queue 中最多 4096 条 access event 与全部 in-flight 随内存
  丢弃。违反 §7.2「consumer 每次注册 notified future 前后都检查 queue，避免 lost wakeup」。
  修复：在 `2723-2740` 的空闲块补上与 `2486-2489` 相同的 `emitter.queue` 复检。（§7.2 §7.3）

- **M-34 UDP 窗口缓存重写把每个窗口的 key 材料从 1 份变成 4 份，默认 65536 窗口下缓存本身实测
  可达 110 MB，突破 §14.5 的 64 MiB RSS 预算。** `user_audit.rs:830-840` 的 `UdpWindowEntry`
  同时持有 `key` 与 `raw_key`，`842-853` 的 shard 同时维护 `by_key` 与 `by_raw_key` 两张 map，
  `insert_entry`（`1122-1123`）两次写入完整 key 拷贝，而 `UdpWindowKey`（`796-802`）内含 `host`
  与 `kind` 两个 `String`。复核者在同一二进制内做新旧布局对照实测（填满 65536 窗口）：
  255 字节域名 110.4 MB（1766 B/窗口）vs 整改前 42.4 MB（678 B/窗口），2.6 倍；
  20 字节普通域名 54.5 MB（872 B/窗口）vs 整改前 12.2 MB（196 B/窗口），4.5 倍。
  即便是普通域名也已占满 §14.5 预算的 85%，255 字节域名场景（已认证用户控制一个通配 DNS 区即可
  构造）直接超出 72%。修复：`by_raw_key` 只存 key 的引用/索引而非第二份完整拷贝，或把
  `UdpWindowKey` 的 host 换成 `Arc<str>` 让四处共享同一份分配。（§14.5 §6.4）

- **M-35 `validate_absolute_path` 无法拒绝 `.` 组件，§5.3 的路径规则失效，且两条交付的回归用例
  在 Linux 上必然 panic。** `config.rs:462-490` 对 `.`/`..` 的判定全部依赖 `path.components()`
  （`479`），而 `std::path::Components` 会把绝对路径中的 `.` 全部归一化掉——本机 rustc 实测
  `/a/./b → [RootDir, Normal("a"), Normal("b")]`、`/./a → [RootDir, Normal("a")]`——因此 `482` 的
  `Component::CurDir` 分支对绝对路径**永远不可达**；`474` 的词法检查只覆盖尾 `/` 与 `//`。
  后果一（规格偏离）：`shadowsocks-auditd --config /etc/shadowsocks-audit/./auditd.json` 被接受，
  配置中 `"spool_dir": "/var/lib/./shadowsocks-audit"`、
  `"export_hmac_key_file": "/etc/shadowsocks-audit/./export-hmac"` 同样通过 `validate()`，
  而 §5.3 要求这些拼写在任何 socket/文件创建前失败。
  后果二（验收阻断）：`config.rs:1069` 的 `paths_reject_variants` 与 `lib.rs:207` 的
  `config_path_rejects_noncanonical_spellings` 两个用例数组都含 `/a/./b` 并 `assert!(...is_err())`，
  在 Linux 上执行 §14.4 要求的 `cargo test -p shadowsocks-auditd` 时必然 panic。
  修复：在词法层直接检查 `/./`、`/../`、以 `/.`/`/..` 结尾的拼写，不要依赖 `components()`。（§5.3 §14.1）

- **M-36 `rebuild_runtime_indexes_locked` 把 `directory_size` 失败永久锁存为
  `spool_bytes = u64::MAX`，一次瞬时错误即导致全部 spool 数据被销毁并永久拒收。**
  `spool.rs:1639-1648` 在 `directory_size` 失败时 `mark_storage_rejection` 后把 `spool_bytes` 置为
  `u64::MAX`；整改后 `capacity_ok`（`2201-2207`）**只读该增量索引、不再现算**，而 rebuild 只在三处
  被调用（`Spool::open:1064`、`recover_runtime_temp_objects_locked` 的 changed 分支 `2513`、
  `seal_locked` 错误分支 `3265`），正常路径上没有任何重测机会。
  而 `directory_size`（`4699-4718`）用 `symlink_metadata` 且对**任何**非普通文件/目录项
  （符号链接、FIFO、socket、设备节点）返回 `Err(InvalidData)`（整改前返回 `Ok(0)`），
  `read_dir` 的 EIO/EMFILE 同样上抛。因此运维在 spool 根或任一子目录留下一个诊断用符号链接、
  或重启瞬间撞上一次 `read_dir` 错误，`Spool::open` 仍会成功返回，但 `spool_bytes` 被终身锁死为
  `u64::MAX` → `capacity_ok` 的 `checked_add` 恒为 `None` → 第一条 producer 事件即触发
  `cleanup_locked`。**与 `C-5` 叠加后果最严重**：不需要磁盘真的满，一个符号链接就足以让 auditd
  进入永不退出的销毁性死循环。错误条件消失后也不自愈，必须重启进程。
  修复：`directory_size` 对非文件/目录项计 0 并记 degraded 而非整体失败；`spool_bytes` 未知时应
  触发一次重测而不是永久锁存；`capacity_ok` 在索引处于“未知”状态时走 §9.5 第 6 条拒绝路径。
  （§9.5 §10.1）

- **M-37 清理候选循环在单个候选失败后不复检容量，一次 cleanup 会把整个未导出 sealed 积压全部
  删除并对外返回 `Ok`。** `cleanup_locked`（`spool.rs:2220-2412`）的容量条件只在外层
  `while !self.capacity_ok(...)`（`2245`）顶部求值；acked（`2263-2307`）、quarantine
  （`2323-2336`）、sealed（`2369-2382`）三个候选 `for` 循环在候选返回 `Err` 时只记录
  `first_error` 就落到下一个候选，中间没有任何 `capacity_ok` 复检。但“失败”的候选其实**已经释放
  了空间**——`evict_sealed_locked` 的删除发生在 `2879 remove_tree_tracked`（已扣减
  `spool_bytes`）/`2880 sync_dir`，其后 `2905 write_gap_locked`、`2913 replace_tombstone_locked`
  才可能失败并 `return Err`；`evict_quarantine_locked`（`2950` 删除 → `2969` 写 gap）与 acked
  循环（`2294`）同构。于是当出现落在删除动作之后的持久失败（`sync_dir(sealed/)` 持续 EIO、
  §20 M-31 描述的 seal 持续故障使 `write_gap_locked` 内的 `seal_locked(inner)?` 恒失败等），
  明明只需释放一个 batch 的容量压力，却会把 `sealed/` 下**全部**未 ACK batch（最坏 5 GiB）逐个
  删光，然后返回 `Ok`——append 正常 ACK、producer 毫无感知。这直接违反 §9.5 清理顺序“若仍超限
  才进入下一级”的逐级语义。修复：每删除一个候选后立即复检 `capacity_ok`，满足即退出循环；
  候选失败但空间已释放的情形必须计入进度并复检。（§9.5）

- **M-38 open 段的 `first/last_received_at` 按写入顺序记录而非取 min/max，接收时间一旦非单调，
  封口永久失败，append 与 lease 全部长期返回错误且重启不自愈。** `update_open_metadata`
  （`spool.rs:4060-4067`）用 `first_received_at.get_or_insert(received)` /
  `last_received_at = Some(received)`，无归一化；`seal_locked`（`3191-3196`）把它们直接交给
  `wire::SpoolMeta::new`，而 `SpoolMeta::validate`（audit-protocol `lib.rs:1500-1502`）拒绝
  `first > last`。seal 失败发生在 `open→sealed/.tmp` rename 之后，`recover_seal_failure` 把段原样
  搬回并保留 in-memory `open_meta`，故失败可无限重复；重启时 `scan_open_bytes`（`3967`）以同一
  逻辑重放，同样不隔离该段。两条触发路径：
  (a) 墙钟向后 step（chrony makestep、VM 快照恢复、启动时 RTC 校正，跳变常达小时级）；
  (b) 一次 crash recovery 同时产生 `state_reset` gap 与 corruption/tail-truncation gap 时，两条
  gap 的 `received_at` 递减（`write_gap_locked:2194` 用 `spec.occurred_at` 当 received_at）。
  后果分两级：只要 open 段非空且非单调，`lease()`（`1241-1247`）立即返回错误，export 全线阻断
  ——恰恰是“数据丢失证据”本身永远无法被 collector 取走；一旦该段涨到 `segment_max_bytes`，
  append 的“先 seal 再写”分支（`1161-1168`）在写入前失败，`ingest.rs:401-405` 把
  `SpoolError::Protocol` 映射为 retryable 的 `internal_error` NACK 并断开，producer 退避重连后
  再次收到同样 NACK，形成永久重连循环，此后所有成功访问事件全部丢失。
  修复：`update_open_metadata` 对 first 取 min、对 last 取 max（或在 seal 前归一化），
  并对已倒置的历史 open 段走 quarantine + gap 而不是无限失败。（§9.3 §9.4）

- **M-39 交付的回归用例 `empty_interrupted_seal_directory_is_reclaimed` 必然失败。**
  `cleanup_recovered_temp_files`（`spool.rs:4622-4691`）在扫描阶段对缺失目录容错
  （`4635` `NotFound => continue`），但收尾的同步阶段（`4685-4689`）对固定的 5 个目录
  `[root, open, sealed, acked, quarantine]` 无任何容错。随 `4a6348e` 交付的用例
  （`spool.rs:5298-5309`）只创建了 `sealed/`，删除孤儿后 `changed = true`，随即
  `sync_dir(root/open)` 返回 ENOENT，`5306` 的 `.expect("cleanup")` panic（实测错误为
  `Os { code: 2, kind: NotFound }`；补建另外三个目录即通过，根因确证）。该失败为纯 ENOENT，
  与平台无关，Linux 上执行 `cargo test -p shadowsocks-auditd` 时 100% 失败并使整个 crate 测试
  套件 FAILED。次生影响：运行期 `cleanup_locked → recover_runtime_temp_objects_locked` 调用同一
  函数，若 `quarantine/` 等目录在运行中被移除，删除临时文件后的 fsync 会把整条容量路径变成 I/O
  错误。修复：同步阶段跳过 `NotFound`。（§14.4 §19.2 M-26）

- **M-40 `C-2` 修复新增的 `InaccessiblePaths` 未加 systemd 的 `-` 可选前缀，auditd 在 ssserver
  未运行时启动即 226/NAMESPACE 失败。** `packaging/shadowsocks-auditd.service:36`
  `InaccessiblePaths=/etc/shadowsocks-rust-plus /run/shadowsocks-rust-plus`，而
  `/run/shadowsocks-rust-plus` 全仓库只由 `packaging/shadowsocks-rust-plus.service:13` 的
  `RuntimeDirectory=`（同样无 `RuntimeDirectoryPreserve=`）创建、ssserver stop 时即被删除、
  `/run` 为 tmpfs 重启后不存在；`/etc/shadowsocks-rust-plus` 在 `packaging/README.md` 第 4 步
  之前也不存在。三条必然触发路径：
  (1) 全新节点按 `packaging/README.md` 第 3 步先装 auditd（第 4/5 步才装 ssserver）→
  `systemctl enable --now shadowsocks-auditd.service` 立即失败，§15.2 第 3 步「auditd 运行但
  ssserver 尚未启用 producer」的验证步骤无法执行；
  (2) 已上线节点重启主机：auditd 因 `Before=shadowsocks-rust-plus.service` 先启动，此时
  `/run/shadowsocks-rust-plus` 尚不存在 → 失败；
  (3) `docs/OPERATIONS.md:406` 的升级屏障要求「先停两个服务，再先启动 auditd」→ 停 ssserver 已
  删掉该目录 → auditd 起不来。
  这是 `C-2` 修复（为落实 §11「auditd 不得能读取 ssserver config」）引入的新回归，方向与 `C-2`
  相反：这次是审计侧被数据面的生命周期绑架。修复：两个路径都加 `-` 前缀。（§11 §15.2）

### 21.5 对 §19/§20 结论的更正

- **§19.4「已执行验证」清单有两条在本机不可能通过。** 本机未安装 `x86_64-unknown-linux-gnu`
  target（`rustup target list --installed` 只有 `aarch64-apple-darwin`），因此
  `cargo check --locked --target x86_64-unknown-linux-gnu -p shadowsocks-auditd --all-targets`
  实测以 `error[E0463]: can't find crate for core — the target may not be installed` 失败；
  `bash scripts/test.sh` 也会在 `scripts/test.sh:100` 的
  `[[ -d "$audit_libdir" ]] || die` 处硬失败。§20.5「§19.4 已执行清单……cargo 侧与 Python 侧均
  复跑一致」因此不成立。建议：把该 target 写成显式前置条件（README/tests/README 目前只在
  `tests/README.md:48` 提到环境变量名），并在 §19.4 中如实标注该条依赖。
- **§20.5 关于 `check-sensitive.sh` 的说明与事实不符。** `rg` 已安装，
  `bash scripts/check-sensitive.sh` 本机实跑通过（rc=0）；§20.5 的“本机缺 `rg`”应删除。
- **§20.3 把 `M-28` 记为“已修复”不准确。** 共享 `tests/golden_vectors.json` 确实建立并双端消费
  （链路闭合、内容经独立重算正确），但其顶层 key 仅
  `hmac_key_hex`/`request`/`response`/`records`——`M-28` 自身点名的 **NDJSON wrapper 行 golden
  vector 仍完全缺失**，§14.4「protocol JSON/**NDJSON**/HMAC golden vectors 在 Rust 与 mock
  collector 间逐字一致」这一条并未达成。应改记为“部分修复”（与 §20 已记的 `m-75` 合并）。
- **`M-27` 的修复引入了新的验证真空。** `crates/shadowsocks-service/src/lib.rs:54-56` 新增的
  `compile_error!` 使 producer 侧全部单元测试在任何非 Linux 主机上无法编译，而
  `scripts/test.sh:60-62` 在非 Linux 上把 workspace features 降级为 `user-stats`。于是
  `user_audit.rs`(25)、`udprelay.rs`(10)、`server/mod.rs`(4)、`tcprelay.rs`(2)、`context.rs`(1)
  共约 42 个 producer 侧测试**只能在 Linux 上执行**——而 Linux runtime 迄今从未执行过
  （§19.4/§20.1 自述）。整改前这些测试在 macOS 上是可跑的（第二轮审计实测 71 项通过）。
  这不是要求回退 `compile_error!`（Linux-only 是 §5.1 的合同），而是必须把“在 Linux 上跑一次
  完整 feature-on 测试”列为发布前置硬条件，否则该批测试等同于未交付。
- **`M-31`（§20）的实际后果比记录的更重。** §20 记为“seal 持续失败时每条 append 触发全量索引
  重建，吞吐崩塌”；结合本轮 `M-37`，同一场景下 `write_gap_locked` 内的 `seal_locked(inner)?`
  恒失败会使清理循环把全部未 ACK sealed batch 删光并返回 `Ok`，是数据丢失而不只是性能问题。
- **§20 的 `m-71`（`ack()` 漏设 `non_gap_degraded`）范围偏小。** 除已记的三个分支外，
  `Spool::ack` 的 rename、双目录 fsync、receipt 落盘四个失败分支（`spool.rs:1529-1533`、
  `1534-1538`、`1539-1543`、`1564-1568`）同样漏设，使无关 gap 的 ACK 可经
  `refresh_degraded_after_ack_locked` 抹掉 ACK durability barrier 故障。
- **§19.2 关于“带回归测试”的表述对 `M-12` 不成立。** per-bucket deadline + round-robin cursor
  是该条修复的核心机制，但 `diagnostic_cursor`（`user_audit.rs:1336/1347/1363/2396/2402`）在整个
  测试集中零覆盖，唯一新增的诊断测试（`3219-3232`）不触及调度顺序。

### 21.6 Minor

| 编号 | 位置 | 问题 |
| --- | --- | --- |
| m-85 | `user_audit.rs:1353-1364` | `mark_diagnostic_attempt` 的非 contention 分支是语义空操作（死代码），掩盖了“正常 snapshot 不限频”的真实语义 |
| m-86 | `user_audit.rs:1698-1701`、`2361-2371` | `diagnostic_retry_buckets` 的位在 `fill_pending` 内循环只读不清，失败 bucket 的 60 秒重试被重复应用一次（实际推迟 60 s + 两次 fill 间隔） |
| m-87 | `user_audit.rs:1413-1459`、`1730-…` | §6.5 要求的“contention 计数达到 `u64::MAX` 时置 producer health degraded”未实现，`AuditHealthSnapshot` 无饱和维度 |
| m-88 | `user_audit.rs:2565-2582` | 整改把生产路径改名为 `connect_and_send_session` 后，旧的 `connect_and_send` 沦为仅测试可达的非 `cfg(test)` 死函数；feature-on `cargo check` 实测 12 条警告（`m-70` 的加深） |
| m-89 | `server/mod.rs`、`context.rs` | §7.3 固定 SIGTERM 顺序被 `stop_accepting()` 的传递性破坏：relay task 在 `close_emitter`/`drain` 之前就被全部 abort，而非规格要求的“drain 之后” |
| m-90 | `context.rs:36-42,49-62` | `RelayTaskGuard::drop` 与 shutdown 共用同一个 `Notify`，每条 relay 任务结束都广播唤醒 accept loop 与全部 UDP 接收 worker（整改引入） |
| m-91 | `user_audit.rs:1194-1233` | `eligible_lazy` 把 UTS #46/lowercase 域名规范化搬进了 shard mutex 临界区，交替拼写可让每个数据报都触发锁内规范化 + 两次 HashMap 改写；§6.4 要求临界区只做有界 lookup/LRU |
| m-92 | `tcprelay.rs:452-459`、`udprelay.rs:630-634` | 审计 identity handle 在 emitter 存在性检查**之前**求值，每条 TCP 连接与每个 UDP session 白付两次 RwLock 读 + 一次 String 分配 |
| m-93 | `audit-protocol/src/lib.rs:2849` | 204 与全部非 lease 响应的 canonical“空字段”分支在两侧均零覆盖：无 golden vector、无单测正例 |
| m-94 | `audit-protocol/src/lib.rs:234-235` | `ProtocolError::DuplicateKey` 携带的 key 名被 serde 的位置后缀污染，重复 key 诊断信息永远是乱码 |
| m-95 | `tests/mock_collector.py:305,356-410` | 参考 collector 从不校验任何 canonical 字节形态：乱序 wrapper、内嵌事件的等价转义写法均被接受，与 §9.2「逐字相等」的示范意图相反 |
| m-96 | `audit-protocol/Cargo.toml:20` | 声明了全树零引用的 dev-dependency `serde_bytes`（`m-81` 同类） |
| m-97 | `spool.rs:4526-4582`、`812-820` | `M-15` 修复不完整：`SpoolLock` 没有 `verify()`，`spool_dir/.lock` 被删除或替换后两个 auditd 仍可并发写同一 ledger |
| m-98 | `auditd/src/config.rs:686,746,810,…` | 6 个 `pub(crate)` socket 路径辅助函数全无调用方，其中一个注释还声称“retained for low-level tests”，在唯一受支持的 Linux 目标上产生 dead_code 警告 |
| m-99 | `user_audit.rs`（退避重置点） | 整改新增的“hello ACK 即重置退避”使 retryable `event_nack` 的重连退避永远停在 100 ms 下限，`storage_unavailable` 期间变成 10 Hz 热重连 + 256 条全量重放 |
| m-100 | `auditd/src/ingest.rs`（dedup） | dedup LRU 改 HashMap+token 后失去“最近 65536 条”容量保证：每次命中都挤掉一条别的活条目，重放场景下有效容量实测坍缩到 in-flight 规模（65536 → 256），违反 §8.3 |
| m-101 | `auditd/src/ingest.rs`（accept 恢复） | transient `accept()` 恢复只有固定 50 ms 睡眠：无退避升级、无连续失败上限、无诊断，且把 ECONNABORTED/ECONNRESET 这类单连接常态错误按资源枯竭处理；sleep 位于 select 分支内，阻塞 shutdown 与任务回收（`m-41` 修复不完整） |
| m-102 | `auditd/src/ingest.rs`（hello 版本） | §19 为 hello `protocol_version` 越界新增的 `unsupported_version` 分支不可达，配套回归测试是空测（在整改前代码上同样通过） |
| m-103 | `spool.rs:2878-2882`、`1752-1762`、`1581-1594` | `evict_sealed_locked` 在 `remove_tree_tracked` 之后、索引更新之前 `sync_dir(...)?` 早退，`unacked_gap_batches`/`sealed_received_at` 残留幽灵条目，health 从此永远无法从 degraded 恢复 |
| m-104 | `spool.rs:2949-2952` | `evict_quarantine_locked` 先删后同步再记账，`sync_dir` 失败使 `spool_bytes` 永久高估，反过来驱动 cleanup 继续淘汰真实 sealed 数据 |
| m-105 | `spool.rs:2294-2306`、`2574-2580` | `remove_tree_tracked(..).and_then(sync_dir)` 只回滚一半：字节已扣、`stored_records` 未扣 |
| m-106 | `spool.rs:4622-4691`、`2421-2426` | `cleanup_recovered_temp_files` 中途出错时丢弃已累计的 `removed_bytes`，调用方用 `?` 传播，`spool_bytes` 对已删除的临时文件永久高估 |
| m-107 | `spool.rs:2463-2510` | `recover_runtime_temp_objects_locked` 在一次 promotion 成功之后遇到 I/O 错误就 `?` 早退，跳过尾部 rebuild，被提升的 sealed batch 永不进入 `sealed_batches`/`stored_records` 索引 |
| m-108 | `spool.rs:1649`、`2019-2023`、`1756-1762` | 整改把未封口的 open 段计入 `oldest_unacked_at_unix_ms`，无 sealed/leased 批次时不再返回 null，与 §10.1「没有 sealed/leased unacked batch 时该字段为 null」明文相反（整改前只统计 sealed） |
| m-109 | `spool.rs`（quarantine 记账） | quarantine 驱逐在 sync/remove 失败时不回补字节记账，`spool_bytes` 永久高估直到下次全量 rebuild（`m-72`/`m-73` 同类） |
| m-110 | `spool.rs`（corruption 隔离） | corruption 隔离复用 `quarantine_pending` variant 并把 reason 硬编码成 `quarantine_eviction`，与 §9.5 的 variant 定义不符，且迫使恢复靠全树扫描反推语义 |
| m-111 | `spool.rs:3698-3709` | 新节点判定分支无条件清空 `degraded`，抹掉已记录的 `storage_rejected_attempts`，health 谎报 `ok`（`§19.3`「新节点不再伪造 state-reset」的副作用） |
| m-112 | `spool.rs:1529-1568` | `m-71` 漏记：`ack()` 在 rename／双目录 fsync／receipt 落盘失败的四个分支同样漏设 `non_gap_degraded` |
| m-113 | `spool.rs:1596-1631`、`1794-1804` | 整改后的 health 不再是 sealed batch 静默损坏的运行期信号：损坏对象仍计入 `sealed_batches` 且 status 保持 `ok`（整改前 health 会因 `read_batch` 失败置 degraded） |
| m-114 | `tests/check_audit_static.py:11-15,126-137` | 越界护栏对变量下标与切片完全失明，且 `unreachable!`/`todo!`/`unimplemented!` 不在禁止表——`M-27` 声称的 panic 静态护栏近乎零杀伤力 |
| m-115 | `tests/check_audit_static.py:132-136` | “附近有守卫”启发式会抑制**已被证明越界**的字面量下标；`test_check_audit_static.py:28-50` 未覆盖该抑制路径 |
| m-116 | `tests/check_audit_static.py:140-176` | `--source` 指向错误或空目录时静默输出“静态审计通过”并 exit 0；`test_check_audit_static.py:68-78` 把该行为固化为正向断言 |
| m-117 | `tests/check_audit_static.py:130` vs `:23-92` | 对注释与字符串字面量中的 `unwrap/expect/panic!` 误报——同文件已有的注释剥离器未被该检查使用 |
| m-118 | `tests/benchmark_audit.py:111,176,…` | `scenario_gate` 与 queue gate 恒为真（`"proxy_errors": 0` 是写死的字面量），`--enforce` 在无 data-path 报告时永远不会失败 |
| m-119 | `tests/benchmark_audit.py:86-120` | §14.5 的 5 个场景中 offline 与 queue_full 完全同构（共用同一 `queue_capacity`），healthy 场景自身报告 79.5% 事件丢弃，且 queue 丢弃从不产生 gap 记录 |
| m-120 | `scripts/test.sh:109-138`、`scripts/verify.sh` | `tests/benchmark_audit.py` 与 `tests/test_check_audit_static.py` 无任何脚本调用，是孤儿交付物 |
| m-121 | `tests/test_audit_packaging.py:30` | 凭据泄露断言是死断言：正则在 JSON 语法下永不匹配 |
| m-122 | `tests/mock_collector.py:488,547-548` | 默认路径（无 `--state`）在**零 durable 提交**下就发送 ACK，而 `integration_audit.py` 走的正是该路径——§12.4「durable 保存后才 ACK」的示范失效 |
| m-123 | `tests/integration_audit.py:102-111` | §12 health 步骤只验签名不验内容，503 也判通过——端到端“health 验证”实质为空 |
| m-124 | `packaging/release-toolchain.lock:8-9` | 把宿主 Python 的 zlib 版本（`1.2.12`，来自 macOS 系统 zlib）作为发布门禁，任何 Linux 发布机都会在构建开始前被拒 |
| m-125 | `scripts/release-artifact.py:128-160,308-492`、`scripts/verify-release.sh` | 仍保留单产物（无 `shadowsocks-auditd`）的 package/verify 通路，`_parse_manifest` 强制 `artifact.name == "ssserver"`，验签成功时打印全绿，绕过 §15.1 的六产物合同（`m-54` 未尽） |
| m-126 | `docs/OPERATIONS.md:293-353`、`docs/API.md:80-81,302-317` | §10.3 要求的 audit export HTTP intermediary 约束与 export peer 入组要求未进入任何运维文档；文档中唯一的反向代理配方是 user-stats 的 GET-only nginx 块，照抄会破坏两个 POST 路由 |
| m-127 | `README.md:86-102`、`patches/README.md` | README 与 patches/README 声称非 Linux 主机不做 auditd 编译检查，但 `scripts/test.sh:99-105` 仍强制交叉 target 编译并硬失败；README 快速开始的三条命令在 macOS 上逐条不可执行 |

### 21.7 验收建议

1. **先修 `C-4`**，否则任何 Linux 验收都无法开始；修好后第一件事是在 Linux 上完整跑一次
   `cargo test -p shadowsocks-auditd` 与 `cargo test --workspace --features user-audit`——
   这会同时暴露 `M-35`、`M-39` 两条必然失败的用例，以及 §20 `M-29`。
2. **`C-5` 与 `M-36`/`M-37` 必须同批修复**：三者共同构成“低磁盘水位 → 无限循环 → 销毁全部审计
   数据”的链条，且 `M-36` 使触发条件从“磁盘真的满”降低到“spool 树里有一个符号链接”。
3. `M-32` 属于 on-wire 数据伪造，修复成本极低（改用 `fetch_update`），应优先。
4. `M-40` 与 `M-33` 各是一行改动，建议随 1-3 一起提交。
5. `M-34` 需要在 §14.5 目标机上复测 RSS，确认修复后回到 64 MiB 预算内。
6. 把“Linux 上完整 feature-on 测试 + fuzz 实跑 + §14.5 压测”写成发布前置硬条件；在此之前
   §19.4/§20.5/§21.1 的所有本机结论都只覆盖可移植部分。
7. 更正 §19.4 与 §20.5 的失真条目（见 21.5），并把 `x86_64-unknown-linux-gnu` target 列为
   非 Linux 主机的显式前置依赖。

## 22. 第四轮审计整改记录（2026-08-28）

本节记录对 §20、§21 保留发现的实现整改，不改变 §1–16 的合同。整改源码位于锁定上游基线加
`0001`/`0002` 后形成的 `0003-user-audit.patch`；补丁、共享 golden vectors 和准备源码树必须在提交前
重新生成并逐字核对。下列“已修复”只表示代码与当前环境可执行的回归验证已经闭合，不能替代
§16 要求的 Linux 发布验收。

### 22.1 Critical 与 Major

| 编号 | 整改结果 |
| --- | --- |
| C-4 | 两个 UDS 均通过已经固定并复核 inode 的 `/proc/self/fd/N` 路径设置 gid 与 `0660`，失败时明确指出 procfs 前置条件；新增 Linux bind 后 mode/gid 回归测试。 |
| C-5 | 容量清理每次至多 seal 活跃 open 一次；循环加入迭代上限和净字节减少判据，清理自己产生的 gap 不再进入 seal/evict/gap 自激循环。 |
| M-29 | 集成测试复用 `mock_collector.unix_http_request`，严格验证 hello/event ACK/NACK、错误 node、伪 UID、durable state 后 ACK，以及签名后的完整 health 响应。 |
| M-30 | 启动恢复时建立 durable gap reason 索引，运行期随写入、隔离、ACK 和驱逐增量维护，不再在持锁路径全树逐行扫描。 |
| M-31 | seal 的 rename 前持续失败不再让每次 append 全量重建；只在磁盘状态确实可能变化的异常路径重建，并对重复恢复工作限频。 |
| M-32 | 饱和累加改用无重试预算的 `fetch_update`；只有真实算术饱和才产生 `u64::MAX`，并把饱和状态纳入 producer health。 |
| M-33 | session 进入空闲等待前复检 queue，避免 drain 消耗唯一 notify permit 后已有事件无限滞留。 |
| M-34 | UDP 窗口只保存一份规范目标 key，lookup/touch 不再复制 association、identity 与 target 材料；保留有界 shard/LRU 语义及内存回归断言。 |
| M-35 | 绝对路径改为词法组件校验，明确拒绝 `.`、`..`、prefix 和 NUL；原有错误测试改成真正可达的失败断言。 |
| M-36 | `spool_bytes` 以独立 `spool_bytes_known` 表达未知，非普通对象计零但置 degraded；后续完整测量成功可恢复已知状态，不再永久锁存 `u64::MAX`。 |
| M-37 | 每处理一个 cleanup 候选就立即复检容量，并把“操作返回错误但对象已经删除”的净字节进度纳入判定，达到水位即停止继续驱逐。 |
| M-38 | open segment 的 first/last receive time 分别取 min/max，接收墙钟回拨不会生成反向区间。 |
| M-39 | 恢复临时对象后的目录同步允许目录已不存在；回归测试创建真实中断目录并验证回收结果。 |
| M-40 | systemd 对 `/etc/shadowsocks-rust-plus` 与 `/run/shadowsocks-rust-plus` 的 `InaccessiblePaths` 使用可选前缀；全新节点或 ssserver 尚未创建运行目录时不再阻止 auditd 启动。 |

### 22.2 Minor 与前序残留

- producer：完成 §20 的 M-4、m-68–m-70 以及 §21 的 m-85–m-92、m-99；删除或测试门控死代码，
  分离 relay completion 与 shutdown 通知，恢复规范 shutdown 顺序，把域名规范化移出 shard mutex，
  emitter 检查先于 identity lookup，并使 retryable event NACK 保持会升级的连接退避。
- protocol、mock 与验证工具：完成 m-75/m-76、m-79/m-82/m-83 和 m-93–m-96、m-114–m-123；
  新增 204 空响应、完整 NDJSON wrapper、escape/null golden，修正 DuplicateKey，严格验证 canonical
  原始字节和 durable ACK；静态检查覆盖变量索引、切片和禁止宏，错误/空 source 失败关闭；benchmark
  场景、gap 和 gate 使用实测不变量，正式 fuzz 入口固定 source、release 与 sanitizer 参数。
- spool 与 ingest：完成 m-71–m-74、m-97、m-100–m-113；每个磁盘 API 入口复核 `.lock` 的
  pathname/dev/inode，回滚失败后冻结本进程磁盘变更，tombstone 按条恢复；所有删除、rename、sync
  失败路径立即更新索引或异常重建，corruption 使用 `segment_corruption` reason，新节点不再清掉已有
  storage rejection，open 不计入 oldest unacked，并以有界轮转巡检发现 sealed 静默损坏。
- packaging、release 与文档：完成 m-67、m-77/m-78、m-80/m-81、m-98、m-121、m-124–m-127；
  删除宿主 zlib 门禁和单二进制发布旁路，发布 manifest/签名/校验固定包含两个 ELF 及两个 checksum；
  补齐 export intermediary、peer 入组、非 Linux 交叉检查前置条件和敏感扫描自证明测试。

### 22.3 当前验证边界

本机为 macOS。整改后的最终补丁已实际通过：

```text
./scripts/verify.sh                                                        # 完整通过；service 304、protocol 22、EIH 4
cargo check --locked --target x86_64-unknown-linux-gnu \
  -p shadowsocks-auditd --all-targets                                      # 通过，零 warning
cargo test --locked -p shadowsocks-service --no-default-features \
  --features server --lib                                                  # 10 passed
python3 tests/test_mock_collector.py                                        # 16 passed
python3 tests/test_check_audit_static.py                                    # 10 passed
python3 tests/test_benchmark_audit.py                                       # 3 passed
python3 tests/test_fuzz_target.py --source .cache/audit-work-source         # 2 passed
python3 tests/test_integration_audit.py                                     # 3 passed（portable 部分）
python3 tests/test_audit_packaging.py                                       # 8 passed
python3 tests/test_release_artifact.py                                      # 9 passed
python3 tests/test_http_unix.py                                             # 14 passed
python3 tests/check_audit_static.py --source .cache/audit-work-source       # 通过
bash scripts/check-sensitive.sh                                            # 通过
cargo fmt --all -- --check && cargo fmt --manifest-path fuzz/Cargo.toml -- --check
git diff --check
```

synthetic benchmark preflight 的五个故障场景均为 `gate=true`，总 `scenario_gate=true`；由于没有真实
data-path report，顶层 `gate` 正确保持 `null`，不能据此声称通过性能门禁。最终 `0003` 在独立基线树以
`patch -p1 --fuzz=0` 无 offset 重放，49 个变更文件的内容与 mode 均和准备源码树一致。

以下项目在本机仍未执行，必须在发布前补齐：Linux 原生 auditd UDS/SO_PEERCRED、mode/gid、signal、
crash/capacity runtime 测试；Linux 上完整 `cargo test --workspace --features user-audit`；`cargo-fuzz`
的 sanitizer 实跑；§14.5 真实数据面吞吐、CPU、RSS 和离线/队列满压测。非 Linux 的交叉
`cargo check` 只能证明 Linux cfg 分支可编译，不能替代这些运行期结论。

## 23. 第五轮代码审计记录（2026-08-28）

> 本节是对第 22 节整改（commit `20480fd`）的对抗性验证记录，不改变合同条文。新增问题编号接续：
> critical 自 `C-6` 起，major 自 `M-41` 起，minor 自 `m-128` 起。行号基于当前
> `.cache/audit-work-source`（与 `patches/0003-user-audit.patch` 逐字一致，49 个文件
> `patch --dry-run -R -p1 --fuzz=0` 干净反向应用）。

### 23.1 审计范围与方法

- 对象：第四轮整改后的全部交付物；重点是 §22 每条"已修复"声明的代码证据，以及整改 diff
  （补丁净变化约 3000 行）引入的回归。
- 方法：四路并行对抗性验证（producer / auditd / 协议与测试工具 / 打包发布与一致性）；两个新
  critical/major 由编排者亲自复核源码确认；本机（macOS）实跑可移植项：protocol 22 项、workspace
  `--features user-stats` 304 项、service server --lib 10 项、EIH 4 项、Python 套件
  （mock 16、check-static 10、benchmark 3、integration-audit 3、packaging 8、release 9、fuzz 2）、
  golden vectors 独立重算全对、`cargo check --target x86_64-unknown-linux-gnu -p
  shadowsocks-auditd --all-targets` 零警告（该 target 现已安装，§21.5 的环境缺口已消除）。
- 未做：Linux runtime 实跑、fuzz 实跑、§14.5 压测（同 §22.3 披露）。`rg` 依赖项当前不可复现，
  见 m-143。

### 23.2 总体结论

§22 的修复声明**逐条基本属实**（汇总见 23.3）：C-4/C-5 两个 critical 的修复方向、实现与回归测试
真实有效，C-5 的三个 §21 复现场景经心智重放确定退出；§17 遗留的 m-1 也在本轮闭合。但本轮仍发现
**1 个新 critical 与 1 个新 major**：

- `C-6` 是一行级启动期 panic，为主机开机 60 秒内启动的节点击落 ssserver 与 auditd 两个进程，
  系第三轮整改引入、两轮漏检；修复成本一行。
- `M-41` 是 M-30 索引化修复引入的启动期幂等回归，可在崩溃窗口把同一固定 ID 的 spool_gap 重复
  写入 durable spool；修复方向是把索引建立挪到启动 flush 块之前。

两者都在 Linux 实跑一次完整 feature-on 测试/集成测试时会立即暴露，再次印证 §21.7 把"Linux 完整
实跑"列为发布前置硬条件的必要性。修完这两条后，本功能的已知缺陷清单收敛到 minor 级。

### 23.3 Critical

- **C-6 `Instant::now() - Duration::from_secs(60)` 在主机开机 60 秒内下溢 panic，panic=abort
  击落整个进程；ssserver 与 auditd 各有一处。** producer 侧
  `crates/shadowsocks-service/src/server/user_audit.rs:2422-2423`：`run_supervisor` 函数体开头以
  `Instant::now() - Duration::from_secs(60)` 初始化 `last_error_log`/`last_hello_nack_log`。Rust
  std 的 `Instant - Duration` 在下溢时 panic（"overflow when subtracting duration from instant"），
  Linux 上 `Instant` 基于 CLOCK_MONOTONIC（自举 uptime），主机启动后 60 秒内执行该减法必然
  panic。fast-boot 的 systemd 节点（云镜像、容器化 VM）开机即拉起了带 user_audit 的 ssserver 属
  常态，此时 emitter 建成后 supervisor 启动即崩，按 §7.3 release `panic="abort"` 数据面进程随之
  终止；systemd 默认 StartLimit 下反复重启进入 failed 态，直到 uptime 超过 60 秒后人工介入。
  auditd 侧同源：`crates/shadowsocks-auditd/src/spool.rs:1019-1020` 的 `SpoolInner` 构造以
  `Instant::now() - INDEX_REBUILD_RETRY_INTERVAL` 与 `Instant::now() - RETENTION_SWEEP_INTERVAL`
  （均为 60 秒）初始化两个限频戳，auditd 在同一窗口内启动同样 panic。
  引入时点：`0f52bd9` 首版补丁无此代码，系 `4a6348e`（§19 整改轮）引入，`20480fd` 原样保留。
  这直接违反 §7.3"审计路径必须 panic-free……属于阻断数据面的发布缺陷"。修复：`checked_sub`
  或初始化为 `None` 在首次日志时填充（一处一行）；全树 grep `- Duration::` / `- *_INTERVAL`
  类初始化模式，补一个 `checked_sub` 单测。

### 23.4 Major

- **M-41 M-30 的 `durable_gap_reasons` 内存索引在启动 flush 窗口内必为空，固定 ID gap 会被重复
  写入 durable spool，违反 §9.4 幂等补写规则。** `open_with_lock` 的顺序是：构造
  `SpoolInner`（`durable_gap_reasons: HashMap::new()`，`spool.rs:1016`）→
  `flush_state_reset_locked`（1034）→ `reconcile_tombstones_locked`（1044）→
  `rebuild_acked_receipts_locked` → `flush_recovery_gaps_locked`（1058）→ 最后才
  `rebuild_runtime_indexes_locked`（1066）建立索引。整改前这三条路径用 `event_id_exists_in_epoch`
  磁盘扫描（启动时天然正确）；整改后全部改查内存表（`spool.rs:2204/2238/2260/3239/3290/3358`），
  而此刻表必为空：崩溃落在"gap 已 durable、pending/marker 尚未压缩或删除"窗口（或
  `quarantine_batch_locked` 的 marker unlink 失败，无需崩溃）时，重启会把同一固定 `gap_event_id`
  的 spool_gap 再写一条（sequence 不同、内容相同），collector 侧丢失量被双计，按 event_id 去重的
  实现报 `event_payload_conflict`。`flush_state_reset` 不受害（`recover_layout:3956-3958` 仍用磁盘
  扫描）。影响有界（每个窗口化 pending/marker 至多一条重复，无循环无 wedge，健康本已 degraded），
  定 major。修复：把 `durable_gap_reasons` 的采集（或整个 `rebuild_runtime_indexes_locked`）挪到
  启动 flush 块之前；该块内函数不依赖"索引尚未建立"的状态，前移安全。现有两个 corrupt-ledger
  测试的 gap 事先均未 durable，恰好在盲区（见 m-136）。

### 23.5 §22 声明验证结果汇总

- **Critical**：C-4 已修复（`/proc/self/fd/N` 方案与 `user_stats.rs` 成熟实现同构，失败信息指明
  procfs 前置条件，带 Linux mode/gid 回归测试）；C-5 已修复（seal 移出循环且每次清理至多一次、
  迭代上限 4096、逐候选净字节判据、超限返回 `StorageUnavailable` 四要件齐全、自激分支排除）。
- **Major**：M-29、M-31、M-32、M-33、M-35、M-36、M-37、M-38、M-39、M-40 逐条证实已修复且带真实
  回归断言；M-30 部分修复（热路径目标达成，但引入 M-41）；M-34 基本修复（保留见 m-131）。
- **Minor**：m-68、m-69、m-70、m-85、m-86、m-88、m-89、m-90、m-91、m-92、m-99；m-71、m-72、
  m-73、m-74、m-97、m-100–m-113；m-75、m-93、m-94、m-95、m-96；m-79、m-82、m-83、m-114–m-123；
  m-66、m-67；m-118–m-122；m-124、m-125、m-127、m-77、m-78、m-80、m-98、m-120 均证实已修复。
  m-126 主体修复（残留 m-141）。
- **额外闭合**：§17 遗留的 m-1（queue_overflow gap 时间语义）实际已修复——三类 gap 的
  first/last_seen 已统一为丢弃观测墙钟（`user_audit.rs:1935-1936、2187-2189、2394`，附断言
  `first_time==last_time==200` 的回归测试）。
- **无回归**：m-23/m-24、m-40/m-41 复核仍成立；producer 与 auditd 非测试代码保持零
  unwrap/expect/panic/越界索引（C-6 属算术下溢 panic，不是裸 unwrap 类，现有静态护栏抓不到，
  见 m-144 建议）；feature-off 行为零变化（除已记录项）。

### 23.6 Minor

| 编号 | 位置 | 问题 |
| --- | --- | --- |
| m-128 | `user_audit.rs:2430-2451` | run_supervisor 空闲块缺 `diagnostic_shutdown_requested` 复检（session 侧已补、supervisor 侧未补，与 M-33/m-68 不对称）；`Notified` 惰性注册窗口另存在于 drain 与 `wait_until_stopped` 两处，均仅限关机路径且有外层超时兜底；建议统一 `Notified::enable()` 或 freeze 信号改 `notify_one` |
| m-129 | `user_audit.rs:2352-2354、2402-2406` | force-final 完成条件实际不可达（`final_diagnostic_attempted` 只对非空桶置位），drain 窗口内退化为 10ms 轮询；逻辑赘疣，与注释意图不符 |
| m-130 | `user_audit.rs:785-792、1100-1101` | M-34 保留：内存回归断言只钉 Arc 共享机制、未钉字节预算；病态交替拼写场景静态估算约 66–70 MB 仍贴近 §14.5 的 64 MiB 预算，待 Linux 实测收口 |
| m-131 | `spool.rs:1566-1579` | `ack()` 在 rename 成功后 `sync_dir` 失败会在 `remove_sealed_index` 前返回，sealed 内存索引幽灵化直到下次 rebuild（m-103 已修模式未覆盖 ack 路径）；方向保守、无 wedge |
| m-132 | `spool.rs:801、2301` | `durable_gap_reasons` 只增不删：segment 被驱逐/24h 清理后其 gap ID 仍标 durable，陈旧索引可抑制 §9.5 要求的同 ID 补写（窗口极窄）；建议删除 segment 时顺带剔除或注释界限 |
| m-133 | `spool.rs:2318-2319、2577` | 两处重测/重建未按 60 秒限频：seal 双重失败置脏后每条 append 触发一次全树 `directory_size_status`；recover temp 错误分支在持续失败时每 cleanup 一次 rebuild；均为 degraded 下的性能残留 |
| m-134 | `spool.rs` 测试集 | 测试盲区：C-5 迭代上限退出、m-74 单条目继续、m-72 冻结、M-41 的"启动时 gap 已 durable"幂等路径均无测试；C-4 新测试的 gid 变更被平凡满足（expected_gid==egid），未真正 exercise 改组路径 |
| m-135 | `tests/check_audit_static.py:241-264` | partial-tree 静默欠扫描：`--source` 指向存在但截断的准备树（缺 crate/文件）时输出"静态审计通过"；m-116 的失败关闭只覆盖空/不存在根目录 |
| m-136 | `tests/check_audit_static.py:171` | `name.len()` 启发式反向盲区：`v[v.len()]`（必然越界）与 `v[1..v.len()]`（空 slice panic）被判 safe 静默 |
| m-137 | `tests/mock_collector.py:498-502、906` | 未知 event_type 回退不钉字段顺序（保留插入序）；204 响应携带多余的已签名 batch headers 不被拒；ACK body 做语义比对而非 §10.1 逐字相等；均无安全后果 |
| m-138 | docs §22.2 | 整改记录枚举遗漏 m-66（更正实际已落在 20480fd） |
| m-139 | `docs/OPERATIONS.md:306-328,388-444` | m-126 残留：user-stats nginx 块未就地标注不可照抄用于 audit export；§10.3 三条细则未落文档——endpoint↔node 一一映射、转发前拒绝 >4096 bytes body（Nginx 片段无 `client_max_body_size`）、access log 不记录 Authorization/MAC 头 |
| m-140 | `export.rs:201-206`、`spool.rs` | export 的瞬时 accept 仍是固定 50ms 睡眠，未享受 ingest 的 m-101 加固（退避升级/上限/诊断）；`integrity_queue` 仅随 health 懒清理 |
| m-141 | `scripts/check-sensitive.sh`、`tests/test_http_unix.py`、`scripts/verify.sh` | 环境/记录可信度：本机当前无 `rg`，三条 §22.3 记录（check-sensitive 通过、test_http_unix 14 passed、verify.sh 完整通过）在现状下不可复现（rc=1 / 5 项失败 / 立即失败）；门禁 fail-closed 设计本身诚实，建议重装 rg 重跑后背书 |
| m-142 | `spool.rs:39` 等 | 建议项：把 `- Duration::`/`Instant` 算术下溢模式加入 `check_audit_static.py` 禁止表（C-6 类缺陷现有护栏抓不到） |

### 23.7 需规格处理的文本项

1. §7.3"成功收到合法 ACK 后重置"与实现（m-99 修复后仅 event ACK 重置，hello ACK 不重置）存在
   字面张力；行为正确，建议升版时把该句收紧为"合法 event ACK"。
2. §9.5 `quarantine_pending` 的 `reason=quarantine_eviction` 固定值与 wire/代码已引入的
   `segment_corruption`（m-110 修复需要）不一致；建议升版时把 reason 枚举同步进合同文本。

### 23.8 交付一致性核查

- 补丁↔源码树：49 个文件 `patch --dry-run -R -p1 --fuzz=0` 干净反向应用，逐字一致。
- 合同文本：`20480fd` 对 §1–16 仅两处改动（版本沿革补齐 v2/v3/v4 链、§14.4 "group sync"术语
  更新），无夹带。
- §22.3 验证清单复跑：cargo 侧与 Python 侧逐数一致（含交叉 check 零警告、benchmark 五场景
  `gate=true`/顶层 `gate=null` 的披露口径）；反例 3 项均由当前机器缺 `rg` 导致（m-141）。
- golden vectors：双端逐字一致，新增向量（204 空响应、escaping/Unicode、nullable spool_gap、
  NDJSON wrapper 行）经独立重算全部正确；mock 的 canonical 对抗探针 11 项全部按预期拒绝。
- 敏感信息：tracked 文件零命中；git 工作树干净；无生成物混入。

### 23.9 验收建议

1. **先修 C-6**（一行 `checked_sub`，两个进程各一处；全树排查同类 Instant 算术初始化模式并把该
   模式加入静态护栏，m-142）；
2. **修 M-41**（索引建立前移到启动 flush 块之前），并补"启动时 gap 已 durable"的幂等回归测试
   （m-134）；
3. 两项修完后在 Linux 上完整实跑：`cargo test --workspace --features user-audit`、
   `cargo test -p shadowsocks-auditd`、`tests/integration_audit.py`、fuzz sanitizer 实跑——
   这会同时坐实 C-4 的 procfs chown 与各 Linux-only 回归测试；
4. §14.5 目标机压测（含 m-130 的 UDP 窗口 RSS 实测）仍为发布前置；
5. 23.7 的两处规格文本随下次升版同步。

## 24. 第六轮代码审计记录（2026-08-29）

> 本节是对第四轮整改（commit `20480fd`）的第六轮审计，不改变合同条文。编号接续：major 自
> `M-42` 起，minor 自 `m-143` 起；本轮无新 critical（§23 的 `C-6` 与 `M-41` 仍未修复）。
> 行号基于当前 `patches/0003-user-audit.patch` 应用树。

### 24.1 审计范围与方法

- 对象：整改后全部交付物；重点是 §22 整改（净变化约 3000 行）引入的回归，以及五轮静态审查的
  共同盲区。
- **方法上的关键变化：本轮要求 agent 真正执行代码，而不只是阅读。** 前五轮几乎全是静态阅读，
  因此 `C-4`（`fchownat` 传 NULL）与 `C-6`（`Instant` 下溢）这类“一跑就炸”的缺陷才会连续漏检
  多轮。本轮 10 个维度并行审查，**45 条保留发现中 38 条（84%）由 agent 实际运行代码复现**——
  做法是把源码复制到私有临时目录、临时打桩（去 `compile_error!`、给 `libc::ucred`/`SO_PEERCRED`
  加 shim）后编译运行真实场景，实验后清理副本（源码树经反向 patch 确认逐字还原）。
- 复核：每条候选交独立复核者对抗性反驳（54 条候选 → 驳回 9 条、保留 45 条），多条被下调严重度
  或修正表述；两个最关键的交付性缺陷（`M-42`、`M-50`）由编排者亲自复现。
- 本机（macOS）实跑结果：`cargo check` feature-off 通过；`cargo test -p
  shadowsocks-audit-protocol` 22 项、`--workspace --features user-stats` 304 项、
  `-p shadowsocks-service --features server --lib` 10 项全部通过；`cargo fmt --all -- --check`
  通过；Python 套件 mock 16、static 10、benchmark 3、packaging 8、release 9、http-unix 14、
  integration 3、fuzz 2 全部通过；`check_audit_static.py`、`check-sensitive.sh` 通过。
- 本机失败：`cargo check --target x86_64-unknown-linux-gnu -p shadowsocks-auditd`
  （target 未安装）；`scripts/verify.sh` 因此在 `scripts/test.sh:99-101` 硬 `die`，退出码 1。
- 未做：Linux runtime 实跑、fuzz sanitizer 实跑、§14.5 真实数据面压测。

### 24.2 总体结论

§22 的整改把 §21 的两个 critical 真正修掉了（`C-4` 的 `/proc/self/fd/N` 方案与 `C-5` 的 seal
移出循环都经本轮复核确认有效）。但本轮仍发现 **10 个 major**，且暴露出三个必须正视的模式：

1. **“修一个引入一个”仍在继续。** 10 个 major 中 4 个直接由 §22 的修复动作产生：`M-43`
   （`M-38` 的 min/max 修复与校验器的首末行语义冲突，反而把整批已 ACK 事件判成损坏）、
   `M-45`（`M-37` 的净字节判据把“移动过对象”当成“释放了字节”）、`M-48`（shutdown 协调的
   `Notify` 注册被塞进每轮收包 select，实测 2%–22% 吞吐回退且**不在 feature gate 内**）、
   `M-44`（`C-5` 的自激分支排除未真正落地）。
2. **交付的测试连续第四轮出现“必然失败”的用例。** `M-42`：`cargo test -p shadowsocks-auditd`
   在**任何平台**都会红——`config::tests::segment_bound_includes_newline` 断言
   `segment_max_bytes = 8705` 时 `validate()` 为 Ok，而同一函数的 `bounded(…, 16_384, …)`
   必然拒绝它。加上 `M-49` 的 producer 侧同类用例，§23.9 列为发布前置的两条 Linux 实跑命令
   现在都跑不过。历轮同类：§20 `M-29`、§21 `M-35`/`M-39`。
3. **“已修复且带回归测试”的声明需要打折。** `M-51` 实测证明：删掉 `M-33` 修复的全部三行代码，
   32 个用例仍全绿；在该空闲块前插入无条件 `panic!` 也不会有任何用例失败——即
   `connect_and_send_session` 的整个空闲等待块从未被任何测试执行过。这类“同名但不绑定”的
   回归测试会让后续整改在无保护网的情况下反复回归。

另外，`M-50` 显示 `0003` 补丁自身携带一条删除不存在文件的幽灵 stanza，影响 overlay 的可重放性。

### 24.3 Major

- **M-42 `cargo test -p shadowsocks-auditd` 在任何平台都无条件失败。**
  `crates/shadowsocks-auditd/src/config.rs:1011-1018` 的 `segment_bound_includes_newline`：

  ```rust
  config.segment_max_bytes = (wire::MAX_SPOOL_RECORD_BYTES as u64).saturating_add(1); // = 8705
  assert!(config.validate().is_ok());                                                  // 必然 panic
  ```

  `MAX_SPOOL_RECORD_BYTES = MAX_EVENT_JSON_BYTES + 512 = 8192 + 512 = 8704`
  （protocol `lib.rs:28,31,38`），而 `validate()` 在到达 `minimum_segment` 检查之前先执行
  `bounded(self.segment_max_bytes, 16_384, DEFAULT_SEGMENT_MAX_BYTES, "segment_max_bytes")`
  （`config.rs:337-342`），`8705 < 16384` 直接返回
  `Err("segment_max_bytes must be between 16384 and 4194304")`。该断言与平台、libc、feature
  组合都无关，`cargo test -p shadowsocks-auditd` 整个 crate 测试套件 FAILED。
  §23.9 步骤 3 把这条命令列为发布前置——发布门禁被一条自相矛盾的测试卡死。风险还在于运维会
  误判为环境问题而用 `--skip` 绕过，从而继续掩盖“该 crate 的全部单测（含 spool 事务/容量/
  tombstone 的回归断言）从未真正跑过”这一事实。
  连带：`config.rs:360-367` 的 `minimum_segment`（8705）分支在合法配置区间内永远不可达，是
  死代码（见 `m-143`）。修复：把该测试改为验证 `bounded` 下界与 `minimum_segment` 两条规则各自
  的真实边界，或直接删除这条与 §5.3 冲突的断言。（§5.3 §14.4 §16）

- **M-43 `M-38` 的修复引入回归：open 段 meta 取 min/max，与校验器要求的“首行/末行时间”矛盾，
  任何接收时间非单调的 sealed segment 一律被判损坏并隔离，整批已 ACK 事件不可导出。**
  `update_open_metadata`（`spool.rs:4316-4317`）按 `M-38` 改成对 `first_received_at` 取 min、
  `last_received_at` 取 max；但 `validate_segment_body`（`spool.rs:4146-4148`、`4155-4156`）仍按
  **body 首行/末行**的 `received_at` 逐字比对 meta。segment 内接收时间一旦非单调，seal
  （`3425-3430`）写出的 meta 就被自己的校验器判为损坏，整个 batch 在 `lease()`（`1890`）、
  恢复（`4069`）或 health 巡检（`1794`）时被移入 `quarantine/` 并另生成一条 `segment_corruption`
  gap。其中**已 `fdatasync`、已向 producer 返回 ACK** 的 access 事件永久不可导出，collector 只
  收到一条“损坏”gap。触发条件有两条，第二条完全不依赖时钟：(a) 墙钟回拨落在同一 open 段的两次
  append 之间（`ingest.rs:428` 直接用 `unix_millis()`，无单调化）；(b) 恢复期写出的多条 gap 其
  `occurred_at` 逆序（gap 的 `received_at` 取 `spec.occurred_at_unix_ms`，`spool.rs:2286/2300`）——
  此时连“要表达丢失”的那条 gap 自己也被吞掉。实测重启后 `recover_layout` 走 `inspect_batch_dir`
  会做同样判定，同样落入 quarantine，不自愈。
  修复：让 `validate_segment_body` 与 meta 语义对齐（改为对 body 全部行取 min/max 后比对），
  或把 meta 恢复成首末行语义并在写入侧拒绝逆序记录；两者必须一致。（§9.3 §9.4）

- **M-44 `C-5` 的自激分支排除未真正落地：低可用空间下 cleanup 变成“永动 gap 机”。**
  可用空间跌破 `min_free_bytes` 且真实 spool 数据已被驱逐完之后，`free_space_ok` 仍为 false
  （磁盘不是 auditd 占的）。此后 ssserver 每重试一次 in-flight 事件，auditd 就把“只含上一轮
  cleanup 自己写的 gap”的 open 段封口并驱逐，伪造一条全新的 `min_free_eviction` spool_gap
  （消耗一个 spool sequence + `fdatasync` + `state.json` 持久化 + 一条 tombstone + `tombstones.json`
  全量重写），`evicted_unacked_records += 1`，然后仍返回 `storage_unavailable`。
  实测：20 个真实事件的节点最终上报 78 条 gap；spool 目录在已经缺空间的分区上**反向增长**约
  238 B/次，直到 tombstone ledger 触顶（4096 条）才稳定在约 0.93 MiB。
  后果：collector 每次轮询收到一条自指 gap，声称丢失了从未存在过的事件，`evicted_unacked_records`
  单调膨胀，而**原始的真实丢失证据被这些自指 gap 覆盖**（§9.5 要求的丢失证据失真）。
  与 §21 `C-5` 的区别：不再挂死、不再无限增长，属于“丢失量被高报”而非“进程 wedge”，故定 major。
  修复：`cleanup_locked` 在 open 段只含本轮自己写入的诊断记录时不得 seal/驱逐；
  `min_free` 触发且无真实可驱逐对象时应直接走 §9.5 第 6 条拒绝路径，不产生 gap。（§9.5）

- **M-45 `M-37` 的“逐候选净字节判据”把“移动过对象”当成“释放了字节”，单个损坏 acked batch 就能
  让整轮 cleanup 跳过 §9.5 第 3/4 步。** `spool.rs:2448-2456`（quarantine/sealed 两处同构于
  `2487-2495`、`2543-2551`）的“一轮内净字节未下降即 break”守卫，与 acked 损坏分支
  （`2401-2414`：quarantine 成功即置 `progressed = true`，而 quarantine 只是 rename + 写 gap、
  **零字节释放**）叠加时，会让 `cleanup_locked` 直接跳过“删除最老 quarantine batch”与“删除最老
  sealed 未 ACK batch”两步并提前返回 retryable `storage_unavailable`，尽管仍有大量可驱逐对象。
  实测：1 个 `meta.json` 损坏的 acked batch + 2 个健康 sealed → 单次 `cleanup_locked` 后
  `sealed_after=2`、`quarantine=1`、返回 `StorageUnavailable`；紧接着第二次调用才把二者驱逐；
  对照组（meta 未损坏）一次即完成。同样的边界还会在“驱逐小 sealed batch 所释放字节 ≤
  `eviction_pending` ledger 更新 + spool_gap 记录成本”时触发。
  能自愈（下一次 append 继续），但每个损坏对象、每次撞上净字节边界都多一批被无谓拒绝的记录和
  一次假的容量告警；sealed batch 数量大时会把一次本应完成的清理拆成几十次 append。
  修复：进度判据应区分“字节释放”与“对象搬移”，quarantine 类搬移不得计为净进展。（§9.5）

- **M-46 `write_barrier_failed` 冻结无任何解除路径，一次瞬时 I/O 故障后 ingest/lease/ACK 永久
  返回 `storage_unavailable`，只能人工重启。** 唯一置位点 `spool.rs:2138`，唯一清零点是构造
  函数 `spool.rs:1017`；拦截点 `append_parsed`(1125)、`lease`(1232)、
  `quarantine_oversized_lease`(1365)、`ack`(1419)。当一次持续性存储故障（fs 转 ro、设备/挂载
  消失、网络挂载 EIO/ESTALE）使 `persist_state_locked` 与 `rollback_open_record` **同时**失败时
  置位（若故障落在 `sync_data` 之前，走的是 `2101-2113` 的可自愈分支）。此后即使存储完全修复，
  四条路径仍永久拒绝；`AuditDaemon` 不因此退出（`lib.rs:48-94` 只响应信号/socket 任务失败），
  systemd `Restart=` 不触发。实测重开 `Spool` 立即恢复，证明是纯内存态锁死。
  冻结期的拒绝还**不计入** `storage_rejected_attempts`，违反 §9.5 第 6 条；唯一外部信号是
  `/health` 恒为 degraded。修复：给冻结加解除条件（重新测量存储可写性后清零），或在冻结时主动
  退出让 systemd 重启。（§9.4 §9.5 §10.1）

- **M-47 open segment 与 `open_meta` 失配后 ingest 永久 wedge。** 当 `open_meta` 为空
  （`event_count == 0 && bytes == 0`）而 `open/current.ndjson` 残留孤儿字节时，
  `ensure_open_file_bound`（`spool.rs:2162`）跳过 `2173-2177` 的第一处 `InvalidState`，重开同一
  路径后在 `2188-2192` 返回 `InvalidState("replacement open segment is unexpectedly non-empty")`；
  该错误经 `ingest.rs:457-461` 映射为 retryable 的 `internal_error` NACK 并断连。按 §8.3 该码
  语义是“保留原始 bytes，断开并退避”，于是 producer 无限退避重连，节点审计事件全量丢失。
  **进程内无任何自愈路径**：唯一能重建 open inode 的 `seal_locked`（`3384`）在
  `event_count == 0` 时于 `3385-3387` 早退，`lease()` 的 seal 又被 `spool.rs:1254` 的
  `event_count > 0` 门控。触发：一次瞬时故障（ENOSPC 导致部分写入，紧接着 EIO 使回滚的
  `ftruncate`/`fsync` 失败）恰好落在刚 seal 过的新 open segment 的第一条记录上。
  health 只显示 degraded、`storage_rejected_attempts=0`、`stored_records=0`，不足以提示“重启是
  唯一解”。修复：`ensure_open_file_bound` 在检测到孤儿字节时把该 open 段 quarantine 并生成
  `segment_corruption` gap 后重建空 open，而不是返回 `InvalidState`。（§8.3 §9.4）

- **M-48 shutdown 协调的 `tokio::Notify` 等待者注册被塞进 UDP 收包与 TCP accept 的每轮 select，
  且不在任何 feature gate 内，实测 2%–22% 吞吐回退，而 §14.5 的 feature-off/on 对比方法学无法
  发现它。** `DataShutdownState::accept_notify`（`context.rs:42`）经
  `wait_until_stopped()`（`context.rs:97-105`）以“每轮循环重新创建 `Notified` 并 poll”的形式
  进入 UDP 多核收包 worker（`udprelay.rs:303`）、UDP 主收包循环（`udprelay.rs:357`）与 TCP
  accept 循环（`tcprelay.rs:100`）；三处**均无 feature gate**，feature-off 构建同样承担。
  tokio 1.48 的 `Notified` 首次 poll 会取 `notify.waiters` 全局 Mutex 并入侵式入链，select 因
  别的分支就绪而结束时再取一次锁摘除——全 server 共用一把锁。实测（tokio 1.48、release、
  10 核、每包约 1.8–3.3 µs 真实工作量）：1–4 worker 回退 2%–5%，6 worker 约 10%，
  8 worker 12%–22%；对照组显示 select 机器本身开销为零，回退全部来自该锁。
  关键在于：因为开销不在 `user-audit` feature 之内，按 §14.5「比较 feature-off 与 feature-on」
  做 A/B 压测时两侧都含此开销、差值为零，测试会判定“无回退”，而相对 upstream v1.24.0 的真实
  回退被完全隐藏。修复：把等待者注册移出热路径（例如用 `watch`/`AtomicBool` 轮询或只在 accept
  返回后检查一次），并把 §14.5 的基线从“feature-off 构建”改为“upstream v1.24.0”。（§1 §7.1 §14.5）

- **M-49 producer 侧 `canonical_user_audit_socket_path` 接受含 `.` 组件的路径，与 auditd 侧
  `M-35` 的修复不对称；其配套回归测试在 Linux 上必然失败。**
  `crates/shadowsocks-service/src/config.rs:280-391`：`299-310` 的词法检查只覆盖开头 `/`、
  `len<=1`、尾 `/`、`//`、NUL、缺 `file_name` 六项，**不含 `.`**；`323-333` 依赖
  `parent.components()`，而 `Components` 会把绝对路径中的 `.` 全部归一化掉（实测
  `/d/./x → [RootDir, Normal("d"), Normal("x")]`），故该分支对 `.` 不可达；`370-377` 的
  `canonicalize(parent) != parent` 同样无效，因为 `Path` 的 `PartialEq` 按 components 比较。
  后果一：部署方把 `ingest_socket_path` 写成 `/run/shadowsocks-audit/./ingest/ingest.sock`
  时，ssserver 的 `load` + `check_integrity` + `build_server_with_signal` 全部放行并正常代理，
  而 auditd 的 `AuditdConfig::validate()` 对同一字符串返回 `InvalidPath` 并以退出码 78 拒绝
  启动——节点数据面正常、审计完全不产出。§5.2 要求这类路径错误在配置校验阶段就明确失败。
  后果二：`user_audit_socket_rejects_noncanonical_path`（`4203-4211`，断言 `4209`）在 Linux 上
  必然失败，使 §23.9 步骤 3 的 `cargo test --workspace --features user-audit` 也跑不过。
  修复：与 `M-35` 一致地改用词法检查显式拒绝 `/./`、`/../`、结尾 `/.`、`/..`。（§5.2 §14.1）

- **M-50 `0003-user-audit.patch` 含一条删除 `crates/shadowsocks-auditd/src/main.rs` 的幽灵
  stanza，而该文件在锁定基线中并不存在。** `patches/0003-user-audit.patch:7474-7476`：

  ```text
  diff --git a/crates/shadowsocks-auditd/src/main.rs b/crates/shadowsocks-auditd/src/main.rs
  deleted file mode 100644
  index e69de29bb2d1d6434b8b29ae775ad8c2e48c5391..0000000000000000000000000000000000000000
  ```

  `e69de29b` 是空 blob 的 hash，整条 stanza 没有任何 hunk。upstream v1.24.0 根本没有
  `shadowsocks-auditd` crate，该文件从不存在——这是第三轮整改删除重复 bin（§21 `m-38`/`m-26`）
  时从开发工作树带出来的残留。
  **本轮实测澄清（对提出者结论的更正）**：`patch(1)` 在整份补丁中遇到无 hunk 的 stanza 会直接
  跳过，因此仓库自己的 `scripts/prepare-source.sh` 与 `verify.sh` **能够正常工作**（本轮三次
  `prepare-source.sh` 均成功，产物中不含 `main.rs`）；提出者所说“补丁无法前向应用”不成立。
  真正受影响的是 `git apply`/`git am` 路径：最小复现
  （`git apply --check` 一条同形 stanza）返回
  `error: ghost.rs: No such file or directory`、rc=1。`patches/README.md` 与 `README.md` 都把
  series 描述为可重放的 overlay，任何用 git 重放的下游会直接失败。
  修复：重新生成补丁时剔除该 stanza，并在 `scripts/verify.sh` 增加一条“补丁不得包含针对基线中
  不存在文件的删除 stanza”的断言。（§3.1 §15.1）

- **M-51 `M-33` 的同名回归测试不绑定被测修复：删掉修复后 32 个用例仍全绿。**
  被测修复是 `user_audit.rs:2670-2673` 的空闲前 queue 复检；回归测试
  `idle_session_rechecks_queue_after_notify_permit_is_consumed` 在 `3543-3601`。
  实测：(1) 删除 `2671-2673` 三行后，单跑该用例 ok、全套 32 passed；(2) 在 `2670` 行之前插入
  无条件 `panic!` 后，单跑该用例仍 ok、全套仍 32 passed——证明
  `connect_and_send_session` 的**整个空闲等待块**（`2666-2690`，含 60 s idle timeout、
  `next_diagnostic_wait` 钳位、`force_final_diagnostics` 的 10 ms 路径与 `notify`/`sleep` 的
  `select!`）在全部 32 个用例中从未被执行。原因是 current_thread 运行时的调度顺序：server task
  在 `write_frame(first ack).await` 返回后同步执行 `3576` 行的 `queue.push`，session 随后在
  `2659` 的 `fill_pending` 就把该事件取走，根本走不到 `notified()`。
  后果：若后续整改删坏该复检，CI 无任何用例会失败，lost-wakeup 会静默回归，而验收记录仍显示
  “M-33 已修复且带真实回归断言”。同一空闲块内的 60 秒 idle timeout、诊断等待钳位也同样零覆盖。
  修复：用可控的时间源/显式 yield 构造真正进入空闲块的用例，并断言“queue 非空时 session 不进入
  长睡眠”。建议对 §22/§23 中所有“已修复且带回归测试”的条目做一次同样的变异检验。
  （§14.4 §22.1 M-33 §23.5）

### 24.4 对 §22/§23 结论的更正

- **§23.1「`x86_64-unknown-linux-gnu` 该 target 现已安装，§21.5 的环境缺口已消除」为假。**
  本机 `rustup target list --installed` 仍只有 `aarch64-apple-darwin`；
  `cargo check --locked --target x86_64-unknown-linux-gnu -p shadowsocks-auditd --all-targets`
  以 `error[E0463]: can't find crate for core — the target may not be installed` 失败。
  连带：`scripts/verify.sh` 在 `scripts/test.sh:99-101` 的
  `[[ -d "$audit_libdir" ]] || die` 处退出（实测 `VERIFY_RC=1`），因此 §22.3 首行
  「`./scripts/verify.sh  # 完整通过`」不可复现——它能跑到 §22.3 引用的数字（service 304、
  protocol 22、EIH 4），但随后即 die，**Python 测试块根本没有执行**。这是第三轮出现同类失真
  （§21.5 记过一次，§23.8 声称已消除）。建议：把该 target 写成显式前置依赖并在 `verify.sh`
  开头 `require_target`，或在非 Linux 主机上改为跳过并明确打印“未验证”。
- **§23 `m-141` 的前提在本机不成立，应撤销。** `rg` 15.1.0 存在于 `/opt/homebrew/bin/rg`；
  `bash scripts/check-sensitive.sh` 退出 0；`python3 tests/test_http_unix.py` 报 14 passed。
  §23.8「反例 3 项均由当前机器缺 `rg` 导致」的归因错误——真正不可复现的是上一条的交叉 target
  项。两条环境判断恰好互为反面，说明验证记录需要在写入前实跑确认。
- **§23 `m-128` 是误报，应撤销。** tokio 1.48 的 `Notified` 在**构造时**即快照
  `notify_waiters_calls`，`m-128` 所说的“惰性注册窗口”并不存在，据此建议的
  `Notified::enable()` 修复是空操作。（`user_audit.rs:1594-1626` drain、`2430-2451` supervisor
  空闲块两处均不受影响。）
- **§23 `m-130` 的数字应更正。** UDP 窗口缓存默认档位的**实测** RSS 是 43.1 MiB（病态交替拼写
  43.3 MiB），不是静态估算的 66–70 MB。结论方向不变：它单项就吃掉 §14.5 64 MiB 预算的约 2/3
  且永不回缩，仍需在目标机复测。
- **§22.3 的验证清单有三类条目按字面无法在本仓库执行。** `cargo fmt --all -- --check`、
  `cargo check ... -p shadowsocks-auditd`、`--source .cache/audit-work-source` 都需要一棵准备好的
  源码树，而 overlay 仓库本身没有 `Cargo.toml`/`fuzz/`，且**没有任何脚本或文档会创建
  `.cache/audit-work-source`**（`prepare-source.sh` 要求目标目录不存在，由调用者指定路径）。
  建议在 §22.3 中写明“先执行 `scripts/prepare-source.sh .cache/audit-work-source`”，否则读者
  无法复现该清单。
- **§23.5「M-33 … 逐条证实已修复且带真实回归断言」需要下调。** 见 `M-51`：该回归测试与被测
  修复不绑定，且相关空闲块从未被任何用例执行。建议对 §22/§23 中所有“已修复且带回归测试”的
  条目做一次变异检验（删除修复后跑测试，观察是否变红）再背书。

### 24.5 Minor

| 编号 | 位置 | 问题 |
| --- | --- | --- |
| m-143 | `auditd/src/config.rs:360-367` | `minimum_segment`（8705）分支在合法配置区间内永不可达，是死代码——`bounded()` 的 16384 下限严格更大（`M-42` 的连带项） |
| m-144 | `audit-protocol/src/lib.rs:603,621-627` | `normalize_domain` 在 UTS #46 转换**之前**只 strip ASCII `.`，而 UTS #46 把 U+3002/U+FF0E/U+FF61 映射为 `.`，末尾带这三种全角句点的域名转换后留下空 label 被判规范化失败（`normalized_host=null`，UDP dedup key 退化为原拼写）。§6.2 要求的顺序是“转 ASCII、小写、再移除一个末尾点”。实测 `例え.テスト。`→失败而 `例え.テスト.`→成功。Linux 默认 hickory 解析器不做 IDNA，此类主机名解析不出来因而不产生事件，实际暴露面限于解析器自带 IDNA 的平台 |
| m-145 | `tests/golden_vectors.json` | 覆盖盲区：access variant 唯一的 nullable 字段 `target.normalized_host=null` 无向量；`producer_gap` 的条件字段组合、6 个 `spool_gap` reason 中的 4 个也无向量；两个测试用精确 key 集合固化了这份不完整清单 |
| m-146 | `fuzz/fuzz_targets/audit_protocol.rs:9-28`、`tests/test_fuzz_target.py:30-52` | fuzz 入口漏掉 `normalize_domain`（唯一攻击者可控的第三方 Unicode 解析面）与 `parse_json_document`/`parse_spool_record`，而测试把这份 19 项清单钉死，新增入口反而会让测试变红 |
| m-147 | `tests/mock_collector.py:33-34,332` | 参考 collector 与协议 crate 在 decimal 值域上双向不一致：collector 接受超 `u64` 的十进制串，又拒绝 crate 视为合法的 `received_at_unix_ms="0"` |
| m-148 | `spool.rs:1203-1205`、`149-161` | 空 open segment 也在计时：`segment_max_age_seconds` 从上一次 seal 起算而非从首条记录起算，低速节点每条事件都被封成独立 batch（§9.3「空 segment 永不 seal」的意图被绕过） |
| m-149 | `spool.rs:1789-1800` | health 巡检发现 sealed 静默损坏后把该 batch 永久移出轮转队列且不做隔离，损坏对象要等到一次 `lease()` 才被处理 |
| m-150 | `user_audit.rs:2478-2487` | §22 的 CLOSED 预检把「非 retryable hello NACK 固定 5 秒重试」降级为关机期 10 Hz hello 风暴，并伪造 `hello_nack_count` |
| m-151 | `user_audit.rs:1180,1194,1206` | §22 把 `eligible_lazy` 的 miss 路径改成两次 `try_lock`：第一次已成功的数据报仍可能被记成 `udp_window_contention`，且 cooldown 条目永不写入；同时违反 §6.4「每个完整 UDP send 成功后只调用一次 `try_lock()`」，并把 `WouldBlock` 丢失系统性地偏向真正会产生事件的数据报 |
| m-152 | `user_audit.rs:2394` 等两处 | §22 把两处 gap 时间戳从 draft 的 `occurred_at` 改成 `wall_now().unwrap_or(0)`，却没有同步计入 `encode_error_time_unknown` 健康计数 |
| m-153 | `user_audit.rs:671,703-714,729-731,750-757,1712` | `GapAccumulator::saturated` 对 5 个 producer-gap 桶只写不读：`dropped_events` 触顶时无任何 degraded 健康信号，与 §6.5 冲突（`M-32` 只覆盖了 contention 桶） |
| m-154 | `user_audit.rs`（UDP 快路径） | 号称“便宜”的 UDP cooldown 命中快路径每个数据报仍有 2 次堆分配（交替拼写 4 次）；`kind` 是两个静态字符串却每包 `Arc::from` 一次 |
| m-155 | `udprelay.rs`（association_id 生成） | `association_id` 与发给客户端的 `server_session_id` 共用同一个非密码学 `SmallRng`（Xoshiro256++）线程局部流，导出审计记录里的 `association_id` 对同线程的已认证客户端可预测；§6.4 只要求“随机 128-bit”，但可预测性会削弱 §13 的证据价值 |
| m-156 | `crates/`（测试集） | §6.3 的 TCP wrapper 接线与 §1 的空 UDP payload 等价性均无端到端测试覆盖（本轮补测后确认行为正确，但护栏缺失） |
| m-157 | `auditd/src/ingest.rs:152-176,678-717` | dedup 触碰历史的压实阈值为容量的 2 倍，实测 order 峰值恰好 131072 条；回归测试只用 `with_capacity` 的小容量，§14.5「dedup LRU 必须证明有界」缺少真实容量下的证据 |
| m-158 | `auditd/src/ingest.rs:330-332,577-…` | hello 的 2 秒 deadline 被重复包裹两层 `timeout`，契约点不唯一 |
| m-159 | auditd 与协议 crate | §5.4 的七个固定常量各定义一份，与紧邻注释“不引入第二份会独立漂移的值”自相矛盾 |
| m-160 | `shadowsocks-service/src/config.rs` | feature-off 下 `user_audit` 的 unsupported-feature 合同零测试覆盖，且 `load` 路径本身不返回该错误（只有 `check_integrity` 返回）——§5.1 要求“load/check_integrity 遇到 Some 时显式返回 unsupported-feature” |
| m-161 | `shadowsocks-service/src/config.rs`（manager 互斥测试） | manager 模式互斥的唯一测试是空测：断言在读到 `ManagerServerMode` 之前就已由 `ConfigType` 决定；且 server 配置同时携带 `manager_address` 与 `user_audit` 未被拒绝 |
| m-162 | `tests/check_audit_static.py:17-27` | 静态护栏完全不扫描 `tcprelay.rs`/`udprelay.rs`/`context.rs` 里的审计代码——在 UDP 审计热路径注入 `.unwrap()` 与必然越界下标后仍输出“静态审计通过”并 exit 0（`m-82`/`m-114` 的第三次残留） |
| m-163 | `tests/benchmark_audit.py:104-107,131,164`、`test_benchmark_audit.py:40` | `m-118` 只修了一半：`proxy_errors` 结构上恒为 0，scenario gate 的 `proxy_errors==0` 项恒真，测试把恒真式当断言 |
| m-164 | `tests/benchmark_audit.py:26-29,246,254-262,287-290` | §14.5 性能门禁会在**从未测量 auditd RSS**、且对照报告不含 user-audit 构建的情况下输出 `gate=true` 并 exit 0 |
| m-165 | `tests/test_check_audit_static.py:116-126` | 把 `m-135` 的 partial-tree 欠扫描当作正确行为写成正向断言（重蹈 `m-116` 的覆辙） |
| m-166 | `scripts/build.sh:64-95`、`scripts/build-linux-release.sh` | 两者共用 `dist/`：README/OPERATIONS 记录的发布顺序会在四次 musl 构建**之后**才失败，且开发产物可覆盖已签名发布目录中的二进制 |
| m-167 | `scripts/build-linux-release.sh:76-84,113-173` | 发布构建的环境净化清单漏掉 `CARGO_PROFILE_RELEASE_OVERFLOW_CHECKS`/`DEBUG_ASSERTIONS`、`RUSTC_BOOTSTRAP` 与 cc-rs 的 target 作用域 `CC`/`CFLAGS`/`AR`；两次孪生构建无法发现，破坏跨构建者可复现性 |
| m-168 | `scripts/release-artifact.py:189-198,382-403` | manifest 的 `independent_builds`/`source_date_epoch` 属于不可验证的自述字段；固定工具链在验证机上不存在，§15.1 的“两次独立可复现构建”从未实际执行且未在 §22.3 披露 |
| m-169 | `tests/README.md` | §14.5 的性能门禁工具 `tests/benchmark_audit.py` 在任何面向使用者的文档中都无记载，§16「足以让另一位工程师独立对接」不成立 |

### 24.6 验收建议

1. **先修 `M-42` 与 `M-49`**——这两条使 §23.9 列为发布前置的两条 Linux 实跑命令
   （`cargo test -p shadowsocks-auditd`、`cargo test --workspace --features user-audit`）
   都跑不过。加上仍未修的 `C-6`，Linux 上的第一次完整实跑目前必然红。
2. **`C-6`（§23）仍未修复**，4 处
   （`spool.rs:1018,1019`、`user_audit.rs:2422,2423`）原样保留；`M-41`（§23）同样未修
   （`durable_gap_reasons` 仍在 `spool.rs:1066` 建立，晚于 `1034/1044/1053/1058` 的 flush）。
3. **`M-43`/`M-44`/`M-45`/`M-46`/`M-47` 应同批处理**：五条都会把“瞬时故障”放大成“永久或长期
   的审计停摆/数据不可导出”，且都发生在 §22 刚重写过的 spool 路径上。
4. `M-48` 需要连同 §14.5 的方法学一起修：基线应改为 upstream v1.24.0，否则任何不在 feature
   gate 内的回退都测不出来。
5. `M-50` 在重新生成补丁时剔除，并在 `verify.sh` 增加对应断言。
6. `M-51` 的变异检验建议扩展成流程：对 §19/§22 声称“已修复且带回归测试”的条目逐条做一次
   “删除修复看测试是否变红”的检验，再写入验收记录。
7. 更正 §22.3 与 §23.1/§23.8 的失真条目（见 24.4），并把
   `x86_64-unknown-linux-gnu` target 与 `.cache/audit-work-source` 的创建步骤写进验证清单。
8. Linux runtime 实跑、fuzz sanitizer 实跑、§14.5 目标机压测仍为发布前置，且在 1-3 修完之前
   无法开始。

## 25. 第六轮整改记录（2026-08-29）

本节记录对 §24 发现的整改结果，不改变 §1–16 的合同。源码先在
`.cache/audit-work-source` 中重放锁定上游与 `0001`/`0002`，再由 `0003-user-audit.patch` 导出；本节
的“已修复”表示当前源码与可执行回归护栏已闭合，不替代 Linux 原生发布验收。

### 25.1 Critical、Major 与回归护栏

| 条目 | 整改结果 |
| --- | --- |
| C-6、M-41、M-42 | supervisor 与 spool 的启动时间计算改为不下溢的 checked 算术；启动时先建立 durable gap 索引再 flush reset；segment 边界测试改为分别验证协议记录上限和 16 KiB 运行下限。 |
| M-43、M-44、M-45 | sealed/open 元数据与 body 使用同一 min/max 语义；cleanup 以真实释放字节和有界迭代判据停止，不把 quarantine 搬移当作净释放，也不让自产 gap 触发清理自激。 |
| M-46、M-47 | 无法确认 durability 的 write barrier 进入 sticky fatal 状态，拒绝所有公开操作并通知 daemon 退出，由 systemd 重启恢复；空 open 与路径失配会先持久化来源绑定 marker，再隔离孤儿文件、写入 `segment_corruption` gap 并重建可写 open 段。 |
| M-48、M-49 | shutdown 等待者改为 `watch` 接收器，移出 TCP accept/UDP 收包热路径；producer 与 auditd 对绝对 socket 路径都做词法 dot-segment 校验。 |
| M-50、M-51 | 重新生成的 patch 不含幽灵删除 stanza；`verify.sh` 与 `prepare-source.sh` 拒绝无实际内容的删除项；idle-session 回归通过 test-only rendezvous 确实覆盖 queue 复检。 |

### 25.2 Minor、协议与交付工具

- `m-143`–`m-161`：整理 segment 最小值计算，修正 UTS #46 末尾全角句点、decimal `u64` 值域、空
  open age、sealed integrity 轮转、诊断时间/饱和计数、UDP cooldown 分配与 association 随机源；补齐
  默认 dedup 容量的历史压实断言、feature-off 配置互斥测试、TCP wrapper 与空 UDP payload 的端到端
  回归。
- `m-162`–`m-165`：静态护栏扫描完整 relay/context 审计接线并拒绝 partial source tree；benchmark
  独立校验代理成功/错误计数，缺少真实 data-path 或 auditd RSS 时保持 `gate=null`，不会伪造通过。
- `m-166`–`m-169`：开发构建与签名发布使用隔离输出目录；发布构建清理额外环境变量并要求两套独立二进制
  字节一致；manifest 绑定两次构建、source epoch 和 patch series；测试文档补充性能门禁命令与验收边界。
- protocol golden vectors 已扩展并同步 Rust 与 Python；fuzz target 覆盖 canonical/document/spool
  parser、frame parser 和 `normalize_domain`。`.cache`、Cargo target、fuzz corpus 与 Python 缓存仍由
  `.gitignore` 排除。

### 25.3 当前验证结果与边界

本机为 macOS，当前已安装 `x86_64-unknown-linux-gnu` Rust target。以下检查已实际通过：

```text
python3 tests/test_mock_collector.py                 # 17 passed
python3 tests/test_audit_packaging.py               # 9 passed
python3 tests/test_release_artifact.py              # 10 passed
python3 tests/test_benchmark_audit.py               # 4 passed
python3 tests/test_check_audit_static.py            # 12 passed
python3 tests/test_fuzz_target.py --source ...      # 2 passed
python3 tests/test_http_unix.py                     # 14 passed
python3 tests/test_integration_audit.py             # 3 passed
cargo fmt --all -- --check
cargo test --locked -p shadowsocks-audit-protocol  # 23 passed
cargo test --locked -p shadowsocks-service --no-default-features --features server --lib # 12 passed
cargo check --locked --target x86_64-unknown-linux-gnu -p shadowsocks-auditd --all-targets
python3 tests/check_audit_static.py --source .cache/audit-work-source
./scripts/verify.sh                                 # 完整通过；auditd Linux runtime 未执行
```

`cargo test -p shadowsocks-auditd` 与完整 `user-audit` workspace runtime 不能在 macOS 执行（auditd
明确是 Linux-only）；交叉 `cargo check` 只证明目标 cfg 可编译，不证明 `SO_PEERCRED`、UDS 权限、signal、
crash/capacity 恢复或真实 producer runtime。`cargo-fuzz` sanitizer 实跑、Linux runtime 集成和 §14.5
目标机吞吐/CPU/RSS/长时间压测仍是发布前置，合成 benchmark 预检不能替代这些证据。

`verify.sh` 会先通过 `scripts/prepare-source.sh` 创建临时源码树；若直接运行源码级 Rust 命令，必须先执行
`scripts/prepare-source.sh .cache/audit-work-source`。本轮 patch 已用
`git apply --check --binary --whitespace=error-all` 和 `patch --dry-run --fuzz=0 -p1` 在包含
`0001`/`0002` 的干净基线验证，并与准备源码树逐文件核对。

## 26. 第六轮整改补充验收（2026-08-29）

本节补充 §25 在崩溃一致性、热路径和发布证据上的最终复核结果。它更正 §25.1 中 M-46 曾描述为
“进程内恢复”的表述；当 state durability 已无法判定时，继续服务会有复用 spool cursor 的风险，
因此正确行为是 fail closed 并让服务管理器重启进程。

### 26.1 Spool 崩溃一致性与恢复

- M-46 的 fatal 状态是 sticky 的；append、lease、oversized quarantine 与 ACK 的每次拒绝都增加
  `storage_rejected_attempts`，watch 通知使 daemon 终止，空闲 ingest/export 连接会被 abort，不会
  阻止 systemd 的 `Restart=on-failure`。
- `state.json` 原子写区分 rename 前失败和 rename 后不确定。rename 前失败可以回滚 open record；
  rename 已完成但父目录同步或路径计量失败时，不再截断已经写入的 record，也不回退内存 cursor，
  而是立即进入 fatal 状态。重启回归证明同一 `(spool_epoch, spool_sequence)` 不会复用。§8.3 仍允许
  producer 在 auditd 重启后重放同一 `event_id`，两者不是同一个唯一性合同。
- M-47 的 orphan-open 处理在 rename 前先写 schema 2 recovery marker，marker 携带私有
  `source_fingerprint`；同一来源在同进程重试和重启后复用同一 gap identity，无关损坏对象不会误用
  该 marker。append、lease、ACK 与 marker unlink 失败后的路径都会重新实测 spool bytes，避免容量
  索引长期高估或漏计。

### 26.2 Producer 热路径

- M-48 的 TCP accept、UDP 主收包与 UDP worker 三个长循环各自在进入循环前只构造并 pin 一次
  shutdown future；静态回归同时约束构造位置与调用次数，把 waiter 移回循环的变异会失败。
- 每个成功 UDP send 最多执行一次 `try_lock()`。cooldown 查询用 `hashbrown::Equivalent` 借用 IP 或
  domain 原始拼写；已缓存命中不创建 owned key。domain alias 明确限制为一个 canonical primary 加
  一个可替换 alternate，第三种拼写会替换 alternate，因此内存有界但不会保留任意数量的拼写。
- idle emitter 测试使用 emitter-local 双向 oneshot rendezvous，确定 session 已进入 queue 复检窗口；
  删除复检的变异会失败，不再依赖调度巧合。

### 26.3 Benchmark 与发布证据

- 原生 data-path 报告绑定三种 case 的二进制哈希与 runtime 状态、ingest/export socket 的
  device/inode、三个互不相同的服务 UID、启用态 runtime ID、前后两份签名 health、
  `stored_records` 增量及 `last_ingest_at` 前移；auditd RSS 和 worker success/error 来自同一 run ID。
- `build-and-receipt` 实际执行两次独立的 `cargo-zigbuild zigbuild`，直接调用经版本与 inode 校验的
  `cargo-zigbuild` 绝对路径；构建 cwd 固定为 source root，使用独立空 `CARGO_HOME`，拒绝 source root
  到文件系统根的 `.cargo/config{,.toml}`，且只向子进程传递显式 allowlist 环境。helper 同时实际核验
  Cargo、rustc commit、Zig 与工具 inode；Python 版本、路径和 inode 绑定实际运行
  `release-artifact.py` 的解释器，不信任 `PATH` 中另一份 `python3`。receipt 绑定 live source/target 与
  两项 ELF64 artifact；entry point 必须位于 file-backed executable `PT_LOAD`。
- `upstream.lock` 固定完整 prepared source tree 的 SHA-256。build、package、sign 与 verify 均要求
  两份 receipt、live tree、manifest 和该 trust anchor 一致。package 持有已验证 device/inode 的发布
  目录 FD，以 exclusive create 写入，并在返回前复核原目录路径身份、精确 unsigned 成员集合及每个成员
  的 inode、mode 和 bytes；失败时只清理本次创建且 inode 仍匹配的文件。
- sign/verify 将整个发布目录一次安全读取为内存 snapshot，绑定目录 device/inode、精确
  unsigned/signed 成员集合及每个成员的 inode、mode 和 bytes；结构校验与 OpenSSL 签名/验签只消费
  snapshot bytes，签名、公钥和私钥通过已经打开的普通文件 FD 传递。manifest、签名、artifact、额外
  成员或目录路径在操作期间替换都会失败；shell 包装器在结束前再次检查 overlay HEAD 与干净工作树，
  签名期间仓库漂移会删除本次新建且 inode 仍匹配的签名。
  receipt 仍不提供恶意构建主机/构建者抗性，也不是密码学的 command-execution attestation；它不
  证明工具二进制或 builder 身份可信，不穷举/证明完整宿主环境或 Cargo config，也不把提交时间戳
  变成可信时间证明。两个 build 进程或主机是否真正相互独立仍属于发布方组织控制，而不是 receipt
  可证明的事实。

### 26.4 验收边界

macOS 本机已执行 protocol、feature-off/service、静态、packaging、release、benchmark、collector、
补丁重放与格式检查；state commit uncertainty 的定向用例另在 Debian x86_64 原生执行通过。
最终 `./scripts/verify.sh` 完整通过，其中 workspace service 308 项、protocol 23 项、release 21 项、
benchmark gate 10 项、packaging 11 项、static 13 项、mock collector 17 项与数据面集成均通过；另行
执行的 feature-off service lib 14 项也通过。
macOS 上的 Linux target `cargo check --all-targets` 不能替代 Linux runtime。完整
`SO_PEERCRED`/UDS/signal/crash-capacity 集成、sanitizer fuzz、§14.5 目标机长压，以及固定工具链的两次
musl release 构建和最终签名验签仍是发布前置，不得由合成 benchmark 或交叉编译结果替代。

## 27. 第七轮代码审计记录（2026-08-29）

> 本节是对第六轮整改（commit `b5f139a` 与 `76f80fc`，§25/§26）的对抗性验证记录，不改变合同条文。
> 新增问题编号接续：major 自 `M-52` 起，minor 自 `m-170` 起；本轮无新 critical。行号基于当前
> `.cache/audit-work-source`（与 `patches/0003-user-audit.patch` 逐字一致，48 个文件
> `patch --dry-run -R -p1 --fuzz=0` 与 `git apply --check --binary --whitespace=error-all` 均通过）。

### 27.1 审计范围与方法

- 对象：第六轮整改后的全部交付物；重点是 §25/§26 每条声明的代码证据、整改 diff 引入的回归，
  以及前轮遗留项的关闭情况。
- 方法：四路并行对抗性验证（producer / auditd / 协议与测试工具 / 发布与一致性）；延续 §24 的
  "真正执行代码"方法——auditd/producer 在 /tmp 桩本（去 compile_error!、libc shim）实测复现，
  关键修复做变异检验（回退修复看测试是否变红）；M-52、M-53、m-171 等关键发现由编排者亲自复核
  源码确认。所有实验副本事后删除，审计树逐字还原。
- 本机（macOS）实跑：protocol 23、workspace user-stats 308、feature-off service lib 14、
  打桩 feature-on 99、EIH 4、Python 套件（mock 17、release 21、packaging 11、static 13、
  benchmark 10、integration 3、fuzz 2）全绿；交叉 `cargo check --target
  x86_64-unknown-linux-gnu -p shadowsocks-auditd --all-targets` 零警告；`cargo fmt --check` 通过。
  例外：`test_http_unix.py` 5 项失败与 `check-sensitive.sh`/`verify.sh` 失败关闭，全部精确落在
  当前机器缺 `rg` 的依赖点（m-180）。
- 未做：Linux runtime 实跑、fuzz sanitizer 实跑、§14.5 压测、两次 musl 实建实签。

### 27.2 总体结论

§25/§26 的修复声明**逐条基本属实且经得起实测**：C-6（两进程四处 `Instant` 下溢）、M-41（索引
前移到 flush 块之前）、M-42/M-43/M-44（§21/§24 复现场景重放全部有界退出且无自指 gap）、M-46
（sticky fatal 状态机逐行核对一致）、M-47（orphan marker 九场景 fault-injection 测试真实）、
M-48（变异检验实测通过）、M-50（幽灵 stanza 剔除且双工具重放均通过）等均确认闭合。交付一致性
三项核查（补丁逐字同步、合同文本无夹带、§26.4 数字复跑 13 项中 10 项逐数一致）通过。

本轮无新 critical，但有 **2 个新 major**：`M-52`（m-157 修复把 dedup 压实改成 O(n²)，且使
`cargo test -p shadowsocks-auditd` 在默认 debug profile 下事实上不可运行——发布门禁的可观测性
反而退步）与 `M-53`（静态护栏对多行签名函数整体失效，m-162 的原始复现依旧漏报）。另有
M-45 残留（部分修复）与 §24 M-51 点名的"同名不绑定"测试模式在 M-41/M-44 上重现（m-173）。

### 27.3 Major

- **M-52 dedup 压实阈值改为 `capacity + 1`，把摊还 O(1) 退化为每次命中 O(n)，重放风暴下为
  O(n²)，并使其 shipped 测试在 debug 下不可运行。** `ingest.rs:168-175`：命中每次向 `order`
  追加一个 token 项，`order.len() > capacity + 1` 即对 65536 项做全量 `retain`（含逐项 HashMap
  查询）——压实测得每 2 次命中触发一次全量扫描。桩本实测：真实容量回归测试
  （`ingest.rs:717-743`）release 46.8 秒、debug 超 120 秒被终止；同负载换回旧阈值 0.02 秒
  （约 2300 倍差）。运行期影响：dedup 查询在每事件热路径（`ingest.rs:399-405`），producer
  重连重放（in-flight 256 + queue 4096）全部命中，一次重放风暴额外消耗秒级 CPU 且持有 dedup 锁。
  连带：发布前置命令 `cargo test -p shadowsocks-auditd` 在默认 debug profile 会在该用例上挂住
  数十分钟级，M-42 修复的"套件绿"在默认工程实践中仍不可观测。修复：恢复滞后阈值（如
  `2 × capacity`）或更换数据结构（侵入式 LRU 链表），而非把阈值压到 capacity+1。（§8.3 §14.5）
- **M-53 静态护栏的函数范围判定对多行签名函数整体失效，m-162 原始复现依旧漏报；另有一条
  新增死代码扫描器。** `tests/check_audit_static.py:373-378`：`saw_brace` 置真后的结束条件
  `depth_before[cursor] <= baseline_depth` 在多行签名的结束行即成立，审计范围只剩签名行，
  **连审计标记行本身都不在范围内**。实测（/tmp 副本）：在 UDP 审计热路径（`udprelay.rs`
  `dispatch_received_packet` 内）注入 `.unwrap()` 与必然越界下标，检查器仍输出"静态审计通过"
  rc=0——即 m-162 的原始复现；对照注入单行签名函数则正确报错。量化：三个 relay/context 文件中
  40 个审计标记有 17 个落空。本轮新增的两条 relay 单测只用单行签名 fixture（M-51 式
  "同名不绑定"再现）；另 `_check_relay_wiring_file`（`check_audit_static.py:399-474`）定义后
  从未被 `check()` 调用，是死代码。修复：结束条件改为"开括号之后深度回落到 baseline"，补多行
  签名 fixture 的回归测试，接线或删除死扫描器。（§14.4 §7.3 panic-free 护栏）

### 27.4 §25/§26 声明验证结果汇总

- **已修复（逐条证实，多数带真实/变异绑定测试）**：C-6（producer 与 auditd 四处全部改
  `checked_sub`，全树同类模式清零）、M-41（代码）、M-42（含 m-143 死代码整理）、M-43（桩本
  双场景实测）、M-44（代码；三场景重放有界退出）、M-46/§26.1（fatal 状态机、health 可观测、
  watch 退出链、不复用游标）、M-47/§26.1（marker schema 2 + source_fingerprint、九场景测试）、
  M-48（watch 移出三个热循环，变异检验实测变红）、M-49（词法 dot 校验，独立 rustc 复算六种
  拼写全拒）、M-50（幽灵 stanza 剔除 + check-patch-deletions.py + 双工具重放实测 rc=0）、
  M-51（rendezvous 绑定，删复检实测变红）、m-144（UTS #46 顺序修正，全角尾点实测一致）、
  m-145（15 条 record + 2 条 NDJSON + 3 条 HMAC 向量独立重算全对）、m-146（fuzz 入口 22 项、
  清单改存在性断言）、m-147（decimal 值域双侧差分实测一致）、m-148、m-149、m-150、m-151、
  m-152、m-153、m-154、m-155、m-156、m-158、m-160、m-161、m-163、m-164（三态门禁实测）、
  m-165、m-166、m-167、m-169。
- **部分修复**：M-45（per-candidate 判据已区分搬移/释放，但迭代级判据把 gap 写入成本计入
  "无进展"，§24 场景仍需两次 cleanup；m-174）；m-157（真实容量测试存在但引入 M-52 回归）；
  m-159（残留 TOMBSTONE_RETENTION_SECONDS 双定义，m-178）；m-162（机制落地但对主要目标失效，
  升级为 M-53）。
- **无回归抽查**：M-30/M-31/M-32/M-33/M-36/M-37、C-5 四要件、m-89/m-90/m-92/m-99、
  m-95/m-122/m-123、m-40/m-41(§21) 均成立。
- **遗留未修（未被 §25/§26 声称）**：§23.7 两项规格文本（v4 未升版）；m-139（§10.3 中介细则
  未落文档）；m-142（Instant 下溢静态护栏）；m-129、m-132、m-140 原样保留。

### 27.5 Minor

| 编号 | 位置 | 问题 |
| --- | --- | --- |
| m-170 | `user_audit.rs:2610-2611`、`spool.rs:1159-1162` | C-6 修复无单测；`Instant - Duration` 下溢模式未加入 check_audit_static.py（§23 m-142 建议未实施），同类回归无护栏 |
| m-171 | `user_audit.rs:1304-1350` | m-91 部分回归：m-151 的单次 try_lock 修复以把 `normalize_domain` 搬回 shard 临界区为代价（≥3 种交替拼写时每包锁内规范化+alternate 替换；两槽 alias 使 ≤2 种拼写零分配命中）；§25.2/§26.2 未披露该权衡，m-91 结论应降为"以 alias 缓存收窄、规范化回到锁内" |
| m-172 | `user_audit.rs:3796-3810` | m-150 后半条无直接测试：`hello_nack_count==0` 断言被平凡满足（mock socket 不存在，走不到 hello NACK 路径）；需先 hello NACK 再 close 的用例 |
| m-173 | `spool.rs` 测试集 | M-41/M-44 两条关键修复均无可绑定的回归测试：变异检验实测（索引挪回 flush 之后/删除自激守卫）shipped 71 项全绿；§23.9 点名的"启动时 gap 已 durable"与 C-5 迭代上限用例仍未交付（§24 M-51 模式重现） |
| m-174 | `spool.rs:2836`（同构 2799/2890） | M-45 残留：迭代级判据 `spool_bytes >= iteration_start_bytes` 把第一条 corruption gap 的写入成本计入"无进展"，§24 场景（1 损坏 acked + 2 健康 sealed）仍需两次 cleanup；建议改跟踪"累计释放字节" |
| m-175 | `spool.rs:2757-2761` | corrupt-acked 分支 break 前把 `progressed` 重算为字节比较（必为 false），与同构分支不一致；cleanup 中途外部释放磁盘时可多驱逐一个 quarantine 对象（有界、方向保守） |
| m-176 | `spool.rs`（durable_gap_reasons） | m-132 残留仍在：索引只增不删，靠 rebuild 清零；建议删除 segment 时剔除或注释界限 |
| m-177 | `spool.rs:190-203` | orphan recovery marker（schema 2 + source_fingerprint）是私有 on-disk 格式，未进协议 crate、无跨栈 golden vector；严格性由 parse_canonical + deny_unknown_fields 提供 |
| m-178 | `spool.rs:36`、`audit-protocol/lib.rs:53` | m-159 残留：`TOMBSTONE_RETENTION_SECONDS` 与 `RECEIPT_RETENTION_SECONDS` 仍是同一 §5.4 常量的两份定义 |
| m-179 | `tests/test_benchmark_audit.py` | 缺负向单测：三 UID 互异、auditd measurement_source/run_id 不一致——删除这些绑定校验不会有测试变红 |
| m-180 | 环境/记录 | `rg` 环境事实第三次翻转（§23 缺→§24.4 有→本轮缺）：§26.4 的 http_unix 14、check-sensitive、verify.sh 三项当前不可复现（均 fail-closed，无假绿）；建议验证记录写入前当日实跑确认环境前提 |
| m-181 | `docs/PERFORMANCE.md:62` | 仍写 feature-off/on 两案口径，与 tests/README 已改的三案方法论（upstream 原始/runtime-off/runtime-on）不同步（非合同文档，应同步） |
| m-182 | `docs/OPERATIONS.md:306-328,388-444` | m-139 遗留未修：§10.3 三条细则（endpoint↔node 一一映射、转发前拒绝 >4096 bytes body、access log 不记 Authorization/MAC 头）未落文档；user-stats nginx 块仍无就地"不可照抄"标注 |
| m-183 | `context.rs`（DataShutdownState） | 关机协调代码无审计标记也无 cfg 属性，脱离静态护栏范围；未来在其中引入 unwrap 不会报警 |
| m-184 | `user_audit.rs:2130` | queue_overflow 观测路径墙钟失败用 `unwrap_or(0)` 且不计 `encode_error_time_unknown`，与另两条 gap 路径不一致（实际不可达，记录不一致性） |
| m-185 | `user_audit.rs:2589-2593` | m-129 残留：force-final 完成条件不可达原样保留 |

### 27.6 需规格处理的文本项

1. §23.7 第 1 条（§7.3"合法 ACK 重置"收紧为"合法 event ACK"）与第 2 条（§9.5
   `quarantine_pending` reason 枚举补 `segment_corruption`）仍未落实，合同维持 v4；
2. §5.1 的 TOML 片段未列本轮新增的 `dep:hashbrown`（根与 service Cargo.toml 已实际引入），
   升版时需补记；
3. 以上随下次升版一并处理；处理前实现与合同文本的偏差以本节与 §23.7 的记录为准。

### 27.7 交付一致性核查

- 补丁↔源码树：48 文件 `patch --dry-run -R -p1 --fuzz=0` 与 `git apply --check --binary
  --whitespace=error-all` 均 rc=0；幽灵删除 stanza 已剔除（`deleted file mode` 计数 0）；
  fresh 重放树 SHA-256 与 `upstream.lock` 的 `prepared_tree_sha256` 逐字一致。
- 合同文本：`b5f139a`/`76f80fc` 对 §1–16 零改动；头部版本沿革链完整。
- §26.4 清单复跑：13 项中 10 项逐数一致（308/23/21/10/11/13/17/14/3/2）；3 项因当前机器
  缺 `rg` 不可复现（m-180，fail-closed 无假绿）。"state commit uncertainty 定向用例在
  Debian x86_64 原生通过"为可信声明（用例真实存在于 spool.rs 测试模块），本机不可复现。
- 敏感信息：tracked 文件零命中；git 工作树干净；`.gitignore` 覆盖新增生成物（fuzz
  corpus/artifacts、dist、tests/audit-runtime 等）；无孤儿测试文件。
- verify.sh 的 `STRICT_FMT` 默认关闭项是有意弱化（上游 v1.24.0 与现版 rustfmt 不兼容，
  有注释说明），非静默跳过。

### 27.8 验收建议

1. **先修 M-52**（恢复滞后压实阈值或换侵入式 LRU），否则发布前置命令
   `cargo test -p shadowsocks-auditd` 在 debug 下挂住数十分钟级，门禁不可观测；
2. **修 M-53**（函数范围结束条件 + 多行签名 fixture + 接线/删除死扫描器），否则 relay 热路径
   的 panic 护栏形同虚设；
3. 补 m-173 的两个变异可绑定用例（M-41 启动幂等、M-44 自激守卫/C-5 迭代上限），并落实
   m-170 的 Instant 下溢静态规则；
4. m-174（M-45 残留）改为跟踪累计释放字节；m-171 的锁内规范化权衡在 §25/§26 记录中补披露；
5. 规格文本项（27.6）随下次升版处理；m-180/m-181/m-182 排期；
6. Linux runtime 完整实跑、fuzz sanitizer、两次 musl 实建实签、§14.5 目标机压测仍为发布前置。

## 28. 第八轮代码审计记录（2026-08-29）

> 本节是对第六轮整改（commit `b5f139a` 与 `76f80fc`，§25/§26）的第八轮审计，不改变合同条文。
> 编号接续：critical 为 `C-7`，major 自 `M-54` 起，minor 自 `m-186` 起。行号基于当前
> `patches/0003-user-audit.patch` 应用树。

### 28.1 审计范围与方法

- 对象：第六轮整改后的全部交付物。重点是整改自身引入的回归（`spool.rs` 5743→6837 行、
  `user_audit.rs` 3714→4052 行）、**新重写的整条发布链路**（§26.3），以及 §27 未覆盖的盲区。
- 方法：10 维度并行审查 + 逐条对抗性复核（59 条候选 → 驳回 5 条、保留 54 条），
  **36 条由 agent 实际运行代码复现**；本轮把“变异检验”（把修复回退掉看测试是否变红）作为
  系统性方法用在每一条“已修复且带回归测试”的声明上。两个最严重的条目（`C-7`、`M-60`）
  由编排者亲自复核确认。
- 本机（macOS）实跑：feature-off `cargo check` 通过；protocol 23、workspace `user-stats` 308、
  service `--features server --lib` 14 全部通过；`cargo fmt --all -- --check` 通过；
  Python 套件 mock 17、static 13、benchmark 10、packaging 11、release 21、http-unix 14、
  integration 3、fuzz 2 全部通过；`check_audit_static.py`、`test_panic_abort.py`、
  `check-sensitive.sh`、`check-patch-deletions.py` 均通过。
- 独立核实的交付锚点：新鲜准备的源码树 SHA-256 = `747a6a08…`，与
  `upstream.lock` 的 `prepared_tree_sha256` **逐字一致**，`verify.sh` 的 fresh-replay 校验真实有效。
- 本机失败：`cargo check --target x86_64-unknown-linux-gnu -p shadowsocks-auditd`
  （该 target 未安装）。
- 未做：Linux runtime 实跑、fuzz sanitizer 实跑、§14.5 目标机压测、两次 musl 实建实签。

### 28.2 总体结论

§25/§26 的整改把 §24 的 10 个 major 基本收敛（§27 已逐条核过），发布链路的新 trust anchor
（prepared tree SHA-256）也经本轮独立复算确认有效。但本轮发现 **1 个 critical 与 9 个 major**，
其中最重要的是三点：

1. **`C-7`：交付的 packaging 模板使审计功能在正确安装的 Linux 节点上完全不可用，并连带阻断
   数据面。** `/run/shadowsocks-audit` 被设为 `0750 shadowsocks-audit:shadowsocks-audit`，而
   producer（`shadowsocks`）与 export peer（`audit-exporter`）都**不在** `shadowsocks-audit` 组里，
   因此无法**遍历**该父目录去访问其下的两个 socket 目录。这与已修复的 `C-4` 同型：只有真正在
   Linux 上装一遍才会暴露。
2. **`M-62`：第六轮把 `verify.sh` 的一道门禁从 `die` 降级为警告，导致本机绿灯不代表审计代码被
   编译过。** `scripts/test.sh:108-113` 现在在缺少 Linux target 时只打印“未验证”并继续
   （需显式 `SHADOWSOCKS_REQUIRE_AUDIT_TARGET=1` 才恢复为失败）；而非 Linux 上 workspace 又只用
   `--features user-stats`。于是在本机 `verify.sh` 输出“验证完成：…均通过”时，
   **`shadowsocks-auditd` 整个 crate 与 `user_audit.rs` 一行都没有被编译**。这正是“交付即失败的
   测试能连续存活多轮”的结构性原因。
3. **变异检验揭示护栏大面积不绑定。** 本轮实测：发布链 `release-artifact.py` 的 96 个变异中
   89 个存活；benchmark 门禁 `_evaluate_data_path` 的 33 条证据校验中 30 条变异存活；
   mock collector 的 85 条协议校验中 76 条存活；`M-45` 的三处修复整体回退后 shipped 用例仍全绿；
   `m-146` 的 fuzz 入口断言注释掉 21/22 仍全绿。§27 用同一方法找到 m-173，本轮说明这是普遍现象
   而非个例。

另外，`M-52`（§27）出现了“修复被自己的测试锁死”的情况：`m-157` 的回归测试把
`capacity + 1` 阈值写死成断言，按 §27.8 的建议修复必然让该测试变红（`m-200`）。

### 28.3 Critical

- **C-7 `/run/shadowsocks-audit` 的属组与权限使 producer 与 export peer 都无法遍历，审计在正确
  安装的 Linux 节点上零可用，并连带阻断 ssserver 启动。**
  `packaging/shadowsocks-auditd.tmpfiles:2` 建出
  `d /run/shadowsocks-audit 0750 shadowsocks-audit shadowsocks-audit`；
  `packaging/shadowsocks-auditd.service:29-30` 的 `RuntimeDirectory=shadowsocks-audit` +
  `RuntimeDirectoryMode=0750` 配合 `User=/Group=shadowsocks-audit` 产生同样的属主属组与模式，
  两条路径一致。而 `packaging/shadowsocks-auditd.sysusers:8-12` 的组成员关系是：

  ```text
  m shadowsocks       shadowsocks-audit-ingest
  m shadowsocks-audit shadowsocks-audit-ingest
  m shadowsocks-audit shadowsocks-audit-export
  m audit-exporter    shadowsocks-audit-export
  ```

  即 **`shadowsocks` 与 `audit-exporter` 都不是 `shadowsocks-audit` 组的成员**。访问
  `/run/shadowsocks-audit/ingest/ingest.sock` 需要对每一级路径组件有 `x` 权限；对这两个账号
  而言 `/run/shadowsocks-audit` 的“other”位是 `---`，因此 `stat`/`connect` 一律 EACCES。
  §11 精确规定了两个子目录（`0750 shadowsocks-audit:shadowsocks-audit-ingest`/`-export`）与两个
  socket（`0660 shadowsocks-audit:<对应组>`）的属组，却没有规定**共同父目录**，实现选择了
  最严格的 `0750 shadowsocks-audit:shadowsocks-audit`，恰好把两个被授权账号挡在门外。

  后果链：按 `packaging/README.md` 第 3 步与 `docs/OPERATIONS.md:96-111` 安装后，auditd 自己
  正常启动并在两个子目录里 bind 成功（它是属主）；随后启动 `shadowsocks-rust-plus.service`，
  ssserver 以 uid `shadowsocks` 加载带 `user_audit` 的配置，配置完整性检查在 stat
  `/run/shadowsocks-audit/ingest` 时拿到 EACCES → 返回配置错误 → 进程退出 →
  `Restart=on-failure` 每 3 秒重启一次，**代理数据面永远起不来**。即使放宽 producer 侧校验，
  `connect()` 也会因同一祖先目录 EACCES 失败，审计事件一条也产生不了；export peer 同理无法
  访问 export socket，controller 永远拿不到数据。

  这不是 §7.1 允许的“审计漏记”，而是数据面完全不可用，违反 §1「合法配置下的运行时审计不得
  阻断代理流量」与 §11 的授权模型意图。与已修复的 `C-4` 同型：只在真实 Linux 安装时暴露，
  静态阅读与 `cargo check` 都看不见。
  修复：把父目录放宽为可遍历（`0755`，或 `0751 shadowsocks-audit:shadowsocks-audit`），
  子目录与 socket 的 `0750`/`0660` 分组隔离保持不变；同时在
  `tests/test_audit_packaging.py` 增加“两个被授权账号必须能遍历到各自 socket”的断言，
  并在 §11 中补写父目录的属组与模式要求。

### 28.4 Major

- **M-54 恢复期多个独立损坏对象被压成同一个 gap ID，只产生一条详情全为 null 的
  `segment_corruption` gap，丢失证据被吞。** `spool.rs:825-842` 的
  `corruption_gap_spec_or_existing` 复用判据为
  `reason == SegmentCorruption && source_fingerprint.is_none() && spec.lost_batch_id == hint.batch_id
  && spec.lost_spool_epoch == hint.spool_epoch`。当 `read_meta_hint`（`4477-4499`）因
  meta.json 不可读/不合法/`node_id` 不匹配而返回 `None` 时，两侧退化为 `None == None`，
  于是**任意一条已入队的 null 详情 gap 都会匹配任意一个新的 null 详情损坏对象**；
  `enqueue_recovery_gap`（`715-737`）又按 `event_id` 去重，队列最终只剩一条。
  实测两个场景：(a) 3 个 `meta.json` 被写坏的 sealed batch → quarantine 4 个对象、
  `segment_corruption gaps = 1`；(b) 只把配置里的 `node_id` 从 `node-test` 改成 `node-other`
  重启 → 5 个 batch 全部进 quarantine，导出记录只有一条
  `reason=SegmentCorruption, lost_batch_id=None, lost_events=None`。
  对照组（meta 可解析、只坏 body）则正确产生 3 条各带独立 `lost_batch_id` 的 gap，证明缺陷特异性。
  下游无法知道丢了几个 batch、哪些 sequence 区间、多少事件，n-1 次丢失完全无证据——正是 §9.4
  禁止的“伪装为无数据丢失”。`tail_truncation_gap_spec_or_existing`（`844-869`）同源。
  归因更正：该函数不是第六轮引入的（`4a6348e` 即存在），第六轮只补了
  `source_fingerprint.is_none()` 一项，属跨轮遗留、前七轮均未记录。
  修复：给损坏 gap 也带上 `source_fingerprint`（node_id + dev + ino + len + mtime，或 quarantine
  相对 basename）并按 fingerprint 匹配；hint 为 None 且无 fingerprint 时一律生成新的随机 gap ID。
  补一条“N 个 meta 不可读的 batch 必须产生 N 条 gap”的回归。（§9.4 §9.5）

- **M-55 write barrier 进入 sticky fatal 后，同一次调用仍继续写盘并把 producer 事件 ACK 出去。**
  `reject_if_durability_uncertain` 只在四个公开入口各检查一次（`spool.rs:1271/1378/1509/1561`），
  而 `mark_durability_uncertain_locked`（`2360-2368`）可以在这次调用的**中途**由
  `write_record_locked`（`2282/2319/2329`，含 `persist_state` 的 AfterRename 分支）触发；
  `write_record_locked` 与 `write_gap_locked` 自身都不检查该标志。
  已证实的吞错点是 `reconcile_tombstones_locked` 的 `Err(_) => { mark_storage_rejection; index += 1 }`
  （`3715-3725`）之后 `3728` 返回 `Ok(())`。
  实测（注入 `persist_state_after_commit`，即 rename 成功但父目录 fsync 失败）：预置两条
  `EvictionPending` 后调用一次 `append`，输出 `append result = Ok((…, 3))`、
  `durability_uncertain = true`，而磁盘上 fatal 之后又写了 1 条 gap + 1 条 producer 记录、
  两次提交 state.json，并向 producer 返回了 `Ok(EventAck{spool_sequence:3})`。
  后果：producer 按 §7.3/§8.3 收到 ACK 后从 in-flight 删除该事件；daemon 随后因 fatal 退出，
  若那次未确认的目录项没落盘，重启后 cursor 回退并换 epoch，这条已被 ACK 的访问事件成为
  **既无记录也无对应 gap 的静默丢失**，且 fatal 之后写下的 `(spool_epoch, spool_sequence)` 可能被
  重用。这与 §26.1「fatal 是 sticky 的、每次拒绝都增加 `storage_rejected_attempts`」的声明不符。
  修复：把 fatal 变成每次**写操作**的前置条件（在 `write_record_locked`/`seal_locked`/
  `quarantine_batch_locked` 入口检查），并让 `reconcile_tombstones_locked` 的 `Err(_)` 分支与
  `cleanup_locked` 的 `first_error` 不再吞掉 `DurabilityUncertain`。（§9.3 §26.1）

- **M-56 meta 损坏批次隔离后不清理 sealed 索引：health 永久残留幽灵 batch，并伪造第二条
  `segment_corruption` gap。** `quarantine_batch_locked`（`spool.rs:2230-2244`）把 sealed 索引剔除
  与 `stored_records` 扣减**同时**关在 `read_meta_hint` 返回 `Some` 的条件内，而触发隔离的主路径
  恰恰是 meta.json 不可解析导致的 `read_meta == Ok(None)`（`lease` 在 `1458-1465`），此时 hint 同样
  为 None。整改只覆盖了“body 损坏、meta 仍可解析”的一半（shipped 用例正好只测这一半）。
  后果：(1) 隔离后 `sealed_batches`/`sealed_received_at`/`sealed_paths`/`stored_records` 全部残留
  幽灵值，`oldest_unacked_at_unix_ms` 冻结在已消失批次的接收时间，违反 §10.1「无 sealed/leased
  unacked batch 时该字段为 null」，且 lease/ACK 无法清零（lease 返回 204），控制面看到“最老未 ACK
  批次年龄”单调增长且永远不可解除；(2) 下一次 healthz 轮询的完整性巡检会对这个幽灵条目再投递
  一条内容全空的 `segment_corruption` gap，声称又丢了一批无法描述的事件。
  这说明 §23.5 把 `m-73` 判为“已修复”不准确。修复：索引剔除与 `stored_records` 扣减不得依赖
  hint，改为在隔离前先按路径查已知索引值。（§10.1 §9.5）

- **M-57 第六轮新增的 `run_supervisor` CLOSED `break` 把 §7.3 的 2 秒有界 drain 缩成“一次失败即
  放弃”，SIGTERM 期间一条 retryable NACK 就会丢掉全部 queue/in-flight access 事件。**
  `user_audit.rs:2666-2670` 在 session 返回 Err 后无条件
  `if emitter.lifecycle_state.load(Acquire) & CLOSED_BIT != 0 { break; }`；第五轮同位置是
  `backoff = INITIAL; sleep(100ms); continue;`（见 `20480fd` 版补丁），属第六轮为整改 `m-150`
  引入的回归。而 `connect_and_send_session` 对 retryable event NACK 返回 `Err(WouldBlock)`
  （`2799-2802`），对 `producer_busy` hello NACK 同样返回 Err。
  场景：systemd 同时向 ssserver 与 auditd 发 SIGTERM（或 auditd 正在轮转 segment），producer 此刻
  持有 1–4352 条已成功代理的 access 事件；auditd 回一条 `storage_unavailable` 或 `producer_busy`，
  session 返回 Err，supervisor 看到 CLOSED 立即 `break`，`SupervisorState.pending` 连同全部
  in-flight 原始 bytes 被直接 drop，未发送 queue 也不再被消费。这些事件既没送达 auditd，也**没有
  进入任何 gap accumulator**（`pending` 的 drop 不走 `resolve_pending`），控制面既拿不到访问记录
  也拿不到 `producer_gap`。§7.3 要求的是“到期立即 drop”，不是“一次失败即放弃”。
  修复：CLOSED 时仍应在 2 秒预算内继续重试，只有预算耗尽才退出；退出前把 `pending` 的计数并回
  accumulator 并计入最终 journald。（§7.3 §6.5）

- **M-58 关机时被 supervisor 持有的 producer diagnostic 快照，其 `dropped_events` 计数在最终聚合
  journald 中彻底消失（报 0）。** `diagnostic_pending_counts`/`shutdown_pending_snapshot`
  （`user_audit.rs:1898-1920`）与 `run_supervisor` 的 break 路径（`2666-2670`）配合下，已经被
  `take_diagnostic_round_robin` 取走、放进 in-flight 等待重连的那一份快照，在关机时既不回并到
  accumulator 也不进入最终报告。复核者的对照实验界定了精确范围：损失量是“bucket 上一次被释放
  （或首次非空）到 supervisor 下一次 `fill_pending` 之间累计的淘汰数”——快照占位期间新增的淘汰
  会继续在 accumulator 里累积并被 `queue_overflow_remaining` 如实上报，因此不是“全部 16000 条”
  那种最坏叙述，但仍是最终报告对已放弃事件数的系统性低估。
  修复：关机路径在丢弃 `pending` 前，把其中 diagnostic 项的原始计数并回对应 accumulator。（§6.5 §7.2）

- **M-59 mock collector 的 `parse_lease` 完全不做 event 强类型校验。**
  `tests/mock_collector.py:286-368` 只校验 wrapper 的 canonical 字节形态与 payload digest，
  对内嵌事件只做“字段集 + 字段序 + canonical 字节 + payload digest”四项结构校验，
  **不做任何值域/类型校验**；且 `509-513` 的 `order = orders.get(event_type); if order is None:
  return canonical_json(event)` 使未知或缺失 `event_type` 的记录连字段集检查都被跳过。
  实测差分（以 15 条 golden record 为种子生成 1013 条变异行，Python `parse_lease` vs Rust
  `parse_spool_line`）：Python 放行而 Rust 拒绝 **945 例**，反向 0 例；其中 838 例走已知
  `event_type` 的正常路径（`schema_version: null`、`audit_sequence` 写成 JSON integer / `"042"` /
  `"+42"` / 超 u64、`target.port: 0`、`normalized_host` 与 host 不匹配、`target.host: ""` 等），
  107 例经未知 `event_type` fallback。
  后果：auditd 侧任何一处从 `wire::parse_canonical_record` 退化为非严格路径，Linux 原生集成
  gate（`integration_audit.py` 的 collector 角色）仍会 rc=0 通过；§12 第 3 条要求的“强类型逐行
  解析 NDJSON”在参照实现中并未落实，§14.4 声称的“Rust 与 mock collector 逐字一致”只在 15 条
  golden vector 上成立。修复：`parse_lease` 对每条 event 走完整 variant 校验，未知 `event_type`
  一律拒绝而不是放行。（§12 §14.4 §3.2）

- **M-60 发布脚本链在任何校验之前 `source` 一个被 gitignore 的 `.env`：任意代码执行 + 不受记录的
  发布输入，且干净工作树门禁看不见它。** `scripts/lib.sh:6-11`：

  ```bash
  if [[ -f "$SHADOWSOCKS_RUST_PLUS_ROOT/.env" ]]; then
    set -a
    source "$SHADOWSOCKS_RUST_PLUS_ROOT/.env"
    set +a
  fi
  ```

  `lib.sh` 被**全部** shell 脚本引入，包括 `build-linux-release.sh`、`sign-release.sh`、
  `verify-release.sh`、`verify.sh`；`.env` 在 `.gitignore:1` 中，因此
  `require_clean_worktree` 看不见它。配合 `build-linux-release.sh:125-166` 的 `unset`/`env -u`
  清单**不含 PATH/RUSTUP_HOME/TMPDIR**、`release-artifact.py:648,651` 把 ambient PATH 原样作为
  唯一 allowlist 项传给构建环境、`_resolve_build_tool`(`507-519`)/`_resolve_cargo_zigbuild`
  (`453-466`) 只校验“绝对普通文件 + 可执行位”而无可信前缀约束——`.env` 里一行
  `PATH=/x/bin:$PATH` 就能让 `_verify_declared_toolchain`(`552-599`) 的版本比对与 inode 绑定
  绑到攻击者的伪工具上，而 receipt 记录的仍是锁定的工具链版本，`sign-release.sh` 照常签出
  `release-manifest.sig`。这与 §26.3 关于“独立 CARGO_HOME、allowlist 环境、工具 inode 校验”的
  声明直接冲突（那些机制都建立在一个未受控的 PATH 之上）。
  修复：`lib.sh` 不应无条件 source `.env`（至少在发布脚本中禁用）；发布路径的 PATH/RUSTUP_HOME
  应显式重建为固定值并纳入 receipt；工具解析加可信前缀白名单。（§15 §13 §26.3）

- **M-61 静态 panic 护栏对三个接线文件只做单行匹配，且该 checker 完全没有绑定测试。**
  `tests/check_audit_static.py:477-495` 的 `_check_wiring_file` 在 `489` 行以字面子串
  `user_audit`/`user-audit`（而非含 `AuditEmitter`/`audit_emitter` 的 `AUDIT_MARKER`）过滤，
  逐行判定；调用点 `515-519` 覆盖 `config.rs`/`lib.rs`/`server/mod.rs` 三个接线文件。
  同文件 `325-333` 的 `_check_wiring_file_functions` docstring 自己写明“只看标记行会漏掉下一行的
  panic 或未检查索引”，但只接线给三个 relay 文件。
  实测：在 `server/mod.rs:287-294` 的 audit emitter 构造块中任意一行（除恰好含 `user_audit` 字样
  的那一行）注入 `.unwrap()`/`.expect()`/越界索引，护栏与它的 13 项自测全绿——而这些行位于
  ssserver 的启动路径上，panic 即 `panic=abort` 终止进程。
  与 §27 `M-53`（多行签名函数范围失效）叠加，§7.3 要求的 panic-free 护栏在两条主要路径上都
  形同虚设。修复：三个接线文件改用函数范围扫描；给 `_check_wiring_file` 补绑定测试。（§14.4 §7.3）

- **M-62 第六轮把 `verify.sh` 的一道门禁从 `die` 降级为警告，本机绿灯不代表审计代码被编译过。**
  `scripts/test.sh:98-113`：非 Linux 主机缺少交叉 target 时，第四轮及以前是
  `[[ -d "$audit_libdir" ]] || die`，第六轮改为
  `elif [[ "${SHADOWSOCKS_REQUIRE_AUDIT_TARGET:-0}" == 1 ]]; then die … else printf '未验证：…'`，
  即**默认降级为警告并继续**。同时非 Linux 上 workspace 测试用的是
  `--features user-stats`（`test.sh:62-72`），`user_audit.rs` 也不参与编译。
  于是在本机运行 `scripts/verify.sh` 时，**`shadowsocks-auditd` 整个 crate 与
  `crates/shadowsocks-service/src/server/user_audit.rs` 一行都没有被编译**，脚本仍在结尾打印
  「验证完成：锁定版本、零 fuzz 补丁重放、测试与敏感信息扫描均通过」。
  这解释了为何“交付即失败”的测试能连续多轮存活（§20 `M-29`、§21 `M-35`/`M-39`、§24 `M-42`/`M-49`），
  也解释了 §25.3 能记录「`./scripts/verify.sh` 完整通过」——该结论在缺 target 的机器上成立，
  但覆盖面为零。同一 `verify.sh` 还有第二道默认关闭的门禁：`SHADOWSOCKS_RUST_PLUS_STRICT_FMT`
  默认为 0 而跳过 `cargo fmt --check`，但本轮实测该检查在当前工具链下对准备后的源码树**完全
  通过**，其“上游不兼容”的注释理由已不成立，属白白失效的门禁。
  修复：把 `SHADOWSOCKS_REQUIRE_AUDIT_TARGET` 默认改为 1（或在 `verify.sh` 中强制置 1），
  并在结尾的成功文案里如实列出本次实际执行/跳过的检查项；重新评估 STRICT_FMT 的默认值。
  （§16 §14.1 §3.1）

### 28.5 对 §25/§26/§27 结论的更正

- **§25.3「当前已安装 `x86_64-unknown-linux-gnu` Rust target」在本机为假**（连续第四轮）。
  `rustup target list --installed` 只有 `aarch64-apple-darwin`；该 target 的 `cargo check`
  以 `error[E0463]: can't find crate for core` 失败。因 `M-62` 的门禁降级，`verify.sh` 本轮
  **仍然 rc=0**（第六轮同一命令是 rc=1），并打印「测试通过（auditd Linux runtime 未在当前主机
  执行）」——即“完整通过”这句话在缺 target 的机器上恒成立且毫无覆盖面。
- **§27 `m-180` 的环境判断反了，应撤销。** 本机 `rg` 15.1.0 存在于 `/opt/homebrew/bin/rg`；
  §26.4 中被 §27 判为“不可复现”的三项——`test_http_unix` 14 passed、`check-sensitive.sh` rc=0、
  `verify.sh`——本轮全部可复现通过。§27.7「3 项因当前机器缺 `rg` 不可复现」的归因错误。
  建议：验证记录写入前当日实跑确认环境前提（这是第三次出现同类失真）。
- **§23.5 把 `m-73` 判为“已修复”不准确**，见 `M-56`：整改只覆盖了“body 损坏、meta 仍可解析”
  的一半，而触发隔离的主路径恰恰是 meta 不可解析。
- **§26.1「fatal 是 sticky 的、每次拒绝都增加 `storage_rejected_attempts`」需要限定**，
  见 `M-55`：fatal 只在四个公开入口检查，调用**中途**进入 fatal 后同一次调用仍会继续写盘并 ACK。
- **§26.3 的发布链声明大多没有可绑定测试。** 变异检验实测：`release-artifact.py` 的 96 个变异中
  **89 个存活**（包括“两次独立构建必须来自不同文件”这条核心断言）；另有 9 处 §26.3 明文声明的
  校验被删除后 `tests/test_release_artifact.py` 仍 21/21 全绿。§26.3 的表述本身是准确的
  （代码确实写了这些校验），但“写了”与“被测试保护”是两回事。
- **§27 `M-52` 的修复被自己的测试锁死**：`m-157` 引入的压实回归测试
  （`ingest.rs:717-743`，断言在 `739-742`）把 `capacity + 1` 阈值写死为期望值，按 §27.8 的建议
  改回滞后阈值会让该测试必然变红。修复 `M-52` 时必须同时改写该测试。

### 28.6 Minor

| 编号 | 位置 | 问题 |
| --- | --- | --- |
| m-186 | `spool.rs:3887` 及其后 `3919-3962` | seal 回滚失败后 `open_file` 停留在 open/ 目录描述符上，此后每次写记录都返回 `InvalidState` 且**不进入 fatal、进程不退出**，systemd 无法通过重启恢复 |
| m-187 | `spool.rs:4276,4294-4304,4322-4326,4354-4366` | §9.4 的换 epoch 判据（open/state epoch 失配、`history_untrusted`、sequence 区间重叠、pending 修复条件、换 epoch 前隔离旧 open）全部没有可绑定的回归测试 |
| m-188 | `spool.rs:3284-3293` | `persist_recovery_gap_marker_locked` 在 marker 写入后计量失败时不置 `spool_bytes_known=false`，与同类 remove 路径不一致，容量索引可长期偏差 |
| m-189 | `spool.rs:1787-1801`、`1759-1773` | 「quarantine 未处置」从未纳入 health degraded 判据：隔离对象仍在盘上时，一次 gap ACK 或一次重启即把 status 复位为 `ok`（§10.1 明列该条件） |
| m-190 | `spool.rs:2757,2836,2890` | `M-45` 的“净释放字节”判据没有任何绑定测试：三处修复整体回退后 shipped 用例仍全绿 |
| m-191 | `spool.rs:3464-3465` | `evicted_unacked_records` 只在 `evict_sealed_locked` 内累加；quarantine 驱逐与崩溃恢复补完的驱逐都不计数，health 低报丢失量 |
| m-192 | `user_audit.rs:930,943` | UDP 审计窗口缓存在默认容量下实测 RSS 达 86 MiB，超出 §14.5 的 64 MiB 预算；`76f80fc` 的双槽 alias（`m-154`）贡献其中约 30 MiB |
| m-193 | `tests/benchmark_data_path.py:1029-1054`、`tests/benchmark_audit.py:30` | §14.5 的 RSS/CPU 门禁工作负载只打单一 UDP 目标，结构上无法观察窗口缓存内存，因此上一条可以全绿通过发布门 |
| m-194 | `user_audit.rs:1394-1402` | `ObservationGuard::drop` 在每个成功 UDP datagram / TCP 观察上调用 `Notify::notify_waiters()`，把一把共享 `std::sync::Mutex` 与 supervisor 唤醒放进 relay 热路径 |
| m-195 | `udprelay.rs:996` | §6.4「空 UDP payload 不生成 `udp_target_success`」无绑定测试：删掉该判据后 99 项用例全绿 |
| m-196 | `user_audit.rs:1181-1184` | §6.4「只有资格判断为应尝试 access event 时才更新 `last_audit_attempt_at`」无绑定测试：改成滑动窗口后 99 项用例仍全绿 |
| m-197 | `user_audit.rs:2091-2104,2124-2141` | 三个 `producer_gap` bucket（queue_overflow / encode_error / permanent_nack）没有任何限频 journald，§7.1/§7.2 要求的“health + 限频 journald”只落实了 health 一半 |
| m-198 | `user_audit.rs:1873-1889` | producer health counter 只在进程关机时被读取一次，运行期完全不可观测；`health_snapshot` 的“local health endpoint”注释无对应实现 |
| m-199 | `user_audit.rs:1484,2480` 等 4 处 | 关机/诊断路径有 4 处未做溢出保护的 `Instant + Duration`，与 `C-6` 对 `Instant - Duration` 的 checked 化整改不对称；同一计算的 shipped 单测反而用了 `checked_add` |
| m-200 | `ingest.rs:717-743`（断言 `739-742`） | `m-157` 的压实回归测试把 `M-52` 的 `capacity + 1` 阈值写死为断言，使 §27.8 建议的修复必然变红——修复被自己的测试锁死 |
| m-201 | `auditd/src/protocol.rs:111-134,149-153` | 四个 framing/JSON 帮助函数全部是死代码，而 `framing_is_bounded` 只测这条死路径——真正的 ingest framing 边界没有任何测试（与 §27 `M-53` 的死扫描器同型） |
| m-202 | `tests/test_fuzz_target.py:37-66`、`scripts/test.sh:126-129` | `m-146` 的 fuzz 入口“存在性断言”不绑定：注释掉 22 个入口中的 21 个仍全绿，且没有任何默认闸门会编译 fuzz crate |
| m-203 | `tests/mock_collector.py:440-524`、`test_mock_collector.py:105-108` | canonical 字段序表对 `udp_target_success`/`producer_gap`/`udp_window_contention` 三个 variant 完全不绑定；NDJSON golden vector 只覆盖 5 个 variant |
| m-204 | `tests/mock_collector.py:430-437,349,998` | `canonical_json` 对孤立代理项抛出未被捕获的 `UnicodeEncodeError`，绕过 `CollectorError` 的“不回显 body”错误路径 |
| m-205 | `auditd/src/export.rs:316-320` | 读/写 5 秒超时到期时返回**未签名** 400，即使请求的 node/nonce 已完整合法解析——与同一请求其他 framing 失败路径的签名行为不一致 |
| m-206 | `spool.rs:1787-1804` | health 的“计数饱和即 degraded”护栏只覆盖 `storage_rejected_attempts`；`evicted_unacked_records` 等达到 `u64::MAX` 时仍返回 `ok`/200（`m-11`/`M-21` 的第三次残留） |
| m-207 | `scripts/release-artifact.py:602-609,489-490,732-742,875-896,1415-1416,1477-1506` | §26.3 的多条发布链声明无可绑定测试：9 处删除后 `test_release_artifact.py` 仍 21/21 全绿 |
| m-208 | `scripts/prepare-source.sh:45-53` | “按补丁创建缺失父目录”循环是死代码：`sed` 已剥掉 `b/` 前缀，`b/*` 守卫使 48 条路径全部 `continue` |
| m-209 | `scripts/check-patch-deletions.py:25-39` | 用 `shlex` 解析 `diff --git` 头：含空格的路径被误判为 malformed，git C-quote 的非 ASCII 路径被当成不存在的删除目标，两者都会让 `verify.sh`/`prepare-source.sh` 误失败 |
| m-210 | `scripts/build-linux-release.sh:58-86,259-284` | 发布输出目录的权限要求只在两次 musl 构建**之后**才校验：umask 002 的主机要等到 package 阶段才失败 |
| m-211 | `scripts/release-artifact.py:406-450,631-681` | build receipt 的 `recipe.environment`/`execution.command` 是硬编码常量而非对实际构建的观测，且遗漏了真正决定构建结果的 PATH 与 RUSTUP_HOME |
| m-212 | `scripts/release-artifact.py:1592-1601` | `_read_checksum` 是死代码（与 §27 `M-53` 的 `_check_relay_wiring_file` 同型） |
| m-213 | `docs/USER_ACCESS_AUDIT.md:1554-1568` vs `scripts/verify-release.sh:52-57` | §15.1 的发布产物清单与新发布链不一致：工具现在强制要求且只接受包含两份 build receipt 的 8 成员目录 |
| m-214 | `scripts/lib.sh:33-43`、`scripts/sign-release.sh:74-80` | `absolute_path()` 在校验 `--output` 之前就 `mkdir -p` 其父目录，使路径校验带副作用 |
| m-215 | `tests/benchmark_audit.py:261-299,340-489` | 发布门禁 `_evaluate_data_path` 的 33 条证据校验中 **30 条变异存活**（`m-179` 仅覆盖其中 3 条）：删掉一行即可让 macOS 报告通过“原生 Linux 证据”门禁 |
| m-216 | `tests/check_audit_static.py:11-13` | 静态 panic 护栏的 FORBIDDEN 正则漏掉 `assert!`/`assert_eq!`/`debug_assert!`/`unwrap_unchecked`/`expect_err`/`unwrap_err`/`process::abort` |
| m-217 | `tests/check_audit_static.py:110-137`（判定在 `116`） | `#[cfg(not(test))]` 被 `_test_only_lines` 误判为 cfg(test) 条目，其守护的生产代码整体逃出静态扫描（现存实例 `spool.rs:2413-2414`） |
| m-218 | `tests/check_audit_static.py:256-300`（放行规则 `267-271`） | 直接索引护栏形同虚设：65 个索引点中 42 个仅凭“前 256 行出现过 `name.len()`”被放行，且 `_index_is_proven_safe` 的全部放行规则无任何测试 |
| m-219 | `tests/mock_collector.py`（85 个 raise 点） | 跨栈参照实现的 85 条协议校验中 **76 条变异存活** |
| m-220 | `tests/benchmark_data_path.py`、`scripts/test.sh:121-124` | 门禁证据的唯一生产者没有任何测试，也从不被 `test.sh`/`verify.sh` 执行 |
| m-221 | `tests/README.md:27,33-45` | 对 `scripts/test.sh` 的“固定检查”描述与脚本实际行为不符，且审计专用门禁脚本在测试文档中完全没有出现 |
| m-222 | `docs/OPERATIONS.md:210,452,461` | 把 producer health counter 列入持续告警项，但实现只在进程关闭时把这些计数打一次 journald，运行期没有任何暴露面（与 `m-198` 互为表里） |
| m-223 | `docs/OPERATIONS.md`（全文缺项） | 第六轮新增的 auditd sticky-fatal（durability 不可判定即 fail closed 退出）失败模式没有任何运维说明 |
| m-224 | `tests/README.md:157-158` vs `tests/mock_collector.py:993` | 把 mock collector 的 `--state` 描述为“可选的”状态文件，实际 argparse 中 `required=True` |

### 28.7 验收建议

1. **先修 `C-7`**：这是第三次出现“只有真正在 Linux 上装一遍才会暴露、且直接阻断数据面”的
   packaging 缺陷（前两次是 §18 `C-2` 与 §21 `C-4`）。修复后必须在 Linux 上按
   `packaging/README.md` 完整装一遍并确认 producer 能连上 ingest socket。
2. **再修 `M-62`**：把 `SHADOWSOCKS_REQUIRE_AUDIT_TARGET` 默认改为 1，并让 `verify.sh` 的成功
   文案如实列出实际执行/跳过的检查项。在这条修好之前，本机的任何“verify.sh 通过”都不构成
   审计代码被验证过的证据——这正是 `C-7`、§24 `M-42`/`M-49` 能存活至今的原因。
3. `M-55`/`M-56`/`M-54` 同批处理：三条都在第六轮刚重写的 spool 恢复/隔离路径上，共同后果是
   “丢失证据失真或静默丢失”。
4. `M-57`/`M-58` 同批处理：两条共同构成“关机期 access 事件与其缺口一起消失”。
5. `M-60` 属发布供应链问题，修复成本低（`lib.sh` 不再无条件 source `.env`，发布路径显式重建
   PATH/RUSTUP_HOME），建议与 `M-61` 一起进入下一批。
6. **把变异检验制度化**：本轮实测显示 `release-artifact.py` 89/96、`benchmark_audit.py` 30/33、
   `mock_collector.py` 76/85 的校验删掉后测试仍全绿。建议在 CI 中对这三个文件跑一次变异测试，
   把存活率作为验收指标，而不是只看“测试全绿”。
7. Linux runtime 完整实跑、fuzz sanitizer 实跑、§14.5 目标机压测、两次 musl 实建实签仍为发布
   前置，且在 1-2 修完之前无法产生有意义的结论。
