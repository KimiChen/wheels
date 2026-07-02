use crate::service::TrafficService;
use axum::{
    extract::State,
    http::{header, HeaderMap, StatusCode},
    response::{IntoResponse, Response},
    routing::get,
    Json, Router,
};
use serde::Serialize;
use std::sync::Arc;

pub fn router(service: Arc<TrafficService>) -> Router {
    Router::new()
        .route("/", get(index))
        .route("/health", get(health))
        .route("/api/v1/traffic", get(traffic))
        .with_state(service)
}

async fn index() -> impl IntoResponse {
    (
        [(header::CONTENT_TYPE, "text/html; charset=utf-8")],
        INDEX_HTML,
    )
}

async fn health() -> Json<HealthResponse> {
    Json(HealthResponse { status: "ok" })
}

async fn traffic(
    State(service): State<Arc<TrafficService>>,
    headers: HeaderMap,
) -> Result<Json<impl Serialize>, ApiError> {
    authorize(&headers, service_auth_token(&service))?;
    let snapshot = service.snapshot().map_err(ApiError::internal)?;
    Ok(Json(snapshot))
}

fn authorize(headers: &HeaderMap, token: &str) -> Result<(), ApiError> {
    let Some(value) = headers.get(header::AUTHORIZATION) else {
        return Err(ApiError::unauthorized());
    };
    let Ok(value) = value.to_str() else {
        return Err(ApiError::unauthorized());
    };
    let Some(received) = value.strip_prefix("Bearer ") else {
        return Err(ApiError::unauthorized());
    };
    if !constant_time_eq(received.as_bytes(), token.as_bytes()) {
        return Err(ApiError::unauthorized());
    }
    Ok(())
}

fn constant_time_eq(left: &[u8], right: &[u8]) -> bool {
    if left.len() != right.len() {
        return false;
    }

    let mut diff = 0_u8;
    for (a, b) in left.iter().zip(right) {
        diff |= a ^ b;
    }
    diff == 0
}

fn service_auth_token(service: &TrafficService) -> &str {
    service.auth_token()
}

#[derive(Serialize)]
struct HealthResponse {
    status: &'static str,
}

struct ApiError {
    status: StatusCode,
}

impl ApiError {
    fn unauthorized() -> Self {
        Self {
            status: StatusCode::UNAUTHORIZED,
        }
    }

    fn internal(error: anyhow::Error) -> Self {
        tracing::error!(%error, "request failed");
        Self {
            status: StatusCode::INTERNAL_SERVER_ERROR,
        }
    }
}

impl IntoResponse for ApiError {
    fn into_response(self) -> Response {
        self.status.into_response()
    }
}

const INDEX_HTML: &str = r#"<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>vps-trafficd</title>
  <style>
    :root {
      color-scheme: light dark;
      --bg: #f7f8fa;
      --panel: #ffffff;
      --text: #17202a;
      --muted: #627084;
      --line: #d7dde5;
      --accent: #0f766e;
      --danger: #b42318;
      --code: #111827;
    }
    @media (prefers-color-scheme: dark) {
      :root {
        --bg: #121417;
        --panel: #1b2026;
        --text: #edf1f5;
        --muted: #a3adba;
        --line: #303844;
        --accent: #2dd4bf;
        --danger: #ff8a7a;
        --code: #f4f7fb;
      }
    }
    * { box-sizing: border-box; }
    body {
      margin: 0;
      min-height: 100vh;
      background: var(--bg);
      color: var(--text);
      font: 14px/1.5 system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
    }
    main {
      width: min(960px, calc(100% - 32px));
      margin: 0 auto;
      padding: 32px 0;
    }
    header {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 16px;
      margin-bottom: 18px;
    }
    h1 {
      margin: 0;
      font-size: 24px;
      font-weight: 700;
      letter-spacing: 0;
    }
    .actions {
      display: flex;
      gap: 8px;
      flex-wrap: wrap;
      justify-content: flex-end;
    }
    button {
      border: 1px solid var(--line);
      border-radius: 6px;
      background: var(--panel);
      color: var(--text);
      cursor: pointer;
      font: inherit;
      padding: 8px 12px;
    }
    button.primary {
      border-color: var(--accent);
      background: var(--accent);
      color: #ffffff;
    }
    section {
      background: var(--panel);
      border: 1px solid var(--line);
      border-radius: 8px;
      overflow: hidden;
    }
    .status {
      min-height: 42px;
      padding: 12px 14px;
      color: var(--muted);
      border-bottom: 1px solid var(--line);
    }
    .status.error {
      color: var(--danger);
    }
    dl {
      display: grid;
      grid-template-columns: repeat(2, minmax(0, 1fr));
      gap: 0;
      margin: 0;
    }
    .metric {
      padding: 14px;
      border-right: 1px solid var(--line);
      border-bottom: 1px solid var(--line);
      min-width: 0;
    }
    .metric:nth-child(2n) {
      border-right: 0;
    }
    dt {
      color: var(--muted);
      font-size: 12px;
      margin: 0 0 4px;
      text-transform: uppercase;
    }
    dd {
      margin: 0;
      font-size: 18px;
      font-weight: 650;
      overflow-wrap: anywhere;
    }
    pre {
      margin: 0;
      padding: 14px;
      max-height: 45vh;
      overflow: auto;
      color: var(--code);
      border-top: 1px solid var(--line);
      font: 12px/1.5 ui-monospace, SFMono-Regular, Consolas, "Liberation Mono", monospace;
      white-space: pre-wrap;
      overflow-wrap: anywhere;
    }
    @media (max-width: 640px) {
      header {
        align-items: stretch;
        flex-direction: column;
      }
      .actions {
        justify-content: stretch;
      }
      button {
        flex: 1 1 120px;
      }
      dl {
        grid-template-columns: 1fr;
      }
      .metric {
        border-right: 0;
      }
    }
  </style>
</head>
<body>
  <main>
    <header>
      <h1>vps-trafficd</h1>
      <div class="actions">
        <button id="change-token">Token</button>
        <button id="refresh" class="primary">Refresh</button>
      </div>
    </header>
    <section>
      <div id="status" class="status">Waiting for token.</div>
      <dl id="metrics"></dl>
      <pre id="raw">{}</pre>
    </section>
  </main>
  <script>
    let token = "";
    const statusEl = document.getElementById("status");
    const metricsEl = document.getElementById("metrics");
    const rawEl = document.getElementById("raw");

    function askToken() {
      const value = window.prompt("Enter Bearer token", token);
      if (value !== null) {
        token = value.trim();
      }
      return token;
    }

    function setStatus(message, error) {
      statusEl.textContent = message;
      statusEl.classList.toggle("error", Boolean(error));
    }

    function formatBytes(value) {
      if (!Number.isFinite(value)) return "-";
      const units = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"];
      let size = value;
      let unit = 0;
      while (size >= 1024 && unit < units.length - 1) {
        size /= 1024;
        unit += 1;
      }
      return `${size.toFixed(unit === 0 ? 0 : 2)} ${units[unit]}`;
    }

    function metric(label, value) {
      const item = document.createElement("div");
      const name = document.createElement("dt");
      const detail = document.createElement("dd");
      item.className = "metric";
      name.textContent = label;
      detail.textContent = value;
      item.append(name, detail);
      return item;
    }

    function render(data) {
      const ratio = Number(data.usage_ratio || 0) * 100;
      const items = [
        ["Node", data.node_id || "-"],
        ["Billing", `${data.cycle_start || "-"} to ${data.cycle_end || "-"}`],
        ["Used", formatBytes(data.used_bytes)],
        ["Remaining", formatBytes(data.remaining_bytes)],
        ["RX", formatBytes(data.rx_bytes)],
        ["TX", formatBytes(data.tx_bytes)],
        ["Quota", formatBytes(data.quota_bytes)],
        ["Usage", `${ratio.toFixed(4)}%`]
      ];
      metricsEl.replaceChildren(...items.map(([label, value]) => metric(label, value)));
      rawEl.textContent = JSON.stringify(data, null, 2);
    }

    async function loadTraffic() {
      if (!token && !askToken()) {
        setStatus("Token is required.", true);
        return;
      }

      setStatus("Loading...");
      try {
        const response = await fetch("/api/v1/traffic", {
          headers: { Authorization: `Bearer ${token}` },
          cache: "no-store"
        });
        if (response.status === 401) {
          token = "";
          setStatus("Unauthorized. Enter the token again.", true);
          askToken();
          return loadTraffic();
        }
        if (!response.ok) {
          throw new Error(`HTTP ${response.status}`);
        }
        const data = await response.json();
        render(data);
        setStatus(`Updated at ${data.updated_at || new Date().toISOString()}`);
      } catch (error) {
        setStatus(error.message || "Request failed.", true);
      }
    }

    document.getElementById("change-token").addEventListener("click", () => {
      token = "";
      askToken();
      loadTraffic();
    });
    document.getElementById("refresh").addEventListener("click", loadTraffic);
    loadTraffic();
  </script>
</body>
</html>
"#;
