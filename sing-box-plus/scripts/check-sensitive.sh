#!/usr/bin/env bash
set -euo pipefail

source "$(dirname "$0")/lib.sh"

# 本仓库是公开 git：提交前扫描明显的敏感物。
# 这不是保证，只是把最容易犯的错挡住——真正的凭据管理见 docs/OPERATIONS.md。
#
# 匹配与允许清单的逻辑在 scripts/sensitive-scan.py；地址清单在
# scripts/sensitive-allowlist.txt，每条都必须带理由，且 [public] 段的条目
# 一旦在仓库中不再出现即判为陈旧并失败——清单只增不减就是它腐烂的方式。

cd "$SING_BOX_PLUS_ROOT"
exec python3 "$SING_BOX_PLUS_ROOT/scripts/sensitive-scan.py"
