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
  patches/0005-deps-modernc-sqlite.patch \
  tests/fixtures/README.md \
  packaging/frp-monitor-server.service packaging/frp-monitor-agent.service \
  packaging/frps.toml.example packaging/frpc.toml.example \
  packaging/credentials.json.example packaging/install.sh
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
pids=()
cleanup() {
  for pid in ${pids[@]+"${pids[@]}"}; do
    kill "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
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

# 4. 上游受影响包单测：覆盖补丁已触及/将触及的 client/server/config/metrics。
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

# 端到端：真实进程回环对接。端口固定为本机回环高端口；冒烟值均为专用虚构值。
smoke_token="p3-smoke-token-fixture"
smoke_token2="p3-smoke-token-fixture-02"
smoke_admin="p3-admin-fixture"

cat > "$temp_dir/credentials.json" <<EOF
{"nodes":[
  {"id":"smoke-node-01","token_sha256":"$(printf '%s' "$smoke_token" | shasum -a 256 | awk '{print $1}')","comment":"e2e 主节点"},
  {"id":"smoke-node-02","token_sha256":"$(printf '%s' "$smoke_token2" | shasum -a 256 | awk '{print $1}')","comment":"e2e 混排节点"}
]}
EOF

cat > "$temp_dir/frps-smoke.toml" <<EOF
bindPort = 17000

[monitor]
enable = true
addr = "127.0.0.1:17400"
credentialsFile = "$temp_dir/credentials.json"
adminPasswordHash = "$(printf '%s' "$smoke_admin" | shasum -a 256 | awk '{print $1}')"
dataDir = "$temp_dir/data"
historyRetentionDays = 7
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

[[proxies]]
name = "smoke-udp"
type = "udp"
localIP = "127.0.0.1"
localPort = 17400
remotePort = 17501
EOF

"$temp_dir/frp-monitor-server" -c "$temp_dir/frps-smoke.toml" > "$temp_dir/frps.log" 2>&1 &
pids+=($!)
server_pid=$!
"$temp_dir/frp-monitor-agent" -c "$temp_dir/frpc-smoke.toml" > "$temp_dir/frpc.log" 2>&1 &
pids+=($!)

dump_logs() {
  for f in frps.log frpc.log frps-baseline.log frpc-baseline.log frpc-mixed.log; do
    [[ -f "$temp_dir/$f" ]] || continue
    echo "---- $f ----" >&2; tail -c 1500 "$temp_dir/$f" >&2
  done
}

python3 - "$temp_dir" <<'PYEOF' || { dump_logs; exit 1; }
import json, sys, time, platform, urllib.request, urllib.error, http.cookiejar

temp_dir = sys.argv[1]
BASE = "http://127.0.0.1:17400"
# 采集器首版仅支持 Linux：其他平台跑通协议链路，但指标组合法降级为 unknown（null）。
IS_LINUX = platform.system() == "Linux"

def get(url, opener=None):
    op = opener or urllib.request.build_opener()
    return op.open(url, timeout=5)

# 1. 等待节点上线 + FRP 控制连接；Linux 上进一步要求真实 CPU 样本
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
    sys.exit(1)

assert node["metrics_stale"] is False, "指标应新鲜"
if IS_LINUX:
    assert node["mem_total"] is not None and node["net_rx"] is not None, "Linux 上内存/网速应有效"
else:
    assert node["cpu"] is None, "非 Linux 平台 CPU 应为 unknown（null）"
assert any(p["name"] == "monitor-api-via-frp" for p in node["proxies"]), "应列出隧道"
# 公开 DTO 裁剪：不得出现私有字段
for banned in ("hostname", "boot_id", "ipv4", "ipv6", "local_addr", "iface", "kernel", "target"):
    assert f'"{banned}"' not in body, f"公开 DTO 泄露字段：{banned}"

# 2. 公开总览（含隧道计数；隧道统计由 2s 轮询喂入，需轮询等待）
deadline_ov = time.time() + 20
ov = {}
while time.time() < deadline_ov:
    ov = json.loads(get(BASE + "/api/public/v1/overview").read().decode())
    if ov.get("tunnels_online", 0) >= 2:
        break
    time.sleep(1)
assert ov["nodes_online"] >= 1, "总览在线节点计数错误"
assert ov.get("tunnels_online", 0) >= 2, "总览应有隧道在线计数"

# 3. 隧道转发仍然工作（经 FRP 隧道访问 monitor API 自身）
tun = json.loads(get("http://127.0.0.1:17500/api/public/v1/overview").read().decode())
assert tun["nodes_online"] >= 1, "隧道转发不通"

# 4. 隧道列表（P3）：公开裁剪 + 类型覆盖
tunnels_body = get(BASE + "/api/public/v1/tunnels").read().decode()
tunnels = {t["name"]: t for t in json.loads(tunnels_body)["tunnels"]}
assert tunnels.get("monitor-api-via-frp", {}).get("online") is True, "TCP 隧道应在线"
assert "smoke-udp" in tunnels, "UDP 隧道应在列表"
assert '"local_addr"' not in tunnels_body, "公开隧道 DTO 不得含 local_addr"
assert tunnels["monitor-api-via-frp"].get("node_id") == "smoke-node-01", "隧道应对账到节点"

# 5. 管理端认证与完整视图
try:
    get(BASE + "/api/admin/v1/nodes")
    print("E2E 失败：管理 API 未认证可访问", file=sys.stderr)
    sys.exit(1)
except urllib.error.HTTPError as e:
    assert e.code == 401

cj = http.cookiejar.CookieJar()
opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(cj))
req = urllib.request.Request(BASE + "/api/admin/v1/login",
                             data=json.dumps({"password": "p3-admin-fixture"}).encode(),
                             headers={"Content-Type": "application/json"})
assert json.loads(opener.open(req, timeout=5).read().decode())["ok"] is True

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
admin_node = admin["nodes"][0]
assert admin_node.get("tunnels"), "管理节点应含隧道列表"
assert any(t.get("local_addr") for t in admin_node["tunnels"]), "管理隧道应含本地目标"

# 6. TCP 探测闭环：下发版本化任务列表 → agent 探测 → 公开/管理视图可见
tasks = {"tasks": [
    {"id": "t-ok", "target": "127.0.0.1:17000", "interval": 5},
    {"id": "t-bad", "target": "127.0.0.1:9", "interval": 5},
]}
req = urllib.request.Request(BASE + "/api/admin/v1/nodes/smoke-node-01/probe-tasks",
                             data=json.dumps(tasks).encode(), method="PUT",
                             headers={"Content-Type": "application/json"})
resp = json.loads(opener.open(req, timeout=5).read().decode())
assert resp.get("ok") is True and resp.get("version", 0) >= 1, f"探测任务下发失败：{resp}"

deadline3 = time.time() + 45
probes = {}
while time.time() < deadline3:
    n = json.loads(get(BASE + "/api/public/v1/nodes/smoke-node-01").read().decode())["node"]
    probes = {p["id"]: p for p in n.get("probes", [])}
    ok_t = probes.get("t-ok", {})
    bad_t = probes.get("t-bad", {})
    if ok_t.get("last_latency_ms") is not None and ok_t.get("samples", 0) >= 1 \
       and (bad_t.get("fail_rate") or 0) > 0:
        break
    time.sleep(2)
assert "target" not in json.dumps(probes), "公开探测 DTO 不得含 target"
assert probes.get("t-ok", {}).get("last_latency_ms") is not None, "t-ok 应有成功延迟"
assert (probes.get("t-bad", {}).get("fail_rate") or 0) > 0, "t-bad 应有失败率"

# 7. 凭据管理（P3）：创建一次性返回 token、冲突 409、吊销后列表移除
cred_req = urllib.request.Request(BASE + "/api/admin/v1/credentials",
                                  data=json.dumps({"id": "smoke-node-99", "comment": "e2e 临时"}).encode(),
                                  headers={"Content-Type": "application/json"})
cred = json.loads(opener.open(cred_req, timeout=5).read().decode())
assert cred.get("token"), "创建凭据应一次性返回 token"
try:
    opener.open(urllib.request.Request(BASE + "/api/admin/v1/credentials",
                                       data=json.dumps({"id": "smoke-node-99"}).encode(),
                                       headers={"Content-Type": "application/json"}), timeout=5)
    print("E2E 失败：重复凭据应 409", file=sys.stderr)
    sys.exit(1)
except urllib.error.HTTPError as e:
    assert e.code == 409
creds = json.loads(opener.open(BASE + "/api/admin/v1/credentials", timeout=5).read().decode())
assert any(c["id"] == "smoke-node-99" for c in creds["credentials"]), "凭据列表应含新建节点"
assert "token" not in json.dumps(creds) and "sha256" not in json.dumps(creds), "凭据列表不得含秘密"
req = urllib.request.Request(BASE + "/api/admin/v1/credentials/smoke-node-99", method="DELETE")
assert json.loads(opener.open(req, timeout=5).read().decode())["ok"] is True

# 8. 备份端点：SQLite 快照可下载
resp = opener.open(BASE + "/api/admin/v1/backup", timeout=10)
blob = resp.read()
assert resp.status == 200 and blob.startswith(b"SQLite format 3"), "备份应为 SQLite 快照"

# 9. 流量字段结构（值断言仅 Linux：macOS 上网卡计数器按契约为 unknown）
node2 = json.loads(get(BASE + "/api/public/v1/nodes/smoke-node-01").read().decode())["node"]
assert "traffic" in node2, "节点 DTO 应含 traffic 字段"
if IS_LINUX:
    assert node2["traffic"] is not None, "Linux 上流量应有数据"
    assert int(node2["traffic"]["total_rx_bytes"]) >= 0

print("E2E 第一段通过：上报/裁剪/认证/隧道/探测/凭据/备份/流量结构正常")
PYEOF

# 6.5 混排兼容矩阵：原版 frpc ↔ 扩展 frps；扩展 frpc ↔ 原版 frps。
cat > "$temp_dir/frpc-baseline.toml" <<'EOF'
serverAddr = "127.0.0.1"
serverPort = 17000
loginFailExit = false

[[proxies]]
name = "baseline-tcp"
type = "tcp"
localIP = "127.0.0.1"
localPort = 17400
remotePort = 17502
EOF
"$temp_dir/frpc" -c "$temp_dir/frpc-baseline.toml" > "$temp_dir/frpc-baseline.log" 2>&1 &
pids+=($!)

cat > "$temp_dir/frps-baseline.toml" <<'EOF'
bindPort = 17020
EOF
"$temp_dir/frps" -c "$temp_dir/frps-baseline.toml" > "$temp_dir/frps-baseline.log" 2>&1 &
pids+=($!)

cat > "$temp_dir/frpc-mixed.toml" <<EOF
serverAddr = "127.0.0.1"
serverPort = 17020
clientID = "smoke-node-02"
loginFailExit = false

[monitor]
enable = true
serverURL = "ws://127.0.0.1:17400/agent/v1/ws"
token = "$smoke_token2"
reportIntervalSeconds = 1

[[proxies]]
name = "mixed-tcp"
type = "tcp"
localIP = "127.0.0.1"
localPort = 17400
remotePort = 17503
EOF
"$temp_dir/frp-monitor-agent" -c "$temp_dir/frpc-mixed.toml" > "$temp_dir/frpc-mixed.log" 2>&1 &
pids+=($!)

python3 - <<'PYEOF' || { dump_logs; exit 1; }
import json, time, urllib.request

def get(url):
    return urllib.request.urlopen(url, timeout=5)

# 原版 frpc 的隧道经扩展 frps 可用
deadline = time.time() + 30
ok = False
while time.time() < deadline:
    try:
        tun = json.loads(get("http://127.0.0.1:17502/api/public/v1/overview").read().decode())
        if tun.get("nodes_online", 0) >= 1:
            ok = True
            break
    except Exception:
        pass
    time.sleep(1)
assert ok, "原版 frpc ↔ 扩展 frps 隧道不通"

# 扩展 frpc 接原版 frps：隧道可用，且监控上报（连扩展 monitor）不受影响
deadline = time.time() + 30
ok = False
while time.time() < deadline:
    try:
        json.loads(get("http://127.0.0.1:17503/api/public/v1/overview").read().decode())
        nodes = json.loads(get("http://127.0.0.1:17400/api/public/v1/nodes").read().decode())["nodes"]
        if any(n["id"] == "smoke-node-02" and n["online"] for n in nodes):
            ok = True
            break
    except Exception:
        pass
    time.sleep(1)
assert ok, "扩展 frpc ↔ 原版 frps 组合下隧道或监控异常"

print("E2E 混排兼容通过：原版/扩展双方向隧道与监控均正常")
PYEOF

# 7. monitor 重启恢复：同 dataDir 重启后节点记录不丢、agent 重连恢复在线。
if ! kill -0 "$server_pid" 2>/dev/null; then
  echo "E2E 失败：monitor 在重启前已退出（疑似崩溃）" >&2
  dump_logs
  exit 1
fi
kill "$server_pid" 2>/dev/null || true
wait "$server_pid" 2>/dev/null || true
"$temp_dir/frp-monitor-server" -c "$temp_dir/frps-smoke.toml" >> "$temp_dir/frps.log" 2>&1 &
pids+=($!)
server_pid=$!

python3 - <<'PYEOF' || { dump_logs; exit 1; }
import json, sys, time, platform, urllib.request

BASE = "http://127.0.0.1:17400"
IS_LINUX = platform.system() == "Linux"

def get(url):
    return urllib.request.urlopen(url, timeout=5)

# 重启后 agent 按退避重连；节点应先以记录形式存在，最终恢复在线
deadline = time.time() + 60
node = None
while time.time() < deadline:
    try:
        data = json.loads(get(BASE + "/api/public/v1/nodes").read().decode())
        for n in data.get("nodes", []):
            if n.get("id") == "smoke-node-01" and n.get("online"):
                node = n
                break
        if node:
            break
    except Exception:
        pass
    time.sleep(1)
assert node, "monitor 重启后节点未恢复在线"

# 历史接口：结构断言（macOS 上数值组全为 null 是契约内行为）
deadline2 = time.time() + 150
hist = None
while time.time() < deadline2:
    h = json.loads(get(BASE + "/api/public/v1/nodes/smoke-node-01/metrics?range=1h").read().decode())
    if h.get("enabled") and h.get("series", {}).get("cpu"):
        hist = h
        break
    time.sleep(5)
assert hist, "历史接口应返回 enabled:true 且含 series"
assert hist["step"] == 60 and len(hist["series"]["cpu"]) > 0, "1h 窗口应为 60s 步长"
if IS_LINUX:
    assert any(v is not None for v in hist["series"]["cpu"]), "Linux 上 1 分钟后应有真实 CPU 样本"

print("E2E 第二段通过：monitor 重启恢复、分钟聚合历史正常")
PYEOF

# 8. 发布目标架构交叉编译冒烟（frp 为纯 Go，无需 C 工具链）。
for arch in amd64 arm64; do
  for cmd in frpc frps; do
    CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -buildvcs=false \
      -tags "$ext_tags" \
      -ldflags "-s -w -buildid= $version_ldflags" \
      -o "$temp_dir/linux-$arch/$cmd" "./cmd/$cmd"
  done
done

# 9. 容量测试档位（默认 100 节点；FRP_MONITOR_LOADTEST=0 可跳过）。
if [[ "$(require_bool_env FRP_MONITOR_LOADTEST 1)" == 1 ]]; then
  "$FRP_MONITOR_ROOT/scripts/loadtest.sh" "${FRP_MONITOR_LOADTEST_NODES:-100}"
fi

printf '验证通过：基线/扩展构建与测试、端到端冒烟（含探测/历史/混排/恢复）、交叉编译与容量档位全部成功。\n'
