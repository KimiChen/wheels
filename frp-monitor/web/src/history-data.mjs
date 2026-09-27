import {UNKNOWN, bytes} from "./format.mjs";

export const windows = {"1h": "最近 1 小时", "6h": "最近 6 小时", "24h": "最近 24 小时", "7d": "最近 7 天"};
const seconds = {"1h": 3600, "6h": 21600, "24h": 86400, "7d": 604800};
export function integer(raw) {
  if (typeof raw !== "string" || !/^(0|[1-9][0-9]*)$/.test(raw) || raw.length > 20) return null;
  const n = BigInt(raw);
  return n <= 18446744073709551615n ? n : null;
}
// Daily sums may exceed a single uint64 counter after counter resets.
export function cumulative(raw) {
  return typeof raw === "string" && raw.length <= 80 && /^(0|[1-9][0-9]*)$/.test(raw) ? BigInt(raw) : null;
}
export const finite = n => typeof n === "number" && Number.isFinite(n) && n >= 0 ? n : null;
export const count = n => Number.isSafeInteger(n) && n >= 0;
export const validDate = raw => typeof raw === "string" && Number.isFinite(Date.parse(raw));
export function memoryPercent(point) {
  const used = integer(point.mem_used), total = integer(point.mem_total);
  return used === null || total === null || total === 0n || used > total ? null : Number(used * 10000n / total) / 100;
}
export function diskPercent(point) {
  const usedField = point.fields?.disk_used, totalField = point.fields?.disk_total;
  const used = count(usedField?.samples) && usedField.samples > 0 ? integer(usedField.value) : null;
  const total = count(totalField?.samples) && totalField.samples > 0 ? integer(totalField.value) : null;
  return used === null || total === null || total === 0n || used > total ? null : Number(used * 10000n / total) / 100;
}
export function history(data, nodeID, window) {
  if (!data || data.node_id !== nodeID || data.window !== window || !Object.hasOwn(windows, window) || !validDate(data.generated_at) ||
      !Number.isSafeInteger(data.step_seconds) || data.step_seconds < 1 || !["ready", "disabled", "degraded"].includes(data.storage?.state) ||
      !Array.isArray(data.points) || data.points.length > 12000 || !Array.isArray(data.probes) || data.probes.length > 64) throw new Error("invalid history");
  const points = rows => {
    let previous = -Infinity;
    for (const p of rows) {
      if (!p || !validDate(p.at) || Date.parse(p.at) <= previous || !count(p.samples)) throw new Error("invalid history point");
      previous = Date.parse(p.at);
    }
  };
  points(data.points);
  const ids = new Set();
  for (const probe of data.probes) {
    if (!probe || typeof probe.id !== "string" || !probe.id || ids.has(probe.id) || typeof probe.name !== "string" ||
        !Array.isArray(probe.points) || probe.points.length > 12000 || !count(probe.samples) || !count(probe.failures) || probe.failures > probe.samples) throw new Error("invalid probe history");
    ids.add(probe.id); points(probe.points);
  }
  return data;
}
export function dateText(raw, withDate = true) {
  return validDate(raw) ? new Date(raw).toLocaleString("zh-CN", {month: withDate ? "2-digit" : undefined, day: withDate ? "2-digit" : undefined, hour: "2-digit", minute: "2-digit", hour12: false}) : UNKNOWN;
}
export function coverage(raw) {
  if (finite(raw) === null) return UNKNOWN;
  return raw < 60 ? `${Math.floor(raw)} 秒` : raw < 3600 ? `${Math.floor(raw / 60)} 分钟` : `${(raw / 3600).toFixed(1)} 小时`;
}
export function failureRate(probe) {
  return finite(probe.failure_rate) !== null && probe.failure_rate <= 100 && probe.samples > 0 ? `${probe.failure_rate.toFixed(1)}%` : UNKNOWN;
}
// BigInt byte counters stay integers. Only a bounded ratio becomes a coordinate.
function fraction(value, maximum) {
  if (typeof value === "bigint") return maximum === 0n ? 0 : Number(value * 1000000n / maximum) / 1000000;
  return maximum === 0 ? 0 : value / maximum;
}
export function chart(rows, series, {window, generatedAt, step, ceiling = null}) {
  const end = Date.parse(generatedAt), start = end - seconds[window] * 1000;
  const width = 440, height = 110;
  const values = series.map(s => rows.map(p => ({at: Date.parse(p.at), value: p.samples > 0 ? s.read(p) : null})).filter(p => p.at >= start && p.at <= end));
  const known = values.flat().filter(p => p.value !== null).map(p => p.value);
  const maximum = ceiling ?? (known.length ? known.reduce((a, b) => a > b ? a : b) : 0);
  const paths = values.map(points => {
    let previous = null, path = "";
    const dots = [];
    for (const p of points) {
      if (p.value === null) { previous = null; continue; }
      const x = ((p.at - start) / (end - start) * width).toFixed(2), y = (height - fraction(p.value, maximum) * height).toFixed(2);
      path += `${previous !== null && p.at - previous <= step * 1500 ? "L" : "M"}${x},${y} `;
      dots.push({x, y}); previous = p.at;
    }
    return {path: path.trim(), dots};
  });
  const summaries = values.map((points, index) => {
    const valid = points.filter(p => p.value !== null), fmt = series[index].format;
    if (!valid.length) return `${series[index].name}：暂无有效采样`;
    const nums = valid.map(p => p.value), min = nums.reduce((a,b) => a < b ? a : b), max = nums.reduce((a,b) => a > b ? a : b);
    return `${series[index].name}：最近有效值 ${fmt(valid.at(-1).value)}（${dateText(new Date(valid.at(-1).at).toISOString())}），范围 ${fmt(min)} – ${fmt(max)}，${valid.length} 个有效时间段`;
  });
  return {paths, summaries, maximum, empty: !known.length, start: new Date(start).toISOString(), end: generatedAt};
}
export const resourceCharts = [
  {key: "cpu", title: "CPU 使用率", ceiling: 100, series: [{name: "CPU", read: p => finite(p.cpu) !== null && p.cpu <= 100 ? p.cpu : null, format: n => `${n.toFixed(1)}%`}]},
  {key: "memory", title: "内存使用率", ceiling: 100, series: [{name: "内存", read: memoryPercent, format: n => `${n.toFixed(1)}%`}]},
  {key: "network", title: "主机收发速率", series: [{name: "接收", read: p => integer(p.net_rx), format: n => bytes(n, true)}, {name: "发送", read: p => integer(p.net_tx), format: n => bytes(n, true)}]},
  {key: "disk", title: "硬盘使用率", ceiling: 100, series: [{name: "硬盘", read: diskPercent, format: n => `${n.toFixed(1)}%`}]},
  {key: "load", title: "系统负载", series: [0,1,2].map((index) => ({name: `${[1,5,15][index]} 分钟`, read: p => Array.isArray(p.load) ? finite(p.load[index]) : null, format: n => n.toFixed(2)}))},
];
