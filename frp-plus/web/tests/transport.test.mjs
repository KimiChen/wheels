import test from "node:test";
import assert from "node:assert/strict";
import {connect} from "../src/transport.mjs";
const payload = {nodes: [], generated_at: "2026-01-01T00:00:00Z"};
const turn = () => new Promise(resolve => setImmediate(resolve));
test("GET precedes SSE; disconnect closes stream and reconnect obtains fresh snapshot", async () => {
  const calls = [], states = [], sources = [], scheduled = new Map(); let id = 0;
  class Source {
    constructor(url) { calls.push(url); sources.push(this); }
    addEventListener(type, callback) { this[type] = callback; }
    close() { this.closed = true; }
  }
  const connection = connect({
    fetcher: async url => { calls.push(url); return {ok: true, json: async () => payload}; }, Source,
    timer: (callback, ms) => { scheduled.set(++id, {callback, ms}); return id; }, cancel: id => scheduled.delete(id),
    onSnapshot: data => calls.push(data.generated_at), onState: state => states.push(state),
  });
  await turn();
  assert.deepEqual(calls.slice(0,3), ["/api/public/v1/nodes", payload.generated_at, "/events/public"]);
  sources[0].snapshot({data: JSON.stringify(payload)});
  assert.equal(states.at(-1), "live");
  sources[0].onerror();
  assert.equal(sources[0].closed, true);
  assert.equal(states.at(-1), "disconnected");
  [...scheduled.values()][0].callback();
  await turn();
  assert.deepEqual(calls.slice(-3), ["/api/public/v1/nodes", payload.generated_at, "/events/public"]);
  const before = calls.length;
  sources[0].snapshot({data: JSON.stringify(payload)});
  assert.equal(calls.length, before, "late events from replaced stream ignored");
  connection.stop();
  assert.equal(sources[1].closed, true);
  assert.equal(scheduled.size, 0);
});
test("failed or malformed GET never opens EventSource", async () => {
  let opened = false, last;
  const connection = connect({fetcher: async () => ({ok: true, json: async () => ({nodes: null})}), Source: class {constructor() {opened = true;}},
    timer: () => 1, cancel: () => {}, onSnapshot: () => assert.fail("invalid snapshot"), onState: state => {last = state;}});
  await turn();
  assert.equal(opened, false); assert.equal(last, "error"); connection.stop();
});
test("a silent SSE connection cannot remain marked live indefinitely", async () => {
  const scheduled = new Map(); let id = 0, source, state;
  class Source {
    constructor() { source = this; }
    addEventListener(type, callback) { this[type] = callback; }
    close() { this.closed = true; }
  }
  const connection = connect({fetcher: async () => ({ok: true, json: async () => payload}), Source,
    timer: (callback, ms) => {scheduled.set(++id, {callback, ms}); return id;}, cancel: id => scheduled.delete(id),
    onSnapshot: () => {}, onState: next => {state = next;}});
  await turn();
  source.snapshot({data: JSON.stringify(payload)});
  assert.equal(state, "live");
  [...scheduled.values()].find(task => task.ms === 10000).callback();
  assert.equal(source.closed, true);
  assert.equal(state, "disconnected");
  connection.stop();
});
