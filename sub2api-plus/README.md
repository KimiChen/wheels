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
| `scripts/build-binary.sh` | 组装源码并生成带版本信息和校验清单的 Linux 发布产物 |
| `scripts/systemd-release.sh` | 在目标服务器发布或回滚 systemd 二进制版本 |
| `scripts/build-image.sh` | 可选的 Docker 镜像构建入口，不是当前生产部署方式 |
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

### 构建 systemd 发布产物

当前生产环境不使用 Docker。以下命令会组装固定上游与 Overlay，安装锁定的
`pnpm@9.15.9`，构建前端，再把前端嵌入 Linux amd64 Go 二进制：

```bash
./scripts/build-binary.sh ../../aiapi
```

本地仓库参数只用于加速。CI 和正式构建不应传该参数，以证明产物能独立从
`upstream.lock` 声明的远程来源生成。默认产物位于 `dist/<release-id>/`：

```text
dist/<release-id>/
├── sub2api
├── resources/
├── release.env
└── SHA256SUMS
```

`release.env` 记录应用版本、定制分支提交、固定上游提交、组装 Git tree、构建时间和
目标架构。Overlay 组装目录相对上游必然有工作区差异，所以构建使用
`-buildvcs=false`，并通过 ldflags 显式写入 `overlay-manifest.tsv` 的
`source_commit`；不要把上游基线提交误当成定制版本。

可用环境变量 `BINARY_GOOS`、`BINARY_GOARCH`、`RELEASE_ID` 和 `OUTPUT_DIR` 覆盖默认值。
默认值见 [`.env.example`](.env.example)。

### 发布到已有 systemd 服务

目标服务器需要已有 `sub2api.service`，其 `ExecStart` 指向稳定入口
`/opt/sub2api/sub2api`，运行目录为 `/opt/sub2api`。实际 unit、数据库连接、域名、IP
和 `/etc/sub2api` 下的配置属于私有运维配置，不进入本仓库。
服务器还需提供 systemd、GNU coreutils、`flock`、`rsync`、`curl`、`file` 和包含
`pg_restore` 的 PostgreSQL 客户端。

发布前必须完成：

1. 确认工作树、定制提交和 `upstream.lock` 基线正确，并检查新增数据库迁移和配置变化。
2. 创建 PostgreSQL custom-format 备份，用 `pg_restore -l` 验证，并记录大小和 SHA-256。
3. 确认目标服务器磁盘足以保存新版本、资源备份和回滚版本。
4. 把完整的 `dist/<release-id>/` 上传到服务器的临时目录，不要只上传二进制。

在目标 Linux 服务器的本项目目录执行：

```bash
ARTIFACT_DIR=/path/to/staging/release-id
sudo DATABASE_BACKUP_FILE=/path/to/backup/sub2api.dump \
  ./scripts/systemd-release.sh deploy "$ARTIFACT_DIR"
```

示例中的 `/path/to/...` 是占位路径。脚本默认拒绝在没有可读数据库备份时发布；只有在
已经由外部系统完成同等门禁时，才能显式设置 `SKIP_DATABASE_BACKUP_CHECK=1`。

脚本会依次校验 SHA-256、Linux ELF 和 CPU 架构，验证数据库备份和磁盘空间，将产物
安装到 `/opt/sub2api/releases/<release-id>/`，必要时备份并同步
`/opt/sub2api/resources/`，原子切换 `/opt/sub2api/sub2api`，重启服务，然后检查：

- `sub2api.service` 为 `active`，且本次启动的 `NRestarts=0`；
- `http://127.0.0.1:8080/status` 返回 `{"status":"perfectly nice"}`；
- 失败时自动恢复上一个二进制和资源，再重启旧版本。

脚本通过 `flock` 阻止并发发布。状态和资源备份保存在
`/opt/sub2api/.release-state/`。发布成功后还应从外部检查公开 `/status` 和首页为
HTTP 200、静态资源可加载、未授权 `/v1/responses` 为 HTTP 401，并查看服务日志中是否
出现新的 error、panic 或 fatal。

### 回滚和保留

回滚到上一次发布记录：

```bash
sudo ./scripts/systemd-release.sh rollback
```

也可以显式指定仍保留在 `releases/` 下的版本：

```bash
sudo ./scripts/systemd-release.sh rollback release-id
```

回滚同样会原子切换、重启并执行本机健康检查；失败时尝试恢复回滚前版本。脚本不会自动
恢复数据库。只有确认新迁移与旧程序不兼容时，才应先保全当前数据库，再经过人工审批
恢复发布前备份。

至少保留当前和上一个二进制版本，以及最近两份已经验证的数据库备份。旧版本和备份不由
脚本自动删除，确认运行稳定并满足保留期后再人工清理。

### 可选 Docker 镜像

Dockerfile 只作为兼容和本地验证入口保留：

```bash
IMAGE_NAME=sub2api-plus:local ./scripts/build-image.sh ../../aiapi
```

它不替代上述 systemd 二进制发布和回滚流程。

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
完整测试和 systemd 发布二进制构建；如仍维护 Docker 兼容性，再额外构建镜像。最后把
锁文件、Overlay 和文档放在同一个提交中。

## 安全与许可证

- `.env`、真实域名、IP、Token、证书、密码和私钥不得提交。
- Overlay 中的配置只提供脱敏示例，运行时配置由部署系统注入。
- 上游来源和许可证见 [`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md) 与
  [`LICENSE`](LICENSE)。
