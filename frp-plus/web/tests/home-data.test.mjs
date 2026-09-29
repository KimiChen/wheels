import test from "node:test";
import assert from "node:assert/strict";
import {expiresSoon, normalizeHomeFilters, filterHomeNodes, sortHomeNodes} from "../src/home-data.mjs";
import {overview} from "../src/store.mjs";

const now = Date.UTC(2026, 8, 30, 12), day = 86400000;
const ok = value => ({quality: "ok", value});
const node = (id, session, expires, name = `Node ${id}`) => ({id, name, session,
  freshness: session === "online" ? "fresh" : "stale", groups: [{id: "2", name: "公开组"}],
  ...(expires === undefined ? {} : {billing: {expires_at_ms: expires}})});

test("seven-day expiry uses public integer timestamps and excludes already expired nodes", () => {
  for (const expires of [now + 1, now + day, now + 7 * day]) assert.equal(expiresSoon(node("1", "online", expires), now), true);
  for (const expires of [undefined, null, "1790769600000", NaN, Infinity, 8.64e15 + 1, now - 1, now, now + 7 * day + 1]) {
    assert.equal(expiresSoon(node("1", "online", expires), now), false);
  }
  const privateOnly = {...node("1", "online"), expires_at_ms: now + day, private_billing: {expires_at_ms: now + day}};
  assert.equal(expiresSoon(privateOnly, now), false);
});

test("state and expiry intersect group and name without disturbing ordering or global summaries", () => {
  const nodes = [node("1", "online", now + day, "East alpha"), node("2", "offline", now + 2 * day, "East beta"),
    node("3", "waiting", now + 3 * day, "East gamma"), node("4", "online", now + 20 * day, "East delta"),
    {...node("5", "online", now + day, "East epsilon"), groups: [{id: "3", name: "其他组"}]},
    node("6", "online", now + day, "West zeta")];
  assert.deepEqual(filterHomeNodes(nodes, {query: " east ", group: "group:2", status: "online", expiring: true}, now).map(n => n.id), ["1"]);
  assert.deepEqual(filterHomeNodes(nodes, {status: "offline"}, now).map(n => n.id), ["2"]);
  assert.deepEqual(filterHomeNodes(nodes, {status: "waiting"}, now).map(n => n.id), ["3"]);
  assert.deepEqual(filterHomeNodes(nodes, {group: "group:99", expiring: true}, now), []);
  assert.equal(overview(nodes).total, 6);
  assert.equal(nodes.filter(n => expiresSoon(n, now)).length, 5);
  assert.deepEqual(filterHomeNodes(nodes, {}, now), nodes);
});

test("stored filter values are limited to supported states and actual booleans", () => {
  for (const value of [null, undefined, "offline", {}, {status: "hidden", expiring: "true"}]) {
    assert.deepEqual(normalizeHomeFilters(value), {status: "all", expiring: false});
  }
  assert.deepEqual(normalizeHomeFilters({status: "waiting", expiring: true, group: "private"}), {status: "waiting", expiring: true});
});

test("expiry order is stable, includes expired nodes, and leaves unpublished dates last", () => {
  const nodes = [node("1", "online"), node("2", "online", now + day), node("3", "offline", now - day),
    node("4", "online", now + day), node("5", "online", "invalid")];
  assert.deepEqual(sortHomeNodes(nodes, "expiry").map(n => n.id), ["3", "2", "4", "1", "5"]);
  assert.deepEqual(nodes.map(n => n.id), ["1", "2", "3", "4", "5"]);
});

test("existing metric ordering keeps BigInt precision and missing samples last", () => {
  const nodes = [node("1", "online"), {...node("2", "online"), metrics: {net_rx_total: ok("9007199254740992")}},
    {...node("3", "online"), metrics: {net_rx_total: ok("9007199254740993")}}];
  assert.deepEqual(sortHomeNodes(nodes, "download-total").map(n => n.id), ["3", "2", "1"]);
  assert.deepEqual(sortHomeNodes(nodes, "default"), nodes);
});
