#!/usr/bin/env bash
set -euo pipefail

source "$(dirname "$0")/lib.sh"

usage() {
  printf '用法：%s [--debug] [--source <已准备源码目录>]\n' "$(basename "$0")" >&2
}

profile=release
source_dir=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --debug)
      profile=debug
      shift
      ;;
    --source)
      [[ $# -ge 2 ]] || { usage; exit 2; }
      source_dir="$2"
      shift 2
      ;;
    *)
      usage
      exit 2
      ;;
  esac
done

require_command cargo
require_command shasum

temp_dir=""
if [[ -z "$source_dir" ]]; then
  temp_dir="$(mktemp -d "${TMPDIR:-/tmp}/shadowsocks-rust-plus.XXXXXX")"
  trap 'safe_remove_temp_dir "$temp_dir"' EXIT
  source_dir="$temp_dir/source"
  "$SHADOWSOCKS_RUST_PLUS_ROOT/scripts/prepare-source.sh" "$source_dir"
else
  source_dir="$(cd "$source_dir" && pwd -P)"
fi

target_dir="$SHADOWSOCKS_RUST_PLUS_ROOT/.cache/cargo-target"
build_args=(--locked --features user-stats --bin ssserver)
if [[ "$profile" == release ]]; then
  build_args+=(--release)
fi

CARGO_TARGET_DIR="$target_dir" cargo build --manifest-path "$source_dir/Cargo.toml" "${build_args[@]}"

binary_path="$target_dir/$profile/ssserver"
[[ -x "$binary_path" ]] || die "构建完成但未找到 ssserver：$binary_path"

dist_dir="$SHADOWSOCKS_RUST_PLUS_ROOT/dist"
artifact_name=ssserver
if [[ "$profile" == debug ]]; then
  artifact_name=ssserver-debug
fi
mkdir -p "$dist_dir"
install -m 0755 "$binary_path" "$dist_dir/$artifact_name"
(
  cd "$dist_dir"
  shasum -a 256 "$artifact_name" > "$artifact_name.sha256"
)

printf '构建完成：%s\n' "$dist_dir/$artifact_name"
printf '校验文件：%s\n' "$dist_dir/$artifact_name.sha256"
