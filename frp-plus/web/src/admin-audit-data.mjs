import {operationLabels} from "./admin-configuration-data.mjs";
import {validNodeID} from "./node-data.mjs";

export const auditKinds = {state:"状态变化", preview:"变更预览", dispatch:"命令下发", retry:"再次请求", restore:"恢复接管", external_drift:"外部变更", export:"审计导出"};
export const actorKinds = {github:"GitHub 管理员", system:"系统", unknown:"未知操作者"};
export const auditStateLabels = {...operationLabels,pending:"恢复待接管",acknowledged:"恢复已接管"};
export const maxAuditExportBytes = 8 * 1024 * 1024;
const uuid = value => typeof value === "string" && /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(value);
const number = (value, max = Number.MAX_SAFE_INTEGER) => Number.isSafeInteger(value) && value >= 0 && value <= max;
const text = (value, max = 256) => typeof value === "string" && new TextEncoder().encode(value).length <= max && !/[\p{Cc}\p{Cf}]/u.test(value);
const id = value => typeof value === "string" && /^(0|[1-9][0-9]{0,18})$/.test(value) && BigInt(value) <= 9223372036854775807n;
const digest = value => typeof value === "string" && /^[0-9a-f]{64}$/.test(value);
const auditFields = new Set("name type enabled localIP localPort remotePort customDomains subdomain locations httpUser httpPassword hostHeaderRewrite routeByHTTPUser requestHeaders responseHeaders multiplexer secretKey allowUsers annotations metadatas transport transport.useEncryption transport.useCompression transport.bandwidthLimit transport.bandwidthLimitMode transport.proxyProtocolVersion healthCheck healthCheck.type healthCheck.timeoutSeconds healthCheck.maxFailed healthCheck.intervalSeconds healthCheck.path healthCheck.httpHeaders loadBalancer loadBalancer.group loadBalancer.groupKey plugin plugin.type serverUser serverName bindAddr bindPort protocol keepTunnelOpen maxRetriesAnHour minRetryInterval fallbackTo fallbackTimeoutMs natTraversal".split(" "));
const object = value => value != null && typeof value === "object" && !Array.isArray(value);
const invalid = () => { throw new Error("invalid_audit_response"); };

export function auditFilters(input = {}) {
  if (!object(input)) throw new Error("审计筛选项无效。");
  const result = {};
  for (const [key, value] of Object.entries(input)) {
    if (value === "" || value == null) continue;
    if (["from_ms", "to_ms"].includes(key)) {
      const parsed = typeof value === "string" ? Number(value) : value;
      if (!number(parsed, 253402300799999) || parsed === 0) throw new Error("审计时间无效。");
      result[key] = parsed;
    } else if (["actor_kind", "kind", "state", "object_kind"].includes(key)) {
      const values = key === "actor_kind" ? actorKinds : key === "kind" ? auditKinds : key === "state" ? auditStateLabels : {proxy:1, visitor:1};
      if (!Object.hasOwn(values, value)) throw new Error("审计筛选项无效。");
      result[key] = value;
    } else if (key === "node_id") {
      if (!validNodeID(value)) throw new Error("节点 ID 无效。");
      result[key] = value;
    } else if (key === "operation_id") {
      if (!uuid(value)) throw new Error("服务或操作 ID 无效。");
      result[key] = value;
    } else if (["actor", "object_name", "service_id"].includes(key)) {
      if (!text(value,key === "object_name" ? 256 : 128) || value !== value.trim()) throw new Error("操作者、服务或对象名过长，或包含首尾空白及控制字符。");
      result[key] = value;
    } else throw new Error("审计筛选项无效。");
  }
  if (result.actor_kind === "unknown" && result.actor) throw new Error("未知操作者不能同时指定操作者标识。");
  if (result.from_ms != null && result.to_ms != null && (result.from_ms >= result.to_ms || result.to_ms - result.from_ms > 366 * 86400000)) throw new Error("请选择不超过 366 天的有效时间范围。");
  return result;
}
export function auditQuery(filters, cursor = "") {
  const params = new URLSearchParams(Object.entries(auditFilters(filters)).sort(([a],[b])=>a.localeCompare(b)).map(([key,value]) => [key,String(value)]));
  params.set("limit", "50");
  if (cursor) { if (!text(cursor,1024)) invalid(); params.set("cursor",cursor); }
  return params.toString();
}
export function readAuditItem(raw) {
  if (!raw || !id(raw.audit_id) || raw.audit_id === "0" || !number(raw.recorded_at_ms) || raw.occurred_at_ms != null && !number(raw.occurred_at_ms) || !Object.hasOwn(auditKinds,raw.kind) || !Object.hasOwn(actorKinds,raw.actor_kind) || !text(raw.actor,256) || !text(raw.observer,256) || !text(raw.node_id,32) || raw.node_id && !validNodeID(raw.node_id) || !text(raw.service_id,128) || !text(raw.operation_id,36) || raw.operation_id && !uuid(raw.operation_id) || !text(raw.state,32) || raw.state && !Object.hasOwn(auditStateLabels,raw.state) || !text(raw.code,64) || !/^[a-z_]{1,64}$/.test(raw.code) || !number(raw.object_count,1024) || !number(raw.field_count,65536) || raw.operation_version != null && !number(raw.operation_version)) invalid();
  let preview = null;
  if (raw.preview_summary != null) {
    const source = raw.preview_summary; preview = {};
    for (const key of ["base_revision","context_revision","candidate_digest","filter_digest"]) { if (source[key] !== "" && !digest(source[key])) invalid(); preview[key] = source[key]; }
    if (source.export_id !== "" && !uuid(source.export_id) || source.watermark_id !== "" && !id(source.watermark_id) || !number(source.rows,10000) || !number(source.bytes,maxAuditExportBytes) || typeof source.reload_required !== "boolean") invalid();
    Object.assign(preview,{export_id:source.export_id,watermark_id:source.watermark_id,rows:source.rows,bytes:source.bytes,reload_required:source.reload_required});
    if (!Array.isArray(source.warnings) || source.warnings.length > 64 || source.warnings.some(v => typeof v !== "string" || !/^[a-z_]{1,64}$/.test(v))) invalid();
    preview.warnings = [...source.warnings];
  }
  return {audit_id:raw.audit_id,recorded_at_ms:raw.recorded_at_ms,occurred_at_ms:raw.occurred_at_ms??null,kind:raw.kind,actor_kind:raw.actor_kind,actor:raw.actor,observer:raw.observer,node_id:raw.node_id,service_id:raw.service_id,operation_id:raw.operation_id,operation_version:raw.operation_version??null,state:raw.state,code:raw.code,object_count:raw.object_count,field_count:raw.field_count,preview_summary:preview};
}
function retention(raw) {
  if (!raw || !number(raw.audit_days,3650) || !number(raw.snapshot_days,3650) || typeof raw.cleanup_enabled !== "boolean") invalid();
  return {audit_days:raw.audit_days,snapshot_days:raw.snapshot_days,cleanup_enabled:raw.cleanup_enabled};
}
export function readAuditPage(raw) {
  if (!raw || !raw.filters || !Array.isArray(raw.items) || raw.items.length > 100 || typeof raw.has_more !== "boolean" || !text(raw.next_cursor,1024) || raw.has_more !== Boolean(raw.next_cursor) || !id(raw.watermark_id)) invalid();
  const items = raw.items.map(readAuditItem), seen = new Set();
  for (const item of items) { if (seen.has(item.audit_id) || BigInt(item.audit_id) > BigInt(raw.watermark_id)) invalid(); seen.add(item.audit_id); }
  return {items,next_cursor:raw.next_cursor,has_more:raw.has_more,watermark_id:raw.watermark_id,filters:auditFilters(raw.filters),retention:retention(raw.retention)};
}
export function readAuditDetail(raw, expectedID) {
  const item = readAuditItem(raw?.item);
  if (item.audit_id !== expectedID || !Array.isArray(raw.changes) || raw.changes.length > 1024) invalid();
  const objects = new Set();
  const changes = raw.changes.map(change => {
    if (!change || !["proxy","visitor"].includes(change.kind) || !text(change.name) || !change.name || !["create","update","delete","enable","disable","rename"].includes(change.action) || !Array.isArray(change.fields) || change.fields.length > 64 || change.fields.some(field => !auditFields.has(field)) || new Set(change.fields).size !== change.fields.length || change.clone_from != null && (!text(change.clone_from) || change.clone_from && (change.action !== "create" || change.clone_from === change.name))) invalid();
    const key = `${change.kind}\0${change.name}`; if (objects.has(key)) invalid(); objects.add(key);
    return {kind:change.kind,name:change.name,action:change.action,fields:[...change.fields],...(change.clone_from ? {clone_from:change.clone_from} : {})};
  });
  if (new TextEncoder().encode(JSON.stringify(changes)).length > 65536) invalid();
  return {item,changes};
}
export function readAuditExport(value) {
  if (typeof value !== "string" || new TextEncoder().encode(value).length > maxAuditExportBytes || !value.endsWith("\n")) invalid();
  // Do not split an untrusted 8 MiB body into millions of empty line objects.
  // A detail is bounded by its 64 KiB changes and 8 KiB summary on the server.
  const lines = []; let start = 0;
  while (start < value.length) {
    const end = value.indexOf("\n",start);
    if (end < 0 || end === start || end-start > (lines.length ? 96*1024 : 8192) || lines.length >= 10001) invalid();
    lines.push(value.slice(start,end));start=end+1;
  }
  if (!lines.length) invalid();
  let header;
  try { header = JSON.parse(lines[0]); } catch { invalid(); }
  if (header?.schema !== 1 || header.kind !== "audit_export" || !header.filters || !uuid(header.export_id) || !number(header.created_at_ms) || !id(header.watermark_id)) invalid();
  const output = [{schema:1,kind:"audit_export",export_id:header.export_id,created_at_ms:header.created_at_ms,watermark_id:header.watermark_id,filters:auditFilters(header.filters),retention:retention(header.retention)}];
  const seen = new Set();
  for (const line of lines.slice(1)) {
    let row; try { row = JSON.parse(line); } catch { invalid(); }
    if (row?.schema !== 1 || row.kind !== "audit_entry") invalid();
    const entry = readAuditDetail(row,row.item?.audit_id);
    if (seen.has(entry.item.audit_id) || BigInt(entry.item.audit_id) > BigInt(header.watermark_id)) invalid();
    seen.add(entry.item.audit_id); output.push({schema:1,kind:"audit_entry",...entry});
  }
  return output.map(row=>JSON.stringify(row)).join("\n")+"\n";
}

export function createAuditController({request,download,onChange=()=>{},onExport=()=>{}}) {
  let epoch=0, detailEpoch=0;
  const state={filters:{},pages:[],pageIndex:0,detail:null,selectedID:null,pending:false,detailPending:false,exporting:false,message:"",loaded:false};
  function clear() { epoch++; detailEpoch++; Object.assign(state,{filters:{},pages:[],pageIndex:0,detail:null,selectedID:null,pending:false,detailPending:false,exporting:false,message:"",loaded:false});onChange(); }
  const fail = error => error?.code === "export_limit" ? "导出超过 10000 条或 8 MiB，请缩小筛选范围后再试。" : error?.status === 404 ? "所选记录已不可用，请重新查询。" : error?.message === "invalid_audit_response" ? "审计响应不完整，已停止显示和导出。" : "审计请求失败，请重新查询。";
  return {state,clear,
    async search(input={}) {
      let filters; try { filters=auditFilters(input); } catch(error) {state.message=error.message;onChange();return;}
      const generation=++epoch;detailEpoch++;Object.assign(state,{filters,pages:[],pageIndex:0,detail:null,selectedID:null,pending:true,detailPending:false,exporting:false,message:"",loaded:false});onChange();
      try { const page=readAuditPage(await request(`/api/admin/v1/configuration/audit?${auditQuery(filters)}`));if(generation!==epoch)return;state.filters=page.filters;state.pages=[page];state.loaded=true; }
      catch(error){if(generation===epoch)state.message=fail(error);}
      finally{if(generation===epoch){state.pending=false;onChange();}}
    },
    async next() {
      const page=state.pages[state.pageIndex];if(state.pending||!page?.has_more)return;
      detailEpoch++;state.detail=null;state.selectedID=null;state.detailPending=false;
      if(state.pages[state.pageIndex+1]){state.pageIndex++;onChange();return;}
      const generation=epoch;state.pending=true;state.message="";onChange();
      try {const next=readAuditPage(await request(`/api/admin/v1/configuration/audit?${auditQuery(state.filters,page.next_cursor)}`));if(generation!==epoch)return;if(next.watermark_id!==page.watermark_id || auditQuery(next.filters)!==auditQuery(page.filters))invalid();state.pages.push(next);if(state.pages.length>20)state.pages.shift();state.pageIndex=state.pages.length-1;}
      catch(error){if(generation===epoch)state.message=fail(error);}
      finally{if(generation===epoch){state.pending=false;onChange();}}
    },
    previous(){if(state.pending||state.pageIndex===0)return;detailEpoch++;state.pageIndex--;state.selectedID=null;state.detail=null;state.detailPending=false;onChange();},
    async select(id) {
      if(!state.pages[state.pageIndex]?.items.some(item=>item.audit_id===id))return;
      const generation=epoch,detailGeneration=++detailEpoch;state.selectedID=id;state.detail=null;state.detailPending=true;state.message="";onChange();
      try{const detail=readAuditDetail(await request(`/api/admin/v1/configuration/audit/${id}`),id);if(generation===epoch&&detailGeneration===detailEpoch)state.detail=detail;}
      catch(error){if(generation===epoch&&detailGeneration===detailEpoch)state.message=fail(error);}
      finally{if(generation===epoch&&detailGeneration===detailEpoch){state.detailPending=false;onChange();}}
    },
    async export() {
      if(state.exporting||state.pending||!state.loaded)return;
      const generation=epoch;state.exporting=true;state.message="";onChange();
      try{const result=await download(state.filters);if(generation!==epoch)return;const clean=readAuditExport(result);if(generation===epoch){onExport(clean);state.message="已准备脱敏审计文件；导出操作已由主控记录。";}}
      catch(error){if(generation===epoch)state.message=fail(error);}
      finally{if(generation===epoch){state.exporting=false;onChange();}}
    }
  };
}
