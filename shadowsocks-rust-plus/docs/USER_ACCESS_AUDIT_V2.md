# shadowsocks-rust-plus 用户成功访问审计 · 待办与未决事项（V2）

> 文档定位：**当前活动的待办清单**。记录截至 2026-08-30 仍未解决、未执行或需要决策的事项。
>
> 与 [`USER_ACCESS_AUDIT.md`](USER_ACCESS_AUDIT.md) 的关系：
>
> - 那份是**历史文档**，保存规范合同（v8，第 1–16 节）与第 1–8 轮审计、整改及 Linux 实装验收的
>   完整过程记录（第 17–30 节）。合同条文仍以那份为准，本文不复制、不改写合同。
> - 本文只列**尚未闭合**的条目。已闭合项不再重复，只在需要时引用其出处小节。
>
> 基线：overlay `main`，规范版本 v8，`patches/0003-user-audit.patch` 与
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

### m-219（minor）`verify.sh` 从环境变量重算覆盖面结论，两个方向都会失配

- **位置**：`scripts/verify.sh` 结尾的结论分支。
- **现象**：`test.sh` 自己知道本次到底跑了什么（`auditd_crate_checked`、`auditd_runtime_available`），
  但 `verify.sh` 不读这个结果，而是从 `SHADOWSOCKS_REQUIRE_AUDIT_TARGET` 与 target 是否存在重新推断。
  两个方向都会错：`=0` 且 target 其实存在时误报「覆盖面不完整」（保守方向）；变量未设置、target
  已安装时，`test.sh` 如实打印「auditd Linux runtime 未在当前主机执行」，而 `verify.sh` 打印的
  「验证完成：……均通过」一个字都不提缺失的 runtime 覆盖面（乐观方向）。
- **影响**：只影响措辞，两条分支都退出 0，Linux 全量验收本就是另一道发布前置。
- **修复方向**：让 `test.sh` 把覆盖面结论写成机器可读的产物，`verify.sh` 读取而不是重算。
- 出处：本轮对 `M-72` 的复核（该条已修，见第 6 节）。

### m-220（minor）exporter 的 lingering close 期间仍占着 client permit

- **位置**：`crates/shadowsocks-service/src/server/user_stats.rs` 的 `handle_client` 直接错误响应路径。
- **现象**：`write_direct_json` 的有界 drain 最多再占 100 毫秒，这段时间 `OwnedSemaphorePermit`
  仍被持有。被 413 大量拒绝时，`max_concurrent_clients` 个 permit 会被 drain 占住，正常连接吞吐下降。
- **对比**：busy（429）路径本来就不占 client permit——它用独立的 `busy_response_semaphore`。
- **修复方向**：主路径在 drain 之前释放 permit，把 shutdown+drain 挪进一个独立限量的任务
  （形如 busy 路径）。两条上界一字不改。属可用性改进，不是正确性缺陷。
- 出处：本轮对 `M-73` 的复核（`M-73` 中成立的那半已修，见第 6 节）。

## 3. 待执行的验证

以下为发布前置。第一项已收窄（v6→v7→v8）并**已在 Linux 上全绿执行**（见 §3.2），原始宽命令的失败记录见 §3.1；其余三项**从未在任何机器上执行过**：

| 项目 | 说明 | 阻塞因素 |
| --- | --- | --- |
| §16 收窄 Rust 门禁（v8：两条 workspace 命令 + ①②两类集成目标） | §16 验收项 | **已在 Linux 全绿执行**（见 §3.2）；v7 的过度排除已修正 |
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

### 3.2 收窄门禁的 Linux 首次执行（2026-08-30，Debian 13 / rustc 1.97.0）

v7 收窄后的门禁此前只在设计上成立、从未在 Linux 上跑过。本轮实跑，三条命令全部 `EXIT=0`：

| 命令 | 结果 |
| --- | --- |
| feature-off `--workspace --lib --bins --features user-stats --exclude shadowsocks-auditd` | 全绿（`shadowsocks` 9、`audit-protocol` 25、根 crate 10、`shadowsocks-service` 65） |
| feature-on `--workspace --lib --bins --features user-audit` | 全绿（`auditd` 99、`shadowsocks-service` 121，其余同上） |
| `-p shadowsocks --test tcp_eih_user` | 4 passed |
| v8 新增的三个 loopback 目标（`-p shadowsocks --test udp`、`--test udp`、`--test tunnel udp_tunnel`） | 4 / 1 / 1 passed |
| `-p shadowsocks-auditd`（含本轮三条新用例） | 102 passed |
| `-p shadowsocks-audit-protocol` | 25 passed |

**顺带澄清一处对 §3.1 的误判。** 有人以「macOS 上同一条 feature-off 命令的 `shadowsocks-service`
lib 是 309，不可能少到 121」为由怀疑 §3.1 的计数有误。实测：macOS 那 309 里有 **244 个是
`local::redir::sys::unix::pfvar`**——macOS PF 结构体的 bindgen 布局用例，Linux 上根本不存在。
309 − 244 = 65，与 Linux 的 feature-off 逐一对上；user-audit 再加到 121。**§3.1 的数字是对的**，
两个平台的计数本就不可直接相比。

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
- 2026-08-30：**对同事回写的 `M-68`–`M-73` 逐条独立核实并做对抗性复核**（每条两名互不知情的审阅者，
  第二名被要求尽力驳倒第一名），六条全部处置完毕，均已修复：
  - `M-68`（**上调为 major**）成立，但**触发路径不是同事描述的那条**。acked/ 没有在线 body 巡检，
    两条进程内的 acked→quarantine 路径只在 meta 不可读时触发（此时 `event_count` 为 `None`，加 0）。
    真正的入口是**启动恢复** `recover_layout`：它对 `sealed/` 与 `acked/` 同构处理，隔离判据是
    `inspect_batch_dir` 失败——**meta.json 完好、body 摘要/成帧损坏**正好落在这里，于是隔离对象带着
    完整 `event_count` 进 quarantine/，随后被当成未确认丢失计数。修复是把来源写进 quarantine basename
    的 label（跨重启持久），驱逐时据此不计。同事建议的「在 `QuarantinePending` 里存来源」治不了这条
    路径——`recover_layout` 根本不写 `QuarantinePending`。
  - `M-69`、`M-70` 成立（minor），描述基本准确；`M-70` 同事建议的「持久化『计数已应用』标记」**不采纳**：
    该计数器每次 `Spool::open` 归零，持久标记会把高报换成漏报。改为把记账移到 durable 状态转换提交之后。
  - `M-71` 部分成立，**定级由 P2 下调为 minor**：故障是 fail-closed 的（构建中止，不产出错误产物），
    且发布链路从不构造 `PATH`，触发前提是操作员宿主 PATH 自带相对/空条目。缺陷本身属实，已修。
  - `M-72` 成立（minor）。**「静默」一词偏重**——两条降级提示都会打印，真正坏掉的是退出码。
    发布链路不经过该分支，Linux 主机走真 `cargo test`，该 fail-open 只能污染非 Linux 自查。
  - `M-73` 部分成立。真正的缺陷是**注释无条件承诺了 "clean end of stream"**，已改为陈述实际保证。
    同事建议的「在拒绝前消耗完整请求」**不采纳**：`ReadHeadError::TooLarge` 在未解析任何 header 时
    就返回，没有请求边界，等价于读到 EOF，恶意客户端不 `shutdown(SHUT_WR)` 就能无限期钉住 client
    permit——那正是 `max_request_bytes` 要防的。残留窗口是有界 drain 的固有代价，已写进注释与用例。
    同事原文的第二个建议（排空与 permit 解耦）成立，另立为 `m-220`。
- 2026-08-30：**审阅同事的 §16 收窄改动本身，发现并修复两处**（规范 v7 → v8，overlay `255b27f`）：
  v7 的兜底条款把三个纯 loopback、正对着本 overlay UDP 改动面的目标一并排除；新增的
  `WorkspaceGateDocsTests` 是文本 grep，36 个变异漏 17 个（含把门禁整行注释掉、加 `|| true`、
  数组留着不传给 cargo）。改为门禁即数据 + `--print-gate` + 与 §16 做集合相等，17 变异 17 抓 0 漏。
  另修 `--without-audit` 的无保留「测试通过」提示（`5a80797`）与 `--exclude` 的注释（`fb1f43b`）。
- 2026-08-30：**一处明确不改的判断**。`recover_layout` 为 acked 来源写出的 `segment_corruption` gap
  与驱逐时的 `quarantine_eviction` gap，`lost_events` 仍是该批次总数 N。§9.5 把 gap 的这些字段定义为
  「能够从损坏对象可靠取得的 nullable batch/digest/epoch/sequence/count/bytes」，即"损坏对象自称持有
  多少"，**不是**"未交付多少"；collector 手上有该批次的 ACK，可自行对账。故 gap 保持原样，只修名字
  就叫"未确认"的 `evicted_unacked_records`。
