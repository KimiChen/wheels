#!/usr/bin/env bash
set -euo pipefail

source "$(dirname "$0")/lib.sh"

require_command bash
require_command cargo
require_command git
require_command openssl
require_command patch
require_command python3
require_command rg

for script_path in "$SHADOWSOCKS_RUST_PLUS_ROOT"/scripts/*.sh; do
  bash -n "$script_path"
done

upstream_tag="$(lock_value tag)"
upstream_commit="$(lock_value commit)"
remote_commit="$(git ls-remote --tags "$(lock_value repository)" "refs/tags/$upstream_tag" | awk 'NR == 1 { print $1 }')"
[[ "$remote_commit" == "$upstream_commit" ]] || \
  die "远端 tag 已漂移或不可用：期望 $upstream_commit，实际 ${remote_commit:-<empty>}"

duplicate_patch="$(awk 'NF && $1 !~ /^#/ { print $1 }' "$SHADOWSOCKS_RUST_PLUS_ROOT/patches/series" | sort | uniq -d | head -n 1)"
[[ -z "$duplicate_patch" ]] || die "series 重复补丁：$duplicate_patch"
while IFS= read -r patch_name; do
  [[ -z "$patch_name" || "$patch_name" == \#* ]] && continue
  [[ -f "$SHADOWSOCKS_RUST_PLUS_ROOT/patches/$patch_name" ]] || die "series 引用不存在文件：$patch_name"
done < "$SHADOWSOCKS_RUST_PLUS_ROOT/patches/series"

for patch_path in "$SHADOWSOCKS_RUST_PLUS_ROOT"/patches/*.patch; do
  patch_name="$(basename "$patch_path")"
  grep -Fxq "$patch_name" "$SHADOWSOCKS_RUST_PLUS_ROOT/patches/series" || \
    die "补丁未列入 series：$patch_name"
done

python3 -m py_compile \
  "$SHADOWSOCKS_RUST_PLUS_ROOT"/scripts/*.py \
  "$SHADOWSOCKS_RUST_PLUS_ROOT"/tests/*.py

temp_dir="$(mktemp -d "${TMPDIR:-/tmp}/shadowsocks-rust-plus.XXXXXX")"
trap 'safe_remove_temp_dir "$temp_dir"' EXIT
source_dir="$temp_dir/source"
"$SHADOWSOCKS_RUST_PLUS_ROOT/scripts/prepare-source.sh" "$source_dir"

# v1.24.0 predates the rustfmt bundled with the current toolchain; formatting the
# untouched upstream tree changes unrelated let-chains and imports. Keep strict
# formatting available for a pinned-compatible toolchain without making the normal
# verifier fail on pristine upstream code.
if [[ "${SHADOWSOCKS_RUST_PLUS_STRICT_FMT:-0}" == 1 ]]; then
  cargo fmt --manifest-path "$source_dir/Cargo.toml" --all -- --check
fi
"$SHADOWSOCKS_RUST_PLUS_ROOT/scripts/test.sh" --source "$source_dir"

bash "$SHADOWSOCKS_RUST_PLUS_ROOT/scripts/check-sensitive.sh"

printf '验证完成：锁定版本、零 fuzz 补丁重放、测试与敏感信息扫描均通过。\n'
