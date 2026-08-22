-- Add upstream gateway traffic byte estimates.
-- Direction semantics:
--   request_bytes: client -> gateway
--   response_bytes: gateway -> client
--   upstream_request_bytes: gateway -> upstream API
--   upstream_response_bytes: upstream API -> gateway

ALTER TABLE usage_logs
    ADD COLUMN IF NOT EXISTS upstream_request_bytes BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS upstream_response_bytes BIGINT NOT NULL DEFAULT 0;

ALTER TABLE usage_dashboard_hourly
    ADD COLUMN IF NOT EXISTS upstream_request_bytes BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS upstream_response_bytes BIGINT NOT NULL DEFAULT 0;

ALTER TABLE usage_dashboard_daily
    ADD COLUMN IF NOT EXISTS upstream_request_bytes BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS upstream_response_bytes BIGINT NOT NULL DEFAULT 0;

COMMENT ON COLUMN usage_logs.upstream_request_bytes IS 'Estimated outbound gateway-to-upstream API wire bytes for this request.';
COMMENT ON COLUMN usage_logs.upstream_response_bytes IS 'Estimated inbound upstream API-to-gateway wire bytes for this request.';
