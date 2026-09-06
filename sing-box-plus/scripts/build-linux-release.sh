#!/usr/bin/env bash
set -euo pipefail

source "$(dirname "$0")/lib.sh"

# 可复现发布构建：两次独立路径构建逐字节一致才产出 manifest + SHA-256。
#
# 「两次」不是仪式：Go 的 -trimpath 之外仍有若干路径与环境泄漏点，一次构建的哈希
# 无法区分「可复现」与「碰巧一样」。两次用不同的工作目录跑，能把这类泄漏暴露成不一致。

require_command go
require_command python3

usage() {
  printf '用法：%s <输出目录> [版本号]\n' "$(basename "$0")" >&2
}

[[ $# -ge 1 && $# -le 2 ]] || { usage; exit 2; }

output_dir="$(absolute_path "$1")"
version="${2:-${SING_BOX_PLUS_VERSION:-0.1.0}}"
[[ ! -e "$output_dir" ]] || die "输出目录已存在，拒绝覆盖：$output_dir"

upstream_tag="$(lock_value tag)"
upstream_commit="$(lock_value commit)"
prepared_tree="$(lock_value prepared_tree_sha256)"
tags="$(production_build_tags)"
go_version="$(go env GOVERSION)"

temp_dir="$(mktemp -d "${TMPDIR:-/tmp}/sing-box-plus.XXXXXX")"
trap 'safe_remove_temp_dir "$temp_dir"' EXIT

# release/LDFLAGS 的内容在 1.14 已改写，必须从准备好的源码树读取而不是硬编码。
source_dir="$temp_dir/source"
"$SING_BOX_PLUS_ROOT/scripts/prepare-source.sh" "$source_dir" >/dev/null
[[ -f "$source_dir/release/LDFLAGS" ]] || die "上游源码树缺少 release/LDFLAGS"
upstream_ldflags="$(cat "$source_dir/release/LDFLAGS")"

targets=("linux/amd64" "linux/arm64")
mkdir -p "$output_dir"

build_once() {
  local build_id="$1" target="$2" destination="$3"
  local goos="${target%%/*}" goarch="${target##*/}"
  local build_root="$temp_dir/build-$build_id"
  mkdir -p "$build_root"
  # 用不同的 GOCACHE 与工作副本跑，避免第二次构建只是把第一次的缓存原样吐出来。
  ( cd "$SING_BOX_PLUS_ROOT" && \
    CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
    GOCACHE="$build_root/gocache" \
    SOURCE_DATE_EPOCH=0 \
    go build -trimpath -buildvcs=false \
      -tags "$tags" \
      -ldflags "-s -w -buildid= $upstream_ldflags \
        -X github.com/sagernet/sing-box/constant.Version=$upstream_tag \
        -X main.Version=$version \
        -X main.UpstreamCommit=$upstream_commit" \
      -o "$destination" ./cmd/sing-box-plus )
}

artifact_args=()
target_args=()
for target in "${targets[@]}"; do
  suffix="${target/\//-}"
  first="$temp_dir/first-$suffix"
  second="$temp_dir/second-$suffix"
  build_once first "$target" "$first"
  build_once second "$target" "$second"
  if ! cmp -s "$first" "$second"; then
    die "两次独立构建不一致（$target）：产物不可复现，拒绝发布"
  fi
  final="$output_dir/sing-box-plus-$version-$suffix"
  cp "$first" "$final"
  chmod 0755 "$final"
  artifact_args+=(--artifact "$final")
  target_args+=(--target "$target")
done

python3 "$SING_BOX_PLUS_ROOT/scripts/release-artifact.py" manifest \
  --version "$version" \
  --upstream-tag "$upstream_tag" \
  --upstream-commit "$upstream_commit" \
  --prepared-tree-sha256 "$prepared_tree" \
  --build-tags "$tags" \
  --go-version "$go_version" \
  "${target_args[@]}" \
  "${artifact_args[@]}" \
  --output "$output_dir/manifest.json"

( cd "$output_dir" && shasum -a 256 ./* > SHA256SUMS 2>/dev/null || sha256sum ./* > SHA256SUMS )
printf '发布产物已生成：%s\n' "$output_dir"
