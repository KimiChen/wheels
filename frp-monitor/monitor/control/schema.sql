-- Fresh P4 control database. No import or migration of development-era stores.
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
 counter_scope TEXT,
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
PRAGMA application_id=1179798836;
PRAGMA user_version=4;
