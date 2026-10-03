# 上游基线

## 锁定

| 项 | 值 |
| --- | --- |
| repository | `https://github.com/SagerNet/sing-box.git` |
| tag | `v1.14.2`（候选；发布被计费契约阻断） |
| commit | `af6e64c3b69e6132ebaee0e1a3d24e93903f6709` |
| commit_date | `2026-09-24T18:52:34+08:00` |
| prepared_tree_sha256 | `8119c3fde55e4716f2afc0192a897a464eaa1960365a799ed0611d8d9dc7be9b` |
| license | GPL-3.0-or-later（含「衍生作品不得使用该应用名称或暗示关联」的附加条款） |
| go 最低版本 | 1.25.5（上游 `go.mod`） |
| go 已验证版本 | 1.26.8 |
| 轨道 | stable |

权威来源是 `upstream.lock`，本文件只是它的可读副本。`scripts/verify.sh` 会重新准备一棵源码树、
复算规范哈希并与 lock 比对，因此两者不一致时以 lock 为准、以 verify 的失败为信号。

### Go 发布工具链：`go1.26.8`（2026-10-03）

独立工具链提交只调整 `go_verified` 和发布门禁，不变更 Go module 依赖、上游核心基线或复制文件。
发布脚本显式设置 `GOTOOLCHAIN=local`，要求本机安装的 `go env GOVERSION` 与锁定值
精确一致；缺失锁定值、非补丁版本、旧版或更高版本均在取源及创建输出目录前失败。
环境中设置 `GOTOOLCHAIN=auto` 或指定其它版本不能绕过此检查。门禁测试使用隔离 Git
夹具验证拒绝路径与成功构建路径，实际兼容性须由完整 Go 测试、Linux 专项与可复现构建验证。
旧版工具链的历史验证和性能数据保留原记录。

### 历史修复：Linux 网络监听依赖：`sing-tun v0.9.2`（2026-10-03）

0.2.2 发布时 `go.mod` 单独固定 `github.com/sagernet/sing-tun v0.9.2`，官方 tag 对应
[`c11c2568b9dea0e60d194982946a1cd797d7f378`](https://github.com/SagerNet/sing-tun/commit/c11c2568b9dea0e60d194982946a1cd797d7f378)
（2026-09-09，修复 netlink receive overrun 后网络监听停滞）。`sing-box v1.14.0`、
`upstream.lock` 和复制文件保持原基线；没有 vendoring、本地源码补丁或 `go -overlay`。

旧版 `v0.9.0-beta.4` 的 `loopUpdate` 接收 route/link/address 三个 channel 时不检查关闭状态。
底层订阅遇到接收错误退出并关闭 channel 后，读取立即返回，循环可持续占满一个 CPU 核心。
`v0.9.2` 改用单个 nonblocking netlink socket，申请 1 MiB 接收缓冲，以容量为 1 的 channel
合并通知；`ENOBUFS` 表示丢失了网络变更事件，因此触发状态重新同步并继续读取。
**恢复边界是 `ENOBUFS`**，其它读取错误会记录日志并退出；未加入一般错误的退避或重建机制。

[相对 beta.4 的官方比较](https://github.com/SagerNet/sing-tun/compare/v0.9.0-beta.4...v0.9.2)
只有 3 个提交、5 个文件，公开 API 和模块依赖不变。除监听器实现与其回归测试，另包含
TUN bypass verdict 转发修复，以及 nftables prerouting prematch 在 DNAT 前求值的优先级修复。
升级采用官方发布版本，普通构建、测试和可复现 release 均通过 Go module 解析使用同一实现。

上游 `TestNetworkUpdateMonitorReceiveOverrun` 会缩小接收缓冲、创建 dummy 接口与 4096 条
路由，确认 socket drops 大于零；随后检查 2 秒空闲期 CPU 时间小于 200 毫秒，并验证再次改变
链路仍有通知。用例需要 Linux root 和网络管理能力，**必须在独立网络命名空间运行**。
`scripts/test-network-monitor.sh` 编译后先用 `-test.list` 防止零用例假成功，再通过
`timeout 120s unshare --net` 执行，隔离失败即终止。一般 `go test ./...` 不会覆盖依赖包测试；
macOS 可交叉编译后在匹配架构的隔离 Linux 测试主机运行，具体命令见 README §9.3。
本节记录版本与验证方法；编译成功不能代替 Linux 内核专项执行结果。

### 1.14.2 候选状态（2026-10-03）

已按规范仓库重新核对 tag、准备源码并复算规范哈希；依赖严格采用该 tag 官方版本。
复制 CLI 中 `cmd_run.go` / `cmd_check.go` 同步 `service.ExtendContext`，新建 Box 的服务注册
不再污染进程上下文，其余七份仅更新来源头并重算双向锁。
已通过提交前的 `git pull --rebase --autostash` 纳入独立安全工具链提交 `9bb67f8`，
`upstream.lock` 的 `go_verified` 与候选实际验证均为 Go 1.26.8。

**发布阻塞**：新的 Linux splice 在成功读入 pipe 后立即执行 read counter，而非目标写入后。
本项目 uplink 由 read counter 记账，实测出现目标收到 0 字节但计费 131072 字节。
计费合同、失败断言和完整设计评审见 [UPGRADE_1_14_2.md](UPGRADE_1_14_2.md)。
此候选不合入生产分支，不生成 release，不部署；不能改计费定义或跳过异常写入用例放行。

## 复制文件

`cmd/sing-box-plus/` 下有 9 个文件复制自上游 `cmd/sing-box/`，合计 610 行——
`github.com/sagernet/sing-box/cmd/sing-box` 是 `package main`，Go 禁止导入。

清单与双向哈希在 `cmd/sing-box-plus/copied-files.lock`：每行记录上游侧与 overlay 侧两个 sha256。
`scripts/release-artifact.py copied-files-check` 两个方向都查——只查一侧会漏掉另一类漂移：

- 上游侧不符 = 钉定版本的内容变了，或 lock 记错了版本；
- overlay 侧不符 = 有人改了复制文件却没登记。

实质修改集中在四个文件（每个文件顶部的 overlay 头有逐条说明）：`cmd.go` 换最小 registry、
`cmd_run.go` 加进程级 registry 与 `Start()` 前的 `AppendTracker`、`cmd_check.go` 加独立校验与
重载不变量、`cmd_version.go` 改四行输出。其余五个只加了注释头。

## 升级规则

1. 跟随 **stable 轨道**，只在 patch 内自动跟进；minor 升级视为新基线，需重跑全部对账矩阵。
2. 每月核对上游 tag；安全修复 T+2 工作日完成评估、T+7 产出可复现构建。
3. 引入补丁后（当前为零补丁），不能零 fuzz 应用即失败，不得静默跟随其他版本。
4. 每次升级重跑全部门禁并把结果追加到本文件末尾。
5. 1.14.x 依赖 `sing`、`sing-tun`、`sing-quic` 的 beta 模块，须用 `go.sum` 固定并跟踪转正节奏。
6. 所跟随轨道转为 oldstable 或 EOL 后 30 天内必须迁移。

### 每次升级必须重新验证的三件事

1. **`AppendTracker` 调用点普查**。`scripts/verify.sh` 断言上游 `box.go` 中恰好两处。
   数量或位置变化都会让「本项目 tracker 恒在包装链最外层」这一前提失效，四向口径会静默退化。
2. **`CountFunc` 与 unwrap 语义**。闸断与计数都实施在回调内，依赖
   `common/bufio/copy.go` 的解包行为与 `splice_linux.go` 仍然调用回调。
3. **一次真实 TLS 握手冒烟**。带 `badlinkname` 时 badtls 的 reflect 字段校验失败会返回
   非 `os.ErrInvalid` 的错误并被直接抛出；Go 工具链升级若改动 `crypto/tls` 内部字段名，
   会表现为 TLS 连接硬失败而不是静默降级。

## 受限网络下的构建主机

实测的一台 Debian 13 构建机上，`curl https://github.com/` 返回 200，但 **git-over-HTTPS 被完全阻断**
（`git ls-remote` 静默失败），`proxy.golang.org` 只解析到 IPv6 且不可达。这类主机上：

- `GOPROXY` 换成可达镜像（实测 `https://goproxy.cn` 可用），`GOSUMDB=sum.golang.google.cn`；
- 上游取源与漂移检查用一个自包含的浅镜像：
  `git clone --bare --depth 1 --branch <tag> <upstream> /path/mirror.git`（v1.14.0 只有 2.3 MB），
  然后 `export UPSTREAM_REPOSITORY=/path/mirror.git`；
- 此时 `scripts/verify.sh` 的结论会自动改写为「上游漂移检查仅对镜像」——它**不证明**上游未漂移，
  规范地址的核对必须在另一台能访问 GitHub 的机器上做。完全无镜像可用时，
  显式置 `SING_BOX_PLUS_SKIP_UPSTREAM_DRIFT=1`，结论会进一步弱化为「本次完全没有核对上游」。

## 已知的上游缺陷

### `route.NetworkManager` 的旧数据竞争已由候选移除

v1.14.0 的 `Start()` 写 `started` 与 `updateInterface()` 并发读该字段无锁。
v1.14.2 以受 `interfaceUpdateAccess` 保护的 `startedCtx` 与 `networkResetPending` 替代，
关闭时取消启动 context、等待 `resetRunAccess`，并注销接口更新回调。
当前候选的 `tests/race-suppressions.txt` 已清空旧三条符号抑制；Go 1.26.8 / darwin arm64
全量 `go test -race` **不带抑制通过**。这只确认本次测试未报告竞争，不宣称所有可能时序无竞态。
Linux 异常写入门禁独立失败，race 通过不影响候选禁止发布的结论。

### sing-vmess 的 Vision 实现与 `checkptr` 不兼容

`NewVisionConn` 用 `unsafe.Pointer(reflectPointer + field.Offset)` 直接读取 `crypto/tls.Conn`
的私有 `input` / `rawInput` 字段（`sing-vmess@v0.2.8 vless/vision.go:87`）。
这类 uintptr 运算被 Go 的 `checkptr` 判为「指向无效分配」，而 `-race` 会一并打开 `checkptr`，
于是整个测试进程 fatal 退出——不是竞争，`GORACE=suppressions` 也压不住（那只作用于 TSan 报告）。

- 与本项目的关系：本项目不触碰该路径，零补丁形态无法规避。
- 处置：`TestVisionByteOracle` 在 `-race` 轮次跳过并说明原因；`scripts/verify.sh` 因此专门跑一轮
  **不带 `-race`** 的全量测试，使 Vision 的字节对账真的被执行过，而不是被 skip 掉之后无人再问。
- 生产影响：`badlinkname` 生产构建不启用 `checkptr`，运行期不受影响。但这条依赖
  `crypto/tls` 的内部字段名，Go 工具链升级若改名会表现为**运行期硬失败**——
  这正是升级门禁里那次真实 TLS 握手冒烟存在的理由。

### `sing-box check` 在畸形 `ssm-api` 配置上 panic

v1.14.0 实测：`ssm-api` 的 `servers` 键缺前导 `/` 时 panic 退出（exit=2，
`panic: chi: routing pattern must begin with '/'`），而非返回可读配置错误。
本项目的校验全部在 `box.New()` 之前独立完成，不依赖上游 check。

## 验证记录

| 日期 | 内容 | 结果 |
| --- | --- | --- |
| 2026-10-03 | 官方 SHA-256 校验的 Go 1.26.8，独立模块缓存与原 go.sum；完整 verify.sh、go mod verify、Linux userstats 与隔离 netlink 回归 | 通过：规范上游漂移、构建、三轮 Go 测试（含 race）、107 项 Python 测试；Linux userstats 115 通过、1 项要求 race 的用例跳过；netlink overrun 回归 4.51 秒通过 |
| 2026-10-03 | govulncheck v1.8.0 对 Go 1.26.8 Linux amd64 生产标签源码复扫 | 标准库告警及符号可达级告警为 0；仍有 9 项模块/包级告警。uTLS fork 的人工风险审查不由标准库升级自动关闭 |
| 2026-10-03 | 固定 `sing-tun v0.9.2`，在 Linux 独立网络命名空间运行上游 `TestNetworkUpdateMonitorReceiveOverrun` | 通过（4.53 秒）；实际 socket drops 大于零，随后 2 秒 CPU 时间小于 200 毫秒，后续 link 变化仍触发通知 |
| 2026-09-15 ~ 2026-09-18 | **门禁红窗**：`ea43087` 为访问审计排除名单引入 Google/Apple 的真实 CIDR，`scripts/check-sensitive.sh` 的允许清单未同步，退出码 1；`verify.sh` 带 `set -e`，因此这段时间内它从未跑到最后一行 | 期间落地的 `e5fa344`、`17a9416`、`1fd1643`、`603ab57` 未经完整门禁 |
| 2026-09-18 | 允许清单改为独立文件 + 按 token 匹配 + 每条强制带理由 + `[public]` 条目陈旧即失败；补 17 项契约测试 | 通过 |
| 2026-09-18 | 无抑制 `-race` 轮改用 `go test -skip` 取补集，并先用 `go test -list` 断言每个被排除的名字存在 | 旧 `-run` 白名单含三个不存在的用例名，该轮实际只跑七个用例；现覆盖 40+ 个 |
| 2026-09-18 | `scripts/verify.sh` 端到端重跑（darwin/arm64，上游漂移检查为规范地址） | 通过——自 2026-09-06 后的首次完整通过 |
| 2026-09-06 | 冻结基线：clone 到钉定 commit，复算 `prepared_tree_sha256` | 与 lock 一致 |
| 2026-09-06 | 复制文件双向漂移门禁（9 个文件） | 通过 |
| 2026-09-06 | darwin/arm64 构建、`linux/amd64` 与 `linux/arm64` `CGO_ENABLED=0` 交叉编译 | 通过 |
| 2026-09-06 | 零 tag 与生产 tag 集两种构建 | 均通过 |
| 2026-09-06 | 四向字节 oracle：VLESS TCP 4/4、100×64KiB 双向 6553600、XUDP 归入 UDP、SS-2022 EIH 身份分离 | 误差 0 |
| 2026-09-06 | Vision 对账：TLS + `xtls-rprx-vision`，小往返 4/4（padding 不计入）、100×64KiB 精确相等（buffered→direct 切换后不退化） | 误差 0 |
| 2026-09-06 | 排空阶段：排空期拒绝新的计费连接且 0 字节不入账，会话归零后立即返回 | 通过 |
| 2026-09-06 | `go test -race` 全量（含上游竞争抑制）与无抑制的纯单元用例（darwin/arm64） | 通过 |
| 2026-09-06 | **Linux 实跑**：linux/arm64 交叉编译的测试二进制在 Ubuntu（kernel 7.0）上跑全量 46 个用例，含真实 splice 路径的字节 oracle 与 splice 上的配额闸断 | 全部通过 |
| 2026-09-06 | Linux 数据面三组基准（真实 splice）：A 1630 / B 1621 / C 1641 MB/s，组内极差 0.6–2.8% | 开销在 1% 量级 |
| 2026-09-06 | **裸机 Debian 13 / x86_64（Intel N100）实跑**：native 构建、`go vet`、零 tag 与生产 tag 两种构建、全量测试、`-race` 全量（本机此前无法覆盖的一项）、Python 47 例、`scripts/verify.sh` 端到端 | 全部通过 |
| 2026-09-06 | 不带抑制的 `-race`：确认报告的读写两端全在上游 `route/network.go:220` 与 `:574`，抑制文件未掩盖本项目的竞争 | 结论成立 |
| 2026-09-06 | 可复现发布构建：linux/amd64 与 linux/arm64 各两次独立构建逐字节一致，产出 manifest + SHA256SUMS | 通过 |
| 2026-09-06 | 跨机签名与验签演练：在 git 工作树内签名 → 送到构建机验签通过；篡改一个字节后验签失败 | 通过（用一次性密钥，正式发布仍需离线私钥） |
| 2026-09-06 | 真实部署：systemd + sysusers + tmpfiles 按 `docs/OPERATIONS.md` 安装，专用用户运行，socket 权限 0600 | 通过 |
| 2026-09-06 | 真实流量计量：VLESS 4/4 与 1 MiB 双向 1048576、SS-2022 4/4，未使用身份恒为 0 | 误差 0 |
| 2026-09-06 | 真实部署上的配额闸断：u1 在途连接被切、新连接被拒，u2 全程不受影响；`active` 与 `health` 未变 | 通过 |
| 2026-09-06 | `systemctl reload`：`runtime_id` 与 `started_at_unix_ms` 逐字节不变、`sequence` 递增、计数不清零；改 `node_id` 的重载被拒且旧实例继续服务 | 通过 |
| 2026-09-06 | `systemctl stop` 触发排空：日志出现「开始排空 → 排空完成」，停止耗时 5.2 s（等待在途连接） | 通过 |
| 2026-09-06 | packaging 中原样的 systemd 单元（`-D … -C …` 目录形式）可直接启动 | 通过 |
| 2026-09-06 | 带持续流量的短长跑：73 个批次、跨 2 个 runtime（中途做了一次 §5.3 计划重启）、实际入账 193 MiB | 四项判据（负增量 / 未知 runtime / 重复 sequence / unhealthy 入账）全为 0 |
| 2026-09-07 | 在一台 18 核 x86_64（Ubuntu 26.04）上复测配额回调成本：8 轮 × 5 秒、约 7.8 亿次迭代 | +1.676 ns（+21.8%），组内极差 0.3% / 1.5%；据此改写了 N100 上 +68% 的结论 |
| 2026-09-07 | 同一台机器上的数据面三组对照：A 48 410 / B 49 218 / C 49 299 ns | 组间 ≤ +1.8%，未超出组内噪声（5–6%） |
| 2026-09-07 ~ 09-14 | **7 天 7 小时真实流量长跑**（officenvidia 节点，VLESS+REALITY 与 SS-2022 两个计费身份）：10 453 轮采集 / 10 453 次额度下发 / 入账 19.2 GiB；审计 65 747 条、765 个独立域名 | 判据数字见 2026-09-19 那行（当时那份报告的四项里三项不可能非零，不作数）；审计 0 半行、0 事件行、0 混入他人、`seq` 逐文件连续 |
| 2026-09-17 | **调低阈值后复现轮转**：`max_bytes` 从 16 MiB 降到 1 MiB 后，`kimi-vless` 的文件写到 1 048 630 字节触发，重命名为 `.20260917T042430.583917141Z` 并重开 | 越界 54 字节（一条记录），与 16 MiB 那次的 31 字节同量级，证明越界量只取决于记录长度而非阈值；`seq` 878→879 无缺无重，新文件首行即 `ev=filter` 留痕并占用 879 号，旧文件 mtime 停在轮转时刻不再增长 |
| 2026-09-15 | **生产首次轮转**：`kimi-vless` 审计文件写到 16 777 247 字节（越过 `max_bytes` 16 MiB 31 字节，即一条记录的越界量）后重命名为 `.20260915T110147.097453886Z` 并重开 | `seq` 69833→69834 连续，两侧各自无缺无重，旧文件停止增长；证明 `rotate()` 的重命名+重开正确。外部删除/改名触发的 `os.SameFile` 路径（纪律 9 本体）与 `max_total_bytes`（纪律 10）仍未触发 |
| 2026-09-15 | **纪律 9 外部改动检测**（同机旁路实例，同一 `/usr/bin/sing-box-plus` 0.2.0，同一 ext4，`flush_interval_ms=200`）：静止时 `rm` 活动文件 → 一个 tick 内自动重建、续写首 `seq` 恰为删除前末 `seq`+1；100 conn/s 在途时外部 `mv` → 旧文件 `seq` 156~459、新文件 460~605 | 两种外部改动都被 `os.SameFile` 检出并重开；`mv` 在途零丢失（跨文件合并后 450 条无断点）。`rm` 分支的 tick 内缓冲记录按定义不可计量，见 README §7.1 第 4 条 |
| 2026-09-15 | **纪律 10 总量封顶**（`max_bytes` 64 KiB / `max_total_bytes` 192 KiB，两个身份）：先让 `verify-b` 轮转两次，再让 `verify-a` 顶过上限 | 被删的是**全目录最老**的已轮转文件（属于 `verify-b`），gap 行写进**它所属身份**的文件而非触发方：`{"ev":"gap","reason":"total_cap","after":600,"seq":601,"n":null}`；目录回落到 152 677 ≤ 196 608 |
| 2026-09-15 | **纪律 10 自指防护**：`max_bytes = max_total_bytes = 64 KiB`，两个身份各写 36 692 B，总量 73 384 B 超限但无已轮转文件可删 | 未写任何 gap 行（符合「无对象可删时只计数不写 gap，否则自指 gap 放大真事件」）。**同时暴露**：该场景当时无日志、无快照健康位，运维侧不可见；已于同日修复——`health.audit_dropped`（schema v2→v3）加上限流日志 |
| 2026-09-08 | 长跑中途换二进制（加逐用户审计）：换前采最终快照、换后按首快照策略处理新 runtime | 基线与用量跨 runtime 连续，额度未被重启清零；自造的 98 秒空窗被如实记为 2 条 `transport_error`，未与「没有流量」混淆 |
| 2026-09-19 | **长跑判据重算**：修好 `report()` 后，对归档账本（`ledger.jsonl` 32 464 行 / sha256 `6aa675d0…`、`state.json` sha256 `b9c2464c…`）按 09-07 00:32 ~ 09-14 17:44 重算。原实现的 `unknown_runtime` 读的是 `schema_rejected`、`duplicate_sequence` 只从已入账行推导、`unhealthy_accepted` 是字面量 0，退出码也不看 `transport_error`/`not_accepted` | 负增量 0 / 未知 runtime 0 / 重复 `sequence` 0 / 计费 health 位入账 0；`unhealthy_rejected` 0（闸门窗口内一次未触发）。窗口内只有三种结果：`accepted` 10 453、`quota_pushed` 10 453、`transport_error` 2，无任何 `not_accepted`/`schema_rejected`/`failed_closed`。与进程内独立累加的 `state.json:stats` 逐项吻合。新判据把传输失败计入失败，**退出码 1**（那 2 条即 09-08 的 98 秒空窗）。`audit_dropped` 不受 `health_ok` 闸断且旧账本不记 `health`，该项无法回溯，10 453 行计入 `health_not_recorded` |
| 2026-09-06 | §5.3 计划重启演练：停流量 → 轮询会话归零 → 采集最终快照 → 重启 → 新 runtime 按首快照策略处理 | 通过 |
