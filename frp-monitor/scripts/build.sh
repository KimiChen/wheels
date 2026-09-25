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
  [[ "$prepared" == "$(expected_tree_marker)" ]] || \
    die "缓存源码树与锁定状态不符（$prepared），请删除后重试：$src"
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
ext_tags="$(frpmonitor_build_tags)"
for cmd in frpc frps; do
  CGO_ENABLED=0 go build -trimpath -buildvcs=false \
    -tags "$tags" \
    -ldflags "-s -w -buildid= $version_ldflags" \
    -o "$out/$cmd" "./cmd/$cmd"
done

# 扩展接线产物：frp-monitor-agent/server（frpmonitor 标签）。
CGO_ENABLED=0 go build -trimpath -buildvcs=false \
  -tags "$ext_tags" \
  -ldflags "-s -w -buildid= $version_ldflags" \
  -o "$out/frp-monitor-agent" ./cmd/frpc
CGO_ENABLED=0 go build -trimpath -buildvcs=false \
  -tags "$ext_tags" \
  -ldflags "-s -w -buildid= $version_ldflags" \
  -o "$out/frp-monitor-server" ./cmd/frps

# 扩展包当前不被任何 cmd 引用，必须显式全量编译，防止「能映射、不能编译」溜走。
CGO_ENABLED=0 go build -tags "$ext_tags" ./...

printf '已构建：%s/{frpc,frps}（基线） %s/{frp-monitor-agent,frp-monitor-server}（扩展）\n' "$out" "$out"
"$out/frp-monitor-server" --version
