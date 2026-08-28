#!/usr/bin/env python3

from __future__ import annotations

import json
import re
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parent.parent


class AuditPackagingTest(unittest.TestCase):
    @staticmethod
    def unit_values(unit: str, directive: str) -> list[list[str]]:
        values: list[list[str]] = []
        for line in unit.splitlines():
            if line.startswith(f"{directive}="):
                values.append(line.split("=", 1)[1].split())
        return values

    def test_server_sample_declares_audit_without_credentials(self) -> None:
        value = json.loads((ROOT / "config/server.example.json").read_text(encoding="utf-8"))
        audit = value["user_audit"]
        self.assertEqual(
            audit,
            {
                "ingest_socket_path": "/run/shadowsocks-audit/ingest/ingest.sock",
                "auditd_user": "shadowsocks-audit",
                "queue_capacity": 4096,
                "max_udp_targets_per_association": 256,
                "max_udp_target_windows": 65536,
            },
        )
        encoded = (ROOT / "config/server.example.json").read_text(encoding="utf-8")
        self.assertNotRegex(encoded, r"BEGIN (?:RSA |OPENSSH )?PRIVATE KEY")
        credential = r'"(?:password|key|[A-Za-z0-9_]+_key)"\s*:\s*"[A-Za-z0-9+/]{20,}={0,2}"'
        self.assertRegex('"password": "AAAAAAAAAAAAAAAAAAAAAA=="', credential)
        self.assertNotRegex(encoded, credential)

    def test_auditd_sample_matches_locked_defaults(self) -> None:
        value = json.loads((ROOT / "config/auditd.example.json").read_text(encoding="utf-8"))
        self.assertEqual(value["schema_version"], 1)
        self.assertEqual(value["max_spool_bytes"], 5 * 1024**3)
        self.assertEqual(value["min_free_bytes"], 1024**3)
        self.assertEqual(value["segment_max_bytes"], 4 * 1024**2)
        self.assertEqual(value["segment_max_age_seconds"], 60)
        self.assertNotIn("group_commit_max_events", value)
        self.assertNotIn("group_commit_max_delay_ms", value)
        self.assertEqual(value["export_max_response_bytes"], 8 * 1024**2)
        self.assertNotIn("key", value)

    def test_systemd_units_are_isolated_and_not_required_by_data_plane(self) -> None:
        auditd = (ROOT / "packaging/shadowsocks-auditd.service").read_text(encoding="utf-8")
        server = (ROOT / "packaging/shadowsocks-rust-plus.service").read_text(encoding="utf-8")
        for directive, expected in (
            ("User", [["shadowsocks-audit"]]),
            ("Group", [["shadowsocks-audit"]]),
            ("RestrictAddressFamilies", [["AF_UNIX"]]),
            ("NoNewPrivileges", [["true"]]),
            ("PrivateTmp", [["true"]]),
            ("ProtectSystem", [["strict"]]),
            ("ProtectHome", [["true"]]),
        ):
            self.assertEqual(self.unit_values(auditd, directive), expected)
        self.assertEqual(
            set(self.unit_values(auditd, "ReadWritePaths")[0]),
            {"/run/shadowsocks-audit", "/var/lib/shadowsocks-audit"},
        )
        self.assertEqual(
            set(self.unit_values(auditd, "InaccessiblePaths")[0]),
            {"-/etc/shadowsocks-rust-plus", "-/run/shadowsocks-rust-plus"},
        )
        self.assertIn("Wants=shadowsocks-auditd.service", server)
        self.assertIn("After=shadowsocks-auditd.service", server)
        self.assertNotIn("Requires=shadowsocks-auditd.service", server)
        self.assertIn("RuntimeDirectoryPreserve=yes", auditd)
        self.assertEqual(self.unit_values(server, "ReadWritePaths"), [["/run/shadowsocks-rust-plus"]])
        self.assertEqual(self.unit_values(auditd, "RestrictAddressFamilies"), [["AF_UNIX"]])

    def test_sysusers_and_tmpfiles_declare_private_boundaries(self) -> None:
        sysusers = (ROOT / "packaging/shadowsocks-auditd.sysusers").read_text(encoding="utf-8")
        tmpfiles = (ROOT / "packaging/shadowsocks-auditd.tmpfiles").read_text(encoding="utf-8")
        self.assertIn("u shadowsocks-audit", sysusers)
        self.assertIn("u shadowsocks -", sysusers)
        self.assertIn("g shadowsocks -", sysusers)
        self.assertIn("g shadowsocks-audit-ingest", sysusers)
        self.assertIn("g shadowsocks-audit-export", sysusers)
        self.assertIn("u audit-exporter -", sysusers)
        self.assertIn("m audit-exporter shadowsocks-audit-export", sysusers)
        self.assertIn("m shadowsocks shadowsocks-audit-ingest", sysusers)
        self.assertIn("m shadowsocks-audit shadowsocks-audit-ingest", sysusers)
        self.assertIn("m shadowsocks-audit shadowsocks-audit-export", sysusers)
        self.assertRegex(tmpfiles, r"d /var/lib/shadowsocks-audit 0700 shadowsocks-audit shadowsocks-audit")
        self.assertRegex(tmpfiles, r"d /run/shadowsocks-audit/ingest 0750 shadowsocks-audit shadowsocks-audit-ingest")
        self.assertRegex(tmpfiles, r"d /run/shadowsocks-audit/export 0750 shadowsocks-audit shadowsocks-audit-export")

    def test_shared_golden_vectors_are_present_and_canonical(self) -> None:
        vectors = json.loads((ROOT / "tests/golden_vectors.json").read_text(encoding="utf-8"))
        self.assertTrue({"hmac_key_hex", "request", "response", "records"}.issubset(vectors))
        self.assertEqual(len(bytes.fromhex(vectors["hmac_key_hex"])), 32)
        self.assertEqual(
            set(vectors["records"]),
            {
                "tcp_access",
                "udp_access",
                "producer_gap",
                "udp_window_contention",
                "spool_gap",
                "unicode_access",
                "escaping_access",
                "nullable_spool_gap",
            },
        )
        self.assertEqual(vectors["request"]["mac"], "c03af3fa5fab585d4f7edd738a4fba9755551d01502402486f0bafc3816659ab")
        self.assertEqual(vectors["response"]["mac"], "55ed61a4ecd614cc1d8a77ada41edb2393cf15f886453eecab888f868fd4b954")

    def test_permission_contract_is_explicit_in_templates_and_docs(self) -> None:
        tmpfiles = (ROOT / "packaging/shadowsocks-auditd.tmpfiles").read_text(encoding="utf-8")
        operations = (ROOT / "docs/OPERATIONS.md").read_text(encoding="utf-8")
        audit_patch = (ROOT / "patches/0003-user-audit.patch").read_text(encoding="utf-8")
        self.assertRegex(tmpfiles, r"d /etc/shadowsocks-audit 0750 root shadowsocks-audit")
        self.assertRegex(tmpfiles, r"d /run/shadowsocks-audit/(?:ingest|export) 0750 shadowsocks-audit shadowsocks-audit-(?:ingest|export)")
        self.assertRegex(audit_patch, r"(?m)^\+pub\(crate\) const SOCKET_MODE: u32 = 0o660;$")
        self.assertIn("socket_mode: \"0660\"", operations)
        self.assertIn("0640", operations)
        self.assertIn("0600", operations)
        self.assertIn("0700", operations)

    def test_audit_export_intermediary_contract_is_documented(self) -> None:
        operations = (ROOT / "docs/OPERATIONS.md").read_text(encoding="utf-8")
        api = (ROOT / "docs/API.md").read_text(encoding="utf-8")
        for text in (operations, api):
            self.assertIn("export_peer_user", text)
            self.assertIn("shadowsocks-audit-export", text)
            self.assertIn("SO_PEERCRED", text)
            self.assertIn("POST /v1/audit/lease", text)
            self.assertIn("POST /v1/audit/ack", text)
        self.assertIn("proxy_pass_request_body on", operations)
        self.assertIn("proxy_next_upstream off", operations)
        self.assertIn("X-Shadowsocks-Audit-Content-SHA256", operations)

    def test_non_linux_audit_check_prerequisite_is_explicit(self) -> None:
        readme = (ROOT / "README.md").read_text(encoding="utf-8")
        tests_readme = (ROOT / "tests/README.md").read_text(encoding="utf-8")
        patches_readme = (ROOT / "patches/README.md").read_text(encoding="utf-8")
        for text in (readme, tests_readme, patches_readme):
            self.assertIn("SHADOWSOCKS_AUDIT_CHECK_TARGET", text)
            self.assertIn("x86_64-unknown-linux-gnu", text)
        self.assertIn("rustup target add x86_64-unknown-linux-gnu", readme)
        self.assertIn("rustup target add x86_64-unknown-linux-gnu", tests_readme)


if __name__ == "__main__":
    unittest.main()
