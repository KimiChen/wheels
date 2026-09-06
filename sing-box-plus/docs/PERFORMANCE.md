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

darwin/arm64、go1.26.5、`with_utls,badlinkname,with_user_stats`，2026-09-06：

| 基准 | ns/op | B/op | allocs/op |
| --- | --- | --- | --- |
| `BenchmarkCountUplinkNoQuota` | 4.01 | 0 | 0 |
| `BenchmarkCountUplinkWithQuota` | 4.09 | 0 | 0 |
| `BenchmarkSnapshot`（512 个身份） | 50 209 | 85 608 | 17 |

两条结论：热路径**零分配**，这是纪律的直接证据；配额闸断多出来的两次原子操作在这台机器上
落在噪声内（约 2%），不构成单独的性能决策项。

回归阈值：`CountUplink*` 的 `allocs/op` 必须恒为 0——它一旦非 0，说明有人在回调里做了分配，
无论 ns/op 看起来多好都要退回。

### 环回吞吐三组对照（已测，但只能得出一个弱结论）

`BenchmarkDataPath{A,B,C}`，64 KiB 分块的 VLESS 往返，darwin/arm64、go1.26.5、
`-benchtime 4000x -count=5`，2026-09-06。取五次的中位数：

| 组 | 中位 ns/op | 中位吞吐 | 五次极差 |
| --- | --- | --- | --- |
| A 未启用统计 | 67 540 | 970 MB/s | 61 897 – 68 191（10%） |
| B 启用四向统计 | 69 209 | 947 MB/s | 65 918 – 73 679（12%） |
| C 统计 + 配额闸断 | 63 942 | 1 025 MB/s | 62 086 – 71 107（15%） |

**结论只能写成这样：三组在这台机器上不可区分。** 组内极差 10–15%，
而组间中位数差不到 3%——C 组「比 A 还快」正是这一点的证据，不是配额闸断加速了转发。
可以说的是：统计与闸断没有引入数量级的开销；不能说的是「开销为 X%」。

这条测量**不能替代**三组端到端验收：环回没有真实 RTT、没有并发爬坡、没有 p99，
也没有 CPU 与 goroutine 随用户数增长的曲线。下表仍然是空的，首个正式发布必须填满它，
并在 `docs/UPSTREAM_BASELINE.md` 的验证记录里留一行指向本表。

| 日期 | 版本 | 组 | 吞吐 | p50 / p99 | CPU | B/op | allocs/op | 备注 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| | | | | | | | | |
