# API

两个本机 Unix domain socket，互不复用：

| 端点 | socket | 方法与路径 | 作用 |
| --- | --- | --- | --- |
| 快照导出 | `user_stats.listen_path` | `GET /v2/snapshot`、`GET /healthz` | 只读，采集与结算的唯一数据源 |
| 配额控制 | `user_stats.quota_control.listen_path` | `PUT /v2/quota` | 只写，下发全量剩余额度表 |

两者的权限档次不同：快照是只读旁观者，配额端点能改变转发行为。**不要把它们放在同一个 socket 上**
（配置校验会直接拒绝），也不要把任何一个直接映射为公网监听——远程读取须经节点上的独立反向代理
提供 HTTPS/mTLS/来源限制与审计，且该代理不得缓存快照。

## 传输层

HTTP/1.1-over-Unix-stream，每连接单请求单响应，禁 keep-alive 与 query。
快照 socket 另外禁请求体；配额 socket 允许且只允许一次性、长度已知的请求体（不支持分块）。

错误一律返回固定对象，并与快照共用同一个 schema 版本常量：

```json
{"schema_version": 2, "error": {"code": 404}}
```

| code | 触发条件 |
| --- | --- |
| 400 | 畸形请求、带 query、只读 socket 上带请求体、请求体 JSON 不合法或含未知字段、`entries[]` 含未知计费身份 |
| 404 | 未知路径。**`GET /v1/snapshot` 恒为 404**——路径版本号与 `schema_version` 同步推进，误配的采集器必须立即失败而不是读到半兼容的 body |
| 405 | 方法不符（快照只接受 GET，配额只接受 PUT） |
| 408 | 读取请求超时 |
| 409 | `node_id` / `runtime_id` 与当前进程不符，或 `epoch` 未前进 |
| 413 | 请求行、请求头或请求体超限 |
| 429 | 并发上限。采集端应视为**可重试且不得入账** |
| 500 | 服务端内部错误 |
| 505 | 非 `HTTP/1.1` |

## `GET /v2/snapshot`

被接受时推进 `sequence`。响应体以 LF 结尾。

```json
{
  "schema_version": 2,
  "node_id": "node-example-01",
  "runtime_id": "0123456789abcdef0123456789abcdef",
  "started_at_unix_ms": 1787587200000,
  "sequence": 42,
  "health": {
    "counter_overflow": false,
    "sequence_overflow": false,
    "identity_limit_reached": false
  },
  "inbounds": [
    {
      "tag": "vless-entry-01",
      "type": "vless",
      "listen": "0.0.0.0",
      "listen_port": 8443,
      "generation": 1,
      "active": true,
      "tcp_sessions": 3,
      "udp_sessions": 1,
      "users": [
        {
          "name": "u_example_01",
          "generation": 1,
          "active": true,
          "tcp_uplink_bytes": 0,
          "tcp_downlink_bytes": 0,
          "udp_uplink_bytes": 0,
          "udp_downlink_bytes": 0
        }
      ]
    }
  ]
}
```

采集端必须遵守的四条，缺一就会出现漏计或重复计费：

1. **u64 字段禁止经 IEEE754 double 解析**（四向计数、`sequence`、`started_at_unix_ms`、两个
   `generation`）。以 64 位整数或十进制字符串解析并做整数运算。
2. **`health` 是闭集**：恰好三个 bool 键，出现额外键即整份拒绝；任一为真即不可入账。
3. **`listen` 是配置值而非实际绑定结果**，且是纯 host——端口在独立的 `listen_port` 里。
   配置省略 `listen` 时输出 `127.0.0.1`（上游默认），不是 `0.0.0.0`。
4. **`tcp_sessions` / `udp_sessions` 是瞬时 gauge**，可减少，不参与结算、不进基线、
   不触发回退告警。它们的用途是计划重启前判断是否已排空。

基线键与差分规则见 README §5.1；参考实现见 `tests/settlement_model.py` 与
`tests/reference_collector.py`。

## `GET /healthz`

不带版本段，不推进 `sequence`。`health` 三位任一为真时返回 503：

```json
{"schema_version": 2, "status": "ok"}
```

## `PUT /v2/quota`

全量覆盖语义：**未出现在 `entries[]` 中的计费身份视为无限额度**。这不是「保持不变」——
下发的是一份完整的限额清单，不是增量。

```json
{
  "schema_version": 2,
  "node_id": "node-example-01",
  "runtime_id": "0123456789abcdef0123456789abcdef",
  "epoch": 137,
  "entries": [
    { "inbound_tag": "vless-entry-01", "name": "u_example_01", "remaining_bytes": 5368709120 }
  ]
}
```

- `remaining_bytes` 的口径固定为**四向之和**（`tcp_uplink + tcp_downlink + udp_uplink + udp_downlink`）。
  按方向计费请在 collector 侧折算。
- `epoch` 单调递增；`≤` 已接受值返回 409。
- `node_id` / `runtime_id` 必须逐字节相同，否则 409。**进程重启后的 409 是信号不是噪声**：
  额度是纯内存的，重启即全部解封，collector 必须按新 `runtime_id` 立即重推。
- 成功返回 `{"schema_version": 2, "epoch": …, "applied": …}`。

命令行：

```bash
scripts/quota-client.py --epoch 137 --entries quota.json
```

## 客户端

```bash
# 读一份快照并做 v2 契约校验（health 有位为真时退出码为 1）
scripts/user-stats-client.py --socket /run/sing-box-plus/user-stats.sock

# 采集一次并幂等落地本地账本
tests/reference_collector.py --socket /run/sing-box-plus/user-stats.sock \
  --ledger /var/lib/sing-box-plus/ledger --first-snapshot baseline
```
