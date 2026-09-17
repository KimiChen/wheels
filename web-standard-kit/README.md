# Web Standard Kit

面向现代浏览器的零依赖网页基础标准套件，包含标准组件示例、正文排版层，以及数据中台与社区论坛两个参考应用。

零构建：交付物就是 `default.html` / `style.css` / `script.js` 三个文件，直接复制即可。

## 快速开始

```bash
python3 -m http.server 8000 --bind 127.0.0.1
```

启动后访问 <http://127.0.0.1:8000/default.html>。

## 项目结构

| 文件                    | 说明                                                                                                                                                       |
| ----------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `default.html`          | 单页文档，含三个视图：带快速目录的「标准组件」、「数据中台参考」、「社区论坛参考」（帖子列表页与正文页）。顶栏切换，视图状态反映在 URL hash（可收藏 / 深链）。 |
| `style.css`             | 全部样式，统一置于单个 `@layer wsk` 低优先级层；顶部 `:root` 集中定义 128 个设计令牌。                                                                        |
| `script.js`             | 零依赖原生脚本：主题切换、视图路由、菜单、页签、表单校验、分页、集合（筛选 / 排序 / 选择，表格与卡片流通用）、对话框、轻提示。全部为 `document` 级委托，导出 `wsk.mount()`。 |

## 主题

浅深两套值**只定义一次**：每个主题化令牌写成 `light-dark(<浅>, <深>)`，`style.css` 里不存在深色覆盖块。主题由 `:root` 的 `color-scheme` 决定：

```css
:root { color-scheme: light dark; }        /* 无 data-theme：跟随系统 */
:root[data-theme="light"] { color-scheme: light; }
:root[data-theme="dark"]  { color-scheme: dark;  }
```

因此**禁用 JavaScript 时深色仍然生效**，表单控件与滚动条也一并跟随系统。

顶栏按钮是三态循环 `跟随系统 → 浅色 → 深色 → 跟随系统`。「跟随系统」用**移除** `data-theme` 表达，并清除 `localStorage`，所以随时回得去。`<head>` 内联脚本只在存在显式选择时写属性，首帧不闪烁。

`--shadow` / `--shadow-hover` / `--shadow-accent` / `--card-sheen` 的浅深差异含几何（层数、渐变停靠点），`light-dark()` 只能出现在颜色位置，所以它们取两套图层的并集，把另一模式不用的层置为 `transparent`。

> `light-dark()` 按**使用点元素**的 `color-scheme` 解析。给某个元素单独设 `color-scheme: dark`，其子树内所有 `light-dark()` 令牌会整体翻到深色值——这是特性也是陷阱。

## 令牌总览

换肤的契约是这 128 个令牌。调整令牌时需保持名称完整，并核对浅深主题的显示效果与对比度。

| 组        | 数量 | 角色                                                                                                                       |
| --------- | ---- | -------------------------------------------------------------------------------------------------------------------------- |
| 颜色 / 表面 | 16   | `--canvas`、`--surface{,-raised,-soft,-strong}`、`--border{,-strong}`、`--field-border`、`--text`、`--muted{,-soft}`、`--input-bg`、`--disabled-{bg,text}` |
| 品牌 / 强调 | 13   | `--accent{,-hover,-soft}`、`--on-accent`、`--brand`、`--on-brand`、`--link{,-hover}`、`--focus`、`--selection`、`--inverse`、`--on-inverse` |
| 状态      | 14   | `--{success,warning,danger,info}`、同名 `-soft`、同名 `-fill` 与 `--on-*-fill`                                               |
| 类别      | 8    | `--cat-1..4-bg` / `--cat-1..4-text`                                                                                         |
| 代码 / 语法 | 17   | `--code-panel-*`、`--code-block-*`、`--code-inline-*`、`--syntax-{keyword,string,number,name,comment}`                       |
| 字号      | 11   | `--type-2xs` … `--type-3xl`、`--type-display-sm`、`--type-display`                                                           |
| 间距      | 13   | `--space-2` … `--space-48`（按承载的像素值命名，值写 `rem`；套件以 2px 步进）                                                 |
| 圆角      | 5    | `--radius-xs`、`--radius-sm`、`--radius`、`--radius-lg`、`--radius-pill`                                                     |
| 字体      | 2    | `--font-sans`、`--font-mono`                                                                                                 |
| 光影 / 装饰 | 29   | `--shadow-*`、`--card-*`、`--overlay*`、`--grid-dot`、各类 `-glow` / `-wash` / `-tint`                                        |

几点约定：

- **状态色有两个变体。** `--danger` 是文字色；深色下它是浅红，做成实心删除按钮会是整页最亮的元素，所以实心件用 `--danger-fill` + `--on-danger-fill`。四种状态同理。
- **链接是独立角色。** `a` 默认 `color: inherit`，只有 `.wsk-prose` 内的链接取 `--link`。把链接绑在 `--accent` 上，内容型站点每行标题都会读成主行动号召。
- **类别色不是语义色。** 语义色承担不了一组彼此无关的类别——挑四个出来就会有两个都落在红色上。
- **`--border-strong` 与 `--field-border` 不是一回事。** 前者用于分隔线，对比度 1.28–1.31 是刻意的；后者是输入框边界的唯一视觉线索，按 WCAG 1.4.11 取到 3:1 以上。

## 视觉原则

- 采用「精密仪器」气质的理性数据工具风格：画布由顶部双色径向光晕与 24px 超细点阵（向下渐隐）提供质感，全部为纯 CSS，零图片资产。
- 品牌主色为靛蓝（`--accent` 浅色 `#4f46e5`、深色 `#818cf8`；`--brand` 是品牌方块的填充色，与 `--accent` 分开取值），中性色为冷灰蓝 slate 系；深色主题的 `--border` 与 `--*-soft` 采用带透明度的同色光染，叠加在不同表面上自动分层。
- 排印按中西文混排校准：中文标题字距归零、负字距只保留给指标数字；眉题 / 徽章 / 目录题统一为小号加字距的大写标签家族；指标大数字提级为展示字阶并启用等宽数字。
- 层级由光影表达：卡片带顶缘高光与微光泽，指标卡有向右淡出的品牌渐变条与主色晕染，深色主题下主按钮、当前页码、开关等实心主色件带克制的辉光。
- 区块标题以品牌色渐变竖条作扫描锚点；流程与服务卡的序号采用「01 ——」发丝线字印。
- 支持悬停的设备上，指标、普通面板、表格容器和业务卡片共享轻微上浮、边框强调与主题化阴影；表格行仅调整背景，避免内容位移。
- 微交互克制而完整：菜单 / 对话框 / 轻提示有 160–240ms 入场动效，页签下划线从中心生长，骨架屏为定向扫光，状态点带心跳光环；全部动效尊重 `prefers-reduced-motion`。
- 触屏设备通过 Pointer Events 提供短暂按压反馈；表单控件获得焦点时同步强调字段标签与表单容器，使当前编辑上下文更明确。

## 参考应用

两个参考应用取自**同一套令牌**，用来证明这套基线不是只服务于一种场景。

| | 数据中台参考 | 社区论坛参考 |
| --- | --- | --- |
| 外壳 | 左侧导航 + 内容（`.wsk-reference-shell`） | 主体 + 右侧栏（`.wsk-forum-shell`） |
| 页面 | 单页多区块 | 帖子列表页 + 帖子正文页（两个视图） |
| 重心 | 指标、表格、流程、服务编排 | 卡片流列表、正文排版、回复与投票 |

论坛参考顺带检验了几件此前只有演示没有用例的东西：`.wsk-prose` 第一次承载真实长文，
四组类别色第一次用来标注彼此无关的板块，`--link` / `--info` 第一次脱离主操作色，
**同一页面第一次出现两个互不干扰的集合实例**（数据中台的表格与论坛的卡片流）。

### 社区原语

论坛场景需要而数据中台不需要的六个原语，全部由既有令牌构成，未新增任何颜色：

| 类 | 说明 |
| --- | --- |
| `.wsk-avatar` | 纯 CSS 缩写块，四色取自 `--cat-*`，三种尺寸，`.wsk-avatar-stack` 为叠层变体。不引入图片资产。 |
| `.wsk-user-row` | 头像 + 名字 + 元信息。配 `.wsk-user-list` 使用（与 `.wsk-status-list` 共享行外框，但不套用它的文字规则——那会把头像压成块级）。 |
| `.wsk-thread` | 帖子行。悬停只换背景不上浮，与表格行一致，避免指针下的文字位移。 |
| `.wsk-reply` | 回复项，含 `.wsk-reply-quote` 引用变体。作为阅读表面，不给悬停反馈。 |
| `.wsk-vote` | 投票控件，焦点是真实 `outline`，强制高对比度模式下可见。 |
| `.wsk-tag` | 自由标签，中性配色。与 `.wsk-cat`（板块，固定集合固定配色）分工明确。 |

图标新增 11 个：`message`、`thumbs-up`、`pin`、`lock`、`user`、`clock`、`tag`、`search`、
`bookmark`、`arrow-up`、`arrow-down`，共 30 个 symbol。

### 视图与子视图

顶栏按钮通过 `data-view-anchor` 声明自己要落到哪个锚点；没有锚点的按钮就是默认视图
（无 hash 时显示）。视图名为 `<父>-<子>` 的是子视图：帖子正文页是 `forum-post`，
不进顶栏切换，但它显示时「社区论坛参考」按钮保持按下态。

```html
<button data-view-target="forum" data-view-anchor="forum-overview" aria-controls="forum-view">社区论坛参考</button>
...
<div class="wsk-page" id="forum-view" data-view="forum" hidden>…</div>
<div class="wsk-page" id="forum-post-view" data-view="forum-post" hidden>…</div>
```

新视图的根部需要一个 `<h1 tabindex="-1">`（切换时的焦点目标），且全局 id 不得重名——
三个视图共享一个 document。

## 正文排版

把 `.wsk-prose` 放在容器上，其中的 `h2`–`h6`、段落、列表与列表标记、定义列表、引用、分隔线、图片与图注、表格与斑马纹、行内代码与代码块都会自动取到令牌，适合 Markdown 渲染结果直接落进来。

正文代码块跟随主题（浅色下是浅底）；`.wsk-code-panel` 则是刻意在两种主题下都保持深色的点缀件，两者走不同的令牌组。语法高亮用 `.wsk-tok-{keyword,string,number,name,comment}`。

## 渐进增强

`<head>` 内联脚本在首帧前写入 `data-js`，两种布局都不会闪烁。约定是双向的：

- 没有 JavaScript 就不起作用的控件标 `data-js-only`，无 `[data-js]` 时隐藏。
- 静态替代内容标 `data-no-js`，有 `[data-js]` 时隐藏；排序表头用它保留无脚本时的列标题。
- 只有 JavaScript 才会揭示的内容反过来放出来：无 `[data-js]` 时，被标记 `hidden` 的 `[role="tabpanel"]` 与 `[data-view]` 一律显示，避免内容被困在读不到的地方。

## 无障碍

- 关键文本 / 表面配对已按 WCAG AA（≥ 4.5:1）校准；**渐变与叠加层的绘制像素需单独核对**，不能只依据令牌中的平色判断对比度。
- 非文字指示器按 WCAG 1.4.11 取到 3:1 以上，输入框边框有专用的 `--field-border`。
- 提供跳转链接、`:focus-visible` 焦点环、`prefers-reduced-motion` 降级。文本控件的焦点内圈是真实的 `outline` 而非 `box-shadow`。
- 有 `@media (forced-colors: active)` 兜底：强制高对比度模式不绘制 `box-shadow`，所以焦点、校验态、选中态、页签下划线与表格选中行改用 `outline` / `border` / 系统色表达，纯装饰关掉。
- 菜单 / 页签遵循 WAI-ARIA APG 键盘模式；对话框使用原生 `<dialog>` + `showModal`。
- 数据表格首列为 `<th scope="row">` 行表头，`<progress>` 具备可访问名称，图标按钮均有 `aria-label`。
- 轻提示按语义分 `success` / `info` / `warning` / `danger` 四种样式，各有专属令牌。
- 固定顶栏与轻提示区已处理 `env(safe-area-inset-*)`。

## 浏览器支持

面向近两年的现代浏览器。用到以下特性，请以其最低版本作为支持下限：

- `light-dark()`：**Chrome/Edge 123+、Safari 17.5+、Firefox 120+**（全套主题机制依赖它，这是实际下限）
- `@layer`、`color-mix()`、`:has()`：Chrome/Edge 111+、Safari 16.2+、Firefox 113+
- 原生 `<dialog>` + `showModal`：Chrome 37+、Safari 15.4+、Firefox 98+
- `backdrop-filter`（顶栏 / 对话框遮罩，含 `-webkit-` 前缀）：Safari 18 前需要前缀，缺失时优雅降级为近实心背景 / 纯色遮罩
- `mask-image`（画布点阵渐隐，含 `-webkit-` 前缀）：缺失时点阵全屏显示，观感仍可接受
- `text-wrap: balance / pretty`：Chrome 114+/117+、Safari 17.5+，其余浏览器静默忽略（渐进增强）
- `100dvh` 动态视口单位：Chrome 108+、Safari 15.4+

## 复用说明

组件以 `wsk-` 命名空间的类 + 语义化标记为复用单元，直接复制对应的 HTML 片段与 `style.css` 中的同名规则即可。

脚本侧没有需要「复制哪个 `init*`」的问题：**所有监听器一次性绑在 `document` 上**，通过 `closest()` 匹配 `data-*` 契约，不持有任何元素引用。因此 HTMX / Turbo / `hx-boost` 整页替换、视图转场之后，控件不会变成死控件，也不需要重新绑定。同一页面放多个实例同样成立。

片段替换后**唯一需要做的**是让新节点完成初始渲染（标签同步、页码按钮、首屏表格页）：

```js
wsk.mount(newFragment); // 省略参数则为整个 document；幂等，可重复调用
```

`mount()` 包含传入的根节点；仅替换表格 `tbody` 等组件内部片段时也会刷新所属组件。
局部或重复全量挂载会保留滚动位置，只有首次加载与 URL hash 导航会滚动到锚点。

例如配合 HTMX：

```js
document.body.addEventListener("htmx:afterSwap", (event) => wsk.mount(event.target));
```

组件的 `data-*` 契约：

| 组件     | 契约                                                                                                                                  |
| -------- | --------------------------------------------------------------------------------------------------------------------------------------- |
| 主题按钮 | `[data-theme-toggle]`                                                                                                                  |
| 视图路由 | `[data-view="<name>"]` + `[data-view-target="<name>"]`                                                                                  |
| 菜单     | `.wsk-menu-wrap` > `[data-menu-button][aria-controls]` + `[role="menu"]`                                                                |
| 页签     | `[data-tabs]` > `[role="tab"][aria-controls]` + `[role="tabpanel"]`                                                                    |
| 表单     | `[data-demo-submit]`（可选 `[data-demo-submit-variant]`）、`[data-password]`、`[data-code-input]` + `[data-code-error]` / `[data-code-success]`（配对规则见下） |
| 集合（表格 / 卡片流） | 见下节「集合」                                                                                                             |
| 分页     | `[data-demo-pagination]` 内的 `[data-page]`、`[data-page-action]`、`[data-page-status]`                                                 |
| 对话框   | `[data-dialog-open="<dialog-id>"]` + `[data-dialog-close]`                                                                              |
| 轻提示   | `[data-toast]`（可选 `[data-toast-variant]`），容器 `#toast-region`                                                                     |
| 视图路由 | `[data-view-target]` + 可选 `[data-view-anchor]`；子视图命名为 `<父>-<子>`                                                              |

### 集合：筛选、排序、分页

同一套机制既服务表格也服务卡片流，**不假设 `<table>` 结构**。唯一的结构性要求是：
空态元素要与行处在同一容器内，排序重插行时用它作插入锚点。

| 角色 | 表格写法 | 卡片流写法 |
| --- | --- | --- |
| 作用域（含控件与集合） | `[data-table-scope]` | `[data-list-scope]` |
| 集合根 | `[data-table]` | `[data-list]` |
| 行容器 | `<tbody>`（默认） | `[data-rows]` |
| 行 | `<tr data-row>` | 任意 `[data-row]` 元素 |

两者共用的控件属性：`[data-table-filter]`、`[data-sort]`（排序键取行上的 `data-<key>`，
可选 `[data-sort-label]`）、`[data-table-result]`、`[data-table-empty]`、
`[data-table-page-action="previous|next"]`、`[data-table-page-numbers]`、
`[data-table-select-all]` + `.wsk-row-select`、`[data-table-selection]`、`[data-density]`。

集合根上可选：`data-page-size`（默认 3）、`data-unit`（结果文案的量词，默认「条记录」）。

排序状态按容器分流：表格挂在 `<th>` 的 `aria-sort` 上；卡片流的排序控件是切换按钮，
状态挂在按钮的 `aria-pressed` 上，方向写进 `aria-label`。排序比较用
`localeCompare(..., { numeric: true })`，所以数值列不会被排成 12 < 7。

校验字段与它的提示是**按输入框逐个配对**的：优先取最近的 `.wsk-field` 祖先，其次取
`aria-describedby` 指向的元素。同一表单里放多个 `[data-code-input]` 时，两者必须至少
有一个成立——否则无法判断哪条提示属于哪个输入框，该字段会被跳过而不是错配。

组件状态存在 `dataset` 上（集合的页码、排序键与方向；分页的页码），可见、可检查，元素被替换时自然重置。
