#!/usr/bin/env bash
set -euo pipefail

source "$(dirname "$0")/lib.sh"

usage() {
  printf '用法：%s [--debug] [--source <已准备源码目录>] [--without-audit] [--output-dir <目录>]\n' "$(basename "$0")" >&2
}

directory_identity() {
  local path="$1"
  stat -Lc '%d:%i' -- "$path" 2>/dev/null || stat -f '%d:%i' -- "$path"
}

profile=release
source_dir=""
build_audit=1
output_dir="$SHADOWSOCKS_RUST_PLUS_ROOT/.cache/dev-dist"
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
    --without-audit)
      build_audit=0
      shift
      ;;
    --output-dir)
      [[ $# -ge 2 ]] || { usage; exit 2; }
      output_dir="$2"
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

# Development artifacts must not share a directory with signed release files.
# Resolve the requested path before preparing or building source so a bad
# destination fails fast.
if [[ "$output_dir" != /* ]]; then
  output_dir="$PWD/$output_dir"
fi
case "$output_dir" in
  "$SHADOWSOCKS_RUST_PLUS_ROOT/dist"|"$SHADOWSOCKS_RUST_PLUS_ROOT/dist"/*)
    die "开发构建不得写入 release 目录：$output_dir"
    ;;
esac
[[ ! -L "$output_dir" ]] || die "开发构建输出目录不能是符号链接：$output_dir"
if [[ -e "$output_dir" ]]; then
  output_dir="$(cd "$output_dir" && pwd -P)" || die "无法解析开发产物目录：$output_dir"
else
  mkdir -p "$(dirname "$output_dir")"
  output_parent="$(cd "$(dirname "$output_dir")" && pwd -P)" || die "无法解析开发产物父目录：$output_dir"
  output_dir="$output_parent/$(basename "$output_dir")"
  mkdir "$output_dir" || die "无法创建开发产物目录：$output_dir"
fi
release_dir="$SHADOWSOCKS_RUST_PLUS_ROOT/dist"
if [[ "$output_dir" == "$release_dir" || "$output_dir" == "$release_dir"/* ]]; then
  die "开发构建不得写入 release 目录：$output_dir"
fi
output_identity="$(directory_identity "$output_dir")" || die "无法取得开发输出目录身份：$output_dir"

validate_development_output_directory() {
  [[ -d "$output_dir" && ! -L "$output_dir" ]] || die "开发输出目录已在构建期间被替换"
  [[ "$(cd "$output_dir" && pwd -P)" == "$output_dir" ]] || \
    die "开发输出目录解析结果已在构建期间变化"
  [[ "$(directory_identity "$output_dir")" == "$output_identity" ]] || \
    die "开发输出目录 inode 已在构建期间变化"
  for release_marker in \
    build-a.receipt.json build-b.receipt.json release-manifest.json release-manifest.sig; do
    if [[ -e "$output_dir/$release_marker" || -L "$output_dir/$release_marker" ]]; then
      die "开发构建目标已包含 release metadata，拒绝覆盖：$output_dir"
    fi
  done
}
validate_development_output_directory

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
if [[ "$build_audit" -eq 1 ]]; then
  # user-audit implies user-stats and is deliberately required for a release
  # build. Keep --without-audit for local work against an older prepared tree.
  build_args=(--locked --features user-audit --bin ssserver)
fi
if [[ "$profile" == release ]]; then
  build_args+=(--release)
fi

CARGO_TARGET_DIR="$target_dir" cargo build --manifest-path "$source_dir/Cargo.toml" "${build_args[@]}"

binary_path="$target_dir/$profile/ssserver"
[[ -x "$binary_path" ]] || die "构建完成但未找到 ssserver：$binary_path"

artifact_name=ssserver
if [[ "$profile" == debug ]]; then
  artifact_name=ssserver-debug
fi
validate_development_output_directory
[[ ! -L "$output_dir/$artifact_name" && ! -L "$output_dir/$artifact_name.sha256" ]] || \
  die "开发产物目标不能是符号链接：$output_dir"
install -m 0755 "$binary_path" "$output_dir/$artifact_name"
validate_development_output_directory
(
  cd "$output_dir"
  shasum -a 256 "$artifact_name" > "$artifact_name.sha256"
)

if [[ "$build_audit" -eq 1 ]]; then
  audit_manifest="$source_dir/crates/shadowsocks-auditd/Cargo.toml"
  [[ -f "$audit_manifest" ]] || die "user-audit 已启用但缺少 shadowsocks-auditd crate：$audit_manifest"
  audit_args=(--manifest-path "$source_dir/Cargo.toml" --locked --features user-audit)
  if [[ "$profile" == release ]]; then
    audit_args+=(--release)
  fi
  CARGO_TARGET_DIR="$target_dir" cargo build "${audit_args[@]}" --bin shadowsocks-auditd
  audit_binary="$target_dir/$profile/shadowsocks-auditd"
  [[ -x "$audit_binary" ]] || die "构建完成但未找到 shadowsocks-auditd：$audit_binary"
  audit_artifact=shadowsocks-auditd
  if [[ "$profile" == debug ]]; then
    audit_artifact=shadowsocks-auditd-debug
  fi
  validate_development_output_directory
  [[ ! -L "$output_dir/$audit_artifact" && ! -L "$output_dir/$audit_artifact.sha256" ]] || \
    die "开发审计产物目标不能是符号链接：$output_dir"
  install -m 0755 "$audit_binary" "$output_dir/$audit_artifact"
  validate_development_output_directory
  (
    cd "$output_dir"
    shasum -a 256 "$audit_artifact" > "$audit_artifact.sha256"
  )
fi

printf '构建完成：%s\n' "$output_dir/$artifact_name"
printf '校验文件：%s\n' "$output_dir/$artifact_name.sha256"
if [[ "$build_audit" -eq 1 ]]; then
  printf '构建完成：%s\n' "$output_dir/$audit_artifact"
  printf '校验文件：%s\n' "$output_dir/$audit_artifact.sha256"
fi
