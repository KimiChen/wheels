-- Agent 本地库 0003：结算屏障暂存。phase A 收到 barrier_required 的部署时，先把新配置落盘并抓取旧进程
-- 最终统计入 meter_outbox，但不停旧进程；返回 awaiting_meter_ack。此表登记「待完成」的部署，供 phase B
-- （收到 Manager meter-ack）读取并执行停旧→原子替换→重启→新 epoch。
-- 安全：只存 revision/sha/config_path/entry_id/epoch/sequence，绝不含 uPSK 或明文密钥。
CREATE TABLE pending_barrier (
    command_id  TEXT PRIMARY KEY,
    revision    INTEGER NOT NULL,
    sha256      TEXT NOT NULL,
    config_path TEXT NOT NULL,
    role        TEXT NOT NULL,
    entry_id    TEXT NOT NULL,
    old_epoch   INTEGER,                       -- 被抓取最终统计的旧 boot id（无旧进程时 NULL）
    sequence    INTEGER NOT NULL,              -- meter_outbox 序号
    new_epoch   INTEGER NOT NULL,              -- phase A 预分配的新 boot id；phase B 重放复用（不再单调新分配）
    drain_clean INTEGER NOT NULL DEFAULT 1,    -- 抓取前会话是否已排空
    created_at  INTEGER NOT NULL
);
