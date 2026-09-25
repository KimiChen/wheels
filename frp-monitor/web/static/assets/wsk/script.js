// Every listener in this file is bound once, on `document`. Nothing is bound
// to an element found at load time, so replacing a fragment — htmx, Turbo,
// hx-boost, a view transition — never leaves a dead control behind. What a
// fresh fragment does need is an initial render (labels, page buttons, the
// first table page); call `wsk.mount(root)` for that. It is idempotent.

const root = document.documentElement;
root.dataset.js = "true";

const $ = (selector, scope = document) => scope.querySelector(selector);
const $$ = (selector, scope = document) => [...scope.querySelectorAll(selector)];

// Fragment roots can themselves be components, not just their containers.
const within = (selector, scope = document) => [
  ...(scope instanceof Element && scope.matches(selector) ? [scope] : []),
  ...$$(selector, scope),
];

function componentsWithin(selector, scope) {
  const owner = scope instanceof Element ? scope.closest(selector) : null;
  // Also refresh a component when only its body or controls were replaced.
  return [...new Set([...(owner ? [owner] : []), ...within(selector, scope)])];
}

/**
 * Delegate `type` on document to whatever matches `selector`.
 * The handler receives (event, matchedElement).
 */
function on(type, selector, handler, options) {
  document.addEventListener(
    type,
    (event) => {
      const target =
        event.target instanceof Element ? event.target.closest(selector) : null;
      if (target) handler(event, target);
    },
    options,
  );
}

function getHashTarget() {
  const encodedId = location.hash.slice(1);
  if (!encodedId) return null;

  try {
    return document.getElementById(decodeURIComponent(encodedId));
  } catch {
    return null;
  }
}

/* ------------------------------------------------------------------ toasts */

const TOAST_ICONS = {
  success: "#check",
  info: "#info",
  warning: "#warning",
  danger: "#warning",
};

function showToast(message, variant = "success") {
  // Looked up per call: the region may have been swapped since load, and a
  // reference captured at load time would write into a detached node.
  const region = document.getElementById("toast-region");
  if (!region) return;

  const toast = document.createElement("div");
  toast.className = "wsk-toast";
  if (variant !== "success") toast.classList.add(`wsk-${variant}`);
  // No role here: the region itself is the live region, and nesting one
  // inside another makes some screen readers announce the toast twice.

  const icon = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  icon.classList.add("wsk-icon");
  icon.setAttribute("aria-hidden", "true");
  const use = document.createElementNS("http://www.w3.org/2000/svg", "use");
  use.setAttribute("href", TOAST_ICONS[variant] ?? TOAST_ICONS.success);
  icon.append(use);

  const text = document.createElement("span");
  text.textContent = message;
  toast.append(icon, text);
  region.append(toast);
  setTimeout(() => toast.remove(), 3200);
}

/* ------------------------------------------------------------------- theme */

const THEME_KEY = "web-standard-kit-theme";
// system -> light -> dark -> system. "system" is the absence of [data-theme]:
// the stylesheet's light-dark() tokens then follow the system on their own,
// so no matchMedia listener is needed here.
const THEME_ORDER = ["system", "light", "dark"];
const THEME_LABELS = {
  system: "主题：跟随系统，切换到浅色",
  light: "主题：浅色，切换到深色",
  dark: "主题：深色，切换到跟随系统",
};

function currentTheme() {
  const value = root.dataset.theme;
  return value === "light" || value === "dark" ? value : "system";
}

function syncThemeButtons(scope = document) {
  const label = THEME_LABELS[currentTheme()];
  within("[data-theme-toggle]", scope).forEach((button) => {
    button.setAttribute("aria-label", label);
    button.setAttribute("title", label);
  });
}

function applyTheme(nextTheme) {
  if (nextTheme === "system") delete root.dataset.theme;
  else root.dataset.theme = nextTheme;
  syncThemeButtons();
  try {
    if (nextTheme === "system") localStorage.removeItem(THEME_KEY);
    else localStorage.setItem(THEME_KEY, nextTheme);
  } catch {
    // The selected theme still applies to the current page.
  }
}

/* ------------------------------------------------------------------- views */

function showView(name, { moveFocus = false, scrollTarget = null } = {}) {
  root.dataset.activeView = name;
  $$("[data-view-target]").forEach((button) => {
    const target = button.dataset.viewTarget;
    // A sub-view is named "<parent>-<child>"; its parent's button stays
    // pressed, so the forum post page still reads as "in the forum".
    const active = target === name || name.startsWith(`${target}-`);
    button.classList.toggle("wsk-is-active", active);
    button.setAttribute("aria-pressed", String(active));
  });
  $$("[data-view]").forEach((view) => {
    view.hidden = view.dataset.view !== name;
  });
  // Reveal the view before scrolling — a hidden anchor cannot be scrolled to.
  if (scrollTarget) scrollTarget.scrollIntoView();
  if (moveFocus) $(`[data-view="${name}"] h1`)?.focus();
}

const viewOf = (element) =>
  element?.closest("[data-view]")?.dataset.view ?? null;

function applyHash(allowFocus, { scroll = true } = {}) {
  if (!$("[data-view]")) return;
  const target = getHashTarget();
  const previous = root.dataset.activeView || null;
  // No hash is the default view, so Back out of #reference-overview still
  // lands on the kit. But a hash whose target belongs to no view — the skip
  // link's #main-content, which is the shared <main> — must leave the current
  // view alone instead of yanking the reader out of the reference app.
  const name = location.hash ? (viewOf(target) ?? previous ?? "kit") : "kit";
  showView(name, {
    moveFocus: allowFocus && name !== previous,
    scrollTarget: scroll && viewOf(target) === name ? target : null,
  });
}

function syncReferenceNav(scope = document) {
  const links = within(".wsk-reference-nav a", scope);
  if (!links.length) return;
  const target = getHashTarget();
  const sectionOf = (link) => {
    const id = link.getAttribute("href")?.slice(1);
    return id ? document.getElementById(id) : null;
  };
  // An exact hit wins. Otherwise take the innermost section that contains the
  // target: a table of contents nests (the body contains every heading in it),
  // and picking the first match in document order would always land on the
  // outermost entry.
  const currentLink = !target
    ? null
    : (links.find((link) => sectionOf(link) === target) ??
      links
        .filter((link) => sectionOf(link)?.contains(target))
        .reduce(
          (best, link) =>
            best && !sectionOf(best).contains(sectionOf(link)) ? best : link,
          null,
        ));
  links.forEach((link) => {
    const current = link === currentLink;
    link.classList.toggle("wsk-active", current);
    if (current) link.setAttribute("aria-current", "location");
    else link.removeAttribute("aria-current");
  });
}

/* -------------------------------------------------------------------- menu */

const menuItems = (menu) => $$('[role="menuitem"]', menu);

function menuParts(fromElement) {
  const wrap = fromElement.closest(".wsk-menu-wrap");
  const button = wrap ? $("[data-menu-button]", wrap) : null;
  const menu = button
    ? document.getElementById(button.getAttribute("aria-controls"))
    : null;
  return button && menu && wrap ? { wrap, button, menu } : null;
}

function closeMenu({ button, menu }, returnFocus = false) {
  menu.hidden = true;
  button.setAttribute("aria-expanded", "false");
  if (returnFocus) button.focus();
}

function openMenu({ button, menu }, focusLast = false) {
  menu.hidden = false;
  button.setAttribute("aria-expanded", "true");
  const items = menuItems(menu);
  (focusLast ? items.at(-1) : items[0])?.focus();
}

/* ---------------------------------------------------------------- collection */

// Filtering, sorting and paging work on any collection of [data-row] elements.
// A table declares [data-table] / [data-table-scope]; a card list declares
// [data-list] / [data-list-scope]. Nothing here assumes a <table>: the only
// structural requirement is that the empty-state element sits inside the same
// container as the rows, so a sort can re-insert rows before it.
const COLLECTION = "[data-table], [data-list]";
const COLLECTION_SCOPE = "[data-table-scope], [data-list-scope]";
const DEFAULT_PAGE_SIZE = 3;

function collectionParts(el) {
  const wrap = el.closest(COLLECTION_SCOPE) ?? document;
  return {
    el,
    wrap,
    body: $("[data-rows]", el) ?? $("tbody", el),
    emptyRow: $("[data-table-empty]", el),
    filter: $("[data-table-filter]", wrap),
    result: $("[data-table-result]", wrap),
    selectionResult: $("[data-table-selection]", wrap),
    selectAll: $("[data-table-select-all]", wrap),
    pagePrevious: $('[data-table-page-action="previous"]', wrap),
    pageNext: $('[data-table-page-action="next"]', wrap),
    pageNumbers: $("[data-table-page-numbers]", wrap),
  };
}

// Rows are read from the DOM every time rather than cached, so sorting (which
// re-inserts them) and any fragment swap both stay consistent.
const collectionRows = (el) => $$("[data-row]", el);

const collectionPage = (el) => Number(el.dataset.wskPage || "1");

const collectionPageSize = (el) =>
  Number(el.dataset.pageSize || DEFAULT_PAGE_SIZE);

function filteredRows(parts) {
  const query = (parts.filter?.value ?? "").trim().toLocaleLowerCase("zh-CN");
  return collectionRows(parts.el).filter((row) =>
    row.textContent.toLocaleLowerCase("zh-CN").includes(query),
  );
}

function visibleRows(parts, list = filteredRows(parts)) {
  const size = collectionPageSize(parts.el);
  const start = (collectionPage(parts.el) - 1) * size;
  return list.slice(start, start + size);
}

function updateSelection(parts) {
  const selected = collectionRows(parts.el).filter(
    (row) => $(".wsk-row-select", row)?.checked,
  ).length;
  if (parts.selectionResult) {
    parts.selectionResult.textContent = `已选择 ${selected} 项`;
  }
  const visible = visibleRows(parts);
  const visibleSelected = visible.filter(
    (row) => $(".wsk-row-select", row)?.checked,
  ).length;
  if (parts.selectAll) {
    parts.selectAll.checked =
      visible.length > 0 && visibleSelected === visible.length;
    parts.selectAll.indeterminate =
      visibleSelected > 0 && visibleSelected < visible.length;
  }
}

function renderCollection(parts, focusPage = null) {
  const { el } = parts;
  const filtered = filteredRows(parts);
  const pageCount = Math.max(
    1,
    Math.ceil(filtered.length / collectionPageSize(el)),
  );
  el.dataset.wskPage = String(Math.min(collectionPage(el), pageCount));
  const page = collectionPage(el);
  const visible = new Set(visibleRows(parts, filtered));
  collectionRows(el).forEach((row) => {
    row.hidden = !visible.has(row);
  });
  if (parts.emptyRow) parts.emptyRow.hidden = filtered.length !== 0;
  if (parts.result) {
    const unit = el.dataset.unit || "条记录";
    parts.result.textContent = `${filtered.length} ${unit} · 第 ${page}/${pageCount} 页`;
  }
  if (parts.pagePrevious) parts.pagePrevious.disabled = page === 1;
  if (parts.pageNext) parts.pageNext.disabled = page === pageCount;

  if (parts.pageNumbers) {
    parts.pageNumbers.replaceChildren();
    let focusTarget = null;
    for (let number = 1; number <= pageCount; number += 1) {
      const button = document.createElement("button");
      button.type = "button";
      button.textContent = String(number);
      button.dataset.tablePageNumber = String(number);
      button.classList.toggle("wsk-current", number === page);
      if (number === page) button.setAttribute("aria-current", "page");
      if (number === focusPage) focusTarget = button;
      parts.pageNumbers.append(button);
    }
    focusTarget?.focus();
  }
  updateSelection(parts);
}

const collectionFrom = (element) =>
  element.closest(COLLECTION_SCOPE)?.querySelector(COLLECTION) ?? null;

/* -------------------------------------------------------------- pagination */

function renderPagination(pagination, focusCurrent = false) {
  const buttons = $$("[data-page]", pagination);
  if (!buttons.length) return;
  const pageCount = buttons.length;
  const current = Math.min(
    Math.max(1, Number(pagination.dataset.wskPage || "1")),
    pageCount,
  );
  pagination.dataset.wskPage = String(current);
  buttons.forEach((button) => {
    const isCurrent = Number(button.dataset.page) === current;
    button.classList.toggle("wsk-current", isCurrent);
    if (isCurrent) button.setAttribute("aria-current", "page");
    else button.removeAttribute("aria-current");
    if (isCurrent && focusCurrent) button.focus();
  });
  const previous = $('[data-page-action="previous"]', pagination);
  const next = $('[data-page-action="next"]', pagination);
  if (previous) previous.disabled = current === 1;
  if (next) next.disabled = current === pageCount;
  const status = $("[data-page-status]", pagination);
  if (status) status.textContent = `第 ${current} 页，共 ${pageCount} 页`;
}

/* ------------------------------------------------------------------- forms */

function syncCode(scope = document) {
  within("[data-code-input]", scope).forEach((code) => {
    // Each input must find its own messages. .wsk-field is the normal wrapper;
    // aria-describedby is the fallback because the markup already names them
    // there. Searching the whole form instead would hand every input in it the
    // same first pair, so the last one processed overwrites the rest.
    const field = code.closest(".wsk-field");
    const described = field
      ? null
      : (code.getAttribute("aria-describedby") ?? "")
          .split(/\s+/)
          .filter(Boolean)
          .map((id) => document.getElementById(id))
          .filter(Boolean);
    const pick = (selector) =>
      field ? $(selector, field) : described.find((el) => el.matches(selector));
    const error = pick("[data-code-error]");
    const success = pick("[data-code-success]");
    if (!error || !success) return;
    const valid = code.checkValidity();
    code.setAttribute("aria-invalid", String(!valid));
    if (valid) code.dataset.state = "success";
    else delete code.dataset.state;
    error.hidden = valid;
    success.hidden = !valid;
  });
}

/* -------------------------------------------------------------------- tabs */

function activateTab(tab) {
  const group = tab.closest("[data-tabs]");
  if (!group) return;
  $$('[role="tab"]', group).forEach((item) => {
    const selected = item === tab;
    item.setAttribute("aria-selected", String(selected));
    item.tabIndex = selected ? 0 : -1;
  });
  $$('[role="tabpanel"]', group).forEach((panel) => {
    panel.hidden = panel.id !== tab.getAttribute("aria-controls");
  });
}

/* ---------------------------------------------------------- press feedback */

const PRESS_TARGETS = [
  ".wsk-metric",
  ".wsk-panel:not(form)",
  ".wsk-service-card",
  ".wsk-auth-card:not(form)",
  ".wsk-empty-state",
  ".wsk-process-step",
  ".wsk-mini-metric-grid > div:not(.wsk-metric)",
  ".wsk-table-wrap",
].join(",");

const INTERACTIVE =
  "button, input, select, textarea, a, [role='button'], [role='link']";

/* --------------------------------------------------------------- listeners */

let listening = false;

function listen() {
  if (listening) return;
  listening = true;

  on("click", "[data-theme-toggle]", () => {
    const index = THEME_ORDER.indexOf(currentTheme());
    applyTheme(THEME_ORDER[(index + 1) % THEME_ORDER.length]);
  });

  on("click", "[data-view-target]", (event, button) => {
    const anchor = button.dataset.viewAnchor;
    if (anchor) {
      // Reflect the view in the URL so it is addressable and bookmarkable.
      location.hash = anchor;
      return;
    }
    // A button with no anchor is the default view. Only worth a history entry
    // when there is a hash to drop; otherwise repeat clicks stack identical
    // entries that Back cannot escape.
    if (location.hash) {
      history.pushState(null, "", location.pathname + location.search);
    }
    showView(button.dataset.viewTarget, { moveFocus: true });
  });

  window.addEventListener("hashchange", () => {
    applyHash(true);
    syncReferenceNav();
  });

  on("click", "[data-menu-button]", (event, button) => {
    const parts = menuParts(button);
    if (!parts) return;
    if (parts.menu.hidden) openMenu(parts);
    else closeMenu(parts);
  });

  on("keydown", "[data-menu-button]", (event, button) => {
    if (!["ArrowDown", "ArrowUp"].includes(event.key)) return;
    const parts = menuParts(button);
    if (!parts) return;
    event.preventDefault();
    openMenu(parts, event.key === "ArrowUp");
  });

  on("keydown", '[role="menu"]', (event, menu) => {
    const parts = menuParts(menu);
    if (!parts) return;
    const items = menuItems(menu);
    const current = items.indexOf(document.activeElement);
    if (event.key === "Escape") {
      event.preventDefault();
      closeMenu(parts, true);
      return;
    }
    if (event.key === "Tab") {
      closeMenu(parts);
      return;
    }
    if (!["ArrowDown", "ArrowUp", "Home", "End"].includes(event.key)) return;
    event.preventDefault();
    let next = current;
    if (event.key === "ArrowDown") next = (current + 1) % items.length;
    if (event.key === "ArrowUp")
      next = (current - 1 + items.length) % items.length;
    if (event.key === "Home") next = 0;
    if (event.key === "End") next = items.length - 1;
    items[next]?.focus();
  });

  on("click", '[role="menuitem"]', (event, item) => {
    const parts = menuParts(item);
    if (parts) closeMenu(parts, true);
  });

  on("focusout", ".wsk-menu-wrap", (event, wrap) => {
    if (wrap.contains(event.relatedTarget)) return;
    const parts = menuParts(wrap);
    if (parts) closeMenu(parts);
  });

  document.addEventListener("click", (event) => {
    const inside =
      event.target instanceof Element &&
      event.target.closest(".wsk-menu-wrap");
    if (inside) return;
    $$(".wsk-menu-wrap").forEach((wrap) => {
      const parts = menuParts(wrap);
      if (parts && !parts.menu.hidden) closeMenu(parts);
    });
  });

  on("click", '[role="tab"]', (event, tab) => activateTab(tab));

  on("keydown", '[role="tab"]', (event, tab) => {
    if (!["ArrowLeft", "ArrowRight", "Home", "End"].includes(event.key)) return;
    const group = tab.closest("[data-tabs]");
    if (!group) return;
    const tabs = $$('[role="tab"]', group);
    const index = tabs.indexOf(tab);
    event.preventDefault();
    let next = index;
    if (event.key === "ArrowLeft") next = (index - 1 + tabs.length) % tabs.length;
    if (event.key === "ArrowRight") next = (index + 1) % tabs.length;
    if (event.key === "Home") next = 0;
    if (event.key === "End") next = tabs.length - 1;
    activateTab(tabs[next]);
    tabs[next].focus();
  });

  on("click", "[data-password]", (event, button) => {
    const field = button.closest(".wsk-input-wrap") ?? document;
    const input = $("input", field);
    if (!input) return;
    const reveal = input.type === "password";
    input.type = reveal ? "text" : "password";
    button.setAttribute("aria-pressed", String(reveal));
    button.setAttribute("aria-label", reveal ? "隐藏密码" : "显示密码");
    $("use", button)?.setAttribute("href", reveal ? "#eye-off" : "#eye");
  });

  on("input", "[data-code-input]", (event, code) =>
    syncCode(code),
  );

  document.addEventListener("reset", (event) => {
    const form = event.target;
    if (form instanceof HTMLFormElement) setTimeout(() => syncCode(form));
  });

  document.addEventListener("submit", (event) => {
    const form = event.target;
    if (!(form instanceof HTMLFormElement)) return;
    const message = form.dataset.demoSubmit;
    if (message === undefined) return;
    event.preventDefault();
    if (!form.reportValidity()) return;
    showToast(message, form.dataset.demoSubmitVariant);
  });

  on("click", "[data-demo-pagination] [data-page]", (event, button) => {
    const pagination = button.closest("[data-demo-pagination]");
    pagination.dataset.wskPage = button.dataset.page;
    renderPagination(pagination, true);
  });

  on("click", "[data-demo-pagination] [data-page-action]", (event, button) => {
    const pagination = button.closest("[data-demo-pagination]");
    const step = button.dataset.pageAction === "previous" ? -1 : 1;
    pagination.dataset.wskPage = String(
      Number(pagination.dataset.wskPage || "1") + step,
    );
    renderPagination(pagination);
  });

  on("change", "[data-density]", (event, group) => {
    const collection = collectionFrom(group);
    if (collection && event.target instanceof HTMLInputElement) {
      collection.dataset.density = event.target.value;
    }
  });

  on("input", "[data-table-filter]", (event, filter) => {
    const collection = collectionFrom(filter);
    if (!collection) return;
    collection.dataset.wskPage = "1";
    renderCollection(collectionParts(collection));
  });

  on("click", "[data-sort]", (event, button) => {
    const collection = collectionFrom(button);
    if (!collection) return;
    const parts = collectionParts(collection);
    const key = button.dataset.sort;
    const direction =
      collection.dataset.wskSortKey === key &&
      collection.dataset.wskSortDirection === "ascending"
        ? "descending"
        : "ascending";
    collection.dataset.wskSortKey = key;
    collection.dataset.wskSortDirection = direction;

    const sorted = collectionRows(collection).sort(
      (a, b) =>
        // numeric: true so a reply count of 7 sorts below 12 rather than
        // above it, which plain lexicographic compare would get wrong.
        (a.dataset[key] ?? "").localeCompare(b.dataset[key] ?? "", "zh-CN", {
          numeric: true,
        }) * (direction === "ascending" ? 1 : -1),
    );
    sorted.forEach((row) => parts.body?.insertBefore(row, parts.emptyRow));

    const scope = button.closest(COLLECTION_SCOPE) ?? document;
    $$("th[aria-sort]", scope).forEach((header) =>
      header.setAttribute("aria-sort", "none"),
    );
    button.closest("th")?.setAttribute("aria-sort", direction);
    // aria-sort carries the direction for screen readers; the icon has to
    // carry it for everyone else, so swap in a one-way chevron.
    $$("[data-sort]", scope).forEach((other) => {
      const current = other === button;
      const icon = current
        ? direction === "ascending"
          ? "#sort-asc"
          : "#sort-desc"
        : "#sort";
      $("use", other)?.setAttribute("href", icon);
      // A table hangs the state on its <th>. A list's sort controls are plain
      // toggle buttons, so the state has to live on the button itself.
      if (other.closest("th")) return;
      other.setAttribute("aria-pressed", String(current));
      const label = other.dataset.sortLabel ?? other.textContent.trim();
      other.setAttribute(
        "aria-label",
        current
          ? `${label}（${direction === "ascending" ? "升序" : "降序"}）`
          : label,
      );
    });
    collection.dataset.wskPage = "1";
    renderCollection(parts);
  });

  on("change", "[data-table-select-all]", (event, selectAll) => {
    const collection = collectionFrom(selectAll);
    if (!collection) return;
    const parts = collectionParts(collection);
    visibleRows(parts).forEach((row) => {
      const checkbox = $(".wsk-row-select", row);
      if (checkbox) checkbox.checked = selectAll.checked;
    });
    updateSelection(parts);
  });

  on("change", ".wsk-row-select", (event, checkbox) => {
    const collection = checkbox.closest(COLLECTION);
    if (collection) updateSelection(collectionParts(collection));
  });

  on("click", "[data-table-page-action]", (event, button) => {
    const collection = collectionFrom(button);
    if (!collection) return;
    const step = button.dataset.tablePageAction === "previous" ? -1 : 1;
    collection.dataset.wskPage = String(
      Math.max(1, collectionPage(collection) + step),
    );
    renderCollection(collectionParts(collection));
  });

  on("click", "[data-table-page-number]", (event, button) => {
    const collection = collectionFrom(button);
    if (!collection) return;
    collection.dataset.wskPage = button.dataset.tablePageNumber;
    renderCollection(
      collectionParts(collection),
      Number(button.dataset.tablePageNumber),
    );
  });

  on("click", "[data-dialog-open]", (event, button) => {
    document.getElementById(button.dataset.dialogOpen)?.showModal();
  });

  on("click", "[data-dialog-close]", (event, button) => {
    button.closest("dialog")?.close();
  });

  document.addEventListener("click", (event) => {
    // A click landing on the <dialog> itself is a click on its backdrop.
    if (event.target instanceof HTMLDialogElement) event.target.close();
  });

  on("click", "[data-toast]", (event, button) =>
    showToast(button.dataset.toast, button.dataset.toastVariant),
  );

  const releasePress = (event) => {
    const target =
      event.target instanceof Element ? event.target.closest(PRESS_TARGETS) : null;
    if (target) target.classList.remove("wsk-is-pressed");
  };
  document.addEventListener(
    "pointerdown",
    (event) => {
      if (event.pointerType === "mouse" || !(event.target instanceof Element)) {
        return;
      }
      const target = event.target.closest(PRESS_TARGETS);
      if (target && !event.target.closest(INTERACTIVE)) {
        target.classList.add("wsk-is-pressed");
      }
    },
    { passive: true },
  );
  document.addEventListener("pointerup", releasePress, { passive: true });
  document.addEventListener("pointercancel", releasePress, { passive: true });
  // pointerleave does not bubble; pointerout does, so check the pointer really
  // left the pressed element rather than moving between its children.
  document.addEventListener(
    "pointerout",
    (event) => {
      const target =
        event.target instanceof Element
          ? event.target.closest(PRESS_TARGETS)
          : null;
      if (target && !target.contains(event.relatedTarget)) {
        target.classList.remove("wsk-is-pressed");
      }
    },
    { passive: true },
  );
}

/* ------------------------------------------------------------------- mount */

/**
 * Bring `scope` up to date: sync labels and render anything that needs an
 * initial pass. Safe to call repeatedly, and safe to call on a fragment that
 * was just swapped in. Listeners are global and bound only once.
 */
function mount(scope = document) {
  listen();
  syncThemeButtons(scope);
  // New views still need their visibility synchronized, but a fragment swap
  // must not navigate away from the reader's current scroll position.
  applyHash(false, { scroll: false });
  syncReferenceNav(scope);
  componentsWithin('[data-tabs]', scope).forEach((group) => {
    const selected =
      $('[role="tab"][aria-selected="true"]', group) ?? $('[role="tab"]', group);
    if (selected) activateTab(selected);
  });
  componentsWithin("[data-demo-pagination]", scope).forEach((pagination) =>
    renderPagination(pagination),
  );
  componentsWithin(COLLECTION, scope).forEach((collection) =>
    renderCollection(collectionParts(collection)),
  );
  syncCode(scope);
}

function start() {
  mount();
  // Only initial navigation and hash changes scroll to the URL's target.
  applyHash(false);
}

if (document.readyState === "loading") {
  document.addEventListener("DOMContentLoaded", start, { once: true });
} else {
  start();
}

globalThis.wsk = { mount, showToast };
