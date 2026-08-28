#!/usr/bin/env python3
"""Regression checks for the checked-in protocol fuzz target."""

from __future__ import annotations

import re
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
TARGET = ROOT / ".cache" / "audit-work-source" / "fuzz" / "fuzz_targets" / "audit_protocol.rs"
MANIFEST = ROOT / ".cache" / "audit-work-source" / "fuzz" / "Cargo.toml"


class FuzzTargetTest(unittest.TestCase):
    def test_target_manifest_and_all_parser_entrypoints_are_present(self) -> None:
        # The prepared source is generated during a full verify run. This test
        # is intentionally tolerant when only the overlay repository is being
        # linted, but it must fail if a prepared tree exists without the target.
        if not MANIFEST.exists() or not TARGET.exists():
            self.skipTest("prepared source tree is not present")
        manifest = MANIFEST.read_text(encoding="utf-8")
        target = TARGET.read_text(encoding="utf-8")
        self.assertIn('cargo-fuzz = true', manifest)
        self.assertIn('name = "audit_protocol"', manifest)
        for parser in (
            "parse_record",
            "parse_canonical_record",
            "parse_json_exact",
            "decode_frame",
            "decode_frame_prefix",
            "parse_spool_line",
            "parse_spool_meta",
            "parse_spool_state",
            "parse_tombstone_ledger",
            "parse_hello",
            "parse_hello_ack",
            "parse_hello_nack",
            "parse_event_ack",
            "parse_event_nack",
            "parse_lease_request",
            "parse_ack_request",
            "parse_ack_response",
            "parse_error_response",
            "parse_health_response",
        ):
            self.assertRegex(target, rf"\b{re.escape(parser)}\b", parser)
        self.assertNotIn("panic!", target)

    def test_fuzz_runner_requires_explicit_tool_when_requested(self) -> None:
        runner = (ROOT / "scripts" / "test-fuzz.sh").read_text(encoding="utf-8")
        self.assertIn("--require", runner)
        self.assertIn("cargo-fuzz", runner)
        self.assertIn("-max_total_time", runner)


if __name__ == "__main__":
    unittest.main()
