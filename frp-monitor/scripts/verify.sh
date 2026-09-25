#!/usr/bin/env bash
set -euo pipefail

source "$(dirname "$0")/lib.sh"

require_command git
require_command go
require_command python3
require_command tar

cd "$FRP_MONITOR_ROOT"

# 0. 交付物存在性——缺一件就说明 README 的规划与实际已经对不上。
for required in \
  upstream.lock .env.example README.md THIRD_PARTY_NOTICES.md \
  agent/README.md monitor/README.md web/README.md \
  shared/protocol/protocol.go shared/metrics/metrics.go \
  patches/series tests/fixtures/README.md
do
  [[ -f "$required" ]] || die "缺少交付物：$required"
done

# 1. 静态检查。
for script_path in scripts/*.sh; do
  bash -n "$script_path"
done

gofmt_output="$(gofmt -l agent monitor shared 2>/dev/null)"
[[ -z "$gofmt_output" ]] || die "gofmt 未通过：$gofmt_output"

# 契约 golden 与 fixture 中的 JSON 必须是合法 JSON。
while IFS= read -r json_path; do
  python3 -m json.tool "$json_path" > /dev/null || die "非法 JSON：$json_path"
done < <(find shared tests -name '*.json' -type f)

# 2. 固定源码树（带缓存）+ 扩展映射。
upstream_tag="$(lock_value tag)"
upstream_commit="$(lock_value commit)"
cache="$(cache_dir)"
src="$cache/src-$upstream_commit"
marker=".frp-monitor-prepared"

if [[ -d "$src" ]]; then
  [[ -f "$src/$marker" ]] && [[ "$(cat "$src/$marker")" == "$upstream_commit" ]] || \
    die "缓存源码树与锁定提交不符，请删除后重试：$src"
else
  mkdir -p "$cache"
  scripts/prepare-source.sh "$src"
fi
scripts/assemble.sh "$src"

temp_dir="$(mktemp -d "${TMPDIR:-/tmp}/frp-monitor.XXXXXX")"
trap 'safe_remove_temp_dir "$temp_dir"' EXIT

version_ldflags="-X github.com/fatedier/frp/pkg/util/version.version=${upstream_tag#v}"
tags="$(frp_build_tags)"

cd "$src"

# 3. 原生基线：默认构建参数（不启用任何扩展接线）构建 frpc/frps。
for cmd in frpc frps; do
  CGO_ENABLED=0 go build -trimpath -buildvcs=false \
    -tags "$tags" \
    -ldflags "-s -w -buildid= $version_ldflags" \
    -o "$temp_dir/$cmd" "./cmd/$cmd"
done

"$temp_dir/frps" --version | grep -q "${upstream_tag#v}" || die "frps 版本输出与锁定 tag 不符"

# 配置校验冒烟：示例配置不含任何真实地址与凭据。
cat > "$temp_dir/frps.toml" <<'EOF'
bindPort = 7000
EOF
"$temp_dir/frps" verify -c "$temp_dir/frps.toml" > /dev/null

cat > "$temp_dir/frpc.toml" <<'EOF'
serverAddr = "192.0.2.1"
serverPort = 7000

[[proxies]]
name = "smoke-tcp"
type = "tcp"
localIP = "127.0.0.1"
localPort = 22
remotePort = 6000
EOF
"$temp_dir/frpc" verify -c "$temp_dir/frpc.toml" > /dev/null

# 4. 上游受影响包单测：覆盖未来补丁将触及的 client/server/config/metrics。
CGO_ENABLED=0 go test -tags "$tags" -count=1 \
  ./pkg/config/... ./pkg/metrics/... ./client/... ./server/...

# 5. 扩展包：编译、vet、契约单测。
CGO_ENABLED=0 go vet -tags "$tags" ./extension/...
CGO_ENABLED=0 go test -tags "$tags" -count=1 ./extension/...

# 6. 发布目标架构交叉编译冒烟（frp 为纯 Go，无需 C 工具链）。
for arch in amd64 arm64; do
  for cmd in frpc frps; do
    CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -buildvcs=false \
      -tags "$tags" \
      -ldflags "-s -w -buildid= $version_ldflags" \
      -o "$temp_dir/linux-$arch/$cmd" "./cmd/$cmd"
  done
done

printf '验证通过：基线构建/测试、扩展契约测试、linux amd64/arm64 交叉编译全部成功。\n'
