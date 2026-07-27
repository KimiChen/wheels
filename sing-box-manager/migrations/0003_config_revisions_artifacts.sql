-- 0003 Phase 2：配置修订与编译产物（todo §7、§10.3）。
-- 约定同 0001/0002：时间 UTC Unix 秒（INTEGER）；枚举用 CHECK；外键显式删除策略；additive-only。
-- 安全：artifact 明文 = 完整 sing-box JSON，含 SS-2022 PSK，故 content 经 crypto::Cipher 信封加密
--   （列形状与 credential_versions/ca_keypairs 一致）；content_sha256 对【明文规范 JSON 字节】计算，
--   单向摘要，可安全出 API / 供幂等与 diff。
-- migrations.rs：MIGRATIONS 数组新增 { version:3, name:"config_revisions_artifacts",
--   sql: include_str!("../../migrations/0003_config_revisions_artifacts.sql") }；
--   store/mod.rs 单测断言 MAX(version)=3。

-- 不可变配置修订：一次 compile 事件的快照锚点。Phase 2 里 Route 仍停 draft，本表记录“预检编译”修订；
-- Phase 3 部署状态机复用同表。
CREATE TABLE config_revisions (
    id            TEXT PRIMARY KEY,
    seq           INTEGER NOT NULL UNIQUE,            -- 单调递增（MAX(seq)+1 分配）；人读引用 + 排序
    status        TEXT NOT NULL DEFAULT 'compiled'
                    CHECK (status IN ('compiled','checked','check_failed','superseded')),
    topology_hash TEXT NOT NULL,                      -- sha256(规范化拓扑快照，不含任何密钥)；同拓扑幂等去重
    summary       TEXT,                               -- 非密：受影响 host/entry/node id、route label、preflight-only 标注
    created_by    TEXT,                               -- 操作者（脱敏；无密钥）
    created_at    INTEGER NOT NULL
);
CREATE INDEX idx_config_revisions_topohash ON config_revisions(topology_hash);

-- 每个受影响目标（一个 Entry 或一个 Node）一份编译产物。一个 Entry 一份 entry 产物；
-- 链中出现的每个受管 Node 一份 node 产物（socks5 外部终端不产 Node 配置）。
CREATE TABLE config_artifacts (
    id                     TEXT PRIMARY KEY,
    revision_id            TEXT NOT NULL REFERENCES config_revisions(id) ON DELETE CASCADE,
    host_id                TEXT NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,  -- 派生数据随 Host 清理，不阻塞删除
    role                   TEXT NOT NULL CHECK (role IN ('entry','node')),
    scope_ref              TEXT NOT NULL,             -- entry_id 或 node_id（纯 TEXT，无 FK：不 pin entries/nodes）
    content_sha256         TEXT NOT NULL,             -- sha256(明文规范 JSON 字节) hex 小写：幂等 / diff 锚点
    byte_size              INTEGER NOT NULL,          -- 明文字节数（观测）
    -- 明文 JSON 含 PSK，信封加密（对齐 crypto::Sealed 列，绝不明文落库）：
    alg                    INTEGER NOT NULL,
    key_version            INTEGER NOT NULL,
    nonce                  BLOB NOT NULL,
    ciphertext             BLOB NOT NULL,             -- Cipher.seal(canonical_plaintext_json)
    target_singbox_version TEXT,                      -- 生成时 agents.singbox_version 快照（可空：Agent 离线/预检未知）
    check_status           TEXT NOT NULL DEFAULT 'pending'
                             CHECK (check_status IN ('pending','passed','failed')),
    check_output           TEXT,                      -- sing-box check stderr（入库前脱敏 PSK 明文与 base64/password token）
    generated_at           INTEGER NOT NULL,
    checked_at             INTEGER,
    UNIQUE (revision_id, role, scope_ref)             -- 一个 revision 内每个对象至多一份 artifact（幂等）
);
CREATE INDEX idx_artifacts_revision ON config_artifacts(revision_id);
CREATE INDEX idx_artifacts_host ON config_artifacts(host_id);
CREATE INDEX idx_artifacts_scope ON config_artifacts(role, scope_ref);
CREATE INDEX idx_artifacts_sha ON config_artifacts(content_sha256);

-- credentials.kind 已含 entry_psk/node_psk/landing_auth（0001），无需 ALTER。
-- 凭据生命周期：删除 Entry/Node/Landing 时，其 entry_psk/node_psk/landing_auth 凭据行在同一事务内
--   一并删除（credential_versions 经 0001 的 ON DELETE CASCADE 连带），避免孤儿凭据累积。