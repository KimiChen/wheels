#!/usr/bin/env bash
set -euo pipefail

source "$(dirname "$0")/lib.sh"

usage() {
  printf '用法：%s [--source <已准备源码目录>] [--no-integration] [--without-audit] [--coverage-status <JSON>] [--print-gate]\n' "$(basename "$0")" >&2
}

source_dir=""
run_integration=1
run_audit=1
coverage_status="${SHADOWSOCKS_TEST_COVERAGE_STATUS_FILE:-}"
print_gate=0
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
    --coverage-status)
      [[ $# -ge 2 ]] || { usage; exit 2; }
      coverage_status="$2"
      shift 2
      ;;
    --print-gate)
      print_gate=1
      shift
      ;;
    *)
      usage
      exit 2
      ;;
  esac
done

require_command cargo

# Parse the switches up front: an invalid value must fail before the workspace
# build, not after it.
require_audit_target="$(require_bool_env SHADOWSOCKS_REQUIRE_AUDIT_TARGET 1)"
run_fuzz="$(require_bool_env SHADOWSOCKS_RUN_FUZZ 0)"

host_os="$(uname -s)"
audit_native=0
auditd_crate_checked=0
auditd_runtime_available=0
auditd_runtime_executed=0
if [[ "$host_os" == "Linux" ]]; then
  audit_native=1
  auditd_runtime_available=1
fi

# The §16 Rust gate, as data: one `scope<TAB>arguments` line per command, in
# execution order.  There is exactly one place that runs them and exactly one
# place that prints them (`--print-gate`), so the documented gate and the
# executed gate cannot drift apart -- and a test can bind the gate without
# re-implementing this script's control flow.
#
# `--exclude shadowsocks-auditd` on the feature-off command keeps that command
# byte-identical on every host.  Note what it does *not* do: `--exclude` only
# drops a member from the set of packages to test, not from the build graph, so
# it would not save a non-Linux host from that crate's `compile_error!`.  What
# does is the root manifest -- `user-stats` does not pull `dep:shadowsocks-auditd`,
# only `user-audit` does.  Moving that dependency into `user-stats` would make
# this command fail on macOS with a Linux-only compile error despite the
# `--exclude`; re-check here if the root feature graph ever changes.
#
# `--lib --bins` on the workspace commands selects no integration target at all,
# so every target that still belongs in the gate is named below.  §16 sorts them
# into overlay-owned, pure-loopback and public-network; the first two run here,
# the third is exempt.  The loopback ones cover the UDP data path this overlay
# modifies -- dropping them along with the public-network targets would be a
# silent loss of coverage, not a narrowing.
gate_commands() {
  printf 'always\t%s\n' \
    "--locked --workspace --lib --bins --features user-stats --no-fail-fast --exclude shadowsocks-auditd"
  printf 'linux-audit\t%s\n' \
    "--locked --workspace --lib --bins --features user-audit --no-fail-fast"
  printf 'always\t%s\n' \
    "--locked --no-fail-fast -p shadowsocks --test tcp_eih_user --features aead-cipher-2022,user-stats"
  printf 'always\t%s\n' \
    "--locked --no-fail-fast -p shadowsocks --test udp --features aead-cipher-2022,user-stats"
  printf 'always\t%s\n' "--locked --no-fail-fast --test udp --features user-stats"
  printf 'always\t%s\n' "--locked --no-fail-fast --test tunnel --features user-stats udp_tunnel"
}

if [[ "$print_gate" -eq 1 ]]; then
  gate_commands
  exit 0
fi

if [[ -n "$coverage_status" ]]; then
  coverage_status="$(absolute_path "$coverage_status")"
fi

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

while IFS=$'\t' read -r gate_scope gate_args; do
  if [[ "$gate_scope" == "linux-audit" && ! ( "$run_audit" -eq 1 && "$audit_native" -eq 1 ) ]]; then
    continue
  fi
  # `gate_args` is deliberately unquoted: it carries a whole argument list.
  # shellcheck disable=SC2086
  CARGO_TARGET_DIR="$target_dir" cargo test \
    --manifest-path "$source_dir/Cargo.toml" $gate_args
done < <(gate_commands)

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
    auditd_crate_checked=1
  else
    audit_target="${SHADOWSOCKS_AUDIT_CHECK_TARGET:-x86_64-unknown-linux-gnu}"
    audit_libdir="$(rustc --print target-libdir --target "$audit_target" 2>/dev/null || true)"
    if [[ -d "$audit_libdir" ]]; then
      printf '非 Linux 主机：auditd 使用 %s 做跨目标 cargo check（不运行 foreign test binary）。\n' "$audit_target" >&2
      CARGO_TARGET_DIR="$target_dir" cargo check \
        --manifest-path "$source_dir/Cargo.toml" \
        --locked --target "$audit_target" -p shadowsocks-auditd --all-targets
      auditd_crate_checked=1
    elif [[ "$require_audit_target" == 1 ]]; then
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
python3 "$SHADOWSOCKS_RUST_PLUS_ROOT/tests/test_script_switches.py"
python3 "$SHADOWSOCKS_RUST_PLUS_ROOT/tests/benchmark_audit.py" \
  --events 2000 --producers 4 --queue-capacity 128 --spool-capacity 256 >/dev/null

if [[ "$run_fuzz" == 1 ]]; then
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
    auditd_runtime_executed=1
  fi
fi

if [[ -n "$coverage_status" ]]; then
  write_test_coverage_status "$coverage_status" \
    "$run_audit" "$run_integration" "$auditd_crate_checked" \
    "$auditd_runtime_available" "$auditd_runtime_executed"
fi

if [[ "$run_audit" -eq 0 ]]; then
  # `--without-audit` now also drops auditd from the feature-off workspace run,
  # so on Linux this mode compiles the crate nowhere at all.  Say so rather than
  # printing an unqualified pass that covers less than the macOS default does.
  printf '测试通过（--without-audit：auditd crate、audit-protocol 与 user-audit feature 路径本次一行都没有编译）。\n'
elif [[ "$run_audit" -eq 1 && "$auditd_crate_checked" -eq 0 ]]; then
  printf '测试通过，但覆盖面不完整：auditd crate 与 user_audit.rs 未编译，auditd Linux runtime 未执行。\n'
elif [[ "$run_audit" -eq 1 && "$auditd_runtime_executed" -eq 0 ]]; then
  if [[ "$auditd_runtime_available" -eq 0 ]]; then
    printf '测试通过（auditd crate 已交叉检查；auditd Linux runtime 未在当前主机执行）。\n'
  else
    printf '测试通过（auditd crate 已检查；auditd Linux runtime 未在本次运行执行）。\n'
  fi
else
  printf '测试通过。\n'
fi
