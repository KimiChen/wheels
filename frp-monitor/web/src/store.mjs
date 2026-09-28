import {UNKNOWN, decimal, percent} from "./format.mjs";
import {validNodeID, nodeGroups, hardwareValue} from "./node-data.mjs";

export const ALL_GROUPS = "all", UNGROUPED = "ungrouped";
export const groupKey = id => `group:${id}`;

// Identity is the server-generated numeric SQLite ID, transmitted as a decimal string.
export function snapshot(data) {
  if (!data || !Array.isArray(data.nodes) || typeof data.generated_at !== "string" || Number.isNaN(Date.parse(data.generated_at))) throw new Error("invalid snapshot");
  const seen = new Set();
  for (const node of data.nodes) {
    if (!node || !validNodeID(node.id) || seen.has(node.id) || typeof node.name !== "string" ||
        !["online", "offline", "waiting"].includes(node.session) || !["fresh", "stale", "waiting"].includes(node.freshness)) throw new Error("invalid node");
    if (node.groups !== undefined && (!Array.isArray(node.groups) || nodeGroups(node).length !== node.groups.length)) throw new Error("invalid node groups");
    seen.add(node.id);
  }
  return data;
}
// The public nodes are the entire source of the directory and counts. Groups
// belonging only to hidden nodes can never enter this list.
export function groupOptions(nodes) {
  const groups = new Map();
  let ungrouped = 0;
  for (const node of nodes) {
    const memberships = nodeGroups(node);
    if (!memberships.length) ungrouped++;
    for (const group of memberships) {
      if (!groups.has(group.id)) groups.set(group.id, {key: groupKey(group.id), name: group.name, count: 0});
      groups.get(group.id).count++;
    }
  }
  return [{key: ALL_GROUPS, name: "全部", count: nodes.length}, {key: UNGROUPED, name: "未分组", count: ungrouped},
    ...[...groups].sort(([a], [b]) => BigInt(a) < BigInt(b) ? -1 : 1).map(([, group]) => group)];
}
export function resolveGroupSelection(selected, options) { return options.some(group => group.key === selected) ? selected : ALL_GROUPS; }
export function select(nodes, query = "", selectedGroup = ALL_GROUPS) {
  const text = query.trim().toLocaleLowerCase("zh-CN");
  return nodes.filter(node => {
    if (!node.name.toLocaleLowerCase("zh-CN").includes(text)) return false;
    if (selectedGroup === ALL_GROUPS) return true;
    const groups = nodeGroups(node);
    return selectedGroup === UNGROUPED ? groups.length === 0 : groups.some(group => groupKey(group.id) === selectedGroup);
  });
}
export const sortOptions = [
  ["default", "默认"], ["name", "名称"], ["uptime", "运行时间"], ["system", "系统"],
  ["cpu", "CPU"], ["memory", "内存"], ["disk", "存储"], ["upload", "上传"],
  ["download", "下载"], ["upload-total", "上传总量"], ["download-total", "下载总量"],
];
const names = new Intl.Collator("zh-CN", {numeric: true, sensitivity: "base"});
const compare = (a, b) => a < b ? -1 : a > b ? 1 : 0;
function usage(metrics, prefix) {
  const used = decimal(metrics?.[`${prefix}_used`]), total = decimal(metrics?.[`${prefix}_total`]);
  return used === null || total === null || total === 0n || used > total ? null : {used, total};
}
export function sortNodes(nodes, order = "default") {
  if (order === "default" || !sortOptions.some(([key]) => key === order)) return [...nodes];
  const fields = {uptime: "uptime", upload: "net_tx", download: "net_rx", "upload-total": "net_tx_total", "download-total": "net_rx_total"};
  const readers = {
    name: node => node.name,
    system: node => { const os = hardwareValue(node.hardware?.os); return os === UNKNOWN ? null : os; },
    cpu: node => percent(node.metrics?.cpu),
    memory: node => usage(node.metrics, "mem"), disk: node => usage(node.metrics, "disk"),
  };
  const read = readers[order] ?? (fields[order] ? node => decimal(node.metrics?.[fields[order]]) : null);
  if (!read) return [...nodes];
  // Equal values retain server order. Unknown values stay last in every mode.
  return nodes.map(node => ({node, value: read(node)})).sort((a, b) => {
    if (a.value === null || b.value === null) return a.value === b.value ? 0 : a.value === null ? 1 : -1;
    if (order === "name" || order === "system") return names.compare(a.value, b.value);
    if (order === "memory" || order === "disk") return compare(b.value.used * a.value.total, a.value.used * b.value.total);
    return compare(b.value, a.value);
  }).map(item => item.node);
}
export function overview(nodes) {
  const live = nodes.filter(n => n.session === "online" && n.freshness === "fresh");
  const sum = (key, source = live) => {
    const values = source.map(n => decimal(n.metrics?.[key])).filter(v => v !== null);
    return {value: values.length ? values.reduce((a,b) => a+b, 0n) : null, count: values.length};
  };
  const cpu = live.map(n => percent(n.metrics?.cpu)).filter(value => value !== null);
  return {total: nodes.length, online: nodes.filter(n => n.session === "online").length,
    offline: nodes.filter(n => n.session === "offline").length, waiting: nodes.filter(n => n.session === "waiting").length,
    rx: sum("net_rx"), tx: sum("net_tx"), rxTotal: sum("net_rx_total", nodes), txTotal: sum("net_tx_total", nodes),
    cpu: {value: cpu.length ? cpu.reduce((a,b) => a+b, 0) / cpu.length : null, count: cpu.length}};
}
