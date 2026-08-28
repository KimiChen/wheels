#!/usr/bin/env bash
set -euo pipefail

source "$(dirname "$0")/lib.sh"

usage() {
  printf '用法：%s [--source <已准备源码目录>] [--no-integration] [--without-audit]\n' "$(basename "$0")" >&2
}

source_dir=""
run_integration=1
run_audit=1
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
    --without-audit)
      run_audit=0
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

[[ -f "$SHADOWSOCKS_RUST_PLUS_ROOT/tests/golden_vectors.json" ]] || die "缺少共享 golden vectors"
protocol_vectors="$source_dir/crates/shadowsocks-audit-protocol/src/golden_vectors.json"
[[ -f "$protocol_vectors" ]] || die "源码缺少 protocol golden vectors"
cmp -s "$SHADOWSOCKS_RUST_PLUS_ROOT/tests/golden_vectors.json" "$protocol_vectors" || \
  die "Rust protocol 与 mock collector 的 golden vectors 不一致"

target_dir="$SHADOWSOCKS_RUST_PLUS_ROOT/.cache/cargo-target"
host_os="$(uname -s)"
audit_native=0
if [[ "$host_os" == "Linux" ]]; then
  audit_native=1
fi

features=user-stats
if [[ "$run_audit" -eq 1 && "$audit_native" -eq 1 ]]; then
  features=user-audit
fi
workspace_args=(--workspace --lib --bins --features "$features")
if [[ "$audit_native" -eq 0 ]]; then
  # The auditd crate has an intentional Linux-only compile gate.  Keep the
  # cross-platform workspace regression useful by excluding that member from
  # the host test run; it is checked below for a Linux target instead.
  workspace_args+=(--exclude shadowsocks-auditd)
fi
CARGO_TARGET_DIR="$target_dir" cargo test \
  --manifest-path "$source_dir/Cargo.toml" \
  --locked "${workspace_args[@]}"

CARGO_TARGET_DIR="$target_dir" cargo test \
  --manifest-path "$source_dir/Cargo.toml" \
  --locked -p shadowsocks --test tcp_eih_user \
  --features aead-cipher-2022,user-stats

# Compile the normal server path independently so a feature-gating mistake cannot
# hide behind Cargo's workspace feature unification.
CARGO_TARGET_DIR="$target_dir" cargo check \
  --manifest-path "$source_dir/Cargo.toml" \
  --locked -p shadowsocks-service --no-default-features --features server

if [[ "$run_audit" -eq 1 ]]; then
  audit_manifest="$source_dir/crates/shadowsocks-auditd/Cargo.toml"
  protocol_manifest="$source_dir/crates/shadowsocks-audit-protocol/Cargo.toml"
  [[ -f "$protocol_manifest" ]] || die "user-audit 测试缺少 shadowsocks-audit-protocol crate"
  [[ -f "$audit_manifest" ]] || die "user-audit 测试缺少 shadowsocks-auditd crate"
  CARGO_TARGET_DIR="$target_dir" cargo test \
    --manifest-path "$source_dir/Cargo.toml" \
    --locked -p shadowsocks-audit-protocol
  if [[ "$audit_native" -eq 1 ]]; then
    CARGO_TARGET_DIR="$target_dir" cargo test \
      --manifest-path "$source_dir/Cargo.toml" \
      --locked -p shadowsocks-auditd
  else
    audit_target="${SHADOWSOCKS_AUDIT_CHECK_TARGET:-x86_64-unknown-linux-gnu}"
    audit_libdir="$(rustc --print target-libdir --target "$audit_target" 2>/dev/null || true)"
    [[ -d "$audit_libdir" ]] || die "非 Linux 主机需要已安装 Rust target 以检查 Linux-only auditd：$audit_target"
    printf '非 Linux 主机：auditd 使用 %s 做跨目标 cargo check（不运行 foreign test binary）。\n' "$audit_target" >&2
    CARGO_TARGET_DIR="$target_dir" cargo check \
      --manifest-path "$source_dir/Cargo.toml" \
      --locked --target "$audit_target" -p shadowsocks-auditd --all-targets
  fi
fi

python3 "$SHADOWSOCKS_RUST_PLUS_ROOT/tests/check_audit_static.py" --source "$source_dir"
python3 "$SHADOWSOCKS_RUST_PLUS_ROOT/tests/test_fuzz_target.py"
python3 "$SHADOWSOCKS_RUST_PLUS_ROOT/tests/test_panic_abort.py" --source "$source_dir"
python3 "$SHADOWSOCKS_RUST_PLUS_ROOT/tests/test_benchmark_audit.py"

if [[ "${SHADOWSOCKS_RUN_FUZZ:-0}" == 1 ]]; then
  "$SHADOWSOCKS_RUST_PLUS_ROOT/scripts/test-fuzz.sh" --source "$source_dir" \
    --seconds "${SHADOWSOCKS_FUZZ_SECONDS:-30}" --require
fi

if [[ "$run_integration" -eq 1 ]]; then
  require_command python3
  python3 "$SHADOWSOCKS_RUST_PLUS_ROOT/tests/test_cluster_users.py"
  python3 "$SHADOWSOCKS_RUST_PLUS_ROOT/tests/test_http_unix.py"
  python3 "$SHADOWSOCKS_RUST_PLUS_ROOT/tests/test_release_artifact.py"
  python3 "$SHADOWSOCKS_RUST_PLUS_ROOT/tests/test_settlement.py"
  python3 "$SHADOWSOCKS_RUST_PLUS_ROOT/tests/test_mock_collector.py"
  python3 "$SHADOWSOCKS_RUST_PLUS_ROOT/tests/test_audit_packaging.py"
  [[ -f "$SHADOWSOCKS_RUST_PLUS_ROOT/tests/integration_user_stats.py" ]] || \
    die "缺少真实数据面集成测试：tests/integration_user_stats.py"
  CARGO_TARGET_DIR="$target_dir" \
    python3 "$SHADOWSOCKS_RUST_PLUS_ROOT/tests/integration_user_stats.py" --source "$source_dir"
  if [[ "$run_audit" -eq 1 ]]; then
    [[ -f "$SHADOWSOCKS_RUST_PLUS_ROOT/tests/integration_audit.py" ]] || \
      die "缺少 auditd 真实集成测试：tests/integration_audit.py"
    CARGO_TARGET_DIR="$target_dir" \
      python3 "$SHADOWSOCKS_RUST_PLUS_ROOT/tests/integration_audit.py" --source "$source_dir" \
        --auditd-binary "$target_dir/debug/shadowsocks-auditd"
  fi
fi

printf '测试通过。\n'
