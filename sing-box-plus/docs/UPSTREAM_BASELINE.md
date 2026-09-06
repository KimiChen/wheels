# 上游基线

## 锁定

| 项 | 值 |
| --- | --- |
| repository | `https://github.com/SagerNet/sing-box.git` |
| tag | `v1.14.0` |
| commit | `0b8995879f29a9b98ee027bc17b75e101445b238` |
| commit_date | `2026-08-31T11:28:31+08:00` |
| prepared_tree_sha256 | `6342b9aeefec6eb9dcdf2a93b6d7b897bbc0724eccda1c0c4d09589d1051c9db` |
| license | GPL-3.0-or-later（含「衍生作品不得使用该应用名称或暗示关联」的附加条款） |
| go 最低版本 | 1.25.5（上游 `go.mod`） |
| go 已验证版本 | 1.26.5 |
| 轨道 | stable |

权威来源是 `upstream.lock`，本文件只是它的可读副本。`scripts/verify.sh` 会重新准备一棵源码树、
复算规范哈希并与 lock 比对，因此两者不一致时以 lock 为准、以 verify 的失败为信号。

## 复制文件

`cmd/sing-box-plus/` 下有 9 个文件复制自上游 `cmd/sing-box/`，合计 608 行——
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

## 已知的上游缺陷

### `route.NetworkManager` 的数据竞争

`Start()`（`route/network.go:220`）写入 interface 快照字段，而 `notifyInterfaceUpdate` 起的
goroutine 在 `updateInterface()`（`:574`）并发读同一字段，两侧无锁。

- 触发条件：同进程内并发启动多个 Box 且宿主发生网络接口变更通知（macOS 上很常见）。
- 与本项目的关系：本项目不触碰 `NetworkManager`，零补丁形态也无法修复。
- 处置：`tests/race-suppressions.txt` 只抑制这三个符号。为了让该文件无法掩盖本项目自身的竞争，
  `scripts/verify.sh` 在带抑制跑完 `-race` 全量之后，会**再跑一遍不带抑制**的纯单元用例
  （那批用例不启动 Box）。

### sing-vmess 的 Vision 实现与 `checkptr` 不兼容

`NewVisionConn` 用 `unsafe.Pointer(reflectPointer + field.Offset)` 直接读取 `crypto/tls.Conn`
的私有 `input` / `rawInput` 字段（`sing-vmess@v0.2.8-0.20250909125414 vless/vision.go:87`）。
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
