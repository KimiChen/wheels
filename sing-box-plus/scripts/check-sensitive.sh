#!/usr/bin/env bash
set -euo pipefail

source "$(dirname "$0")/lib.sh"

# 本仓库是公开 git：提交前扫描明显的敏感物。
# 这不是保证，只是把最容易犯的错挡住——真正的凭据管理见 docs/OPERATIONS.md。

cd "$SING_BOX_PLUS_ROOT"

failures=0
report() {
  printf '敏感信息疑似命中：%s\n' "$*" >&2
  failures=$((failures + 1))
}

scan_files() {
  git -C "$SING_BOX_PLUS_ROOT" ls-files -- . 2>/dev/null || find . -type f -not -path './.upstream/*'
}

while IFS= read -r file; do
  [[ -f "$file" ]] || continue
  case "$file" in
    LICENSE|THIRD_PARTY_NOTICES.md|*.lock) continue ;;
  esac
  if grep -qE 'BEGIN (RSA |EC |OPENSSH |PGP )?PRIVATE KEY' "$file" 2>/dev/null; then
    report "$file 含私钥块"
  fi
  # 真实公网 IPv4：排除文档/回环/私有网段与掩码式示例。
  if grep -qE '(^|[^0-9.])((25[0-5]|2[0-4][0-9]|1[0-9]{2}|[1-9]?[0-9])\.){3}(25[0-5]|2[0-4][0-9]|1[0-9]{2}|[1-9]?[0-9])([^0-9.]|$)' "$file" 2>/dev/null; then
    if grep -vE '127\.0\.0\.1|0\.0\.0\.0|255\.255|192\.0\.2\.|198\.51\.100\.|203\.0\.113\.|10\.[0-9]|172\.(1[6-9]|2[0-9]|3[01])\.|192\.168\.|1\.14\.0|0\.9\.0|1\.24\.0|1\.13\.|1\.15\.|1\.25\.|1\.26\.|3\.0\.|2\.0\.|0\.2\.|0\.3\.|0\.8\.|0\.6\.|0\.7\.|0\.1\.' "$file" 2>/dev/null | \
       grep -qE '(^|[^0-9.])((25[0-5]|2[0-4][0-9]|1[0-9]{2}|[1-9]?[0-9])\.){3}(25[0-5]|2[0-4][0-9]|1[0-9]{2}|[1-9]?[0-9])([^0-9.]|$)'; then
      report "$file 疑似含真实 IPv4（请改用 192.0.2.0/24 等文档网段）"
    fi
  fi
done < <(scan_files)

if [[ "$failures" -gt 0 ]]; then
  die "敏感信息扫描未通过：$failures 处"
fi
printf '敏感信息扫描通过。\n'
