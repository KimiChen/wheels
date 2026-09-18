#!/usr/bin/env bash
set -euo pipefail

source "$(dirname "$0")/lib.sh"

# 可复现发布构建：两次独立路径构建逐字节一致才产出 manifest + SHA-256。
#
# 「两次」不是仪式：Go 的 -trimpath 之外仍有若干路径与环境泄漏点，一次构建的哈希
# 无法区分「可复现」与「碰巧一样」。两次用不同的工作目录跑，能把这类泄漏暴露成不一致。

require_command go
require_command python3

# 构建前就要求干净工作副本：此前只有 sign-release.sh 调它，于是产物可以从脏树产出，
# 事后签名才被拒——而那时产物已经存在并可能已经发出去。0.1.0 正是从这个洞里出去的。
require_clean_worktree

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
overlay_commit="$(git -C "$SING_BOX_PLUS_ROOT" rev-parse HEAD)"
[[ -n "$overlay_commit" ]] || die "无法取得叠加层 commit"

# 工具链对照 upstream.lock：go env GOVERSION 此前只被记进 manifest，从不与任何东西比对，
# 于是「从未验证过的工具链构建并发布」是可能的。
go_verified="$(lock_value go_verified || true)"
if [[ -n "$go_verified" && "$go_version" != "go$go_verified" ]]; then
  printf '注意：当前 Go 为 %s，upstream.lock 记录的已验证版本是 go%s\n' "$go_version" "$go_verified" >&2
fi

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
  local work="$build_root/src"
  mkdir -p "$work" "$build_root/tmp"
  # 真正换工作目录，而不是只换 GOCACHE。此前两次都 cd 到同一个源码根，
  # 于是 cmp 只证明了「没复用构建缓存」，证明不了路径与环境没泄漏进产物——
  # 而那正是脚本开头宣称的用意。顺带：只导出已提交内容，与上面的干净树要求呼应。
  git -C "$SING_BOX_PLUS_ROOT" archive HEAD | tar -x -C "$work"
  ( cd "$work" && \
    CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
    GOCACHE="$build_root/gocache" \
    TMPDIR="$build_root/tmp" \
    go build -trimpath -buildvcs=false \
      -tags "$tags" \
      -ldflags "-s -w -buildid= $upstream_ldflags \
        -X github.com/sagernet/sing-box/constant.Version=$upstream_tag \
        -X main.Version=$version \
        -X main.UpstreamCommit=$upstream_commit \
        -X main.OverlayCommit=$overlay_commit" \
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

# 实测而不是从 lock 抄：manifest 里的字段应当是量出来的，不是断言出来的。
measured_tree="$(python3 "$SING_BOX_PLUS_ROOT/scripts/release-artifact.py" source-tree-sha256 \
  --source-root "$source_dir")"
[[ "$measured_tree" == "$prepared_tree" ]] || \
  die "准备出的源码树哈希与 upstream.lock 不一致：实测 $measured_tree，锁定 $prepared_tree"

python3 "$SING_BOX_PLUS_ROOT/scripts/release-artifact.py" manifest \
  --version "$version" \
  --upstream-tag "$upstream_tag" \
  --upstream-commit "$upstream_commit" \
  --overlay-commit "$overlay_commit" \
  --prepared-tree-sha256 "$measured_tree" \
  --build-tags "$tags" \
  --go-version "$go_version" \
  "${target_args[@]}" \
  "${artifact_args[@]}" \
  --output "$output_dir/manifest.json"

( cd "$output_dir" && shasum -a 256 ./* > SHA256SUMS 2>/dev/null || sha256sum ./* > SHA256SUMS )
printf '发布产物已生成：%s\n' "$output_dir"
