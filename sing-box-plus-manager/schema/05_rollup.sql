-- 聚合控制与查询缓存（docs/data-model.md §3、§7）。
--
-- 查询缓存**只保留到 runtime_identity 这一最细维度**，靠冗余外键索引做汇总——
-- 不维护 global / node / service / user 四套互相容易不一致的缓存。

-- 可重复领取、带 version 的脏桶。**无租约**（D15）。
--
-- 参考蓝图用的是有限租约 + fencing token，那是为跨进程、可能被网络分区的多 worker 设计的，
-- 而且它的目标数据库没有 SKIP LOCKED。本项目是 SQLite 单写者，两个前提都不成立。
--
-- 正确性不依赖「谁领到了」，而依赖幂等：小时桶是**完整重算并覆盖**的，
-- 同一个桶算两次结果相同，重复领取是浪费而不是错误。
-- version 只用来检测「算的时候又脏了」：写回时版本不符即丢弃本次结果并保留 pending。
CREATE TABLE rollup_jobs (
    hour_bucket TEXT PRIMARY KEY NOT NULL,   -- UTC 整点，半开区间 [start, end) 的 start
    version     INTEGER NOT NULL CHECK(version >= 0),
    -- 不提交中间 processing 态：进程崩溃时该桶仍可领取。
    status      TEXT NOT NULL CHECK(status IN ('pending', 'done')),
    dirtied_at  TEXT NOT NULL,
    completed_at TEXT
) STRICT;

-- 缓存覆盖范围与新鲜度。**聚合失败不推进已完成水位。**
CREATE TABLE rollup_watermarks (
    scope        TEXT PRIMARY KEY NOT NULL,
    completed_through TEXT NOT NULL,
    updated_at   TEXT NOT NULL
) STRICT;

-- 小时 × runtime identity 的叶子缓存。合计必须等于同一完成小时的账本合计（§10 不变量）。
CREATE TABLE usage_dashboard_hourly (
    hour_bucket TEXT NOT NULL,               -- UTC 整点
    runtime_identity_id INTEGER NOT NULL
        REFERENCES runtime_identities(runtime_identity_id),
    -- 冗余外键：汇总靠索引，不建第二套缓存。
    node_id     TEXT NOT NULL REFERENCES nodes(node_id),
    runtime_service_id INTEGER NOT NULL REFERENCES runtime_services(runtime_service_id),
    user_id     INTEGER REFERENCES users(user_id),
    tcp_uplink_bytes   TEXT COLLATE BINARY NOT NULL   -- 本小时增量合计（u128，39 位）
        CHECK(typeof(tcp_uplink_bytes) = 'text' AND length(tcp_uplink_bytes) = 39
              AND tcp_uplink_bytes NOT GLOB '*[^0-9]*'
              AND tcp_uplink_bytes <= '340282366920938463463374607431768211455'),
    tcp_downlink_bytes TEXT COLLATE BINARY NOT NULL   -- 本小时增量合计
        CHECK(typeof(tcp_downlink_bytes) = 'text' AND length(tcp_downlink_bytes) = 39
              AND tcp_downlink_bytes NOT GLOB '*[^0-9]*'
              AND tcp_downlink_bytes <= '340282366920938463463374607431768211455'),
    udp_uplink_bytes   TEXT COLLATE BINARY NOT NULL   -- 本小时增量合计
        CHECK(typeof(udp_uplink_bytes) = 'text' AND length(udp_uplink_bytes) = 39
              AND udp_uplink_bytes NOT GLOB '*[^0-9]*'
              AND udp_uplink_bytes <= '340282366920938463463374607431768211455'),
    udp_downlink_bytes TEXT COLLATE BINARY NOT NULL   -- 本小时增量合计
        CHECK(typeof(udp_downlink_bytes) = 'text' AND length(udp_downlink_bytes) = 39
              AND udp_downlink_bytes NOT GLOB '*[^0-9]*'
              AND udp_downlink_bytes <= '340282366920938463463374607431768211455'),
    rebuilt_at  TEXT NOT NULL,
    PRIMARY KEY(hour_bucket, runtime_identity_id)
) STRICT;

-- 日 × 时区 × runtime identity 的叶子缓存。
--
-- 日桶按**持久化的展示时区**的日历日计算，不能写成「前一日开始时间 + 24 小时」
-- （夏令时与跨月会错）。改展示时区必须完整重建这张表，所以时区进主键。
CREATE TABLE usage_dashboard_daily (
    day_key     TEXT NOT NULL,               -- 展示时区下的日历日，形如 '2026-09-18'
    display_timezone TEXT NOT NULL,
    runtime_identity_id INTEGER NOT NULL
        REFERENCES runtime_identities(runtime_identity_id),
    node_id     TEXT NOT NULL REFERENCES nodes(node_id),
    runtime_service_id INTEGER NOT NULL REFERENCES runtime_services(runtime_service_id),
    user_id     INTEGER REFERENCES users(user_id),
    tcp_uplink_bytes   TEXT COLLATE BINARY NOT NULL   -- 本日增量合计
        CHECK(typeof(tcp_uplink_bytes) = 'text' AND length(tcp_uplink_bytes) = 39
              AND tcp_uplink_bytes NOT GLOB '*[^0-9]*'
              AND tcp_uplink_bytes <= '340282366920938463463374607431768211455'),
    tcp_downlink_bytes TEXT COLLATE BINARY NOT NULL   -- 本日增量合计
        CHECK(typeof(tcp_downlink_bytes) = 'text' AND length(tcp_downlink_bytes) = 39
              AND tcp_downlink_bytes NOT GLOB '*[^0-9]*'
              AND tcp_downlink_bytes <= '340282366920938463463374607431768211455'),
    udp_uplink_bytes   TEXT COLLATE BINARY NOT NULL   -- 本日增量合计
        CHECK(typeof(udp_uplink_bytes) = 'text' AND length(udp_uplink_bytes) = 39
              AND udp_uplink_bytes NOT GLOB '*[^0-9]*'
              AND udp_uplink_bytes <= '340282366920938463463374607431768211455'),
    udp_downlink_bytes TEXT COLLATE BINARY NOT NULL   -- 本日增量合计
        CHECK(typeof(udp_downlink_bytes) = 'text' AND length(udp_downlink_bytes) = 39
              AND udp_downlink_bytes NOT GLOB '*[^0-9]*'
              AND udp_downlink_bytes <= '340282366920938463463374607431768211455'),
    rebuilt_at  TEXT NOT NULL,
    PRIMARY KEY(day_key, display_timezone, runtime_identity_id)
) STRICT;

CREATE INDEX idx_dashboard_hourly_user ON usage_dashboard_hourly(user_id, hour_bucket);
CREATE INDEX idx_dashboard_hourly_node ON usage_dashboard_hourly(node_id, hour_bucket);
CREATE INDEX idx_dashboard_daily_user ON usage_dashboard_daily(user_id, day_key);
CREATE INDEX idx_rollup_jobs_pending ON rollup_jobs(status, hour_bucket);
