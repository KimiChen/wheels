#!/usr/bin/env bash
set -euo pipefail

source "$(dirname "$0")/lib.sh"

[[ $# -le 1 ]] || { printf '用法: test-source.sh [上游仓库覆盖地址]\n' >&2; exit 2; }
repository_override="${1:-}"

require_command git
require_command go
require_command make
require_command npm

test_root="$(temporary_directory test)"
source_dir="$test_root/source"
trap 'rm -rf -- "$test_root"' EXIT

if [[ -n "$repository_override" ]]; then
  "$SUB2API_PLUS_ROOT/scripts/prepare-source.sh" "$source_dir" "$repository_override"
else
  "$SUB2API_PLUS_ROOT/scripts/prepare-source.sh" "$source_dir"
fi

tooling_dir="$test_root/tooling"
npm install --prefix "$tooling_dir" --no-save --ignore-scripts pnpm@9.15.9
pnpm_bin="$tooling_dir/node_modules/.bin/pnpm"

"$pnpm_bin" --dir "$source_dir/frontend" install --frozen-lockfile
(
  cd "$source_dir/backend"
  go test ./...
  if command -v golangci-lint >/dev/null 2>&1; then
    golangci-lint run ./...
  elif [[ "${REQUIRE_GOLANGCI_LINT:-0}" == "1" ]]; then
    die "REQUIRE_GOLANGCI_LINT=1，但系统未安装 golangci-lint"
  else
    printf '提示: 未安装 golangci-lint，已跳过后端 lint；Go 测试仍已执行。\n'
  fi
)
PATH="$(dirname "$pnpm_bin"):$PATH" make -C "$source_dir" test-frontend
PATH="$(dirname "$pnpm_bin"):$PATH" make -C "$source_dir" build
printf '测试和构建通过。\n'
