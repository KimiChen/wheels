-- 会话与服务端密钥（README §4.9）。
--
-- 会话 cookie 用 `__Host-` 前缀 + HttpOnly + Secure + SameSite=Lax；
-- 服务端会话表**可主动吊销**；写操作走双提交 CSRF token。
-- **会话不放任何 IM token。**

CREATE TABLE sessions (
    session_pk        INTEGER PRIMARY KEY AUTOINCREMENT,
    -- 存的是会话 ID 的**哈希**，不是 ID 本身。
    -- 库被读到也不能直接冒充会话——攻击者拿到的是摘要，反推不出 cookie 里那个值。
    session_id_sha256 TEXT NOT NULL UNIQUE CHECK(length(session_id_sha256) = 64),
    user_id           INTEGER NOT NULL REFERENCES users(user_id),
    -- provider 抽象：feishu / wecom / local（break-glass）。
    -- M4 只实现会话机制，登录入口属于 M5。
    provider          TEXT NOT NULL CHECK(provider IN ('feishu', 'wecom', 'local')),
    -- 双提交 CSRF：同样只存哈希。
    csrf_token_sha256 TEXT NOT NULL CHECK(length(csrf_token_sha256) = 64),
    created_at        TEXT NOT NULL,
    expires_at        TEXT NOT NULL,
    last_seen_at      TEXT NOT NULL,
    -- 主动吊销。**每请求检查**，本地撤销下一个请求立即生效（D13）。
    revoked_at        TEXT,
    -- 成员事实（D13）：上游账号是否仍有效、参与规则判断的组。
    -- TTL 默认 60 秒，**过期必须先刷新再决定授权**，不允许用过期事实继续放行。
    -- M4 不接 IM，这两列留空；M5 的 provider 负责填与刷新。
    member_fact_verified_at TEXT,
    member_fact_expires_at  TEXT
) STRICT;

CREATE INDEX idx_sessions_user ON sessions(user_id, revoked_at);

-- 服务端密钥。当前只有一个：会话 ID 的 HMAC 证明用的 key（§4.9 加固 4）。
--
-- 放库里而不是配置文件：它要随库一起进一致性备份（R3），
-- 而且与会话表同生共死——单独丢一个都会让所有会话失效，不如放在一起。
--
-- 它与会话 ID 哈希是**两道独立的机制**：拿到 key 也伪造不出一个哈希在表里的 ID。
CREATE TABLE server_secrets (
    name       TEXT PRIMARY KEY NOT NULL,
    secret_hex TEXT NOT NULL CHECK(length(secret_hex) = 64),
    created_at TEXT NOT NULL
) STRICT;
