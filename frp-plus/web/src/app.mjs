import {UNKNOWN, bytes, decimal, percent, percentage, ratio, onlineUptime, timeText, sessionLabels, freshnessLabels} from "./format.mjs";
import {overview, groupOptions, resolveGroupSelection, ALL_GROUPS, UNGROUPED} from "./store.mjs";
import {homeSortOptions, normalizeHomeFilters, expiresSoon, filterHomeNodes, sortHomeNodes, homeRowDetails} from "./home-data.mjs";
import {connect} from "./transport.mjs";
import {createConnectionStatus} from "./connection-status.mjs";
import {planText, expiryText, dateTime} from "./node-settings.mjs";
import {nodeURL, cardHardwareText, cpuCores, groupText} from "./node-data.mjs";

const byID = id => document.getElementById(id);
const write = (element, text) => { const next = String(text ?? UNKNOWN); if (element.textContent !== next) element.textContent = next; };
const put = (id, text) => write(byID(id), text);
const list = byID("node-list"), cards = new Map();
const search = byID("node-search"), viewButtons = [...document.querySelectorAll("[data-fm-view]")];
const quickSearch = byID("node-quick-search"), statusButtons = [...document.querySelectorAll("[data-status-filter]")];
const expiryFilter = byID("expiry-filter"), filterStorageKey = "frp-monitor-public-filters";
const searchDialog = byID("node-search-dialog"), searchToggle = byID("search-toggle"), searchResults = byID("search-results"), searchItems = new Map();
const groupBar = byID("node-group-filter"), groupButtons = new Map(), groupStorageKey = "frp-monitor-public-group";
const sort = byID("node-sort"), sortStorageKey = "frp-monitor-public-sort";
sort.replaceChildren(...homeSortOptions.map(([value, name]) => {
  const option = document.createElement("option"); option.value = value; option.textContent = name; return option;
}));
const connectionStatus = createConnectionStatus({notice: byID("connection-notice")});
let current = null, view = "cards", selectedGroup = ALL_GROUPS;
let filters = normalizeHomeFilters(null);
try { if (localStorage.getItem("frp-monitor-view") === "table") view = "table"; } catch { /* Storage is optional. */ }
try { selectedGroup = sessionStorage.getItem(groupStorageKey) ?? ALL_GROUPS; } catch { /* Storage is optional. */ }
try { const saved = sessionStorage.getItem(sortStorageKey); if (homeSortOptions.some(([key]) => key === saved)) sort.value = saved; } catch { /* Storage is optional. */ }
try { filters = normalizeHomeFilters(JSON.parse(sessionStorage.getItem(filterStorageKey))); } catch { /* Storage is optional. */ }
function rememberFilters() {
  try { sessionStorage.setItem(filterStorageKey, JSON.stringify(filters)); } catch { /* Storage is optional. */ }
}
function syncFilters() {
  for (const button of statusButtons) button.setAttribute("aria-pressed", String(button.dataset.statusFilter === filters.status));
  expiryFilter.setAttribute("aria-pressed", String(filters.expiring));
  byID("clear-filters").hidden = selectedGroup === ALL_GROUPS && !search.value.trim() && filters.status === "all" && !filters.expiring;
}
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
    write(button.firstElementChild, group.key === ALL_GROUPS ? "全部" : group.name);
    button.title = `${group.name} · ${group.count} 个节点`;
    const selected = group.key === selectedGroup;
    button.setAttribute("aria-pressed", String(selected)); button.classList.toggle("wsk-is-active", selected);
    button.setAttribute("aria-label", `${group.key === ALL_GROUPS ? "全部节点" : group.key === UNGROUPED ? "没有分组的节点" : `分组：${group.name}`}，${group.count} 个公开节点`);
    if (groupBar.children[index] !== button) groupBar.insertBefore(button, groupBar.children[index] ?? null);
  });
}
function syncView() {
  list.dataset.fmLayout = view;
  list.setAttribute("aria-label", view === "table" ? "节点横向列表，可横向滚动" : "节点卡片列表");
  if (view === "table") list.tabIndex = 0;
  else list.removeAttribute("tabindex");
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
  put("search-summary", `${selectedGroup === ALL_GROUPS ? "全部节点" : "当前分组"}${filters.status === "all" ? "" : ` · ${sessionLabels[filters.status]}`}${filters.expiring ? " · 7 天内到期" : ""} · ${nodes.length} 个结果`);
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
    write(item.name, node.name); item.name.title = node.name; write(item.badge, sessionLabels[node.session]);
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
  const labels = new Map(), meters = new Map();
  for (const el of element.querySelectorAll("[data-value]")) labels.set(el.dataset.value, [...(labels.get(el.dataset.value) ?? []), el]);
  for (const el of element.querySelectorAll("[data-meter]")) meters.set(el.dataset.meter, [...(meters.get(el.dataset.meter) ?? []), el]);
  const links = [...element.querySelectorAll("a")], url = nodeURL(node.id);
  for (const link of links) if (url) link.href = url;
  return {element, labels, meters, links};
}
function patchCard(card, node, now) {
  const labels = key => card.labels.get(key) ?? [];
  const text = (key, next) => { for (const el of labels(key)) write(el, next); };
  const state = (key, next) => { for (const el of labels(key)) el.dataset.state = next; };
  const title = (key, next) => { for (const el of labels(key)) el.title = next; };
  const metric = node.metrics ?? {};
  const meter = (key, n) => { for (const el of card.meters.get(key) ?? []) {
    el.hidden = n === null;
    if (n === null) el.removeAttribute("value");
    else if (!el.hasAttribute("value") || el.value !== n) el.value = n;
  } };
  text("name", node.name);
  const groups = groupText(node);
  text("groups", groups);
  for (const label of labels("groups")) label.hidden = !groups;
  for (const link of card.links) link.setAttribute("aria-label", `${node.name} · 节点详情`);
  const hardware = cardHardwareText(node.hardware);
  text("hardware-short", hardware); title("hardware-short", hardware);
  const cores = cpuCores(node.hardware);
  text("cpu-capacity", cores === null ? UNKNOWN : `${cores} 核`);
  card.element.dataset.state = node.session;
  text("status", sessionLabels[node.session]); state("status", node.session);
  title("status", sessionLabels[node.session]);
  text("freshness", freshnessLabels[node.freshness]); state("freshness", node.freshness);
  const cpu = percent(metric.cpu), mem = ratio(metric.mem_used, metric.mem_total), disk = ratio(metric.disk_used, metric.disk_total);
  text("cpu", percentage(cpu)); text("mem", percentage(mem)); text("disk", percentage(disk));
  meter("cpu", cpu); meter("mem", mem); meter("disk", disk);
  text("mem-capacity", bytes(decimal(metric.mem_total)));
  text("disk-capacity", bytes(decimal(metric.disk_total)));
  text("rx", bytes(decimal(metric.net_rx), true)); text("tx", bytes(decimal(metric.net_tx), true));
  const plan = planText(node.traffic_plan);
  text("traffic-capacity", plan.quota); text("traffic", plan.percent); meter("traffic", plan.meter);
  const planTitle = node.traffic_plan ? `周期已用 ${plan.used} / ${plan.quota}` : "套餐未公开";
  for (const key of ["traffic", "traffic-capacity"]) title(key, planTitle);
  const expiry = expiryText(node.billing?.expires_at_ms, now);
  text("expiry", expiry);
  for (const label of labels("expiry")) {
    label.hidden = !expiry;
    label.title = expiry ? `到期时间：${dateTime(node.billing.expires_at_ms)}` : "";
    label.dataset.urgent = String(Boolean(expiry) && (node.billing.expires_at_ms <= now || expiresSoon(node, now)));
  }
  text("uptime", onlineUptime(metric.uptime, node.session)); state("uptime", node.session);
  title("freshness", node.metrics_at ? `资源采样于 ${timeText(node.metrics_at)}` : "尚未收到资源报告");
  title("uptime", "系统运行时间，不代表监控连续在线时长");
  const details = homeRowDetails(node, now);
  for (const [key, value] of Object.entries(details.values)) text(key, value);
  for (const [key, value] of Object.entries(details.titles)) title(key, value);
  for (const label of labels("row-expiry")) label.dataset.urgent = String(details.expiryUrgent);
}
function render() {
  if (!current) return;
  const nodes = current.nodes, summary = overview(nodes), now = Date.now();
  syncGroups(nodes);
  syncFilters();
  put("stat-online", summary.online); put("stat-total", summary.total);
  put("stat-offline", summary.offline); put("stat-waiting", summary.waiting);
  put("stat-expiring", nodes.filter(node => expiresSoon(node, now)).length);
  put("fleet-rx", bytes(summary.rx.value, true)); put("fleet-tx", bytes(summary.tx.value, true));
  byID("fleet-network-rates").title = `仅汇总在线且新鲜的有效采样：发送 ${summary.tx.count} / 接收 ${summary.rx.count} 个节点`;
  put("stat-rx-total", bytes(summary.rxTotal.value)); put("stat-tx-total", bytes(summary.txTotal.value));
  byID("fleet-network-totals").title = `系统累计流量（节点重启可能归零），含离线节点最后有效采样：上传 ${summary.txTotal.count} / 下载 ${summary.rxTotal.count} 个节点`;

  const focused = list.contains(document.activeElement) ? document.activeElement : null;
  const existing = new Set(nodes.map(n => n.id));
  for (const [id, card] of cards) if (!existing.has(id)) {
    card.element.remove(); cards.delete(id);
  }
  for (const node of nodes) {
    if (!cards.has(node.id)) cards.set(node.id, createCard(node));
    patchCard(cards.get(node.id), node, now);
  }
  const ordered = sortHomeNodes(nodes, sort.value), shown = filterHomeNodes(ordered, {query: search.value, group: selectedGroup, ...filters}, now), visible = new Set(shown.map(n => n.id));
  syncSearch(shown);
  for (const [id, card] of cards) card.element.hidden = !visible.has(id);
  // Both layouts share stable elements; restore focus when sorting moves a card.
  ordered.forEach((node, index) => {
    const element = cards.get(node.id).element, before = list.children[index] ?? null;
    if (before !== element) list.insertBefore(element, before);
  });
  if (focused?.isConnected && !focused.closest(".fm-node-card")?.hidden && document.activeElement !== focused) focused.focus({preventScroll: true});
  syncView();
  byID("empty-state").hidden = shown.length !== 0;
  if (!nodes.length) { put("empty-title", "还没有监控节点"); put("empty-copy", "节点接入后，资源和连接状态会自动出现在这里。"); }
  else { put("empty-title", "没有匹配的节点"); put("empty-copy", "请调整名称、分组、状态或到期筛选，也可以重置筛选查看全部节点。"); }
  put("list-summary", `显示 ${shown.length} / ${nodes.length} 个节点${filters.status === "all" ? "" : ` · ${sessionLabels[filters.status]}`}${filters.expiring ? " · 7 天内到期" : ""}`);
}
function onState(next) {
  connectionStatus.update(next, current ? "浏览器实时连接已中断，正在自动重连。以下为最近一次快照，不代表节点的当前状态。" : "暂时无法连接监控服务，正在自动重试。尚未获取节点状态。");
  if (!current && next === "error") { byID("empty-state").hidden = false; put("empty-title", "暂时无法读取节点"); put("empty-copy", "监控服务恢复后页面会自动重新同步，也可以点击刷新重试。"); }
}
search.addEventListener("input", () => { quickSearch.value = search.value; render(); });
quickSearch.addEventListener("input", () => { search.value = quickSearch.value; render(); });
searchToggle.addEventListener("click", openSearch);
searchDialog.addEventListener("close", () => { searchToggle.setAttribute("aria-expanded", "false"); searchToggle.focus(); });
byID("clear-search").addEventListener("click", () => { search.value = ""; quickSearch.value = ""; render(); quickSearch.focus(); });
for (const button of statusButtons) button.addEventListener("click", () => {
  filters.status = button.dataset.statusFilter; rememberFilters(); syncFilters(); render();
});
expiryFilter.addEventListener("click", () => {
  filters.expiring = !filters.expiring; rememberFilters(); syncFilters(); render();
});
byID("clear-filters").addEventListener("click", () => {
  search.value = ""; quickSearch.value = ""; selectedGroup = ALL_GROUPS; filters = normalizeHomeFilters(null);
  rememberGroup(); rememberFilters(); syncFilters(); render(); quickSearch.focus();
});
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
syncView(); syncFilters();
const connection = connect({onSnapshot(data) { current = data; render(); }, onState});
byID("retry").addEventListener("click", () => connection.refresh());
window.addEventListener("pagehide", () => connection.stop());
window.addEventListener("pageshow", event => { if (event.persisted) window.location.reload(); });
