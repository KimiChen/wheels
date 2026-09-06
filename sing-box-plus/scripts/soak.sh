#!/usr/bin/env bash
set -euo pipefail

source "$(dirname "$0")/lib.sh"

# 里程碑 5 的长跑采集器：按固定间隔取快照、交给参考 collector 差分入账，并累计四项判据。
#
# 本脚本不代替长跑，只保证长跑的结论是可判定的：
# 负增量 = 0、未知 runtime = 0、sequence 重复 = 0、unhealthy 快照全部被拒。

require_command python3

usage() {
  printf '用法：%s <快照 socket 路径> <账本目录> [采集间隔秒，默认 60]\n' "$(basename "$0")" >&2
}

[[ $# -ge 2 && $# -le 3 ]] || { usage; exit 2; }

socket_path="$1"
ledger_dir="$(absolute_path "$2")"
interval="${3:-60}"

[[ "$interval" =~ ^[0-9]+$ && "$interval" -ge 1 ]] || die "采集间隔必须是正整数秒"
mkdir -p "$ledger_dir"

printf '长跑采集开始：socket=%s 账本=%s 间隔=%ss\n' "$socket_path" "$ledger_dir" "$interval"
printf '按 Ctrl-C 结束；结束后用 tests/reference_collector.py --report 查看四项判据。\n'

while true; do
  if ! python3 "$SING_BOX_PLUS_ROOT/tests/reference_collector.py" \
    --socket "$socket_path" --ledger "$ledger_dir" --first-snapshot baseline; then
    printf '采集失败（%s），继续下一轮；失败本身已记入账本。\n' "$(date -u +%FT%TZ)" >&2
  fi
  sleep "$interval"
done
