-- 0001 init：控制面 + 用户/密钥（todo.md §10.1、§10.2）。
-- 约定：所有时间为 UTC Unix 秒（INTEGER）；枚举用 CHECK 约束；外键显式声明删除策略。

CREATE TABLE settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE hosts (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    note       TEXT,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE host_capabilities (
    host_id    TEXT NOT NULL,
    capability TEXT NOT NULL CHECK (capability IN ('manage','entry','node')),
    PRIMARY KEY (host_id, capability),
    FOREIGN KEY (host_id) REFERENCES hosts(id) ON DELETE CASCADE
);

-- 密钥：credentials 为逻辑句柄，密文按版本存 credential_versions（信封加密）。
CREATE TABLE credentials (
    id         TEXT PRIMARY KEY,
    kind       TEXT NOT NULL CHECK (kind IN (
                   'entry_psk','node_psk','user_route_upsk','user_route_uuid',
                   'subscription_token','landing_auth','agent_server_cert','manager_client_cert')),
    scope      TEXT,
    created_at INTEGER NOT NULL
);

CREATE TABLE credential_versions (
    credential_id TEXT NOT NULL,
    version       INTEGER NOT NULL,
    alg           INTEGER NOT NULL,
    key_version   INTEGER NOT NULL,
    nonce         BLOB NOT NULL,
    ciphertext    BLOB NOT NULL,
    active        INTEGER NOT NULL DEFAULT 1,
    created_at    INTEGER NOT NULL,
    PRIMARY KEY (credential_id, version),
    FOREIGN KEY (credential_id) REFERENCES credentials(id) ON DELETE CASCADE
);

CREATE TABLE agents (
    host_id          TEXT PRIMARY KEY,
    mgmt_address     TEXT NOT NULL,
    status           TEXT NOT NULL DEFAULT 'unknown' CHECK (status IN ('unknown','online','offline','error')),
    singbox_version  TEXT,
    current_revision INTEGER,
    last_polled_at   INTEGER,
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL,
    FOREIGN KEY (host_id) REFERENCES hosts(id) ON DELETE CASCADE
);

CREATE TABLE agent_certificates (
    host_id       TEXT PRIMARY KEY,
    credential_id TEXT NOT NULL,
    trust_status  TEXT NOT NULL DEFAULT 'pending' CHECK (trust_status IN ('pending','trusted','revoked')),
    created_at    INTEGER NOT NULL,
    FOREIGN KEY (host_id) REFERENCES hosts(id) ON DELETE CASCADE,
    FOREIGN KEY (credential_id) REFERENCES credentials(id) ON DELETE CASCADE
);

CREATE TABLE entries (
    id               TEXT PRIMARY KEY,
    host_id          TEXT NOT NULL,
    public_address   TEXT NOT NULL,
    port             INTEGER NOT NULL DEFAULT 19736,
    inbound_kind     TEXT NOT NULL CHECK (inbound_kind IN ('shadowsocks','vless-reality')),
    ss_method        TEXT,
    allow_direct     INTEGER NOT NULL DEFAULT 0,
    current_revision INTEGER,
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL,
    FOREIGN KEY (host_id) REFERENCES hosts(id) ON DELETE RESTRICT
);

CREATE TABLE nodes (
    id                TEXT PRIMARY KEY,
    host_id           TEXT NOT NULL,
    data_address      TEXT NOT NULL,
    port              INTEGER NOT NULL DEFAULT 29736,
    allow_direct_exit INTEGER NOT NULL DEFAULT 1,
    current_revision  INTEGER,
    created_at        INTEGER NOT NULL,
    updated_at        INTEGER NOT NULL,
    FOREIGN KEY (host_id) REFERENCES hosts(id) ON DELETE RESTRICT
);

CREATE TABLE landings (
    id            TEXT PRIMARY KEY,
    kind          TEXT NOT NULL CHECK (kind IN ('managed_node','socks5')),
    node_id       TEXT,
    socks5_address TEXT,
    socks5_port   INTEGER,
    network       TEXT NOT NULL DEFAULT 'both' CHECK (network IN ('tcp','udp','both')),
    auth_credential_id TEXT,
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL,
    FOREIGN KEY (node_id) REFERENCES nodes(id) ON DELETE RESTRICT,
    FOREIGN KEY (auth_credential_id) REFERENCES credentials(id) ON DELETE SET NULL
);

CREATE TABLE routes (
    id              TEXT PRIMARY KEY,
    label           TEXT NOT NULL UNIQUE,
    entry_id        TEXT NOT NULL,
    exit_kind       TEXT NOT NULL CHECK (exit_kind IN ('entry_direct','node','landing')),
    exit_node_id    TEXT,
    exit_landing_id TEXT,
    status          TEXT NOT NULL DEFAULT 'draft' CHECK (status IN ('draft','active','disabled')),
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    FOREIGN KEY (entry_id) REFERENCES entries(id) ON DELETE CASCADE,
    FOREIGN KEY (exit_node_id) REFERENCES nodes(id) ON DELETE RESTRICT,
    FOREIGN KEY (exit_landing_id) REFERENCES landings(id) ON DELETE RESTRICT
);

CREATE TABLE route_hops (
    route_id TEXT NOT NULL,
    position INTEGER NOT NULL,
    node_id  TEXT NOT NULL,
    PRIMARY KEY (route_id, position),
    FOREIGN KEY (route_id) REFERENCES routes(id) ON DELETE CASCADE,
    FOREIGN KEY (node_id) REFERENCES nodes(id) ON DELETE RESTRICT
);
-- 同一 Route 不能重复经过同一 Node。
CREATE UNIQUE INDEX idx_route_hops_unique_node ON route_hops(route_id, node_id);

CREATE TABLE users (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    quota_bytes INTEGER NOT NULL DEFAULT 0,
    reset_cycle TEXT NOT NULL DEFAULT 'monthly' CHECK (reset_cycle IN ('monthly','yearly','never')),
    expire_at   INTEGER,
    disabled    INTEGER NOT NULL DEFAULT 0,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);

CREATE TABLE user_routes (
    user_id    TEXT NOT NULL,
    route_id   TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (user_id, route_id),
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
    FOREIGN KEY (route_id) REFERENCES routes(id) ON DELETE CASCADE
);

CREATE TABLE subscription_tokens (
    user_id       TEXT PRIMARY KEY,
    token_hash    TEXT NOT NULL UNIQUE,
    credential_id TEXT,
    created_at    INTEGER NOT NULL,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
    FOREIGN KEY (credential_id) REFERENCES credentials(id) ON DELETE SET NULL
);

CREATE TABLE audit_logs (
    id          TEXT PRIMARY KEY,
    actor       TEXT,
    action      TEXT NOT NULL,
    target_kind TEXT,
    target_id   TEXT,
    request_id  TEXT,
    detail      TEXT,
    created_at  INTEGER NOT NULL
);

CREATE INDEX idx_entries_host ON entries(host_id);
CREATE INDEX idx_nodes_host ON nodes(host_id);
CREATE INDEX idx_routes_entry ON routes(entry_id);
CREATE INDEX idx_user_routes_route ON user_routes(route_id);
CREATE INDEX idx_cred_versions_cred ON credential_versions(credential_id);
CREATE INDEX idx_audit_created ON audit_logs(created_at);
