import {validNodeID} from "./node-data.mjs";
import {operationLabels} from "./admin-configuration-data.mjs";
import {readAuditDetail} from "./admin-audit-data.mjs";

export const tunnelWindows={"1h":"最近 1 小时","6h":"最近 6 小时","24h":"最近 24 小时","7d":"最近 7 天"};
export const storageLabels={ready:"可用",disabled:"未启用",degraded:"降级",capacity_limited:"容量受限"};
export const qualityLabels={ok:"有效",partial:"部分覆盖",unknown:"未知",unsupported:"不支持观察"};
export const stateLabels={starting:"启动中",registered:"已登记",running:"运行中",error:"异常",closed:"已关闭",unknown:"未知",configured:"已配置",listening:"本地监听",connecting:"连接中",connected:"已连接",failed:"失败",active:"正在使用"};
export const eventLabels={created:"创建实例",registered:"代理登记",state_changed:"状态变化",closed:"实例关闭",settled:"关闭后计数结清",counter_invalid:"计数异常",collector_gap:"采集缺口",event_gap:"事件缺口",source_overflow:"源事件溢出",source_truncated:"源列表截断",source_unavailable:"源暂不可用"};
export const accountingLabels={connection_close:"连接关闭时记账",datagram:"按数据报记账",not_observed:"未观察字节量"};
export const scopeLabels={forwarded_stream:"转发流字节",datagram_payload:"数据报载荷",not_observed:"无统一可观察口径"};
const invalid=()=>{throw new Error("invalid_tunnel_response");};
const num=v=>Number.isSafeInteger(v)&&v>=0;
const finite=v=>typeof v==="number"&&Number.isFinite(v)&&v>=0;
const text=(v,max=256)=>typeof v==="string"&&new TextEncoder().encode(v).length<=max&&!/[\p{Cc}\p{Cf}]/u.test(v);
const id=v=>validNodeID(v);
const decimal=v=>typeof v==="string"&&/^(0|[1-9][0-9]{0,19})$/.test(v)&&BigInt(v)<=18446744073709551615n;
const historyDecimal=v=>typeof v==="string"&&/^(0|[1-9][0-9]{0,63})$/.test(v);
const uuid=v=>typeof v==="string"&&/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(v);
const enumValue=(value,values)=>{if(!values.includes(value))invalid();return value;};
const checkedID=value=>{if(!id(value))invalid();return value;};
const maybeID=value=>value==null?null:checkedID(value);
const bindingID=value=>{if(value!=="0"&&!id(value))invalid();return value;};
const time=value=>{if(!num(value)||value>253402300799999)invalid();return value;};
const safeText=(value,max=256)=>{if(!text(value,max))invalid();return value;};
function storage(raw,events=false){if(!raw||!Object.hasOwn(storageLabels,raw.state)||!num(raw.retention_days)||raw.retention_days>3650||events&&(!num(raw.max_events)||raw.max_events>1000000))invalid();return{state:raw.state,retention_days:raw.retention_days,...(events?{max_events:raw.max_events}:{})};}
function base(raw){if(!raw||!text(raw.code,64)||!/^[a-z_]+$/.test(raw.code))invalid();return{code:raw.code,generated_at_ms:time(raw.generated_at_ms),event_storage:storage(raw.event_storage,true),metric_storage:storage(raw.metric_storage)};}
function list(raw,max){if(!Array.isArray(raw.items)||raw.items.length>max||!text(raw.next_cursor,2048)||typeof raw.events_pruned!=="boolean")invalid();return{...base(raw),next_cursor:raw.next_cursor,retained_from_ms:time(raw.retained_from_ms),events_pruned:raw.events_pruned};}
function state(raw){if(!raw)invalid();const out={};for(const key of ["status","local_state","remote_state","p2p_state","fallback_state"])out[key]=enumValue(raw[key],Object.keys(stateLabels));out.error_code=safeText(raw.error_code,64);if(out.error_code&&!/^[a-z_]+$/.test(out.error_code))invalid();return out;}
function counters(raw){
  if(!raw||!text(raw.epoch,36)||raw.epoch&&!uuid(raw.epoch))invalid();
  const out={quality:enumValue(raw.quality,["ok","unknown","unsupported"]),connections_quality:enumValue(raw.connections_quality,["ok","unknown","unsupported"]),epoch:raw.epoch,accounting:enumValue(raw.accounting,Object.keys(accountingLabels)),byte_scope:enumValue(raw.byte_scope,Object.keys(scopeLabels))};
  for(const key of ["connections","rx_bytes","tx_bytes"]){const known=(key==="connections"?out.connections_quality:out.quality)==="ok";if(known?!decimal(raw[key]):raw[key]!==null)invalid();out[key]=raw[key];}return out;
}
export function readTunnelInstance(raw,tunnelID){
  if(!raw||raw.tunnel_id!==tunnelID||!uuid(raw.native_instance_id)||!uuid(raw.process_epoch)||!text(raw.service_id,36)||raw.service_id&&!uuid(raw.service_id))invalid();
  const c=counters(raw.counters);if(raw.accounting!==c.accounting||raw.byte_scope!==c.byte_scope)invalid();
  return{instance_id:checkedID(raw.instance_id),tunnel_id:checkedID(raw.tunnel_id),native_instance_id:raw.native_instance_id,source:enumValue(raw.source,["client","server"]),process_epoch:raw.process_epoch,generation:checkedID(raw.generation),protocol:enumValue(raw.protocol,["tcp","udp","http","https","tcpmux","stcp","sudp","xtcp"]),state:state(raw.state),counters:c,accounting:c.accounting,byte_scope:c.byte_scope,service_id:raw.service_id,created_at_ms:time(raw.created_at_ms),closed_at_ms:raw.closed_at_ms==null?null:time(raw.closed_at_ms),last_observed_at_ms:time(raw.last_observed_at_ms),freshness:enumValue(raw.freshness,["fresh","stale"])};
}
function unique(items,key){const seen=new Set();for(const item of items){if(seen.has(item[key]))invalid();seen.add(item[key]);}return items;}
export function tunnelFilters(raw={}){const out={};for(const [key,value]of Object.entries(raw)){if(value===""||value==null)continue;if(key==="node_id")out[key]=checkedID(value);else if(key==="kind")out[key]=enumValue(value,["proxy","visitor"]);else invalid();}return out;}
export function readTunnelList(raw){
  const out=list(raw,100);out.items=unique(raw.items.map(t=>{if(!t||!Array.isArray(t.current_instances)||t.current_instances.length>512||t.raw_client_id!==null&&!text(t.raw_client_id,128))invalid();const tunnelID=checkedID(t.tunnel_id);return{tunnel_id:tunnelID,node_id:maybeID(t.node_id),binding_epoch:bindingID(t.binding_epoch),server_id:safeText(t.server_id,128),user:safeText(t.user,128),raw_client_id:t.raw_client_id,kind:enumValue(t.kind,["proxy","visitor"]),raw_name:safeText(t.raw_name),identity_quality:enumValue(t.identity_quality,["stable","instance_only","conflict"]),current_instances:unique(t.current_instances.map(i=>readTunnelInstance(i,tunnelID)),"instance_id"),retained_from_ms:time(t.retained_from_ms)};}),"tunnel_id");return out;
}
export function readTunnelInstances(raw,tunnelID){return{...list(raw,100),items:unique(raw.items.map(i=>readTunnelInstance(i,tunnelID)),"instance_id")};}
function operationLink(raw){
  if(!raw||!uuid(raw.operation_id)||!Object.hasOwn(operationLabels,raw.state)||raw.relation!=="declared_change"||!Array.isArray(raw.changes)||raw.changes.length>1024)invalid();
  // Reuse the audit object's strict field-name projection, without accepting
  // arbitrary native before/after values as operation-link metadata.
  const item={audit_id:"1",recorded_at_ms:1,occurred_at_ms:null,kind:"state",actor_kind:"unknown",actor:"",observer:"",node_id:"",service_id:"",operation_id:raw.operation_id,operation_version:null,state:raw.state,code:"declared_change",object_count:raw.changes.length,field_count:0,preview_summary:null};
  const {changes}=readAuditDetail({item,changes:raw.changes},"1");
  return{operation_id:raw.operation_id,state:raw.state,relation:"declared_change",created_at_ms:time(raw.created_at_ms),changes};
}
export function readTunnelEvents(raw,instanceID){
  const out=list(raw,200);if(!Array.isArray(raw.operation_links)||raw.operation_links.length>100)invalid();
  out.items=unique(raw.items.map(e=>{if(!e||e.instance_id!==null&&e.instance_id!==instanceID||!text(e.operation_id,36)||e.operation_id&&!uuid(e.operation_id))invalid();return{event_id:checkedID(e.event_id),instance_id:maybeID(e.instance_id),generation:maybeID(e.generation),source:enumValue(e.source,["server","client"]),code:enumValue(e.code,Object.keys(eventLabels)),state:e.state==null?null:state(e.state),time_basis:enumValue(e.time_basis,["native","observed"]),occurred_at_ms:e.occurred_at_ms==null?null:time(e.occurred_at_ms),received_at_ms:time(e.received_at_ms),operation_id:e.operation_id,operation_relation:enumValue(e.operation_relation,["none","declared_change"])};}),"event_id");
  out.operation_links=unique(raw.operation_links.map(operationLink),"operation_id");return out;
}
export function readTunnelHistory(raw,instanceID,generation){
  const out=base(raw);if(raw.instance_id!==instanceID||raw.generation!==generation||!num(raw.step_seconds)||raw.step_seconds<1||raw.step_seconds>86400||raw.precision!=="approximate"||!Array.isArray(raw.points)||raw.points.length>500||!Array.isArray(raw.gap_intervals)||raw.gap_intervals.length>500)invalid();
  Object.assign(out,{instance_id:instanceID,generation,step_seconds:raw.step_seconds,accounting:enumValue(raw.accounting,Object.keys(accountingLabels)),byte_scope:enumValue(raw.byte_scope,Object.keys(scopeLabels)),precision:"approximate"});
  let previous=-1;
  out.points=raw.points.map(p=>{if(!p)invalid();const at=time(p.at_ms);if(at<=previous||!num(p.samples)||!finite(p.coverage_seconds)||p.coverage_seconds>raw.step_seconds)invalid();previous=at;const row={at_ms:at,quality:enumValue(p.quality,Object.keys(qualityLabels)),connections_quality:enumValue(p.connections_quality,Object.keys(qualityLabels)),samples:p.samples,coverage_seconds:p.coverage_seconds};
    for(const key of ["connections","rx_estimated_bytes_per_second","tx_estimated_bytes_per_second"]){if(p[key]!==null&&!finite(p[key]))invalid();row[key]=p[key];}
    for(const key of ["rx_recorded_bytes","tx_recorded_bytes"]){if(p[key]!==null&&!historyDecimal(p[key]))invalid();row[key]=p[key];}
    if(["unknown","unsupported"].includes(row.connections_quality)&&row.connections!==null)invalid();if(["unknown","unsupported"].includes(row.quality)&&[row.rx_recorded_bytes,row.tx_recorded_bytes,row.rx_estimated_bytes_per_second,row.tx_estimated_bytes_per_second].some(v=>v!==null))invalid();return row;});
  out.gap_intervals=raw.gap_intervals.map(g=>{if(!g||!num(g.from_ms)||!num(g.to_ms)||g.from_ms>=g.to_ms)invalid();for(const key of ["rx_recorded_bytes","tx_recorded_bytes"])if(g[key]!==null&&!historyDecimal(g[key]))invalid();return{from_ms:time(g.from_ms),to_ms:time(g.to_ms),reason:enumValue(g.reason,["collection_gap","counter_reset","collector_gap","invalid_sample"]),quality:enumValue(g.quality,Object.keys(qualityLabels)),rx_recorded_bytes:g.rx_recorded_bytes,tx_recorded_bytes:g.tx_recorded_bytes};});return out;
}
// The shared plot's samples field is only its draw/no-draw gate. Byte sample
// counts cannot hide a valid connection mean (for example SUDP). The actual
// sample/coverage numbers remain unchanged in the response and detail table.
export function tunnelChartRows(history,series){return history.points.map(p=>({...p,at:new Date(p.at_ms).toISOString(),samples:series.some(s=>s.read(p)!==null)?1:0}));}

export function createTunnelController({request,onChange=()=>{}}){
  let epoch=0,selection=0,historyEpoch=0;
  const initial=()=>({filters:{},list:null,pages:[],pageIndex:0,loaded:false,pending:false,detailPending:false,historyPending:false,message:"",selected:null,instances:[],instanceCursor:"",instanceID:null,events:null,history:null,window:"1h"});
  const state=initial();const notify=()=>onChange();
  const fail=e=>e?.status===404?"所选历史已不可用，请重新查询。":e?.message==="invalid_tunnel_response"?"隧道历史响应不完整，已停止显示。":"隧道历史请求失败，请重新查询。";
  const path=()=>`/api/admin/v1/tunnels/${state.selected.tunnel_id}`;
  const controller={state,clear(){epoch++;selection++;historyEpoch++;Object.assign(state,initial());notify();},
    async search(filters={}){let clean;try{clean=tunnelFilters(filters);}catch{state.message="节点或对象类型筛选无效。";notify();return;}controller.clear();state.filters=clean;state.pending=true;notify();const current=epoch;try{const data=readTunnelList(await request(`/api/admin/v1/tunnels?${new URLSearchParams({...clean,limit:"50"})}`));if(current!==epoch)return;state.pages=[data];state.list=data;state.loaded=true;}catch(e){if(current===epoch)state.message=fail(e);}finally{if(current===epoch){state.pending=false;notify();}}},
    async next(){if(state.pending||!state.list?.next_cursor)return;selection++;historyEpoch++;Object.assign(state,{selected:null,instances:[],instanceID:null,events:null,history:null,detailPending:false,historyPending:false});if(state.pages[state.pageIndex+1]){state.list=state.pages[++state.pageIndex];notify();return;}const current=epoch;state.pending=true;notify();try{const data=readTunnelList(await request(`/api/admin/v1/tunnels?${new URLSearchParams({...state.filters,limit:"50",cursor:state.list.next_cursor})}`));if(current!==epoch)return;state.pages.push(data);if(state.pages.length>10)state.pages.shift();state.pageIndex=state.pages.length-1;state.list=data;}catch(e){if(current===epoch)state.message=fail(e);}finally{if(current===epoch){state.pending=false;notify();}}},
    previous(){if(state.pending||state.pageIndex===0)return;selection++;historyEpoch++;Object.assign(state,{selected:null,instances:[],instanceID:null,events:null,history:null,detailPending:false,historyPending:false});state.list=state.pages[--state.pageIndex];notify();},
    async select(tunnelID){const selected=state.list?.items.find(t=>t.tunnel_id===tunnelID);if(!selected)return;const current=++selection;historyEpoch++;Object.assign(state,{selected,instances:[],instanceCursor:"",instanceID:null,events:null,history:null,detailPending:true,historyPending:false,message:""});notify();try{const data=readTunnelInstances(await request(`${path()}/instances?limit=100`),tunnelID);if(current!==selection)return;state.instances=data.items;state.instanceCursor=data.next_cursor;state.detailPending=false;if(data.items.length)await controller.selectInstance(data.items[0].instance_id);}catch(e){if(current===selection)state.message=fail(e);}finally{if(current===selection){state.detailPending=false;notify();}}},
    async moreInstances(){if(state.detailPending||!state.instanceCursor)return;const current=selection,tunnelID=state.selected.tunnel_id;state.detailPending=true;notify();try{const data=readTunnelInstances(await request(`${path()}/instances?${new URLSearchParams({limit:"100",cursor:state.instanceCursor})}`),tunnelID);if(current!==selection)return;const combined=unique([...state.instances,...data.items],"instance_id");if(combined.length>512){state.message="实例列表达到本页上限，请缩小对象范围后查询。";state.instanceCursor="";}else{state.instances=combined;state.instanceCursor=data.next_cursor;}}catch(e){if(current===selection)state.message=fail(e);}finally{if(current===selection){state.detailPending=false;notify();}}},
    async selectInstance(instanceID){if(!state.instances.some(i=>i.instance_id===instanceID))return;const current=++selection;historyEpoch++;Object.assign(state,{instanceID,events:null,history:null,detailPending:true,historyPending:false,message:""});notify();await Promise.all([controller.loadHistory(),(async()=>{try{const data=readTunnelEvents(await request(`${path()}/events?${new URLSearchParams({instance_id:instanceID,limit:"100"})}`),instanceID);if(current===selection)state.events=data;}catch(e){if(current===selection)state.message=fail(e);}})()]);if(current===selection){state.detailPending=false;notify();}},
    async loadHistory(){const instance=state.instances.find(i=>i.instance_id===state.instanceID);if(!instance)return;const current=++historyEpoch,selected=selection;state.historyPending=true;state.history=null;notify();try{const data=readTunnelHistory(await request(`${path()}/history?${new URLSearchParams({instance_id:instance.instance_id,window:state.window})}`),instance.instance_id,instance.generation);if(current===historyEpoch&&selected===selection)state.history=data;}catch(e){if(current===historyEpoch&&selected===selection)state.message=fail(e);}finally{if(current===historyEpoch&&selected===selection){state.historyPending=false;notify();}}},
    async setWindow(window){if(!Object.hasOwn(tunnelWindows,window))return;state.window=window;await controller.loadHistory();},
    async moreEvents(){if(state.detailPending||!state.events?.next_cursor)return;const current=selection,instanceID=state.instanceID;state.detailPending=true;notify();try{const data=readTunnelEvents(await request(`${path()}/events?${new URLSearchParams({instance_id:instanceID,limit:"100",cursor:state.events.next_cursor})}`),instanceID);if(current!==selection)return;const items=unique([...state.events.items,...data.items],"event_id");if(items.length>1000){state.message="事件列表已达 1000 条，请重新选择实例查询。";state.events.next_cursor="";}else state.events={...data,items};}catch(e){if(current===selection)state.message=fail(e);}finally{if(current===selection){state.detailPending=false;notify();}}}
  };return controller;
}
