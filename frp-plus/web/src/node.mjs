import {UNKNOWN, bytes, decimal, percent, percentage, ratio, capacity, loadText, uptime, timeText, sessionLabels, freshnessLabels} from "./format.mjs";
import {nodeID, hardwareValue, cpuModel, cpuCores, cardHardwareText, groupText} from "./node-data.mjs";
import {billingText, todayText, planText, trafficModes, dateOnly} from "./node-settings.mjs";
import {connect, SNAPSHOT_REFRESH_SECONDS} from "./transport.mjs";
import {createConnectionStatus} from "./connection-status.mjs";
import {createHistoryPanel} from "./history-view.mjs";

const byID = id => document.getElementById(id);
const write = (element, value) => { const next = String(value ?? UNKNOWN); if (element.textContent !== next) element.textContent = next; };
const labels = new Map([...document.querySelectorAll("[data-value]")].map(element => [element.dataset.value, element]));
const meters = new Map([...document.querySelectorAll("[data-meter]")].map(element => [element.dataset.meter, element]));
const put = (key, value) => write(labels.get(key), value);
const id = nodeID(window.location.pathname), detail = byID("node-detail"), empty = byID("node-empty"), historyHost = byID("node-history");
const connectionStatus = createConnectionStatus({notice: byID("connection-notice")});
write(byID("snapshot-refresh-seconds"), SNAPSHOT_REFRESH_SECONDS);
let current = null, panel = null, connection = null;

function showEmpty(title, copy) {
  detail.hidden = true; empty.hidden = false;
  write(byID("empty-title"), title); write(byID("empty-copy"), copy);
}
function clearNode() {
  panel?.stop(); panel = null; historyHost.replaceChildren();
  for (const element of labels.values()) { write(element, UNKNOWN); element.removeAttribute("data-state"); element.removeAttribute("title"); }
  for (const element of meters.values()) { element.hidden = true; element.removeAttribute("value"); }
  byID("node-public-note").hidden = true; write(byID("node-public-note"), "");
  document.title = "FRP Plus · 节点详情";
}
function patchNode(node) {
  detail.hidden = false; empty.hidden = true;
  const metrics = node.metrics ?? {}, hardware = node.hardware;
  document.title = `${node.name} · FRP Plus`;
  put("name", node.name); put("session", sessionLabels[node.session] ?? UNKNOWN);
  labels.get("session").dataset.state = node.session; labels.get("session").title = sessionLabels[node.session];
  put("hardware-summary", cardHardwareText(hardware));
  const groups = groupText(node);
  put("groups", groups); labels.get("groups").hidden = !groups;
  put("freshness", freshnessLabels[node.freshness] ?? UNKNOWN); labels.get("freshness").dataset.state = node.freshness;
  labels.get("freshness").title = node.metrics_at ? `资源采样于 ${timeText(node.metrics_at)}` : "尚未收到资源报告";
  const usage = {cpu: percent(metrics.cpu), memory: ratio(metrics.mem_used, metrics.mem_total), disk: ratio(metrics.disk_used, metrics.disk_total)};
  for (const [key, value] of Object.entries(usage)) {
    put(`${key}-current`, percentage(value));
    const meter = meters.get(key); meter.hidden = value === null;
    if (value === null) meter.removeAttribute("value"); else meter.value = value;
  }
  const cores = cpuCores(hardware);
  put("cpu-capacity", cores === null ? "逻辑核心数未知" : `${cores} 个逻辑核心`);
  put("memory-capacity", capacity(metrics.mem_used, metrics.mem_total)); put("disk-capacity", capacity(metrics.disk_used, metrics.disk_total));
  put("network-rx", bytes(decimal(metrics.net_rx), true)); put("network-tx", bytes(decimal(metrics.net_tx), true));
  put("uptime", uptime(metrics.uptime));
  const system = ["os", "arch", "virt"].map(key => hardwareValue(hardware?.[key])).filter(value => value !== UNKNOWN);
  put("os", system.length ? system.join(" · ") : UNKNOWN);
  put("cpu-model", cpuModel(hardware)); put("load", loadText(metrics.load));
  put("memory-usage", `${bytes(decimal(metrics.mem_used))} / ${bytes(decimal(metrics.mem_total))}`);
  put("storage-usage", `${bytes(decimal(metrics.disk_used))} / ${bytes(decimal(metrics.disk_total))}`);
  for (const [key, field] of [["processes", "procs"], ["tcp", "tcp"], ["udp", "udp"]]) put(key, decimal(metrics[field]));
  const today = todayText(node.traffic_today), plan = planText(node.traffic_plan);
  put("traffic-rx", today.rx); put("traffic-tx", today.tx);
  put("system-rx", bytes(decimal(metrics.net_rx_total))); put("system-tx", bytes(decimal(metrics.net_tx_total)));
  for (const field of document.querySelectorAll("[data-plan-field]")) field.hidden = !node.traffic_plan;
  put("plan-usage", `${plan.used} / ${plan.quota}`);
  put("plan-period", `${(trafficModes[node.traffic_plan?.mode] ?? UNKNOWN).toUpperCase()} · ${plan.percent} · ${dateOnly(node.traffic_plan?.period_start_at_ms)} - ${dateOnly(node.traffic_plan?.period_end_at_ms)}`);
  byID("node-billing").hidden = !node.billing; put("billing", billingText(node.billing)); put("renewal-note", node.billing?.renewal_note || "未填写续费说明");
  byID("node-public-note").hidden = !node.public_note; write(byID("node-public-note"), node.public_note);
  if (!panel) panel = createHistoryPanel(historyHost, id);
  panel.updateMetrics(metrics);
  panel.setVisible(document.visibilityState !== "hidden");
}
function onSnapshot(data) {
  current = data;
  const node = data.nodes.find(node => node.id === id);
  if (node) patchNode(node);
  else { clearNode(); showEmpty("节点不存在或已移除", "当前公开列表中没有这个节点。返回节点列表查看仍在监控的节点。"); }
}
function onState(state) {
  connectionStatus.update(state, current ? "实时连接已中断，正在自动重连。当前为最近一次快照，不代表节点的当前状态。" : "暂时无法连接监控服务，正在自动重试。尚未获取节点状态。");
  detail.dataset.connection = state;
  if (!current && state === "error") showEmpty("暂时无法读取节点", "监控服务恢复后会自动同步，也可以点击重新同步重试。");
}
if (id) connection = connect({onSnapshot, onState});
else {
  clearNode(); showEmpty("节点地址无效", "请从公开节点列表打开节点详情。");
  byID("connection-notice").hidden = true; byID("retry-empty").hidden = true;
}
byID("retry-empty").addEventListener("click", () => connection?.refresh());
document.addEventListener("visibilitychange", () => panel?.setVisible(document.visibilityState !== "hidden"));
window.addEventListener("pagehide", () => { connection?.stop(); panel?.stop(); });
window.addEventListener("pageshow", event => { if (event.persisted) window.location.reload(); });
