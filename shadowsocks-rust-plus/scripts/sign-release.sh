#!/usr/bin/env bash
set -euo pipefail

source "$(dirname "$0")/lib.sh"
# shellcheck disable=SC1091
source "$SHADOWSOCKS_RUST_PLUS_ROOT/packaging/release-toolchain.lock"

usage() {
  printf '用法：%s --release-manifest <release-manifest.json> --private-key <离线私钥 PEM> --output <不存在的签名文件> [--overlay-commit <commit>]\n' \
    "$(basename "$0")" >&2
}

private_key=""
output=""
overlay_commit=""
release_manifest=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --private-key)
      [[ $# -ge 2 ]] || { usage; exit 2; }
      private_key="$2"
      shift 2
      ;;
    --output)
      [[ $# -ge 2 ]] || { usage; exit 2; }
      output="$2"
      shift 2
      ;;
    --overlay-commit)
      [[ $# -ge 2 ]] || { usage; exit 2; }
      overlay_commit="$2"
      shift 2
      ;;
    --release-manifest)
      [[ $# -ge 2 ]] || { usage; exit 2; }
      release_manifest="$2"
      shift 2
      ;;
    *)
      usage
      exit 2
      ;;
  esac
done

[[ -n "$release_manifest" && -n "$private_key" && -n "$output" ]] || { usage; exit 2; }
manifest="$release_manifest"
require_command openssl
require_command python3
require_command git

[[ -f "$manifest" && ! -L "$manifest" ]] || die "release manifest 必须是普通文件且不能是符号链接"
[[ -f "$private_key" && ! -L "$private_key" ]] || die "私钥必须是普通文件且不能是符号链接"
private_key_mode="$(stat -c '%a' -- "$private_key" 2>/dev/null || stat -f '%Lp' -- "$private_key")"
(( (8#$private_key_mode & 077) == 0 )) || die "发布私钥不得授予 group/other 任何权限"

actual_head="$(git -C "$SHADOWSOCKS_RUST_PLUS_ROOT" rev-parse HEAD)"
[[ "$actual_head" =~ ^[0-9a-f]{40}$ ]] || die "无法取得当前 overlay HEAD"
if [[ -n "$overlay_commit" && "$overlay_commit" != "$actual_head" ]]; then
  die "显式 overlay commit 必须等于当前 HEAD"
fi
overlay_commit="$actual_head"
[[ "$overlay_commit" =~ ^[0-9a-f]{40}$ ]] || die "期望 overlay commit 格式错误"
expected_source_date_epoch="$(
  git -C "$SHADOWSOCKS_RUST_PLUS_ROOT" show -s --format=%ct "$overlay_commit"
)" || die "无法从 overlay commit 推导 SOURCE_DATE_EPOCH"
[[ "$expected_source_date_epoch" =~ ^[1-9][0-9]*$ ]] || \
  die "overlay commit 时间戳无效"
expected_prepared_tree_sha256="$(lock_value prepared_tree_sha256)"
[[ "$expected_prepared_tree_sha256" =~ ^[0-9a-f]{64}$ ]] || \
  die "upstream.lock prepared_tree_sha256 格式错误"

require_clean_worktree
multi_output_dir="$(dirname "$manifest")"
[[ "$(basename "$output")" == "release-manifest.sig" ]] || \
  die "双二进制发布签名文件必须命名为 release-manifest.sig"
[[ "$(dirname "$(absolute_path "$output")")" == "$(cd "$multi_output_dir" && pwd -P)" ]] || \
  die "双二进制发布签名必须位于 release-manifest.json 同一目录"
output="$(absolute_path "$output")"
[[ ! -e "$output" && ! -L "$output" ]] || die "签名输出已存在，拒绝覆盖：$output"

"$SHADOWSOCKS_RUST_PLUS_ROOT/scripts/release-artifact.py" sign-multi \
  --output-dir "$multi_output_dir" \
  --manifest "$manifest" \
  --private-key "$private_key" \
  --signature-output "$output" \
  --expected-version "$(lock_value tag)" \
  --expected-upstream-commit "$(lock_value commit)" \
  --expected-overlay-commit "$overlay_commit" \
  --expected-source-date-epoch "$expected_source_date_epoch" \
  --expected-prepared-tree-sha256 "$expected_prepared_tree_sha256" \
  --expected-rustc-version "$RELEASE_RUSTC_VERSION" \
  --expected-rustc-commit "$RELEASE_RUSTC_COMMIT" \
  --expected-cargo-version "$RELEASE_CARGO_VERSION" \
  --expected-cargo-zigbuild-version "$RELEASE_CARGO_ZIGBUILD_VERSION" \
  --expected-zig-version "$RELEASE_ZIG_VERSION" \
  --expected-python-version "$RELEASE_PYTHON_VERSION"

signature_identity="$(stat -Lc '%d:%i' -- "$output" 2>/dev/null || stat -f '%d:%i' -- "$output")" || \
  die "无法取得新签名身份"
cleanup_new_signature() {
  local current_identity
  if [[ -f "$output" && ! -L "$output" ]]; then
    current_identity="$(stat -Lc '%d:%i' -- "$output" 2>/dev/null || stat -f '%d:%i' -- "$output")" || return
    if [[ "$current_identity" == "$signature_identity" ]]; then
      rm -f -- "$output"
    fi
  fi
}
final_head="$(git -C "$SHADOWSOCKS_RUST_PLUS_ROOT" rev-parse HEAD)" || {
  cleanup_new_signature
  die "签名后无法重新取得 overlay HEAD"
}
if [[ "$final_head" != "$overlay_commit" ]]; then
  cleanup_new_signature
  die "overlay HEAD 在签名期间发生变化"
fi
final_status="$(git -C "$SHADOWSOCKS_RUST_PLUS_ROOT" status --porcelain --untracked-files=normal)" || {
  cleanup_new_signature
  die "签名后无法重新检查 overlay 工作树"
}
if [[ -n "$final_status" ]]; then
  cleanup_new_signature
  die "overlay 工作树在签名期间发生变化"
fi
