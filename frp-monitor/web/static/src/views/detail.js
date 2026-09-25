// SPDX-License-Identifier: Apache-2.0
// 节点详情共享渲染：状态行、资源当前值、Proxy 列表为公开内容；
// Facts、计数器范围、frp_clients 对账仅在管理端存在（公开 DTO 不含这些字段，
// 页面也无对应区块）。所有函数按 data-f 选择器就地写文本，初次渲染与
// node 增量事件走同一代码路径。

import {
  fmtAgo,
  fmtBytes,
  fmtClock,
  fmtCount,
  fmtCpu,
  fmtInterval,
  fmtLoad,
  fmtPair,
  fmtPercent,
  fmtRate,
  fmtText,
  fmtUptime,
  isNil,
  percentValue,
} from "../format.js";
import { fillStatusBadges, setBadge } from "./status.js";

function setText(scope, field, text) {
  const el = scope.querySelector(`[data-f="${field}"]`);
  if (el) el.textContent = text;
}

function setProgress(scope, field, percent) {
  const el = scope.querySelector(`[data-f-progress="${field}"]`);
  if (!el) return;
  if (percent === null) el.removeAttribute("value");
  else el.value = percent;
}

function setTime(scope, field, ts, nowSec) {
  const el = scope.querySelector(`[data-f="${field}"]`);
  if (!el) return;
  el.textContent = fmtAgo(ts, nowSec);
  el.title = fmtClock(ts);
}

/** 状态区块：三徽章 + 上报间隔 + 采集 / 更新时间。 */
export function fillStatus(scope, node, nowSec) {
  fillStatusBadges(scope, node);
  setText(scope, "report_interval", fmtInterval(node.report_interval));
  setTime(scope, "collected_at", node.collected_at, nowSec);
  setTime(scope, "last_seen", node.last_seen, nowSec);
}

/** 资源区块：CPU / 内存 / 磁盘 / 网络 / 负载 / 系统动态。 */
export function fillResources(scope, node) {
  setText(scope, "cpu", fmtCpu(node.cpu));
  setProgress(scope, "cpu", Number.isFinite(node.cpu) ? node.cpu : null);

  setText(scope, "mem_pct", fmtPercent(node.mem_used, node.mem_total));
  setProgress(scope, "mem", percentValue(node.mem_used, node.mem_total));
  setText(scope, "mem_pair", fmtPair(node.mem_used, node.mem_total));

  setText(scope, "swap_pair", fmtPair(node.swap_used, node.swap_total));

  setText(scope, "disk_pct", fmtPercent(node.disk_used, node.disk_total));
  setProgress(scope, "disk", percentValue(node.disk_used, node.disk_total));
  setText(scope, "disk_pair", fmtPair(node.disk_used, node.disk_total));

  setText(scope, "net_rx", fmtRate(node.net_rx));
  setText(scope, "net_tx", fmtRate(node.net_tx));

  setText(scope, "load", fmtLoad(node.load));
  setText(scope, "uptime", fmtUptime(node.uptime));
  setText(scope, "tcp", fmtCount(node.tcp));
  setText(scope, "udp", fmtCount(node.udp));
  setText(scope, "procs", fmtCount(node.procs));
}

const PROXY_STATUS = {
  online: { variant: "success", text: "运行中", dot: true },
  offline: { variant: "danger", text: "离线" },
  error: { variant: "danger", text: "异常" },
};

function renderSimpleRows(tbody, template, items, fillItem, emptyText) {
  const rows = items.map((item) => {
    const fragment = template.content.cloneNode(true);
    const row = fragment.querySelector("tr");
    fillItem(row, item);
    return row;
  });
  if (rows.length === 0) {
    const row = document.createElement("tr");
    const cell = document.createElement("td");
    cell.className = "wsk-table-empty";
    const columns = template.content.querySelectorAll("td, th").length || 1;
    cell.colSpan = columns;
    cell.textContent = emptyText;
    row.append(cell);
    rows.push(row);
  }
  tbody.replaceChildren(...rows);
}

/** Proxy 表格：name / type / status / enabled，管理端模板多一列 local_addr。 */
export function fillProxies(tbody, template, proxies) {
  renderSimpleRows(
    tbody,
    template,
    Array.isArray(proxies) ? proxies : [],
    (row, proxy) => {
      setText(row, "name", fmtText(proxy.name));
      setText(row, "type", fmtText(proxy.type));
      setText(row, "local_addr", fmtText(proxy.local_addr));
      const statusCell = row.querySelector('[data-f="status"]');
      if (statusCell) {
        const badge = document.createElement("span");
        badge.className = "wsk-badge";
        statusCell.replaceChildren(badge);
        setBadge(
          badge,
          PROXY_STATUS[proxy.status] ?? {
            variant: null,
            text: fmtText(proxy.status),
          },
        );
      }
      setText(
        row,
        "enabled",
        isNil(proxy.enabled) ? fmtText(null) : proxy.enabled ? "启用" : "禁用",
      );
    },
    "该节点暂无 Proxy。",
  );
}

/** Facts 区块（仅管理端）：null → 未知。 */
export function fillFacts(scope, facts) {
  const source = facts ?? {};
  const fields = [
    "hostname",
    "os",
    "kernel",
    "arch",
    "virt",
    "cpu_name",
    "cpu_cores",
    "agent_version",
    "ipv4",
    "ipv6",
  ];
  for (const field of fields) {
    const value = source[field];
    setText(
      scope,
      field,
      field === "cpu_cores" ? fmtCount(value) : fmtText(value),
    );
  }
}

/** 计数器范围与 FRP 关联（仅管理端）。 */
export function fillCounters(scope, node, nowSec) {
  setText(scope, "boot_id", fmtText(node.boot_id));
  setText(scope, "iface", fmtText(node.iface));
  setText(scope, "net_rx_total", fmtBytes(node.net_rx_total));
  setText(scope, "net_tx_total", fmtBytes(node.net_tx_total));
  setText(scope, "frp_client_id", fmtText(node.frp_client_id));
  setTime(scope, "connected_at", node.connected_at, nowSec);
}

/** FRP 客户端对账表（仅管理端）。 */
export function fillFrpClients(tbody, template, clients, nowSec) {
  renderSimpleRows(
    tbody,
    template,
    Array.isArray(clients) ? clients : [],
    (row, client) => {
      setText(row, "user", fmtText(client.user));
      setText(row, "client_id", fmtText(client.client_id));
      setText(row, "run_id", fmtText(client.run_id));
      setText(row, "version", fmtText(client.version));
      const onlineCell = row.querySelector('[data-f="online"]');
      if (onlineCell) {
        const badge = document.createElement("span");
        badge.className = "wsk-badge";
        onlineCell.replaceChildren(badge);
        setBadge(
          badge,
          isNil(client.online)
            ? { variant: null, text: fmtText(null) }
            : client.online
              ? { variant: "success", text: "在线" }
              : { variant: "danger", text: "离线" },
        );
      }
      setTime(row, "first_connected_at", client.first_connected_at, nowSec);
      setTime(row, "last_connected_at", client.last_connected_at, nowSec);
    },
    "该节点暂无 FRP 客户端记录。",
  );
}
