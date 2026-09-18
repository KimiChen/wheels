-- 接收审计、结算状态、永久事实与持久累计（docs/data-model.md §3、§4、§5、§9）。
--
-- 每个流量列都写明口径：**增量**还是**累计**，UTC 还是展示时区。
-- 同类系统为 164 个字段逐个补注释，起因就是看不出口径（data-model.md §1）。

-- 批次回执。只要 envelope 还能识别出 node / runtime / sequence，
-- **可审计的拒绝也要留一条回执**（status = 'rejected'，绝不推进 sequence 与游标）。
CREATE TABLE snapshot_batches (
    batch_pk     INTEGER PRIMARY KEY AUTOINCREMENT,
    -- 幂等批次 ID：对 node / runtime / 完整 lineage / sequence / **四向增量**做
    -- **长度明确的 canonical 编码**后取 SHA-256，不用字符串拼接（分隔符歧义会撞哈希）。
    --
    -- **接收时为空，结算时才填。** 它含增量，而增量要持锁读到游标之后才算得出来。
    -- 这不是妥协：批次 ID 本来就是**第二道**防线，不是第一道——
    -- 它挡不住「先产出后推进」的重放（重放出来的 sequence 与增量都不同），
    -- 第一道是下面那个 (node_id, runtime_id, sequence) 唯一索引，接收时就生效。
    batch_id     TEXT UNIQUE CHECK(batch_id IS NULL OR length(batch_id) = 64),
    node_id      TEXT NOT NULL REFERENCES nodes(node_id),
    -- envelope 里的原始 runtime_id。**不依赖 runtime_pk 解析成功**：
    -- 可审计的拒绝也要留回执，而那时 runtime 可能还没登记。
    runtime_id   TEXT NOT NULL CHECK(length(runtime_id) = 32),
    runtime_pk   INTEGER REFERENCES node_runtimes(runtime_pk),
    sequence     TEXT COLLATE BINARY NOT NULL
        CHECK(typeof(sequence) = 'text' AND length(sequence) = 20
              AND sequence NOT GLOB '*[^0-9]*'
              AND sequence <= '18446744073709551615'),
    -- 不持久化中间 processing 状态（data-model.md §4）：崩溃整笔回滚，批次自然保持可重试。
    status       TEXT NOT NULL
        CHECK(status IN ('pending', 'applied', 'rejected', 'superseded', 'conflict', 'unknown')),
    reject_reason TEXT,
    payload_sha256 TEXT NOT NULL CHECK(length(payload_sha256) = 64),
    -- 三个时间都要存，且**归桶选择结果必须持久化**——否则重放同一批次会落进另一个桶。
    collected_at TEXT NOT NULL,   -- agent 完成采集的时刻
    received_at  TEXT NOT NULL,   -- 主控收到响应的可信时刻
    accounting_at TEXT NOT NULL,  -- 主控按规则生成的归属时间桶（UTC）
    time_quality TEXT NOT NULL CHECK(time_quality IN ('trusted', 'fallback')),
    -- 时间不确定范围是**两次成功采集的实际间隔**，不是一个调度周期（R11）。
    observation_gap_ms TEXT COLLATE BINARY
        CHECK(observation_gap_ms IS NULL OR (typeof(observation_gap_ms) = 'text'
              AND length(observation_gap_ms) = 20
              AND observation_gap_ms NOT GLOB '*[^0-9]*'
              AND observation_gap_ms <= '18446744073709551615')),
    -- 排空证据取自同一份快照（operations.md §3）：gauge 不可用时不能标为正常排空。
    drained      INTEGER CHECK(drained IS NULL OR drained IN (0, 1)),
    created_at   TEXT NOT NULL,
    settled_at   TEXT,
    -- 接收时的幂等键：同一个 (节点, runtime, sequence) 只允许一条回执。
    -- 同号重投若 payload 摘要相同则幂等返回；不同则是契约违反，记 conflict。
    UNIQUE(node_id, runtime_id, sequence)
) STRICT;

-- 压缩后的原始 JSON。**它是结算输入，不是可选的审计附件**（data-model.md §5）：
-- 异步结算要能从库里恢复完整快照；C5 的失败关闭要人工介入，没有原始字节无法复盘；
-- C3 的 u64 解析争议也只能靠原始字节佐证。
--
-- 哈希对**解压后的原始响应字节**计算，不要重新序列化对象后再算。
CREATE TABLE snapshot_payloads (
    batch_pk   INTEGER PRIMARY KEY REFERENCES snapshot_batches(batch_pk),
    codec      TEXT NOT NULL CHECK(codec IN ('identity', 'zstd')),
    raw_length INTEGER NOT NULL CHECK(raw_length >= 0),
    payload    BLOB NOT NULL,
    created_at TEXT NOT NULL
) STRICT;

-- 每个六元组最后接受的四个绝对累计值。六元组由 runtime_identity_id 物化。
--
-- 零增量与 active = false 的身份**也要更新**这一行（data-model.md §4 第 9 步）：
-- 游标记的是「最后接受到的绝对值」，不是「最后有流量的时刻」。
CREATE TABLE counter_cursors (
    runtime_identity_id INTEGER PRIMARY KEY
        REFERENCES runtime_identities(runtime_identity_id),
    tcp_uplink_bytes   TEXT COLLATE BINARY NOT NULL   -- 累计（进程生命周期内）
        CHECK(typeof(tcp_uplink_bytes) = 'text' AND length(tcp_uplink_bytes) = 20
              AND tcp_uplink_bytes NOT GLOB '*[^0-9]*'
              AND tcp_uplink_bytes <= '18446744073709551615'),
    tcp_downlink_bytes TEXT COLLATE BINARY NOT NULL   -- 累计
        CHECK(typeof(tcp_downlink_bytes) = 'text' AND length(tcp_downlink_bytes) = 20
              AND tcp_downlink_bytes NOT GLOB '*[^0-9]*'
              AND tcp_downlink_bytes <= '18446744073709551615'),
    udp_uplink_bytes   TEXT COLLATE BINARY NOT NULL   -- 累计
        CHECK(typeof(udp_uplink_bytes) = 'text' AND length(udp_uplink_bytes) = 20
              AND udp_uplink_bytes NOT GLOB '*[^0-9]*'
              AND udp_uplink_bytes <= '18446744073709551615'),
    udp_downlink_bytes TEXT COLLATE BINARY NOT NULL   -- 累计
        CHECK(typeof(udp_downlink_bytes) = 'text' AND length(udp_downlink_bytes) = 20
              AND udp_downlink_bytes NOT GLOB '*[^0-9]*'
              AND udp_downlink_bytes <= '18446744073709551615'),
    last_batch_pk INTEGER REFERENCES snapshot_batches(batch_pk),
    updated_at   TEXT NOT NULL
) STRICT;

-- 只追加的四向**增量**账本。禁止 INSERT OR REPLACE（它是删除后插入，破坏审计与引用语义）。
CREATE TABLE usage_ledger (
    ledger_id  INTEGER PRIMARY KEY AUTOINCREMENT,
    batch_pk   INTEGER NOT NULL REFERENCES snapshot_batches(batch_pk),
    batch_id   TEXT NOT NULL,
    runtime_identity_id INTEGER NOT NULL REFERENCES runtime_identities(runtime_identity_id),
    -- 归属在写入时固定，之后不随 identity_routes 变化（D14）。
    user_id    INTEGER REFERENCES users(user_id),
    tcp_uplink_bytes   TEXT COLLATE BINARY NOT NULL   -- 本次增量
        CHECK(typeof(tcp_uplink_bytes) = 'text' AND length(tcp_uplink_bytes) = 20
              AND tcp_uplink_bytes NOT GLOB '*[^0-9]*'
              AND tcp_uplink_bytes <= '18446744073709551615'),
    tcp_downlink_bytes TEXT COLLATE BINARY NOT NULL   -- 本次增量
        CHECK(typeof(tcp_downlink_bytes) = 'text' AND length(tcp_downlink_bytes) = 20
              AND tcp_downlink_bytes NOT GLOB '*[^0-9]*'
              AND tcp_downlink_bytes <= '18446744073709551615'),
    udp_uplink_bytes   TEXT COLLATE BINARY NOT NULL   -- 本次增量
        CHECK(typeof(udp_uplink_bytes) = 'text' AND length(udp_uplink_bytes) = 20
              AND udp_uplink_bytes NOT GLOB '*[^0-9]*'
              AND udp_uplink_bytes <= '18446744073709551615'),
    udp_downlink_bytes TEXT COLLATE BINARY NOT NULL   -- 本次增量
        CHECK(typeof(udp_downlink_bytes) = 'text' AND length(udp_downlink_bytes) = 20
              AND udp_downlink_bytes NOT GLOB '*[^0-9]*'
              AND udp_downlink_bytes <= '18446744073709551615'),
    -- 版本化的来源事件哈希：canonical 编码后的 SHA-256（data-model.md §4 第 8 步）。
    source_event_hash TEXT NOT NULL CHECK(length(source_event_hash) = 64),
    hash_version INTEGER NOT NULL,
    accounting_at TEXT NOT NULL,          -- UTC
    hour_bucket  TEXT NOT NULL,           -- UTC 整点，半开区间 [start, end) 的 start
    created_at   TEXT NOT NULL,
    -- 账本幂等的兜底。**不能依赖「通常不会重复」**（data-model.md §4）。
    UNIQUE(batch_id, runtime_identity_id)
) STRICT;

-- 每个 runtime identity 的四向永久累计。
-- **不是对保留期内账本求 SUM 的缓存**——它在每次结算事务中更新，明细清理后不变。
CREATE TABLE usage_lifetime_totals (
    runtime_identity_id INTEGER PRIMARY KEY
        REFERENCES runtime_identities(runtime_identity_id),
    user_id    INTEGER REFERENCES users(user_id),
    tcp_uplink_bytes   TEXT COLLATE BINARY NOT NULL   -- 永久累计（u128，39 位）
        CHECK(typeof(tcp_uplink_bytes) = 'text' AND length(tcp_uplink_bytes) = 39
              AND tcp_uplink_bytes NOT GLOB '*[^0-9]*'
              AND tcp_uplink_bytes <= '340282366920938463463374607431768211455'),
    tcp_downlink_bytes TEXT COLLATE BINARY NOT NULL   -- 永久累计
        CHECK(typeof(tcp_downlink_bytes) = 'text' AND length(tcp_downlink_bytes) = 39
              AND tcp_downlink_bytes NOT GLOB '*[^0-9]*'
              AND tcp_downlink_bytes <= '340282366920938463463374607431768211455'),
    udp_uplink_bytes   TEXT COLLATE BINARY NOT NULL   -- 永久累计
        CHECK(typeof(udp_uplink_bytes) = 'text' AND length(udp_uplink_bytes) = 39
              AND udp_uplink_bytes NOT GLOB '*[^0-9]*'
              AND udp_uplink_bytes <= '340282366920938463463374607431768211455'),
    udp_downlink_bytes TEXT COLLATE BINARY NOT NULL   -- 永久累计
        CHECK(typeof(udp_downlink_bytes) = 'text' AND length(udp_downlink_bytes) = 39
              AND udp_downlink_bytes NOT GLOB '*[^0-9]*'
              AND udp_downlink_bytes <= '340282366920938463463374607431768211455'),
    updated_at TEXT NOT NULL
) STRICT;

-- 按用户、周期和计费规则版本保存的**周期累计**，与订阅 / 配额共用同一口径。
CREATE TABLE usage_cycle_totals (
    user_id      INTEGER NOT NULL REFERENCES users(user_id),
    -- 自然月周期（D21）：所有人同一个周期锚点，形如 '2026-09'。
    cycle_key    TEXT NOT NULL,
    rule_version INTEGER NOT NULL,
    total_bytes  TEXT COLLATE BINARY NOT NULL       -- 周期累计，四向之和（C17）
        CHECK(typeof(total_bytes) = 'text' AND length(total_bytes) = 39
              AND total_bytes NOT GLOB '*[^0-9]*'
              AND total_bytes <= '340282366920938463463374607431768211455'),
    updated_at   TEXT NOT NULL,
    PRIMARY KEY(user_id, cycle_key, rule_version)
) STRICT;

-- 原本这里还有一张 ledger_retention_state，记已清理边界与累计检查点。
-- 本轮不做保留期清理，账本只增不删，所以它没有写入者；而一个恒为空的
-- purged_through 会让「这段区间的账本还在不在」这个问题得到一个假的肯定答案。
-- 做清理时再加回来——它要和「清理不改变 lifetime 合计」的不变量测试一起加。

CREATE INDEX idx_snapshot_batches_settle
    ON snapshot_batches(node_id, runtime_pk, status, sequence);
CREATE INDEX idx_snapshot_batches_receipt
    ON snapshot_batches(node_id, runtime_pk, sequence, payload_sha256);
CREATE INDEX idx_usage_ledger_bucket ON usage_ledger(hour_bucket, runtime_identity_id);
CREATE INDEX idx_usage_ledger_user ON usage_ledger(user_id, accounting_at);
