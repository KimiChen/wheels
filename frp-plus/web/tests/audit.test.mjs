import test from "node:test";
import assert from "node:assert/strict";
import {auditFilters,auditQuery,readAuditItem,readAuditPage,readAuditDetail,readAuditExport,createAuditController,maxAuditExportBytes} from "../src/admin-audit-data.mjs";
import {adminClient} from "../src/admin-data.mjs";

const uuid="10000000-0000-4000-8000-000000000001";
const summary={base_revision:"a".repeat(64),context_revision:"b".repeat(64),candidate_digest:"c".repeat(64),filter_digest:"",export_id:"",watermark_id:"",warnings:[],reload_required:true,rows:0,bytes:0};
const item={audit_id:"9223372036854775806",recorded_at_ms:1791000000000,occurred_at_ms:null,kind:"preview",actor_kind:"github",actor:"test-admin",observer:"",node_id:"1",service_id:uuid,operation_id:uuid,operation_version:3,state:"prepared",code:"preview_available",object_count:1,field_count:1,preview_summary:summary};
const filters={from_ms:1790900000000,to_ms:1791100000000};
const retention={audit_days:90,snapshot_days:30,cleanup_enabled:false};
const page=(overrides={})=>({items:[item],next_cursor:"",has_more:false,watermark_id:"9223372036854775807",filters,retention,...overrides});
const changes=[{kind:"proxy",name:"sample",action:"update",fields:["secretKey"]}];
const detail=(overrides={})=>({item,changes,...overrides});
const exported=()=>JSON.stringify({schema:1,kind:"audit_export",export_id:uuid,created_at_ms:1791000000000,watermark_id:"9223372036854775807",filters,retention,secret:"must-strip"})+"\n"+JSON.stringify({schema:1,kind:"audit_entry",...detail(),config:"must-strip"})+"\n";
const deferred=()=>{let resolve,reject;const promise=new Promise((a,b)=>{resolve=a;reject=b;});return{resolve,reject,promise};};

test("audit filters preserve exact IDs, normalize query order and bound dates and private names",()=>{
  assert.deepEqual(auditFilters({node_id:"9223372036854775807",operation_id:uuid,state:"pending",actor:"管理员",from_ms:"1",to_ms:"2"}),{node_id:"9223372036854775807",operation_id:uuid,state:"pending",actor:"管理员",from_ms:1,to_ms:2});
  assert.equal(auditQuery({kind:"state",node_id:"1"}),auditQuery({node_id:"1",kind:"state"}));
  assert.equal(auditFilters({service_id:"primary"}).service_id,"primary");
  assert.throws(()=>auditFilters({actor_kind:"unknown",actor:"someone"}));
  assert.throws(()=>auditFilters({actor:"x".repeat(129)}));
  for(const input of [[],{node_id:"01"},{node_id:"9223372036854775808"},{actor:"x\ny"},{object_name:"字".repeat(86)},{operation_id:"../x"},{state:"active"},{from_ms:2,to_ms:1},{from_ms:0,to_ms:367*86400000},{where:"raw"}])assert.throws(()=>auditFilters(input));
  assert.throws(()=>auditQuery({},"x".repeat(1025)));
});
test("audit projections strip unknown payloads and distinguish unknown changer from observer",()=>{
  const clean=readAuditItem({...item,kind:"external_drift",actor_kind:"unknown",actor:"",observer:"test-admin",preview_summary:{...summary,token:"secret"},raw_config:"secret"});
  assert.equal(clean.actor,"");assert.equal(clean.observer,"test-admin");assert.equal(clean.audit_id,item.audit_id);assert(!JSON.stringify(clean).includes("secret"));
  assert.equal(readAuditItem({...item,kind:"restore",state:"acknowledged"}).state,"acknowledged");
  for(const patch of [{audit_id:9223372036854775806},{code:"secret=https://x"},{preview_summary:{...summary,rows:10001}},{preview_summary:{...summary,warnings:["line\nbreak"]}},{preview_summary:{base_revision:"a".repeat(64)}},{observer:"a\nb"}])assert.throws(()=>readAuditItem({...item,...patch}));
});
test("audit pages reject inconsistent cursors, duplicates, unknown range and watermark drift",()=>{
  assert.equal(readAuditPage(page()).items.length,1);
  for(const input of [page({items:[item,item]}),page({watermark_id:"1"}),page({next_cursor:"next"}),page({has_more:true}),page({filters:null})])assert.throws(()=>readAuditPage(input));
});
test("audit retention capacity remains explicit and an expired cursor requires a new query",async()=>{
  const policy={...retention,cleanup_enabled:true,max_rows:100000,max_bytes:268435456,rows:100002,bytes:270000000,capacity_blocked:true,generation:"9007199254740993",last_gc_at_ms:1791000000000};
  assert.deepEqual(readAuditPage(page({retention:{...policy,private_path:"must-strip"}})).retention,policy);
  for(const bad of [{...policy,generation:9007199254740993},{...policy,rows:-1},{...retention,max_rows:10}])assert.throws(()=>readAuditPage(page({retention:bad})));
  let calls=0;const c=createAuditController({request:async()=>{if(++calls===1)return page({has_more:true,next_cursor:"next",retention:policy});throw {status:409,code:"audit_cursor_expired"};}});
  await c.search({});await c.next();assert.match(c.state.message,/重新查询第一页/);assert.equal(c.state.pageIndex,0);assert.equal(c.state.pending,false);
});
test("detail and JSONL export whitelist fields and never include submitted values",()=>{
  const raw=detail({changes:[{...changes[0],before:"private-value",after:"private-value"}]});
  assert(!JSON.stringify(readAuditDetail(raw,item.audit_id)).includes("private-value"));
  const clean=readAuditExport(exported());assert(!clean.includes("must-strip"));assert.equal(clean.split("\n").length,3);
  for(const fields of [["secretKey=private"],["plugin.secret-option"],["remotePort","remotePort"],Array(65).fill("remotePort")])assert.throws(()=>readAuditDetail(detail({changes:[{...changes[0],fields}]}),item.audit_id));
  assert.throws(()=>readAuditDetail(detail(),"1"));assert.throws(()=>readAuditDetail(detail({changes:[changes[0],changes[0]]}),item.audit_id));
  assert.throws(()=>readAuditExport(exported().trimEnd()));assert.throws(()=>readAuditExport(exported()+exported().split("\n")[1]+"\n"));
  assert.throws(()=>readAuditExport("x".repeat(maxAuditExportBytes+1)));
  assert.throws(()=>readAuditExport("\ufeff"+exported()));
  assert.throws(()=>readAuditExport("\n".repeat(10002)));
  assert.throws(()=>readAuditExport(exported().split("\n")[0]+"\n"+JSON.stringify({ignored:"x".repeat(96*1024)})+"\n"));
});
test("audit controller keeps stable pagination and ignores previous searches, details and exports after clear",async()=>{
  const requests=[],downloads=[],saved=[];
  const c=createAuditController({request:path=>{const d=deferred();requests.push({path,...d});return d.promise;},download:f=>{const d=deferred();downloads.push({filters:f,...d});return d.promise;},onExport:text=>saved.push(text)});
  const old=c.search({node_id:"1"}), current=c.search({node_id:"2"});
  requests[1].resolve(page({filters:{...filters,node_id:"2"},has_more:true,next_cursor:"next"}));await current;requests[0].resolve(page());await old;assert.equal(c.state.filters.node_id,"2");
  const next=c.next();assert(requests[2].path.includes("cursor=next"));assert(requests[2].path.includes("from_ms="));
  requests[2].resolve(page({filters:{node_id:"2",to_ms:filters.to_ms,from_ms:filters.from_ms},items:[{...item,audit_id:"2"}]}));await next;assert.equal(c.state.pageIndex,1);
  c.previous();assert.equal(c.state.pages[0].items[0].audit_id,item.audit_id);
  const selected=c.select(item.audit_id), download=c.export();assert.equal(downloads[0].filters.node_id,"2");c.clear();
  requests[3].resolve(detail());downloads[0].resolve(exported());await Promise.all([selected,download]);assert.equal(c.state.detail,null);assert.equal(c.state.loaded,false);assert.deepEqual(saved,[]);
});
test("pagination rejects a changed snapshot or filter while keeping the last verified page",async()=>{
  for(const change of [{watermark_id:"9223372036854775806"},{filters:{...filters,node_id:"2"}}]){
    let count=0;const c=createAuditController({request:async()=>count++?page(change):page({has_more:true,next_cursor:"next"})});
    await c.search();await c.next();assert.equal(c.state.pages.length,1);assert.match(c.state.message,/响应不完整/);
  }
});
test("export failures are explicit, not automatically retried",async()=>{
  let calls=0;const c=createAuditController({request:async()=>page(),download:async()=>{calls++;throw{status:413,code:"export_limit"};}});
  await c.export();assert.equal(calls,0);await c.search();await c.export();assert.equal(calls,1);assert.match(c.state.message,/缩小筛选/);assert.equal(c.state.exporting,false);
});

async function clientHarness(response) {
  const calls=[];let expired=0;
  const client=adminClient({fetcher:async(path,options)=>{calls.push({path,options});return path.endsWith("/session")?{ok:true,status:200,json:async()=>({csrf_token:"synthetic-session-token",expires_at:"2099-01-01T00:00:00Z"})}:typeof response==="function"?response():response;},onExpired:()=>expired++});
  await client.session();return{client,calls,expired:()=>expired};
}
test("audit download uses authenticated POST and bounded UTF-8 streaming",async()=>{
  const bytes=new TextEncoder().encode(exported());let index=0;
  const body=new ReadableStream({pull(controller){if(index<bytes.length)controller.enqueue(bytes.subarray(index,index+=11));else controller.close();}});
  const h=await clientHarness(new Response(body,{headers:{"content-type":"application/x-ndjson;charset=utf-8"}}));
  assert.equal(await h.client.downloadAudit(filters),exported());const {path,options}=h.calls[1];
  assert.equal(path,"/api/admin/v1/configuration/audit/export");assert.equal(options.method,"POST");assert.equal(options.headers["X-CSRF-Token"],"synthetic-session-token");assert.equal(options.credentials,"same-origin");assert.equal(options.redirect,"error");assert.deepEqual(JSON.parse(options.body),{filters});
});
test("download cancels oversized streams, rejects wrong type and malformed UTF-8",async()=>{
  let cancelled=false;
  const oversized=new ReadableStream({pull(controller){controller.enqueue(new Uint8Array(1024*1024));},cancel(){cancelled=true;}});
  let h=await clientHarness(new Response(oversized,{headers:{"content-type":"application/x-ndjson"}}));await assert.rejects(h.client.downloadAudit({}));assert(cancelled);
  for(const response of [new Response("{}",{headers:{"content-type":"application/json"}}),new Response(new Uint8Array([0xff]),{headers:{"content-type":"application/x-ndjson"}}),new Response("x",{headers:{"content-type":"application/x-ndjson","content-length":String(maxAuditExportBytes+1)}})]){h=await clientHarness(response);await assert.rejects(h.client.downloadAudit({}));}
});
test("expired audit download clears authorization and late streamed data cannot escape logout",async()=>{
  for(const status of [401,403]){const h=await clientHarness(new Response("",{status}));await assert.rejects(h.client.downloadAudit({}),e=>e.status===status);assert.equal(h.expired(),1);await assert.rejects(h.client.downloadAudit({}),e=>e.status===401);assert.equal(h.calls.length,2);}
  let stream;const h=await clientHarness(new Response(new ReadableStream({start(c){stream=c;}}),{headers:{"content-type":"application/x-ndjson"}}));
  const pending=h.client.downloadAudit({});await new Promise(resolve=>setImmediate(resolve));h.client.clear();stream.enqueue(new TextEncoder().encode(exported()));
  await assert.rejects(pending,{name:"AbortError"});assert.equal(h.calls[1].options.signal.aborted,true);
});
