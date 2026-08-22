#!/usr/bin/env bash

SUB2API_PLUS_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
SUB2API_PLUS_LOCK="$SUB2API_PLUS_ROOT/upstream.lock"

die() {
  printf '错误: %s\n' "$*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "缺少命令: $1"
}

lock_value() {
  local key="$1"
  local raw

  raw="$(awk -v key="$key" '
    $1 == key && $2 == "=" {
      sub(/^[^=]*=[[:space:]]*/, "")
      print
      exit
    }
  ' "$SUB2API_PLUS_LOCK")"
  [[ -n "$raw" ]] || die "upstream.lock 缺少字段: $key"
  [[ "$raw" == \"*\" ]] || die "upstream.lock 字段不是字符串: $key"
  raw="${raw#\"}"
  raw="${raw%\"}"
  printf '%s\n' "$raw"
}

manifest_value() {
  local key="$1"
  local manifest="${2:-$SUB2API_PLUS_ROOT/overlay-manifest.tsv}"
  local value

  value="$(awk -F= -v prefix="# $key" '$1 == prefix {print substr($0, index($0, "=") + 1); exit}' "$manifest")"
  [[ -n "$value" ]] || die "Overlay 清单缺少字段: $key"
  printf '%s\n' "$value"
}

validate_relative_path() {
  local path="$1"

  [[ -n "$path" ]] || die "发现空路径"
  [[ "$path" != /* ]] || die "不允许绝对路径: $path"
  [[ "$path" != *$'\n'* && "$path" != *$'\t'* ]] || die "路径不能包含换行或 Tab"
  case "/$path/" in
    */../*|*/./*) die "路径包含不安全段: $path" ;;
  esac
}

temporary_directory() {
  local purpose="$1"
  mktemp -d "${TMPDIR:-/tmp}/sub2api-plus-${purpose}.XXXXXX"
}

sha256_file() {
  local path="$1"

  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$path" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$path" | awk '{print $1}'
  else
    die "缺少 SHA-256 工具（sha256sum 或 shasum）"
  fi
}

validate_safe_name() {
  local label="$1"
  local value="$2"

  [[ -n "$value" ]] || die "$label 不能为空"
  [[ "$value" =~ ^[A-Za-z0-9._+-]+$ ]] || die "$label 包含不安全字符: $value"
  [[ "$value" != "." && "$value" != ".." ]] || die "$label 不能是路径特殊段"
}

validate_absolute_directory() {
  local label="$1"
  local value="$2"

  [[ "$value" == /* && "$value" != "/" ]] || die "$label 必须是非根目录的绝对路径"
  [[ "$value" != *$'\n'* && "$value" != *$'\t'* ]] || die "$label 包含不安全字符"
  case "/${value#/}/" in
    */../*|*/./*) die "$label 包含不安全路径段: $value" ;;
  esac
}
