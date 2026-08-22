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

升级不是直接修改 `upstream.lock`，也不是把新版上游文件复制进 `overlay/`。更新脚本会先
基于旧锁定提交还原完整定制源码，在临时 Git 仓库中合并新上游，最后相对新基线重新计算
Overlay。这样，上游已经吸收的相同修改会自动退出 Overlay，仍有差异的文件继续以完整
文件形式保存。

除“提交更新”一节会明确切回 `wheels` 根目录外，以下命令均在 `sub2api-plus/` 目录执行。

### 1. 更新前准备

开始前确认：

1. `wheels` 当前分支中没有未提交的 `sub2api-plus` 改动；已有定制应先提交。
2. 当前 Overlay 能通过 `./scripts/verify-overlay.sh`，并记录 `upstream.lock` 中的旧提交。
3. `.cache/update` 不存在；该目录是被忽略的完整临时源码，绝不能提交。
4. 已检查本次上游更新范围，尤其是 `backend/migrations/`、配置默认值、启动参数和依赖版本。

先验证当前基线：

```bash
./scripts/verify-overlay.sh ../../aiapi
```

### 2. 获取并合并最新上游

更新到上游 `main` 在执行时的最新提交：

```bash
./scripts/update-upstream.sh prepare main .cache/update
```

`prepare` 会自动完成：

1. 从 `upstream.lock` 的远程仓库克隆并检出旧的固定提交。
2. 应用现有 `overlay/` 和 `deleted-files.txt`，重建完整定制树并创建临时提交。
3. 获取指定的上游 ref，将它解析为精确 commit，并确认它是旧基线的后代。
4. 把新上游合并进完整定制树，将精确 commit 记录在临时仓库配置中。

这里的 `main` 只用于发现最新提交。`finalize` 后，构建仍以写入 `upstream.lock` 的精确
commit 为准，不会直接构建浮动分支。需要升级到指定版本时，也可以把 `main` 换成上游
tag 或 commit。

正式更新建议直接使用 `upstream.lock` 中的远程地址。第三个参数只用于本地仓库加速；
只有确认该本地仓库已经获取了目标 ref 和 commit 时才使用：

```bash
./scripts/update-upstream.sh prepare main .cache/update /path/to/upstream-mirror
```

### 3. 处理并审查合并结果

没有冲突时，`prepare` 会直接完成 merge。先查看新基线和相对新上游仍保留的定制差异：

```bash
new_upstream="$(git -C .cache/update config --get sub2api-plus.newUpstream)"
git -C .cache/update log --oneline --decorate -5
git -C .cache/update diff --stat "$new_upstream"..HEAD
git -C .cache/update diff "$new_upstream"..HEAD
```

如果脚本以退出码 `3` 报告冲突，先列出未解决文件：

```bash
git -C .cache/update status
git -C .cache/update diff --name-only --diff-filter=U
```

冲突中的 `HEAD` 是重建后的现有定制，另一侧是新上游。逐个文件理解新上游意图后再合并：

- 应继续跟随上游的文件，采用新版上游内容。
- 仍需要的公司定制，应基于新版结构重新放入，不能不审查就整文件保留旧版。
- 上游重命名、删除文件或修改数据库迁移时，要同时检查 `deleted-files.txt` 和回滚兼容性。
- 不得把 `.env`、真实域名、IP、凭据或私有运维文档带入更新工作树。

解决后暂存并完成 merge 提交：

```bash
git -C .cache/update add -A
git -C .cache/update \
  -c user.name=sub2api-plus-overlay \
  -c user.email=sub2api-plus-overlay@localhost \
  commit -m "merge: update sub2api upstream"
git -C .cache/update status --short
```

最后一条命令必须没有输出。不要在 merge 尚未完成或工作树仍有未提交修改时执行
`finalize`。

### 4. 重新导出 Overlay

确认 `.cache/update` 中的合并结果正确后执行：

```bash
./scripts/update-upstream.sh finalize .cache/update
```

`finalize` 会拒绝未完成的 merge 或不干净的临时工作树，然后相对新上游重新生成：

- `upstream.lock` 中的固定上游 commit；
- `overlay/` 中相对新上游修改或新增后的完整文件；
- `deleted-files.txt`；
- `overlay-manifest.tsv` 中的基线、目标 tree 和文件对象清单。

脚本最后会自动执行树级校验。`export-exclude.txt` 中的私有运维文件仍会被排除。

### 5. 完整验证

使用远程上游重新组装、测试并构建，确认公司仓库不依赖本地 `aiapi` 工作区：

```bash
./scripts/verify-overlay.sh
./scripts/test-source.sh
./scripts/build-binary.sh
```

其中 `test-source.sh` 负责 Go 测试、前端 lint、类型检查、关键测试和构建；
`build-binary.sh` 生成最终 systemd Linux 发布包并校验其元数据。仍维护 Docker 兼容性时，
再额外执行：

```bash
IMAGE_NAME=sub2api-plus:upgrade-check ./scripts/build-image.sh
```

如果测试失败，应回到 `.cache/update` 修复并提交，然后再次执行 `finalize` 和全部验证，
不要直接修改导出后的 `overlay/`，否则临时完整树与 Overlay 会失去一致性。

发布前还要人工检查：

- 新增或变化的数据库迁移是否可前向执行，旧二进制能否在必要时回滚。
- 新增配置项是否有安全默认值，私有配置系统是否需要同步更新。
- `git diff --stat` 中 Overlay 文件数量是否合理，是否出现意外的大文件或完整上游目录。
- `aipick.md`、`racknerd.md`、`.env` 和任何真实基础设施信息均未进入暂存区。

### 6. 提交更新

只暂存本次升级生成的 `sub2api-plus` 文件；`overlay/` 受目标项目自身 `.gitignore` 影响，
需要保留 `-f`。先回到 `wheels` 根目录：

```bash
cd ..
git add sub2api-plus/upstream.lock \
  sub2api-plus/overlay-manifest.tsv \
  sub2api-plus/deleted-files.txt
git add -f sub2api-plus/overlay
git status --short
git commit -m "升级：更新 sub2api-plus 上游基线"
```

如果升级时同时修改了 README、排除清单或许可证说明，再逐个显式加入；不要使用
`git add .`，避免把 `wheels` 中其他项目的改动混入提交。

提交后可以保留 `.cache/update` 到发布验证完成。确认新版本稳定后再删除该精确目录；
构建产物位于被忽略的 `dist/`，同样不应提交。

### 7. 放弃或重新开始

更新目录是独立的临时 Git 仓库。发生无法接受的冲突时，不要修改当前 Overlay 或强行
`finalize`。可以把 `.cache/update` 重命名留作分析，再用一个不存在的新目录重新执行
`prepare`。只要尚未执行 `finalize`，`wheels/sub2api-plus` 中的锁文件和 Overlay 就不会
被更新；执行过 `finalize` 但尚未提交时，则根据 Git diff 逐个恢复本轮生成文件，不能
影响仓库中其他项目或之前的提交。

## 安全与许可证

- `.env`、真实域名、IP、Token、证书、密码和私钥不得提交。
- Overlay 中的配置只提供脱敏示例，运行时配置由部署系统注入。
- 上游来源和许可证见 [`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md) 与
  [`LICENSE`](LICENSE)。
