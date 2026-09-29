import test from "node:test";
import assert from "node:assert/strict";
import {memoryPercent, history, chart, resourceCharts, failureRate, coverage} from "../src/history-data.mjs";
import {connectHistory} from "../src/history-transport.mjs";

const generated_at = "2026-09-27T12:00:00Z";
const payload = (window = "1h") => ({node_id: "node-test", window, generated_at, step_seconds: 60, storage: {state: "ready", dropped: 0}, points: [], probes: []});
const turn = () => new Promise(resolve => setImmediate(resolve));
const point = (at, cpu, extra = {}) => ({at: `2026-09-27T11:${at}:00Z`, cpu, samples: 1, ...extra});
test("history ratios preserve uint64 precision and zero remains known", () => {
  assert.equal(memoryPercent({mem_used: "9007199254740993", mem_total: "18014398509481986"}), 50);
  assert.equal(memoryPercent({mem_used: "0", mem_total: "1"}), 0);
  assert.equal(memoryPercent({mem_used: "0", mem_total: "0"}), null);
  assert.equal(memoryPercent({mem_used: "2", mem_total: "1"}), null);
  assert.equal(failureRate({failure_rate: 0, samples: 1}), "0.0%");
  assert.equal(failureRate({failure_rate: 0, samples: 0}), "—");
  assert.equal(failureRate({failure_rate: 25, samples: 4}), "25.0%");
  assert.equal(coverage(0), "0 秒"); assert.equal(coverage(null), "—");
});
test("SVG paths leave explicit and implicit sample gaps without turning zero into a gap", () => {
  const rows = [point("00", 0), point("01", 50), point("02", null), point("03", 80), point("05", 20), point("06", 40), point("07", 90, {samples: 0})];
  const graph = chart(rows, resourceCharts[0].series, {window: "1h", generatedAt: generated_at, step: 60, ceiling: 100});
  assert.equal(graph.paths[0].path.match(/M/g).length, 3);
  assert.equal(graph.paths[0].path.match(/L/g).length, 2);
  assert.equal(graph.paths[0].dots.length, 5);
  assert.match(graph.paths[0].path, /^M0.00,110.00 L/);
  assert.match(graph.summaries[0], /范围 0.0% – 80.0%/);
  assert.equal(graph.empty, false);
});
test("network chart scales BigInts only after taking a bounded ratio", () => {
  const rows = [point("59", null, {net_rx: "9007199254740993", net_tx: "18014398509481986"})];
  const graph = chart(rows, resourceCharts.find(spec => spec.key === "network").series, {window: "1h", generatedAt: generated_at, step: 60});
  assert.equal(graph.maximum, 18014398509481986n);
  assert.equal(graph.paths[0].dots[0].y, "55.00");
  assert.equal(graph.paths[1].dots[0].y, "0.00");
  assert.equal(chart([point("59", null)], resourceCharts[0].series, {window: "1h", generatedAt: generated_at, step: 60}).empty, true);
});
test("history response identity, selected window and chronological bounds are checked", () => {
  assert.equal(history(payload(), "node-test", "1h").window, "1h");
  for (const override of [{node_id: "another"}, {window: "6h"}, {step_seconds: 0}, {generated_at: "invalid"}, {storage: {state: "unknown"}}, {points: [point("02", 0), point("01", 0)]}, {points: [point("01", 0), point("01", 0)]}]) {
    assert.throws(() => history({...payload(), ...override}, "node-test", "1h"));
  }
  const probe = {id: "tcp-one", name: "公开标签", samples: 1, failures: 0, points: []};
  assert.throws(() => history({...payload(), probes: [probe, probe]}, "node-test", "1h"));
});

function harness() {
  let index = 0;
  const requests = [], scheduled = new Map(), data = [], states = [];
  const connection = connectHistory({nodeID: "node-test", onData: n => data.push(n), onState: n => states.push(n),
    fetcher: (url, options) => new Promise(resolve => requests.push({url, options, resolve})),
    timer: (callback, ms) => { scheduled.set(++index, {callback, ms}); return index; }, cancel: id => scheduled.delete(id),
  });
  const resolve = (n, window = "1h") => requests[n].resolve({ok: true, json: async () => payload(window)});
  return {connection, requests, scheduled, data, states, resolve};
}
test("history starts only when visible and hiding cancels request and future polling", async () => {
  const h = harness(); assert.equal(h.requests.length, 0);
  h.connection.activate(true); assert.equal(h.requests.length, 1);
  assert.equal(h.requests[0].url, "/api/public/v1/nodes/node-test/history?window=1h");
  h.connection.activate(false); assert.equal(h.requests[0].options.signal.aborted, true);
  h.resolve(0); await turn(); assert.equal(h.data.length, 0); assert.equal(h.scheduled.size, 0);
  h.connection.activate(true); h.resolve(1); await turn();
  assert.equal(h.data.length, 1);
  assert.equal([...h.scheduled.values()][0].ms, 30000);
  h.connection.stop(); assert.equal(h.scheduled.size, 0);
});
test("switching ranges discards late replies without cancelling the new poll", async () => {
  const h = harness(); h.connection.activate(true); h.connection.select("7d");
  assert.equal(h.requests[0].options.signal.aborted, true);
  h.resolve(1, "7d"); await turn(); h.resolve(0); await turn();
  assert.deepEqual(h.data.map(n => n.window), ["7d"]);
  assert.equal(h.states.at(-1), "ready"); assert.equal(h.scheduled.size, 1);
  h.connection.stop();
});
test("malformed refresh preserves successful data and explicitly reports an error", async () => {
  const h = harness(); h.connection.activate(true); h.resolve(0); await turn();
  h.connection.refresh(); h.requests[1].resolve({ok: true, json: async () => ({})}); await turn();
  assert.equal(h.data.length, 1); assert.equal(h.states.at(-1), "error");
  assert.equal([...h.scheduled.values()][0].ms, 30000);
  h.connection.stop();
});
