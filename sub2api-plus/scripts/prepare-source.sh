#!/usr/bin/env bash
set -euo pipefail

source "$(dirname "$0")/lib.sh"

usage() {
  printf '用法: prepare-source.sh <不存在的输出目录> [上游仓库覆盖地址]\n' >&2
}

[[ $# -ge 1 && $# -le 2 ]] || { usage; exit 2; }

output_dir="$1"
repository_override="${2:-}"

require_command git
require_command rsync

[[ ! -e "$output_dir" ]] || die "输出路径已存在，拒绝覆盖: $output_dir"
mkdir -p "$(dirname "$output_dir")"

repository="${repository_override:-$(lock_value repository)}"
upstream_ref="$(lock_value ref)"
upstream_commit="$(lock_value commit)"
expected_tree="$(manifest_value source_tree)"
manifest_upstream="$(manifest_value upstream_commit)"
[[ "$manifest_upstream" == "$upstream_commit" ]] || \
  die "Overlay 清单基线与 upstream.lock 不一致"

if [[ -n "$repository_override" ]]; then
  # 本地维护仓库的 main 可能故意停在旧镜像点；完整克隆各本地分支，
  # 只要锁定提交可从其中任一分支到达即可。
  git clone --no-checkout "$repository" "$output_dir"
else
  git clone --filter=blob:none --no-checkout --single-branch --branch "$upstream_ref" \
    "$repository" "$output_dir"
fi
git -C "$output_dir" cat-file -e "$upstream_commit^{commit}" 2>/dev/null || \
  die "克隆结果中不存在锁定提交: $upstream_commit"
git -C "$output_dir" checkout --detach "$upstream_commit"

actual_commit="$(git -C "$output_dir" rev-parse HEAD)"
[[ "$actual_commit" == "$upstream_commit" ]] || die "检出的上游提交不匹配"

while IFS= read -r deleted_path; do
  [[ -z "$deleted_path" || "$deleted_path" == \#* ]] && continue
  validate_relative_path "$deleted_path"
  target_path="$output_dir/$deleted_path"
  if [[ -d "$target_path" && ! -L "$target_path" ]]; then
    die "删除清单指向目录而不是文件: $deleted_path"
  fi
  rm -f -- "$target_path"
done < "$SUB2API_PLUS_ROOT/deleted-files.txt"

rsync -a "$SUB2API_PLUS_ROOT/overlay/" "$output_dir/"

git -C "$output_dir" add -A -f -- .
assembled_tree="$(git -C "$output_dir" write-tree)"
if [[ "$assembled_tree" != "$expected_tree" ]]; then
  printf '期望 tree: %s\n实际 tree: %s\n' "$expected_tree" "$assembled_tree" >&2
  git -C "$output_dir" status --short >&2
  die "组装源码与 Overlay 清单记录的目标 tree 不一致"
fi
git -C "$output_dir" reset --mixed --quiet "$upstream_commit"

printf '源码已准备: %s\n' "$(cd "$output_dir" && pwd -P)"
printf '上游提交: %s\n组装 tree: %s\n' "$upstream_commit" "$assembled_tree"
