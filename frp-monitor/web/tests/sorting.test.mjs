import test from "node:test";
import assert from "node:assert/strict";
import {sortNodes, select, overview, groupKey} from "../src/store.mjs";

const ok = value => ({value, quality: "ok"});
const ids = nodes => nodes.map(node => node.id);

test("default order and equal values preserve server order without mutating the snapshot", () => {
  const nodes = Object.freeze([{id: "3", metrics: {cpu: ok(50)}}, {id: "1", metrics: {cpu: ok(50)}}]);
  for (const order of ["default", "cpu", "invalid", "__proto__"]) {
    assert.deepEqual(ids(sortNodes(nodes, order)), ["3", "1"]);
    assert.notEqual(sortNodes(nodes, order), nodes);
  }
});

test("name and system sorts use natural ascending order and missing systems go last", () => {
  const nodes = [
    {id: "1", name: "Node 10", hardware: {os: ok("Debian 13")}},
    {id: "2", name: "node 2", hardware: null},
    {id: "3", name: "Node 1", hardware: {os: ok("Debian 9")}},
    {id: "4", name: "Node 3", hardware: {os: {value: "Alpine", quality: "unavailable"}}},
  ];
  assert.deepEqual(ids(sortNodes(nodes, "name")), ["3", "2", "4", "1"]);
  assert.deepEqual(ids(sortNodes(nodes, "system")), ["3", "1", "2", "4"]);
});

test("uptime, rates and cumulative counters sort descending with uint64 precision", () => {
  for (const [order, key] of [["uptime", "uptime"], ["upload", "net_tx"], ["download", "net_rx"], ["upload-total", "net_tx_total"], ["download-total", "net_rx_total"]]) {
    const nodes = [
      {id: "1", metrics: {[key]: ok("9007199254740992")}},
      {id: "2", metrics: null},
      {id: "3", metrics: {[key]: ok("9007199254740993")}},
      {id: "4", metrics: {[key]: ok("0")}},
      {id: "5", metrics: {[key]: {value: "18446744073709551615", quality: "unavailable"}}},
    ];
    assert.deepEqual(ids(sortNodes(nodes, order)), ["3", "1", "4", "2", "5"], order);
  }
});

test("CPU ranks valid zero above missing or invalid values", () => {
  const nodes = [
    {id: "1", metrics: {cpu: ok(0)}}, {id: "2", metrics: {cpu: ok(100.1)}},
    {id: "3", metrics: {cpu: ok(60)}}, {id: "4", metrics: {cpu: ok("99")}},
  ];
  assert.deepEqual(ids(sortNodes(nodes, "cpu")), ["3", "1", "2", "4"]);
});

test("memory and storage sort by precise utilization rather than total size or rounded percentages", () => {
  for (const [order, prefix] of [["memory", "mem"], ["disk", "disk"]]) {
    const node = (id, used, total) => ({id, metrics: {[`${prefix}_used`]: ok(used), [`${prefix}_total`]: ok(total)}});
    const nodes = [node("1", "9007199254740992", "18014398509481984"), node("2", "3", "4"),
      node("3", "9007199254740993", "18014398509481984"), node("4", "0", "0"), node("5", "5", "4"), node("6", "0", "4")];
    assert.deepEqual(ids(sortNodes(nodes, order)), ["2", "3", "1", "6", "4", "5"], order);
  }
});

test("sorting composes with group and name filters without changing the underlying counts", () => {
  const group = {id: "1", name: "示例分组"};
  const nodes = [{id: "1", name: "Node 10", groups: [group]}, {id: "2", name: "Node 2", groups: [group]},
    {id: "3", name: "Node 1", groups: []}, {id: "4", name: "Other", groups: [group]}];
  assert.deepEqual(ids(select(sortNodes(nodes, "name"), "node", groupKey("1"))), ["2", "1"]);
  assert.deepEqual(ids(nodes), ["1", "2", "3", "4"]);
});

test("network totals include last reported offline counters, while rates use only fresh online nodes", () => {
  const max = "18446744073709551615";
  const metric = {net_tx: ok("100"), net_rx: ok("0"), net_tx_total: ok(max), net_rx_total: ok("0")};
  const stats = overview([
    {session: "online", freshness: "fresh", metrics: metric},
    {session: "offline", freshness: "stale", metrics: metric},
    {session: "waiting", freshness: "waiting", metrics: null},
    {session: "online", freshness: "fresh", metrics: {net_tx_total: {value: max, quality: "unavailable"}}},
  ]);
  assert.deepEqual(stats.txTotal, {value: 36893488147419103230n, count: 2});
  assert.deepEqual(stats.rxTotal, {value: 0n, count: 2});
  assert.deepEqual(stats.tx, {value: 100n, count: 1});
  assert.deepEqual(stats.rx, {value: 0n, count: 1});
  assert.equal(stats.total, 4); assert.equal(stats.online, 2); assert.equal(stats.offline, 1); assert.equal(stats.waiting, 1);
  assert.deepEqual(overview([]).txTotal, {value: null, count: 0});
});
