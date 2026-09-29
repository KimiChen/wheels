import test from "node:test";
import assert from "node:assert/strict";
import {uint64, cumulative, decimal, bytes, ratio, percent, percentage, uptime, onlineUptime, loadText, reconciliationText} from "../src/format.mjs";
import {overview, select, snapshot} from "../src/store.mjs";
const ok = value => ({value, quality: "ok"});
test("uint64 bytes preserve integer precision and distinguish missing from zero", () => {
  assert.equal(uint64("9007199254740993"), 9007199254740993n);
  assert.equal(uint64("18446744073709551615"), 18446744073709551615n);
  assert.equal(uint64("0"), 0n);
  for (const n of ["18446744073709551616", "01", -1, 9007199254740992, null, "1e3"]) assert.equal(uint64(n), null);
  assert.equal(decimal(ok("18446744073709551615")), 18446744073709551615n);
  assert.equal(decimal({quality: "unavailable", value: "1"}), null);
  assert.equal(decimal(ok("18446744073709551616")), null);
  assert.equal(bytes(decimal(ok("18446744073709551615"))), "16.0 EiB");
  assert.equal(bytes(decimal(ok("0")), true), "0 B/s");
  assert.equal(bytes(decimal({value: null, quality: "warming_up"}), true), "—");
  assert.equal(decimal(ok(9007199254740992)), null);
  assert.equal(decimal(ok("1e8")), null);
  assert.equal(ratio(ok("9007199254740993"), ok("18014398509481986")), 50);
  assert.equal(ratio(ok("0"), ok("0")), null);
  assert.equal(percentage(percent({value: null, quality: "unavailable"})), "—");
  assert.equal(percentage(percent(ok(0))), "0.0%");
  assert.equal(uptime(ok("90061")), "1 天 1 小时");
  assert.equal(loadText(ok([0, 1.1, 2.23])), "0.00 / 1.10 / 2.23");
});
test("daily traffic sums may exceed uint64 without relaxing resource counter bounds", () => {
  const raw = "36893488147419103231";
  assert.equal(cumulative(raw), 36893488147419103231n);
  assert.equal(bytes(cumulative(raw)), "32.0 EiB");
  assert.equal(uint64(raw), null);
  assert.equal(cumulative("0"), 0n);
  assert.equal(cumulative("9".repeat(80)), BigInt("9".repeat(80)));
  for (const n of ["9".repeat(81), "01", "-1", "1e8", "1.5", "", null, 123]) assert.equal(cumulative(n), null);
});
const nodes = [
  {id: "1", name: "节点 2", session: "online", freshness: "fresh", metrics: {cpu: ok(0), net_rx: ok("9007199254740993"), net_tx: ok("0")}},
  {id: "2", name: "节点 10", session: "offline", freshness: "stale", metrics: {cpu: ok(10), net_rx: ok("9999"), net_tx: ok("9999")}},
  {id: "3", name: "等待", session: "waiting", freshness: "waiting", metrics: null},
];
test("name search preserves server order and does not change the input", () => {
  const ordered = [nodes[1], nodes[0], nodes[2]];
  assert.deepEqual(select(ordered, " 节点 ").map(n => n.id), ["2","1"]);
  assert.deepEqual(select(ordered).map(n => n.id), ["2","1","3"]);
  assert.deepEqual(select(nodes, "不存在"), []);
  const named = [{...nodes[0], name: "Test Node"}];
  assert.deepEqual(select(named, " tEsT "), named);
  assert.deepEqual(ordered.map(n => n.id), ["2","1","3"]);
  assert.deepEqual(nodes.map(n => n.id), ["1","2","3"]);
});
test("overview excludes stale and disconnected samples, but valid zero remains zero", () => {
  const stats = overview(nodes);
  assert.equal(stats.rx.value, 9007199254740993n);
  assert.equal(stats.tx.value, 0n);
  assert.equal(stats.online, 1);
  assert.equal(stats.offline, 1);
  assert.equal(overview([]).rx.value, null);
});
test("CPU average includes valid zero and excludes stale, offline and unknown samples", () => {
  const node = (cpu, session = "online", freshness = "fresh") => ({session, freshness, metrics: {cpu}});
  const valid = [node(ok(0)), node(ok(60))];
  const ignored = [
    node(ok(100), "online", "stale"),
    node(ok(100), "offline"),
    node(ok(100), "waiting", "waiting"),
    node({value: 50, quality: "unavailable"}),
    node({value: null, quality: "warming_up"}),
    node(undefined), node(ok(null)), node(ok("10")), node(ok(-1)), node(ok(101)), node(ok(Infinity)),
    {session: "online", freshness: "fresh", metrics: null},
  ];
  assert.deepEqual(overview([...valid, ...ignored]).cpu, {value: 30, count: 2});
  assert.deepEqual(overview([valid[0]]).cpu, {value: 0, count: 1});
  assert.deepEqual(overview(ignored).cpu, {value: null, count: 0});
  assert.deepEqual(overview([]).cpu, {value: null, count: 0});
});
test("malformed or duplicate identities cannot corrupt the DOM map", () => {
  assert.throws(() => snapshot({nodes: [nodes[0], nodes[0]], generated_at: "2026-01-01T00:00:00Z"}));
  assert.throws(() => snapshot({nodes, generated_at: "invalid"}));
  for (const id of ["0", "01", "-1", "1.5", "9223372036854775808", "", "../escape", 1]) assert.throws(() => snapshot({nodes: [{...nodes[0], id}], generated_at: "2026-01-01T00:00:00Z"}));
  assert.equal(snapshot({nodes: [{...nodes[0], id: "9223372036854775807"}], generated_at: "2026-01-01T00:00:00Z"}).nodes[0].id, "9223372036854775807");
  assert.equal(snapshot({nodes, generated_at: "2026-01-01T00:00:00Z"}).nodes.length, 3);
});

test("public reconciliation separates registration from agent-reported control state", () => {
  assert.equal(reconciliationText(null), "等待核对");
  assert.equal(reconciliationText({control_state: "connected"}), "等待核对");
  assert.equal(reconciliationText({reconciliation: "unbound", registered: 0}), "未设置可信绑定");
  assert.equal(reconciliationText({reconciliation: "conflict", server_online: true, registered: 9}), "归属冲突");
  assert.equal(reconciliationText({reconciliation: "stale", registered: 1}), "节点报告已过期");
  assert.equal(reconciliationText({reconciliation: "matched", server_online: true, registered: 0}), "已核对 · 服务端在线 · 已登记 0 条");
  assert.equal(reconciliationText({reconciliation: "matched", server_online: false, registered: 1}), "已核对 · 服务端离线 · 已登记 1 条");
  assert.equal(reconciliationText({reconciliation: "matched", server_online: null, registered: -1}), "已核对 · 服务端状态未知 · 登记数未知");
});

test("online uptime labels follow node status and preserve missing samples", () => {
  assert.equal(onlineUptime(ok("90061"), "online"), "在线 1 天 1 小时");
  assert.equal(onlineUptime(ok("86399"), "online"), "在线 23 小时");
  assert.equal(onlineUptime(ok("86400"), "online"), "在线 1 天 0 小时");
  assert.equal(onlineUptime(ok("3660"), "online"), "在线 1 小时");
  assert.equal(onlineUptime(ok("0"), "online"), "在线 0 小时");
  assert.equal(onlineUptime(ok("90061"), "offline"), "运行 1 天 1 小时");
  assert.equal(onlineUptime(undefined, "waiting"), "—");
  assert.equal(onlineUptime({quality: "unavailable", value: "90061"}, "online"), "—");
});
