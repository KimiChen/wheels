// SPDX-License-Identifier: Apache-2.0
// 公开节点详情页：?id= 指定节点。首次经 REST 取得 {now, node} 立刻渲染并
// 暴露 404；之后由 /events/public 的 snapshot / node 事件就地更新。
// 公开 DTO 不含 Facts、本地目标等私有字段，本页也没有对应区块。

import { ApiError, publicApi } from "../api.js";
import { createStream, STREAM_STATE } from "../stream.js";
import {
  fillProbes,
  fillProxies,
  fillResources,
  fillStatus,
  fillTraffic,
} from "./detail.js";
import { createServerClock, sessionBadge, setBadge, setConnPill } from "./status.js";
import { createTrends } from "./trends.js";

const clock = createServerClock();
const pill = document.getElementById("conn-status");
const content = document.getElementById("node-content");
const notFound = document.getElementById("node-notfound");
const headState = document.getElementById("node-head-state");
const subtitle = document.getElementById("node-sub");
const proxyBody = document.getElementById("proxy-tbody");
const proxyTemplate = document.getElementById("proxy-row-template");
const probeBody = document.getElementById("probe-tbody");
const probeTemplate = document.getElementById("probe-row-template");

const nodeId = new URLSearchParams(location.search).get("id");

// 趋势走 metrics 历史接口（公开路由，管理端同用）；当前值仍由 SSE 实时更新。
const trends = createTrends({
  root: document.getElementById("trend-section"),
  fetchMetrics: (id, range) => publicApi.nodeMetrics(id, range),
});

function showNotFound(text) {
  content.hidden = true;
  notFound.hidden = false;
  trends.hide();
  const message = notFound.querySelector("[data-notfound-text]");
  if (message && text) message.textContent = text;
}

function fill(node, nowSec) {
  notFound.hidden = true;
  content.hidden = false;
  setBadge(headState, sessionBadge(node));
  fillStatus(content, node, nowSec);
  fillResources(content, node);
  fillProxies(proxyBody, proxyTemplate, node.proxies);
  fillProbes(probeBody, probeTemplate, node.probes);
  fillTraffic(content, node.traffic);
  trends.show(node.id);
}

function onNode(node, nowSec) {
  if (!node || node.id !== nodeId) return;
  fill(node, nowSec);
}

async function boot() {
  if (!nodeId) {
    showNotFound("链接缺少节点 ID，请从总览页进入。");
    setConnPill(pill, { state: STREAM_STATE.FAILED });
    return;
  }
  subtitle.textContent = nodeId;
  document.title = `节点 ${nodeId} · frp-monitor`;

  try {
    const data = await publicApi.node(nodeId);
    clock.update(data?.now);
    if (data?.node) fill(data.node, clock.now());
    else showNotFound("节点不存在或已被移除。");
  } catch (error) {
    if (error instanceof ApiError && error.status === 404) {
      showNotFound("节点不存在或已被移除。");
    } else {
      // 首次加载失败不判死刑，SSE snapshot 仍可能带来该节点。
      globalThis.wsk?.showToast("节点数据加载失败，等待实时通道…", "warning");
    }
  }

  if (!("EventSource" in window)) {
    setConnPill(pill, { state: STREAM_STATE.FAILED });
    return;
  }
  createStream("/events/public", {
    onSnapshot(data) {
      clock.update(data?.now);
      const nodes = Array.isArray(data?.nodes) ? data.nodes : [];
      const node = nodes.find((item) => item.id === nodeId);
      if (node) fill(node, clock.now());
      else showNotFound("节点不存在或已被移除。");
    },
    onNode: (node) => onNode(node, clock.now()),
    onStatus: (status) => setConnPill(pill, status),
  });
}

void boot();
