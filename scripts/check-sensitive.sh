#!/usr/bin/env bash
set -euo pipefail

# 全仓敏感物扫描。
#
# 与 sing-box-plus/scripts/check-sensitive.sh 的分工：那份只管自己的子树，
# 是该项目 verify.sh 的一环，必须能脱离本仓单独成立；这份管整个仓库。
#
# 存在的理由：2026-09-19 推送前的一次审计发现，104 个待推文件里有 71 个从未被
# 任何检查看过——扫描范围一直钉在 sing-box-plus 子树上，而泄露发生在隔壁目录。
#
# 扫描器本身在 sing-box-plus/scripts/sensitive-scan.py：那是它的家，
# 复制一份到这里只会让两份慢慢长得不一样。

root="$(cd "$(dirname "$0")/.." && pwd)"
exec python3 "$root/sing-box-plus/scripts/sensitive-scan.py" \
    --root "$root" --allowlist "$root/scripts/sensitive-allowlist.txt"
