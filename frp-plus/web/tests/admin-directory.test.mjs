import test from "node:test";
import assert from "node:assert/strict";
import {directoryRow} from "../src/admin-directory.mjs";

const ok = value => ({quality: "ok", value});
const node = {id: "3", name: "节点", session: "online", settings: {is_public: false}, facts: {os: ok("Debian GNU/Linux 12 (bookworm)"), ipv4: ok("192.0.2.3"), ipv6: ok("2001:db8::3")}};

test("directory shows private addresses only when valid, even for offline nodes, without fabricating public state", () => {
  const groups = [{id: "1", name: "亚洲", node_ids: ["3"]}, {id: "2", name: "生产", node_ids: ["3", "4"]}];
  const row = directoryRow({...node, session: "offline"}, groups);
  assert.equal(row.summary, "#3 · Debian 12 · 隐藏");
  assert.equal(row.session, "离线"); assert.equal(row.ipv4, "192.0.2.3"); assert.equal(row.ipv6, "2001:db8::3");
  assert.deepEqual(row.groups, ["亚洲", "生产"]);
  for (const field of [null, ok(null), ok("  "), ok(0), {quality: "unavailable", value: "192.0.2.3"}]) {
    const missing = directoryRow({...node, facts: {ipv4: field, ipv6: field}, settings: undefined});
    assert.equal(missing.ipv4, "—"); assert.equal(missing.ipv6, "—");
    assert.equal(missing.summary, "#3 · 系统信息待上报");
  }
});

test("directory preserves exact large usage ratios and separates zero, unlimited, missing and overage", () => {
  const plan = traffic_plan => directoryRow({...node, traffic_plan}).plan;
  const large = plan({used_bytes: "27021597764222979", quota_bytes: "18014398509481986"});
  assert.equal(large.text, "150.0%"); assert.equal(large.meter, 100);
  const zero = plan({used_bytes: "0", quota_bytes: "100"});
  assert.equal(zero.text, "0.0%"); assert.equal(zero.meter, 0); assert.match(zero.title, /已用 0 B \/ 100 B/);
  const noQuota = plan({used_bytes: "0", quota_bytes: "0"});
  assert.equal(noQuota.text, "—"); assert.equal(noQuota.meter, null); assert.match(noQuota.title, /0 B \/ 0 B/);
  const unlimited = plan({used_bytes: "123", quota_bytes: null});
  assert.equal(unlimited.text, "∞"); assert.equal(unlimited.meter, null); assert.match(unlimited.title, /123 B \/ ∞/);
  for (const source of [undefined, {}, {used_bytes: null, quota_bytes: "100"}, {used_bytes: 0, quota_bytes: "100"}]) {
    assert.equal(plan(source).text, "—"); assert.equal(plan(source).meter, null);
  }
});

test("directory keeps relative expiry paired with exact timestamp and never interprets absent date as epoch", () => {
  const now = Date.UTC(2026, 8, 30, 12), day = 86400000;
  const expires = at => directoryRow({...node, settings: {expires_at_ms: at}}, [], now).expires;
  const due = expires(now + day + 1);
  assert.equal(due.text, "2 天后"); assert.equal(due.state, "due"); assert.notEqual(due.title, "—");
  assert.equal(expires(now).text, "已到期"); assert.equal(expires(0).state, "expired");
  for (const at of [undefined, null]) { assert.equal(expires(at).text, "未设置"); assert.equal(expires(at).state, "unset"); }
  for (const at of ["0", NaN, Infinity, Number.MAX_SAFE_INTEGER]) {
    assert.equal(expires(at).text, "—"); assert.equal(expires(at).state, "unset");
  }
});
