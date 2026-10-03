import test from 'node:test';
import assert from 'node:assert/strict';
import {fieldSpecs, fieldValue, makeEdit, readInventory, proxyTypes, visitorTypes, canApply, resultFacts, resultReason, operationMaterials, canRollback, configErrors, createConfigController, readRestore, canAcknowledgeRestore, restoreAcknowledgement} from '../src/admin-configuration-data.mjs';
const service = '10000000-0000-4000-8000-000000000001', operation = '20000000-0000-4000-8000-000000000001', reference = '30000000-0000-4000-8000-000000000001';
const base = 'a'.repeat(64), context = 'b'.repeat(64), candidate = 'c'.repeat(64);
const object = {kind: 'proxy', name: 'existing', type: 'tcp', source: 'store', writable: true, active: true, fields: [{path: 'localIP', value: '127.0.0.1'}, {path: 'localPort', value: 8080}], secrets: [{path: 'loadBalancer.groupKey', present: true}], read_only_fields: ['healthCheck'], issues: []};
const inventory = () => ({node_id:'1', service_id:service, received_at_ms:100, inventory:{revision:base, context_revision:context, state:'ready', objects:[object], dependencies:[], issues:[]}});
const preview = () => ({base_revision:base, context_revision:context, candidate_digest:candidate, changes:[], warnings:[]});
const result = (state='prepared') => ({code:'ok', operation:{operation_id:operation,node_id:'1',service_id:service,state,version:3,base_revision:base,candidate_digest:candidate,deadline_at_ms:400000}, agent:{context_revision:context}, events:[], agent_received_at_ms:200});

test('all eight Proxy and three Visitor forms have bounded native fields', () => {
  assert.equal(proxyTypes.length, 8); assert.equal(visitorTypes.length, 3);
  for (const kind of ['proxy','visitor']) for (const type of kind === 'proxy' ? proxyTypes : visitorTypes) {
    const fields = fieldSpecs(kind,type); assert.equal(fields.enabled,'bool');
    assert.equal(fields[kind === 'proxy' ? 'localPort' : 'bindPort'], kind === 'proxy' ? 'port' : 'bind_port');
    const edit = makeEdit({mode:'create',kind,type,name:`${kind}-${type}`,inputs:{enabled:'true', ...(kind==='proxy'?{localPort:'8080'}:{serverName:'peer',bindPort:'-1'})}});
    assert.equal(edit.changes[0].type,type); assert.deepEqual(edit.secret_values,[]);
  }
  assert.throws(() => fieldSpecs('visitor','http'));
  assert.throws(() => fieldSpecs('invalid','tcp'));
});
test('type validation does not turn blank into zero and preserves false', () => {
  assert.equal(fieldValue('port',''),null); assert.equal(fieldValue('bool','false'),false); assert.equal(fieldValue('remote_port','0'),0);
  assert.equal(fieldValue('bind_port','-1'),-1); assert.deepEqual(fieldValue('strings','a.example.invalid\nb.example.invalid'),['a.example.invalid','b.example.invalid']);
  for (const [type,value] of [['port','0'],['port','65536'],['bind_port','0'],['port','1.1'],['nonnegative','2147483648'],['bool','yes'],['string','a\nsecret']]) assert.throws(() => fieldValue(type,value));
});
test('edits only patch changed fields, preserve local advanced values and explicit resets', () => {
  const edit = makeEdit({mode:'update',kind:'proxy',type:'tcp',name:'existing',source:object, inputs:{localIP:'127.0.0.1',localPort:'8081'}});
  assert.deepEqual(edit.changes[0].fields,[{path:'localPort',value:8081}]); assert.equal(edit.changes[0].type,'');
  assert.equal(JSON.stringify(edit).includes('healthCheck'),false);
  assert.deepEqual(makeEdit({mode:'update',kind:'proxy',type:'tcp',name:'existing',source:object,inputs:{localIP:''}}).changes[0].fields,[{path:'localIP',value:null}]);
  assert.throws(() => makeEdit({mode:'update',kind:'proxy',type:'tcp',name:'existing',source:object,inputs:{localPort:'8080'}}),/尚未修改/);
});
test('rename and copy instruct Agent to preserve original secrets and advanced fields', () => {
  const edit = makeEdit({mode:'rename',kind:'proxy',type:'tcp',name:'renamed',source:object,inputs:{localPort:'8080'}});
  assert.deepEqual(edit.changes.map(change => [change.operation,change.name,change.clone_from]),[['delete','existing',undefined],['create','renamed','existing']]);
  assert.deepEqual(edit.changes[1].fields,[]); assert.deepEqual(edit.secret_values,[]);
  for (const source of [{...object,source:'file'},{...object,writable:false}]) assert.throws(() => makeEdit({mode:'clone',kind:'proxy',type:'tcp',name:'copy',source}),/原生配置/);
  assert.throws(() => makeEdit({mode:'rename',kind:'proxy',type:'tcp',name:'existing',source:object}),/新的名称/);
});
test('secret replacements use private write-only values and opaque references', () => {
  const edit = makeEdit({mode:'create',kind:'proxy',type:'stcp',name:'secure',secrets:{secretKey:{mode:'replace',value:'example-only-secret'},'loadBalancer.groupKey':{mode:'clear'}}},()=>reference);
  assert.equal(JSON.stringify(edit.changes).includes('example-only-secret'),false);
  assert.deepEqual(edit.secret_values,[{reference,value:'example-only-secret'}]);
  assert.deepEqual(edit.changes[0].secrets,[{path:'secretKey',mode:'reference',reference},{path:'loadBalancer.groupKey',mode:'clear',reference:''}]);
  assert.throws(() => makeEdit({mode:'create',kind:'proxy',type:'stcp',name:'x',secrets:{secretKey:{mode:'reference',value:'****'}}}),/UUID/);
  assert.throws(() => makeEdit({mode:'create',kind:'proxy',type:'stcp',name:'x',secrets:{secretKey:{mode:'replace',value:'\n'}}}),/新秘密/);
});
test('inventory identity, operation candidate and expiration guard apply', () => {
  assert.equal(readInventory(inventory(),'1').service_id,service);
  assert.throws(() => readInventory(inventory(),'2')); assert.throws(() => readInventory({...inventory(),service_id:'invalid'},'1'));
  assert.equal(canApply(result(),preview(),300000),true); assert.equal(canApply(result(),preview(),500000),false);
  assert.equal(canApply(result(),{...preview(),candidate_digest:base},300000),false); assert.equal(canApply(result(),null,300000),false);
  assert.equal(resultFacts({store_persisted:true,runtime_loaded:true,resources_ready:true,business_checked:false})[3][1],'未测试');
});
test('controller only applies an explicitly prepared preview, queries use GET, no secrets retained', async () => {
  const requests=[];
  const controller=createConfigController({now:()=>100000,uuid:()=>reference,request:async(path,options)=>{
    requests.push([path,options]);
    if(path.endsWith('/configuration')) return inventory();
    if(path.endsWith('/operations') && !options) return {operations:[]};
    if(path.endsWith('/operations')) return {...result(),preview:preview()};
    if(path.endsWith('/apply')) return result('confirmed');
    return result('confirmed');
  }});
  await controller.open('1',true);
  const edit=makeEdit({mode:'create',kind:'proxy',type:'stcp',name:'s',secrets:{secretKey:{mode:'replace',value:'ephemeral-value'}}},()=>reference);
  await controller.prepare(edit);
  assert.equal(JSON.stringify(controller.state).includes('ephemeral-value'),false);
  assert.equal(requests.some(([path])=>path.endsWith('/apply')),false);
  await controller.action('apply');
  const apply=requests.find(([path])=>path.endsWith('/apply'));
  assert.equal(apply[1].body.expected_version,3); assert.equal(apply[1].body.candidate_digest,candidate);
  assert.equal(controller.state.needsReload,true);
  await controller.query(); assert.equal(requests.at(-1)[1],undefined);
});
test('clearing a session rejects late inventory and operation responses', async () => {
  let finish;
  const controller=createConfigController({request:async path=>path.endsWith('/configuration')?new Promise(resolve=>{finish=resolve;}):{operations:[]}});
  const loading=controller.open('1',true); controller.clear(); finish(inventory()); await loading;
  assert.equal(controller.state.nodeID,null); assert.equal(controller.state.inventory,null); assert.equal(controller.state.pending,false);
});
test('offline inventory failure still exposes historical operation without enabling writes', async () => {
  const controller=createConfigController({request:async path=>{if(path.endsWith('/configuration'))throw Object.assign(new Error('unavailable'),{status:503});return {operations:[result('confirmed').operation]};}});
  await controller.open('1',false); assert.equal(controller.state.inventory,null); assert.equal(controller.state.operations.length,1); assert.equal(controller.state.online,false);
  await controller.action('apply'); assert.equal(controller.state.result,null);
});
test('opening a history entry never reuses another operation preview', async () => {
  let other=false;
  const controller=createConfigController({now:()=>100000,uuid:()=>reference,request:async(path,options)=>{
    if(path.endsWith('/configuration'))return inventory();
    if(path.endsWith('/operations')&&!options)return {operations:[]};
    if(options)return {...result(),preview:preview()};
    const value=result(); if(other)value.operation.operation_id=reference; return value;
  }});
  await controller.open('1',true); await controller.prepare(makeEdit({mode:'create',kind:'proxy',type:'tcp',name:'new'}));
  assert.ok(controller.state.preview); other=true; await controller.query(reference); assert.equal(controller.state.preview,null);
});

const restoreResponse = (state = 'verified') => ({code:'ok',node_id:'1',service_id:service,received_at_ms:100000,
  restore:{state,epoch:reference,backup_service_id:operation,replaced_service_id:operation,manifest_digest:base,context_revision:context,store_digest:candidate,acknowledgement_id:state==='acknowledged'?reference:'',runtime_loaded:true,resources_ready:true,operations_count:3},receipt:null,active_operation:null});
test('restore acknowledgement binds exact identity and active operation version', () => {
  const input=restoreResponse();input.active_operation={...result('outcome_unknown').operation,service_id:operation,version:7};
  const value=readRestore(input,'1');assert.equal(canAcknowledgeRestore(value,120000),true);
  assert.deepEqual(restoreAcknowledgement(value),{service_id:service,epoch:reference,manifest_digest:base,context_revision:context,store_digest:candidate,expected_active_operation_id:operation,expected_active_version:7});
  assert.equal(canAcknowledgeRestore(value,160001),false);assert.equal(canAcknowledgeRestore(value,99999),false);
  value.active_operation.service_id=service;assert.equal(canAcknowledgeRestore(value,120000),false);
});
test('restore response rejects cross-node facts and strips unrecognized private properties', () => {
  const value=restoreResponse();value.restore.secret='never-copy';value.private_path='/private/ignored';
  assert(!JSON.stringify(readRestore(value,'1')).includes('never-copy'));assert(!JSON.stringify(readRestore(value,'1')).includes('/private/ignored'));
  assert.throws(()=>readRestore(value,'2'));
  assert.throws(()=>readRestore({...value,restore:{...value.restore,manifest_digest:'invalid'}},'1'));
  assert.throws(()=>readRestore({...value,restore:{...value.restore,resources_ready:false}},'1'));
  const pending=readRestore({...value,restore:{...value.restore,state:'pending',runtime_loaded:false,resources_ready:false,store_digest:''}},'1');
  assert.equal(canAcknowledgeRestore(pending,120000),false);
});
test('restore receipt remains pending until Agent confirms and does not imply write access', async () => {
  const calls=[];
  const controller=createConfigController({now:()=>120000,request:async(path,options)=>{
    calls.push([path,options]);
    if(path.endsWith('/configuration'))return inventory();
    if(path.endsWith('/operations'))return {operations:[]};
    if(path.endsWith('/restore'))return restoreResponse();
    const value=restoreResponse('acknowledged');
    value.receipt={id:reference,node_id:'1',service_id:service,epoch:reference,manifest_digest:base,context_revision:context,store_digest:candidate,state:'acknowledged',version:2};
    return value;
  }});
  await controller.open('1',true);assert(!calls.some(([path])=>path.endsWith('/restore')));
  await controller.inspectRestore();assert(!calls.some(([,options])=>options?.method==='POST'));
  await controller.prepare(makeEdit({mode:'create',kind:'proxy',type:'tcp',name:'blocked'}));
  assert(!calls.some(([,options])=>options?.method==='POST'));
  await controller.acknowledgeRestore();
  assert(calls.at(-1)[0].endsWith('/restore/acknowledge'));assert.equal(calls.at(-1)[1].body.expected_active_version,0);
  assert.equal(controller.state.needsReload,true);assert.equal(controller.state.restoration.receipt.state,'acknowledged');
  const length=calls.length;await controller.acknowledgeRestore();assert.equal(calls.length,length);
});
test('late restore response is discarded after switching or clearing sessions', async () => {
  let finish;
  const controller=createConfigController({request:async path=>path.endsWith('/configuration')?inventory():path.endsWith('/restore')?new Promise(resolve=>{finish=resolve;}):{operations:[]}});
  await controller.open('1',true);const pending=controller.inspectRestore();controller.clear();finish(restoreResponse());await pending;
  assert.equal(controller.state.nodeID,null);assert.equal(controller.state.restoration,null);
});
test('restore inspect shares the pending gate with configuration and never queues after it', async () => {
  let finish;const calls=[];
  const controller=createConfigController({now:()=>120000,request:async(path)=>{calls.push(path);return path.endsWith('/configuration')?inventory():path.endsWith('/restore')?new Promise(resolve=>{finish=resolve;}):{operations:[]};}});
  await controller.open('1',true);const pending=controller.inspectRestore();await controller.inspectRestore();await controller.reload();assert.equal(calls.filter(path=>path.endsWith('/restore')).length,1);
  finish(restoreResponse());await pending;assert.equal(controller.state.pending,false);assert.equal(calls.length,3);
});

test('operation reasons separate runtime failure and recovery without exposing unknown raw errors', () => {
  assert.match(resultReason({error_code:'verify_failed'}), /注册或本地启动/);
  assert.match(resultReason({error_code:'recovered'}), /事务日志/);
  assert.equal(resultReason({error_code:''}), '');
  assert.equal(resultReason(null), '');
  const privateError='untrusted-private-error';
  assert(!resultReason({error_code:privateError}).includes(privateError));
});

test('expired materials preserve the terminal result and suppress rollback dispatch', async () => {
  const calls=[];
  const expired={...result('confirmed'),code:'operation_not_found',agent:{materials_state:'expired',materials_expired_at_ms:120000,materials_expiry_reason:'ttl'}};
  const controller=createConfigController({request:async(path,options)=>{
    calls.push({path,options});
    if(path.endsWith('/configuration'))return inventory();
    if(path.endsWith('/operations'))return {operations:[expired.operation]};
    return expired;
  }});
  await controller.open('1',true);await controller.query(operation);
  assert.equal(controller.state.result.operation.state,'confirmed');
  assert.match(controller.state.message,/回退材料已过保留期限/);
  assert.equal(canRollback(expired),false);
  assert.deepEqual(operationMaterials(expired),{state:'expired',expiredAt:120000});
  await controller.action('rollback');assert.equal(calls.length,3);
  assert.equal(resultFacts(expired.agent)[4][1],'已按保留期限清理');
  assert.match(configErrors.managed_capacity,/本次配置未应用/);
});

test('legacy material status remains unknown and only valid expiry facts suppress rollback', () => {
  const legacy=result('confirmed');
  assert.deepEqual(operationMaterials(legacy),{state:'unknown'});assert.equal(canRollback(legacy),true);
  legacy.agent.materials_state='retained';assert.deepEqual(operationMaterials(legacy),{state:'retained'});
  legacy.agent={materials_state:'expired',materials_expired_at_ms:120000,materials_expiry_reason:'ttl'};
  assert.equal(canRollback(legacy),false);
  legacy.agent.materials_expired_at_ms='120000';assert.deepEqual(operationMaterials(legacy),{state:'unknown'});
});

test('relocated material context remains separate from TTL expiry and cannot dispatch rollback', async () => {
  const calls=[],relocated={...result('confirmed'),code:'context_changed',agent:{materials_state:'context_changed'}};
  const controller=createConfigController({request:async path=>{calls.push(path);return path.endsWith('/configuration')?inventory():path.endsWith('/operations')?{operations:[]}:relocated;}});
  await controller.open('1',true);await controller.query(operation);await controller.action('rollback');
  assert.equal(calls.length,3);assert.equal(controller.state.result.operation.state,'confirmed');
  assert.deepEqual(operationMaterials(relocated),{state:'context_changed'});assert.equal(canRollback(relocated),false);
  assert.equal(resultFacts(relocated.agent)[4][1],'保留于迁移前上下文');assert.match(controller.state.message,/迁移前检查点/);
  relocated.agent={materials_state:'expired',materials_expired_at_ms:120000,materials_expiry_reason:'ttl'};
  assert.deepEqual(operationMaterials(relocated),{state:'expired',expiredAt:120000});assert.equal(canRollback(relocated),false);
});


test("a restored identity without portable rollback authorization disables historical writes", () => {
  assert.equal(canRollback({code:"service_mismatch", operation:{state:"confirmed"}, agent:{materials_state:"retained"}}), false);
});
