# TCP 探测

本目录为原创 Go 实现。字段、成功/失败/DNS 超时口径参考 MIT 项目
[monitor-probe/agent 的固定版本](https://github.com/monitor-probe/agent/blob/cebc5383abb5963abeb722877a2d82c1a7aea5c6/src/main.rs)，未复制 Rust 源码。

`probeEnabled` 默认关闭；开启后才协商 `ping.v1`。DNS 限时 900ms，每地址 TCP
握手限时 900ms，最多尝试解析顺序中的三个地址。成功延迟为整毫秒，不包含 DNS
与前一个地址的失败耗时。普通解析错误或全部握手失败为 `-1`；DNS 超时、任务取消、
本地目标策略阻止均缺样，不能作为探测失败率的分母。

默认只允许公网单播目标。`probeAllowPrivate = true` 额外允许回环与 RFC1918/ULA
私网；链路本地、未指定、多播、CGNAT、基准测试、文档及保留地址始终拒绝。每轮先解析，
逐个校验最终 IP，再直接拨号该 IP，避免二次解析绕过策略；带 zone 的 IPv6 地址拒绝。

每个会话最多 64 个任务，间隔 5–3600 秒，固定 4 个 worker，无任务 goroutine。
完整任务列表必须使用递增版本，替换时取消所有旧任务并清空旧结果；断线也取消探测。
64 个结果的有界队列满时丢弃新增样本。按配置间隔运行，错过的轮次延后且不补发。
接收任务和 TCP 测量独立于 host report/心跳写循环；测量不执行命令、不发送应用层数据。
