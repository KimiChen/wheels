// SPDX-License-Identifier: Apache-2.0
// 展示格式化。所有「质量未知」（null / undefined / 非有限数）一律显示「未知」，
// 不得伪造为 0；大整数以后端十进制字符串到达，超出 Number 安全范围时保留原始
// 字符串并附 BigInt 换算的近似值。

export const UNKNOWN = "未知";

const BYTE_UNITS = ["B", "KB", "MB", "GB", "TB", "PB", "EB"];

export function isNil(value) {
  return value === null || value === undefined;
}

function humanize(bytes, suffix = "") {
  const sign = bytes < 0 ? "-" : "";
  let value = Math.abs(bytes);
  let unit = 0;
  while (value >= 1024 && unit < BYTE_UNITS.length - 1) {
    value /= 1024;
    unit += 1;
  }
  const text =
    unit === 0
      ? String(Math.round(value))
      : value >= 100
        ? value.toFixed(0)
        : value >= 10
          ? value.toFixed(1)
          : value.toFixed(2);
  // 去掉小数末尾的 0：「1.00 GB」显示为「1 GB」。
  const trimmed = text.includes(".") ? String(Number(text)) : text;
  return `${sign}${trimmed} ${BYTE_UNITS[unit]}${suffix}`;
}

// 十进制字符串超出 Number 安全范围时用 BigInt 取近似值，同时保留原始字符串。
function humanizeBig(raw, suffix = "") {
  try {
    const value = BigInt(raw);
    const sign = value < 0n ? "-" : "";
    let rest = value < 0n ? -value : value;
    let unit = 0;
    while (rest >= 1024n && unit < BYTE_UNITS.length - 1) {
      rest /= 1024n;
      unit += 1;
    }
    const divisor = 1024n ** BigInt(unit);
    const tenth = ((value < 0n ? -value : value) * 10n) / divisor;
    const approx = `${sign}${Number(tenth) / 10} ${BYTE_UNITS[unit]}${suffix}`;
    return `${raw}（约 ${approx}）`;
  } catch {
    return raw;
  }
}

// 解析十进制字符串或数字；返回 Number 或对超长字符串走 BigInt 近似。
function toFiniteNumber(value) {
  if (isNil(value) || value === "") return null;
  if (typeof value === "string") {
    if (!/^-?\d+(\.\d+)?$/.test(value.trim())) return null;
    const n = Number(value);
    if (Number.isSafeInteger(n)) return n;
    return Number.isFinite(n) && Math.abs(n) < Number.MAX_SAFE_INTEGER
      ? n
      : { overflow: value.trim() };
  }
  const n = Number(value);
  return Number.isFinite(n) ? n : null;
}

/** 字节量：十进制字符串 / 数字 / null。 */
export function fmtBytes(value) {
  const parsed = toFiniteNumber(value);
  if (parsed === null) return UNKNOWN;
  if (typeof parsed === "object") return humanizeBig(parsed.overflow);
  return humanize(parsed);
}

/** 速率：B/s 浮点，可为 null。 */
export function fmtRate(value) {
  const parsed = toFiniteNumber(value);
  if (parsed === null) return UNKNOWN;
  if (typeof parsed === "object") return humanizeBig(parsed.overflow, "/s");
  return humanize(parsed, "/s");
}

/** 整型计数（tcp / udp / procs 等），null → 未知。 */
export function fmtCount(value) {
  const parsed = toFiniteNumber(value);
  if (parsed === null) return UNKNOWN;
  if (typeof parsed === "object") return humanizeBig(parsed.overflow);
  return Math.round(parsed).toLocaleString("zh-CN");
}

/** CPU 百分比：0-100 浮点，null → 未知。 */
export function fmtCpu(value) {
  const parsed = toFiniteNumber(value);
  if (parsed === null || typeof parsed === "object") return UNKNOWN;
  return `${parsed.toFixed(1)}%`;
}

/** TCP 探测失败率：0..1 浮点 → 百分比；null（无样本）→ 未知。 */
export function fmtFailRate(value) {
  const parsed = toFiniteNumber(value);
  if (parsed === null || typeof parsed === "object") return UNKNOWN;
  const clamped = Math.min(1, Math.max(0, parsed));
  return `${(clamped * 100).toFixed(1)}%`;
}

/** TCP 握手延迟：毫秒，null（无样本）→ 未知。 */
export function fmtLatency(value) {
  const parsed = toFiniteNumber(value);
  if (parsed === null || typeof parsed === "object" || parsed < 0) {
    return UNKNOWN;
  }
  return `${Math.round(parsed)} ms`;
}

/**
 * 两个十进制字符串 / 数字字节量求和：任一为 null → null（调用方显示未知）。
 * 均在 Number 安全范围时返回 Number，否则用 BigInt 求和后返回十进制字符串。
 */
export function sumByteValues(a, b) {
  if (isNil(a) || isNil(b)) return null;
  const pa = toFiniteNumber(a);
  const pb = toFiniteNumber(b);
  if (pa === null || pb === null) return null;
  if (typeof pa === "object" || typeof pb === "object") {
    try {
      return (BigInt(String(a).trim()) + BigInt(String(b).trim())).toString();
    } catch {
      return null;
    }
  }
  return pa + pb;
}

/** 占比：used / total（十进制字符串），total 为 0 或任一未知 → 未知。 */
export function fmtPercent(used, total) {
  const u = toFiniteNumber(used);
  const t = toFiniteNumber(total);
  if (
    u === null ||
    t === null ||
    typeof u === "object" ||
    typeof t === "object" ||
    t <= 0
  ) {
    return UNKNOWN;
  }
  return `${((u / t) * 100).toFixed(1)}%`;
}

/** 占比数值（供 <progress> 使用），未知返回 null。 */
export function percentValue(used, total) {
  const u = toFiniteNumber(used);
  const t = toFiniteNumber(total);
  if (
    u === null ||
    t === null ||
    typeof u === "object" ||
    typeof t === "object" ||
    t <= 0
  ) {
    return null;
  }
  return Math.min(100, Math.max(0, (u / t) * 100));
}

/** 「已用 / 总量」对。 */
export function fmtPair(used, total) {
  if (isNil(used) && isNil(total)) return UNKNOWN;
  return `${fmtBytes(used)} / ${fmtBytes(total)}`;
}

/** 负载：[1, 5, 15] 分钟。 */
export function fmtLoad(load) {
  if (!Array.isArray(load) || load.length !== 3) return UNKNOWN;
  return load
    .map((v) => (Number.isFinite(Number(v)) ? Number(v).toFixed(2) : UNKNOWN))
    .join(" · ");
}

/** 运行时长：秒 → 「3 天 4 小时」。 */
export function fmtUptime(seconds) {
  const parsed = toFiniteNumber(seconds);
  if (parsed === null || typeof parsed === "object" || parsed < 0) {
    return UNKNOWN;
  }
  const s = Math.floor(parsed);
  const days = Math.floor(s / 86400);
  const hours = Math.floor((s % 86400) / 3600);
  const minutes = Math.floor((s % 3600) / 60);
  if (days > 0) return `${days} 天 ${hours} 小时`;
  if (hours > 0) return `${hours} 小时 ${minutes} 分`;
  if (minutes > 0) return `${minutes} 分 ${s % 60} 秒`;
  return `${s} 秒`;
}

/** 上报间隔：秒 → 「5 秒」「1 分钟」。 */
export function fmtInterval(seconds) {
  const parsed = toFiniteNumber(seconds);
  if (parsed === null || typeof parsed === "object" || parsed <= 0) {
    return UNKNOWN;
  }
  const s = Math.round(parsed);
  if (s % 3600 === 0) return `${s / 3600} 小时`;
  if (s % 60 === 0) return `${s / 60} 分钟`;
  return `${s} 秒`;
}

/** 相对时间：ts 为 Unix 秒，now 为服务端当前秒。 */
export function fmtAgo(ts, now) {
  const t = toFiniteNumber(ts);
  if (t === null || typeof t === "object") return UNKNOWN;
  const n = toFiniteNumber(now);
  if (n === null || typeof n === "object") return fmtClock(t);
  const diff = Math.max(0, Math.floor(n - t));
  if (diff < 10) return "刚刚";
  if (diff < 60) return `${diff} 秒前`;
  if (diff < 3600) return `${Math.floor(diff / 60)} 分钟前`;
  if (diff < 86400) return `${Math.floor(diff / 3600)} 小时前`;
  return `${Math.floor(diff / 86400)} 天前`;
}

/** 绝对时间：本地时区「2026-09-26 10:00:01」。 */
export function fmtClock(ts) {
  const t = toFiniteNumber(ts);
  if (t === null || typeof t === "object") return UNKNOWN;
  const d = new Date(t * 1000);
  const pad = (n) => String(n).padStart(2, "0");
  return (
    `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ` +
    `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`
  );
}

/** 普通文本：null / 空串 → 未知。 */
export function fmtText(value) {
  if (isNil(value) || value === "") return UNKNOWN;
  return String(value);
}
