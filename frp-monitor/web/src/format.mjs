export const UNKNOWN = "—";
export function value(field) {
  return field?.quality === "ok" && field.value !== null ? field.value : null;
}
export function decimal(field) {
  const raw = value(field);
  if (typeof raw !== "string" || !/^(0|[1-9][0-9]*)$/.test(raw) || raw.length > 20) return null;
  const n = BigInt(raw);
  return n <= 18446744073709551615n ? n : null;
}
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
