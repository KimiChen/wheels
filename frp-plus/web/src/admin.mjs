import {adminClient, adminSnapshot, groupDocument, groupDraft, groupDraftState, groupRequest, probeDocument, nextProbeDocument, errorText, fieldText, byteText, reconciliationLabels, proxyLabels, clientProxyLabels} from "./admin-data.mjs";
import {settingsRequest, priceInput, gibInput, localDateInput, todayText, planText, dateTime} from "./node-settings.mjs";
import {sessionLabels, freshnessLabels, frpLabel, bytes, decimal, percent, percentage, capacity, timeText} from "./format.mjs";
import {overview, filterAdminNodes, expiryCalendar, localDayKey} from "./admin-overview.mjs";
import {directoryRow} from "./admin-directory.mjs";
import {membershipDraft, membershipChanges, saveNodeChanges, retainAcknowledgedSettings} from "./admin-editor.mjs";
import {renderFRPDetail, clearFRPDetail, serverEndpointText, createFRPDetailReader, renderNativeRegistryAccess} from "./admin-frp.mjs";
import {createConfigPanel} from "./admin-configuration.mjs";
import {createAuditPanel} from "./admin-audit.mjs";
import {createTunnelPanel} from "./admin-tunnels.mjs";

const $ = id => document.getElementById(id);
const client = adminClient({onExpired: () => locked("管理会话已失效，请重新登录。")});
let loggedIn = false, busy = false, snapshot = null, selectedID = null, poll = null, probes = null, probeEpoch = 0, pendingAction = null, refreshing = false, snapshotEpoch = 0, settingsRevision = null, settingsDirty = false;
const cards = new Map();
let groups = [], groupsReady = false, groupsEpoch = 0, draft = groupDraft(), groupDirty = false, groupMessage = "", pendingGroupDelete = null;
const groupRows = new Map(), groupMembers = new Map();
let calendarMonth = new Date(new Date().getFullYear(), new Date().getMonth(), 1), calendarDay = null;
let editorMembership = null, settingsNeedsReload = false, editorSettingsSaved = false, bindingBaseline = null, bindingDirty = false;
const acknowledgedSettings = new Map();
const frpDetails = createFRPDetailReader({request: path => client.request(path), onChange: () => renderSelectedFRPDetail()});
const configuration = createConfigPanel($("node-configuration"), {request: (path, options) => client.request(path, options)});
const audit = createAuditPanel($("audit-panel"), {request: path => client.request(path), download: filters => client.downloadAudit(filters)});
const tunnels = createTunnelPanel($("tunnels-panel"), {request:path=>client.request(path),onAudit:operationID=>{audit.showOperation(operationID);location.hash="audit";}});
const el = (tag, text, className) => { const node = document.createElement(tag); if (text !== undefined) node.textContent = text; if (className) node.className = className; return node; };
const safe = raw => typeof raw === "string" || (typeof raw === "number" && Number.isFinite(raw)) ? String(raw) : "—";
function notice(text, state = "ready") {
  for (const box of [$("notice"), ...document.querySelectorAll("[data-dialog-notice]")]) { box.hidden = true; box.textContent = ""; }
  const dialog = [...document.querySelectorAll("dialog[open]")].at(-1);
  const box = dialog?.querySelector("[data-dialog-notice]") ?? $("notice");
  box.textContent = text; box.dataset.state = state; box.hidden = !text;
}
function openDialog(id) { const dialog = $(id); dialog.hidden = false; if (!dialog.open) dialog.showModal(); }
function clearSecret() { $("secret-id").value = ""; $("secret-token").value = ""; $("credential-result").close(); $("credential-result").hidden = true; }
function showSecret(data) {
  if (!data || typeof data.id !== "string" || typeof data.token !== "string" || data.token.length < 32 || data.token.length > 512) throw new Error("invalid_credential");
  closeNodeEditor();
  $("secret-id").value = data.id; $("secret-token").value = data.token; openDialog("credential-result");
  $("secret-token").focus(); $("secret-token").select();
}
function locked(message = "请使用 GitHub 登录。", disabled = false) {
  for (const dialog of document.querySelectorAll("dialog[open]")) dialog.close();
  client.clear(); loggedIn = false; refreshing = false; clearTimeout(poll); probeEpoch++;
  snapshotEpoch++; snapshot = null; selectedID = null; probes = null; pendingAction = null;
  frpDetails.clear();
  configuration.clear();
  audit.clear();
  tunnels.clear();
  groupsEpoch++; groups = []; groupsReady = false; draft = groupDraft(); groupDirty = false; groupMessage = ""; pendingGroupDelete = null;
  groupRows.clear(); groupMembers.clear(); $("group-list").replaceChildren(); $("group-members").replaceChildren(); $("group-form").reset(); $("group-confirm").hidden = true;
  $("group-editor-title").textContent = "新建分组"; $("group-save").textContent = "创建分组";
  for (const id of ["group-count", "group-member-count", "group-status", "group-confirm-copy"]) $(id).textContent = "";
  clearSecret(); cards.clear(); $("node-list").replaceChildren(); $("node-details").replaceChildren(); $("server-registry").replaceChildren(); $("probe-rows").replaceChildren();
  clearFRPDetail($("node-frp-details"));
  $("binding-form").reset(); $("settings-form").reset(); settingsRevision = null; settingsDirty = false; $("create-form").reset(); $("node-search").value = "";
  editorMembership = null; settingsNeedsReload = false; editorSettingsSaved = false; bindingBaseline = null; bindingDirty = false; acknowledgedSettings.clear();
  $("node-editor-groups").replaceChildren(); $("node-editor-groups-status").textContent = ""; $("node-editor-groups-status").hidden = true;
  $("node-filter").value = "all"; $("node-group").replaceChildren(new Option("全部分组", "all"), new Option("未分组", "ungrouped"));
  syncDirectoryFilters();
  for (const id of ["attention-list", "calendar-days", "calendar-events"]) $(id).replaceChildren();
  for (const count of document.querySelectorAll("[data-overview-count]")) count.textContent = "—";
  for (const id of ["nav-node-count", "node-filter-count", "overview-updated", "attention-count", "expiry-task-copy", "review-task-copy", "calendar-caption", "calendar-month"]) $(id).textContent = "";
  calendarDay = null;
  $("expired-shortcut").hidden = true; $("expired-shortcut").textContent = "已到期";
  $("node-name-label").textContent = ""; $("node-id").textContent = ""; $("node-session").textContent = ""; $("binding-summary").textContent = "—";
  for (const id of ["settings-current", "settings-status", "snapshot-at", "node-count", "confirm-copy"]) $(id).textContent = "";
  $("node-panel").hidden = true; $("confirm-action").hidden = true; $("workspace").hidden = true; $("logout").hidden = true; $("login-panel").hidden = false; $("github-login").hidden = disabled;
  notice(message, disabled ? "error" : "ready");
}
async function unlocked() {
  loggedIn = true; $("login-panel").hidden = true; $("workspace").hidden = false; $("logout").hidden = false;
  notice("已登录，正在获取私有快照…");
  await refreshNodes(); if (loggedIn) await loadProbes();
  if (loggedIn) showView();
}
async function refreshNodes(force = false) {
  if (!loggedIn || (refreshing && !force)) return;
  const generation = ++snapshotEpoch, groupGeneration = groupsEpoch;
  refreshing = true; clearTimeout(poll);
  try {
    const next = retainAcknowledgedSettings(adminSnapshot(await client.request("/api/admin/v1/nodes")), acknowledgedSettings);
    if (!loggedIn || generation !== snapshotEpoch) return;
    snapshot = next; renderNodes(); renderRegistry();
    if (selectedID && !$("editor-resources").hidden) void refreshSelectedFRPDetail();
    if (groupGeneration === groupsEpoch) { groups = next.groups; groupsReady = next.groups_state === "ready"; }
    renderGroups();
    $("probe-health").hidden = next.probes_state !== "degraded";
    if (next.credentials_state === "degraded") notice("节点配置暂不可用，请检查服务端后再操作。", "error");
    else if (!document.querySelector("dialog[open]")) notice("");
    return next;
  } catch (error) { if (loggedIn && generation === snapshotEpoch) notice(`${errorText(error)}${snapshot ? " 当前显示上次成功快照。" : " 尚无可用快照。"}`, "error"); }
  finally { if (generation === snapshotEpoch) { refreshing = false; if (loggedIn) poll = setTimeout(refreshNodes, 10000); } }
}
function rowList(items) {
  const dl = el("dl", undefined, "fa-detail-list");
  for (const [label, text] of items) { const row = el("div"); row.append(el("dt", label), el("dd", safe(text))); dl.append(row); }
  return dl;
}
function table(headers, rows) {
  if (!rows.length) return el("p", "暂无登记记录。", "fa-muted");
  const wrap = el("div", undefined, "wsk-table-wrap fa-table-wrap"), table = el("table", undefined, "wsk-table fa-table"), thead = el("thead"), head = el("tr"), body = el("tbody");
  for (const title of headers) { const th = el("th", title); th.scope = "col"; head.append(th); }
  thead.append(head);
  for (const cells of rows) {
    const row = el("tr");
    cells.forEach((cell, index) => { const item = el(index === 0 ? "th" : "td", safe(cell)); if (index === 0) item.scope = "row"; row.append(item); });
    body.append(row);
  }
  table.append(thead, body); wrap.append(table); return wrap;
}
function renderNodes() {
  const nodes = snapshot.nodes, known = new Set(nodes.map(node => node.id));
  for (const [id, card] of cards) if (!known.has(id)) { card.remove(); cards.delete(id); }
  for (const node of nodes) {
    let row = cards.get(node.id);
    if (!row) {
      row = $("admin-node-row").content.firstElementChild.cloneNode(true);
      for (const button of row.querySelectorAll("[data-node-action]")) button.addEventListener("click", () => selectNode(node.id, button.dataset.nodeAction));
      cards.set(node.id, row); $("node-list").append(row);
    }
    const put = (key, value) => { row.querySelector(`[data-cell="${key}"]`).textContent = value; };
    const data = directoryRow(node, snapshot.groups);
    put("name", node.name); put("summary", data.summary);
    const groupCell = row.querySelector('[data-cell="groups"]');
    groupCell.replaceChildren(...(data.groups.length ? data.groups.map(name => el("span", name, "wsk-badge")) : [el("span", "未分组", "fa-muted")]));
    for (const family of ["ipv4", "ipv6"]) put(family, data[family]);
    put("session", data.session);
    const session = row.querySelector('[data-cell="session"]'); session.dataset.state = node.session;
    session.title = `${sessionLabels[node.session]} · ${freshnessLabels[node.freshness]}`;
    put("plan", data.plan.text); row.querySelector('[data-cell="plan"]').title = data.plan.title;
    const meter = row.querySelector("[data-plan-meter]");
    meter.max = 100; meter.hidden = data.plan.meter === null; meter.title = data.plan.title;
    meter.setAttribute("aria-label", `${node.name} · 套餐用量`);
    if (data.plan.meter === null) { meter.removeAttribute("value"); meter.removeAttribute("aria-valuetext"); }
    else { meter.value = data.plan.meter; meter.setAttribute("aria-valuetext", data.plan.text); }
    put("expires", data.expires.text);
    const expires = row.querySelector('[data-cell="expires"]'); expires.title = data.expires.title; expires.dataset.state = data.expires.state;
    for (const button of row.querySelectorAll("button")) {
      button.disabled = busy;
      if (button.title) button.setAttribute("aria-label", `${node.name} · ${button.title}`);
    }
  }
  $("node-count").textContent = nodes.length;
  $("nav-node-count").textContent = nodes.length;
  $("snapshot-at").textContent = `更新于 ${timeText(snapshot.generated_at)} · 每 10 秒同步`;
  if (selectedID && !known.has(selectedID)) selectNode(null); else renderDetail();
  renderNodeFilters(); filterNodes(); renderOverview();
}
function renderNodeFilters() {
  const select = $("node-group"), selected = select.value;
  const options = [["all", "全部分组"], ["ungrouped", "未分组"], ...snapshot.groups.map(group => [group.id, group.name])];
  if (JSON.stringify([...select.options].map(option => [option.value, option.text])) !== JSON.stringify(options)) {
    select.replaceChildren(...options.map(([value, text]) => new Option(text, value)));
    select.value = options.some(([value]) => value === selected) ? selected : "all";
  }
}
function filterNodes() {
  const visible = new Set(filterAdminNodes(snapshot, {query: $("node-search").value, state: $("node-filter").value, group: $("node-group").value}).map(node => node.id)), count = visible.size;
  for (const [id, row] of cards) row.hidden = !visible.has(id);
  $("node-filter-count").textContent = `${count} / ${snapshot?.nodes.length ?? 0} 台节点`;
  syncDirectoryFilters();
  $("nodes-empty").hidden = count > 0;
  $("nodes-empty").textContent = $("node-filter").value === "review" && snapshot?.frp?.state !== "ready" ? "服务端快照暂不可用，请前往 FRP 对账页查看状态。" : snapshot?.nodes.length ? "没有匹配的节点，请调整状态、分组或搜索条件。" : "尚无节点，创建凭据后即可接入。";
}
function syncDirectoryFilters() {
  const current = $("node-filter").value;
  for (const button of document.querySelectorAll("[data-directory-filter]")) {
    const selected = button.dataset.directoryFilter === current;
    button.setAttribute("aria-pressed", String(selected));
    button.hidden = ["expired", "review"].includes(button.dataset.directoryFilter) && !selected;
  }
}
function openNodeFilter(state) {
  $("node-filter").value = state; $("node-group").value = "all"; $("node-search").value = "";
  filterNodes(); location.hash = "nodes"; showView();
}
function replaceList(box, children) {
  const focusKey = box.contains(document.activeElement) ? document.activeElement.dataset.focusKey : null;
  const scrollTop = box.scrollTop;
  box.replaceChildren(...children); box.scrollTop = scrollTop;
  if (focusKey) [...box.querySelectorAll("[data-focus-key]")].find(button => button.dataset.focusKey === focusKey)?.focus({preventScroll: true});
}
function nodeButton(node, pane = "settings", text = node.name, className = "fa-name") {
  const button = el("button", text, className); button.type = "button"; button.disabled = busy;
  button.dataset.focusKey = `${node.id}-${pane}`;
  button.addEventListener("click", () => selectNode(node.id, pane)); return button;
}
function renderOverview() {
  if (!snapshot) return;
  const summary = overview(snapshot), {counts} = summary;
  for (const count of document.querySelectorAll("[data-overview-count]")) count.textContent = counts[count.dataset.overviewCount] ?? "—";
  $("overview-updated").textContent = `更新于 ${timeText(snapshot.generated_at)} · 每 10 秒同步`;
  $("expiry-task-copy").textContent = counts.expired ? `另有 ${counts.expired} 个已到期，见下方需关注列表` : "检查续费时间";
  $("expired-shortcut").hidden = counts.expired === 0;
  $("expired-shortcut").textContent = `已到期 ${counts.expired}`;
  $("review-task-copy").textContent = summary.reviewAvailable ? "检查绑定与服务端登记" : "服务端快照暂不可用，查看对账页";
  $("attention-count").textContent = `${summary.attention.length} 个节点`;
  const labels = {expired: "已到期", due: "7 天内到期", offline: "监控离线", review: "FRP 需核对"};
  replaceList($("attention-list"), summary.attention.map(({node, reasons, record}) => {
    const row = el("div", undefined, "fa-attention-row"), content = el("div", undefined, "fa-attention-summary");
    content.append(nodeButton(node, "resources"));
    const details = [];
    if (reasons.includes("expired") || reasons.includes("due")) details.push(`${dateTime(node.settings.expires_at_ms)} 到期`);
    if (reasons.includes("review")) details.push(record?.state === "matched" ? "检查 FRP 连接与隧道" : reconciliationLabels[record?.state] ?? "等待核对");
    if (!details.length) details.push(`最近活动 ${node.last_seen ? new Date(node.last_seen).toLocaleString("zh-CN", {hour12: false}) : "尚无报告"}`);
    content.append(el("small", details.join(" · ")));
    const badges = el("div", undefined, "fa-attention-reasons");
    for (const reason of reasons) { const badge = el("span", labels[reason], "wsk-badge"); badge.dataset.reason = reason; badges.append(badge); }
    content.append(badges); row.append(content, nodeButton(node, reasons.length === 1 && reasons[0] === "review" ? "binding" : "settings", "查看", "wsk-button wsk-secondary fa-small-button"));
    return row;
  }));
  $("attention-empty").hidden = summary.attention.length > 0;
  $("attention-empty").textContent = counts.total === 0 ? "添加首个节点，开始查看运行状态与续费时间。" : summary.reviewAvailable ? "当前没有离线、临近到期或需要核对的节点。" : "暂无离线或临近到期节点；FRP 状态仍待确认。";
  renderCalendar();
}
function renderCalendar() {
  if (!snapshot) return;
  const year = calendarMonth.getFullYear(), month = calendarMonth.getMonth(), calendar = expiryCalendar(snapshot.nodes, year, month);
  const today = localDayKey(Date.now()), cells = [];
  $("calendar-month").textContent = `${year} 年 ${month + 1} 月`;
  for (let index = 0; index < calendar.offset; index++) cells.push(el("span"));
  for (const day of calendar.days) {
    const button = el("button", day.day, "fa-calendar-day"); button.type = "button"; button.disabled = busy;
    button.dataset.events = String(day.nodes.length > 0); button.dataset.focusKey = day.key;
    button.setAttribute("aria-label", `${year} 年 ${month + 1} 月 ${day.day} 日，${day.nodes.length} 个节点到期`);
    button.setAttribute("aria-pressed", String(calendarDay === day.key));
    if (day.key === today) button.setAttribute("aria-current", "date");
    button.addEventListener("click", () => { calendarDay = calendarDay === day.key ? null : day.key; renderCalendar(); }); cells.push(button);
  }
  replaceList($("calendar-days"), cells);
  const days = calendarDay ? calendar.days.filter(day => day.key === calendarDay) : calendar.days;
  const nodes = days.flatMap(day => day.nodes);
  $("calendar-caption").textContent = `${calendarDay ? `${Number(calendarDay.slice(-2))} 日` : "本月"} ${nodes.length} 个节点到期 · 浏览器时区`;
  replaceList($("calendar-events"), nodes.length ? nodes.map(node => {
    const row = el("div", undefined, "fa-calendar-event");
    row.append(nodeButton(node), el("small", new Date(node.settings.expires_at_ms).toLocaleString("zh-CN", {month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit", hour12: false}))); return row;
  }) : [el("p", calendarDay ? "这一天没有已设置的到期时间。" : "本月没有已设置的到期时间。", "fa-muted")]);
}
function selectNode(id, pane = "settings") {
  const changed = selectedID !== id;
  if (changed) { frpDetails.clear(); configuration.clear(); }
  selectedID = id; pendingAction = null; $("confirm-action").hidden = true;
  const node = snapshot?.nodes.find(item => item.id === id);
  $("node-panel").hidden = !node;
  if (!node) { closeNodeEditor(); return; }
  if (changed || !settingsDirty) loadSettings(node);
  if (changed || !bindingDirty) loadBinding(node);
  renderDetail(); showEditorPane(pane); openDialog("node-panel");
}
function discardNodeEditor() {
  frpDetails.clear();
  configuration.clear();
  selectedID = null; pendingAction = null; editorMembership = null; settingsRevision = null; settingsDirty = false;
  settingsNeedsReload = false; editorSettingsSaved = false; bindingBaseline = null; bindingDirty = false;
  $("settings-form").reset(); $("binding-form").reset(); $("node-editor-groups").replaceChildren(); $("node-details").replaceChildren();
  clearFRPDetail($("node-frp-details"));
  for (const id of ["node-name-label", "node-id", "node-session", "settings-current", "settings-status", "node-editor-groups-status", "confirm-copy"]) $(id).textContent = "";
  $("node-editor-groups-status").hidden = true; $("binding-summary").textContent = "—"; $("confirm-action").hidden = true;
  $("node-panel").hidden = true;
  for (const details of $("node-panel").querySelectorAll("details")) details.open = false;
}
function closeNodeEditor() { $("node-panel").close(); discardNodeEditor(); }
function loadBinding(node) {
  bindingBaseline = Object.fromEntries(["server_id", "user", "raw_client_id"].map(key => [key, node.frp_binding?.[key] ?? ""]));
  bindingDirty = false;
  for (const [key, value] of Object.entries(bindingBaseline)) $("binding-form").elements[key].value = value;
}
function showEditorPane(pane) {
  const settingsPane = pane === "settings" || pane === "plan";
  for (const panel of document.querySelectorAll("[data-editor-panel]")) panel.hidden = panel.dataset.editorPanel !== (settingsPane ? "settings" : pane);
  for (const fieldset of document.querySelectorAll("[data-settings-pane]")) fieldset.hidden = fieldset.dataset.settingsPane !== pane;
  for (const button of document.querySelectorAll("[data-editor-pane]")) {
    const selected = button.dataset.editorPane === pane;
    button.setAttribute("aria-pressed", String(selected)); button.classList.toggle("wsk-is-active", selected);
  }
  $("editor-save").hidden = !settingsPane && pane !== "binding";
  $("editor-save").setAttribute("form", pane === "binding" ? "binding-form" : "settings-form");
  $("editor-save").textContent = "保存修改";
  if (pane === "resources") void refreshSelectedFRPDetail();
  if (pane === "configuration") {
    const node = snapshot?.nodes.find(item => item.id === selectedID);
    if (loggedIn && node) void configuration.open(node.id, node.session === "online");
  }
}

async function refreshSelectedFRPDetail() {
  if (!loggedIn || !selectedID || $("editor-resources").hidden) return;
  const id = selectedID, generation = snapshotEpoch;
  await frpDetails.load(id, () => loggedIn && selectedID === id && snapshotEpoch === generation && $("node-panel").open && !$("editor-resources").hidden);
}
function renderSelectedFRPDetail() {
  const node = snapshot?.nodes.find(item => item.id === selectedID); if (!loggedIn || !node) return;
  const rec = snapshot.frp?.nodes?.find(item => item.id === node.id);
  renderFRPDetail($("node-frp-details"), frpDetails.view(node), rec, snapshot.native_access);
}
function showView() {
  const key = ["dashboard", "nodes", "groups", "probes", "frp", "audit", "tunnels", "access"].includes(location.hash.slice(1)) ? location.hash.slice(1) : "dashboard";
  for (const panel of document.querySelectorAll("[data-admin-panel]")) panel.hidden = panel.dataset.adminPanel !== key;
  for (const link of document.querySelectorAll("[data-admin-view]")) {
    const selected = link.dataset.adminView === key;
    if (selected) link.setAttribute("aria-current", "page"); else link.removeAttribute("aria-current");
    link.classList.toggle("wsk-active", selected);
  }
  if (loggedIn && key === "audit") audit.open();
  if (loggedIn && key === "tunnels") tunnels.open();
}
function renderDetail() {
  const node = snapshot?.nodes.find(item => item.id === selectedID); if (!node) return;
  if (configuration.state.nodeID === node.id) configuration.setOnline(node.session === "online");
  $("node-name-label").textContent = node.name; $("node-id").textContent = node.id;
  $("node-session").textContent = sessionLabels[node.session]; $("node-session").dataset.state = node.session;
  const today = todayText(node.traffic_today), plan = planText(node.traffic_plan);
  $("settings-current").textContent = `${plan.used} / ${plan.quota} · ${plan.note}`;
  if (node.settings?.config_revision !== settingsRevision) $("settings-status").textContent = "设置已被其他操作更新，请重新读取后保存。";
  const box = $("node-details"); box.replaceChildren();
  box.append(rowList([["今日接收 / 发送", `${today.rx} / ${today.tx}`], ["今日统计范围", today.note], ["套餐已用 / 额度", `${plan.used} / ${plan.quota}`], ["套餐计费", plan.note]]));
  box.append(rowList([["资源新鲜度", freshnessLabels[node.freshness]], ["最近资源报告", timeText(node.metrics_at)], ["监控最近活动", timeText(node.last_seen)], ["FRP 控制连接 · 节点报告", frpLabel(node.frp?.control_state)]]));
  box.append(el("h3", "主机资源"));
  if (node.metrics) box.append(rowList([["CPU 使用率", percentage(percent(node.metrics.cpu))], ["内存用量", capacity(node.metrics.mem_used, node.metrics.mem_total)], ["硬盘用量", capacity(node.metrics.disk_used, node.metrics.disk_total)], ["主机接收速率", bytes(decimal(node.metrics.net_rx), true)], ["主机发送速率", bytes(decimal(node.metrics.net_tx), true)]]));
  else box.append(el("p", "尚未收到主机资源报告。", "fa-muted"));
  if (node.facts) box.append(rowList([["主机名", fieldText(node.facts.hostname)], ["系统 / 架构", `${fieldText(node.facts.os)} / ${fieldText(node.facts.arch)}`], ["内核", fieldText(node.facts.kernel)], ["CPU", fieldText(node.facts.cpu_name)], ["核心数", fieldText(node.facts.cpu_cores)], ["IPv4", fieldText(node.facts.ipv4)], ["IPv6", fieldText(node.facts.ipv6)], ["虚拟化", fieldText(node.facts.virt)], ["Agent 版本", fieldText(node.facts.agent_version)]]));
  else box.append(el("p", "主机详情将在节点首次报告后显示。", "fa-muted"));
  const section = el("section", undefined, "fa-section"); section.append(el("h3", "FRP 隧道对账"));
  const rec = snapshot.frp?.nodes?.find(item => item.id === node.id);
  $("binding-summary").textContent = reconciliationLabels[rec?.state] ?? "等待服务端快照";
  section.append(rowList([["核对结果", reconciliationLabels[rec?.state] ?? "等待服务端快照"], ["服务端控制连接", rec?.server_online === true ? "在线" : rec?.server_online === false ? "离线" : "未知"], ["可信绑定", node.frp_binding ? `${node.frp_binding.server_id} / ${node.frp_binding.user || "（空用户）"} / ${node.frp_binding.raw_client_id}` : "未设置"], ["节点自报关联", node.frp ? `${node.frp.association?.server_id ?? "—"} / ${node.frp.association?.user || "（空用户）"} / ${node.frp.association?.raw_client_id ?? "无稳定 ID"}` : "尚未报告"]]));
  section.append(el("p", "FRP 流量按服务端本地日统计。接收为公网访客 → frpc，发送为 frpc → 公网访客；与上方主机网卡速率和流量分别计算。", "fa-detail-note"));
  if (rec && Array.isArray(rec.proxies)) {
    section.append(table(["隧道 / 类型", "本地目标", "节点报告", "服务端核对", "连接数", "FRP 今日接收 / 发送"], rec.proxies.map(proxy => [`${proxy.name} / ${proxy.type}`, proxy.local_target ?? "—", proxy.enabled ? clientProxyLabels[proxy.client_status] ?? "未知" : "未启用", proxyLabels[proxy.server_state] ?? "未知", proxy.server?.connections ?? "—", `${byteText(proxy.server?.today_rx_bytes)} / ${byteText(proxy.server?.today_tx_bytes)}`])));
  } else if (node.frp?.proxies?.length) section.append(table(["隧道 / 类型", "本地目标", "节点报告"], node.frp.proxies.map(proxy => [`${proxy.name} / ${proxy.type}`, proxy.local_target ?? "—", proxy.enabled ? clientProxyLabels[proxy.status] ?? "未知" : "未启用"])));
  else section.append(el("p", "暂无隧道报告。无隧道不代表 FRP 控制连接离线。", "fa-muted"));
  box.append(section);
  renderSelectedFRPDetail();
}
function renderRegistry() {
  const box = $("server-registry"), frp = snapshot.frp;
  const active = box.contains(document.activeElement) ? document.activeElement.dataset.frpFocus : null;
  box.replaceChildren();
  if (!frp) { box.append(el("p", "等待服务端注册表。", "fa-muted")); return; }
  box.append(rowList([["状态", reconciliationLabels[frp.state] ?? safe(frp.state)], ["服务端", frp.server_id], ["快照时间", timeText(frp.generated_at)]]));
  box.append(renderNativeRegistryAccess(document, frp, snapshot.native_access));
  const name = id => snapshot.nodes.find(node => node.id === id)?.name ?? (id ? id : "未绑定监控节点");
  box.append(el("h3", "客户端"), table(["用户 / 稳定 ID", "主机 / 地址", "FRP 版本 / wire", "连接", "监控节点"], (frp.clients ?? []).map(c => [`${c.user || "（空用户）"} / ${c.raw_client_id ?? "无稳定 ID"}`, `${c.hostname || "—"} / ${c.ip || "—"}`, `${c.version || "—"} / ${["v1", "v2"].includes(c.wire_protocol) ? c.wire_protocol : "未知"}`, c.online ? "在线" : "离线", name(c.agent_id)])));
  box.append(el("h3", "隧道"), el("p", "FRP 服务端本地日流量：接收为公网访客 → frpc；发送为 frpc → 公网访客。实际入口为服务端运行观察，监听地址不代表公网可达。", "fa-muted"), table(["隧道 / 类型", "实际入口", "连接", "连接数", "今日接收 / 发送", "监控节点"], (frp.proxies ?? []).map(p => [`${p.name} / ${p.type}`, serverEndpointText(p, frp.state), p.online ? "在线" : "离线", p.connections ?? "—", `${byteText(p.today_rx_bytes)} / ${byteText(p.today_tx_bytes)}`, name(p.agent_id)])));
  if (active) [...box.querySelectorAll("[data-frp-focus]")].find(item => item.dataset.frpFocus === active)?.focus({preventScroll: true});
}
function setBusy(value) {
  busy = value;
  for (const node of document.querySelectorAll("#workspace button, #workspace input, #workspace select, #workspace textarea, #github-login")) node.disabled = value;
  $("logout").disabled = value;
  updateGroupControls();
}
async function mutation(action, success) {
  if (busy) return; setBusy(true); clearTimeout(poll);
  try { await action(); if (loggedIn && success) notice(success); }
  catch (error) { if (loggedIn) notice(error.userMessage || errorText(error), "error"); }
  finally { setBusy(false); clearTimeout(poll); if (loggedIn) poll = setTimeout(refreshNodes, 10000); }
}
function renderGroups() {
  const known = new Set(groups.map(group => group.id));
  for (const [id, row] of groupRows) if (!known.has(id)) { row.remove(); groupRows.delete(id); }
  for (const group of groups) {
    let row = groupRows.get(group.id);
    if (!row) {
      row = el("li"); const button = el("button", undefined, "fa-group-choice"); button.type = "button";
      button.append(el("span"), el("small")); button.addEventListener("click", () => selectGroup(groups.find(item => item.id === group.id)));
      row.append(button); groupRows.set(group.id, row); $("group-list").append(row);
    }
    const button = row.firstElementChild; button.firstElementChild.textContent = group.name; button.lastElementChild.textContent = `${group.node_ids.length} 个节点`;
    button.setAttribute("aria-pressed", String(draft.id === group.id)); button.disabled = busy;
  }
  $("group-count").textContent = `${groups.length} / 128`;
  $("groups-empty").hidden = groups.length > 0;
  $("groups-empty").textContent = groupsReady ? "尚无分组，可以在右侧创建。" : "尚无可用分组快照。";
  $("groups-health").hidden = groupsReady;
  renderGroupMembers(); renderGroupStatus(); updateGroupControls();
}
function selectGroup(group = null) {
  draft = groupDraft(group); groupDirty = false; groupMessage = ""; pendingGroupDelete = null;
  $("group-name").value = draft.name; $("group-confirm").hidden = true; $("group-confirm-copy").textContent = "";
  renderGroups();
}
function renderGroupMembers() {
  const selected = new Set(draft.node_ids), nodes = snapshot?.nodes ?? [], known = new Set(nodes.map(node => node.id));
  const candidates = [...nodes, ...draft.node_ids.filter(id => !known.has(id)).map(id => ({id, name: `已移除节点 #${id}`, missing: true}))];
  const shown = new Set(candidates.map(node => node.id));
  for (const [id, label] of groupMembers) if (!shown.has(id)) { label.remove(); groupMembers.delete(id); }
  for (const node of candidates) {
    let label = groupMembers.get(node.id);
    if (!label) {
      label = el("label", undefined, "fa-group-member"); const input = el("input"); input.type = "checkbox"; input.value = node.id;
      const text = el("span"); text.append(el("span"), el("small")); label.append(input, text);
      groupMembers.set(node.id, label); $("group-members").append(label);
    }
    label.firstElementChild.checked = selected.has(node.id);
    label.lastElementChild.firstElementChild.textContent = node.name;
    label.lastElementChild.lastElementChild.textContent = node.missing ? "请取消勾选后再保存。" : `#${node.id} · ${node.settings?.is_public === true ? "公开节点" : "不公开节点"}`;
  }
  $("group-members-empty").hidden = candidates.length > 0;
  $("group-member-count").textContent = `${selected.size} 个已选`;
}
function renderGroupStatus() {
  const state = groupDraftState(draft, groups);
  $("group-editor-title").textContent = draft.id ? `编辑分组 #${draft.id}` : "新建分组";
  $("group-save").textContent = draft.id ? "保存分组" : "创建分组";
  $("group-status").textContent = !groupsReady ? "分组配置暂不可用，编辑内容已保留；请重新读取。" : state === "deleted" ? "此分组已被删除，编辑内容已保留。重新读取后可创建新分组。" : state === "changed" ? "此分组已被其他操作更新，编辑内容已保留。请重新读取后再保存。" : groupMessage || (groupDirty ? "有未保存的修改；自动同步不会覆盖编辑内容。" : "修改名称或勾选成员后保存；不勾选任何节点可保存空分组。");
}
function updateGroupControls() {
  const unavailable = busy || !groupsReady, stale = ["changed", "deleted"].includes(groupDraftState(draft, groups));
  $("group-name").disabled = unavailable;
  for (const label of groupMembers.values()) label.firstElementChild.disabled = unavailable;
  $("group-new").disabled = unavailable; $("groups-reload").disabled = busy;
  $("group-save").disabled = unavailable || stale;
  $("group-delete").hidden = !draft.id; $("group-delete").disabled = unavailable || stale;
  $("group-delete-yes").disabled = unavailable || stale; $("group-delete-no").disabled = busy;
}
async function reloadGroups() {
  const generation = ++groupsEpoch, selected = draft.id;
  const next = groupDocument(await client.request("/api/admin/v1/groups"));
  if (!loggedIn || generation !== groupsEpoch) return;
  groups = next.groups; groupsReady = true;
  selectGroup(groups.find(group => group.id === selected));
}
async function changeGroup(action) {
  if (busy || !groupsReady) return;
  await mutation(async () => {
    groupsEpoch++;
    try { await action(); }
    catch (error) {
      if (loggedIn) {
        groupMessage = error.status === 409 ? "保存冲突：分组名称已存在、已达到 128 个分组上限，或配置已被其他操作更新。请检查名称或重新读取；编辑内容已保留。" : `${errorText(error)} 编辑内容已保留。`;
        renderGroupStatus();
      }
      throw error;
    }
  });
}
async function loadProbes() {
  if (!loggedIn) return;
  const generation = ++probeEpoch;
  $("probe-status").textContent = "正在读取任务配置…";
  try {
    const next = probeDocument(await client.request("/api/admin/v1/probes"));
    if (!loggedIn || generation !== probeEpoch) return;
    probes = next; $("probe-rows").replaceChildren();
    for (const node of next.nodes) for (const task of node.tasks) addProbe({...task, agent_id: node.agent_id});
    $("probes-form").hidden = false; $("probe-status").textContent = "已读取任务清单。保存前请确认所有节点的任务；并发变更会拒绝覆盖。";
  } catch (error) {
    if (!loggedIn || generation !== probeEpoch) return;
    if (error.status === 404) { probes = null; $("probes-form").hidden = true; $("probe-status").textContent = "探测任务暂不可用，请检查服务端配置后重新读取。"; }
    else $("probe-status").textContent = `${errorText(error)}${probes ? " 保留未保存的编辑内容。" : " 尚未载入任务。"}`;
  }
}
function addProbe(task = {}) {
  const row = el("div", undefined, "wsk-panel fa-probe-row");
  const field = (title, control) => {
    const label = el("label", undefined, "wsk-field"); control.classList.add("wsk-control");
    label.append(el("span", title, "wsk-label"), control); row.append(label); return control;
  };
  const node = el("select"); node.name = "agent_id"; node.required = true;
  const empty = el("option", "选择节点"); empty.value = ""; node.append(empty);
  for (const item of snapshot?.nodes ?? []) { const option = el("option", item.name); option.value = item.id; node.append(option); }
  if (task.agent_id && !snapshot?.nodes.some(item => item.id === task.agent_id)) { const option = el("option", `已撤销节点 ${task.agent_id}`); option.value = task.agent_id; node.append(option); }
  node.value = task.agent_id ?? selectedID ?? ""; field("执行节点", node);
  for (const [key, label, max, placeholder] of [["id", "任务 ID", 128, "稳定任务标识"], ["name", "公开名称", 128, "公开标签"], ["target", "私有目标", 256, "host:port"], ["interval", "间隔 / 秒", 4, "5–3600"]]) {
    const input = el("input"); input.name = key; input.required = true; input.autocomplete = "off"; input.maxLength = max; input.placeholder = placeholder;
    input.value = task[key] ?? (key === "interval" ? "30" : "");
    if (key === "interval") { input.type = "number"; input.min = "5"; input.max = "3600"; input.step = "1"; }
    field(label, input);
  }
  const remove = el("button", "移除", "wsk-button wsk-secondary"); remove.type = "button"; remove.addEventListener("click", () => { row.remove(); $("probe-add").focus(); }); row.append(remove); $("probe-rows").append(row); return row;
}
function requestConfirmation(kind) {
  const node = snapshot?.nodes.find(item => item.id === selectedID); if (!node) return;
  pendingAction = {kind, id: node.id, name: node.name}; $("confirm-action").hidden = false;
  $("confirm-copy").textContent = kind === "rotate" ? `确认轮换「${node.name}」的令牌？旧令牌立即失效，需在节点更新令牌文件并重新连接监控。` : `确认撤销「${node.name}」？节点将无法继续上报，需重新创建凭据才能接入。`;
  $("confirm-yes").textContent = kind === "rotate" ? "确认轮换" : "确认撤销"; $("confirm-yes").focus();
}
function loadSettings(node) {
  const form = $("settings-form"); form.reset(); settingsDirty = false;
  const settings = node?.settings; settingsRevision = settings?.config_revision ?? null;
  settingsNeedsReload = false; editorSettingsSaved = false;
  editorMembership = node && snapshot?.groups_state === "ready" ? membershipDraft(node.id, snapshot.groups) : null;
  renderEditorGroups(); $("settings-status").textContent = "";
  if (!settings) return;
  for (const key of ["name", "public_note", "private_note", "billing_cycle", "renewal_note", "traffic_mode", "traffic_reset_mode", "traffic_reset_day", "traffic_reset_timezone"]) form.elements[key].value = settings[key] ?? "";
  for (const key of ["is_public", "publish_billing", "publish_traffic_plan"]) form.elements[key].checked = settings[key] === true;
  form.elements.currency.value = settings.currency || "CNY";
  form.elements.price.value = priceInput(settings.price_minor, settings.currency);
  form.elements.expires_at_ms.value = localDateInput(settings.expires_at_ms);
  form.elements.traffic_quota.value = gibInput(settings.traffic_quota_bytes);
  form.elements.traffic_used.value = "";
}
function renderEditorGroups() {
  const box = $("node-editor-groups"), message = $("node-editor-groups-status"); box.replaceChildren();
  for (const group of editorMembership?.base ?? []) {
    const label = el("label", undefined, "wsk-choice"), input = el("input");
    input.type = "checkbox"; input.value = group.id; input.dataset.editorGroup = "";
    input.checked = editorMembership.selected.includes(group.id); input.defaultChecked = input.checked; input.disabled = busy;
    label.append(input, document.createTextNode(group.name)); box.append(label);
  }
  message.textContent = !editorMembership ? "分组暂不可用，重新读取后可编辑。" : !editorMembership.base.length ? "尚无分组，可先在分组管理中创建。" : "";
  message.hidden = !message.textContent;
}
function acceptEditorSettings(id, saved) {
  acknowledgedSettings.set(id, saved);
  const node = snapshot?.nodes.find(item => item.id === id);
  if (node) { node.settings = saved; node.name = saved.name; }
  settingsRevision = saved.config_revision; settingsDirty = false; settingsNeedsReload = false; editorSettingsSaved = true;
  $("settings-form").elements.traffic_used.value = "";
  $("settings-form").elements.name.value = saved.name;
  $("settings-status").textContent = "节点设置已保存。";
  if (snapshot) renderNodes();
}
function revealInvalid(control, pane) {
  showEditorPane(control.closest("[data-settings-pane]")?.dataset.settingsPane || pane);
  for (let parent = control.parentElement; parent; parent = parent.parentElement) if (parent.tagName === "DETAILS") parent.open = true;
  control.focus(); control.reportValidity();
}
function validateEditorForm(form, pane) {
  const invalid = [...form.elements].find(control => control.willValidate && !control.validity.valid);
  if (!invalid) return true;
  revealInvalid(invalid, pane); return false;
}
function submitNodeEditor(event) {
  event.preventDefault(); const id = selectedID;
  if (!id || busy) return;
  if (settingsNeedsReload || editorMembership?.blocked) { $("settings-status").textContent = "配置已变更或上次保存结果待确认，请重新读取后再编辑。"; return; }
  if (!validateEditorForm($("settings-form"), "settings") || bindingDirty && !validateEditorForm($("binding-form"), "binding")) return;
  let body = null;
  try {
    if (settingsDirty) {
      const values = Object.fromEntries([...$("settings-form").elements].filter(field => field.name).map(field => [field.name, field.type === "checkbox" ? field.checked : field.value]));
      body = settingsRequest(values, settingsRevision);
    }
  } catch (error) {
    showEditorPane(error.message.includes("节点名称") ? "settings" : "plan");
    if ($("settings-form").elements.traffic_used.value.trim()) $("settings-plan").querySelector("details").open = true;
    $("settings-status").textContent = error.message; return;
  }
  const binding = bindingDirty ? Object.fromEntries(["server_id", "user", "raw_client_id"].map(key => [key, $("binding-form").elements[key].value.trim()])) : null;
  const membership = editorMembership;
  mutation(async () => {
    groupsEpoch++;
    try {
      await saveNodeChanges({nodeID: id, membership, settings: body, binding, settingsAlreadySaved: editorSettingsSaved, request: (path, options) => client.request(path, options),
        onGroupSaved: group => {
          if (!loggedIn || selectedID !== id) return;
          groups = [...groups.filter(item => item.id !== group.id), group];
          if (snapshot) snapshot.groups = [...snapshot.groups.filter(item => item.id !== group.id), group];
          renderGroups();
          $("node-editor-groups-status").textContent = `已保存 ${membership.savedIDs.length} 个分组，继续保存其余修改…`;
          $("node-editor-groups-status").hidden = false;
        },
        onSettingsSaved: saved => { if (loggedIn && selectedID === id) acceptEditorSettings(id, saved); },
        onBindingSaved: saved => {
          if (!loggedIn || selectedID !== id) return;
          bindingBaseline = {...saved}; bindingDirty = false;
          const node = snapshot?.nodes.find(item => item.id === id); if (node) node.frp_binding = {...saved};
        }
      });
      if (!loggedIn || selectedID !== id) return;
      closeNodeEditor();
      const refreshed = await refreshNodes(true);
      if (loggedIn) notice(refreshed ? "节点修改已保存。" : "节点修改已保存；列表刷新失败，请稍后重新读取。", refreshed ? "ready" : "error");
    } catch (error) {
      if (loggedIn && selectedID === id) {
        if (error.stage === "settings" && error.needsReload) settingsNeedsReload = true;
        $("settings-status").textContent = error.userMessage || errorText(error);
        if (error.stage === "groups") { $("node-editor-groups-status").textContent = error.userMessage; $("node-editor-groups-status").hidden = false; showEditorPane("settings"); }
        if (error.stage === "binding") showEditorPane("binding");
      }
      throw error;
    }
  });
}
$("group-new").addEventListener("click", () => { selectGroup(); $("group-name").focus(); });
$("groups-reload").addEventListener("click", () => mutation(reloadGroups, "已重新读取分组；未保存的编辑已放弃。"));
$("group-form").addEventListener("input", () => {
  draft.name = $("group-name").value;
  draft.node_ids = [...$("group-members").querySelectorAll("input:checked")].map(input => input.value);
  groupDirty = true; groupMessage = ""; pendingGroupDelete = null; $("group-confirm").hidden = true;
  $("group-member-count").textContent = `${draft.node_ids.length} 个已选`; renderGroupStatus();
});
$("group-form").addEventListener("submit", event => {
  event.preventDefault();
  if (["changed", "deleted"].includes(groupDraftState(draft, groups))) { renderGroupStatus(); return; }
  let body;
  try { body = groupRequest(draft, new Set((snapshot?.nodes ?? []).map(node => node.id))); }
  catch (error) { groupMessage = error.message; renderGroupStatus(); return; }
  const id = draft.id;
  changeGroup(async () => {
    const saved = groupDocument({groups: [await client.request(`/api/admin/v1/groups${id ? `/${id}` : ""}`, {method: id ? "PATCH" : "POST", body})]}).groups[0];
    groups = [...groups.filter(group => group.id !== saved.id), saved]; selectGroup(saved);
    groupMessage = "分组已保存。节点的其他分组不受影响。"; renderGroupStatus();
    await refreshNodes(true); notice("分组已保存。");
  });
});
$("group-delete").addEventListener("click", () => {
  const group = groups.find(item => item.id === draft.id); if (!group) return;
  pendingGroupDelete = {id: draft.id, config_revision: draft.config_revision};
  $("group-confirm-copy").textContent = `确认删除「${group.name}」分组？仅删除分组及其成员关系，不会删除节点，节点的其他分组也会保留。${groupDirty ? "当前未保存的编辑将一并放弃。" : ""}`;
  $("group-confirm").hidden = false; $("group-delete-yes").focus();
});
$("group-delete-no").addEventListener("click", () => { pendingGroupDelete = null; $("group-confirm").hidden = true; $("group-delete").focus(); });
$("group-delete-yes").addEventListener("click", () => {
  const action = pendingGroupDelete; if (!action) return;
  changeGroup(async () => {
    await client.request(`/api/admin/v1/groups/${action.id}`, {method: "DELETE", body: {config_revision: action.config_revision}});
    groups = groups.filter(group => group.id !== action.id); selectGroup();
    await refreshNodes(true); notice("分组已删除，成员节点仍保留。");
  });
});
$("settings-form").addEventListener("input", () => {
  settingsDirty = true;
  if (editorMembership) editorMembership.selected = [...$("node-editor-groups").querySelectorAll("input:checked")].map(input => input.value);
  $("settings-status").textContent = "有未保存的修改。";
});
$("settings-form").addEventListener("submit", submitNodeEditor);
$("binding-form").noValidate = true;
$("binding-form").addEventListener("input", () => {
  bindingDirty = ["server_id", "user", "raw_client_id"].some(key => $("binding-form").elements[key].value.trim() !== (bindingBaseline?.[key] ?? ""));
  $("settings-status").textContent = settingsDirty || bindingDirty ? "有未保存的修改。" : "";
});
$("binding-form").addEventListener("submit", submitNodeEditor);
$("settings-reload").addEventListener("click", () => mutation(async () => {
  const id = selectedID, next = await refreshNodes(true);
  if (!loggedIn || selectedID !== id) return;
  const node = next?.nodes.find(item => item.id === id);
  if (!node?.settings || next.credentials_state !== "ready" || next.groups_state !== "ready") throw Object.assign(new Error("reload_failed"), {userMessage: "重新读取未完成，编辑内容已保留，请稍后重试。"});
  loadSettings(node); loadBinding(node); renderDetail();
}, "已重新读取，未保存的编辑已放弃。"));
$("traffic-reset").addEventListener("click", () => {
  const id = selectedID; if (!id) return;
  if (settingsDirty) { $("settings-status").textContent = "请先保存或重新读取设置，再重置套餐周期。"; return; }
  if (settingsNeedsReload) { $("settings-status").textContent = "请先重新读取设置，再重置套餐周期。"; return; }
  mutation(async () => {
    const saved = await client.request(`/api/admin/v1/nodes/${id}/reset-traffic`, {method: "POST", body: {config_revision: settingsRevision}});
    if (loggedIn && selectedID === id) acceptEditorSettings(id, saved);
    await refreshNodes(true);
  }, "当前套餐周期已重置，今日与系统累计流量保留。");
});
$("logout").addEventListener("click", async () => {
  if (busy) return; setBusy(true); let message = "已退出管理工作台。";
  try { await client.logout(); } catch { message = "已清除本页私有数据。退出请求失败，服务端会话可能仍有效；请重试登录后退出或关闭浏览器。"; }
  finally { locked(message); setBusy(false); }
});
$("create-form").addEventListener("submit", event => {
  event.preventDefault(); const name = $("node-name").value.trim(); if (!name) return;
  mutation(async () => { clearSecret(); const result = await client.request("/api/admin/v1/nodes", {method: "POST", body: {name}}); $("create-dialog").close(); showSecret(result); $("create-form").reset(); await refreshNodes(true); }, "节点已创建。请保存一次性令牌。");
});
$("binding-clear").addEventListener("click", () => {
  const id = selectedID; if (!id) return;
  if (settingsDirty || settingsNeedsReload || membershipChanges(editorMembership).length) { $("settings-status").textContent = "请先保存或重新读取基本信息与套餐修改，再解除绑定。"; return; }
  mutation(async () => {
    await client.request(`/api/admin/v1/nodes/${encodeURIComponent(id)}/binding`, {method: "DELETE"});
    if (!loggedIn || selectedID !== id) return;
    loadBinding({frp_binding: null});
    const next = await refreshNodes(true), node = next?.nodes.find(item => item.id === id);
    if (node?.settings && next.credentials_state === "ready") loadSettings(node);
    else { settingsNeedsReload = true; $("settings-status").textContent = "绑定已解除；请重新读取节点设置后继续编辑。"; }
  }, "已解除可信绑定。");
});
$("rotate").addEventListener("click", () => requestConfirmation("rotate"));
$("revoke").addEventListener("click", () => requestConfirmation("revoke"));
$("confirm-no").addEventListener("click", () => { pendingAction = null; $("confirm-action").hidden = true; $("rotate").focus(); });
$("confirm-yes").addEventListener("click", () => {
  const action = pendingAction; if (!action) return;
  mutation(async () => {
    clearSecret(); const result = await client.request(`/api/admin/v1/nodes/${encodeURIComponent(action.id)}${action.kind === "rotate" ? "/rotate" : ""}`, {method: action.kind === "rotate" ? "POST" : "DELETE"});
    pendingAction = null; $("confirm-action").hidden = true;
    if (action.kind === "rotate") showSecret(result);
    await refreshNodes(true);
  }, action.kind === "rotate" ? "令牌已轮换。请保存上方新令牌并更新节点。" : "节点接入权限已撤销。");
});
$("secret-clear").addEventListener("click", () => { clearSecret(); if ($("node-panel").open) $("rotate").focus(); else $("add-node").focus(); });
$("secret-copy").addEventListener("click", async () => {
  try { await navigator.clipboard.writeText($("secret-token").value); notice("令牌已复制。保存到私有文件后请清除显示。"); }
  catch { $("secret-token").focus(); $("secret-token").select(); notice("浏览器不允许自动复制，请复制已选中的令牌。", "error"); }
});
$("refresh").addEventListener("click", () => refreshNodes());
for (const button of document.querySelectorAll("[data-refresh]")) button.addEventListener("click", () => refreshNodes());
for (const button of document.querySelectorAll("#add-node, [data-add-node]")) button.addEventListener("click", () => { openDialog("create-dialog"); $("node-name").focus(); });
for (const button of document.querySelectorAll("[data-node-filter]")) button.addEventListener("click", () => openNodeFilter(button.dataset.nodeFilter));
for (const button of document.querySelectorAll("[data-directory-filter]")) button.addEventListener("click", () => { $("node-filter").value = button.dataset.directoryFilter; filterNodes(); });
$("review-shortcut").addEventListener("click", () => { if (snapshot?.frp?.state === "ready") openNodeFilter("review"); else { location.hash = "frp"; showView(); } });
for (const [id, delta] of [["calendar-prev", -1], ["calendar-next", 1]]) $(id).addEventListener("click", () => {
  calendarMonth = new Date(calendarMonth.getFullYear(), calendarMonth.getMonth() + delta, 1); calendarDay = null; renderCalendar();
});
$("calendar-today").addEventListener("click", () => { const now = new Date(); calendarMonth = new Date(now.getFullYear(), now.getMonth(), 1); calendarDay = null; renderCalendar(); });
for (const button of document.querySelectorAll("[data-close-dialog]")) button.addEventListener("click", () => { if (!busy) { const dialog = button.closest("dialog"); if (dialog.id === "node-panel") closeNodeEditor(); else dialog.close(); } });
for (const dialog of document.querySelectorAll("dialog")) dialog.addEventListener("cancel", event => { if (busy) event.preventDefault(); else if (dialog.id === "node-panel") { event.preventDefault(); closeNodeEditor(); } });
$("node-panel").addEventListener("close", () => { if (!$("node-panel").open) discardNodeEditor(); });
$("credential-result").addEventListener("close", () => { $("secret-id").value = ""; $("secret-token").value = ""; });
for (const button of document.querySelectorAll("[data-editor-pane]")) button.addEventListener("click", () => showEditorPane(button.dataset.editorPane));
window.addEventListener("hashchange", showView); showView();
$("node-search").addEventListener("input", filterNodes);
$("node-filter").addEventListener("change", filterNodes);
$("node-group").addEventListener("change", filterNodes);
$("probes-reload").addEventListener("click", () => mutation(loadProbes));
$("probe-add").addEventListener("click", () => { const row = addProbe(); row.querySelector("input").focus(); });
$("probes-form").addEventListener("submit", event => {
  event.preventDefault(); if (!probes) return;
  let next;
  try { const rows = [...$("probe-rows").children].map(row => Object.fromEntries([...row.querySelectorAll("input,select")].map(input => [input.name, input.value]))); next = nextProbeDocument(probes, rows, new Set((snapshot?.nodes ?? []).map(node => node.id))); }
  catch (error) { $("probe-status").textContent = error.message; return; }
  mutation(async () => { probes = probeDocument(await client.request("/api/admin/v1/probes", {method: "PUT", body: next})); $("probe-status").textContent = "任务清单已保存。已接入且开启探测的节点将接收新清单。"; }, "探测任务已保存。");
});
window.addEventListener("pagehide", () => locked());
window.addEventListener("pageshow", event => { if (event.persisted) boot(); });
document.addEventListener("visibilitychange", () => { if (!document.hidden && loggedIn) refreshNodes(); });
async function boot() {
  const authError = new URLSearchParams(window.location.search).get("auth_error");
  const authMessage = {access_denied: "此 GitHub 账号未获管理权限，请切换已授权账号。", failed: "GitHub 登录未完成，请重试。"}[authError];
  try { await client.session(); await unlocked(); }
  catch (error) {
    if (![401, 404].includes(error.status)) { locked(errorText(error)); return; }
    try {
      const auth = await client.request("/api/admin/v1/auth");
      const enabled = auth?.provider === "github" && auth.enabled === true;
      locked(enabled ? authMessage || "请使用已授权的 GitHub 账号登录。" : "管理员 GitHub 登录尚未配置，请先完成服务端设置。", !enabled);
    } catch { locked("暂时无法读取 GitHub 登录配置，请稍后刷新。", true); }
  }
}
boot();
