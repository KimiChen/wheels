export const UNKNOWN = "—";
export function value(field) {
  return field?.quality === "ok" && field.value !== null ? field.value : null;
}
export function uint64(raw) {
  if (typeof raw !== "string" || !/^(0|[1-9][0-9]*)$/.test(raw) || raw.length > 20) return null;
  const n = BigInt(raw);
  return n <= 18446744073709551615n ? n : null;
}
// Current daily/period sums can exceed a single system counter's uint64 range.
export function cumulative(raw) {
  return typeof raw === "string" && raw.length <= 80 && /^(0|[1-9][0-9]*)$/.test(raw) ? BigInt(raw) : null;
}
export function decimal(field) { return uint64(value(field)); }

export function percent(field) {
  const n = value(field);
  return typeof n === "number" && Number.isFinite(n) && n >= 0 && n <= 100 ? n : null;
}
export function percentage(n) { return n === null ? UNKNOWN : `${n.toFixed(1)}%`; }
export function ratio(used, total) {
  const u = decimal(used), t = decimal(total);
  return u === null || t === null || t === 0n || u > t ? null : Number(u * 1000n / t) / 10;
}
export function bytes(n, rate = false) {
  if (n === null) return UNKNOWN;
  const units = ["B", "KiB", "MiB", "GiB", "TiB", "PiB", "EiB"];
  let index = 0, unit = 1n;
  while (n >= unit * 1024n && index < units.length - 1) { unit *= 1024n; index++; }
  const tenths = (n * 10n + unit / 2n) / unit;
  const result = index === 0 ? n.toString() : `${tenths / 10n}.${tenths % 10n}`;
  return `${result} ${units[index]}${rate ? "/s" : ""}`;
}
export function capacity(used, total) {
  const u = decimal(used), t = decimal(total);
  return u === null || t === null ? quality(used?.quality !== "ok" ? used : total) : `${bytes(u)} / ${bytes(t)}`;
}
export function quality(field) {
  return ({warming_up: "等待差分采样", unavailable: "采集暂不可用", unsupported: "暂不支持"})[field?.quality] ?? "暂无有效采样";
}
export function loadText(field) {
  const values = value(field);
  return Array.isArray(values) && values.length === 3 && values.every(n => typeof n === "number" && Number.isFinite(n) && n >= 0)
    ? values.map(n => n.toFixed(2)).join(" / ") : UNKNOWN;
}
export function uptime(field) {
  const seconds = decimal(field);
  if (seconds === null) return UNKNOWN;
  if (seconds >= 86400n) return `${seconds / 86400n} 天 ${seconds % 86400n / 3600n} 小时`;
  if (seconds >= 3600n) return `${seconds / 3600n} 小时 ${seconds % 3600n / 60n} 分钟`;
  return `${seconds / 60n} 分钟`;
}
export function onlineUptime(field, session) {
  const seconds = decimal(field);
  if (seconds === null) return UNKNOWN;
  return `${session === "online" ? "在线" : "运行"} ${seconds / 86400n} 天 ${seconds % 86400n / 3600n} 小时`;
}
export function timeText(raw) {
  if (typeof raw !== "string") return UNKNOWN;
  const date = new Date(raw);
  return Number.isNaN(date.getTime()) ? UNKNOWN : date.toLocaleTimeString("zh-CN", {hour12: false});
}
export const sessionLabels = {online: "监控在线", offline: "监控离线", waiting: "等待接入"};
export const freshnessLabels = {fresh: "新鲜", stale: "已过期", waiting: "等待报告"};
export function frpLabel(state) {
  return ({connected: "已连接", disconnected: "已断开", connecting: "连接中", unknown: "未知", disabled: "未启用"})[state] ?? "未知";
}

// Public reconciliation is a server-cropped summary, never raw proxy identity data.
export function reconciliationText(frp) {
  const labels = {matched: "已核对", unbound: "未设置可信绑定", conflict: "归属冲突", mismatch: "绑定不匹配", missing: "服务端未登记", stale: "节点报告已过期", transient: "缺少稳定 ID", unavailable: "服务端暂不可核对"};
  if (!frp || !labels[frp.reconciliation]) return "等待核对";
  if (frp.reconciliation !== "matched") return labels[frp.reconciliation];
  const connection = frp.server_online === true ? "服务端在线" : frp.server_online === false ? "服务端离线" : "服务端状态未知";
  const count = Number.isSafeInteger(frp.registered) && frp.registered >= 0 ? `已登记 ${frp.registered} 条` : "登记数未知";
  return `已核对 · ${connection} · ${count}`;
}
