#!/usr/bin/env bash
set -euo pipefail

source "$(dirname "$0")/lib.sh"
# shellcheck disable=SC1091
source "$SHADOWSOCKS_RUST_PLUS_ROOT/packaging/release-toolchain.lock"

usage() {
  printf '用法：%s --release-manifest <release-manifest.json> --signature <sig> --public-key <PEM> [--overlay-commit <commit>]\n' \
    "$(basename "$0")" >&2
}

signature=""
public_key=""
overlay_commit=""
release_manifest=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --release-manifest|--signature|--public-key|--overlay-commit)
      [[ $# -ge 2 ]] || { usage; exit 2; }
      case "$1" in
        --release-manifest) release_manifest="$2" ;;
        --signature) signature="$2" ;;
        --public-key) public_key="$2" ;;
        --overlay-commit) overlay_commit="$2" ;;
      esac
      shift 2
      ;;
    *)
      usage
      exit 2
      ;;
  esac
done

[[ -n "$release_manifest" && -n "$signature" && -n "$public_key" ]] || { usage; exit 2; }
manifest="$release_manifest"
require_command openssl
require_command python3
require_command git

for input_path in "$manifest" "$signature" "$public_key"; do
  [[ -f "$input_path" && ! -L "$input_path" ]] || \
    die "验签输入必须是普通文件且不能是符号链接：$input_path"
done
require_clean_worktree
release_dir="$(cd "$(dirname "$manifest")" && pwd -P)"
[[ "$(basename "$signature")" == "release-manifest.sig" ]] || \
  die "双二进制发布签名文件必须命名为 release-manifest.sig"
[[ "$(cd "$(dirname "$signature")" && pwd -P)" == "$release_dir" ]] || \
  die "双二进制发布签名必须位于 release-manifest.json 同一目录"
for artifact_name in ssserver ssserver.sha256 shadowsocks-auditd shadowsocks-auditd.sha256 release-manifest.json release-manifest.sig; do
  [[ -f "$release_dir/$artifact_name" && ! -L "$release_dir/$artifact_name" ]] || \
    die "发布目录缺少普通产物：$artifact_name"
done

if [[ -z "$overlay_commit" ]]; then
  overlay_commit="$(git -C "$SHADOWSOCKS_RUST_PLUS_ROOT" rev-parse HEAD)"
fi
[[ "$overlay_commit" =~ ^[0-9a-f]{40}$ ]] || die "期望 overlay commit 格式错误"

openssl dgst -sha256 -verify "$public_key" -signature "$signature" "$manifest" >/dev/null || \
  die "manifest detached 签名验证失败"

"$SHADOWSOCKS_RUST_PLUS_ROOT/scripts/release-artifact.py" verify-multi \
  --output-dir "$(dirname "$manifest")" \
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

printf '签名验证通过；双二进制发布产物、来源与 SHA-256 全部匹配。\n'
