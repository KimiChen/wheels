#!/usr/bin/env bash
set -euo pipefail

source "$(dirname "$0")/lib.sh"

# 本地构建。可复现发布走 scripts/build-linux-release.sh。

require_command go

output="${1:-$SING_BOX_PLUS_ROOT/bin/sing-box-plus}"
mkdir -p "$(dirname "$output")"

upstream_tag="$(lock_value tag)"
upstream_commit="$(lock_value commit)"
version="${SING_BOX_PLUS_VERSION:-dev}"
tags="$(production_build_tags)"

# release/LDFLAGS 的内容在 1.14 已改写，必须读取准备好的源码树而不是硬编码；
# 本地构建没有源码树时退化为空，并明确提示（README §9.1）。
extra_ldflags="${SING_BOX_PLUS_UPSTREAM_LDFLAGS:-}"
if [[ -z "$extra_ldflags" ]]; then
  printf '提示：未提供上游 release/LDFLAGS，本次为本地开发构建，不可作为发布产物。\n' >&2
fi

cd "$SING_BOX_PLUS_ROOT"
# shellcheck disable=SC2086
go build -trimpath -buildvcs=false \
  -tags "$tags" \
  -ldflags "-s -w -buildid= $extra_ldflags \
    -X github.com/sagernet/sing-box/constant.Version=$upstream_tag \
    -X main.Version=$version \
    -X main.UpstreamCommit=$upstream_commit" \
  -o "$output" ./cmd/sing-box-plus

printf '已构建：%s\n' "$output"
"$output" version
