-- 配置维度（docs/data-model.md §3）。
--
-- 数值编码契约见 data-model.md §3.1：u64 走 20 位定宽文本，u128 走 39 位。
-- 每个 CHECK 都写全四项（typeof / length / 纯数字 / 上界），不省略——
-- 少写一项就有一类坏值能进来，而这些列的错法都是静默的。

CREATE TABLE nodes (
    node_id           TEXT PRIMARY KEY NOT NULL,
    -- provider 只剩 plus_v3 一种（D3）。留列是为了让「将来接管别的节点」成为一次
    -- 显式的 schema 变更，而不是在代码里加一个 if。
    provider          TEXT NOT NULL CHECK(provider IN ('plus_v3')),
    address           TEXT NOT NULL,
    -- 首快照策略必须显式持久化（C8）：缺省即拒绝结算。
    first_snapshot    TEXT NOT NULL CHECK(first_snapshot IN ('baseline', 'include')),
    -- 在线信任的是那一张带外核对过的精确 leaf，不是 intermediate（README §4.2）。
    agent_spki_sha256 TEXT NOT NULL CHECK(length(agent_spki_sha256) = 64),
    quota_enabled     INTEGER NOT NULL DEFAULT 0 CHECK(quota_enabled IN (0, 1)),
    status            TEXT NOT NULL DEFAULT 'active' CHECK(status IN ('active', 'disabled')),
    created_at        TEXT NOT NULL,
    updated_at        TEXT NOT NULL
) STRICT;

-- 节点内稳定的 inbound 标识。与 runtime_services 是两种语义：
-- 这里是「配置里有哪些入口」，那里是「某个 runtime 窗口内观察到的 generation」。
CREATE TABLE services (
    service_id  INTEGER PRIMARY KEY AUTOINCREMENT,
    node_id     TEXT NOT NULL REFERENCES nodes(node_id),
    inbound_tag TEXT NOT NULL,
    inbound_type TEXT NOT NULL,
    created_at  TEXT NOT NULL,
    UNIQUE(node_id, inbound_tag)
) STRICT;

-- 业务用户。角色收敛为两档（D19），管理员即全权。
-- **没有 per-user 额度字段**——额度是全局设置（D21）。
CREATE TABLE users (
    user_id     INTEGER PRIMARY KEY AUTOINCREMENT,
    login_name  TEXT NOT NULL UNIQUE,
    display_name TEXT NOT NULL,
    role        TEXT NOT NULL CHECK(role IN ('admin', 'user')),
    status      TEXT NOT NULL DEFAULT 'active' CHECK(status IN ('active', 'disabled')),
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
) STRICT;

-- 当前映射规则：节点 / inbound / 身份名 → 业务用户。
-- 与 identity_assignment_events 是两张表、两种语义，不要合并（data-model.md §8）：
-- 这张表是「现在归谁」，那张表是「历史上归过谁」。
CREATE TABLE identity_routes (
    route_id    INTEGER PRIMARY KEY AUTOINCREMENT,
    node_id     TEXT NOT NULL REFERENCES nodes(node_id),
    inbound_tag TEXT NOT NULL,
    identity_name TEXT NOT NULL,
    user_id     INTEGER NOT NULL REFERENCES users(user_id),
    created_at  TEXT NOT NULL,
    -- 不同入口的同名身份不合并：唯一键必须带 inbound_tag。
    UNIQUE(node_id, inbound_tag, identity_name)
) STRICT;

-- 默认节点组与首次登录分配进度（D11）。
CREATE TABLE default_node_groups (
    group_id   INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT NOT NULL UNIQUE,
    node_id    TEXT NOT NULL REFERENCES nodes(node_id),
    created_at TEXT NOT NULL,
    UNIQUE(name, node_id)
) STRICT;

-- 用户对节点的授权。显式撤销不被重复登录覆盖，所以 revoked_at 是状态而不是删行。
CREATE TABLE user_node_grants (
    grant_id   INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id    INTEGER NOT NULL REFERENCES users(user_id),
    node_id    TEXT NOT NULL REFERENCES nodes(node_id),
    granted_at TEXT NOT NULL,
    revoked_at TEXT,
    UNIQUE(user_id, node_id)
) STRICT;

CREATE INDEX idx_identity_routes_user ON identity_routes(user_id);
CREATE INDEX idx_user_node_grants_node ON user_node_grants(node_id);
