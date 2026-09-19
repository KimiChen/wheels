-- 生命周期与归属历史（docs/data-model.md §3、§8）。
--
-- 一个 runtime_id 的存活区间是一个 runtime 窗口；跨窗口累计值清零。
-- 因此 runtime 是结算键的一部分，也是「什么时候不能相减」的分界线。

CREATE TABLE node_runtimes (
    runtime_pk       INTEGER PRIMARY KEY AUTOINCREMENT,
    node_id          TEXT NOT NULL REFERENCES nodes(node_id),
    -- 节点侧是 32 位小写 hex。
    runtime_id       TEXT NOT NULL CHECK(length(runtime_id) = 32),
    -- u64，20 位定宽文本（data-model.md §3.1）。同一 runtime 首次确认后固定不变。
    started_at_unix_ms TEXT COLLATE BINARY NOT NULL
        CHECK(typeof(started_at_unix_ms) = 'text' AND length(started_at_unix_ms) = 20
              AND started_at_unix_ms NOT GLOB '*[^0-9]*'
              AND started_at_unix_ms <= '18446744073709551615'),
    -- 每个 runtime 显式登记策略、原因与操作者（README §4.4 runtime 批准登记）。
    -- 未登记的 runtime（含影子 runtime）不得进入批准文件，其上的身份不可分配。
    first_snapshot   TEXT NOT NULL CHECK(first_snapshot IN ('baseline', 'include')),
    approval_status  TEXT NOT NULL DEFAULT 'pending'
        CHECK(approval_status IN ('pending', 'approved', 'rejected')),
    approval_reason  TEXT,
    approved_by      TEXT,
    approved_at      TEXT,
    -- 最后已接受的 sequence。NULL 表示这个 runtime 还没有任何一份快照入过账。
    last_sequence    TEXT COLLATE BINARY
        CHECK(last_sequence IS NULL OR (typeof(last_sequence) = 'text' AND length(last_sequence) = 20
              AND last_sequence NOT GLOB '*[^0-9]*'
              AND last_sequence <= '18446744073709551615')),
    -- 窗口闭合状态。异常退出的旧 runtime 标 unclosed 且「可能丢尾」（R4）：
    -- 绝不能用新 runtime 的累计值去减旧 runtime 的游标。
    window_state     TEXT NOT NULL DEFAULT 'open'
        CHECK(window_state IN ('open', 'closed', 'unclosed')),
    first_seen_at    TEXT NOT NULL,
    last_seen_at     TEXT NOT NULL,
    UNIQUE(node_id, runtime_id)
) STRICT;

-- runtime 内的 inbound generation。
CREATE TABLE runtime_services (
    runtime_service_id INTEGER PRIMARY KEY AUTOINCREMENT,
    runtime_pk   INTEGER NOT NULL REFERENCES node_runtimes(runtime_pk),
    inbound_tag  TEXT NOT NULL,
    generation   TEXT COLLATE BINARY NOT NULL
        CHECK(typeof(generation) = 'text' AND length(generation) = 20
              AND generation NOT GLOB '*[^0-9]*'
              AND generation <= '18446744073709551615'),
    inbound_type TEXT NOT NULL,
    active       INTEGER NOT NULL CHECK(active IN (0, 1)),
    first_seen_at TEXT NOT NULL,
    UNIQUE(runtime_pk, inbound_tag, generation)
) STRICT;

-- 完整身份 generation。这一行就是六元组的物化：
-- node_id + inbound.tag + inbound.generation + user.name + user.generation + runtime_id。
--
-- generation 属于协议键，**不能因为当前实现里它恒为 1 就从主键里省略**（data-model.md §2）。
CREATE TABLE runtime_identities (
    runtime_identity_id INTEGER PRIMARY KEY AUTOINCREMENT,
    runtime_pk       INTEGER NOT NULL REFERENCES node_runtimes(runtime_pk),
    runtime_service_id INTEGER NOT NULL REFERENCES runtime_services(runtime_service_id),
    identity_name    TEXT NOT NULL,
    generation       TEXT COLLATE BINARY NOT NULL
        CHECK(typeof(generation) = 'text' AND length(generation) = 20
              AND generation NOT GLOB '*[^0-9]*'
              AND generation <= '18446744073709551615'),
    -- active 可在快照间切换，但切换不创建新基线、不重置累计值（C2、C9）。
    active           INTEGER NOT NULL CHECK(active IN (0, 1)),
    -- 这里曾经有一个 user_id。它是错的地方：本表按 (node_id, runtime_id) 键，
    -- 节点一重启就生成全新的行，写在这里的归属会**静默全部丢失**，
    -- 于是 bump_cycle 不再被调用、所有身份在下一轮推送里掉到零额度。
    -- 归属改由 identity_routes 承担——那张表按 (节点, 身份名) 键，
    -- 跨重启存活。读取方一律 join 过去。
    first_seen_at    TEXT NOT NULL,
    last_seen_at     TEXT NOT NULL,
    UNIQUE(runtime_service_id, identity_name, generation)
) STRICT;

-- 只追加的身份 → 用户分配事件（D14）。
--
-- 计费身份被重新分配给另一个业务用户后，旧账本行必须保留原归属，
-- 否则历史账单会在重新分配的瞬间被改写。
-- 归属时刻用**已批准 runtime 的 started_at_unix_ms + 单调增量**做 checked arithmetic，
-- 不用节点墙钟单独决定；存疑时归属留空并标 clock_ambiguous，**不猜**。
CREATE TABLE identity_assignment_events (
    event_id   INTEGER PRIMARY KEY AUTOINCREMENT,
    -- 指向**槽位**而不是某个 runtime 下的身份行：一个用户可以跨四次重启持有同一个
    -- 槽位，而 runtime_identity_id 每次重启都是新的，NOT NULL 指向它根本表达不了这件事。
    route_id   INTEGER NOT NULL REFERENCES identity_routes(route_id),
    -- 当时观察到的那一行，留作审计关联；可空，因为事件本身不依赖它。
    runtime_identity_id INTEGER REFERENCES runtime_identities(runtime_identity_id),
    user_id    INTEGER REFERENCES users(user_id),
    state      TEXT NOT NULL
        CHECK(state IN ('assigned', 'unassigned', 'unmapped', 'clock_ambiguous')),
    -- 半开区间 [effective_from, effective_to)；effective_to 为 NULL 表示仍在持有。
    effective_from TEXT NOT NULL,
    effective_to   TEXT,
    recorded_at TEXT NOT NULL,
    reason      TEXT
) STRICT;

CREATE INDEX idx_node_runtimes_node ON node_runtimes(node_id, window_state);
CREATE INDEX idx_runtime_identities_runtime ON runtime_identities(runtime_pk, identity_name);
CREATE INDEX idx_assignment_events_route
    ON identity_assignment_events(route_id, effective_from);
