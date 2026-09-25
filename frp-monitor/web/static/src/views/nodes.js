// SPDX-License-Identifier: Apache-2.0
// 节点列表：snapshot 全量渲染（仅重建 tbody，保留筛选输入、排序状态、页码与
// 滚动位置）；node 增量事件逐项更新单元格数值，不重建整表。新行来自
// <template> 片段，插入后调用 wsk.mount(fragment) 完成集合初始渲染。

import {
  fmtAgo,
  fmtBytes,
  fmtClock,
  fmtCpu,
  fmtFailRate,
  fmtPair,
  fmtPercent,
  fmtRate,
  isNil,
  sumByteValues,
  UNKNOWN,
} from "../format.js";
import { fillStatusBadges } from "./status.js";

function setText(row, field, text) {
  const el = row.querySelector(`[data-f="${field}"]`);
  if (el) el.textContent = text;
}

/**
 * TCP 探测列：取各任务近 15 分钟窗口失败率的最差值；无任务显示「—」，
 * 有任务但均无样本显示「未知」。title 逐任务列出明细。
 */
function fillProbeCell(row, probes) {
  const cell = row.querySelector('[data-f="probe"]');
  if (!cell) return;
  const sub = row.querySelector('[data-f="probe_sub"]');
  if (!Array.isArray(probes) || probes.length === 0) {
    cell.textContent = "—";
    cell.title = "未配置探测任务";
    if (sub) sub.textContent = "";
    return;
  }
  const rates = probes
    .map((probe) => probe?.fail_rate)
    .filter((rate) => typeof rate === "number" && Number.isFinite(rate));
  cell.textContent = rates.length > 0 ? fmtFailRate(Math.max(...rates)) : UNKNOWN;
  cell.title = probes
    .map((probe) => `${probe?.id ?? "?"}: ${fmtFailRate(probe?.fail_rate)}`)
    .join("\n");
  if (sub) sub.textContent = `${probes.length} 个任务`;
}

/** 今日流量列：节点网卡今日收 + 发合计；traffic 为 null 显示「未知」。 */
function fillTrafficTodayCell(row, traffic) {
  const cell = row.querySelector('[data-f="traffic_today"]');
  if (!cell) return;
  const sum = traffic
    ? sumByteValues(traffic.today_rx_bytes, traffic.today_tx_bytes)
    : null;
  cell.textContent = sum === null ? UNKNOWN : fmtBytes(sum);
  cell.title = "节点网卡今日收 + 发合计，与 FRP 隧道流量口径不同";
}

/** 就地填充一行：只改文本与徽章样式，不替换可聚焦元素本身。 */
export function fillRow(row, node, nowSec, { hrefFor } = {}) {
  row.dataset.nodeId = node.id;
  row.dataset.name = node.id ?? "";
  row.dataset.cpu = isNil(node.cpu) ? "" : String(node.cpu);
  row.dataset.mem = isNil(node.mem_used) ? "" : String(node.mem_used);

  setText(row, "id", node.id);
  const link = row.querySelector("[data-f-link]");
  if (link && typeof hrefFor === "function") link.href = hrefFor(node);

  fillStatusBadges(row, node);

  setText(row, "cpu", fmtCpu(node.cpu));
  setText(row, "mem", fmtPair(node.mem_used, node.mem_total));
  setText(row, "mem_pct", `占比 ${fmtPercent(node.mem_used, node.mem_total)}`);
  setText(row, "disk", fmtPercent(node.disk_used, node.disk_total));
  setText(row, "disk_pair", fmtPair(node.disk_used, node.disk_total));
  setText(row, "net_rx", `↓ ${fmtRate(node.net_rx)}`);
  setText(row, "net_tx", `↑ ${fmtRate(node.net_tx)}`);
  fillProbeCell(row, node.probes);
  fillTrafficTodayCell(row, node.traffic);

  const updated = row.querySelector('[data-f="updated"]');
  if (updated) {
    updated.textContent = isNil(node.last_seen)
      ? fmtAgo(null, nowSec)
      : fmtAgo(node.last_seen, nowSec);
    updated.title = fmtClock(node.last_seen);
  }
}

/** 重新应用表格上保存的排序状态（全量重建行之后调用）。隧道页复用。 */
export function applyStoredSort(table) {
  const key = table.dataset.wskSortKey;
  const direction = table.dataset.wskSortDirection;
  if (!key || !direction) return;
  const body = table.tBodies[0];
  if (!body) return;
  const emptyRow = table.querySelector("[data-table-empty]");
  const rows = [...body.querySelectorAll("[data-row]")];
  rows
    .sort(
      (a, b) =>
        (a.dataset[key] ?? "").localeCompare(b.dataset[key] ?? "", "zh-CN", {
          numeric: true,
        }) * (direction === "ascending" ? 1 : -1),
    )
    .forEach((row) => body.insertBefore(row, emptyRow));

  const scope = table.closest("[data-table-scope]") ?? document;
  scope.querySelectorAll("th[aria-sort]").forEach((header) => {
    header.setAttribute("aria-sort", "none");
  });
  scope.querySelectorAll("[data-sort]").forEach((button) => {
    const current = button.dataset.sort === key;
    const icon = current
      ? direction === "ascending"
        ? "#sort-asc"
        : "#sort-desc"
      : "#sort";
    button.querySelector("use")?.setAttribute("href", icon);
    if (current) button.closest("th")?.setAttribute("aria-sort", direction);
  });
}

export function createNodeTable({ table, template, hrefFor }) {
  const body = table.tBodies[0];
  const emptyRow = table.querySelector("[data-table-empty]");
  const nodes = new Map();

  // 空态文案写在 <td> 上；tbody 初始用它展示「正在连接…」占位。
  function setEmptyText(text) {
    const cell = emptyRow.querySelector("td") ?? emptyRow;
    cell.textContent = text;
  }

  function buildRow(node, nowSec) {
    const fragment = template.content.cloneNode(true);
    const row = fragment.querySelector("[data-row]");
    fillRow(row, node, nowSec, { hrefFor });
    return row;
  }

  return {
    nodes,
    table,

    /** snapshot：全量重建行，保留集合控件状态。 */
    setAll(list, nowSec) {
      nodes.clear();
      const rows = list.map((node) => {
        nodes.set(node.id, node);
        return buildRow(node, nowSec);
      });
      body.replaceChildren(...rows, emptyRow);
      setEmptyText(list.length === 0 ? "暂无节点接入。" : "没有符合条件的节点。");
      applyStoredSort(table);
      globalThis.wsk?.mount(table);
    },

    /** node 事件：存在则就地更新数值；不存在则插入新行。 */
    upsert(node, nowSec) {
      nodes.set(node.id, node);
      const selector = `[data-node-id="${CSS.escape(node.id)}"]`;
      const existing = body.querySelector(selector);
      if (existing) {
        fillRow(existing, node, nowSec, { hrefFor });
        return;
      }
      body.insertBefore(buildRow(node, nowSec), emptyRow);
      setEmptyText("没有符合条件的节点。");
      globalThis.wsk?.mount(table);
    },

    /** 相对时间随本地时钟推进，不重排不重绘其它单元格。 */
    refreshTimes(nowSec) {
      for (const row of body.querySelectorAll("[data-row]")) {
        const node = nodes.get(row.dataset.nodeId);
        if (!node) continue;
        const updated = row.querySelector('[data-f="updated"]');
        if (updated) {
          updated.textContent = fmtAgo(node.last_seen, nowSec);
          updated.title = fmtClock(node.last_seen);
        }
      }
    },
  };
}
