import {decimal, percent} from "./format.mjs";

// Identity is an opaque, server-generated public ID. It is used only as a map key.
export function snapshot(data) {
  if (!data || !Array.isArray(data.nodes) || typeof data.generated_at !== "string" || Number.isNaN(Date.parse(data.generated_at))) throw new Error("invalid snapshot");
  const seen = new Set();
  for (const node of data.nodes) {
    if (!node || typeof node.id !== "string" || !node.id || seen.has(node.id) || typeof node.name !== "string" ||
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
  const proxies = live.filter(n => n.frp?.control_state && n.frp.control_state !== "unknown" && Number.isSafeInteger(n.frp.proxy_total) && Number.isSafeInteger(n.frp.proxy_running) && n.frp.proxy_total >= 0 && n.frp.proxy_running >= 0 && n.frp.proxy_running <= n.frp.proxy_total);
  const cpu = live.map(n => percent(n.metrics?.cpu)).filter(value => value !== null);
  return {total: nodes.length, online: nodes.filter(n => n.session === "online").length,
    offline: nodes.filter(n => n.session === "offline").length, waiting: nodes.filter(n => n.session === "waiting").length,
    fresh: nodes.filter(n => n.freshness === "fresh").length, stale: nodes.filter(n => n.freshness === "stale").length,
    rx: sum("net_rx"), tx: sum("net_tx"), cpu: {value: cpu.length ? cpu.reduce((a,b) => a+b, 0) / cpu.length : null, count: cpu.length}, proxyNodes: proxies.length,
    proxyRunning: proxies.length ? proxies.reduce((a,n) => a+n.frp.proxy_running, 0) : null,
    proxyTotal: proxies.length ? proxies.reduce((a,n) => a+n.frp.proxy_total, 0) : null};
}
