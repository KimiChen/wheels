-- VLESS-Reality listener 运行材料。
-- private_key + short_id 合并为 JSON 后信封加密；public_key 与非秘密握手参数明文保存。
CREATE TABLE entry_reality (
    entry_id           TEXT PRIMARY KEY,
    flow               TEXT NOT NULL,
    public_key         TEXT NOT NULL,
    server_name        TEXT NOT NULL,
    handshake_server   TEXT NOT NULL,
    handshake_port     INTEGER NOT NULL,
    client_fingerprint TEXT NOT NULL DEFAULT 'chrome',
    alg                INTEGER NOT NULL,
    key_version        INTEGER NOT NULL,
    nonce              BLOB NOT NULL,
    ciphertext         BLOB NOT NULL,
    created_at         INTEGER NOT NULL,
    updated_at         INTEGER NOT NULL,
    FOREIGN KEY (entry_id) REFERENCES entries(id) ON DELETE CASCADE
);
