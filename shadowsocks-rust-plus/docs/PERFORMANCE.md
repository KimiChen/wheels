# 参考性能基线

本文记录一组可复核的参考结果，只用于证明 benchmark 工具可运行、计数校验成立，以及发现量级
异常。它不是生产 SLO、容量承诺或目标机型验收结果。原始 JSON 只保存在临时证据目录中，不提交
包含完整机器信息的报告。

## 环境与基线

- 日期：2026-08-26（Asia/Shanghai）
- 系统：macOS 26.5.2，Apple arm64，16 GiB，10 个逻辑 CPU
- 工具链：rustc/cargo 1.97.0，Python 3.14.6
- 构建：Cargo `release --locked`
- 原始上游：`7ee1aa9223ed8f4d34734aac919036c8ad4502c2`
- plus：由同一提交重放 `patches/series` 后的干净 Git worktree

两套测试都只使用 IPv4 loopback、本机 echo 服务和每次运行时随机生成的临时密钥，不产生公网
代理流量。

## 快照规模

`tests/benchmark_snapshot.py` 通过 HTTP/1.1-over-UDS 完成 request-line/header 发送、状态与响应
header 校验、完整 JSON body 读取和 schema 校验。每种身份数运行 25 次；表中响应大小只统计
JSON body，RSS 是 `ssserver` 进程观测值。

| 身份数 | JSON 响应 | 延迟中位数 | p95 | 最大值 | RSS |
| ---: | ---: | ---: | ---: | ---: | ---: |
| 100 | 16,417 B | 0.300 ms | 0.514 ms | 0.583 ms | 11,552 KiB |
| 500 | 80,817 B | 0.793 ms | 0.815 ms | 0.835 ms | 12,336 KiB |
| 1,000 | 161,317 B | 1.567 ms | 1.620 ms | 2.013 ms | 12,672 KiB |

该组 HTTP 测试中，1,000 身份下最大往返时间为 2.013 ms，低于工具的 1 秒异常门槛。JSON body
大小随身份数近似线性增长；部署时仍须根据实际服务数、标识符长度和 `max_response_bytes` 复测，
不能把本表外推到 10,000 身份，也不能把参考环境延迟当作目标节点的验收值。

## 回环数据面

`tests/benchmark_data_path.py` 运行 1 次 warm-up 和 5 个测量样本。每个样本并发 4 个 TCP 与
4 个 UDP worker：每个 TCP worker 每方向传输 32 MiB，每个 UDP worker 每方向回显 2,000 个
1,200 B 数据报；提供的 payload 中 TCP 占 93.3%。表中 CPU 是 `ssserver + sslocal` 的进程
CPU 中位数，RSS 为两个进程的采样峰值之和。

| 配置 | 双向总吞吐中位数 | 范围 | CPU 中位数 | 合计峰值 RSS |
| --- | ---: | ---: | ---: | ---: |
| 锁定原始上游 | 768.162 MiB/s | 757.473–774.406 MiB/s | 0.75 s | 31,264 KiB |
| plus，运行时关闭统计 | 813.118 MiB/s | 739.727–831.035 MiB/s | 0.72 s | 30,864 KiB |
| plus，运行时开启统计 | 798.678 MiB/s | 748.219–816.100 MiB/s | 0.71 s | 31,296 KiB |

统计开启相对运行时关闭的中位吞吐为 `-1.78%`，相对原始上游为 `+3.97%`；各组范围高度重叠，
这些差异只反映短时 loopback 样本的调度与测量噪声，不构成性能回归或提升结论。CPU 与合计
RSS 的小幅差异也不能归因于统计功能。正式运行结束后，exporter 观测到的四向增量与 1 次
warm-up 加 5 个测量样本的全部载荷完全一致：

```text
TCP uplink/downlink: 805,306,368 / 805,306,368 bytes
UDP uplink/downlink:  57,600,000 /  57,600,000 bytes
```

## 结论与复测要求

### user-audit 对照与场景

目标 Linux 节点必须按 [`../tests/benchmark_data_path.py`](../tests/benchmark_data_path.py) 与发布门禁
[`../tests/benchmark_audit.py`](../tests/benchmark_audit.py) 的三案口径复测，case 名与工具保持一致：
`locked_upstream`（锁定 commit 的原始上游 release）、`plus_compiled_runtime_disabled`（编译了
`user-audit` 但运行时不配置 `user_stats`/`user_audit` 的 plus 二进制）与 `plus_runtime_enabled`
（同一个 plus 二进制、运行时同时开启统计与审计）。阈值以 `locked_upstream` 为主基线；不得退回
feature-off/feature-on 两案口径，也不得用两个不同二进制冒充 runtime-off 对照。三案之上还要覆盖
auditd healthy、offline、慢 ACK、producer queue 接近上限和 spool 接近容量上限五种场景。每种场景
至少 30 次重复，记录 p50/p95/p99、代理成功率、queue 深度、spool bytes、auditd RSS 与错误计数；
审计故障不得增加代理请求错误，达到水位时只能产生规范 gap/health degraded。当前 macOS 参考表不是
目标机门槛，不得据此伪造 Linux 验收结果；原始 JSON 必须注明 CPU、内核、磁盘和工具链。

本机结果没有发现可见的数据面回归，并验证了 benchmark 开启统计时的精确计数断言。由于每个
样本仅约 0.33–0.37 秒、仅覆盖单机 loopback，结果不能评估真实 RTT、丢包、拥塞、NUMA、长期
运行、更多身份或生产 TLS/隧道开销。

生产候选发布前必须在目标机型延长每个样本的传输量和采样时长，至少重复三组独立运行，并分别评审
TCP/UDP 比例、吞吐分布、CPU、RSS、长连接、突发并发和 exporter 采集压力。原始 JSON、二进制
SHA-256、plus commit、上游 commit 与运行参数必须随发布证据一起归档。
