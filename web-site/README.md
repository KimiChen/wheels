# web-site

\`web-site\` 是 Wheels 仓库的官方网站的仓库。页面以
[Web Standard Kit](../web-standard-kit/README.md) 为基础，采用 Bento 卡片布局；每张卡片介绍一个
子项目，并整卡链接到对应的 GitHub 目录。

## 本地预览

在仓库根目录执行：

\`\`\`bash
python3 -m http.server 8000 --bind 127.0.0.1 --directory web-site
\`\`\`

然后访问 <http://127.0.0.1:8000/>。

这是零依赖静态网站，不需要构建步骤或运行时 \`.env\` 配置。

## 项目结构

| 文件 | 说明 |
| --- | --- |
| \`index.html\` | 页面语义结构、项目卡片内容与 SVG 图标符号。 |
| \`style.css\` | 基于 WSK 设计令牌的深浅主题、Bento 网格、响应式布局与交互状态。 |
| \`script.js\` | 主题切换、年份更新和触屏按压反馈。 |
| \`favicon.svg\` | Wheels 矢量站点图标。 |

## 更新项目

项目列表直接写在 \`index.html\` 的 \`#projects\` 区域中。添加或修改卡片时：

1. 简介以对应子项目根目录的 \`README.md\` 为准。
2. 链接使用 \`https://github.com/KimiChen/wheels/tree/main/<project-id>\`。
3. 为卡片保留可见的项目名称、简述、技术或状态信息，以及说明新窗口行为的
   \`aria-label\`。
4. 根据卡片内容量选择 \`wsk-card-featured\`、\`wsk-card-tall\`、
   \`wsk-card-banner\`、\`wsk-card-wide\` 或 \`wsk-card-small\`。

## 设计与无障碍

- 沿用 Web Standard Kit 的 \`wsk-\` 命名空间、设计令牌和首帧主题脚本。
- 支持系统深浅主题、手动切换与本地持久化。
- 提供跳转链接、语义化标题、整卡焦点态和清晰的新窗口标签。
- 在触屏设备上提供按压反馈，并对 \`prefers-reduced-motion\` 自动降级。
- 断点下依次收敛为双列和单列布局，适配桌面、平板与手机。

## 基础检查

\`\`\`bash
node --check web-site/script.js
python3 -m http.server 8000 --bind 127.0.0.1 --directory web-site
\`\`\`
