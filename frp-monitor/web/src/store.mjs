import {decimal, percent} from "./format.mjs";
import {validNodeID, nodeGroups} from "./node-data.mjs";

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
export function overview(nodes) {
  const live = nodes.filter(n => n.session === "online" && n.freshness === "fresh");
  const sum = key => {
    const values = live.map(n => decimal(n.metrics?.[key])).filter(v => v !== null);
    return {value: values.length ? values.reduce((a,b) => a+b, 0n) : null, count: values.length};
  };
  const cpu = live.map(n => percent(n.metrics?.cpu)).filter(value => value !== null);
  return {total: nodes.length, online: nodes.filter(n => n.session === "online").length,
    offline: nodes.filter(n => n.session === "offline").length, waiting: nodes.filter(n => n.session === "waiting").length,
    rx: sum("net_rx"), tx: sum("net_tx"), cpu: {value: cpu.length ? cpu.reduce((a,b) => a+b, 0) / cpu.length : null, count: cpu.length}};
}
