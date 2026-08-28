#!/usr/bin/env python3
"""Regression tests for the audit Rust static guard."""

from __future__ import annotations

import importlib.util
import tempfile
import unittest
from pathlib import Path


MODULE_PATH = Path(__file__).with_name("check_audit_static.py")
SPEC = importlib.util.spec_from_file_location("check_audit_static", MODULE_PATH)
assert SPEC is not None and SPEC.loader is not None
CHECKER = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(CHECKER)


class AuditStaticGuardTests(unittest.TestCase):
    def check_source(self, source: str) -> list[str]:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            path = root / "crates/shadowsocks-auditd/src/lib.rs"
            path.parent.mkdir(parents=True)
            path.write_text(source, encoding="utf-8")
            return CHECKER.check(root, require_complete=False)

    def test_accepts_safe_fixed_array_indexes(self) -> None:
        findings = self.check_source(
            """
fn frame(input: &[u8; 16]) {
    let mut header = [0_u8; 4];
    header[0] = input[15];
    let _ = &header[1..4];
}
"""
        )
        self.assertEqual(findings, [])

    def test_rejects_fixed_array_out_of_bounds_indexes(self) -> None:
        findings = self.check_source(
            """
fn frame(input: &[u8; 16]) {
    let header: [u8; 4] = [0; 4];
    let _ = input[16];
    let _ = &header[1..=4];
}
"""
        )
        self.assertEqual(len(findings), 2)

    def test_rejects_variable_indexes_and_slices(self) -> None:
        findings = self.check_source(
            """
fn frame(input: &[u8], index: usize, end: usize) {
    let _ = input[index];
    let _ = &input[index..end];
}
"""
        )
        self.assertEqual(len(findings), 2)

    def test_nearby_guard_cannot_hide_proven_out_of_bounds_index(self) -> None:
        findings = self.check_source(
            """
fn frame(input: &[u8; 4]) {
    if input.len() == 4 {
        let _ = input[4];
    }
}
"""
        )
        self.assertEqual(len(findings), 1)
        self.assertIn("direct index is not statically bounded", findings[0])

    def test_ignores_forbidden_spelling_in_comments_and_strings(self) -> None:
        findings = self.check_source(
            """
fn describe() -> &'static str {
    // Never call panic!(\"comment only\") or value.unwrap().
    \"expect( and unimplemented!( are documentation text\"
}
"""
        )
        self.assertEqual(findings, [])

    def test_rejects_all_explicit_panic_macros(self) -> None:
        findings = self.check_source(
            """
fn unfinished(value: Option<u8>) {
    let _ = value.expect(\"required\");
    unreachable!(\"bad\");
    todo!(\"later\");
    unimplemented!(\"later\");
}
"""
        )
        self.assertEqual(len(findings), 4)

    def test_skips_only_the_cfg_test_item(self) -> None:
        findings = self.check_source(
            """
#[cfg(test)]
fn fixture() {
    panic!("test-only { brace in string }");
}

fn production() {
    risky().expect("must be reported after the test item");
}
"""
        )
        self.assertEqual(len(findings), 1)
        self.assertIn(":8: forbidden panic path", findings[0])

    def test_requires_service_linux_feature_gate(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            service_lib = root / "crates/shadowsocks-service/src/lib.rs"
            service_lib.parent.mkdir(parents=True)
            service_lib.write_text(
                '#[cfg(all(feature = "user-audit", not(target_os = "linux")))]\n'
                'compile_error!("feature `user-audit` is supported on Linux only");\n',
                encoding="utf-8",
            )
            self.assertEqual(CHECKER.check(root, require_complete=False), [])

    def test_rejects_missing_service_linux_feature_gate(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            service_lib = root / "crates/shadowsocks-service/src/lib.rs"
            service_lib.parent.mkdir(parents=True)
            service_lib.write_text("pub fn run() {}\n", encoding="utf-8")
            findings = CHECKER.check(root, require_complete=False)
            self.assertEqual(len(findings), 1)
            self.assertIn("missing Linux-only user-audit compile gate", findings[0])

    def test_rejects_missing_and_empty_source_roots(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self.assertTrue(any("no audit production" in item for item in CHECKER.check(root)))
            missing = root / "missing"
            self.assertTrue(any("does not exist" in item for item in CHECKER.check(missing)))

    def test_rejects_partial_prepared_source_tree(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "Cargo.toml").write_text("[workspace]\n", encoding="utf-8")
            path = root / "crates/shadowsocks-auditd/src/lib.rs"
            path.parent.mkdir(parents=True)
            path.write_text("pub fn run() {}\n", encoding="utf-8")
            findings = CHECKER.check(root)
            self.assertTrue(any("required audit production source is missing" in item for item in findings))

    def test_scans_audit_relay_item_beyond_marker_line(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            service = root / "crates/shadowsocks-service/src"
            service.mkdir(parents=True)
            (service / "lib.rs").write_text(
                '#[cfg(all(feature = "user-audit", not(target_os = "linux")))]\n'
                'compile_error!("feature `user-audit` is supported on Linux only");\n',
                encoding="utf-8",
            )
            relay = service / "server/tcprelay.rs"
            relay.parent.mkdir(parents=True)
            relay.write_text(
                '#[cfg(feature = "user-audit")]\n'
                'fn relay() {\n'
                '    let value: Option<u8> = None;\n'
                '    value.unwrap();\n'
                '}\n',
                encoding="utf-8",
            )
            findings = CHECKER.check(root, require_complete=False)
            self.assertTrue(any(":4: forbidden panic path in user-audit wiring" in item for item in findings))


if __name__ == "__main__":
    unittest.main()
