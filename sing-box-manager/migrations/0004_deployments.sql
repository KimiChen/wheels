-- 0004 Phase 3：版本发布状态机（todo §7、§9.1、§10.3/§10.4）。
-- 约定同 0001-0003：UTC Unix 秒、枚举 CHECK、外键显式删除策略、additive-only。
-- 不改 0003 config_revisions.status CHECK（SQLite 无法 ALTER CHECK）；"deployed" 是 deployments 表的事实，
-- 新 revision 部署成功后旧 revision 置 superseded（0003 已允许该值）。
-- 安全：deploy 命令的明文配置绝不落库——deployment_targets 只存句柄(artifact_id/content_sha256)，
--   完整配置在派发时解封、经 mTLS 下发，不持久化。command_id 为发给 Agent 的幂等键（纯 TEXT，无 FK）。

CREATE TABLE deployments (
    id                   TEXT PRIMARY KEY,
    kind                 TEXT NOT NULL DEFAULT 'deploy' CHECK (kind IN ('deploy','rollback')),
    revision_id          TEXT NOT NULL REFERENCES config_revisions(id) ON DELETE CASCADE,
    previous_revision_id TEXT REFERENCES config_revisions(id) ON DELETE SET NULL,
    status               TEXT NOT NULL DEFAULT 'pending' CHECK (status IN
                           ('pending','deploying_nodes','deploying_entries','activating',
                            'succeeded','failed','rolling_back','rolled_back')),
    strategy             TEXT NOT NULL DEFAULT 'normal' CHECK (strategy IN ('normal','forced')),
    canary_host_ids      TEXT,                  -- JSON 数组；空/NULL=全量
    diff_json            TEXT NOT NULL,         -- 结构化 diff（非密：scope/role/sha 变化）
    created_by           TEXT,
    error_summary        TEXT,
    created_at           INTEGER NOT NULL,
    updated_at           INTEGER NOT NULL,
    completed_at         INTEGER
);
CREATE INDEX idx_deployments_status ON deployments(status);

CREATE TABLE deployment_targets (
    id             TEXT PRIMARY KEY,
    deployment_id  TEXT NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
    host_id        TEXT NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
    artifact_id    TEXT NOT NULL REFERENCES config_artifacts(id) ON DELETE CASCADE,
    role           TEXT NOT NULL CHECK (role IN ('entry','node')),
    scope_ref      TEXT NOT NULL,
    batch_order    INTEGER NOT NULL,           -- 0=node（先），1=entry（后）
    content_sha256 TEXT NOT NULL,
    command_id     TEXT,                        -- 发给 Agent 的幂等键（无 FK；deploy 不入 agent_commands）
    status         TEXT NOT NULL DEFAULT 'pending' CHECK (status IN
                     ('pending','dispatched','awaiting_meter_ack','deployed',
                      'check_failed','health_failed','failed','rolled_back','skipped')),
    applied_revision INTEGER,
    runtime_epoch  INTEGER,
    health_detail  TEXT,
    attempts       INTEGER NOT NULL DEFAULT 0,
    error_summary  TEXT,
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL,
    UNIQUE (deployment_id, host_id, scope_ref)
);
CREATE INDEX idx_deptargets_deployment ON deployment_targets(deployment_id, batch_order);

-- §10.4：新进程新 epoch，基线从 0；Phase 3 建表起用，Phase 5 计量依赖。回滚也起新 epoch。
CREATE TABLE entry_runtime_epochs (
    entry_id       TEXT NOT NULL REFERENCES entries(id) ON DELETE CASCADE,
    epoch          INTEGER NOT NULL,
    deployment_id  TEXT REFERENCES deployments(id) ON DELETE SET NULL,
    revision_id    TEXT REFERENCES config_revisions(id) ON DELETE SET NULL,
    barrier_status TEXT NOT NULL DEFAULT 'not_required' CHECK (barrier_status IN
                     ('not_required','awaiting_meter_ack','settled','forced')),
    active         INTEGER NOT NULL DEFAULT 0,
    started_at     INTEGER NOT NULL,
    meter_acked_at INTEGER,
    ended_at       INTEGER,
    PRIMARY KEY (entry_id, epoch)
);
CREATE INDEX idx_epochs_entry_active ON entry_runtime_epochs(entry_id, active);

-- 台账：本次部署把哪些 draft→active，回滚精确还原（不误降既有 active）。
CREATE TABLE deployment_route_activations (
    deployment_id TEXT NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
    route_id      TEXT NOT NULL REFERENCES routes(id) ON DELETE CASCADE,
    prev_status   TEXT NOT NULL,
    activated_at  INTEGER NOT NULL,
    PRIMARY KEY (deployment_id, route_id)
);

-- §7/§9.1 Entry 独占操作锁骨架：获取=INSERT ON CONFLICT DO NOTHING 原子单飞；释放=DELETE WHERE holder_id=?；
-- expires_at 租约供单活 Manager 崩溃恢复。Phase 5 计量/SSM reconcile 争同锁 → 排斥并发改运行态。
CREATE TABLE entry_locks (
    entry_id    TEXT PRIMARY KEY REFERENCES entries(id) ON DELETE CASCADE,
    holder_kind TEXT NOT NULL CHECK (holder_kind IN ('deploy','metering','reconcile')),
    holder_id   TEXT NOT NULL,
    acquired_at INTEGER NOT NULL,
    expires_at  INTEGER NOT NULL
);
