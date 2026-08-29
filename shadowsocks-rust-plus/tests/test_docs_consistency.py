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


if __name__ == "__main__":
    unittest.main()
