#!/usr/bin/env bash
set -euo pipefail

source "$(dirname "$0")/lib.sh"

# 上游回归会创建 dummy 接口并添加数千条路由，只允许在新建的网络命名空间运行。
# 创建 namespace 失败即终止，绝不回退到宿主或生产服务所在的 namespace。
[[ $# == 0 ]] || die "用法：bash scripts/test-network-monitor.sh"
[[ "$(uname -s)" == Linux ]] || die "本专项必须在 Linux 上运行；macOS 可交叉编译测试二进制后交给隔离 Linux 测试主机"
[[ "$EUID" == 0 ]] || die "本专项需要 root，以及创建网络命名空间和管理其中网络的能力"
for command_name in go unshare timeout; do
  require_command "$command_name"
done

cd "$SING_BOX_PLUS_ROOT"
tags="$(production_build_tags)"
temp_dir="$(mktemp -d "${TMPDIR:-/tmp}/sing-box-plus.XXXXXX")"
trap 'safe_remove_temp_dir "$temp_dir"' EXIT
test_binary="$temp_dir/network-monitor.test"
test_name=TestNetworkUpdateMonitorReceiveOverrun

# 明确使用本机 Linux 架构；构建时不执行依赖包中的测试。
CGO_ENABLED=0 GOOS=linux GOARCH="$(go env GOHOSTARCH)" \
  go test -c -tags "$tags" -o "$test_binary" github.com/sagernet/sing-tun
listed="$("$test_binary" -test.list "^${test_name}\$")"
grep -Fxq "$test_name" <<<"$listed" || die "上游测试二进制缺少 $test_name，拒绝把零用例当作成功"

# 外层 120 秒同时约束 unshare 与测试进程，Go 的 110 秒超时保留栈信息。
timeout 120s unshare --net "$test_binary" \
  -test.run "^${test_name}\$" -test.count=1 -test.timeout=110s -test.v
printf '网络监听器专项回归已在独立网络命名空间通过。\n'
