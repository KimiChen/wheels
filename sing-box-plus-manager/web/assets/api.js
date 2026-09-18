// proxy-manager 控制台 · 真实数据层
//
// 与 app.js 的分工：app.js 管导航、主题与**原型模式**；本文件只在**真实模式**下工作。
// 模式靠一次 `GET /api/v1/me` 探测——它成功就说明页面是被主控服务出来的，
// 而不是从仓库里直接打开的静态原型。
//
// 三条纪律，都是 C3 在前端的延伸：
//
// 1. **字节一律以十进制字符串到达，用 BigInt 解析。** 全文件不出现任何把字节
//    转成浮点的调用——u64 的高位会被静默削掉，而削掉之后的数字看起来完全正常。
//    这条由 tests/m4/assets.rs 机械断言，不靠人眼。
// 2. **晚到的旧响应不得覆盖新选择。** 筛选器变化会并发发请求，
//    所以每个渲染目标带一个请求序号，序号旧的直接丢弃。
// 3. **片段替换后调用 `wsk.mount(root)`**，否则新节点是死控件。

// 全文件包在 IIFE 里，**顶层一个全局都不留**，唯一的出口是最后那句 `window.pm`。
// 不这样做的话：三个脚本（kit、app.js、本文件）共用同一个全局词法作用域，
// 同名的 `const` 直接抛 SyntaxError，而同名的 `function` 更糟——
// 它不报错，只是后加载的那个把全局绑定悄悄换掉，
// 于是 kit 内部调自己的 `renderCollection` 时拿到的是本文件的同名函数。
// 这是本轮真的踩到的：分页和集合渲染因此坏掉，页面上没有任何报错。

(function () {
  "use strict";

  const API_BASE = "/api/v1";

  // ---- 字节格式化：全程 BigInt ----

  // 顶到 EiB：u64 字节的上界约 16 EiB，止步 PiB 的话上界会显示成
  // 「16,383.99 PiB」——不算错，但没人这么读数。
  const UNITS = [
    ["EiB", 1152921504606846976n],
    ["PiB", 1125899906842624n],
    ["TiB", 1099511627776n],
    ["GiB", 1073741824n],
    ["MiB", 1048576n],
    ["KiB", 1024n],
  ];

  // 千分位分组。纯字符串操作——**不经 Number**，所以 u64 上界也不会失真。
  function groupDigits(text) {
    return String(text).replace(/\B(?=(\d{3})+(?!\d))/g, ",");
  }

  // 人读的量级。除法在 BigInt 里做，小数位用取模拼字符串。
  function formatBytes(decimalText) {
    let value;
    try {
      value = BigInt(decimalText);
    } catch {
      return "—";
    }
    for (const [unit, size] of UNITS) {
      if (value >= size) {
        const whole = value / size;
        const fraction = ((value * 100n) / size) % 100n;
        return `${groupDigits(whole)}.${String(fraction).padStart(2, "0")} ${unit}`;
      }
    }
    return `${groupDigits(value)} B`;
  }

  // 精确字节数，给「大字下面那行小字」用。§4.11 要求指标卡大字放量级，
  // 而精确值仍然要能看到——否则对账时没法用。
  function exactBytes(decimalText) {
    try {
      return `${groupDigits(BigInt(decimalText))} 字节`;
    } catch {
      return "—";
    }
  }

  // ---- 取数 ----

  async function getJson(path) {
    const response = await fetch(`${API_BASE}${path}`, {
      credentials: "same-origin",
      headers: { accept: "application/json" },
    });
    const body = await response.json().catch(() => null);
    if (!response.ok) {
      const error = new Error(body?.error?.message ?? `HTTP ${response.status}`);
      error.code = body?.error?.code ?? "unknown";
      error.status = response.status;
      throw error;
    }
    return body;
  }

  // 按点号路径取值。取不到返回 undefined，调用方决定显示什么。
  function pick(source, path) {
    return path.split(".").reduce((value, key) => (value == null ? value : value[key]), source);
  }

  // ---- 声明式绑定 ----
  //
  // 页面上用三个属性描述「这一格显示什么」，脚本里不维护第二份字段表——
  // 两份表一定会漂移，这是本项目自己的纪律。
  //
  //   data-pm="a.b"        → 文本
  //   data-pm-bytes="a.b"  → 量级（大字）
  //   data-pm-exact="a.b"  → 精确字节数（小字）

  // 表单控件要写 `.value`，其余写 `textContent`。少了这一支，
  // 绑到 `<input>` 上的值会变成一段看不见的文本节点——页面上什么都没变，
  // 而且不报错，是最难发现的那种。
  function setText(node, text) {
    if (node instanceof HTMLInputElement || node instanceof HTMLTextAreaElement) {
      node.value = text;
    } else {
      node.textContent = text;
    }
  }

  // 布尔值直接 String() 会变成 `true` / `false` 出现在界面上。
  // 它不算错，但那是 JSON 的词，不是给人看的词。
  function display(value) {
    if (value == null) return "—";
    if (typeof value === "boolean") return value ? "是" : "否";
    return String(value);
  }

  function applyBindings(root, data) {
    root.querySelectorAll("[data-pm]").forEach((node) => {
      setText(node, display(pick(data, node.dataset.pm)));
    });
    root.querySelectorAll("[data-pm-bytes]").forEach((node) => {
      const value = pick(data, node.dataset.pmBytes);
      setText(node, value == null ? "—" : formatBytes(value));
    });
    // 纯数字（千分位，无单位后缀）。整列都是字节时，每格再写一遍「字节」
    // 会把列挤到换行，而列头已经说明了单位。
    root.querySelectorAll("[data-pm-digits]").forEach((node) => {
      const value = pick(data, node.dataset.pmDigits);
      setText(node, value == null ? "—" : groupDigits(value));
    });
    root.querySelectorAll("[data-pm-exact]").forEach((node) => {
      const value = pick(data, node.dataset.pmExact);
      setText(node, value == null ? "—" : exactBytes(value));
    });
  }

  // 行渲染：把 `<template data-pm-template>` 按集合复制一遍。
  //
  // 替换完**必须** `wsk.mount(container)`，否则新行里的排序、分页、对话框
  // 都是死控件——kit 的监听器挂在 document 上，但初始渲染要靠 mount。
  function renderRows(container, items, emptyText) {
    const template = container.querySelector("[data-pm-template]");
    if (!template) return;
    const body = template.parentElement;
    // 清空**所有**既有行，不只是上一轮插进去的。页面里躺着的是原型示例行，
    // 只删 data-pm-row 的话，真实模式下真行会追加在假行后面——
    // 混排出来的表格每一格都长得像真的，是本轮最容易漏的错。
    for (const child of Array.from(body.children)) {
      if (child !== template && !child.hasAttribute("data-table-empty")) child.remove();
    }

    if (!items.length) {
      const empty = body.querySelector("[data-table-empty]");
      if (empty) {
        empty.hidden = false;
        const cell = empty.querySelector("[data-pm-empty-text]");
        if (cell && emptyText) cell.textContent = emptyText;
      }
      return;
    }
    const empty = body.querySelector("[data-table-empty]");
    if (empty) empty.hidden = true;

    for (const item of items) {
      const clone = template.content.firstElementChild.cloneNode(true);
      clone.setAttribute("data-pm-row", "");
      applyBindings(clone, item);
      body.insertBefore(clone, template);
    }
    // 片段替换之后让 kit 完成初始渲染。幂等，可重复调用。
    window.wsk?.mount?.(container);
  }

  // ---- 请求序号门禁 ----
  //
  // 筛选器变化会并发发请求。**晚到的旧响应不得覆盖新选择**——
  // 没有这道门禁，快速切两次筛选器就可能停在第一次的结果上，
  // 而界面上显示的是第二次的筛选条件。

  const sequences = new Map();

  async function guarded(key, loader, render) {
    const next = (sequences.get(key) ?? 0) + 1;
    sequences.set(key, next);
    const data = await loader();
    if (sequences.get(key) !== next) return; // 旧响应，丢弃
    render(data);
  }

  // ---- 各页面 ----

  // 每页拆成 `load`（取数）与 `render`（落到 DOM）两段——
  // 中间隔着请求序号门禁。写成一个函数的话门禁就是摆设：
  // 渲染发生在比对序号之前，晚到的旧响应照样覆盖界面。

  const PAGES = {
    "me.html": {
      // 两个端点：`/me` 给身份与额度，`/me/subscription` 给订阅地址。
      // 订阅地址单独一个端点是服务端的决定（它是凭据，不该出现在每一页的
      // `/me` 响应里），页面这边照做即可。
      load: async () => {
        const [me, sub] = await Promise.all([getJson("/me"), getJson("/me/subscription")]);
        return { me, sub };
      },
      render: ({ me, sub }) => {
        // `cycle` 整个取自 `/me`——它带 `key` 与 `remaining_bytes`，
        // 而订阅端点那份只带 `Subscription-Userinfo` 要的三个数。
        // 两份都铺一遍的话，后铺的会把 `remaining_bytes` 抹成 undefined。
        applyBindings(document, {
          ...me,
          url: sub.enabled ? sub.url : null,
          identity: sub.identity,
          entry_count: sub.enabled ? sub.entry_count : 0,
        });
        renderCollection("me-entries", sub.enabled ? sub.entries : [], "还没有为你配置入口");

        // 三种状态各说各的话。**不要合并成一句「订阅不可用」**——
        // 「服务端没配」「你还没分到身份」「加载失败」要做的事完全不同。
        toggle("[data-pm-sub-disabled]", !sub.enabled);
        toggle(".pm-subscription-address", sub.enabled);
        toggle("[data-pm-copy]", sub.enabled);
        toggle("[data-pm-no-identity]", sub.enabled && !sub.identity);
        // 徽标说的是状态，不是装饰。**没有地址就不许说「有效」**，
        // 「0 条入口可用」也不该顶着一个绿勾——那两种写法都会让人
        // 把一次真实的缺失读成「页面没加载出来」，然后反复刷新。
        toggle("[data-pm-sub-valid]", sub.enabled && !!sub.url);
        toggle("[data-pm-entry-badge]", sub.enabled && sub.entry_count > 0);
      },
    },

    "me-usage.html": {
      load: () => getJson("/me/usage"),
      render: (usage) => renderCollection("me-usage", usage.nodes, "本周期还没有入账记录"),
    },

    "overview.html": {
      load: async () => {
        const [alerts, nodes] = await Promise.all([getJson("/alerts"), getJson("/nodes")]);
        return { alerts, nodes };
      },
      render: ({ alerts, nodes }) => {
        applyBindings(document, {
          criteria: alerts.acceptance_criteria,
          counts: alerts.counts,
          nodes: { total: nodes.nodes.length },
        });
        markCriteria(alerts.acceptance_criteria);
        renderCollection("nodes", nodes.nodes, "还没有配置任何节点");
      },
    },

    "alerts.html": {
      load: () => getJson("/alerts"),
      render: (alerts) => {
        applyBindings(document, { counts: alerts.counts, criteria: alerts.acceptance_criteria });
        markCriteria(alerts.acceptance_criteria);
      },
    },

    "nodes.html": {
      load: () => getJson("/nodes"),
      render: (data) => renderCollection("nodes", data.nodes, "还没有配置任何节点"),
    },

    "users.html": {
      load: () => getJson("/users"),
      render: (data) => renderCollection("users", data.users, "还没有用户"),
    },

    "usage.html": {
      // 初次装载与筛选器变化共用同一条路径（见 chart.js），
      // 这里只负责把节点下拉框填成真实节点。
      load: () => getJson("/nodes"),
      render: (data) => {
        const select = document.querySelector("#usage-node");
        if (!select) return;
        select.textContent = "";
        const all = document.createElement("option");
        all.value = "";
        all.textContent = "全部节点";
        select.append(all);
        for (const node of data.nodes) {
          const option = document.createElement("option");
          option.value = node.node_id;
          option.textContent = node.node_id;
          select.append(option);
        }
        window.dispatchEvent(new CustomEvent("pm:usage-ready"));
      },
    },

    "settings.html": {
      load: () => getJson("/settings/quota"),
      render: (settings) => {
        applyBindings(document, settings);
        renderCollection("quota-nodes", settings.nodes, "还没有待下发的节点任务");
      },
    },
  };

  // `hidden` 而不是 `style.display`：前者是语义属性，辅助技术据此跳过，
  // 而且不会与 kit 自己的显示规则打架。
  function toggle(selector, visible) {
    document.querySelectorAll(selector).forEach((node) => {
      node.hidden = !visible;
    });
  }

  // 复制订阅地址。
  //
  // **只复制看起来像地址的东西**：原型模式下那一格是脱敏占位符，
  // 复制它会得到一串圆点，而用户会把它粘进客户端然后说「订阅坏了」。
  //
  // **失败要留一条能走的路。** `navigator.clipboard.writeText` 会在非安全上下文里
  // 根本不存在，也会在权限被拒时抛 NotAllowedError（企业策略、无痕窗口、
  // 自动化环境都会）。那时只说一句「请手动选中复制」是不够的——
  // 那一格是一串 64 位十六进制，手工拖选很容易少选一头，
  // 而少选一头得到的是一个看起来对、导进去 404 的地址。所以替他选好。
  //
  // **判据是「有没有跑在真实模式下」，不是「这串字符像不像 URL」。**
  // 第一版写的是 `text.startsWith("http")`，它挡不住原型页面里那个脱敏占位符
  // ——`https://<控制台域名>/sub/Proxy-••••.yaml` 也是以 http 开头的，
  // 于是点一下就把一串圆点复制走了，而它看起来完全像个地址。
  // 这是在浏览器里真跑一遍才发现的：读代码时那个判断看着是对的。
  document.addEventListener("click", async (event) => {
    const button = event.target.closest?.("[data-pm-copy]");
    if (!button) return;
    if (document.body.dataset.pmMode !== "live") {
      window.wsk?.showToast?.("这是原型页面，上面的地址是示例。", "warning");
      return;
    }
    const node = document.querySelector(button.dataset.pmCopy);
    const text = node?.textContent?.trim() ?? "";
    if (!/^https:\/\/[^\s<>\u2022]+$/.test(text)) {
      window.wsk?.showToast?.("还没有可复制的订阅地址。", "warning");
      return;
    }
    try {
      await navigator.clipboard.writeText(text);
      window.wsk?.showToast?.("订阅地址已复制。请勿转发给他人。");
    } catch {
      // 不把地址写进任何日志——它是凭据。
      selectText(node);
      window.wsk?.showToast?.("这台设备不允许自动复制，已替你选中，按 Ctrl/Cmd+C。", "warning");
    }
  });

  function selectText(node) {
    if (!node) return;
    const selection = window.getSelection?.();
    if (!selection) return;
    const range = document.createRange();
    range.selectNodeContents(node);
    selection.removeAllRanges();
    selection.addRange(range);
  }

  function renderCollection(name, items, emptyText) {
    const container = document.querySelector(`[data-pm-collection="${name}"]`);
    if (container) renderRows(container, items ?? [], emptyText);
  }

  // 四项判据非 0 是 P0：账本已经不可信。这里只改语义色，不自己发明文案。
  function markCriteria(criteria) {
    document.querySelectorAll("[data-pm-criteria]").forEach((node) => {
      const key = node.dataset.pmCriteria;
      const value = Number.isFinite(criteria?.[key]) ? criteria[key] : 0;
      node.classList.toggle("wsk-danger", value !== 0);
      node.classList.toggle("wsk-success", value === 0);
    });
  }

  // ---- 启动 ----

  async function start() {
    let me;
    try {
      me = await getJson("/me");
    } catch {
      // 取不到 `/me`（401、被直接从仓库打开、主控没起）→ 这是**原型模式**。
      // 让 app.js 的原型分流照旧工作，本文件从此不碰页面。
      return;
    }

    // 真实模式：角色来自服务端这一次求值，**不是页面上的选择器**。
    document.body.dataset.principal = me.role;
    document.body.dataset.pmMode = "live";
    window.dispatchEvent(new CustomEvent("pm:principal", { detail: me }));

    const page = location.pathname.split("/").pop() || "index.html";
    const entry = PAGES[page];
    if (!entry) return;
    try {
      await guarded(page, entry.load, entry.render);
    } catch (error) {
      reportError(error);
    }
  }

  // 失败要可见。静默失效的界面比报错的界面糟得多——
  // 它让人以为「没有数据」，而真相是「没取到数据」。
  function reportError(error) {
    const banner = document.querySelector("[data-pm-error]");
    if (banner) {
      banner.hidden = false;
      const text = banner.querySelector("[data-pm-error-text]") ?? banner;
      text.textContent =
        error.code === "audit_not_enabled"
          ? "该能力尚未启用。"
          : `加载失败：${error.message}`;
    } else {
      console.error("[proxy-manager] 加载失败", error);
    }
  }

  // 给 `chart.js` 的接缝。无构建步骤、无包管理器（§5），所以模块边界只能靠一个
  // 明确的全局对象——比让图表自己再实现一遍字节格式化好：
  // 那会变成第二份 C3 纪律，而两份纪律里总有一份会先松掉。
  window.pm = { getJson, guarded, formatBytes, exactBytes, groupDigits, applyBindings, renderRows };

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", start, { once: true });
  } else {
    start();
  }

})();
