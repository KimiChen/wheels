#!/usr/bin/env bash
set -euo pipefail

source "$(dirname "$0")/lib.sh"

# 本地构建：固定源码 + 映射扩展后的全量编译。
# P0 阶段尚无生命周期补丁，frpc/frps 产物为原生基线；扩展包随 `go build ./...`
# 一并编译验证。verify.sh 负责完整的基线与回归检查。

require_command go

upstream_tag="$(lock_value tag)"
upstream_commit="$(lock_value commit)"

cache="$(cache_dir)"
src="$cache/src-$upstream_commit"
marker=".frp-monitor-prepared"

if [[ -d "$src" ]]; then
  [[ -f "$src/$marker" ]] || die "缓存源码树缺少准备标记，请删除后重试：$src"
  prepared="$(cat "$src/$marker")"
  [[ "$prepared" == "$upstream_commit" ]] || \
    die "缓存源码树提交为 $prepared，与锁定 $upstream_commit 不符，请删除后重试：$src"
else
  mkdir -p "$cache"
  "$FRP_MONITOR_ROOT/scripts/prepare-source.sh" "$src"
fi

"$FRP_MONITOR_ROOT/scripts/assemble.sh" "$src"

out="$(output_dir)"
mkdir -p "$out"

# 与上游 Makefile 一致的版本注入（pkg/util/version.version，不带 v 前缀）。
version_ldflags="-X github.com/fatedier/frp/pkg/util/version.version=${upstream_tag#v}"

cd "$src"
tags="$(frp_build_tags)"
for cmd in frpc frps; do
  CGO_ENABLED=0 go build -trimpath -buildvcs=false \
    -tags "$tags" \
    -ldflags "-s -w -buildid= $version_ldflags" \
    -o "$out/$cmd" "./cmd/$cmd"
done

# 扩展包当前不被任何 cmd 引用，必须显式全量编译，防止「能映射、不能编译」溜走。
CGO_ENABLED=0 go build -tags "$tags" ./...

printf '已构建基线二进制：%s/frpc %s/frps\n' "$out" "$out"
"$out/frps" --version
