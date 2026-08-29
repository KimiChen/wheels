#!/usr/bin/env python3
"""Regression tests for the audit Rust static guard."""

from __future__ import annotations

import importlib.util
import re
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

    def test_scans_complete_multiline_audit_block_inside_relay_function(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            service = root / "crates/shadowsocks-service/src"
            service.mkdir(parents=True)
            (service / "lib.rs").write_text(
                '#[cfg(all(feature = "user-audit", not(target_os = "linux")))]\n'
                'compile_error!("feature `user-audit` is supported on Linux only");\n',
                encoding="utf-8",
            )
            relay = service / "server/udprelay.rs"
            relay.parent.mkdir(parents=True)
            relay.write_text(
                'fn dispatch() {\n'
                '    #[cfg(feature = "user-audit")]\n'
                '    if let (Some(audit_emitter), Some(identity)) = (\n'
                '        emitter(),\n'
                '        identity(),\n'
                '    ) {\n'
                '        let padding = 0;\n'
                '        let value: Option<u8> = None;\n'
                '        value.unwrap();\n'
                '        let bytes: &[u8] = &[];\n'
                '        let _ = bytes[padding];\n'
                '    }\n'
                '}\n',
                encoding="utf-8",
            )
            findings = CHECKER.check(root, require_complete=False)
            self.assertTrue(any(":9: forbidden panic path in user-audit wiring" in item for item in findings))
            self.assertTrue(any(":11: direct index in user-audit wiring" in item for item in findings))


    SERVICE_LINUX_GATE = (
        '#[cfg(all(feature = "user-audit", not(target_os = "linux")))]\n'
        'compile_error!("feature `user-audit` is supported on Linux only");\n'
    )

    def check_service_source(self, relative: str, source: str) -> list[str]:
        """Check one service module inside an otherwise minimal prepared tree."""
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            service = root / "crates/shadowsocks-service/src"
            service.mkdir(parents=True)
            (service / "lib.rs").write_text(self.SERVICE_LINUX_GATE, encoding="utf-8")
            path = service / relative
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(source, encoding="utf-8")
            return CHECKER.check(root, require_complete=False)

    def test_scans_body_of_multiline_signature_relay_function(self) -> None:
        # §27 M-53 的原始复现：多行签名函数的审计范围一度只剩签名行。
        findings = self.check_service_source(
            "server/udprelay.rs",
            'impl UdpAssociation {\n'
            '    async fn dispatch_received_packet(\n'
            '        &mut self,\n'
            '        peer_addr: SocketAddr,\n'
            '        data: &[u8],\n'
            '    ) {\n'
            '        let injected: Option<u8> = None;\n'
            '        let _ = injected.unwrap();\n'
            '        let probe: [u8; 2] = [0; 2];\n'
            '        let _ = probe[7];\n'
            '        #[cfg(feature = "user-audit")]\n'
            '        if let Some(audit_emitter) = self.context.user_audit_emitter() {\n'
            '            audit_emitter.observe(peer_addr, data);\n'
            '        }\n'
            '    }\n'
            '}\n',
        )
        self.assertTrue(any(":8: forbidden panic path in user-audit wiring" in item for item in findings))
        self.assertTrue(any(":10: direct index in user-audit wiring" in item for item in findings))

    def test_every_module_scanner_is_wired_into_check(self) -> None:
        # §27 M-53 / §28 m-212 的死代码模式：定义了扫描器却从不被 check() 调用。
        source = MODULE_PATH.read_text(encoding="utf-8")
        defined = set(re.findall(r"^def (_check_[A-Za-z0-9_]+)\(", source, re.MULTILINE))
        body = source[source.index("def check(root: Path"):]
        called = set(re.findall(r"(_check_[A-Za-z0-9_]+)\(", body))
        self.assertEqual(defined - called, set())


    def test_rejects_additional_panic_and_abort_spellings(self) -> None:
        # m-216：assert 系列、unwrap_err/expect_err/unwrap_unchecked 与 process::abort。
        findings = self.check_source(
            """
fn checks(value: Result<u8, u8>, slice: &[u8]) {
    assert!(slice.is_empty());
    assert_eq!(slice.len(), 0);
    assert_ne!(slice.len(), 1);
    debug_assert!(slice.is_empty());
    debug_assert_eq!(slice.len(), 0);
    debug_assert_ne!(slice.len(), 1);
    let _ = value.unwrap_err();
    let _ = value.expect_err("must fail");
    let _ = unsafe { value.ok().unwrap_unchecked() };
    std::process::abort();
}
"""
        )
        self.assertEqual(
            sorted(int(item.rsplit(":", 2)[1]) for item in findings),
            [3, 4, 5, 6, 7, 8, 9, 10, 11, 12],
        )


if __name__ == "__main__":
    unittest.main()
