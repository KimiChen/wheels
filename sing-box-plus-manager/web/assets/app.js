// proxy-manager 控制台 · 原型脚本
//
// 侧栏导航、两种角色的原型分流和趋势图。
// 业务逻辑一概没有——数据全是假的。

// 包在 IIFE 里：本文件的 `start` 与 kit 的 `start` 同名，
// 顶层函数声明会互相覆盖全局绑定。

(function () {
  "use strict";

  const body = document.body;
  const PAGE = location.pathname.split("/").pop() || "overview.html";

  /* ----------------------------------------------------------- 侧栏选中态 */

  // kit 的 syncReferenceNav 是按 hash 定位的：本原型改成真实页面后没有 hash，
  // 它会把所有 wsk-active 清掉。所以选中态在这里按路径重设。
  // app.js 是 defer 且排在 kit 之后，mount() 已经跑完。
  function markActiveNav() {
    document.querySelectorAll(".wsk-reference-nav a").forEach((link) => {
      const current = link.getAttribute("href") === PAGE;
      link.classList.toggle("wsk-active", current);
      if (current) link.setAttribute("aria-current", "page");
      else link.removeAttribute("aria-current");
    });
  }
  window.addEventListener("hashchange", markActiveNav);

  /* --------------------------------------------------------- 主体与分流 */

  // 仅管理员和普通用户。普通用户登录后由服务端分配默认节点组；
  // 这里仅预览结果，不执行认证、身份分配或真实节点授权。
  const HOME = {
    admin: "overview.html",
    user: "me.html",
  };

  const STORAGE_KEY = "proxy-manager-prototype-principal";

  // 主控服务页面时会在 `<body>` 上盖 `data-pm-mode="live"`，并把**这一次求值出的角色**
  // 一起写进 `data-principal`。live 模式下本文件不再碰身份：
  // localStorage 里存的是原型里点着玩留下的偏好，让它去决定真实会话的导航
  // 会把管理员弹到个人页——而且是在任何请求回来之前就弹走。
  const LIVE = document.body.dataset.pmMode === "live";

  // 某个页面对当前主体是否可见——直接读导航项上的 data-roles，不维护第二份表。
  // 两份表一定会漂移，这是本项目自己的纪律（见 docs/integration-contract.md）。
  function pageAllowed(page, principal) {
    if (page === "login.html") return true;
    const link = document.querySelector(`.wsk-reference-nav a[href="${page}"]`);
    if (!link) return false;
    return (link.dataset.roles || "").split(/\s+/).includes(principal);
  }

  function readPrincipal() {
    if (LIVE) return Object.hasOwn(HOME, body.dataset.principal) ? body.dataset.principal : "user";
    try {
      const saved = localStorage.getItem(STORAGE_KEY);
      if (Object.hasOwn(HOME, saved)) return saved;
      // 迁移旧版演示偏好；这不是生产权限迁移。
      if (["owner", "operator", "auditor", "viewer"].includes(saved)) return "admin";
      if (["member", "none"].includes(saved)) return "user";
    } catch {
      // 隐私模式或站点数据被清空：用页面上的默认值即可。
    }
    return Object.hasOwn(HOME, body.dataset.principal) ? body.dataset.principal : "user";
  }

  function applyPrincipal(principal, { navigate = true } = {}) {
    if (!Object.hasOwn(HOME, principal)) principal = "user";
    body.dataset.principal = principal;
    if (!LIVE) {
      try {
        localStorage.setItem(STORAGE_KEY, principal);
      } catch {
        // 存不下就只在本页生效，不影响别的。
      }
    }
    const select = document.querySelector("[data-principal-select]");
    if (select && select.value !== principal) select.value = principal;

    // 停在一个当前主体看不到的页面上比跳走更糟：导航是空的，内容却还在。
    // 这在原型里等价于服务端的每请求重新求值（D13）——真实实现里越权请求
    // 根本不会返回页面内容，而不是「发过来再藏起来」。
    if (navigate && !pageAllowed(PAGE, principal)) {
      location.replace(HOME[principal]);
    }
  }

  document.addEventListener("change", (event) => {
    const select = event.target.closest("[data-principal-select]");
    if (select) applyPrincipal(select.value);
  });

  document.addEventListener("click", (event) => {
    const preview = event.target.closest("[data-demo-principal]");
    if (!preview) return;
    event.preventDefault();
    const principal = preview.dataset.demoPrincipal;
    if (!Object.hasOwn(HOME, principal)) return;
    applyPrincipal(principal, { navigate: false });
    location.assign(HOME[principal]);
  });

  /* ----------------------------------------------------------- 响应式导航 */

  const NAV_ICONS = {
    "me.html": "M9 15l6-6M8 17l-1 1a4 4 0 01-6-6l4-4a4 4 0 015 0M16 7l1-1a4 4 0 016 6l-4 4a4 4 0 01-5 0",
    "me-usage.html": "M4 19V9m8 10V5m8 14v-7",
    "me-audit.html": "M2 12h4l3 8 4-16 3 8h6",
    "overview.html": "M3 3h7v7H3zM14 3h7v7h-7zM3 14h7v7H3zM14 14h7v7h-7z",
    "nodes.html": "M3 4h18v6H3zM3 14h18v6H3zM7 7h.01M7 17h.01",
    "alerts.html": "M18 8a6 6 0 00-12 0c0 7-3 7-3 9h18c0-2-3-2-3-9M10 21h4",
    "usage.html": "M3 3v18h18M6 15l4-5 4 3 6-7",
    "users.html": "M16 21v-2a4 4 0 00-4-4H6a4 4 0 00-4 4v2M16 4a4 4 0 010 8M22 21v-2a4 4 0 00-3-4M13 7a4 4 0 11-8 0 4 4 0 018 0",
    "settings.html": "M4 7h16M4 17h16M8 4v6M16 14v6",
  };

  function mountNavigation() {
    const disclosure = document.querySelector(".pm-nav-disclosure");
    if (!disclosure) return;
    const breakpoint = matchMedia("(max-width: 960px)");
    const sync = () => { disclosure.open = !breakpoint.matches; };
    sync();
    breakpoint.addEventListener("change", sync);
    disclosure.addEventListener("keydown", (event) => {
      if (event.key === "Escape" && breakpoint.matches && disclosure.open) {
        disclosure.open = false;
        disclosure.querySelector("summary").focus();
      }
    });
    document.querySelector(".pm-nav-current").textContent = document.querySelector("h1").textContent;
    document.querySelectorAll(".wsk-reference-nav a").forEach((link) => {
      const path = NAV_ICONS[link.getAttribute("href")];
      if (!path) return;
      const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
      svg.setAttribute("class", "wsk-icon pm-nav-icon");
      svg.setAttribute("aria-hidden", "true");
      svg.setAttribute("viewBox", "0 0 24 24");
      const shape = document.createElementNS(svg.namespaceURI, "path");
      shape.setAttribute("d", path);
      svg.append(shape);
      link.prepend(svg);
    });
  }

  /* ------------------------------------------------------------------- 图表 */

  // 字节一律以十进制字符串到达前端（C3）。这里刻意用 BigInt 解析、只在最后一步
  // 为了算 SVG 坐标才转成 Number——也就是只有「画到哪一像素」这一件事用浮点，
  // 任何会被人读到的数字都不经过 Number。
  const SERIES = {
    上行: [
      "8402117", "9113880", "12884901", "10223117", "7884210", "6113880",
      "9402118", "14002776", "16884210", "12113880", "9884210", "8402117",
    ],
    下行: [
      "91003882", "104223117", "140223117", "118884210", "88402117", "70884210",
      "99402118", "150223117", "171884210", "131113880", "104884210", "91003882",
    ],
  };

  function drawChart(svg) {
    if (!svg) return;

    const W = 720;
    const H = 220;
    const PAD = { top: 16, right: 12, bottom: 28, left: 12 };

    // 峰值用 BigInt 求，避免大数在比较时被转成浮点。
    let peak = 0n;
    for (const values of Object.values(SERIES)) {
      for (const v of values) {
        const n = BigInt(v);
        if (n > peak) peak = n;
      }
    }
    if (peak === 0n) return;

    const plotW = W - PAD.left - PAD.right;
    const plotH = H - PAD.top - PAD.bottom;

    // 千分之一精度的定点缩放：先放大再整除，最后一次性转 Number 得到比例。
    const ratio = (value) => Number((BigInt(value) * 1000n) / peak) / 1000;

    const path = (values) =>
      values
        .map((v, i) => {
          const x = PAD.left + (plotW * i) / (values.length - 1);
          const y = PAD.top + plotH * (1 - ratio(v));
          return `${i === 0 ? "M" : "L"}${x.toFixed(1)} ${y.toFixed(1)}`;
        })
        .join(" ");

    const colors = { 上行: "var(--info)", 下行: "var(--accent)" };
    const parts = [
      `<line x1="${PAD.left}" y1="${PAD.top + plotH}" x2="${W - PAD.right}" y2="${
        PAD.top + plotH
      }" stroke="var(--border)" stroke-width="1" />`,
    ];

    for (const fraction of [0, 0.25, 0.5, 0.75]) {
      const y = PAD.top + plotH * fraction;
      parts.push(`<line x1="${PAD.left}" y1="${y}" x2="${W - PAD.right}" y2="${y}" stroke="var(--border)" stroke-dasharray="3 5" />`);
    }
    const gradientId = `${svg.id}-fill`;
    parts.push(`<defs><linearGradient id="${gradientId}" x1="0" x2="0" y1="0" y2="1"><stop offset="0%" stop-color="var(--accent)" stop-opacity=".12"/><stop offset="100%" stop-color="var(--accent)" stop-opacity="0"/></linearGradient></defs>`);
    parts.push(`<path d="${path(SERIES.下行)} L${W - PAD.right} ${PAD.top + plotH} L${PAD.left} ${PAD.top + plotH} Z" fill="url(#${gradientId})" />`);

    for (const [name, values] of Object.entries(SERIES)) {
      parts.push(
        `<path d="${path(values)}" fill="none" stroke="${colors[name]}" stroke-width="2" stroke-linejoin="round" stroke-linecap="round" />`,
      );
    }

    let lx = PAD.left;
    for (const name of Object.keys(SERIES)) {
      parts.push(
        `<rect x="${lx}" y="${H - 16}" width="10" height="3" rx="1.5" fill="${colors[name]}" />`,
        `<text x="${lx + 16}" y="${H - 10}" font-size="12" fill="var(--muted)">${name}</text>`,
      );
      lx += 72;
    }

    svg.innerHTML = parts.join("");
  }

  /* -------------------------------------------------------------------- 启动 */

  function start() {
    markActiveNav();
    if (LIVE) {
      // 身份由服务端决定，页面上不能再留一个看起来能切换身份的控件——
      // 它切不动任何东西，只会让人以为自己切过了。
      document.querySelectorAll(".pm-principal-switch, [data-demo-principal]").forEach((node) => {
        node.remove();
      });
      // 规则引擎属于 M5。原型里的告警条目是编的文案，live 模式必须说清楚，
      // 否则「没有新告警」会被读成「系统正常」，而真相是「还没有人在报警」。
      document.querySelectorAll("[data-pm-prototype]").forEach((node) => {
        const note = document.createElement("p");
        note.className = "wsk-help";
        note.textContent = `尚未启用：${node.dataset.pmPrototype}`;
        node.replaceWith(note);
      });
    }
    if (PAGE !== "login.html") applyPrincipal(readPrincipal());
    mountNavigation();
    // 主题切换改的是 CSS 变量，SVG 用 var(...) 自动跟随，不需要重绘。
    // 换成真的 canvas 图表库时这条就不成立了，那时必须重建配色。
    drawChart(document.getElementById("usage-chart-svg"));
    drawChart(document.getElementById("me-chart-svg"));
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", start, { once: true });
  } else {
    start();
  }

})();
