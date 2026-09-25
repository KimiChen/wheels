#!/usr/bin/env bash
set -euo pipefail

source "$(dirname "$0")/lib.sh"

usage() {
  printf '用法：%s <已准备的上游源码树>\n' "$(basename "$0")" >&2
}

[[ $# -eq 1 ]] || { usage; exit 2; }

tree="$(absolute_path "$1")"

[[ -f "$tree/go.mod" ]] || die "源码树缺少 go.mod：$tree"
head -n 5 "$tree/go.mod" | grep -q '^module github.com/fatedier/frp$' || \
  die "源码树不是 fatedier/frp 模块：$tree"

# 软链接会逃逸出子项目目录，把构建产物绑到本机路径上；一律拒绝。
for dir in agent monitor shared web tests/fixtures; do
  [[ -d "$FRP_MONITOR_ROOT/$dir" ]] || continue
  if [[ -n "$(find "$FRP_MONITOR_ROOT/$dir" -type l -print -quit)" ]]; then
    die "扩展目录包含软链接，拒绝映射：$dir"
  fi
done

target="$tree/extension/frpmonitor"
rm -rf "$target"
mkdir -p "$target"

mapped=()
for dir in agent monitor shared web; do
  src="$FRP_MONITOR_ROOT/$dir"
  [[ -d "$src" ]] || continue
  cp -R "$src" "$target/$dir"
  mapped+=("$dir")
done

# tests/fixtures 供采集器与协议测试在树内引用；tests/loadgen 为容量测试工具。
for sub in fixtures loadgen; do
  if [[ -d "$FRP_MONITOR_ROOT/tests/$sub" ]]; then
    mkdir -p "$target/tests"
    cp -R "$FRP_MONITOR_ROOT/tests/$sub" "$target/tests/$sub"
    mapped+=("tests/$sub")
  fi
done

((${#mapped[@]} > 0)) || die "没有可映射的扩展目录（agent/monitor/shared/web 均不存在）"

printf '已映射到 %s：%s\n' "$target" "${mapped[*]}"
