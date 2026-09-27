import {decimal, percent} from "./format.mjs";
import {validNodeID} from "./node-data.mjs";

// Identity is the server-generated numeric SQLite ID, transmitted as a decimal string.
export function snapshot(data) {
  if (!data || !Array.isArray(data.nodes) || typeof data.generated_at !== "string" || Number.isNaN(Date.parse(data.generated_at))) throw new Error("invalid snapshot");
  const seen = new Set();
  for (const node of data.nodes) {
    if (!node || !validNodeID(node.id) || seen.has(node.id) || typeof node.name !== "string" ||
        !["online", "offline", "waiting"].includes(node.session) || !["fresh", "stale", "waiting"].includes(node.freshness)) throw new Error("invalid node");
    seen.add(node.id);
  }
  return data;
}
export function select(nodes, query = "") {
  const text = query.trim().toLocaleLowerCase("zh-CN");
  return nodes.filter(n => n.name.toLocaleLowerCase("zh-CN").includes(text));
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
