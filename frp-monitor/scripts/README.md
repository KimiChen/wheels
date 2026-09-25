# Scripts

上游源码准备、扩展映射、构建与验证入口。所有脚本 `bash` 执行，依赖 git、tar、
go、python3；应用补丁时另需 patch。

| 入口 | 用途 |
|---|---|
| `prepare-source.sh <输出目录> [镜像]` | 按 `upstream.lock` 浅抓 tag、双重校验 tag 对象与 commit、`git archive` 展开、零 fuzz 应用 `patches/series` |
| `assemble.sh <源码树>` | 把本仓 `agent/`、`monitor/`、`shared/`、`web/` 映射到源码树 `extension/frpmonitor/`，拒绝软链接 |
| `build.sh` | 准备（带 `.cache` 缓存）→ 映射 → 构建基线 frpc/frps 到 `dist/` → 全量编译验证扩展包 |
| `verify.sh` | 交付物检查、`bash -n`、gofmt、JSON 合法性、原生基线构建与受影响包单测、扩展契约测试、linux amd64/arm64 交叉编译 |

本地目录与镜像地址从 `.env` 读取（见 `.env.example`，键白名单见 `lib.sh`）；
`.env` 不提交，也不允许出现白名单以外的键。

约定：补丁必须在 `upstream.lock` 固定提交上干净应用；准备后的源码树不得包含嵌套
`.git`；输出目录已存在时一律拒绝覆盖。
