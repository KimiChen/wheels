-- Add gateway traffic byte estimates to usage logs and dashboard aggregates.
-- request_bytes/response_bytes are app-side estimates including HTTP headers plus
-- configurable TLS/TCP/IP overhead, intended to trend closer to NIC counters.

ALTER TABLE usage_logs
    ADD COLUMN IF NOT EXISTS request_bytes BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS response_bytes BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS traffic_source VARCHAR(32),
    ADD COLUMN IF NOT EXISTS traffic_estimated BOOLEAN NOT NULL DEFAULT FALSE;

ALTER TABLE usage_dashboard_hourly
    ADD COLUMN IF NOT EXISTS request_bytes BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS response_bytes BIGINT NOT NULL DEFAULT 0;

ALTER TABLE usage_dashboard_daily
    ADD COLUMN IF NOT EXISTS request_bytes BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS response_bytes BIGINT NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS idx_usage_logs_request_id
    ON usage_logs (request_id);

COMMENT ON COLUMN usage_logs.request_bytes IS 'Estimated inbound client-to-gateway wire bytes for this request.';
COMMENT ON COLUMN usage_logs.response_bytes IS 'Estimated outbound gateway-to-client wire bytes for this response.';
COMMENT ON COLUMN usage_logs.traffic_source IS 'Traffic byte source, e.g. app_estimate.';
COMMENT ON COLUMN usage_logs.traffic_estimated IS 'Whether traffic bytes include estimated transport overhead rather than exact NIC counters.';
