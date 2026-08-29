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
auditd_crate_checked=1
auditd_runtime_available=0
if [[ "$host_os" == "Linux" ]]; then
  audit_native=1
  auditd_runtime_available=1
fi

workspace_args=(--workspace --lib --bins --no-fail-fast)
run_workspace_tests() {
  local feature_set="$1"
  shift
  CARGO_TARGET_DIR="$target_dir" cargo test \
    --manifest-path "$source_dir/Cargo.toml" \
    --locked "${workspace_args[@]}" --features "$feature_set" "$@"
}

# `--lib --bins` is deliberate.  The locked upstream integration targets make
# public-network assumptions (including a stale HTTP/1.0 response assertion),
# so they are a separate upstream-baseline diagnostic rather than this gate.
# Keep the feature-off regression independent from Cargo feature unification;
# on Linux the audit-enabled workspace is then run as a second, explicit gate.
run_workspace_tests user-stats --exclude shadowsocks-auditd
if [[ "$run_audit" -eq 1 && "$audit_native" -eq 1 ]]; then
  run_workspace_tests user-audit
fi

CARGO_TARGET_DIR="$target_dir" cargo test \
  --manifest-path "$source_dir/Cargo.toml" \
  --locked --no-fail-fast -p shadowsocks --test tcp_eih_user \
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
    if [[ -d "$audit_libdir" ]]; then
      printf '非 Linux 主机：auditd 使用 %s 做跨目标 cargo check（不运行 foreign test binary）。\n' "$audit_target" >&2
      CARGO_TARGET_DIR="$target_dir" cargo check \
        --manifest-path "$source_dir/Cargo.toml" \
        --locked --target "$audit_target" -p shadowsocks-auditd --all-targets
    elif [[ "${SHADOWSOCKS_REQUIRE_AUDIT_TARGET:-1}" == 1 ]]; then
      die "非 Linux 主机缺少 auditd 交叉检查 target：$audit_target（\`rustup target add $audit_target\` 安装；确知要放弃该覆盖面时用 SHADOWSOCKS_REQUIRE_AUDIT_TARGET=0 显式降级）"
    else
      auditd_crate_checked=0
      printf '未验证（已被 SHADOWSOCKS_REQUIRE_AUDIT_TARGET=0 显式降级）：auditd crate 与 user_audit.rs 在本次运行中一行都没有被编译；缺少 target %s。\n' \
        "$audit_target" >&2
    fi
  fi
fi

python3 "$SHADOWSOCKS_RUST_PLUS_ROOT/tests/check_audit_static.py" --source "$source_dir"
python3 "$SHADOWSOCKS_RUST_PLUS_ROOT/tests/test_check_audit_static.py"
python3 "$SHADOWSOCKS_RUST_PLUS_ROOT/tests/test_fuzz_target.py" --source "$source_dir"
python3 "$SHADOWSOCKS_RUST_PLUS_ROOT/tests/test_panic_abort.py" --source "$source_dir"
python3 "$SHADOWSOCKS_RUST_PLUS_ROOT/tests/test_benchmark_audit.py"
python3 "$SHADOWSOCKS_RUST_PLUS_ROOT/tests/test_benchmark_data_path.py"
python3 "$SHADOWSOCKS_RUST_PLUS_ROOT/tests/test_integration_audit.py"
python3 "$SHADOWSOCKS_RUST_PLUS_ROOT/tests/test_docs_consistency.py"
python3 "$SHADOWSOCKS_RUST_PLUS_ROOT/tests/benchmark_audit.py" \
  --events 2000 --producers 4 --queue-capacity 128 --spool-capacity 256 >/dev/null

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
  if [[ "$run_audit" -eq 1 && "$auditd_runtime_available" -eq 1 ]]; then
    [[ -f "$SHADOWSOCKS_RUST_PLUS_ROOT/tests/integration_audit.py" ]] || \
      die "缺少 auditd 真实集成测试：tests/integration_audit.py"
    CARGO_TARGET_DIR="$target_dir" \
      python3 "$SHADOWSOCKS_RUST_PLUS_ROOT/tests/integration_audit.py" --source "$source_dir" \
        --auditd-binary "$target_dir/debug/shadowsocks-auditd"
  fi
fi

if [[ "$run_audit" -eq 1 && "$auditd_crate_checked" -eq 0 ]]; then
  printf '测试通过，但覆盖面不完整：auditd crate 与 user_audit.rs 未编译，auditd Linux runtime 未执行。\n'
elif [[ "$run_audit" -eq 1 && "$auditd_runtime_available" -eq 0 ]]; then
  printf '测试通过（auditd crate 已交叉检查；auditd Linux runtime 未在当前主机执行）。\n'
else
  printf '测试通过。\n'
fi
