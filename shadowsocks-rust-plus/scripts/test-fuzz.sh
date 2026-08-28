#!/usr/bin/env bash
set -euo pipefail

source "$(dirname "$0")/lib.sh"

usage() {
  printf '用法：%s --source <准备源码目录> [--seconds N] [--require]\n' "$(basename "$0")" >&2
}

source_dir=""
seconds=30
require_fuzz=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --source)
      [[ $# -ge 2 ]] || { usage; exit 2; }
      source_dir="$2"
      shift 2
      ;;
    --seconds)
      [[ $# -ge 2 ]] || { usage; exit 2; }
      seconds="$2"
      shift 2
      ;;
    --require)
      require_fuzz=1
      shift
      ;;
    *)
      usage
      exit 2
      ;;
  esac
done

[[ -n "$source_dir" ]] || { usage; exit 2; }
source_dir="$(cd "$source_dir" && pwd -P)"
manifest="$source_dir/fuzz/Cargo.toml"
target="$source_dir/fuzz/fuzz_targets/audit_protocol.rs"
[[ -f "$manifest" && -f "$target" ]] || die "缺少 audit protocol fuzz target"
[[ "$seconds" =~ ^[1-9][0-9]*$ ]] || die "--seconds 必须是正整数"

if ! command -v cargo-fuzz >/dev/null 2>&1; then
  if [[ "$require_fuzz" -eq 1 ]]; then
    die "缺少 cargo-fuzz；Linux fuzz 验收不能跳过"
  fi
  printf '未安装 cargo-fuzz：仅完成 fuzz target 静态存在性检查。\n' >&2
  exit 0
fi

target_dir="$SHADOWSOCKS_RUST_PLUS_ROOT/.cache/fuzz-target"
mkdir -p "$target_dir"
CARGO_TARGET_DIR="$target_dir" cargo fuzz run \
  --manifest-path "$manifest" audit_protocol -- \
  -max_total_time="$seconds" -max_len=8192 -close_fd_mask=3
