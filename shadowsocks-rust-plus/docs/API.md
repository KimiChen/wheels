# 用户统计与用户成功访问审计接口 v1

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

## 用户成功访问审计

审计是独立于用户统计快照的节点侧协议。权威字段、错误语义和 golden vectors 见
[`USER_ACCESS_AUDIT.md`](USER_ACCESS_AUDIT.md)；本节给出实现和采集器需要稳定依赖的 wire 摘要。
审计只记录已经满足成功条件的访问，不记录失败认证、ACL/DNS/connect 失败、单向 TCP、UDP send
失败、连接时长、payload 或 transport peer。审计故障允许漏记，但不得阻断代理流量。

### Feature 与配置

根 crate 的非默认 `user-audit` feature 传播到 `shadowsocks-service/user-audit`，并依赖
`user-stats`。第一版只支持 Linux、静态 `servers[]` 和 EIH AEAD-2022 server；它与
built-in/standalone manager 互斥。未编译 feature 但配置存在 `user_audit` 时，配置加载必须明确
返回 unsupported-feature；feature 未启用或未配置时不得创建 audit queue、task、callback 或元数据。

ssserver 顶层配置在已有 `user_stats.node_id` 下增加以下严格字段；不得重复配置 node ID：

| 字段 | 默认值 | 有效范围 |
| --- | ---: | ---: |
| `ingest_socket_path` | 无 | 规范绝对路径；无 `.`、`..`、空组件或符号链接祖先 |
| `auditd_user` | 无 | 部署解析的专用账号，示例为 `shadowsocks-audit` |
| `queue_capacity` | `4096` | `256..=4096` |
| `max_udp_targets_per_association` | `256` | `1..=256` |
| `max_udp_target_windows` | `65536` | `16384..=65536`，必须能被 64 整除 |

UDP 去重窗口固定为 60 秒、固定 64 个 shard；这些值不出现在配置文件。任何范围和路径错误都在
配置加载及 service 运行入口各校验一次。auditd 使用 [`../config/auditd.example.json`](../config/auditd.example.json)
中的严格 schema；默认 spool 上限 5 GiB、最小可用空间 1 GiB、segment 上限 4 MiB、acked 保留
86400 秒，HMAC key 文件必须是 64 个小写 hex 字符（可带一个末尾 LF）并解码为 32 bytes。

### Event schema

所有 event/diagnostic 是 UTF-8、无重复 key、无 NaN/Infinity/尾随数据的 JSON object。可能达到
`u64` 的值使用无前导零十进制字符串；128-bit ID 使用 32 个小写 hex 字符。access event 的公共
字段为：

```text
schema_version=1, record_type=access,
event_type=tcp_target_success|udp_target_success,
event_id=runtime_id ":" audit_sequence,
audit_sequence, occurred_at_unix_ms, runtime_monotonic_ms,
node_id, runtime_id, server_id, server_generation=1,
identity_kind=user, identity_name, identity_generation=1,
transport, target, success_evidence
```

`identity_name` 只能来自已认证的 `ServerUser.name()`。`target` 保留地址头原始 `host`、规范化后的
`normalized_host`（失败时为 `null`）、端口和本次实际使用的 `remote_ip`；不记录 URL、DNS payload、
TLS 名称、客户端地址或任何 payload。域名规范化使用 UTS #46 non-transitional + STD3，ASCII
域名小写并移除一个末尾点；IP 使用 `IpAddr::to_string()`。

TCP 事件只在同一 relay 已成功向目标写入至少一个应用字节且目标已成功向客户端写回至少一个应用
字节时生成，单连接最多一条；TFO 与普通 connect 语义相同。`success_evidence` 固定为
`tcp_bidirectional_payload`。UDP 事件只在 outbound datagram 非空且完整 send 成功时生成，
`success_evidence` 固定为 `udp_send_ok`；这只证明本机内核接受发送，不证明远端收到。每个 association
使用随机 `association_id`，相同规范目标 60 秒内最多一条，缓存淘汰允许提前重复。

诊断不是访问事实，也使用相同通道：`producer_gap`（`queue_overflow`、`encode_error` 或
`permanent_nack`）、`udp_window_contention` 和 auditd 生成的 `spool_gap`。诊断不得包含 identity、
target 或把“可能漏记”冒充确定访问；variant 的必填/nullable/禁止字段必须逐项按规格校验。

### Ingest UDS

ssserver 到 auditd 使用 Linux `AF_UNIX` stream。每帧为 4-byte big-endian unsigned length 加 JSON
payload，request/response 共用 framing，单帧最多 8192 bytes；零长度、超长、partial frame 超时或
尾随数据直接断开。第一帧必须是 hello，最多 4 条连接但同一 daemon lifetime 只允许一个完成 hello
的 producer。hello、partial frame 和 response 各自受 2 秒截止；frame 边界 idle 不计超时。

hello 的 canonical object 为：

```json
{"protocol_version":1,"frame_type":"hello","node_id":"node-example-01","runtime_id":"0123456789abcdef0123456789abcdef"}
```

auditd 同时检查 `SO_PEERCRED.uid == producer_user`、node/runtime 与配置及格式匹配。成功返回
`hello_ack/ready`；第二个 producer 返回 retryable `producer_busy` 后断开，其他 hello 错误
（`unsupported_version`、`unauthorized_peer`、`invalid_hello`、`node_mismatch`）均不可重试地
保留 producer queue/in-flight。event 成功写入并完成该事件的 `fdatasync()` 后才返回 `ack/stored`，
ACK 包含 audit `event_id`、`spool_epoch` 和 `spool_sequence`。固定 NACK 为
`invalid_schema`、`event_id_conflict`、`runtime_mismatch`（永久）以及 `storage_unavailable`、
`internal_error`（可重试）。producer 必须保留原始 bytes 重放，未知/非法 ACK 不得生成 gap。

同一 auditd lifetime 最近 65536 个 event ID 做内存 LRU：相同 ID/相同 payload 返回原 ACK，
相同 ID/不同 payload 返回 conflict 并断开；重启或淘汰后允许重新写入。producer relay 只做有界
`try_lock`/`try_emit`，绝不等待 auditd、ACK、fsync 或 drain。

### Export HTTP/1.1 over UDS

auditd 默认在 `/run/shadowsocks-audit/export/export.sock` 只监听 `AF_UNIX`，每连接只处理一个
请求并返回 `Connection: close`。精确路由为：

| 请求 | 成功 | 语义 |
| --- | --- | --- |
| `GET /v1/audit/healthz` | `200` 或 `503` JSON | 完整 health，不泄露 identity/target |
| `POST /v1/audit/lease` | `200` NDJSON 或 `204` | 最老 sealed、未确认 batch；同一 leased batch 重试返回相同 bytes |
| `POST /v1/audit/ack` | `200` JSON | 按 batch ID 与 body digest 幂等确认 |

request-target 必须是精确 origin-form path（无 query、重定向、百分号变体或尾斜杠）；HTTP/1.1
必须有唯一合法 `Host`。两个 POST 的 body 分别是严格 canonical lease `{"schema_version":1}`
和字段顺序固定的 ACK object，无空白/BOM/LF。请求 header 总量最多 16 KiB/32 个，body 最多 4096
bytes，并拒绝 `Transfer-Encoding`、`Content-Encoding`、obs-fold、HTAB 和控制字符；export 最多
4 个并发连接，读写截止 5 秒。除 204 外响应都有准确 `Content-Length`、`Cache-Control: no-store`
和 `Content-Type`；204 明确省略 body/framing headers。

200 lease 必须携带 `X-Shadowsocks-Audit-Schema: 1`、node、batch、epoch、first/last sequence、
event count，以及 `X-Shadowsocks-Audit-Body-SHA256`；该 digest 等于实际 raw NDJSON body 和通用
`X-Shadowsocks-Audit-Response-SHA256`。collector 必须先校验响应 digest/MAC，再按 LF 分行校验
wrapper、连续 spool sequence、epoch 和 `event_payload_sha256`，保留 event raw JSON bytes，不得
普通 reserialize 改变 escaping。ACK 成功将 batch 原子移入 `acked/` 并保留 24 小时；未知、淘汰或
digest 不同分别返回 `404 unknown_batch`、`410 batch_evicted`、`409 digest_mismatch`。

### HMAC 与 health

三个 export API 都需要独立 node HMAC。请求 canonical UTF-8 bytes（末尾无 LF）为：

```text
SHADOWSOCKS-AUDIT-V1\n<METHOD>\n<exact-path>\n<node-id>\n<timestamp>\n<nonce>\n<body-sha256>
```

请求 header 必须包含 node、无前导零 unix timestamp、32 位 hex nonce、实际 body 的 64 位 digest 和
`Authorization: Shadowsocks-Audit-HMAC-SHA256 <64 lowercase hex>`。timestamp 偏差最多 300 秒，
nonce 在 10 分钟/4096 条 cache 内不可重放；只有 timestamp、body digest 和 constant-time HMAC
全部通过后才写入 cache。响应签名 canonical 以 `SHADOWSOCKS-AUDIT-RESPONSE-V1`、request nonce、
status、content type/schema/digest、node、batch/epoch/sequence/count 和 response body digest 逐行
组成，同样不带末尾 LF。无 key、错误 node、过期或重放请求不得回显输入。

health object 固定包含 `schema_version`、node、`ok|degraded`、producer 状态、runtime、ingest
时间、spool epoch/bytes/上限、sealed batch 数、最老未 ACK 时间、stored records、storage rejected
attempts 和 evicted unacked records；无 producer/无未 ACK batch 的可选值为 `null`。所有计数饱和为
`u64::MAX` 并置 degraded。已签名的 503 health 仍必须返回完整对象；其他错误使用固定 error code，
不得泄露请求正文。仓库 [`../tests/mock_collector.py`](../tests/mock_collector.py) 提供无第三方依赖的
lease/ACK 校验、幂等与冲突隔离参考实现。
