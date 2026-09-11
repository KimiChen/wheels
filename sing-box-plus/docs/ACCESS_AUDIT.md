# 基础访问审计（可选，默认关闭）

回答「哪个计费身份在什么时候访问了哪个域名或 IP 的哪个端口」。

**不**回答 URL、Header、证书链；**不**提供抗篡改与抗抵赖；**不**保证事件不丢。
未配置时不创建任何 goroutine、文件或包装，数据面与未启用完全一致。

启用它是产品与合规决策，不因计量已上线而自动成立：开启即意味着节点上存在「用户 → 域名」明细。
它与计费链路完全独立——不进快照、不走 UDS、不参与差分与结算，两条链路的保留期、访问控制与
对外声明必须分别制定。

## 配置

```json
{
  "services": [
    {
      "type": "user_stats",
      "node_id": "node-example-01",
      "listen_path": "/run/sing-box-plus/user-stats.sock",
      "inbounds": ["vless-entry-01"],
      "access_log": {
        "path": "/var/lib/sing-box-plus/access.jsonl",
        "max_bytes": 268435456,
        "max_total_bytes": 1073741824,
        "flush_interval_ms": 1000,
        "queue_size": 8192
      }
    }
  ]
}
```

域名维度需要在计费 inbound 上配 `action: "sniff"`；未配置时 `host_src` 恒为 `fqdn` 或 `ip`，
这是正常降级而非故障。

## 记录形态

每条成功访问一行 JSONL：

```json
{"seq":10241,"ts":1787587200123,"node":"node-example-01","run":"0123456789abcdef0123456789abcdef","user":"u_example_01","in":"vless-entry-01","net":"tcp","host":"example.com","host_src":"sniff","port":443,"up":5120,"down":81920,"ms":2317}
```

| 字段 | 含义 |
| --- | --- |
| `seq` | writer 侧单调递增序号，进程内唯一。与快照的 `sequence` 是两个独立命名空间 |
| `ts` | 连接关闭时刻，Unix 毫秒，**墙钟**。时钟回拨时 `seq` 仍严格递增，下游按 `seq` 排序 |
| `node` / `run` | 与快照同源的 `node_id` 与 `runtime_id` |
| `user` / `in` | 计费身份名与 inbound tag |
| `net` | `tcp` / `udp` |
| `host` | `metadata.Domain` → `Destination.Fqdn` → `Destination.Addr` 三级降级；已归一化 |
| `host_src` | `sniff` / `fqdn` / `ip`，标明 `host` 的来源与可信度 |
| `port` | 目标端口 |
| `up` / `down` | 该连接四向计数中对应方向的字节数，**原样落盘**，成功判据交由读取方复核 |
| `ms` | 连接时长 |

`host` 的归一化到此为止：只做小写 + 去掉一个尾点，并截断到 255 字节、转为合法 UTF-8。
**没有独立的 `host_norm` 键**，归一化结果直接写进 `host`。不引入 IDNA/UTS #46 转换。

缺口用同一条流表达，不另开通道：

```json
{"seq":10242,"ev":"gap","after":10241,"n":37,"reason":"queue_full"}
```

边界不确定时 `n` 写 `null`，**不合成一个不存在的区间**。关闭时另写一条 `{"ev":"stop"}`。

## 落盘判据

成功判据是 **AND 不是 OR**：TCP 要求 `up > 0 && down > 0`，UDP 只要求 `up > 0`。
用 `up + down > 0` 会把端口扫描和被 RST 的 ClientHello 逐条渲染成「成功访问」，
而那正是审计要排除的情形。

载体标记地址不写记录（真实目标在子流上）：`sp.mux.sing-box.arpa`、`sp.v2.udp-over-tcp.arpa`、
`sp.udp-over-tcp.arpa`、`sp.packet-addr.v2fly.arpa`。

## 运维

- **不要给该目录配 logrotate。** 文件由进程按 `max_bytes` 自轮转，并在每个 flush tick 用
  `os.SameFile` 比对 inode；外部轮转会与它抢同一个文件，产生难以归因的丢记录。
- **总量封顶时删最老的已轮转文件并写一条 gap；无对象可删时只累加计数、不写 gap**，
  以免自指 gap 把真事件放大。
- 启动 fail-hard、运行 fail-open：启动期权限或属主不符即启动失败；进入运行期后，
  审计的任何写入错误、队列满或 panic 都不会阻断转发或触发进程退出。

## 已知缺口

| 来源 | 性质 | 处置 |
| --- | --- | --- |
| 崩溃丢失缓冲区内的记录 | 不做 `fsync`，且该丢失**不产生 gap 行**，事后无法与「本就没有访问」区分 | 需要不丢即需另建 spool（当前决策为不做） |
| 审计文件与数据面同 uid | 数据面被非 root 攻破即可就地改写或删除自己的历史 | `0600` 可挡本机其他非 root 账号；**同 uid 改写无廉价缓解** |
| `host` 来自 sniff 时可能是伪装外层 | 启用 ECH 的客户端给出的是伪装 SNI，属**错误记录**而非缺失 | `host_src` 标明来源，下游按可信度分级 |
| 未启用 sniff 或客户端直接给 IP | 只能记 IP，无域名 | 正常降级，`host_src` 为 `ip` |
| UDP 只到 association 粒度 | 记首包目标，一条 QUIC session 内的多目标不可见 | 需要逐包目标即需另立设计 |
| 不经 router 的伪装中继（REALITY / ShadowTLS） | tracker 不可见 | 声明不支持 |
