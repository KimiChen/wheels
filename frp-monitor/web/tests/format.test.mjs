import test from "node:test";
import assert from "node:assert/strict";
import {decimal, bytes, ratio, percent, percentage, uptime, loadText} from "../src/format.mjs";
import {overview, select, snapshot} from "../src/store.mjs";
const ok = value => ({value, quality: "ok"});
test("uint64 bytes preserve integer precision and distinguish missing from zero", () => {
  assert.equal(decimal(ok("18446744073709551615")), 18446744073709551615n);
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
const nodes = [
  {id: "a", name: "节点 2", session: "online", freshness: "fresh", metrics: {cpu: ok(0), net_rx: ok("9007199254740993"), net_tx: ok("0")}, frp: {control_state: "connected", proxy_total: 0, proxy_running: 0}},
  {id: "b", name: "节点 10", session: "offline", freshness: "stale", metrics: {cpu: ok(10), net_rx: ok("9999"), net_tx: ok("9999")}, frp: {control_state: "disconnected", proxy_total: 9, proxy_running: 9}},
  {id: "c", name: "等待", session: "waiting", freshness: "waiting", metrics: null},
];
test("sorting puts missing CPU last and keeps search/filter input data unchanged", () => {
  assert.deepEqual(select(nodes, " 节点 ", "all", "cpu").map(n => n.id), ["b","a"]);
  assert.deepEqual(select(nodes, "", "online").map(n => n.id), ["a"]);
  assert.deepEqual(select(nodes, "", "all", "cpu").map(n => n.id), ["b","a","c"]);
  assert.deepEqual(nodes.map(n => n.id), ["a","b","c"]);
});
test("overview excludes stale and disconnected samples, but valid zero remains zero", () => {
  const stats = overview(nodes);
  assert.equal(stats.rx.value, 9007199254740993n);
  assert.equal(stats.tx.value, 0n);
  assert.equal(stats.proxyRunning, 0);
  assert.equal(stats.online, 1);
  assert.equal(stats.stale, 1);
  assert.equal(overview([]).rx.value, null);
  assert.equal(overview([]).proxyTotal, null);
});
test("malformed or duplicate identities cannot corrupt the DOM map", () => {
  assert.throws(() => snapshot({nodes: [nodes[0], nodes[0]], generated_at: "2026-01-01T00:00:00Z"}));
  assert.throws(() => snapshot({nodes, generated_at: "invalid"}));
  assert.equal(snapshot({nodes, generated_at: "2026-01-01T00:00:00Z"}).nodes.length, 3);
});
