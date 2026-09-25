// SPDX-License-Identifier: Apache-2.0
// 总览页：GET /api/public/v1/overview、/api/public/v1/nodes 由 SSE
// /events/public 的 snapshot 承载；node 事件逐项更新行，不重建整表。

import { publicApi } from "../api.js";
import { fmtClock } from "../format.js";
import { createStream, STREAM_STATE } from "../stream.js";
import { createNodeTable } from "./nodes.js";
import {
  renderOverview,
  renderTunnelsCount,
  updateOverviewFromNodes,
} from "./overview.js";
import { createServerClock, setConnPill } from "./status.js";

const clock = createServerClock();
const pill = document.getElementById("conn-status");
const overviewGrid = document.getElementById("overview-grid");
const updatedBadge = document.getElementById("ov-updated");

const nodeTable = createNodeTable({
  table: document.getElementById("node-table"),
  template: document.getElementById("node-row-template"),
  hrefFor: (node) => `/node.html?id=${encodeURIComponent(node.id)}`,
});

const state = { overview: null };

function onSnapshot(data) {
  clock.update(data?.now);
  state.overview = data?.overview ?? null;
  renderOverview(overviewGrid, state.overview);
  nodeTable.setAll(Array.isArray(data?.nodes) ? data.nodes : [], clock.now());
  if (updatedBadge) updatedBadge.textContent = `数据时间 ${fmtClock(data?.now)}`;
}

function onNode(node) {
  if (!node || node.id === undefined) return;
  nodeTable.upsert(node, clock.now());
  updateOverviewFromNodes(overviewGrid, nodeTable.nodes, state.overview);
}

// tunnels 事件携带全量隧道列表，用于刷新「隧道 在线 / 总数」卡片。
function onTunnels(tunnels) {
  renderTunnelsCount(overviewGrid, tunnels);
}

function onStatus(status) {
  setConnPill(pill, status);
  if (status.state === STREAM_STATE.FAILED) {
    // SSE 不可用（如被中间件拦截）：退回一次性 REST，页面仍可看快照。
    void loadOnce();
    globalThis.wsk?.showToast("实时通道不可用，已回退到一次性加载。", "warning");
  }
}

async function loadOnce() {
  try {
    const [overview, nodes] = await Promise.all([
      publicApi.overview(),
      publicApi.nodes(),
    ]);
    onSnapshot({ now: nodes?.now ?? overview?.now, overview, nodes: nodes?.nodes });
  } catch {
    globalThis.wsk?.showToast("无法加载监控数据，请稍后刷新重试。", "danger");
  }
}

if ("EventSource" in window) {
  createStream("/events/public", { onSnapshot, onNode, onTunnels, onStatus });
} else {
  setConnPill(pill, { state: STREAM_STATE.FAILED });
  void loadOnce();
}

// 相对时间随本地时钟推进，不重排行、不动筛选与焦点。
setInterval(() => nodeTable.refreshTimes(clock.now()), 15000);
