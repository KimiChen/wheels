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
  patches/series patches/0001-version-suffix.patch \
  patches/0002-monitor-config-types.patch \
  patches/0003-client-monitor-hook.patch patches/0004-server-monitor-hook.patch \
  tests/fixtures/README.md
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
  [[ -f "$src/$marker" ]] && [[ "$(cat "$src/$marker")" == "$(expected_tree_marker)" ]] || \
    die "缓存源码树与锁定状态不符，请删除后重试：$src"
else
  mkdir -p "$cache"
  scripts/prepare-source.sh "$src"
fi
scripts/assemble.sh "$src"

temp_dir="$(mktemp -d "${TMPDIR:-/tmp}/frp-monitor.XXXXXX")"
agent_pid=""
server_pid=""
cleanup() {
  for pid in $agent_pid $server_pid; do
    [[ -n "$pid" ]] && { kill "$pid" 2>/dev/null; wait "$pid" 2>/dev/null; } || true
  done
  safe_remove_temp_dir "$temp_dir"
}
trap cleanup EXIT

version_ldflags="-X github.com/fatedier/frp/pkg/util/version.version=${upstream_tag#v}"
tags="$(frp_build_tags)"
ext_tags="$(frpmonitor_build_tags)"

cd "$src"

# 3. 原生基线：默认构建参数（不启用扩展接线）构建 frpc/frps。
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

# 4. 上游受影响包单测：覆盖未来补丁已触及/将触及的 client/server/config/metrics。
CGO_ENABLED=0 go test -tags "$tags" -count=1 \
  ./pkg/config/... ./pkg/metrics/... ./client/... ./server/...

# 5. 扩展包：编译、vet、契约与组件单测（含 frpmonitor 接线标签）。
CGO_ENABLED=0 go vet -tags "$ext_tags" ./extension/... ./client/... ./server/...
CGO_ENABLED=0 go test -tags "$ext_tags" -count=1 ./extension/...

# 6. 扩展接线构建与端到端冒烟。
CGO_ENABLED=0 go build -trimpath -buildvcs=false \
  -tags "$ext_tags" -ldflags "-s -w -buildid= $version_ldflags" \
  -o "$temp_dir/frp-monitor-agent" ./cmd/frpc
CGO_ENABLED=0 go build -trimpath -buildvcs=false \
  -tags "$ext_tags" -ldflags "-s -w -buildid= $version_ldflags" \
  -o "$temp_dir/frp-monitor-server" ./cmd/frps
"$temp_dir/frp-monitor-server" --version | grep -q "frp-monitor" || \
  die "扩展版本输出缺少 frp-monitor 后缀"

# 端到端：真实 frps+frpc 回环对接，监控凭据/上报/公开裁剪/管理认证/隧道转发全链路。
# 端口固定为本机回环高端口；冒烟值均为专用虚构值。
smoke_token="p1-smoke-token-fixture"
smoke_admin="p1-admin-fixture"
smoke_token_hash="$(printf '%s' "$smoke_token" | shasum -a 256 | awk '{print $1}')"
smoke_admin_hash="$(printf '%s' "$smoke_admin" | shasum -a 256 | awk '{print $1}')"

cat > "$temp_dir/credentials.json" <<EOF
{"nodes":[{"id":"smoke-node-01","token_sha256":"$smoke_token_hash","comment":"e2e 冒烟"}]}
EOF

cat > "$temp_dir/frps-smoke.toml" <<EOF
bindPort = 17000

[monitor]
enable = true
addr = "127.0.0.1:17400"
credentialsFile = "$temp_dir/credentials.json"
adminPasswordHash = "$smoke_admin_hash"
EOF

cat > "$temp_dir/frpc-smoke.toml" <<EOF
serverAddr = "127.0.0.1"
serverPort = 17000
clientID = "smoke-node-01"

[monitor]
enable = true
serverURL = "ws://127.0.0.1:17400/agent/v1/ws"
token = "$smoke_token"
reportIntervalSeconds = 1

[[proxies]]
name = "monitor-api-via-frp"
type = "tcp"
localIP = "127.0.0.1"
localPort = 17400
remotePort = 17500
EOF

"$temp_dir/frp-monitor-server" -c "$temp_dir/frps-smoke.toml" > "$temp_dir/frps.log" 2>&1 &
server_pid=$!
"$temp_dir/frp-monitor-agent" -c "$temp_dir/frpc-smoke.toml" > "$temp_dir/frpc.log" 2>&1 &
agent_pid=$!

python3 - "$temp_dir" <<'PYEOF'
import json, sys, time, platform, urllib.request, urllib.error, http.cookiejar

temp_dir = sys.argv[1]
BASE = "http://127.0.0.1:17400"
# 采集器首版仅支持 Linux：其他平台跑通协议链路，但指标组合法降级为 unknown（null）。
IS_LINUX = platform.system() == "Linux"

def get(url, opener=None):
    op = opener or urllib.request.build_opener()
    return op.open(url, timeout=5)

# 1. 等待节点上线；Linux 上进一步要求真实 CPU 样本（首个样本 cpu 为 null）
deadline = time.time() + 30
node = None
while time.time() < deadline:
    try:
        body = get(BASE + "/api/public/v1/nodes").read().decode()
        data = json.loads(body)
        for n in data.get("nodes", []):
            ok = (n.get("id") == "smoke-node-01" and n.get("online")
                  and n.get("frp_control_connected") is True)
            if ok and (not IS_LINUX or n.get("cpu") is not None):
                node = n
                break
        if node:
            break
    except Exception:
        pass
    time.sleep(1)
if not node:
    print("E2E 失败：节点未上线或指标长期无效", file=sys.stderr)
    print(open(temp_dir + "/frps.log").read()[-2000:], file=sys.stderr)
    print(open(temp_dir + "/frpc.log").read()[-2000:], file=sys.stderr)
    sys.exit(1)

assert node["frp_control_connected"] is True, "FRP 控制连接应在线"
assert node["metrics_stale"] is False, "指标应新鲜"
if IS_LINUX:
    assert node["mem_total"] is not None and node["net_rx"] is not None, "Linux 上内存/网速应有效"
else:
    # 非 Linux：指标组降级为未知（null），链路与会话语义仍需正确
    assert node["cpu"] is None, "非 Linux 平台 CPU 应为 unknown（null）"
assert any(p["name"] == "monitor-api-via-frp" for p in node["proxies"]), "应列出隧道"
# 公开 DTO 裁剪：不得出现私有字段
for banned in ("hostname", "boot_id", "ipv4", "ipv6", "local_addr", "iface", "kernel"):
    assert f'"{banned}"' not in body, f"公开 DTO 泄露字段：{banned}"

# 2. 公开总览
ov = json.loads(get(BASE + "/api/public/v1/overview").read().decode())
assert ov["nodes_online"] >= 1, "总览在线节点计数错误"

# 3. 隧道转发仍然工作（经 FRP 隧道访问 monitor API 自身）
tun = json.loads(get("http://127.0.0.1:17500/api/public/v1/overview").read().decode())
assert tun["nodes_online"] >= 1, "隧道转发不通"

# 4. 管理端认证
try:
    get(BASE + "/api/admin/v1/nodes")
    print("E2E 失败：管理 API 未认证可访问", file=sys.stderr)
    sys.exit(1)
except urllib.error.HTTPError as e:
    assert e.code == 401

cj = http.cookiejar.CookieJar()
opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(cj))
req = urllib.request.Request(BASE + "/api/admin/v1/login",
                             data=json.dumps({"password": "p1-admin-fixture"}).encode(),
                             headers={"Content-Type": "application/json"})
assert json.loads(opener.open(req, timeout=5).read().decode())["ok"] is True
# FRP 注册表对账按 2s 周期快照，轮询等待其刷新
deadline2 = time.time() + 15
admin_body = ""
while time.time() < deadline2:
    admin_body = opener.open(BASE + "/api/admin/v1/nodes", timeout=5).read().decode()
    admin = json.loads(admin_body)
    if admin["nodes"] and admin["nodes"][0]["frp_clients"]:
        break
    time.sleep(1)
assert '"hostname"' in admin_body and '"boot_id"' in admin_body, "管理 DTO 应含完整字段"
assert admin["nodes"][0]["frp_clients"], "管理 DTO 应含 FRP 注册表对账结果"

print("E2E 冒烟通过：节点上报、公开裁剪、管理认证、FRP 隧道转发均正常")
PYEOF

# 7. 发布目标架构交叉编译冒烟（frp 为纯 Go，无需 C 工具链）。
for arch in amd64 arm64; do
  for cmd in frpc frps; do
    CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -buildvcs=false \
      -tags "$ext_tags" \
      -ldflags "-s -w -buildid= $version_ldflags" \
      -o "$temp_dir/linux-$arch/$cmd" "./cmd/$cmd"
  done
done

printf '验证通过：基线/扩展构建与测试、端到端冒烟、linux amd64/arm64 交叉编译全部成功。\n'
