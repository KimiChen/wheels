import {UNKNOWN, bytes, cumulative, capacity, decimal, percent, percentage, uptime, timeText, sessionLabels, freshnessLabels, frpLabel, reconciliationText} from "./format.mjs";
import {nodeID, hardwareValue, cpuModel} from "./node-data.mjs";
import {billingText, todayText, planText, dateTime} from "./node-settings.mjs";
import {connect} from "./transport.mjs";
import {createHistoryPanel} from "./history-view.mjs";

const byID = id => document.getElementById(id);
const write = (element, value) => { const next = String(value ?? UNKNOWN); if (element.textContent !== next) element.textContent = next; };
const labels = new Map([...document.querySelectorAll("[data-value]")].map(element => [element.dataset.value, element]));
const put = (key, value) => write(labels.get(key), value);
const id = nodeID(window.location.pathname), detail = byID("node-detail"), empty = byID("node-empty"), historyHost = byID("node-history");
let current = null, panel = null, connection = null;

function showEmpty(title, copy) {
  detail.hidden = true; empty.hidden = false;
  write(byID("empty-title"), title); write(byID("empty-copy"), copy);
}
function clearNode() {
  panel?.stop(); panel = null; historyHost.replaceChildren();
  for (const element of labels.values()) { write(element, UNKNOWN); element.removeAttribute("data-state"); element.removeAttribute("title"); }
  write(byID("traffic-note"), "等待今日统计"); write(byID("footer-sample"), "无可显示的节点数据");
  document.title = "FRP Monitor · 节点详情";
}
function patchNode(node) {
  detail.hidden = false; empty.hidden = true;
  const metrics = node.metrics ?? {}, hardware = node.hardware;
  document.title = `${node.name} · FRP Monitor`;
  put("name", node.name); put("session", sessionLabels[node.session]); labels.get("session").dataset.state = node.session;
  put("freshness", `数据${freshnessLabels[node.freshness]}`); labels.get("freshness").dataset.state = node.freshness;
  put("uptime", uptime(metrics.uptime)); put("agent", hardwareValue(hardware?.agent_version));
  put("os", hardwareValue(hardware?.os)); put("cpu-model", cpuModel(hardware)); put("cpu", percentage(percent(metrics.cpu)));
  put("memory", capacity(metrics.mem_used, metrics.mem_total)); put("disk", capacity(metrics.disk_used, metrics.disk_total));
  const environment = [hardwareValue(hardware?.arch), hardwareValue(hardware?.virt)].filter(value => value !== UNKNOWN);
  put("environment", environment.length ? environment.join(" · ") : UNKNOWN);
  for (const [key, field] of [["processes", "procs"], ["tcp", "tcp"], ["udp", "udp"]]) put(key, decimal(metrics[field]));
  put("scope", ({host: "主机采集", namespace: "容器 / 命名空间采集", unknown: "采集范围未知"})[metrics.scope] ?? "等待资源报告");
  const frp = node.frp;
  put("proxies", frp?.control_state && frp.control_state !== "unknown" && Number.isSafeInteger(frp.proxy_running) && Number.isSafeInteger(frp.proxy_total) && frp.proxy_running >= 0 && frp.proxy_total >= frp.proxy_running ? `${frp.proxy_running} / ${frp.proxy_total}` : UNKNOWN);
  const today = todayText(node.traffic_today), plan = planText(node.traffic_plan);
  put("traffic-rx", today.rx); put("traffic-tx", today.tx); write(byID("traffic-note"), today.note);
  put("system-rx", bytes(decimal(metrics.net_rx_total))); put("system-tx", bytes(decimal(metrics.net_tx_total)));
  byID("node-plan").hidden = !node.traffic_plan; put("plan-usage", `${plan.used} / ${plan.quota}`);
  put("plan-remaining", `${plan.remaining} · ${plan.percent}`); put("plan-note", plan.note);
  put("plan-period", `${dateTime(node.traffic_plan?.period_start_at_ms)} 起 · ${node.traffic_plan?.period_end_at_ms == null ? "等待手动重置" : `${dateTime(node.traffic_plan.period_end_at_ms)} 重置`}`);
  byID("node-billing").hidden = !node.billing; put("billing", billingText(node.billing)); put("renewal-note", node.billing?.renewal_note || "未填写续费说明");
  byID("node-public-note").hidden = !node.public_note; write(byID("node-public-note"), node.public_note);
  put("frp-traffic", `↓ ${bytes(cumulative(frp?.today_rx_bytes))} / ↑ ${bytes(cumulative(frp?.today_tx_bytes))}`);
  put("frp", frpLabel(frp?.control_state)); put("frp-reconciliation", reconciliationText(frp));
  put("sample-time", node.metrics_at ? `资源采样 ${timeText(node.metrics_at)}${node.freshness === "stale" ? " · 已过期" : ""}` : "尚未收到资源报告");
  put("interval", Number.isFinite(node.interval_seconds) && node.interval_seconds > 0 ? `${node.interval_seconds} 秒 / 次` : UNKNOWN);
  write(byID("footer-sample"), `CPU ${percentage(percent(metrics.cpu))} · ↑ ${bytes(decimal(metrics.net_tx), true)} · ↓ ${bytes(decimal(metrics.net_rx), true)}`);
  if (!panel) panel = createHistoryPanel(historyHost, id);
  panel.setVisible(document.visibilityState !== "hidden");
}
function onSnapshot(data) {
  current = data;
  write(byID("snapshot-at"), timeText(data.generated_at)); byID("snapshot-at").dateTime = data.generated_at;
  const node = data.nodes.find(node => node.id === id);
  if (node) patchNode(node);
  else { clearNode(); showEmpty("节点不存在或已移除", "当前公开列表中没有这个节点。返回节点列表查看仍在监控的节点。"); }
}
function onState(state) {
  byID("stream-status").dataset.state = state;
  write(byID("stream-label"), ({loading: "正在同步", live: "已连接", disconnected: "实时连接中断", error: "暂时无法同步"})[state]);
  const notice = byID("connection-notice"); notice.hidden = state === "live";
  detail.dataset.connection = state;
  if (state === "loading") write(notice, current ? "正在重新同步节点，当前保留最近一次快照。" : "正在获取节点快照…");
  else if (state !== "live") write(notice, current ? "实时连接已中断，正在自动重连。当前为最近一次快照，不代表节点的当前状态。" : "暂时无法连接监控服务，正在自动重试。尚未获取节点状态。");
  if (!current && state === "error") showEmpty("暂时无法读取节点", "监控服务恢复后会自动同步，也可以点击重新同步重试。");
}
if (id) connection = connect({onSnapshot, onState});
else {
  clearNode(); showEmpty("节点地址无效", "请从公开节点列表打开节点详情。");
  byID("connection-notice").hidden = true; byID("retry-empty").hidden = true;
  byID("stream-status").dataset.state = "error"; write(byID("stream-label"), "地址无效");
}
for (const button of [byID("retry"), byID("retry-empty")]) button.addEventListener("click", () => connection?.refresh());
document.addEventListener("visibilitychange", () => panel?.setVisible(document.visibilityState !== "hidden"));
window.addEventListener("pagehide", () => { connection?.stop(); panel?.stop(); });
window.addEventListener("pageshow", event => { if (event.persisted) window.location.reload(); });
