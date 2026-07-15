# Web Standard Kit

面向现代浏览器的零依赖网页基础标准套件，包含标准组件示例和数据中台参考应用。

## 快速开始

```bash
python3 -m http.server 8000 --bind 127.0.0.1
```

启动后访问 <http://127.0.0.1:8000/default.html>。

## 项目结构

| 文件           | 说明                                                                                                                   |
| -------------- | ---------------------------------------------------------------------------------------------------------------------- |
| `default.html` | 单页文档，包含「标准组件」示例视图与「数据中台参考」应用视图，通过顶栏切换，视图状态反映在 URL hash（可收藏 / 深链）。 |
| `style.css`    | 全部样式，统一置于单个 `@layer wsk` 低优先级层；顶部 `:root` / `[data-theme="dark"]` 定义浅色与深色两套设计令牌。      |
| `script.js`    | 零依赖原生脚本：主题切换、视图路由、菜单、页签、表单校验、分页、数据表格（筛选 / 排序 / 选择）、对话框、轻提示。       |

## 主题与设计令牌

- 颜色、圆角、阴影等均以 CSS 自定义属性集中定义在 `style.css` 顶部，浅色在 `:root`、深色在 `[data-theme="dark"]`。
- 主题在首帧前由 `default.html` `<head>` 内联脚本按 `localStorage` 与 `prefers-color-scheme` 设定，避免深色首屏闪烁；顶栏按钮可切换并持久化。
- 代码面板等有意固定为深色的组件也走 `--code-*` 令牌，便于统一调整。

## 无障碍

- 提供跳转链接、`:focus-visible` 焦点环、`prefers-reduced-motion` 降级。
- 菜单 / 页签遵循 WAI-ARIA APG 键盘模式；对话框使用原生 `<dialog>` + `showModal`。
- 数据表格首列为 `<th scope="row">` 行表头，`<progress>` 具备可访问名称，图标按钮均有 `aria-label`。
- 轻提示按语义分 `success` / `info` / `warning` / `danger` 四种样式。

## 浏览器支持

面向近两年的现代浏览器。用到以下特性，请以其最低版本作为支持下限：

- `@layer`、`color-mix()`、`:has()`：Chrome/Edge 111+、Safari 16.2+、Firefox 113+
- 原生 `<dialog>` + `showModal`：Chrome 37+、Safari 15.4+、Firefox 98+
- `backdrop-filter`（顶栏，含 `-webkit-` 前缀）：Safari 18 前需要前缀，缺失时优雅降级为近实心背景
- `100dvh` 动态视口单位：Chrome 108+、Safari 15.4+

## 复用说明

组件以 `wsk-` 命名空间的类 + 语义化标记为复用单元，直接复制对应的 HTML 片段、`style.css` 中的同名规则与 `script.js` 中的 `init*` 函数即可。注意：`initMenu` / `initDataTable` / `initDialog` 目前按固定 id 绑定，仅支持单实例；若需在同一页面放置多个实例，请参照 `initTabs` / `initToasts` 改为 `data-*` 属性 + `forEach` 的多实例写法。
