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
    -- 月度额度，u64 字节（C3 的 20 位定宽文本）。**二进制 TiB**：
    -- 1 TB = 2^40 = 1 099 511 627 776（2026-09-20 由十进制 10^12 改来）。
    --
    -- 改的理由是它每天都在说错话：客户端按 GiB 显示，于是一个叫「1 TB」的档
    -- 在用户屏幕上是 931 G。档位名与用户看到的数字对不上，而对不上的那一方
    -- 是用户天天看的那一方。
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
    -- 上游账号名。**大小写敏感**（README §4.9），也是建号的幂等键。
    login_name  TEXT NOT NULL UNIQUE,
    display_name TEXT NOT NULL,
    -- `role` 与 `quota_group` 都是**展示缓存**，不是授权依据。
    --
    -- 授权每请求从配置里的成员名单现解析（D27）：库里这两列只在登录与
    -- reconcile 时同步，供控制台与 CLI 显示。把它们当成真相的后果是
    -- 「改了名单要等下次登录才生效」，而撤权恰恰是最不能等的那件事。
    role        TEXT NOT NULL CHECK(role IN ('admin', 'user')),
    quota_group TEXT NOT NULL REFERENCES quota_groups(group_name),
    status      TEXT NOT NULL DEFAULT 'active' CHECK(status IN ('active', 'disabled')),
    -- 身份档案（README §4.9）：provider + 外部标识 + 显示名。
    -- **不存手机号、头像 URL**——上游 check-token 会返回它们，主控明确丢弃。
    --
    -- 「外部标识」在这里占两列而不是一列，因为泡游给了两个：
    -- `login_name`（账号名，同时是建号幂等键）与 `feishu_fs_id`。
    -- §4.9 那句「只存三个字段」说的是**不存哪些**（手机号、头像、部门树），
    -- 不是「外部标识只许有一个」——两个标识都参与名单匹配（D27）。
    auth_provider TEXT CHECK(auth_provider IS NULL
                             OR auth_provider IN ('paoyou', 'feishu', 'wecom', 'local')),
    -- 泡游返回的字段名叫 `wxwork_id`（企业微信），实际装的是**飞书 fs_id**。
    -- 本列按实测语义命名，不按上游字段名命名（README §4.9 的明文纪律）。
    --
    -- 可空：接入说明写明「账号未关联用户或未设置飞书标识时返回空字符串」，
    -- 所以它**不能**当身份主键，只作关联用。空字符串一律规范化成 NULL——
    -- 否则两个都没设飞书标识的人会在唯一索引上撞车。
    feishu_fs_id TEXT CHECK(feishu_fs_id IS NULL OR length(feishu_fs_id) > 0),
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
) STRICT;

-- SQLite 的唯一索引允许多个 NULL，正是这里要的：没有飞书标识的人互不冲突。
CREATE UNIQUE INDEX idx_users_feishu_fs_id ON users(feishu_fs_id) WHERE feishu_fs_id IS NOT NULL;

-- 计费槽位注册表：节点 / 身份名 → 业务用户。
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
--
-- **键是 (节点, 身份名)，不带 inbound_tag。** 一个人在一个节点上同时有 SS 与
-- VLESS 两条入口下的**同名**身份，那是同一个人、同一份归属、同一个计费口径——
-- 两条 inbound 记录只是同一个名字的两种到达方式。一个计费身份是一个人的一个
-- 名额，它挂在几个入口上是部署细节。
CREATE TABLE identity_routes (
    route_id    INTEGER PRIMARY KEY AUTOINCREMENT,
    node_id     TEXT NOT NULL REFERENCES nodes(node_id),
    -- 这里曾经有一个 inbound_tag，唯一键带着它。删掉它有两个理由。
    --
    -- 一、**它必然是个谎。** register_slot 在 `for inbound → for user` 的内层循环里
    -- 调用，`ON CONFLICT … DO NOTHING` 让列里留下的是快照 inbounds[] 里**先出现的
    -- 那一个**入口，之后永不更新。哪条入口承载这个名字，真相在 runtime_services /
    -- runtime_identities 上，是活的；抄一份到这里只会分家。
    --
    -- 二、**删列是这次改动唯一能被门禁看见的部分。** table_fingerprints 只读
    -- PRAGMA table_info，看不见 UNIQUE——把唯一键从三列改成两列对启动漂移检查
    -- 完全隐形。留着列的话，一个旧库会顺利通过门禁，然后 register_slot 每轮结算
    -- 报 `no unique index matching`，更坏的是 pick_free_slot 在旧库上 count 翻倍，
    -- **池子永远空、零报错**。删列把一次隐形的 schema 变更换成一次响亮的启动失败。
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
    -- 一个节点上一个名字一个槽位。该槽位天然覆盖承载这个名字的**全部**入口。
    UNIQUE(node_id, identity_name),
    -- state 与 user_id 必须自洽，否则「无主的 claimed」能被写进来。
    -- 三种状态各自的自洽形状。
    --
    -- `retired` 有两种来历，都要允许：
    --   * 用过之后退役——保留 user_id 与 claimed_at（退役**不解绑**归属：
    --     撤权是额度置零而非删除凭据，退役后到达的字节确实是那个人产生的）；
    --   * 从未认领就退役——两者都为 NULL。这一种是给「这个名字永远不该
    --     发给任何人」用的：部署侧的测试身份（`deploy-test`）、原型控制器的
    --     测试账号（`user0001`）都在节点的 inbound 里，但它们的凭据在别处流通，
    --     发给真人等于让两个人共用一份凭据。
    --
    -- 没有后一种的时候，排除它们只能靠「计数非零」这个**巧合**——
    -- 而一次进程重启就会让计数归零，于是它们悄悄变回可领取。
    CHECK((state = 'free'    AND user_id IS NULL     AND claimed_at IS NULL)
       OR (state = 'claimed' AND user_id IS NOT NULL AND claimed_at IS NOT NULL)
       OR (state = 'retired' AND (user_id IS NULL) = (claimed_at IS NULL))),
    CHECK((state = 'retired') = (retired_at IS NOT NULL))
) STRICT;

-- D17：同一用户在同一节点上只能有一个**在用**身份。
--
-- 它与上面那条 UNIQUE 不重复：UNIQUE 挡的是「同一个名字两行」，
-- 这条挡的是「同一个人两个不同的名字」。
--
-- **2026-09-20 起它守的东西变重了。** 原来没有它，assemble 会走 split_identities
-- 把他在该节点的额度切成两半——那是**少给**，而且在观测上看不出来（PUT 照样 200、
-- 表照样全量）。现在 assemble 对同名的多条入口**各发全额**，于是没有它就是
-- 「2 个名字 × 每个名字 N 条入口，份份全额」——从「省一半」变成「再翻一倍敞口」。
CREATE UNIQUE INDEX idx_identity_routes_active_per_user
    ON identity_routes(node_id, user_id) WHERE state = 'claimed';
CREATE INDEX idx_identity_routes_free ON identity_routes(node_id, state, route_id);

-- 原本这里还有 default_node_groups 与 user_node_grants，用来表达「某个用户能用哪些节点」。
-- 两张都没被写过，而且在当前形态下是纯粹的间接层：开通就是在**所有**启用配额的节点上
-- 认领一个槽位，撤权是把用户置为 disabled 再把额度打到零，两者都不按节点区分。
-- 真要做按用户选节点时再加回来——那是一个会改变额度表生成方式的功能，
-- 不是一张表能顺带支持的。

CREATE INDEX idx_identity_routes_user ON identity_routes(user_id);
