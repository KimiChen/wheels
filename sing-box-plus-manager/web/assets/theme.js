// 首屏 CSS 之前同步执行，避免主题闪烁（README §4.11）。
//
// 刻意做成外部文件而不是内联 <script>：控制台的 CSP 是 script-src 'self'，
// 页面内不允许任何内联脚本。代价是多一次请求，换来不必开 'unsafe-inline'。
//
// 没有存储选择时不写 data-theme，让 style.css 的 light-dark() 跟随系统——
// 这样禁用 JavaScript 时深色仍然成立。
document.documentElement.dataset.js = "true";
try {
  // 与当前复用的 kit 主题按钮使用同一个键，页面跳转后保留选择。
  const saved = localStorage.getItem("web-standard-kit-theme");
  if (saved === "light" || saved === "dark") {
    document.documentElement.dataset.theme = saved;
  }
} catch {
  // 隐私模式或站点数据被清空：按跟随系统处理即可。
}
