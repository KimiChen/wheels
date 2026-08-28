#!/usr/bin/env python3

from __future__ import annotations

import json
import re
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parent.parent


class AuditPackagingTest(unittest.TestCase):
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
        self.assertNotRegex(encoded, r"(?:password|key)\s*:\s*\"[A-Za-z0-9+/]{20,}={0,2}\"")

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
        for directive in (
            "User=shadowsocks-audit",
            "Group=shadowsocks-audit",
            "RestrictAddressFamilies=AF_UNIX",
            "NoNewPrivileges=true",
            "PrivateTmp=true",
            "ProtectSystem=strict",
            "ProtectHome=true",
            "ReadWritePaths=/run/shadowsocks-audit /var/lib/shadowsocks-audit",
            "InaccessiblePaths=/etc/shadowsocks-rust-plus /run/shadowsocks-rust-plus",
        ):
            self.assertIn(directive, auditd)
        self.assertIn("Wants=shadowsocks-auditd.service", server)
        self.assertIn("After=shadowsocks-auditd.service", server)
        self.assertNotIn("Requires=shadowsocks-auditd.service", server)
        self.assertIn("RuntimeDirectoryPreserve=yes", auditd)
        self.assertNotIn("ReadWritePaths=/run/shadowsocks-rust-plus /run/shadowsocks-audit/ingest", server)
        self.assertNotIn("RestrictAddressFamilies=AF_INET", auditd)

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
            {"tcp_access", "udp_access", "producer_gap", "udp_window_contention", "spool_gap", "unicode_access"},
        )
        self.assertEqual(vectors["request"]["mac"], "c03af3fa5fab585d4f7edd738a4fba9755551d01502402486f0bafc3816659ab")
        self.assertEqual(vectors["response"]["mac"], "55ed61a4ecd614cc1d8a77ada41edb2393cf15f886453eecab888f868fd4b954")

    def test_permission_contract_is_explicit_in_templates_and_docs(self) -> None:
        tmpfiles = (ROOT / "packaging/shadowsocks-auditd.tmpfiles").read_text(encoding="utf-8")
        operations = (ROOT / "docs/OPERATIONS.md").read_text(encoding="utf-8")
        self.assertRegex(tmpfiles, r"d /etc/shadowsocks-audit 0750 root shadowsocks-audit")
        self.assertRegex(tmpfiles, r"d /run/shadowsocks-audit/(?:ingest|export) 0750 shadowsocks-audit shadowsocks-audit-(?:ingest|export)")
        for text in (operations,):
            self.assertIn("socket_mode: \"0660\"", text)
            self.assertIn("0640", text)
            self.assertIn("0600", text)
            self.assertIn("0700", text)


if __name__ == "__main__":
    unittest.main()
