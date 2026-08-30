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

# Parse the switches up front so an invalid value fails before the long
# prepare/build/test sequence rather than after it.  The final coverage
# conclusion comes from test.sh's recorded execution state below, not from
# re-evaluating this policy switch.
require_bool_env SHADOWSOCKS_REQUIRE_AUDIT_TARGET 1 >/dev/null
strict_fmt="$(require_bool_env SHADOWSOCKS_RUST_PLUS_STRICT_FMT 0)"

[[ -f "$SHADOWSOCKS_RUST_PLUS_ROOT/config/auditd.example.json" ]] || \
  die "缺少 auditd 配置样例"
[[ -f "$SHADOWSOCKS_RUST_PLUS_ROOT/packaging/shadowsocks-auditd.service" ]] || \
  die "缺少 shadowsocks-auditd systemd 模板"
[[ -f "$SHADOWSOCKS_RUST_PLUS_ROOT/packaging/shadowsocks-auditd.sysusers" ]] || \
  die "缺少 auditd sysusers 模板"
[[ -f "$SHADOWSOCKS_RUST_PLUS_ROOT/packaging/shadowsocks-auditd.tmpfiles" ]] || \
  die "缺少 auditd tmpfiles 模板"
[[ -f "$SHADOWSOCKS_RUST_PLUS_ROOT/tests/mock_collector.py" ]] || \
  die "缺少 mock collector"
[[ -f "$SHADOWSOCKS_RUST_PLUS_ROOT/tests/test_mock_collector.py" ]] || \
  die "缺少 mock collector 测试"

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
  if ! python3 "$SHADOWSOCKS_RUST_PLUS_ROOT/scripts/check-patch-deletions.py" "$patch_path"; then
    die "补丁包含没有实际内容的删除 stanza：$patch_name"
  fi
done

grep -Fxq "0003-user-audit.patch" "$SHADOWSOCKS_RUST_PLUS_ROOT/patches/series" || \
  die "patches/series 未包含 0003-user-audit.patch"
[[ -f "$SHADOWSOCKS_RUST_PLUS_ROOT/patches/0003-user-audit.patch" ]] || \
  die "缺少 0003-user-audit.patch"

python3 -m py_compile \
  "$SHADOWSOCKS_RUST_PLUS_ROOT"/scripts/*.py \
  "$SHADOWSOCKS_RUST_PLUS_ROOT"/tests/*.py

python3 -m json.tool "$SHADOWSOCKS_RUST_PLUS_ROOT/config/server.example.json" >/dev/null
python3 -m json.tool "$SHADOWSOCKS_RUST_PLUS_ROOT/config/auditd.example.json" >/dev/null

temp_dir="$(mktemp -d "${TMPDIR:-/tmp}/shadowsocks-rust-plus.XXXXXX")"
trap 'safe_remove_temp_dir "$temp_dir"' EXIT
source_dir="$temp_dir/source"
coverage_status="$temp_dir/test-coverage.json"
"$SHADOWSOCKS_RUST_PLUS_ROOT/scripts/prepare-source.sh" "$source_dir"
expected_prepared_tree_sha256="$(lock_value prepared_tree_sha256)"
[[ "$expected_prepared_tree_sha256" =~ ^[0-9a-f]{64}$ ]] || \
  die "upstream.lock prepared_tree_sha256 格式错误"
actual_prepared_tree_sha256="$(
  "$SHADOWSOCKS_RUST_PLUS_ROOT/scripts/release-artifact.py" source-tree-sha256 \
    --source-root "$source_dir"
)" || die "无法计算 fresh prepared source tree SHA-256"
[[ "$actual_prepared_tree_sha256" == "$expected_prepared_tree_sha256" ]] || \
  die "fresh prepared source tree SHA-256 与 upstream.lock 不一致"

# v1.24.0 predates the rustfmt bundled with the current toolchain; formatting the
# untouched upstream tree changes unrelated let-chains and imports. Keep strict
# formatting available for a pinned-compatible toolchain without making the normal
# verifier fail on pristine upstream code.
if [[ "$strict_fmt" == 1 ]]; then
  cargo fmt --manifest-path "$source_dir/Cargo.toml" --all -- --check
fi
"$SHADOWSOCKS_RUST_PLUS_ROOT/scripts/test.sh" \
  --source "$source_dir" --coverage-status "$coverage_status"

bash "$SHADOWSOCKS_RUST_PLUS_ROOT/scripts/check-sensitive.sh"

report_verification_conclusion "$coverage_status"
