(() => {
  const root = document.documentElement;
  const themeToggle = document.querySelector("[data-theme-toggle]");
  const themeColor = document.querySelector('meta[name="theme-color"]');

  function setTheme(theme, persist = true) {
    root.dataset.theme = theme;
    const isDark = theme === "dark";
    const nextLabel = isDark ? "切换到浅色主题" : "切换到深色主题";

    if (themeToggle) {
      themeToggle.setAttribute("aria-label", nextLabel);
      themeToggle.title = nextLabel;
    }

    if (themeColor) {
      themeColor.content = isDark ? "#090e19" : "#f7f8fb";
    }

    if (persist) {
      try {
        localStorage.setItem("wheels-site-theme", theme);
      } catch {}
    }
  }

  setTheme(root.dataset.theme || "light", false);

  themeToggle?.addEventListener("click", () => {
    setTheme(root.dataset.theme === "dark" ? "light" : "dark");
  });

  document.querySelectorAll(".wsk-project-card").forEach((card) => {
    const release = () => card.classList.remove("wsk-is-pressed");

    card.addEventListener("pointerdown", (event) => {
      if (event.button === 0) {
        card.classList.add("wsk-is-pressed");
      }
    });
    card.addEventListener("pointerup", release);
    card.addEventListener("pointercancel", release);
    card.addEventListener("pointerleave", release);
  });

  const year = document.querySelector("[data-year]");
  if (year) {
    year.textContent = String(new Date().getFullYear());
  }
})();
