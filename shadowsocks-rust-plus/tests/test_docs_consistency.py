#!/usr/bin/env python3
"""Consistency guards between the shipped documentation and the implementation.

Every assertion pins a documented claim to the file that actually decides the
behaviour: `scripts/test.sh`, the Python tooling under `tests/`, or the added
lines of `patches/0003-user-audit.patch`.  Doc-only drift is the failure mode
these tests exist for, so each check must fail when either side moves alone.
"""

from __future__ import annotations

import re
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


if __name__ == "__main__":
    unittest.main()
