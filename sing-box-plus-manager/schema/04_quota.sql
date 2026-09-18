-- 全局额度、逐节点任务、分配与下发请求（docs/data-model.md §3、§11；README §4.5）。
--
-- 额度与周期都是全局的（D21）：所有人同一个月度额度、同一个自然月周期。
-- 没有 per-user 套餐，因此这里没有任何按用户存额度的列——
-- 加回来之前先重估 D21，不要在这张表上打补丁。

CREATE TABLE quota_settings (
    -- 单行表。用固定主键而不是「取最新一行」，让「同时存在两份设置」在库层就不可能。
    id           INTEGER PRIMARY KEY CHECK(id = 1),
    -- 全局月度额度，u64 字节（C3 的 20 位定宽文本）。
    monthly_bytes TEXT COLLATE BINARY NOT NULL
        CHECK(typeof(monthly_bytes) = 'text' AND length(monthly_bytes) = 20
              AND monthly_bytes NOT GLOB '*[^0-9]*'
              AND monthly_bytes <= '18446744073709551615'),
    -- 乐观并发用的 revision（README §6 的 If-Match）。非负 i64，耗尽即报错不回绕。
    revision     INTEGER NOT NULL CHECK(revision >= 0),
    display_timezone TEXT NOT NULL,
    updated_by   TEXT NOT NULL,
    updated_at   TEXT NOT NULL
) STRICT;

-- 改额度落审计：一条记录含操作者、时间、旧值、新值与受影响人数（README §8）。
CREATE TABLE quota_setting_audits (
    audit_id     INTEGER PRIMARY KEY AUTOINCREMENT,
    revision     INTEGER NOT NULL,
    old_monthly_bytes TEXT COLLATE BINARY NOT NULL
        CHECK(typeof(old_monthly_bytes) = 'text' AND length(old_monthly_bytes) = 20
              AND old_monthly_bytes NOT GLOB '*[^0-9]*'
              AND old_monthly_bytes <= '18446744073709551615'),
    new_monthly_bytes TEXT COLLATE BINARY NOT NULL
        CHECK(typeof(new_monthly_bytes) = 'text' AND length(new_monthly_bytes) = 20
              AND new_monthly_bytes NOT GLOB '*[^0-9]*'
              AND new_monthly_bytes <= '18446744073709551615'),
    affected_users INTEGER NOT NULL CHECK(affected_users >= 0),
    actor        TEXT NOT NULL,
    created_at   TEXT NOT NULL
) STRICT;

-- 逐节点待下发任务。新配置成功应用前可重试，重启后不丢任务。
--
-- 状态与 quota_requests.result 是两回事：这里是「这个 revision 在这个节点落到哪一步」，
-- 那里是「某一次 PUT 的结果」。零表确认不冒充新配置正常生效。
CREATE TABLE quota_update_tasks (
    task_id    INTEGER PRIMARY KEY AUTOINCREMENT,
    revision   INTEGER NOT NULL,
    node_id    TEXT NOT NULL REFERENCES nodes(node_id),
    status     TEXT NOT NULL
        CHECK(status IN ('pending', 'applied', 'safe_zero', 'unknown', 'failed')),
    last_error TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE(revision, node_id)
) STRICT;

-- 身份目标、权重、观察窗口与样本有效性（data-model.md §11）。
CREATE TABLE quota_allocations (
    allocation_id INTEGER PRIMARY KEY AUTOINCREMENT,
    node_id    TEXT NOT NULL REFERENCES nodes(node_id),
    runtime_identity_id INTEGER NOT NULL REFERENCES runtime_identities(runtime_identity_id),
    user_id    INTEGER NOT NULL REFERENCES users(user_id),
    -- 单身份下发额度：wire 上是**非负 int64**（C31），不是 u64。
    -- 这里用 INTEGER 而不是定宽文本，正是为了让「超出 int64」在库层就写不进去。
    remaining_bytes INTEGER NOT NULL CHECK(remaining_bytes >= 0),
    -- 确认下发的 a_i，用于需求权重。跨 epoch 或结果不明时不更新权重。
    confirmed_bytes INTEGER CHECK(confirmed_bytes IS NULL OR confirmed_bytes >= 0),
    -- 正整数需求权重（D7）。冷节点权重始终为正，a_i = 0 不做除法。
    demand_weight INTEGER NOT NULL CHECK(demand_weight > 0),
    observation_from TEXT,
    observation_to   TEXT,
    sample_valid  INTEGER NOT NULL CHECK(sample_valid IN (0, 1)),
    budget_version INTEGER NOT NULL CHECK(budget_version >= 0),
    updated_at TEXT NOT NULL,
    UNIQUE(node_id, runtime_identity_id, budget_version)
) STRICT;

-- 完整节点请求、发送前分配的 epoch 与结果。
--
-- 单写者事务先检查预算，写入完整请求和单调递增的 epoch，**再允许网络发送**。
-- 最后一次 200 的 epoch 不是计数器恢复依据，**最大已分配值才是**（data-model.md §11）。
CREATE TABLE quota_requests (
    request_id INTEGER PRIMARY KEY AUTOINCREMENT,
    node_id    TEXT NOT NULL REFERENCES nodes(node_id),
    runtime_id TEXT NOT NULL CHECK(length(runtime_id) = 32),
    -- epoch 是 per-(node_id, runtime_id) 的持久化 u64，**必须 ≥ 1**（C31）；
    -- 耗尽时失败关闭，不回绕。
    epoch      TEXT COLLATE BINARY NOT NULL
        CHECK(typeof(epoch) = 'text' AND length(epoch) = 20
              AND epoch NOT GLOB '*[^0-9]*'
              AND epoch <= '18446744073709551615'
              AND epoch >= '00000000000000000001'),
    -- 'positive' = 含正额度的完整表；'zero' = 纯零额度安全表。
    -- 这个区分不是记账口径，而是门禁口径：见下面的部分唯一索引。
    table_kind TEXT NOT NULL CHECK(table_kind IN ('positive', 'zero')),
    -- 每个含正额度的请求必须绑定该节点**上次正额度请求之后重新采集并结算**的 sequence。
    source_sequence TEXT COLLATE BINARY
        CHECK(source_sequence IS NULL OR (typeof(source_sequence) = 'text'
              AND length(source_sequence) = 20
              AND source_sequence NOT GLOB '*[^0-9]*'
              AND source_sequence <= '18446744073709551615')),
    request_body BLOB NOT NULL,
    body_sha256  TEXT NOT NULL CHECK(length(body_sha256) = 64),
    budget_version INTEGER NOT NULL CHECK(budget_version >= 0),
    setting_revision INTEGER NOT NULL CHECK(setting_revision >= 0),
    freshness_deadline TEXT,
    prepared_at TEXT NOT NULL,
    sent_at     TEXT,
    settled_at  TEXT,
    -- 任何已准备却无法确认是否发送的请求，恢复时按 unknown 对待。
    result      TEXT NOT NULL
        CHECK(result IN ('prepared', 'applied', 'rejected', 'unknown')),
    response_status INTEGER,
    -- 409 没有 discriminator：错误体对 runtime 不符与 epoch 未前进是同一个。
    -- 这一列记的是**取快照读回 runtime_id 之后**判定的成因，不是从响应体猜的。
    conflict_cause TEXT
        CHECK(conflict_cause IS NULL OR conflict_cause IN ('runtime_mismatch', 'stale_epoch', 'undetermined')),
    -- epoch 在 (node, runtime) 内单调，同一个值不得分配两次。
    UNIQUE(node_id, runtime_id, epoch),
    -- 纯零额度安全表不占用 sequence，因此允许 source_sequence 为空；
    -- 正额度表必须有来源 sequence，否则「新鲜度门禁」形同虚设。
    CHECK(table_kind = 'zero' OR source_sequence IS NOT NULL)
) STRICT;

-- 每份正额度请求占用一个新的已结算 (node_id, runtime_id, sequence)（data-model.md §11）。
-- 数据库唯一约束防止重复使用：仅「快照还没过 TTL」不足以授权反复刷新同一份余额。
-- **纯零额度安全表不受该占用限制**，所以是部分索引。
CREATE UNIQUE INDEX idx_quota_requests_sequence_claim
    ON quota_requests(node_id, runtime_id, source_sequence)
    WHERE table_kind = 'positive';

CREATE INDEX idx_quota_requests_recovery ON quota_requests(node_id, runtime_id, result);
CREATE INDEX idx_quota_update_tasks_status ON quota_update_tasks(status, node_id);
