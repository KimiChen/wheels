#!/usr/bin/env bash
set -euo pipefail

# frp-monitor 脚本公共库。约定移植自 sing-box-plus/scripts/lib.sh。

readonly FRP_MONITOR_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"

die() {
  printf '错误：%s\n' "$*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "缺少命令：$1"
}

# 开关型环境变量是三态契约，不是真值测试：只接受 0 和 1，其余一律大声拒绝，
# 避免笔误（yes、true、2）把门禁静默塌缩到「关」。
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
# 按固定键白名单逐字导入。允许的键见 .env.example。
readonly FRP_MONITOR_DOTENV_KEYS=(
  "FRP_MONITOR_CACHE_DIR"
  "FRP_MONITOR_OUTPUT_DIR"
  "FRP_MONITOR_UPSTREAM_MIRROR"
  "GOMODCACHE"
  "GOCACHE"
)

load_dotenv() {
  local file="$FRP_MONITOR_ROOT/.env"
  local no_dotenv

  no_dotenv="$(require_bool_env FRP_MONITOR_NO_DOTENV 0)" || return 1
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
    for candidate in "${FRP_MONITOR_DOTENV_KEYS[@]}"; do
      [[ "$key" == "$candidate" ]] && allowed=1 && break
    done
    [[ "$allowed" == 1 ]] || die ".env 不允许的键：$key（允许：${FRP_MONITOR_DOTENV_KEYS[*]}）"
    if [[ "$value" == \"*\" || "$value" == \'*\' ]]; then
      value="${value:1:${#value}-2}"
    fi
    [[ -n "$value" ]] || continue
    export "$key=$value"
  done < "$file"
}

load_dotenv

# upstream.lock 使用 `key = value`（允许空白，值可带双引号）。逐键精确匹配，拒绝缺失或空值。
lock_value() {
  local key="$1"
  local lock_file="$FRP_MONITOR_ROOT/upstream.lock"
  local value

  value="$(awk -v key="$key" '
    {
      line = $0
      sub(/[[:space:]]*#.*/, "", line)
      if (line !~ /=/) next
      k = line; sub(/=.*/, "", k)
      v = line; sub(/^[^=]*=/, "", v)
      gsub(/^[[:space:]]+|[[:space:]]+$/, "", k)
      gsub(/^[[:space:]]+|[[:space:]]+$/, "", v)
      sub(/^"/, "", v); sub(/"$/, "", v)
      if (k == key) { print v; found = 1; exit }
    }
    END { if (!found) exit 1 }
  ' "$lock_file")" || die "upstream.lock 缺少字段：$key"
  [[ -n "$value" ]] || die "upstream.lock 字段为空：$key"
  printf '%s\n' "$value"
}

absolute_path() {
  local input_path="$1"
  local parent_path base_name

  parent_path="$(dirname "$input_path")"
  base_name="$(basename "$input_path")"
  # 解析路径必须无副作用：在这里顺手创建父目录，会让每一个被拒绝的输出参数
  # 都在磁盘上留下目录。
  [[ -d "$parent_path" ]] || die "路径的父目录不存在：$parent_path"
  parent_path="$(cd "$parent_path" && pwd -P)"
  printf '%s/%s\n' "$parent_path" "$base_name"
}

safe_remove_temp_dir() {
  local temp_path="$1"
  case "$temp_path" in
    /tmp/frp-monitor.*|"${TMPDIR:-/tmp}"/frp-monitor.*)
      rm -rf -- "$temp_path"
      ;;
    *)
      die "拒绝删除未识别的临时目录：$temp_path"
      ;;
  esac
}

# 上游 Dashboard 前端资源由 npm 构建、不随源码分发；没有 dist 目录时
# web/frpc/embed.go 的 //go:embed 会让任何 go build 直接失败。
# 上游为此提供 noweb build tag（web/*/embed_stub.go），本项目网页由
# monitor 自行嵌入，统一使用 noweb。
frp_build_tags() {
  printf '%s\n' "noweb"
}

# 相对路径一律相对子项目根解析，与调用者 cwd 无关。
cache_dir() {
  local dir="${FRP_MONITOR_CACHE_DIR:-.cache/frp-monitor}"
  case "$dir" in
    /*) printf '%s\n' "$dir" ;;
    *) printf '%s/%s\n' "$FRP_MONITOR_ROOT" "$dir" ;;
  esac
}

output_dir() {
  local dir="${FRP_MONITOR_OUTPUT_DIR:-dist}"
  case "$dir" in
    /*) printf '%s\n' "$dir" ;;
    *) printf '%s/%s\n' "$FRP_MONITOR_ROOT" "$dir" ;;
  esac
}
