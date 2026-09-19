# 数据模型：账本、结算事务与聚合

> 本文是 [`../README.md`](../README.md) §4.4 / §4.5 / §5 / §6 / §8 的展开，不重复那里已有的约束编号。
> 设计参考 `shadowsocks-rust-plus/docs/CONTROL_PLANE_USAGE_STATISTICS.md`（一份针对
> MySQL 5.7 的同类主控数据设计），本文按本项目的 SQLite 单写者模型（D8）做了改写。
> 凡与节点契约冲突处，以节点 README 为准。

## 1. 三个口径

同一批字节在系统里有三个口径，**不要混用**：

| 口径 | 含义 | 用在哪 |
| --- | --- | --- |
| 本次增量 | 一个批次结算出的四向 delta | 账本行、小时/日汇总 |
| 周期累计 | 本计费周期起点以来的和 | 配额判定、额度池（README §4.5） |
| 永久累计（lifetime） | 跨周期、跨 runtime 的总和 | 用户总览、长期报表；订阅头使用与总额同周期的累计 |

字段命名上必须能一眼看出是哪个口径；建库时每个流量列都要写注释，**明确是增量还是累计，
以及时间是 UTC 还是展示时区**。同类系统为 164 个字段逐个补注释，起因就是看不出口径。

## 2. 结算键

每条累计基线用完整六元组：

```text
node_id
+ inbound.tag + inbound.generation
+ user.name  + user.generation
+ runtime_id
```

`generation` 属于协议键，**不能因为当前实现里它恒为 1 就从主键、游标或幂等哈希里省略**。
节点侧在同一 runtime 内把 `(tag, name)` 当作稳定逻辑身份：首次出现 `generation = 1`，
移除时只把同一条记录切成 `active = false`，同名重激活时**复用原记录、原累计值与原 generation**
并切回 `active = true`。

由此得到两条不对称但都必须成立的规则：

- `active = false` 的 lineage **继续保留基线**，不删除、不归零（C9）。
- 已经观察过的 lineage 在后续完整快照里**无故消失 → 整份拒绝**，不得解释成零增量或删除（C25）。

稳定的 `name` 是计费身份的一部分。**同一 runtime 内不得把它重新分配给另一个业务用户**；
确需改变归属时，要么换一个新 `name`，要么启动新 runtime 后按新映射建立基线。

## 3. 表清单

| 层次 | 表 | 作用 |
| --- | --- | --- |
| 配置维度 | `nodes` | 节点身份、provider 类型、采集策略、授信材料指纹、状态 |
| 配置维度 | `services` | 节点内稳定的 inbound 标识 |
| 配置维度 | `users` | 业务用户，角色仅 admin / user；尚未映射的身份可不关联用户 |
| 全局额度 | `quota_settings` / `quota_update_tasks` | 月度额度及 revision、逐节点待下发任务与结果；设置、审计、任务同事务提交，任务按最新 revision 收敛 |
| 默认授权 | `default_node_groups` / `user_node_grants` | 默认节点组、首次登录分配进度及用户节点授权；唯一用户/节点键，显式撤销不被重复登录覆盖 |
| 配置维度 | `identity_routes` | 节点 / 身份名 → 业务用户的当前映射规则；同节点跨入口的同名身份是**同一个**槽位（SS 与 VLESS 只是同一个名字的两种到达方式） |
| 生命周期 | `node_runtimes` | 进程运行周期、首快照策略、批准状态、最后 sequence |
| 生命周期 | `runtime_services` | runtime 内的 inbound generation |
| 生命周期 | `runtime_identities` | 完整身份 generation |
| 接收审计 | `snapshot_batches` | envelope、幂等键、原始字节摘要、排空证据、状态与错误；终态回执不依赖 payload TTL |
| 接收审计 | `snapshot_payloads` | 压缩后的原始 JSON；**结算输入**，不是审计附件 |
| 结算状态 | `counter_cursors` | 每个六元组最后接受的四个绝对累计值 |
| 配额状态 | `quota_allocations` / `quota_requests` | 身份目标、权重、观察窗口、完整节点请求及发送前分配的 epoch（见 §11） |
| 永久事实 | `usage_ledger` | 只追加的四向增量账本 |
| 持久累计 | `usage_lifetime_totals` | 每个 runtime identity 的四向永久累计，关联固定归属；跨 runtime 汇总为用户 lifetime |
| 持久累计 | `usage_cycle_totals` | 按用户、周期和计费规则版本保存的周期累计，与订阅/配额共用 |
| 清理控制 | `ledger_retention_state` | 已清理流水边界、累计检查点和归档索引；不属于可丢弃缓存 |
| 归属历史 | `identity_assignment_events` | 只追加的身份 → 用户分配事件（D14） |
| 聚合控制 | `rollup_jobs` | 可重复领取、带 version 的脏桶（**无租约**，见 §7.1） |
| 聚合控制 | `rollup_watermarks` | 缓存覆盖范围与新鲜度 |
| 查询缓存 | `usage_dashboard_hourly` | 小时 × runtime identity 的叶子缓存 |
| 查询缓存 | `usage_dashboard_daily` | 日 × 时区 × runtime identity 的叶子缓存 |

**查询缓存只保留到 `runtime_identity` 这一最细维度**，靠冗余的 node / service / user 外键索引做汇总。
这样不需要同时维护 global / node / service / user 四套互相容易不一致的缓存。

### 3.1 SQLite 事务与数值编码

SQLite 没有 `SELECT ... FOR UPDATE` 行锁。本项目使用**单写者队列 + 短 `BEGIN IMMEDIATE` 事务**，
在同一连接完成读改写；`SQLITE_BUSY` 有界退避，失败整笔重试。事务内不调用网络、不等待节点，
也不运行长时间聚合。WAL 可用于并发读，但不会增加写入者数量。

SQLite INTEGER 是有符号 64 位，SQLx 不支持直接编码完整 Rust u64；TEXT 的隐式数值转换也会丢精度。
因此存储契约固定如下，详见 [SQLite 事务说明](https://www.sqlite.org/lang_transaction.html) 与
[SQLx SQLite 类型说明](https://docs.rs/sqlx/latest/sqlx/sqlite/types/)：

| 数值 | Rust 运算 | SQLite 编码 |
| --- | --- | --- |
| 四向累计游标、sequence、generation、started_at_unix_ms、epoch | `u64`，checked 差分/递增 | `TEXT COLLATE BINARY`，左补零到 20 位 |
| 四向 delta | `u64` | 同上，20 位文本 |
| 多方向/多节点总和、周期累计、lifetime | checked `u128` | `TEXT COLLATE BINARY`，左补零到 39 位 |
| 单身份下发额度 | 范围 `0..=i64::MAX` | INTEGER，`CHECK(value >= 0)`；wire 仍为 JSON 整数 |
| 内部主键、任务版本与 revision | 非负 `i64`，checked 递增 | INTEGER；耗尽时报错，不回绕 |

文本列加 `NOT NULL` 与 CHECK：`typeof(value) = 'text'`、长度精确为 20/39、
`value NOT GLOB '*[^0-9]*'`，且二进制字典序分别不大于
`18446744073709551615` / `340282366920938463463374607431768211455`。
统一 Rust 包装类型负责解析、补零和绑定；固定宽度使同类型的排序、范围查询与索引保持整数顺序。

**禁止对这些文本列使用 SQL `SUM`、加减乘除或 CAST 到 INTEGER/REAL。** 在 Rust 中 checked 聚合，
溢出即回滚并告警；回写完整编码。内部配额配置上限为 u64，先在 u128 中对周期累计做饱和减法，
再 checked 转回 u64；权重比例使用 checked u128。API 输出去掉数据库补零的十进制字符串。
至少覆盖 `2^63−1`、`2^63`、`u64::MAX`、跨 u64 的聚合、u128 溢出，以及编码后的排序与往返。

## 4. 结算事务：11 步

同一 `(node_id, runtime_id)` 的批次**必须串行结算**。顺序如下，任何一步失败整笔回滚：

1. 经单写者队列开启 `BEGIN IMMEDIATE`，读取 `node_runtimes` 与待处理批次。
2. **不按外部传入的批次 ID 结算。** 在该写事务内，按 `(node_id, runtime_id, status, sequence)` 索引
   选择**最小的未处理 sequence**。批次已是终态时幂等返回；更旧且此前未处理的标 `superseded`。
   这条同时回答了「乱序到达怎么办」和「首快照策略会不会误落在一个已排队的较新快照上」。
3. `sequence` 必须**严格大于** runtime 最后已接受值。**允许跳号**（真实采集丢失，C26）：
   跳号告警并记录缺口，但照常按累计差值结算，**绝不对缺失时段插值**。
   `sequence` ≤ 已接受值则整份丢弃且不推进任何状态（C5）。
4. 建立或读取快照中的 inbound generation 与身份 generation。
   同一快照内出现重复完整键 → 整份拒绝。已观察过的 lineage 缺失 → 整份拒绝（C25）。
5. **按 `runtime_identity_id` 排序**后读取游标，保证运算和哈希顺序确定；不用模拟行锁。
6. 对每个已有游标逐项检查 `current >= previous`。**任何一项下降都回滚整份快照**——
   不能局部入账，也不能擅自解释成重启；合法重启必须更换 `runtime_id`（C5，P0 失败关闭）。
7. 新游标按 runtime 创建时**已持久化**的策略处理（C8）：
   `baseline` 首次 delta 为 0；`include` 首次 delta 等于当前累计值。
   策略、原因与操作者一并持久化。
8. **先在内存里完成全快照校验，再插入任何一行非零 delta。**
   每条账本行同时计算版本化的 `source_event_hash`：对 node / runtime / 完整 lineage /
   `sequence` / 四向 delta 做**长度明确的 canonical 编码**后取 SHA-256，
   **不用字符串拼接**（分隔符歧义会让两组不同的输入算出同一个哈希）。
9. 对本次新插入的账本增量，同事务 checked 更新 `usage_lifetime_totals` 和 `usage_cycle_totals`；
   更新**所有**游标，**包括零增量与 `active = false` 的身份**。幂等返回不累加第二次。
10. 更新 runtime 的最后 sequence、批次状态，并 upsert 对应小时脏桶：
    已存在时 `version = version + 1, status = 'pending'`。
11. 全部成功后一次提交。

补充规则：

- **禁止 `INSERT OR REPLACE`**：它是删除后插入，会破坏审计与引用语义。
- 账本幂等由 `UNIQUE(batch_id, runtime_identity_id)` 保证；
  **缓存正确性不能依赖「通常不会重复」**。
- **不持久化中间 `processing` 状态**：runtime、批次和累计更新在同一写事务里，
  崩溃时整笔回滚，批次自然保持可重试。
- 只要 envelope 还能识别出 node / runtime / sequence，**可审计的拒绝也要留一条批次回执**
  （状态 `rejected`，绝不推进 sequence 与游标）；完全无法识别 envelope 的畸形请求只进限量安全日志。
- 每个新 runtime 必须显式登记 `baseline` 或 `include`，缺省拒绝结算；
  选择 `include` 必须能证明不存在其他数据源的重复入账，不能因计数从零开始就隐式批准。
- 异常退出的旧 runtime 标为 `unclosed` 且「可能丢尾」。
  **绝不能用新 runtime 的累计值去减旧 runtime 的游标。**
- 最终快照回执绑定 `node_id / runtime_id / sequence / payload_sha256`，只读停机确认查询该精确批次。
  `applied` 须来自上述事务的提交结果，不能从 runtime 的最大 sequence 推导；排空判据取自同一份快照。
  关闭窗口时保存回执及排空证据的引用，完整参数与失败分支见 `operations.md` §3。

## 5. 原始快照字节

压缩后的原始响应与批次在**同一事务**保存。它是结算输入，不是可选的审计附件：

- 异步结算必须能**从库里恢复完整快照**；
- C5 的失败关闭要人工介入，没有原始字节无法复盘；
- C3 的 u64 解析争议也只能靠原始字节佐证。

哈希对**解压后的原始请求字节**计算，**不要重新序列化对象后再算**——
字段顺序与数字表示都可能变化。

清理任务必须联表确认批次已处于终态（`applied` / `rejected` / `superseded` / `conflict`）
并超过恢复宽限期，**不得仅按过期时间删除仍未结算的输入**。

## 6. 时间与归桶

每份快照保存三个时间，**并持久化归桶选择结果**（否则重放同一批次会落进另一个桶）：

| 字段 | 来源 | 用途 |
| --- | --- | --- |
| `collected_at` | agent 完成采集的时刻 | 时间质量判定 |
| `received_at` | 主控收到响应的可信时刻 | 偏差核对；回落值 |
| `accounting_at` | 主控按规则生成 | 增量归属的时间桶 |

- 节点必须启用 NTP（C27）。`collected_at` 在同一 runtime 内单调不倒退，
  并限制它相对 `received_at` 的未来偏差。不合法时回落 `received_at` 并标
  `time_quality = fallback`；**无论用哪个，都要把选择结果落库**。
- 时间在应用层统一编码为 UTC；SQLite 不依赖连接会话时区。
- 小时桶用 UTC 整点 + **半开区间 `[start, end)`**。
- 日桶用一个**持久化的展示时区**，按该时区的**日历日**计算——
  不能写成「前一日开始时间 + 24 小时」。
- **改展示时区必须完整重建日缓存**，API 必须回传当前时区。

口径三元组随事实一起固定：

```text
metric_scope          = 认证解码后进入转发边界的应用负载
traffic_estimated     = false
time_bucket_estimated = true
```

第三项不是笔误。快照只有累计值、没有逐次传输的事件时间，
所以两次成功快照之间的增量被整体归到本次采集所在的桶：
在同一健康 runtime 内累计差分保持字节守恒，但**时间不确定范围是两次成功采集的实际间隔**，
还需披露时钟回落带来的不确定性；断网多轮不能按一个调度周期计算（README §10 R11）。
持久化观察窗口起止及 `observation_gap_ms`，首次 include 则从 runtime 起点标记。
窗口跨计费周期时，第一版按已持久化的 `accounting_at` 整段归入接收周期，并标记跨周期估算，
不插值伪造真实分布；配额与订阅使用同一归属。要求精确分割周期的套餐在有缺口时需人工对账。
若 runtime 已变，旧窗口未持久化的尾账不可恢复，标 `unclosed`，不得用新 runtime 累计补旧账。

## 7. 聚合

- 脏桶 upsert 时 `version = version + 1, status = 'pending'`。
- **小时桶完整重算并覆盖，不做 `+=`。** 日桶依赖小时桶重算。
- **聚合失败不推进已完成水位。**

### 7.1 任务领取：单写者下不要租约

参考蓝图用的是**有限租约 + fencing token**——那是为**跨进程、可能被网络分区的多 worker**
设计的，而且它的目标数据库没有 `SKIP LOCKED`，只能靠租约模拟。本项目是 SQLite 单写者（D8），
这两个前提都不成立，照搬会引入一套没有对应威胁的机制。

**本项目的做法**：

- 同一桶用**进程内互斥**领取，数据库中的状态保持 `pending`，不提交中间 `processing`。
  在短读事务中取得版本和一致的输入，随后释放读事务并在 Rust 中计算；进程崩溃时该桶仍可领取。
- 正确性不依赖「谁领到了」，而依赖**幂等**：小时桶是完整重算并覆盖的（上面第 2 条），
  同一个桶被算两次结果相同。因此重复领取是浪费，不是错误。
- `version` 用来检测「算的时候又脏了」：计算完成后，经单写者队列开启短 `BEGIN IMMEDIATE`，
  若当前版本等于读取版本，才将缓存覆盖、任务完成和水位一起提交；否则丢弃结果、保留 `pending`。
  这一条与 worker 数量无关，单写者同样需要——手动触发重算（README §6 的
  `POST /nodes/{id}/collect`、`POST /users/{id}/quota:recalc`）会与调度器并发。

**什么时候才需要租约与 fencing**：主控变成多进程时。那时要换的不只是这一处——
D8 的单写者假设、SQLite 本身、以及 README §10 R3 的单点风险都要一起重做。
所以**不要为了「将来好改」提前引入租约**：它不会让那次迁移变简单，只会让现在多一套
没人验证过的代码路径。届时按 §8 的措辞纪律，把它作为一次显式的架构变更处理。

## 8. 历史身份归属（D14）

计费身份被重新分配给另一个业务用户后，**旧账本行必须保留原归属**，
否则历史账单会在重新分配的瞬间被改写。

- `identity_assignment_events` 是**只追加**的：禁止修改或删除历史事件。
  状态集：`assigned` / `unassigned` / `unmapped` / `clock_ambiguous`。
- 归属时刻用**已批准 runtime 的 `started_at_unix_ms` + 单调增量**做 checked arithmetic，
  **不用节点墙钟单独决定**。
- runtime 未批准或缺失，或与事件时刻的偏差超阈值 → 归属留空并标 `clock_ambiguous`。
  **存疑时不猜。**
- **禁止用采集时的当前账号关系覆盖历史归属。**
- 当前映射关系（`identity_routes`）与历史归属是两张表、两种语义，不要合并。

**审计查询复用历史归属，但不以文件名授权（README C35）。** plus 审计文件按身份名分组，
同一文件可能包含不同入口、不同 runtime 的同名记录。逐行用 `node / run / in / user` 唯一关联
已批准的 `runtime_identity` 及其 generation，再检查该用户的历史归属与持有半开区间；
节点记录没有 generation，不能自行补常量或用当前同名映射兜底。缺字段、无法唯一关联、未批准、
`unmapped` 或 `clock_ambiguous` 的记录不返回。审计 `ts` 是墙钟，不单独用于选择身份归属。
过滤在聚合、计数和分页前完成，个人与管理员按用户查询共用这一条规则。

## 9. 保留期分层

| 数据 | 建议保留期 |
| --- | --- |
| 压缩原始快照 payload | 终态并过恢复宽限期后清理，通常 3–14 天 |
| 批次回执 / 幂等元数据 | 不短于账本与最大重试窗口 |
| 只追加账本 | 180–400 天，按审计要求定 |
| `usage_lifetime_totals` 与清理检查点 | 与业务账号生命周期一致；不得随账本/缓存 TTL 清理 |
| `usage_cycle_totals` | 至少覆盖全部未结束周期及账单审计期限 |
| 归属事件 | 与账本同期或更长 |
| 小时缓存 | 180 天 |
| 日缓存 | 730 天或更长 |
| 冲突、人工冲正与安全审计 | 按合规策略长期保存 |

永久累计在每次结算事务中更新，**不是对保留期内账本求 SUM 的缓存**。
明细清理前验证对应批次已结算、累计已提交，记录清理范围、摘要与累计检查点；
检查点和分批删除在同一写事务推进。清理重试不能再次累加累计表，不能级联删除累计所依赖的身份与归属。
累计表、检查点、runtime 最后 sequence 与幂等状态须纳入一致性备份；历史明细过期不放宽重放拒绝规则。

周期未关闭且缺少可重建事实时不删对应账本。需从零重建长期累计、旧日期缓存或改变历史时区时，
必须保留可校验的归档明细；否则 API 明确返回不可重建的覆盖范围，不能把缺失历史重算成零。
归档恢复应从同一检查点的累计/游标继续，而非将归档明细再次叠加到完整 lifetime。

清理必须是**分队列、限时、分批**的，并对积压告警——一次性大删会和结算抢锁。
容量测算按「身份数 × 每身份字段数 × 采集频率」估算未压缩原文体积，
需要长期取证时把原文放对象存储，库里只留 URI、SHA-256 与长度。

审计链路的保留期与对外声明**独立于计费链路**制定（C24）；访问权限按 D18/D19 与 C35 执行。

## 10. 验收不变量

自动化测试与生产巡检要持续验证：

```text
上行合计 == tcp_uplink_bytes + udp_uplink_bytes
下行合计 == tcp_downlink_bytes + udp_downlink_bytes
总流量   == 上行合计 + 下行合计

同一完整基线键的绝对计数永不下降
同一 runtime 的已接受 sequence 严格增加
同一 runtime 的 started_at_unix_ms 首次确认后固定不变
同一 runtime 已观察的 lineage 后续不得消失
同名逻辑身份重激活复用原 generation 与原累计值，只切换 active
同一 runtime identity 的用户映射首次确认后不可修改、清空或删除重建
不可入账的 health 位为真时整份快照不入账
同一 batch + runtime identity 最多一条账本行
任一非法身份使整份快照不入账
同一完成小时：小时缓存合计 == 账本合计
同一完成日期：日缓存合计 == 对应小时缓存合计
重复接收、重复结算、重复聚合不改变最终合计
新计划分配额之和 ≤ 额度池剩余
额度池剩余 ≥ 0（quota − used 不得下溢，README C32）
下发清单枚举该节点所有受限用户的全部当前有效身份，零额度条目在列（README C30）
同一用户同一节点的各身份限额之和 == 该节点分配额 —— 仅当这些身份口径相同（README D17 情形 3）
地板可用时每个可更新节点的额度不低于地板；零权重/零额度不会阻断探测
无新鲜健康结算输入时不刷新正额度
账本、永久累计、周期累计、游标在同一事务提交；重放和清理不改变已确认累计
订阅头的 upload/download 与 total 使用同一计费周期及口径
```

身份限额之和的等式**只在同口径的多身份下成立**。若两个身份的计费折算系数不同
（README D17 情形 2），它们的字节不可直接相加，断言这条反而会掩盖「两个口径被错误地
加在一起了」这个真问题——那种情况下它们应当各自独立成池，根本不共享一个分配额。

只有账本、游标、缓存与水位**同时**满足这些不变量，控制台才算完整。

> **「接口返回了一个非空数字」本身不是正确性证明。**

## 11. 配额请求与恢复

| 状态 | 必须持久化的内容 |
| --- | --- |
| `quota_allocations` | 完整身份键与业务池、目标额、确认下发的 `a_i`、正整数需求权重、观察窗口和样本有效性 |
| `quota_requests` | 节点/runtime、发送前分配的 epoch、完整节点请求字节及摘要、预算版本、来源 sequence、新鲜度期限、发送/确认时间、结果 |
| 池调度状态 | 探测轮转游标、受限节点集合、预算版本、最近健康结算时间 |
| `quota_update_tasks` | 设置 revision、节点、待下发状态及最后错误；新配置成功应用前可重试，重启后不丢任务 |

单写者事务先检查预算，写入完整请求和单调递增的 epoch，再允许网络发送。
结果集为 `prepared` / `applied` / `rejected` / `unknown`；任何已准备却无法确认是否发送的请求，
恢复时按 `unknown` 对待。最后一次 200 的 epoch 不是计数器恢复依据，最大已分配值才是。
每份正额度请求占用一个新的已结算 `(node_id, runtime_id, sequence)`，数据库唯一约束防止重复使用；
发送前仍检查快照新鲜度，不能在 TTL 内用同一份健康快照反复刷新余额。纯零额度安全表不受该占用限制。

额度变更在单写者事务中更新 `quota_settings` 的额度与 revision、记录审计并生成逐节点任务。
提交后主动采集，不等待定时周期；必须取得上次正额度请求之后的新健康 sequence，结算后才重算完整表。
发送前校验设置 revision 与预算版本，过时任务重算；已发送但结果不明的请求仍走 `unknown` 恢复，
已分配 epoch 与 sequence 占用不得回滚复用。更新状态按 README §4.5 区分 `pending / applied / safe_zero / unknown / failed`，
不是配额请求表的状态；零表确认不冒充新配置正常生效。更新配置本身不写用量账本，新采集中的真实增量照常入账。

节点的每次全量表重设所有列出的余额，所以单用户重算也要为其他受限用户重算，
不能复制它们上次的正额度。需求样本必须关联明确的下发与观察窗口；跨 epoch 或结果不明时不更新权重。
`a_i = 0` 不计算消耗率，`u_i > a_i` 允许且只对权重输入截断，不篡改账本。

**`unknown` 不做特别记账（README D20）。** 早先版本为「结果不明的在途授信」维护一笔预留，
冻结预算并要求人工核销才能释放。那一层连同 C34 一起去掉了：配额是内部限额，
几 GiB 的重复授信不构成问题，而预留带来的冻结、核销与 `H > pool` 对账是一整套没有对应收益的机制。
现在的处理是：恢复后先核对 runtime，再以更高 epoch 的表收敛，下一轮正常重算。
**代价是超发没有有限上界**，写在 README §10 R2。

清理仅覆盖已终结的旧请求，但要保留足够的恢复依据，不能简单按「最近 N 轮」删除。

节点额度仍是内存状态，表内记录不等于远端剩余额度。409 只是核对入口；
恢复先按 README §4.5 收敛零额度、核对 runtime 与账本，再决定是否恢复正额度。
