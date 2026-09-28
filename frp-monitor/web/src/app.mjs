import {UNKNOWN, bytes, capacity, decimal, percent, percentage, ratio, quality, loadText, uptime, timeText, sessionLabels, freshnessLabels} from "./format.mjs";
import {select, overview, groupOptions, resolveGroupSelection, sortOptions, sortNodes, ALL_GROUPS, UNGROUPED} from "./store.mjs";
import {connect} from "./transport.mjs";
import {createConnectionStatus} from "./connection-status.mjs";
import {planText} from "./node-settings.mjs";
import {nodeURL, hardwareText, cardHardwareText, cpuLabel, groupText, nodeBadgeText} from "./node-data.mjs";

const byID = id => document.getElementById(id);
const write = (element, text) => { const next = String(text ?? UNKNOWN); if (element.textContent !== next) element.textContent = next; };
const put = (id, text) => write(byID(id), text);
const list = byID("node-list"), table = byID("table-view"), rows = byID("node-table-body"), cards = new Map();
const search = byID("node-search"), viewButtons = [...document.querySelectorAll("[data-fm-view]")];
const searchDialog = byID("node-search-dialog"), searchToggle = byID("search-toggle"), searchResults = byID("search-results"), searchItems = new Map();
const groupBar = byID("node-group-filter"), groupButtons = new Map(), groupStorageKey = "frp-monitor-public-group";
const sort = byID("node-sort"), sortStorageKey = "frp-monitor-public-sort";
sort.replaceChildren(...sortOptions.map(([value, name]) => {
  const option = document.createElement("option"); option.value = value; option.textContent = name; return option;
}));
const connectionStatus = createConnectionStatus({status: byID("stream-status"), label: byID("stream-label"), notice: byID("connection-notice")});
let current = null, view = "cards", selectedGroup = ALL_GROUPS;
try { if (localStorage.getItem("frp-monitor-view") === "table") view = "table"; } catch { /* Storage is optional. */ }
try { selectedGroup = sessionStorage.getItem(groupStorageKey) ?? ALL_GROUPS; } catch { /* Storage is optional. */ }
try { const saved = sessionStorage.getItem(sortStorageKey); if (sortOptions.some(([key]) => key === saved)) sort.value = saved; } catch { /* Storage is optional. */ }
function rememberGroup() {
  try {
    if (selectedGroup === ALL_GROUPS) sessionStorage.removeItem(groupStorageKey);
    else sessionStorage.setItem(groupStorageKey, selectedGroup);
  } catch { /* Storage is optional. */ }
}
function syncGroups(nodes) {
  const options = groupOptions(nodes).filter(group => group.key !== UNGROUPED), valid = new Set(options.map(group => group.key));
  const next = resolveGroupSelection(selectedGroup, options);
  if (next !== selectedGroup) { selectedGroup = next; rememberGroup(); }
  for (const [key, button] of groupButtons) if (!valid.has(key)) { button.remove(); groupButtons.delete(key); }
  options.forEach((group, index) => {
    let button = groupButtons.get(group.key);
    if (!button) {
      button = document.createElement("button"); button.type = "button"; button.className = "wsk-button wsk-quiet fm-group-button";
      button.append(document.createElement("span"));
      button.addEventListener("click", () => { selectedGroup = group.key; rememberGroup(); render(); });
      groupButtons.set(group.key, button);
    }
    write(button.firstElementChild, group.key === ALL_GROUPS ? "All" : group.name);
    button.title = `${group.name} · ${group.count} 个节点`;
    const selected = group.key === selectedGroup;
    button.setAttribute("aria-pressed", String(selected)); button.classList.toggle("wsk-is-active", selected);
    button.setAttribute("aria-label", `${group.key === ALL_GROUPS ? "全部节点" : group.key === UNGROUPED ? "没有分组的节点" : `分组：${group.name}`}，${group.count} 个公开节点`);
    if (groupBar.children[index] !== button) groupBar.insertBefore(button, groupBar.children[index] ?? null);
  });
}
function syncView() {
  list.hidden = view !== "cards";
  table.hidden = view !== "table" || !current?.nodes.length;
  for (const button of viewButtons) {
    const selected = button.dataset.fmView === view;
    button.setAttribute("aria-pressed", String(selected)); button.classList.toggle("wsk-is-active", selected);
  }
}
function syncSearch(nodes) {
  const query = search.value.trim();
  byID("clear-search").hidden = !query;
  put("search-filter-label", `搜索：${query}`);
  searchToggle.dataset.active = String(Boolean(query));
  put("search-summary", `${selectedGroup === ALL_GROUPS ? "全部节点" : "当前分组"} · ${nodes.length} 个结果`);
  byID("search-empty").hidden = nodes.length > 0;
  const visible = new Set(nodes.map(node => node.id)), focused = searchResults.contains(document.activeElement) ? document.activeElement : null;
  for (const [id, item] of searchItems) if (!visible.has(id)) { item.element.remove(); searchItems.delete(id); }
  nodes.forEach((node, index) => {
    let item = searchItems.get(node.id);
    if (!item) {
      const element = document.createElement("li"), link = document.createElement("a"), name = document.createElement("span"), badge = document.createElement("span");
      link.className = "fm-search-result"; link.href = nodeURL(node.id);
      name.className = "fm-search-result-name"; badge.className = "wsk-badge fm-badge";
      link.append(name, badge); element.append(link);
      item = {element, link, name, badge}; searchItems.set(node.id, item);
    }
    write(item.name, node.name); item.name.title = node.name; write(item.badge, nodeBadgeText(node));
    item.badge.dataset.state = node.session; item.badge.title = sessionLabels[node.session];
    item.link.setAttribute("aria-label", `${node.name} · ${sessionLabels[node.session]}`);
    if (searchResults.children[index] !== item.element) searchResults.insertBefore(item.element, searchResults.children[index] ?? null);
  });
  if (focused?.isConnected && document.activeElement !== focused) focused.focus({preventScroll: true});
}
function openSearch() {
  if (!searchDialog.open) searchDialog.showModal();
  searchToggle.setAttribute("aria-expanded", "true");
  search.focus(); search.select();
}
function createCard(node) {
  const element = byID("node-template").content.firstElementChild.cloneNode(true);
  const row = byID("row-template").content.firstElementChild.cloneNode(true);
  const labels = new Map(), meters = new Map();
  for (const root of [element, row]) {
    for (const el of root.querySelectorAll("[data-value]")) labels.set(el.dataset.value, [...(labels.get(el.dataset.value) ?? []), el]);
    for (const el of root.querySelectorAll("[data-meter]")) meters.set(el.dataset.meter, [...(meters.get(el.dataset.meter) ?? []), el]);
    for (const link of root.querySelectorAll("a")) { const url = nodeURL(node.id); if (url) link.href = url; }
  }
  return {element, row, labels, meters};
}
function patchCard(card, node) {
  const text = (key, next) => { for (const el of card.labels.get(key) ?? []) write(el, next); };
  const state = (key, next) => { for (const el of card.labels.get(key) ?? []) el.dataset.state = next; };
  const metric = node.metrics ?? {};
  const meter = (key, n) => { for (const el of card.meters.get(key) ?? []) { el.hidden = n === null; if (n !== null && el.value !== n) el.value = n; } };
  text("name", node.name);
  const groups = groupText(node);
  text("groups", groups);
  for (const label of card.labels.get("groups")) label.hidden = !groups;
  for (const link of [card.element.querySelector(".fm-open-node"), card.row.lastElementChild.querySelector("a")]) link.setAttribute("aria-label", `${node.name} · 节点详情`);
  for (const [key, value] of [["hardware", hardwareText(node.hardware)], ["hardware-short", cardHardwareText(node.hardware)]]) {
    text(key, value);
    for (const label of card.labels.get(key) ?? []) label.title = value;
  }
  text("cpu-label", cpuLabel(node.hardware));
  card.element.dataset.state = node.session;
  text("session", nodeBadgeText(node)); state("session", node.session);
  for (const label of card.labels.get("session")) label.title = sessionLabels[node.session];
  text("freshness", freshnessLabels[node.freshness]); state("freshness", node.freshness);
  const cpu = percent(metric.cpu), mem = ratio(metric.mem_used, metric.mem_total), disk = ratio(metric.disk_used, metric.disk_total);
  text("cpu", percentage(cpu)); text("mem", percentage(mem)); text("disk", percentage(disk));
  meter("cpu", cpu); meter("mem", mem); meter("disk", disk);
  text("mem-label", `内存 ${bytes(decimal(metric.mem_total))}`);
  text("disk-label", `存储 ${bytes(decimal(metric.disk_total))}`);
  text("cpu-note", cpu === null ? quality(metric.cpu) : "CPU 使用率");
  text("mem-note", capacity(metric.mem_used, metric.mem_total));
  text("disk-note", capacity(metric.disk_used, metric.disk_total));
  text("rx", bytes(decimal(metric.net_rx), true)); text("tx", bytes(decimal(metric.net_tx), true));
  const plan = planText(node.traffic_plan);
  text("traffic-label", `流量 ${plan.quota}`); text("traffic", plan.percent); meter("traffic", plan.meter);
  card.element.querySelector('[data-value="traffic"]').title = "周期流量使用率";
  text("load", loadText(metric.load)); text("uptime", uptime(metric.uptime));
  text("sample-time", node.metrics_at ? `采样于 ${timeText(node.metrics_at)}${node.freshness === "stale" ? " · 已过期" : ""}` : "尚未收到资源报告");
  text("interval", Number.isFinite(node.interval_seconds) && node.interval_seconds > 0 ? `${node.interval_seconds} 秒 / 次` : UNKNOWN);
  for (const el of card.labels.get("freshness")) el.title = node.metrics_at ? `资源采样于 ${timeText(node.metrics_at)}` : "尚未收到资源报告";
  card.element.querySelector("[data-value=uptime]").title = "系统运行时间，不代表监控连续在线时长";
}
function render() {
  if (!current) return;
  const nodes = current.nodes, summary = overview(nodes);
  syncGroups(nodes);
  put("snapshot-at", timeText(current.generated_at)); byID("snapshot-at").dateTime = current.generated_at;
  put("stat-online", summary.online); put("stat-total", summary.total);
  put("stat-offline", summary.offline); put("stat-waiting", summary.waiting);
  byID("stat-waiting-note").hidden = summary.waiting === 0;
  put("stat-cpu", percentage(summary.cpu.value));
  byID("stat-cpu-note").title = `来自 ${summary.cpu.count} 个在线且新鲜的有效 CPU 样本`;
  put("stat-rx", bytes(summary.rx.value, true)); put("stat-tx", bytes(summary.tx.value, true));
  byID("stat-network-note").title = `仅汇总在线且新鲜的有效采样：发送 ${summary.tx.count} / 接收 ${summary.rx.count} 个节点`;
  put("fleet-rx", bytes(summary.rx.value, true)); put("fleet-tx", bytes(summary.tx.value, true));
  byID("fleet-network-rates").title = byID("stat-network-note").title;
  put("stat-rx-total", bytes(summary.rxTotal.value)); put("stat-tx-total", bytes(summary.txTotal.value));
  byID("fleet-network-totals").title = `系统累计流量（节点重启可能归零），含离线节点最后有效采样：上传 ${summary.txTotal.count} / 下载 ${summary.rxTotal.count} 个节点`;

  const existing = new Set(nodes.map(n => n.id));
  for (const [id, card] of cards) if (!existing.has(id)) {
    card.element.remove(); card.row.remove(); cards.delete(id);
  }
  for (const node of nodes) {
    if (!cards.has(node.id)) cards.set(node.id, createCard(node));
    patchCard(cards.get(node.id), node);
  }
  const ordered = sortNodes(nodes, sort.value), shown = select(ordered, search.value, selectedGroup), visible = new Set(shown.map(n => n.id));
  syncSearch(shown);
  for (const [id, card] of cards) {
    card.element.hidden = !visible.has(id); card.row.hidden = !visible.has(id);
  }
  // Stable card and row identities preserve keyboard focus across live snapshots.
  ordered.forEach((node, index) => {
    const card = cards.get(node.id);
    for (const [parent, element] of [[list, card.element], [rows, card.row]]) {
      const before = parent.children[index] ?? null;
      if (before !== element) parent.insertBefore(element, before);
    }
  });
  syncView();
  table.hidden = view !== "table" || !shown.length;
  byID("empty-state").hidden = shown.length !== 0;
  if (!nodes.length) { put("empty-title", "还没有监控节点"); put("empty-copy", "节点接入后，资源和连接状态会自动出现在这里。"); }
  else { put("empty-title", "没有匹配的节点"); put("empty-copy", "请清空名称搜索，或切换到其他分组查看节点。"); }
  put("list-summary", `显示 ${shown.length} / ${nodes.length} 个节点`);
}
function onState(next) {
  connectionStatus.update(next, current ? "浏览器实时连接已中断，正在自动重连。以下为最近一次快照，不代表节点的当前状态。" : "暂时无法连接监控服务，正在自动重试。尚未获取节点状态。");
  if (!current && next === "error") { byID("empty-state").hidden = false; put("empty-title", "暂时无法读取节点"); put("empty-copy", "监控服务恢复后页面会自动重新同步，也可以点击刷新重试。"); }
}
search.addEventListener("input", render);
searchToggle.addEventListener("click", openSearch);
searchDialog.addEventListener("close", () => { searchToggle.setAttribute("aria-expanded", "false"); searchToggle.focus(); });
byID("clear-search").addEventListener("click", () => { search.value = ""; render(); searchToggle.focus(); });
document.addEventListener("keydown", event => {
  if (!event.isComposing && (event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "k") { event.preventDefault(); openSearch(); }
});
searchDialog.addEventListener("keydown", event => {
  if (event.isComposing) return;
  if (event.key === "Escape") { event.preventDefault(); event.stopPropagation(); searchDialog.close(); return; }
  const links = [...searchResults.querySelectorAll("a")], index = links.indexOf(document.activeElement);
  if (["ArrowDown", "ArrowUp"].includes(event.key) && (event.target === search || index >= 0) && links.length) {
    event.preventDefault();
    const next = event.key === "ArrowDown" ? (index + 1) % links.length : (index < 0 ? links.length - 1 : index - 1);
    if (next < 0) search.focus(); else links[next].focus();
  } else if (event.key === "Enter" && event.target === search && links.length) {
    event.preventDefault(); links[0].click();
  }
});
sort.addEventListener("change", () => {
  try { sessionStorage.setItem(sortStorageKey, sort.value); } catch { /* Storage is optional. */ }
  render();
});
for (const button of viewButtons) button.addEventListener("click", () => {
  view = button.dataset.fmView;
  try { localStorage.setItem("frp-monitor-view", view); } catch { /* Storage is optional. */ }
  syncView(); render();
});
syncView();
const connection = connect({onSnapshot(data) { current = data; render(); }, onState});
byID("retry").addEventListener("click", () => connection.refresh());
window.addEventListener("pagehide", () => { connection.stop(); connectionStatus.stop(); });
window.addEventListener("pageshow", event => { if (event.persisted) window.location.reload(); });
