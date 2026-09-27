# Linux 采集参考向量

`cases.json` 为自行构造的脱敏输入和预期值，没有读取当前机器的 `/proc`、地址或挂载。
数据用于 P1 Go 采集器验收，P0 只交付参考向量，尚不表示采集器或实机对照已通过。

参考为 [monitor-probe/agent collect.rs](https://github.com/monitor-probe/agent/blob/cebc5383abb5963abeb722877a2d82c1a7aea5c6/src/collect.rs)
固定提交中的 `parse_cpu_jiffies/busy_percent`、`mem_used/swap_used`、`disk_usage`、
`parse_mounts`、`parse_sockstat`、`Ifaces::counts` 和 `net_rate`。
输入数值与测试材料为本项目新写，未复制参考项目源码或测试。

格式约定：`operation` 指定被测功能，`input` 为虚拟文件文本、statvfs 返回值或显式
单调时间；`expected` 为字节、B/s、百分比或计数，`null` 是未知。
`reference_expected` 仅在自有质量语义与上游有意不同时记录原值。

- CPU 只求前八列总量；idle 含 iowait，guest 不重复计数。
- meminfo 的 kB 按 1024 字节换算；MemAvailable=0 有效，仅缺失才回退。
- statvfs 优先 f_frsize，used 使用 f_bfree，饱和到 uint64；f_bavail 不参与 used。
- 挂载按最后覆盖层解析，去重设备/ZFS pool，过滤伪文件系统和远程盘。
- 网络速率向下取整，按实际单调时差，显式排除优先于纳入。
- 首报、读取失败和计数器失效采用未知。网卡集合变化先重建范围基线；与上游对
  新旧接口交集计算速率的差异在向量中写明。P1 不得把质量差异称为逐字节协议兼容。

消费 fixture 时应实际调用生产采集函数；不能另写一份同样公式仅验证 JSON 自洽。
后续需继续增加完整 `/sys` 拓扑、Facts、地址优先级、读取错误恢复及 TCP 探测用例。
