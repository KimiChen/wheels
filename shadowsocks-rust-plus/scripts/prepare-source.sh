#!/usr/bin/env bash
set -euo pipefail

source "$(dirname "$0")/lib.sh"

usage() {
  printf '用法：%s <不存在的输出目录> [上游仓库覆盖地址]\n' "$(basename "$0")" >&2
}

[[ $# -ge 1 && $# -le 2 ]] || { usage; exit 2; }

require_command git
require_command patch
require_command tar

output_dir="$(absolute_path "$1")"
repository_override="${2:-${UPSTREAM_REPOSITORY:-}}"
repository="${repository_override:-$(lock_value repository)}"
upstream_tag="$(lock_value tag)"
upstream_commit="$(lock_value commit)"

[[ ! -e "$output_dir" ]] || die "输出路径已存在，拒绝覆盖：$output_dir"

temp_dir="$(mktemp -d "${TMPDIR:-/tmp}/shadowsocks-rust-plus.XXXXXX")"
trap 'safe_remove_temp_dir "$temp_dir"' EXIT

git init --bare --quiet "$temp_dir/upstream.git"
git --git-dir="$temp_dir/upstream.git" remote add origin "$repository"
git --git-dir="$temp_dir/upstream.git" fetch --quiet --depth 1 origin \
  "refs/tags/$upstream_tag:refs/tags/$upstream_tag"

actual_commit="$(git --git-dir="$temp_dir/upstream.git" rev-parse "$upstream_tag^{commit}")"
[[ "$actual_commit" == "$upstream_commit" ]] || \
  die "上游 tag 指向 $actual_commit，与锁定提交 $upstream_commit 不符"

mkdir "$output_dir"
git --git-dir="$temp_dir/upstream.git" archive "$upstream_commit" | tar -x -C "$output_dir"

while IFS= read -r patch_name; do
  [[ -z "$patch_name" || "$patch_name" == \#* ]] && continue
  [[ "$patch_name" != */* && "$patch_name" == *.patch ]] || die "非法补丁名：$patch_name"
  patch_path="$SHADOWSOCKS_RUST_PLUS_ROOT/patches/$patch_name"
  [[ -f "$patch_path" ]] || die "补丁不存在：$patch_name"
  # `patch` cannot create missing parent directories for a new file.  Create
  # only directories named by this patch, inside the disposable source tree,
  # so newly added crates and fuzz targets remain replayable from a clean
  # upstream archive.
  while IFS= read -r target_path; do
    [[ "$target_path" == b/* ]] || continue
    target_path="${target_path#b/}"
    mkdir -p "$output_dir/$(dirname "$target_path")"
  done < <(sed -n 's#^+++ b/##p' "$patch_path")
  (
    cd "$output_dir"
    patch --batch --forward --fuzz=0 -E -p1 < "$patch_path"
  )
done < "$SHADOWSOCKS_RUST_PLUS_ROOT/patches/series"

[[ ! -e "$output_dir/.git" ]] || die "准备后的源码树不得包含嵌套 .git"

printf '源码已准备：%s\n' "$output_dir"
printf '上游版本：%s (%s)\n' "$upstream_tag" "$upstream_commit"
