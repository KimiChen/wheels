# shadowsocks-rust-plus 用户成功访问审计 · 待办与未决事项（V2）

> 文档定位：**当前活动的待办清单**。记录截至 2026-08-30 仍未解决、未执行或需要决策的事项。
>
> 与 [`USER_ACCESS_AUDIT.md`](USER_ACCESS_AUDIT.md) 的关系：
>
> - 那份是**历史文档**，保存规范合同（v6，第 1–16 节）与第 1–8 轮审计、整改及 Linux 实装验收的
>   完整过程记录（第 17–30 节）。合同条文仍以那份为准，本文不复制、不改写合同。
> - 本文只列**尚未闭合**的条目。已闭合项不再重复，只在需要时引用其出处小节。
>
> 基线：overlay `main`，规范版本 v6，`patches/0003-user-audit.patch` 与
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

## 3. 待执行的验证

以下为发布前置，**从未在任何机器上执行过**：

| 项目 | 说明 | 阻塞因素 |
| --- | --- | --- |
| `cargo test --workspace --features user-audit`（Linux 全绿） | §16 验收项 | `M-66` 已修复解除阻塞；**该命令本身仍未跑完过一次全绿** |
| `cargo-fuzz` sanitizer 实跑 | §3.2/§14.4 要求交付并运行 fuzz target | 无，尚未安排 |
| §14.5 目标机压测 | 吞吐 ≤5%、CPU ≤10%、ssserver RSS ≤64 MiB、auditd RSS ≤128 MiB，及离线/队列满/慢 ACK/spool 满四类专项 | 需目标机与真实数据面负载 |
| 真实流量端到端审计事件 | 经 ssserver 转发真实 TCP/UDP 流量后，验证 access event 落入 spool 并可经 lease 导出 | `integration_audit.py` 覆盖的是 ingest/export 协议链路，**不含**真实代理流量 |

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

## 5. 需要决策的事项

**当前无未决事项。** 此前列出的三项均已决策并落实，结论见第 6 节：

1. `M-66` 的归属 → 并入 `0003`（已修复）；
2. `D-5` 是否收窄发布构建的 feature 集 → 收窄（已实施，规范升版到 v6）；
3. 实装节点的处置 → 清理（`10.0.1.3`、`10.0.2.3` 均已回到基线）。

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
  修复在位 5/5 通过，去掉 drain 后 4 项转红。
- 2026-08-30：**`D-5` 已实施**（overlay `8711cbd`，规范升版 v5 → v6）。发布构建改为
  `--no-default-features` 加显式 feature 集，§15.1 增列该集合。x86_64-unknown-linux-musl
  下少编译 29 个 crate（brotli/zstd/flate2、tun/smoltcp/etherparse、nix、qrcode、
  tokio-util、rustls-native-certs 等）；`webpki-roots` 保留，DoT/DoH 根证书来源不变。
  **更正 D-5 原文的一处事实错误**：`reqwest`/`web-sys` 并不在该 target 的依赖图中——
  `local-online-config` 拉入的是 `mime`/`flate2`/`brotli`/`zstd`，收窄真正移除的是上述这批。
