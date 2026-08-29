#!/usr/bin/env bash
set -euo pipefail

# 发布链路的环境输入必须完全由脚本与 lock 文件决定，不接受未跟踪的 .env。
SHADOWSOCKS_RUST_PLUS_NO_DOTENV=1
export SHADOWSOCKS_RUST_PLUS_NO_DOTENV
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
for artifact_name in \
  ssserver ssserver.sha256 shadowsocks-auditd shadowsocks-auditd.sha256 \
  build-a.receipt.json build-b.receipt.json release-manifest.json release-manifest.sig; do
  [[ -f "$release_dir/$artifact_name" && ! -L "$release_dir/$artifact_name" ]] || \
    die "发布目录缺少普通产物：$artifact_name"
done

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

"$SHADOWSOCKS_RUST_PLUS_ROOT/scripts/release-artifact.py" verify-signed-multi \
  --output-dir "$(dirname "$manifest")" \
  --manifest "$manifest" \
  --signature "$signature" \
  --public-key "$public_key" \
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

final_head="$(git -C "$SHADOWSOCKS_RUST_PLUS_ROOT" rev-parse HEAD)" || \
  die "验签后无法重新取得 overlay HEAD"
[[ "$final_head" == "$overlay_commit" ]] || die "overlay HEAD 在验签期间发生变化"
final_status="$(git -C "$SHADOWSOCKS_RUST_PLUS_ROOT" status --porcelain --untracked-files=normal)" || \
  die "验签后无法重新检查 overlay 工作树"
[[ -z "$final_status" ]] || die "overlay 工作树在验签期间发生变化"
