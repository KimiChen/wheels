-- migrations/0007_admin_auth.sql
-- Phase 6 管理员认证 + 会话 + RBAC + 审计（todo §11.2/§8）。
-- 约定沿用 0001-0006：UTC Unix 秒、枚举 CHECK、外键显式删除策略、additive-only。
-- audit_logs 表在 0001 已建、Phase 6 首次写入——此处仅补检索索引，不改结构。

CREATE TABLE admin_users (
    id                  TEXT PRIMARY KEY,
    username            TEXT NOT NULL UNIQUE,
    -- Argon2id PHC 字符串（含盐+参数）；非明文、非可逆、非信封加密（密码本就不可逆，不用 Cipher）。
    password_hash       TEXT NOT NULL,
    role                TEXT NOT NULL CHECK (role IN ('admin','operator','readonly')),
    disabled            INTEGER NOT NULL DEFAULT 0,
    -- 改密后使早于此刻签发的会话全部失效（session.created_at < password_changed_at 即作废）。
    password_changed_at INTEGER NOT NULL,
    last_login_at       INTEGER,
    -- 登录节流：失败计数 + 锁定窗口，防在线爆破。
    failed_attempts     INTEGER NOT NULL DEFAULT 0,
    locked_until        INTEGER,
    created_at          INTEGER NOT NULL,
    updated_at          INTEGER NOT NULL
);

CREATE TABLE admin_sessions (
    -- 只存会话 token 的 sha256（明文只在 Cookie，不入库，与 subscription_tokens 同范式）。
    id_hash             TEXT PRIMARY KEY,
    admin_id            TEXT NOT NULL REFERENCES admin_users(id) ON DELETE CASCADE,
    -- CSRF 同步器 token 的 sha256（不落 cookie，登录响应体下发明文供前端回填 X-CSRF-Token）。
    csrf_hash           TEXT NOT NULL,
    created_at          INTEGER NOT NULL,
    last_seen_at        INTEGER NOT NULL,
    -- 敏感操作 re-auth 时间戳；NULL=从未 re-auth。
    last_reauth_at      INTEGER,
    idle_expires_at     INTEGER NOT NULL,        -- 滑动过期：每次访问续期
    absolute_expires_at INTEGER NOT NULL,        -- 硬顶：不随访问续期
    ip                  TEXT,                    -- 观测/审计用（脱敏截断），非认证要素
    user_agent          TEXT
);
CREATE INDEX idx_admin_sessions_admin ON admin_sessions(admin_id);
CREATE INDEX idx_admin_sessions_exp   ON admin_sessions(idle_expires_at);

-- audit_logs（0001 已建）补检索索引：按目标、按 actor。
CREATE INDEX idx_audit_target ON audit_logs(target_kind, target_id);
CREATE INDEX idx_audit_actor  ON audit_logs(actor);
