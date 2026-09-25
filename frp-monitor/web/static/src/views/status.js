// SPDX-License-Identifier: Apache-2.0
// 三种状态分别展示：监控会话（online）、指标新鲜度（metrics_stale）、
// FRP 控制连接（frp_control_connected）。未知（null）不得显示为 0 或在线。

import { UNKNOWN } from "../format.js";

const BADGE_VARIANTS = new Set(["success", "warning", "danger", "accent"]);

export function setBadge(el, { variant, text, dot = false }) {
  if (!el) return;
  el.classList.remove(
    ...[...BADGE_VARIANTS].map((name) => `wsk-${name}`),
  );
  if (variant && BADGE_VARIANTS.has(variant)) {
    el.classList.add(`wsk-${variant}`);
  }
  el.replaceChildren();
  if (dot) {
    const span = document.createElement("span");
    span.className = "wsk-dot";
    el.append(span);
  }
  el.append(document.createTextNode(text));
}

/** 监控会话：在线 / 离线。 */
export function sessionBadge(node) {
  return node?.online === true
    ? { variant: "success", text: "在线", dot: true }
    : { variant: "danger", text: "离线" };
}

/** 指标新鲜度：metrics_stale=true 显示「数据过期」。 */
export function freshnessBadge(node) {
  if (node?.metrics_stale === true) {
    return { variant: "warning", text: "数据过期" };
  }
  if (node?.metrics_stale === false) {
    return { variant: "success", text: "指标新鲜" };
  }
  return { variant: null, text: `指标${UNKNOWN}` };
}

/** FRP 控制连接：true / false / null（未知）。 */
export function frpBadge(node) {
  if (node?.frp_control_connected === true) {
    return { variant: "success", text: "FRP 已连接" };
  }
  if (node?.frp_control_connected === false) {
    return { variant: "danger", text: "FRP 未连接" };
  }
  return { variant: null, text: `FRP ${UNKNOWN}` };
}

export function fillStatusBadges(scope, node) {
  setBadge(scope.querySelector('[data-badge="session"]'), sessionBadge(node));
  setBadge(scope.querySelector('[data-badge="freshness"]'), freshnessBadge(node));
  setBadge(scope.querySelector('[data-badge="frp"]'), frpBadge(node));
}

/**
 * 顶栏连接状态提示。
 * @param {HTMLElement} pill .wsk-badge 容器
 */
export function setConnPill(pill, status) {
  if (!pill) return;
  const { state, delayMs } = status;
  if (state === "connected") {
    setBadge(pill, { variant: "success", text: "实时已连接", dot: true });
  } else if (state === "reconnecting") {
    const seconds = delayMs ? Math.round(delayMs / 1000) : null;
    setBadge(pill, {
      variant: "warning",
      text: seconds ? `连接断开，${seconds} 秒后重连` : "连接断开，重连中",
    });
  } else if (state === "failed") {
    setBadge(pill, { variant: "danger", text: "实时通道不可用" });
  } else {
    setBadge(pill, { text: "连接中…" });
  }
}

/**
 * 服务端时钟跟踪：事件携带的 now（Unix 秒）为基准，客户端单调推进，
 * 避免客户端本机时钟偏差影响相对时间显示。
 */
export function createServerClock() {
  let serverSec = null;
  let clientAt = 0;
  return {
    update(now) {
      if (Number.isFinite(now)) {
        serverSec = now;
        clientAt = Date.now() / 1000;
      }
    },
    now() {
      if (serverSec === null) return Date.now() / 1000;
      return serverSec + (Date.now() / 1000 - clientAt);
    },
  };
}
