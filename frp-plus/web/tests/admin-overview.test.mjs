import test from "node:test";
import assert from "node:assert/strict";
import {expiryState, overview, filterAdminNodes, expiryCalendar, localDayKey, requiresReview} from "../src/admin-overview.mjs";

const now = Date.UTC(2026, 8, 30, 12), day = 86400000;
const node = (id, session, at = null) => ({id, name: `节点 ${id}`, session, settings: {expires_at_ms: at}});
test("overview keeps waiting distinct, counts near expiry once and does not claim unavailable FRP is healthy", () => {
  const nodes = [node("1", "online", now + 7 * day), node("2", "offline", now), node("3", "waiting"), node("4", "online", now + 7 * day + 1)];
  const snapshot = {nodes, frp: {state: "ready", nodes: [{id: "1", state: "matched", server_online: true}, {id: "2", state: "mismatch"}, {id: "3", state: "unbound"}]}};
  const result = overview(snapshot, now);
  assert.deepEqual(result.counts, {total: 4, online: 2, offline: 1, waiting: 1, due: 1, expired: 1, review: 2});
  assert.equal(result.attention.length, 3);
  assert.deepEqual(result.attention[0].reasons, ["expired", "offline", "review"]);
  assert.equal(overview({...snapshot, frp: {state: "unavailable"}}, now).counts.review, null);
  assert.equal(overview({nodes}, now).reviewAvailable, false);
  assert.deepEqual(nodes.map(item => item.id), ["1", "2", "3", "4"]);
});
test("expired timestamps and unknown dates are distinct; disabled proxies do not create false FRP warnings", () => {
  for (const at of [null, undefined, "0", NaN, Infinity, 8640000000000001]) assert.equal(expiryState(node("1", "online", at), now), "unset");
  assert.equal(expiryState(node("1", "online", 0), now), "expired");
  assert.equal(expiryState(node("1", "online", now + 1), now), "due");
  assert.equal(requiresReview({state: "matched", proxies: [{enabled: false, server_state: "missing"}]}), false);
  assert.equal(requiresReview({state: "matched", proxies: [{enabled: true, server_state: "missing"}]}), true);
  assert.equal(requiresReview({state: "matched", server_online: false}), true);
});
test("directory intersects state, multi-membership groups and private address search without duplicates", () => {
  const nodes = [node("1", "online", now + day), {...node("2", "offline"), facts: {ipv4: {quality: "ok", value: "192.0.2.2"}}}, node("3", "waiting")];
  const snapshot = {nodes, groups: [{id: "10", name: "生产", node_ids: ["1", "2"]}, {id: "20", name: "亚洲", node_ids: ["1"]}]};
  assert.deepEqual(filterAdminNodes(snapshot, {group: "10", query: "192.0.2", state: "offline"}, now).map(item => item.id), ["2"]);
  assert.deepEqual(filterAdminNodes(snapshot, {group: "20", state: "due"}, now).map(item => item.id), ["1"]);
  assert.deepEqual(filterAdminNodes(snapshot, {group: "ungrouped"}, now).map(item => item.id), ["3"]);
  assert.deepEqual(filterAdminNodes(snapshot, {group: "deleted"}, now), []);
  assert.deepEqual(filterAdminNodes(snapshot, {state: "review"}, now), []);
  assert.equal(filterAdminNodes(snapshot, {}, now).length, 3);
});
test("expiry calendar observes local month boundaries, leap days and multiple nodes on a day", () => {
  const first = new Date(2024, 1, 1).getTime(), last = new Date(2024, 2, 1).getTime() - 1;
  const nodes = [node("1", "online", first), node("2", "online", last), node("3", "offline", last), node("4", "online", first - 1), node("5", "online", last + 1), node("6", "waiting")];
  const calendar = expiryCalendar(nodes, 2024, 1);
  assert.equal(calendar.offset, 3);
  assert.equal(calendar.days.length, 29);
  assert.equal(calendar.days[0].key, "2024-02-01");
  assert.deepEqual(calendar.days[28].nodes.map(item => item.id), ["2", "3"]);
  assert.equal(calendar.days.flatMap(item => item.nodes).length, 3);
  assert.equal(localDayKey(last), "2024-02-29");
});
