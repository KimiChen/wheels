#!/usr/bin/env bash
set -euo pipefail

source "$(dirname "$0")/lib.sh"

require_command openssl
require_command python3

usage() {
  printf '用法：%s <发布目录> <ed25519 公钥路径|--no-signature>\n' "$(basename "$0")" >&2
}

[[ $# -eq 2 ]] || { usage; exit 2; }

release_dir="$(absolute_path "$1")"
public_key="${2:-}"

[[ -f "$release_dir/manifest.json" ]] || die "发布目录缺少 manifest.json"

# --no-signature 是给「还没做离线签名」的产物用的。此前本脚本硬要求签名文件，
# 于是尚未签名的产物根本无法用本项目的工具做任何核验——运维只能 sha256sum 手对，
# 那种手对不会检查 manifest 与本仓库锁定的一致性。宁可提供一个明确降级的模式，
# 也不要把人推到脚本之外去。
signature_checked=no
if [[ "$public_key" == "--no-signature" ]]; then
  printf '注意：本次未验签，仅核对产物哈希与源码锁定。未签名的产物不得部署。\n' >&2
else
  [[ -n "$public_key" ]] || die "用法：<发布目录> <公钥|--no-signature>"
  [[ -f "$release_dir/manifest.json.sig" ]] || die "发布目录缺少签名"
  [[ -f "$public_key" ]] || die "公钥不存在：$public_key"
  openssl pkeyutl -verify -pubin -inkey "$public_key" -rawin \
    -in "$release_dir/manifest.json" \
    -sigfile "$release_dir/manifest.json.sig" >/dev/null || die "签名验证失败"
  signature_checked=yes
fi

python3 "$SING_BOX_PLUS_ROOT/scripts/release-artifact.py" verify-manifest \
  --manifest "$release_dir/manifest.json"

# manifest 里的上游锁定必须与本仓库一致，否则「验签通过」只是证明有人签过一份别的东西。
manifest_commit="$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["upstream_commit"])' "$release_dir/manifest.json")"
[[ "$manifest_commit" == "$(lock_value commit)" ]] || \
  die "manifest 的 upstream_commit 与本仓库 upstream.lock 不一致：$manifest_commit"

# 叠加层 commit 同样要对上：没有它，「这份产物出自哪个源码状态」无人能答。
overlay_commit="$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1])).get("overlay_commit",""))' "$release_dir/manifest.json")"
[[ -n "$overlay_commit" ]] || die "manifest 缺少 overlay_commit：请用当前构建脚本重新产出"
if [[ "$overlay_commit" != "$(git -C "$SING_BOX_PLUS_ROOT" rev-parse HEAD)" ]]; then
  printf '注意：manifest 的 overlay_commit 为 %s，与当前工作副本 HEAD 不同。\n' "$overlay_commit" >&2
fi

if [[ "$signature_checked" == yes ]]; then
  printf '验签通过：签名有效、产物哈希一致、上游与叠加层锁定均与本仓库相符。\n'
else
  printf '核验通过（未验签）：产物哈希一致、上游与叠加层锁定均与本仓库相符。\n'
fi
