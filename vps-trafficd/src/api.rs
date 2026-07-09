use crate::service::{ConfigUpdate, TrafficService};
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
        .route("/api/v1/config", get(config_get).put(config_update))
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
    let token = service.auth_token().map_err(ApiError::internal)?;
    authorize(&headers, &token)?;
    let snapshot = service.snapshot().map_err(ApiError::internal)?;
    Ok(Json(snapshot))
}

async fn config_get(
    State(service): State<Arc<TrafficService>>,
    headers: HeaderMap,
) -> Result<Json<impl Serialize>, ApiError> {
    let token = service.auth_token().map_err(ApiError::internal)?;
    authorize(&headers, &token)?;
    let config = service.config_snapshot().map_err(ApiError::internal)?;
    Ok(Json(config))
}

async fn config_update(
    State(service): State<Arc<TrafficService>>,
    headers: HeaderMap,
    Json(update): Json<ConfigUpdate>,
) -> Result<Json<impl Serialize>, ApiError> {
    let token = service.auth_token().map_err(ApiError::internal)?;
    authorize(&headers, &token)?;
    let config = service
        .update_config(update)
        .map_err(ApiError::bad_request)?;
    Ok(Json(config))
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

    fn bad_request(error: anyhow::Error) -> Self {
        tracing::warn!(%error, "invalid request");
        Self {
            status: StatusCode::BAD_REQUEST,
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
      margin-bottom: 16px;
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
    form {
      display: grid;
      grid-template-columns: repeat(2, minmax(0, 1fr));
      gap: 12px;
      padding: 14px;
    }
    label {
      display: grid;
      gap: 5px;
      color: var(--muted);
      font-size: 12px;
      min-width: 0;
      text-transform: uppercase;
    }
    input,
    select {
      width: 100%;
      min-height: 38px;
      border: 1px solid var(--line);
      border-radius: 6px;
      background: var(--bg);
      color: var(--text);
      font: inherit;
      padding: 7px 9px;
    }
    .quota-row {
      display: grid;
      grid-template-columns: minmax(0, 1fr) 82px;
      gap: 8px;
    }
    .form-actions {
      display: flex;
      justify-content: flex-end;
      gap: 8px;
      grid-column: 1 / -1;
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
      form {
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
    <section>
      <div id="config-status" class="status">Config is locked.</div>
      <form id="config-form">
        <label>
          Traffic refill start
          <input id="traffic-anchor" type="datetime-local" step="1" required>
        </label>
        <label>
          Traffic refill months
          <input id="traffic-months" type="number" min="1" step="1" required>
        </label>
        <label>
          Billing mode
          <select id="billing-mode" required>
            <option value="total">Total (RX + TX)</option>
            <option value="rx">RX only</option>
            <option value="tx">TX only</option>
            <option value="max">Max of RX/TX</option>
          </select>
        </label>
        <label>
          Traffic quota
          <span class="quota-row">
            <input id="quota-value" type="number" min="0.01" step="0.01" required>
            <select id="quota-unit">
              <option value="K">K</option>
              <option value="M">M</option>
              <option value="G" selected>G</option>
              <option value="T">T</option>
            </select>
          </span>
        </label>
        <label>
          Current cycle used
          <span class="quota-row">
            <input id="current-used-value" type="number" min="0" step="0.01" required>
            <select id="current-used-unit">
              <option value="K">K</option>
              <option value="M">M</option>
              <option value="G" selected>G</option>
              <option value="T">T</option>
            </select>
          </span>
        </label>
        <div class="form-actions">
          <button id="load-config" type="button">Load</button>
          <button class="primary" type="submit">Save</button>
        </div>
      </form>
    </section>
  </main>
  <script>
    let token = "";
    const statusEl = document.getElementById("status");
    const configStatusEl = document.getElementById("config-status");
    const metricsEl = document.getElementById("metrics");
    const rawEl = document.getElementById("raw");
    const configForm = document.getElementById("config-form");
    const trafficAnchorEl = document.getElementById("traffic-anchor");
    const trafficMonthsEl = document.getElementById("traffic-months");
    const billingModeEl = document.getElementById("billing-mode");
    const quotaValueEl = document.getElementById("quota-value");
    const quotaUnitEl = document.getElementById("quota-unit");
    const currentUsedValueEl = document.getElementById("current-used-value");
    const currentUsedUnitEl = document.getElementById("current-used-unit");
    const unitBytes = { B: 1, K: 1024, M: 1048576, G: 1073741824, T: 1099511627776, P: 1125899906842624 };

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

    function setConfigStatus(message, error) {
      configStatusEl.textContent = message;
      configStatusEl.classList.toggle("error", Boolean(error));
    }

    function formatBytes(value) {
      if (!Number.isFinite(value)) return "-";
      const units = ["B", "K", "M", "G", "T", "P"];
      let size = value;
      let unit = 0;
      while (size >= 1024 && unit < units.length - 1) {
        size /= 1024;
        unit += 1;
      }
      return `${size.toFixed(2)} ${units[unit]}`;
    }

    function splitBytes(bytes, preferredUnit) {
      const numericBytes = Number(bytes || 0);
      if (preferredUnit && unitBytes[preferredUnit]) {
        return { value: numericBytes / unitBytes[preferredUnit], unit: preferredUnit };
      }
      const units = ["T", "G", "M", "K"];
      for (const unit of units) {
        if (numericBytes >= unitBytes[unit] && numericBytes % unitBytes[unit] === 0) {
          return { value: numericBytes / unitBytes[unit], unit };
        }
      }
      for (const unit of units) {
        if (numericBytes >= unitBytes[unit]) {
          return { value: numericBytes / unitBytes[unit], unit };
        }
      }
      return { value: numericBytes, unit: "K" };
    }

    function setByteInput(bytes, valueEl, unitEl, preferredUnit) {
      const split = splitBytes(Number(bytes || 0), preferredUnit);
      valueEl.value = split.value.toFixed(2);
      unitEl.value = split.unit;
    }

    function toLocalInputValue(iso) {
      const date = new Date(iso);
      const local = new Date(date.getTime() - date.getTimezoneOffset() * 60000);
      return local.toISOString().slice(0, 19);
    }

    function localInputToIso(value) {
      const localValue = normalizeLocalDateTimeInput(value);
      const date = new Date(localValue);
      if (!Number.isFinite(date.getTime())) {
        throw new Error("Invalid traffic refill start.");
      }
      const offsetMinutes = -date.getTimezoneOffset();
      const sign = offsetMinutes >= 0 ? "+" : "-";
      const abs = Math.abs(offsetMinutes);
      const offset = `${sign}${String(Math.floor(abs / 60)).padStart(2, "0")}:${String(abs % 60).padStart(2, "0")}`;
      return `${localValue}${offset}`;
    }

    function normalizeLocalDateTimeInput(value) {
      const trimmed = value.trim();
      if (/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}$/.test(trimmed)) {
        return `${trimmed}:00`;
      }
      if (/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}$/.test(trimmed)) {
        return trimmed;
      }
      throw new Error("Invalid traffic refill start.");
    }

    function bytesFromForm(valueEl, unitEl, message) {
      const value = Number(valueEl.value);
      const unit = unitEl.value;
      if (!Number.isFinite(value) || value <= 0 || !unitBytes[unit]) {
        throw new Error(message);
      }
      return Math.round(value * unitBytes[unit]);
    }

    function quotaBytesFromForm() {
      return bytesFromForm(quotaValueEl, quotaUnitEl, "Invalid quota.");
    }

    function usedBytesFromForm() {
      const value = Number(currentUsedValueEl.value);
      const unit = currentUsedUnitEl.value;
      if (!Number.isFinite(value) || value < 0 || !unitBytes[unit]) {
        throw new Error("Invalid current cycle used traffic.");
      }
      return Math.round(value * unitBytes[unit]);
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
      const items = [
        ["Node", data.node_id || "-"],
        ["Cycle", `${data.cycle_start || "-"} to ${data.cycle_end || "-"}`],
        ["Used", formatBytes(data.used_bytes)],
        ["Remaining", formatBytes(data.remaining_bytes)],
        ["RX", formatBytes(data.rx_bytes)],
        ["TX", formatBytes(data.tx_bytes)],
        ["Billing", data.billing_mode || "-"],
        ["Quota", formatBytes(data.quota_bytes)]
      ];
      metricsEl.replaceChildren(...items.map(([label, value]) => metric(label, value)));
      rawEl.textContent = JSON.stringify(data, null, 2);
      if (
        document.activeElement !== currentUsedValueEl &&
        document.activeElement !== currentUsedUnitEl
      ) {
        setByteInput(data.used_bytes, currentUsedValueEl, currentUsedUnitEl, "G");
      }
    }

    function renderConfig(config) {
      trafficAnchorEl.value = toLocalInputValue(config.traffic_cycle_anchor);
      trafficMonthsEl.value = config.traffic_cycle_months || 1;
      billingModeEl.value = config.billing_mode || "total";
      setByteInput(config.quota_bytes, quotaValueEl, quotaUnitEl, "G");
      setConfigStatus(`Config loaded for ${config.node_id || "-"}.`);
    }

    async function fetchAuthed(path, options) {
      if (!token && !askToken()) {
        throw new Error("Token is required.");
      }
      const response = await fetch(path, {
        ...options,
        headers: {
          ...(options && options.headers ? options.headers : {}),
          Authorization: `Bearer ${token}`
        },
        cache: "no-store"
      });
      if (response.status === 401) {
        token = "";
        throw new Error("Unauthorized.");
      }
      return response;
    }

    async function loadTraffic() {
      if (!token && !askToken()) {
        setStatus("Token is required.", true);
        return;
      }

      setStatus("Loading...");
      try {
        const response = await fetchAuthed("/api/v1/traffic");
        if (!response.ok) {
          throw new Error(`HTTP ${response.status}`);
        }
        const data = await response.json();
        render(data);
        setStatus(`Updated at ${data.updated_at || new Date().toISOString()}`);
      } catch (error) {
        setStatus(error.message || "Request failed.", true);
        if ((error.message || "").includes("Unauthorized")) {
          askToken();
        }
      }
    }

    async function loadConfig() {
      setConfigStatus("Loading config...");
      try {
        const response = await fetchAuthed("/api/v1/config");
        if (!response.ok) {
          throw new Error(`HTTP ${response.status}`);
        }
        renderConfig(await response.json());
      } catch (error) {
        setConfigStatus(error.message || "Config request failed.", true);
        if ((error.message || "").includes("Unauthorized")) {
          askToken();
        }
      }
    }

    async function saveConfig(event) {
      event.preventDefault();
      setConfigStatus("Saving config...");
      try {
        const payload = {
          traffic_cycle_anchor: localInputToIso(trafficAnchorEl.value),
          traffic_cycle_months: Number(trafficMonthsEl.value),
          quota_bytes: quotaBytesFromForm(),
          billing_mode: billingModeEl.value,
          current_cycle_used_bytes: usedBytesFromForm()
        };
        const response = await fetchAuthed("/api/v1/config", {
          method: "PUT",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(payload)
        });
        if (!response.ok) {
          throw new Error(`HTTP ${response.status}`);
        }
        renderConfig(await response.json());
        setConfigStatus("Config saved.");
        loadTraffic();
      } catch (error) {
        setConfigStatus(error.message || "Config save failed.", true);
        if ((error.message || "").includes("Unauthorized")) {
          askToken();
        }
      }
    }

    document.getElementById("change-token").addEventListener("click", () => {
      token = "";
      askToken();
      loadTraffic();
      loadConfig();
    });
    document.getElementById("refresh").addEventListener("click", loadTraffic);
    document.getElementById("load-config").addEventListener("click", loadConfig);
    configForm.addEventListener("submit", saveConfig);
    loadTraffic();
    loadConfig();
  </script>
</body>
</html>
"#;
