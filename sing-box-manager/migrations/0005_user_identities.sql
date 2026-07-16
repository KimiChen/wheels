-- migrations/0005_user_identities.sql
-- Phase 4 用户身份与订阅（todo.md §5/§8/§13）。additive-only：0001 已建 users/user_routes/
-- subscription_tokens 与 credentials.kind(user_route_upsk,subscription_token)。约定沿用：UTC Unix 秒、
-- 枚举 CHECK、外键显式删除策略。
--
-- user_routes 既是 (user_id,route_id) ACL 连接表，也承载每 (用户×Route) 的内部身份句柄与 uPSK 凭据。
-- identity_name 由不可变 (user_id,route_id) 确定性派生（代码 users::identity_name()），全局唯一→
-- 天然满足「每 Entry inbound 内唯一」。identity_label 为可读名，仅供 Phase 5 SSM stats 归因/调试，
-- 不参与任何唯一性或路由逻辑。upsk_credential_id 指向信封加密的 uPSK（credentials.kind=user_route_upsk）。

ALTER TABLE user_routes ADD COLUMN identity_name TEXT;
ALTER TABLE user_routes ADD COLUMN identity_label TEXT;
ALTER TABLE user_routes ADD COLUMN upsk_credential_id TEXT
    REFERENCES credentials(id) ON DELETE SET NULL;

-- 全局唯一（强于每 Entry 唯一）。SQLite 中多个 NULL 互异；空表迁移无历史 NULL 冲突，
-- 且 Phase 4 所有 grant 均带非空 identity 写入。
CREATE UNIQUE INDEX idx_user_routes_identity ON user_routes(identity_name);

-- 每 Entry 的 SSM reconcile 观测（todo.md §9「SSM 下发失败保存重试任务和最后错误」）。
-- last_desired_hash 供告警/去噪；不作为幂等键（幂等由 Agent 侧 execute_once + fresh command_id 负责）。
CREATE TABLE entry_ssm_state (
    entry_id           TEXT PRIMARY KEY,
    last_desired_hash  TEXT,
    last_reconciled_at INTEGER,
    last_error         TEXT,
    updated_at         INTEGER NOT NULL,
    FOREIGN KEY (entry_id) REFERENCES entries(id) ON DELETE CASCADE
);

-- migrations.rs：MIGRATIONS 追加 Migration{ version:5, name:"user_identities",
--   sql: include_str!("../../migrations/0005_user_identities.sql") }。
-- Agent 本地库无需新迁移：reconcile 复用现有 executed_commands（agent_0001）。
-- §10.4 计量/观测表（usage_buckets/traffic_*/user_runtime_state/entry_runtime_state）留 Phase 5。