// SPDX-License-Identifier: Apache-2.0
// 总览卡片：节点总数 / 在线 / 离线、FRP 在线客户端数、隧道在线 / 总数、
// 聚合收 / 发速率。snapshot 全量渲染；node 增量事件到来时按本地节点表重算
// 可推导的计数与速率，frp_clients_online 与隧道计数只能来自 overview /
// tunnels 事件，node 增量路径保留最近一次值不覆盖。

import { fmtCount, fmtRate, isNil, UNKNOWN } from "../format.js";

const FIELDS = [
  "nodes_total",
  "nodes_online",
  "nodes_offline",
  "frp_clients_online",
  "net_rx_bps",
  "net_tx_bps",
];

/** 「在线 / 总数」对；任一缺失显示「未知」，不伪造为 0。 */
function fmtOnlineTotal(online, total) {
  if (isNil(online) || isNil(total)) return UNKNOWN;
  return `${fmtCount(online)} / ${fmtCount(total)}`;
}

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
  setField(
    root,
    "tunnels",
    fmtOnlineTotal(overview.tunnels_online, overview.tunnels_total),
  );
  const unknown = root.querySelector("[data-ov-note]");
  if (unknown) unknown.textContent = "";
}

/** tunnels 事件携带全量隧道列表（含未关联节点的），据此重算隧道卡片。 */
export function renderTunnelsCount(root, tunnels) {
  if (!root || !Array.isArray(tunnels)) return;
  const online = tunnels.filter((tunnel) => tunnel?.online === true).length;
  setField(root, "tunnels", fmtOnlineTotal(online, tunnels.length));
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
