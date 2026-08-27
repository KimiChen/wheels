# shadowsocks-rust-plus 用户成功访问审计实现规格

> 文档状态：实现就绪（implementation-ready）
>
> 目标读者：独立负责 `shadowsocks-rust-plus` 节点侧审计功能的开发同事
>
> 规范版本：2
>
> 最后决策日期：2026-08-27
>
> 版本 2 变更：落实同日评审修正——UDP 去重窗口固定 60 秒、acked 保留固定 86400 秒、auditd 配置
> 改为逐字段校验表、producer ACK 超时与 auditd 写截止对齐、UDP shard index 与固定常量清单明确、
> runtime 起点外部依赖声明及术语统一。

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
user-audit = ["user-stats", "shadowsocks-service/user-audit"]

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
  "group_commit_max_events": 256,
  "group_commit_max_delay_ms": 100,
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
| `group_commit_max_events` | 256 | 1–256 | 256 |
| `group_commit_max_delay_ms` | 100 | 1–100 | 100 |
| `export_max_response_bytes` | 8388608 | 4194304–8388608 | 8388608 |

另需满足：

- `max_spool_bytes` 必须大于 `2 × segment_max_bytes`；
- `min_free_bytes` 必须至少 256 MiB 且小于所在文件系统总容量；
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
- ASCII domain：转换为小写并移除一个末尾点；
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
- 首次重连等待 100 ms，指数退避到 5 秒并加入 0–20% jitter；成功收到合法 ACK 后重置；
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
- group writer 最多聚合配置的 `group_commit_max_events` 条或 `group_commit_max_delay_ms` 毫秒
  （默认 256 条 / 100 ms），以先到者为准；依次完成 record `write_all`、open file `fdatasync`、把
  `next_spool_sequence` 原子持久化到 `state.json` 后才 ACK；任一步失败均不 ACK；
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
- group sync 只在 durability barrier 后 ACK；
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
