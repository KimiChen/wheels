#!/usr/bin/env bash
set -euo pipefail

source "$(dirname "$0")/lib.sh"

[[ $# -le 1 ]] || { printf '用法: build-image.sh [上游仓库覆盖地址]\n' >&2; exit 2; }
repository_override="${1:-}"

require_command docker

image_name="${IMAGE_NAME:-sub2api-plus:local}"
docker_platform="${DOCKER_PLATFORM:-}"
docker_pull="${DOCKER_PULL:-0}"

build_root="$(temporary_directory image)"
source_dir="$build_root/source"
trap 'rm -rf -- "$build_root"' EXIT

if [[ -n "$repository_override" ]]; then
  "$SUB2API_PLUS_ROOT/scripts/prepare-source.sh" "$source_dir" "$repository_override"
else
  "$SUB2API_PLUS_ROOT/scripts/prepare-source.sh" "$source_dir"
fi

docker_args=(build --tag "$image_name")
[[ "$docker_pull" == "1" ]] && docker_args+=(--pull)
[[ -n "$docker_platform" ]] && docker_args+=(--platform "$docker_platform")
docker_args+=("$source_dir")

docker "${docker_args[@]}"
printf '镜像构建完成: %s\n' "$image_name"
