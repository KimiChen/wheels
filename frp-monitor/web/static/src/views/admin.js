// SPDX-License-Identifier: Apache-2.0
// 管理端：Cookie 会话（login / logout / session），节点列表 + 完整详情
// （Facts、本地目标、计数器范围、隧道、frp_clients 对账、最近事件）。
// 实时走 /events/admin；401 时回到登录表单。登录与凭据表单走真实 POST，
// 不使用 data-demo-submit 伪保存。新凭据 Token 仅创建时展示一次。

import { ApiError, adminApi, probeBackup, publicApi } from "../api.js";
import { fmtClock, fmtText, UNKNOWN } from "../format.js";
import { createStream, STREAM_STATE } from "../stream.js";
import {
  fillCounters,
  fillEvents,
  fillFacts,
  fillFrpClients,
  fillProbes,
  fillProxies,
  fillResources,
  fillStatus,
  fillTraffic,
  fillTrafficDaily,
  fillTunnels,
} from "./detail.js";
import { createNodeTable } from "./nodes.js";
import {
  renderOverview,
  renderTunnelsCount,
  updateOverviewFromNodes,
} from "./overview.js";
import { createServerClock, sessionBadge, setBadge, setConnPill } from "./status.js";
import { createTrends } from "./trends.js";

const clock = createServerClock();
const loginView = document.getElementById("login-view");
const dashView = document.getElementById("dash-view");
const loginForm = document.getElementById("login-form");
const loginError = document.getElementById("login-error");
const passwordInput = document.getElementById("admin-password");
const logoutButton = document.getElementById("logout-button");
const pill = document.getElementById("conn-status");
const overviewGrid = document.getElementById("overview-grid");
const updatedBadge = document.getElementById("ov-updated");
const detail = document.getElementById("admin-detail");
const detailTitle = document.getElementById("admin-detail-title");
const detailState = document.getElementById("admin-detail-state");
const proxyBody = document.getElementById("proxy-tbody");
const proxyTemplate = document.getElementById("proxy-row-template");
const frpBody = document.getElementById("frp-tbody");
const frpTemplate = document.getElementById("frp-row-template");
const probeBody = document.getElementById("probe-tbody");
const probeTemplate = document.getElementById("probe-row-template");
const trafficBody = document.getElementById("traffic-tbody");
const trafficTemplate = document.getElementById("traffic-row-template");
const tunnelBody = document.getElementById("tunnel-tbody");
const tunnelTemplate = document.getElementById("tunnel-row-template");
const eventBody = document.getElementById("event-tbody");
const eventTemplate = document.getElementById("event-row-template");
const credForm = document.getElementById("cred-form");
const credError = document.getElementById("cred-error");
const credIdInput = document.getElementById("cred-id");
const credCommentInput = document.getElementById("cred-comment");
const credBody = document.getElementById("cred-tbody");
const credTemplate = document.getElementById("cred-row-template");
const tokenBox = document.getElementById("cred-token-box");
const tokenTitle = document.getElementById("cred-token-title");
const tokenInput = document.getElementById("cred-token");
const tokenCopy = document.getElementById("cred-copy");
const tokenDone = document.getElementById("cred-token-done");
const revokeDialog = document.getElementById("cred-revoke-dialog");
const revokeText = document.getElementById("cred-revoke-text");
const revokeConfirm = document.getElementById("cred-revoke-confirm");
const backupLink = document.getElementById("backup-link");
const backupNote = document.getElementById("backup-note");

// 趋势历史走公开 metrics 路由（契约仅此一个）；日流量表为管理端专属接口。
const trends = createTrends({
  root: document.getElementById("trend-section"),
  fetchMetrics: (id, range) => publicApi.nodeMetrics(id, range),
});

const nodeTable = createNodeTable({
  table: document.getElementById("node-table"),
  template: document.getElementById("node-row-template"),
});

const state = { overview: null, selectedId: null };
let stream = null;

function showLogin() {
  dashView.hidden = true;
  detail.hidden = true;
  loginView.hidden = false;
  state.selectedId = null;
  trends.hide();
  clearToken();
  if (revokeDialog.open) revokeDialog.close();
  if (stream) {
    stream.close();
    stream = null;
  }
  pill.hidden = true;
}

function enterDashboard() {
  loginView.hidden = true;
  dashView.hidden = false;
  pill.hidden = false;
  void loadCredentials();
  void probeBackupState();
  if (stream) stream.close();
  if (!("EventSource" in window)) {
    setConnPill(pill, { state: STREAM_STATE.FAILED });
    void loadOnce();
    return;
  }
  stream = createStream("/events/admin", { onSnapshot, onNode, onTunnels, onStatus });
}

function onSnapshot(data) {
  clock.update(data?.now);
  state.overview = data?.overview ?? null;
  renderOverview(overviewGrid, state.overview);
  const nodes = Array.isArray(data?.nodes) ? data.nodes : [];
  nodeTable.setAll(nodes, clock.now());
  if (updatedBadge) updatedBadge.textContent = `数据时间 ${fmtClock(data?.now)}`;
  if (state.selectedId) {
    const current = nodes.find((item) => item.id === state.selectedId);
    if (current) fillDetail(current);
  }
}

function onNode(node) {
  if (!node || node.id === undefined) return;
  nodeTable.upsert(node, clock.now());
  updateOverviewFromNodes(overviewGrid, nodeTable.nodes, state.overview);
  if (node.id === state.selectedId) fillDetail(node);
}

// tunnels 事件（管理端裁剪，含 user / client_id / local_addr）：刷新总览
// 隧道卡片，并就地更新选中节点的隧道表（隧道变化不一定伴随 node 事件）。
function onTunnels(tunnels) {
  renderTunnelsCount(overviewGrid, tunnels);
  if (!state.selectedId) return;
  fillTunnels(
    tunnelBody,
    tunnelTemplate,
    tunnels.filter((tunnel) => tunnel?.node_id === state.selectedId),
  );
}

function onStatus(status) {
  setConnPill(pill, status);
  if (status.state === STREAM_STATE.FAILED) {
    globalThis.wsk?.showToast("会话已失效，请重新登录。", "warning");
    showLogin();
  }
}

async function loadOnce() {
  try {
    const nodes = await adminApi.nodes();
    onSnapshot({ now: nodes?.now, nodes: nodes?.nodes });
  } catch (error) {
    if (error instanceof ApiError && error.status === 401) showLogin();
  }
}

function fillDetail(node) {
  detail.hidden = false;
  detailTitle.textContent = node.id;
  setBadge(detailState, sessionBadge(node));
  fillStatus(detail, node, clock.now());
  fillResources(detail, node);
  fillFacts(detail, node.facts);
  fillCounters(detail, node, clock.now());
  fillProxies(proxyBody, proxyTemplate, node.proxies);
  fillFrpClients(frpBody, frpTemplate, node.frp_clients, clock.now());
  fillProbes(probeBody, probeTemplate, node.probes);
  fillTraffic(detail, node.traffic);
  fillTunnels(tunnelBody, tunnelTemplate, node.tunnels);
  trends.show(node.id);
}

/** 近 7 日流量：仅在选择节点时拉取一次，SSE 增量不重复请求。 */
async function loadTrafficDaily(id) {
  try {
    const data = await adminApi.trafficDaily(id, 7);
    if (state.selectedId !== id) return;
    fillTrafficDaily(trafficBody, trafficTemplate, data?.days);
  } catch (error) {
    if (error instanceof ApiError && error.status === 401) {
      showLogin();
      return;
    }
    if (state.selectedId !== id) return;
    fillTableError(trafficBody, 3, "日流量加载失败，请重新选择节点。");
  }
}

/** 最近事件：仅在选择节点时拉取一次（倒序，最新在前）。 */
async function loadEvents(id) {
  try {
    const data = await adminApi.nodeEvents(id, 50);
    if (state.selectedId !== id) return;
    fillEvents(eventBody, eventTemplate, data?.events, clock.now());
  } catch (error) {
    if (error instanceof ApiError && error.status === 401) {
      showLogin();
      return;
    }
    if (state.selectedId !== id) return;
    fillTableError(eventBody, 4, "事件加载失败，请重新选择节点。");
  }
}

function fillTableError(tbody, colSpan, text) {
  const row = document.createElement("tr");
  const cell = document.createElement("td");
  cell.className = "wsk-table-empty";
  cell.colSpan = colSpan;
  cell.textContent = text;
  row.append(cell);
  tbody.replaceChildren(row);
}

async function selectNode(id, { moveFocus = false } = {}) {
  state.selectedId = id;
  nodeTable.table.querySelectorAll("[data-row]").forEach((row) => {
    const current = row.dataset.nodeId === id;
    row.classList.toggle("fm-row-active", current);
    row.querySelector("[data-detail]")?.setAttribute("aria-pressed", String(current));
  });
  const cached = nodeTable.nodes.get(id);
  if (cached) fillDetail(cached);
  void loadTrafficDaily(id);
  void loadEvents(id);
  try {
    const data = await adminApi.node(id);
    clock.update(data?.now);
    if (state.selectedId !== id) return;
    if (data?.node) fillDetail(data.node);
  } catch (error) {
    if (error instanceof ApiError && error.status === 401) {
      showLogin();
      return;
    }
    if (!cached) {
      globalThis.wsk?.showToast("节点详情加载失败，请重试。", "danger");
      return;
    }
  }
  if (moveFocus) detailTitle.focus();
}

nodeTable.table.addEventListener("click", (event) => {
  const button =
    event.target instanceof Element ? event.target.closest("[data-detail]") : null;
  if (!button) return;
  const row = button.closest("[data-row]");
  if (row?.dataset.nodeId) void selectNode(row.dataset.nodeId, { moveFocus: true });
});

loginForm.addEventListener("submit", async (event) => {
  event.preventDefault();
  loginError.hidden = true;
  if (!loginForm.reportValidity()) return;
  const submit = loginForm.querySelector('[type="submit"]');
  submit.disabled = true;
  submit.classList.add("wsk-loading");
  submit.setAttribute("aria-busy", "true");
  try {
    await adminApi.login(passwordInput.value);
    passwordInput.value = "";
    globalThis.wsk?.showToast("登录成功。", "success");
    enterDashboard();
  } catch (error) {
    if (error instanceof ApiError && error.status === 401) {
      loginError.textContent = "密码错误，请重试。";
      loginError.hidden = false;
      passwordInput.focus();
    } else {
      globalThis.wsk?.showToast("登录请求失败，请稍后重试。", "danger");
    }
  } finally {
    submit.disabled = false;
    submit.classList.remove("wsk-loading");
    submit.removeAttribute("aria-busy");
  }
});

logoutButton.addEventListener("click", async () => {
  try {
    await adminApi.logout();
  } catch {
    // 会话可能已过期，本地同样回到登录态。
  }
  globalThis.wsk?.showToast("已退出登录。", "info");
  showLogin();
});

/* ------------------------------------------------------ 接入与设置：凭据 */

/** created_at 兼容 Unix 秒（数字）与 RFC3339 等可解析字符串。 */
function fmtCreatedAt(value) {
  if (typeof value === "number" && Number.isFinite(value)) {
    return fmtClock(value);
  }
  if (typeof value === "string" && value !== "") {
    const ms = Date.parse(value);
    return Number.isFinite(ms) ? fmtClock(ms / 1000) : value;
  }
  return UNKNOWN;
}

function fillCredentials(credentials) {
  const list = Array.isArray(credentials) ? credentials : [];
  const rows = list.map((cred) => {
    const fragment = credTemplate.content.cloneNode(true);
    const row = fragment.querySelector("tr");
    row.querySelector('[data-f="id"]').textContent = fmtText(cred.id);
    row.querySelector('[data-f="comment"]').textContent =
      cred.comment ? String(cred.comment) : "—";
    row.querySelector('[data-f="created_at"]').textContent = fmtCreatedAt(
      cred.created_at,
    );
    const revoke = row.querySelector("[data-revoke]");
    revoke.dataset.revoke = cred.id ?? "";
    revoke.setAttribute("aria-label", `吊销凭据 ${cred.id ?? ""}`);
    return row;
  });
  if (rows.length === 0) {
    const row = document.createElement("tr");
    const cell = document.createElement("td");
    cell.className = "wsk-table-empty";
    cell.colSpan = 4;
    cell.textContent = "暂无凭据，请在上方创建。";
    row.append(cell);
    rows.push(row);
  }
  credBody.replaceChildren(...rows);
}

async function loadCredentials() {
  try {
    const data = await adminApi.credentials();
    fillCredentials(data?.credentials);
  } catch (error) {
    if (error instanceof ApiError && error.status === 401) {
      showLogin();
      return;
    }
    fillTableError(credBody, 4, "凭据列表加载失败。");
  }
}

/* 新凭据 Token 仅此一次展示：可见期间刷新 / 关闭页面前给出浏览器确认提示。 */
function warnUnsavedToken(event) {
  event.preventDefault();
  event.returnValue = "";
}

function showToken(id, token) {
  tokenTitle.textContent = `凭据 ${id} 创建成功`;
  tokenInput.value = token;
  tokenBox.hidden = false;
  window.addEventListener("beforeunload", warnUnsavedToken);
  tokenBox.scrollIntoView({ block: "nearest" });
  tokenInput.focus();
  tokenInput.select();
}

function clearToken() {
  tokenInput.value = "";
  tokenBox.hidden = true;
  window.removeEventListener("beforeunload", warnUnsavedToken);
}

tokenDone.addEventListener("click", clearToken);

tokenCopy.addEventListener("click", async () => {
  const token = tokenInput.value;
  if (!token) return;
  try {
    if (!navigator.clipboard?.writeText) throw new Error("no-clipboard");
    await navigator.clipboard.writeText(token);
    globalThis.wsk?.showToast("Token 已复制。", "success");
  } catch {
    // 非安全上下文（如纯 HTTP 内网）没有 Clipboard API，退回选中复制。
    tokenInput.focus();
    tokenInput.select();
    let copied = false;
    try {
      copied = document.execCommand("copy");
    } catch {
      copied = false;
    }
    globalThis.wsk?.showToast(
      copied ? "Token 已复制。" : "复制失败，请手动选中 Token 复制。",
      copied ? "success" : "warning",
    );
  }
});

credForm.addEventListener("submit", async (event) => {
  event.preventDefault();
  credError.hidden = true;
  if (!credForm.reportValidity()) return;
  const submit = credForm.querySelector('[type="submit"]');
  submit.disabled = true;
  submit.classList.add("wsk-loading");
  submit.setAttribute("aria-busy", "true");
  const id = credIdInput.value.trim();
  const comment = credCommentInput.value.trim();
  try {
    const data = await adminApi.createCredential(id, comment);
    credForm.reset();
    showToken(data?.id ?? id, data?.token ?? "");
    globalThis.wsk?.showToast("凭据已创建，请立即保存 Token。", "success");
    void loadCredentials();
  } catch (error) {
    if (error instanceof ApiError && error.status === 401) {
      showLogin();
      return;
    }
    if (error instanceof ApiError && error.status === 409) {
      credError.textContent = "凭据 ID 已存在，请更换一个。";
      credError.hidden = false;
      credIdInput.focus();
    } else {
      globalThis.wsk?.showToast("创建凭据失败，请稍后重试。", "danger");
    }
  } finally {
    submit.disabled = false;
    submit.classList.remove("wsk-loading");
    submit.removeAttribute("aria-busy");
  }
});

/* 吊销：二次确认对话框，确认按钮触发真实 DELETE。 */
let pendingRevoke = null;

credBody.addEventListener("click", (event) => {
  const button =
    event.target instanceof Element ? event.target.closest("[data-revoke]") : null;
  if (!button) return;
  pendingRevoke = button.dataset.revoke;
  revokeText.textContent =
    `确定吊销凭据「${pendingRevoke}」？使用该凭据的节点将无法再上报监控数据，` +
    "此操作不可撤销。";
  revokeDialog.showModal();
});

revokeConfirm.addEventListener("click", async () => {
  if (!pendingRevoke) return;
  revokeConfirm.disabled = true;
  revokeConfirm.classList.add("wsk-loading");
  revokeConfirm.setAttribute("aria-busy", "true");
  try {
    await adminApi.deleteCredential(pendingRevoke);
    globalThis.wsk?.showToast(`凭据 ${pendingRevoke} 已吊销。`, "success");
    pendingRevoke = null;
    revokeDialog.close();
    void loadCredentials();
  } catch (error) {
    if (error instanceof ApiError && error.status === 401) {
      revokeDialog.close();
      showLogin();
      return;
    }
    globalThis.wsk?.showToast("吊销失败，请稍后重试。", "danger");
  } finally {
    revokeConfirm.disabled = false;
    revokeConfirm.classList.remove("wsk-loading");
    revokeConfirm.removeAttribute("aria-busy");
  }
});

/* ------------------------------------------------------ 接入与设置：备份 */

function setBackupEnabled(enabled) {
  backupNote.hidden = enabled;
  backupLink.classList.toggle("fm-disabled", !enabled);
  backupLink.setAttribute("aria-disabled", String(!enabled));
  if (enabled) backupLink.setAttribute("href", "/api/admin/v1/backup");
  else backupLink.removeAttribute("href");
}

/** 进入管理端时探测一次：409 no_data_dir → 禁用下载并说明。 */
async function probeBackupState() {
  const state = await probeBackup();
  if (state === "unauthorized") {
    showLogin();
    return;
  }
  if (state === "no_data_dir") setBackupEnabled(false);
  else if (state === "ok") setBackupEnabled(true);
  // 网络等未知结果保持当前状态，用户点击后由服务端响应兜底。
}

// 启动：先探会话，避免已登录用户看到登录表单闪烁。
try {
  await adminApi.session();
  enterDashboard();
} catch {
  showLogin();
}
