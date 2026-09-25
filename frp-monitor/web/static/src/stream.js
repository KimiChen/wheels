// SPDX-License-Identifier: Apache-2.0
// SSE 封装：首个事件 snapshot 全量、之后 node 增量、心跳为 ": ping" 注释行；
// tunnels 事件在隧道状态变化时推送全量隧道列表（公开 / 管理各自裁剪）。
// 断线后由本模块指数退避重连（带抖动）；重连成功服务端会重新下发 snapshot，
// 页面据此全量刷新。服务端返回非 SSE 响应（如 401）时浏览器放弃重连，
// 此时上报 failed，由调用方决定后续动作（如回到登录页）。

export const STREAM_STATE = {
  CONNECTING: "connecting",
  CONNECTED: "connected",
  RECONNECTING: "reconnecting",
  FAILED: "failed",
};

export function createStream(
  url,
  {
    onSnapshot,
    onNode,
    onTunnels,
    onStatus,
    minDelayMs = 1000,
    maxDelayMs = 30000,
  } = {},
) {
  let source = null;
  let attempt = 0;
  let timer = 0;
  let closed = false;

  const emitStatus = (state, extra = {}) => {
    if (typeof onStatus === "function") onStatus({ state, attempt, ...extra });
  };

  const parse = (raw) => {
    try {
      return JSON.parse(raw);
    } catch {
      return null;
    }
  };

  function schedule() {
    const base = Math.min(maxDelayMs, minDelayMs * 2 ** attempt);
    const delay = base + Math.floor(Math.random() * minDelayMs);
    attempt += 1;
    emitStatus(STREAM_STATE.RECONNECTING, { delayMs: delay });
    timer = setTimeout(connect, delay);
  }

  function connect() {
    if (closed) return;
    emitStatus(attempt === 0 ? STREAM_STATE.CONNECTING : STREAM_STATE.RECONNECTING);
    source = new EventSource(url);

    source.addEventListener("snapshot", (event) => {
      attempt = 0;
      emitStatus(STREAM_STATE.CONNECTED);
      const data = parse(event.data);
      if (data && typeof onSnapshot === "function") onSnapshot(data);
    });

    source.addEventListener("node", (event) => {
      const data = parse(event.data);
      if (data && typeof onNode === "function") onNode(data);
    });

    source.addEventListener("tunnels", (event) => {
      const data = parse(event.data);
      if (data && typeof onTunnels === "function") {
        onTunnels(Array.isArray(data.tunnels) ? data.tunnels : []);
      }
    });

    source.onopen = () => {
      attempt = 0;
      emitStatus(STREAM_STATE.CONNECTED);
    };

    source.onerror = () => {
      // 浏览器在收到非 200 / 非 text/event-stream 响应时置 CLOSED 并停止
      // 自带重连；其余情况它处于 CONNECTING 并会自行重试，这里统一接管，
      // 关掉后按自己的退避节奏重建，保证重连后一定先拿到最新 snapshot。
      const terminal = source.readyState === EventSource.CLOSED;
      source.close();
      if (closed) return;
      if (terminal) {
        emitStatus(STREAM_STATE.FAILED);
        return;
      }
      schedule();
    };
  }

  connect();

  return {
    close() {
      closed = true;
      clearTimeout(timer);
      if (source) source.close();
    },
  };
}
