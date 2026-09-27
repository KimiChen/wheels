import {UNKNOWN, bytes, capacity, decimal, percent, percentage, ratio, quality, loadText, uptime, timeText, sessionLabels, freshnessLabels, frpLabel, reconciliationText} from "./format.mjs";
import {select, overview} from "./store.mjs";
import {connect} from "./transport.mjs";
import {createHistoryPanel} from "./history-view.mjs";

const byID = id => document.getElementById(id);
const write = (element, text) => { const next = String(text ?? UNKNOWN); if (element.textContent !== next) element.textContent = next; };
const put = (id, text) => write(byID(id), text);
const list = byID("node-list"), table = byID("table-view"), rows = byID("node-table-body"), cards = new Map();
const search = byID("node-search"), viewButtons = [...document.querySelectorAll("[data-fm-view]")];
let current = null, view = "cards", sequence = 0;
try { if (localStorage.getItem("frp-monitor-view") === "table") view = "table"; } catch { /* Storage is optional. */ }
function syncView() {
  list.hidden = view !== "cards";
  table.hidden = view !== "table" || !current?.nodes.length;
  for (const button of viewButtons) button.setAttribute("aria-pressed", String(button.dataset.fmView === view));
}
function syncDetail(card) {
  if (view === "table" && !card.details.open && card.element.contains(document.activeElement)) card.toggle.focus();
  card.detailRow.hidden = view !== "table" || card.element.hidden || !card.details.open;
  card.toggle.setAttribute("aria-expanded", String(card.details.open));
  write(card.toggle, card.details.open ? "收起" : "展开");
}

function createCard(node) {
  const element = byID("node-template").content.firstElementChild.cloneNode(true);
  const row = byID("row-template").content.firstElementChild.cloneNode(true);
  const detailRow = document.createElement("tr"), cell = document.createElement("td");
  detailRow.className = "fm-table-detail"; detailRow.hidden = true;
  detailRow.id = `node-detail-${++sequence}`; cell.colSpan = 10; detailRow.append(cell);
  const labels = new Map(), meters = new Map();
  for (const root of [element, row]) {
    for (const el of root.querySelectorAll("[data-value]")) labels.set(el.dataset.value, [...(labels.get(el.dataset.value) ?? []), el]);
    for (const el of root.querySelectorAll("[data-meter]")) meters.set(el.dataset.meter, [...(meters.get(el.dataset.meter) ?? []), el]);
  }
  const details = element.querySelector(".fm-history"), toggle = row.querySelector(".fm-detail-toggle");
  toggle.setAttribute("aria-controls", detailRow.id);
  const card = {element, row, detailRow, cell, labels, meters, details, toggle, history: createHistoryPanel(element, node.id)};
  toggle.addEventListener("click", () => { details.open = !details.open; syncDetail(card); });
  details.addEventListener("toggle", () => syncDetail(card));
  return card;
}
function patchCard(card, node) {
  const text = (key, next) => { for (const el of card.labels.get(key) ?? []) write(el, next); };
  const state = (key, next) => { for (const el of card.labels.get(key) ?? []) el.dataset.state = next; };
  const metric = node.metrics ?? {};
  const meter = (key, n) => { for (const el of card.meters.get(key)) { el.hidden = n === null; if (n !== null && el.value !== n) el.value = n; } };
  text("name", node.name);
  card.history.setName(node.name);
  card.toggle.setAttribute("aria-label", `${node.name} · 趋势与探测`);
  card.element.dataset.state = node.session;
  text("scope", ({host: "主机采集", namespace: "容器 / 命名空间采集", unknown: "采集范围未知"})[metric.scope] ?? "等待资源报告");
  text("session", sessionLabels[node.session]); state("session", node.session);
  text("freshness", freshnessLabels[node.freshness]); state("freshness", node.freshness);
  text("frp", frpLabel(node.frp?.control_state));
  text("frp-reconciliation", reconciliationText(node.frp));
  const cpu = percent(metric.cpu), mem = ratio(metric.mem_used, metric.mem_total), disk = ratio(metric.disk_used, metric.disk_total);
  text("cpu", percentage(cpu)); text("mem", percentage(mem)); text("disk", percentage(disk));
  meter("cpu", cpu); meter("mem", mem); meter("disk", disk);
  text("cpu-note", cpu === null ? quality(metric.cpu) : "CPU 使用率");
  text("mem-note", capacity(metric.mem_used, metric.mem_total));
  text("disk-note", capacity(metric.disk_used, metric.disk_total));
  text("rx", bytes(decimal(metric.net_rx), true)); text("tx", bytes(decimal(metric.net_tx), true));
  text("rx-total", `系统计数器累计 ${bytes(decimal(metric.net_rx_total))}`); text("tx-total", `系统计数器累计 ${bytes(decimal(metric.net_tx_total))}`);
  text("load", loadText(metric.load)); text("uptime", uptime(metric.uptime));
  const frp = node.frp;
  text("proxies", frp?.control_state && frp.control_state !== "unknown" && Number.isSafeInteger(frp.proxy_running) && Number.isSafeInteger(frp.proxy_total) ? `${frp.proxy_running} / ${frp.proxy_total}` : UNKNOWN);
  text("sample-time", node.metrics_at ? `采样于 ${timeText(node.metrics_at)}${node.freshness === "stale" ? " · 已过期" : ""}` : "尚未收到资源报告");
  text("interval", Number.isFinite(node.interval_seconds) && node.interval_seconds > 0 ? `${node.interval_seconds} 秒 / 次` : UNKNOWN);
}
function render() {
  if (!current) return;
  const nodes = current.nodes, summary = overview(nodes);
  put("snapshot-at", timeText(current.generated_at)); byID("snapshot-at").dateTime = current.generated_at;
  put("stat-online", summary.online); put("stat-total", summary.total);
  put("stat-offline", summary.offline); put("stat-waiting", summary.waiting);
  put("stat-cpu", percentage(summary.cpu.value));
  byID("stat-cpu-note").title = `来自 ${summary.cpu.count} 个在线且新鲜的有效 CPU 样本`;
  put("stat-rx", bytes(summary.rx.value, true)); put("stat-tx", bytes(summary.tx.value, true));
  byID("stat-network-note").title = `仅汇总在线且新鲜的有效采样：发送 ${summary.tx.count} / 接收 ${summary.rx.count} 个节点`;

  const existing = new Set(nodes.map(n => n.id));
  for (const [id, card] of cards) if (!existing.has(id)) {
    card.history.stop(); card.element.remove(); card.row.remove(); card.detailRow.remove(); cards.delete(id);
  }
  for (const node of nodes) {
    if (!cards.has(node.id)) cards.set(node.id, createCard(node));
    patchCard(cards.get(node.id), node);
  }
  const shown = select(nodes, search.value), visible = new Set(shown.map(n => n.id));
  // Both views share one detail instance; a hidden node never polls history.
  for (const [id, card] of cards) {
    card.element.hidden = !visible.has(id); card.row.hidden = !visible.has(id);
    card.history.setVisible(visible.has(id));
    syncDetail(card);
  }
  // Keep existing DOM nodes, focus, form state and scroll across live snapshots.
  nodes.forEach((node, index) => {
    const card = cards.get(node.id);
    for (const [offset, el] of [card.row, card.detailRow].entries()) {
      const before = rows.children[index * 2 + offset] ?? null;
      if (before !== el) rows.insertBefore(el, before);
    }
    if (view === "table") {
      if (card.element.parentElement !== card.cell) card.cell.append(card.element);
    } else {
      const before = list.children[index] ?? null;
      if (before !== card.element) list.insertBefore(card.element, before);
    }
  });
  syncView();
  table.hidden = view !== "table" || !shown.length;
  byID("empty-state").hidden = shown.length !== 0;
  if (!nodes.length) { put("empty-title", "还没有监控节点"); put("empty-copy", "节点接入后，资源和连接状态会自动出现在这里。"); }
  else { put("empty-title", "没有匹配的节点"); put("empty-copy", "试试其他名称，或清空搜索查看全部节点。"); }
  put("list-summary", `显示 ${shown.length} / ${nodes.length} 个节点`);
}
function onState(next) {
  byID("stream-status").dataset.state = next;
  put("stream-label", ({loading: "正在同步", live: "已连接", disconnected: "实时连接中断", error: "暂时无法同步"})[next]);
  const notice = byID("connection-notice");
  notice.hidden = next === "live";
  if (next === "loading") write(notice, current ? "正在重新获取快照并连接实时更新，当前显示最近一次数据。" : "正在获取监控快照…");
  else if (next !== "live") write(notice, current ? "浏览器实时连接已中断，正在自动重连。以下为最近一次快照，不代表节点的当前状态。" : "暂时无法连接监控服务，正在自动重试。尚未获取节点状态。");
  if (!current && next === "error") { put("empty-title", "暂时无法读取节点"); put("empty-copy", "监控服务恢复后页面会自动重新同步，也可以点击刷新重试。"); }
}
search.addEventListener("input", render);
for (const button of viewButtons) button.addEventListener("click", () => {
  view = button.dataset.fmView;
  try { localStorage.setItem("frp-monitor-view", view); } catch { /* Storage is optional. */ }
  syncView(); render();
});
syncView();
const connection = connect({onSnapshot(data) { current = data; render(); }, onState});
byID("retry").addEventListener("click", () => connection.refresh());
window.addEventListener("pagehide", () => { connection.stop(); for (const card of cards.values()) card.history.stop(); });
window.addEventListener("pageshow", event => { if (event.persisted) window.location.reload(); });
