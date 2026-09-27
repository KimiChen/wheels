# 可选指标历史

本包内嵌 VictoriaMetrics `lib/storage v1.151.0`，与主控处于同一进程，不启动
额外服务或监听端口。只有主控明确启用历史并提供存储目录时才调用 `Open`。
SQLite 由 `monitor/control` 保存配置和当前流量；本包不负责今日流量、套餐记账、
鉴权或在线状态。

## 配置和接口

`Open(Config{Path, RetentionDays, ReportInterval})` 使用独立的私有目录，目录默认
0700，拒绝符号链接和公开权限的既有目录。`Path` 是 TSDB 目录，不是 SQLite 文件。
保留期默认 30 天，允许 1～365 天，由 VictoriaMetrics 存储层执行后台清理；过期
磁盘块不会在到期毫秒立即删除。节点 ID 使用正整数的十进制字符串。

- `Accept` / `AcceptProbe` 复制样本到有界队列，不等待磁盘。默认队列容量 4096，
  允许 1～65536；队列满时丢弃新样本，并通过 `Status` 标记历史降级。
- 后台每 512 条时序值批量写入，每 5 秒刷新一次。`Flush(ctx)` 等待此前接受的
  事件在存储中变为可查询，不承诺该调用完成 fsync；`Close(ctx)` 停止接受并排空。
  调用者超时只结束等待，后台继续
  关闭存储，不能把超时当成已完成关闭。
- 查询逐块聚合，不保留整个原始历史数组。时间范围最大 31 天，最小步长 1 分钟，
  自动扩大步长确保最多 500 点；上下界按分钟对齐。查询期限 3 秒，最多读取
  3200 万条时序值。调用者可传更短的 context 期限。
- `History` 保留各字段有效样本数、覆盖秒数和平均值。缺样为空值，不补零。
  `ProbeHistory` 分别给出成功探测平均延迟、失败数及最后一份结果；`-1` 表示失败，
  不参与成功延迟平均。任务键应绑定探测任务与目标，避免不同目标共用历史。

## 数据口径

时间戳使用主控接收时间，UTC 毫秒。只保存 `quality=ok` 的指标，覆盖时间按相邻
接收时间计算，每份样本最多贡献一个报告间隔；加速上报不会虚增整分钟覆盖。
存储使用 1 毫秒去重粒度，在 VictoriaMetrics 后台合并时执行；同一毫秒重复上报
不作为精确事件计数。

VictoriaMetrics 用 float64 保存曲线指标，所以超过 float64 精确整数范围的累计
计数器可能近似。API 为适配现有页面仍以十进制字符串返回整数均值，但不将其当成
精确流量账本。SQLite 中的系统基线、今日和套餐字节统计保持独立和精确。

## 资源和故障

VictoriaMetrics 的缓存配置是进程级设置，因此同一进程仅允许打开一个 Store；
必须等待旧 Store 关闭完成才能重新打开。VM 总缓存计算基数为物理/容器内存限额的八分之一，
上限 128 MiB，避免按整台机器内存无限扩张；TSID 32 MiB、
metricID→TSID 上限 8 MiB、名称 8 MiB、筛选 8 MiB、元数据 1 MiB。缓存预算不是整个
进程的内存硬上限；库内部还会按缓存分片粒度上调小预算。
内存限额未知或低于 64 MiB 时，`Open` 返回错误，使可选历史降级，不调用可能直接
退出进程的 VM 初始化。Linux 同时检查物理内存和 cgroup 限额。磁盘剩余空间低于
1 GiB 时暂停接收历史写入。

写入失败后不会重试提交结果未知的批次，以免重复计算样本。后续批次仍可写入。
丢样、低磁盘空间及查询/写入错误会让 `Status.degraded` 保持为 true，直到主控重启；
公开状态只返回固定错误类别，不泄露目录。调用栈内的存储 panic 会被转换为降级
错误；嵌入式库的后台线程及操作系统磁盘阻塞仍共享主控的进程边界，无法提供独立
进程级隔离。核心流量记账和实时接收不等待历史写入。

VM 自带的刷新与退出处理负责持久化；非正常掉电可能丢失最新尚未刷盘的历史。
本包不迁移旧 SQLite 历史，不保存原始令牌、地址、硬件事实或节点在线状态。

## 构建与验证

依赖补丁为 `patches/0002-storage-dependencies.patch`，需要 Go 1.26.6 或更高。
构建仍支持 `CGO_ENABLED=0` 的 Linux amd64/arm64。发布包随附 `licenses/` 中的依赖
许可证及来源。测试使用真实内嵌 VictoriaMetrics，覆盖字段质量、缺口、均值、
失败探测、重启恢复、缩短保留期清理旧分区、队列背压、并发关闭、目录保护及查询取消。
独立子进程测试覆盖 FRP 使用 Cobra、标准库 flag 尚未解析时的初始化，并检查 metricID
缓存上限不会随机器内存扩张；小内存预算和无法确定限额时的降级路径也有独立进程验证。已通过 store 测试和 race、Linux amd64/arm64 无 CGO
交叉编译，以及完整上游 config/metrics/client/server 测试和 frpc/frps 命令编译。

参考：[哪吒 TSDB](https://nezha.wiki/guide/q15.html)、
[VictoriaMetrics 存储源码](https://github.com/VictoriaMetrics/VictoriaMetrics/tree/v1.151.0/lib/storage)、
[保留机制](https://docs.victoriametrics.com/victoriametrics/single-server-victoriametrics/#retention)。
