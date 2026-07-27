-- migrations/0009_key_rotation.sql
-- Phase 6 主密钥轮换（todo §8）。additive-only。
-- re-encrypt 扫描器把库内全部信封密文（credential_versions/ca_keypairs/config_artifacts）逐条 re-seal
-- 到当前主密钥版本；本表仅记进度/审计——正确性由「WHERE key_version<>current」过滤式跳过保证，
-- 进度表可随时重建。证书轮换/吊销（certificate_revocations/crls/agent_pin_state）随其编排半边后续迁移。

CREATE TABLE key_rotation_progress (
    target_version INTEGER NOT NULL,
    table_name     TEXT NOT NULL,
    rows_total     INTEGER NOT NULL DEFAULT 0,
    rows_done      INTEGER NOT NULL DEFAULT 0,
    status         TEXT NOT NULL DEFAULT 'pending'
                       CHECK (status IN ('pending','running','done')),
    started_at     INTEGER,
    updated_at     INTEGER NOT NULL,
    finished_at    INTEGER,
    PRIMARY KEY (target_version, table_name)
);
