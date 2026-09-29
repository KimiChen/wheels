// Derived presentation data only. All configuration writes stay in adminClient.
const dayMS = 86400000;
const reviewStates = new Set(["unbound", "conflict", "mismatch", "missing", "stale", "transient", "unavailable"]);
const proxyReviewStates = new Set(["missing", "conflict", "offline", "unavailable"]);
export function expiryState(node, now = Date.now()) {
  const at = node.settings?.expires_at_ms;
  if (!Number.isSafeInteger(at) || !Number.isFinite(new Date(at).getTime())) return "unset";
  if (at <= now) return "expired";
  return at - now <= 7 * dayMS ? "due" : "later";
}
export function requiresReview(record) {
  return Boolean(record && (reviewStates.has(record.state) || record.state === "matched" &&
    (record.server_online === false || record.proxies?.some(proxy => proxy.enabled && proxyReviewStates.has(proxy.server_state)))));
}
export function overview(snapshot, now = Date.now()) {
  const nodes = snapshot?.nodes ?? [], records = new Map((snapshot?.frp?.nodes ?? []).map(record => [record.id, record]));
  const counts = {total: nodes.length, online: 0, offline: 0, waiting: 0, due: 0, expired: 0, review: null};
  const reviewAvailable = snapshot?.frp?.state === "ready";
  if (reviewAvailable) counts.review = 0;
  const attention = [];
  for (const node of nodes) {
    if (["online", "offline", "waiting"].includes(node.session)) counts[node.session]++;
    const expiry = expiryState(node, now), record = records.get(node.id), reasons = [];
    if (expiry === "due" || expiry === "expired") { counts[expiry]++; reasons.push(expiry); }
    if (node.session === "offline") reasons.push("offline");
    if (reviewAvailable && requiresReview(record)) { counts.review++; reasons.push("review"); }
    if (reasons.length) attention.push({node, reasons, record});
  }
  const priority = item => item.reasons.includes("expired") ? 0 : item.reasons.includes("offline") ? 1 : item.reasons.includes("due") ? 2 : 3;
  attention.sort((a, b) => priority(a) - priority(b) || (a.node.settings?.expires_at_ms ?? Infinity) - (b.node.settings?.expires_at_ms ?? Infinity) || a.node.name.localeCompare(b.node.name, "zh-CN"));
  return {counts, attention, reviewAvailable};
}
export function filterAdminNodes(snapshot, {query = "", state = "all", group = "all"} = {}, now = Date.now()) {
  const needle = query.trim().toLocaleLowerCase(), groups = snapshot?.groups ?? [];
  const selected = groups.find(item => item.id === group), membership = new Set(selected?.node_ids ?? []);
  const grouped = new Set(groups.flatMap(item => item.node_ids)), records = new Map((snapshot?.frp?.nodes ?? []).map(item => [item.id, item]));
  return (snapshot?.nodes ?? []).filter(node => {
    if (group === "ungrouped" && grouped.has(node.id) || group !== "all" && group !== "ungrouped" && !membership.has(node.id)) return false;
    if (["online", "offline", "waiting"].includes(state) && node.session !== state) return false;
    if (["due", "expired"].includes(state) && expiryState(node, now) !== state) return false;
    if (state === "review" && (snapshot?.frp?.state !== "ready" || !requiresReview(records.get(node.id)))) return false;
    const addresses = [node.facts?.ipv4, node.facts?.ipv6].filter(field => field?.quality === "ok").map(field => field.value);
    return `${node.name} ${node.id} ${addresses.join(" ")}`.toLocaleLowerCase().includes(needle);
  });
}
export function localDayKey(timestamp) {
  const date = new Date(timestamp);
  return `${date.getFullYear()}-${String(date.getMonth() + 1).padStart(2, "0")}-${String(date.getDate()).padStart(2, "0")}`;
}
export function expiryCalendar(nodes, year, month) {
  const start = new Date(year, month, 1), end = new Date(year, month + 1, 1), byDay = new Map();
  for (const node of nodes) {
    const at = node.settings?.expires_at_ms;
    if (!Number.isSafeInteger(at) || at < start.getTime() || at >= end.getTime()) continue;
    const key = localDayKey(at);
    if (!byDay.has(key)) byDay.set(key, []);
    byDay.get(key).push(node);
  }
  const days = Array.from({length: new Date(year, month + 1, 0).getDate()}, (_, index) => {
    const day = index + 1, key = localDayKey(new Date(year, month, day).getTime());
    return {day, key, nodes: (byDay.get(key) ?? []).sort((a, b) => a.settings.expires_at_ms - b.settings.expires_at_ms)};
  });
  return {offset: (start.getDay() + 6) % 7, days};
}
