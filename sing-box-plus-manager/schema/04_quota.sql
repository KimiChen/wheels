-- 分档额度、逐节点任务与下发请求（docs/data-model.md §3、§11；README §4.5）。
--
-- **额度挂在组上，不挂在用户上。** D21 移除的是「一个贴在用户身上的额度数字」
-- 及其带来的周期锚点、折算系数与套餐迁移；四个固定档位一样没有这些东西：
-- 全体同一个周期、同一个折算口径（四向求和），换档只是改一列。
-- 所以这是对 D21 的收紧而不是推翻——用户仍然不携带额度，只携带一个组名。

-- 档位表 quota_groups 在 01_config_dimensions.sql——users 要外键引用它，
-- 而 schema/ 下的文件按外键顺序加载，被引用者必须先建。

CREATE TABLE quota_settings (
    -- 单行表。用固定主键而不是「取最新一行」，让「同时存在两份设置」在库层就不可能。
    id           INTEGER PRIMARY KEY CHECK(id = 1),
    -- **额度这个数已经不在这里了**——它在 quota_groups。留在这里的是真正全局的三样。
    --
    -- revision 保持单条序列：任何与额度有关的变更（改档位字节数、给用户换档）
    -- 都推进这同一个数。不做 per-档 revision——quota_update_tasks 的唯一键是
    -- (revision, node_id)，而 latest_tasks 只看 MAX(revision)；
    -- 两条并行的序列会让「最新」失去全序，收敛循环随即失去意义。
    revision     INTEGER NOT NULL CHECK(revision >= 0),
    -- 新建用户的默认档位。
    default_group TEXT NOT NULL REFERENCES quota_groups(group_name),
    -- 日桶用的展示时区。计费周期是 UTC+8（CYCLE_RULE_VERSION = 2），
    -- 这里要与之对齐，否则某一天的用量会显示进「错误的月份」。
    display_timezone TEXT NOT NULL,
    updated_by   TEXT NOT NULL,
    updated_at   TEXT NOT NULL
) STRICT;

-- 改额度落审计：操作者、时间、旧值、新值与受影响人数（README §8）。
--
-- 两种动作都会降低某人的额度，所以都要留痕，用 scope + subject 区分：
--   group      改某一档的字节数，影响该档全体
--   user_group 把某个用户换档，影响一人
CREATE TABLE quota_setting_audits (
    audit_id     INTEGER PRIMARY KEY AUTOINCREMENT,
    revision     INTEGER NOT NULL,
    scope        TEXT NOT NULL CHECK(scope IN ('group', 'user_group')),
    -- scope = 'group' 时是档位名，scope = 'user_group' 时是 login_name。
    subject      TEXT NOT NULL,
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

-- 原本这里还有一张 quota_allocations，存单身份下发目标、需求权重与观察窗口，
-- 供加权跨节点分配使用。本轮采用**镜像分配**：每个节点都收到该用户本月的全额剩余，
-- 不做拆分。理由是额度的定位是观察用量而非强制封顶，而拆分会让用户把流量压在
-- 单一节点时在 1/4 额度处被切断——那是没人要求的强制行为。
-- 镜像不需要权重，也就不需要这张表。allocate.rs / weight.rs 及其测试保留但不接线，
-- 将来要换回加权分配时，这张表要连同它们一起恢复。

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
