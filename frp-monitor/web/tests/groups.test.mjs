import test from "node:test";
import assert from "node:assert/strict";
import {snapshot, select, overview, groupOptions, groupKey, resolveGroupSelection, ALL_GROUPS, UNGROUPED} from "../src/store.mjs";
import {nodeGroups, groupText} from "../src/node-data.mjs";

const group = (id, name) => ({id, name});
const asia = group("2", "亚洲"), europe = group("10", "欧洲");
const node = (id, name, groups) => ({id, name, groups, session: "online", freshness: "fresh"});
const nodes = [node("1", "HK Alpha", [asia, europe]), node("2", "DE Alpha", [europe]), node("3", "HK Beta", [])];
const now = "2026-09-28T12:00:00Z";

test("public group counts derive only from supplied nodes and count each membership once", () => {
  const options = groupOptions(nodes);
  assert.deepEqual(options, [
    {key: ALL_GROUPS, name: "全部", count: 3},
    {key: UNGROUPED, name: "未分组", count: 1},
    {key: groupKey("2"), name: "亚洲", count: 1},
    {key: groupKey("10"), name: "欧洲", count: 2},
  ]);
  assert.deepEqual(groupOptions([node("1", "repeated", [asia, asia])]).at(-1), {key: groupKey("2"), name: "亚洲", count: 1});
  // A global directory, if accidentally attached, must not disclose groups
  // which have no publicly visible node.
  const publicData = snapshot({generated_at: now, nodes: [nodes[2]], groups: [group("99", "private-only")]});
  assert.deepEqual(groupOptions(publicData.nodes).map(option => option.key), [ALL_GROUPS, UNGROUPED]);
});

test("multi-group filters intersect name search without duplicating or reordering nodes", () => {
  assert.deepEqual(select(nodes, "", groupKey("10")).map(node => node.id), ["1", "2"]);
  assert.deepEqual(select(nodes, " hK ", groupKey("10")).map(node => node.id), ["1"]);
  assert.deepEqual(select(nodes, "alpha", groupKey("2")).map(node => node.id), ["1"]);
  assert.deepEqual(select(nodes, "beta", groupKey("2")), []);
  assert.deepEqual(select(nodes, "", UNGROUPED).map(node => node.id), ["3"]);
  assert.deepEqual(select(nodes, "alpha", UNGROUPED), []);
  assert.deepEqual(select(nodes, "", ALL_GROUPS), nodes);
  assert.deepEqual(select(nodes, "", groupKey("99")), []);
  assert.deepEqual(nodes.map(node => node.id), ["1", "2", "3"]);
  assert.equal(overview(nodes).total, 3);
});

test("built-in filters do not collide with real group names", () => {
  const reserved = [node("1", "A", [group("1", "全部")]), node("2", "B", [group("2", "未分组")]), node("3", "C", [])];
  assert.deepEqual(select(reserved, "", ALL_GROUPS).map(node => node.id), ["1", "2", "3"]);
  assert.deepEqual(select(reserved, "", UNGROUPED).map(node => node.id), ["3"]);
  assert.deepEqual(select(reserved, "", groupKey("1")).map(node => node.id), ["1"]);
  assert.deepEqual(select(reserved, "", groupKey("2")).map(node => node.id), ["2"]);
  assert.equal(new Set(groupOptions(reserved).map(group => group.key)).size, 4);
});

test("selection survives fresh snapshots and renames, but a vanished group stays forgotten", () => {
  let selected = groupKey("2");
  selected = resolveGroupSelection(selected, groupOptions(structuredClone(nodes)));
  assert.equal(selected, groupKey("2"));
  selected = resolveGroupSelection(selected, groupOptions([node("1", "HK", [group("2", "new name")])]));
  assert.equal(selected, groupKey("2"));
  selected = resolveGroupSelection(selected, groupOptions([nodes[2]]));
  assert.equal(selected, ALL_GROUPS);
  selected = resolveGroupSelection(selected, groupOptions(nodes));
  assert.equal(selected, ALL_GROUPS);
  assert.equal(resolveGroupSelection("invalid stored value", groupOptions(nodes)), ALL_GROUPS);
  assert.equal(resolveGroupSelection(groupKey("2"), groupOptions([])), ALL_GROUPS);
  assert.equal(resolveGroupSelection(UNGROUPED, groupOptions([])), UNGROUPED);
});

test("old snapshots without groups remain ungrouped and names remain plain display text", () => {
  const legacy = {id: "1", name: "legacy", session: "waiting", freshness: "waiting"};
  assert.equal(snapshot({generated_at: now, nodes: [legacy]}).nodes[0], legacy);
  assert.deepEqual(nodeGroups(legacy), []);
  assert.equal(groupText(legacy), "未分组");
  assert.deepEqual(select([legacy], "", UNGROUPED), [legacy]);
  assert.equal(groupText(nodes[0]), "亚洲 · 欧洲");
  const text = `<img src=x onerror="alert(1)">`;
  const named = node("1", "node", [group("1", text)]);
  assert.equal(groupText(named), text);
  assert.equal(groupOptions([named]).at(-1).name, text);
});

test("malformed group payloads are rejected instead of corrupting group identity", () => {
  for (const groups of [null, {}, [null], [group("0", "bad")], [group("01", "bad")], [group("9223372036854775808", "bad")], [group("1", " ")], [group("1", 1)], [asia, asia]]) {
    assert.throws(() => snapshot({generated_at: now, nodes: [node("1", "node", groups)]}));
  }
  assert.equal(snapshot({generated_at: now, nodes}).nodes.length, 3);
});
