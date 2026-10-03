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
-- Configuration audit ledger (schema v10).
CREATE TABLE config_audit_entries (
 audit_id INTEGER PRIMARY KEY AUTOINCREMENT,
 recorded_at_ms INTEGER NOT NULL CHECK (recorded_at_ms > 0),
 occurred_at_ms INTEGER,
 kind TEXT NOT NULL CHECK (kind IN ('state','preview','dispatch','retry','restore','external_drift','export')),
 actor_kind TEXT NOT NULL CHECK (actor_kind IN ('github','system','unknown')),
 actor TEXT NOT NULL DEFAULT '',
 observer TEXT NOT NULL DEFAULT '',
 node_id INTEGER,
 service_id TEXT NOT NULL DEFAULT '',
 operation_id TEXT NOT NULL DEFAULT '',
 operation_version INTEGER,
 state TEXT NOT NULL DEFAULT '',
 code TEXT NOT NULL,
 object_count INTEGER NOT NULL CHECK (object_count >= 0),
 field_count INTEGER NOT NULL CHECK (field_count >= 0),
 summary_json TEXT CHECK (summary_json IS NULL OR (length(summary_json)<=8192 AND json_valid(summary_json) AND json_type(summary_json)='object')),
 changes_json TEXT NOT NULL CHECK (length(changes_json)<=65536 AND json_valid(changes_json) AND json_type(changes_json)='array'),
 origin_kind TEXT NOT NULL,
 origin_id TEXT NOT NULL,
 UNIQUE(origin_kind,origin_id)
);
CREATE INDEX config_audit_time ON config_audit_entries(recorded_at_ms DESC,audit_id DESC);
CREATE INDEX config_audit_node ON config_audit_entries(node_id,recorded_at_ms DESC,audit_id DESC);
CREATE INDEX config_audit_actor ON config_audit_entries(actor_kind,actor,recorded_at_ms DESC,audit_id DESC);
CREATE INDEX config_audit_operation ON config_audit_entries(operation_id,audit_id);
CREATE INDEX config_audit_state ON config_audit_entries(state,recorded_at_ms DESC,audit_id DESC);
CREATE TABLE config_audit_objects (
 audit_id INTEGER NOT NULL REFERENCES config_audit_entries(audit_id) ON DELETE CASCADE,
 kind TEXT NOT NULL CHECK (kind IN ('proxy','visitor')),
 name TEXT NOT NULL,
 action TEXT NOT NULL,
 PRIMARY KEY(audit_id,kind,name)
);
CREATE INDEX config_audit_object_name ON config_audit_objects(kind,name,audit_id);
CREATE TABLE config_audit_observations (
 node_id INTEGER NOT NULL CHECK (node_id > 0),
 service_id TEXT NOT NULL,
 context_revision TEXT NOT NULL,
 updated_at_ms INTEGER NOT NULL,
 PRIMARY KEY(node_id,service_id)
);
-- Old events retain their original facts; unavailable previews stay NULL.
WITH event_targets AS (
 SELECT e.*,COALESCE(NULLIF(e.changes_json,'[]'),(SELECT first.changes_json FROM config_operation_events first WHERE first.operation_id=e.operation_id AND first.changes_json<>'[]' ORDER BY first.event_id LIMIT 1),'[]') AS targets FROM config_operation_events e
)
INSERT OR IGNORE INTO config_audit_entries(recorded_at_ms,kind,actor_kind,actor,node_id,service_id,operation_id,operation_version,state,code,object_count,field_count,changes_json,origin_kind,origin_id)
 SELECT e.created_at_ms,'state',CASE WHEN e.actor='' THEN 'unknown' WHEN e.actor LIKE 'system:%' THEN 'system' ELSE 'github' END,e.actor,o.node_id,o.service_id,e.operation_id,e.version,e.state,e.code,json_array_length(e.targets),COALESCE((SELECT sum(json_array_length(json_extract(j.value,'$.fields'))) FROM json_each(e.targets) j),0),e.targets,'operation_event',CAST(e.event_id AS TEXT)
 FROM event_targets e JOIN config_operations o ON o.operation_id=e.operation_id ORDER BY e.event_id;
INSERT OR IGNORE INTO config_audit_objects(audit_id,kind,name,action)
 SELECT a.audit_id,json_extract(j.value,'$.kind'),json_extract(j.value,'$.name'),json_extract(j.value,'$.action')
 FROM config_audit_entries a,json_each(a.changes_json) j WHERE a.origin_kind='operation_event';
INSERT OR IGNORE INTO config_audit_entries(recorded_at_ms,kind,actor_kind,actor,node_id,service_id,state,code,object_count,field_count,changes_json,origin_kind,origin_id)
 SELECT updated_at_ms,'restore',CASE WHEN creator='' THEN 'unknown' WHEN creator LIKE 'system:%' THEN 'system' ELSE 'github' END,creator,node_id,service_id,state,CASE WHEN state='pending' THEN 'restore_pending' ELSE 'restore_acknowledged' END,0,0,'[]','restore_receipt',id||':'||version FROM config_restores;
-- Tunnel history (schema v11).
-- Bindings are immutable intervals. Comparison excludes ordinary counter saves.
CREATE TABLE tunnel_binding_epochs (
 binding_epoch INTEGER PRIMARY KEY AUTOINCREMENT,
 node_id INTEGER NOT NULL,
 binding_json TEXT,
 started_at_ms INTEGER NOT NULL,
 ended_at_ms INTEGER
);
CREATE UNIQUE INDEX tunnel_binding_current ON tunnel_binding_epochs(node_id) WHERE ended_at_ms IS NULL;
INSERT INTO tunnel_binding_epochs(node_id,binding_json,started_at_ms) SELECT id,frp_binding,updated_at_ms FROM nodes;
CREATE TRIGGER tunnel_binding_insert AFTER INSERT ON nodes BEGIN
 INSERT INTO tunnel_binding_epochs(node_id,binding_json,started_at_ms) VALUES(NEW.id,NEW.frp_binding,NEW.created_at_ms);
END;
CREATE TRIGGER tunnel_binding_update AFTER UPDATE OF frp_binding ON nodes WHEN OLD.frp_binding IS NOT NEW.frp_binding BEGIN
 UPDATE tunnel_binding_epochs SET ended_at_ms=NEW.updated_at_ms WHERE node_id=NEW.id AND ended_at_ms IS NULL;
 INSERT INTO tunnel_binding_epochs(node_id,binding_json,started_at_ms) VALUES(NEW.id,NEW.frp_binding,NEW.updated_at_ms);
END;
CREATE TRIGGER tunnel_binding_delete AFTER DELETE ON nodes BEGIN
 UPDATE tunnel_binding_epochs SET ended_at_ms=CAST(unixepoch('subsec')*1000 AS INTEGER) WHERE node_id=OLD.id AND ended_at_ms IS NULL;
END;
CREATE TABLE tunnels (
 tunnel_id INTEGER PRIMARY KEY AUTOINCREMENT,
 logical_key TEXT NOT NULL UNIQUE,
 node_id INTEGER,
 binding_epoch INTEGER NOT NULL DEFAULT 0,
 server_id TEXT NOT NULL,
 user_name TEXT NOT NULL,
 raw_client_id TEXT,
 kind TEXT NOT NULL CHECK(kind IN ('proxy','visitor')),
 raw_name TEXT NOT NULL,
 identity_quality TEXT NOT NULL CHECK(identity_quality IN ('stable','instance_only')),
 created_at_ms INTEGER NOT NULL
);
CREATE INDEX tunnels_node ON tunnels(node_id,tunnel_id);
CREATE TABLE tunnel_generations (
 tunnel_id INTEGER NOT NULL REFERENCES tunnels(tunnel_id),
 source TEXT NOT NULL,
 generation INTEGER NOT NULL,
 PRIMARY KEY(tunnel_id,source)
);
CREATE TABLE tunnel_instances (
 instance_id INTEGER PRIMARY KEY AUTOINCREMENT,
 tunnel_id INTEGER NOT NULL REFERENCES tunnels(tunnel_id),
 source_key TEXT NOT NULL,
 source TEXT NOT NULL CHECK(source IN ('server','client')),
 process_epoch TEXT NOT NULL,
 native_instance_id TEXT NOT NULL,
 generation INTEGER NOT NULL,
 origin_json TEXT NOT NULL,
 object_json TEXT NOT NULL,
 state_key TEXT NOT NULL,
 created_at_ms INTEGER NOT NULL,
 closed_at_ms INTEGER,
 last_observed_at_ms INTEGER NOT NULL,
 UNIQUE(source_key,process_epoch,native_instance_id),
 UNIQUE(tunnel_id,source,generation)
);
CREATE INDEX tunnel_instances_tunnel ON tunnel_instances(tunnel_id,instance_id);
CREATE TABLE tunnel_sources (
 source_key TEXT NOT NULL,
 process_epoch TEXT NOT NULL,
 collector_epoch TEXT NOT NULL,
 last_sequence TEXT NOT NULL,
 dropped_events TEXT NOT NULL,
 snapshot_state TEXT NOT NULL,
 updated_at_ms INTEGER NOT NULL,
 PRIMARY KEY(source_key,process_epoch)
);
CREATE TABLE tunnel_events (
 event_id INTEGER PRIMARY KEY AUTOINCREMENT,
 tunnel_id INTEGER REFERENCES tunnels(tunnel_id),
 instance_id INTEGER REFERENCES tunnel_instances(instance_id),
 source_key TEXT NOT NULL,
 process_epoch TEXT NOT NULL,
 native_sequence TEXT,
 code TEXT NOT NULL,
 state_json TEXT,
 time_basis TEXT NOT NULL,
 occurred_at_ms INTEGER,
 received_at_ms INTEGER NOT NULL,
 UNIQUE(source_key,process_epoch,native_sequence)
);
CREATE INDEX tunnel_events_tunnel ON tunnel_events(tunnel_id,event_id DESC);
CREATE INDEX tunnel_events_source ON tunnel_events(source_key,process_epoch,event_id DESC);
CREATE TABLE tunnel_retention (
 id INTEGER PRIMARY KEY CHECK(id=1),
 pruned_through_ms INTEGER NOT NULL DEFAULT 0,
 pruned_events INTEGER NOT NULL DEFAULT 0
);
INSERT INTO tunnel_retention(id) VALUES(1);
-- Database identity; must match the restore check in scripts/ops.py
-- and the startup check in monitor/control/store.go.
PRAGMA application_id=1179798836;
PRAGMA user_version=11;
