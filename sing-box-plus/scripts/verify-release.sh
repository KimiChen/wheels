#!/usr/bin/env bash
set -euo pipefail

source "$(dirname "$0")/lib.sh"

require_command openssl
require_command python3

usage() {
  printf '用法：%s <发布目录> <ed25519 公钥路径>\n' "$(basename "$0")" >&2
}

[[ $# -eq 2 ]] || { usage; exit 2; }

release_dir="$(absolute_path "$1")"
public_key="$2"

[[ -f "$release_dir/manifest.json" ]] || die "发布目录缺少 manifest.json"
[[ -f "$release_dir/manifest.json.sig" ]] || die "发布目录缺少签名"
[[ -f "$public_key" ]] || die "公钥不存在：$public_key"

openssl pkeyutl -verify -pubin -inkey "$public_key" -rawin \
  -in "$release_dir/manifest.json" \
  -sigfile "$release_dir/manifest.json.sig" >/dev/null || die "签名验证失败"

python3 "$SING_BOX_PLUS_ROOT/scripts/release-artifact.py" verify-manifest \
  --manifest "$release_dir/manifest.json"

# manifest 里的上游锁定必须与本仓库一致，否则「验签通过」只是证明有人签过一份别的东西。
manifest_commit="$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["upstream_commit"])' "$release_dir/manifest.json")"
[[ "$manifest_commit" == "$(lock_value commit)" ]] || \
  die "manifest 的 upstream_commit 与本仓库 upstream.lock 不一致：$manifest_commit"

printf '验签通过：签名有效、产物哈希一致、上游锁定与本仓库相符。\n'
