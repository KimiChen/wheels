// SPDX-License-Identifier: Apache-2.0
// 节点详情趋势区块：range 切换（1h/6h/24h/7d）+ 分组 SVG 折线。
// 数据来自 metrics 历史接口；{enabled:false} 表示服务端未配置数据目录，
// 显示「历史未开启」说明而非报错。实时当前值仍由 SSE 负责，本模块不轮询。

import { buildChartCard } from "../charts.js";
import { fmtBytes, fmtCount, fmtCpu, fmtRate } from "../format.js";

export const TREND_RANGES = ["1h", "6h", "24h", "7d"];

const LINE_CLASSES = [
  { line: "fm-line-1", dot: "fm-dot-1" },
  { line: "fm-line-2", dot: "fm-dot-2" },
  { line: "fm-line-3", dot: "fm-dot-3" },
  { line: "fm-line-4", dot: "fm-dot-4" },
];

const fmtLoad = (v) => v.toFixed(2);

/** 图表分组：标题 / 序列键 / 值格式化。 */
const GROUPS = [
  {
    key: "cpu",
    title: "CPU 使用率",
    fmt: fmtCpu,
    lines: [{ key: "cpu", label: "CPU" }],
  },
  {
    key: "mem",
    title: "内存 / 交换分区",
    fmt: fmtBytes,
    lines: [
      { key: "mem_used_bytes", label: "内存已用" },
      { key: "swap_used_bytes", label: "交换分区已用" },
    ],
  },
  {
    key: "disk",
    title: "磁盘",
    fmt: fmtBytes,
    lines: [{ key: "disk_used_bytes", label: "磁盘已用" }],
  },
  {
    key: "net",
    title: "网络速率（节点网卡）",
    fmt: fmtRate,
    lines: [
      { key: "net_rx_bps", label: "接收" },
      { key: "net_tx_bps", label: "发送" },
    ],
  },
  {
    key: "load",
    title: "负载",
    fmt: fmtLoad,
    lines: [
      { key: "load1", label: "1 分钟" },
      { key: "load5", label: "5 分钟" },
      { key: "load15", label: "15 分钟" },
    ],
  },
  {
    key: "sys",
    title: "连接与进程",
    fmt: fmtCount,
    lines: [
      { key: "tcp", label: "TCP" },
      { key: "udp", label: "UDP" },
      { key: "procs", label: "进程数" },
    ],
  },
];

function fmtAxisTime(ts, range) {
  const d = new Date(ts * 1000);
  const pad = (n) => String(n).padStart(2, "0");
  const hm = `${pad(d.getHours())}:${pad(d.getMinutes())}`;
  if (range === "7d") return `${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${hm}`;
  return hm;
}

/**
 * 趋势区块控制器。root 内需包含：
 * [data-trend-range] 切换按钮、[data-trend-meta] 状态文本、[data-trend-grid]
 * 卡片容器，以及 hidden 的 [data-trend-loading] / [data-trend-empty] /
 * [data-trend-error]（内含 [data-trend-retry] 按钮）。
 * @param {(nodeId:string, range:string)=>Promise<object>} fetchMetrics
 */
export function createTrends({ root, fetchMetrics }) {
  const grid = root.querySelector("[data-trend-grid]");
  const meta = root.querySelector("[data-trend-meta]");
  const loadingEl = root.querySelector("[data-trend-loading]");
  const emptyEl = root.querySelector("[data-trend-empty]");
  const errorEl = root.querySelector("[data-trend-error]");
  const buttons = [...root.querySelectorAll("[data-trend-range]")];

  const state = {
    nodeId: null,
    range: "1h",
    loadedKey: null,
    errorKey: null,
    errorAt: 0,
    token: 0,
  };

  function setMeta(text) {
    if (meta) meta.textContent = text;
  }

  function showOnly(which) {
    if (loadingEl) loadingEl.hidden = which !== "loading";
    if (emptyEl) emptyEl.hidden = which !== "empty";
    if (errorEl) errorEl.hidden = which !== "error";
  }

  function setRange(range) {
    state.range = range;
    for (const button of buttons) {
      const current = button.dataset.trendRange === range;
      button.classList.toggle("wsk-is-active", current);
      button.setAttribute("aria-pressed", String(current));
    }
  }

  function render(data) {
    const series = data?.series ?? {};
    grid.replaceChildren(
      ...GROUPS.map((group) =>
        buildChartCard({
          title: group.title,
          t0: data.t0,
          step: data.step,
          fmtValue: group.fmt,
          fmtTime: (ts) => fmtAxisTime(ts, data.range),
          lines: group.lines.map((line, index) => ({
            ...line,
            values: series[line.key],
            className: LINE_CLASSES[index % LINE_CLASSES.length].line,
            dotClassName: LINE_CLASSES[index % LINE_CLASSES.length].dot,
          })),
        }),
      ),
    );
    showOnly(null);
    setMeta("");
  }

  async function load() {
    if (!state.nodeId) return;
    const token = ++state.token;
    const key = `${state.nodeId}:${state.range}`;
    showOnly("loading");
    setMeta("历史数据加载中…");
    try {
      const data = await fetchMetrics(state.nodeId, state.range);
      if (token !== state.token) return; // 已切节点或范围，丢弃过期响应
      if (data?.enabled === false) {
        grid.replaceChildren();
        showOnly("empty");
        setMeta("");
        state.loadedKey = key;
        state.errorKey = null;
        return;
      }
      render(data);
      state.loadedKey = key;
      state.errorKey = null;
    } catch (error) {
      if (token !== state.token) return;
      grid.replaceChildren();
      showOnly("error");
      setMeta("");
      state.loadedKey = null;
      state.errorKey = key;
      state.errorAt = Date.now();
      if (error?.status !== 401) {
        globalThis.wsk?.showToast("历史数据加载失败，可点击重试。", "warning");
      }
    }
  }

  buttons.forEach((button) => {
    button.addEventListener("click", () => {
      const range = button.dataset.trendRange;
      if (!TREND_RANGES.includes(range) || range === state.range) return;
      setRange(range);
      void load();
    });
  });
  root.querySelector("[data-trend-retry]")?.addEventListener("click", () => {
    state.errorKey = null;
    void load();
  });

  setRange(state.range);

  return {
    /**
     * 选中节点后调用；同节点同范围不重复请求（SSE 每次增量都会走到这里）。
     * 加载失败后 30 秒内不自动重试，避免 SSE 高频增量触发连续失败请求；
     * 用户可点「重试」按钮立即再试。
     */
    show(nodeId) {
      if (!nodeId) return;
      state.nodeId = nodeId;
      const key = `${nodeId}:${state.range}`;
      if (state.loadedKey === key) return;
      if (state.errorKey === key && Date.now() - state.errorAt < 30000) return;
      void load();
    },
    /** 管理端退出登录等场景：清空并复位，避免展示上一个节点的历史。 */
    hide() {
      state.nodeId = null;
      state.loadedKey = null;
      state.errorKey = null;
      state.token += 1;
      grid.replaceChildren();
      showOnly(null);
      setMeta("");
    },
  };
}
