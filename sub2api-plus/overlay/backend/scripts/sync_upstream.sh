#!/usr/bin/env bash
# fork 与上游同步脚本
#
# 用法: backend/scripts/sync_upstream.sh [--preflight-only] [--skip-build] [--allow-conflicts] [--ack-upstream-review]
#
# 流程:
#   1. 校验当前分支与工作区，配置 rerere/merge driver
#   2. fetch upstream，生成持久化上游评审 patch
#   3. 用 merge-tree 预演，默认不让仓库进入冲突状态
#   4. 合并 upstream/main，ent schema 有变化时重新生成代码
#   5. 后端/前端构建验证
#
# 冲突处理约定:
#   - ent 生成物(ent/*.go,schema 除外)冲突: 接受上游后重新生成
#       git checkout --theirs backend/ent && git add backend/ent
#   - backend/cmd/server/VERSION: 始终取上游
#       git checkout --theirs backend/cmd/server/VERSION && git add backend/cmd/server/VERSION
#   - 其余冲突: 手工解决，fork 定制应优先位于 fork 自有文件
set -euo pipefail
cd "$(dirname "$0")/../.."

SKIP_BUILD=false
PREFLIGHT_ONLY=false
ALLOW_CONFLICTS=false
ACK_UPSTREAM_REVIEW=false
for arg in "$@"; do
  case "$arg" in
    --skip-build) SKIP_BUILD=true ;;
    --preflight-only) PREFLIGHT_ONLY=true ;;
    --allow-conflicts) ALLOW_CONFLICTS=true ;;
    --ack-upstream-review) ACK_UPSTREAM_REVIEW=true ;;
    *) echo "未知参数: $arg"; exit 2 ;;
  esac
done

# fork 接管文件列表(与 .gitattributes 中 merge=ours 条目保持一致)
FORK_OWNED_FILES=(
  backend/internal/web/embed_on.go
  backend/internal/web/embed_test.go
  backend/internal/web/static_cache.go
  backend/internal/web/static_cache_test.go
)

# 这些上游文件保持原样，但它们的新逻辑可能需移植到 fork 替代实现。
# main.ts 的生产替代实现是 app-main.ts/index-main.ts。
PORT_REVIEW_FILES=(
  frontend/src/main.ts
)

REVIEW_FILES=("${FORK_OWNED_FILES[@]}" "${PORT_REVIEW_FILES[@]}")

CURRENT_BRANCH="$(git branch --show-current)"
if [[ -z "$CURRENT_BRANCH" ]]; then
  echo "!! 当前处于 detached HEAD，拒绝同步"
  exit 1
fi
if [[ "$CURRENT_BRANCH" == "main" ]]; then
  echo "!! main 是上游镜像分支，请在 kimi 定制分支运行本脚本"
  exit 1
fi
if git rev-parse -q --verify MERGE_HEAD >/dev/null; then
  echo "!! 已存在未完成的合并，请先解决或中止"
  exit 1
fi
if ! git diff --cached --quiet; then
  echo "!! 暂存区不为空，请先提交或取消暂存"
  exit 1
fi

echo "==> 配置 rerere 与 merge.ours 合并驱动"
git config rerere.enabled true
git config rerere.autoupdate false
git config merge.conflictStyle zdiff3
git config merge.ours.driver true

echo "==> fetch upstream"
git fetch upstream

BASE="$(git merge-base HEAD upstream/main)"
UPSTREAM_SHA="$(git rev-parse upstream/main)"
echo "==> 合并基点: $BASE"
echo "==> 上游提交: $UPSTREAM_SHA"

echo "==> 检查未暂存改动是否与上游重叠"
DIRTY_FILES=()
while IFS= read -r file; do
  [[ -n "$file" ]] && DIRTY_FILES+=("$file")
done < <(git diff --name-only)
for file in "${DIRTY_FILES[@]}"; do
  if ! git diff --quiet "$BASE" upstream/main -- "$file"; then
    echo "!! 未暂存文件与上游改动重叠: $file"
    exit 1
  fi
done

UNTRACKED_FILES=()
while IFS= read -r file; do
  [[ -n "$file" ]] && UNTRACKED_FILES+=("$file")
done < <(git ls-files --others --exclude-standard)
for file in "${UNTRACKED_FILES[@]}"; do
  if git cat-file -e "upstream/main:$file" 2>/dev/null; then
    echo "!! 未跟踪文件将被上游覆盖: $file"
    exit 1
  fi
done
if (( ${#DIRTY_FILES[@]} > 0 || ${#UNTRACKED_FILES[@]} > 0 )); then
  echo "    未发现与上游重叠，Git 会保留这些本地改动"
fi

echo "==> 检查 fork 接管/移植文件在上游的更新"
REVIEW_DIR="$(git rev-parse --git-path upstream-review)"
REVIEW_PATCH="$REVIEW_DIR/$UPSTREAM_SHA.patch"
mkdir -p "$REVIEW_DIR"
git diff "$BASE" upstream/main -- "${REVIEW_FILES[@]}" > "$REVIEW_PATCH"
if [[ -s "$REVIEW_PATCH" ]]; then
  git diff --name-only "$BASE" upstream/main -- "${REVIEW_FILES[@]}" | sed 's/^/    [需评审] /'
  echo "    完整 patch: $REVIEW_PATCH"
  if [[ "$ACK_UPSTREAM_REVIEW" == false ]]; then
    echo "!! 请先评审/移植上游改动，确认后加 --ack-upstream-review 重试"
    exit 3
  fi
else
  echo "    (无)"
fi

echo "==> 预演合并"
PREFLIGHT_STATUS=0
PREFLIGHT_OUTPUT="$(git merge-tree --write-tree --name-only --messages HEAD upstream/main 2>&1)" || PREFLIGHT_STATUS=$?
if [[ $PREFLIGHT_STATUS -ne 0 ]]; then
  printf '%s\n' "$PREFLIGHT_OUTPUT"
  if [[ "$ALLOW_CONFLICTS" == false ]]; then
    echo "!! 预演发现冲突，工作区未改动。评审后加 --allow-conflicts 进入人工解决流程"
    exit 4
  fi
fi
if [[ "$PREFLIGHT_ONLY" == true ]]; then
  echo "==> 预演完成，未修改工作区或分支"
  exit 0
fi

echo "==> 合并 upstream/main"
if ! git merge --no-edit upstream/main; then
  cat << 'MSG'
!! 合并出现冲突，按约定处理:
   - ent 生成物: git checkout --theirs backend/ent && git add backend/ent
   - VERSION:    git checkout --theirs backend/cmd/server/VERSION && git add backend/cmd/server/VERSION
   - 其余文件:   手工解决，优先将 fork 定制保留在 fork 自有文件
   解决后运行: git commit --no-edit
   然后重新运行本脚本（保留 --ack-upstream-review）完成生成与验证。
MSG
  exit 1
fi

if ! git diff --quiet "$BASE" HEAD -- backend/ent/schema/ 2>/dev/null; then
  echo "==> ent schema 有变化，重新生成 ent 代码"
  (cd backend && go generate ./ent)
  git add backend/ent
  git diff --cached --quiet || git commit -m "chore: regenerate ent code after upstream sync"
fi

if [[ "$SKIP_BUILD" == false ]]; then
  echo "==> 后端构建验证"
  (cd backend && go build ./...)

  echo "==> 前端构建验证"
  pnpm --dir frontend build
fi

echo "==> 同步完成"
