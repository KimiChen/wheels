# wxapkg-helper

一个 Node.js CLI，用来扫描本机微信小游戏/小程序缓存里的 `wxapkg`，从候选目录里选择目标，并使用项目内置引擎解密、解包和反编译。运行时不需要安装或调用 `wedecode` 命令。

> 仅用于你拥有权利或已获授权的代码审计、恢复和学习场景。请遵守相关法律与平台协议。

## 安装

需要 Node.js 25 或更高版本。项目使用该版本的 Node 权限模型隔离包内代码分析进程。

```bash
npm ci
```

需要在本机直接使用 `wxapkg-helper` 命令时，可在项目目录执行 `npm link`；日常开发仍建议通过下面的 npm scripts 运行。

## 交互式运行

```bash
npm start
```

流程：

1. 扫描常见微信缓存目录。
2. 列出发现的 `wxapkg` 候选目录，并根据游戏标识 ID（appid）、小游戏名称、本地路径和修改时间生成识别线索。
3. 选择目录。
4. 确认输出目录和参数。
5. 在受限的子进程中运行项目内置反编译引擎。

## 只扫描

```bash
npm run scan
```

指定扫描根目录：

```bash
node ./bin/wxapkg-helper.js scan --root "/path/to/wechat/cache"
```

按游戏标识 ID（appid）或修改时间过滤，并可选控制展示数量：

```bash
node ./bin/wxapkg-helper.js scan --appid wx0123456789abcdef --since 2026-07-10 --limit 20
```

默认显示全部匹配候选；传 `--limit <n>` 时只显示最近的 `n` 个，`--limit 0` 也表示显示全部。

输出 JSON：

```bash
node ./bin/wxapkg-helper.js scan --json
```

JSON 的每个候选目录会包含 `identity` 字段，常用字段包括：

- `gameId` / `appid`：从路径或本地 metadata 中识别到的游戏标识 ID，不等同于游戏名称。
- `gameIdSource` / `appidSource`：标识 ID 的来源。
- `name` / `nameSource`：从候选目录、有限层级的祖先目录，以及识别到的 appid 目录浅层 metadata 中提取疑似名称；不会直接把 `app_data`、`radium` 等通用缓存路径段当作名称。
- `displayName`：识别到的游戏名称；找不到名称时为 `null`。
- `pathHint`、`latestMtimeMs`、`latestPackage`：辅助判断目标是否匹配的路径和修改时间线索。

## 直接反解指定路径

```bash
node ./bin/wxapkg-helper.js decode "/path/to/pkg-dir" --out "./decoded/game-a" --clear
```

也可以传单个包：

```bash
node ./bin/wxapkg-helper.js decode "/path/to/__APP__.wxapkg" --out "./decoded/game-a"
```

反解前预览包清单：

```bash
node ./bin/wxapkg-helper.js decode "/path/to/pkg-dir" --list-packages
```

## 常用参数

- `--root <path>`：指定扫描根目录，可重复传入。
- `--max-depth <n>`：最大递归深度，默认 `14`。
- `--include-public-lib`：候选列表中显示只有 `publicLib.wxapkg` 的目录；反解目录时也将公共库纳入包清单。
- `--appid <wxid>`：扫描选择时只显示指定游戏标识 ID/appid 的候选目录。
- `--since <date>`：扫描选择时只显示指定日期之后修改过的候选目录，例如 `2026-07-10`。
- `--limit <n>`：最多显示最近的候选数量，默认显示全部，`0` 表示全部。
- `--list-packages`：用于 `decode`，只预览目标目录包清单，不执行反解。
- `--out <path>`：输出目录。
- `--clear`：在执行前清空旧产物。
- `--open-dir`：完成后打开输出目录。
- `--px`：使用 `px` 而不是 `rpx` 解析 CSS。
- `--unpack-only`：只解密和解包，不反编译。
- `--wxid <wxid>`：指定加密包所需的微信小程序 WXID；未指定时会尝试从路径识别。
- `--dry-run`：只打印内置引擎的执行计划和包清单。
- `--yes`：跳过确认。

如果通过交互式扫描选择候选目录且没有显式传 `--out`，默认输出目录会优先使用识别到的小游戏名称，其次使用游戏标识 ID/appid，最后才回退到路径片段。

安全提示：

- 真实反解前会显示授权用途确认。
- 输出目录已存在且未传 `--clear` 时，交互模式会提示继续写入、换目录或取消。
- 传入 `--clear` 且输出目录已存在时，交互模式会二次确认清空风险。
- `--dry-run` 会显示输入路径、输出路径、处理模式、权限隔离状态和完整包清单，不执行反编译。
- 反解成功后会打印输出目录关键文件统计，例如 `game.json`、`app-config.json`、`game.js` 和资源数量。
- 反解失败时保留内置引擎错误，并追加格式、解密、路径和超时排查建议。
- 默认拒绝包内 `..`、绝对路径、Windows 盘符、UNC 路径、NUL、备用数据流、保留设备名、大小写冲突和符号链接逃逸。
- 会拒绝对根目录、用户主目录、当前项目目录，以及包含输入包的输出目录执行 `--clear`。

## 内置引擎与隔离

离线反编译核心基于 [`wedecode v0.10.6`](https://github.com/biggerstar/wedecode/tree/v0.10.6) 源码移植和修改，固定对应提交 `9bf626224511a42f77e4b682756472e06953854e`。本项目不包含上游 CLI、UI、workspace 服务器、更新检查或小程序信息联网查询。

解密和解包由父进程通过严格格式解析器完成。完整反编译需要分析部分包内编译后 JavaScript，该步骤在独立 Node.js worker 中运行，并设置总超时和内存上限。worker 强制启用 Node 权限模型，只允许读取内置引擎、依赖、已解包目录、输入包和经过检查的自定义 polyfill，不授予文件写入、网络、子进程或嵌套 worker 权限。

worker 的文件变更先记录为内存 manifest；父进程把 manifest 当作不可信输入，重新检查字段、路径、大小、操作数量、符号链接和硬链接后才写入输出目录。包旁可选的 `polyfill/` 目录也必须是没有符号链接或特殊文件的真实目录，父进程会先复制只读快照，再授权 worker 读取快照。完整反编译会在 `--clear` 修改旧输出前检查 Node 隔离能力、准备 polyfill 快照并校验全部 worker 读取授权路径。

完整反编译会把微信保留的 `__plugin__/` 目录映射为开发者工具可用的 `plugin_/`；`--unpack-only` 保留包内原始目录名。

这些保护不是处理不可信代码的完美安全边界。仍应只处理你拥有权利或已获授权的包。

## 默认扫描位置

脚本会根据系统尝试扫描常见目录。由于微信客户端版本和安装方式会改变缓存位置，如果默认扫描没找到，建议手动传入目录：

```bash
node ./bin/wxapkg-helper.js scan --root "你的微信缓存目录"
```

Windows 常见方向包括：

- `%APPDATA%/Tencent/xwechat`
- `%APPDATA%/Tencent/WeChat`
- `%LOCALAPPDATA%/Tencent/WeChat`
- `%USERPROFILE%/Documents/WeChat Files/Applet`
- `%USERPROFILE%/Documents/WeChat Files`

macOS 常见方向包括：

- `~/Library/Containers/com.tencent.xinWeChat/Data/Library/Application Support/com.tencent.xinWeChat`
- `~/Library/Containers/com.tencent.xinWeChat/Data/Documents/app_data/radium/users`
- `~/Library/Containers/com.tencent.xinWeChat/Data/Documents/xwechat_files`
- `~/Library/Containers/com.tencent.xinWeChat/Data/Documents/WeChat Files`
- `~/Library/Containers/com.tencent.WeChat/Data/Library/Application Support`
- `~/Library/Application Support/com.tencent.xinWeChat`
- `~/Library/Application Support/WeChat`

Linux 常见方向包括：

- `~/.config/WeChat`
- `~/.config/Tencent/WeChat`
- `~/Documents/WeChat Files`

## 说明

除单独标注的 Babel runtime helper 使用 MIT 许可证外，本项目与上游 `wedecode` 均使用 `GPL-3.0-or-later` 许可证。移植范围、上游版本和修改说明见 `THIRD_PARTY_NOTICES.md`、`LICENSES/BABEL-MIT.txt` 与 `src/decoder/core/SOURCE.md`。

变更记录见 `CHANGELOG.md`，开发与提交检查见 `DEVELOPMENT.md`，许可证见 `LICENSE`。

## 测试

```bash
npm test
npm run check
```

检查成功时测试摘要中的 `fail` 应为 `0`。`npm pack --dry-run` 会先自动执行同一套检查。

真实流程使用匿名化的已授权小游戏缓存验证：主包加三个分包共生成 139 个文件；两次独立运行的相对路径和逐文件 SHA-256 完全一致。真实包、appid、缓存路径和反编译产物不会进入仓库。
