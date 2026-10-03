import test from "node:test";
import assert from "node:assert/strict";
import {detailModel, endpointText, endpointList, serverEndpointText, errorSummary, sourceText, renderFRPDetail, clearFRPDetail, createFRPDetailReader, configuredLink, publishedLinks, renderNativeRegistryAccess} from "../src/admin-frp.mjs";

const endpoint = {kind: "tcp", source: "configured", host: "", port: 0, path: "", subdomain: null};
const proxy = {name: "echo", type: "tcp", source: "file", source_state: "active", status: "running", enabled: true, local_target: "127.0.0.1:8000", endpoints: [endpoint, {...endpoint, source: "observed", host: "0.0.0.0", port: 18000}], encryption: false, compression: null};
const visitor = {name: "private", type: "xtcp", source: "store", source_state: "active", status: "listening", enabled: true, server_user: "", server_name: "peer", protocol: "quic", bind_endpoint: {...endpoint, host: "127.0.0.1", port: 19000}, local_state: "listening", remote_state: "unknown", p2p_state: "connecting", fallback_state: "available", fallback_to: "fallback"};
function node() {
  return {id: "1", frp_detail_state: "ready", frp_detail_at: "2026-10-03T01:00:00Z", frp_detail: {state: "ready", service_id: "primary", transport: {protocol: "quic", wire_protocol: "v2", source: "configured", tls: true, tcp_mux: null}, configuration: {source: "mixed", source_state: "conflict", revision: "a".repeat(64), shadowed_sources: ["file"], native_entry: "store"}, proxies: [structuredClone(proxy)], visitors: [structuredClone(visitor)]}};
}
const field = (object, name) => object.fields.find(row => row[0] === name)?.[1];

test("FRP detail separates control transport, wire version, proxy and visitor state", () => {
  const model = detailModel(node());
  assert.equal(model.state, "ready");
  assert.equal(field(model, "控制传输"), "QUIC · 配置值");
  assert.equal(field(model, "FRP wire 协议"), "v2");
  assert.equal(field(model, "控制 TLS / TCP 复用"), "启用 / 未知");
  assert.match(field(model, "配置来源"), /混合来源.*来源冲突.*主配置文件/);
  assert.match(field(model.proxies[0], "配置期望入口"), /动态分配/);
  assert.match(field(model.proxies[0], "节点运行观察"), /0\.0\.0\.0.*18000/);
  assert.equal(field(model.proxies[0], "代理加密 / 压缩"), "关闭 / 未知");
  assert.equal(field(model.visitors[0], "本地监听 / 对端连接"), "监听中 / 未知");
  assert.equal(field(model.visitors[0], "P2P / fallback"), "连接中 / 可用");
});

test("legacy, stale, unavailable and oversized details never masquerade as empty complete lists", () => {
  const legacy = detailModel({id: "1"});
  assert.equal(legacy.state, "unsupported"); assert.match(legacy.message, /上方原有隧道对账/);
  for (const state of ["waiting", "stale", "busy", "unavailable", "truncated", "unexpected"]) {
    const model = detailModel({...node(), frp_detail_state: state});
    assert.notEqual(model.state, "ready"); assert.deepEqual(model.proxies, []); assert.deepEqual(model.visitors, []);
    assert.equal(model.fields.length, 0);
  }
  assert.equal(detailModel({...node(), frp_detail: null}).state, "unavailable");
  for (const proxies of [null, [null], [{}]]) {
    const value = node(); value.frp_detail.proxies = proxies;
    assert.equal(detailModel(value).state, "unavailable");
  }
  const huge = node(); huge.frp_detail.proxies = Array(513).fill(proxy);
  assert.equal(detailModel(huge).state, "truncated");
});

test("observed endpoints require live matching observations and never become a public URL", () => {
  const actual = {...proxy, online: true, endpoints: [{...endpoint, source: "observed", host: "::", port: 18000}]};
  assert.equal(serverEndpointText(actual), "TCP · [::] · 18000");
  assert.match(serverEndpointText({...actual, online: false}), /已离线.*未知/);
  assert.match(serverEndpointText(actual, "stale"), /快照不可用/);
  const reconciliation = {state: "matched", proxies: [{name: "echo", type: "tcp", server_state: "registered", server: actual}]};
  assert.match(field(detailModel(node(), reconciliation).proxies[0], "服务端实际入口"), /\[::\].*18000/);
  assert.match(field(detailModel(node(), {...reconciliation, state: "conflict"}).proxies[0], "服务端实际入口"), /尚无唯一匹配/);
  const stopped = node(); stopped.frp_detail.proxies[0].status = "closed";
  assert.match(field(detailModel(stopped).proxies[0], "节点运行观察"), /未观测/);
  assert.equal(endpointText({kind: "visitor"}), "通过 Visitor 访问（无公网监听入口）");
  assert.match(endpointText({kind: "https", subdomain: "private"}), /后缀待服务端确认/);
  assert.equal(endpointList([endpoint], "observed"), "未观测到当前入口");
});

test("formatting is bounded, handles unknown values and selects only safe error fields", () => {
  assert.equal(endpointText(null), "入口未知");
  assert.match(endpointText({...endpoint, host: "user:private-password@example.invalid", port: 80}), /地址待确认/);
  assert.doesNotMatch(endpointText({kind: "http", host: "example.invalid", port: 80, path: "/?token=private"}), /private/);
  assert.match(sourceText("new-source", "new-state", ["new-source"]), /来源未知.*待确认/);
  assert.equal(errorSummary(null), "未报告错误");
  assert.match(errorSummary({stage: "visitor", code: "nat_traversal_failed"}), /Visitor.*NAT 穿透失败/);
  assert.match(errorSummary({stage: "control", code: "connect_failed", recovered_at: "invalid"}), /^最近错误/);
  assert.match(errorSummary({stage: "control", code: "connect_failed", recovered_at: "2026-10-03T01:00:00Z"}), /^已恢复/);
  const raw = node(); raw.frp_detail.proxies[0].name = "界".repeat(2000); raw.frp_detail.transport.protocol = "future";
  raw.frp_detail.control_error = {code: "private-password", stage: "private-path", message: "private-raw-error"};
  raw.frp_detail.configuration.secret = "private-config"; raw.frp_detail.proxies[0].secret_key = "private-key";
  const model = detailModel(raw), serialized = JSON.stringify(model);
  assert.ok(model.proxies[0].title.length < 280); assert.equal(field(model, "控制传输"), "未知 · 配置值");
  for (const privateValue of ["private-password", "private-path", "private-raw-error", "private-config", "private-key"]) assert.equal(serialized.includes(privateValue), false);
});

// A minimal DOM exercises the real renderer without a browser dependency. The
// source module only needs these standard DOM operations; HTML is never parsed.
class Element {
  constructor(tag, document) { this.tagName = tag.toUpperCase(); this.ownerDocument = document; this.children = []; this.dataset = {}; this.open = false; this.className = ""; this.scrollTop = 0; this.value = ""; }
  set textContent(value) { this.value = String(value); this.children = []; }
  get textContent() { return this.value + this.children.map(child => child.textContent).join(""); }
  append(...children) { for (const child of children) { child.parentElement = this; this.children.push(child); } }
  replaceChildren(...children) { for (const child of this.children) child.parentElement = null; this.children = []; this.value = ""; this.append(...children); }
  contains(child) { return child === this || this.children.some(item => item.contains(child)); }
  querySelectorAll(selector) {
    const matches = node => selector === "details[data-frp-key]" ? node.tagName === "DETAILS" && "frpKey" in node.dataset : selector === "[data-frp-focus]" ? "frpFocus" in node.dataset : selector === "a" ? node.tagName === "A" : false;
    return this.children.flatMap(child => [...(matches(child) ? [child] : []), ...child.querySelectorAll(selector)]);
  }
  closest(selector) { return selector === ".fa-editor-body" && this.className === "fa-editor-body" ? this : this.parentElement?.closest(selector) ?? null; }
  focus(options) { this.ownerDocument.activeElement = this; this.focusOptions = options; }
}
function dom() {
  const document = {activeElement: null, createElement(tag) { return new Element(tag, this); }};
  const body = document.createElement("div"); body.className = "fa-editor-body";
  const container = document.createElement("div"); body.append(container);
  return {document, body, container};
}

test("real renderer shows Proxy/Visitor, safe text and native read-only steps without links", () => {
  const {container} = dom(), current = node(); current.frp_detail.proxies[0].name = '<img src=x onerror="alert(1)">';
  renderFRPDetail(container, current);
  for (const phrase of ["Proxy 代理", "Visitor 访问端", "配置期望入口", "服务端实际入口", "配置管理", "verify -c", "原生 Store", "需要重启", "只读", "<img src=x"]) assert.ok(container.textContent.includes(phrase), phrase);
  assert.equal(container.querySelectorAll("a").length, 0);
  assert.equal(container.querySelectorAll("details[data-frp-key]").length, 3);
});

test("refresh preserves expanded object, focus and scroll; node changes and logout clear private state", () => {
  const {document, body, container} = dom(); renderFRPDetail(container, node());
  const first = container.querySelectorAll("details[data-frp-key]")[0]; first.open = true;
  const focus = container.querySelectorAll("[data-frp-focus]")[0]; focus.focus(); body.scrollTop = 318;
  renderFRPDetail(container, node());
  assert.equal(container.querySelectorAll("details[data-frp-key]")[0].open, true);
  assert.notEqual(document.activeElement, focus); assert.equal(document.activeElement.dataset.frpFocus, focus.dataset.frpFocus);
  assert.deepEqual(document.activeElement.focusOptions, {preventScroll: true}); assert.equal(body.scrollTop, 318);
  renderFRPDetail(container, {...node(), id: "2"});
  assert.equal(container.querySelectorAll("details[data-frp-key]").some(item => item.open), false);
  clearFRPDetail(container); assert.equal(container.textContent, ""); assert.equal(container.dataset.frpNode, undefined);
  renderFRPDetail(container, {...node(), frp_detail_state: "stale"});
  assert.match(container.textContent, /详情已过期/); assert.doesNotMatch(container.textContent, /18000|本次完整详情没有/);
});

test("private detail reads are on demand and discard replies after node changes, snapshot changes or logout", async () => {
  const requests = [], reader = createFRPDetailReader({request: path => new Promise((resolve, reject) => requests.push({path, resolve, reject}))});
  const payload = id => ({node_id: id, state: "ready", received_at: "2026-10-03T01:00:01Z", detail: node().frp_detail});
  assert.equal(requests.length, 0); assert.equal(reader.view(node()).frp_detail_state, "waiting");
  const first = reader.load("1"), second = reader.load("2");
  assert.equal(requests[0].path, "/api/admin/v1/nodes/1/frp-detail");
  requests[0].resolve(payload("1")); await first;
  assert.equal(reader.view({...node(), id: "2"}).frp_detail, null);
  requests[1].resolve(payload("2")); await second;
  assert.equal(reader.view({...node(), id: "2"}).frp_detail.service_id, "primary");
  let current = true;
  const outdated = reader.load("2", () => current); current = false;
  requests[2].resolve(payload("2")); await outdated;
  assert.equal(reader.view({...node(), id: "2"}).frp_detail, null);
  const closing = reader.load("1"); reader.clear(); requests[3].resolve(payload("1")); await closing;
  assert.equal(reader.view(node()).frp_detail, null);
});

test("failed or mismatched detail responses hide old data and newer summaries invalidate cached observations", async () => {
  let respond = async () => ({node_id: "1", state: "ready", received_at: "2026-10-03T01:00:01Z", detail: node().frp_detail});
  const reader = createFRPDetailReader({request: () => respond()}); await reader.load("1");
  const oldSummary = {...node(), frp_detail_state: "stale"};
  assert.equal(reader.view(oldSummary).frp_detail_state, "ready");
  const newSummary = {...oldSummary, frp_detail_at: "2026-10-03T01:00:02Z"};
  assert.equal(reader.view(newSummary).frp_detail_state, "stale"); assert.equal(reader.view(newSummary).frp_detail, null);
  respond = async () => { throw new Error("private-network-error"); }; await reader.load("1");
  const model = detailModel(reader.view(node()));
  assert.equal(model.state, "unavailable"); assert.match(model.message, /读取失败.*旧详情已隐藏/); assert.doesNotMatch(model.message, /private-network/);
  respond = async () => ({node_id: "2", state: "ready", received_at: null, detail: node().frp_detail}); await reader.load("1");
  assert.equal(reader.view(node()).frp_detail, null);
});

test("a new connection never reuses the previous session detail", async () => {
  const reader = createFRPDetailReader({request: async () => ({node_id: "1", state: "ready", received_at: "2026-10-03T01:00:01Z", detail: node().frp_detail})});
  await reader.load("1");
  for (const state of ["waiting", "unsupported"]) {
    const view = reader.view({...node(), frp_detail_state: state, frp_detail_at: null});
    assert.equal(view.frp_detail_state, state);
    assert.equal(view.frp_detail, null);
  }
});

test("refresh preserves keyboard focus on an explicitly configured dashboard link", () => {
  const {container, document} = dom();
  const access = {client_dashboard_urls: {1: "https://dashboard.example.invalid/"}};
  renderFRPDetail(container, node(), null, access);
  const link = container.querySelectorAll("a")[0]; link.focus();
  renderFRPDetail(container, node(), null, access);
  assert.notEqual(document.activeElement, link);
  assert.equal(document.activeElement.href, "https://dashboard.example.invalid/");
  assert.equal(container.contains(document.activeElement), true);
});

test("only explicit safe administrator links become anchors and published identities must match exactly", () => {
  for (const raw of ["javascript:alert(1)", "https://user:secret@example.invalid", "https://example.invalid/?token=secret", "https://example.invalid/#secret", "https://example.invalid/%0a", "https:\\example.invalid"]) assert.equal(configuredLink(raw), null, raw);
  assert.equal(configuredLink("http://example.invalid/", true), null);
  assert.equal(configuredLink("http://127.0.0.1:7400/", true)?.href, "http://127.0.0.1:7400/");
  assert.equal(configuredLink("tcp://example.invalid:8000")?.href, null);
  assert.equal(configuredLink("tcp://example.invalid:8000/path"), null);
  const actual = {...proxy, user: "tenant", client_id: "full-client", online: true};
  const access = {server_dashboard_url: "https://dashboard.example.invalid/", client_dashboard_urls: {1: "https://agent.example.invalid/"}, published_endpoints: [
    {proxy_name: "echo", user: "tenant", client_id: "full-client", url: "https://public.example.invalid/"},
    {proxy_name: "echo", user: "tenant", client_id: "full-client", url: "tcp://public.example.invalid:8000"},
    {proxy_name: "echo", user: "different", client_id: "full-client", url: "https://wrong.example.invalid/"}
  ]};
  assert.equal(publishedLinks(actual, access).length, 2);
  assert.equal(publishedLinks({...actual, client_id: "other"}, access).length, 0);
  const {document, container} = dom();
  container.append(renderNativeRegistryAccess(document, {proxies: [actual]}, access));
  assert.equal(container.querySelectorAll("a").length, 2);
  assert.match(container.textContent, /tcp:\/\/public/); assert.doesNotMatch(container.textContent, /wrong\.example/);
  for (const anchor of container.querySelectorAll("a")) { assert.equal(anchor.rel, "noopener noreferrer"); assert.equal(anchor.referrerPolicy, "no-referrer"); }
  renderFRPDetail(container, node(), null, access);
  assert.equal(container.querySelectorAll("a")[0].href, "https://agent.example.invalid/");
});
