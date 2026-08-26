#!/usr/bin/env bash
set -euo pipefail

source "$(dirname "$0")/lib.sh"

usage() {
  printf '用法：%s <上游 tag>\n' "$(basename "$0")" >&2
}

[[ $# -eq 1 ]] || { usage; exit 2; }

require_command git

new_tag="$1"
repository="$(lock_value repository)"
remote_line="$(git ls-remote --tags "$repository" "refs/tags/$new_tag" | awk 'NR == 1 { print $0 }')"
[[ -n "$remote_line" ]] || die "上游不存在 tag：$new_tag"
new_object="${remote_line%%[[:space:]]*}"

temp_dir="$(mktemp -d "${TMPDIR:-/tmp}/shadowsocks-rust-plus.XXXXXX")"
trap 'safe_remove_temp_dir "$temp_dir"' EXIT
git init --bare --quiet "$temp_dir/upstream.git"
git --git-dir="$temp_dir/upstream.git" remote add origin "$repository"
git --git-dir="$temp_dir/upstream.git" fetch --quiet --depth 1 origin \
  "refs/tags/$new_tag:refs/tags/$new_tag"
new_commit="$(git --git-dir="$temp_dir/upstream.git" rev-parse "$new_tag^{commit}")"
new_date="$(git --git-dir="$temp_dir/upstream.git" show -s --format=%cI "$new_commit")"

printf '候选上游：\n'
printf '  tag=%s\n  tag_object=%s\n  commit=%s\n  commit_date=%s\n' \
  "$new_tag" "$new_object" "$new_commit" "$new_date"
printf '\n未修改 upstream.lock。请先在临时分支重放补丁并审查冲突，再更新锁文件。\n'
