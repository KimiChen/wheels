import {proxyTypes, visitorTypes, operationLabels, activeStates, configErrors, fieldSpecs, secretPaths, inputText, makeEdit, resultFacts, resultReason, operationMaterials, canRollback, canApply, canAcknowledgeRestore, createConfigController} from "./admin-configuration-data.mjs";

const labels = {enabled: "启用", localIP: "本地目标地址", localPort: "本地目标端口", remotePort: "远端端口（0 为动态）", customDomains: "域名（每行一个）", subdomain: "子域名", locations: "HTTP 路径（每行一个）", httpUser: "HTTP 用户", hostHeaderRewrite: "Host 重写", routeByHTTPUser: "按 HTTP 用户路由", multiplexer: "复用协议（httpconnect）", allowUsers: "允许用户（每行一个）", serverUser: "远端用户", serverName: "远端代理名称", bindAddr: "Visitor 本地监听地址", bindPort: "Visitor 端口（-1 为内部模式）", protocol: "Visitor 协议", keepTunnelOpen: "保持隧道", maxRetriesAnHour: "每小时最大重试", minRetryInterval: "最短重试间隔", fallbackTo: "fallback Visitor 名称", fallbackTimeoutMs: "fallback 超时（毫秒）", "transport.useEncryption": "代理加密", "transport.useCompression": "代理压缩", "transport.bandwidthLimit": "限速（如 1MB）", "transport.bandwidthLimitMode": "限速位置（client/server）", "transport.proxyProtocolVersion": "PROXY 协议版本", secretKey: "隧道密钥", httpPassword: "HTTP 密码", "loadBalancer.groupKey": "负载均衡密钥"};
const modes = {create: "新建", update: "编辑", clone: "复制", rename: "改名", delete: "删除", enable: "启用", disable: "禁用"};
const originLabels = {store: "Store", file: "主文件", include: "include 文件"};
const date = value => Number.isFinite(value) && value > 0 ? new Date(value).toLocaleString("zh-CN", {hour12: false}) : "未知";
const display = value => value === undefined || value === null ? "默认 / 未设置" : Array.isArray(value) ? value.join("、") || "空列表" : String(value);

export function createConfigPanel(container, {request}) {
  const document = container.ownerDocument;
  const element = (tag, text, className) => { const node = document.createElement(tag); if (text !== undefined) node.textContent = text; if (className) node.className = className; return node; };
  const button = (title, handler, write = false) => {
    const node = element("button", title, "wsk-button wsk-secondary fa-small-button"); node.type = "button";
    if (write) node.dataset.configWrite = "";
    node.addEventListener("click", handler); return node;
  };
  const row = (parent, label, value) => { const entry = element("div"); entry.append(element("dt", label), element("dd", value)); parent.append(entry); };
  const field = (label, control) => { const wrapper = element("label", undefined, "wsk-field"); wrapper.append(element("span", label, "wsk-label"), control); return wrapper; };
  let mounted = false, lastInventory = null, lastResult = null, lastHistory = null, lastRestoration = null, form = null, formMode = null, source = null, secretActions = [], poll = null;
  let message, inventoryBox, editorBox, previewBox, resultBox, historyBox, applyButton, reloadButton;
  let restoreBox, restoreButton, acknowledgeButton;
  const controller = createConfigController({request, onChange: render});
  function mount() {
    if (mounted) return; mounted = true;
    const heading = element("h3", "隧道配置管理"), intro = element("p", "选择配置来源：已在 Agent 本机启用的 Store 可走校验、预览、应用与回退；主文件 / include 通过原生维护流程调整。", "fa-muted");
    message = element("p", "正在读取…", "wsk-alert wsk-info"); message.setAttribute("role", "status");
    reloadButton = button("重新读取配置", () => { clearEditor(); void controller.reload(); });
    const native = element("details", undefined, "fa-editor-advanced"); native.append(element("summary", "原生文件管理与迁移步骤"));
    const steps = element("ol");
    for (const text of ["在 Agent 主机建立完整私有备份，记录当前 Store、主文件与 include 来源。关闭托管后才能使用原生写入口。", "启动配置、认证、TLS、telemetry、Store 路径及 frps 配置需在原生文件维护并重启。普通 frpc reload 不能替换所有启动配置。", "文件迁移到 Store 需要显式准备候选、清理原文件同名定义并核对完整有效集合。不要用同名覆盖代替迁移。", "重新启用托管并读取来源；只读、来源冲突、旧版本、多 Service 或恢复未完成时不开放写入。原生 Dashboard 地址见资源详情，由管理员显式配置。"] ) steps.append(element("li", text));
    native.append(steps);
    inventoryBox = element("div", undefined, "fa-config-inventory"); editorBox = element("div", undefined, "fa-config-editor"); previewBox = element("div", undefined, "fa-config-preview"); resultBox = element("div", undefined, "fa-config-result"); historyBox = element("div", undefined, "fa-config-history");
    const restoreSection = element("section", undefined, "fa-config-restoration");
    restoreButton = button("恢复核对", () => void controller.inspectRestore());
    restoreBox = element("div");
    restoreSection.append(element("h4", "备份恢复接管"), element("p", "仅在本机离线安装恢复包后使用。先读取新身份与验证事实，再明确接管；接管不会直接开放配置写入。", "fa-muted"), restoreButton, restoreBox);
    container.replaceChildren(heading, intro, message, reloadButton, native, restoreSection, inventoryBox, editorBox, previewBox, resultBox, historyBox);
  }
  function clearEditor() {
    if (form) { for (const input of form.querySelectorAll('input[type="password"]')) input.value = ""; form.reset(); }
    form = null; source = null; formMode = null; secretActions = []; editorBox?.replaceChildren(); previewBox?.replaceChildren();
  }
  function render() {
    const state = controller.state;
    if (!state.nodeID) return;
    mount();
    message.textContent = state.message || (state.pending ? "正在与 Agent 核对，请稍候…" : !state.online ? "节点监控离线；以下操作记录为历史信息，配置写入暂不可用。" : state.needsReload ? "操作状态已更新；下次编辑前请重新读取当前配置。" : state.inventory ? `当前来源${state.inventory.inventory.state === "ready" ? "可管理" : "只读"} · ${date(state.inventory.received_at_ms)}` : "无法读取配置来源。请确认 Agent 已显式启用 Store 托管与管理通道。");
    reloadButton.textContent = state.draft ? "放弃草稿并重新读取" : "重新读取配置"; reloadButton.disabled = state.pending;
    if (lastInventory !== state.inventory) { lastInventory = state.inventory; renderInventory(); }
    if (lastResult !== state.result) { lastResult = state.result; renderResult(); renderPreview(); }
    if (lastHistory !== state.operations) { lastHistory = state.operations; renderHistory(); }
    if (lastRestoration !== state.restoration) { lastRestoration = state.restoration; renderRestoration(); }
    const recovering = ["pending", "verified", "acknowledged"].includes(state.restoration?.restore?.state);
    const blocked = state.pending || !state.online || recovering || state.inventory?.inventory?.state !== "ready";
    for (const input of container.querySelectorAll("[data-config-write]")) input.disabled = blocked;
    const editingBlocked = blocked || state.needsReload || state.operations.some(op => activeStates.has(op.state));
    for (const input of inventoryBox.querySelectorAll("[data-config-write]")) input.disabled = editingBlocked;
    for (const input of form?.querySelectorAll("input, select, textarea, button") ?? []) input.disabled = editingBlocked;
    if (applyButton) applyButton.disabled = blocked || !canApply(state.result, state.preview) || !previewBox.querySelector('input[type="checkbox"]')?.checked;
    restoreButton.disabled = state.pending || !state.online;
    if (acknowledgeButton) acknowledgeButton.disabled = state.pending || !state.online || !canAcknowledgeRestore(state.restoration) || !restoreBox.querySelector('input[type="checkbox"]')?.checked;
    clearTimeout(poll);
    if (!state.pending && state.result?.operation.state === "prepared" && state.result.operation.deadline_at_ms > Date.now()) {
      poll = setTimeout(render, Math.min(1000, state.result.operation.deadline_at_ms - Date.now() + 1));
    } else if (!state.pending && state.result && activeStates.has(state.result.operation.state) && state.result.operation.state !== "rollback_failed" && state.online) {
      poll = setTimeout(() => { if (container.isConnected && !container.closest("[hidden]")) void controller.query(); }, 3000);
    }
  }
  function renderRestoration() {
    restoreBox.replaceChildren(); acknowledgeButton = null;
    const data = controller.state.restoration; if (!data) return;
    const value = data.restore;
    if (value.state === "none") { restoreBox.append(element("p", "Agent 当前没有恢复接管记录。", "fa-muted")); return; }
    const labels = {pending: "本机恢复尚待运行验证", verified: "本机已验证，等待管理员接管", acknowledged: "接管已确认，等待离线确认", confirmed: "离线确认已记录"};
    restoreBox.append(element("h5", labels[value.state]), element("p", `核对时间：${date(data.received_at_ms)}。业务连通仍需独立验证。`, "fa-muted"));
    const facts = element("dl", undefined, "fa-detail-list fa-config-digest");
    for (const [label, text] of [["恢复 epoch", value.epoch], ["当前服务身份", data.service_id], ["备份服务身份", value.backup_service_id], ["被替换服务身份", value.replaced_service_id || "未记录"], ["恢复包摘要", value.manifest_digest], ["配置上下文摘要", value.context_revision], ["Store 摘要", value.store_digest || "尚未验证"], ["运行时 / 资源", value.runtime_loaded && value.resources_ready ? "已验证" : "未确认"], ["恢复操作记录", String(value.operations_count)], ["主控接管记录", data.receipt?.state === "acknowledged" ? "Agent 已确认" : data.receipt ? "已记录意图，Agent 结果待核对" : "尚未接管"]]) row(facts, label, text);
    restoreBox.append(facts);
    if (data.active_operation) restoreBox.append(element("p", `原活动操作 ${data.active_operation.operation_id} · ${operationLabels[data.active_operation.state]}。接管先查询真实记录；无法从恢复包继续核对的旧意图将以恢复取代事件留存，不能视为已回退。`, "wsk-alert wsk-warning fa-config-digest"));
    if (["verified", "acknowledged"].includes(value.state) && data.receipt?.state !== "acknowledged") {
      const consent = element("input"); consent.type = "checkbox";
      restoreBox.append(field("我已核对恢复身份与摘要，允许接管并保留旧操作处理审计", consent));
      acknowledgeButton = button("确认接管恢复状态", () => void controller.acknowledgeRestore());
      consent.addEventListener("change", render); restoreBox.append(acknowledgeButton);
      restoreBox.append(element("p", "核对结果超过一分钟后请重新读取；接管请求失联时先核对记录，不重复应用配置。", "fa-muted"));
    }
    if (value.state === "acknowledged" || data.receipt?.state === "acknowledged") restoreBox.append(element("p", "下一步在 Agent 本机停止进程，使用上面的 epoch 与恢复包摘要运行 ops.py restore-confirm --directory <原安装目录> --epoch <epoch> --manifest-digest <摘要> --offline。随后重新启动、核对配置来源及实际业务；本页面不执行本机命令。", "wsk-alert wsk-warning"));
    if (value.state === "confirmed") restoreBox.append(element("p", "请重新读取配置来源。只有本次启动重新核对通过、来源显示可管理时才能编辑；历史确认不代表当前业务可达。", "fa-muted"));
  }
  function renderInventory() {
    inventoryBox.replaceChildren();
    const inventory = controller.state.inventory?.inventory;
    if (!inventory) return;
    const header = element("div", undefined, "fa-actions"); header.append(element("h4", `配置对象 · ${inventory.objects.length}`), button("新建 Proxy", () => edit("create", null, "proxy"), true), button("新建 Visitor", () => edit("create", null, "visitor"), true)); inventoryBox.append(header);
    if (inventory.issues.length) inventoryBox.append(element("p", inventory.issues.map(issue => configErrors[issue.code] ?? `来源限制：${issue.code}`).join("；"), "wsk-alert wsk-warning"));
    const revision = element("p", `基础修订 ${inventory.revision}`, "fa-muted fa-config-digest"); inventoryBox.append(revision);
    if (!inventory.objects.length) inventoryBox.append(element("p", "当前没有 Proxy 或 Visitor。", "fa-muted"));
    for (const item of inventory.objects) {
      const card = element("section", undefined, "fa-config-object");
      card.append(element("h5", `${item.kind === "proxy" ? "Proxy" : "Visitor"} · ${item.name}`), element("p", `${item.type.toUpperCase()} · ${originLabels[item.source] ?? "未知来源"} · ${item.active ? "在启用集合中" : "未启用 / 被 start 筛选"}`, "fa-muted"));
      if (item.read_only_fields.length) card.append(element("p", `以下高级字段在本机保留：${item.read_only_fields.join("、")}`, "fa-muted"));
      if (item.writable && inventory.state === "ready") {
        const actions = element("div", undefined, "fa-actions");
        for (const mode of ["update", "clone", "rename", item.fields.find(f => f.path === "enabled")?.value === false ? "enable" : "disable", "delete"]) actions.append(button(modes[mode], () => edit(mode, item), true));
        card.append(actions);
      } else card.append(element("p", item.issues.map(issue => configErrors[issue.code] ?? ({source_read_only: "请在原生文件维护", advanced_read_only: "插件对象暂保持原生维护"})[issue.code] ?? "来源暂不可编辑").join("；") || "当前只读", "fa-muted"));
      inventoryBox.append(card);
    }
  }
  function edit(mode, item, kind = item?.kind) {
    clearEditor(); controller.draft(); source = item; formMode = mode;
    form = element("form"); form.autocomplete = "off";
    form.append(element("h4", `${modes[mode]} ${kind === "proxy" ? "Proxy" : "Visitor"}`));
    const kindInput = element("input"); kindInput.type = "hidden"; kindInput.name = "kind"; kindInput.value = kind; form.append(kindInput);
    const type = element("select", undefined, "wsk-control"); type.name = "type";
    for (const name of kind === "proxy" ? proxyTypes : visitorTypes) { const option = element("option", name.toUpperCase()); option.value = name; type.append(option); }
    if (item) type.value = item.type;
    // Keep type immutable for existing objects; a hidden input carries it.
    if (item) { type.hidden = true; form.append(type, element("p", `类型：${item.type.toUpperCase()}`, "fa-muted")); }
    else form.append(field("类型", type));
    const name = element("input", undefined, "wsk-control"); name.name = "name"; name.required = true; name.maxLength = 256; name.value = item ? ["clone", "rename"].includes(mode) ? "" : item.name : ""; name.readOnly = !!item && !["clone", "rename"].includes(mode); form.append(field(["clone", "rename"].includes(mode) ? "新名称" : "名称", name));
    const fields = element("div", undefined, "fa-settings-grid fa-config-fields"); form.append(fields);
    function renderFields() {
      fields.replaceChildren();
      if (!["create", "update", "clone", "rename"].includes(mode)) { fields.append(element("p", `${modes[mode]}将影响此对象；下一步先校验并预览，不会立即改变运行配置。`, "fa-muted")); return; }
      const values = new Map((item?.fields ?? []).map(value => [value.path, value.value]));
      for (const [path, spec] of Object.entries(fieldSpecs(kind, type.value))) {
        let input;
        if (spec === "bool") { input = element("select", undefined, "wsk-control"); for (const [value, label] of [["", "原生默认"], ["true", "启用"], ["false", "关闭"]]) { const option = element("option", label); option.value = value; input.append(option); } }
        else { input = element(spec === "strings" ? "textarea" : "input", undefined, spec === "strings" ? "wsk-textarea" : "wsk-control"); if (spec === "strings") input.rows = 2; else if (["port", "bind_port", "remote_port", "nonnegative"].includes(spec)) input.inputMode = "numeric"; input.maxLength = spec === "strings" ? 32768 : 1024; }
        input.dataset.configField = path; input.value = inputText(values.get(path));
        if (!item && path === "multiplexer") input.value = "httpconnect";
        if (!item && ["localIP", "bindAddr"].includes(path)) input.value = "127.0.0.1";
        fields.append(field(labels[path] ?? path, input));
      }
      for (const path of secretPaths(kind, type.value)) {
        const group = element("div", undefined, "wsk-field fa-config-secret"); group.dataset.configSecret = path;
        const present = item?.secrets.some(secret => secret.path === path && secret.present);
        const select = element("select", undefined, "wsk-control");
        for (const [value, label] of [["keep", "保持本机值"], ["replace", "写入新秘密"], ["reference", "使用已有本机引用"], ["clear", "清除"]]) { const option = element("option", label); option.value = value; select.append(option); }
        const input = element("input", undefined, "wsk-control"); input.type = "password"; input.autocomplete = "new-password"; input.maxLength = 1024; input.hidden = true;
        select.addEventListener("change", () => { input.value = ""; input.hidden = !["replace", "reference"].includes(select.value); input.placeholder = select.value === "reference" ? "本机引用 UUID" : "只在本次请求写入"; });
        group.append(field(`${labels[path]} · ${present ? "本机已设置" : "未设置"}`, select), field("秘密值或引用", input)); fields.append(group);
      }
    }
    type.addEventListener("change", renderFields); renderFields();
    const note = element("p", "高级字段留在 Agent 本机；复制及改名从原始 Store 对象复制。秘密仅经专用请求写入，不保存到浏览器存储。每次最多两个新秘密。", "fa-muted");
    const submit = element("button", "校验并生成预览", "wsk-button fa-primary"); submit.type = "submit"; submit.dataset.configWrite = "";
    const actions = element("div", undefined, "fa-actions"); actions.append(submit, button("放弃草稿", () => { clearEditor(); controller.draft(false); }));
    const error = element("p", "", "wsk-alert wsk-warning"); error.hidden = true; error.setAttribute("role", "status"); form.append(note, error, actions);
    form.addEventListener("submit", async event => {
      event.preventDefault(); error.hidden = true;
      const inputs = Object.fromEntries([...form.querySelectorAll("[data-config-field]")].map(input => [input.dataset.configField, input.value]));
      const secrets = Object.fromEntries([...form.querySelectorAll("[data-config-secret]")].map(group => [group.dataset.configSecret, {mode: group.querySelector("select").value, value: group.querySelector('input[type="password"]').value}]));
      try {
        const intent = makeEdit({mode: formMode, kind, type: type.value, name: name.value, source, inputs, secrets});
        secretActions = Object.entries(secrets).map(([path, secret]) => `${labels[path]}：${({keep: "保持", clear: "清除", replace: "替换", reference: "使用本机引用"})[secret.mode]}`);
        // Clear visible values before waiting for network/Agent confirmation.
        for (const input of form.querySelectorAll('input[type="password"]')) input.value = "";
        await controller.prepare(intent);
      } catch (err) { error.textContent = err.message; error.hidden = false; }
    });
    editorBox.append(form); name.focus(); render();
  }
  function renderPreview() {
    previewBox.replaceChildren(); applyButton = null;
    const state = controller.state, preview = state.preview;
    if (!preview || !state.result) return;
    previewBox.append(element("h4", "变更预览"), element("p", "变更可能重建代理或 Visitor 并中断现有连接。校验通过不代表远端策略接受或业务已经可达。", "wsk-alert wsk-warning"));
    for (const change of preview.changes ?? []) {
      const object = change.after ?? change.before;
      const section = element("section", undefined, "fa-config-object"); section.append(element("h5", `${modes[change.operation] ?? change.operation} · ${object?.kind ?? ""} · ${object?.name ?? ""}`));
      const before = new Map((change.before?.fields ?? []).map(f => [f.path, f.value])), after = new Map((change.after?.fields ?? []).map(f => [f.path, f.value]));
      const fields = element("dl", undefined, "fa-detail-list");
      for (const path of new Set([...before.keys(), ...after.keys()])) if (JSON.stringify(before.get(path)) !== JSON.stringify(after.get(path))) row(fields, labels[path] ?? path, `${display(before.get(path))} → ${display(after.get(path))}`);
      for (const text of secretActions) row(fields, "秘密处理", text);
      if (!fields.children.length) row(fields, "对象变化", "名称、启停或完整对象发生变化；未编辑的高级字段保持本机值。"); section.append(fields); previewBox.append(section);
    }
    for (const warning of preview.warnings ?? []) previewBox.append(element("p", configErrors[warning.code] ?? `验证提醒：${warning.code}`, "fa-muted"));
    previewBox.append(element("p", `候选摘要 ${preview.candidate_digest}`, "fa-muted fa-config-digest"));
    const confirm = element("input"); confirm.type = "checkbox"; const acknowledge = field("我已核对预览，接受可能发生的连接重建", confirm); acknowledge.classList.add("fa-config-confirm");
    applyButton = button("应用本次预览", () => void controller.action("apply"), true);
    confirm.addEventListener("change", render); previewBox.append(acknowledge, applyButton);
    if (state.result.operation.state !== "prepared") { acknowledge.hidden = true; applyButton.hidden = true; }
  }
  function renderResult() {
    resultBox.replaceChildren();
    const result = controller.state.result; if (!result) return;
    const op = result.operation;
    resultBox.append(element("h4", operationLabels[op.state] ?? "状态未知"), element("p", `操作 ${op.operation_id} · ${date(op.updated_at_ms)}`, "fa-muted fa-config-digest"));
    resultBox.append(element("p", `预览 / 确认期限：${date(op.deadline_at_ms)}。到期后只能查询或处理恢复，不能继续应用旧预览。`, "fa-muted"));
    const facts = element("dl", undefined, "fa-detail-list"); for (const [label, text] of resultFacts(result.agent)) row(facts, label, text); resultBox.append(facts);
    const materials = operationMaterials(result);
    if (materials.state === "expired") resultBox.append(element("p", `回退材料于 ${date(materials.expiredAt)} 按保留期限清理。操作结果与审计仍保留；需要恢复旧配置时请使用另存的完整备份并走本机恢复流程。`, "wsk-alert wsk-warning"));
    resultBox.append(element("p", `Agent 事实记录于 ${date(result.agent_received_at_ms)}；历史成功不代表后续配置仍保持该版本。业务连通需另行实际验证。`, "fa-muted"));
    const reason = resultReason(result.agent); if (reason) resultBox.append(element("p", reason, "wsk-alert wsk-warning"));
    if (op.state === "verifying") resultBox.append(element("p", "正在等待实际登记与本地资源。本次明确注册或启动失败会触发回退；正常等待连接仍按操作期限核对，业务可达性需另行测试。", "fa-muted"));
    const actions = element("div", undefined, "fa-actions"); actions.append(button("查询实际结果", () => void controller.query()));
    if (["prepared", "draft", "validated"].includes(op.state)) actions.append(button("取消未应用操作", () => void controller.action("cancel"), true));
    if (canRollback(result)) {
      const rollback = button("准备回退", () => {
        rollback.disabled = true;
        const confirm = button("确认恢复此操作前的配置", () => void controller.action("rollback"), true);
        actions.append(element("p", "回退前会比对当前 Store 内容和来源摘要；不匹配时返回冲突。", "fa-muted"), confirm);
      }, true); actions.append(rollback);
    }
    resultBox.append(actions);
    if (op.state === "prepared" && !controller.state.preview) resultBox.append(element("p", "此页面没有本次预览，不能直接应用。请取消后重新读取、编辑并校验。", "fa-muted"));
    if (op.state === "rollback_failed") resultBox.append(element("p", "请保留 Agent 私有恢复目录和日志。核对当前 Store、文件/include 与磁盘权限后，通过本机恢复流程处理；不要删除未完成事务来绕过恢复保护。", "wsk-alert wsk-warning"));
    const timeline = element("ol", undefined, "fa-config-timeline");
    if (result.events_truncated) resultBox.append(element("p", "此处显示最近 256 条事件，较早记录仍保留在审计库。", "fa-muted"));
    for (const event of result.events ?? []) {
      const item = element("li", `${date(event.created_at_ms)} · ${operationLabels[event.state] ?? event.state} · ${event.actor || op.creator || "未知操作者"} · ${event.code}`);
      for (const change of event.changes ?? []) item.append(element("p", `${modes[change.action] ?? change.action} ${change.kind} · ${change.name}${change.clone_from ? `；复制来源 ${change.clone_from}` : ""}${change.fields?.length ? `；字段 ${change.fields.map(path => labels[path] ?? path).join("、")}` : ""}`, "fa-muted"));
      timeline.append(item);
    }
    resultBox.append(timeline);
  }
  function renderHistory() {
    historyBox.replaceChildren(element("h4", "最近配置操作"));
    if (!controller.state.operations.length) historyBox.append(element("p", "暂无操作记录。", "fa-muted"));
    for (const op of controller.state.operations) {
      const item = button(`${date(op.created_at_ms)} · ${operationLabels[op.state] ?? "状态未知"} · ${op.creator ?? "未知操作者"}`, () => void controller.query(op.operation_id)); item.classList.add("fa-config-history-item"); historyBox.append(item);
    }
  }
  return {
    async open(nodeID, online) { mount(); await controller.open(nodeID, online); },
    setOnline(online) { controller.setOnline(online); },
    clear() { clearTimeout(poll); clearEditor(); controller.clear(); mounted = false; lastInventory = lastResult = lastHistory = lastRestoration = null; container.replaceChildren(); },
    state: controller.state
  };
}
