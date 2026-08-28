#!/usr/bin/env bash
set -euo pipefail

readonly SHADOWSOCKS_RUST_PLUS_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"

if [[ -f "$SHADOWSOCKS_RUST_PLUS_ROOT/.env" ]]; then
  set -a
  # shellcheck disable=SC1091
  source "$SHADOWSOCKS_RUST_PLUS_ROOT/.env"
  set +a
fi

die() {
  printf '错误：%s\n' "$*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "缺少命令：$1"
}

lock_value() {
  local key="$1"
  local lock_file="$SHADOWSOCKS_RUST_PLUS_ROOT/upstream.lock"
  local value

  value="$(awk -F= -v key="$key" '$1 == key { sub(/^[^=]*=/, ""); print; found = 1 } END { if (!found) exit 1 }' "$lock_file")" || \
    die "upstream.lock 缺少字段：$key"
  [[ -n "$value" ]] || die "upstream.lock 字段为空：$key"
  printf '%s\n' "$value"
}

absolute_path() {
  local input_path="$1"
  local parent_path
  local base_name

  parent_path="$(dirname "$input_path")"
  base_name="$(basename "$input_path")"
  mkdir -p "$parent_path"
  parent_path="$(cd "$parent_path" && pwd -P)"
  printf '%s/%s\n' "$parent_path" "$base_name"
}

safe_remove_temp_dir() {
  local temp_path="$1"

  case "$temp_path" in
    /tmp/shadowsocks-rust-plus.*|"${TMPDIR:-/tmp}"/shadowsocks-rust-plus.*)
      rm -rf -- "$temp_path"
      ;;
    *)
      die "拒绝删除未识别的临时目录：$temp_path"
      ;;
  esac
}

require_clean_worktree() {
  local status

  status="$(git -C "$SHADOWSOCKS_RUST_PLUS_ROOT" status --porcelain --untracked-files=normal)"
  [[ -z "$status" ]] || die "发布签名与验签要求 overlay 工作树干净"
}
