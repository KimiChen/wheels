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

-- 原本这里还有一张 services（「配置里有哪些入口」）。它从建库起就没被写过：
-- 入口是从快照里观察到的，runtime_services 才是真正在用的那张。一张永远为空、
-- 却看起来像权威的配置表，只会让下一个人去查它并得到「这个节点没有入口」。
-- 需要「配置声明的入口」与「实际观察到的入口」对账时再加回来，那时它要连同
-- 写入路径和对账测试一起加。

-- 四个额度档位（定案第三条）。额度挂在**组**上而不是用户上：
-- 改一档是一次写入而不是 N 次，用户换档也不复制字节数，
-- 于是「两个地方存着同一个额度」这种状态不存在。
--
-- 闭集 CHECK 沿用本库口味：加第五档是一次显式的 schema 变更（D22 下即重建库），
-- 不是在代码里加一个 if。当前恰好四档是被明确批准的。
CREATE TABLE quota_groups (
    group_name   TEXT PRIMARY KEY NOT NULL
        CHECK(group_name IN ('normal', 'advanced', 'manage', 'admin')),
    display_name TEXT NOT NULL,
    -- 月度额度，u64 字节（C3 的 20 位定宽文本）。**十进制 TB**：
    -- 1 TB = 1 000 000 000 000，不是 2^40。
    --
    -- 上界钉在 i64::MAX 而不是 u64::MAX：wire 上 remaining_bytes 是非负 int64（C31），
    -- 超界值在分配那一层是**静默截断**。把上界写进库，等于把一次静默截断
    -- 换成一次写入失败。
    monthly_bytes TEXT COLLATE BINARY NOT NULL
        CHECK(typeof(monthly_bytes) = 'text' AND length(monthly_bytes) = 20
              AND monthly_bytes NOT GLOB '*[^0-9]*'
              AND monthly_bytes <= '09223372036854775807'),
    sort_order   INTEGER NOT NULL UNIQUE CHECK(sort_order >= 0),
    updated_by   TEXT NOT NULL,
    updated_at   TEXT NOT NULL
) STRICT;

-- 业务用户。角色收敛为两档（D19），管理员即全权。
--
-- 用户身上**没有额度字段**，只有一个组名：额度在 quota_groups 上。
-- NOT NULL + 外键：每个用户都有确定的档位，不存在「NULL 表示默认」这种二义；
-- 默认值在建号事务里从 quota_settings.default_group 取成具体值。
CREATE TABLE users (
    user_id     INTEGER PRIMARY KEY AUTOINCREMENT,
    login_name  TEXT NOT NULL UNIQUE,
    display_name TEXT NOT NULL,
    role        TEXT NOT NULL CHECK(role IN ('admin', 'user')),
    quota_group TEXT NOT NULL REFERENCES quota_groups(group_name),
    status      TEXT NOT NULL DEFAULT 'active' CHECK(status IN ('active', 'disabled')),
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
) STRICT;

-- 计费槽位注册表：节点 / inbound / 身份名 → 业务用户。
--
-- **这是归属的唯一落点，而且它是持久的。** 归属不能写在 runtime_identities 上：
-- 那张表按 (node_id, runtime_id) 键，节点一重启就生成全新的行，
-- 写在那里的归属会**静默全部丢失**——推送失败很响，账目失败完全无声。
--
-- 与 identity_assignment_events 是两张表、两种语义，不要合并（data-model.md §8）：
-- 这张表是「现在归谁」，那张表是「历史上归过谁」。
--
-- 行由**结算**发现并以 free 登记（见 ledger/settle.rs 第 4 步），
-- 因此注册表的内容永远来自节点实际上报的东西，不可能与节点现实漂移。
-- 结算只负责发现，从不移动状态；移动状态只发生在认领与退役事务里。
CREATE TABLE identity_routes (
    route_id    INTEGER PRIMARY KEY AUTOINCREMENT,
    node_id     TEXT NOT NULL REFERENCES nodes(node_id),
    inbound_tag TEXT NOT NULL,
    identity_name TEXT NOT NULL,
    -- free → claimed → retired，**单向**。retired 永不回到 free（定案第一条）：
    -- 复用前必须更换 uPSK，而那要改节点配置并 reload；不换就直接复用，
    -- 前一个持有者存在客户端里的凭据会去花下一个人的额度。
    --
    -- 单向性还有一个附带好处：槽位与用户的绑定因此是单调的，
    -- 「同一个槽位在周期中途换了主人、增量该拆给谁」这个问题在数据层就不存在。
    state       TEXT NOT NULL CHECK(state IN ('free', 'claimed', 'retired')),
    -- 归属**写一次就不变**：claimed 与 retired 都保留它。
    -- 退役不解绑——撤权是额度置零而不是删除凭据（定案第二条），
    -- 退役后到达的字节确实是那个人产生的，置空会把真实尾账变成无主字节。
    user_id     INTEGER REFERENCES users(user_id),
    -- 认领时用来证明「四向全零」的那份**已结算**快照（D11 §4.6：用观察代替相信配置）。
    -- 留下它，是为了让「认领前那一小段可能的流量」这个敞口事后可核，而不只是被断言。
    --
    -- 记 runtime_id 而不是 runtime_pk：它是自然键，不需要外键（node_runtimes
    -- 建在 02，而本文件在 01，被引用者必须先建），读起来也直接对得上节点上报的值。
    claim_fence_runtime_id TEXT
        CHECK(claim_fence_runtime_id IS NULL OR length(claim_fence_runtime_id) = 32),
    claim_fence_sequence   TEXT COLLATE BINARY
        CHECK(claim_fence_sequence IS NULL OR (typeof(claim_fence_sequence) = 'text'
              AND length(claim_fence_sequence) = 20
              AND claim_fence_sequence NOT GLOB '*[^0-9]*'
              AND claim_fence_sequence <= '18446744073709551615')),
    claimed_at  TEXT,
    retired_at  TEXT,
    created_at  TEXT NOT NULL,
    -- 不同入口的同名身份不合并：唯一键必须带 inbound_tag。
    UNIQUE(node_id, inbound_tag, identity_name),
    -- state 与 user_id 必须自洽，否则「无主的 claimed」能被写进来。
    CHECK((state = 'free'  AND user_id IS NULL     AND claimed_at IS NULL)
       OR (state <> 'free' AND user_id IS NOT NULL AND claimed_at IS NOT NULL)),
    CHECK((state = 'retired') = (retired_at IS NOT NULL))
) STRICT;

-- D17：同一用户在同一节点上只能有一个**在用**身份。
-- 没有这条，assemble 会走 split_identities 把他在该节点的额度切成两半，
-- 而这个错在观测上看不出来——PUT 照样 200，表也照样是全量的。
CREATE UNIQUE INDEX idx_identity_routes_active_per_user
    ON identity_routes(node_id, user_id) WHERE state = 'claimed';
CREATE INDEX idx_identity_routes_free ON identity_routes(node_id, state, route_id);

-- 原本这里还有 default_node_groups 与 user_node_grants，用来表达「某个用户能用哪些节点」。
-- 两张都没被写过，而且在当前形态下是纯粹的间接层：开通就是在**所有**启用配额的节点上
-- 认领一个槽位，撤权是把用户置为 disabled 再把额度打到零，两者都不按节点区分。
-- 真要做按用户选节点时再加回来——那是一个会改变额度表生成方式的功能，
-- 不是一张表能顺带支持的。

CREATE INDEX idx_identity_routes_user ON identity_routes(user_id);
