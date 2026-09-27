import {UNKNOWN, bytes} from "./format.mjs";
import {windows, cumulative, finite, count, dateText, coverage, failureRate, chart, resourceCharts} from "./history-data.mjs";
import {connectHistory} from "./history-transport.mjs";

const svgNS = "http://www.w3.org/2000/svg";
let sequence = 0;
function el(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
}
function svg(tag, attributes = {}) {
  const node = document.createElementNS(svgNS, tag);
  for (const [name, value] of Object.entries(attributes)) node.setAttribute(name, value);
  return node;
}
function write(node, value) { if (node.textContent !== value) node.textContent = value; }
function metric(label, className = "") {
  const node = el("div", className), title = el("dt", "", label), value = el("dd", "", UNKNOWN);
  node.append(title, value); return {node, value};
}
function createChart(spec) {
  const element = el("figure", "fm-history-chart"), heading = el("figcaption", "fm-chart-heading");
  const range = el("span", "fm-chart-range"); heading.append(el("span", "", spec.title), range);
  const plot = svg("svg", {viewBox: "0 0 440 120", role: "img", preserveAspectRatio: "none"});
  const title = svg("title"), desc = svg("desc"), drawing = svg("g", {transform: "translate(0 5)"});
  title.textContent = spec.title;
  plot.append(title, desc);
  for (const y of [0, 55, 110]) drawing.append(svg("line", {x1: 0, x2: 440, y1: y, y2: y, class: "fm-chart-grid"}));
  const paths = spec.series.map((_, index) => {
    const group = svg("g", {class: `fm-chart-series fm-chart-series-${index}`});
    drawing.append(group); return group;
  });
  plot.append(drawing);
  const empty = el("p", "fm-chart-empty", "暂无有效采样"), labels = el("div", "fm-chart-times");
  const from = el("time"), to = el("time"); labels.append(from, to);
  const summaries = el("ul", "fm-chart-summary"), summaryRows = spec.series.map((_, index) => el("li", `fm-series-label-${index}`));
  summaries.append(...summaryRows); element.append(heading, plot, empty, labels, summaries);
  return {element, update(rows, data) {
    const result = chart(rows, spec.series, {window: data.window, generatedAt: data.generated_at, step: data.step_seconds, ceiling: spec.ceiling});
    if (result.empty) plot.setAttribute("hidden", ""); else plot.removeAttribute("hidden");
    empty.hidden = !result.empty;
    const ceiling = result.empty ? UNKNOWN : spec.series[0].format(result.maximum);
    write(range, result.empty ? UNKNOWN : `0 – ${ceiling}`);
    title.textContent = `${spec.title}，纵轴 0 至 ${ceiling}`;
    desc.textContent = `${windows[data.window]}。${result.summaries.join("；")}。缺失时间段留白，不补零。`;
    for (let i = 0; i < paths.length; i++) {
      paths[i].replaceChildren(svg("path", {d: result.paths[i].path}));
      // A lone valid bucket must remain visible even when no line can be drawn.
      for (const point of result.paths[i].dots) paths[i].append(svg("circle", {cx: point.x, cy: point.y, r: 1.7}));
      write(summaryRows[i], result.summaries[i]);
    }
    write(from, dateText(result.start)); from.dateTime = result.start;
    write(to, dateText(result.end)); to.dateTime = result.end;
  }};
}
function createProbe(probe) {
  const element = el("article", "fm-probe"), name = el("h5", "", probe.name), timing = el("p", "fm-history-note");
  const stats = el("dl", "fm-probe-stats"), latency = metric("最近 TCP 建连耗时"), failures = metric("TCP 探测失败率"), total = metric("失败 / 总次数");
  stats.append(latency.node, failures.node, total.node);
  const graph = createChart({title: "TCP 建连耗时趋势", series: [{name: "建连耗时", read: p => finite(p.latency_ms), format: n => `${n.toFixed(1)} ms`}]});
  element.append(name, timing, stats, graph.element);
  return {element, update(next, data) {
    write(name, next.name);
    write(timing, `${finite(next.interval_seconds) !== null ? `每 ${next.interval_seconds} 秒` : "间隔未知"} · ${next.latest_at ? `最近执行 ${dateText(next.latest_at)}` : "等待执行结果"}`);
    write(latency.value, next.latency_ms === -1 ? "建连失败" : finite(next.latency_ms) === null ? UNKNOWN : `${next.latency_ms.toFixed(1)} ms`);
    write(failures.value, failureRate(next));
    write(total.value, `${next.failures} / ${next.samples}`);
    graph.update(next.points, data);
  }};
}

export function createHistoryPanel(card, nodeID, options = {}) {
  const detail = options.mode === "detail";
  const details = detail ? null : card.querySelector(".fm-history"), body = detail ? card : card.querySelector(".fm-history-body");
  const summary = details?.querySelector("summary"), identifier = detail && body.id ? body.id : `history-${++sequence}`;
  body.id = identifier; summary?.setAttribute("aria-controls", identifier);
  const toolbar = el("div", "fm-history-toolbar"), windowLabel = el("label", "fm-select"), selector = el("select");
  selector.setAttribute("aria-label", "历史时间范围");
  for (const [value, label] of Object.entries(windows)) { const option = el("option", "", label); option.value = value; selector.append(option); }
  windowLabel.append(el("span", "", "时间范围"), selector);
  const refresh = el("button", "wsk-button fm-refresh", "刷新历史"); refresh.type = "button";
  const sectionSwitch = el("div", "fm-history-sections"), rangeSwitch = el("div", "fm-history-ranges");
  sectionSwitch.setAttribute("role", "group"); sectionSwitch.setAttribute("aria-label", "历史图表内容");
  rangeSwitch.setAttribute("role", "group"); rangeSwitch.setAttribute("aria-label", "历史时间范围");
  let section = "resources", selected = "1h", visible = !detail, last = null, stopped = false;
  const sectionButtons = new Map(), rangeButtons = new Map();
  if (detail) {
    for (const [key, name] of [["resources", "资源"], ["network", "网络延迟"]]) {
      const button = el("button", "fm-history-tab", name); button.type = "button";
      button.setAttribute("aria-pressed", String(section === key)); sectionSwitch.append(button); sectionButtons.set(key, button);
      button.addEventListener("click", () => { section = key; applySection(); });
    }
    for (const [key, name] of Object.entries(windows)) {
      const button = el("button", "fm-history-range", key); button.type = "button";
      button.setAttribute("aria-label", name); button.setAttribute("aria-pressed", String(selected === key)); rangeSwitch.append(button); rangeButtons.set(key, button);
      button.addEventListener("click", () => {
        selected = key; for (const [value, item] of rangeButtons) item.setAttribute("aria-pressed", String(value === selected));
        connection.select(key);
      });
    }
    const controls = el("div", "fm-history-controls"); controls.append(rangeSwitch, refresh);
    toolbar.append(sectionSwitch, controls);
  } else toolbar.append(windowLabel, refresh);
  const status = el("p", "fm-history-status", detail ? "正在准备历史记录…" : "展开后获取历史记录"); status.setAttribute("role", "status");
  const storage = el("p", "fm-history-storage"), content = el("div", "fm-history-content"); content.hidden = true;
  const trafficHeader = el("div", "fm-history-heading"), trafficTitle = el("h4", "", "今日主机流量"), day = el("span", "fm-history-note"); trafficHeader.append(trafficTitle, day);
  const traffic = el("dl", "fm-traffic"), rx = metric("累计接收"), tx = metric("累计发送"); traffic.append(rx.node, tx.node);
  const trafficNote = el("p", "fm-history-note"), chartsTitle = el("h4", "", "资源趋势"), chartNote = el("p", "fm-history-note");
  const charts = el("div", "fm-history-charts"), graphs = resourceCharts.map(createChart); charts.append(...graphs.map(g => g.element));
  const probeTitle = el("h4", "", "TCP 探测"), probeNote = el("p", "fm-history-note", "失败率是所选范围内 TCP 建连失败次数占比，不表示 IP 丢包率。耗时曲线仅汇总成功建连；全部失败的时间段留白。"), probeState = el("p", "fm-history-storage"), probeEmpty = el("p", "fm-history-empty", "暂无 TCP 探测记录。"), probeList = el("div", "fm-probe-list");
  const probes = new Map(), resources = el("div", "fm-resource-history"), probeGroup = el("div", "fm-network-history");
  resources.id = `${identifier}-resources`; probeGroup.id = `${identifier}-network`;
  sectionButtons.get("resources")?.setAttribute("aria-controls", resources.id); sectionButtons.get("network")?.setAttribute("aria-controls", probeGroup.id);
  if (!detail) resources.append(trafficHeader, traffic, trafficNote, chartsTitle);
  resources.append(chartNote, charts); probeGroup.append(probeTitle, probeState, probeNote, probeEmpty, probeList);
  content.append(resources, probeGroup); body.append(toolbar, status, storage, content);
  function applySection() {
    resources.hidden = last?.storage.state === "disabled" || (detail && section !== "resources");
    probeGroup.hidden = detail && section !== "network";
    for (const [key, button] of sectionButtons) button.setAttribute("aria-pressed", String(section === key));
  }
  applySection();
  function onState(state) {
    const stamp = last ? `${windows[last.window]} · 更新于 ${dateText(last.generated_at)}` : "";
    status.dataset.state = state;
    body.setAttribute("aria-busy", state === "loading" ? "true" : "false");
    write(status, state === "loading" ? (last ? `正在读取；当前保留上次成功结果（${stamp}）。` : "正在读取历史记录…") : state === "error" ? (last ? `历史更新失败，30 秒后重试。以下为上次成功结果（${stamp}），可能已过期。` : "暂时无法读取历史记录，30 秒后重试。") : `${stamp} · ${detail ? "页面可见" : "展开"}期间每 30 秒更新`);
    options.onState?.(state, last);
  }
  function onData(data) {
    last = data;
    const state = data.storage.state, dropped = count(data.storage.dropped) ? data.storage.dropped : null;
    storage.dataset.state = state;
    write(storage, state === "disabled" ? "历史存储未启用，趋势与今日累计流量暂不可用。" : state === "degraded" ? `历史存储降级，记录可能不完整${dropped ? `；已丢弃 ${dropped} 条记录` : ""}。缺失数据不会补成 0。` : "历史按分钟归档，最新记录可能延迟约 1 分钟；图表缺口表示没有有效采样。");
    content.hidden = false; applySection();
    const flow = data.traffic ?? {};
    write(day, /^\d{4}-\d{2}-\d{2}$/.test(flow.day) ? `${flow.day} · UTC` : "UTC 日期未知");
    const receive = cumulative(flow.rx_bytes), send = cumulative(flow.tx_bytes);
    write(rx.value, bytes(receive)); write(tx.value, bytes(send));
    if (receive !== null) rx.value.title = `${receive} bytes`; else rx.value.removeAttribute("title");
    if (send !== null) tx.value.title = `${send} bytes`; else tx.value.removeAttribute("title");
    write(trafficNote, `基于 UTC 当日收到的有效网卡计数器差分 · 覆盖 ${coverage(flow.coverage_seconds)}${count(flow.resets) ? ` · ${flow.resets} 次计数重置` : ""}。覆盖时间只计有效观察；同一计数范围可补回断线增量，跨日按接收日归属可能有误差。不等于上方系统计数器累计量、FRP 隧道流量或流量账单。`);
    write(chartNote, `${windows[data.window]} · 每个时间段 ${coverage(data.step_seconds)}。数值仅汇总有效采样；缺失时段留白。`);
    for (const graph of graphs) graph.update(data.points, data);
    probeState.dataset.state = data.probes_state;
    write(probeState, ({disabled: "未配置 TCP 探测任务。", waiting: "等待节点接入并启用 TCP 探测；已有结果可能不是当前状态。", ready: "节点已启用 TCP 探测，按配置间隔执行。", degraded: "TCP 任务配置不可用；如有记录，以下保留最近有效配置与结果。"})[data.probes_state] ?? "TCP 任务状态未知。");
    write(probeEmpty, state === "disabled" ? "历史存储未启用，当前没有持久化探测结果。" : "暂无 TCP 探测记录。");
    const existing = new Set(data.probes.map(p => p.id));
    for (const [id, panel] of probes) if (!existing.has(id)) { panel.element.remove(); probes.delete(id); }
    data.probes.forEach((probe, index) => {
      if (!probes.has(probe.id)) probes.set(probe.id, createProbe(probe));
      const panel = probes.get(probe.id); panel.update(probe, data);
      if (probeList.children[index] !== panel.element) probeList.insertBefore(panel.element, probeList.children[index] ?? null);
    });
    probeEmpty.hidden = data.probes.length !== 0;
    options.onData?.(data);
  }
  const connection = connectHistory({nodeID, onData, onState});
  details?.addEventListener("toggle", () => { if (!stopped) connection.activate(visible && details.open); });
  selector.addEventListener("change", () => connection.select(selector.value));
  refresh.addEventListener("click", () => connection.refresh());
  return {
    setVisible(next) { if (stopped) return; visible = next; connection.activate(visible && (detail || details.open)); },
    setName(name) { summary?.setAttribute("aria-label", `${name} · 趋势与探测`); },
    stop() { stopped = true; connection.stop(); },
  };
}
