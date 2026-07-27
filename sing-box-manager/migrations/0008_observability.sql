-- migrations/0008_observability.sql
-- Phase 6 系统指标/健康/告警/数据保留（todo §11.1 总览 / §9）。additive-only。
-- 指标按需从既有表只读聚合，无需新表；此处仅建告警状态机 + 补保留裁剪所需的时间范围索引。

-- 告警状态机：仅状态跃迁才通知，重启不重复告警（append-only 观测流仍走 health_events）。
CREATE TABLE alert_state (
    rule_id          TEXT NOT NULL,
    subject_kind     TEXT NOT NULL,              -- host/entry/user/deployment/global
    subject_id       TEXT NOT NULL DEFAULT '',
    severity         TEXT NOT NULL CHECK (severity IN ('info','warning','critical')),
    status           TEXT NOT NULL CHECK (status IN ('firing','resolved')),
    detail           TEXT,                        -- 脱敏；禁密钥
    since            INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL,
    last_notified_at INTEGER,
    resolved_at      INTEGER,
    PRIMARY KEY (rule_id, subject_kind, subject_id)
);
CREATE INDEX idx_alert_state_status ON alert_state(status);

-- 保留裁剪的时间范围扫描索引（现有索引多为 (host_id, time) 复合，无法服务全局 range delete）。
-- health_events / agent_status_snapshots 现有索引均以 host_id 起头，无法服务全局 created_at 范围删除。
-- audit_logs(created_at) 已有 idx_audit_created(0001)，无需重复。
CREATE INDEX idx_health_created     ON health_events(created_at);
CREATE INDEX idx_snapshots_polled   ON agent_status_snapshots(polled_at);
CREATE INDEX idx_batches_ingested   ON traffic_batches(ingested_at);
