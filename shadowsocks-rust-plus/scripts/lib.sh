#!/usr/bin/env bash
set -euo pipefail

readonly SHADOWSOCKS_RUST_PLUS_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"

die() {
  printf '错误：%s\n' "$*" >&2
  exit 1
}

# Switch environment variables are a three-state contract, not a truthiness
# test.  `${VAR:-default} == 1` collapses every unrecognised value onto the
# "off" branch, so a typo (`yes`, `true`, `2`) silently opts out of a gate the
# operator meant to keep -- the wrong direction for a fail-closed switch.
# Accept only `0` and `1`; reject anything else loudly.
require_bool_env() {
  local name="$1" default="$2" value
  value="${!name-}"
  [[ -n "$value" ]] || value="$default"
  case "$value" in
    0 | 1) printf '%s\n' "$value" ;;
    *) die "$name 只接受 0 或 1，实际：$value" ;;
  esac
}

# `.env` is an untracked developer convenience.  It must never be executed as
# shell (arbitrary code) and must never be able to introduce build inputs that
# the release receipt does not record.  Import a fixed key allowlist literally,
# and let release scripts opt out entirely with
# SHADOWSOCKS_RUST_PLUS_NO_DOTENV=1.
readonly SHADOWSOCKS_RUST_PLUS_DOTENV_KEYS=("UPSTREAM_REPOSITORY" "CARGO_HOME")

load_dotenv() {
  local file="$SHADOWSOCKS_RUST_PLUS_ROOT/.env"
  local no_dotenv

  # Validate the opt-out even when no `.env` exists.  Keep the assignment
  # outside a conditional command substitution so Bash cannot suppress the
  # helper's failure under `set -e`.
  no_dotenv="$(require_bool_env SHADOWSOCKS_RUST_PLUS_NO_DOTENV 0)" || return 1
  [[ -f "$file" ]] || return 0
  if [[ "$no_dotenv" == 1 ]]; then
    printf '忽略 .env：当前脚本要求可复现的环境输入。\n' >&2
    return 0
  fi
  local line key value allowed
  while IFS= read -r line || [[ -n "$line" ]]; do
    [[ -z "${line//[[:space:]]/}" || "$line" == \#* ]] && continue
    [[ "$line" =~ ^[A-Za-z_][A-Za-z0-9_]*= ]] || \
      die ".env 只接受 KEY=VALUE 行，不是 shell 脚本：$line"
    key="${line%%=*}"
    value="${line#*=}"
    allowed=0
    for candidate in "${SHADOWSOCKS_RUST_PLUS_DOTENV_KEYS[@]}"; do
      [[ "$key" == "$candidate" ]] && allowed=1 && break
    done
    [[ "$allowed" == 1 ]] || die ".env 不允许的键：$key（允许：${SHADOWSOCKS_RUST_PLUS_DOTENV_KEYS[*]}）"
    if [[ "$value" == \"*\" || "$value" == \'*\' ]]; then
      value="${value:1:${#value}-2}"
    fi
    export "$key=$value"
  done < "$file"
}

load_dotenv

write_test_coverage_status() {
  [[ $# -eq 6 ]] || die "测试覆盖面状态参数数量错误"
  local path="$1"
  local run_audit="$2"
  local run_integration="$3"
  local auditd_crate_checked="$4"
  local auditd_runtime_available="$5"
  local auditd_runtime_executed="$6"
  local value_name value directory temp_path coverage_complete

  for value_name in \
    run_audit run_integration auditd_crate_checked auditd_runtime_available \
    auditd_runtime_executed; do
    value="${!value_name}"
    case "$value" in
      0 | 1) ;;
      *) die "测试覆盖面状态不是 0/1：$value_name=$value" ;;
    esac
  done
  if [[ "$run_audit" == 0 && ( "$auditd_crate_checked" == 1 || \
        "$auditd_runtime_executed" == 1 ) ]]; then
    die "未请求 audit 时不能报告 auditd 检查已执行"
  fi
  if [[ "$auditd_runtime_executed" == 1 && \
        ( "$run_audit" != 1 || "$run_integration" != 1 || \
          "$auditd_runtime_available" != 1 ) ]]; then
    die "auditd runtime 执行状态与运行条件不一致"
  fi
  directory="$(dirname -- "$path")"
  [[ -d "$directory" ]] || die "测试覆盖面状态的父目录不存在：$directory"
  [[ ! -L "$path" ]] || die "测试覆盖面状态不能写入符号链接：$path"
  # `mv -f src dir` does not fail -- it moves `src` *into* `dir`.  Without this
  # check the writer reports success while leaving no file at `$path` at all,
  # and the caller (a release-facing conclusion) never learns.
  [[ ! -d "$path" ]] || die "测试覆盖面状态不能写入已存在的目录：$path"
  [[ ! -e "$path" || -f "$path" ]] || die "测试覆盖面状态必须是普通文件：$path"

  if [[ "$run_audit" == 1 && "$auditd_crate_checked" == 1 && \
        "$auditd_runtime_executed" == 1 ]]; then
    coverage_complete=1
  else
    coverage_complete=0
  fi

  temp_path="$(mktemp "$directory/.shadowsocks-rust-plus-test-coverage.XXXXXX")" || \
    die "无法创建测试覆盖面状态临时文件：$directory"
  if ! {
    printf '{\n'
    printf '  "schema_version": 1,\n'
    printf '  "run_audit": %s,\n' "$run_audit"
    printf '  "run_integration": %s,\n' "$run_integration"
    printf '  "auditd_crate_checked": %s,\n' "$auditd_crate_checked"
    printf '  "auditd_runtime_available": %s,\n' "$auditd_runtime_available"
    printf '  "auditd_runtime_executed": %s,\n' "$auditd_runtime_executed"
    printf '  "coverage_complete": %s\n' "$coverage_complete"
    printf '}\n'
  } > "$temp_path"; then
    rm -f -- "$temp_path"
    die "无法写入测试覆盖面状态：$path"
  fi
  if ! mv -f -- "$temp_path" "$path"; then
    rm -f -- "$temp_path"
    die "无法提交测试覆盖面状态：$path"
  fi
}

read_test_coverage_status() {
  [[ $# -eq 1 ]] || die "测试覆盖面状态参数数量错误"
  local path="$1"

  # Parse with Python's JSON decoder instead of sourcing a shell fragment.  The
  # status is an input to a release-facing conclusion and must be exact,
  # complete, and internally consistent.
  python3 - "$path" <<'PY'
import json
from pathlib import Path
import sys


def fail(message: str) -> None:
    print(f"错误：{message}", file=sys.stderr)
    raise SystemExit(1)


def strict_object(pairs):
    result = {}
    for key, item in pairs:
        if key in result:
            raise ValueError(f"重复字段：{key}")
        result[key] = item
    return result


path = Path(sys.argv[1])
if path.is_symlink() or not path.is_file():
    fail(f"测试覆盖面状态必须是普通文件：{path}")
try:
    payload = path.read_bytes()
except OSError as exc:
    fail(f"无法读取测试覆盖面状态：{exc}")
if len(payload) > 64 * 1024:
    fail("测试覆盖面状态超过大小上限")
try:
    value = json.loads(payload.decode("utf-8"), object_pairs_hook=strict_object)
except (UnicodeDecodeError, json.JSONDecodeError, ValueError) as exc:
    fail(f"测试覆盖面状态不是有效 JSON：{exc}")
if not isinstance(value, dict):
    fail("测试覆盖面状态顶层必须是对象")

required = {
    "schema_version",
    "run_audit",
    "run_integration",
    "auditd_crate_checked",
    "auditd_runtime_available",
    "auditd_runtime_executed",
    "coverage_complete",
}
if set(value) != required:
    fail("测试覆盖面状态字段集合错误")
if type(value["schema_version"]) is not int or value["schema_version"] != 1:
    fail("测试覆盖面状态 schema_version 错误")
for key in required - {"schema_version"}:
    if type(value[key]) is not int or value[key] not in (0, 1):
        fail(f"测试覆盖面状态字段不是 0/1：{key}")

if value["run_audit"] == 0 and (
    value["auditd_crate_checked"] == 1
    or value["auditd_runtime_executed"] == 1
):
    fail("未请求 audit 时却声称执行了 auditd 检查")
if value["auditd_runtime_executed"] == 1 and (
    value["run_audit"] != 1
    or value["run_integration"] != 1
    or value["auditd_runtime_available"] != 1
):
    fail("auditd runtime 执行状态与运行条件不一致")
expected = int(
    value["run_audit"] == 1
    and value["auditd_crate_checked"] == 1
    and value["auditd_runtime_executed"] == 1
)
if value["coverage_complete"] != expected:
    fail("测试覆盖面状态 coverage_complete 与明细不一致")
print(value["coverage_complete"])
PY
}

# The release-facing conclusion, derived only from what `test.sh` recorded.
# Kept here rather than inline in `verify.sh` so it can be exercised directly:
# inlined, the only way to reach it was a full prepare + build + test run, and
# the branch could be inverted without any test noticing.
report_verification_conclusion() {
  [[ $# -eq 1 ]] || die "验证结论参数数量错误"
  local coverage_complete
  coverage_complete="$(read_test_coverage_status "$1")" || \
    die "无法验证 scripts/test.sh 的测试覆盖面状态"
  if [[ "$coverage_complete" == 1 ]]; then
    printf '验证完成：锁定版本、零 fuzz 补丁重放、测试、auditd crate/runtime 覆盖与敏感信息扫描均通过。\n'
  else
    printf '验证完成（覆盖面不完整）：锁定版本、零 fuzz 补丁重放、测试与敏感信息扫描通过；scripts/test.sh 的实际状态未同时证明 auditd crate 编译和 Linux runtime 集成执行。\n'
  fi
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "缺少命令：$1"
}

lock_value() {
  local key="$1"
  local lock_file="$SHADOWSOCKS_RUST_PLUS_ROOT/upstream.lock"
  local value

  value="$(awk -F= -v key="$key" '$1 == key { sub(/^[^=]*=/, ""); print; found = 1 } END { if (!found) exit 1 }' "$lock_file")" || \
    die "upstream.lock 缺少字段：$key"
  [[ -n "$value" ]] || die "upstream.lock 字段为空：$key"
  printf '%s\n' "$value"
}

absolute_path() {
  local input_path="$1"
  local parent_path
  local base_name

  parent_path="$(dirname "$input_path")"
  base_name="$(basename "$input_path")"
  # Resolving a path must stay side-effect free: creating the parent here made
  # every rejected `--output` leave directories behind on the release host.
  [[ -d "$parent_path" ]] || die "路径的父目录不存在：$parent_path"
  parent_path="$(cd "$parent_path" && pwd -P)"
  printf '%s/%s\n' "$parent_path" "$base_name"
}

safe_remove_temp_dir() {
  local temp_path="$1"

  case "$temp_path" in
    /tmp/shadowsocks-rust-plus.*|"${TMPDIR:-/tmp}"/shadowsocks-rust-plus.*)
      rm -rf -- "$temp_path"
      ;;
    *)
      die "拒绝删除未识别的临时目录：$temp_path"
      ;;
  esac
}

require_clean_worktree() {
  local status

  status="$(git -C "$SHADOWSOCKS_RUST_PLUS_ROOT" status --porcelain --untracked-files=normal)"
  [[ -z "$status" ]] || die "发布签名与验签要求 overlay 工作树干净"
}
