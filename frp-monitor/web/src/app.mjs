import {UNKNOWN, bytes, capacity, decimal, percent, percentage, ratio, quality, loadText, uptime, timeText, sessionLabels, freshnessLabels, frpLabel, reconciliationText} from "./format.mjs";
import {select, overview} from "./store.mjs";
import {connect} from "./transport.mjs";
import {createHistoryPanel} from "./history-view.mjs";

const byID = id => document.getElementById(id);
const write = (element, text) => { const next = String(text ?? UNKNOWN); if (element.textContent !== next) element.textContent = next; };
const put = (id, text) => write(byID(id), text);
const list = byID("node-list"), cards = new Map();
const search = byID("node-search"), filter = byID("node-filter"), sort = byID("node-sort");
let current = null;

function createCard(node) {
  const element = byID("node-template").content.firstElementChild.cloneNode(true);
  const labels = Object.fromEntries([...element.querySelectorAll("[data-value]")].map(el => [el.dataset.value, el]));
  const meters = Object.fromEntries([...element.querySelectorAll("[data-meter]")].map(el => [el.dataset.meter, el]));
  return {element, labels, meters, history: createHistoryPanel(element, node.id)};
}
function patchCard(card, node) {
  const text = (key, next) => write(card.labels[key], next);
  const metric = node.metrics ?? {};
  const meter = (key, n) => { card.meters[key].hidden = n === null; if (n !== null && card.meters[key].value !== n) card.meters[key].value = n; };
  text("name", node.name);
  card.history.setName(node.name);
  text("scope", ({host: "主机采集", namespace: "容器 / 命名空间采集", unknown: "采集范围未知"})[metric.scope] ?? "等待资源报告");
  text("session", sessionLabels[node.session]); card.labels.session.dataset.state = node.session;
  text("freshness", freshnessLabels[node.freshness]); card.labels.freshness.dataset.state = node.freshness;
  text("frp", frpLabel(node.frp?.control_state));
  text("frp-reconciliation", reconciliationText(node.frp));
  const cpu = percent(metric.cpu), mem = ratio(metric.mem_used, metric.mem_total), disk = ratio(metric.disk_used, metric.disk_total);
  text("cpu", percentage(cpu)); text("mem", percentage(mem)); text("disk", percentage(disk));
  meter("cpu", cpu); meter("mem", mem); meter("disk", disk);
  text("cpu-note", cpu === null ? quality(metric.cpu) : "整机 CPU 使用率");
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
  put("stat-online-note", `${summary.offline} 个离线 · ${summary.waiting} 个等待接入`);
  put("stat-fresh", summary.fresh); put("stat-fresh-note", `${summary.stale} 个数据过期 · 以服务端判定为准`);
  put("stat-proxies", summary.proxyRunning); put("stat-proxies-total", summary.proxyTotal);
  put("stat-proxy-note", `来自 ${summary.proxyNodes} 个在线且新鲜的节点`);
  put("stat-rx", bytes(summary.rx.value, true)); put("stat-tx", bytes(summary.tx.value, true));
  put("stat-network-note", `有效采样：接收 ${summary.rx.count} / 发送 ${summary.tx.count} 个节点`);
  put("node-count", summary.total);

  const existing = new Set(nodes.map(n => n.id));
  for (const [id, card] of cards) if (!existing.has(id)) { card.history.stop(); card.element.remove(); cards.delete(id); }
  for (const node of nodes) {
    if (!cards.has(node.id)) cards.set(node.id, createCard(node));
    patchCard(cards.get(node.id), node);
  }
  const shown = select(nodes, search.value, filter.value, sort.value), visible = new Set(shown.map(n => n.id));
  for (const [id, card] of cards) { card.element.hidden = !visible.has(id); card.history.setVisible(visible.has(id)); }
  // Keep existing DOM nodes, focus, form state and scroll; move only changed order.
  const ordered = [...shown, ...nodes.filter(n => !visible.has(n.id))];
  ordered.forEach((node, index) => {
    const el = cards.get(node.id).element, before = list.children[index] ?? null;
    if (before !== el) list.insertBefore(el, before);
  });
  byID("empty-state").hidden = shown.length !== 0;
  if (!nodes.length) { put("empty-title", "还没有监控节点"); put("empty-copy", "节点接入后，资源和连接状态会自动出现在这里。"); }
  else { put("empty-title", "没有匹配的节点"); put("empty-copy", "试试其他名称，或将状态筛选切换为全部节点。"); }
  put("list-summary", `显示 ${shown.length} / ${nodes.length} 个节点`);
}
function onState(next) {
  byID("stream-status").dataset.state = next;
  put("stream-label", ({loading: "正在同步", live: "实时连接正常", disconnected: "实时连接中断", error: "暂时无法同步"})[next]);
  const notice = byID("connection-notice");
  notice.hidden = next === "live";
  if (next === "loading") write(notice, current ? "正在重新获取快照并连接实时更新，当前显示最近一次数据。" : "正在获取监控快照…");
  else if (next !== "live") write(notice, current ? "浏览器实时连接已中断，正在自动重连。以下为最近一次快照，不代表节点的当前状态。" : "暂时无法连接监控服务，正在自动重试。尚未获取节点状态。");
  if (!current && next === "error") { put("empty-title", "暂时无法读取节点"); put("empty-copy", "监控服务恢复后页面会自动重新同步，也可以点击刷新重试。"); }
}
search.addEventListener("input", render);
filter.addEventListener("change", render);
sort.addEventListener("change", render);
const connection = connect({onSnapshot(data) { current = data; render(); }, onState});
byID("retry").addEventListener("click", () => connection.refresh());
window.addEventListener("pagehide", () => { connection.stop(); for (const card of cards.values()) card.history.stop(); });
window.addEventListener("pageshow", event => { if (event.persisted) window.location.reload(); });
