import test from "node:test";
import assert from "node:assert/strict";
import {adminClient, adminSnapshot, groupDocument, groupDraft, groupDraftState, groupRequest, probeDocument, nextProbeDocument, byteText, fieldText, errorText} from "../src/admin-data.mjs";

const session = {csrf_token: "memory-only-csrf-value", expires_at: "2026-09-28T00:00:00Z"};
const ok = (data, status = 200) => ({ok: true, status, json: async () => data});
function harness() {
  const requests = [], timers = new Map(); let expired = 0, nextTimer = 0;
  const client = adminClient({fetcher: (url, options) => new Promise(resolve => requests.push({url, options, resolve})), onExpired: () => expired++, timer: callback => { timers.set(++nextTimer, callback); return nextTimer; }, cancel: id => timers.delete(id)});
  return {client, requests, timers, expired: () => expired};
}
test("GitHub session writes use memory CSRF, same-origin cookies and no token-login method", async () => {
  const h = harness();
  await assert.rejects(h.client.request("/api/admin/v1/nodes", {method: "POST", anonymous: true}), error => error.status === 401);
  assert.equal(h.requests.length, 0);
  const login = h.client.session();
  assert.equal(h.requests[0].url, "/api/admin/v1/session");
  assert.equal(h.requests[0].options.body, undefined);
  assert.equal(h.client.login, undefined);
  assert.equal(h.requests[0].options.headers["X-CSRF-Token"], undefined);
  h.requests[0].resolve(ok(session)); await login;
  const create = h.client.request("/api/admin/v1/nodes", {method: "POST", body: {name: "公开标签"}});
  assert.equal(h.requests[1].options.headers["X-CSRF-Token"], session.csrf_token);
  assert.equal(h.requests[1].options.credentials, "same-origin");
  assert.equal(h.requests[1].options.cache, "no-store");
  assert.equal(h.requests[1].options.redirect, "error");
  h.requests[1].resolve(ok({id: "1", token: "once-only"}, 201)); await create;
  assert.equal(h.timers.size, 0);
  await assert.rejects(h.client.request("https://example.test/api/admin/v1/nodes"));
});
test("expired or rejected CSRF ends the session, cancels reads, and disallows later writes", async () => {
  for (const status of [401, 403]) {
    const h = harness(); const restore = h.client.session(); h.requests[0].resolve(ok(session)); await restore;
    const reading = h.client.request("/api/admin/v1/nodes");
    const writing = h.client.request("/api/admin/v1/nodes/1/rotate", {method: "POST"});
    h.requests[2].resolve({ok: false, status}); await assert.rejects(writing, error => error.status === status);
    assert.equal(h.expired(), 1); assert.equal(h.requests[1].options.signal.aborted, true);
    h.requests[1].resolve(ok({private: true})); await assert.rejects(reading, {name: "AbortError"});
    await assert.rejects(h.client.request("/api/admin/v1/logout", {method: "POST"}), error => error.status === 401);
    assert.equal(h.requests.length, 3);
  }
});
test("clearing the page discards late credential and session responses", async () => {
  const h = harness(); const login = h.client.session(); h.client.clear();
  h.requests[0].resolve(ok(session)); await assert.rejects(login, {name: "AbortError"});
  const restore = h.client.session(); h.requests[1].resolve(ok(session)); await restore;
  const rotate = h.client.request("/api/admin/v1/nodes/1/rotate", {method: "POST"});
  h.client.clear(); h.requests[2].resolve(ok({token: "must-not-render"})); await assert.rejects(rotate, {name: "AbortError"});
  assert.equal(h.timers.size, 0);
});
test("request timeout aborts and a late response cannot render private data", async () => {
  const h = harness(); const pending = h.client.request("/api/admin/v1/session");
  [...h.timers.values()][0](); assert.equal(h.requests[0].options.signal.aborted, true);
  h.requests[0].resolve(ok(session)); await assert.rejects(pending, {name: "AbortError"});
});
test("logout clears memory even when the server cannot be reached", async () => {
  const h = harness(); const restore = h.client.session(); h.requests[0].resolve(ok(session)); await restore;
  const logout = h.client.logout(); h.requests[1].resolve({ok: false, status: 503}); await assert.rejects(logout);
  await assert.rejects(h.client.request("/api/admin/v1/nodes", {method: "POST"}), error => error.status === 401);
});
test("snapshot validates node identity and states without accepting duplicate rows", () => {
  const node = {id: "1", name: "测试节点", session: "online", freshness: "fresh", facts: null, frp: null};
  const payload = {generated_at: "2026-09-27T12:00:00Z", credentials_state: "ready", groups_state: "ready", groups: [], nodes: [node]};
  assert.equal(adminSnapshot(payload), payload);
  for (const changed of [{generated_at: "bad"}, {credentials_state: "unknown"}, {groups_state: "unknown"}, {groups: null}, {nodes: [node, node]}, {nodes: [{...node, session: "running"}]}, {nodes: [{...node, id: "../escape"}]}]) assert.throws(() => adminSnapshot({...payload, ...changed}));
});
test("group documents accept multiple memberships and empty groups, with bounded valid identities", () => {
  const group = {id: "1", name: "亚洲", node_ids: ["1", "2"], config_revision: 1};
  const document = {groups: [group, {...group, id: "2", name: "生产"}, {...group, id: "3", name: "待接入", node_ids: []}]};
  assert.equal(groupDocument(document), document);
  assert.deepEqual(groupDocument({groups: []}), {groups: []});
  assert.doesNotThrow(() => groupDocument({groups: [{...group, id: "9223372036854775807", name: `${"字".repeat(42)}ab`}]}));
  for (const change of [{id: "01"}, {id: "9223372036854775808"}, {name: " "}, {name: " 亚洲 "}, {name: "字".repeat(43)}, {name: "a\u0085b"}, {node_ids: ["1", "1"]}, {node_ids: ["0"]}, {node_ids: Array(1025).fill("1")}, {config_revision: 0}, {config_revision: Number.MAX_SAFE_INTEGER + 1}]) assert.throws(() => groupDocument({groups: [{...group, ...change}]}));
  assert.throws(() => groupDocument({groups: [group, group]}));
  assert.throws(() => groupDocument({groups: Array.from({length: 129}, (_, i) => ({...group, id: `${i + 1}`}))}));
});
test("group requests trim names, measure UTF-8 bytes, retain revision and allow no members", () => {
  const nodes = new Set(["1", "2"]), draft = groupDraft();
  draft.name = `  ${"字".repeat(42)}ab  `;
  assert.deepEqual(groupRequest(draft, nodes), {name: `${"字".repeat(42)}ab`, node_ids: []});
  const existing = {id: "8", name: "亚洲", node_ids: ["1", "2"], config_revision: 7};
  assert.deepEqual(groupRequest(groupDraft(existing), nodes), {name: "亚洲", node_ids: ["1", "2"], config_revision: 7});
  for (const name of ["", "  ", "字".repeat(43), "a\nb", "a\u007fb", "a\u009fb", "\tasia"]) assert.throws(() => groupRequest({...draft, name}, nodes), /名称/);
  assert.throws(() => groupRequest({...draft, name: "亚洲", node_ids: ["3"]}, nodes), /节点已不存在/);
  assert.throws(() => groupRequest({...draft, name: "亚洲", node_ids: ["1", "1"]}, nodes));
  assert.throws(() => groupRequest({...existing, config_revision: null}, nodes), /重新读取/);
});
test("group polling detects changes and deletion without changing a draft or its revision", () => {
  const current = {id: "1", name: "亚洲", node_ids: ["1"], config_revision: 5}, draft = groupDraft(current);
  draft.name = "待保存的中文名称"; draft.node_ids.push("2");
  const before = structuredClone(draft);
  assert.equal(groupDraftState(draft, [current]), "current");
  assert.equal(groupDraftState(draft, [{...current, name: "其他管理员改名", node_ids: [], config_revision: 6}]), "changed");
  assert.equal(groupDraftState(draft, []), "deleted");
  assert.deepEqual(draft, before); assert.deepEqual(current.node_ids, ["1"]);
  assert.equal(groupDraftState(groupDraft(), [current]), "new");
  assert.equal(groupRequest(draft, new Set(["1", "2"])).config_revision, 5);
});
test("group create, update and delete use CSRF; revision conflicts preserve the session", async () => {
  const h = harness(), restore = h.client.session(); h.requests[0].resolve(ok(session)); await restore;
  const draft = groupDraft({id: "1", name: "亚洲", node_ids: ["1"], config_revision: 3});
  for (const [index, method, path, body] of [[1, "POST", "/api/admin/v1/groups", {name: "空分组", node_ids: []}], [2, "PATCH", "/api/admin/v1/groups/1", groupRequest(draft, new Set(["1"]))], [3, "DELETE", "/api/admin/v1/groups/1", {config_revision: 3}]]) {
    const writing = h.client.request(path, {method, body});
    const request = h.requests[index]; assert.equal(request.url, path); assert.equal(request.options.method, method);
    assert.equal(request.options.headers["X-CSRF-Token"], session.csrf_token); assert.equal(request.options.credentials, "same-origin");
    assert.deepEqual(JSON.parse(request.options.body), body);
    if (method === "PATCH") { request.resolve({ok: false, status: 409}); await assert.rejects(writing, error => error.status === 409); }
    else { request.resolve(ok(null, method === "DELETE" ? 204 : 201)); await writing; }
  }
  assert.equal(h.expired(), 0); assert.equal(draft.config_revision, 3); assert.equal(draft.name, "亚洲");
  const reading = h.client.request("/api/admin/v1/groups"); h.client.clear(); h.requests[4].resolve(ok({groups: [draft]}));
  await assert.rejects(reading, {name: "AbortError"});
});
test("probe replacement preserves all rows, increments version and allows empty clearing", () => {
  const current = {version: 7, nodes: [{agent_id: "1", tasks: [{id: "测试 task", name: "公开标签", target: "[2001:db8::1]:443", interval: 30}]}]};
  assert.equal(probeDocument(current), current);
  const rows = [{agent_id: "1", ...current.nodes[0].tasks[0]}, {agent_id: "2", id: "tcp", name: "第二项", target: "example.test:80", interval: "5"}];
  const next = nextProbeDocument(current, rows, new Set(["1", "2"]));
  assert.equal(next.version, 8); assert.equal(next.nodes.length, 2); assert.equal(next.nodes[1].tasks[0].interval, 5);
  assert.deepEqual(nextProbeDocument(current, [], new Set()), {version: 8, nodes: []});
  for (const change of [{interval: "4"}, {interval: "5.5"}, {target: "https://example.test:443"}, {target: "example.test:65536"}, {target: "[::1]"}, {id: "a\nb"}, {name: "字".repeat(43)}, {agent_id: "unknown"}]) assert.throws(() => nextProbeDocument(current, [{...rows[0], ...change}], new Set(["1"])));
  assert.throws(() => nextProbeDocument(current, [rows[0], rows[0]], new Set(["1"])));
  assert.throws(() => nextProbeDocument({version: Number.MAX_SAFE_INTEGER, nodes: []}, [], new Set()));
});
test("private facts and FRP byte values retain unknown versus zero and integer precision", () => {
  assert.equal(byteText("0"), "0 B"); assert.equal(byteText(null), "—"); assert.equal(byteText(9007199254740992), "—");
  assert.equal(byteText("18446744073709551615"), "16.0 EiB");
  assert.equal(fieldText({quality: "ok", value: 0}), "0"); assert.equal(fieldText({quality: "unsupported", value: null}), "暂不支持");
  assert.match(errorText({status: 409}), /重新读取/);
});

test('administrative errors expose only a bounded code, never raw downstream errors', async () => {
  for (const [body, expected] of [[{code: 'source_drift', error: 'private native error'}, 'source_drift'], [{code: 'https://private.invalid/secret'}, undefined], [{code: 'x'.repeat(65)}, undefined]]) {
    const h = harness(); const result = h.client.request('/api/admin/v1/nodes/1/configuration');
    h.requests[0].resolve({ok: false, status: 409, json: async () => body});
    await assert.rejects(result, error => error.status === 409 && error.code === expected && error.message === 'request_failed');
  }
});
