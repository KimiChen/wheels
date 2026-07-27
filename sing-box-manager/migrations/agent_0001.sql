-- Agent 本地库（AGENT_STATE_PATH）：与 Manager 库物理隔离，Agent 不连 Manager 库。
-- 仅存命令幂等去重、计量 outbox、本地已应用 revision。时间统一 UTC Unix 秒。

-- 命令幂等：command_id 为 PK，INSERT ON CONFLICT DO NOTHING 抢占（execute_once）。
CREATE TABLE executed_commands (
    command_id   TEXT PRIMARY KEY,
    kind         TEXT NOT NULL,
    request_hash TEXT NOT NULL,
    status       TEXT NOT NULL CHECK (status IN ('in_flight','succeeded','failed')),
    result_json  TEXT,
    http_status  INTEGER,
    created_at   INTEGER NOT NULL,
    completed_at INTEGER
);

-- 计量 outbox：重启前最终统计批次，Manager 确认（ack）前禁止清理。
CREATE TABLE meter_outbox (
    entry_id      TEXT NOT NULL,
    runtime_epoch INTEGER NOT NULL,
    sequence      INTEGER NOT NULL,
    payload_json  TEXT NOT NULL,
    acked         INTEGER NOT NULL DEFAULT 0,
    created_at    INTEGER NOT NULL,
    PRIMARY KEY (entry_id, runtime_epoch, sequence)
);

-- 本地已应用 revision（当前/上一个成功，供状态查询与回滚）。
CREATE TABLE local_revisions (
    revision   INTEGER PRIMARY KEY,
    sha256     TEXT NOT NULL,
    applied_at INTEGER NOT NULL,
    active     INTEGER NOT NULL DEFAULT 0
);
