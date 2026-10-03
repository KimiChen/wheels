import {actorKinds, auditKinds, auditStateLabels, createAuditController} from "./admin-audit-data.mjs";

const actionLabels = {create:"创建",update:"修改",delete:"删除",enable:"启用",disable:"禁用",rename:"重命名",clone:"克隆"};
const filterLabels = {node_id:"节点",kind:"类型",actor_kind:"操作者类型",actor:"操作者",object_kind:"对象类型",object_name:"对象",operation_id:"操作",state:"结果",service_id:"Service"};
const time = ms => ms == null ? "未知" : new Date(ms).toLocaleString("zh-CN",{hour12:false});
export function createAuditPanel(root, {request,download}) {
  const document = root.ownerDocument, $ = id => root.querySelector(`#${id}`);
  const element = (tag,text,className) => {const node=document.createElement(tag);if(text!==undefined)node.textContent=text;if(className)node.className=className;return node;};
  const form = $("audit-filters"), links = new Map();
  const controller = createAuditController({request,download,onChange:render,onExport:save});
  for (const [name,labels] of [["kind",auditKinds],["actor_kind",actorKinds],["state",auditStateLabels]]) {
    for (const [value,label] of Object.entries(labels)) {const option=element("option",label);option.value=value;form.elements[name].append(option);}
  }
  function fields() {
    return Object.fromEntries([...new FormData(form)].map(([key,value]) => [key,["from_ms","to_ms"].includes(key) ? value ? new Date(value).getTime() : "" : value]));
  }
  function facts(values) {
    const list=element("dl",undefined,"fa-detail-list");
    for(const [label,value] of values){const row=element("div");row.append(element("dt",label),element("dd",value===""||value==null?"—":String(value)));list.append(row);}return list;
  }
  function button(text,action) {const node=element("button",text,"wsk-button wsk-secondary fa-small-button");node.type="button";node.addEventListener("click",action);return node;}
  function save(text) {
    const url=URL.createObjectURL(new Blob([text],{type:"application/x-ndjson;charset=utf-8"}));
    const link=element("a");link.href=url;link.download="frp-plus-config-audit.jsonl";link.hidden=true;root.append(link);link.click();link.remove();
    links.set(url,setTimeout(()=>{URL.revokeObjectURL(url);links.delete(url);},1000));
  }
  function renderDetail() {
    const {detail,detailPending,selectedID}=controller.state, box=$("audit-detail");box.replaceChildren();
    if (!detail) {box.append(element("p",detailPending?"正在读取记录…":selectedID?"此记录详情暂不可用。":"选择一条记录查看脱敏摘要。","fa-muted"));return;}
    const {item,changes}=detail;
    box.append(facts([["审计 ID",item.audit_id],["记录时间",time(item.recorded_at_ms)],["发生时间",time(item.occurred_at_ms)],["记录类型",auditKinds[item.kind]],["操作者类型",actorKinds[item.actor_kind]],["操作者标识",item.actor],["观察记录者",item.observer],["节点 ID",item.node_id],["Service ID",item.service_id],["操作 ID",item.operation_id],["操作版本",item.operation_version],["事件时的结果",auditStateLabels[item.state]??"不适用"],["原因代码",item.code],["对象 / 字段数",`${item.object_count} / ${item.field_count}`]]));
    if(item.operation_id)box.append(button("查询同一操作",()=>{form.elements.operation_id.value=item.operation_id;void controller.search({...controller.state.filters,operation_id:item.operation_id});}));
    const preview=item.preview_summary;
    if(preview){
      const details=element("details",undefined,"fa-audit-more");details.append(element("summary","修订与预览摘要"));
      const values=[["需要重新加载",preview.reload_required?"是":"否"]];
      for(const [key,label] of [["base_revision","基础修订"],["context_revision","上下文修订"],["candidate_digest","候选摘要"],["filter_digest","筛选摘要"],["export_id","导出 ID"],["watermark_id","导出上界 ID"]])if(preview[key])values.push([label,preview[key]]);
      if(preview.export_id)values.push(["导出条数",preview.rows],["生成字节数",preview.bytes]);
      if(item.code==="audit_gc")values.push(["清理记录数",preview.rows],["释放逻辑字节数",preview.bytes]);
      details.append(facts(values));
      if(preview.warnings.length)details.append(element("p",`提示代码：${preview.warnings.join("、")}`));box.append(details);
    }
    box.append(element("h3","对象变更"));
    if(!changes.length)box.append(element("p","此事件没有对象字段摘要。","fa-muted"));
    for(const change of changes){
      const entry=element("div",undefined,"fa-config-object");entry.append(element("h4",`${change.kind === "proxy"?"Proxy":"Visitor"} · ${change.name}`),element("p",actionLabels[change.action]));
      if(change.clone_from)entry.append(element("p",`克隆来源：${change.clone_from}`));
      entry.append(element("p",change.fields.length?`变更字段：${change.fields.join("、")}`:"没有字段明细。"));box.append(entry);
    }
  }
  function render() {
    const state=controller.state, page=state.pages[state.pageIndex];
    $("audit-message").textContent=state.message;
    $("audit-search").disabled=state.pending;$("audit-export").disabled=state.pending||state.exporting||!state.loaded;
    $("audit-export").textContent=state.exporting?"正在准备导出…":"导出本次查询";
    $("audit-previous").disabled=state.pending||state.pageIndex===0;$("audit-next").disabled=state.pending||!page?.has_more;
    $("audit-page").textContent=page?`本页 ${page.items.length} 条`:"";
    $("audit-range").textContent=page?`已查询 ${time(page.filters.from_ms)} 至 ${time(page.filters.to_ms)}（不含结束时间） · 查询上界 #${page.watermark_id}`:"";
    if(page)$("audit-range").textContent += Object.entries(page.filters).filter(([key])=>filterLabels[key]).map(([key,value])=>` · ${filterLabels[key]}：${value}`).join("");
    $("audit-retention").textContent=page?`保留目标：审计 ${page.retention.audit_days} 天；Agent 私有快照默认 ${page.retention.snapshot_days} 天，以该 Agent 启动配置为准。${page.retention.cleanup_enabled?"按服务端策略清理。":"自动清理尚未启用。"}`:"";
    if(page?.retention.generation!=null){const r=page.retention;$("audit-retention").textContent+=` 审计逻辑用量 ${r.rows} / ${r.max_rows} 条，${(r.bytes/1048576).toFixed(1)} / ${(r.max_bytes/1048576).toFixed(1)} MiB；清理代际 ${r.generation}${r.last_gc_at_ms?`，最近清理 ${time(r.last_gc_at_ms)}`:""}。${r.capacity_blocked?"已达到新操作准入上限；已有操作仍可查询和恢复。":""}`;}
    const list=$("audit-list"), focus=list.contains(document.activeElement)?document.activeElement.dataset.auditID:null;
    list.replaceChildren();
    for(const item of page?.items??[]){
      const row=element("li"), choose=element("button",undefined,"fa-audit-record");choose.type="button";choose.dataset.auditID=item.audit_id;choose.setAttribute("aria-pressed",String(state.selectedID===item.audit_id));
      choose.append(element("strong",`${auditKinds[item.kind]} · ${auditStateLabels[item.state]??item.code}`),element("span",`${time(item.recorded_at_ms)} · #${item.audit_id}`),element("small",`${actorKinds[item.actor_kind]}${item.actor?` · ${item.actor}`:""}${item.node_id?` · 节点 ${item.node_id}`:""} · ${item.object_count} 个对象`));
      choose.addEventListener("click",()=>void controller.select(item.audit_id));row.append(choose);list.append(row);
    }
    if(focus)[...list.querySelectorAll("button")].find(node=>node.dataset.auditID===focus)?.focus({preventScroll:true});
    $("audit-empty").hidden=Boolean(page?.items.length);$("audit-empty").textContent=state.pending?"正在查询…":state.loaded?"此范围内没有匹配记录。":"尚无查询结果。";
    renderDetail();
  }
  form.addEventListener("submit",event=>{event.preventDefault();void controller.search(fields());});
  $("audit-reset").addEventListener("click",()=>{form.reset();void controller.search({});});
  $("audit-previous").addEventListener("click",()=>controller.previous());
  $("audit-next").addEventListener("click",()=>void controller.next());
  $("audit-export").addEventListener("click",()=>void controller.export());
  return {state:controller.state,open(){if(!controller.state.loaded&&!controller.state.pending)void controller.search(fields());},showOperation(operationID){form.reset();form.elements.operation_id.value=operationID;void controller.search({operation_id:operationID});},clear(){for(const [url,timer]of links){clearTimeout(timer);URL.revokeObjectURL(url);}links.clear();form.reset();for(const details of root.querySelectorAll("details"))details.open=false;controller.clear();}};
}
