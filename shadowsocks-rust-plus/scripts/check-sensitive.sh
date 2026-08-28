#!/usr/bin/env bash
set -euo pipefail

source "$(dirname "$0")/lib.sh"

require_command rg

if [[ $# -gt 1 ]]; then
  printf '用法：%s [扫描目录]\n' "$(basename "$0")" >&2
  exit 2
fi

scan_root="${1:-$SHADOWSOCKS_RUST_PLUS_ROOT}"
[[ -d "$scan_root" ]] || die "敏感信息扫描目录不存在或不是目录：$scan_root"

# This is deliberately a pre-commit hygiene check, not a general secret scanner:
# ripgrep honors ignore files, and only a small set of high-signal key patterns is
# checked. Credential records must occupy a real content line (or a unified-diff
# content line) so prose that merely names a marker is not reported. Diff prefixes
# are included because committed patch payloads are part of this overlay's source.
pem_record='-----BEGIN (RSA |EC |OPENSSH |ENCRYPTED )?PRIVATE KEY-----'
assignment_record='(export[[:space:]]+)?(PrivateKey|Passphrase)[[:space:]]*=[[:space:]]*[^[:space:]#]+'
# Keep both JSON and shell/TOML assignment forms recognizable.  The value
# shapes are deliberately narrow: a 32-byte hex HMAC key or a padded Base64
# encoding of a 16-byte iPSK/uPSK.  Use a double-quoted shell string so the
# optional quote class is actually `['"]?` after shell expansion.
secret_name='export[-_]?hmac([-_]?key)?|hmac[-_]?key|uPSK|iPSK|shared_i_psk|password'
secret_assignment="[\"']?(${secret_name})[\"']?[[:space:]]*[:=][[:space:]]*[\"']?[A-Fa-f0-9]{64}|[\"']?(${secret_name})[\"']?[[:space:]]*[:=][[:space:]]*[\"']?[A-Za-z0-9+/]{22}==?"
sensitive_pattern="(AKIA[0-9A-Z]{16}|^[[:space:]]*(${pem_record}|${assignment_record}|${secret_assignment})|^[+-][[:space:]]*(${pem_record}|${assignment_record}|${secret_assignment}))"
scan_output=""
scan_status=0
scan_output="$(
  rg --files-with-matches --hidden --no-require-git --glob '!target/**' --glob '!.git/**' \
    "$sensitive_pattern" -- "$scan_root"
)" || scan_status=$?

case "$scan_status" in
  0)
    printf '发现疑似敏感信息的文件，请在安全环境中人工审查：\n' >&2
    while IFS= read -r matched_file; do
      printf '  %s\n' "$matched_file" >&2
    done <<< "$scan_output"
    die "发现疑似私钥、访问密钥或凭据"
    ;;
  1)
    exit 0
    ;;
  *)
    die "敏感信息扫描工具执行失败（状态码 $scan_status）"
    ;;
esac
