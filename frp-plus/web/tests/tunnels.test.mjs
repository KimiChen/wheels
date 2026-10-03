import test from "node:test";
import assert from "node:assert/strict";
import {readFileSync} from "node:fs";
import {tunnelFilters,readTunnelList,readTunnelInstances,readTunnelEvents,readTunnelHistory,createTunnelController,tunnelChartRows} from "../src/admin-tunnels-data.mjs";
import {chart} from "../src/history-data.mjs";

const fixture=JSON.parse(readFileSync(new URL("../../tests/fixtures/tunnels/api.json",import.meta.url),"utf8"));
const copy=value=>structuredClone(value);
const deferred=()=>{let resolve;const promise=new Promise(done=>resolve=done);return{promise,resolve};};
const first=fixture.list.items[0],instance=fixture.instances.items[0];
const turn=()=>new Promise(resolve=>setImmediate(resolve));
function response(path){if(path.includes("/instances"))return copy(fixture.instances);if(path.includes("/events"))return copy(fixture.events);if(path.includes("/history"))return copy(fixture.history);return copy(fixture.list);}

test("tunnel list keeps unbound identity and exact decimal IDs without exposing unknown fields",()=>{
  const raw=copy(fixture.list);raw.items[0].token="must-strip";raw.items[0].current_instances[0].raw_config="must-strip";
  const data=readTunnelList(raw);assert(!JSON.stringify(data).includes("must-strip"));
  const unbound=copy(raw.items[0]);unbound.tunnel_id="9007199254740993";unbound.binding_epoch="0";unbound.node_id=null;unbound.identity_quality="instance_only";unbound.raw_client_id=null;unbound.current_instances=[];
  assert.equal(readTunnelList({...raw,items:[unbound]}).items[0].tunnel_id,"9007199254740993");
  for(const patch of [{tunnel_id:7},{binding_epoch:0},{raw_client_id:"x\ny"},{current_instances:[{...instance,tunnel_id:"999"}]}])assert.throws(()=>readTunnelList({...raw,items:[{...raw.items[0],...patch}]}));
  assert.deepEqual(tunnelFilters({node_id:"9007199254740993",kind:"visitor"}),{node_id:"9007199254740993",kind:"visitor"});
  for(const bad of [{node_id:"01"},{node_id:"9223372036854775808"},{kind:"http"},{where:"raw"}])assert.throws(()=>tunnelFilters(bad));
});
test("instance counters retain uint64 precision and keep unsupported byte and connection qualities independent",()=>{
  const raw=copy(fixture.instances);raw.items[0].counters.rx_bytes="18446744073709551615";
  assert.equal(readTunnelInstances(raw,first.tunnel_id).items[0].counters.rx_bytes,"18446744073709551615");
  raw.items[0].protocol="sudp";Object.assign(raw.items[0].counters,{quality:"unsupported",rx_bytes:null,tx_bytes:null,accounting:"not_observed",byte_scope:"not_observed"});Object.assign(raw.items[0],{accounting:"not_observed",byte_scope:"not_observed"});
  assert.equal(readTunnelInstances(raw,first.tunnel_id).items[0].counters.connections,"1");
  raw.items[0].counters.rx_bytes="0";assert.throws(()=>readTunnelInstances(raw,first.tunnel_id));
  assert.throws(()=>readTunnelInstances({...fixture.instances,items:[instance,instance]},first.tunnel_id));
});
test("events preserve unknown time and declared operation links without inferring event causation",()=>{
  const raw=copy(fixture.events);raw.operation_links[0].changes[0].before="secret";
  const result=readTunnelEvents(raw,instance.instance_id);assert.equal(result.items[0].event_id,"9007199254740993");assert.equal(result.items[1].occurred_at_ms,null);assert.equal(result.items[0].operation_relation,"none");assert.equal(result.operation_links[0].relation,"declared_change");assert(!JSON.stringify(result).includes("secret"));
  raw.items[0].instance_id="999";assert.throws(()=>readTunnelEvents(raw,instance.instance_id));
});
test("history keeps missing buckets and gap bytes separate and rejects wrong generations or invalid numbers",()=>{
  const raw=copy(fixture.history),result=readTunnelHistory(raw,instance.instance_id,instance.generation);
  assert.equal(result.points[1].rx_recorded_bytes,null);assert.equal(result.gap_intervals[0].rx_recorded_bytes,"128");
  assert.throws(()=>readTunnelHistory(raw,instance.instance_id,"99"));
  for(const patch of [{connections:Infinity},{coverage_seconds:61},{rx_recorded_bytes:4096}]){const bad=copy(raw);Object.assign(bad.points[0],patch);assert.throws(()=>readTunnelHistory(bad,instance.instance_id,instance.generation));}
  const bad=copy(raw);bad.points[1].rx_estimated_bytes_per_second=0;assert.throws(()=>readTunnelHistory(bad,instance.instance_id,instance.generation));
  const duplicate=copy(raw);duplicate.points.push(duplicate.points[1]);assert.throws(()=>readTunnelHistory(duplicate,instance.instance_id,instance.generation));
});
test("trend does not connect across an explicitly missing bucket or apportion gap bytes",()=>{
  const h=copy(fixture.history);h.points.push({...h.points[0],at_ms:h.points[1].at_ms+60000});h.generated_at_ms=h.points.at(-1).at_ms;
  const result=chart(h.points.map(p=>({...p,at:new Date(p.at_ms).toISOString()})),[{name:"RX",read:p=>p.rx_estimated_bytes_per_second,format:String}],{window:"1h",generatedAt:new Date(h.generated_at_ms).toISOString(),step:60});
  assert.equal((result.paths[0].path.match(/M/g)||[]).length,2);assert(!result.paths[0].path.includes("L"));
});
test("connection-only history remains visible and approximate sums may exceed one uint64",()=>{
  const h=copy(fixture.history);h.points[0].rx_recorded_bytes="18446744073709551616";
  assert.equal(readTunnelHistory(h,instance.instance_id,instance.generation).points[0].rx_recorded_bytes,"18446744073709551616");
  Object.assign(h.points[0],{samples:0,quality:"unsupported",connections_quality:"ok",connections:1});
  const rows=tunnelChartRows(h,[{read:p=>p.connections}]);assert.equal(rows[0].samples,1);assert.equal(h.points[0].samples,0);assert.equal(rows[1].samples,0);
});
test("controller queries an explicit instance, changes only trend window, and clears every private result",async()=>{
  const calls=[],c=createTunnelController({request:async path=>{calls.push(path);return response(path);}});
  await c.search({node_id:"1"});await c.select(first.tunnel_id);
  assert.equal(c.state.instanceID,instance.instance_id);assert(c.state.events);assert(c.state.history);assert.equal(c.state.detailPending,false);
  const eventCalls=calls.filter(p=>p.includes("/events")).length;await c.setWindow("7d");assert(calls.at(-1).includes("window=7d"));assert.equal(calls.filter(p=>p.includes("/events")).length,eventCalls);
  c.clear();for(const key of ["list","selected","events","history","instanceID"])assert.equal(c.state[key],null);assert.equal(c.state.instances.length,0);
});
test("late list, events and history cannot restore data after logout or another instance selection",async()=>{
  const hold=deferred(),c=createTunnelController({request:()=>hold.promise});const search=c.search({});c.clear();hold.resolve(copy(fixture.list));await search;assert.equal(c.state.list,null);
  const histories=[];const d=createTunnelController({request:async path=>{if(path.includes("/history")){const value=deferred();histories.push(value);return value.promise;}return response(path);}});
  await d.search({});const selected=d.select(first.tunnel_id);await turn();assert.equal(histories.length,1);d.clear();histories[0].resolve(copy(fixture.history));await selected;assert.equal(d.state.events,null);assert.equal(d.state.history,null);
});
test("partial request failure preserves independently validated events and does not fabricate a trend",async()=>{
  const c=createTunnelController({request:async path=>{if(path.includes("/history"))throw {status:503};return response(path);}});
  await c.search({});await c.select(first.tunnel_id);assert(c.state.events);assert.equal(c.state.history,null);assert.match(c.state.message,/失败/);assert.equal(c.state.detailPending,false);
});
test("list pagination caches validated pages and clears selection when changing pages",async()=>{
  let calls=0;const next=copy(fixture.list);next.items=[];const firstPage={...copy(fixture.list),next_cursor:"next"};
  const c=createTunnelController({request:async path=>{calls++;return path.includes("cursor=next")?next:firstPage;}});
  await c.search({});await c.next();assert.equal(c.state.list.items.length,0);c.previous();assert.equal(c.state.list.items.length,firstPage.items.length);await c.next();assert.equal(calls,2);
});
