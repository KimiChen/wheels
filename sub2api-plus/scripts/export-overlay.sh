#!/usr/bin/env bash
set -euo pipefail

source "$(dirname "$0")/lib.sh"

usage() {
  cat <<'EOF'
用法:
  export-overlay.sh <完整源码仓库> [目标 ref] [基线 ref] [--update-lock]

默认目标 ref 为 kimi-next，默认基线 ref 为 upstream/main。
--update-lock 只用于完成上游升级：新基线必须是旧锁定提交的后代。
EOF
}

[[ $# -ge 1 && $# -le 4 ]] || { usage >&2; exit 2; }

source_repo="$1"
target_ref="${2:-kimi-next}"
base_ref="${3:-upstream/main}"
update_lock=false
if [[ "${4:-}" == "--update-lock" ]]; then
  update_lock=true
elif [[ $# -eq 4 ]]; then
  usage >&2
  exit 2
fi

require_command git
require_command rsync
require_command tar

source_repo="$(cd "$source_repo" && pwd -P)"
git -C "$source_repo" rev-parse --git-dir >/dev/null 2>&1 || die "不是 Git 仓库: $source_repo"

base_commit="$(git -C "$source_repo" rev-parse "$base_ref^{commit}")"
target_commit="$(git -C "$source_repo" rev-parse "$target_ref^{commit}")"
locked_commit="$(lock_value commit)"

if [[ "$base_commit" != "$locked_commit" ]]; then
  if [[ "$update_lock" != true ]]; then
    die "基线 $base_commit 与 upstream.lock 的 $locked_commit 不一致"
  fi
  git -C "$source_repo" merge-base --is-ancestor "$locked_commit" "$base_commit" || \
    die "新基线不是旧锁定提交的后代"
fi

git -C "$source_repo" merge-base --is-ancestor "$base_commit" "$target_commit" || \
  die "基线不是目标 ref 的祖先，不能安全导出"

while IFS= read -r -d '' changed_path; do
  validate_relative_path "$changed_path"
done < <(git -C "$source_repo" diff --name-only -z --no-renames "$base_commit" "$target_commit")

export_root="$(temporary_directory export)"
full_tree="$export_root/full-tree"
staged_overlay="$export_root/overlay"
staged_manifest="$export_root/overlay-manifest.tsv"
staged_deleted="$export_root/deleted-files.txt"
staged_lock="$export_root/upstream.lock"
manifest_body="$export_root/manifest-body.tsv"
clean_excludes="$export_root/export-exclude.txt"
temporary_index="$export_root/index"
mkdir -p "$full_tree" "$staged_overlay"
trap 'rm -rf -- "$export_root"' EXIT
: > "$export_root/overlay-paths.z"
: > "$manifest_body"

if [[ -f "$SUB2API_PLUS_ROOT/export-exclude.txt" ]]; then
  while IFS= read -r excluded_path; do
    [[ -z "$excluded_path" || "$excluded_path" == \#* ]] && continue
    validate_relative_path "$excluded_path"
    printf '%s\n' "$excluded_path" >> "$clean_excludes"
  done < "$SUB2API_PLUS_ROOT/export-exclude.txt"
else
  : > "$clean_excludes"
fi

is_excluded() {
  local candidate="$1"
  grep -Fqx -- "$candidate" "$clean_excludes"
}

git -C "$source_repo" archive "$target_commit" | tar -xf - -C "$full_tree"
while IFS= read -r -d '' overlay_path; do
  is_excluded "$overlay_path" || printf '%s\0' "$overlay_path" >> "$export_root/overlay-paths.z"
done < <(git -C "$source_repo" diff \
  --name-only -z --no-renames --diff-filter=ACMT \
  "$base_commit" "$target_commit")
rsync -a --from0 --files-from="$export_root/overlay-paths.z" "$full_tree/" "$staged_overlay/"

while IFS=$'\t' read -r status path; do
  [[ -n "$status" && -n "$path" ]] || continue
  is_excluded "$path" && continue
  entry="$(git -C "$source_repo" ls-tree "$target_commit" -- "$path")"
  [[ -n "$entry" ]] || die "无法读取目标文件对象: $path"
  metadata="${entry%%$'\t'*}"
  read -r mode object_type object_id <<< "$metadata"
  [[ "$object_type" == "blob" ]] || die "不支持的 Git 对象类型 $object_type: $path"
  printf '%s\t%s\t%s\t%s\n' "$status" "$mode" "$object_id" "$path" >> "$manifest_body"
done < <(git -C "$source_repo" diff --name-status --no-renames --diff-filter=ACMT "$base_commit" "$target_commit")

{
  printf '# 上游中需要删除的文件，每行一个相对仓库根目录的路径。\n'
  while IFS= read -r deleted_path; do
    [[ -n "$deleted_path" ]] || continue
    is_excluded "$deleted_path" || printf '%s\n' "$deleted_path"
  done < <(git -C "$source_repo" diff --name-only --no-renames --diff-filter=D "$base_commit" "$target_commit")
} > "$staged_deleted"

GIT_INDEX_FILE="$temporary_index" git -C "$source_repo" read-tree "$base_commit"
while IFS=$'\t' read -r status mode object_id path; do
  [[ -n "$status" ]] || continue
  GIT_INDEX_FILE="$temporary_index" git -C "$source_repo" update-index \
    --add --cacheinfo "$mode,$object_id,$path"
done < "$manifest_body"
while IFS= read -r deleted_path; do
  [[ -z "$deleted_path" || "$deleted_path" == \#* ]] && continue
  GIT_INDEX_FILE="$temporary_index" git -C "$source_repo" update-index --force-remove -- "$deleted_path"
done < "$staged_deleted"
source_tree="$(GIT_INDEX_FILE="$temporary_index" git -C "$source_repo" write-tree)"

{
  printf '# schema_version=1\n'
  printf '# upstream_commit=%s\n' "$base_commit"
  printf '# source_commit=%s\n' "$target_commit"
  printf '# source_tree=%s\n' "$source_tree"
  printf '# columns=status<TAB>mode<TAB>object<TAB>path\n'
  cat "$manifest_body"
} > "$staged_manifest"

if [[ "$update_lock" == true ]]; then
  awk -v commit="$base_commit" '
    $1 == "commit" && $2 == "=" { print "commit = \"" commit "\""; next }
    { print }
  ' "$SUB2API_PLUS_LOCK" > "$staged_lock"
else
  cp "$SUB2API_PLUS_LOCK" "$staged_lock"
fi

mkdir -p "$SUB2API_PLUS_ROOT/overlay"
rsync -a --delete "$staged_overlay/" "$SUB2API_PLUS_ROOT/overlay/"
install -m 0644 "$staged_manifest" "$SUB2API_PLUS_ROOT/overlay-manifest.tsv"
install -m 0644 "$staged_deleted" "$SUB2API_PLUS_ROOT/deleted-files.txt"
install -m 0644 "$staged_lock" "$SUB2API_PLUS_LOCK"

overlay_count="$(awk '!/^#/ && NF {count++} END {print count + 0}' "$staged_manifest")"
deleted_count="$(awk '!/^#/ && NF {count++} END {print count + 0}' "$staged_deleted")"
printf '已导出 Overlay: %s 个文件，%s 个删除路径\n' "$overlay_count" "$deleted_count"
printf '上游提交: %s\n目标提交: %s\n目标 tree: %s\n' "$base_commit" "$target_commit" "$source_tree"
