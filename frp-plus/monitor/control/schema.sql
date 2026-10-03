-- Current control database. Schema v9 adds durable restoration takeover receipts.
CREATE TABLE nodes (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 name TEXT NOT NULL,
 public_note TEXT NOT NULL DEFAULT '',
 private_note TEXT NOT NULL DEFAULT '',
 is_public INTEGER NOT NULL DEFAULT 1 CHECK (is_public IN (0,1)),
 publish_billing INTEGER NOT NULL DEFAULT 0 CHECK (publish_billing IN (0,1)),
 publish_traffic_plan INTEGER NOT NULL DEFAULT 1 CHECK (publish_traffic_plan IN (0,1)),
 price_minor INTEGER CHECK (price_minor IS NULL OR price_minor >= 0),
 currency TEXT,
 billing_cycle TEXT,
 expires_at_ms INTEGER,
 renewal_note TEXT NOT NULL DEFAULT '',
 traffic_quota_bytes TEXT,
 traffic_mode TEXT NOT NULL DEFAULT 'max' CHECK (traffic_mode IN ('max','total','rx','tx')),
 traffic_reset_mode TEXT NOT NULL DEFAULT 'monthly' CHECK (traffic_reset_mode IN ('monthly','manual')),
 traffic_reset_day INTEGER NOT NULL DEFAULT 1 CHECK (traffic_reset_day BETWEEN 1 AND 31),
 traffic_reset_timezone TEXT NOT NULL DEFAULT 'UTC',
 traffic_period_start_at_ms INTEGER,
 traffic_period_end_at_ms INTEGER,
 traffic_period_rx_bytes TEXT NOT NULL DEFAULT '0',
 traffic_period_tx_bytes TEXT NOT NULL DEFAULT '0',
 traffic_adjustment_bytes TEXT NOT NULL DEFAULT '0',
 traffic_period_partial INTEGER NOT NULL DEFAULT 1 CHECK (traffic_period_partial IN (0,1)),
 traffic_day TEXT,
 traffic_today_rx_bytes TEXT NOT NULL DEFAULT '0',
 traffic_today_tx_bytes TEXT NOT NULL DEFAULT '0',
 traffic_today_partial INTEGER NOT NULL DEFAULT 1 CHECK (traffic_today_partial IN (0,1)),
 counter_boot_id TEXT,
 counter_interface TEXT,
 counter_rx_bytes TEXT,
 counter_tx_bytes TEXT,
 counter_received_at_ms INTEGER,
 config_revision INTEGER NOT NULL DEFAULT 1,
 created_at_ms INTEGER NOT NULL,
 updated_at_ms INTEGER NOT NULL,
 token_sha256 TEXT NOT NULL UNIQUE CHECK (length(token_sha256)=64 AND token_sha256 NOT GLOB '*[^0-9a-f]*'),
 frp_binding TEXT CHECK (frp_binding IS NULL OR (json_valid(frp_binding) AND json_type(frp_binding)='object'))
);
CREATE UNIQUE INDEX nodes_frp_binding ON nodes (
 json_extract(frp_binding,'$.server_id'),
 json_extract(frp_binding,'$.user'),
 json_extract(frp_binding,'$.raw_client_id')
) WHERE frp_binding IS NOT NULL;
CREATE TABLE settings (
 id INTEGER PRIMARY KEY CHECK (id=1),
 probe_json TEXT NOT NULL CHECK (json_valid(probe_json) AND json_type(probe_json)='object')
);
INSERT INTO settings(id,probe_json) VALUES (1,'{"version":1,"nodes":[]}');
-- Node groups (schema v5).
CREATE TABLE node_groups (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 name TEXT NOT NULL UNIQUE,
 config_revision INTEGER NOT NULL DEFAULT 1
);
CREATE TABLE node_group_members (
 group_id INTEGER NOT NULL REFERENCES node_groups(id) ON DELETE CASCADE,
 node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
 PRIMARY KEY (group_id,node_id)
);
CREATE INDEX node_group_members_node_id ON node_group_members(node_id);
-- Configuration operations (schema v7).
-- node_id deliberately has no foreign key: deleting a node retains its audit.
CREATE TABLE config_operations (
 operation_id TEXT PRIMARY KEY NOT NULL,
 node_id INTEGER NOT NULL CHECK (node_id > 0),
 service_id TEXT NOT NULL,
 base_revision TEXT NOT NULL CHECK (length(base_revision)=64 AND base_revision NOT GLOB '*[^0-9a-f]*'),
 candidate_digest TEXT NOT NULL CHECK (candidate_digest='' OR (length(candidate_digest)=64 AND candidate_digest NOT GLOB '*[^0-9a-f]*')),
 creator TEXT NOT NULL,
 deadline_at_ms INTEGER NOT NULL,
 idempotency_key TEXT NOT NULL UNIQUE,
 request_digest TEXT NOT NULL CHECK (length(request_digest)=64 AND request_digest NOT GLOB '*[^0-9a-f]*'),
 state TEXT NOT NULL CHECK (state IN ('draft','validated','prepared','applying','verifying','confirmed','rejected','conflict','failed','outcome_unknown','cancelled','rolling_back','rolled_back','rollback_failed')),
 version INTEGER NOT NULL CHECK (version >= 1),
 created_at_ms INTEGER NOT NULL,
 updated_at_ms INTEGER NOT NULL
);
CREATE UNIQUE INDEX config_operations_active_node ON config_operations(node_id)
 WHERE state IN ('draft','validated','prepared','applying','verifying','outcome_unknown','rolling_back','rollback_failed');
CREATE INDEX config_operations_node_created ON config_operations(node_id,created_at_ms,operation_id);
CREATE TABLE config_operation_events (
 event_id INTEGER PRIMARY KEY AUTOINCREMENT,
 operation_id TEXT NOT NULL REFERENCES config_operations(operation_id),
 version INTEGER NOT NULL CHECK (version >= 1),
 state TEXT NOT NULL,
 code TEXT NOT NULL,
 changes_json TEXT NOT NULL CHECK (length(changes_json)<=65536 AND json_valid(changes_json) AND json_type(changes_json)='array'),
 created_at_ms INTEGER NOT NULL,
 UNIQUE(operation_id,version)
);
CREATE INDEX config_operation_events_operation ON config_operation_events(operation_id,event_id);
-- Configuration observations (schema v8).
ALTER TABLE config_operations ADD COLUMN agent_result_json TEXT CHECK (agent_result_json IS NULL OR (length(agent_result_json)<=8192 AND json_valid(agent_result_json) AND json_type(agent_result_json)='object'));
ALTER TABLE config_operations ADD COLUMN agent_observed_at_ms INTEGER;
ALTER TABLE config_operation_events ADD COLUMN actor TEXT NOT NULL DEFAULT '';
UPDATE config_operation_events SET actor=(SELECT creator FROM config_operations WHERE operation_id=config_operation_events.operation_id);
-- Configuration restoration takeovers (schema v9).
CREATE TABLE config_restores (
 id TEXT PRIMARY KEY,
 node_id INTEGER NOT NULL CHECK (node_id > 0),
 token_sha256 TEXT NOT NULL CHECK (length(token_sha256)=64 AND token_sha256 NOT GLOB '*[^0-9a-f]*'),
 service_id TEXT NOT NULL,
 epoch TEXT NOT NULL,
 backup_service_id TEXT NOT NULL,
 replaced_service_id TEXT NOT NULL DEFAULT '',
 manifest_digest TEXT NOT NULL,
 context_revision TEXT NOT NULL,
 store_digest TEXT NOT NULL,
 state TEXT NOT NULL CHECK (state IN ('pending','acknowledged')),
 creator TEXT NOT NULL,
 created_at_ms INTEGER NOT NULL,
 updated_at_ms INTEGER NOT NULL,
 version INTEGER NOT NULL CHECK (version >= 1),
 UNIQUE(node_id,epoch)
);
CREATE INDEX config_restores_node ON config_restores(node_id,created_at_ms DESC);
-- Database identity; must match the restore check in scripts/ops.py
-- and the startup check in monitor/control/store.go.
PRAGMA application_id=1179798836;
PRAGMA user_version=9;
