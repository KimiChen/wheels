import {select, sortNodes, sortOptions} from "./store.mjs";
import {UNKNOWN, decimal, loadText, uptime} from "./format.mjs";
import {dateTime, planText, todayText} from "./node-settings.mjs";

export const homeSortOptions = [...sortOptions, ["expiry", "到期时间"]];
export const statusFilters = ["all", "online", "offline", "waiting"];
const week = 7 * 86400000;
const rowTrafficModes = {max: "收发取较大值", total: "双向合计", rx: "仅接收", tx: "仅发送"};

function expiration(node) {
  const value = node.billing?.expires_at_ms;
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

// These values use only the public snapshot, including its last valid samples.
// Missing samples and unpublished plans must never be represented as zero usage.
export function homeRowDetails(node) {
  const metrics = node?.metrics ?? {}, rawPlan = node?.traffic_plan;
  const planPublished = Boolean(rawPlan && typeof rawPlan === "object" && !Array.isArray(rawPlan));
  const plan = planPublished ? rawPlan : undefined, usage = planText(plan), today = todayText(node?.traffic_today);
  const count = field => String(decimal(field) ?? UNKNOWN);
  const pair = (left, right) => left === UNKNOWN && right === UNKNOWN ? UNKNOWN : `${left} / ${right}`;
  const reset = plan?.reset_mode === "manual" ? "手动重置"
    : plan?.reset_mode === "monthly" ? dateTime(plan.period_end_at_ms) : UNKNOWN;
  let resetShort = reset;
  if (plan?.reset_mode === "monthly" && reset !== UNKNOWN) {
    const date = new Date(plan.period_end_at_ms), pad = value => String(value).padStart(2, "0");
    resetShort = `${String(date.getFullYear()).padStart(4, "0")}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}`;
  }
  const resetTitle = plan?.reset_mode === "monthly" && reset !== UNKNOWN ? `下次重置：${reset}（浏览器本地时区）` : reset;
  const planTitle = planPublished ? `周期已用 ${usage.used} / ${usage.quota}` : "套餐未公开";
  const todayTitle = node?.traffic_today ? `${node.traffic_today.day || "日期未知"}${node.traffic_today.timezone ? ` · 统计日时区：${node.traffic_today.timezone}` : ""}` : "等待今日统计";
  return {planPublished, values: {
    "runtime-uptime": uptime(metrics.uptime),
    "runtime-load": loadText(metrics.load),
    "runtime-connections": pair(count(metrics.tcp), count(metrics.udp)),
    "runtime-processes": count(metrics.procs),
    "row-today": pair(today.rx, today.tx),
    "row-plan-usage": planPublished ? pair(usage.used, usage.quota) : UNKNOWN,
    "row-plan-remaining": plan?.quota_bytes === null ? "无限流量" : usage.remaining,
    "row-plan-mode": Object.hasOwn(rowTrafficModes, plan?.mode) ? rowTrafficModes[plan.mode] : UNKNOWN,
    "row-plan-reset": reset,
    "row-plan-reset-short": resetShort,
  }, titles: {
    "runtime-uptime": "系统运行时间，不代表监控连续在线时长",
    "runtime-load": "最近有效采样的 1 / 5 / 15 分钟系统负载",
    "runtime-connections": "最近有效采样的 TCP / UDP 数量",
    "runtime-processes": "最近有效采样的进程数",
    "row-today": todayTitle,
    "row-plan-usage": planTitle,
    "row-plan-remaining": planTitle,
    "row-plan-mode": planTitle,
    "row-plan-reset": resetTitle,
    "row-plan-reset-short": resetTitle,
  }};
}
