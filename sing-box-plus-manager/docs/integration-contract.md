# 跨仓库契约纪律

> 本文回答一个具体问题：**本项目与节点侧项目由不同的人维护，怎么保证两边对协议的理解不漂移。**
>
> 写这份文档的直接起因见 §4——一次真实的、已经发生的漂移。

## 1. 职责边界

| 范围 | 权威位置 | 谁负责 |
| --- | --- | --- |
| 数据面、快照/配额端点、审计写入、golden vectors、节点侧测试 | `sing-box-plus` | 节点侧维护者 |
| 采集、结算、账本、配额判定、订阅、控制台、告警 | 本项目 | 主控侧维护者 |
| 节点上的进程编排、密钥注入、证书轮换、回滚 | 部署资料 | 运维 |

配套的硬规则：

- **本项目不写节点侧代码**，也不在节点仓库里提交兼容补丁。
- **节点侧维护者不写主控代码**，不碰主控的数据库迁移。
- 任何一方要改 wire 合同，先在**节点侧**升版并更新对应链路的 fixtures / 契约测试，主控再跟进。

## 2. 三条不可协商的纪律

### 2.1 wire 合同变更必须从节点侧发起

主控**不得自己发明兼容分支**。

看到一个不认识的字段、一个对不上的版本号、一个多出来的 health 位时，正确动作是
**整份拒绝并告警**（C4 C10），然后回报上游——不是在主控里加一个 `if` 让它「先跑起来」。
那个 `if` 会活得比记得它为什么存在的人更久。

### 2.2 每次集成变更都要记录三元组

```text
节点实现 commit + 协议版本 + 对应链路的 fixture/契约测试摘要
```

三者缺一不可。只记版本号不够——同一个版本号下 fixture 可能已经变了；
只记 commit 也不够——它不能告诉你主控当时按哪份 fixture 验的。

### 2.3 消费节点侧的 fixtures，不要转抄文档

主控**直接消费与节点 release 匹配的计量 fixtures 与契约测试**，
**不在主控侧重定义第二份 schema、错误码表或编码规则**。

两份定义总会漂移，唯一的问题是什么时候、以及谁先发现。

## 3. 落地做法

| 链路 | 应消费的仓库文件 | 用途 |
| --- | --- | --- |
| `plus_v3` 计量 | [`tests/fixtures.py`](../../sing-box-plus/tests/fixtures.py)、[`tests/settlement_model.py`](../../sing-box-plus/tests/settlement_model.py) 的 `parse_snapshot()`、[`tests/http_unix.py`](../../sing-box-plus/tests/http_unix.py)、[`internal/userstats/contract_test.go`](../../sing-box-plus/internal/userstats/contract_test.go) | v3 样例、整数/health/gauge 校验与 HTTP 契约；http_unix 只负责传输，不导出 validate_snapshot |
| `plus_v3` 配额 | [`internal/userstats/quota_endpoint.go`](../../sing-box-plus/internal/userstats/quota_endpoint.go)、[`internal/userstats/quota_integration_test.go`](../../sing-box-plus/internal/userstats/quota_integration_test.go) | 全量覆盖、错误分支、事后闸断与超额行为 |
| `plus_v3` 文件审计 | [`internal/userstats/audit.go`](../../sing-box-plus/internal/userstats/audit.go)、[`internal/userstats/audit_file.go`](../../sing-box-plus/internal/userstats/audit_file.go)、[`internal/userstats/audit_test.go`](../../sing-box-plus/internal/userstats/audit_test.go)、[`internal/userstats/audit_integration_test.go`](../../sing-box-plus/internal/userstats/audit_integration_test.go) | JSONL 的完整身份、`ts / up / down / ms`、TCP/UDP 成功判据、同名文件与按大小轮转；C35 的查询过滤在主控另测 |
| 审计 HMAC 参考 | [`tests/golden_vectors.json`](../../shadowsocks-rust-plus/tests/golden_vectors.json)、[`tests/mock_collector.py`](../../shadowsocks-rust-plus/tests/mock_collector.py) | **仅 auditd 协议参考**，不能代替计量快照或 plus 文件审计夹具 |

M0 的假节点 UDS 服务需要新建：读取上述 provider 专属样例，注入 health、sequence、计数与 HTTP 故障。
`mock_collector.py` 是连接 auditd 的审计客户端，不会提供 `GET /v1/snapshot` 或 `/v3/snapshot` 服务；
`golden_vectors.json` 覆盖 `/v1/audit/lease` 的 HMAC 与审计事件，没有两个计量 provider 的快照形状。
校验入口也不能只看文件名：v3 的解析入口是 `settlement_model.parse_snapshot(bytes)`；
v1 的 `http_unix.validate_snapshot()` 本身允许额外 health 键，须同时核对结算模型的闭集规则。
主控 provider 的验收覆盖整条校验/入账路径，不能把单个 HTTP helper 通过当作 C4 已满足。

- 按锁定 commit 的文件/测试入口读取样例，不复制粘贴；隔离两个 Python 模块命名空间。
- M0 必须建立各链路的输入清单与摘要锁定；CI 在摘要变化时失败，要求更新 §2.2 的三元组。
  本轮仅修正文档，尚未建立这些 CI 门禁，不把它们写成已经执行。
- 版本断言对齐节点代码：`sing-box-plus` 的 `internal/userstats/snapshot.go`，不对齐它的 README。
- 注入版本错配与 health 多/少字段，验证合法样例被接受、畸形样例被整份拒绝；
  配额测试必须包含多用户全量覆盖与分块超额，不能用审计向量“全绿”替代。
- 文件审计另测跨入口同名、跨 runtime 复用、归属裁剪、字段缺失和个人端点授权；
  UDP 单向成功与 TCP 双向成功按节点样例区分。当前只有按大小轮转，不能由主控文案推导固定归档延迟。

## 4. 这份文档的由来

2026-09-17 的契约核对发现：

- 节点侧 `sing-box-plus/internal/userstats/snapshot.go` 中 `const SchemaVersion = 3`，
  契约测试按该常量断言；源码注释明确写着 `audit_dropped` 是把版本从 2 推到 3 的原因，
  路径同步改为 `/v3`。
- 但该项目的 `README.md` 有约 10 处、`docs/API.md` 有约 6 处仍写 `2`。
- 本项目的计划书**忠实照抄了 README**，把 `plus_v3` 的版本断言写成「必须 `2`」。

叠加 C10「路径与版本号成对硬校验，不符整份拒绝」，这会让采集器在 M1 第一天
**拒绝每一份真实快照**——而且失败信息会指向「节点返回了错误的版本」，
把排查方向引到节点侧去。

同一次核对还发现了第二类漂移：当时还支持的第二个 provider 与 `sing-box-plus` 的
`health` 闭集大小不同（四位 vs 两位），且没有 session gauge。这些差异在任何一份
README 里都没有并排写出来过。第二个 provider 已按 D3 去掉，但教训留下：
**能力差异必须从源码读出来，不能从文档推断。**

只有消费节点侧的样例和契约测试，这两类漂移才能被机械捕获。
最初把审计 `golden_vectors.json` 当成快照契约依据的说法不成立；文件名含 golden 不代表覆盖了目标协议。

上游文档已于同轮勘误。但勘误只解决这一次；§2.3 才解决下一次。

## 5. 一次真实部署核对（2026-09-18）

§2.3 要求消费节点侧的 fixtures 而不是转抄文档。这一节记的是**那套锁定输入与一台
真实运行的节点对不对得上**——`tests/node-fixtures.lock.toml` 锁的是仓库里的文件，
而仓库里的文件与线上跑的是不是同一份，是另一个问题。

对着一台部署了 `sing-box-plus` 的节点做了只读核对（主机名、IP、节点名与计费身份名
按仓库根 CLAUDE.md 一律不落文档）：

| 核对项 | 结果 |
| --- | --- |
| 锁定的 4 个上游文件 SHA-256 vs 节点上的同名文件 | **逐字节相同**——锁与线上是同一份 |
| 真实快照过主控校验器（`collect check-snapshot`） | **接受**：`schema_version = 3`、`health` 恰好四位、`(tag, generation)` 升序、`Content-Length` 自洽、行尾 LF |
| 审计文件名映射（`wire::audit`）vs 节点上的真实文件名 | **逐字节一致**（base64url 无填充），活动 / 已轮转判定均正确 |
| 参考 collector 的四项判据（其账本，非本项目的） | 15 043 个批次：负增量 0、未知 runtime 0、重复 `sequence` 0、unhealthy 入账 0 |

**这条核对证明的是「我们不会拒绝真实快照」，不是「我们的结算是对的」。**
后者要等 M1 的账本落地后用本项目自己的账本重新取证；按 §8 的措辞纪律，
两者不能混为一谈。

核对时顺带发现的、属于**主控侧设计缺口**的一条，已写进 README §4.5：
**接管一个 live runtime 时无法发现节点当前的 `epoch`**——409 的错误体不带它。

同一次核对还确认了一个容易误判的顺序：节点的 `admit()` **先查 `exhausted()`、再查 stale**，
因此 `stale_action = allow` **救不了**一个已经把额度用尽的身份。
这正是 README §4.5「停推本身不会将已下发的限额变为无限」那句话的机制。
停掉一个正在下发的采集器之前，要么先推一张空表把这些身份交回无限额度（C16），
要么按节点侧 §5.3 换 runtime。

### 5.1 整条链路的真机核对（2026-09-18）

§5 验的是「我们认不认真实快照」。这一节验的是**整条链路在真机上跑不跑得起来**：
真 `proxy-manager-agent` + 真节点的快照 socket（经 SSH 转发到本机）+ 真主控服务，
只读端点，不碰配额。

| 核对项 | 结果 |
| --- | --- |
| PKI → agent → mTLS → 主控 → 账本 | 跑通。7 个批次 `applied`，账本行含真实增量 |
| runtime 未批准时的行为 | 批次保持 `pending`，批准后一次 tick 排空积压 |
| 四项判据 | 全为 0 |
| **`sequence` 跳号** | **自然发生并被正确处理**：`gap = 1`，按累计差值结算、不插值（C26） |

最后一条值得单独说。跳号此前只在夹具里注入过，这次是**真实发生的**——
节点侧的参考实现每分钟也在轮询同一个端点，于是它取走的序号在我们两次采集之间，
表现为 `gap = 1`。这正是 C26 描述的形态：

> `sequence` **跳号是合法的**（真实采集丢失），只要求严格大于最后已接受值。
> 跳号告警，但**绝不对缺失时段插值**。

**同时它也是一条运维提醒**：两个采集器共存时跳号是常态而不是异常。
真要并行跑（例如与参考实现对账），告警阈值要按这个前提设，
否则跳号告警会淹没掉真正的采集失败。**配额下发则不能两边都做**——那是 epoch 冲突的来源。

### 5.2 与参考实现的账本逐桶对账（2026-09-18）

§5.1 验的是链路跑不跑得起来。这一节回答更硬的那个问题：
**我们算出来的账，和节点侧参考实现算出来的，是不是同一笔。**

做法：主控以**只采不下发**的形态跑在参考实现旁边 8 分钟（15 秒一采，参考实现 60 秒一采），
各持各的账本。工具是 `scripts/reconcile-with-reference.py`。

对账能做到**精确**而不是「看着差不多」，靠的是一条恒等式：两边都是对同一组累计计数器
做差分，所以「从序号 A 到序号 B 的增量之和」恒等于 `C(B) − C(A)`，**与中间采了几次无关**。

窗口：我方 `sequence` 3252 → 3293（33 个批次），参考实现有 7 个样本落在其中。

| 断言 | 结果 |
| --- | --- |
| 字节守恒（我方内部，两端绝对值取自**原始快照字节**） | 逐身份精确相等 |
| 夹逼：参考内侧 ≤ 我方 ≤ 参考外侧 | 成立 |
| **反向夹逼：我方密采样夹住参考侧报出的数** | 成立，带宽 **1.48%** |
| 轨迹交错：两边重建的绝对值按序号归并后单调不减 | 36 个采样点，0 处违反 |

反向夹逼是这组里最有说服力的一条。参考实现说某段是 292 750 字节；
我方采样密度足够把这个数夹到 `[288 687, 293 023]`，带宽 4 336 字节——
而**那点带宽全部来自两端各一个序号的间隙**。它消不掉：每次 `GET` 都消耗一个序号，
两个采集器**不可能**采到同一个序号，所以这就是跨账本对账在原理上能达到的最紧程度。
另一个身份该窗口内无流量，带宽为 0，属于精确相等。

**这证明了什么、没证明什么。** 证明的是：两边认的是同一条计数器轨迹，
各自都没有重复入账或漏记。**没有**证明结算的其余部分——归属裁剪、周期切分、
配额判定都不在这条链路上；也**没有**证明四向拆分口径一致（两边口径本就相同，
比出来是同义反复）。按 §8 的措辞纪律，这是一次**自动化核对**，不是生产演练。

**运维口径**：两个采集器共存时 `sequence` 跳号是常态（§5.1），
而**配额下发不能两边都做**——那是 epoch 冲突的来源。节点侧运维文档写死了另一半：
两者不得同时对同一个账本写入，差分基线是有状态的。

### 5.3 一次漂移核对（2026-09-18）

`tests/m0/fixture_lock.rs` 三项全红，触发原因是节点侧在 `fbc29ae7` 之后连续落地了
六个修复。按 §2.2 逐条核对后更新锁到 `c1102a4`，**没有在主控里加任何兼容分支**。

核对结论先摆在前面：**线上契约没有变**。协议版本仍是 `3`；
`tests/fixtures.py`、`tests/settlement_model.py`、`scripts/http_unix.py`
三个被消费的 Python 文件**摘要逐字未变**；`snapshot.go` 的 diff 里没有任何
`json:` 标签增删，快照形状逐字段兼容；`quota_endpoint.go` 的 409 映射逐字保留。
变的是节点内部行为，而且六条里有五条是朝着既有契约修的。

| 节点提交 | 内容 | 对主控的影响 |
| --- | --- | --- |
| `e73eda7` | epoch 单调性判定移入锁内；快照取序号与读计数绑成原子 | **有利**。并发 PUT 乱序落地会让节点应用比控制器意图更大的表；序号与计数倒序会让结算方对着一个正确的进程 fail-closed 停止入账。两者都是主控此前无法自保的失败 |
| `9808015` | `max_identities` 改为只约束活跃血统，墓碑由派生上限约束 | **唯一的真契约变更**，见下 |
| `3ca4c42` | 启动窗口内配额与审计未生效；配额策略数据竞争 | **有利**。该窗口内 `admit()` 会放行已耗尽的身份；`quotaPolicy` 改原子替换后，跨 SIGHUP 改配额不再出现「新 staleAction 配旧 staleAfter」的撕裂——定案第五条正是靠 reload 落地的 |
| `b6e7cc6` | `max_request_bytes` 对无体请求失效；503 无原因短语；`deny` 缺 `stale_after` 静默 fail-open | **无影响**。主控自己的 C12 门禁本就要求 `stale_after_secs != 0`，与节点侧新加的硬失败同向 |
| `c1102a4` | `audit_dropped` 提升为 registry 上的粘滞位 | **无影响**。文档一直写着它粘滞，这是让实现符合文档，不是改契约 |
| `c868e4d` | 新增 `GET /v3/quota_status` | **加法式**。不改任何既有形状，主控暂不消费，因此不进 `[consumed]` |

`9808015` 的契约变更是把 `identity_limit_reached` 的触发条件**收窄**：
此前它数的是含墓碑的总血统数并以 `max_identities` 为界，而墓碑按 §4.3 必须保留，
于是每一轮身份轮换都让这个数单调增长，最终把一个完全健康的节点变成永久 503。
主控把这一位当作不可入账位的判断**不需要改**——收窄之后它只在真正不可信时置位。

但它让 `docs/operations.md` §3.3 的一段话变成了错的：那段告诉运维「SIGHUP 扩容
会静默停掉计费」，据此把账号池扩容列为最容易踩到的场景。已按新语义改写，
并保留原文与改写理由。**这是本次核对唯一的实质产出**——
如果只是把锁里的哈希改掉让用例变绿，这段会一直误导下去。

复核方式：逐文件摘要与导出文档摘要均由脚本重算而非手抄；
`cargo test --workspace --locked` 全绿。
