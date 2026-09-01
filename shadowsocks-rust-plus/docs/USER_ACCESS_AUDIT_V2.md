# shadowsocks-rust-plus 用户成功访问审计 · 待办、审阅与处置记录（V2）

> 文档定位：**当前活动的待办清单与本轮审阅处置记录**。记录截至 2026-08-30 仍未解决、未执行、需要决策，
> 或在本轮审阅中发现并已修复的事项。
>
> 与 [`USER_ACCESS_AUDIT.md`](USER_ACCESS_AUDIT.md) 的关系：
>
> - 那份是**历史文档**，保存规范合同（v8，第 1–16 节）与第 1–8 轮审计、整改及 Linux 实装验收的
>   完整过程记录（第 17–30 节）。合同条文仍以那份为准，本文不复制、不改写合同。
> - 本文保留本轮新增问题的根因、修复和验证边界；已闭合项明确标注“已修复”，避免与待办混淆。
>
> 基线：overlay `main`，规范版本 v8，`patches/0003-user-audit.patch` 与
> `upstream.lock` 的 `prepared_tree_sha256` 一致（`250b6a7a…`，2026-09-01 四轮复核新增 m-245 后更新；
> 这一行由 `tests/test_docs_consistency.py` 的 `test_declared_anchor_prefix_matches_upstream_lock` 绑定）。

## 1. 当前真机验收状态（背景）

2026-08-29/30 在 Debian 13 (trixie) 节点按 `packaging/README.md` 完整实装一次，README 五步全部
通过，详见历史文档第 30 节。**真机基线已证实**：两次独立 musl 构建产物字节一致、签名与独立验签
通过、§11 权限模型逐项一致、`C-2`/`C-4`/`C-7` 三条 packaging critical 修复成立、
`cargo test -p shadowsocks-auditd` 102 passed/0 failed（原 99 + 已交付的三条回归）、
`tests/integration_audit.py` 端到端通过。

`31993e0` 交付时自述「最终目标为 107 条、最终源码尚未在 Linux 真机复跑」。两处都已更正：
实际新增 **6** 条而非 5 条（漏数了 m-226 的 `export_deadline_is_checked_against_overflow`），
即 108；**该源码已由本轮审阅在 Linux 真机跑通**，逐目标结果见 §3.2。

**这不等于第 16 节验收通过**，原因见下文第 2、3 节。

## 2. 审阅发现与处置

### m-142（minor，已修复）`Instant` 算术下溢模式未纳入静态护栏

- **位置**：`tests/check_audit_static.py`（全文无 `Instant`/`Duration` 相关规则）。
- **背景**：`C-6` 是 `Instant::now() - Duration` 在开机 60 秒内下溢 panic，`panic=abort` 下击落
  整个进程。该缺陷由第五轮发现、第六轮修复，但现有静态护栏抓不到同类模式。
- **修复**：`tests/check_audit_static.py` 新增 `INSTANT_ARITHMETIC` 规则——审计路径的非测试代码里
  不得出现裸的 `Instant` 加减，必须走 checked 助手（`deadline_after`、`rate_limit_stamp_*` 一族）。
  当前树零误报；把 `export.rs` 或 `user_audit.rs` 任一处改回裸加法即报错。它同时补上了 `m-226`
  缺失的生产调用点绑定（85fe4c2）。
- 出处：历史文档 §23.6 `m-142`、§27.5 `m-170`、§27.4「遗留未修」。

### m-219（minor，已修复）`verify.sh` 从环境变量重算覆盖面结论，两个方向都会失配

- **位置**：`scripts/verify.sh` 结尾的结论分支。
- **现象**：`test.sh` 自己知道本次到底跑了什么（`auditd_crate_checked`、`auditd_runtime_available`），
  但 `verify.sh` 不读这个结果，而是从 `SHADOWSOCKS_REQUIRE_AUDIT_TARGET` 与 target 是否存在重新推断。
  两个方向都会错：`=0` 且 target 其实存在时误报「覆盖面不完整」（保守方向）；变量未设置、target
  已安装时，`test.sh` 如实打印「auditd Linux runtime 未在当前主机执行」，而 `verify.sh` 打印的
  「验证完成：……均通过」一个字都不提缺失的 runtime 覆盖面（乐观方向）。
- **影响**：只影响措辞，两条分支都退出 0，Linux 全量验收本就是另一道发布前置。
- **修复**：`test.sh --coverage-status <JSON>` 记录本次实际执行的 auditd crate/runtime 状态；`verify.sh`
  严格解析该 JSON，不再从策略环境变量重算结论，并修复无 `.env` 时非法 `NO_DOTENV` 值可能被吞掉的问题。
- **回归与验证**：`tests/test_script_switches.py` 覆盖严格 JSON、篡改/不一致记录、无 `.env` 三态开关以及
  fake test 分支（12 tests passed）；`bash -n scripts/*.sh`、与发布/文档测试联合 69 tests passed。
- 出处：本轮对 `M-72` 的复核；修复已写入工作树并纳入最终 `0003` 旁路门禁。

### m-220（minor，已修复）exporter 的 lingering close 期间仍占着 client permit

- **位置**：`crates/shadowsocks-service/src/server/user_stats.rs` 的 `handle_client` 直接错误响应路径。
- **现象**：`write_direct_json` 的有界 drain 最多再占 100 毫秒，这段时间 `OwnedSemaphorePermit`
  仍被持有。被 413 大量拒绝时，`max_concurrent_clients` 个 permit 会被 drain 占住，正常连接吞吐下降。
- **对比**：busy（429）路径本来就不占 client permit——它用独立的 `busy_response_semaphore`。
- **修复**：直接错误响应写完后立即释放普通 client permit，drain 交给独立 semaphore（最多 32 个）
  的后台 worker；worker 满时直接关闭连接。`64 KiB / 100 ms` 两个 drain 上界保持不变。
- **回归与验证**：`detached_lingering_close_releases_client_permit_and_is_bounded` 与
  `direct_error_lingering_does_not_hold_client_permit` 已加入。
- **本轮审阅更正**：这两条只绑住了「提前释放 permit」那一半。变异检验（macOS，49 条 user_stats 用例）——
  ① 去掉 worker 上界改成无条件 spawn，**全绿**；② 把 worker 体内的 drain 换成 `shutdown` + `drop`，即对
  413 路径完整复原 `M-73` 的缺陷，**也全绿**。原因是守 `M-73` 的两条旧用例调的是
  `write_direct_json`，而该函数在生产代码里已只剩 429 busy 一个调用点。已补
  `a_detached_direct_error_drains_the_unread_request`（659f149）与
  `an_exhausted_lingering_pool_closes_without_draining`（476c411），两条变异均转红。
  另外 `lingering_close` 的文档在改造后仍写着无条件的「干净 EOF」，而 worker 池耗尽时一次 drain 都不跑，
  已更正（ad8bcfd）。自述里「67/67」这个数在 macOS 上即可复现，与 §1「尚未在 Linux 复跑」矛盾。
- 出处：本轮对 `M-73` 的复核（`M-73` 中成立的那半已修）。

### m-221（minor，已修复）恢复期 ACKed quarantine 丢失来源，误计未确认损失

- **位置**：`recover_layout` 对 `acked/` 中 body/meta 损坏对象的隔离，以及后续 `evict_quarantine_locked`。
- **根因**：恢复期按 `inspect_batch_dir` 隔离对象时，旧 basename 只写 `batch-corrupt-*`，驱逐阶段重新构造
  pending entry 时无法知道对象原先来自 `acked/`，于是把已经收到 ACK 的 `event_count` 计入
  `evicted_unacked_records`。
- **影响**：健康计数高报，告警把已交付事件误报成未确认丢失；gap 的 `lost_events` 仍按损坏对象 metadata
  自称的总数保留，符合 §9.5，不能用该字段替代来源判断。
- **修复**：恢复期按父目录写入持久 basename label：`acked-corrupt-*` 或 `batch-corrupt-*`；驱逐与崩溃恢复
  统一按 label 守卫计账。
- **回归与验证**：`pending_quarantine_recovery_preserves_acked_source_label`、
  `evicting_an_acked_quarantine_object_reports_no_unacknowledged_loss`；最终 Linux 测试计数见 §3.2。

### m-222（minor，已修复）tombstone 已提交后 marker cleanup 失败破坏事务语义

- **位置**：`remove_tombstone_locked`、`replace_tombstone_locked` 与启动时 pending-marker 合并。
- **根因**：aggregate ledger 已经 durable 后，私有 marker 的 unlink/fsync 失败仍被当作整个事务失败；重启时
  重新把已完成的 marker 放回 aggregate ledger，可能再次执行破坏性操作并重复健康计账。
- **影响**：同一 gap 可能被重复恢复/计数，或因错误回滚而遗留不可解释的 pending 状态。
- **修复**：引入 `TombstoneCommit` 区分 ledger 提交和 marker 清理；ledger 成功后保留提交后的内存状态，cleanup
  错误只作为可观测的后续错误；已存在 durable gap 的 stale marker 不再重新加入 aggregate ledger。
- **回归与验证**：`committed_quarantine_eviction_counts_once_when_marker_cleanup_fails`、
  `pending_quarantine_survives_corrupt_tombstone_ledger`；Linux 目标交叉检查通过。

### m-223（minor，已修复）旧版 `batch-corrupt-*` 无法识别 ACK 来源

- **根因**：升级前已经落盘的 quarantine 对象没有 `acked-corrupt` label，单靠新命名规则会把历史 ACKed 对象
  当作未确认对象。
- **修复**：使用 pending 中的 `batch_id + body_sha256` 与 durable `AckedReceipt` 做保守匹配；
  只有匹配成功才抑制 `evicted_unacked_records`，缺少可靠字段时维持 fail-closed 计账。
- **本轮审阅更正两处**：(1) 这条 fallback **并不限于 legacy 对象**——`QUARANTINE_LABEL_UNACKED` 的字面值
  就是 `"batch-corrupt"`，与自述中的 legacy 名字完全相同，代码与文档里没有任何东西把它限制在升级前落盘的
  对象上。今天不出假阴性，靠的是 `ack()` 先做 rename+两次 `sync_dir`、之后才写 receipt，且 `batch_id` 是
  128-bit 随机值——这个时序不变量既无注释也无测试守护。(2) 「保守匹配」这条主张原先只有正例，三种放宽
  （任意 receipt / 只比 batch_id / 只比 body_sha256）下 70 条用例全绿，已补负例（00444bb）。
  另需注意兼容窗口只有 7 天（`RECEIPT_RETENTION_SECONDS`），超期后 legacy 对象会退回按未确认计账
  ——方向是 fail-closed 的高报，可接受。
- **回归与验证**：`legacy_acked_quarantine_uses_the_durable_ack_receipt`；与 m-221 一起纳入最终
  `0003-user-audit.patch`，并通过 Linux 目标 `cargo check --locked -p shadowsocks-auditd --lib --tests`。

### m-224（minor，已修复）eviction ledger 重试造成重复计账

- **根因**：`reconcile_tombstones_locked` 在 pending→receipt 的 aggregate ledger 替换提交前就增加
  `evicted_unacked_records`；若写盘失败并回滚 pending，下一轮重试会再次增加同一批次的计数。
- **修复**：把计数移动到 durable pending removal/replacement 成功之后；marker cleanup 失败也不回滚已提交的
  ledger 状态，下一轮只重试 cleanup，不重复收费。
- **回归与验证**：`quarantine_eviction_ledger_retry_does_not_double_count`、
  `recovered_eviction_counts_lost_records_once_across_a_persistence_failure`；Linux 目标交叉检查通过。

### m-225（文档，已修复）README 对缺失 auditd target 的策略表述矛盾

- **现象**：README 同时声称缺失 target 会继续执行并打印“未验证”，又声称默认 fail-closed；与脚本实际
  默认行为不一致。
- **修复**：明确 target 缺失时默认 fail-closed，只有显式设置 `SHADOWSOCKS_REQUIRE_AUDIT_TARGET=0` 才继续其余
  检查并报告“未验证”。脚本行为未被放宽。
- **验证**：脚本开关回归与文档一致性测试通过；该条不改变运行时合同。

### m-226（minor，已修复）export deadline 的 `Instant` 加法可能在边界时钟上溢出

- **位置**：`crates/shadowsocks-auditd/src/export.rs` 的 `read_request`。
- **根因**：请求读取截止时间直接使用 `Instant::now() + HTTP_DEADLINE`；在平台时钟表示范围接近上界时，
  `Instant` 加法可能 panic，审计进程的 `panic=abort` 配置会把一次畸形请求升级为进程退出。
- **修复**：通过 `checked_add` 封装 `deadline_after`；无法表示的间隔按“当前时刻已到期”处理，保持
  fail-closed，同时不改变正常 100 ms 请求窗口。
- **同轮耐久性边界**：tombstone aggregate ledger 的 rename 已发生后，后续 fsync/计量错误统一进入 sticky
  `DurabilityUncertain`；内存中的 post-state 不回滚。启动 salvage 对 `AfterRename` 同样拒绝继续服务并交由
  supervisor 重试；`BeforeRename` 仍按 degraded 计账。这样不会把“文件已替换但目录耐久性未知”误报成普通可重试失败。
- **本轮审阅更正**：自述把它写成「一次畸形请求可以打死审计进程」并称窗口是「100 ms」，两处都不准确。
  `HTTP_DEADLINE` 是编译期常量 **5 秒**（`EXPORT_DEADLINE_SECONDS`），请求内容影响不了这次加法；实测
  `Instant::now()` 距表示上界还有约 2.9e11 年，在 Linux/macOS 上该 panic **不可达**。这是无害加固，
  不是修一个可触发的缺陷。原用例只对助手函数断言——把生产调用点改回裸加法（即整个撤销 m-226），
  auditd 全量用例仍全绿；该缺口已由 `m-142` 的静态规则一并堵上（85fe4c2）。
- **回归与验证**：`export_deadline_is_checked_against_overflow` 覆盖正常与 `Duration::MAX` 两条路径；
  `tombstone_after_rename_failure_keeps_post_commit_state` 覆盖 add/prune/remove/replace 四个入口。当前
  macOS 临时移除 Linux-only compile gate 后的 service 测试为 122/122；auditd native tests 仍受 Linux libc
  API 限制，未在 macOS 执行。`x86_64-unknown-linux-gnu` 的 `cargo check --all-targets` 已通过；Linux 真机
  新增用例已于 2026-08-30 复跑（§3.3）并在后续每轮最终态复核中随门禁一并复跑（§2 末两节）。

### m-227（minor，已修复）GapFallback 的未知边界与并发计数被错误合并

- **位置**：`crates/shadowsocks-service/src/server/user_audit.rs` 的 `GapSlot`、`GapFallback` 和
  `combine_gap_snapshots`。
- **根因**：fallback 锁竞争时原实现只设置一个 sticky `metadata_unknown`，并把 count 写进同一个原子；
  `try_take` 与生产者交错时，未知 count 可能被配到另一代的已知边界。后续合并还把 `None`/零时间当成中性值，
  能重新制造并不存在的 sequence/time 范围。
- **修复**：增加独立 `unknown_count`，为 sequence/time 边界保留逐字段 unknown 位，并让 slot、fallback、
  requeue 与 snapshot combine 贯穿传播；未知 count 仍保留并按饱和规则计数，未知边界不再被猜测。
- **回归与验证**：`fallback_unknown_count_keeps_metadata_unknown`、`requeued_unknown_metadata_stays_unknown_in_accumulators`
  和 `combining_snapshots_preserves_unknown_boundaries` 三条 service 回归已加入。macOS 临时移除 Linux-only
  compile gate 的 feature-on service 测试为 122/122；这不是 Linux auditd runtime 的实机结果。

### m-228（minor，已修复）`fetch_update` 重试把 speculative saturation 记成真实饱和

- **位置**：`saturating_add_atomic` 及 producer-gap 健康计数。
- **根因**：旧实现把闭包每次试算的 `next == u64::MAX` 写入外部 flag；`fetch_update` 的 CAS 失败后会再次调用
  闭包，早期失败尝试留下的 flag 可能把尚未达到上限的当前代标成 saturated，进而错误触发 sticky degraded health。
- **修复**：只依据成功 CAS 返回的 previous 值计算本次是否达到上限，不采纳 speculative 尝试；并以独立回归锁定
  一次确定性的失败 CAS/重试交错。
- **回归与验证**：`saturating_atomic_add_ignores_saturation_from_failed_cas` 与
  `accumulator_take_propagates_unknown_fallback_bounds` 两条 service 回归已加入；当时以 122/122 的 macOS 临时结果为边界，
  Linux 真机复跑已于 2026-08-30 完成（§3.3），并随后续每轮最终态门禁复跑（§2 末两节）。

### 本轮审阅新增的条目（m-229 – m-234，**已全部修复**）

上一轮的 `31993e0` 把十条修复捆在一个提交里、提交说明只有一行标题、且自述「最终源码尚未在 Linux
真机复跑」。本轮逐条核实后，**行为层面没有发现功能缺陷**——事务语义、permit 释放、饱和计账都站得住，
Linux 全量门禁也已跑通。问题集中在**回归覆盖面**：多处自述的「已绑定」经变异检验并不成立。已修的见
第 6 节；以下六条随后也已逐条修复，每条一个提交，见第 6 节末尾。

| # | 事项 | 变异证据（修复前） | 处置 |
| --- | --- | --- | --- |
| m-229 | `m-227` 的核心改动（独立 `unknown_count`）无鉴别性绑定；`approximate_count`、`is_nonempty` 的新增项与两个时间 unknown 位同样无绑定 | 把 `merge`/`try_take` 精确退回旧的「sticky bool + 共用 count」设计，124 条 service 用例全绿；单独删掉 `approximate_count` 或 `is_nonempty` 里的 `unknown_count` 项，也全绿。后者的真实后果是关机 drain 判空时静默丢弃只挂在无锁 fallback 上的计数 | `46584ca` 补两条鉴别性用例，四个变异全部转红 |
| m-230 | `m-222` 的**启动期**那一半零覆盖 | 把 salvage 里删掉的 `tombstones.push(marker)` 加回去、或把新增的 `AfterRename => DurabilityUncertain` 换回旧的计数递增，整套 auditd 用例都全绿 | `7da8935` 加两个启动期 thread-local 故障开关并补两条用例 |
| m-231 | `m-224` 的「先提交后记账」四个提交点里仍有两个无绑定 | 把 `evict_sealed_locked` 与 reconcile 的 QuarantinePending 腿的 `commit.finish()?` 挪到记账之前，全绿（`evict_quarantine_locked` 那个点已由 989a0aa 绑住） | `ab4a2a4` 为另外两个提交点各补一条用例 |
| m-232 | lingering worker 用 `tokio::spawn`，不在 `run()` 的 `JoinSet` 里 | `run()` 被 abort 后最多 32 个任务带着 fd 再活 ≤100 ms。有界且短，但打破了「exporter 的所有 client I/O 任务由 run() 拥有」这条既有性质 | `e0be754` 改为经 channel 交回接受循环，`workers.spawn` 拥有 |
| m-233 | 孤儿 marker 只在启动时清理 | marker 清理失败后一直留在盘上；运行期 reconcile 只看内存 ledger。若该 gap 的批次已被 ACK 并过了 24 小时保留期，下次启动时 `gap_already_durable` 变假，marker 会被复活成 pending 并用同一固定 ID 再写一条 `spool_gap`。非本轮引入，但与 `m-222` 的自述直接相关 | `b192938` 清理失败记入重试集合，每次 reconcile 重试 |
| m-234 | 提交后的纯记账 `stat` 失败被升级成 sticky `DurabilityUncertain` 与进程退出 | `persist_tombstones_locked` 在 rename + 目录 fsync **都已成功**之后，若 `file_len` 失败仍返回 `AfterRename`，daemon 关停退出。`OPERATIONS.md` 把 fail-closed 退出限定在「写屏障结果无法判定时」，这一格不属于该范围。与既有 `persist_state_locked` 同构，但 tombstone 是每次 ACK/驱逐都要写的高频路径 | `9d8b7bc` 屏障已成功时不再升级为 `DurabilityUncertain`。**范围理由更正**（2026-09-01 复核）：state.json 每条 record 都要持久化（§9.3 lock-step），频率不低于 tombstones.json，「高频」不是有效的范围理由；state.json 侧的同型残留记为 m-235，见下节 |

另记两条交付层面的观察（不影响运行时）：`31993e0` 一次提交涵盖 10 条修复、正文为空、无
`Co-Authored-By`，与仓库「一个问题一个 commit + 根因/修复/绑定/变异检验」的既有约定不符；`m-221`、
`m-222`、`m-224` 的「回归与验证」各混入一条上一轮交付、本轮逐字节未改的旧用例，读者会高估其绑定强度。

### 2026-09-01 复核：六个修复提交逐条核实，新增 m-235（已修复）

复核对象为 `8b7b534` 之后同事交付的六个修复提交（`e0be754`–`46584ca`）与文档提交 `e8258bb`；
物化的七个提交点里，第七个就是基线 `8b7b534` 本身。
**六条修复全部成立。** 逐项核实结果：
（本节原文同时宣告「未发现新的行为缺陷」。该宣告**已被下一节的 m-236 证伪**：
同族第四个记账点漏改，属这一轮本应一并看到的同型残留，故此处删去该宣告。）

- **提交分区**：物化七个提交点的源码树逐对比较，每个提交只动其声明范围内的一个文件
  （m-232→`user_stats.rs`；m-230/m-233/m-231/m-234→`spool.rs`；m-229→`user_audit.rs`）；
  七个点的 `prepared_tree_sha256` 全部与干净重放一致，最终态 `b8c7d6ac`；补丁规范性门禁绿。
  「重建后最终态与原工作树逐字节一致」的自述无法独立复证，但逐提交哈希链 + 单文件分区 +
  最终态一致三者合起来覆盖了它的实质。
- **Linux 门禁**（Debian 13 / rustc 1.98.0 / `10.0.1.3`）：八条命令全部 `EXIT=0`，计数与自述逐一对上：
  auditd 116、protocol 25、service lib 134（user-audit）/ 71（feature-off）、四个集成目标 4/4/1/1。
- **变异检验**：同事自述的十条变异全部独立复核转红——m-229 四条（sticky-bool 退回、删两个时间
  unknown 位、删 `approximate_count` 项、删 `is_nonempty` 项）、m-230 两条（复活 marker、salvage
  退回计数继续）、m-231 两条（两个提交点各交换一次顺序）、m-233（摘掉 reconcile 里的重试）、
  m-234（恢复旧升级）；m-232 的 `tokio::spawn` 逃逸变异在 macOS 单独复核转红。
- **审阅方脚手架教训**（与同事上一条记录的三处同类，值得并列）：十个变异共享一个
  `CARGO_TARGET_DIR` 连跑时，首轮有六条误绿——同一 package 在相同 target dir 下复用了前一棵
  变异树的测试二进制；逐条 `cargo clean -p` 强制重建后六条全部转红。变异检验必须先证明被测
  代码真的重新编译过，否则「全绿」什么都不能说明。

**m-235（minor，已修复）`persist_state_locked` 把屏障成功后的纯记账失败升级成致命退出——m-234 的同型残留**

- **位置**：`crates/shadowsocks-auditd/src/spool.rs` 的 `persist_state_locked`。
- **根因**：与 m-234 完全同型。`persist_state_atomic` 完整成功（rename + 目录 fsync 均已确认）后，
  `update_path_size`（一次事后 `stat`）失败仍返回 `AfterRename`，`write_record_locked`（`spool.rs:2435`，
  升级发生在其 `AfterRename` 分支 2493 行；仓库中没有名为 accept_record_locked 的函数（此处刻意不加反引号：
  §2 的函数名锚点已被 `test_docs_consistency.py` 校验存在性））随即升级为
  sticky `DurabilityUncertain`，fatal watcher 关停整个 daemon。`OPERATIONS.md` 把 fail-closed 退出限定在
  「写屏障结果无法判定时」，屏障返回 `Ok` 时结果已确定，这一格不属于该范围。state.json 在 §9.3 的
  lock-step 提交里**每条 record** 都重写，暴露面高于 m-234 修的 tombstones.json——m-234 自述的范围
  理由方向是反的。该行为由 `committed_state_errors_keep_the_record_and_never_reuse_epoch_sequence`
  的 `path-size-accounting` 用例绑定，但它源自初版交付（`76f80fc`）的实现惯性，不是规格决策。
- **修复**：与 m-234 对齐——屏障已确定成功时，记账失败只清 `spool_bytes_known`（强制下次容量决策前
  全量重测）并记 `storage_rejected_attempts`，返回原 `Ok`；屏障自身返回 `AfterRename` 时维持原有
  升级路径不变。原用例的 `path-size-accounting` 分支拆到新用例
  `post_commit_state_accounting_failure_degrades_without_stopping`。**这次拆分不是等价重组**：
  新用例当时只覆盖「非致命降级 + 游标 durable + 服务继续」，原分支在盘上 wrapper 的
  epoch/sequence、内存 `next_sequence`/`open_meta.event_count`、重启后 epoch 连续、以及
  `drain_record_positions` 上 (epoch, sequence) 不复用（正是原用例名字里
  `never_reuse_epoch_sequence` 的落点）这四类断言**一条都没有带过**，还留下一个退化的
  单元素循环。四类断言与循环已由 m-237 补回/摊平，见下一节。`after-commit` 分支断言不变。
- **回归与验证**：Linux auditd 117 passed / 0 failed（116 + 新增一条）；八条门禁在最终态
  （`58c5e777`）全部 `EXIT=0`；macOS `verify.sh` 通过。变异检验：把记账失败改回
  `return Err(AtomicWriteError::AfterRename(..))`，新用例立刻转红。


### 2026-09-01 二次复核：核实 m-235，新增 m-236、m-237（均已修复）

复核对象为上一节之后交付的修复提交 `0ec6c2c`（m-235）与文档提交 `1e5d112`。

- **提交范围**：`0ec6c2c` 只改 `crates/shadowsocks-auditd/src/spool.rs` 一个文件；补丁规范性门禁
  （`test_audit_packaging.py` 的单 stanza 规范化检查）绿；起点 `prepared_tree_sha256` `58c5e777…`
  与干净重放一致。
- **m-235 成立**：独立复证。`persist_state_atomic` 返回 `Ok` 表示 rename 与目录 fsync 都已确认，
  此后 `update_path_size` 的一次事后 `stat` 失败纯属记账问题，`OPERATIONS.md` 的 fail-closed
  退出限定在「写屏障结果无法判定时」，不覆盖这一格。修复方式与 m-234 对齐，范围完整；对
  m-234 自述范围理由的更正（state.json 在 §9.3 lock-step 里每条 record 都重写，暴露面不低于
  tombstones.json）也成立。
- **验证工具链偏差（方法学，非缺陷）**：上一节的 Linux 复跑用的是 rustc **1.98.0**，而
  `packaging/release-toolchain.lock` 钉的是 `RELEASE_RUSTC_VERSION=1.97.0`。测试门禁不受该 lock
  约束（它只约束 §15.1 的发布构建），但「门禁在锁定工具链上绿」与「门禁在某个更新的工具链上绿」
  不是同一条结论。本次二次复核的最终态门禁已在 **1.97.0** 上复跑，两者现均有记录。

**m-236（minor，已修复）pending marker 写路径的记账失败不让增量字节索引失效——m-235 同族的第四个点**

- **位置**：`crates/shadowsocks-auditd/src/spool.rs` 的 `persist_tombstone_pending_marker_locked`。
- **根因**：`update_path_size` 一共有四个调用点，前三个（`persist_state_locked`、
  `persist_tombstones_locked`、`persist_recovery_gap_marker_locked`）在记账失败时都会同时
  `inner.spool_bytes_known = false` + `mark_storage_rejection`，唯独这一个只做了后者：

  | 调用点 | 清 `spool_bytes_known` | 记 storage rejection | 记账失败返回 |
  | --- | --- | --- | --- |
  | `persist_state_locked` | 是 | 是 | 原 `result` |
  | `persist_tombstones_locked` | 是 | 是 | 原 `result` |
  | `persist_recovery_gap_marker_locked` | 是 | 是 | `Err` |
  | `persist_tombstone_pending_marker_locked` | **否** | 是 | `Err` |

  marker 此刻可能已经落盘而其大小无法确定，`spool_bytes` 仍被标记为 known，于是增量字节索引
  带着一个**永久偏移**继续参与 `capacity_ok` 的容量判定——低估则 spool 越过配额仍继续收，
  高估则未满即开始拒收。它自己的删除对手方 `remove_tombstone_pending_marker_locked` 和同型
  写入方 `persist_recovery_gap_marker_locked` 的注释里都写明了这条理由，只是写入侧漏掉了。
- **修复**：补上 `inner.spool_bytes_known = false;`，强制下次容量决策前走一次
  `refresh_spool_bytes_locked` 全量重测；返回值语义（仍返回 `Err`）不变。
- **回归与验证**：新增 `a_pending_marker_write_accounting_failure_invalidates_the_byte_index`，
  配套新增 `TestFaults::tombstone_pending_marker_accounting` 注入点，断言注入记账失败后
  `spool_bytes_known` 转 false 且 storage rejection 计数递增。Linux auditd 118 passed / 0 failed。
  **变异检验**：删掉新增的 `inner.spool_bytes_known = false;`，该用例转红（117 passed; 1 failed），
  恢复后回绿；变异用独立 `CARGO_TARGET_DIR` 编译，已排除上一节记录的旧二进制复用陷阱。
- **提交**：`0aba624`，锚点 `58c5e777…` → `8d6f5b5b…`。

**m-237（minor，已修复）m-235 拆分测试时丢掉四类断言，并留下一个退化的单元素循环**

- **位置**：`crates/shadowsocks-auditd/src/spool.rs` 的
  `committed_state_errors_keep_the_record_and_never_reuse_epoch_sequence` 与拆出的
  `post_commit_state_accounting_failure_degrades_without_stopping`。
- **根因**：m-235 把 `path-size-accounting` 分支从原用例拆出去时，新用例只断言了「非致命降级 +
  游标 durable + 后续 append 可继续」，原用例对该分支覆盖的四类断言没有跟着搬过去：落盘
  wrapper 的 epoch/sequence、内存里的 `next_sequence` 与 `open_meta.event_count`、重启后
  epoch 不变而 sequence 续接、drain 出来的 `(epoch, sequence)` 两两不重复。拆分是重构，
  不应减少覆盖面。同时原用例的循环被裁成
  `for (case, fail_after_commit) in [("after-commit", true)]`——只剩一个元素，退化成噪声。
- **修复**：在新用例里补回上述四类断言（按同一分支的原语义重写，不是照抄），并把退化循环
  内联展开。无生产代码改动。
- **回归与验证**：Linux auditd 118 passed / 0 failed。**变异检验不具鉴别性，如实记录**：我构造的
  变异（在提交后把 `inner.open_meta.event_count` 清零）让**修复前与修复后两个版本都转红**——
  修复前的版本经由别的用例同样能抓到它，因此该变异不能证明补回的断言有独立价值。此条的
  依据是「拆分导致的覆盖面客观丢失」这一可直接比对的事实，不是变异背书。
- **提交**：`51d17bf`，锚点 `8d6f5b5b…` → `849ace0b…`。

**m-238（nit，已修复）m-234 的代码注释仍写着与已更正范围理由相反的方向**

- **位置**：`crates/shadowsocks-auditd/src/spool.rs` 的 `persist_tombstones_locked` 记账失败分支。
- **根因**：m-234 的注释原文是「`tombstones.json` is rewritten on every ACK and every eviction,
  so that exposure is far larger than `state.json`'s」。m-235 已经证明这个方向是反的，但更正只落到
  台账和提交正文，源码里被否定的那句原封不动地留着——同一文件里紧挨的两段孪生注释因此互相
  矛盾，后续读者按注释理解范围理由会被误导。
- **修复**：删掉这半句纯频率比较的从句，写出正确方向，并点明频率不是判据——判据是屏障结果
  是否已确定。
- **提交**：`718d937`，锚点 `849ace0b…` → `523b96d9…`。变异检验不适用（改动全部位于注释行）。

**m-239（nit，已修复）`mark_storage_rejection` 的 `non_gap` 实参在四处记账用例上均无绑定**

- **位置**：`spool.rs` 的四条记账故障用例（`persist_state_locked`、`persist_tombstones_locked`、
  `persist_recovery_gap_marker_locked`、`persist_tombstone_pending_marker_locked` 各一条）。
- **根因**：四条用例都只断言 `storage_rejected_attempts > 0`。把修复块里的
  `mark_storage_rejection(inner, true)` 改成 `(inner, false)`，全部用例照样绿。这个实参不是无关
  紧要的：`non_gap_degraded` 的唯一读者是 `refresh_degraded_after_ack_locked`，它靠这个标志
  拦住「一次 gap 的 ACK 把无关的存储拒绝静默清掉」。变异后的行为是——记账失败之后若有
  `spool_gap` 被 ACK，`degraded` 会被清回 false、health 从 degraded 变回 ok，这次存储拒绝只剩
  计数器留痕。当前代码写的是 `true`（正确），但没有任何用例守住它。这说明「m-235 的四条变异
  全杀」并不等于那段修复的每个决策都有绑定。
- **修复**：四条用例的 `storage_rejected_attempts > 0` 各补一对
  `assert!(inner.degraded)` + `assert!(inner.non_gap_degraded)`，并在注释里写明这是在绑定实参
  本身而不只是计数器。无生产代码改动。
- **回归与验证**：见下节「本轮最终态验证」。

**m-240（minor，已修复）本台账无任何门禁覆盖，开头的锚点声明已实际漂移**

- **根因**：`grep -rl USER_ACCESS_AUDIT_V2 tests/ scripts/` 零命中——本文件是这个项目唯一的问题
  溯源载体，却不被任何门禁读取。后果不是假设性的：文首「与 `upstream.lock` 的
  `prepared_tree_sha256` 一致（`849ace0b…`）」是一条可判真假的断言，而 m-238 改补丁后 lock 变成
  `523b96d9…`，没有任何东西拦住这次漂移。m-235 条目里写成不存在的 accept_record_locked
  那次属同一类：文档里的可判定断言无人校验。
- **修复**：`tests/test_docs_consistency.py` 新增 `AuditLedgerV2Tests` 两条门禁——
  ①文首声明的 8 位锚点前缀必须是 `upstream.lock` 的 `prepared_tree_sha256` 的前缀；
  ②台账里反引号包起来、以 `_locked` 结尾的函数名必须在 0003 补丁的增行里能找到 `fn <name>(`。
  加入时**两条都是红的**（分别报出 `849ace0b…` 的漂移和 `['accept_record_locked']`），修正台账后
  转绿。此后台账中提到「不存在的函数名 accept_record_locked」时一律不加反引号。
- **变异检验**：把文首前缀改成 `deadbeef…` → 锚点条转红；插入一个不存在的 foo_bar_locked（此处同样刻意不加反引号）→ 函数名条
  转红；两者恢复后均转绿。已实跑。
- **提交**：`f29363a`。

**m-241（minor，已修复）m-235 条目把用例拆分写成等价重组，掩盖了实际的断言丢失**

- **根因**：原文读起来是一次等价重组，实际新用例对四类断言一条都没有继承，连 drain 都没有，
  还留下退化的单元素循环而文档只字未提。读者会据此高估 m-235 的回归绑定强度——与台账自己
  在 m-221/m-222/m-224 批评过的「混入旧用例致读者高估绑定」是同一类问题。
- **修复**：如实写出丢失了哪四类断言、点名 `never_reuse_epoch_sequence` 这个落点、注明遗留的
  退化循环，并指向已把它们补回的 m-237。**提交**：`027b00f`。

**m-242（minor，已修复）§3.2 与 m-226/m-227 的「待复跑」措辞与 §3 首行「已复跑」正面冲突**

- **根因**：§3 引言把 §3.2 当作依据来源，首行已改成「已复跑」，而 §3.2 一字未动，仍写「本轮最终
  源码还需复跑新增用例」「最终补丁待 Linux 真机复跑」；其中「5 条 / 107」两个数早已被 §1 与
  §3.3 推翻。m-226、m-227 的「待复跑 / 尚未完成」同属这批陈账。
- **修复**：§3.2 点明该表是 2026-08-30 的基线快照而非当前状态并指向 §3.3 与 §2；被推翻的旧记法
  标注出处；m-226/m-227 改为陈述已复跑的时点。计数一个未改，只让每个数字挂到它成立的时点。
  **提交**：`fb9fd06`。

**m-243（nit，已修复）复核基线提交写成 `4ad0ffb`，实际是 `8b7b534`**

- **根因**：`e0be754^` 即 `8b7b534`；`4ad0ffb` 与它之间还夹着 `85fe4c2`（m-142）、`7f465c7` 等一批
  代码提交，按字面读会把它们一并算进复核范围，也说不清第七个物化点是谁。
- **修复**：基线改为 `8b7b534`，点明第七个物化点就是该基线本身。**提交**：`e3befa3`。

**两条交付纪律观察（历史提交，不回改）**

- `0ec6c2c` 与 `1e5d112` 都缺 `Co-Authored-By` 尾注。m-229–m-243 这条修复链上只有这两个提交
  没有；加倍尴尬的是 `1e5d112` 写进本文档的正是「`31993e0` …无 `Co-Authored-By`，与既有约定
  不符」这句判词。两者已在 `main` 上且有下游提交，不 rebase 重写。
- `0ec6c2c` 改了 `upstream.lock` 却没有「同一提交刷新 `prepared_tree_sha256`」这一行，是
  m-229–m-243 十余个改补丁的提交里唯一的例外；其「绑定」内容也折在修复段里，没有独立成段。
- 这两条都属「靠自觉」的约定。可门禁化的做法：对 `修复：`/`文档：` 开头的提交要求尾注存在
  `Co-Authored-By:`；改动 `patches/` 的提交要求正文含刷新说明。尚未实施。

**m-244（minor，已修复）台账按短哈希引用提交，而这些引用无门禁覆盖、且已实际失效两次**

- **根因**：台账 §2/§6 用反引号 7 位短哈希引用具体提交，是这份文档的主要溯源手段，但
  m-240 加的两条门禁只校验文首锚点前缀和 `*_locked` 函数名，**不校验提交号**。任何
  rebase / filter-branch / amend 都会让这些引用整片指向不存在的对象。已经踩到两次：
  ①把本轮十个未 push 提交的 `Co-Authored-By` 尾注改成 Kimi 时，提交内容一字未变但
  哈希全变，15 处引用同时失效；②更早的 `ee6829b` 写下的一个哈希是该提交在某次改写
  前的版本，此后一直悬空，直到这次全量扫描才被发现。这与 m-243 修的是同一类问题，
  区别只在于 m-243 是人写错、这一类是历史改写造成的整片失效。
- **修复**：`tests/test_docs_consistency.py` 的 `AuditLedgerV2Tests` 新增第三条
  `test_every_commit_hash_in_the_ledger_still_resolves`：台账中每个 7 位短哈希都必须
  能 `git rev-parse --verify` 解析，且必须是 `HEAD` 的祖先。非 git 检出时跳过。
  同时把因本次改写失效的 15 处引用按 commit subject 逐条改回（不靠记忆），并修掉
  `ee6829b` 留下的那处悬空引用。
- **变异检验**：①把一个引用改成无法解析的哈希 → 报「无法解析」转红；②把一个引用改成
  真实但不在当前历史上的悬空对象 → 报「不在当前历史上」转红；两者恢复后均转绿。已实跑。
- **顺带归正一处身份不一致**：本轮有八个提交的 author 邮箱被写成了与仓库 git user
  （`KimiChen <jianxieshui@gmail.com>`）不同的地址，已在同一次改写里统一。改写只动
  提交正文的尾注行与作者邮箱：新旧 HEAD 的 `git diff` 为空、树哈希一致，`origin/main`
  及更早的历史未被触及。

**本轮最终态验证（`ee27bfbb…`）**

- **Linux 门禁**（Debian 13 / `10.0.1.3` / **rustc 1.97.0**，即 `packaging/release-toolchain.lock`
  锁定的版本）：八条命令全部 `EXIT=0`——auditd **118**、protocol **25**、service lib
  **134**（user-audit）/ **71**（feature-off）、四个集成目标 **4/4/1/1**。
- **本地门禁**：`check_audit_static.py`、`test_fuzz_target.py`、`test_panic_abort.py` 与
  11 个 Python 门禁（docs/script-switches/release-artifact/audit-packaging/check-audit-static/
  cluster-users/http-unix/settlement/mock-collector/integration-audit/benchmark-audit）全绿；
  `prepare-source.sh` 干净重放的 `source-tree-sha256` 与 `upstream.lock` 一致，且与工作树逐文件相同。
- **变异纪律**：本轮所有变异一律「单一 `CARGO_TARGET_DIR` + 每段 `cargo clean -p shadowsocks-auditd`」，
  并在日志里逐段确认出现 `Compiling shadowsocks-auditd`，以排除上一节记录的旧二进制复用陷阱。

**本节顺带更正上一节文档的两处**：①「未发现新的行为缺陷」的宣告已按上文删去；
②m-235 条目里的 accept_record_locked 在仓库中不存在，实际是 `write_record_locked`
（`spool.rs:2435`，升级路径在其 `AfterRename` 分支 2493 行），已改。


## 3. 待执行的验证

以下为发布前置。§16 已收窄为 v8 门禁并有 Linux 基线结果（见 §3.2）；m-229–m-237 新增的 Linux 用例与
最终态已由 2026-09-01 两轮复核在 `10.0.1.3` 复跑全绿（见 §2 末两节与「本轮最终态验证」）。原始宽命令的失败记录见
§3.1；其余三项**从未在任何机器上执行过**：

| 项目 | 说明 | 阻塞因素 |
| --- | --- | --- |
| §16 收窄 Rust 门禁（v8：两条 workspace 命令 + ①②两类集成目标） | §16 验收项 | 已复跑：2026-09-01 二次复核在 m-237 最终态（`849ace0b`）八条命令全 `EXIT=0`（auditd 118、protocol 25、service 134/71、集成 4/4/1/1），**用的是 `release-toolchain.lock` 钉定的 rustc 1.97.0**（前一轮用的是 1.98.0）；下次补丁变更后仍需再跑 |
| `cargo-fuzz` sanitizer 实跑 | §3.2/§14.4 要求交付并运行 fuzz target | 无，尚未安排 |
| §14.5 目标机压测 | 吞吐 ≤5%、CPU ≤10%、ssserver RSS ≤64 MiB、auditd RSS ≤128 MiB，及离线/队列满/慢 ACK/spool 满四类专项 | 需目标机与真实数据面负载 |
| 真实流量端到端审计事件 | 经 ssserver 转发真实 TCP/UDP 流量后，验证 access event 落入 spool 并可经 lease 导出 | `integration_audit.py` 覆盖的是 ingest/export 协议链路，**不含**真实代理流量 |

### 3.1 首次执行结果（2026-08-30，Debian 13 / rustc 1.97.0）

`M-66` 修复后在 `10.0.1.3` 上以 `--no-fail-fast` 完整跑了一次。**overlay 自有的每一个目标全绿**：
`shadowsocks-audit-protocol` 25、`shadowsocks-auditd` 99、`shadowsocks-service` lib 121、
`shadowsocks` lib 9、`0001` 新增的 `tcp_eih_user.rs` 4、根 crate lib 10，以及 `dns`/`http`/`udp`
三组集成用例。

失败 9 条，**全部落在上游 v1.24.0 自带、三个补丁都未改动的联网集成用例**上：
`crates/shadowsocks/tests/tcp.rs` 5 条、`tcp_tfo.rs` 1 条、根 crate 的
`tests/{socks4,socks5,tunnel}.rs` 各 1 条。它们经真实隧道向 `www.example.com` 发
`GET / HTTP/1.0` 并断言应答行是 `HTTP/1.0 200 OK`，而真实服务器一律回 `HTTP/1.1 200 OK`。
`tests/socks5.rs` 一个文件里两条用例正好互证：期望 `HTTP/1.1` 的那条通过，期望 `HTTP/1.0`
的那条失败——既不是网络问题，也与本功能无关，是上游用例自身过时。

由此得到一个此前没暴露过的结论：**只要 overlay 继续原样携带上游 v1.24.0，旧的
「`cargo test --workspace --features user-audit` 全绿」按字面在任何主机上都不可能成立。**
现已按第 5 节第 4 项选择收窄命令；当前合同命令和排除边界以历史文档 v8 §16 为准。

第四项值得单独强调：目前**没有任何一次验证**证明过「真实用户流量 → 产生 access event →
写入 spool → 被 collector 取走」这条完整链路。§6 的两类成功事件语义在真机上仍未被端到端验证。

### 3.2 收窄门禁的 Linux 执行与本轮复跑状态（2026-08-30，Debian 13 / rustc 1.97.0）

v8 收窄后的基线门禁已在 Linux 实跑，命令全部 `EXIT=0`。**下表是 2026-08-30 的基线快照，不是当前状态**——
最终源码的复跑见 §3.3 与 §2 的两节复核，那里的数字取代本表：

| 命令 | 结果 |
| --- | --- |
| feature-off `--workspace --lib --bins --features user-stats --exclude shadowsocks-auditd` | 全绿（`shadowsocks` 9、`audit-protocol` 25、根 crate 10、`shadowsocks-service` 65） |
| feature-on `--workspace --lib --bins --features user-audit` | 全绿（`auditd` 99、`shadowsocks-service` 121，其余同上） |
| `-p shadowsocks --test tcp_eih_user` | 4 passed |
| v8 新增的三个 loopback 目标（`-p shadowsocks --test udp`、`--test udp`、`--test tunnel udp_tunnel`） | 4 / 1 / 1 passed |
| `-p shadowsocks-auditd`（基线含此前三条回归） | 102 passed。此处「本轮新增 5 条后目标为 107」的旧记法已被 §1 与 §3.3 推翻（实际新增 6 条→108）；最终态见 §2 末节，已复跑 |
| `-p shadowsocks-audit-protocol` | 25 passed |

### 3.3 `31993e0` 最终源码的 Linux 复跑（2026-08-30，本轮审阅执行）

`31993e0` 自述未复跑。本轮在同一节点跑完，**八条命令全部 `EXIT=0`**：

| 目标 | 31993e0 | 本轮修复后 |
| --- | --- | --- |
| `-p shadowsocks-auditd` | 108 passed（自述写 107，漏数 m-226 的 export 用例） | 110 passed |
| `-p shadowsocks-audit-protocol` | 25 | 25 |
| feature-on workspace 的 `shadowsocks-service` lib | 129（自述给的是 macOS 打桩的 122） | 131 |
| feature-off workspace 的 `shadowsocks-service` lib | 68 | 70 |
| `tcp_eih_user` / `-p shadowsocks --test udp` / `--test udp` / `--test tunnel udp_tunnel` | 4 / 4 / 1 / 1 | 同左 |

**顺带澄清一处对 §3.1 的误判。** 有人以「macOS 上同一条 feature-off 命令的 `shadowsocks-service`
lib 是 309，不可能少到 121」为由怀疑 §3.1 的计数有误。实测：macOS 那 309 里有 **244 个是
`local::redir::sys::unix::pfvar`**——macOS PF 结构体的 bindgen 布局用例，Linux 上根本不存在。
309 − 244 = 65，与 Linux 的 feature-off 逐一对上；user-audit 再加到 121。**§3.1 的数字是对的**，
两个平台的计数本就不可直接相比。

## 4. 待补的文档（Linux 实装中发现，均非代码缺陷）

| # | 事项 | 建议落点 |
| --- | --- | --- |
| D-1 | `scripts/prepare-source.sh` **每次调用都从 GitHub 拉取上游 tag**；节点失去外网时挂满 300 秒后失败，并连带使 `verify.sh`、`build-linux-release.sh` 全部不可用。`UPSTREAM_REPOSITORY`/第二参数可指向本地镜像且实测可用，但未见于任何文档 | `packaging/README.md`、`docs/OPERATIONS.md` |
| D-2 | 若按 D-1 准备本地镜像：`rsync -a` 以 root 接收会保留发送方 uid，git 因 `safe.directory`（dubious ownership）拒绝，需 `chown -R root:root` | 同上 |
| D-3 | §15.1 要求两次独立 musl 构建，但**未声明主机资源要求**；`build-linux-release.sh` 只对工具**版本**做硬门禁，对内存不做检查。实测 3.8 GiB 无 swap 的节点在 `lto = "fat"` + `codegen-units = 1` 下耗尽内存并使主机失去响应 | §15.1 或 `packaging/README.md` |
| D-4 | 同样未声明**连通性要求**：两次构建各用全新空 `CARGO_HOME`，整个依赖图每次发布完整下载两遍且不可复用；净化环境不放行 `CARGO_HTTP_*`，运维无法调整 cargo「30 秒内不足 10 字节即失败」的停滞阈值；无 vendor/离线 registry 方案。网络受限恰是签名主机的合理姿态 | 同上 |
| D-6 | `_sign_snapshot` 使用 `openssl dgst -sha256 -sign`，支持 RSA/ECDSA 但**不支持 Ed25519**（需 `pkeyutl` 且不预哈希）。§15.1 与 `packaging/README.md` 未声明可用密钥类型，选用 Ed25519 只会得到 `manifest 签名失败` | §15.1、`packaging/README.md` |
| D-7 | `tests/integration_audit.py` 会以三个非特权账号重新执行自身，因此**解释器、脚本与 `config/auditd.example.json` 模板都必须位于这些账号可读可执行的路径**。放在 `/root`（Debian 默认 `0550`）下只会得到难以定位的 `PermissionError` | `tests/README.md` |
| D-8 | `cluster-users.py verify-five` 要求**待校验的配置文件不得有 group/other 权限**（`0600`），而安装后的 `/etc/shadowsocks-rust-plus/server.json` 是 `0640`。两者不矛盾（不同阶段不同文件），但 README 第 4 步未说明 | `packaging/README.md` |

## 5. 决策记录

此前列出的四项均已决策并落实，结论见第 6 节：

1. `M-66` 的归属 → 并入 `0003`（已修复）；
2. `D-5` 是否收窄发布构建的 feature 集 → 收窄（已实施，规范升版到 v6）；
3. 实装节点的处置 → 清理（`10.0.1.3`、`10.0.2.3` 均已回到基线）。
4. **§16 的工作区测试判据如何收口** → 选择收窄命令（已实施，规范升版到 v8）：
   workspace 命令只运行 `--lib --bins` 的 feature-off/feature-on 目标，再单独运行 overlay 自有的
   `tcp_eih_user` 集成目标；其余 workspace integration targets 不纳入这两个 workspace 命令，
   上游公网 targets 保留为基线诊断，不改写、不纳入 §16 全绿判据。

## 6. 变更记录

- 2026-08-30：**对 `31993e0`（m-219–m-228）的审阅与整改**。逐条核实 + 对抗性复核后，行为层面未发现
  功能缺陷；十条修复共产生 **10 个整改提交**，全部为「一个问题一个 commit」并附变异检验：
  - `530c480` 补丁重新规范化。`31993e0` 的 `patches/0003-user-audit.patch` 是**拼接产物**——53 个
    `+++ b/` 文件头、去重后 48（`spool.rs`/`user_audit.rs` 各 3 次、`export.rs` 2 次），还有一行带 `+`
    前缀的 `diff --git` 夹在 spool.rs 的 hunk 里。它能应用只是因为前一个 hunk 的行数恰好耗尽、git 把
    该行当垃圾跳过再从 `--- a/` 恢复；生成的源码未被污染，重放树 SHA-256 也对。但它无法由文档规定的
    `git diff --full-index --binary --no-renames HEAD~1 HEAD` 复现（27991 行 vs 27516 行），
    「重新生成再逐字节比对」这条校验链就此断掉。已用规范输出替换（源码一字未改、锚点不变），并新增
    `test_patches_are_canonical_single_stanza_diffs` 拦住同类产物。
  - `ad8bcfd` / `659f149` / `476c411`：`m-220` 的三处——文档承诺失准、413 路径的 drain 失去绑定、
    worker 上界无护栏。
  - `00444bb`：`m-223` 的「保守匹配」补负例。
  - `989a0aa`：`m-222` 的「cleanup 错误可观测」补绑定，`TombstoneCommit` 加 `#[must_use]`。
  - `a4c372b`：`write_test_coverage_status` 被 `mv` 的目录语义骗过（真 bug，静默成功却无文件）。
  - `7dac9ec`：`m-219` 整条链路补行为绑定，结论逻辑抽成 `report_verification_conclusion` 以便单测。
  - `85fe4c2`：落实挂了五轮的 `m-142` 静态规则，同时补上 `m-226` 缺失的生产调用点绑定。
  - `7f465c7`：最后一个绕过三态契约的开关（`KEEP_FAILED_BUILD`），以及 `--coverage-status` 的交付说明。
  仍未修的六条记为 `m-229`–`m-234`，见第 2 节。

- 2026-08-30：完成对 `20ac4784e4735ab115469c936082e04717218e02^..ee6829bf3699012eef9229b44296f77851710215`
  的全部 98 个 commit（含起始 commit；起始之后 97 个）审阅。新增 m-219 至 m-228 的问题记录；其中
  m-219 至 m-228 已按本节所述修复或更正，m-142 与发布前置仍未闭合。
- 2026-08-30：**m-219/m-220 已修复**。`test.sh` 记录真实 auditd 覆盖面并由 `verify.sh` 严格读取；
  exporter 直接错误响应在释放 client permit 后进入独立上限的 lingering worker。补丁重放、脚本语法检查、
  Python 回归及 `shadowsocks-service` user-stats 目标检查通过。
- 2026-08-30：**m-221 至 m-224 已修复**。quarantine basename 持久记录 ACK 来源并兼容旧版
  `batch-corrupt-*`；tombstone ledger 提交与 marker cleanup 分离；eviction 计数移到 durable 状态转换之后，
  避免重试重复计账。新增 4 条 focused auditd 回归，并完成 x86_64 Linux 目标 `cargo check`。
- 2026-08-30：**AfterRename 边界已补齐**。tombstone aggregate ledger rename 后的 fsync/计量错误不再回滚
  内存 post-state，改为 sticky `DurabilityUncertain`；新增 `tombstone_after_rename_failure_keeps_post_commit_state`
  覆盖 add/prune/remove/replace 四个入口。最终 `prepared_tree_sha256` 更新为
  `4643abe734bf64d3ed3c801c9526afddef264fe6e1a292b707eef19dfb92ebf3`。
- 2026-08-30：**m-225 已修复**。README 明确缺失 auditd target 默认 fail-closed，只有显式
  `SHADOWSOCKS_REQUIRE_AUDIT_TARGET=0` 才报告“未验证”并继续其余检查。
- 2026-08-30：**m-226 至 m-228 已修复**。export deadline 改用 checked arithmetic；rename 后的
  `DurabilityUncertain` 与启动 salvage 边界已固定；GapFallback 以独立 `unknown_count` 和逐字段 unknown
  位传播未知边界；saturation 只从成功 CAS 结果计账。新增 auditd deadline/耐久性回归、3 条 m-227
  service 和 2 条 m-228 service 回归；clean replay 的最终 `prepared_tree_sha256` 为
  `4643abe734bf64d3ed3c801c9526afddef264fe6e1a292b707eef19dfb92ebf3`。
- 2026-08-30：自历史文档第 17–30 节抽取仍未闭合的事项，建立本文件。
- 2026-08-30：清理两台实装节点。`10.0.1.3` 停用并移除两个 systemd unit、账号/组、
  `/etc/shadowsocks-{audit,rust-plus}`、`/usr/local/bin/{ssserver,shadowsocks-auditd}`、
  `/srv/ss-integ` 与 sysusers/tmpfiles 配置，密钥材料 `shred -u -n 3`，磁盘 11 GB → 4.6 GB；
  `10.0.2.3` 移除工作目录与 `/tmp` 残留，11 GB → 8.5 GB。两台的 `build-essential` 等系统包
  有意保留。第 5 节第 3 项决策就此闭合。
- 2026-08-30：**`M-66` 已修复**（overlay `69069b3`，并入 `0003`）。根因确认为 Linux
  `unix_release_sock()` 在关闭接收队列非空的 AF_UNIX socket 时给对端置 `ECONNRESET`：
  exporter 的直接错误响应都在未读完请求时关闭连接，客户端因而在收到完整 413/429 之后仍
  读到错误；macOS 无此语义。修复是 `write_direct_json` 关闭写半边后有界 drain
  （上界 `USER_STATS_MAX_REQUEST_BYTES` 与 100 毫秒）。`exporter_bounds_busy_response_workers`
  属另一情形——超过 busy 上界的连接本就不读不答直接丢弃，复位是符合契约的结局，该用例改为
  接受 EOF 或 ECONNRESET 并仍断言零响应字节。新增
  `direct_error_response_drains_unread_request_input` 以 `UnixStream::pair()` + FIONREAD
  直接断言队列被读空，不依赖平台 close 语义。Linux（Debian 13 / rustc 1.97.0）变异检验：
  修复在位 5/5 通过，去掉 drain 后 4 项转红。修复解除阻塞后随即在同一节点首次跑完
  `cargo test --workspace --features user-audit --no-fail-fast`，结果见 §3.1。该结论只覆盖
  65,536 字节/100 毫秒有界 drain 场景；超大或慢速请求仍可能在错误响应后触发 RST，见本节 `M-73`。
- 2026-08-30：**`D-5` 已实施**（overlay `8711cbd`，规范升版 v5 → v6）。发布构建改为
  `--no-default-features` 加显式 feature 集，§15.1 增列该集合。x86_64-unknown-linux-musl
  下少编译 29 个 crate（brotli/zstd/flate2、tun/smoltcp/etherparse、nix、qrcode、
  tokio-util、rustls-native-certs 等）；`webpki-roots` 保留，DoT/DoH 根证书来源不变。
  **更正 D-5 原文的一处事实错误**：`reqwest`/`web-sys` 并不在该 target 的依赖图中——
  `local-online-config` 拉入的是 `mime`/`flate2`/`brotli`/`zstd`，收窄真正移除的是上述这批。
- 2026-08-30：**§16 工作区测试判据已收窄**（overlay 本轮修订，规范升版 v6 → v7）。
  `scripts/test.sh` 固定运行 `--workspace --lib --bins --no-fail-fast` 的 feature-off 回归，
  Linux 再运行 feature-on `user-audit` 回归，并显式运行 `tcp_eih_user`；其余 workspace
  integration targets 不再进入这两个 workspace 命令，锁定上游的公网 HTTP/1.0 targets
  不再阻塞本项目 §16 全绿判据。
- 2026-08-30：完成对 `20ac4784e4735ab115469c936082e04717218e02^..47a65a4e175879e0a758644f185f4d93ca6eac1d`
  的 89 个 commit（含起始 commit）审阅，新增并记录 `M-68`–`M-73` 六条 P2；均待修复，未改变规范合同或源码。
- 2026-08-30：**对同事回写的 `M-68`–`M-73` 逐条独立核实并做对抗性复核**（每条两名互不知情的审阅者，
  第二名被要求尽力驳倒第一名），六条全部处置完毕，均已修复：
  - `M-68`（**上调为 major**）成立，但**触发路径不是同事描述的那条**。acked/ 没有在线 body 巡检，
    两条进程内的 acked→quarantine 路径只在 meta 不可读时触发（此时 `event_count` 为 `None`，加 0）。
    真正的入口是**启动恢复** `recover_layout`：它对 `sealed/` 与 `acked/` 同构处理，隔离判据是
    `inspect_batch_dir` 失败——**meta.json 完好、body 摘要/成帧损坏**正好落在这里，于是隔离对象带着
    完整 `event_count` 进 quarantine/，随后被当成未确认丢失计数。修复是把来源写进 quarantine basename
    的 label（跨重启持久），驱逐时据此不计。同事建议的「在 `QuarantinePending` 里存来源」治不了这条
    路径——`recover_layout` 根本不写 `QuarantinePending`。
  - `M-69`、`M-70` 成立（minor），描述基本准确；`M-70` 同事建议的「持久化『计数已应用』标记」**不采纳**：
    该计数器每次 `Spool::open` 归零，持久标记会把高报换成漏报。改为把记账移到 durable 状态转换提交之后。
  - `M-71` 部分成立，**定级由 P2 下调为 minor**：故障是 fail-closed 的（构建中止，不产出错误产物），
    且发布链路从不构造 `PATH`，触发前提是操作员宿主 PATH 自带相对/空条目。缺陷本身属实，已修。
  - `M-72` 成立（minor）。**「静默」一词偏重**——两条降级提示都会打印，真正坏掉的是退出码。
    发布链路不经过该分支，Linux 主机走真 `cargo test`，该 fail-open 只能污染非 Linux 自查。
  - `M-73` 部分成立。真正的缺陷是**注释无条件承诺了 "clean end of stream"**，已改为陈述实际保证。
    同事建议的「在拒绝前消耗完整请求」**不采纳**：`ReadHeadError::TooLarge` 在未解析任何 header 时
    就返回，没有请求边界，等价于读到 EOF，恶意客户端不 `shutdown(SHUT_WR)` 就能无限期钉住 client
    permit——那正是 `max_request_bytes` 要防的。残留窗口是有界 drain 的固有代价，已写进注释与用例。
    同事原文的第二个建议（排空与 permit 解耦）成立，另立为 `m-220`。
- 2026-08-30：**审阅同事的 §16 收窄改动本身，发现并修复两处**（规范 v7 → v8，overlay `255b27f`）：
  v7 的兜底条款把三个纯 loopback、正对着本 overlay UDP 改动面的目标一并排除；新增的
  `WorkspaceGateDocsTests` 是文本 grep，36 个变异漏 17 个（含把门禁整行注释掉、加 `|| true`、
  数组留着不传给 cargo）。改为门禁即数据 + `--print-gate` + 与 §16 做集合相等，17 变异 17 抓 0 漏。
  另修 `--without-audit` 的无保留「测试通过」提示（`5a80797`）与 `--exclude` 的注释（`a20851a`）。
- 2026-08-30：**一处明确不改的判断**。`recover_layout` 为 acked 来源写出的 `segment_corruption` gap
  与驱逐时的 `quarantine_eviction` gap，`lost_events` 仍是该批次总数 N。§9.5 把 gap 的这些字段定义为
  「能够从损坏对象可靠取得的 nullable batch/digest/epoch/sequence/count/bytes」，即"损坏对象自称持有
  多少"，**不是**"未交付多少"；collector 手上有该批次的 ACK，可自行对账。故 gap 保持原样，只修名字
  就叫"未确认"的 `evicted_unacked_records`。
- 2026-08-30：**m-229–m-234 已全部修复**，一个问题一个提交（`e0be754`、`7da8935`、`b192938`、
  `ab4a2a4`、`9d8b7bc`、`46584ca`），每条附变异检验。其中四条是补绑定、两条是改行为：
  - 行为改动：`m-232` 把 lingering worker 从 `tokio::spawn` 改为经 channel 交回接受循环、由
    `run()` 的 `JoinSet` 拥有（`select!` 在等 accept 的同时服务 hand-back，保持排空的及时性）；
    `m-234` 让写屏障已确定成功后的纯记账失败不再升级为 `DurabilityUncertain` 与进程退出。
    `m-233` 介于两者之间：把 marker 清理的重试从「下次启动」提前到「下次 reconcile」，
    把复活重放固定 ID gap 的窗口从跨重启 24 小时压到一个 reconcile 间隔。彻底消除它需要一份
    能跨保留期的「已了结 gap」durable 记录，属合同层面的新结构，未做。
  - 补绑定：`m-230`（启动期两个 thread-local 故障开关）、`m-231`（另外两个提交点）、
    `m-229`（独立 `unknown_count` 与三处派生量）。
  三条测试脚手架上的教训值得记下：`salvage_tombstones` 自己要求文件是合法 JSON，截断文件会让
  「替换 ledger」整段被跳过；被复活的 pending 会被 `Spool::open` 自己那趟 reconcile 立刻消化掉，
  所以只能观察它留下的重复计账；`GapAccumulator::merge` 优先选空闲 slot，验 fallback 的派生量
  必须直接驱动 `fallback.merge`。三处我都先写错过一版，变异照样全绿。
- 2026-08-30：**重建了本轮的提交序列**。`m-229` 的提交在生成补丁时把当时工作树里已有的
  `m-230`/`m-231`/`m-233`/`m-234` 代码一并带了进去，违反「一个问题一个 commit」。做法是把四条
  的改动按文本块切分、以「逐条移除后须与基线逐字节相同」自证分区完整，再从 `m-232` 之后重放成
  五个提交。
- 2026-09-01：**复核 m-229–m-234 的六个修复提交**（自 `4ad0ffb` 起）。逐棵物化源码树核实提交分区、
  逐点核对 `prepared_tree_sha256` 哈希链、在 `10.0.1.3`（Debian 13 / rustc 1.98.0）复跑八条门禁
  全 `EXIT=0` 且计数与自述一致（auditd 116、protocol 25、service 134/71、集成 4/4/1/1），同事自述的
  十条变异检验独立复核全部转红，另在 macOS 单独转红 m-232 的 `tokio::spawn` 逃逸变异。复核中发现
  自己的变异脚手架也踩了一次坑（共享 target dir 复用旧二进制致六条误绿，`cargo clean -p` 后全部
  转红），已记入 §2 末节。结论：**六条修复全部成立**。新增并修复 `m-235`（m-234 的同型残留，
  `persist_state_locked` 把屏障成功后的纯记账失败升级为致命退出），一个问题一个提交；同时更正
  m-234 自述的范围理由（state.json 每条 record 都重写，频率不低于 tombstones.json）。修复后
  auditd 117 passed，最终态 `prepared_tree_sha256` 为 `58c5e777…`，门禁与 macOS `verify.sh`
  均已复跑通过。
- 2026-09-01：**二次复核 m-235 的修复提交 `0ec6c2c` 与文档提交 `1e5d112`**。m-235 独立复证成立、
  范围完整，对 m-234 范围理由的更正也成立。新增并修复两条，一个问题一个提交：`m-236`
  （`0aba624`，四个 `update_path_size` 记账点里 `persist_tombstone_pending_marker_locked` 是唯一
  记账失败后不清 `spool_bytes_known` 的，增量字节索引会带永久偏移参与容量判定；变异检验转红）与
  `m-237`（`51d17bf`，m-235 拆分测试时丢掉四类断言并留下退化的单元素循环；该条的变异**不具
  鉴别性**，已在条目里如实标注，依据是可直接比对的覆盖面丢失）。同时更正上一节文档两处：
  删去被 m-236 证伪的「未发现新的行为缺陷」宣告，改正不存在的函数名 accept_record_locked
  （实为 `write_record_locked`）。最终态 `prepared_tree_sha256` 为 `849ace0b…`；八条 Linux 门禁在
  **锁定的 rustc 1.97.0** 上全部 `EXIT=0`（auditd 118、protocol 25、service 134/71、集成 4/4/1/1），
  本地 12 项 Python/静态门禁与锚点重放一致性全绿。发布前置仍是三项（fuzz 实跑、§14.5 压测、
  真实流量端到端），无变化。
- 2026-09-01：**第三轮复核（同轮内的补充审阅）**，对象为 m-235 一族与 2026-09-01 的两次文档回写。
  确认成立并逐条修掉六条，一个问题一个提交：`m-238`（`718d937`，m-234 的代码注释仍写着与已更正
  范围理由相反的方向）、`m-240`（`f29363a`，本台账无任何门禁覆盖，文首锚点声明已实际漂移；
  新增两条门禁并实跑变异确认）、`m-241`（`027b00f`，m-235 条目把用例拆分写成等价重组）、
  `m-242`（`fb9fd06`，§3.2 与 m-226/m-227 的「待复跑」措辞与 §3 首行「已复跑」冲突）、
  `m-243`（`e3befa3`，复核基线提交写成 `4ad0ffb`，实为 `8b7b534`）、`m-239`（`bf4dd9d`，
  `mark_storage_rejection` 的 `non_gap` 实参在四处记账用例上均无绑定，变异 `true→false`
  在补断言前全绿、补后转红）。另记两条不回改的交付纪律观察（`0ec6c2c`/`1e5d112` 缺
  `Co-Authored-By`；`0ec6c2c` 缺刷新说明），见 §2。最终态 `prepared_tree_sha256` 为
  `ee27bfbb…`，本地 12 项 Python/静态门禁与锚点重放一致性全绿。发布前置仍是三项，无变化。
- 2026-09-01：按要求把本轮未 push 提交的 `Co-Authored-By` 尾注改为
  `Kimi <jianxieshui@gmail.com>`，并把其中八个提交与仓库 git user 不一致的 author
  邮箱一并归正。改写只动尾注行与作者邮箱（新旧 HEAD `git diff` 为空、树哈希一致，
  `origin/main` 及更早历史未触及）。改写使台账里 15 处提交引用失效，已逐条改回，
  并新增 `m-244`：给台账的提交哈希引用加上门禁，顺带修掉 `ee6829b` 遗留的一处悬空引用。
