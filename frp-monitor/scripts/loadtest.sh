#!/usr/bin/env bash
set -euo pipefail

source "$(dirname "$0")/lib.sh"

# 容量测试档位（README §9：100/500 节点档位是容量测试，不是性能承诺）。
# 用法：scripts/loadtest.sh [节点数=100]

nodes="${1:-100}"
[[ "$nodes" =~ ^[0-9]+$ && "$nodes" -ge 1 ]] || die "节点数必须为正整数：$nodes"

require_command go
require_command python3

cache="$(cache_dir)"
src="$cache/src-$(lock_value commit)"
[[ -f "$src/.frp-monitor-prepared" ]] || die "缓存源码树不存在，先运行 scripts/build.sh"
"$FRP_MONITOR_ROOT/scripts/assemble.sh" "$src" > /dev/null

temp_dir="$(mktemp -d "${TMPDIR:-/tmp}/frp-monitor.XXXXXX")"
server_pid=""
loadgen_pid=""
cleanup() {
  for pid in $server_pid $loadgen_pid; do
    [[ -n "$pid" ]] && { kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; }
  done
  safe_remove_temp_dir "$temp_dir"
}
trap cleanup EXIT

# 专用虚构凭据：token=loadtest-token-<i>，摘要进凭据文件。
python3 - "$temp_dir/credentials.json" "$nodes" <<'PY'
import hashlib, json, sys
path, n = sys.argv[1], int(sys.argv[2])
nodes = [{"id": f"loadgen-{i}", "token_sha256": hashlib.sha256(f"loadtest-token-{i}".encode()).hexdigest()}
         for i in range(n)]
json.dump({"nodes": nodes}, open(path, "w"))
PY

cat > "$temp_dir/frps-load.toml" <<EOF
bindPort = 17600

[monitor]
enable = true
addr = "127.0.0.1:17610"
credentialsFile = "$temp_dir/credentials.json"
dataDir = "$temp_dir/data"
EOF

tags="$(frpmonitor_build_tags)"
cd "$src"
CGO_ENABLED=0 go build -trimpath -tags "$tags" -o "$temp_dir/frp-monitor-server" ./cmd/frps
CGO_ENABLED=0 go build -trimpath -tags "$tags" -o "$temp_dir/loadgen" ./extension/frpmonitor/tests/loadgen

"$temp_dir/frp-monitor-server" -c "$temp_dir/frps-load.toml" > "$temp_dir/frps.log" 2>&1 &
server_pid=$!

# 等监听就绪再启动负载，避免连接被拒被计为失败。
ready=0
for _ in $(seq 1 50); do
  if curl -fsS -o /dev/null --max-time 2 http://127.0.0.1:17610/api/public/v1/overview 2>/dev/null; then
    ready=1
    break
  fi
  sleep 0.2
done
[[ "$ready" == 1 ]] || die "monitor 监听未就绪"

"$temp_dir/loadgen" -server ws://127.0.0.1:17610/agent/v1/ws -nodes "$nodes" -hold 15s \
  > "$temp_dir/loadgen.log" 2>&1 &
loadgen_pid=$!

# 在 loadgen 保持窗口内验证全部节点上线
deadline=$((SECONDS + 90))
online=0
while (( SECONDS < deadline )); do
  online="$(curl -fsS --max-time 5 http://127.0.0.1:17610/api/public/v1/overview 2>/dev/null \
    | python3 -c 'import json,sys; print(json.load(sys.stdin)["nodes_online"])' 2>/dev/null || echo 0)"
  [[ "$online" == "$nodes" ]] && break
  sleep 2
done
[[ "$online" == "$nodes" ]] || { tail -c 1500 "$temp_dir/frps.log" >&2; die "上线节点不足：$online/$nodes"; }

rss_before="$(ps -o rss= -p "$server_pid" | tr -d ' ')"
sleep 10
online="$(curl -fsS --max-time 5 http://127.0.0.1:17610/api/public/v1/overview \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["nodes_online"])')"
rss_after="$(ps -o rss= -p "$server_pid" | tr -d ' ')"
[[ "$online" == "$nodes" ]] || die "保持窗口后在线数掉落：$online/$nodes"

wait "$loadgen_pid" || { cat "$temp_dir/loadgen.log" >&2; die "loadgen 报告失败"; }
cat "$temp_dir/loadgen.log"

printf '容量测试通过：%s 节点在线并保持；monitor RSS %s KB → %s KB\n' "$nodes" "$rss_before" "$rss_after"
