#!/usr/bin/env bash
set -euo pipefail

source "$(dirname "$0")/lib.sh"

usage() {
  cat >&2 <<'EOF'
用法:
  systemd-release.sh deploy <发布产物目录>
  systemd-release.sh rollback [release-id]

deploy 默认要求 DATABASE_BACKUP_FILE 指向已验证的 PostgreSQL custom-format 备份。
只有明确设置 SKIP_DATABASE_BACKUP_CHECK=1 时才跳过此门禁。
EOF
}

[[ $# -ge 1 ]] || { usage; exit 2; }
action="$1"
shift

service_name="${SERVICE_NAME:-sub2api}"
install_root="${INSTALL_ROOT:-/opt/sub2api}"
health_url="${HEALTH_URL:-http://127.0.0.1:8080/status}"
health_attempts="${HEALTH_ATTEMPTS:-30}"
health_interval="${HEALTH_INTERVAL:-1}"

validate_safe_name "SERVICE_NAME" "$service_name"
validate_absolute_directory "INSTALL_ROOT" "$install_root"
[[ "$health_attempts" =~ ^[1-9][0-9]*$ ]] || die "HEALTH_ATTEMPTS 必须是正整数"
[[ "$health_interval" =~ ^[0-9]+$ ]] || die "HEALTH_INTERVAL 必须是非负整数"

[[ "$(uname -s)" == "Linux" ]] || die "systemd 发布脚本只能在目标 Linux 服务器执行"
[[ "$EUID" -eq 0 ]] || die "请使用 root 权限执行 systemd 发布脚本"

require_command curl
require_command file
require_command flock
require_command mv
require_command readlink
require_command rsync
require_command sha256sum
require_command systemctl

releases_dir="$install_root/releases"
resources_dir="$install_root/resources"
state_dir="$install_root/.release-state"
binary_link="$install_root/sub2api"

mkdir -p "$releases_dir" "$state_dir"
exec 9> "$state_dir/release.lock"
flock -n 9 || die "已有另一个发布或回滚任务正在执行"

systemctl cat "$service_name" >/dev/null 2>&1 || die "systemd 服务不存在: $service_name"

run_user="${RUN_USER:-$(systemctl show "$service_name" -p User --value)}"
run_user="${run_user:-root}"
id "$run_user" >/dev/null 2>&1 || die "运行用户不存在: $run_user"
run_group="${RUN_GROUP:-$(systemctl show "$service_name" -p Group --value)}"
run_group="${run_group:-$(id -gn "$run_user")}"

atomic_binary_switch() {
  local target="$1"
  local temporary_link="$binary_link.new.$$"

  [[ ! -e "$binary_link" || -L "$binary_link" ]] || \
    die "稳定入口不是软链接，拒绝覆盖: $binary_link"
  rm -f -- "$temporary_link"
  ln -s -- "$target" "$temporary_link"
  mv -Tf -- "$temporary_link" "$binary_link"
}

sync_resources() {
  local source_dir="$1"
  install -d -m 0755 -o "$run_user" -g "$run_group" "$resources_dir"
  rsync -rlptD --delete "$source_dir/" "$resources_dir/"
  chown -R "$run_user:$run_group" "$resources_dir"
}

wait_until_healthy() {
  local response=""
  local attempt

  systemctl is-active --quiet "$service_name" || return 1
  for ((attempt = 1; attempt <= health_attempts; attempt++)); do
    if response="$(curl --fail --silent --show-error --max-time 5 "$health_url" 2>/dev/null)" &&
      grep -Fq '"status":"perfectly nice"' <<<"$response"; then
      [[ "$(systemctl show "$service_name" -p NRestarts --value)" == "0" ]] || return 1
      return 0
    fi
    sleep "$health_interval"
  done
  return 1
}

restore_after_failure() {
  local previous_target="$1"
  local resources_backup="$2"
  local resources_existed="$3"
  local had_previous=0

  printf '版本切换验证失败，正在恢复切换前版本。\n' >&2
  if [[ -n "$previous_target" && -f "$previous_target" ]]; then
    had_previous=1
    atomic_binary_switch "$previous_target"
  elif [[ -L "$binary_link" ]]; then
    mv -- "$binary_link" "$resources_backup.binary-link.failed"
  fi
  if [[ -d "$resources_backup" ]]; then
    sync_resources "$resources_backup"
  elif [[ "$resources_existed" == "0" && -d "$resources_dir" ]]; then
    mv -- "$resources_dir" "$resources_backup.failed"
  fi
  if [[ "$had_previous" == "1" ]]; then
    systemctl restart "$service_name" || true
  else
    systemctl stop "$service_name" || true
  fi
}

artifact_value() {
  local key="$1"
  local manifest="$2"
  local value

  value="$(awk -F= -v key="$key" '$1 == key {print substr($0, index($0, "=") + 1); exit}' "$manifest")"
  [[ -n "$value" ]] || die "发布清单缺少字段: $key"
  printf '%s\n' "$value"
}

deploy_release() {
  local artifact_input="$1"
  local artifact_dir
  local manifest
  local release_id
  local release_dir
  local previous_target=""
  local release_state
  local resources_backup
  local resources_existed=0
  local required_kb
  local available_kb
  local backup_file
  local backup_sha256="skipped"
  local backup_size="skipped"
  local server_arch

  [[ -d "$artifact_input" ]] || die "发布产物目录不存在: $artifact_input"
  artifact_dir="$(cd "$artifact_input" && pwd -P)"
  manifest="$artifact_dir/release.env"
  [[ -f "$manifest" && -f "$artifact_dir/SHA256SUMS" && -f "$artifact_dir/sub2api" ]] || \
    die "发布产物不完整"
  [[ -d "$artifact_dir/resources" ]] || die "发布产物缺少 resources"

  (cd "$artifact_dir" && sha256sum -c SHA256SUMS)
  file "$artifact_dir/sub2api" | grep -q 'ELF' || die "发布二进制不是 Linux ELF"

  [[ "$(artifact_value format "$manifest")" == "1" ]] || die "不支持的发布清单格式"
  [[ "$(artifact_value goos "$manifest")" == "linux" ]] || die "发布产物目标系统不是 Linux"
  case "$(uname -m)" in
    x86_64) server_arch=amd64 ;;
    aarch64|arm64) server_arch=arm64 ;;
    *) server_arch="$(uname -m)" ;;
  esac
  [[ "$(artifact_value goarch "$manifest")" == "$server_arch" ]] || \
    die "发布产物架构与服务器不匹配（服务器: $server_arch）"
  release_id="$(artifact_value release_id "$manifest")"
  validate_safe_name "release_id" "$release_id"
  release_dir="$releases_dir/$release_id"
  [[ ! -e "$release_dir" ]] || die "发布版本已存在，拒绝覆盖: $release_id"

  if [[ "${SKIP_DATABASE_BACKUP_CHECK:-0}" == "1" ]]; then
    printf '警告: 已显式跳过数据库备份门禁。\n' >&2
  else
    backup_file="${DATABASE_BACKUP_FILE:-}"
    [[ -n "$backup_file" && -f "$backup_file" ]] || \
      die "请设置 DATABASE_BACKUP_FILE 指向本次发布前的 PostgreSQL custom-format 备份"
    require_command pg_restore
    pg_restore -l "$backup_file" >/dev/null
    backup_sha256="$(sha256_file "$backup_file")"
    backup_size="$(wc -c < "$backup_file" | tr -d '[:space:]')"
    printf '数据库备份已验证: %s bytes；SHA-256 %s\n' "$backup_size" "$backup_sha256"
  fi

  required_kb="$(( $(du -sk "$artifact_dir" | awk '{print $1}') * 3 + 10240 ))"
  available_kb="$(df -Pk "$install_root" | awk 'NR == 2 {print $4}')"
  [[ "$available_kb" =~ ^[0-9]+$ && "$available_kb" -ge "$required_kb" ]] || \
    die "可用磁盘空间不足，需要至少 ${required_kb} KiB"

  [[ ! -e "$binary_link" || -L "$binary_link" ]] || \
    die "稳定入口不是软链接，拒绝发布: $binary_link"
  if [[ -L "$binary_link" ]]; then
    previous_target="$(readlink -f "$binary_link")"
    [[ -f "$previous_target" ]] || die "当前稳定入口指向无效文件"
  fi

  release_state="$state_dir/$release_id"
  resources_backup="$release_state/resources-before"
  mkdir -p "$release_state"
  printf '%s\n' "$previous_target" > "$release_state/previous-target"
  printf 'size_bytes=%s\nsha256=%s\n' "$backup_size" "$backup_sha256" > \
    "$release_state/database-backup"

  install -d -m 0755 -o "$run_user" -g "$run_group" "$release_dir/resources"
  install -m 0755 -o "$run_user" -g "$run_group" "$artifact_dir/sub2api" "$release_dir/sub2api"
  install -m 0644 "$manifest" "$artifact_dir/SHA256SUMS" "$release_dir/"
  rsync -rlptD --delete "$artifact_dir/resources/" "$release_dir/resources/"
  chown -R "$run_user:$run_group" "$release_dir"
  (cd "$release_dir" && sha256sum -c SHA256SUMS)

  if [[ -d "$resources_dir" ]]; then
    resources_existed=1
    if [[ -n "$(rsync -rlptDcni --delete "$release_dir/resources/" "$resources_dir/")" ]]; then
      mkdir -p "$resources_backup"
      rsync -rlptD "$resources_dir/" "$resources_backup/"
    fi
  fi
  printf '%s\n' "$resources_existed" > "$release_state/resources-existed"

  sync_resources "$release_dir/resources"
  atomic_binary_switch "$release_dir/sub2api"
  if ! systemctl restart "$service_name" || ! wait_until_healthy; then
    restore_after_failure "$previous_target" "$resources_backup" "$resources_existed"
    die "新版本未通过本机健康检查，已尝试恢复上一版本"
  fi

  printf '%s\n' "$release_id" > "$state_dir/current-release"
  printf '发布完成: %s\n' "$release_id"
  printf '服务状态: %s；健康检查: %s\n' "$(systemctl is-active "$service_name")" "$health_url"
}

rollback_release() {
  local requested_release="${1:-}"
  local current_target
  local current_release=""
  local target_release
  local target_dir
  local target_binary
  local target_resources=""
  local rollback_id
  local rollback_state
  local resources_backup
  local resources_existed=0

  [[ -L "$binary_link" ]] || die "当前稳定入口不是软链接，无法安全回滚"
  current_target="$(readlink -f "$binary_link")"
  [[ -f "$current_target" ]] || die "当前稳定入口指向无效文件"
  [[ -f "$state_dir/current-release" ]] && current_release="$(<"$state_dir/current-release")"
  [[ -z "$current_release" ]] || validate_safe_name "当前 release-id" "$current_release"

  if [[ -n "$requested_release" ]]; then
    target_release="$requested_release"
    validate_safe_name "release-id" "$target_release"
    target_dir="$releases_dir/$target_release"
  else
    [[ -n "$current_release" ]] || die "没有当前发布状态，请显式指定 release-id"
    [[ -f "$state_dir/$current_release/previous-target" ]] || die "没有可用的上一版本记录"
    target_binary="$(<"$state_dir/$current_release/previous-target")"
    [[ -n "$target_binary" ]] || die "当前版本是首次发布，没有可回滚版本"
    target_dir="$(dirname "$target_binary")"
    target_release="$(basename "$target_dir")"
    validate_safe_name "上一 release-id" "$target_release"
  fi

  target_binary="$target_dir/sub2api"
  [[ -f "$target_binary" ]] || die "回滚二进制不存在: $target_binary"
  [[ "$target_dir" == "$releases_dir/"* ]] || die "回滚目标不在 releases 目录内"
  [[ "$target_binary" != "$current_target" ]] || die "目标版本已经是当前版本"

  if [[ -d "$target_dir/resources" ]]; then
    target_resources="$target_dir/resources"
  elif [[ -n "$current_release" && -d "$state_dir/$current_release/resources-before" ]]; then
    target_resources="$state_dir/$current_release/resources-before"
  fi

  rollback_id="rollback-$(date -u +%Y%m%d-%H%M%S)"
  rollback_state="$state_dir/$rollback_id"
  resources_backup="$rollback_state/resources-before"
  mkdir -p "$rollback_state"
  printf '%s\n' "$current_target" > "$rollback_state/previous-target"
  if [[ -d "$resources_dir" ]]; then
    resources_existed=1
    mkdir -p "$resources_backup"
    rsync -rlptD "$resources_dir/" "$resources_backup/"
  fi

  [[ -z "$target_resources" ]] || sync_resources "$target_resources"
  atomic_binary_switch "$target_binary"
  if ! systemctl restart "$service_name" || ! wait_until_healthy; then
    restore_after_failure "$current_target" "$resources_backup" "$resources_existed"
    die "回滚版本未通过本机健康检查，已尝试恢复回滚前版本"
  fi

  printf '%s\n' "$target_release" > "$state_dir/current-release"
  printf '回滚完成: %s\n' "$target_release"
  printf '数据库未自动回滚；如迁移不兼容，必须经过人工确认后单独恢复。\n'
}

case "$action" in
  deploy)
    [[ $# -eq 1 ]] || { usage; exit 2; }
    deploy_release "$1"
    ;;
  rollback)
    [[ $# -le 1 ]] || { usage; exit 2; }
    rollback_release "${1:-}"
    ;;
  *)
    usage
    exit 2
    ;;
esac
