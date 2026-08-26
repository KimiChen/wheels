# 用户统计接口 v1

## 配置

顶层 `user_stats` 只有在 Unix 上构建非默认 `user-stats` feature 时可用；配置存在但 feature
未编译，或在非 Unix 平台使用，都会明确失败。配置可 JSON round-trip，字段如下：

| 字段 | 必填/默认 | 有效范围 |
| --- | --- | --- |
| `node_id` | 必填 | 非空、最多 128 字节，且每个字节都是 ASCII 可显示非空白字符 |
| `socket_path` | 必填 | 带文件名的绝对 Unix socket 路径；不得与 manager socket 相同 |
| `socket_mode` | `"0600"` | 只接受精确的四字符字符串 `"0600"` 或 `"0660"`；`"600"`、`"660"` 等非规范形式失败 |
| `read_timeout_ms` | `1000` | `1..=60000` |
| `write_timeout_ms` | `1000` | `1..=60000`；HTTP 响应处理期限，包含快照构造、JSON 序列化与 socket 写入 |
| `max_request_bytes` | `1024` | `256..=65536`；原始 request-line、headers 与终止 CRLF 的总字节上限 |
| `max_response_bytes` | `1048576` | `1024..=16777216` |
| `max_identities` | `1000` | `1..=10000`，统计进程内逻辑用户记录（`server_id + name`）总数；逻辑 server ID 另有相同数值的独立上限 |
| `max_concurrent_clients` | `4` | `1..=32`；限制连接及其派生的后台快照工作 |

socket 路径必须已经是词法与文件系统意义上的规范绝对路径，不能含 `.`、`..` 或任何符号链接
祖先。每一级祖先都必须是真实目录，属主为 root 或服务最终 euid。直接父目录不得让
group/other 写入；更高层级只有 root 持有且带 sticky bit 的目录（例如规范化后的 `/tmp`）
可以让 group/other 写入。非 root 启动时，配置检查和最终 bind 前都会完成全部验证；root
启动器可能在配置检查后降权，因此配置阶段会暂缓祖先属主判断，并在最终 euid 的 bind 前完成
全量复查。`user_stats` 对未知字段使用 fail-closed 解析；任何拼写错误或未支持字段都会使
配置失败，不会静默回落到默认值。
启用统计时，每个 `servers[]` 必须有唯一合法 `id`，只能使用支持 EIH 的
`2022-blake3-aes-128-gcm` 或 `2022-blake3-aes-256-gcm`，并至少有一个用户；同一服务内
`users[].name` 和用户密码都必须唯一。非 EIH、空用户列表或仅主身份服务会失败关闭，不会生成
归属不完整的快照。`servers[].id` 和 `users[].name` 使用与 `node_id` 相同的 ASCII 可显示非空白字符
规则。当前五节点部署 profile 在上述兼容范围内进一步固定为
`2022-blake3-aes-128-gcm`，iPSK/uPSK 均为 16 字节安全随机值的标准 Base64；完整示例见
[`../config/server.example.json`](../config/server.example.json)。

启用 `user_stats` 时，`ConfigType::Manager` 的 builtin 和 standalone 模式都会在绑定 manager
socket 前被明确拒绝；运行时入口还有独立的失败关闭检查。因此统计模式不支持 manager 动态
`add`，只会统计经过完整配置校验的静态 `servers[]`。未配置 `user_stats` 时，上游 manager 行为不变。

## HTTP/1.1 over Unix stream

接口只监听配置的 Unix stream socket，不创建 TCP 监听。每条连接只接受一个 HTTP/1.1 请求，
返回一个响应后关闭连接；不支持 HTTP/1.0、keep-alive、pipeline、upgrade 或 `CONNECT`。请求
必须使用 origin-form 的精确路径并且恰好包含一个合法的 `Host` authority；`Host` 值不参与授权
或路由。不得带 query，也不得包含 `Content-Length`（即使值为 0）或 `Transfer-Encoding`，因此
按 HTTP framing 不存在请求 body。请求最多包含 64 个 headers；这是独立于可配置字节上限的
固定硬上限。
可用标准 curl 直接读取：

```bash
curl --fail-with-body --silent --show-error \
  --unix-socket /run/shadowsocks-rust-plus/user-stats.sock \
  http://localhost/v1/snapshot
```

固定路由如下：

| 请求 | 响应状态 | 语义 |
| --- | --- | --- |
| `GET /v1/snapshot` | `200 OK` | 生成并返回一个完整快照；请求被接受并调用 `snapshot()` 时立即推进 `sequence` |
| `GET /healthz` | `200 OK` 或 `503 Service Unavailable` | 健康时为 200，不健康时为 503；检查本身不推进 `sequence` |

其他路径返回 404，其他方法返回 405 并带 `Allow: GET`。带 query、声明请求体、绝对形式
URI、缺少/重复/非法 `Host` 及其他不符合契约的请求返回 400；HTTP/1.0 或其他版本在能够形成
应用响应时返回 505。原始 request-line、headers 与终止 CRLF 超过 `max_request_bytes`，或
header 数量超过 64 时返回 413。客户端必须验证 HTTP 状态、`Content-Length`、完整 JSON body、
`schema_version` 与所需字段，不能把错误或截断响应记账。

所有完整响应均使用 `Content-Type: application/json`、准确的 `Content-Length`、
`Cache-Control: no-store` 和 `Connection: close`；JSON body 以 LF 结尾。唯一例外是 HTTP 标准规定
HEAD 响应不得携带 wire body：`HEAD` 在这里仍返回 405 和 `Allow: GET`，`Content-Length` 表示
对应错误 representation 的长度，但连接上没有 JSON body。慢速或不完整 header、客户端提前
断开等情况下，连接可能直接关闭而没有完整 HTTP 错误响应。

preflight 已经读到的 header 终止符后附加字节会返回 400。若客户端在 exporter 开始响应后才发送
未由 `Content-Length`/`Transfer-Encoding` 声明的字节，这些字节按 HTTP framing 不属于当前
请求；exporter 会关闭连接并且永远不会把它们作为第二个请求处理。客户端不能依赖分包时序发送
body 或 pipeline。

Unix socket 是本机权限边界，不能把 `ssserver` exporter 直接暴露到公网。需要远程访问时，应由
节点上的独立 Nginx、Caddy 或同类反向代理以该 socket 为 HTTP upstream，并由代理提供 HTTPS、
mTLS、来源限制和审计。反向代理不得缓存快照，也不得开放其他代理能力；其 upstream 请求必须
保持 HTTP/1.1 origin-form、单个合法 Host，并且不能注入 `Content-Length: 0` 或
`Transfer-Encoding`。

## 成功响应

```json
{
  "schema_version": 1,
  "node_id": "node-example-01",
  "runtime_id": "0123456789abcdef0123456789abcdef",
  "started_at_unix_ms": 1787587200000,
  "sequence": 42,
  "health": {
    "counter_overflow": false,
    "sequence_overflow": false
  },
  "servers": [
    {
      "server_id": "ss-entry-01",
      "listen": "127.0.0.1:8388",
      "generation": 1,
      "active": true,
      "users": [
        {
          "identity_kind": "user",
          "name": "u_000123",
          "generation": 1,
          "active": true,
          "tcp_uplink_bytes": 1000,
          "tcp_downlink_bytes": 2000,
          "udp_uplink_bytes": 300,
          "udp_downlink_bytes": 400
        }
      ]
    }
  ]
}
```

服务按 `server_id`、再按服务 `generation` 排序；用户按 `name`、再按用户 `generation`
排序。所有计数字段都是 JSON 非负整数，范围为 `0..=u64::MAX`。响应不包含密码、密钥摘要、
目标或客户端地址。

同一运行周期内：

- `runtime_id` 和 `started_at_unix_ms` 固定；
- 每次接受 snapshot 请求并调用 `snapshot()` 时 `sequence` 严格增加；后续响应过大或传输失败也
  已消耗该序号，达到上限后饱和并报告不健康；
- 每个计数字段单调不减；
- 同一 `server_id` 重激活时复用服务 `generation=1`；同名用户重激活时也复用
  `generation=1` 和原累计计数器，只切换 `active`。未重新注册的旧用户保留为 inactive；
- 对象重激活会更新内部生命周期令牌，过期 server 句柄不能注册用户、获取活动计数器或改变
  当前生命周期；这类 registry 查询会在生命周期状态稳定期间完成。已经交给已建立 TCP/UDP
  会话的计数器仍可在排空期间累计到同一稳定计费身份，不会被强制撤销；
- 服务重启后生成新的 `runtime_id`，计数从零开始。

在同一 runtime 内，稳定的 `server_id` 和用户 `name` 是计费身份，不得重分配给另一个业务用户。
需要改变归属时应换用新名称，或完成计划重启并进入新 `runtime_id`。逻辑用户名与 server ID 分别受独立上限
约束；达到任一上限时新身份注册失败并记录错误，不会污染 registry 映射。

## 健康响应与错误响应

`GET /healthz` 不构造快照，也不推进 `sequence`。健康和不健康的固定 body 分别为：

```json
{"schema_version":1,"status":"ok"}
```

```json
{"schema_version":1,"status":"unhealthy"}
```

不健康由计数器或序号已经饱和触发；采集器仍可调用 `/v1/snapshot` 取得包含完整 `health` 字段的
快照用于诊断，但不得把不健康快照入账。

可安全回应时，错误统一为固定的小型 JSON，不回显 URI、header 或 body：

```json
{"schema_version":1,"error":{"code":"invalid_request"}}
```

客户端不得用 HEAD 探测错误 JSON；如上所述，HEAD 的 405 只返回 representation headers。

| HTTP 状态 | `error.code` | 场景 |
| --- | --- | --- |
| `400 Bad Request` | `invalid_request` | query、body framing、已缓冲尾随字节、绝对形式 URI、非法 Host 或请求格式 |
| `404 Not Found` | `not_found` | 未知路径 |
| `405 Method Not Allowed` | `method_not_allowed` | 非 GET 方法；响应包含 `Allow: GET` |
| `408 Request Timeout` | `read_timeout` | 在仍能安全形成 HTTP 响应时请求读取超时 |
| `413 Payload Too Large` | `request_too_large` | 请求元数据总字节数超过配置限制，或 header 数量超过 64 |
| `429 Too Many Requests` | `busy` | 并发客户端名额已满；空闲的有界 busy-response worker 可用时返回 |
| `500 Internal Server Error` | `response_too_large` | 快照 JSON body 超过 `max_response_bytes` |
| `500 Internal Server Error` | `internal` | 其他内部构造错误 |
| `505 HTTP Version Not Supported` | `http_version_not_supported` | HTTP/1.0 或其他不支持的版本 |

无法可靠确定响应边界、客户端断开或写超时时，服务可以直接关闭连接；这类连接以及任何非 200
快照响应都没有可结算的 `runtime_id + servers` 完整结构，采集器必须拒绝入账。一个已经被
接受的 `/v1/snapshot` 会先推进 `sequence`，因此响应过大、随后写失败或客户端断开可能在下一
份成功快照中表现为序号跳跃；采集器应记录并告警跳号，但不能把它当作可以推导缺失流量的依据。

快照构造和 JSON 序列化运行在阻塞任务中。该任务一旦开始就不能被连接超时强制终止；连接关闭
后，它仍持有原客户端的并发许可以及尚未释放的响应结果，直到任务真正完成。此时新连接若超过
`max_concurrent_clients` 会收到 429。该收紧保证后台 CPU、任务数和响应内存仍受同一并发上限
约束；不要通过降低 `write_timeout_ms` 或盲目提高并发数来规避持续 429。

429 写入不在 accept 循环中等待：独立 worker 最多 32 个，每个最长占用 100ms（若
`write_timeout_ms` 更短则使用较短值）。这 32 个 worker 也已满时，后续 busy 连接会立即关闭而不保证收到
429 body，以确保慢读客户端既不阻塞 accept 也不能创建无界任务。采集端必须将这类关闭与 429 都视为
可重试的资源繁忙，不得入账。

## 结算算法

仓库中的 `tests/settlement_model.py` 是契约测试模型。它对每份新序号快照先做完整验证，再原子
更新内存状态：同一 `(node_id, runtime_id)` 的 `started_at_unix_ms` 必须固定，未知
`identity_kind` 必须拒绝，且已在该 runtime 观察过的服务或身份 lineage 不得从后续完整快照
无故消失。任一违反项都不得推进序号、基线或批次去重状态。生产采集器还必须持久化：

1. 每个运行周期最后处理的 `sequence`；
2. 每个“服务代次 + 用户代次”最后接受的四向累计值；
3. 由节点、服务及其代次、身份及其代次、运行周期、序号和增量确定的唯一批次 ID。

服务基线键为
`node_id + server_id + server_generation + identity_name + identity_generation + runtime_id`。
当前 exporter 在同一 runtime 内对同名服务/用户复用 `generation=1` 和原计数器，因此重激活不创建新
lineage；`active=false` 的已观察 lineage 仍必须出现在后续快照中。完整 generation 键仍是 v1 合同的一部分，
用于严格区分 schema 定义的 lineage 并保留未来兼容性；控制面不得因当前实现固定为 1 而省略它们。

首次看到新 `runtime_id` 时必须显式选择：

- `baseline`：只建立基线，减少重复风险；
- `include`：把首次累计值全部作为新增，减少漏记风险。

只有已实现“停止接入、排空、最终快照、确认入账”的重启屏障时，控制面才能可靠地决定
如何切换策略。exporter 不隐式选择。
