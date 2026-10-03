// Store edits are instructions. Native objects and secret values never become
// a browser-side replacement configuration or persistent draft.
export const proxyTypes = ["tcp", "udp", "http", "https", "tcpmux", "stcp", "sudp", "xtcp"];
export const visitorTypes = ["stcp", "sudp", "xtcp"];
export const operationLabels = {draft: "草案已记录", validated: "校验通过", prepared: "预览已准备", applying: "正在应用", verifying: "正在确认", confirmed: "配置已确认", rejected: "校验拒绝", conflict: "版本冲突", failed: "操作失败", outcome_unknown: "结果待核对", cancelled: "已取消", rolling_back: "正在回退", rolled_back: "已回退", rollback_failed: "回退失败，需本机处理"};
export const activeStates = new Set(["draft", "validated", "prepared", "applying", "verifying", "outcome_unknown", "rolling_back", "rollback_failed"]);
export const configErrors = {
  unsupported: "此节点未启用 Store 托管能力，请在 Agent 本机按维护说明配置并重启。",
  unavailable: "节点配置通道暂不可用；稍后查询实际结果。", busy: "已有配置操作正在处理，请稍后核对。",
  audit_capacity: "审计记录已达到新操作准入上限，请查看配置审计的保留与容量状态；已有操作仍可查询和恢复。",
  managed_capacity: "Agent 本地快照或操作记录已达到容量上限，本次配置未应用。请在本机核对保留策略与恢复材料后重新准备。",
  conflict: "配置版本已变化，请取消旧预览并重新读取比较。", source_drift: "原生配置来源已变化，需本机核对后重新读取。",
  ownership_conflict: "文件与 Store 存在同名对象，请先清理来源。", source_conflict: "配置来源冲突，请先在本机核对。",
  invalid_field: "字段值不符合该类型要求。", validation_failed: "原生整集校验未通过，请检查端口、引用和必填字段。",
  invalid_config: "配置请求不完整或格式不正确。", field_read_only: "此字段暂不支持远程编辑。",
  source_read_only: "当前恢复或来源状态只允许核对，请完成本机维护后重新读取。",
  verify_failed: "运行资源核对失败；请查看资源详情中的本次注册或本地启动错误，并核对回退结果。",
  runtime_failed: "原生运行时应用未完成，请核对回退结果与本机恢复记录。",
  expired: "预览或操作已到期，请查询最终结果后重新准备。", timeout: "请求超时，实际结果仍需查询。",
  connection_lost: "管理连接已断开，Agent 会继续本地确认或回退。", outcome_unknown: "结果暂不明确，请查询 Agent 本地记录。",
  service_mismatch: "服务身份已变化，请在本机核对恢复状态。", operation_not_found: "Agent 未找到该操作，不能据此推断配置未改变。",
  rollback_failed: "本地回退未确认，请保留恢复材料并在本机处理。", invalid_request: "请求不符合配置管理约束。"
};
export function resultReason(agent) {
  if (!agent?.error_code) return "";
  if (agent.error_code === "recovered") return "已根据 Agent 本机事务日志完成恢复核对。";
  return configErrors[agent.error_code] ?? "本次操作有未确认的原生结果，请查看状态与资源详情。";
}
const validDigest = value => typeof value === "string" && /^[0-9a-f]{64}$/.test(value);
const validID = value => typeof value === "string" && /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(value);
const byteLength = value => new TextEncoder().encode(value).length;
function requireName(value) {
  if (typeof value !== "string" || !value || byteLength(value) > 256 || /[\p{Cc}\p{Cf}]/u.test(value)) throw new Error("名称需为 1–256 字节的可见文字。");
  return value;
}
export function fieldSpecs(kind, type) {
  if (!(kind === "proxy" ? proxyTypes : kind === "visitor" ? visitorTypes : []).includes(type)) throw new Error("不支持的对象类型。");
  const fields = {enabled: "bool", "transport.useEncryption": "bool", "transport.useCompression": "bool"};
  const add = keys => keys.split(" ").forEach(key => { fields[key] = "string"; });
  if (kind === "proxy") {
    add("localIP transport.bandwidthLimit transport.bandwidthLimitMode transport.proxyProtocolVersion"); fields.localPort = "port";
    if (["tcp", "udp"].includes(type)) fields.remotePort = "remote_port";
    if (["http", "https", "tcpmux"].includes(type)) { fields.customDomains = "strings"; add("subdomain"); }
    if (type === "http") { fields.locations = "strings"; add("httpUser hostHeaderRewrite routeByHTTPUser"); }
    if (type === "tcpmux") add("multiplexer httpUser routeByHTTPUser");
    if (["stcp", "sudp", "xtcp"].includes(type)) fields.allowUsers = "strings";
  } else {
    add("serverUser serverName bindAddr"); fields.bindPort = "bind_port";
    if (type === "xtcp") {
      add("protocol fallbackTo"); fields.keepTunnelOpen = "bool";
      for (const key of ["maxRetriesAnHour", "minRetryInterval", "fallbackTimeoutMs"]) fields[key] = "nonnegative";
    }
  }
  return fields;
}
export function secretPaths(kind, type) {
  if (kind === "visitor") return ["secretKey"];
  return [...(["stcp", "sudp", "xtcp"].includes(type) ? ["secretKey"] : ["http", "tcpmux"].includes(type) ? ["httpPassword"] : []), "loadBalancer.groupKey"];
}
export function fieldValue(spec, text) {
  if (text === "") return null;
  if (spec === "bool") {
    if (!["true", "false"].includes(text)) throw new Error("布尔字段应选择默认、启用或关闭。");
    return text === "true";
  }
  if (["port", "remote_port", "bind_port", "nonnegative"].includes(spec)) {
    if (!/^-?\d+$/.test(text)) throw new Error("端口与重试参数必须为整数。");
    const value = Number(text), min = spec === "port" ? 1 : spec === "bind_port" ? -1 : 0, max = spec === "nonnegative" ? 2147483647 : 65535;
    if (!Number.isSafeInteger(value) || value < min || value > max || spec === "bind_port" && value === 0) throw new Error("端口或重试参数超出允许范围；Visitor 非监听模式可填 -1。");
    return value;
  }
  if (spec === "strings") {
    const values = text.split(/\r?\n/u).map(value => value.trim()).filter(Boolean);
    if (values.length > 32 || values.some(value => byteLength(value) > 1024 || /\p{Cc}/u.test(value))) throw new Error("列表最多 32 项，每项不超过 1024 字节。");
    return values;
  }
  if (byteLength(text) > 1024 || /\p{Cc}/u.test(text)) throw new Error("字段最长 1024 字节，不能包含控制字符。");
  return text;
}
export function inputText(value) { return value === null || value === undefined ? "" : Array.isArray(value) ? value.join("\n") : String(value); }
export function readInventory(data, nodeID) {
  const inv = data?.inventory;
  if (!data || data.node_id !== nodeID || !validID(data.service_id) || !inv || !validDigest(inv.revision) || !validDigest(inv.context_revision) || !["ready", "read_only"].includes(inv.state) || !Array.isArray(inv.objects) || inv.objects.length > 1024 || !Array.isArray(inv.issues)) throw new Error("配置盘点响应无效。");
  for (const item of inv.objects) {
    requireName(item?.name); fieldSpecs(item.kind, item.type);
    if (!Array.isArray(item.fields) || !Array.isArray(item.secrets) || !Array.isArray(item.read_only_fields) || !Array.isArray(item.issues)) throw new Error("配置对象响应无效。");
  }
  return data;
}
export function makeEdit({mode, kind, type, name, source, inputs = {}, secrets = {}}, uuid = () => crypto.randomUUID()) {
  requireName(name);
  if (!["create", "update", "clone", "rename", "delete", "enable", "disable"].includes(mode)) throw new Error("不支持的编辑动作。");
  if (mode !== "create" && (!source || !source.writable || source.source !== "store" || source.kind !== kind || source.type !== type)) throw new Error("此对象需通过原生配置维护。");
  if (["clone", "rename"].includes(mode) && name === source.name) throw new Error("复制或改名需要新的名称。");
  if (!["create", "clone", "rename"].includes(mode) && name !== source.name) throw new Error("请使用改名操作更改名称。");
  const change = {operation: ["clone", "rename"].includes(mode) ? "create" : mode, kind, name, type: ["create", "clone", "rename"].includes(mode) ? type : "", fields: [], secrets: []};
  if (["clone", "rename"].includes(mode)) change.clone_from = source.name;
  const secretValues = [];
  if (["create", "update", "clone", "rename"].includes(mode)) {
    const specs = fieldSpecs(kind, type), before = new Map((source?.fields ?? []).map(field => [field.path, field.value]));
    for (const [path, text] of Object.entries(inputs)) {
      if (!Object.hasOwn(specs, path)) throw new Error("包含不支持的字段。");
      const value = fieldValue(specs[path], text);
      if (mode === "create" ? value !== null : JSON.stringify(value) !== JSON.stringify(before.get(path) ?? null)) change.fields.push({path, value});
    }
    for (const [path, secret] of Object.entries(secrets)) {
      if (!secretPaths(kind, type).includes(path) || !["keep", "clear", "replace", "reference"].includes(secret.mode)) throw new Error("秘密字段操作无效。");
      if (secret.mode === "replace") {
        if (typeof secret.value !== "string" || !secret.value || byteLength(secret.value) > 1024 || /[\0\r\n]/u.test(secret.value)) throw new Error("新秘密需为 1–1024 字节且不含换行。");
        const reference = uuid(); if (!validID(reference)) throw new Error("浏览器无法生成安全的秘密引用。");
        change.secrets.push({path, mode: "reference", reference}); secretValues.push({reference, value: secret.value});
      } else {
        if (secret.mode === "reference" && !validID(secret.value)) throw new Error("本机秘密引用必须为有效的 UUID。");
        change.secrets.push({path, mode: secret.mode, reference: secret.mode === "reference" ? secret.value : ""});
      }
    }
  }
  if (secretValues.length > 2) throw new Error("一次操作最多写入两个新秘密。");
  if (mode === "update" && !change.fields.length && change.secrets.every(secret => secret.mode === "keep")) throw new Error("尚未修改任何字段。");
  const changes = mode === "rename" ? [{operation: "delete", kind, name: source.name, type: "", fields: [], secrets: []}, change] : [change];
  return {changes, secret_values: secretValues};
}
export function resultFacts(agent) {
  if (!agent) return [["配置已持久化", "未知"], ["运行时已加载", "未知"], ["代理登记 / Visitor 监听", "未知"], ["业务连通", "未测试"], ["回退材料", "状态未知"]];
  const yes = value => value === true ? "已确认" : "未确认";
  return [["配置已持久化", agent.state === "rolled_back" ? "已恢复旧版本" : yes(agent.store_persisted)], ["运行时已加载", yes(agent.runtime_loaded)], ["代理登记 / Visitor 监听", yes(agent.resources_ready)], ["业务连通", agent.business_checked === true ? "已验证" : "未测试"], ["回退材料", ({retained:"观察时仍保留",expired:"已按保留期限清理",unknown:"状态未知"})[operationMaterials({agent}).state]]];
}
export function operationMaterials(result) {
  const agent = result?.agent;
  if (agent?.materials_state === "retained") return {state:"retained"};
  if (agent?.materials_state === "expired" && Number.isSafeInteger(agent.materials_expired_at_ms) && agent.materials_expired_at_ms > 0 && agent.materials_expiry_reason === "ttl") return {state:"expired",expiredAt:agent.materials_expired_at_ms};
  return {state:"unknown"};
}
export function canRollback(result) {
  return ["confirmed", "rollback_failed"].includes(result?.operation?.state) && operationMaterials(result).state !== "expired";
}
export function canApply(result, preview, now = Date.now()) {
  return result?.operation?.state === "prepared" && Number(result.operation.deadline_at_ms) > now && validDigest(preview?.candidate_digest) && preview.candidate_digest === result.operation.candidate_digest && preview.base_revision === result.operation.base_revision && validDigest(preview.context_revision);
}
export function readRestore(data, nodeID) {
  const value = data?.restore;
  const fail = () => { throw new Error("恢复核对响应无效。"); };
  if (data?.node_id !== nodeID || !validID(data.service_id) || !value || !["none", "pending", "verified", "acknowledged", "confirmed"].includes(value.state) || !Number.isSafeInteger(data.received_at_ms) || data.received_at_ms <= 0 || typeof data.code !== "string" || !/^[a-z_]{1,64}$/.test(data.code)) fail();
  const restore = {state: value.state};
  if (value.state !== "none") {
    for (const field of ["epoch", "backup_service_id"]) if (!validID(value[field])) fail();
    if (value.replaced_service_id && !validID(value.replaced_service_id)) fail();
    for (const field of ["manifest_digest", "context_revision"]) if (!validDigest(value[field])) fail();
    if (!Number.isInteger(value.operations_count) || value.operations_count < 0 || value.operations_count > 1024) fail();
    if (value.state !== "pending" && (!validDigest(value.store_digest) || value.runtime_loaded !== true || value.resources_ready !== true)) fail();
    if (["acknowledged", "confirmed"].includes(value.state) && !validID(value.acknowledgement_id)) fail();
    for (const field of ["epoch", "backup_service_id", "replaced_service_id", "manifest_digest", "context_revision", "store_digest", "acknowledgement_id", "runtime_loaded", "resources_ready", "operations_count"]) restore[field] = value[field];
  }
  let receipt = null, active = null;
  if (data.receipt != null) {
    const item = data.receipt;
    if (!validID(item.id) || item.node_id !== nodeID || item.service_id !== data.service_id || item.epoch !== restore.epoch || item.manifest_digest !== restore.manifest_digest || item.context_revision !== restore.context_revision || item.store_digest !== restore.store_digest || !["pending", "acknowledged"].includes(item.state) || !Number.isSafeInteger(item.version) || item.version < 1) fail();
    receipt = {id: item.id, state: item.state, version: item.version};
  }
  if (data.active_operation != null) {
    const item = data.active_operation;
    if (!validID(item.operation_id) || item.node_id !== nodeID || !validID(item.service_id) || !activeStates.has(item.state) || !Number.isSafeInteger(item.version) || item.version < 1) fail();
    active = {operation_id: item.operation_id, node_id: item.node_id, service_id: item.service_id, state: item.state, version: item.version};
  }
  // Copy only safe contract fields; unexpected server properties never become
  // persistent panel state or a subsequent acknowledgement request.
  return {code: data.code, node_id: nodeID, service_id: data.service_id, received_at_ms: data.received_at_ms, restore, receipt, active_operation: active};
}
export function canAcknowledgeRestore(data, now = Date.now()) {
  const restore = data?.restore, active = data?.active_operation;
  return !!(data?.code === "ok" && data.received_at_ms <= now && now - data.received_at_ms <= 60000 &&
    ["verified", "acknowledged"].includes(restore?.state) && data.receipt?.state !== "acknowledged" &&
    (!active || [restore.backup_service_id, restore.replaced_service_id].includes(active.service_id)));
}
export function restoreAcknowledgement(data) {
  return {service_id: data.service_id, epoch: data.restore.epoch, manifest_digest: data.restore.manifest_digest,
    context_revision: data.restore.context_revision, store_digest: data.restore.store_digest,
    expected_active_operation_id: data.active_operation?.operation_id ?? "", expected_active_version: data.active_operation?.version ?? 0};
}
const restorationBlocksWrites = data => ["pending", "verified", "acknowledged"].includes(data?.restore?.state);

// Clearing/switching invalidates every in-flight reply. Plaintext secrets are
// passed directly to request and are not retained in this controller's state.
export function createConfigController({request, onChange = () => {}, now = () => Date.now(), uuid = () => crypto.randomUUID()}) {
  let epoch = 0;
  const state = {nodeID: null, inventory: null, operations: [], result: null, preview: null, restoration: null, pending: false, message: "", draft: false, online: false, needsReload: false};
  const notify = () => onChange(state);
  const path = () => `/api/admin/v1/nodes/${encodeURIComponent(state.nodeID)}/configuration`;
  async function perform(work) {
    if (state.pending) return;
    const generation = epoch; state.pending = true; state.message = ""; notify();
    try { const result = await work(); if (generation === epoch) { state.pending = false; notify(); } return result; }
    catch (error) { if (generation === epoch) { state.message = configErrors[error.code] ?? configErrors[error.message] ?? (error.status === 409 ? configErrors.conflict : error.status === 503 ? configErrors.unavailable : "请求未完成，请查询操作记录；不要重复应用。"); state.pending = false; notify(); } }
  }
  async function load() {
    const generation = epoch, nodeID = state.nodeID, base = path();
    const [inventory, history] = await Promise.allSettled([request(base), request(`${base}/operations`)]);
    if (generation !== epoch) return;
    state.inventory = inventory.status === "fulfilled" ? readInventory(inventory.value, nodeID) : null;
    state.needsReload = false;
    state.operations = history.status === "fulfilled" && Array.isArray(history.value?.operations) ? history.value.operations.slice(0, 100) : [];
    if (inventory.status === "rejected") throw inventory.reason;
    if (history.status === "rejected") throw history.reason;
  }
  function accept(result) {
    if (!result?.operation || result.operation.node_id !== state.nodeID || !validID(result.operation.operation_id) || !Object.hasOwn(operationLabels, result.operation.state)) throw new Error("invalid_response");
    if (state.result?.operation?.operation_id !== result.operation.operation_id) state.preview = null;
    state.result = result;
    if (result.preview) state.preview = result.preview;
    if (result.code && result.code !== "ok") state.message = result.code === "operation_not_found" && operationMaterials(result).state === "expired" ? "本次操作的回退材料已过保留期限，无法直接回退；已记录的配置结果仍保留。" : configErrors[result.code] ?? "操作结果需核对，请查看状态与时间线。";
    if (["confirmed", "rolled_back", "rollback_failed", "conflict"].includes(result.operation.state)) state.needsReload = true;
    state.operations = [result.operation, ...state.operations.filter(op => op.operation_id !== result.operation.operation_id)].slice(0, 100);
  }
  return {
    state,
    clear() { epoch++; Object.assign(state, {nodeID: null, inventory: null, operations: [], result: null, preview: null, restoration: null, pending: false, message: "", draft: false, online: false, needsReload: false}); notify(); },
    setOnline(value) { state.online = value === true; notify(); },
    async open(nodeID, online = true) { if (state.nodeID === nodeID) { state.online = online; notify(); return; } this.clear(); state.nodeID = nodeID; state.online = online; await perform(load); },
    async reload() { if (state.pending) return; state.draft = false; state.preview = null; await perform(load); },
    draft(value = true) { state.draft = value; state.preview = null; notify(); },
    async inspectRestore() {
      if (!state.online || state.pending) return;
      const generation = epoch; state.restoration = null;
      await perform(async () => { const response = await request(`${path()}/restore`); if (generation === epoch) state.restoration = readRestore(response, state.nodeID); });
    },
    async acknowledgeRestore() {
      if (!state.online || !canAcknowledgeRestore(state.restoration, now())) return;
      const generation = epoch, body = restoreAcknowledgement(state.restoration);
      await perform(async () => {
        const response = await request(`${path()}/restore/acknowledge`, {method: "POST", body});
        if (generation !== epoch) return;
        state.restoration = readRestore(response, state.nodeID); state.needsReload = true;
      });
    },
    async prepare(edit) {
      if (!state.inventory || state.inventory.inventory.state !== "ready" || !state.online || restorationBlocksWrites(state.restoration) || state.needsReload || state.operations.some(op => activeStates.has(op.state))) return;
      const generation = epoch, body = {service_id: state.inventory.service_id, base_revision: state.inventory.inventory.revision, idempotency_key: uuid(), deadline_at_ms: now() + 300000, ...edit};
      await perform(async () => { const result = await request(`${path()}/operations`, {method: "POST", body}); if (generation !== epoch) return; accept(result); state.draft = false; });
    },
    async query(operationID = state.result?.operation?.operation_id) {
      if (!validID(operationID)) return;
      const generation = epoch;
      await perform(async () => { const result = await request(`${path()}/operations/${operationID}`); if (generation === epoch) accept(result); });
    },
    async action(action) {
      if (!["apply", "cancel", "rollback"].includes(action) || !state.result || !state.online || restorationBlocksWrites(state.restoration)) return;
      if (action === "apply" && !canApply(state.result, state.preview, now())) return;
      if (action === "rollback" && !canRollback(state.result)) return;
      const generation = epoch, op = state.result.operation, agent = state.result.agent, preview = state.preview;
      const body = {expected_version: op.version, context_revision: preview?.context_revision ?? agent?.context_revision ?? "", candidate_digest: op.candidate_digest};
      await perform(async () => { const result = await request(`${path()}/operations/${op.operation_id}/${action}`, {method: "POST", body}); if (generation !== epoch) return; accept(result); });
    }
  };
}
