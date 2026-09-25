// SPDX-License-Identifier: Apache-2.0
// 管理端：Cookie 会话（login / logout / session），节点列表 + 完整详情
// （Facts、本地目标、计数器范围、frp_clients 对账）。实时走 /events/admin；
// 401 时回到登录表单。登录表单走真实 POST，不使用 data-demo-submit 伪保存。

import { ApiError, adminApi } from "../api.js";
import { fmtClock } from "../format.js";
import { createStream, STREAM_STATE } from "../stream.js";
import {
  fillCounters,
  fillFacts,
  fillFrpClients,
  fillProxies,
  fillResources,
  fillStatus,
} from "./detail.js";
import { createNodeTable } from "./nodes.js";
import { renderOverview, updateOverviewFromNodes } from "./overview.js";
import { createServerClock, sessionBadge, setBadge, setConnPill } from "./status.js";

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
  if (stream) stream.close();
  if (!("EventSource" in window)) {
    setConnPill(pill, { state: STREAM_STATE.FAILED });
    void loadOnce();
    return;
  }
  stream = createStream("/events/admin", { onSnapshot, onNode, onStatus });
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

// 启动：先探会话，避免已登录用户看到登录表单闪烁。
try {
  await adminApi.session();
  enterDashboard();
} catch {
  showLogin();
}
