import test from "node:test";
import assert from "node:assert/strict";
import {adminClient, adminSnapshot, probeDocument, nextProbeDocument, byteText, fieldText, errorText} from "../src/admin-data.mjs";

const session = {csrf_token: "memory-only-csrf-value", expires_at: "2026-09-28T00:00:00Z"};
const ok = (data, status = 200) => ({ok: true, status, json: async () => data});
function harness() {
  const requests = [], timers = new Map(); let expired = 0, nextTimer = 0;
  const client = adminClient({fetcher: (url, options) => new Promise(resolve => requests.push({url, options, resolve})), onExpired: () => expired++, timer: callback => { timers.set(++nextTimer, callback); return nextTimer; }, cancel: id => timers.delete(id)});
  return {client, requests, timers, expired: () => expired};
}
test("admin writes use memory CSRF, same-origin cookies and no token in URLs", async () => {
  const h = harness(); const login = h.client.login("private-admin-token");
  assert.equal(h.requests[0].url, "/api/admin/v1/login");
  assert.deepEqual(JSON.parse(h.requests[0].options.body), {token: "private-admin-token"});
  assert.equal(h.requests[0].options.headers["X-CSRF-Token"], undefined);
  h.requests[0].resolve(ok(session)); await login;
  const create = h.client.request("/api/admin/v1/nodes", {method: "POST", body: {name: "公开标签"}});
  assert.equal(h.requests[1].options.headers["X-CSRF-Token"], session.csrf_token);
  assert.equal(h.requests[1].options.credentials, "same-origin");
  assert.equal(h.requests[1].options.cache, "no-store");
  assert.equal(h.requests[1].options.redirect, "error");
  h.requests[1].resolve(ok({id: "node-test", token: "once-only"}, 201)); await create;
  assert.equal(h.timers.size, 0);
  await assert.rejects(h.client.request("https://example.test/api/admin/v1/nodes"));
});
test("expired or rejected CSRF ends the session, cancels reads, and disallows later writes", async () => {
  for (const status of [401, 403]) {
    const h = harness(); const restore = h.client.session(); h.requests[0].resolve(ok(session)); await restore;
    const reading = h.client.request("/api/admin/v1/nodes");
    const writing = h.client.request("/api/admin/v1/nodes/node-test/rotate", {method: "POST"});
    h.requests[2].resolve({ok: false, status}); await assert.rejects(writing, error => error.status === status);
    assert.equal(h.expired(), 1); assert.equal(h.requests[1].options.signal.aborted, true);
    h.requests[1].resolve(ok({private: true})); await assert.rejects(reading, {name: "AbortError"});
    await assert.rejects(h.client.request("/api/admin/v1/logout", {method: "POST"}), error => error.status === 401);
    assert.equal(h.requests.length, 3);
  }
});
test("clearing the page discards late credential and session responses", async () => {
  const h = harness(); const login = h.client.login("token"); h.client.clear();
  h.requests[0].resolve(ok(session)); await assert.rejects(login, {name: "AbortError"});
  const restore = h.client.session(); h.requests[1].resolve(ok(session)); await restore;
  const rotate = h.client.request("/api/admin/v1/nodes/node-test/rotate", {method: "POST"});
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
  const node = {id: "node-test", name: "测试节点", session: "online", freshness: "fresh", facts: null, frp: null};
  const payload = {generated_at: "2026-09-27T12:00:00Z", credentials_state: "ready", nodes: [node]};
  assert.equal(adminSnapshot(payload), payload);
  for (const changed of [{generated_at: "bad"}, {credentials_state: "unknown"}, {nodes: [node, node]}, {nodes: [{...node, session: "running"}]}, {nodes: [{...node, id: "../escape"}]}]) assert.throws(() => adminSnapshot({...payload, ...changed}));
});
test("probe replacement preserves all rows, increments version and allows empty clearing", () => {
  const current = {version: 7, nodes: [{agent_id: "node-test", tasks: [{id: "测试 task", name: "公开标签", target: "[2001:db8::1]:443", interval: 30}]}]};
  assert.equal(probeDocument(current), current);
  const rows = [{agent_id: "node-test", ...current.nodes[0].tasks[0]}, {agent_id: "node-two2", id: "tcp", name: "第二项", target: "example.test:80", interval: "5"}];
  const next = nextProbeDocument(current, rows, new Set(["node-test", "node-two2"]));
  assert.equal(next.version, 8); assert.equal(next.nodes.length, 2); assert.equal(next.nodes[1].tasks[0].interval, 5);
  assert.deepEqual(nextProbeDocument(current, [], new Set()), {version: 8, nodes: []});
  for (const change of [{interval: "4"}, {interval: "5.5"}, {target: "https://example.test:443"}, {target: "example.test:65536"}, {target: "[::1]"}, {id: "a\nb"}, {name: "字".repeat(43)}, {agent_id: "unknown"}]) assert.throws(() => nextProbeDocument(current, [{...rows[0], ...change}], new Set(["node-test"])));
  assert.throws(() => nextProbeDocument(current, [rows[0], rows[0]], new Set(["node-test"])));
  assert.throws(() => nextProbeDocument({version: Number.MAX_SAFE_INTEGER, nodes: []}, [], new Set()));
});
test("private facts and FRP byte values retain unknown versus zero and integer precision", () => {
  assert.equal(byteText("0"), "0 B"); assert.equal(byteText(null), "—"); assert.equal(byteText(9007199254740992), "—");
  assert.equal(byteText("18446744073709551615"), "16.0 EiB");
  assert.equal(fieldText({quality: "ok", value: 0}), "0"); assert.equal(fieldText({quality: "unsupported", value: null}), "暂不支持");
  assert.match(errorText({status: 409}), /重新读取/);
});
