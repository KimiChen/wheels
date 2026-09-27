import {value, bytes, uint64, quality} from "./format.mjs";
import {validNodeID} from "./node-data.mjs";

export const reconciliationLabels = {matched: "已核对", unbound: "未设可信绑定", conflict: "归属冲突", mismatch: "报告与绑定不一致", missing: "服务端未登记", stale: "节点报告已过期", transient: "未配置稳定 ID", unavailable: "服务端快照不可用", ready: "可用", disabled: "未启用"};
export const clientProxyLabels = {unknown: "未知", disabled: "未启用", starting: "启动中", running: "运行中", error: "错误", closed: "已关闭"};
export const proxyLabels = {unavailable: "暂不可核对", registered: "已登记", offline: "已离线", missing: "未登记", conflict: "归属冲突", disabled: "未启用"};
export function fieldText(field) { const v = value(field); return typeof v === "string" || typeof v === "number" ? String(v) : quality(field); }
export function byteText(raw) { return bytes(uint64(raw)); }
export function adminSnapshot(raw) {
  if (!raw || !Array.isArray(raw.nodes) || raw.nodes.length > 1024 || !Number.isFinite(Date.parse(raw.generated_at)) || !["ready", "degraded"].includes(raw.credentials_state)) throw new Error("invalid_snapshot");
  const ids = new Set();
  for (const node of raw.nodes) {
    if (!node || !validNodeID(node.id) || ids.has(node.id) || typeof node.name !== "string" || node.name.length > 128 || !["online", "offline", "waiting"].includes(node.session) || !["fresh", "stale", "waiting"].includes(node.freshness)) throw new Error("invalid_node");
    ids.add(node.id);
  }
  return raw;
}
export function probeDocument(raw) {
  if (!raw || !Number.isSafeInteger(raw.version) || raw.version < 0 || !Array.isArray(raw.nodes) || raw.nodes.length > 1024) throw new Error("invalid_probes");
  const ids = new Set();
  for (const node of raw.nodes) {
    if (!node || !validNodeID(node.agent_id) || ids.has(node.agent_id) || !Array.isArray(node.tasks) || node.tasks.length > 64) throw new Error("invalid_probes");
    ids.add(node.agent_id);
    const tasks = new Set();
    for (const task of node.tasks) {
      if (!validTask(task) || tasks.has(task.id)) throw new Error("invalid_probes");
      tasks.add(task.id);
    }
  }
  return raw;
}
const bounded = (text, max) => typeof text === "string" && text.trim() !== "" && new TextEncoder().encode(text).length <= max && !/[\0\r\n]/.test(text);
function validTask(task) {
  if (!task || !bounded(task.id, 128) || !bounded(task.name, 128) || !bounded(task.target, 256) || !Number.isSafeInteger(task.interval) || task.interval < 5 || task.interval > 3600) return false;
  const target = /^(?:\[([^\]]+)\]|([^:]+)):(\d+)$/.exec(task.target);
  return Boolean(target && !/[\s/@\\]/.test(target[1] ?? target[2]) && Number(target[3]) >= 1 && Number(target[3]) <= 65535);
}
export function nextProbeDocument(current, rows, nodeIDs) {
  const previous = probeDocument(current);
  if (previous.version >= Number.MAX_SAFE_INTEGER) throw new Error("任务版本超出浏览器安全范围，请使用服务端配置工具处理。");
  const nodes = new Map();
  for (const row of rows) {
    const task = {id: row.id.trim(), name: row.name.trim(), target: row.target.trim(), interval: Number(row.interval)};
    if (!nodeIDs.has(row.agent_id)) throw new Error("请选择有效节点。");
    if (!validTask(task)) throw new Error("检查任务：ID 与名称最多 128 字节，目标为 host:port（IPv6 加方括号），间隔为 5–3600 秒整数。");
    if (!nodes.has(row.agent_id)) nodes.set(row.agent_id, []);
    const tasks = nodes.get(row.agent_id);
    if (tasks.some(item => item.id === task.id)) throw new Error("同一节点不能使用重复任务 ID。");
    if (tasks.length >= 64) throw new Error("单节点最多配置 64 个探测任务。");
    tasks.push(task);
  }
  return {version: previous.version + 1, nodes: [...nodes].map(([agent_id, tasks]) => ({agent_id, tasks}))};
}
export function errorText(error) {
  if (error?.name === "AbortError") return "请求已取消，请重试。";
  return ({400: "输入内容无效，请检查后重试。", 401: "管理会话已失效，请重新登录。", 403: "请求校验未通过，请重新登录后重试。", 404: "所选节点或功能不可用，请刷新。", 409: "配置已变更或存在冲突，请重新读取后再保存。", 413: "内容超过允许大小，请减少任务数量。", 429: "请求过于频繁，请稍后重试。", 503: "服务暂不可用或配置写入失败，请稍后重试。"})[error?.status] ?? "请求失败，请检查连接后重试。";
}

// The CSRF token lives only in this instance; GitHub OAuth credentials stay on the server.
export function adminClient({fetcher = globalThis.fetch, onExpired = () => {}, timer = setTimeout, cancel = clearTimeout} = {}) {
  let csrf = null, epoch = 0;
  const active = new Set();
  function clear() { epoch++; csrf = null; for (const controller of active) controller.abort(); active.clear(); }
  async function request(path, {method = "GET", body} = {}) {
    if (!path.startsWith("/api/admin/v1/")) throw new Error("invalid_admin_path");
    const write = method !== "GET", generation = epoch, controller = new AbortController();
    if (write && !csrf) throw Object.assign(new Error("unauthorized"), {status: 401});
    active.add(controller);
    const timeout = timer(() => controller.abort(), 10000);
    try {
      const headers = {Accept: "application/json"};
      if (body !== undefined) headers["Content-Type"] = "application/json";
      if (write) headers["X-CSRF-Token"] = csrf;
      const response = await fetcher(path, {method, headers, body: body === undefined ? undefined : JSON.stringify(body), credentials: "same-origin", cache: "no-store", signal: controller.signal, redirect: "error"});
      if (generation !== epoch || controller.signal.aborted) throw Object.assign(new Error("cancelled"), {name: "AbortError"});
      if (!response.ok) {
        if (response.status === 401 || response.status === 403) { clear(); onExpired(); }
        throw Object.assign(new Error("request_failed"), {status: response.status});
      }
      const data = response.status === 204 ? null : await response.json();
      if (generation !== epoch || controller.signal.aborted) throw Object.assign(new Error("cancelled"), {name: "AbortError"});
      return data;
    } finally { cancel(timeout); active.delete(controller); }
  }
  function acceptSession(data) {
    if (!data || typeof data.csrf_token !== "string" || data.csrf_token.length < 16 || data.csrf_token.length > 256 || !Number.isFinite(Date.parse(data.expires_at))) throw new Error("invalid_session");
    csrf = data.csrf_token; return data;
  }
  return {request, clear, session: async () => acceptSession(await request("/api/admin/v1/session")), logout: async () => { try { await request("/api/admin/v1/logout", {method: "POST"}); } finally { clear(); } }};
}
