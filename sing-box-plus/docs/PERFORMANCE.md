# 性能

## 验收方法

比较三组，而不是追求「零开销」：

| 组 | 构建与配置 |
| --- | --- |
| A 未启用 | 不带 `with_user_stats` 构建 |
| B 编译但未配置 | 带 `with_user_stats` 构建，配置中无 `user_stats` |
| C 启用四向统计 | 带 `with_user_stats` 构建，配置中有 `user_stats` |

B 组存在的意义是把「编进去的成本」与「用起来的成本」分开：未配置时本项目不创建 registry、
exporter 或任何附加包装，B 与 A 的差应当落在噪声内；真正的开销都应出现在 C。

启用 §4.9 配额闸断时再加一组 C′（`quota_control` 打开且已下发一份全量表），
用来单独衡量回调里那两次原子操作的代价。

记录：吞吐、p50/p99 延迟、CPU、分配次数、goroutine 数、内存随用户数与并发数的增长，
以及 exporter 被慢客户端占满时代理数据面的隔离性。

目标不是预设某个百分比，而是给出**可复现基线并设置回归阈值**。

## 热路径的成本上限

计数回调是全进程最热的路径：每次 copy 迭代与每次 splice 循环都会跑。因此它的实现被限制成：

- 四向累加：一次 CAS 循环上的饱和加法；
- 配额扣减：一次 `atomic.Uint32` 读 + 一次 `atomic.Int64` 加；
- 审计计数（仅在启用审计时）：一次 `atomic.Int64` 加。

**禁止在回调里查 map、取全局锁或做任何分配。** 违反这一条时，功能上仍然只封一个用户，
吞吐上却会拖累所有人——这是最容易漏掉的一类回归，因此它是基准的第一个观察项。

## 运行

```bash
# 数据面三组对照（A 未启用 / B 启用统计 / C 统计+配额闸断）
go test -tags "with_utls,badlinkname,with_user_stats" -run '^$' \
  -bench BenchmarkDataPath -benchtime 4000x -count=5 ./internal/userstats/

# 微基准：回调本身、配额扣减与快照序列化
go test -tags "with_utls,badlinkname,with_user_stats" -run '^$' -bench . -benchmem ./internal/userstats/

# 端到端吞吐：用集成测试的拓扑跑大数据往返
go test -tags "with_utls,badlinkname,with_user_stats" -run TestVLESSByteOracleLarge -count=5 ./internal/userstats/

# pprof
go test -tags "with_utls,badlinkname,with_user_stats" -run '^$' -bench BenchmarkCountUplink \
  -cpuprofile cpu.out -memprofile mem.out ./internal/userstats/
go tool pprof -top cpu.out
```

## 基线记录

### 微基准（已测）

`with_utls,badlinkname,with_user_stats`，go1.26.5，2026-09-06：

| 基准 | arm64 (M 系列) | arm64 (lima Linux) | **x86_64 (Intel N100)** | B/op | allocs/op |
| --- | --- | --- | --- | --- | --- |
| `BenchmarkCountUplinkNoQuota` | 4.01 ns | 4.05 ns | **8.16 ns** | 0 | 0 |
| `BenchmarkCountUplinkWithQuota` | 4.09 ns | 4.13 ns | **13.7–14.7 ns** | 0 | 0 |
| `BenchmarkSnapshot`（512 个身份） | 50 209 ns | — | 138 000 ns | 85 608 | 17 |

**热路径零分配**，三个平台一致——这是「禁止在回调里做任何分配」这条纪律的直接证据。

**配额扣减的代价强烈依赖架构，不能用一个数字概括。** arm64 上多出来的两次原子操作约 +2%
（落在噪声内），x86_64 上却是 **+68%**（8.16 → 13.7 ns），而且三次采样高度一致，不是噪声。
原因是这一步多了一次 `atomic.Int64.Add`：x86 上编译成 `LOCK XADD`（全屏障、代价高），
arm64 有 LSE 的 `LDADD` 则便宜得多。

两点必须一起读，否则会把这个数字用错：

- **它只落在真正带额度的 lineage 上**。`consume` 的第一步是 `state.Load()`，无限额度的身份
  在这里就返回了，只付一次宽松读——上表的 NoQuota 一列已经包含了这次读。
- **在 64 KiB 分块的转发里它看不见**。回调每 64 KiB 跑一次，5 ns 相对一次约 100 µs 的往返是
  0.005%，下面的端到端对照也确实测不出差别。只有当写入变得非常碎（大量小包）导致回调频率
  上升几个数量级时，它才会浮出水面——那种场景需要单独测。

回归阈值：`CountUplink*` 的 `allocs/op` 必须恒为 0——它一旦非 0，说明有人在回调里做了分配，
无论 ns/op 看起来多好都要退回。

### 环回吞吐三组对照（已测）

`BenchmarkDataPath{A,B,C}`，64 KiB 分块的 VLESS 往返。**以 Linux 那组为准**：
darwin 上的组内极差有 10–15%，组间差被噪声完全淹没；Linux 走真实 splice、噪声低一个量级。

Linux/arm64（Ubuntu、kernel 7.0、lima、4 vCPU）、`-benchtime 4000x -count=3`：

| 组 | 中位 ns/op | 中位吞吐 | 三次极差 | 相对 A |
| --- | --- | --- | --- | --- |
| A 未启用统计 | 40 213 | 1 630 MB/s | 40 086 – 40 433（0.9%） | — |
| B 启用四向统计 | 40 437 | 1 621 MB/s | 39 522 – 40 627（2.8%） | +0.6% |
| C 统计 + 配额闸断 | 39 937 | 1 641 MB/s | 39 750 – 40 001（0.6%） | −0.7% |

Linux/x86_64（Debian 13、kernel 6.12、Intel N100、4 核裸机）、`-benchtime 6000x -count=5`，取中位数：
A 100 579 ns / 652 MB/s（极差 16%）、B 99 089 ns / 661 MB/s（11%）、C 98 656 ns / 665 MB/s（2%）。

darwin/arm64、`-count=5`（供对照，噪声最大）：A 67 540 ns / 970 MB/s（10%）、
B 69 209 ns / 947 MB/s（12%）、C 63 942 ns / 1 025 MB/s（15%）。

**能说的**：在 64 KiB 分块的 splice 路径上，三个平台都测不出统计与闸断的开销——
arm64 上组间中位差 0.6–0.7%（组内噪声 0.6–2.8%），x86_64 上 B/C 甚至略快于 A
（组内噪声 2–16%）。开销确实存在（见上面的微基准），只是被 64 KiB 的往返成本淹没了。
**不能说的**：「开销恰为 0.6%」——这个精度三次采样撑不起来，跨平台也不成立。

这条测量**不能替代**三组端到端验收：环回没有真实 RTT、没有并发爬坡、没有 p99，
也没有 CPU 与 goroutine 随用户数增长的曲线。下表仍然是空的，首个正式发布必须填满它，
并在 `docs/UPSTREAM_BASELINE.md` 的验证记录里留一行指向本表。

| 日期 | 版本 | 组 | 吞吐 | p50 / p99 | CPU | B/op | allocs/op | 备注 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| | | | | | | | | |
