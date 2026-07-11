# Development

## 环境

- Node.js 25 或更高版本
- npm 11 或兼容当前 `package-lock.json` 的 npm 版本

首次安装或验证干净依赖树时使用：

```bash
npm ci
```

只有在明确更新依赖及 lockfile 时才使用 `npm install`。

## 项目结构

- `bin/`：CLI 可执行入口。
- `src/cli.js`：命令、参数和交互流程。
- `src/scan.js`、`src/identity.js`：缓存扫描与离线身份线索。
- `src/decode.js`：反解计划、输出目录保护和流程编排。
- `src/decoder/wxapkg/`：V1MMWX 解密、格式校验和安全解包。
- `src/decoder/core/`：从 wedecode 移植并修改的离线反编译核心。
- `test/`、`test-support/`：单元、集成和合成 wxapkg fixture。

## 隔离模型

包内编译后 JavaScript 不应在拥有文件写权限的进程中执行。完整反编译按以下边界运行：

1. 父进程检查全部输入包和输出目录。
2. 父进程在清空旧输出前检查 worker 隔离能力、准备自定义 polyfill 私有快照，并校验全部读取授权路径。
3. 父进程通过严格 wxapkg 解析器解密并安全解包。
4. 只读 worker 分析包内代码，把文件变更记录为内存操作清单。
5. 父进程把该清单视为不可信输入，重新校验路径、类型、数量和总大小后再写入。
6. 父进程写文件时拒绝符号链接和越界路径。

worker 不获得文件写入、网络、子进程或嵌套 worker 权限。`node:vm` 只提供执行上下文和单次代码超时，不被视为安全边界；真正的主机边界来自只读进程权限和父进程的清单校验。

当前 manifest 应用不是事务：磁盘 I/O 故障或反编译失败可能留下部分新输出，使用 `--clear` 时也不会自动恢复旧目录。输出目录应位于当前用户独占、不会被其他进程并发改名或替换链接的位置；独立的同用户文件系统竞争者不在当前隔离边界内。

修改这条链路时，至少保留以下回归覆盖：

- 路径穿越、绝对路径、Windows 路径别名和大小写冲突；
- 输出目录及内部路径的符号链接；
- 包内代码尝试写出输出目录、联网、启动进程或伪造 worker 完成消息；
- 包内代码尝试创建外部符号链接或向父进程发送信号；
- `--clear` 前的包校验、polyfill 快照和 worker 读取授权路径校验；
- worker 超时、异常退出和不完整操作清单。

## 上游代码

`src/decoder/core/` 基于固定的 wedecode 版本。修改该目录时：

1. 保留文件头中的 SPDX 和来源说明。
2. 在 `THIRD_PARTY_NOTICES.md` 与 `src/decoder/core/SOURCE.md` 记录实质修改。
3. 不重新引入上游 CLI、联网查询、更新检查或 workspace 服务。
4. 单独许可的 vendored 文件必须保留其许可证，不应笼统改为 GPL。

## 提交前检查

```bash
npm run check
npm audit --omit=dev
npm pack --dry-run
```

`npm pack --dry-run` 会通过 `prepack` 自动重复执行 `npm run check`。检查打包清单时应确认：

- 包含 `bin/`、`src/`、运行时脚本、README、变更记录和全部许可证；
- 不包含 `node_modules/`、`decoded/`、测试 fixture、TODO、日志、环境文件或 `.tgz`；
- `bin/wxapkg-helper.js` 保持可执行权限。

真实样例只用于本地验证，不得把缓存路径、真实 appid、包文件或反编译产物提交到公开仓库。
