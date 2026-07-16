-- 0002 Phase 1：Host/Agent 数据模型 + 双 CA PKI（私钥内联信封）+ 命令幂等 + 发布门禁观测。
-- 约定同 0001：所有时间为 UTC Unix 秒（INTEGER）；枚举用 CHECK；外键显式声明删除策略。
-- 全部为 additive（新表 + ALTER ADD COLUMN）；不重建 credentials/credential_versions/agent_certificates
-- （运行器在 foreign_keys=ON 的单事务内执行，重建带外键子表不安全且会毁坏已存密文）。

-- === PKI：双根 CA（角色分离：agent_ca 只签服务端证书，client_ca 只签 Manager 客户端证书）===
-- CA 公证书公开明文；CA 私钥（PKCS#8 PEM）经 crypto::Cipher 信封加密，列形状与 credential_versions 一致。
CREATE TABLE ca_keypairs (
    id           TEXT PRIMARY KEY,
    role         TEXT NOT NULL CHECK (role IN ('agent_ca','client_ca')),
    cert_pem     TEXT NOT NULL,               -- CA 公证书（公开）
    spki_sha256  TEXT NOT NULL,               -- CA SubjectPublicKeyInfo 指纹（hex 小写）
    alg          INTEGER NOT NULL,            -- 信封算法版本（= crypto::Sealed.alg）
    key_version  INTEGER NOT NULL,            -- 主密钥版本（= crypto::Sealed.key_version，供 Phase 6 重密钥）
    nonce        BLOB NOT NULL,               -- 信封 nonce（24B）
    ciphertext   BLOB NOT NULL,               -- CA 私钥密文（PKCS#8 PEM 经 Cipher.seal）
    not_before   INTEGER NOT NULL,
    not_after    INTEGER NOT NULL,
    next_serial  INTEGER NOT NULL DEFAULT 2,  -- 该 CA 下一叶证书序列（原子自增；1 预留）
    active       INTEGER NOT NULL DEFAULT 1,
    created_at   INTEGER NOT NULL
);
-- 每个角色至多一个 active CA。
CREATE UNIQUE INDEX idx_ca_active_role ON ca_keypairs(role) WHERE active = 1;

-- === Manager 客户端身份（唯一；client_ca 签发；私钥走既有 credentials(kind='manager_client_cert') 信封）===
CREATE TABLE manager_identity (
    id            TEXT PRIMARY KEY,
    credential_id TEXT NOT NULL REFERENCES credentials(id) ON DELETE RESTRICT,
    ca_keypair_id TEXT NOT NULL REFERENCES ca_keypairs(id) ON DELETE RESTRICT,
    cert_pem      TEXT NOT NULL,              -- Manager 客户端公证书（公开）
    spki_sha256   TEXT NOT NULL,              -- 打入每个 enrollment 包的 pin，Agent 在握手层强制
    not_before    INTEGER NOT NULL,
    not_after     INTEGER NOT NULL,
    active        INTEGER NOT NULL DEFAULT 1,
    created_at    INTEGER NOT NULL
);
-- 至多一个 active Manager 身份。
CREATE UNIQUE INDEX idx_manager_identity_active ON manager_identity(active) WHERE active = 1;

-- === agent_certificates 扩展：保存 Agent 服务端叶证书公开元数据（私钥仍走 credentials 信封，不变）===
ALTER TABLE agent_certificates ADD COLUMN cert_pem      TEXT;
ALTER TABLE agent_certificates ADD COLUMN spki_sha256   TEXT;
ALTER TABLE agent_certificates ADD COLUMN san_json      TEXT;    -- SAN 列表 JSON（含 IP/DNS 与 URI sbm://host/<id>）
ALTER TABLE agent_certificates ADD COLUMN serial        INTEGER;
ALTER TABLE agent_certificates ADD COLUMN ca_keypair_id TEXT REFERENCES ca_keypairs(id) ON DELETE RESTRICT;
ALTER TABLE agent_certificates ADD COLUMN not_before    INTEGER;
ALTER TABLE agent_certificates ADD COLUMN not_after     INTEGER;

-- === agents 扩展：实际状态观测（与期望状态分离），供发布门禁与详情页 ===
ALTER TABLE agents ADD COLUMN last_ok_at           INTEGER;
ALTER TABLE agents ADD COLUMN last_error           TEXT;
ALTER TABLE agents ADD COLUMN agent_version        TEXT;
ALTER TABLE agents ADD COLUMN singbox_running      INTEGER NOT NULL DEFAULT 0;
ALTER TABLE agents ADD COLUMN os_info              TEXT;
ALTER TABLE agents ADD COLUMN consecutive_failures INTEGER NOT NULL DEFAULT 0;

-- === 轮询快照历史（实际状态时间线；agents 行存最新去规范化值）===
CREATE TABLE agent_status_snapshots (
    id               TEXT PRIMARY KEY,
    host_id          TEXT NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
    ok               INTEGER NOT NULL,
    singbox_version  TEXT,
    agent_version    TEXT,
    current_revision INTEGER,
    singbox_running  INTEGER NOT NULL DEFAULT 0,
    sys_info_json    TEXT,                     -- 非密系统信息（os/arch/uptime）
    error_code       TEXT,
    polled_at        INTEGER NOT NULL
);
CREATE INDEX idx_snapshots_host_time ON agent_status_snapshots(host_id, polled_at);

-- === Agent 命令（Manager 侧编排；command_id 幂等）===
CREATE TABLE agent_commands (
    command_id      TEXT PRIMARY KEY,          -- Manager 生成 UUIDv4；即发给 Agent 的幂等键
    host_id         TEXT NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
    kind            TEXT NOT NULL CHECK (kind IN
                      ('status','stats','users','reconcile','deploy','meter_ack','rollback')),
    idempotency_key TEXT NOT NULL,             -- 业务级去重键（同一逻辑操作稳定）
    request_hash    TEXT NOT NULL,             -- sha256(规范化 body)，检测同键不同体（409）
    request_json    TEXT NOT NULL,             -- 参数（禁明文密钥；只放句柄/摘要）
    status          TEXT NOT NULL DEFAULT 'pending' CHECK (status IN
                      ('pending','in_flight','succeeded','failed','timed_out','canceled')),
    attempts        INTEGER NOT NULL DEFAULT 0,
    max_attempts    INTEGER NOT NULL DEFAULT 3,
    timeout_ms      INTEGER NOT NULL DEFAULT 15000,
    not_before      INTEGER NOT NULL DEFAULT 0,
    deadline_at     INTEGER,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    dispatched_at   INTEGER,
    completed_at    INTEGER
);
-- 幂等：同一 (host_id, idempotency_key) 只存一条命令；Manager 创建走 INSERT ON CONFLICT DO NOTHING + SELECT。
CREATE UNIQUE INDEX idx_cmd_idem ON agent_commands(host_id, idempotency_key);
-- 派发领取：按 status + not_before 扫描待发命令。
CREATE INDEX idx_cmd_dispatch ON agent_commands(status, not_before);

-- === 命令结果（1:1；结果入库前脱敏）===
CREATE TABLE agent_command_results (
    command_id            TEXT PRIMARY KEY REFERENCES agent_commands(command_id) ON DELETE CASCADE,
    ok                    INTEGER NOT NULL,
    http_status           INTEGER,
    result_json           TEXT,               -- 已脱敏 Agent 响应（禁明文密钥）
    agent_echo_command_id TEXT,               -- Agent 回显 command_id，交叉核对
    error_code            TEXT,
    error_message         TEXT,
    observed_at           INTEGER NOT NULL
);

-- === enrollment 签发审计（不存任何私钥；供轮换与带外交付追踪）===
CREATE TABLE enrollment_packages (
    id                 TEXT PRIMARY KEY,
    host_id            TEXT NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
    serial             INTEGER NOT NULL,
    package_fp_sha256  TEXT NOT NULL,          -- 包规范字节指纹（打印供 OOB 校验）
    cert_spki_sha256   TEXT NOT NULL,          -- 该 Agent 服务端证书指纹
    not_after          INTEGER,
    issued_by          TEXT,
    delivered          INTEGER NOT NULL DEFAULT 0,
    created_at         INTEGER NOT NULL
);
CREATE UNIQUE INDEX idx_enroll_host_serial ON enrollment_packages(host_id, serial);

-- === 健康事件（观测；发布门禁与 Web 详情用；todo §10.4 子集）===
CREATE TABLE health_events (
    id         TEXT PRIMARY KEY,
    host_id    TEXT REFERENCES hosts(id) ON DELETE CASCADE,
    kind       TEXT NOT NULL,                  -- poll_failure / cert_expiring / gate_blocked ...
    detail     TEXT,
    created_at INTEGER NOT NULL
);
CREATE INDEX idx_health_host_time ON health_events(host_id, created_at);
