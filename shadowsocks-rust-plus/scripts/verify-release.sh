#!/usr/bin/env bash
set -euo pipefail

source "$(dirname "$0")/lib.sh"
# shellcheck disable=SC1091
source "$SHADOWSOCKS_RUST_PLUS_ROOT/packaging/release-toolchain.lock"

usage() {
  printf '用法：%s --archive <tar.gz> --manifest <manifest.json> --checksum <sha256> --signature <sig> --public-key <PEM> [--overlay-commit <commit>]\n' \
    "$(basename "$0")" >&2
}

archive=""
manifest=""
checksum=""
signature=""
public_key=""
overlay_commit=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --archive|--manifest|--checksum|--signature|--public-key|--overlay-commit)
      [[ $# -ge 2 ]] || { usage; exit 2; }
      case "$1" in
        --archive) archive="$2" ;;
        --manifest) manifest="$2" ;;
        --checksum) checksum="$2" ;;
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

[[ -n "$archive" && -n "$manifest" && -n "$checksum" && -n "$signature" && -n "$public_key" ]] || \
  { usage; exit 2; }
require_command openssl
require_command python3
require_command git

for input_path in "$archive" "$manifest" "$checksum" "$signature" "$public_key"; do
  [[ -f "$input_path" && ! -L "$input_path" ]] || \
    die "验签输入必须是普通文件且不能是符号链接：$input_path"
done

if [[ -z "$overlay_commit" ]]; then
  overlay_commit="$(git -C "$SHADOWSOCKS_RUST_PLUS_ROOT" rev-parse HEAD)"
fi
[[ "$overlay_commit" =~ ^[0-9a-f]{40}$ ]] || die "期望 overlay commit 格式错误"

openssl dgst -sha256 -verify "$public_key" -signature "$signature" "$manifest" >/dev/null || \
  die "manifest detached 签名验证失败"

"$SHADOWSOCKS_RUST_PLUS_ROOT/scripts/release-artifact.py" verify \
  --archive "$archive" \
  --manifest "$manifest" \
  --checksum "$checksum" \
  --expected-version "$(lock_value tag)" \
  --expected-upstream-commit "$(lock_value commit)" \
  --expected-overlay-commit "$overlay_commit" \
  --expected-rustc-version "$RELEASE_RUSTC_VERSION" \
  --expected-rustc-commit "$RELEASE_RUSTC_COMMIT" \
  --expected-cargo-version "$RELEASE_CARGO_VERSION" \
  --expected-cargo-zigbuild-version "$RELEASE_CARGO_ZIGBUILD_VERSION" \
  --expected-zig-version "$RELEASE_ZIG_VERSION" \
  --expected-python-version "$RELEASE_PYTHON_VERSION" \
  --expected-zlib-version "$RELEASE_ZLIB_VERSION"

printf '签名验证通过；发布包来源、结构与 SHA-256 全部匹配。\n'
