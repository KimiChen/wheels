# sub2api-plus

`sub2api-plus` 是基于上游 Sub2API 的完整文件 Overlay 子项目。仓库不保存完整上游
源码，只保存固定上游版本、修改或新增后的完整文件、删除清单以及组装、测试、构建
和升级工具。

## 上游基线

| 项目 | 值 |
|---|---|
| 上游仓库 | `https://github.com/Wei-Shaw/sub2api.git` |
| Ref | `main` |
| 源码 Commit | 见 `upstream.lock` 的 `commit` 字段 |
| 上游许可证 | LGPL-3.0 |

机器可读值以 [`upstream.lock`](upstream.lock) 为准。构建必须使用其中的固定提交，
不能直接构建浮动的 `main`。

## 目录

| 路径 | 用途 |
|---|---|
| `overlay/` | 相对上游新增或修改后的完整文件，保持上游仓库中的原路径 |
| `overlay-manifest.tsv` | 导出基线、目标树和 Overlay 文件对象清单 |
| `deleted-files.txt` | 相对上游需要删除的路径 |
| `export-exclude.txt` | 因公开仓库安全要求而不进入 Overlay 的精确路径 |
| `scripts/export-overlay.sh` | 从完整 Git 工作仓库重新导出 Overlay |
| `scripts/prepare-source.sh` | 克隆固定上游版本并组装完整源码 |
| `scripts/verify-overlay.sh` | 校验清单、忽略规则和组装后的 Git tree |
| `scripts/test-source.sh` | 在临时组装源码上执行项目测试和构建 |
| `scripts/build-image.sh` | 在临时组装源码上构建 Docker 镜像 |
| `scripts/update-upstream.sh` | 重建完整定制分支并合并新版上游 |
| `tests/` | Overlay 专项测试说明和后续测试资源 |
| `packaging/` | 公司环境部署和发布配置；不得提交密钥 |

构建缓存放在系统临时目录或本项目被忽略的 `.cache/`，构建产物放在被忽略的
`dist/`。不要提交完整上游源码、嵌套 `.git` 或 Git submodule。

## 日常命令

从完整维护仓库导出已提交差异：

```bash
./scripts/export-overlay.sh ../../aiapi kimi-next upstream/main
```

该命令只读取指定 Git ref，不会包含完整仓库中未提交的工作区修改。
Overlay 自身包含目标项目的 `.gitignore`，因此首次提交或重新导出后应使用
`git add -f sub2api-plus/overlay`；`verify-overlay.sh` 会区分目标项目自己的忽略规则
与 `wheels` 仓库级忽略规则。

当前 `aipick.md` 和 `racknerd.md` 包含真实基础设施与历史运维信息，且不参与应用
构建，因此通过 `export-exclude.txt` 明确排除。相关资料只能存放在私有运维系统中。

验证 Overlay；第二个参数省略时从上游 URL 克隆，开发时可传本地镜像仓库加速：

```bash
./scripts/verify-overlay.sh ../../aiapi
```

组装一份完整源码：

```bash
./scripts/prepare-source.sh .cache/source ../../aiapi
```

运行前后端测试和构建：

```bash
./scripts/test-source.sh ../../aiapi
```

测试脚本会在临时目录安装项目约定的 `pnpm@9.15.9`，不使用或修改系统全局 pnpm。
后端 Go 测试始终执行；本机存在 `golangci-lint` 时同时运行后端 lint。CI 可设置
`REQUIRE_GOLANGCI_LINT=1`，在缺少该工具时直接失败。

构建镜像：

```bash
IMAGE_NAME=sub2api-plus:local ./scripts/build-image.sh ../../aiapi
```

CI 和正式构建不应传本地仓库参数，以确保能够独立从 `upstream.lock` 声明的远程
来源完成构建。

## 上游升级

升级不是简单修改 `upstream.lock` 后继续覆盖。使用两阶段流程：

```bash
./scripts/update-upstream.sh prepare main .cache/update
```

脚本会基于旧上游重建完整定制源码，并 merge 新上游。如果发生冲突，在输出的更新
工作树中解决、暂存并提交，然后执行：

```bash
./scripts/update-upstream.sh finalize .cache/update
```

`finalize` 会相对新上游重新导出完整 Overlay、更新锁文件并做树级校验。随后仍须运行
完整测试和镜像构建，再把锁文件、Overlay 和文档放在同一个提交中。

## 安全与许可证

- `.env`、真实域名、IP、Token、证书、密码和私钥不得提交。
- Overlay 中的配置只提供脱敏示例，运行时配置由部署系统注入。
- 上游来源和许可证见 [`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md) 与
  [`LICENSE`](LICENSE)。
