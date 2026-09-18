-- 聚合控制（docs/data-model.md §3、§7）。
--
-- **查询缓存已移除。** 原本这里还有 rollup_watermarks 与 usage_dashboard_hourly /
-- usage_dashboard_daily 三张叶子缓存，从建库起就没有任何代码写过或读过它们：
-- api/trend.rs 直接查 usage_ledger。四节点 × 301 身份、每分钟一轮的数据量，
-- 多年之内都用不上这层缓存，而一张没人写的缓存表只会让下一个人以为它是权威。
-- 需要时按 §7 重新加回来——那时它要连同重建流程和「同一完成小时合计相等」
-- 的不变量测试一起加，而不是只留一个空壳。
--
-- 日缓存另有一条不能忘的性质：日桶按**持久化的展示时区**的日历日计算，
-- 改时区必须完整重建，所以当初把时区放进了主键。重新引入时这一条仍然成立，
-- 而且现在计费周期是 UTC+8（CYCLE_RULE_VERSION = 2），展示时区要与之对齐，
-- 否则用户会看到某一天的用量落进「错误的月份」。

-- 可重复领取、带 version 的脏桶。**无租约**（D15）。
--
-- 参考蓝图用的是有限租约 + fencing token，那是为跨进程、可能被网络分区的多 worker 设计的，
-- 而且它的目标数据库没有 SKIP LOCKED。本项目是 SQLite 单写者，两个前提都不成立。
--
-- 正确性不依赖「谁领到了」，而依赖幂等：小时桶是**完整重算并覆盖**的，
-- 同一个桶算两次结果相同，重复领取是浪费而不是错误。
-- version 只用来检测「算的时候又脏了」：写回时版本不符即丢弃本次结果并保留 pending。
--
-- 结算会把受影响的小时标脏（ledger/settle.rs），目前没有 worker 消费它们。
-- 保留这张表是因为标脏本身是对的：将来接上聚合时，这段时间的脏桶不会丢。
CREATE TABLE rollup_jobs (
    hour_bucket TEXT PRIMARY KEY NOT NULL,   -- UTC 整点，半开区间 [start, end) 的 start
    version     INTEGER NOT NULL CHECK(version >= 0),
    -- 不提交中间 processing 态：进程崩溃时该桶仍可领取。
    status      TEXT NOT NULL CHECK(status IN ('pending', 'done')),
    dirtied_at  TEXT NOT NULL,
    completed_at TEXT
) STRICT;

CREATE INDEX idx_rollup_jobs_pending ON rollup_jobs(status, hour_bucket);
