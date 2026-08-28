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
            return CHECKER.check(root)

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
            self.assertEqual(CHECKER.check(root), [])

    def test_rejects_missing_service_linux_feature_gate(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            service_lib = root / "crates/shadowsocks-service/src/lib.rs"
            service_lib.parent.mkdir(parents=True)
            service_lib.write_text("pub fn run() {}\n", encoding="utf-8")
            findings = CHECKER.check(root)
            self.assertEqual(len(findings), 1)
            self.assertIn("missing Linux-only user-audit compile gate", findings[0])


if __name__ == "__main__":
    unittest.main()
