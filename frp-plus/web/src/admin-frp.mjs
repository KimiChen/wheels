// Private administrative observations only. Never render native objects or raw
// error messages; this module selects the documented detail fields explicitly.
const sourceLabels = {file: "主配置文件", include: "include 文件", store: "原生 Store", mixed: "混合来源", unknown: "来源未知"};
const sourceStates = {active: "当前生效", overridden: "被覆盖", conflict: "来源冲突", unknown: "生效来源待确认"};
const states = {
  ready: ["详情可用", "以下入口来自配置或运行观察；实际监听地址不等于公网可达地址。"],
  unsupported: ["详情能力未启用", "当前 Agent 或服务端未协商详情能力。上方原有隧道对账仍可使用；配置通过原生方式维护。"],
  waiting: ["等待详情", "已协商详情能力，等待独立详情报告。"],
  stale: ["详情已过期", "节点离线或详情报告超时，旧入口与运行状态已隐藏；主机指标的新鲜度独立判断。"],
  busy: ["原生状态正在变更", "原生配置或运行状态的锁暂时繁忙，本次未取得完整详情。稍后自动重试，主机指标继续上报。"],
  truncated: ["详情超过限制", "对象或详情大小超过上限，无法展示完整列表。不能将本次缺失理解为没有 Proxy 或 Visitor。"],
  unavailable: ["详情暂不可用", "详情采集暂不可用，主机监控与原生隧道可继续运行。"]
};
const proxyStates = {unknown: "未知", disabled: "未启用", starting: "启动中", running: "运行中", error: "错误", closed: "已关闭"};
const visitorStates = {unknown: "未知", disabled: "未启用", configured: "已配置", starting: "启动中", listening: "本地监听中", connected: "有连接", error: "错误", closed: "已关闭"};
const connectionStates = {unknown: "未知", configured: "已配置", listening: "监听中", disabled: "未启用", error: "错误", closed: "已关闭", connecting: "连接中", connected: "已建立", unsupported: "不适用", failed: "失败", available: "可用", active: "使用中"};
const protocolLabels = {tcp: "TCP", kcp: "KCP", quic: "QUIC", websocket: "WebSocket", wss: "WSS", unknown: "未知"};
const fixedErrors = {unknown: "原因未知", unavailable: "状态暂不可用", connect_failed: "连接失败", login_failed: "FRP 登录失败", register_failed: "代理注册失败", health_check_failed: "健康检查失败", listen_failed: "本地监听失败", peer_failed: "对端连接失败", nat_traversal_failed: "NAT 穿透失败", plugin_failed: "插件启动失败", reload_failed: "配置重载失败"};
const errorStages = {control: "控制连接", login: "认证", proxy: "代理", health_check: "健康检查", visitor: "Visitor", transport: "传输", configuration: "配置"};
const boolText = value => value === true ? "启用" : value === false ? "关闭" : "未知";
const text = (value, limit = 1024, empty = "—") => typeof value === "string" && value.trim() ? value.replace(/[\u0000-\u001f\u007f-\u009f]/gu, " ").slice(0, limit) + (value.length > limit ? "…" : "") : empty;
const timestamp = value => typeof value === "string" && Number.isFinite(Date.parse(value)) ? new Date(value).toLocaleString("zh-CN", {hour12: false}) : "未知";
const list = value => Array.isArray(value) ? value : [];

export function sourceText(source, state, shadowed) {
  const covered = list(shadowed).slice(0, 3).filter(value => ["file", "include", "store"].includes(value)).map(value => sourceLabels[value]);
  return `${sourceLabels[source] ?? sourceLabels.unknown} · ${sourceStates[state] ?? sourceStates.unknown}${covered.length ? `；覆盖 ${covered.join("、")}` : ""}`;
}

export function errorSummary(error) {
  if (!error || typeof error !== "object") return "未报告错误";
  const recovered = typeof error.recovered_at === "string" && Number.isFinite(Date.parse(error.recovered_at));
  return `${recovered ? "已恢复" : "最近错误"} · ${errorStages[error.stage] ?? "未知阶段"} · ${fixedErrors[error.code] ?? fixedErrors.unknown}${error.at ? ` · ${timestamp(error.at)}` : ""}${recovered ? `；恢复于 ${timestamp(error.recovered_at)}` : ""}`;
}

export function endpointText(endpoint) {
  if (!endpoint || typeof endpoint !== "object") return "入口未知";
  if (endpoint.kind === "visitor") return "通过 Visitor 访问（无公网监听入口）";
  if (!["tcp", "udp", "http", "https", "tcpmux"].includes(endpoint.kind)) return "入口未知";
  if (typeof endpoint.subdomain === "string" && /^[a-zA-Z0-9_-]{1,253}$/.test(endpoint.subdomain)) return `子域名 ${endpoint.subdomain}（域名后缀待服务端确认）`;
  const host = typeof endpoint.host === "string" && endpoint.host.length <= 253 && !/[\s/@?#%\\]/u.test(endpoint.host) && (!endpoint.host.includes(":") || /^[a-fA-F0-9:.]+$/.test(endpoint.host)) ? endpoint.host : "";
  if (!host && !Number.isInteger(endpoint.port)) return "入口地址待确认";
  const address = host.includes(":") ? `[${host}]` : host || "服务端地址待确认";
  const port = Number.isInteger(endpoint.port) && endpoint.port >= 0 && endpoint.port <= 65535 ? endpoint.port === 0 ? "动态分配" : String(endpoint.port) : "端口待确认";
  const path = endpoint.kind === "http" && typeof endpoint.path === "string" && endpoint.path.startsWith("/") && !/[?#\u0000-\u001f\u007f-\u009f]/u.test(endpoint.path) ? text(endpoint.path) : "";
  return `${endpoint.kind.toUpperCase()} · ${address} · ${port}${path ? ` · 路由 ${path}` : ""}`;
}

export function endpointList(endpoints, source) {
  return list(endpoints).slice(0, 32).filter(endpoint => endpoint && (!source || endpoint.source === source)).map(endpointText).join("\n") || "未观测到当前入口";
}

export function serverEndpointText(proxy, registryState = "ready") {
  if (registryState !== "ready") return "服务端快照不可用";
  if (proxy?.online !== true) return "已离线，当前入口未知";
  return endpointList(proxy.endpoints, "observed");
}

export function detailModel(node, reconciliation) {
  let state = Object.hasOwn(states, node?.frp_detail_state) ? node.frp_detail_state : node?.frp_detail_state === undefined ? "unsupported" : "unavailable";
  let detail = node?.frp_detail;
  if (state === "ready" && (!detail || detail.state !== "ready" || !Array.isArray(detail.proxies) || !Array.isArray(detail.visitors))) state = "unavailable";
  if (state === "ready" && (detail.proxies.length > 512 || detail.visitors.length > 512)) state = "truncated";
  if (state === "ready" && [...detail.proxies, ...detail.visitors].some(item => !item || typeof item.name !== "string" || typeof item.type !== "string")) state = "unavailable";
  if (state !== "ready") detail = null;
  const result = {state, label: states[state][0], message: states[state][1], at: timestamp(node?.frp_detail_at), fields: [], proxies: [], visitors: []};
  if (node?.frp_detail_warning === "loading") result.message = `正在读取独立详情${detail ? "；当前显示上次记录" : ""}。${result.message}`;
  if (node?.frp_detail_warning === "failed") result.message = "独立详情读取失败，旧详情已隐藏。稍后刷新或重新打开资源详情。";
  if (!detail) return result;
  const transport = detail.transport ?? {}, configuration = detail.configuration ?? {};
  result.fields = [
    ["服务实例", text(detail.service_id, 128)],
    ["控制传输", `${protocolLabels[transport.protocol] ?? "未知"} · ${({configured: "配置值", observed: "运行观察", unknown: "来源未知"})[transport.source] ?? "来源未知"}`],
    ["FRP wire 协议", ["v1", "v2"].includes(transport.wire_protocol) ? transport.wire_protocol : "未知"],
    ["控制 TLS / TCP 复用", `${boolText(transport.tls)} / ${boolText(transport.tcp_mux)}`],
    ["配置来源", sourceText(configuration.source, configuration.source_state, configuration.shadowed_sources)],
    ["配置修订", typeof configuration.revision === "string" && /^[a-f0-9]{64}$/.test(configuration.revision) ? configuration.revision : "尚未提供"],
    ["来源最近读取", timestamp(configuration.read_at)],
    ["原生 Dashboard", configuration.dashboard_enabled === true ? "已配置监听；访问入口由管理员设置" : configuration.dashboard_enabled === false ? "未启用" : "启用状态未知"],
    ["管理能力", "只读观察；远程配置管理尚未开放"],
    ["原生对象重载", configuration.reloadable === true ? "可按原生流程重载 Proxy/Visitor；启动字段仍需重启" : configuration.reloadable === false ? "当前实例没有可用的文件 / Store 重载入口" : "尚未确认"],
    ["控制连接错误", errorSummary(detail.control_error)]
  ];
  result.proxies = detail.proxies.filter(proxy => proxy && typeof proxy === "object").map(proxy => {
    const observed = reconciliation?.state === "matched" ? list(reconciliation.proxies).find(item => item?.name === proxy.name && item?.type === proxy.type && item.server_state === "registered")?.server : null;
    return {key: `proxy:${text(proxy.type, 16)}:${text(proxy.name, 256)}`, title: `${text(proxy.name, 256)} · ${text(proxy.type, 16).toUpperCase()}`, status: proxy.enabled === false ? "未启用" : proxyStates[proxy.status] ?? "未知", fields: [
      ["配置来源", sourceText(proxy.source, proxy.source_state, proxy.shadowed_sources)], ["本地目标", text(proxy.local_target, 1024)],
      ["客户端插件", text(proxy.plugin_type, 64, "无 / 未报告")],
      ["配置期望入口", endpointList(proxy.endpoints, "configured")],
      ["节点运行观察", proxy.status === "running" && proxy.enabled !== false ? endpointList(proxy.endpoints, "observed") : "未观测到当前入口"],
      ["服务端实际入口", observed ? serverEndpointText(observed) : "尚无唯一匹配的服务端观察"],
      ["代理加密 / 压缩", `${boolText(proxy.encryption)} / ${boolText(proxy.compression)}`], ["错误情况", errorSummary(proxy.error)]
    ]};
  });
  result.visitors = detail.visitors.filter(visitor => visitor && typeof visitor === "object").map(visitor => ({
    key: `visitor:${text(visitor.type, 16)}:${text(visitor.name, 256)}`, title: `${text(visitor.name, 256)} · ${text(visitor.type, 16).toUpperCase()}`, status: visitor.enabled === false ? "未启用" : visitorStates[visitor.status] ?? "未知", fields: [
      ["配置来源", sourceText(visitor.source, visitor.source_state, visitor.shadowed_sources)],
      ["远端引用", `${text(visitor.server_user, 128, "（空用户）")} / ${text(visitor.server_name, 256)}`],
      ["本地监听配置", visitor.bind_endpoint ? endpointText(visitor.bind_endpoint) : "未提供监听地址（可能为非监听模式）"],
      ["本地监听 / 对端连接", `${connectionStates[visitor.local_state] ?? "未知"} / ${connectionStates[visitor.remote_state] ?? "未知"}`],
      ["P2P / fallback", `${connectionStates[visitor.p2p_state] ?? "未知"} / ${connectionStates[visitor.fallback_state] ?? "未知"}`],
      ["fallback 目标", text(visitor.fallback_to, 256)], ["Visitor 传输", protocolLabels[visitor.protocol] ?? "未知"],
      ["客户端插件", text(visitor.plugin_type, 64, "无 / 未报告")],
      ["加密 / 压缩", `${boolText(visitor.encryption)} / ${boolText(visitor.compression)}`], ["错误情况", errorSummary(visitor.error)]
    ]
  }));
  return result;
}

// One selected node at a time. A caller-supplied session/snapshot guard prevents
// a late response from reintroducing private data after closing or switching.
export function createFRPDetailReader({request, onChange = () => {}}) {
  let generation = 0, selected = null, value = null, pending = false, failed = false;
  return {
    clear() { generation++; selected = null; value = null; pending = false; failed = false; },
    view(node) {
      if (selected !== node.id) return {...node, frp_detail: null, frp_detail_state: node.frp_detail_state === "ready" ? "waiting" : node.frp_detail_state};
      const result = {...node, frp_detail: value?.detail ?? null, frp_detail_state: failed ? "unavailable" : value?.state ?? (node.frp_detail_state === "ready" ? "waiting" : node.frp_detail_state), frp_detail_at: value?.received_at ?? node.frp_detail_at};
      const summaryIsNewer = !value || !value.received_at || Date.parse(node.frp_detail_at) >= Date.parse(value.received_at);
      if (["unsupported", "waiting"].includes(node.frp_detail_state) || summaryIsNewer && ["stale", "busy", "unavailable", "truncated"].includes(node.frp_detail_state)) { result.frp_detail_state = node.frp_detail_state; result.frp_detail = null; }
      result.frp_detail_warning = failed ? "failed" : pending ? "loading" : null;
      return result;
    },
    async load(id, isCurrent = () => true) {
      const current = ++generation;
      if (selected !== id) value = null;
      selected = id; pending = true; failed = false; onChange();
      try {
        const next = await request(`/api/admin/v1/nodes/${encodeURIComponent(id)}/frp-detail`);
        if (current !== generation || !isCurrent()) return;
        if (!next || next.node_id !== id || !Object.hasOwn(states, next.state) || (next.received_at !== null && (typeof next.received_at !== "string" || !Number.isFinite(Date.parse(next.received_at)))) || (next.state === "ready" && (!next.detail || next.detail.state !== "ready"))) throw new Error("invalid_private_detail");
        value = next;
      } catch {
        if (current !== generation || !isCurrent()) return;
        value = null; failed = true;
      } finally {
        if (current === generation) { pending = false; if (isCurrent()) onChange(); else value = null; }
      }
    }
  };
}

export function configuredLink(raw, dashboard = false) {
  if (typeof raw !== "string" || raw.length > 2048 || /[\s\\\u0000-\u001f\u007f-\u009f]/u.test(raw)) return null;
  try {
    const parsed = new URL(raw);
    if (!parsed.hostname || parsed.username || parsed.password || parsed.search || parsed.hash || raw.includes("?") || raw.includes("#") || /[\u0000-\u001f\u007f-\u009f]/u.test(decodeURIComponent(parsed.pathname))) return null;
    if (["http:", "https:"].includes(parsed.protocol)) {
      if (dashboard && parsed.protocol === "http:" && !/^127\./.test(parsed.hostname) && parsed.hostname !== "[::1]") return null;
      return {text: raw, href: parsed.href};
    }
    if (!dashboard && ["tcp:", "udp:", "tcpmux:"].includes(parsed.protocol) && parsed.port && !parsed.pathname) return {text: raw, href: null};
  } catch { /* Invalid or unsupported administrator configuration is not a link. */ }
  return null;
}

export function publishedLinks(proxy, nativeAccess) {
  if (!proxy || typeof proxy.client_id !== "string" || !proxy.client_id) return [];
  return list(nativeAccess?.published_endpoints).slice(0, 512)
    .filter(entry => entry && entry.proxy_name === proxy.name && entry.user === proxy.user && entry.client_id === proxy.client_id)
    .slice(0, 32).map(entry => configuredLink(entry.url)).filter(Boolean);
}

function linkNode(document, link, label) {
  const node = document.createElement(link.href ? "a" : "span"); node.textContent = label ?? link.text;
  if (link.href) { node.href = link.href; node.target = "_blank"; node.rel = "noopener noreferrer"; node.referrerPolicy = "no-referrer"; node.dataset.frpFocus = `link:${label ?? ""}:${link.href}`; }
  return node;
}

export function renderNativeRegistryAccess(document, registry, nativeAccess) {
  const section = document.createElement("section"); section.className = "fa-frp-access";
  const dashboard = configuredLink(nativeAccess?.server_dashboard_url, true);
  const heading = document.createElement("h3"); heading.textContent = "原生管理与公布入口"; section.append(heading);
  const note = document.createElement("p"); note.className = "fa-muted";
  note.textContent = "以下地址由管理员显式配置，未自动验证可达性；不会从监听地址推断公网链接。"; section.append(note);
  if (dashboard) section.append(linkNode(document, dashboard, "打开原生 frps Dashboard ↗"));
  else { const missing = document.createElement("p"); missing.className = "fa-muted"; missing.textContent = "尚未配置原生 frps Dashboard 入口。"; section.append(missing); }
  for (const proxy of list(registry?.proxies)) {
    const links = publishedLinks(proxy, nativeAccess);
    if (!links.length) continue;
    const item = document.createElement("div"); item.className = "fa-frp-published";
    const label = document.createElement("strong"); label.textContent = `${text(proxy.name, 256)} · 管理员公布${proxy.online === true ? "" : "（隧道当前离线）"}`; item.append(label);
    for (const link of links) {
      const entry = linkNode(document, link);
      if (link.href) entry.dataset.frpFocus = JSON.stringify(["published", proxy.user, proxy.client_id, proxy.name, link.href]);
      item.append(entry);
    }
    section.append(item);
  }
  return section;
}

const maintenance = [
  ["文件 / include", "在 Agent 所在主机备份并修改原生配置，先运行 frp-plus-agent verify -c <配置文件>。Proxy/Visitor 变更按原生 reload 流程应用，再核对运行结果。"],
  ["原生 Store", "仅当 Store 已启用时，通过原生 frpc Dashboard 或 Store API 维护。文件与 Store 同名会发生覆盖；禁用或删除 Store 条目可能重新启用文件定义。"],
  ["需要重启的配置", "serverAddr、认证、TLS、telemetry、Store 路径等启动配置不能依靠 frpc reload 全部替换。frps 监听、monitor 等服务端启动配置需原生维护和重启。"],
  ["原生 Dashboard", "仅访问管理员明确配置的入口。未启用或仅监听回环时，需在对应主机访问或使用 SSH 转发；本页面不自动开放管理端口、不提供原生密码。"],
  ["服务实例与归属", "当前仅观察此节点对应的单个 frpc Service。缺少稳定 clientID 或运行多个 Service 时，不能按名称推断唯一归属；应在对应原生进程中维护配置。"],
  ["当前操作范围", "此处只读展示配置来源和维护步骤。节点可信绑定只改变对账归属，不会修改隧道配置；尚不提供远程编辑、应用或回退。"]
];

export function clearFRPDetail(container) {
  container.replaceChildren(); delete container.dataset.frpNode;
}

// Rebuild only this read-only region and restore disclosure/focus by object key.
// The surrounding settings form, drafts and legacy reconciliation stay intact.
export function renderFRPDetail(container, node, reconciliation, nativeAccess) {
  const document = container.ownerDocument, sameNode = container.dataset.frpNode === node.id;
  const previous = sameNode ? new Map([...container.querySelectorAll("details[data-frp-key]")].map(item => [item.dataset.frpKey, item.open])) : new Map();
  const active = sameNode && container.contains(document.activeElement) ? document.activeElement.dataset.frpFocus : null;
  const scroll = container.closest(".fa-editor-body"), scrollTop = scroll?.scrollTop;
  const model = detailModel(node, reconciliation);
  const el = (tag, value, className) => { const item = document.createElement(tag); if (value !== undefined) item.textContent = value; if (className) item.className = className; return item; };
  const rows = fields => {
    const dl = el("dl", undefined, "fa-detail-list fa-frp-fields");
    for (const [label, value] of fields) { const row = el("div"); row.append(el("dt", label), el("dd", value)); dl.append(row); }
    return dl;
  };
  const disclosure = (key, title, fields, status = "") => {
    const detail = el("details", undefined, "fa-frp-object"); detail.dataset.frpKey = key; detail.open = previous.get(key) === true;
    const summary = el("summary"); summary.dataset.frpFocus = key;
    summary.append(el("span", title), el("small", status)); detail.append(summary, rows(fields)); return detail;
  };
  const section = el("section", undefined, "fa-section fa-frp-detail"); section.dataset.state = model.state;
  section.append(el("h3", "FRP 配置与访问详情"), el("p", `${model.label} · 最近详情 ${model.at}`, "fa-frp-state"), el("p", model.message, "fa-muted"));
  if (model.state === "ready") {
    section.append(rows(model.fields));
    for (const [title, objects, empty] of [["Proxy 代理", model.proxies, "本次完整详情没有 Proxy。"], ["Visitor 访问端", model.visitors, "本次完整详情没有 Visitor。"]]) {
      section.append(el("h4", `${title} · ${objects.length}`));
      if (!objects.length) section.append(el("p", empty, "fa-muted"));
      for (const object of objects) section.append(disclosure(object.key, object.title, object.fields, object.status));
    }
    section.append(el("p", "Visitor 的本地监听、对端连接、P2P 与 fallback 分别报告。监听成功不能证明远端可达，Visitor 不计入服务端 Proxy 注册数。", "fa-muted"));
  }
  const native = disclosure("native-config", "配置管理 · 原生维护说明", maintenance, "只读");
  const dashboard = configuredLink(nativeAccess?.client_dashboard_urls?.[node.id], true);
  if (dashboard) { const entry = el("p", undefined, "fa-frp-dashboard"); entry.append(linkNode(document, dashboard, "打开原生 frpc Dashboard ↗")); native.append(entry); }
  section.append(native);
  container.replaceChildren(section); container.dataset.frpNode = node.id;
  if (active) [...container.querySelectorAll("[data-frp-focus]")].find(item => item.dataset.frpFocus === active)?.focus({preventScroll: true});
  if (scroll && scrollTop !== undefined) scroll.scrollTop = scrollTop;
  return model;
}
