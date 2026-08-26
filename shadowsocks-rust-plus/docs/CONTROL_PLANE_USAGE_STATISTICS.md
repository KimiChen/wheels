# 中控流量统计与 MySQL 数据设计

## 1. 目的与边界

本文设计一个以 Slim Framework 为 HTTP/API 与网页层、MySQL 5.7 为持久层的中控，用于汇总和
展示多个 `shadowsocks-rust-plus` 节点的用户级流量。设计参考 sub2api 的以下做法：

- 原始事实和 Dashboard 查询缓存分离；
- 事实写入使用确定性幂等键；
- 小时、日缓存按完整时间桶重新聚合并覆盖，不对旧缓存做 `+=`；
- 使用 watermark、回看/脏桶、范围重算和不同保留期；
- 聚合失败时不推进已完成水位。

sub2api 实际使用的是 `request_bytes` 和 `response_bytes` 两个客户端方向字段，并另有
`upstream_request_bytes`、`upstream_response_bytes` 两个上游方向字段。本文中的
Shadowsocks 链路没有第二段独立的“网关到上游 API”流量，因此不建立虚假的 `upstream_*`
字段，而是完整保留 TCP/UDP 四向数据。

本文只覆盖采集、结算、存储、聚合、查询、网页展示和运维接口，不负责：

- Shadowsocks 配置或密钥分发；
- 套餐、余额、支付和欠费停机；
- 云厂商/VPS 网卡计费对账；
- 生产节点部署和服务重启授权。

exporter 契约以 [API.md](API.md)、计数边界以 [ARCHITECTURE.md](ARCHITECTURE.md)、重启屏障
以 [OPERATIONS.md](OPERATIONS.md) 为准。本文不得改变这些上游数据面契约。

## 2. 统计口径

### 2.1 四向字段

`shadowsocks-rust-plus` exporter 输出进程运行周期内的四个累计 `u64`：

| 原始字段 | 方向 | 中控展示含义 |
| --- | --- | --- |
| `tcp_uplink_bytes` | 客户端经代理到目标的 TCP 应用负载 | TCP 上行 |
| `tcp_downlink_bytes` | 目标经代理到客户端的 TCP 应用负载 | TCP 下行 |
| `udp_uplink_bytes` | 客户端经代理到目标的 UDP 应用负载 | UDP 上行 |
| `udp_downlink_bytes` | 目标经代理到客户端的 UDP 应用负载 | UDP 下行 |

为兼容 sub2api 风格的 Dashboard 命名，派生字段定义为：

```text
request_bytes  = tcp_uplink_delta + udp_uplink_delta
response_bytes = tcp_downlink_delta + udp_downlink_delta
traffic_bytes  = request_bytes + response_bytes
```

四向字段是规范字段；`request_bytes`、`response_bytes`、`traffic_bytes` 只作为生成列或 API
别名，不能反过来替代四向事实。

四个原子计数器不是同一时刻的事务快照，只分别保证单调不减。结算器不能因为某轮的上下行比例、
TCP/UDP 比例或四项读取时刻略有差异而拒绝快照。

### 2.2 精确值和时间精度

这些字节是成功进入代理转发边界的解密后应用负载精确值，不包含 Shadowsocks/EIH、AEAD、
TCP、UDP、IP 头、隧道封装或 TCP 重传，也不等于主机 NIC 字节。事实表应固定记录：

```text
metric_scope          = ssserver_proxy_payload
traffic_estimated     = false
time_bucket_estimated = true
```

`time_bucket_estimated=true` 的原因是 exporter 只有累计快照，没有每次传输的事件时间。V1 将两次
成功快照之间的增量归到本次采集时间所在桶；总字节精确守恒，但流量实际发生时间最多偏移一个
采集周期。若采集周期为 30–60 秒，这个误差通常足够用于小时和日 Dashboard。

### 2.3 不可破坏的结算键

每条累计基线必须使用完整键：

```text
node_id
+ runtime_id
+ server_id
+ server_generation
+ identity_name
+ identity_generation
```

generation 字段属于协议键，不能因为当前值固定而从中控主键、cursor 或幂等哈希中省略。当前
exporter 在同一 `runtime_id` 内把 `server_id` 和用户 `name` 视为稳定逻辑身份：首次出现时输出
`generation=1`，移除时只把同一记录切换为 `active=false`，以后以同名重激活时继续复用该记录、
累计 counter 和 `generation=1`，并切回 `active=true`。已经输出过的 service/identity lineage 不会
从后续完整快照中消失；`active` 只表示当前生命周期状态，不表示可以删除、忽略或重置基线。

完整键仍保留两个 generation，以兼容既有 schema 以及未来可能引入新代次的 exporter。若未来
快照出现多个代次，`active=false` 的旧代次仍可能被未结束连接增加流量，中控必须按完整键继续
采集和结算。稳定的用户 `name` 也是计费身份的一部分，同一 runtime 内不得把它重新分配给另一
个业务用户；确需改变归属时必须使用新的 `name`，或启动新的 runtime 后按新映射建立基线。

## 3. 总体架构

exporter 只在节点本机 Unix stream socket 上提供严格 HTTP/1.1，中控不能直接通过公网访问它。
推荐的数据流为：

```text
┌────────────────────────── 节点 ──────────────────────────┐
│ ssserver                                                  │
│   └─ 本机 HTTP/1.1-over-Unix-stream exporter              │
│          └─ node collector agent                          │
│               ├─ 30–60 秒采集                             │
│               ├─ 本地 SQLite/outbox 断网暂存              │
│               └─ HTTPS + mTLS 上报                        │
└───────────────────────────┬───────────────────────────────┘
                            │
                            ▼
┌────────────────────────── 中控 ──────────────────────────┐
│ Slim ingest API                                           │
│   └─ 原始快照批次                                         │
│          └─ settlement CLI worker                         │
│               ├─ runtime/sequence/generation 校验         │
│               ├─ 累计游标求差                             │
│               └─ 不可变 usage ledger + 脏桶任务           │
│                         └─ rollup CLI worker               │
│                              ├─ hourly cache               │
│                              └─ daily cache                │
│ Slim dashboard API / HTML                                 │
└───────────────────────────────────────────────────────────┘
```

职责边界：

- **node collector agent**：通过 Unix socket 调用 `GET /healthz` 和 `GET /v1/snapshot`，严格检查
  HTTP 状态、响应长度、完整 JSON 和 health，采集后立即写本地 outbox；同一 runtime 严格按
  sequence 单路上传，前一批得到中控持久化确认后才发送下一批。只有中控确认幂等接收后才能
  清理本地记录。健康检查不推进 snapshot sequence。
- **Slim ingest API**：认证节点、限制大小、解析严格 envelope、计算 payload hash、幂等落原始批次，
  快速返回，不在 HTTP 请求内完成大范围聚合。
- **settlement worker**：按 `(node, runtime)` 串行化，原子验证整个快照，累计值求差并写入账本。
- **rollup worker**：领取脏桶，完整重算并覆盖小时/日缓存。
- **Slim dashboard**：鉴权、参数验证、查询缓存/事实表、渲染页面；不得在页面请求中 SSH 节点、
  轮询 Unix socket 或执行长时间聚合。

Slim 4 可用 route group middleware 分离 agent、普通用户和管理员权限，参见
[Slim Routing Middleware](https://www.slimframework.com/docs/v4/middleware/routing.html)。worker 应复用
同一领域服务代码，但从 CLI 命令启动，而不是伪装成内部 HTTP 请求。

优先让 node collector 主动通过 HTTPS+mTLS 向中控上报，避免增加节点入站面。如果确需由中控
远程轮询，节点上必须另设独立反向代理，以 exporter Unix socket 为 HTTP upstream，并在外侧
实施 HTTPS、mTLS、来源限制、速率限制与审计；不得让 `ssserver` exporter 监听 TCP，也不得用
无认证的字节转发把 socket 暴露到公网。代理必须禁止缓存 `/v1/snapshot`，且不得提供正向代理
或 manager 权限。

## 4. 时间与上报模型

每份上报同时保存三个时间：

| 字段 | 来源 | 用途 |
| --- | --- | --- |
| `collected_at` | node agent | 快照在节点上完成采集的时刻 |
| `received_at` | 中控 | 中控收到请求的可信时刻 |
| `accounting_at` | 中控规则生成 | 增量归属时间桶的时刻 |

节点应启用 NTP。`collected_at` 必须在同一 runtime 内单调不倒退，中控还应限制它与
`received_at` 的未来偏差。正常和断网补传使用合法的 `collected_at`，不合法时使用
`received_at` 并把 `time_quality` 标成 fallback。无论采用哪一个时间，都必须持久化选择结果，
避免重放同一批次时落入另一个桶。

数据库连接时区固定为 UTC：

```sql
SET time_zone = '+00:00';
```

- 小时桶使用 UTC 整点和半开区间 `[start, end)`。
- 日桶使用一个持久化的 Dashboard 时区，默认可设为 `Asia/Shanghai`。
- 日桶按该时区的日历日期计算，不能简单写成“前一日开始时间 + 24 小时”。
- 改变 Dashboard 时区必须完整重建 daily cache；API 必须返回当前时区。

## 5. 数据层次与表清单

| 层次 | 表 | 作用 |
| --- | --- | --- |
| 配置维度 | `ss_nodes` | 节点身份、采集策略、认证和状态 |
| 配置维度 | `ss_services` | 节点内稳定的逻辑 `server_id` |
| 配置维度 | `ss_users` | 中控业务用户，可为空以支持未映射身份 |
| 配置维度 | `ss_identity_routes` | 新身份名称到业务用户的当前映射规则 |
| 生命周期 | `ss_node_runtimes` | 进程运行周期、首次快照策略和最后 sequence |
| 生命周期 | `ss_runtime_services` | runtime 内的服务 generation |
| 生命周期 | `ss_runtime_identities` | 完整身份 generation，并冻结历史用户归属 |
| 接收审计 | `ss_snapshot_batches` | envelope、幂等、状态和错误 |
| 接收审计 | `ss_snapshot_payloads` | settlement 完成前必需、终态后可过期的压缩原始 JSON |
| 结算状态 | `ss_counter_cursors` | 每个完整基线键最后接受的四个绝对累计值 |
| 永久事实 | `ss_usage_ledger` | 只追加的四向增量账本 |
| 聚合控制 | `ss_rollup_jobs` | 可重复领取、带版本和租约的脏桶 |
| 聚合控制 | `ss_rollup_watermarks` | 缓存覆盖范围与新鲜度 |
| 查询缓存 | `ss_usage_dashboard_hourly` | 小时 × runtime identity 的叶子缓存 |
| 查询缓存 | `ss_usage_dashboard_daily` | 日 × 时区 × runtime identity 的叶子缓存 |

Dashboard 缓存保留到 `runtime_identity` 这一最细维度，然后通过冗余的 node/service/user 外键索引
做汇总。这样不需要同时维护 global、node、service、user 四套容易不一致的缓存。

## 6. MySQL 5.7 类型与兼容性约定

目标版本固定为 Oracle MySQL 5.7.44，不把 MariaDB 或更早的 5.7 patch release 视为自动兼容。
MySQL 5.7.44 是 5.7 系列最终版本，已进入 Sustaining Support；本方案可以在该版本实施，但部署
必须记录这一安全和维护风险，并保留升级到受支持版本的路线，参见
[MySQL 5.7.44 release note](https://dev.mysql.com/doc/relnotes/mysql/5.7/en/news-5-7-44.html)。

服务端和每个连接必须启用严格 SQL mode，至少包含：

```text
STRICT_TRANS_TABLES,ONLY_FULL_GROUP_BY,NO_ZERO_IN_DATE,NO_ZERO_DATE,
ERROR_FOR_DIVISION_BY_ZERO,NO_ENGINE_SUBSTITUTION
```

- 存储引擎统一为 InnoDB，字符集统一为 `utf8mb4`。
- exporter 的 ID/name 使用 `VARBINARY(128)` 保存已经校验的 ASCII 原始字节。MySQL 5.7 没有
  `utf8mb4_0900_bin`，而 `utf8mb4_bin` 字符比较仍可能忽略尾随空格，不能精确代替 Rust 字符串
  身份键。应用负责在写入前校验非空、最多 128 字节，且每个字节都是 ASCII 可显示非空白
  字符，读取后再解码为字符串。
  5.7 的二进制字符串比较语义参见
  [The BINARY and VARBINARY Types](https://dev.mysql.com/doc/refman/5.7/en/binary-varbinary.html)。
- 普通展示文字继续使用 `VARCHAR`；不要把展示列当 exporter identity 的唯一键。
- `runtime_id` 是 32 位十六进制字符串，校验后以 `BINARY(16)` 存储。
- 单节点绝对累计值、差值、sequence 和 generation 用 `BIGINT UNSIGNED`。
- 跨身份聚合可能超过单个 `u64`，缓存及生成合计用 `DECIMAL(39,0)`。
- `started_at_unix_ms` 原样使用 `DECIMAL(20,0)`；不要假设任意协议值都能转成 MySQL
  `DATETIME`。
- PHP 使用 `json_decode($json, true, 512, JSON_THROW_ON_ERROR | JSON_BIGINT_AS_STRING)`；该选项只会
  把超出 PHP 整数范围的值解成字符串，因此应用还要把较小的 int 和较大的 string 统一规范成
  十进制字符串，再校验 `0..=18446744073709551615`。求差使用数据库无符号运算或
  BCMath/Brick Math，禁止经过 float。
- API 中所有 byte、sequence、generation 和 revision 均返回 JSON 字符串，避免浏览器 JavaScript
  53 位整数上限。
- MySQL 5.7 会解析但忽略 `CHECK` 约束，因此本文 DDL 不使用 `CHECK` 制造虚假的安全感。poll
  范围、正 sequence/generation、schema 版本、严格布尔和时间区间等约束必须在领域服务验证，
  并由 migration 创建 `BEFORE INSERT/UPDATE` trigger 以 `SIGNAL SQLSTATE '45000'` 做数据库兜底。
- MySQL 5.7 支持本文使用的 `STORED` generated columns，参见
  [CREATE TABLE and Generated Columns](https://dev.mysql.com/doc/refman/5.7/en/create-table-generated-columns.html)。

## 7. 参考 DDL

以下为逻辑完整的起始结构。正式实施时应拆为有顺序、可回滚的 migration，并根据应用的认证和
用户模型补充外键删除策略。

### 7.1 节点、服务和业务用户

```sql
CREATE TABLE ss_users (
    id              BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    public_id       BINARY(16) NOT NULL,
    display_name    VARCHAR(128) NOT NULL,
    status          TINYINT UNSIGNED NOT NULL DEFAULT 1,
    created_at      DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at      DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)
                                  ON UPDATE CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    UNIQUE KEY uq_ss_users_public_id (public_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE ss_nodes (
    id                       BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    public_id                BINARY(16) NOT NULL,
    exporter_node_id         VARBINARY(128) NOT NULL,
    display_name             VARCHAR(128) NOT NULL,
    enabled                  TINYINT(1) NOT NULL DEFAULT 1,
    poll_interval_sec        INT UNSIGNED NOT NULL DEFAULT 60,
    initial_counter_policy   ENUM('baseline', 'include') NOT NULL DEFAULT 'baseline',
    mtls_subject             VARCHAR(255) NULL,
    agent_key_hash           VARBINARY(64) NULL,
    last_seen_at             DATETIME(6) NULL,
    created_at               DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at               DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)
                                           ON UPDATE CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    UNIQUE KEY uq_ss_nodes_public_id (public_id),
    UNIQUE KEY uq_ss_nodes_exporter_id (exporter_node_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE ss_services (
    id                  BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    node_id             BIGINT UNSIGNED NOT NULL,
    exporter_server_id  VARBINARY(128) NOT NULL,
    display_name        VARCHAR(128) NULL,
    created_at          DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    UNIQUE KEY uq_ss_services_exporter (node_id, exporter_server_id),
    CONSTRAINT fk_ss_services_node
        FOREIGN KEY (node_id) REFERENCES ss_nodes(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE ss_identity_routes (
    service_id          BIGINT UNSIGNED NOT NULL,
    identity_name       VARBINARY(128) NOT NULL,
    user_id             BIGINT UNSIGNED NOT NULL,
    mapping_version     BIGINT UNSIGNED NOT NULL DEFAULT 1,
    updated_at          DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)
                                          ON UPDATE CURRENT_TIMESTAMP(6),
    PRIMARY KEY (service_id, identity_name),
    KEY idx_ss_identity_routes_user (user_id),
    CONSTRAINT fk_ss_identity_routes_service
        FOREIGN KEY (service_id) REFERENCES ss_services(id),
    CONSTRAINT fk_ss_identity_routes_user
        FOREIGN KEY (user_id) REFERENCES ss_users(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

`ss_identity_routes` 只决定以后首次观察到的身份归属。身份进入
`ss_runtime_identities` 后，`user_id` 和 `mapping_version` 被历史冻结；修改 route 不能篡改旧账，
也不能改变同一 runtime 内同名身份重激活后的归属。稳定 `identity_name` 不得转给另一个计费
用户；改变归属必须使用新的名称，或切换到新的 runtime 后再应用新 route。

这是生产持久化层的验收约束，而不只是操作约定：settlement 事务必须拒绝修改或清空既有
`ss_runtime_identities.user_id`/`mapping_version`，migration 还应以 `BEFORE UPDATE` trigger
或等价的数据库写权限约束兜底。即使当前 route 已改变，既有 runtime identity 也只能更新
`active`、`last_sequence` 等生命周期字段；禁止删除后按同名重新插入以绕过冻结。

### 7.2 runtime、服务代次和身份代次

```sql
CREATE TABLE ss_node_runtimes (
    id                       BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    node_id                  BIGINT UNSIGNED NOT NULL,
    runtime_token            BINARY(16) NOT NULL,
    started_at_unix_ms       DECIMAL(20,0) NOT NULL,
    initial_counter_policy   ENUM('baseline', 'include') NOT NULL,
    policy_reason            VARCHAR(255) NULL,
    last_applied_sequence    BIGINT UNSIGNED NULL,
    first_seen_at            DATETIME(6) NOT NULL,
    last_seen_at             DATETIME(6) NOT NULL,
    last_collected_at        DATETIME(6) NULL,
    status                   ENUM('active', 'closed', 'unclosed', 'conflict')
                                 NOT NULL DEFAULT 'active',
    tail_loss_possible       TINYINT(1) NOT NULL DEFAULT 0,
    PRIMARY KEY (id),
    UNIQUE KEY uq_ss_node_runtimes (node_id, runtime_token),
    CONSTRAINT fk_ss_node_runtimes_node
        FOREIGN KEY (node_id) REFERENCES ss_nodes(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE ss_runtime_services (
    id                   BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    runtime_id           BIGINT UNSIGNED NOT NULL,
    service_id           BIGINT UNSIGNED NOT NULL,
    server_generation    BIGINT UNSIGNED NOT NULL,
    listen_address       VARCHAR(255) NULL,
    active               TINYINT(1) NOT NULL,
    first_sequence       BIGINT UNSIGNED NOT NULL,
    last_sequence        BIGINT UNSIGNED NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uq_ss_runtime_services
        (runtime_id, service_id, server_generation),
    CONSTRAINT fk_ss_runtime_services_runtime
        FOREIGN KEY (runtime_id) REFERENCES ss_node_runtimes(id),
    CONSTRAINT fk_ss_runtime_services_service
        FOREIGN KEY (service_id) REFERENCES ss_services(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE ss_runtime_identities (
    id                       BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    runtime_service_id       BIGINT UNSIGNED NOT NULL,
    identity_name            VARBINARY(128) NOT NULL,
    identity_generation      BIGINT UNSIGNED NOT NULL,
    user_id                  BIGINT UNSIGNED NULL,
    mapping_version          BIGINT UNSIGNED NULL,
    active                   TINYINT(1) NOT NULL,
    first_sequence           BIGINT UNSIGNED NOT NULL,
    last_sequence            BIGINT UNSIGNED NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uq_ss_runtime_identities
        (runtime_service_id, identity_name, identity_generation),
    KEY idx_ss_runtime_identities_user (user_id),
    CONSTRAINT fk_ss_runtime_identities_service
        FOREIGN KEY (runtime_service_id) REFERENCES ss_runtime_services(id),
    CONSTRAINT fk_ss_runtime_identities_user
        FOREIGN KEY (user_id) REFERENCES ss_users(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

上述 generation 列是完整协议键的一部分，不代表当前 exporter 会在同名重建时自动递增。当前
实现中，同一 runtime 的逻辑 `server_id` 与用户 `identity_name` 均复用 `generation=1` 和既有累计
counter；移除、重激活只更新 `active`，相应 runtime service/identity 行不得删除或另建同名代次。
中控仍须接受并正确隔离未来协议版本可能产生的不同 generation。

### 7.3 原始快照批次

```sql
CREATE TABLE ss_snapshot_batches (
    id                   BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    node_id              BIGINT UNSIGNED NOT NULL,
    runtime_id           BIGINT UNSIGNED NOT NULL,
    sequence             BIGINT UNSIGNED NOT NULL,
    schema_version       SMALLINT UNSIGNED NOT NULL,
    collected_at         DATETIME(6) NOT NULL,
    received_at          DATETIME(6) NOT NULL,
    accounting_at        DATETIME(6) NOT NULL,
    time_quality         ENUM('collector', 'received_fallback') NOT NULL,
    payload_sha256       BINARY(32) NOT NULL,
    identity_count       INT UNSIGNED NOT NULL,
    counter_overflow     TINYINT(1) NOT NULL,
    sequence_overflow    TINYINT(1) NOT NULL,
    status               ENUM(
                             'received', 'applied', 'superseded',
                             'rejected', 'conflict'
                         ) NOT NULL DEFAULT 'received',
    error_code           VARCHAR(64) NULL,
    applied_at           DATETIME(6) NULL,
    created_at           DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    UNIQUE KEY uq_ss_snapshot_runtime_sequence (runtime_id, sequence),
    KEY idx_ss_snapshot_runtime_status_sequence (runtime_id, status, sequence),
    KEY idx_ss_snapshot_node_received (node_id, received_at),
    KEY idx_ss_snapshot_status_created (status, created_at),
    CONSTRAINT fk_ss_snapshot_node
        FOREIGN KEY (node_id) REFERENCES ss_nodes(id),
    CONSTRAINT fk_ss_snapshot_runtime
        FOREIGN KEY (runtime_id) REFERENCES ss_node_runtimes(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE ss_snapshot_payloads (
    batch_id           BIGINT UNSIGNED NOT NULL,
    compression        ENUM('zstd') NOT NULL,
    original_bytes     INT UNSIGNED NOT NULL,
    stored_bytes       INT UNSIGNED NOT NULL,
    payload_blob       LONGBLOB NOT NULL,
    expires_at         DATETIME(6) NOT NULL,
    PRIMARY KEY (batch_id),
    KEY idx_ss_snapshot_payloads_expiry (expires_at),
    CONSTRAINT fk_ss_snapshot_payloads_batch
        FOREIGN KEY (batch_id) REFERENCES ss_snapshot_batches(id)
        ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

同一 `(runtime_id, sequence)` 的处理结果必须为：

- payload SHA-256 相同：返回已有批次状态，幂等成功；
- payload SHA-256 不同：把事件记为高优先级 `conflict`，返回 HTTP 409，绝不能覆盖旧批次；
- sequence 小于 runtime 已结算 sequence 且此前不存在：记为 `superseded`，不能再次求差；
- sequence 跳号：允许，较新累计值已经包含漏采区间的总量。

冲突本身可写入单独的安全审计日志；不能为了记录第二个 payload 而破坏上面的唯一键。

### 7.4 绝对计数游标和不可变增量账本

```sql
CREATE TABLE ss_counter_cursors (
    runtime_identity_id   BIGINT UNSIGNED NOT NULL,
    last_batch_id         BIGINT UNSIGNED NOT NULL,
    last_sequence         BIGINT UNSIGNED NOT NULL,
    last_collected_at     DATETIME(6) NOT NULL,
    tcp_uplink_bytes      BIGINT UNSIGNED NOT NULL,
    tcp_downlink_bytes    BIGINT UNSIGNED NOT NULL,
    udp_uplink_bytes      BIGINT UNSIGNED NOT NULL,
    udp_downlink_bytes    BIGINT UNSIGNED NOT NULL,
    baseline_batch_id     BIGINT UNSIGNED NOT NULL,
    baseline_policy       ENUM('baseline', 'include') NOT NULL,
    updated_at            DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)
                                          ON UPDATE CURRENT_TIMESTAMP(6),
    PRIMARY KEY (runtime_identity_id),
    CONSTRAINT fk_ss_counter_cursors_identity
        FOREIGN KEY (runtime_identity_id) REFERENCES ss_runtime_identities(id),
    CONSTRAINT fk_ss_counter_cursors_last_batch
        FOREIGN KEY (last_batch_id) REFERENCES ss_snapshot_batches(id),
    CONSTRAINT fk_ss_counter_cursors_baseline_batch
        FOREIGN KEY (baseline_batch_id) REFERENCES ss_snapshot_batches(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE ss_usage_ledger (
    id                     BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    batch_id               BIGINT UNSIGNED NOT NULL,
    runtime_identity_id    BIGINT UNSIGNED NOT NULL,
    source_event_hash      BINARY(32) NOT NULL,
    node_id                BIGINT UNSIGNED NOT NULL,
    service_id             BIGINT UNSIGNED NOT NULL,
    user_id                BIGINT UNSIGNED NULL,
    period_start           DATETIME(6) NOT NULL,
    period_end             DATETIME(6) NOT NULL,
    accounting_at          DATETIME(6) NOT NULL,
    previous_sequence      BIGINT UNSIGNED NULL,
    current_sequence       BIGINT UNSIGNED NOT NULL,
    tcp_uplink_bytes       BIGINT UNSIGNED NOT NULL,
    tcp_downlink_bytes     BIGINT UNSIGNED NOT NULL,
    udp_uplink_bytes       BIGINT UNSIGNED NOT NULL,
    udp_downlink_bytes     BIGINT UNSIGNED NOT NULL,
    request_bytes          DECIMAL(39,0)
        GENERATED ALWAYS AS (
            CAST(tcp_uplink_bytes AS DECIMAL(39,0)) +
            CAST(udp_uplink_bytes AS DECIMAL(39,0))
        ) STORED,
    response_bytes         DECIMAL(39,0)
        GENERATED ALWAYS AS (
            CAST(tcp_downlink_bytes AS DECIMAL(39,0)) +
            CAST(udp_downlink_bytes AS DECIMAL(39,0))
        ) STORED,
    traffic_bytes          DECIMAL(39,0)
        GENERATED ALWAYS AS (
            CAST(tcp_uplink_bytes AS DECIMAL(39,0)) +
            CAST(tcp_downlink_bytes AS DECIMAL(39,0)) +
            CAST(udp_uplink_bytes AS DECIMAL(39,0)) +
            CAST(udp_downlink_bytes AS DECIMAL(39,0))
        ) STORED,
    metric_scope           VARCHAR(32) NOT NULL DEFAULT 'ssserver_proxy_payload',
    traffic_estimated      TINYINT(1) NOT NULL DEFAULT 0,
    time_bucket_estimated  TINYINT(1) NOT NULL DEFAULT 1,
    created_at             DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    UNIQUE KEY uq_ss_usage_ledger_source (batch_id, runtime_identity_id),
    UNIQUE KEY uq_ss_usage_ledger_event_hash (source_event_hash),
    KEY idx_ss_usage_ledger_accounting (accounting_at),
    KEY idx_ss_usage_ledger_node_time (node_id, accounting_at),
    KEY idx_ss_usage_ledger_service_time (service_id, accounting_at),
    KEY idx_ss_usage_ledger_user_time (user_id, accounting_at),
    KEY idx_ss_usage_ledger_identity_time (runtime_identity_id, accounting_at),
    CONSTRAINT fk_ss_usage_ledger_batch
        FOREIGN KEY (batch_id) REFERENCES ss_snapshot_batches(id),
    CONSTRAINT fk_ss_usage_ledger_identity
        FOREIGN KEY (runtime_identity_id) REFERENCES ss_runtime_identities(id),
    CONSTRAINT fk_ss_usage_ledger_node
        FOREIGN KEY (node_id) REFERENCES ss_nodes(id),
    CONSTRAINT fk_ss_usage_ledger_service
        FOREIGN KEY (service_id) REFERENCES ss_services(id),
    CONSTRAINT fk_ss_usage_ledger_user
        FOREIGN KEY (user_id) REFERENCES ss_users(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

账本只插入至少一个 delta 大于零的行；零增量快照仍要更新所有已观察身份的 cursor、active 和
runtime `last_applied_sequence`。账本插入后不可更新或删除。人工冲正应另建
`ss_usage_adjustments`，使用有符号 `DECIMAL(39,0)`、关联原账本、操作人、原因和审批记录，
rollup 时通过 `UNION ALL` 纳入，不能直接改原事实。

### 7.5 小时和日缓存

```sql
CREATE TABLE ss_usage_dashboard_hourly (
    bucket_start           DATETIME(0) NOT NULL,
    runtime_identity_id    BIGINT UNSIGNED NOT NULL,
    node_id                BIGINT UNSIGNED NOT NULL,
    service_id             BIGINT UNSIGNED NOT NULL,
    user_id                BIGINT UNSIGNED NULL,
    tcp_uplink_bytes       DECIMAL(39,0) NOT NULL,
    tcp_downlink_bytes     DECIMAL(39,0) NOT NULL,
    udp_uplink_bytes       DECIMAL(39,0) NOT NULL,
    udp_downlink_bytes     DECIMAL(39,0) NOT NULL,
    request_bytes          DECIMAL(39,0)
        GENERATED ALWAYS AS (tcp_uplink_bytes + udp_uplink_bytes) STORED,
    response_bytes         DECIMAL(39,0)
        GENERATED ALWAYS AS (tcp_downlink_bytes + udp_downlink_bytes) STORED,
    traffic_bytes          DECIMAL(39,0)
        GENERATED ALWAYS AS (
            tcp_uplink_bytes + tcp_downlink_bytes +
            udp_uplink_bytes + udp_downlink_bytes
        ) STORED,
    ledger_rows            BIGINT UNSIGNED NOT NULL,
    revision               BIGINT UNSIGNED NOT NULL,
    computed_at            DATETIME(6) NOT NULL,
    PRIMARY KEY (bucket_start, runtime_identity_id),
    KEY idx_ss_usage_hourly_node (node_id, bucket_start),
    KEY idx_ss_usage_hourly_service (service_id, bucket_start),
    KEY idx_ss_usage_hourly_user (user_id, bucket_start)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE ss_usage_dashboard_daily (
    bucket_date            DATE NOT NULL,
    timezone_name          VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    runtime_identity_id    BIGINT UNSIGNED NOT NULL,
    node_id                BIGINT UNSIGNED NOT NULL,
    service_id             BIGINT UNSIGNED NOT NULL,
    user_id                BIGINT UNSIGNED NULL,
    tcp_uplink_bytes       DECIMAL(39,0) NOT NULL,
    tcp_downlink_bytes     DECIMAL(39,0) NOT NULL,
    udp_uplink_bytes       DECIMAL(39,0) NOT NULL,
    udp_downlink_bytes     DECIMAL(39,0) NOT NULL,
    request_bytes          DECIMAL(39,0)
        GENERATED ALWAYS AS (tcp_uplink_bytes + udp_uplink_bytes) STORED,
    response_bytes         DECIMAL(39,0)
        GENERATED ALWAYS AS (tcp_downlink_bytes + udp_downlink_bytes) STORED,
    traffic_bytes          DECIMAL(39,0)
        GENERATED ALWAYS AS (
            tcp_uplink_bytes + tcp_downlink_bytes +
            udp_uplink_bytes + udp_downlink_bytes
        ) STORED,
    hourly_rows            BIGINT UNSIGNED NOT NULL,
    revision               BIGINT UNSIGNED NOT NULL,
    computed_at            DATETIME(6) NOT NULL,
    PRIMARY KEY (bucket_date, timezone_name, runtime_identity_id),
    KEY idx_ss_usage_daily_node (node_id, bucket_date),
    KEY idx_ss_usage_daily_service (service_id, bucket_date),
    KEY idx_ss_usage_daily_user (user_id, bucket_date)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

小时缓存从 immutable ledger 汇总。日缓存默认从已经落地的小时缓存汇总；每次小时桶成功替换后
都要在同一事务中把对应日标脏。即使多个小时和日 worker 发生竞态，脏桶 version 也必须保证
最终再重算一次。若不能实现这个依赖约束，日缓存应直接从 ledger 重算，不能容忍静默缺桶。

若页面需要“活跃用户数”，只有桶内 `traffic_bytes > 0` 的非空 `user_id` 才算活跃。跨小时的
日活不能把小时活跃数相加；应对 leaf cache 做 `COUNT(DISTINCT user_id)`，或像 sub2api 一样建立
`(bucket, node_id, service_id, user_id)` 去重表。

### 7.6 脏桶任务和水位

```sql
CREATE TABLE ss_rollup_jobs (
    grain                 ENUM('hour', 'day') NOT NULL,
    bucket_start_utc      DATETIME(0) NOT NULL,
    timezone_name         VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    version               BIGINT UNSIGNED NOT NULL DEFAULT 1,
    status                ENUM('pending', 'leased', 'done', 'failed')
                              NOT NULL DEFAULT 'pending',
    requested_at          DATETIME(6) NOT NULL,
    lease_owner           VARCHAR(128) NULL,
    lease_token           BINARY(16) NULL,
    lease_until           DATETIME(6) NULL,
    attempts              INT UNSIGNED NOT NULL DEFAULT 0,
    last_error_code       VARCHAR(64) NULL,
    completed_at          DATETIME(6) NULL,
    PRIMARY KEY (grain, bucket_start_utc, timezone_name),
    UNIQUE KEY uq_ss_rollup_jobs_lease_token (lease_token),
    KEY idx_ss_rollup_jobs_claim (status, lease_until, requested_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE ss_rollup_watermarks (
    grain                 ENUM('hour', 'day') NOT NULL,
    timezone_name         VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    completed_through     DATETIME(6) NULL,
    finalized_through     DATETIME(6) NULL,
    allowed_lateness_sec  INT UNSIGNED NOT NULL,
    pending_jobs          BIGINT UNSIGNED NOT NULL DEFAULT 0,
    updated_at            DATETIME(6) NOT NULL,
    PRIMARY KEY (grain, timezone_name)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

小时任务的 `timezone_name` 固定为 `UTC`。日任务使用配置的 Dashboard 时区，并以该本地日历日
起点所对应的 UTC 时刻作为 `bucket_start_utc`。

### 7.7 MySQL 5.7 校验 trigger

由于 5.7 不执行 `CHECK`，migration 至少要为下列规则同时创建 `BEFORE INSERT` 和适用的
`BEFORE UPDATE` trigger：

| 表 | 数据库兜底规则 |
| --- | --- |
| `ss_nodes` | `poll_interval_sec BETWEEN 5 AND 3600` |
| `ss_runtime_services` | generation、first/last sequence 均大于 0，且 last 不小于 first |
| `ss_runtime_identities` | generation、first/last sequence 均大于 0，且 last 不小于 first；完整身份键、`user_id` 与 `mapping_version` 首次写入后不可改变，既有行不可用删除重建绕过冻结 |
| `ss_snapshot_batches` | schema 为 1、sequence 大于 0、health 字段只能为 0/1；envelope 字段不可更新 |
| `ss_counter_cursors` | last sequence 大于 0；四向绝对累计只能由 settlement repository 更新 |
| `ss_usage_ledger` | `period_end >= period_start`、current sequence 大于 0、固定 metric flags；事实行不可更新 |
| `ss_rollup_jobs` | leased 状态必须同时具有 owner/token/until，done 状态不能保留过期 lease |

单个 trigger 的写法示例：

```sql
DELIMITER //

CREATE TRIGGER bi_ss_snapshot_batches_validate
BEFORE INSERT ON ss_snapshot_batches
FOR EACH ROW
BEGIN
    IF NEW.schema_version <> 1
       OR NEW.sequence = 0
       OR NEW.counter_overflow NOT IN (0, 1)
       OR NEW.sequence_overflow NOT IN (0, 1) THEN
        SIGNAL SQLSTATE '45000'
            SET MESSAGE_TEXT = 'invalid snapshot batch envelope';
    END IF;
END//

DELIMITER ;
```

trigger 是防止旁路写入破坏不变量的第二道防线，不能替代 PHP 对整份快照的原子验证。migration
测试必须故意插入每种非法值，证明目标 MySQL 5.7 实例确实拒绝，而不是只检查 DDL 成功执行。

## 8. 快照接收与结算事务

### 8.1 ingest API

请求流程：

1. mTLS subject 或 bearer credential 定位唯一 `ss_nodes` 行。
2. 限制压缩前后 body 大小，拒绝未知 content type。
3. 使用严格 JSON 解码检查 `schema_version=1`、类型、必填字段、所有 `u64`、重复服务/身份键；
   `identity_kind` 只接受已定义的 `user`，未知值必须整份拒绝。
4. agent 绑定节点必须与 payload `node_id` 完全一致。
5. `health.counter_overflow` 和 `health.sequence_overflow` 必须存在且均为 `false`。
6. 对解压后的原始请求字节计算 payload SHA-256；不要重新编码 PHP 对象后再计算，因为对象属性
   顺序和数字表示可能改变。
7. 创建或查找 runtime，确认同一 runtime 的 `started_at_unix_ms` 固定。
8. 插入 `ss_snapshot_batches`；相同 hash 返回已有结果，不同 hash 返回冲突。
9. 在同一事务中保存 zstd 压缩 payload，提交后返回。异步 settlement 必须能从数据库恢复完整快照，
   因此 `received` 批次的 payload 不是可选审计附件。

只要 envelope 已足以识别 node/runtime/sequence，health 不健康等可审计拒绝也应保留 batch receipt，
状态设为 `rejected` 且绝不推进 sequence/cursor；完全无法识别 envelope 的畸形请求只进入限量安全
日志。

建议响应：

- `202 Accepted`：已接收，等待结算；
- `200 OK`：同 payload 已存在，返回原批次状态；
- `409 Conflict`：同 runtime/sequence 但 hash 不同；
- `422 Unprocessable Content`：完整 envelope、health 或数值不合法；
- `413 Content Too Large`：超过上限；
- `401/403`：认证失败或节点不匹配。

### 8.2 settlement 原子规则

同一 `(node_id, runtime_id)` 的批次必须串行结算。推荐在 MySQL 事务中按以下顺序执行：

1. `SELECT ss_node_runtimes ... FOR UPDATE`，再锁定 batch。
2. 不直接按外部传入的 batch ID 结算；持有 runtime 锁后，使用
   `(runtime_id, status, sequence)` 索引选择最小的 `received` sequence。batch 已 `applied` 时幂等
   返回；较旧且此前未处理的 sequence 标成 `superseded`。
3. sequence 必须大于 runtime 最后已接受值；允许真实采集丢失造成跳号。agent 的单路顺序上传和
   中控的最小 pending 选择共同保证 `baseline` 不会错误落在一个已排队的较新快照上。
4. 创建/读取快照中的 service generation 和 identity generation；未知 `identity_kind` 或同一快照
   内重复完整键时整份拒绝。成功响应是 runtime 的完整快照，已在该 runtime 观察过的
   service/identity lineage 若无故消失，也要整份拒绝，不能把“缺失”解释成零增量或删除；
   lineage 的 `active` 可以在快照间切换，但切换不创建新基线或重置累计值。
5. 按 `runtime_identity_id` 排序后读取 cursor，避免不同 worker 锁顺序产生死锁。
6. 对已有 cursor 逐项检查 `current >= previous`。任何一项下降都回滚整份快照，不能局部入账，也
   不能擅自把它解释成重启；合法重启必须更换 `runtime_id`。
7. 新 cursor 采用 runtime 创建时已持久化的策略：
   - `baseline`：首次 delta 为 0；
   - `include`：首次 delta 等于当前累计值。
8. 先在内存/临时结构中完成全快照验证，再插入非零 ledger delta。每条账本同时计算版本化
   `source_event_hash`：对 node/runtime、完整 service/identity lineage、sequence 和四向 delta 做
   长度明确的 canonical 编码后取 SHA-256，不能用容易产生分隔符歧义的字符串拼接。
9. 更新所有 cursor，包括零增量和 `active=false` 身份。
10. 更新 runtime last sequence、batch 状态，并 upsert 对应小时脏桶：已存在时
    `version=version+1, status='pending'`。
11. 全部成功后一次提交。

不得使用 `REPLACE INTO`，因为它是删除后插入，会破坏审计和引用语义。账本幂等由
`UNIQUE(batch_id, runtime_identity_id)` 保证；缓存正确性不能依赖“通常不会重复”。

新的 runtime 默认建议使用 `baseline`。只有控制面明确执行“停止新接入 → 排空 → 最终健康快照
→ 确认入账 → 停止旧进程 → 启动新 runtime”的屏障，并能证明没有与上一数据源重复时，才可
选择 `include`。策略、原因、操作者必须持久化。

异常退出的旧 runtime 标为 `unclosed` 且 `tail_loss_possible=1`。绝不能用新 runtime 的累计值减
旧 runtime cursor。agent 本地延迟上传的旧 runtime 快照可以按其原 runtime 独立结算，但采集时刻
必须早于已知切换边界。

## 9. 聚合算法

### 9.1 领取任务

MySQL 5.7 没有 `SKIP LOCKED`。多个 worker 使用单表 `UPDATE ... ORDER BY ... LIMIT 1` 和唯一
lease token 原子领取一个任务，不能先无锁 `SELECT` 再无条件 `UPDATE`：

```sql
START TRANSACTION;

UPDATE ss_rollup_jobs
SET status      = 'leased',
    lease_owner = :worker_id,
    lease_token = :new_random_binary_16,
    lease_until = :lease_until_utc,
    attempts    = attempts + 1
WHERE (
        status IN ('pending', 'failed')
        AND requested_at <= UTC_TIMESTAMP(6)
      )
   OR (
        status = 'leased'
        AND lease_until < UTC_TIMESTAMP(6)
      )
ORDER BY requested_at, bucket_start_utc
LIMIT 1;

-- affected_rows = 0 时提交并稍后重试；等于 1 时读取本 worker 刚领取的行。
SELECT grain, bucket_start_utc, timezone_name, version
FROM ss_rollup_jobs
WHERE lease_token = :new_random_binary_16
FOR UPDATE;

COMMIT;
```

`lease_token` 的唯一索引既用于取回本次任务，也充当 fencing token。领取事务随即提交，任何网络
或耗时工作都在事务外完成。5.7 下竞争 worker 可能短暂等待行锁，但不会领取同一任务；任务表应
保持精简，一次只领取一个，再由 worker 并发处理不同桶。过期 `status='leased'` 必须在领取条件中，
否则崩溃任务永远无法重领。应用对 MySQL 1205 lock wait timeout 和 1213 deadlock 使用有限次数、
带 jitter 的重试。单表 UPDATE 的 `ORDER BY`/`LIMIT` 语义见
[MySQL 5.7 UPDATE Statement](https://dev.mysql.com/doc/refman/5.7/en/update.html)，锁定读语义见
[MySQL 5.7 Locking Reads](https://dev.mysql.com/doc/refman/5.7/en/innodb-locking-reads.html)。

### 9.2 完整重算并覆盖

对单个小时桶：

1. 开启短事务并锁定 job，确认 lease token 和领取时的 version。
2. 从 `ss_usage_ledger` 按 `[bucket_start, bucket_end)` 和 `runtime_identity_id` 完整 `SUM` 四向字段。
3. 删除该小时旧缓存行，再插入本次完整结果；删除和插入必须处于同一事务。
4. 删除本次已不存在的 identity 行，不能只 upsert 新结果而留下幽灵行。
5. version 未变化时把小时 job 标成 done，清空 owner/token/until，并 upsert 对应日 job；日 job
   的 version 同样递增。
6. 提交。

日桶以同样方式从小时缓存完整重算。若实现选择覆盖式 UPSERT，其语义必须是下面的伪代码：

```sql
tcp_uplink_bytes = new_complete_bucket.tcp_uplink_bytes
```

而不是：

```sql
tcp_uplink_bytes = old_bucket.tcp_uplink_bytes
                   + new_complete_bucket.tcp_uplink_bytes
```

如果结算在 worker 计算期间写入同一桶，它也必须更新 job version。worker 只能在 version 和
lease token 仍匹配时完成任务；否则保留 pending 再跑一遍。这使重复执行、迟到数据和 worker
崩溃都不会造成双计。

### 9.3 查询完整性

不能因为某个范围“查到了缓存行”就认为整段缓存完整。查询层必须结合 watermark 和 dirty jobs：

- 已对齐且 `finalized_through` 覆盖的完整小时/日使用缓存；
- 未对齐的左右边界从 ledger 精确查询；
- 尚未聚合的尾部从 ledger 查询，或明确返回 `data_incomplete=true`；
- 管理端展示 `data_through`、`pending_dirty_buckets`、`cache_generated_at` 和最近失败。

迟到数据重新打开旧桶时，必须把连续 `finalized_through` 回退到该桶之前，或保证每次缓存查询都
检查请求范围内是否存在 dirty job；二者至少实现一个，不能让旧 watermark 掩盖新脏桶。

提供范围重算命令，按小时标记 `[from, to)` 为 dirty；不要让管理员手工修改缓存行。

## 10. Slim API 与页面

### 10.1 Agent 写入面

```text
POST /api/v1/nodes/{nodePublicId}/snapshots
GET  /api/v1/ingestion/batches/{batchId}
```

### 10.2 Dashboard 查询面

```text
GET /api/v1/dashboard/summary
GET /api/v1/dashboard/trend?grain=hour|day
GET /api/v1/nodes
GET /api/v1/nodes/{id}/status
GET /api/v1/nodes/{id}/usage
GET /api/v1/services/{id}/usage
GET /api/v1/users/{id}/usage
GET /api/v1/admin/ingestion/batches
POST /api/v1/admin/rollups/recompute
```

统一过滤参数为 `from`、`to`、`node_id`、`service_id`、`user_id`，范围统一使用 `[from, to)`。
所有输入采用绑定参数，grain 和排序字段使用白名单，不能把查询参数拼接进 SQL。

趋势元素示例：

```json
{
  "bucket_start": "2026-08-26T01:00:00Z",
  "tcp_uplink_bytes": "12345678901234567890",
  "tcp_downlink_bytes": "42",
  "udp_uplink_bytes": "10",
  "udp_downlink_bytes": "20",
  "request_bytes": "12345678901234567900",
  "response_bytes": "62",
  "traffic_bytes": "12345678901234567962",
  "computed_at": "2026-08-26T01:02:01Z",
  "revision": "7"
}
```

Dashboard 至少展示：

- 总上行、下行、TCP、UDP 和总量；
- 小时/日趋势；
- 节点、服务和用户排行；
- 节点在线状态、最后采集时间、runtime、sequence、连续失败次数；
- 不健康/冲突/计数回退/未闭合 runtime；
- 聚合水位、脏桶数、失败任务和数据时区。

在线状态是控制面判断，例如 `now - last_seen_at > 3 * poll_interval`，不能从 exporter 的
`active` 推导。页面对“精确流量”和“时间桶估算”分别给出说明，避免把 payload 统计误解为
运营商账单。

## 11. 安全与可靠性

- Agent 使用每节点 mTLS 证书，或独立高熵 token；数据库只保存 hash/证书标识，不保存明文密钥。
- 凭据映射出的 node 是授权真相，payload `node_id` 只用于交叉校验，不能由请求自行选择归属。
- ingest 限制 body、解压比、身份数、服务器数、超时和并发；原始错误不得回显密钥或 payload。
- Slim 路由按 agent/user/admin 分组，中控管理操作写审计日志并启用 CSRF 防护。
- collector outbox 加容量和最长期限告警；丢弃任何未确认批次都属于可观测的数据损失。
- settlement 和 rollup 使用有限租约与 fencing token；任务超时可重领，但结果仍受幂等键保护。
- MySQL 备份必须覆盖事实、cursor、batch receipt 和配置；只备份缓存不能恢复账本。
- 日志中只记录 batch ID、node public ID 和错误码，不记录代理密钥、完整原始快照或用户密码。

## 12. 保留期与容量

建议初始值：

| 数据 | 建议保留期 |
| --- | --- |
| node agent 本地 outbox | 已确认即清理，另设 3–7 天容量告警上限 |
| 压缩原始快照 payload | 终态并经过恢复宽限期后清理，通常共保留 3–14 天 |
| batch receipt/幂等元数据 | 不短于 ledger 与最大重试窗口 |
| immutable ledger | 180–400 天，按审计要求决定 |
| hourly cache | 180 天 |
| daily cache | 730 天或更长 |
| 冲突、人工冲正和安全审计 | 按合规策略长期保存 |

1000 身份的示例快照约 161 KB。每分钟保存一次未压缩原文约为 220 MiB/节点/日，因此原始 JSON
应压缩短期保存；需要长期取证时可放对象存储，在 MySQL 中只保留 URI、SHA-256 和长度。
payload 清理任务必须联表确认 batch 已处于 `applied/rejected/superseded/conflict` 终态并超过宽限期；
不得仅按 `expires_at` 删除仍为 `received` 的结算输入。settlement 不持久化中间 `processing` 状态：
它在同一个 MySQL 事务里锁 runtime 和 batch，崩溃时整笔回滚，batch 自然保持 `received` 可重试。

V1 建议 `ss_usage_ledger` 不分区，以保留简单的全局唯一键和外键。达到数千万行并经压测确认后，
再按月分区。迁移时必须注意：

- MySQL 分区表的每个唯一键都必须包含分区列，参见
  [Partitioning Keys, Primary Keys, and Unique Keys](https://dev.mysql.com/doc/mysql-partitioning-excerpt/5.7/en/partitioning-limitations-partitioning-keys-unique-keys.html)；
- InnoDB 分区表不能沿用普通表的外键设计，参见
  [MySQL 5.7 partitioning limitations](https://dev.mysql.com/doc/mysql-reslimits-excerpt/5.7/en/partitioning-limitations.html)；
- 全局 `(runtime, sequence)` 去重继续由非分区 `ss_snapshot_batches` 承担；
- 分区 ledger 的唯一键至少包含 accounting date；引用完整性由 settlement 事务和定期校验保证；
- 分区应提前创建，不允许业务请求临时执行 DDL。

## 13. 中控落地顺序

### 阶段 A：冻结契约

1. 固定四向字段、派生方向、`metric_scope` 和时间归桶规则。
2. 固定 Dashboard 时区、采集周期和允许时钟偏差。
3. 固定新 runtime 的默认 `baseline` 策略及受控 `include` 审批流程。
4. 定义版本化 canonical hash 编码和 API 错误码。
5. 固定 runtime identity 归属冻结规则，以及 settlement 拒绝、数据库 trigger/权限兜底和审计事件。

### 阶段 B：数据与写入链路

1. 建维度、runtime、batch、cursor 和 ledger migration。
2. 实现 node collector：HTTP/1.1-over-UDS 路由/status/header/body 校验、health 校验、本地
   outbox、mTLS 和重试。
3. 实现 Slim ingest API 的认证、限制、幂等 receipt 和压缩 payload。
4. 实现 settlement CLI worker 和管理端批次查询。

### 阶段 C：聚合与查询

1. 建 rollup jobs、watermarks、hourly 和 daily cache。
2. 实现小时覆盖式重算、日依赖重算、租约、version 和范围回填。
3. 实现 summary/trend/node/service/user API。
4. 实现 Slim 页面和缓存新鲜度/异常状态展示。

### 阶段 D：上线验证

1. 单节点影子运行，不把结果用于真实扣费。
2. 对账 `ledger SUM == cursor 差值`、`hourly == ledger`、`daily == hourly`。
3. 演练断网补传、重复批次、乱序、sequence 跳号、runtime 重启、完整 generation 键、同名身份
   重激活复用、lineage 缺失、未知 `identity_kind`、计数回退、`u64::MAX`、worker 崩溃和 MySQL
   恢复。
4. 观察容量、结算延迟、dirty job 延迟、查询耗时和原始 payload 压缩率。
5. 通过验收并取得生产授权后再逐节点启用。

## 14. 验收不变量

自动化测试和生产巡检至少持续验证：

```text
request_bytes  == tcp_uplink_bytes + udp_uplink_bytes
response_bytes == tcp_downlink_bytes + udp_downlink_bytes
traffic_bytes  == request_bytes + response_bytes

同一完整基线键的绝对计数永不下降
同一 runtime 的已接受 sequence 严格增加
同一 runtime 的 started_at_unix_ms 首次确认后固定不变
同一 runtime 已观察的 service/identity lineage 后续不得消失
同名逻辑身份重激活复用 generation=1 和累计 counter，只切换 active
同一 runtime identity 的 user_id/mapping_version 首次确认后不可修改、清空或删除重建
未知 identity_kind 使整个 snapshot 不入账
同一 batch + runtime identity 最多一条 ledger
任一非法 identity 会使整个 snapshot 不入账
同一完成小时：hourly SUM == ledger SUM
同一完成日期：daily SUM == 对应 hourly SUM
重复接收、重复 settlement、重复 rollup 不改变最终合计
```

只有账本、cursor、缓存和水位同时满足这些不变量，Dashboard 才能被视为完整；“接口返回了一个
非空数字”本身不是正确性证明。
