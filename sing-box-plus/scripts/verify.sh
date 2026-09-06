#!/usr/bin/env bash
set -euo pipefail

source "$(dirname "$0")/lib.sh"

require_command git
require_command go
require_command python3
require_command tar

cd "$SING_BOX_PLUS_ROOT"

# 0. 交付物存在性——缺一件就说明发布说明与实际产物已经对不上。
for required in \
  upstream.lock LICENSE THIRD_PARTY_NOTICES.md \
  config/server.example.json \
  packaging/sing-box-plus.service packaging/sing-box-plus.sysusers packaging/sing-box-plus.tmpfiles \
  tests/reference_collector.py tests/http_unix.py tests/settlement_model.py \
  scripts/user-stats-client.py scripts/quota-client.py \
  docs/API.md docs/ARCHITECTURE.md docs/OPERATIONS.md docs/UPSTREAM_BASELINE.md \
  docs/PERFORMANCE.md docs/ACCESS_AUDIT.md
do
  [[ -f "$required" ]] || die "缺少交付物：$required"
done

# 1. 静态检查。
for script_path in scripts/*.sh; do
  bash -n "$script_path"
done
python3 -m py_compile scripts/*.py tests/*.py
python3 -m json.tool config/server.example.json >/dev/null

gofmt_output="$(gofmt -l cmd internal)"
[[ -z "$gofmt_output" ]] || die "gofmt 未通过：$gofmt_output"

tags="$(production_build_tags)"
go vet -tags "$tags" ./...
go build -tags "$tags" ./...
# 零 tag 也必须编译通过：未启用统计时不得因缺符号而构建失败。
go build ./...

# 2. 上游锁定：远端 tag 不得漂移。
#
# 三种结局必须可区分，因为它们对「验证通过」这句话的支撑力度完全不同：
#   canonical —— 对着 upstream.lock 里的规范地址查，这是唯一能真正证明「上游没漂移」的一种；
#   mirror    —— 对着 UPSTREAM_REPOSITORY 指定的镜像查，只能证明镜像与 lock 一致；
#   skipped   —— 显式跳过，什么都没证明。
# 受限网络下的构建主机常常只能走后两种，脚本必须把这件事说出来，而不是让最后那句
# 「验证完成」照旧打印。
upstream_tag="$(lock_value tag)"
upstream_commit="$(lock_value commit)"
canonical_repository="$(lock_value repository)"
drift_repository="${UPSTREAM_REPOSITORY:-$canonical_repository}"
skip_drift="$(require_bool_env SING_BOX_PLUS_SKIP_UPSTREAM_DRIFT 0)"

if [[ "$skip_drift" == 1 ]]; then
  drift_scope="skipped"
  printf '警告：已跳过上游漂移检查（SING_BOX_PLUS_SKIP_UPSTREAM_DRIFT=1）。本次验证不证明上游未漂移。\n' >&2
else
  remote_commit="$(git ls-remote --tags "$drift_repository" "refs/tags/$upstream_tag" | awk 'NR == 1 { print $1 }')"
  [[ "$remote_commit" == "$upstream_commit" ]] || \
    die "远端 tag 已漂移或不可用（$drift_repository）：期望 $upstream_commit，实际 ${remote_commit:-<empty>}"
  if [[ "$drift_repository" == "$canonical_repository" ]]; then
    drift_scope="canonical"
  else
    drift_scope="mirror"
    printf '警告：漂移检查走的是镜像 %s，只证明镜像与 upstream.lock 一致，不证明上游未漂移。\n' \
      "$drift_repository" >&2
  fi
fi

# 3. 新准备一棵源码树，复算规范哈希与复制文件漂移。
temp_dir="$(mktemp -d "${TMPDIR:-/tmp}/sing-box-plus.XXXXXX")"
trap 'safe_remove_temp_dir "$temp_dir"' EXIT
source_dir="$temp_dir/source"
"$SING_BOX_PLUS_ROOT/scripts/prepare-source.sh" "$source_dir" >/dev/null

expected_tree="$(lock_value prepared_tree_sha256)"
[[ "$expected_tree" =~ ^[0-9a-f]{64}$ ]] || die "upstream.lock prepared_tree_sha256 格式错误"
actual_tree="$(python3 scripts/release-artifact.py source-tree-sha256 --source-root "$source_dir")"
[[ "$actual_tree" == "$expected_tree" ]] || \
  die "新准备源码树的 SHA-256 与 upstream.lock 不一致：$actual_tree"

python3 scripts/release-artifact.py copied-files-check \
  --source-root "$source_dir" \
  --overlay-root cmd/sing-box-plus \
  --lock cmd/sing-box-plus/copied-files.lock

# 4. 上游 AppendTracker 调用点普查（§9.2 第 8 条）。
#    本项目的 tracker 恒在包装链最外层这一前提，依赖「上游全部 AppendTracker 都在 box.New 体内」。
append_sites="$(grep -rn "AppendTracker" "$source_dir" --include='*.go' | grep -v "_test.go" | grep -vc "func (r \*Router) AppendTracker\|AppendTracker(tracker" || true)"
box_sites="$(grep -c "AppendTracker" "$source_dir/box.go" || true)"
[[ "$box_sites" == "2" ]] || \
  die "上游 box.go 的 AppendTracker 调用点不再是 2 处（实际 $box_sites）：§4.1 的最外层前提需重新验证"

# 5. 测试。三轮，缺一轮就会有一批断言从来没被执行过：
#    (a) 不带 -race 的全量——Vision 用例只能在这一轮跑（-race 会打开 checkptr，
#        而上游 sing-vmess 的 Vision 实现用 uintptr 运算读 crypto/tls 私有字段，
#        会被 checkptr 判为 fatal，见 docs/UPSTREAM_BASELINE.md）；
#    (b) 带抑制的 -race 全量；
#    (c) 不带抑制的纯单元用例，使 tests/race-suppressions.txt 无法掩盖本项目自身的竞争。
go test -count=1 -tags "$tags" ./...
GORACE="suppressions=$SING_BOX_PLUS_ROOT/tests/race-suppressions.txt" \
  go test -race -count=1 -tags "$tags" ./...
go test -race -count=1 -tags "$tags" \
  -run 'TestSaturatingCounter|TestQuotaCellWriteOrder|TestTrackerFailClose|TestTrackerPassthroughForUnbilledInbound|TestReconcileLineage|TestMaxIdentities|TestValidate|TestScanRawConfig|TestCheckReloadInvariant|TestAudit(StartupHardening|SuccessCriteria|SentinelSkipped|HostNormalization|QueueFullGap|WriteFailureResets|RepairTrailingNewline|RotationDetection|TotalCapNoSelfReferentialGap|CloseIsIdempotent)' \
  ./internal/userstats/

python3 -m unittest discover -s tests -p 'test_*.py' -v

# 6. 敏感信息扫描。
bash scripts/check-sensitive.sh

case "$drift_scope" in
  canonical)
    printf '验证完成：上游漂移检查（规范地址）、复制文件漂移门禁、上游注入点普查、构建、-race 测试与敏感信息扫描均通过。\n'
    ;;
  mirror)
    printf '验证完成（上游漂移检查仅对镜像 %s）：复制文件漂移门禁、上游注入点普查、构建、-race 测试与敏感信息扫描均通过；本次未对规范地址核对上游是否漂移。\n' \
      "$drift_repository"
    ;;
  skipped)
    printf '验证完成（未做上游漂移检查）：复制文件漂移门禁、上游注入点普查、构建、-race 测试与敏感信息扫描均通过；本次完全没有核对上游是否漂移。\n'
    ;;
esac
