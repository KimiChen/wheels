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
