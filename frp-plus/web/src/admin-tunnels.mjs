import {createTunnelController,tunnelWindows,storageLabels,qualityLabels,stateLabels,eventLabels,accountingLabels,scopeLabels,tunnelChartRows} from "./admin-tunnels-data.mjs";
import {operationLabels} from "./admin-configuration-data.mjs";
import {chart} from "./history-data.mjs";
import {bytes,cumulative} from "./format.mjs";

const when=value=>value?new Date(value).toLocaleString("zh-CN",{hour12:false}):"未知";
const source=value=>value==="server"?"服务端":"Agent";
const recorded=value=>bytes(cumulative(value));
const rate=value=>value==null?"未知":`${value.toLocaleString("zh-CN",{maximumFractionDigits:2})} B/s`;
export function createTunnelPanel(root,{request,onAudit=()=>{}}){
  const document=root.ownerDocument,$=id=>root.querySelector(`#${id}`);
  const el=(tag,text,className)=>{const node=document.createElement(tag);if(text!==undefined)node.textContent=text;if(className)node.className=className;return node;};
  const svg=(tag,attrs={})=>{const node=document.createElementNS("http://www.w3.org/2000/svg",tag);for(const [key,value]of Object.entries(attrs))node.setAttribute(key,value);return node;};
  const controller=createTunnelController({request,onChange:render});
  const button=(label,action)=>{const node=el("button",label,"wsk-button wsk-secondary fa-small-button");node.type="button";node.addEventListener("click",action);return node;};
  function facts(entries){const dl=el("dl",undefined,"fa-detail-list");for(const [name,value]of entries){const row=el("div");row.append(el("dt",name),el("dd",value==null||value===""?"—":String(value)));dl.append(row);}return dl;}
  function plot(title,series,history){
    const figure=el("figure",undefined,"wsk-panel fa-tunnel-chart"),caption=el("figcaption",title);
    const rows=tunnelChartRows(history,series);
    const result=chart(rows,series,{window:controller.state.window,generatedAt:new Date(history.generated_at_ms).toISOString(),step:history.step_seconds});
    figure.append(caption);
    if(result.empty){figure.append(el("p","暂无有效采样；缺失值保持未知。","fa-muted"));return figure;}
    const drawing=svg("svg",{viewBox:"0 0 450 130",role:"img"}),name=svg("title"),description=svg("desc"),group=svg("g",{transform:"translate(5 10)"});
    name.textContent=title;description.textContent=result.summaries.join("；")+"。缺口不连线，不补零。";drawing.append(name,description,group);
    for(const y of [0,55,110])group.append(svg("line",{x1:0,x2:440,y1:y,y2:y,class:"fa-tunnel-grid"}));
    result.paths.forEach((path,index)=>{const line=svg("g",{class:`fa-tunnel-series-${index}`});line.append(svg("path",{d:path.path}));for(const point of path.dots)line.append(svg("circle",{cx:point.x,cy:point.y,r:1.6}));group.append(line);});
    figure.append(el("p",`纵轴：0 至 ${series[0].format(result.maximum)}`,"fa-muted"),drawing,el("p",`${when(Date.parse(result.start))} — ${when(history.generated_at_ms)}`,"fa-muted"));
    for(const summary of result.summaries)figure.append(el("p",summary,"fa-muted"));return figure;
  }
  function renderHistory(){
    const s=controller.state,box=$("tunnel-history");box.replaceChildren();
    if(!s.history){box.append(el("p",s.historyPending?"正在读取趋势…":"选择实例后读取趋势。","fa-muted"));return;}
    const h=s.history;
    box.append(el("p",`指标存储：${storageLabels[h.metric_storage.state]} · 保留 ${h.metric_storage.retention_days} 天 · ${h.step_seconds} 秒一格。历史采用近似值；${accountingLabels[h.accounting]}，${scopeLabels[h.byte_scope]}。`,"fa-muted"));
    box.append(plot("已记账字节的估算速率",[
      {name:"服务端 → frpc",read:p=>["ok","partial"].includes(p.quality)?p.rx_estimated_bytes_per_second:null,format:rate},
      {name:"frpc → 服务端",read:p=>["ok","partial"].includes(p.quality)?p.tx_estimated_bytes_per_second:null,format:rate}
    ],h),plot("连接数",[{name:"连接数",read:p=>["ok","partial"].includes(p.connections_quality)?p.connections:null,format:n=>Number(n).toLocaleString("zh-CN",{maximumFractionDigits:2})}],h));
    const partial=h.points.filter(p=>p.quality==="partial").length,unknown=h.points.filter(p=>p.quality==="unknown").length;
    box.append(el("p",`本窗口 ${h.points.length} 个时间格；字节量部分覆盖 ${partial} 格，未知 ${unknown} 格。缺口字节量单列，不分摊到最后一个时间格。`,"fa-muted"));
    const samples=el("details"),summary=el("summary","采样覆盖与入账明细");samples.append(summary);
    const table=el("table",undefined,"fa-tunnel-samples"),head=el("thead"),row=el("tr");
    for(const label of ["时间","字节质量 / 连接质量","样本 / 覆盖秒","记录字节：服务端 → frpc / 反向"])row.append(el("th",label));head.append(row);table.append(head);const body=el("tbody");
    for(const p of h.points){const tr=el("tr");for(const value of [when(p.at_ms),`${qualityLabels[p.quality]} / ${qualityLabels[p.connections_quality]}`,`${p.samples} / ${p.coverage_seconds}`,`${recorded(p.rx_recorded_bytes)} / ${recorded(p.tx_recorded_bytes)}`])tr.append(el("td",value));body.append(tr);}table.append(body);samples.append(table);box.append(samples);
    if(h.gap_intervals.length){const gaps=el("details");gaps.append(el("summary",`缺失与重置区间（${h.gap_intervals.length}）`));const labels={collection_gap:"采样间隔缺失",counter_reset:"计数重置",collector_gap:"采集器缺失",invalid_sample:"无效样本"};for(const gap of h.gap_intervals)gaps.append(el("p",`${when(gap.from_ms)} — ${when(gap.to_ms)} · ${labels[gap.reason]} · ${qualityLabels[gap.quality]} · 期间已记账 ${recorded(gap.rx_recorded_bytes)} / ${recorded(gap.tx_recorded_bytes)}`));box.append(gaps);}
  }
  function renderEvents(){
    const s=controller.state,box=$("tunnel-events");box.replaceChildren();$("tunnel-more-events").disabled=s.detailPending||!s.events?.next_cursor;
    if(!s.events){box.append(el("p",s.detailPending?"正在读取事件…":"尚无事件结果。","fa-muted"));return;}
    const events=s.events;box.append(el("p",`事件存储：${storageLabels[events.event_storage.state]} · 保留 ${events.event_storage.retention_days} 天。${events.events_pruned?`部分旧事件已清理；保留边界 ${when(events.retained_from_ms)}。`:""}`,"fa-muted"));
    if(!events.items.length)box.append(el("p","此实例暂无保留事件。","fa-muted"));
    const list=el("ol",undefined,"fa-audit-list");for(const event of events.items){const li=el("li",undefined,"fa-tunnel-event");li.append(el("strong",`${eventLabels[event.code]} · ${source(event.source)}`),el("p",`${event.time_basis==="native"?"原生发生":"采样观察"} ${when(event.occurred_at_ms)} · 主控收到 ${when(event.received_at_ms)} · #${event.event_id}`,"fa-muted"));if(event.state)li.append(el("p",`${stateLabels[event.state.status]}${event.state.error_code?` · ${event.state.error_code}`:""}`));if(event.operation_id)li.append(button("查看关联操作审计",()=>onAudit(event.operation_id)));list.append(li);}box.append(list);
    if(events.operation_links.length){box.append(el("h3","声明关联的配置操作"),el("p","这些操作声明修改了同一对象；该关联不表示某个服务端事件由此操作导致。","fa-muted"));for(const link of events.operation_links){const entry=el("div",undefined,"fa-tunnel-event");entry.append(el("p",`${when(link.created_at_ms)} · ${operationLabels[link.state]}`),button(`查询操作 ${link.operation_id}`,()=>onAudit(link.operation_id)));for(const change of link.changes)entry.append(el("p",`${change.kind} · ${change.name} · ${change.action} · ${change.fields.join("、")||"没有字段摘要"}`));box.append(entry);}}
  }
  function render(){
    const s=controller.state;$("tunnel-message").textContent=s.message;$("tunnel-search").disabled=s.pending;$("tunnel-previous").disabled=s.pending||s.pageIndex===0;$("tunnel-next").disabled=s.pending||!s.list?.next_cursor;
    $("tunnel-list-status").textContent=s.pending?"正在查询…":s.list?`本页 ${s.list.items.length} 个对象 · 更新于 ${when(s.list.generated_at_ms)} · 事件 ${storageLabels[s.list.event_storage.state]} / 指标 ${storageLabels[s.list.metric_storage.state]}`:"进入页面后查询。";
    const list=$("tunnel-list"),focus=list.contains(document.activeElement)?document.activeElement.dataset.tunnelId:null;list.replaceChildren();
    for(const item of s.list?.items??[]){const row=el("li"),choose=button(`${item.raw_name} · ${item.kind}`,()=>void controller.select(item.tunnel_id));choose.className="fa-audit-record";choose.dataset.tunnelId=item.tunnel_id;choose.setAttribute("aria-pressed",String(s.selected?.tunnel_id===item.tunnel_id));choose.append(el("span",`节点 ${item.node_id??"未绑定"} · ${item.identity_quality==="stable"?"稳定身份":item.identity_quality==="conflict"?"绑定冲突":"仅实例内观察"} · 当前 ${item.current_instances.length} 个实例`));row.append(choose);list.append(row);}
    if(focus)[...list.querySelectorAll("button")].find(b=>b.dataset.tunnelId===focus)?.focus({preventScroll:true});
    $("tunnel-empty").hidden=s.pending||Boolean(s.list?.items.length);$("tunnel-empty").textContent=s.loaded?"没有匹配的隧道历史。":"尚未查询。";
    const detail=$("tunnel-identity");detail.replaceChildren();$("tunnel-selection").hidden=!s.selected;
    if(!s.selected){$("tunnel-instance").replaceChildren();$("tunnel-instance-facts").replaceChildren();$("tunnel-history").replaceChildren();$("tunnel-events").replaceChildren();return;}
    const t=s.selected;detail.append(facts([["对象",`${t.kind} · ${t.raw_name}`],["节点",t.node_id??"未绑定"],["服务端 / 用户",`${t.server_id} / ${t.user||"空用户"}`],["客户端身份",t.raw_client_id??"未知"],["绑定代际",t.binding_epoch],["历史起点",when(t.retained_from_ms)]]));
    const selector=$("tunnel-instance");selector.replaceChildren();for(const i of s.instances){const option=el("option",`${source(i.source)} · 第 ${i.generation} 代 · ${i.protocol} · ${stateLabels[i.state.status]}${i.freshness==="stale"?" · 已过时":""}`);option.value=i.instance_id;selector.append(option);}selector.value=s.instanceID??"";selector.disabled=!s.instances.length;$("tunnel-more-instances").disabled=s.detailPending||!s.instanceCursor;
    const instance=s.instances.find(i=>i.instance_id===s.instanceID),factsBox=$("tunnel-instance-facts");factsBox.replaceChildren();
    if(instance){const c=instance.counters;factsBox.append(facts([["最近观察",`${when(instance.last_observed_at_ms)}${instance.freshness==="stale"?"（已过时）":""}`],["状态",stateLabels[instance.state.status]],["本地 / 远端",`${stateLabels[instance.state.local_state]} / ${stateLabels[instance.state.remote_state]}`],["P2P / fallback",`${stateLabels[instance.state.p2p_state]} / ${stateLabels[instance.state.fallback_state]}`],["当前连接",c.connections_quality==="ok"?c.connections:qualityLabels[c.connections_quality]],["实例累计：服务端 → frpc / 反向",c.quality==="ok"?`${recorded(c.rx_bytes)} / ${recorded(c.tx_bytes)}`:qualityLabels[c.quality]],["字节口径",`${accountingLabels[c.accounting]} · ${scopeLabels[c.byte_scope]}`],["创建 / 关闭",`${when(instance.created_at_ms)} / ${instance.closed_at_ms==null?"尚无关闭观察":when(instance.closed_at_ms)}`]]));const more=el("details");more.append(el("summary","实例标识"),facts([["实例 ID",instance.instance_id],["原生实例 ID",instance.native_instance_id],["进程代际",instance.process_epoch],["计数代际",c.epoch],["托管 Service",instance.service_id],["错误代码",instance.state.error_code]]));factsBox.append(more);}
    $("tunnel-window").value=s.window;renderHistory();renderEvents();
  }
  for(const [value,label]of Object.entries(tunnelWindows)){const option=el("option",label);option.value=value;$("tunnel-window").append(option);}
  $("tunnel-filters").addEventListener("submit",event=>{event.preventDefault();void controller.search(Object.fromEntries(new FormData(event.currentTarget)));});
  $("tunnel-previous").addEventListener("click",()=>controller.previous());$("tunnel-next").addEventListener("click",()=>void controller.next());
  $("tunnel-instance").addEventListener("change",event=>void controller.selectInstance(event.target.value));$("tunnel-more-instances").addEventListener("click",()=>void controller.moreInstances());
  $("tunnel-window").addEventListener("change",event=>void controller.setWindow(event.target.value));$("tunnel-more-events").addEventListener("click",()=>void controller.moreEvents());
  return{state:controller.state,open(){if(!controller.state.loaded&&!controller.state.pending)void controller.search({});},clear(){$("tunnel-filters").reset();controller.clear();}};
}
