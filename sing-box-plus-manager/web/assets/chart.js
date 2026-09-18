// proxy-manager 控制台 · 趋势图
//
// vendored uPlot（`vendor/`，MIT，随二进制发布，运行时不走 CDN）。
//
// 本文件是**整个前端唯一允许把字节转成浮点的地方**，而且只有一处：
// `toPixelScale`。理由是几何上绕不开——canvas 的坐标就是浮点。
// 但它的结果**只用来决定画到哪一像素**，绝不参与任何对外显示的数字：
// tooltip、图例、明细表一律回查服务端返回的十进制字符串。
// `tests/m4/assets.rs` 机械断言这个文件里恰好只有一处转换，且必须叫这个名字。

// 同 api.js：全文件包在 IIFE 里，不留任何全局。
// 第一版没包，`const SERIES` 与 app.js 里的同名常量直接撞出 SyntaxError，
// 整个文件一行都没执行——图是空的，而控制台之外看不出任何异常。

(function () {
  "use strict";

  // —— 唯一的浮点出口 ————————————————————————————————————————————
  //
  // u64 上界附近会丢精度，这是**可以接受的**：720 像素宽的图上，
  // 一个字节的差别对应不到 10^-17 个像素。不可接受的是让这个值流回文案里，
  // 所以它的返回值从不进 DOM——进 DOM 的是 `exact[]` 里的原始字符串。
  function toPixelScale(decimalText) {
    return Number(decimalText);
  }

  // —— 主题配色 ————————————————————————————————————————————————
  //
  // 深浅切换要**重建**图表配色（README §4.11）。uPlot 的 stroke/fill 在构造时
  // 就烘进了绘制路径，改 CSS 变量不会让已经画好的线变色——
  // 这是「改完主题图还是旧颜色」那类问题的根因，所以下面直接销毁重建。
  // kit 的颜色变量是 CSS 的 `light-dark(a, b)`。
  // **`getPropertyValue` 拿到的是没有解析过的原始 token**——
  // 把 `light-dark(#4f46e5, #818cf8)` 交给 canvas 的 strokeStyle，
  // 它既不报错也不画线，图就是一片空白。第一版就是这么空的。
  //
  // 想拿到真正的颜色，必须让浏览器在**真实的层叠上下文里**算一次：
  // 挂一个探针元素，把 `color` 设成那个变量，再读回计算后的 `color`。
  function resolveColor(token, fallback) {
    const probe = document.createElement("span");
    probe.style.cssText = `position:absolute;visibility:hidden;color:var(${token},${fallback})`;
    document.body.append(probe);
    const resolved = getComputedStyle(probe).color;
    probe.remove();
    return resolved || fallback;
  }

  function palette() {
    const token = (name, fallback) => resolveColor(name, fallback);
    return {
      axis: token("--muted", "#888"),
      grid: token("--border", "#ddd"),
      series: [
        token("--accent", "#3b82f6"),
        token("--info", "#0ea5e9"),
        token("--success", "#22c55e"),
        token("--warning", "#f59e0b"),
      ],
    };
  }

  // 桶是 **UTC 整点**（页面上也是这么写的），所以刻度必须按 UTC 排版。
  // uPlot 默认用浏览器本地时区且是 12 小时制——那会让「UTC 整点桶」这句话
  // 与轴上的数字对不上，而且对不上的方式取决于看的人在哪个时区。
  function utcTick(epochSeconds) {
    const iso = new Date(epochSeconds * 1000).toISOString();
    return `${iso.slice(5, 10)} ${iso.slice(11, 16)}`;
  }

  const SERIES = [
    ["TCP 上行", "tcp_uplink_bytes"],
    ["TCP 下行", "tcp_downlink_bytes"],
    ["UDP 上行", "udp_uplink_bytes"],
    ["UDP 下行", "udp_downlink_bytes"],
  ];

  /// 一个图表实例连同它当时的数据。
  ///
  /// **个人视图与管理员视图各自持有一个**，不共用全局单例：
  /// 共用的话切换视图会把另一张图的数据画进来，而且没有任何报错。
  class TrendChart {
    constructor(host) {
      this.host = host;
      this.plot = null;
      this.exact = [];
      this.payload = null;

      // 面板从隐藏变可见、窗口改宽，都要重新量尺寸。
      // 用 ResizeObserver 而不是挂 tab 的点击事件：后者只覆盖一种成因，
      // 而「canvas 宽度为 0」这件事对所有成因表现完全一样——一张空白图。
      this.observer = new ResizeObserver(() => this.resize());
      this.observer.observe(host);

      // 主题变化：kit 把选择写在 <html data-theme>，跟随系统时该属性不存在，
      // 所以两个来源都要听。
      this.themeWatcher = new MutationObserver(() => this.rebuild());
      this.themeWatcher.observe(document.documentElement, {
        attributes: true,
        attributeFilter: ["data-theme"],
      });
      this.media = window.matchMedia("(prefers-color-scheme: dark)");
      this.media.addEventListener("change", () => this.rebuild());
    }

    /// 画一份新数据。`payload` 是 `/api/v1/usage/trend` 的原始响应。
    draw(payload) {
      this.payload = payload;
      const points = payload.points ?? [];
      // 精确值按「系列 → 下标」存成字符串，tooltip 只从这里取。
      this.exact = SERIES.map(([, field]) => points.map((p) => p[field]));
      this.rebuild();
    }

    rebuild() {
      if (!this.payload) return;
      const points = this.payload.points ?? [];
      const colors = palette();
      const xs = points.map((p) => Date.parse(p.bucket) / 1000);
      const data = [xs, ...SERIES.map(([, field]) => points.map((p) => toPixelScale(p[field])))];

      this.plot?.destroy();
      this.plot = new window.uPlot(
        {
          width: this.host.clientWidth || 720,
          height: 260,
          // 图例即 tooltip：uPlot 把 `value` 的返回值放进图例，
          // 游标移动时按下标回调。**这里回查 exact[]，不用上面那个浮点。**
          series: [
            {
              label: "时间（UTC）",
              // 图例里的时间也回查原始桶键，不让它经过一次本地时区换算。
              value: (_self, _raw, _si, idx) => points[idx]?.bucket ?? "—",
            },
            ...SERIES.map(([label], i) => ({
              label,
              stroke: colors.series[i],
              width: 2,
              value: (_self, _raw, seriesIdx, idx) => this.exactText(seriesIdx - 1, idx),
            })),
          ],
          axes: [
            {
              stroke: colors.axis,
              grid: { stroke: colors.grid },
              values: (_self, splits) => splits.map(utcTick),
            },
            {
              stroke: colors.axis,
              grid: { stroke: colors.grid },
              // 字节刻度比 uPlot 默认的数字宽得多。不显式留够宽度，
              // 标签会被左边界裁掉——`1,033 MiB` 显示成 `33 MiB`，
              // 于是一串刻度看起来不再单调递增。图没画错，读的人会读错。
              size: 86,
              // y 轴刻度是量级，允许是近似值——它本来就是「大概多少」。
              values: (_self, ticks) => ticks.map((t) => window.pm.formatBytes(String(Math.round(t)))),
            },
          ],
          cursor: { drag: { x: false, y: false } },
        },
        data,
        this.host
      );
    }

    /// 图例/tooltip 的文案。**只从服务端返回的十进制字符串来**。
    exactText(seriesIdx, idx) {
      const column = this.exact[seriesIdx];
      if (!column || idx == null || column[idx] == null) return "—";
      const raw = column[idx];
      return `${window.pm.formatBytes(raw)}（${window.pm.groupDigits(raw)} 字节）`;
    }

    resize() {
      const width = this.host.clientWidth;
      // 宽度为 0 说明面板还藏着；这时候 setSize 会把图压成一条线，
      // 而且之后再显示出来也不会自己恢复。
      if (this.plot && width > 0) this.plot.setSize({ width, height: 260 });
    }
  }

  // —— 页面接线 ————————————————————————————————————————————————

  let chart = null;

  function currentFilters() {
    const checked = (name) => document.querySelector(`input[name="${name}"]:checked`)?.value;
    return {
      range: checked("range") ?? "24h",
      grain: checked("grain") ?? "hour",
      node_id: document.querySelector("#usage-node")?.value ?? "",
    };
  }

  async function reload() {
    const filters = currentFilters();
    const params = new URLSearchParams({ range: filters.range, grain: filters.grain });
    if (filters.node_id) params.set("node_id", filters.node_id);

    // **请求序号门禁**：筛选器变化会并发发请求，晚到的旧响应不得覆盖新选择。
    // 没有这道门禁，快速切两次筛选器就可能停在第一次的结果上，
    // 而界面上显示的是第二次的筛选条件——图和筛选器互相矛盾，且不报错。
    await window.pm.guarded(
      "usage-trend",
      () => window.pm.getJson(`/usage/trend?${params}`),
      (payload) => {
        chart?.draw(payload);
        renderDetail(payload);
        setNotice(null);
      }
    );
  }

  function renderDetail(payload) {
    const body = document.querySelector("[data-pm-collection='usage-detail'] tbody");
    const template = body?.querySelector("[data-pm-template]");
    if (!template) return;
    for (const child of Array.from(body.children)) {
      if (child !== template && !child.hasAttribute("data-table-empty")) child.remove();
    }
    // 明细只列**有流量**的桶：空桶对图很重要（不然中间那段会被压没），
    // 但在表里是 700 行 0，把真正要看的东西埋掉。
    const rows = (payload.points ?? []).filter((p) => p.total_bytes !== "0").reverse();
    const empty = body.querySelector("[data-table-empty]");
    if (empty) empty.hidden = rows.length > 0;
    for (const point of rows) {
      const clone = template.content.firstElementChild.cloneNode(true);
      clone.setAttribute("data-pm-row", "");
      window.pm.applyBindings(clone, point);
      body.insertBefore(clone, template);
    }
    window.wsk?.mount?.(body);
  }

  function setNotice(text) {
    const notice = document.querySelector("[data-pm-chart-notice]");
    if (!notice) return;
    notice.hidden = !text;
    if (text) notice.textContent = text;
  }

  function boot() {
    const host = document.querySelector("[data-pm-chart]");
    if (!host || !window.uPlot || !window.pm) return;
    chart = new TrendChart(host);

    // 筛选器：range / grain / node 任何一个变化都重新取数。
    document.addEventListener("change", (event) => {
      if (event.target.closest('[name="range"], [name="grain"], #usage-node')) {
        reload().catch((error) => setNotice(`加载失败：${error.message}`));
      }
    });
    reload().catch((error) => setNotice(`加载失败：${error.message}`));
  }

  // 等 api.js 把节点下拉框填好再首次取数——它填完会发这个事件。
  window.addEventListener("pm:usage-ready", boot, { once: true });

})();
