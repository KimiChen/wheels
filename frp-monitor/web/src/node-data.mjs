import {UNKNOWN, value} from "./format.mjs";

export function validNodeID(id) { return typeof id === "string" && /^[1-9][0-9]{0,18}$/.test(id) && BigInt(id) <= 9223372036854775807n; }
export function nodeURL(id) { return validNodeID(id) ? `/node/${id}` : null; }
export function nodeGroups(node) {
  const seen = new Set();
  return (Array.isArray(node?.groups) ? node.groups : []).filter(group => {
    if (!group || !validNodeID(group.id) || typeof group.name !== "string" || !group.name.trim() || seen.has(group.id)) return false;
    seen.add(group.id); return true;
  });
}
export function groupText(node) { return nodeGroups(node).map(group => group.name).join(" · "); }
export function nodeBadgeText(node) {
  return groupText(node) || ({online: "在线", waiting: "等待", offline: "离线"})[node?.session] || UNKNOWN;
}
export function nodeID(pathname) {
  const match = /^\/node\/([^/]+)\/?$/.exec(pathname);
  if (!match) return null;
  try { const id = decodeURIComponent(match[1]); return validNodeID(id) ? id : null; } catch { return null; }
}
export function hardwareValue(field) {
  const raw = value(field);
  return typeof raw === "string" && raw.trim() ? raw.trim() : UNKNOWN;
}
export function cpuCores(hardware) {
  const n = value(hardware?.cpu_cores);
  return Number.isInteger(n) && n > 0 && n <= 4294967295 ? n : null;
}
export function hardwareText(hardware) {
  const parts = ["os", "virt", "arch"].map(key => hardwareValue(hardware?.[key])).filter(item => item !== UNKNOWN);
  return parts.length ? parts.join(" · ") : "系统信息待上报";
}
export function cpuLabel(hardware) { const n = cpuCores(hardware); return n === null ? "CPU" : `CPU ${n} 核`; }
export function cpuModel(hardware) {
  const name = hardwareValue(hardware?.cpu_name), cores = cpuCores(hardware);
  return name === UNKNOWN ? (cores === null ? UNKNOWN : `${cores} 个逻辑核心`) : `${name}${cores === null ? "" : ` × ${cores}`}`;
}
