// SPDX-License-Identifier: Apache-2.0
// 隧道页（公开只读）：全量隧道表。首屏与 10 秒兜底轮询走
// GET /api/public/v1/tunnels + /nodes；SSE /events/public 的 snapshot
// 维护已知节点集合，tunnels 事件携带全量列表就地更新（按行键复用 <tr>，
// 保留筛选、排序、页码与焦点）。node_id 为 null 或不在已知节点中时，
// 节点列显示「未关联节点」。

import { publicApi } from "../api.js";
import { fmtBytes, fmtClock, fmtCount, fmtText, isNil } from "../format.js";
import { createStream, STREAM_STATE } from "../stream.js";
import { tunnelBadge } from "./detail.js";
import { applyStoredSort } from "./nodes.js";
import { createServerClock, setBadge, setConnPill } from "./status.js";

// 行键：node_id + 名称。同名隧道可能属于不同节点；frps 侧有效 Proxy 名
// 全局唯一（按 user 前缀），未关联节点（node_id 为 null）的极小概率同名
// 冲突会合并为一行，属可接受取舍。
function tunnelKey(tunnel) {
  return `${tunnel?.node_id ?? ""}\n${tunnel?.name ?? ""}`;
}

function setText(row, field, text) {
  const el = row.querySelector(`[data-f="${field}"]`);
  if (el) el.textContent = text;
}

function fillNodeCell(row, tunnel, knownNodeIds) {
  const cell = row.querySelector('[data-f="node"]');
  if (!cell) return;
  const id = tunnel.node_id;
  if (!isNil(id) && id !== "" && knownNodeIds.has(id)) {
    const link = document.createElement("a");
    link.className = "fm-node-link";
    link.href = `/node.html?id=${encodeURIComponent(id)}`;
    link.textContent = id;
    cell.replaceChildren(link);
    cell.removeAttribute("title");
    return;
  }
  cell.textContent = "未关联节点";
  cell.title =
    isNil(id) || id === "" ? "服务端未关联到监控节点" : `原始节点 ID：${id}`;
}

function fillRow(row, tunnel, knownNodeIds) {
  row.dataset.tunnelKey = tunnelKey(tunnel);
  row.dataset.node = tunnel?.node_id ?? "";
  row.dataset.name = tunnel?.name ?? "";

  fillNodeCell(row, tunnel, knownNodeIds);
  setText(row, "name", fmtText(tunnel.name));
  setText(row, "type", fmtText(tunnel.type));
  const statusCell = row.querySelector('[data-f="status"]');
  if (statusCell) {
    const badge = document.createElement("span");
    badge.className = "wsk-badge";
    statusCell.replaceChildren(badge);
    setBadge(badge, tunnelBadge(tunnel));
  }
  setText(row, "cur_conns", fmtCount(tunnel.cur_conns));
  setText(row, "today_rx", fmtBytes(tunnel.today_rx_bytes));
  setText(row, "today_tx", fmtBytes(tunnel.today_tx_bytes));
}

function createTunnelTable({ table, template }) {
  const body = table.tBodies[0];
  const emptyRow = table.querySelector("[data-table-empty]");
  const knownNodeIds = new Set();
  let current = [];

  function setEmptyText(text) {
    const cell = emptyRow.querySelector("td") ?? emptyRow;
    cell.textContent = text;
  }

  function buildRow(tunnel) {
    const fragment = template.content.cloneNode(true);
    const row = fragment.querySelector("[data-row]");
    fillRow(row, tunnel, knownNodeIds);
    return row;
  }

  /** 全量调和：按行键复用已有行就地更新，增删行，保留集合控件状态。 */
  function render() {
    const existing = new Map();
    for (const row of body.querySelectorAll("[data-row]")) {
      existing.set(row.dataset.tunnelKey, row);
    }
    const rows = current.map((tunnel) => {
      const key = tunnelKey(tunnel);
      const row = existing.get(key);
      if (row) {
        existing.delete(key);
        fillRow(row, tunnel, knownNodeIds);
        return row;
      }
      return buildRow(tunnel);
    });
    body.replaceChildren(...rows, emptyRow);
    setEmptyText(current.length === 0 ? "暂无隧道。" : "没有符合条件的隧道。");
    applyStoredSort(table);
    globalThis.wsk?.mount(table);
  }

  return {
    setAll(tunnels) {
      current = Array.isArray(tunnels) ? tunnels : [];
      render();
    },
    setKnownNodes(ids) {
      knownNodeIds.clear();
      for (const id of ids) knownNodeIds.add(id);
      render();
    },
  };
}

const clock = createServerClock();
const pill = document.getElementById("conn-status");
const updatedBadge = document.getElementById("tunnels-updated");
const tunnelTable = createTunnelTable({
  table: document.getElementById("tunnel-table"),
  template: document.getElementById("tunnel-row-template"),
});

let pollFailed = false;

function stampUpdated(nowSec) {
  if (updatedBadge) updatedBadge.textContent = `数据时间 ${fmtClock(nowSec)}`;
}

function applyNodes(nodes, nowSec) {
  clock.update(nowSec);
  const ids = (Array.isArray(nodes) ? nodes : [])
    .map((node) => node?.id)
    .filter((id) => !isNil(id) && id !== "");
  tunnelTable.setKnownNodes(ids);
}

function applyTunnels(tunnels, nowSec) {
  clock.update(nowSec);
  tunnelTable.setAll(tunnels);
  stampUpdated(clock.now());
}

/** 仅拉隧道列表：SSE 重连补偿（tunnels 仅在变化时推送）。 */
async function pollTunnels() {
  try {
    const data = await publicApi.tunnels();
    applyTunnels(data?.tunnels, data?.now);
    pollFailed = false;
  } catch {
    notifyPollFailure();
  }
}

/** 兜底轮询：隧道 + 节点（SSE 不可用时节点关联也要保持新鲜）。 */
async function pollAll() {
  try {
    const [tunnelsData, nodesData] = await Promise.all([
      publicApi.tunnels(),
      publicApi.nodes(),
    ]);
    applyNodes(nodesData?.nodes, nodesData?.now);
    applyTunnels(tunnelsData?.tunnels, tunnelsData?.now);
    pollFailed = false;
  } catch {
    notifyPollFailure();
  }
}

function notifyPollFailure() {
  if (pollFailed) return;
  pollFailed = true;
  globalThis.wsk?.showToast("隧道数据加载失败，稍候自动重试。", "warning");
}

if ("EventSource" in window) {
  createStream("/events/public", {
    onSnapshot(data) {
      applyNodes(data?.nodes, data?.now);
      void pollTunnels();
    },
    onTunnels(tunnels) {
      tunnelTable.setAll(tunnels);
      stampUpdated(clock.now());
    },
    onStatus(status) {
      setConnPill(pill, status);
      if (status.state === STREAM_STATE.FAILED) {
        globalThis.wsk?.showToast(
          "实时通道不可用，已回退到 10 秒轮询。",
          "warning",
        );
      }
    },
  });
} else {
  setConnPill(pill, { state: STREAM_STATE.FAILED });
}

// 首屏 REST + 10 秒轮询兜底：SSE 正常时它与 tunnels 事件同为全量渲染，
// 幂等无冲突；SSE 断开时保证页面仍然更新。
void pollAll();
setInterval(() => void pollAll(), 10000);
