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

if [[ -z "$overlay_commit" ]]; then
  overlay_commit="$(git -C "$SHADOWSOCKS_RUST_PLUS_ROOT" rev-parse HEAD)"
fi
[[ "$overlay_commit" =~ ^[0-9a-f]{40}$ ]] || die "期望 overlay commit 格式错误"

require_clean_worktree
multi_output_dir="$(dirname "$manifest")"
[[ "$(basename "$output")" == "release-manifest.sig" ]] || \
  die "双二进制发布签名文件必须命名为 release-manifest.sig"
[[ "$(dirname "$(absolute_path "$output")")" == "$(cd "$multi_output_dir" && pwd -P)" ]] || \
  die "双二进制发布签名必须位于 release-manifest.json 同一目录"
"$SHADOWSOCKS_RUST_PLUS_ROOT/scripts/release-artifact.py" verify-multi \
  --output-dir "$multi_output_dir" \
  --manifest "$manifest" \
  --expected-version "$(lock_value tag)" \
  --expected-upstream-commit "$(lock_value commit)" \
  --expected-overlay-commit "$overlay_commit" \
  --expected-rustc-version "$RELEASE_RUSTC_VERSION" \
  --expected-rustc-commit "$RELEASE_RUSTC_COMMIT" \
  --expected-cargo-version "$RELEASE_CARGO_VERSION" \
  --expected-cargo-zigbuild-version "$RELEASE_CARGO_ZIGBUILD_VERSION" \
  --expected-zig-version "$RELEASE_ZIG_VERSION" \
  --expected-python-version "$RELEASE_PYTHON_VERSION"

output="$(absolute_path "$output")"
[[ ! -e "$output" && ! -L "$output" ]] || die "签名输出已存在，拒绝覆盖：$output"

output_parent="$(dirname "$output")"
output_name="$(basename "$output")"
temporary_signature="$(mktemp "$output_parent/.${output_name}.XXXXXX")"
cleanup_signature() {
  [[ ! -e "$temporary_signature" ]] || rm -f -- "$temporary_signature"
}
trap cleanup_signature EXIT
chmod 0600 "$temporary_signature"
openssl dgst -sha256 -sign "$private_key" > "$temporary_signature" < "$manifest" || \
  die "manifest 签名失败"
chmod 0644 "$temporary_signature"
ln "$temporary_signature" "$output" 2>/dev/null || \
  die "签名输出已存在或无法原子创建，拒绝覆盖：$output"
rm -f -- "$temporary_signature"
trap - EXIT
printf '已生成 detached SHA-256 签名：%s\n' "$output"
