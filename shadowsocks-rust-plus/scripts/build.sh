#!/usr/bin/env bash
set -euo pipefail

source "$(dirname "$0")/lib.sh"

usage() {
  printf '用法：%s [--debug] [--source <已准备源码目录>] [--without-audit]\n' "$(basename "$0")" >&2
}

profile=release
source_dir=""
build_audit=1
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
  install -m 0755 "$audit_binary" "$dist_dir/$audit_artifact"
  (
    cd "$dist_dir"
    shasum -a 256 "$audit_artifact" > "$audit_artifact.sha256"
  )
fi

printf '构建完成：%s\n' "$dist_dir/$artifact_name"
printf '校验文件：%s\n' "$dist_dir/$artifact_name.sha256"
if [[ "$build_audit" -eq 1 ]]; then
  printf '构建完成：%s\n' "$dist_dir/$audit_artifact"
  printf '校验文件：%s\n' "$dist_dir/$audit_artifact.sha256"
fi
