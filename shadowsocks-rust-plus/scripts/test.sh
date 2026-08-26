#!/usr/bin/env bash
set -euo pipefail

source "$(dirname "$0")/lib.sh"

usage() {
  printf '用法：%s [--source <已准备源码目录>] [--no-integration]\n' "$(basename "$0")" >&2
}

source_dir=""
run_integration=1
while [[ $# -gt 0 ]]; do
  case "$1" in
    --source)
      [[ $# -ge 2 ]] || { usage; exit 2; }
      source_dir="$2"
      shift 2
      ;;
    --no-integration)
      run_integration=0
      shift
      ;;
    *)
      usage
      exit 2
      ;;
  esac
done

require_command cargo

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
CARGO_TARGET_DIR="$target_dir" cargo test \
  --manifest-path "$source_dir/Cargo.toml" \
  --locked --workspace --lib --bins --features user-stats

CARGO_TARGET_DIR="$target_dir" cargo test \
  --manifest-path "$source_dir/Cargo.toml" \
  --locked -p shadowsocks --test tcp_eih_user \
  --features aead-cipher-2022,user-stats

# Compile the normal server path independently so a feature-gating mistake cannot
# hide behind Cargo's workspace feature unification.
CARGO_TARGET_DIR="$target_dir" cargo check \
  --manifest-path "$source_dir/Cargo.toml" \
  --locked -p shadowsocks-service --no-default-features --features server

if [[ "$run_integration" -eq 1 ]]; then
  require_command python3
  python3 "$SHADOWSOCKS_RUST_PLUS_ROOT/tests/test_http_unix.py"
  python3 "$SHADOWSOCKS_RUST_PLUS_ROOT/tests/test_settlement.py"
  [[ -f "$SHADOWSOCKS_RUST_PLUS_ROOT/tests/integration_user_stats.py" ]] || \
    die "缺少真实数据面集成测试：tests/integration_user_stats.py"
  CARGO_TARGET_DIR="$target_dir" \
    python3 "$SHADOWSOCKS_RUST_PLUS_ROOT/tests/integration_user_stats.py" --source "$source_dir"
fi

printf '测试通过。\n'
