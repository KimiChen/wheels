// SPDX-License-Identifier: Apache-2.0
// 总览卡片：节点总数 / 在线 / 离线、FRP 在线客户端数、聚合收 / 发速率。
// snapshot 全量渲染；node 增量事件到来时按本地节点表重算可推导的计数与速率，
// frp_clients_online 只能来自 overview，保留最近一次 snapshot 的值。

import { fmtCount, fmtRate, isNil } from "../format.js";

const FIELDS = [
  "nodes_total",
  "nodes_online",
  "nodes_offline",
  "frp_clients_online",
  "net_rx_bps",
  "net_tx_bps",
];

function setField(root, name, text) {
  const el = root.querySelector(`[data-ov="${name}"]`);
  if (el) el.textContent = text;
}

export function renderOverview(root, overview) {
  if (!root || !overview) return;
  for (const name of FIELDS) {
    const value = overview[name];
    const text =
      name === "net_rx_bps" || name === "net_tx_bps"
        ? fmtRate(value)
        : fmtCount(value);
    setField(root, name, text);
  }
  const unknown = root.querySelector("[data-ov-note]");
  if (unknown) unknown.textContent = "";
}

/** 依据本地节点表重算计数与聚合速率（node 增量事件路径）。 */
export function updateOverviewFromNodes(root, nodes, lastOverview) {
  if (!root) return;
  const list = [...nodes.values()];
  const online = list.filter((node) => node.online === true);
  setField(root, "nodes_total", fmtCount(list.length));
  setField(root, "nodes_online", fmtCount(online.length));
  setField(root, "nodes_offline", fmtCount(list.length - online.length));
  if (lastOverview && !isNil(lastOverview.frp_clients_online)) {
    setField(root, "frp_clients_online", fmtCount(lastOverview.frp_clients_online));
  }

  let rx = 0;
  let tx = 0;
  let unknown = 0;
  for (const node of list) {
    if (Number.isFinite(node?.net_rx)) rx += node.net_rx;
    else unknown += 1;
    if (Number.isFinite(node?.net_tx)) tx += node.net_tx;
  }
  setField(root, "net_rx_bps", fmtRate(rx));
  setField(root, "net_tx_bps", fmtRate(tx));
  const note = root.querySelector("[data-ov-note]");
  if (note) {
    note.textContent =
      unknown > 0 ? `${unknown} 个节点速率未知，合计仅计已知项` : "";
  }
}
