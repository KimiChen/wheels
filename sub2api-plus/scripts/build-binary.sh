#!/usr/bin/env bash
set -euo pipefail

source "$(dirname "$0")/lib.sh"

usage() {
  printf '用法: build-binary.sh [上游仓库覆盖地址]\n' >&2
}

[[ $# -le 1 ]] || { usage; exit 2; }
repository_override="${1:-}"

require_command file
require_command git
require_command go
require_command npm
require_command rsync

target_os="${BINARY_GOOS:-linux}"
target_arch="${BINARY_GOARCH:-amd64}"
validate_safe_name "BINARY_GOOS" "$target_os"
validate_safe_name "BINARY_GOARCH" "$target_arch"

source_commit="$(manifest_value source_commit)"
source_tree="$(manifest_value source_tree)"
upstream_commit="$(lock_value commit)"
validate_safe_name "source_commit" "$source_commit"
validate_safe_name "source_tree" "$source_tree"

release_id="${RELEASE_ID:-$(date -u +%Y%m%d-%H%M%S)-${source_commit:0:12}}"
validate_safe_name "RELEASE_ID" "$release_id"

output_dir="${OUTPUT_DIR:-$SUB2API_PLUS_ROOT/dist/$release_id}"
[[ ! -e "$output_dir" ]] || die "输出路径已存在，拒绝覆盖: $output_dir"

build_root="$(temporary_directory binary)"
source_dir="$build_root/source"
artifact_dir="$build_root/artifact"
trap 'rm -rf -- "$build_root"' EXIT

if [[ -n "$repository_override" ]]; then
  "$SUB2API_PLUS_ROOT/scripts/prepare-source.sh" "$source_dir" "$repository_override"
else
  "$SUB2API_PLUS_ROOT/scripts/prepare-source.sh" "$source_dir"
fi

tooling_dir="$build_root/tooling"
npm install --prefix "$tooling_dir" --no-save --ignore-scripts pnpm@9.15.9
pnpm_bin="$tooling_dir/node_modules/.bin/pnpm"
"$pnpm_bin" --dir "$source_dir/frontend" install --frozen-lockfile
"$pnpm_bin" --dir "$source_dir/frontend" build

version="$(
  tr -d '\r\n' < "$source_dir/backend/cmd/server/VERSION"
  tr -d '\r\n' < "$source_dir/backend/cmd/server/SUB_VERSION"
)"
validate_safe_name "应用版本" "$version"
build_date="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

mkdir -p "$artifact_dir/resources"
(
  cd "$source_dir/backend"
  CGO_ENABLED=0 GOOS="$target_os" GOARCH="$target_arch" go build \
    -buildvcs=false \
    -tags embed \
    -ldflags="-s -w -X main.Version=$version -X main.Commit=$source_commit -X main.Date=$build_date -X main.BuildType=release" \
    -trimpath \
    -o "$artifact_dir/sub2api" \
    ./cmd/server
)
chmod 0755 "$artifact_dir/sub2api"
rsync -a --delete "$source_dir/backend/resources/" "$artifact_dir/resources/"

if [[ "$target_os" == "linux" ]]; then
  file "$artifact_dir/sub2api" | grep -q 'ELF' || die "构建产物不是 Linux ELF"
fi

cat > "$artifact_dir/release.env" <<EOF
format=1
release_id=$release_id
version=$version
source_commit=$source_commit
source_tree=$source_tree
upstream_commit=$upstream_commit
build_date=$build_date
goos=$target_os
goarch=$target_arch
EOF

(
  cd "$artifact_dir"
  : > SHA256SUMS
  while IFS= read -r path; do
    printf '%s  %s\n' "$(sha256_file "$path")" "${path#./}" >> SHA256SUMS
  done < <(find . -type f ! -name SHA256SUMS -print | LC_ALL=C sort)
)

mkdir -p "$(dirname "$output_dir")"
mv "$artifact_dir" "$output_dir"
output_dir="$(cd "$output_dir" && pwd -P)"

printf 'systemd 发布产物已生成: %s\n' "$output_dir"
printf '版本: %s\n定制提交: %s\n组装 tree: %s\n' \
  "$version" "$source_commit" "$source_tree"
printf '二进制 SHA-256: %s\n' "$(sha256_file "$output_dir/sub2api")"
