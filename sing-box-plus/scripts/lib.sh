#!/usr/bin/env bash
set -euo pipefail

readonly SING_BOX_PLUS_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"

die() {
  printf '错误：%s\n' "$*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "缺少命令：$1"
}

# 开关型环境变量是三态契约，不是真值测试。
# `${VAR:-default} == 1` 会把每一个无法识别的取值都塌缩到「关」的分支上，
# 于是一个笔误（yes、true、2）就静默跳过了运维本想保留的门禁——对失败关闭的开关而言方向是反的。
# 只接受 0 和 1，其余一律大声拒绝。
require_bool_env() {
  local name="$1" default="$2" value
  value="${!name-}"
  [[ -n "$value" ]] || value="$default"
  case "$value" in
    0 | 1) printf '%s\n' "$value" ;;
    *) die "$name 只接受 0 或 1，实际：$value" ;;
  esac
}

# `.env` 是未纳入版本控制的开发便利品。它绝不能被当作 shell 执行（等于任意代码），
# 也绝不能引入发布凭据未记录的构建输入。按固定键白名单逐字导入。
readonly SING_BOX_PLUS_DOTENV_KEYS=("UPSTREAM_REPOSITORY" "GOMODCACHE" "GOCACHE" "SING_BOX_BUILD_TAGS")

load_dotenv() {
  local file="$SING_BOX_PLUS_ROOT/.env"
  local no_dotenv

  no_dotenv="$(require_bool_env SING_BOX_PLUS_NO_DOTENV 0)" || return 1
  [[ -f "$file" ]] || return 0
  if [[ "$no_dotenv" == 1 ]]; then
    printf '忽略 .env：当前脚本要求可复现的环境输入。\n' >&2
    return 0
  fi
  local line key value allowed candidate
  while IFS= read -r line || [[ -n "$line" ]]; do
    [[ -z "${line//[[:space:]]/}" || "$line" == \#* ]] && continue
    [[ "$line" =~ ^[A-Za-z_][A-Za-z0-9_]*= ]] || \
      die ".env 只接受 KEY=VALUE 行，不是 shell 脚本：$line"
    key="${line%%=*}"
    value="${line#*=}"
    allowed=0
    for candidate in "${SING_BOX_PLUS_DOTENV_KEYS[@]}"; do
      [[ "$key" == "$candidate" ]] && allowed=1 && break
    done
    [[ "$allowed" == 1 ]] || die ".env 不允许的键：$key（允许：${SING_BOX_PLUS_DOTENV_KEYS[*]}）"
    if [[ "$value" == \"*\" || "$value" == \'*\' ]]; then
      value="${value:1:${#value}-2}"
    fi
    [[ -n "$value" ]] || continue
    export "$key=$value"
  done < "$file"
}

load_dotenv

lock_value() {
  local key="$1"
  local lock_file="$SING_BOX_PLUS_ROOT/upstream.lock"
  local value

  value="$(awk -F= -v key="$key" '$1 == key { sub(/^[^=]*=/, ""); print; found = 1 } END { if (!found) exit 1 }' "$lock_file")" || \
    die "upstream.lock 缺少字段：$key"
  [[ -n "$value" ]] || die "upstream.lock 字段为空：$key"
  printf '%s\n' "$value"
}

absolute_path() {
  local input_path="$1"
  local parent_path base_name

  parent_path="$(dirname "$input_path")"
  base_name="$(basename "$input_path")"
  # 解析路径必须无副作用：在这里顺手创建父目录，会让每一个被拒绝的 --output
  # 都在发布主机上留下目录。
  [[ -d "$parent_path" ]] || die "路径的父目录不存在：$parent_path"
  parent_path="$(cd "$parent_path" && pwd -P)"
  printf '%s/%s\n' "$parent_path" "$base_name"
}

safe_remove_temp_dir() {
  local temp_path="$1"
  case "$temp_path" in
    /tmp/sing-box-plus.*|"${TMPDIR:-/tmp}"/sing-box-plus.*)
      rm -rf -- "$temp_path"
      ;;
    *)
      die "拒绝删除未识别的临时目录：$temp_path"
      ;;
  esac
}

require_clean_worktree() {
  local status
  status="$(git -C "$SING_BOX_PLUS_ROOT" status --porcelain --untracked-files=normal -- .)"
  [[ -z "$status" ]] || die "发布签名与验签要求 overlay 工作树干净"
}

# 生产 tag 集（README §9.1 / D4）。裁 tag 不等于隔离上游账本，配置校验仍须自行实现。
production_build_tags() {
  printf '%s\n' "${SING_BOX_BUILD_TAGS:-with_utls,badlinkname,with_user_stats}"
}
