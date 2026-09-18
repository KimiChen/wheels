# M0 开工提示词

给新会话用的起手提示词。直接复制下面代码块里的内容。

**它刻意不复述约束内容，只给编号和位置。** 这是踩出来的：
把上游 README 的 `schema_version` 转抄进计划书，结果抄到了一个已经与代码漂移的值，
按它实现会拒绝每一份真实快照。`integration-contract.md` 的「消费 fixtures 而非转抄文档」
就是为此立的——同一条纪律对提示词同样成立。

---

```markdown
在 wheels 仓库里，完成子项目 sing-box-plus-manager 的 M0。

## 先读这些，它们是权威，不要以本提示词的转述为准

- `sing-box-plus-manager/README.md` —— 计划书，**正文即规范**。约束 C1–C35、决策 D1–D22、
  风险 R1–R13 都在里面，每条都有编号，实现时按编号对照。
- `sing-box-plus-manager/docs/data-model.md` —— 表清单、结算事务 11 步、**SQLite 数值编码契约（§3.1）**、
  归桶规则、13 条验收不变量。
- `sing-box-plus-manager/docs/integration-contract.md` —— 跨仓库契约：该消费节点侧哪些 fixture、
  为什么不能转抄文档。
- `sing-box-plus-manager/docs/operations.md`、`sing-box-plus-manager/docs/threat-model.md` —— 运维手册与信任边界。
- 节点侧契约以 `sing-box-plus` 的**源码**为准：`internal/userstats/` 下的
  `snapshot.go` / `quota_endpoint.go` / `quota.go` / `registry.go` / `audit.go`。
  该项目的 README 曾与代码漂移过，**冲突时以代码为准并回报**。

## 目标：M0

交付：仓库骨架、配置解析、SQLite 建库与无损数值编码、按 `sing-box-plus` 计量样例
新建的假节点 UDS 服务；**夹具还须实现配额端点并向测试暴露当前生效的配额表**。

完成标准：`cargo test` 全绿；版本错配与 health 多/少字段各覆盖一次，输入摘要锁定；
u64/u128 存储往返与事务回滚通过；能复现 409 的两种成因、响应丢失与延迟到达。

技术选型已定（§5、D8、D9）：Rust + axum + sqlx/SQLite 单写者 + rustls/ring，
单二进制 + systemd，无构建步骤的静态前端。**不要重新选型。**
**不做迁移功能**（D22）：`schema/` 下的 DDL 一次性建出，schema 变更靠重建数据库。

## 最容易做错的地方，逐条核对

1. **`schema_version` 是 `3`**，以 `snapshot.go` 的 Go 常量为准。路由 `/v3/snapshot`，
   `/v1` 与 `/v2` 恒 404。读到 1 或 2 整份拒绝（C10）。
2. **`health` 是恰好四位的闭集**（C4）：多一个键或少一个键都整份拒绝。
   前三位任一为真即不可入账；`audit_dropped` 告警但照常入账。
3. **u64 全程不经 `Number` / `f64`**（C3）。SQLite 的文本编码、补零位数、
   禁止 SQL `SUM`/CAST 的规则在 `data-model.md` §3.1，**照做，不要自创**。
4. **配额全量清单缺条目 = 解除限额**，不是保持原值（C16、C30）。
   零额度必须显式下发；`pool ≤ 0` 时下发全零表而不是空表。
5. **`pool = quota − used` 用饱和减法**（C32）。用 u64 相减会下溢成 9.2 EB，
   而 `used > quota` 是预期状态。
6. **`remaining_bytes` 是 `int64` 不是 u64**，`epoch` 必须 ≥ 1（C31）。
7. **409 没有 discriminator**：`runtime_id` 不符与 `epoch` 未前进返回同一个响应体，
   必须取快照读回 `runtime_id` 才能分支（§4.5）。
8. **身份数含 `active=false` 且同 runtime 内只增不减**；SIGHUP 越界会置位
   粘滞的 `identity_limit_reached`，让该 runtime 余下时间全部不可入账（C29）。
9. **夹具不能拿 `mock_collector.py` 凑数**——它是审计导出协议的采集器，与快照接口无关。
   可复用的是 `sing-box-plus/tests/http_unix.py`（传输层）与
   `settlement_model.py`（baseline/include 参考）。

## 工作约定（仓库根 CLAUDE.md）

- 所有文件 UTF-8。
- **`wheels` 是公开 GitHub 仓库**：域名、IP、账号名、节点名、密钥一律不得写进代码或文档，
  示例数据用 RFC 保留段（`example.com` / `203.0.113.0/24`）。提交前跑一次脱敏 grep。
- 每轮改动后自己 `git pull` → `git add`（只加本轮相关文件）→ `git commit`，
  提交信息用中文、说明「为什么」而不只是「改了什么」。不要 push。

## 边界

- **不要重开已定的决策**。D1–D22 都有理由和代价写在表里；
  如果实现中发现某条决策站不住，**说出来并给出证据**，不要默默绕开。
- 已按 D20/D21/D3 移除的东西不要加回来：在途授信预留、自动发布编排、per-user 套餐、
  第二个 provider（`srp_v1`）。
- `sing-box-plus-manager/web/` 下是控制台的静态原型（12 页，已可点），M0 不动它。
```

---

## 后续里程碑怎么复用这份提示词

三段照抄不动：**「先读这些」**、**「工作约定」**、**「边界」**。
换掉的是「目标」和「最容易做错的地方」——后者按里程碑挑对应的坑：

| 里程碑 | 该强调的条目 |
| --- | --- |
| M1 采集入账 | C3 / C4 / C5 / C8 / C25 / C26；结算事务 11 步；D14 归属只追加 |
| M2 周期与配额 | C11–C17 / C30–C33；409 三步分支；D7 闭环权重；D21 改额度立即生效 |
| M4 Web 控制台 | C3 在图表层的延伸；`wsk.mount()`；`data-page-size`；指标卡大字放量级 |
| M5 IM 登录与告警 | D13 每请求重新求值；§4.9 的 OAuth 五项加固；R13「从零实现」 |
| M6 订阅与审计 | C23 只读已轮转文件；C35 归属裁剪先于聚合分页；D18 的口径与命名 |

**每次都要带上的一句**：节点侧契约以源码为准，README 可能漂移。
这条在本项目已经兑现过两次——一次是 `schema_version`，一次是审计的轮转触发条件。
