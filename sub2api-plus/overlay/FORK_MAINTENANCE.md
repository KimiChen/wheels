# Fork 维护约定

本仓库采用“双分支”模型，避免 GitHub `Sync fork` 与本地定制长期互相制造冲突：

- `main`：上游镜像，只跟踪 `Wei-Shaw/sub2api` 的 `main`，不放定制提交。
- `kimi`：实际定制与部署分支，通过 merge 吸收 `upstream/main`。

## 日常同步

在 `kimi` 分支执行：

```bash
git switch kimi
backend/scripts/sync_upstream.sh --preflight-only
backend/scripts/sync_upstream.sh
git push origin kimi
```

如果预检提示“需评审”，先查看脚本打印的 patch，移植或确认相关上游变化，再执行：

```bash
backend/scripts/sync_upstream.sh --ack-upstream-review
```

如果预检提示冲突，工作区不会被修改。先评估冲突；确实要进入人工解决流程时，显式加 `--allow-conflicts`。

## 更新镜像分支

只有在 `main` 没有 fork 定制提交时才更新它：

```bash
git fetch upstream
git switch main
git merge --ff-only upstream/main
git push origin main
git switch kimi
```

禁止直接在 `main` 开发、cherry-pick 或解决定制冲突。首次建立镜像关系前，必须先把现有定制历史完整保存在并推送到 `kimi`。

## 降低冲突的代码约定

- 上游公共文件尽量保持原样；定制放在 fork 专用入口、配置或新文件。
- `frontend/vite.config.ts` 与 `frontend/src/main.ts` 跟随上游；定制分别放在 `vite.fork*.ts` 与 `app-main.ts`/`index-main.ts`。
- 不修改公共 `DataTable` API；页面如需关闭虚拟化，使用 `virtualize-threshold`。
- `.gitattributes` 标记的 fork 接管文件会保留本仓库版本，但每次同步都必须评审对应上游 patch。
- Ent 生成物不手工合并；schema 合并后重新生成。

## 冲突处理后

人工解决过的冲突会被 Git `rerere` 记录。本仓库启用 `rerere.enabled=true`、`rerere.autoupdate=false` 和 `zdiff3`，下次可复用结果，但仍需人工检查并暂存。
