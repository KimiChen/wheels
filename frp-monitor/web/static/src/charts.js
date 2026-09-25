// SPDX-License-Identifier: Apache-2.0
// SVG 折线趋势图：无图表库，DOM API 构建（CSP 友好，无 innerHTML / 内联样式）。
// null 为断线缺口：连续的 null 段不连线；孤立单点以小圆点呈现。
// 颜色只用 app.css 中的 fm-line-* / fm-dot-* 类引用套件令牌，不新增颜色值。

const SVG_NS = "http://www.w3.org/2000/svg";

const WIDTH = 640;
const HEIGHT = 160;
const PAD_X = 4;
const PAD_Y = 10;

/** 历史序列值：数字或十进制字符串 → Number；null / 非有限 → null（缺口）。 */
function toChartValue(value) {
  if (value === null || value === undefined) return null;
  const n = typeof value === "string" ? Number(value.trim()) : value;
  return Number.isFinite(n) ? n : null;
}

/** 序列摘要：当前（最后一个非 null）/ 均值 / 峰值，均无有效样本时为 null。 */
export function summarize(values) {
  let current = null;
  let max = null;
  let sum = 0;
  let count = 0;
  for (const raw of values) {
    const v = toChartValue(raw);
    if (v === null) continue;
    current = v;
    sum += v;
    count += 1;
    if (max === null || v > max) max = v;
  }
  return { current, max, mean: count > 0 ? sum / count : null, samples: count };
}

function svgEl(tag, attrs) {
  const el = document.createElementNS(SVG_NS, tag);
  for (const [name, value] of Object.entries(attrs)) {
    el.setAttribute(name, String(value));
  }
  return el;
}

function xAt(index, length) {
  if (length <= 1) return WIDTH / 2;
  return PAD_X + (index * (WIDTH - 2 * PAD_X)) / (length - 1);
}

function yAt(value, min, max) {
  const span = max - min || 1;
  const ratio = (value - min) / span;
  return HEIGHT - PAD_Y - ratio * (HEIGHT - 2 * PAD_Y);
}

/**
 * 构建一张趋势卡片。
 * @param {object} spec
 * @param {string} spec.title 卡片标题
 * @param {number} spec.t0 起始 Unix 秒
 * @param {number} spec.step 步长秒
 * @param {Array<{key:string,label:string,values:Array,className:string,dotClassName:string}>} spec.lines
 * @param {(v:number)=>string} spec.fmtValue 数值格式化（摘要与纵轴）
 * @param {(ts:number)=>string} spec.fmtTime 横轴时间格式化
 * @returns {HTMLElement}
 */
export function buildChartCard({ title, t0, step, lines, fmtValue, fmtTime }) {
  const card = document.createElement("div");
  card.className = "wsk-panel fm-chart";

  const head = document.createElement("p");
  head.className = "wsk-panel-title";
  head.textContent = title;
  card.append(head);

  const length = Math.max(0, ...lines.map((line) => line.values?.length ?? 0));
  const parsed = lines.map((line) =>
    (Array.isArray(line.values) ? line.values : []).map(toChartValue),
  );
  const all = parsed.flat().filter((v) => v !== null);

  const plot = document.createElement("div");
  plot.className = "fm-chart-plot";
  card.append(plot);

  if (all.length === 0 || length === 0) {
    const empty = document.createElement("p");
    empty.className = "fm-chart-nodata";
    empty.textContent = "该时间范围内暂无有效样本。";
    plot.append(empty);
  } else {
    let min = Math.min(...all);
    let max = Math.max(...all);
    if (min === max) {
      // 平线扩展上下界，避免除零并把线画在垂直居中位置。
      const pad = Math.abs(min) > 0 ? Math.abs(min) * 0.05 : 1;
      min -= pad;
      max += pad;
    }

    const yAxis = document.createElement("div");
    yAxis.className = "fm-chart-y";
    const yMax = document.createElement("span");
    yMax.textContent = fmtValue(max);
    const yMin = document.createElement("span");
    yMin.textContent = fmtValue(min);
    yAxis.append(yMax, yMin);
    plot.append(yAxis);

    const svg = svgEl("svg", {
      viewBox: `0 0 ${WIDTH} ${HEIGHT}`,
      role: "img",
      "aria-label": `${title}折线趋势图，各序列当前 / 均值 / 峰值见下方图例`,
    });
    svg.append(
      svgEl("line", {
        x1: 0,
        x2: WIDTH,
        y1: yAt(max, min, max),
        y2: yAt(max, min, max),
        class: "fm-chart-gridline",
      }),
      svgEl("line", {
        x1: 0,
        x2: WIDTH,
        y1: yAt(min, min, max),
        y2: yAt(min, min, max),
        class: "fm-chart-gridline",
      }),
    );

    lines.forEach((line, lineIndex) => {
      const values = parsed[lineIndex];
      // 按非 null 连续段切分；null 缺口不连线。
      let run = [];
      const flush = () => {
        if (run.length === 1) {
          const [point] = run;
          svg.append(
            svgEl("circle", {
              cx: point.x,
              cy: point.y,
              r: 2.5,
              class: `${line.className} fm-chart-dot`,
            }),
          );
        } else if (run.length > 1) {
          const d = run
            .map((point, i) => `${i === 0 ? "M" : "L"}${point.x.toFixed(1)} ${point.y.toFixed(1)}`)
            .join(" ");
          svg.append(svgEl("path", { d, class: line.className }));
        }
        run = [];
      };
      values.forEach((value, index) => {
        if (value === null) {
          flush();
          return;
        }
        run.push({ x: xAt(index, length), y: yAt(value, min, max) });
      });
      flush();
    });
    plot.append(svg);

    const xAxis = document.createElement("div");
    xAxis.className = "fm-chart-x";
    const start = document.createElement("span");
    start.textContent = fmtTime(t0);
    const end = document.createElement("span");
    end.textContent = fmtTime(t0 + (length - 1) * step);
    xAxis.append(start, end);
    card.append(xAxis);
  }

  const legend = document.createElement("ul");
  legend.className = "fm-chart-legend";
  for (const line of lines) {
    const summary = summarize(line.values ?? []);
    const item = document.createElement("li");
    const dot = document.createElement("span");
    dot.className = `fm-legend-dot ${line.dotClassName}`;
    dot.setAttribute("aria-hidden", "true");
    const text = document.createElement("span");
    const fmt = (v) => (v === null ? "未知" : fmtValue(v));
    text.textContent =
      `${line.label} · 当前 ${fmt(summary.current)} · ` +
      `均值 ${fmt(summary.mean)} · 峰值 ${fmt(summary.max)}`;
    item.append(dot, text);
    legend.append(item);
  }
  card.append(legend);

  return card;
}
