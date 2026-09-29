import {select, sortNodes, sortOptions} from "./store.mjs";
import {UNKNOWN, decimal, uptime} from "./format.mjs";
import {dateTime, expiryText} from "./node-settings.mjs";

export const homeSortOptions = [...sortOptions, ["expiry", "到期时间"]];
export const statusFilters = ["all", "online", "offline", "waiting"];
const week = 7 * 86400000;

function expiration(node) {
  const value = node?.billing?.expires_at_ms;
  return Number.isSafeInteger(value) && !Number.isNaN(new Date(value).getTime()) ? value : null;
}

// Only the public billing projection may contribute to this count or filter.
// Expired nodes are not described as expiring within the next seven days.
export function expiresSoon(node, now = Date.now()) {
  const value = expiration(node);
  return value !== null && value > now && value <= now + week;
}

export function normalizeHomeFilters(value) {
  return {status: statusFilters.includes(value?.status) ? value.status : "all", expiring: value?.expiring === true};
}

export function filterHomeNodes(nodes, {query = "", group = "all", status = "all", expiring = false} = {}, now = Date.now()) {
  const filters = normalizeHomeFilters({status, expiring});
  return select(nodes, query, group).filter(node =>
    (filters.status === "all" || node.session === filters.status) && (!filters.expiring || expiresSoon(node, now)));
}

export function sortHomeNodes(nodes, order = "default") {
  if (order !== "expiry") return sortNodes(nodes, order);
  return [...nodes].sort((a, b) => {
    const left = expiration(a), right = expiration(b);
    if (left === null || right === null) return left === right ? 0 : left === null ? 1 : -1;
    return left < right ? -1 : left > right ? 1 : 0;
  });
}

function megabits(field) {
  const value = decimal(field);
  if (value === null) return {number: UNKNOWN, unit: ""};
  // B/s -> bit/s -> decimal Mbps, rounded only after exact integer scaling.
  const hundredths = (value * 800n + 500000n) / 1000000n;
  return {number: `${hundredths / 100n}.${String(hundredths % 100n).padStart(2, "0")}`, unit: "Mbps"};
}

function transfer(field) {
  const value = decimal(field);
  if (value === null) return {number: UNKNOWN, unit: ""};
  const units = ["B", "KB", "MB", "GB", "TB", "PB", "EB"];
  let index = 0, divisor = 1n;
  while (value >= divisor * 1000n && index < units.length - 1) { divisor *= 1000n; index++; }
  let tenths = (value * 10n + divisor / 2n) / divisor;
  // Keep rounded values compact at a unit boundary, e.g. 999.95 KB -> 1.0 MB.
  if (tenths >= 10000n && index < units.length - 1) { divisor *= 1000n; index++; tenths = (value * 10n + divisor / 2n) / divisor; }
  return {number: index === 0 ? String(value) : `${tenths / 10n}.${tenths % 10n}`, unit: units[index]};
}

function localDay(raw) {
  const date = new Date(raw), pad = value => String(value).padStart(2, "0");
  return `${String(date.getFullYear()).padStart(4, "0")}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}`;
}

// Use only public fields and retain the last valid samples. Cumulative transfer
// is a system counter, not a daily total or the billing plan's period usage.
export function homeRowDetails(node, now = Date.now()) {
  const metrics = node?.metrics ?? {}, expires = expiration(node);
  const values = {}, titles = {};
  for (const [direction, label] of [["rx", "接收"], ["tx", "发送"]]) {
    for (const [kind, display, title] of [
      ["rate", megabits(metrics[`net_${direction}`]), `最近有效采样的${label}速率，1 Mbps = 1,000,000 bit/s`],
      ["total", transfer(metrics[`net_${direction}_total`]), `系统累计${label}量（十进制单位），节点重启可能归零`],
    ]) {
      const key = `row-${direction}-${kind}`;
      values[key] = display.number; values[`${key}-unit`] = display.unit;
      titles[key] = title; titles[`${key}-unit`] = title;
    }
  }
  values["row-expiry"] = expires === null ? UNKNOWN : localDay(expires);
  values["row-uptime"] = uptime(metrics.uptime);
  titles["row-expiry"] = expires === null ? "到期时间未公开或未设置" : `到期时间：${dateTime(expires)}（浏览器本地时区） · ${expiryText(expires, now)}`;
  titles["row-uptime"] = "最近有效采样的系统运行时间，不代表监控连续在线时长";
  return {values, titles, expiryUrgent: expires !== null && expires <= now + week};
}
