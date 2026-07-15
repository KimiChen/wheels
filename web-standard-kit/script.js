const root = document.documentElement;
const toastRegion = document.getElementById("toast-region");

function getHashTarget() {
  const encodedId = location.hash.slice(1);
  if (!encodedId) return null;

  try {
    return document.getElementById(decodeURIComponent(encodedId));
  } catch {
    return null;
  }
}

const TOAST_ICONS = {
  success: "#check",
  info: "#info",
  warning: "#warning",
  danger: "#warning",
};

function showToast(message, variant = "success") {
  if (!toastRegion) return;

  const toast = document.createElement("div");
  toast.className = "wsk-toast";
  if (variant !== "success") toast.classList.add(`wsk-${variant}`);
  toast.setAttribute("role", "status");

  const icon = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  icon.classList.add("wsk-icon");
  icon.setAttribute("aria-hidden", "true");
  const use = document.createElementNS("http://www.w3.org/2000/svg", "use");
  use.setAttribute("href", TOAST_ICONS[variant] ?? TOAST_ICONS.success);
  icon.append(use);

  const text = document.createElement("span");
  text.textContent = message;
  toast.append(icon, text);
  toastRegion.append(toast);
  setTimeout(() => toast.remove(), 3200);
}

function initTheme() {
  const button = document.querySelector("[data-theme-toggle]");
  if (!button) return;

  const storageKey = "web-standard-kit-theme";

  function applyTheme(nextTheme) {
    root.dataset.theme = nextTheme;
    const nextLabel = nextTheme === "dark" ? "浅色" : "深色";
    button.setAttribute("aria-label", `切换到${nextLabel}主题`);
    button.setAttribute("title", `切换到${nextLabel}主题`);
  }

  // The inline head script already applied the initial theme before first paint.
  applyTheme(root.dataset.theme === "dark" ? "dark" : "light");
  button.addEventListener("click", () => {
    const nextTheme = root.dataset.theme === "dark" ? "light" : "dark";
    applyTheme(nextTheme);
    try {
      localStorage.setItem(storageKey, nextTheme);
    } catch {
      // The selected theme still applies to the current page.
    }
  });
}

function initViews() {
  const buttons = [...document.querySelectorAll("[data-view-target]")];
  const views = [...document.querySelectorAll("[data-view]")];
  if (!buttons.length || !views.length) return;

  let currentView = null;

  function showView(name, { moveFocus = false, scrollTarget = null } = {}) {
    root.dataset.activeView = name;
    currentView = name;
    buttons.forEach((button) => {
      const active = button.dataset.viewTarget === name;
      button.classList.toggle("wsk-is-active", active);
      button.setAttribute("aria-pressed", String(active));
    });
    views.forEach((view) => {
      view.hidden = view.dataset.view !== name;
    });
    // Reveal the view before scrolling — a hidden anchor cannot be scrolled to.
    if (scrollTarget) scrollTarget.scrollIntoView();
    if (moveFocus) document.querySelector(`[data-view="${name}"] h1`)?.focus();
  }

  function viewOf(element) {
    return element?.closest("[data-view]")?.dataset.view ?? null;
  }

  function applyHash(allowFocus) {
    const target = getHashTarget();
    const name = viewOf(target) ?? "kit";
    const changed = name !== currentView;
    showView(name, {
      moveFocus: allowFocus && changed,
      scrollTarget: viewOf(target) === name ? target : null,
    });
  }

  applyHash(false);
  buttons.forEach((button) => {
    button.addEventListener("click", () => {
      const name = button.dataset.viewTarget;
      if (name === "reference") {
        // Reflect the view in the URL so it is addressable and bookmarkable.
        location.hash = "reference-overview";
      } else {
        history.pushState(null, "", location.pathname + location.search);
        showView("kit", { moveFocus: true });
      }
    });
  });
  window.addEventListener("hashchange", () => applyHash(true));
}

function initReferenceNavigation() {
  const links = [...document.querySelectorAll(".wsk-reference-nav a")];
  if (!links.length) return;

  function syncCurrentLink() {
    const target = getHashTarget();
    const currentLink = links.find((link) => {
      const id = link.getAttribute("href")?.slice(1);
      const section = id ? document.getElementById(id) : null;
      return (
        section && target && (section === target || section.contains(target))
      );
    });

    links.forEach((link) => {
      const current = link === currentLink;
      link.classList.toggle("wsk-active", current);
      if (current) link.setAttribute("aria-current", "location");
      else link.removeAttribute("aria-current");
    });
  }

  syncCurrentLink();
  window.addEventListener("hashchange", syncCurrentLink);
}

function initMenu() {
  const button = document.querySelector("[data-menu-button]");
  const menu = document.getElementById(button?.getAttribute("aria-controls"));
  const wrap = button?.closest(".wsk-menu-wrap");
  if (!button || !menu || !wrap) return;

  const items = [...menu.querySelectorAll('[role="menuitem"]')];

  function closeMenu(returnFocus = false) {
    menu.hidden = true;
    button.setAttribute("aria-expanded", "false");
    if (returnFocus) button.focus();
  }

  function openMenu(focusLast = false) {
    menu.hidden = false;
    button.setAttribute("aria-expanded", "true");
    const target = focusLast ? items.at(-1) : items[0];
    target?.focus();
  }

  button.addEventListener("click", () => {
    if (menu.hidden) openMenu();
    else closeMenu();
  });
  button.addEventListener("keydown", (event) => {
    if (!["ArrowDown", "ArrowUp"].includes(event.key)) return;
    event.preventDefault();
    openMenu(event.key === "ArrowUp");
  });
  menu.addEventListener("keydown", (event) => {
    const current = items.indexOf(document.activeElement);
    if (event.key === "Escape") {
      event.preventDefault();
      closeMenu(true);
      return;
    }
    if (event.key === "Tab") {
      closeMenu();
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
  items.forEach((item) =>
    item.addEventListener("click", () => closeMenu(true)),
  );
  wrap.addEventListener("focusout", (event) => {
    if (!wrap.contains(event.relatedTarget)) closeMenu();
  });
  document.addEventListener("click", (event) => {
    const insideMenu =
      event.target instanceof Element && event.target.closest(".wsk-menu-wrap");
    if (!menu.hidden && !insideMenu) closeMenu();
  });
}

function initTabs() {
  document.querySelectorAll("[data-tabs]").forEach((group) => {
    const tabs = [...group.querySelectorAll('[role="tab"]')];
    const panels = [...group.querySelectorAll('[role="tabpanel"]')];

    function activateTab(tab) {
      tabs.forEach((item) => {
        const selected = item === tab;
        item.setAttribute("aria-selected", String(selected));
        item.tabIndex = selected ? 0 : -1;
      });
      panels.forEach((panel) => {
        panel.hidden = panel.id !== tab.getAttribute("aria-controls");
      });
    }

    tabs.forEach((tab, index) => {
      tab.addEventListener("click", () => activateTab(tab));
      tab.addEventListener("keydown", (event) => {
        if (!["ArrowLeft", "ArrowRight", "Home", "End"].includes(event.key))
          return;
        event.preventDefault();
        let next = index;
        if (event.key === "ArrowLeft")
          next = (index - 1 + tabs.length) % tabs.length;
        if (event.key === "ArrowRight") next = (index + 1) % tabs.length;
        if (event.key === "Home") next = 0;
        if (event.key === "End") next = tabs.length - 1;
        activateTab(tabs[next]);
        tabs[next].focus();
      });
    });
  });
}

function initForms() {
  const password = document.getElementById("password");
  const passwordButton = document.querySelector("[data-password]");
  if (password && passwordButton) {
    passwordButton.addEventListener("click", () => {
      const reveal = password.type === "password";
      password.type = reveal ? "text" : "password";
      passwordButton.setAttribute("aria-pressed", String(reveal));
      passwordButton.setAttribute(
        "aria-label",
        reveal ? "隐藏密码" : "显示密码",
      );
      passwordButton
        .querySelector("use")
        ?.setAttribute("href", reveal ? "#eye-off" : "#eye");
    });
  }

  const code = document.getElementById("code");
  const codeError = document.getElementById("code-error");
  const codeSuccess = document.getElementById("code-success");
  const form = document.getElementById("sample-form");

  function syncCode() {
    if (!code || !codeError || !codeSuccess) return;
    const valid = code.checkValidity();
    code.setAttribute("aria-invalid", String(!valid));
    if (valid) code.dataset.state = "success";
    else delete code.dataset.state;
    codeError.hidden = valid;
    codeSuccess.hidden = !valid;
  }

  code?.addEventListener("input", syncCode);
  form?.addEventListener("reset", () => setTimeout(syncCode));
  form?.addEventListener("submit", (event) => {
    event.preventDefault();
    if (!form.reportValidity()) return;
    showToast("表单校验通过，示例数据未提交。");
  });

  const referenceLogin = document.getElementById("reference-login");
  referenceLogin?.addEventListener("submit", (event) => {
    event.preventDefault();
    if (!referenceLogin.reportValidity()) return;
    showToast("静态参考页面未连接后端认证服务。", "info");
  });
}

function initDemoPagination() {
  const pagination = document.querySelector("[data-demo-pagination]");
  if (!pagination) return;

  const pageButtons = [...pagination.querySelectorAll("[data-page]")];
  const previous = pagination.querySelector('[data-page-action="previous"]');
  const next = pagination.querySelector('[data-page-action="next"]');
  const status = pagination.querySelector("[data-page-status]");
  if (!pageButtons.length || !previous || !next || !status) return;

  const pageCount = pageButtons.length;
  let currentPage = 1;

  function render(focusCurrent = false) {
    pageButtons.forEach((button) => {
      const page = Number(button.dataset.page);
      const current = page === currentPage;
      button.classList.toggle("wsk-current", current);
      if (current) button.setAttribute("aria-current", "page");
      else button.removeAttribute("aria-current");
      if (current && focusCurrent) button.focus();
    });
    previous.disabled = currentPage === 1;
    next.disabled = currentPage === pageCount;
    status.textContent = `第 ${currentPage} 页，共 ${pageCount} 页`;
  }

  pageButtons.forEach((button) => {
    button.addEventListener("click", () => {
      currentPage = Number(button.dataset.page);
      render(true);
    });
  });
  previous.addEventListener("click", () => {
    currentPage = Math.max(1, currentPage - 1);
    render();
  });
  next.addEventListener("click", () => {
    currentPage = Math.min(pageCount, currentPage + 1);
    render();
  });
  render();
}

function initDataTable() {
  const table = document.getElementById("asset-table");
  const filter = document.getElementById("asset-filter");
  const result = document.getElementById("filter-result");
  const selectionResult = document.getElementById("selection-result");
  const selectAll = document.getElementById("select-all");
  const pagePrevious = document.getElementById("page-prev");
  const pageNext = document.getElementById("page-next");
  const pageNumbers = document.getElementById("page-numbers");
  if (
    !table ||
    !filter ||
    !result ||
    !selectionResult ||
    !selectAll ||
    !pagePrevious ||
    !pageNext ||
    !pageNumbers
  )
    return;

  const tableBody = table.querySelector("tbody");
  const emptyRow = document.getElementById("table-empty");
  const density = document.querySelector("[data-density]");
  const rows = [...table.querySelectorAll("tbody tr[data-row]")];
  if (!tableBody || !emptyRow || !rows.length) return;

  const pageSize = 3;
  let currentPage = 1;
  let sortKey = "";
  let sortDirection = "ascending";

  function filteredRows() {
    const query = filter.value.trim().toLocaleLowerCase("zh-CN");
    return rows.filter((row) =>
      row.textContent.toLocaleLowerCase("zh-CN").includes(query),
    );
  }

  function visibleRows(list = filteredRows()) {
    const start = (currentPage - 1) * pageSize;
    return list.slice(start, start + pageSize);
  }

  function updateSelection() {
    const selected = rows.filter(
      (row) => row.querySelector(".wsk-row-select")?.checked,
    ).length;
    selectionResult.textContent = `已选择 ${selected} 项`;
    const visible = visibleRows();
    const visibleSelected = visible.filter(
      (row) => row.querySelector(".wsk-row-select")?.checked,
    ).length;
    selectAll.checked =
      visible.length > 0 && visibleSelected === visible.length;
    selectAll.indeterminate =
      visibleSelected > 0 && visibleSelected < visible.length;
  }

  function renderPageButtons(pageCount, focusPage = null) {
    pageNumbers.replaceChildren();
    let focusTarget = null;
    for (let page = 1; page <= pageCount; page += 1) {
      const button = document.createElement("button");
      button.type = "button";
      button.textContent = String(page);
      button.classList.toggle("wsk-current", page === currentPage);
      if (page === currentPage) button.setAttribute("aria-current", "page");
      button.addEventListener("click", () => {
        currentPage = page;
        renderTable(page);
      });
      if (page === focusPage) focusTarget = button;
      pageNumbers.append(button);
    }
    focusTarget?.focus();
  }

  function renderTable(focusPage = null) {
    const filtered = filteredRows();
    const pageCount = Math.max(1, Math.ceil(filtered.length / pageSize));
    currentPage = Math.min(currentPage, pageCount);
    const visible = new Set(visibleRows(filtered));
    rows.forEach((row) => {
      row.hidden = !visible.has(row);
    });
    emptyRow.hidden = filtered.length !== 0;
    result.textContent = `${filtered.length} 条记录 · 第 ${currentPage}/${pageCount} 页`;
    pagePrevious.disabled = currentPage === 1;
    pageNext.disabled = currentPage === pageCount;
    renderPageButtons(pageCount, focusPage);
    updateSelection();
  }

  density?.addEventListener("change", (event) => {
    if (event.target instanceof HTMLInputElement)
      table.dataset.density = event.target.value;
  });
  filter.addEventListener("input", () => {
    currentPage = 1;
    renderTable();
  });
  document.querySelectorAll("[data-sort]").forEach((button) => {
    button.addEventListener("click", () => {
      const key = button.dataset.sort;
      sortDirection =
        sortKey === key && sortDirection === "ascending"
          ? "descending"
          : "ascending";
      sortKey = key;
      rows.sort(
        (a, b) =>
          (a.dataset[key] ?? "").localeCompare(b.dataset[key] ?? "", "zh-CN") *
          (sortDirection === "ascending" ? 1 : -1),
      );
      rows.forEach((row) => tableBody.insertBefore(row, emptyRow));
      document
        .querySelectorAll("th[aria-sort]")
        .forEach((header) => header.setAttribute("aria-sort", "none"));
      button.closest("th")?.setAttribute("aria-sort", sortDirection);
      currentPage = 1;
      renderTable();
    });
  });
  selectAll.addEventListener("change", () => {
    visibleRows().forEach((row) => {
      const checkbox = row.querySelector(".wsk-row-select");
      if (checkbox) checkbox.checked = selectAll.checked;
    });
    updateSelection();
  });
  rows.forEach((row) =>
    row
      .querySelector(".wsk-row-select")
      ?.addEventListener("change", updateSelection),
  );
  pagePrevious.addEventListener("click", () => {
    currentPage = Math.max(1, currentPage - 1);
    renderTable();
  });
  pageNext.addEventListener("click", () => {
    currentPage += 1;
    renderTable();
  });
  renderTable();
}

function initDialog() {
  const dialog = document.getElementById("confirm-dialog");
  if (!dialog) return;

  document
    .querySelector("[data-dialog-open]")
    ?.addEventListener("click", () => dialog.showModal());
  document.querySelectorAll("[data-dialog-close]").forEach((button) => {
    button.addEventListener("click", () => dialog.close());
  });
  dialog.addEventListener("click", (event) => {
    if (event.target === dialog) dialog.close();
  });
}

function initToasts() {
  document.querySelectorAll("[data-toast]").forEach((button) => {
    button.addEventListener("click", () =>
      showToast(button.dataset.toast, button.dataset.toastVariant),
    );
  });
}

initTheme();
initViews();
initReferenceNavigation();
initMenu();
initTabs();
initForms();
initDemoPagination();
initDataTable();
initDialog();
initToasts();
