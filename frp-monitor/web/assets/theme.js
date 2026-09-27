// Preserve the kit's theme key and system/light/dark contract before first paint.
(() => {
  document.documentElement.dataset.js = "true";
  try {
    const theme = localStorage.getItem("web-standard-kit-theme");
    if (theme === "light" || theme === "dark") document.documentElement.dataset.theme = theme;
  } catch { /* A disabled storage API must not prevent the page from rendering. */ }
})();
