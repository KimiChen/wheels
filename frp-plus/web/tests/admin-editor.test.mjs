import test from "node:test";
import assert from "node:assert/strict";
import {membershipDraft, membershipChanges, saveNodeChanges, retainAcknowledgedSettings} from "../src/admin-editor.mjs";

const groups = () => [{id: "1", name: "生产", node_ids: ["2"], config_revision: 4}, {id: "2", name: "备用", node_ids: ["1", "3"], config_revision: 7}];
const settings = {name: "节点", config_revision: 2, traffic_used_bytes: "0"};
const change = () => { const draft = membershipDraft("1", groups()); draft.selected = ["1"]; return draft; };
const saved = {name: "节点", config_revision: 3};
const status = code => Object.assign(new Error("request failed"), {status: code});
const groupResult = (id, body) => ({id, ...body, config_revision: body.config_revision + 1});

test("node membership updates only changed groups and preserves every other node and original revision", () => {
  const original = groups(), draft = membershipDraft("1", original); draft.selected = ["1"];
  assert.deepEqual(membershipChanges(draft), [
    {id: "1", member: true, body: {name: "生产", node_ids: ["2", "1"], config_revision: 4}},
    {id: "2", member: false, body: {name: "备用", node_ids: ["3"], config_revision: 7}}
  ]);
  assert.deepEqual(original, groups());
});

test("a partial group failure retains acknowledgments, retries only pending groups, then saves settings once", async () => {
  const draft = change(), requests = []; let failing = true;
  const request = async (path, options) => {
    requests.push({path, options});
    if (path === "/api/admin/v1/groups") return {groups: [{...groups()[0], node_ids: ["2", "1"], config_revision: 5}, groups()[1]]};
    if (path.endsWith("groups/2") && failing) throw status(503);
    if (path.endsWith("settings")) return saved;
    return groupResult(path.split("/").at(-1), options.body);
  };
  await assert.rejects(saveNodeChanges({nodeID: "1", membership: draft, settings, request}), error => error.stage === "groups" && /已保存 1 个分组/.test(error.userMessage));
  assert.deepEqual(membershipChanges(draft).map(item => item.id), ["2"]);
  assert.equal(requests.some(item => item.path.endsWith("settings")), false);
  failing = false;
  assert.deepEqual(await saveNodeChanges({nodeID: "1", membership: draft, settings, request}), saved);
  assert.equal(requests.filter(item => item.path.endsWith("groups/1")).length, 1);
  assert.equal(requests.filter(item => item.path.endsWith("settings")).length, 1);
  assert.deepEqual(requests.at(-1).options.body, settings);
});

test("an uncertain committed group response is verified without repeating its write or overwriting other memberships", async () => {
  const draft = change(), writes = [];
  const request = async (path, options) => {
    if (options) writes.push({path, options});
    if (path.endsWith("groups/1")) throw Object.assign(new Error("timeout"), {name: "AbortError"});
    if (path === "/api/admin/v1/groups") return {groups: [{...groups()[0], node_ids: ["2", "1", "4"], config_revision: 6}, groups()[1]]};
    if (path.endsWith("settings")) return saved;
    return groupResult("2", options.body);
  };
  await saveNodeChanges({nodeID: "1", membership: draft, settings, request});
  assert.equal(writes.filter(item => item.path.endsWith("groups/1")).length, 1);
  assert.deepEqual(draft.base[0].node_ids, ["2", "1", "4"]);
  assert.equal(draft.base[0].config_revision, 6);
});

test("revision conflicts stop staged saving and never rebase old member lists onto a fresh revision", async () => {
  for (const direct of [true, false]) {
    const draft = change(), calls = [];
    const request = async (path, options) => {
      calls.push({path, options});
      if (path.endsWith("groups/1")) throw status(direct ? 409 : 503);
      return {groups: [{...groups()[0], node_ids: ["2", "4"], config_revision: 5}, groups()[1]]};
    };
    await assert.rejects(saveNodeChanges({nodeID: "1", membership: draft, settings, request}), error => error.needsReload === true && error.status === 409);
    const count = calls.length;
    await assert.rejects(saveNodeChanges({nodeID: "1", membership: draft, settings, request}));
    assert.equal(calls.length, count);
    assert.equal(draft.base[0].config_revision, 4);
    assert.equal(calls.some(item => item.path.endsWith("settings")), false);
  }
});

test("uncertain settings writes require explicit reload and do not automatically replay calibration", async () => {
  const calls = [];
  await assert.rejects(saveNodeChanges({nodeID: "1", membership: membershipDraft("1", groups()), settings, request: async (path, options) => { calls.push({path, options}); throw status(503); }}), error => error.stage === "settings" && error.needsReload);
  assert.equal(calls.length, 1); assert.equal(calls[0].options.body.traffic_used_bytes, "0");
});

test("binding failure after settings success keeps its retry independent of the acknowledged calibration", async () => {
  const calls = [], binding = {server_id: "server", user: "", raw_client_id: "client"}; let settingsPending = settings, settingsAccepted = false, failing = true;
  const request = async (path, options) => {
    calls.push({path, options});
    if (path.endsWith("settings")) return saved;
    if (failing) throw status(503);
    return null;
  };
  const onSettingsSaved = result => { assert.equal(result.config_revision, 3); settingsPending = null; settingsAccepted = true; };
  await assert.rejects(saveNodeChanges({nodeID: "1", settings: settingsPending, binding, request, onSettingsSaved}), error => error.stage === "binding" && /节点设置已保存/.test(error.userMessage));
  failing = false;
  await saveNodeChanges({nodeID: "1", settings: settingsPending, binding, request, onSettingsSaved, settingsAlreadySaved: settingsAccepted});
  assert.equal(calls.filter(call => call.path.endsWith("settings")).length, 1);
  assert.equal(calls.filter(call => call.path.endsWith("binding")).length, 2);
});

test("acknowledged settings survive stale reads and are released when the snapshot catches up", () => {
  const acknowledged = new Map([["1", saved]]);
  const stale = {nodes: [{id: "1", name: "旧名称", settings: {config_revision: 2}}]};
  retainAcknowledgedSettings(stale, acknowledged);
  assert.equal(stale.nodes[0].settings.config_revision, 3); assert.equal(stale.nodes[0].name, "节点");
  assert.equal(acknowledged.size, 1);
  retainAcknowledgedSettings({nodes: [{id: "1", name: "另一次编辑", settings: {config_revision: 4}}]}, acknowledged);
  assert.equal(acknowledged.size, 0);
});
