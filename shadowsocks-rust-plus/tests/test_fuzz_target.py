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

# Every parser entrypoint the fuzz target has to drive. The check below is a
# semantic extraction of the calls that survive comment stripping inside the
# ``fuzz_target!`` body, so a commented-out entrypoint no longer satisfies the
# assertion by matching its own commented text.
FUZZED_PARSERS = (
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
)
CALL_PATTERN = re.compile(r"\bwire::([A-Za-z_][A-Za-z0-9_]*)\s*(?:::<[^>]*>\s*)?\(")


def strip_rust_comments(source: str) -> str:
    """Drop line and (nested) block comments, keeping string literals intact."""

    output: list[str] = []
    index = 0
    length = len(source)
    depth = 0
    while index < length:
        rest = source[index:]
        if depth:
            if rest.startswith("/*"):
                depth += 1
                index += 2
            elif rest.startswith("*/"):
                depth -= 1
                index += 2
            else:
                if source[index] == "\n":
                    output.append("\n")
                index += 1
            continue
        if rest.startswith("//"):
            end = source.find("\n", index)
            index = length if end < 0 else end
            continue
        if rest.startswith("/*"):
            depth = 1
            index += 2
            continue
        if source[index] == '"':
            output.append('"')
            index += 1
            while index < length:
                character = source[index]
                output.append(character)
                index += 1
                if character == "\\" and index < length:
                    output.append(source[index])
                    index += 1
                elif character == '"':
                    break
            continue
        output.append(source[index])
        index += 1
    return "".join(output)


def fuzz_target_body(source: str) -> str:
    """Return the balanced ``fuzz_target!( ... )`` body of a stripped target."""

    marker = "fuzz_target!("
    start = source.find(marker)
    if start < 0:
        raise AssertionError("fuzz target does not invoke fuzz_target!")
    cursor = start + len(marker)
    depth = 1
    while cursor < len(source) and depth:
        if source[cursor] == "(":
            depth += 1
        elif source[cursor] == ")":
            depth -= 1
        cursor += 1
    if depth:
        raise AssertionError("fuzz_target! body is not balanced")
    return source[start + len(marker) : cursor - 1]


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
        body = fuzz_target_body(strip_rust_comments(target))
        self.assertEqual(
            set(CALL_PATTERN.findall(body)),
            set(FUZZED_PARSERS),
            "the fuzz target must invoke exactly the tracked parser entrypoints",
        )
        self.assertNotIn("panic!", body)

    def test_commented_out_entrypoints_do_not_count_as_coverage(self) -> None:
        # The previous text match accepted a commented-out call, so 21 of the
        # 22 entrypoints could be disabled without a single test turning red.
        self.assertIsNotNone(SOURCE, "test requires an explicit prepared --source tree")
        assert SOURCE is not None
        target = (SOURCE / "fuzz" / "fuzz_targets" / "audit_protocol.rs").read_text(
            encoding="utf-8"
        )
        disabled = target.replace(
            "let _ = wire::parse_record(data);",
            "// let _ = wire::parse_record(data);",
        )
        self.assertNotEqual(disabled, target, "expected a plain parse_record call")
        body = fuzz_target_body(strip_rust_comments(disabled))
        self.assertNotIn("parse_record", set(CALL_PATTERN.findall(body)))

    def test_every_fuzzed_parser_exists_in_the_protocol_crate(self) -> None:
        self.assertIsNotNone(SOURCE, "test requires an explicit prepared --source tree")
        assert SOURCE is not None
        library = (
            SOURCE / "crates" / "shadowsocks-audit-protocol" / "src" / "lib.rs"
        ).read_text(encoding="utf-8")
        exported = set(re.findall(r"^pub fn ([A-Za-z_][A-Za-z0-9_]*)", library, re.MULTILINE))
        self.assertEqual(
            [name for name in FUZZED_PARSERS if name not in exported],
            [],
            "the fuzz entrypoint list names functions the protocol crate does not export",
        )

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
