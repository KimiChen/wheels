#!/usr/bin/env bash
set -euo pipefail

readonly SHADOWSOCKS_RUST_PLUS_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"

die() {
  printf '错误：%s\n' "$*" >&2
  exit 1
}

# Switch environment variables are a three-state contract, not a truthiness
# test.  `${VAR:-default} == 1` collapses every unrecognised value onto the
# "off" branch, so a typo (`yes`, `true`, `2`) silently opts out of a gate the
# operator meant to keep -- the wrong direction for a fail-closed switch.
# Accept only `0` and `1`; reject anything else loudly.
require_bool_env() {
  local name="$1" default="$2" value
  value="${!name-}"
  [[ -n "$value" ]] || value="$default"
  case "$value" in
    0 | 1) printf '%s\n' "$value" ;;
    *) die "$name 只接受 0 或 1，实际：$value" ;;
  esac
}

# `.env` is an untracked developer convenience.  It must never be executed as
# shell (arbitrary code) and must never be able to introduce build inputs that
# the release receipt does not record.  Import a fixed key allowlist literally,
# and let release scripts opt out entirely with
# SHADOWSOCKS_RUST_PLUS_NO_DOTENV=1.
readonly SHADOWSOCKS_RUST_PLUS_DOTENV_KEYS=("UPSTREAM_REPOSITORY" "CARGO_HOME")

load_dotenv() {
  local file="$SHADOWSOCKS_RUST_PLUS_ROOT/.env"
  [[ -f "$file" ]] || return 0
  if [[ "$(require_bool_env SHADOWSOCKS_RUST_PLUS_NO_DOTENV 0)" == 1 ]]; then
    printf '忽略 .env：当前脚本要求可复现的环境输入。\n' >&2
    return 0
  fi
  local line key value allowed
  while IFS= read -r line || [[ -n "$line" ]]; do
    [[ -z "${line//[[:space:]]/}" || "$line" == \#* ]] && continue
    [[ "$line" =~ ^[A-Za-z_][A-Za-z0-9_]*= ]] || \
      die ".env 只接受 KEY=VALUE 行，不是 shell 脚本：$line"
    key="${line%%=*}"
    value="${line#*=}"
    allowed=0
    for candidate in "${SHADOWSOCKS_RUST_PLUS_DOTENV_KEYS[@]}"; do
      [[ "$key" == "$candidate" ]] && allowed=1 && break
    done
    [[ "$allowed" == 1 ]] || die ".env 不允许的键：$key（允许：${SHADOWSOCKS_RUST_PLUS_DOTENV_KEYS[*]}）"
    if [[ "$value" == \"*\" || "$value" == \'*\' ]]; then
      value="${value:1:${#value}-2}"
    fi
    export "$key=$value"
  done < "$file"
}

load_dotenv

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
  # Resolving a path must stay side-effect free: creating the parent here made
  # every rejected `--output` leave directories behind on the release host.
  [[ -d "$parent_path" ]] || die "路径的父目录不存在：$parent_path"
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
