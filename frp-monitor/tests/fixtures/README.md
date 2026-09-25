# 参考 Fixture

自制脱敏样本，供采集算法逐字段对照验收（根 README §3、§9）。所有数值为虚构，
不含真实主机名、地址或凭据；IP 一律来自文档保留段（192.0.2.0/24、2001:db8::/32），
域名使用 RFC 2606 保留域。

## 目录布局

`proc/` 镜像 Linux `/proc` 相对路径，未来采集器测试可直接以本目录为根：

| 文件 | 覆盖场景 |
|---|---|
| `proc/meminfo` | 常规样本：MemTotal 4194304 kB，MemAvailable 存在 |
| `proc/meminfo.available-zero` | MemAvailable 为 0 是有效数据，不得回退或标未知 |
| `proc/meminfo.legacy` | 缺失 MemAvailable，回退 MemFree + Buffers + Cached = 2490368 kB |
| `proc/stat` | CPU 累计时间差分（含 iowait、guest），boot 时间、进程数 |
| `proc/loadavg` | 1/5/15 负载 0.42/0.38/0.35，进程总数 187 |
| `proc/uptime` | 启动秒数 123456.78（4 核 idle 累计） |
| `proc/net/dev` | 接口过滤：lo、docker0、veth、br-、tun0、tailscale0 应剔除，仅计 eth0 |
| `proc/net/sockstat` + `proc/net/sockstat6` | TCP = IPv4 inuse 31 + tw 17 + IPv6 inuse 11 = 59；UDP = 9 + 5 = 14 |
| `proc/diskstats` | 整盘/分区/dm/loop 混合，供磁盘去重与汇总参考 |
| `proc/mounts` | 伪文件系统（proc/sysfs/tmpfs）、overlay、NFS、同设备重复挂载的过滤与去重 |

`proc/net/dev` 中 eth0 的累计字节（12345678901 / 9876543210）、sockstat 汇总数、
loadavg 与 uptime 与 `shared/` 契约 golden 中的样本值刻意一致，便于跨层对账。

协议 golden（hello/report/ping.tasks/ping.result）随代码放在
`shared/protocol/testdata/` 与 `shared/metrics/testdata/`，由 Go 单测逐字节比对。

## 使用约束

- 计数器回退、boot_id 变化、网卡集合变化等场景在 P2 以增量 fixture 补充。
- 修改样本数值时同步检查 `shared/` golden 的一致性。
- 新增样本必须保持脱敏，并在此登记来源与覆盖场景。
