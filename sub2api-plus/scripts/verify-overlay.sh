#!/usr/bin/env bash
set -euo pipefail

source "$(dirname "$0")/lib.sh"

[[ $# -le 1 ]] || { printf '用法: verify-overlay.sh [上游仓库覆盖地址]\n' >&2; exit 2; }
repository_override="${1:-}"

require_command git
require_command find
require_command sort

manifest="$SUB2API_PLUS_ROOT/overlay-manifest.tsv"
[[ -f "$manifest" ]] || die "缺少 Overlay 清单"
[[ -d "$SUB2API_PLUS_ROOT/overlay" ]] || die "缺少 overlay 目录"
hosting_git_root="$(git -C "$SUB2API_PLUS_ROOT" rev-parse --show-toplevel 2>/dev/null || true)"

verify_root="$(temporary_directory verify)"
actual_paths="$verify_root/actual-paths"
manifest_paths="$verify_root/manifest-paths"
source_dir="$verify_root/source"
trap 'rm -rf -- "$verify_root"' EXIT

while IFS=$'\t' read -r status mode object_id path; do
  [[ -z "$status" || "$status" == \#* ]] && continue
  [[ "$status" == "A" || "$status" == "M" || "$status" == "T" ]] || \
    die "清单包含不支持的状态: $status"
  validate_relative_path "$path"
  [[ -e "$SUB2API_PLUS_ROOT/overlay/$path" || -L "$SUB2API_PLUS_ROOT/overlay/$path" ]] || \
    die "清单文件不存在: $path"

  ignore_detail=""
  if [[ -n "$hosting_git_root" ]]; then
    ignore_detail="$(git -C "$SUB2API_PLUS_ROOT" check-ignore --no-index -v "overlay/$path" || true)"
  fi
  if [[ -n "$ignore_detail" ]]; then
    ignore_file="${ignore_detail%%:*}"
    case "$ignore_file" in
      sub2api-plus/overlay/*)
        # Overlay 中的 .gitignore 是目标上游文件。它可能忽略另一个已经由目标
        # Git tree 跟踪的文件；提交 Overlay 时用 git add -f 保留该文件。
        ;;
      *)
        die "wheels 的仓库级忽略规则会忽略 Overlay 文件: $path ($ignore_detail)"
        ;;
    esac
  fi
done < "$manifest"

find "$SUB2API_PLUS_ROOT/overlay" \( -type f -o -type l \) -print |
  sed "s#^$SUB2API_PLUS_ROOT/overlay/##" | LC_ALL=C sort > "$actual_paths"
awk -F'\t' '!/^#/ && NF {print $4}' "$manifest" | LC_ALL=C sort > "$manifest_paths"
cmp -s "$actual_paths" "$manifest_paths" || {
  diff -u "$manifest_paths" "$actual_paths" >&2 || true
  die "overlay 目录与清单文件集合不一致"
}

while IFS= read -r deleted_path; do
  [[ -z "$deleted_path" || "$deleted_path" == \#* ]] && continue
  validate_relative_path "$deleted_path"
  [[ ! -e "$SUB2API_PLUS_ROOT/overlay/$deleted_path" && ! -L "$SUB2API_PLUS_ROOT/overlay/$deleted_path" ]] || \
    die "同一路径同时出现在 Overlay 和删除清单: $deleted_path"
done < "$SUB2API_PLUS_ROOT/deleted-files.txt"

if [[ -n "$repository_override" ]]; then
  "$SUB2API_PLUS_ROOT/scripts/prepare-source.sh" "$source_dir" "$repository_override"
else
  "$SUB2API_PLUS_ROOT/scripts/prepare-source.sh" "$source_dir"
fi

git -C "$source_dir" diff --check
printf 'Overlay 验证通过，共 %s 个文件。\n' "$(wc -l < "$actual_paths" | tr -d ' ')"
