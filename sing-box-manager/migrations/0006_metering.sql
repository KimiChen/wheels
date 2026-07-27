-- migrations/0006_metering.sql
-- Phase 5 流量与配额（todo §9/§9.1/§10.4/§13）。register: MIGRATIONS 追加 Migration{version:6,
--   name:"metering", sql: include_str!("../../migrations/0006_metering.sql")} in src/store/migrations.rs。
-- 约定同 0001-0005：UTC Unix 秒、枚举 CHECK、外键显式删除策略、additive-only。
-- 复用既有（不重建）：entry_runtime_epochs/entry_locks(0004)、health_events(0002)、audit_logs(0001)、
--   entry_ssm_state(0005，其 last_reconciled_at 即 last_synced_at)、Agent meter_outbox(agent_0001，
--   PK(entry_id,runtime_epoch,sequence)+acked，final 批复用之，无新 Agent 迁移)。
-- 安全：本文件所有表只存 identity_name + 字节数/会话数，绝不含 uPSK/明文密钥。

-- (1) §9 基线维度。singbox_boot_id = Agent 上报的 runtime_epoch(local_revisions.active)，
--     与 Manager entry_runtime_epochs.epoch 是两个独立计数命名空间，此处存 Agent 值。
--     epoch 入 PK ⇒ 新进程首读命中缺失行→last=0→delta=cur，结构性"新 epoch 从 0"，永不产负增量。
CREATE TABLE traffic_baselines (
    entry_id            TEXT NOT NULL REFERENCES entries(id) ON DELETE CASCADE,
    inbound_tag         TEXT NOT NULL,
    identity_name       TEXT NOT NULL,
    singbox_boot_id     INTEGER NOT NULL,
    last_uplink_bytes   INTEGER NOT NULL DEFAULT 0,
    last_downlink_bytes INTEGER NOT NULL DEFAULT 0,
    observed_at         INTEGER NOT NULL,
    updated_at          INTEGER NOT NULL,
    PRIMARY KEY (entry_id, inbound_tag, identity_name, singbox_boot_id)
);

-- (2) §9.1 精确一次台账：仅结算屏障 final 批入账去重（例行 poll 由基线同事务天然幂等，不入台账）。
--     UNIQUE(entry,boot_id,sequence) ⇒ Manager 在 apply 与 meter-ack 间崩溃后重投为 delta 0。
CREATE TABLE traffic_batches (
    entry_id        TEXT NOT NULL REFERENCES entries(id) ON DELETE CASCADE,
    singbox_boot_id INTEGER NOT NULL,
    sequence        INTEGER NOT NULL,
    kind            TEXT NOT NULL DEFAULT 'final' CHECK (kind IN ('poll','final')),
    deployment_id   TEXT REFERENCES deployments(id) ON DELETE SET NULL,
    observed_at     INTEGER NOT NULL,
    ingested_at     INTEGER NOT NULL,
    PRIMARY KEY (entry_id, singbox_boot_id, sequence)
);

-- (3) 全局每用户累计：跨 Entry/Route 全部身份 delta 汇入同一 (user_id, period) 桶。
CREATE TABLE usage_buckets (
    user_id        TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    period         TEXT NOT NULL,               -- 'YYYY-MM' | 'YYYY' | 'never'
    uplink_bytes   INTEGER NOT NULL DEFAULT 0,
    downlink_bytes INTEGER NOT NULL DEFAULT 0,
    updated_at     INTEGER NOT NULL,
    PRIMARY KEY (user_id, period)
);

-- (4) 每用户资格缓存 + 周期指针（周期切换/自动恢复检测）。
--     effective_disabled 让 eligible_desired 廉价排除；与 users.disabled(管理员) 独立。
CREATE TABLE user_runtime_state (
    user_id            TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    active_period      TEXT NOT NULL,
    quota_state        TEXT NOT NULL DEFAULT 'ok' CHECK (quota_state IN ('ok','over','expired')),
    effective_disabled INTEGER NOT NULL DEFAULT 0,
    over_since         INTEGER,
    last_evaluated_at  INTEGER NOT NULL,
    updated_at         INTEGER NOT NULL
);

-- (5) §8/§9 每 Entry 计量观测 + 故障隔离 + 过期告警。last_synced_at 复用 entry_ssm_state.last_reconciled_at。
CREATE TABLE entry_runtime_state (
    entry_id              TEXT PRIMARY KEY REFERENCES entries(id) ON DELETE CASCADE,
    last_stats_attempt_at INTEGER,
    last_stats_at         INTEGER,
    last_reported_boot_id INTEGER,
    last_error            TEXT,
    consecutive_failures  INTEGER NOT NULL DEFAULT 0,
    stale                 INTEGER NOT NULL DEFAULT 0,
    updated_at            INTEGER NOT NULL
);

-- 关联 Manager 每-Entry epoch 行 ↔ Agent host 全局 boot id（DeployReport.runtime_epoch 回填）。
-- 使结算/审计能把 barrier_status 转移 join 到真正拥有 baseline/outbox/台账行的 boot id。
ALTER TABLE entry_runtime_epochs ADD COLUMN agent_boot_epoch INTEGER;
-- 强制/超时切换的未结算窗口审计标记（配合 barrier_status='forced' 与 audit_logs 时间戳）。
ALTER TABLE entry_runtime_epochs ADD COLUMN unsettled_window INTEGER NOT NULL DEFAULT 0;

CREATE INDEX idx_usage_user_period ON usage_buckets(user_id, period);
CREATE INDEX idx_entry_runtime_stale ON entry_runtime_state(stale);