#!/usr/bin/env bash
set -euo pipefail

source "$(dirname "$0")/lib.sh"
# shellcheck disable=SC1091
source "$SHADOWSOCKS_RUST_PLUS_ROOT/packaging/release-toolchain.lock"

usage() {
  printf '用法：%s [--repository <锁定上游的本地镜像或 URL>] [--output-dir <目录>]\n' \
    "$(basename "$0")" >&2
}

repository_override=""
output_dir="$SHADOWSOCKS_RUST_PLUS_ROOT/dist"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --repository)
      [[ $# -ge 2 ]] || { usage; exit 2; }
      repository_override="$2"
      shift 2
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
require_command cargo-zigbuild
require_command cmp
require_command git
require_command python3
require_command rustc
require_command shasum
require_command zig

[[ "$RELEASE_TARGET" == x86_64-unknown-linux-musl ]] || \
  die "发布 target 锁定文件错误：$RELEASE_TARGET"

actual_head="$(git -C "$SHADOWSOCKS_RUST_PLUS_ROOT" rev-parse HEAD)"
[[ "$actual_head" =~ ^[0-9a-f]{40}$ ]] || die "无法取得 overlay commit"
[[ -z "$(git -C "$SHADOWSOCKS_RUST_PLUS_ROOT" status --porcelain=v1 --untracked-files=all)" ]] || \
  die "发布构建要求 overlay 工作树完全干净，请先审查并提交"

actual_rustc_version="$(rustc -Vv | awk '$1 == "release:" { print $2 }')"
actual_rustc_commit="$(rustc -Vv | awk '$1 == "commit-hash:" { print $2 }')"
actual_cargo_version="$(cargo -V | awk '{ print $2 }')"
actual_cargo_zigbuild_version="$(cargo-zigbuild --version | awk '{ print $2 }')"
actual_zig_version="$(zig version)"
actual_python_version="$(python3 -c 'import platform; print(platform.python_version())')"
actual_zlib_version="$(python3 -c 'import zlib; print(zlib.ZLIB_RUNTIME_VERSION)')"

[[ "$actual_rustc_version" == "$RELEASE_RUSTC_VERSION" ]] || \
  die "rustc 版本不匹配：期望 $RELEASE_RUSTC_VERSION，实际 $actual_rustc_version"
[[ "$actual_rustc_commit" == "$RELEASE_RUSTC_COMMIT" ]] || \
  die "rustc commit 不匹配：期望 $RELEASE_RUSTC_COMMIT，实际 $actual_rustc_commit"
[[ "$actual_cargo_version" == "$RELEASE_CARGO_VERSION" ]] || \
  die "cargo 版本不匹配：期望 $RELEASE_CARGO_VERSION，实际 $actual_cargo_version"
[[ "$actual_cargo_zigbuild_version" == "$RELEASE_CARGO_ZIGBUILD_VERSION" ]] || \
  die "cargo-zigbuild 版本不匹配：期望 $RELEASE_CARGO_ZIGBUILD_VERSION，实际 $actual_cargo_zigbuild_version"
[[ "$actual_zig_version" == "$RELEASE_ZIG_VERSION" ]] || \
  die "zig 版本不匹配：期望 $RELEASE_ZIG_VERSION，实际 $actual_zig_version"
[[ "$actual_python_version" == "$RELEASE_PYTHON_VERSION" ]] || \
  die "Python 版本不匹配：期望 $RELEASE_PYTHON_VERSION，实际 $actual_python_version"
[[ "$actual_zlib_version" == "$RELEASE_ZLIB_VERSION" ]] || \
  die "zlib 版本不匹配：期望 $RELEASE_ZLIB_VERSION，实际 $actual_zlib_version"

release_user_home="${HOME:?HOME 未设置}"
release_cargo_home="${CARGO_HOME:-$release_user_home/.cargo}"
[[ -d "$release_cargo_home" ]] || die "Cargo home 不存在：$release_cargo_home"
release_cargo_home="$(cd "$release_cargo_home" && pwd -P)"

# Do not let ambient compiler/linker/profile overrides become an unrecorded build input.
unset AR CC CFLAGS CPPFLAGS CXX CXXFLAGS LDFLAGS RUSTC RUSTC_WRAPPER
unset RUSTC_WORKSPACE_WRAPPER RUSTFLAGS RUSTDOCFLAGS CARGO_BUILD_RUSTC
unset CARGO_BUILD_RUSTC_WRAPPER CARGO_BUILD_RUSTFLAGS CARGO_ENCODED_RUSTFLAGS
unset CARGO_TARGET_X86_64_UNKNOWN_LINUX_MUSL_LINKER
unset CARGO_TARGET_X86_64_UNKNOWN_LINUX_MUSL_RUSTFLAGS
unset CARGO_PROFILE_RELEASE_CODEGEN_UNITS CARGO_PROFILE_RELEASE_DEBUG
unset CARGO_PROFILE_RELEASE_LTO CARGO_PROFILE_RELEASE_OPT_LEVEL
unset CARGO_PROFILE_RELEASE_PANIC CARGO_PROFILE_RELEASE_RPATH CARGO_PROFILE_RELEASE_STRIP

temp_dir="$(mktemp -d "${TMPDIR:-/tmp}/shadowsocks-rust-plus.XXXXXX")"
trap 'safe_remove_temp_dir "$temp_dir"' EXIT
source_a="$temp_dir/source-a"
source_b="$temp_dir/source-b"
target_a="$temp_dir/target-a"
target_b="$temp_dir/target-b"

prepare_args=("$source_a")
if [[ -n "$repository_override" ]]; then
  prepare_args+=("$repository_override")
fi
"$SHADOWSOCKS_RUST_PLUS_ROOT/scripts/prepare-source.sh" "${prepare_args[@]}"
mkdir "$source_b"
cp -R "$source_a/." "$source_b/"

source_date_epoch="$(git -C "$SHADOWSOCKS_RUST_PLUS_ROOT" show -s --format=%ct "$actual_head")"
[[ "$source_date_epoch" =~ ^[1-9][0-9]*$ ]] || die "overlay commit 时间戳无效"
stable_build_time_utc="$(python3 - "$source_date_epoch" <<'PY'
from datetime import datetime, timezone
import sys

print(datetime.fromtimestamp(int(sys.argv[1]), timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"))
PY
)"
[[ "$stable_build_time_utc" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$ ]] \
  || die "无法生成稳定 UTC build time"

build_once() {
  local prepared_source="$1"
  local target_dir="$2"
  local encoded_flags

  encoded_flags="--remap-path-prefix=${prepared_source}=/usr/src/shadowsocks-rust"
  encoded_flags+=$'\x1f'
  encoded_flags+="--remap-path-prefix=${target_dir}=/usr/src/target"
  encoded_flags+=$'\x1f'
  encoded_flags+="--remap-path-prefix=${release_cargo_home}=/usr/local/cargo"
  encoded_flags+=$'\x1f-C\x1flink-arg=-Wl,--build-id=none\x1f-C\x1fstrip=symbols'

  CARGO_ENCODED_RUSTFLAGS="$encoded_flags" \
    CARGO_INCREMENTAL=0 \
    CARGO_PROFILE_RELEASE_INCREMENTAL=false \
    CARGO_TARGET_DIR="$target_dir" \
    LANG=C \
    LC_ALL=C \
    SOURCE_DATE_EPOCH="$source_date_epoch" \
    SHADOWSOCKS_BUILD_TIME_UTC="$stable_build_time_utc" \
    TZ=UTC \
    ZERO_AR_DATE=1 \
    cargo zigbuild \
      --manifest-path "$prepared_source/Cargo.toml" \
      --locked \
      --release \
      --target "$RELEASE_TARGET" \
      --features user-audit \
      --bin ssserver
}

build_auditd_once() {
  local prepared_source="$1"
  local target_dir="$2"
  local encoded_flags

  encoded_flags="--remap-path-prefix=${prepared_source}=/usr/src/shadowsocks-rust"
  encoded_flags+=$'\x1f'
  encoded_flags+="--remap-path-prefix=${target_dir}=/usr/src/target"
  encoded_flags+=$'\x1f'
  encoded_flags+="--remap-path-prefix=${release_cargo_home}=/usr/local/cargo"
  encoded_flags+=$'\x1f-C\x1flink-arg=-Wl,--build-id=none\x1f-C\x1fstrip=symbols'

  CARGO_ENCODED_RUSTFLAGS="$encoded_flags" \
    CARGO_INCREMENTAL=0 \
    CARGO_PROFILE_RELEASE_INCREMENTAL=false \
    CARGO_TARGET_DIR="$target_dir" \
    LANG=C \
    LC_ALL=C \
    SOURCE_DATE_EPOCH="$source_date_epoch" \
    SHADOWSOCKS_BUILD_TIME_UTC="$stable_build_time_utc" \
    TZ=UTC \
    ZERO_AR_DATE=1 \
    cargo zigbuild \
      --manifest-path "$prepared_source/Cargo.toml" \
      --locked \
      --release \
      --target "$RELEASE_TARGET" \
      --features user-audit \
      --bin shadowsocks-auditd
}

[[ -f "$source_a/crates/shadowsocks-auditd/Cargo.toml" ]] || \
  die "user-audit 已启用但准备源码缺少 shadowsocks-auditd crate"

build_once "$source_a" "$target_a"
build_auditd_once "$source_a" "$target_a"
build_once "$source_b" "$target_b"
build_auditd_once "$source_b" "$target_b"

binary_a="$target_a/$RELEASE_TARGET/release/ssserver"
binary_b="$target_b/$RELEASE_TARGET/release/ssserver"
auditd_binary_a="$target_a/$RELEASE_TARGET/release/shadowsocks-auditd"
auditd_binary_b="$target_b/$RELEASE_TARGET/release/shadowsocks-auditd"
[[ -x "$binary_a" && -x "$binary_b" ]] || die "独立构建未生成两个 ssserver"
[[ -x "$auditd_binary_a" && -x "$auditd_binary_b" ]] || die "独立构建未生成两个 shadowsocks-auditd"
if ! cmp -s "$binary_a" "$binary_b"; then
  printf '第一次构建 SHA-256：%s\n' "$(shasum -a 256 "$binary_a" | awk '{ print $1 }')" >&2
  printf '第二次构建 SHA-256：%s\n' "$(shasum -a 256 "$binary_b" | awk '{ print $1 }')" >&2
  if [[ "${SHADOWSOCKS_RUST_PLUS_KEEP_FAILED_BUILD:-0}" == "1" ]]; then
    trap - EXIT
    printf '失败构建保留在：%s\n' "$temp_dir" >&2
  fi
  die "两次独立构建的 ssserver 不一致，拒绝发布"
fi
if ! cmp -s "$auditd_binary_a" "$auditd_binary_b"; then
  printf '第一次 auditd 构建 SHA-256：%s\n' "$(shasum -a 256 "$auditd_binary_a" | awk '{ print $1 }')" >&2
  printf '第二次 auditd 构建 SHA-256：%s\n' "$(shasum -a 256 "$auditd_binary_b" | awk '{ print $1 }')" >&2
  if [[ "${SHADOWSOCKS_RUST_PLUS_KEEP_FAILED_BUILD:-0}" == "1" ]]; then
    trap - EXIT
    printf '失败构建保留在：%s\n' "$temp_dir" >&2
  fi
  die "两次独立构建的 shadowsocks-auditd 不一致，拒绝发布"
fi

binary_sha256="$(shasum -a 256 "$binary_a" | awk '{ print $1 }')"
[[ "$binary_sha256" =~ ^[0-9a-f]{64}$ ]] || die "无法计算 ssserver SHA-256"
auditd_binary_sha256="$(shasum -a 256 "$auditd_binary_a" | awk '{ print $1 }')"
[[ "$auditd_binary_sha256" =~ ^[0-9a-f]{64}$ ]] || die "无法计算 shadowsocks-auditd SHA-256"

mkdir -p "$output_dir"
output_dir="$(cd "$output_dir" && pwd -P)"
"$SHADOWSOCKS_RUST_PLUS_ROOT/scripts/release-artifact.py" package-multi \
  --binary "$binary_a" \
  --auditd-binary "$auditd_binary_a" \
  --output-dir "$output_dir" \
  --version "$(lock_value tag)" \
  --upstream-commit "$(lock_value commit)" \
  --overlay-commit "$actual_head" \
  --source-date-epoch "$source_date_epoch" \
  --rustc-version "$actual_rustc_version" \
  --rustc-commit "$actual_rustc_commit" \
  --cargo-version "$actual_cargo_version" \
  --cargo-zigbuild-version "$actual_cargo_zigbuild_version" \
  --zig-version "$actual_zig_version" \
  --python-version "$actual_python_version" \
  --zlib-version "$actual_zlib_version"

printf '可复现性检查通过：两次独立构建 SHA-256 均为 %s\n' "$binary_sha256"
printf 'shadowsocks-auditd SHA-256：%s\n' "$auditd_binary_sha256"
printf '下一步：离线签署 manifest，再用 scripts/verify-release.sh 验签；本脚本不部署产物。\n'
