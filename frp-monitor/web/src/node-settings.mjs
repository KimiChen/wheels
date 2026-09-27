import {UNKNOWN, bytes, cumulative} from "./format.mjs";

export const trafficModes = {max: "Max", total: "total", rx: "rx", tx: "tx"};
const gib = 1073741824n;
const maxInteger = 9223372036854775807n;

export function dateTime(raw) {
  if (!Number.isSafeInteger(raw)) return UNKNOWN;
  const date = new Date(raw);
  return Number.isNaN(date.getTime()) ? UNKNOWN : date.toLocaleString("zh-CN", {hour12: false});
}
export function localDateInput(raw) {
  if (!Number.isSafeInteger(raw)) return "";
  const date = new Date(raw), pad = value => String(value).padStart(2, "0");
  if (Number.isNaN(date.getTime())) return "";
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}`;
}
export function currencyDigits(currency) {
  if (typeof currency !== "string" || !/^[A-Z]{3}$/.test(currency)) throw new Error("币种请填写三个大写字母，如 CNY、USD。");
  return new Intl.NumberFormat("zh-CN", {style: "currency", currency}).resolvedOptions().maximumFractionDigits;
}
function scaledText(value, divisor) {
  let rest = value % divisor, fraction = "";
  while (rest !== 0n && fraction.length < 40) { rest *= 10n; fraction += String(rest / divisor); rest %= divisor; }
  return `${value / divisor}${fraction ? `.${fraction}` : ""}`;
}
export function priceInput(raw, currency) {
  const value = cumulative(raw);
  if (value === null) return "";
  try { return scaledText(value, 10n ** BigInt(currencyDigits(currency))); } catch { return ""; }
}
export function priceMinor(raw, currency) {
  if (raw.trim() === "") return null;
  const digits = currencyDigits(currency), pattern = new RegExp(`^(?:0|[1-9][0-9]*)(?:\\.[0-9]{1,${Math.max(1, digits)}})?$`);
  if (!pattern.test(raw) || (!digits && raw.includes("."))) throw new Error(`金额必须为非负数，${currency} 最多保留 ${digits} 位小数。`);
  const [whole, fraction = ""] = raw.split(".");
  const value = BigInt(whole) * 10n ** BigInt(digits) + BigInt(fraction.padEnd(digits, "0") || "0");
  if (value > maxInteger) throw new Error("金额超出允许范围。");
  return String(value);
}
export function gibInput(raw) { const value = cumulative(raw); return value === null ? "" : scaledText(value, gib); }
export function gibBytes(raw) {
  const text = raw.trim();
  if (!/^(?:0|[1-9][0-9]*)(?:\.[0-9]{1,40})?$/.test(text) || text.length > 100) throw new Error("流量请填写非负数字，单位为 GiB。");
  const [whole, fraction = ""] = text.split(".");
  return String(BigInt(whole) * gib + BigInt(fraction || "0") * gib / 10n ** BigInt(fraction.length));
}
export function billingText(billing) {
  if (!billing) return "";
  const amount = priceInput(billing.price_minor, billing.currency);
  const fee = amount === "" ? "费用未设置" : `${amount} ${billing.currency}${billing.billing_cycle ? ` / ${billing.billing_cycle}` : ""}`;
  return `${fee} · ${billing.expires_at_ms == null ? "到期未设置" : `${dateTime(billing.expires_at_ms)} 到期`}`;
}
export function todayText(today) {
  return {rx: bytes(cumulative(today?.rx_bytes)), tx: bytes(cumulative(today?.tx_bytes)),
    note: today ? `${today.day || "日期未知"} · 主控时区 ${today.timezone || "未知"}${today.partial ? " · 统计不完整" : ""}` : "等待今日统计"};
}
export function planText(plan) {
  const used = cumulative(plan?.used_bytes), quota = cumulative(plan?.quota_bytes);
  const unlimited = plan?.quota_bytes === null;
  const tenths = used === null ? null : unlimited ? 0n : quota === null || quota === 0n ? null : used * 1000n / quota;
  const percent = tenths === null ? UNKNOWN : `${tenths / 10n}.${tenths % 10n}%`;
  return {used: bytes(used), quota: unlimited ? "∞" : bytes(quota), percent,
    meter: tenths === null ? null : Number(tenths > 1000n ? 1000n : tenths) / 10,
    remaining: used === null || quota === null ? UNKNOWN : `${used > quota ? "超出 " : "剩余 "}${bytes(used > quota ? used - quota : quota - used)}`,
    note: plan ? `${trafficModes[plan.mode] ?? "未知类型"} · ${plan.reset_mode === "manual" ? "手动重置" : `每月 ${plan.reset_day} 日重置 · ${plan.reset_timezone || "UTC"}`}${plan.partial ? " · 统计不完整" : ""}` : "套餐未公开"};
}

// Values are read from the form before controls are disabled by a mutation.
// Empty used input deliberately omits recalibration so polling cannot overwrite new traffic.
export function settingsRequest(values, revision) {
  if (!Number.isSafeInteger(revision) || revision < 1) throw new Error("请重新读取节点设置后保存。");
  const name = values.name.trim(), currency = values.currency.trim().toUpperCase();
  if (!name || new TextEncoder().encode(name).length > 128) throw new Error("节点名称不能为空，且不能超过 128 字节。");
  const resetDay = Number(values.traffic_reset_day), zone = values.traffic_reset_timezone.trim();
  if (!Object.hasOwn(trafficModes, values.traffic_mode) || !["monthly", "manual"].includes(values.traffic_reset_mode) || !Number.isInteger(resetDay) || resetDay < 1 || resetDay > 31) throw new Error("请检查套餐类型和每月重置日期。");
  try { new Intl.DateTimeFormat("zh-CN", {timeZone: zone}).format(); } catch { throw new Error("重置时区无效，请填写 UTC、Asia/Shanghai 等时区。"); }
  const expires = values.expires_at_ms === "" ? null : new Date(values.expires_at_ms).getTime();
  if (expires !== null && !Number.isSafeInteger(expires)) throw new Error("到期时间无效。");
  const amount = priceMinor(values.price, currency);
  const result = {config_revision: revision, name, public_note: values.public_note, private_note: values.private_note,
    is_public: Boolean(values.is_public), publish_billing: Boolean(values.publish_billing), publish_traffic_plan: Boolean(values.publish_traffic_plan),
    price_minor: amount, currency: amount === null ? null : currency, billing_cycle: values.billing_cycle.trim() || null,
    expires_at_ms: expires, renewal_note: values.renewal_note,
    traffic_quota_bytes: values.traffic_quota.trim() === "" ? null : gibBytes(values.traffic_quota), traffic_mode: values.traffic_mode,
    traffic_reset_mode: values.traffic_reset_mode, traffic_reset_day: resetDay, traffic_reset_timezone: zone};
  if (values.traffic_used.trim() !== "") result.traffic_used_bytes = gibBytes(values.traffic_used);
  return result;
}
