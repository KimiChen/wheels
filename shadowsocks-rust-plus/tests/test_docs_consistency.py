#!/usr/bin/env python3
"""Consistency guards between the shipped documentation and the implementation.

Every assertion pins a documented claim to the file that actually decides the
behaviour: `scripts/test.sh`, the Python tooling under `tests/`, or the added
lines of `patches/0003-user-audit.patch`.  Doc-only drift is the failure mode
these tests exist for, so each check must fail when either side moves alone.
"""

from __future__ import annotations

import re
import subprocess
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
TESTS = ROOT / "tests"
DOCS = ROOT / "docs"
AUDIT_PATCH = ROOT / "patches" / "0003-user-audit.patch"


def read(path: Path) -> str:
    return path.read_text(encoding="utf-8")


def flat(text: str) -> str:
    """Drop all whitespace so a prose assertion survives line rewrapping."""
    return re.sub(r"\s+", "", text)


def patch_added_lines() -> dict[str, list[str]]:
    """Map each post-image path to the lines the user-audit patch adds to it."""
    added: dict[str, list[str]] = {}
    current: list[str] | None = None
    for line in read(AUDIT_PATCH).splitlines():
        if line.startswith("diff --git "):
            current = added.setdefault(line.split(" b/", 1)[1], [])
        elif current is not None and line.startswith("+") and not line.startswith("+++"):
            current.append(line[1:])
    return added


class MockCollectorDocsTests(unittest.TestCase):
    REQUIRED_FLAGS = ("--socket", "--node", "--key-file", "--state")

    def test_documented_cli_contract_matches_argparse(self) -> None:
        source = read(TESTS / "mock_collector.py")
        for option in self.REQUIRED_FLAGS:
            declaration = re.search(rf'add_argument\("{option}"[^\n]*\)', source)
            self.assertIsNotNone(declaration, f"mock_collector.py 不再声明 {option}")
            self.assertIn("required=True", declaration.group(0), option)
        readme = flat(read(TESTS / "README.md"))
        self.assertFalse(
            flat("的幂等状态写入可选的 `0600` JSON 状态文件") in readme,
            "tests/README.md 仍把 --state 描述为可选",
        )
        self.assertIn(
            flat("`--socket`、`--node`、`--key-file` 和 `--state` 四个参数都是必填的"),
            readme,
        )


class TestScriptDocsTests(unittest.TestCase):
    """tests/README.md 对 scripts/test.sh 的描述必须与脚本实际分支一致。"""

    ALWAYS_HEADING = "`scripts/test.sh` 无条件执行（不受 `--no-integration` 影响）的检查是："
    INTEGRATION_HEADING = "只有未给出 `--no-integration` 时才追加执行的检查是："
    INTEGRATION_END = "`SHADOWSOCKS_RUN_FUZZ=1` 时还会追加调用"
    INTEGRATION_MARKER = 'if [[ "$run_integration" -eq 1 ]]; then'
    # 审计专用门禁：它们无条件运行，必须在测试文档里点名，否则等于没有交付说明。
    REQUIRED_AUDIT_GATES = (
        "check_audit_static.py",
        "test_check_audit_static.py",
        "test_fuzz_target.py",
        "test_panic_abort.py",
        "test_benchmark_audit.py",
        "test_integration_audit.py",
        "benchmark_audit.py",
        "test_docs_consistency.py",
    )

    def setUp(self) -> None:
        self.readme = read(TESTS / "README.md")
        self.script = read(ROOT / "scripts" / "test.sh")
        self.assertIn(self.INTEGRATION_MARKER, self.script)
        self.unconditional, self.integration_only = self.script.split(self.INTEGRATION_MARKER, 1)

    def documented(self, start: str, end: str) -> list[str]:
        self.assertIn(start, self.readme)
        head = self.readme.index(start)
        self.assertIn(end, self.readme[head:])
        tail = self.readme.index(end, head)
        names = re.findall(r"\]\(([A-Za-z0-9_]+\.py)\)", self.readme[head:tail])
        self.assertTrue(names, f"{start} 段落没有列出任何脚本")
        return names

    def test_documented_unconditional_checks_really_run_unconditionally(self) -> None:
        for name in self.documented(self.ALWAYS_HEADING, self.INTEGRATION_HEADING):
            self.assertIn(f"tests/{name}", self.unconditional, name)
            self.assertNotIn(f"tests/{name}", self.integration_only, name)

    def test_documented_integration_checks_are_really_integration_gated(self) -> None:
        for name in self.documented(self.INTEGRATION_HEADING, self.INTEGRATION_END):
            self.assertIn(f"tests/{name}", self.integration_only, name)
            self.assertNotIn(f"tests/{name}", self.unconditional, name)

    def test_audit_gate_scripts_are_named_in_the_test_docs(self) -> None:
        documented = set(self.documented(self.ALWAYS_HEADING, self.INTEGRATION_HEADING))
        for gate in self.REQUIRED_AUDIT_GATES:
            self.assertIn(gate, documented)

    def test_without_audit_switch_is_documented(self) -> None:
        self.assertIn("--without-audit", self.script)
        self.assertIn("--without-audit", self.readme)


class WorkspaceGateDocsTests(unittest.TestCase):
    """§16 的 Rust 门禁必须与 `scripts/test.sh` 真正会执行的命令逐条一致。

    早先这些断言是在 `scripts/test.sh` 的文本里 grep 字面量，于是把命令整行注释
    掉、加 `|| true` 吞掉失败、把数组声明留着却不传给 cargo，测试都照样全绿。改为
    向脚本本身索取门禁（`--print-gate`，与执行点同一份数据），再与 §16 做集合相等。
    """

    def setUp(self) -> None:
        self.script = read(ROOT / "scripts" / "test.sh")
        self.spec = read(DOCS / "USER_ACCESS_AUDIT.md")
        self.gate = self.printed_gate()

    def printed_gate(self, *arguments: str) -> list[tuple[str, str]]:
        result = subprocess.run(
            ["bash", str(ROOT / "scripts" / "test.sh"), "--print-gate", *arguments],
            capture_output=True,
            text=True,
            cwd=ROOT,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        rows = []
        for line in result.stdout.splitlines():
            scope, _, args = line.partition("\t")
            self.assertIn(scope, {"always", "linux-audit"}, line)
            self.assertTrue(args, line)
            rows.append((scope, args))
        self.assertTrue(rows, "--print-gate 没有输出任何门禁命令")
        return rows

    def spec_section(self) -> str:
        start = self.spec.index("## 16. 最终验收清单")
        return self.spec[start : self.spec.index("## 17.", start)]

    def test_the_spec_lists_exactly_the_commands_the_script_runs(self) -> None:
        """集合相等：多一条、少一条、改一个旗标都判红。"""

        documented = set()
        for block in re.findall(r"```text\n(.*?)```", self.spec_section(), re.DOTALL):
            for line in block.splitlines():
                line = line.strip()
                if line.startswith("cargo test"):
                    documented.add(line)
        executed = {f"cargo test {args}" for _, args in self.gate}
        self.assertEqual(
            documented,
            executed,
            "§16 声明的门禁命令与 scripts/test.sh 实际执行的不一致",
        )

    def test_the_loopback_targets_are_in_the_gate(self) -> None:
        """v8：三个纯 loopback 目标覆盖 overlay 改过的 UDP 数据面，不得随公网目标一并豁免。"""

        executed = [args for _, args in self.gate]
        for required in (
            "-p shadowsocks --test tcp_eih_user",
            "-p shadowsocks --test udp",
            "--test udp --features user-stats",
            "--test tunnel --features user-stats udp_tunnel",
        ):
            self.assertTrue(
                any(required in args for args in executed),
                f"门禁缺少 integration target：{required}",
            )

    def test_the_workspace_commands_stay_narrow_and_locked(self) -> None:
        workspace = [args for _, args in self.gate if "--workspace" in args]
        self.assertEqual(len(workspace), 2, "应当恰好两条 workspace 命令")
        for args in workspace:
            for flag in ("--lib", "--bins", "--locked", "--no-fail-fast"):
                self.assertIn(flag, args, args)
        features = {args.split("--features ")[1].split()[0] for args in workspace}
        self.assertEqual(features, {"user-stats", "user-audit"})
        for _, args in self.gate:
            self.assertIn("--locked", args, args)

    def test_the_feature_on_command_is_the_only_linux_gated_one(self) -> None:
        gated = [args for scope, args in self.gate if scope == "linux-audit"]
        self.assertEqual(len(gated), 1)
        self.assertIn("--features user-audit", gated[0])
        # `--without-audit` must drop it and nothing else.
        without = self.printed_gate("--without-audit")
        self.assertEqual(without, self.gate, "--print-gate 必须输出完整门禁与作用域，由执行点过滤")
        self.assertIn(
            'if [[ "$gate_scope" == "linux-audit" && ! ( "$run_audit" -eq 1 && "$audit_native" -eq 1 ) ]]',
            self.script,
            "linux-audit 作用域的守卫条件被改动了",
        )

    def test_the_gate_has_exactly_one_execution_point_and_cannot_swallow_failures(self) -> None:
        self.assertIn("set -euo pipefail", self.script)
        self.assertEqual(
            self.script.count("done < <(gate_commands)"),
            1,
            "门禁必须只有一个执行点",
        )
        code = "\n".join(
            line for line in self.script.splitlines() if not line.lstrip().startswith("#")
        )
        end = code.index("done < <(gate_commands)")
        loop = code[code.rindex("while IFS=", 0, end) : end]
        self.assertEqual(loop.count("cargo test"), 1, "门禁循环体里应当只有一条 cargo test")
        for swallow in ("|| true", "|| :"):
            self.assertFalse(swallow in loop, f"门禁不得用 {swallow} 吞掉失败")
        self.assertNotIn("|| true", self.script[: self.script.index("gate_commands() {")])

    def test_declared_spec_version_covers_the_v8_gate(self) -> None:
        declared = re.search(r"^> 规范版本：(\d+)$", self.spec, re.M)
        self.assertIsNotNone(declared)
        self.assertGreaterEqual(int(declared.group(1)), 8)

    def test_maintenance_docs_quote_only_real_gate_commands(self) -> None:
        """文档里引用的每条 cargo test 命令都必须真的在门禁里。"""

        executed = {f"cargo test {args}" for _, args in self.gate}
        for path in (TESTS / "README.md", ROOT / "patches" / "README.md"):
            quoted = {
                span
                # 单反引号跨度：不能与 ``` 围栏配错对。
                for span in re.findall(r"(?<!`)`([^`\n]+)`(?!`)", read(path))
                if span.startswith("cargo test")
            }
            self.assertTrue(quoted, f"{path} 没有引用任何门禁命令")
            self.assertTrue(
                quoted <= executed,
                f"{path} 引用了门禁里不存在的命令：{sorted(quoted - executed)}",
            )


class ProducerHealthDocsTests(unittest.TestCase):
    """producer health counter 的运维描述必须与其真实暴露面一致。"""

    SERVICE_MOD = "crates/shadowsocks-service/src/server/mod.rs"
    PRODUCER = "crates/shadowsocks-service/src/server/user_audit.rs"
    RUNTIME_WARNINGS = (
        "user audit sequence exhausted",
        "user audit ingest session unavailable",
    )

    def setUp(self) -> None:
        self.added = patch_added_lines()
        self.operations = read(DOCS / "OPERATIONS.md")

    def test_health_counters_are_only_read_by_the_final_shutdown_log(self) -> None:
        callers = {
            path
            for path, lines in self.added.items()
            if any("emitter.health_snapshot()" in line for line in lines)
        }
        self.assertEqual(callers, {self.SERVICE_MOD, self.PRODUCER})
        lines = self.added[self.SERVICE_MOD]
        hits = [index for index, line in enumerate(lines) if "emitter.health_snapshot()" in line]
        self.assertEqual(len(hits), 1, "mod.rs 出现了新的 health 读取点，文档需同步")
        owner = max(index for index, line in enumerate(lines[: hits[0]]) if line.startswith("fn "))
        self.assertIn("fn log_final_shutdown_skipped(", lines[owner])

    def test_operations_describes_shutdown_only_exposure(self) -> None:
        operations = flat(self.operations)
        self.assertIn(flat("没有运行期暴露面"), operations)
        self.assertIn(flat("user audit shutdown drain_completed="), operations)
        self.assertFalse(
            flat("`sequence_exhausted` 或 producer health counter 饱和") in operations,
            "OPERATIONS.md 仍把 producer health counter 列为可轮询的告警项",
        )
        for message in self.RUNTIME_WARNINGS:
            self.assertTrue(
                any(message in line for lines in self.added.values() for line in lines),
                message,
            )
            self.assertIn(flat(message), operations)


class AuditdFatalDocsTests(unittest.TestCase):
    """第六轮引入的 sticky durability fail-closed 必须有运维说明，且引用真实错误文本。"""

    SPOOL = "crates/shadowsocks-auditd/src/spool.rs"

    def durability_uncertain_message(self) -> str:
        lines = patch_added_lines()[self.SPOOL]
        for index, line in enumerate(lines):
            if line.strip() != "DurabilityUncertain," or index == 0:
                continue
            attribute = re.fullmatch(r'\s*#\[error\("([^"]+)"\)\]', lines[index - 1])
            if attribute is not None:
                return attribute.group(1)
        self.fail("补丁中找不到 SpoolError::DurabilityUncertain 的错误文本")

    def test_operations_documents_the_sticky_fatal_failure_mode(self) -> None:
        message = self.durability_uncertain_message()
        operations = flat(read(DOCS / "OPERATIONS.md"))
        self.assertIn(flat("### auditd durability fail-closed 退出"), operations)
        self.assertIn(flat(message), operations)
        for expected in ("storage_unavailable", "Restart=on-failure", "spool recovery"):
            self.assertIn(flat(expected), operations)


class PerformanceDocsTests(unittest.TestCase):
    """PERFORMANCE.md 的复测口径必须与发布门禁的 case 名一致。"""

    def test_data_path_case_ids_match_the_release_gate(self) -> None:
        gate = read(TESTS / "benchmark_audit.py")
        block = re.search(r"DATA_PATH_CASES = \(([^)]*)\)", gate)
        self.assertIsNotNone(block, "benchmark_audit.py 不再声明 DATA_PATH_CASES")
        cases = re.findall(r'"([a-z_]+)"', block.group(1))
        self.assertEqual(len(cases), 3, cases)
        performance = read(DOCS / "PERFORMANCE.md")
        for case in cases:
            self.assertIn(f"`{case}`", performance)
        self.assertFalse(
            flat("必须分别运行 feature-off 与 `--features user-audit` 构建") in flat(performance),
            "PERFORMANCE.md 仍写 feature-off/on 两案口径",
        )


class AuditIntermediaryDocsTests(unittest.TestCase):
    """§10.3 的中介细则必须在 OPERATIONS.md 里有可执行的落地写法。"""

    LOG_FORMAT = "log_format audit_export_min"
    USER_STATS_LOCATION = "location ~ ^/(?:v1/snapshot|healthz)$ {"

    def setUp(self) -> None:
        self.operations = read(DOCS / "OPERATIONS.md")

    def test_request_body_limit_matches_the_contract(self) -> None:
        spec = read(DOCS / "USER_ACCESS_AUDIT.md")
        limit = re.search(
            r"在转发前拒绝 query、百分号编码路径变体、非预期 method 和超过 (\d+) bytes 的 request body",
            spec,
        )
        self.assertIsNotNone(limit, "§10.3 的 body 上限条文已改写")
        self.assertIn(f"client_max_body_size {limit.group(1)};", self.operations)

    def test_endpoint_to_node_mapping_is_documented(self) -> None:
        self.assertIn(
            flat("每个对外 endpoint 只映射一个 `node_id`"),
            flat(self.operations),
        )

    def test_access_log_format_drops_authorization_and_mac_headers(self) -> None:
        self.assertIn(self.LOG_FORMAT, self.operations)
        self.assertIn("access_log /var/log/nginx/shadowsocks-audit-export.log audit_export_min;",
                      self.operations)
        declaration = re.search(rf"{self.LOG_FORMAT}(.*?);", self.operations, re.S)
        self.assertIsNotNone(declaration)
        rendered = declaration.group(1).lower()
        for forbidden in ("authorization", "x_shadowsocks_audit", "request_body", "resp_body"):
            self.assertNotIn(forbidden, rendered)

    def test_user_stats_nginx_block_is_marked_non_transferable(self) -> None:
        location = self.operations.index(self.USER_STATS_LOCATION)
        fence = self.operations.rindex("```nginx", 0, location)
        self.assertIn("不可照抄", self.operations[fence:location])


class SpecContractTests(unittest.TestCase):
    """§1–§16 合同正文与实际实现的三处对齐点，以及版本沿革链的自洽。"""

    def setUp(self) -> None:
        self.spec = read(DOCS / "USER_ACCESS_AUDIT.md")
        self.added = patch_added_lines()

    def section(self, heading: str) -> str:
        self.assertIn(heading, self.spec)
        start = self.spec.index(heading) + len(heading)
        rest = self.spec[start:]
        end = re.search(r"^#{2,3} ", rest, re.M)
        return rest[: end.start()] if end is not None else rest

    def feature_list(self, manifest: str) -> list[str]:
        joined = "\n".join(self.added[manifest])
        block = re.search(r"^user-audit = \[(.*?)^\]", joined, re.S | re.M)
        self.assertIsNotNone(block, f"{manifest} 中找不到 user-audit feature 定义")
        return re.findall(r'"([^"]+)"', block.group(1))

    def test_declared_version_matches_the_changelog_chain(self) -> None:
        declared = re.search(r"^> 规范版本：(\d+)$", self.spec, re.M)
        self.assertIsNotNone(declared)
        versions = re.findall(r"^> - v(\d+)（", self.spec, re.M)
        self.assertEqual(versions, [str(item) for item in range(2, int(declared.group(1)) + 1)])

    def test_cargo_feature_snippet_matches_the_manifests(self) -> None:
        section = self.section("### 5.1 Cargo feature")
        service = self.feature_list("crates/shadowsocks-service/Cargo.toml")
        self.assertIn("dep:hashbrown", service)
        for feature in service:
            self.assertIn(f'"{feature}"', section, feature)
        root = self.feature_list("Cargo.toml")
        self.assertNotIn("dep:hashbrown", root, "根 Cargo.toml 现在也声明了 hashbrown")
        for feature in root:
            self.assertIn(f'"{feature}"', section, feature)

    def test_quarantine_pending_reason_enum_matches_the_protocol(self) -> None:
        protocol = "\n".join(self.added["crates/shadowsocks-audit-protocol/src/lib.rs"])
        guard = re.search(
            r'self\.entry_type != "quarantine_pending".*?'
            r"matches!\(self\.reason\.as_str\(\), ([^)]+)\)",
            protocol,
            re.S,
        )
        self.assertIsNotNone(guard, "protocol 不再用 matches! 校验 quarantine reason")
        reasons = sorted(re.findall(r'"([a-z_]+)"', guard.group(1)))
        self.assertEqual(reasons, ["quarantine_eviction", "segment_corruption"])
        section = self.section("### 9.5 容量和循环覆盖")
        for reason in reasons:
            self.assertIn(f"`{reason}`", section)

    def test_backoff_reset_wording_matches_the_producer(self) -> None:
        producer = "\n".join(self.added["crates/shadowsocks-service/src/server/user_audit.rs"])
        self.assertIn("an event ACK resets reconnect backoff", producer)
        section = flat(self.section("### 7.3 AuditSupervisor 与 AuditClient session"))
        self.assertIn(flat("合法 event ACK 后立即重置"), section)
        self.assertFalse(
            flat("任一合法 ACK 后立即重置") in section,
            "§7.3 仍写“任一合法 ACK”",
        )


class AuditLedgerV2Tests(unittest.TestCase):
    """m-240: `USER_ACCESS_AUDIT_V2.md` 是这个项目唯一的问题溯源台账，却不被任何
    门禁读取。它开头声明的锚点前缀是一条可判真假的断言，实测已随下游提交漂移过一次
    而无人拦住。把台账里机器可判定的两类断言绑到源码事实上。
    """

    LEDGER = DOCS / "USER_ACCESS_AUDIT_V2.md"
    LOCK = ROOT / "upstream.lock"

    def test_declared_anchor_prefix_matches_upstream_lock(self) -> None:
        declared = re.search(
            r"`prepared_tree_sha256` 一致（`([0-9a-f]{8})…`", read(self.LEDGER)
        )
        self.assertIsNotNone(declared, "台账开头不再声明 prepared_tree_sha256 前缀")
        anchor = re.search(r"^prepared_tree_sha256=([0-9a-f]{64})$", read(self.LOCK), re.M)
        self.assertIsNotNone(anchor, "upstream.lock 不再有 prepared_tree_sha256")
        self.assertTrue(
            anchor.group(1).startswith(declared.group(1)),
            f"台账声明的锚点 {declared.group(1)}… 已与 upstream.lock 的 "
            f"{anchor.group(1)[:8]}… 漂移；改补丁的提交必须同时更新台账开头这一行",
        )

    def test_every_named_rust_function_in_the_ledger_exists(self) -> None:
        """台账用反引号引函数名做溯源锚点。写错一个（m-235 条目曾写成不存在的
        `accept_record_locked`）就让后来者按名搜索直接落空。"""
        haystack = "\n".join("\n".join(lines) for lines in patch_added_lines().values())
        names = set(re.findall(r"`([a-z_][a-z0-9_]*_locked)`", read(self.LEDGER)))
        self.assertTrue(names, "台账不再引用任何 `*_locked` 函数名")
        missing = sorted(n for n in names if f"fn {n}(" not in haystack)
        self.assertEqual(missing, [], f"台账引用了补丁中不存在的函数：{missing}")


    def test_every_commit_hash_in_the_ledger_still_resolves(self) -> None:
        """m-244: 台账按 7 位短哈希引用具体提交，而 rebase / filter-branch / amend
        都会让这些引用指向不存在的对象。已经踩过两次——2026-09-01 改 `Co-Authored-By`
        尾注时失效 15 处；更早 `ee6829b` 写下的一个哈希在某次改写后就一直悬空着没人
        发现。这条门禁把每个引用绑到「能解析且在当前历史上」这个事实上。
        """
        try:
            subprocess.run(
                ["git", "rev-parse", "--git-dir"],
                cwd=ROOT, check=True, capture_output=True,
            )
        except (OSError, subprocess.CalledProcessError):  # pragma: no cover - 非 git 检出
            self.skipTest("不在 git 仓库内，无法校验提交引用")

        broken: list[str] = []
        for short in sorted(set(re.findall(r"`([0-9a-f]{7})`", read(self.LEDGER)))):
            resolved = subprocess.run(
                ["git", "rev-parse", "--verify", "--quiet", f"{short}^{{commit}}"],
                cwd=ROOT, capture_output=True, text=True,
            )
            if resolved.returncode != 0:
                broken.append(f"{short}（无法解析）")
                continue
            reachable = subprocess.run(
                ["git", "merge-base", "--is-ancestor", short, "HEAD"],
                cwd=ROOT, capture_output=True,
            )
            if reachable.returncode != 0:
                broken.append(f"{short}（不在当前历史上，多半是历史改写后的悬空对象）")
        self.assertEqual(
            broken, [], f"台账引用的提交号已失效：{broken}；改写历史后须按 subject 逐条改回"
        )


if __name__ == "__main__":
    unittest.main()
