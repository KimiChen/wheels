#!/usr/bin/env bash
set -euo pipefail

# 发布链路的环境输入必须完全由脚本与 lock 文件决定，不接受未跟踪的 .env。
SHADOWSOCKS_RUST_PLUS_NO_DOTENV=1
export SHADOWSOCKS_RUST_PLUS_NO_DOTENV
source "$(dirname "$0")/lib.sh"
# shellcheck disable=SC1091
source "$SHADOWSOCKS_RUST_PLUS_ROOT/packaging/release-toolchain.lock"

usage() {
  printf '用法：%s [--repository <锁定上游的本地镜像或 URL>] [--output-dir <目录>]\n' \
    "$(basename "$0")" >&2
}

directory_identity() {
  local path="$1"
  stat -Lc '%d:%i' -- "$path" 2>/dev/null || stat -f '%d:%i' -- "$path"
}

require_empty_release_directory() {
  local path="$1"
  local entries
  entries="$(find "$path" -mindepth 1 -maxdepth 1 -print -quit)"
  [[ -z "$entries" ]] || die "发布输出目录必须为空，拒绝覆盖或保留未绑定文件：$path"
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

# Refuse a destination that overlaps the development output tree or already
# contains release payloads before spending time on reproducibility builds.
# A signed directory is immutable; publishing a new candidate must use a fresh
# directory.
if [[ "$output_dir" != /* ]]; then
  output_dir="$PWD/$output_dir"
fi
case "$output_dir" in
  "$SHADOWSOCKS_RUST_PLUS_ROOT/.cache/dev-dist"|"$SHADOWSOCKS_RUST_PLUS_ROOT/.cache/dev-dist"/*)
    die "发布构建不得写入开发产物目录：$output_dir"
    ;;
esac
[[ ! -L "$output_dir" ]] || die "发布输出目录不能是符号链接：$output_dir"
if [[ -e "$output_dir" ]]; then
  output_dir="$(cd "$output_dir" && pwd -P)" || die "无法解析发布产物目录：$output_dir"
else
  mkdir -p "$(dirname "$output_dir")"
  output_parent="$(cd "$(dirname "$output_dir")" && pwd -P)" || die "无法解析发布产物父目录：$output_dir"
  output_dir="$output_parent/$(basename "$output_dir")"
  mkdir "$output_dir" || die "无法创建发布产物目录：$output_dir"
fi
case "$output_dir" in
  "$SHADOWSOCKS_RUST_PLUS_ROOT/.cache/dev-dist"|"$SHADOWSOCKS_RUST_PLUS_ROOT/.cache/dev-dist"/*)
    die "发布构建不得写入开发产物目录：$output_dir"
    ;;
esac
require_empty_release_directory "$output_dir"
output_identity="$(directory_identity "$output_dir")" || die "无法取得发布输出目录身份：$output_dir"
output_device="${output_identity%%:*}"
output_inode="${output_identity#*:}"
[[ "$output_device" =~ ^[0-9]+$ && "$output_inode" =~ ^[0-9]+$ ]] || \
  die "发布输出目录身份格式错误：$output_dir"

validate_release_output_directory() {
  [[ -d "$output_dir" && ! -L "$output_dir" ]] || die "发布输出目录已在构建期间被替换"
  [[ "$(cd "$output_dir" && pwd -P)" == "$output_dir" ]] || \
    die "发布输出目录解析结果已在构建期间变化"
  [[ "$(directory_identity "$output_dir")" == "$output_identity" ]] || \
    die "发布输出目录 inode 已在构建期间变化"
  require_empty_release_directory "$output_dir"
}

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

# Do not let ambient compiler/linker/profile overrides become an unrecorded build input.
unset AR ARFLAGS CC CFLAGS CPPFLAGS CXX CXXFLAGS LDFLAGS RANLIB RANLIBFLAGS
unset RUSTC RUSTC_WRAPPER
unset RUSTC_WORKSPACE_WRAPPER RUSTFLAGS RUSTDOCFLAGS CARGO_BUILD_RUSTC
unset CARGO_BUILD_RUSTC_WRAPPER CARGO_BUILD_RUSTC_WORKSPACE_WRAPPER
unset CARGO_BUILD_RUSTFLAGS CARGO_ENCODED_RUSTFLAGS
unset CARGO_ALIAS_ZIGBUILD
unset CARGO_TARGET_X86_64_UNKNOWN_LINUX_MUSL_LINKER
unset CARGO_TARGET_X86_64_UNKNOWN_LINUX_MUSL_RUSTFLAGS
unset CARGO_PROFILE_RELEASE_CODEGEN_UNITS CARGO_PROFILE_RELEASE_DEBUG
unset CARGO_PROFILE_RELEASE_LTO CARGO_PROFILE_RELEASE_OPT_LEVEL
unset CARGO_PROFILE_RELEASE_PANIC CARGO_PROFILE_RELEASE_RPATH CARGO_PROFILE_RELEASE_STRIP
unset CARGO_PROFILE_RELEASE_OVERFLOW_CHECKS CARGO_PROFILE_RELEASE_DEBUG_ASSERTIONS
unset RUSTC_BOOTSTRAP

normalized_release_target="${RELEASE_TARGET//-/_}"
uppercase_release_target="$(printf '%s' "$normalized_release_target" | tr '[:lower:]' '[:upper:]')"
clean_release_environment_args=()
for cc_variable in AR ARFLAGS CC CFLAGS CXX CXXFLAGS RANLIB RANLIBFLAGS; do
  clean_release_environment_args+=(
    -u "$cc_variable"
    -u "${cc_variable}_${RELEASE_TARGET}"
    -u "${cc_variable}_${normalized_release_target}"
    -u "${cc_variable}_${uppercase_release_target}"
    -u "${RELEASE_TARGET}_${cc_variable}"
    -u "${normalized_release_target}_${cc_variable}"
    -u "${uppercase_release_target}_${cc_variable}"
    -u "TARGET_${cc_variable}"
    -u "HOST_${cc_variable}"
  )
done
clean_release_environment_args+=(
  -u CARGO_ALIAS_ZIGBUILD
  -u CARGO_BUILD_RUSTC_WORKSPACE_WRAPPER
  -u CROSS_COMPILE
  -u CRATE_CC_NO_DEFAULTS
  -u CC_SHELL_ESCAPED_FLAGS
  -u CC_KNOWN_WRAPPER_CUSTOM
  -u "CARGO_TARGET_${uppercase_release_target}_LINKER"
  -u "CARGO_TARGET_${uppercase_release_target}_RUSTFLAGS"
  -u "CARGO_TARGET_${uppercase_release_target}_RUNNER"
)
run_with_clean_release_environment() {
  env "${clean_release_environment_args[@]}" "$@"
}

temp_dir="$(mktemp -d "${TMPDIR:-/tmp}/shadowsocks-rust-plus.XXXXXX")"
trap 'safe_remove_temp_dir "$temp_dir"' EXIT
source_a="$temp_dir/source-a"
source_b="$temp_dir/source-b"
target_a="$temp_dir/target-a"
target_b="$temp_dir/target-b"
cargo_home_a="$temp_dir/cargo-home-a"
cargo_home_b="$temp_dir/cargo-home-b"
mkdir -m 0700 "$cargo_home_a" "$cargo_home_b"

prepare_args=("$source_a")
if [[ -n "$repository_override" ]]; then
  prepare_args+=("$repository_override")
fi
"$SHADOWSOCKS_RUST_PLUS_ROOT/scripts/prepare-source.sh" "${prepare_args[@]}"
mkdir "$source_b"
cp -R "$source_a/." "$source_b/"

source_date_epoch="$(git -C "$SHADOWSOCKS_RUST_PLUS_ROOT" show -s --format=%ct "$actual_head")"
[[ "$source_date_epoch" =~ ^[1-9][0-9]*$ ]] || die "overlay commit 时间戳无效"
expected_prepared_tree_sha256="$(lock_value prepared_tree_sha256)"
[[ "$expected_prepared_tree_sha256" =~ ^[0-9a-f]{64}$ ]] || \
  die "upstream.lock prepared_tree_sha256 格式错误"

[[ -f "$source_a/crates/shadowsocks-auditd/Cargo.toml" ]] || \
  die "user-audit 已启用但准备源码缺少 shadowsocks-auditd crate"

receipt_common_args=(
  --version "$(lock_value tag)"
  --upstream-commit "$(lock_value commit)"
  --overlay-commit "$actual_head"
  --source-date-epoch "$source_date_epoch"
  --expected-prepared-tree-sha256 "$expected_prepared_tree_sha256"
  --rustc-version "$actual_rustc_version"
  --rustc-commit "$actual_rustc_commit"
  --cargo-version "$actual_cargo_version"
  --cargo-zigbuild-version "$actual_cargo_zigbuild_version"
  --zig-version "$actual_zig_version"
  --python-version "$actual_python_version"
)
run_with_clean_release_environment \
  "$SHADOWSOCKS_RUST_PLUS_ROOT/scripts/release-artifact.py" build-and-receipt \
  --build-id build-a \
  --source-root "$source_a" \
  --target-root "$target_a" \
  --cargo-home "$cargo_home_a" \
  "${receipt_common_args[@]}"
run_with_clean_release_environment \
  "$SHADOWSOCKS_RUST_PLUS_ROOT/scripts/release-artifact.py" build-and-receipt \
  --build-id build-b \
  --source-root "$source_b" \
  --target-root "$target_b" \
  --cargo-home "$cargo_home_b" \
  "${receipt_common_args[@]}"

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

receipt_a="$target_a/build-receipt.json"
receipt_b="$target_b/build-receipt.json"

validate_release_output_directory
"$SHADOWSOCKS_RUST_PLUS_ROOT/scripts/release-artifact.py" package-multi \
  --binary "$binary_a" \
  --auditd-binary "$auditd_binary_a" \
  --second-binary "$binary_b" \
  --second-auditd-binary "$auditd_binary_b" \
  --first-build-receipt "$receipt_a" \
  --second-build-receipt "$receipt_b" \
  --first-source-root "$source_a" \
  --second-source-root "$source_b" \
  --first-target-root "$target_a" \
  --second-target-root "$target_b" \
  --output-dir "$output_dir" \
  --expected-output-device "$output_device" \
  --expected-output-inode "$output_inode" \
  --version "$(lock_value tag)" \
  --upstream-commit "$(lock_value commit)" \
  --overlay-commit "$actual_head" \
  --source-date-epoch "$source_date_epoch" \
  --expected-prepared-tree-sha256 "$expected_prepared_tree_sha256" \
  --rustc-version "$actual_rustc_version" \
  --rustc-commit "$actual_rustc_commit" \
  --cargo-version "$actual_cargo_version" \
  --cargo-zigbuild-version "$actual_cargo_zigbuild_version" \
  --zig-version "$actual_zig_version" \
  --python-version "$actual_python_version"

printf '可复现性检查通过：两次独立构建 SHA-256 均为 %s\n' "$binary_sha256"
printf 'shadowsocks-auditd SHA-256：%s\n' "$auditd_binary_sha256"
printf '下一步：离线签署 manifest，再用 scripts/verify-release.sh 验签；本脚本不部署产物。\n'
