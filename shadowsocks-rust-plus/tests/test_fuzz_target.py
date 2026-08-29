#!/usr/bin/env python3
"""Regression checks for the checked-in protocol fuzz target."""

from __future__ import annotations

import argparse
import re
import sys
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
SOURCE: Path | None = None


class FuzzTargetTest(unittest.TestCase):
    def test_target_manifest_and_all_parser_entrypoints_are_present(self) -> None:
        self.assertIsNotNone(SOURCE, "test requires an explicit prepared --source tree")
        assert SOURCE is not None
        manifest_path = SOURCE / "fuzz" / "Cargo.toml"
        target_path = SOURCE / "fuzz" / "fuzz_targets" / "audit_protocol.rs"
        self.assertTrue(manifest_path.is_file(), manifest_path)
        self.assertTrue(target_path.is_file(), target_path)
        manifest = manifest_path.read_text(encoding="utf-8")
        target = target_path.read_text(encoding="utf-8")
        embedded_vectors = SOURCE / "crates" / "shadowsocks-audit-protocol" / "src" / "golden_vectors.json"
        outer_vectors = ROOT / "tests" / "golden_vectors.json"
        self.assertTrue(embedded_vectors.is_file(), embedded_vectors)
        self.assertEqual(
            embedded_vectors.read_bytes(),
            outer_vectors.read_bytes(),
            "prepared protocol golden vectors drifted from the outer collector contract",
        )
        self.assertIn('cargo-fuzz = true', manifest)
        self.assertIn('name = "audit_protocol"', manifest)
        for parser in (
            "parse_record",
            "parse_canonical_record",
            "parse_json_exact",
            "parse_json_document",
            "parse_spool_record",
            "normalize_domain",
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
            self.assertRegex(
                target,
                rf"\bwire::{re.escape(parser)}(?:\s*::<[^>]+>)?\s*\(",
                f"{parser} must be invoked by the fuzz target",
            )
        self.assertNotIn("panic!", target)

    def test_fuzz_runner_requires_explicit_tool_when_requested(self) -> None:
        runner = (ROOT / "scripts" / "test-fuzz.sh").read_text(encoding="utf-8")
        self.assertIn("--require", runner)
        self.assertIn("cargo-fuzz", runner)
        self.assertIn("--release", runner)
        self.assertIn("--sanitizer address", runner)
        self.assertIn("-max_total_time", runner)


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--source", type=Path, required=True)
    arguments = parser.parse_args()
    SOURCE = arguments.source.resolve()
    unittest.main(argv=[sys.argv[0]])
