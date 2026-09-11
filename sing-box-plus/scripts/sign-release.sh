#!/usr/bin/env bash
set -euo pipefail

source "$(dirname "$0")/lib.sh"

# 对 manifest 做 detached 签名。私钥离线保管，本脚本只接受路径，绝不生成、绝不落盘私钥。

require_command openssl
require_command python3

usage() {
  printf '用法：%s <发布目录> <ed25519 私钥路径>\n' "$(basename "$0")" >&2
}

[[ $# -eq 2 ]] || { usage; exit 2; }

release_dir="$(absolute_path "$1")"
key_path="$2"

[[ -d "$release_dir" ]] || die "发布目录不存在：$release_dir"
[[ -f "$key_path" ]] || die "私钥不存在：$key_path"
[[ -f "$release_dir/manifest.json" ]] || die "发布目录缺少 manifest.json"

require_clean_worktree

# 先校验 manifest 与产物一致，再签名：对一份自己都没核对过的清单签名毫无意义。
python3 "$SING_BOX_PLUS_ROOT/scripts/release-artifact.py" verify-manifest \
  --manifest "$release_dir/manifest.json"

openssl pkeyutl -sign -inkey "$key_path" -rawin \
  -in "$release_dir/manifest.json" \
  -out "$release_dir/manifest.json.sig"
chmod 0644 "$release_dir/manifest.json.sig"

printf '已签名：%s\n' "$release_dir/manifest.json.sig"
printf '提醒：请单独分发公钥，并把公钥指纹写进 docs/OPERATIONS.md。\n'
