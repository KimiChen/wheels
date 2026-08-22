#!/usr/bin/env bash
set -euo pipefail

source "$(dirname "$0")/lib.sh"

usage() {
  cat >&2 <<'EOF'
用法:
  update-upstream.sh prepare <新上游 ref> <不存在的更新目录> [上游仓库覆盖地址]
  update-upstream.sh finalize <更新目录>
EOF
}

[[ $# -ge 2 ]] || { usage; exit 2; }
command_name="$1"
shift

case "$command_name" in
  prepare)
    [[ $# -ge 2 && $# -le 3 ]] || { usage; exit 2; }
    new_ref="$1"
    update_dir="$2"
    repository_override="${3:-}"

    require_command git
    [[ ! -e "$update_dir" ]] || die "更新目录已存在，拒绝覆盖: $update_dir"

    if [[ -n "$repository_override" ]]; then
      "$SUB2API_PLUS_ROOT/scripts/prepare-source.sh" "$update_dir" "$repository_override"
    else
      "$SUB2API_PLUS_ROOT/scripts/prepare-source.sh" "$update_dir"
    fi

    old_commit="$(lock_value commit)"
    git -C "$update_dir" add -A -f -- .
    git -C "$update_dir" \
      -c user.name=sub2api-plus-overlay \
      -c user.email=sub2api-plus-overlay@localhost \
      commit -m "chore: reconstruct sub2api-plus overlay"

    git -C "$update_dir" fetch origin "$new_ref"
    new_commit="$(git -C "$update_dir" rev-parse FETCH_HEAD^{commit})"
    git -C "$update_dir" merge-base --is-ancestor "$old_commit" "$new_commit" || \
      die "新上游不是当前锁定提交的后代"

    git -C "$update_dir" config sub2api-plus.newUpstream "$new_commit"
    if ! git -C "$update_dir" \
      -c user.name=sub2api-plus-overlay \
      -c user.email=sub2api-plus-overlay@localhost \
      merge --no-edit "$new_commit"; then
      printf '\n上游合并存在冲突。请在以下目录解决、暂存并提交：\n%s\n' \
        "$(cd "$update_dir" && pwd -P)" >&2
      printf '解决后执行: %s finalize %s\n' "$0" "$update_dir" >&2
      exit 3
    fi

    printf '新上游已合并。检查并测试该工作树后执行：\n'
    printf '%s finalize %s\n' "$0" "$update_dir"
    ;;

  finalize)
    [[ $# -eq 1 ]] || { usage; exit 2; }
    update_dir="$1"
    require_command git

    git -C "$update_dir" rev-parse --git-dir >/dev/null 2>&1 || die "不是更新工作树: $update_dir"
    git -C "$update_dir" rev-parse -q --verify MERGE_HEAD >/dev/null 2>&1 && \
      die "仍有未完成的 merge"
    [[ -z "$(git -C "$update_dir" status --porcelain)" ]] || die "更新工作树尚未提交干净"

    new_commit="$(git -C "$update_dir" config --get sub2api-plus.newUpstream || true)"
    [[ -n "$new_commit" ]] || die "更新工作树缺少新上游状态"
    git -C "$update_dir" merge-base --is-ancestor "$new_commit" HEAD || \
      die "更新结果未包含记录的新上游提交"

    "$SUB2API_PLUS_ROOT/scripts/export-overlay.sh" \
      "$update_dir" HEAD "$new_commit" --update-lock
    "$SUB2API_PLUS_ROOT/scripts/verify-overlay.sh" "$update_dir"
    printf '上游升级 Overlay 已重新导出并验证。提交前仍须运行完整测试和 systemd 二进制构建。\n'
    ;;

  *)
    usage
    exit 2
    ;;
esac
