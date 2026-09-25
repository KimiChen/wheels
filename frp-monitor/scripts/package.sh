#!/usr/bin/env bash
set -euo pipefail

source "$(dirname "$0")/lib.sh"

# 发布打包：Linux amd64/arm64 的 frp-monitor-agent / frp-monitor-server tar.gz。
# 先跑完整 verify（基线/扩展/端到端/交叉编译），再从同一棵树取发布二进制。

require_command go
require_command tar
require_command shasum

"$FRP_MONITOR_ROOT/scripts/verify.sh"

version="$(awk -F\" '/const Version/ {print $2}' "$FRP_MONITOR_ROOT/shared/version/version.go")"
[[ -n "$version" ]] || die "无法从 shared/version/version.go 读取版本"

src="$(cache_dir)/src-$(lock_value commit)"
out="$(output_dir)"
temp_dir="$(mktemp -d "${TMPDIR:-/tmp}/frp-monitor.XXXXXX")"
trap 'safe_remove_temp_dir "$temp_dir"' EXIT

tags="$(frpmonitor_build_tags)"
upstream_tag="$(lock_value tag)"
version_ldflags="-X github.com/fatedier/frp/pkg/util/version.version=${upstream_tag#v}"

cd "$src"
sums="$out/sha256sums.txt"
: > "$sums"
for arch in amd64 arm64; do
  for pair in "agent:./cmd/frpc" "server:./cmd/frps"; do
    name="frp-monitor-${pair%%:*}"
    pkg_dir="$temp_dir/${name}_${version}_linux_${arch}"
    mkdir -p "$pkg_dir/packaging"
    CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -buildvcs=false \
      -tags "$tags" -ldflags "-s -w -buildid= $version_ldflags" \
      -o "$pkg_dir/$name" "${pair#*:}"
    cp "$FRP_MONITOR_ROOT/LICENSE" "$FRP_MONITOR_ROOT/THIRD_PARTY_NOTICES.md" \
      "$FRP_MONITOR_ROOT/README.md" "$pkg_dir/"
    cp "$FRP_MONITOR_ROOT"/packaging/*.service "$FRP_MONITOR_ROOT"/packaging/*.example \
      "$FRP_MONITOR_ROOT/packaging/install.sh" "$pkg_dir/packaging/"
    tarball="$out/${name}_${version}_linux_${arch}.tar.gz"
    mkdir -p "$out"
    tar -C "$temp_dir" -czf "$tarball" "$(basename "$pkg_dir")"
    (cd "$out" && shasum -a 256 "$(basename "$tarball")" >> "$sums")
    printf '已打包：%s\n' "$tarball"
  done
done
printf '校验和：%s\n' "$sums"
