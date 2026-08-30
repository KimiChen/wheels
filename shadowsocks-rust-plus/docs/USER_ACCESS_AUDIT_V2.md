# shadowsocks-rust-plus 用户成功访问审计 · 待办与未决事项（V2）

> 文档定位：**当前活动的待办清单**。记录截至 2026-08-30 仍未解决、未执行或需要决策的事项。
>
> 与 [`USER_ACCESS_AUDIT.md`](USER_ACCESS_AUDIT.md) 的关系：
>
> - 那份是**历史文档**，保存规范合同（v7，第 1–16 节）与第 1–8 轮审计、整改及 Linux 实装验收的
>   完整过程记录（第 17–30 节）。合同条文仍以那份为准，本文不复制、不改写合同。
> - 本文只列**尚未闭合**的条目。已闭合项不再重复，只在需要时引用其出处小节。
>
> 基线：overlay `main`，规范版本 v7，`patches/0003-user-audit.patch` 与
> `upstream.lock` 的 `prepared_tree_sha256` 一致（`7f57d709…`）。

## 1. 当前真机验收状态（背景）

2026-08-29/30 在 Debian 13 (trixie) 节点按 `packaging/README.md` 完整实装一次，README 五步全部
通过，详见历史文档第 30 节。**已在真机证实**：两次独立 musl 构建产物字节一致、签名与独立验签
通过、§11 权限模型逐项一致、`C-2`/`C-4`/`C-7` 三条 packaging critical 修复成立、
`cargo test -p shadowsocks-auditd` 99 passed/0 failed（root 与非 root 各一次）、
`tests/integration_audit.py` 端到端通过。

**这不等于第 16 节验收通过**，原因见下文第 2、3 节。

## 2. 待解决的代码问题

### m-142（minor）`Instant` 算术下溢模式未纳入静态护栏

- **位置**：`tests/check_audit_static.py`（全文无 `Instant`/`Duration` 相关规则）。
- **背景**：`C-6` 是 `Instant::now() - Duration` 在开机 60 秒内下溢 panic，`panic=abort` 下击落
  整个进程。该缺陷由第五轮发现、第六轮修复，但现有静态护栏抓不到同类模式。
- **现状**：本轮 `m-170` 修复补上了 checked 助手与 C-6 的绑定单测，**但未实施 m-142 建议的静态
  规则**，因此同类回归仍无护栏。
- 出处：历史文档 §23.6 `m-142`、§27.5 `m-170`、§27.4「遗留未修」。

### 本轮审阅新增问题（P2，`20ac4784e4735ab115469c936082e04717218e02^..47a65a4e175879e0a758644f185f4d93ca6eac1d`）

> 2026-08-30 对该范围共 89 个 commit（含起始 commit，截至当前 `HEAD`）进行复核。以下条目接续
> 历史文档 §30 已记录的 `M-67`，编号为 `M-68`–`M-73`，当前均未修复；`P2` 表示应在发布前处理的代码/交付门禁
> 缺陷。行号以当前工作树中的交付文件为准，补丁内的行号指 `patches/0003-user-audit.patch` 的
> post-image 行。

#### M-68（P2）ACKed 批次进入 `quarantine/` 后被计入未确认丢失

- **位置**：`patches/0003-user-audit.patch:10348-10360,11745-11747,2195-2207`。
- **现象/影响**：`quarantine_batch_locked` 将来源目录同时判定为 `sealed/` 或 `acked/`，并把
  `QuarantinePending.event_count` 保存为该批次的总事件数。后续 `evict_quarantine_locked` 不区分
  来源，始终以 `spec.lost_events` 调用 `add_evicted_unacked_records`。因此，一个已经收到 ACK、因
  损坏从 `acked/` 移入 `quarantine/` 的批次在随后被清理时，会把已经交付的事件重复算成
  `evicted_unacked_records`，健康状态和丢失告警均被高报。
- **触发/复现**：构造一个 meta 可读但 body 损坏的 ACKed 批次，令完整性检查将其隔离，再触发
  `quarantine` 驱逐；gap 的 `lost_events` 为该批次总数，而实际未 ACK 数应为 0。
- **修复建议**：在 pending 记录中保留来源的 ACK 状态或直接保存未 ACK 计数，所有驱逐路径只按该
  计数更新 health；补充 ACKed quarantine → eviction 的回归测试，并断言 gap 与 health 计数语义不混用。
- **出处/引入**：`fc5bfb7b5ebfe0e18c97bfc4cb98d7f84a528c44`（修复 m-191）。

#### M-69（P2）`QuarantinePending` 崩溃恢复完成删除和 gap 后漏计未确认丢失

- **位置**：`patches/0003-user-audit.patch:11825-11938`。
- **现象/影响**：重启恢复 `TombstoneEntry::QuarantinePending` 时，代码会重试 rename/删除、写入
  固定 gap，并移除 pending marker/tombstone，但该分支没有调用 `add_evicted_unacked_records`。
  对 `reason=quarantine_eviction` 且 `event_count=N` 的事务，崩溃前后实际丢失了 N 条未 ACK 事件，
  但恢复后的 `evicted_unacked_records` 少 N，health 低报丢失量（而普通进程内路径
  `evict_quarantine_locked` 会计数）。
- **触发/复现**：在 quarantine eviction 的删除屏障之后、gap 持久化/事务收尾之前终止 auditd，
  再启动触发 `reconcile_tombstones_locked`；检查 gap 的 `lost_events` 与 health counter 不一致。
- **修复建议**：把“完成 quarantine eviction”设计成带 gap ID 的幂等会计事务，在成功完成 durable
  状态转换时补记一次未 ACK 数；区分 `segment_corruption`（可能来自 ACKed 批次）与
  `quarantine_eviction`，并增加崩溃恢复回归测试。
- **出处/引入**：`fc5bfb7b5ebfe0e18c97bfc4cb98d7f84a528c44`（修复 m-191）。

#### M-70（P2）`EvictionPending` 恢复计数在 tombstone 持久化失败后重复累加

- **位置**：`patches/0003-user-audit.patch:11813-11822`，关联
  `11435-11457` 的 `replace_tombstone_locked`。
- **现象/影响**：`reconcile_tombstones_locked` 在把 `EvictionPending` 替换为
  `EvictedReceipt` 之前就调用 `add_evicted_unacked_records`。若随后
  `replace_tombstone_locked` 写 `tombstones.json` 失败，该函数会回滚内存 tombstone 并返回错误；
  下次 reconcile 再走同一分支，又会先累加一次。单个被驱逐批次因此可在可恢复的存储故障期间按
  重试次数放大 `evicted_unacked_records`，健康数据失真。
- **触发/复现**：注入一次 `persist_tombstones_locked` 失败，使 pending entry 保留，然后恢复存储
  并再次执行 reconcile；gap 只应存在一条，但 counter 增加两次。
- **修复建议**：将计数更新放在成功的 durable tombstone 状态转换之后，或为每个 gap ID 持久化
  “计数已应用”标记并以其做幂等保护；用失败后重试测试绑定该顺序。
- **出处/引入**：`fc5bfb7b5ebfe0e18c97bfc4cb98d7f84a528c44`（修复 m-191）。

#### M-71（P2）相对 `PATH` 仍会折叠 `rustup` 代理，发布构建失败

- **位置**：`scripts/release-artifact.py:563-575,636-644`。
- **现象/影响**：`d0c29ff` 新增 `_absolute_tool_path` 以保留 `cargo`/`rustc` 的 rustup 代理文件名，
  但只处理绝对候选路径；当 `PATH` 含相对目录时，`shutil.which` 可返回相对的
  `bin/cargo`，随后 `candidate.resolve(strict=True)` 又把符号链接折叠为 `rustup`。执行版本检查时
  实际运行的是 `rustup -V` 而不是 `cargo -V`，合法的固定 rustup 工具链会被误判为版本不一致，
  README 的发布构建步骤因此不可完成。
- **触发/复现**：准备 `bin/cargo -> rustup` 的 rustup 风格目录，设置 `PATH=bin`（相对当前工作目录），
  调用 `_resolve_build_tool("cargo", environment)`；返回路径的 basename 变为 `rustup`。
- **修复建议**：无论候选路径是否绝对，都只解析其父目录并保留原 basename（同时保留现有
  `stat`、普通文件、执行位和 inode 校验）；增加相对 `PATH` 的代理符号链接回归测试。
- **出处/引入**：`d0c29ff49b4ff4a8303606fe3285c23b522cdf45`（修复 M-63 的不完整分支）。

#### M-72（P2）`SHADOWSOCKS_REQUIRE_AUDIT_TARGET` 非法值绕过 fail-closed 门禁

- **位置**：`scripts/test.sh:113-119`、`scripts/verify.sh:90-93`。
- **现象/影响**：缺少 auditd 交叉检查 target 时，脚本只判断变量是否精确等于字符串 `1`；除合法
  `0` 外的其它非空值（如 `yes`、`2`、`false`）都会进入降级分支，等同于用户明确设置 `=0`，并可能让
  `verify.sh` 打印“验证完成（覆盖面不完整）”。这使拼写错误、CI 统一注入的布尔值或恶意环境值
  将应失败的 auditd 编译覆盖面静默变成成功，违背 `b918b8f` 设定的 fail-closed 合同。
- **触发/复现**：在未安装 `x86_64-unknown-linux-gnu` target 的非 Linux 主机执行
  `SHADOWSOCKS_REQUIRE_AUDIT_TARGET=yes scripts/verify.sh`（同样适用于 `2`/`false`）；返回码可为
  0，且 auditd crate 实际未被编译。
- **修复建议**：集中解析为三态：仅 `0` 允许显式降级、仅 `1`要求失败闭合、其它值立即报错；
  `test.sh` 与 `verify.sh` 共用规范化结果，并为 `0/1/非法值` 分别补 shell 回归测试。
- **出处/引入**：`b918b8f34dd50351e6e1310b2698380de908327c`（修复 M-62 的门禁解析）。

#### M-73（P2）AF_UNIX lingering close 的有界 drain 仍可能在错误响应后复位连接

- **位置**：`patches/0003-user-audit.patch:24933-24937,24980-25003`，对应原始 exporter
  `patches/0001-eih-user-stats-http-unix-exporter.patch:4623-4627` 的错误响应路径。
- **现象/影响**：Linux 在关闭接收队列非空的 AF_UNIX stream 时可能向对端报告 `ECONNRESET`。
  `69069b3` 将直接错误响应改为 lingering close，但 drain 上限仍只有 65,536 字节或 100 ms。
  `PAYLOAD_TOO_LARGE` 可能在更大的请求尚未读完时返回，`TOO_MANY_REQUESTS` 更可能完全未读入站
  数据；对超大、持续发送或慢速客户端，预算耗尽时队列仍非空，客户端会在已收到完整 413/429 后
  读到 `ECONNRESET`。这使错误响应的 HTTP 语义在 Linux 上仍不稳定。
- **触发/复现**：向 busy/超限路径持续发送超过 65,536 字节的请求，或以低于 drain 预算的速率发送，
  读取完整响应后继续读 socket；可观察响应已写出但读取以 `ECONNRESET` 结束。
- **修复建议**：定义可证明的 graceful-close 策略（例如在拒绝前消耗完整请求、把排空与 permit
  生命周期解耦并设置明确的连接级上限），而不是仅提高常量；Linux 上补充超大和慢速发送者的
  回归/压力测试，确保客户端能稳定看到响应后的 EOF 或合同规定的错误。
- **出处/引入**：`69069b3f57846aab6027d570454166f8eb39a1c4`（修复 M-66 的不完整 lingering close）。

## 3. 待执行的验证

以下为发布前置。第一项已经按第 5 节决策收窄，原始宽命令的失败记录见表下；其余三项**从未在任何机器上执行过**：

| 项目 | 说明 | 阻塞因素 |
| --- | --- | --- |
| §16 收窄 Rust 门禁（`--workspace --lib --bins`，并单跑 `tcp_eih_user`） | §16 验收项 | 已决策并实施（规范 v6 → v7）；原始宽命令的 9 条上游联网用例不属于该门禁 |
| `cargo-fuzz` sanitizer 实跑 | §3.2/§14.4 要求交付并运行 fuzz target | 无，尚未安排 |
| §14.5 目标机压测 | 吞吐 ≤5%、CPU ≤10%、ssserver RSS ≤64 MiB、auditd RSS ≤128 MiB，及离线/队列满/慢 ACK/spool 满四类专项 | 需目标机与真实数据面负载 |
| 真实流量端到端审计事件 | 经 ssserver 转发真实 TCP/UDP 流量后，验证 access event 落入 spool 并可经 lease 导出 | `integration_audit.py` 覆盖的是 ingest/export 协议链路，**不含**真实代理流量 |

### 3.1 首次执行结果（2026-08-30，Debian 13 / rustc 1.97.0）

`M-66` 修复后在 `10.0.1.3` 上以 `--no-fail-fast` 完整跑了一次。**overlay 自有的每一个目标全绿**：
`shadowsocks-audit-protocol` 25、`shadowsocks-auditd` 99、`shadowsocks-service` lib 121、
`shadowsocks` lib 9、`0001` 新增的 `tcp_eih_user.rs` 4、根 crate lib 10，以及 `dns`/`http`/`udp`
三组集成用例。

失败 9 条，**全部落在上游 v1.24.0 自带、三个补丁都未改动的联网集成用例**上：
`crates/shadowsocks/tests/tcp.rs` 5 条、`tcp_tfo.rs` 1 条、根 crate 的
`tests/{socks4,socks5,tunnel}.rs` 各 1 条。它们经真实隧道向 `www.example.com` 发
`GET / HTTP/1.0` 并断言应答行是 `HTTP/1.0 200 OK`，而真实服务器一律回 `HTTP/1.1 200 OK`。
`tests/socks5.rs` 一个文件里两条用例正好互证：期望 `HTTP/1.1` 的那条通过，期望 `HTTP/1.0`
的那条失败——既不是网络问题，也与本功能无关，是上游用例自身过时。

由此得到一个此前没暴露过的结论：**只要 overlay 继续原样携带上游 v1.24.0，旧的
「`cargo test --workspace --features user-audit` 全绿」按字面在任何主机上都不可能成立。**
现已按第 5 节第 4 项选择收窄命令；当前合同命令和排除边界以历史文档 v7 §16 为准。

第四项值得单独强调：目前**没有任何一次验证**证明过「真实用户流量 → 产生 access event →
写入 spool → 被 collector 取走」这条完整链路。§6 的两类成功事件语义在真机上仍未被端到端验证。

## 4. 待补的文档（Linux 实装中发现，均非代码缺陷）

| # | 事项 | 建议落点 |
| --- | --- | --- |
| D-1 | `scripts/prepare-source.sh` **每次调用都从 GitHub 拉取上游 tag**；节点失去外网时挂满 300 秒后失败，并连带使 `verify.sh`、`build-linux-release.sh` 全部不可用。`UPSTREAM_REPOSITORY`/第二参数可指向本地镜像且实测可用，但未见于任何文档 | `packaging/README.md`、`docs/OPERATIONS.md` |
| D-2 | 若按 D-1 准备本地镜像：`rsync -a` 以 root 接收会保留发送方 uid，git 因 `safe.directory`（dubious ownership）拒绝，需 `chown -R root:root` | 同上 |
| D-3 | §15.1 要求两次独立 musl 构建，但**未声明主机资源要求**；`build-linux-release.sh` 只对工具**版本**做硬门禁，对内存不做检查。实测 3.8 GiB 无 swap 的节点在 `lto = "fat"` + `codegen-units = 1` 下耗尽内存并使主机失去响应 | §15.1 或 `packaging/README.md` |
| D-4 | 同样未声明**连通性要求**：两次构建各用全新空 `CARGO_HOME`，整个依赖图每次发布完整下载两遍且不可复用；净化环境不放行 `CARGO_HTTP_*`，运维无法调整 cargo「30 秒内不足 10 字节即失败」的停滞阈值；无 vendor/离线 registry 方案。网络受限恰是签名主机的合理姿态 | 同上 |
| D-6 | `_sign_snapshot` 使用 `openssl dgst -sha256 -sign`，支持 RSA/ECDSA 但**不支持 Ed25519**（需 `pkeyutl` 且不预哈希）。§15.1 与 `packaging/README.md` 未声明可用密钥类型，选用 Ed25519 只会得到 `manifest 签名失败` | §15.1、`packaging/README.md` |
| D-7 | `tests/integration_audit.py` 会以三个非特权账号重新执行自身，因此**解释器、脚本与 `config/auditd.example.json` 模板都必须位于这些账号可读可执行的路径**。放在 `/root`（Debian 默认 `0550`）下只会得到难以定位的 `PermissionError` | `tests/README.md` |
| D-8 | `cluster-users.py verify-five` 要求**待校验的配置文件不得有 group/other 权限**（`0600`），而安装后的 `/etc/shadowsocks-rust-plus/server.json` 是 `0640`。两者不矛盾（不同阶段不同文件），但 README 第 4 步未说明 | `packaging/README.md` |

## 5. 决策记录

此前列出的四项均已决策并落实，结论见第 6 节：

1. `M-66` 的归属 → 并入 `0003`（已修复）；
2. `D-5` 是否收窄发布构建的 feature 集 → 收窄（已实施，规范升版到 v6）；
3. 实装节点的处置 → 清理（`10.0.1.3`、`10.0.2.3` 均已回到基线）。
4. **§16 的工作区测试判据如何收口** → 选择收窄命令（已实施，规范升版到 v7）：
   workspace 命令只运行 `--lib --bins` 的 feature-off/feature-on 目标，再单独运行 overlay 自有的
   `tcp_eih_user` 集成目标；其余 workspace integration targets 不纳入这两个 workspace 命令，
   上游公网 targets 保留为基线诊断，不改写、不纳入 §16 全绿判据。

## 6. 变更记录

- 2026-08-30：自历史文档第 17–30 节抽取仍未闭合的事项，建立本文件。
- 2026-08-30：清理两台实装节点。`10.0.1.3` 停用并移除两个 systemd unit、账号/组、
  `/etc/shadowsocks-{audit,rust-plus}`、`/usr/local/bin/{ssserver,shadowsocks-auditd}`、
  `/srv/ss-integ` 与 sysusers/tmpfiles 配置，密钥材料 `shred -u -n 3`，磁盘 11 GB → 4.6 GB；
  `10.0.2.3` 移除工作目录与 `/tmp` 残留，11 GB → 8.5 GB。两台的 `build-essential` 等系统包
  有意保留。第 5 节第 3 项决策就此闭合。
- 2026-08-30：**`M-66` 已修复**（overlay `69069b3`，并入 `0003`）。根因确认为 Linux
  `unix_release_sock()` 在关闭接收队列非空的 AF_UNIX socket 时给对端置 `ECONNRESET`：
  exporter 的直接错误响应都在未读完请求时关闭连接，客户端因而在收到完整 413/429 之后仍
  读到错误；macOS 无此语义。修复是 `write_direct_json` 关闭写半边后有界 drain
  （上界 `USER_STATS_MAX_REQUEST_BYTES` 与 100 毫秒）。`exporter_bounds_busy_response_workers`
  属另一情形——超过 busy 上界的连接本就不读不答直接丢弃，复位是符合契约的结局，该用例改为
  接受 EOF 或 ECONNRESET 并仍断言零响应字节。新增
  `direct_error_response_drains_unread_request_input` 以 `UnixStream::pair()` + FIONREAD
  直接断言队列被读空，不依赖平台 close 语义。Linux（Debian 13 / rustc 1.97.0）变异检验：
  修复在位 5/5 通过，去掉 drain 后 4 项转红。修复解除阻塞后随即在同一节点首次跑完
  `cargo test --workspace --features user-audit --no-fail-fast`，结果见 §3.1。该结论只覆盖
  65,536 字节/100 毫秒有界 drain 场景；超大或慢速请求仍可能在错误响应后触发 RST，见本节 `M-73`。
- 2026-08-30：**`D-5` 已实施**（overlay `8711cbd`，规范升版 v5 → v6）。发布构建改为
  `--no-default-features` 加显式 feature 集，§15.1 增列该集合。x86_64-unknown-linux-musl
  下少编译 29 个 crate（brotli/zstd/flate2、tun/smoltcp/etherparse、nix、qrcode、
  tokio-util、rustls-native-certs 等）；`webpki-roots` 保留，DoT/DoH 根证书来源不变。
  **更正 D-5 原文的一处事实错误**：`reqwest`/`web-sys` 并不在该 target 的依赖图中——
  `local-online-config` 拉入的是 `mime`/`flate2`/`brotli`/`zstd`，收窄真正移除的是上述这批。
- 2026-08-30：**§16 工作区测试判据已收窄**（overlay 本轮修订，规范升版 v6 → v7）。
  `scripts/test.sh` 固定运行 `--workspace --lib --bins --no-fail-fast` 的 feature-off 回归，
  Linux 再运行 feature-on `user-audit` 回归，并显式运行 `tcp_eih_user`；其余 workspace
  integration targets 不再进入这两个 workspace 命令，锁定上游的公网 HTTP/1.0 targets
  不再阻塞本项目 §16 全绿判据。
- 2026-08-30：完成对 `20ac4784e4735ab115469c936082e04717218e02^..47a65a4e175879e0a758644f185f4d93ca6eac1d`
  的 89 个 commit（含起始 commit）审阅，新增并记录 `M-68`–`M-73` 六条 P2；均待修复，未改变规范合同或源码。
