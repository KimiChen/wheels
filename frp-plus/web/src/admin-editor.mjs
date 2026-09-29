import {groupDocument} from "./admin-data.mjs";

export function membershipDraft(nodeID, groups) {
  const base = groupDocument({groups}).groups.map(group => ({...group, node_ids: [...group.node_ids]}));
  return {nodeID, base, selected: base.filter(group => group.node_ids.includes(nodeID)).map(group => group.id), savedIDs: [], blocked: false};
}
export function membershipChanges(draft) {
  if (!draft) return [];
  const selected = new Set(draft.selected);
  return draft.base.filter(group => group.node_ids.includes(draft.nodeID) !== selected.has(group.id)).map(group => ({
    id: group.id, member: selected.has(group.id),
    body: {name: group.name, node_ids: selected.has(group.id) ? [...group.node_ids, draft.nodeID] : group.node_ids.filter(id => id !== draft.nodeID), config_revision: group.config_revision}
  }));
}
function acceptGroup(draft, group, change) {
  const saved = groupDocument({groups: [group]}).groups[0];
  if (saved.id !== change.id || saved.node_ids.includes(draft.nodeID) !== change.member) throw new Error("invalid_group_save");
  draft.base = draft.base.map(item => item.id === saved.id ? {...saved, node_ids: [...saved.node_ids]} : item);
  if (!draft.savedIDs.includes(saved.id)) draft.savedIDs.push(saved.id);
  return saved;
}
const uncertain = error => error?.name === "AbortError" || !error?.status || error.status >= 500;
function saveError(stage, cause, draft, needsReload = false) {
  const count = draft?.savedIDs.length ?? 0;
  const prefix = count ? `已保存 ${count} 个分组；` : "";
  const detail = stage === "groups" ? "其余分组与节点设置尚未保存。" : "节点设置尚未确认保存。";
  const next = needsReload ? "配置已变更或保存结果待确认，请重新读取后再编辑。" : "编辑内容已保留，可以重试。";
  return Object.assign(new Error(`${prefix}${detail}${next}`, {cause}), {userMessage: `${prefix}${detail}${next}`, stage, needsReload, status: cause?.status});
}

// Group membership has per-group revisions, so acknowledged writes are retained
// when a later operation fails. Never rebase a full member list onto a new revision.
export async function saveNodeChanges({nodeID, membership, settings, binding = null, request, onGroupSaved = () => {}, onSettingsSaved = () => {}, onBindingSaved = () => {}, settingsAlreadySaved = false}) {
  if (membership?.blocked) throw saveError("groups", {status: 409}, membership, true);
  for (const change of membershipChanges(membership)) {
    let saved;
    try {
      saved = acceptGroup(membership, await request(`/api/admin/v1/groups/${change.id}`, {method: "PATCH", body: change.body}), change);
    } catch (error) {
      if (error.status === 409 || error.status === 404) { membership.blocked = true; throw saveError("groups", error, membership, true); }
      if (!uncertain(error)) throw saveError("groups", error, membership);
      let current;
      try { current = groupDocument(await request("/api/admin/v1/groups")).groups.find(group => group.id === change.id); }
      catch { throw saveError("groups", error, membership); }
      if (current?.node_ids.includes(nodeID) === change.member) saved = acceptGroup(membership, current, change);
      else if (!current || current.config_revision !== change.body.config_revision) {
        membership.blocked = true; throw saveError("groups", {status: 409}, membership, true);
      } else throw saveError("groups", error, membership);
    }
    onGroupSaved(saved);
  }
  let saved = null;
  if (settings) {
    try {
      saved = await request(`/api/admin/v1/nodes/${nodeID}/settings`, {method: "PATCH", body: settings});
      if (!saved || !Number.isSafeInteger(saved.config_revision) || saved.config_revision <= settings.config_revision || typeof saved.name !== "string") throw new Error("invalid_settings_save");
    } catch (error) {
      throw saveError("settings", error, membership, uncertain(error) || error.status === 409 || error.status === 404);
    }
    onSettingsSaved(saved);
  }
  if (binding) {
    try { await request(`/api/admin/v1/nodes/${nodeID}/binding`, {method: "PUT", body: binding}); }
    catch (error) {
      const message = `${membership?.savedIDs.length ? `已保存 ${membership.savedIDs.length} 个分组；` : ""}${saved || settingsAlreadySaved ? "节点设置已保存；" : ""}FRP 绑定尚未确认保存。编辑内容已保留，请重试或重新读取。`;
      throw Object.assign(new Error(message, {cause: error}), {userMessage: message, stage: "binding", status: error.status, needsReload: error.status === 409});
    }
    onBindingSaved(binding);
  }
  return saved;
}

// A successful settings response is authoritative even if the following read
// still returns the previous snapshot. Clear this cache once that read catches up.
export function retainAcknowledgedSettings(snapshot, acknowledged) {
  for (const node of snapshot.nodes) {
    const saved = acknowledged.get(node.id);
    if (!saved) continue;
    if (node.settings?.config_revision >= saved.config_revision) acknowledged.delete(node.id);
    else { node.settings = saved; node.name = saved.name; }
  }
  const present = new Set(snapshot.nodes.map(node => node.id));
  if (snapshot.credentials_state !== "degraded") for (const id of acknowledged.keys()) if (!present.has(id)) acknowledged.delete(id);
  return snapshot;
}
