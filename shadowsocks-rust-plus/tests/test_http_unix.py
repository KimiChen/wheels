#!/usr/bin/env python3

from __future__ import annotations

import importlib.util
import math
from pathlib import Path
import socket
import subprocess
import tempfile
import threading
import time
import unittest

from http_unix import receive_response, validate_snapshot


PROJECT_ROOT = Path(__file__).resolve().parents[1]
CLIENT_SPEC = importlib.util.spec_from_file_location(
    "user_stats_client", PROJECT_ROOT / "scripts" / "user-stats-client.py"
)
assert CLIENT_SPEC is not None and CLIENT_SPEC.loader is not None
USER_STATS_CLIENT = importlib.util.module_from_spec(CLIENT_SPEC)
CLIENT_SPEC.loader.exec_module(USER_STATS_CLIENT)


class HttpUnixResponseParserTest(unittest.TestCase):
    def parse(self, wire: bytes, max_body_bytes: int = 1024, *, close_write: bool = True):
        client, server = socket.socketpair()
        try:
            client.settimeout(0.1)
            server.sendall(wire)
            if close_write:
                server.shutdown(socket.SHUT_WR)
            return receive_response(client, max_body_bytes)
        finally:
            client.close()
            server.close()

    def test_content_length_framing(self) -> None:
        response = self.parse(
            b"HTTP/1.1 200 OK\r\n"
            b"Content-Type: application/json\r\n"
            b"Content-Length: 3\r\n\r\n{}\n",
            close_write=False,
        )
        self.assertEqual(response.status, 200)
        self.assertEqual(response.body, b"{}\n")

    def test_eof_framing(self) -> None:
        response = self.parse(b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n\r\n{}\n")
        self.assertEqual(response.body, b"{}\n")

    def test_chunked_framing(self) -> None:
        response = self.parse(
            b"HTTP/1.1 200 OK\r\n"
            b"Transfer-Encoding: chunked\r\n\r\n"
            b"1\r\n{\r\n2\r\n}\n\r\n0\r\n\r\n",
            close_write=False,
        )
        self.assertEqual(response.body, b"{}\n")

    def test_rejects_truncated_content_length(self) -> None:
        with self.assertRaisesRegex(RuntimeError, "Content-Length"):
            self.parse(b"HTTP/1.1 200 OK\r\nContent-Length: 4\r\n\r\n{}x")

    def test_rejects_body_over_limit(self) -> None:
        with self.assertRaisesRegex(RuntimeError, "allowed size"):
            self.parse(b"HTTP/1.1 200 OK\r\n\r\n12345", max_body_bytes=4)

    def test_rejects_conflicting_framing(self) -> None:
        with self.assertRaisesRegex(RuntimeError, "both Transfer-Encoding and Content-Length"):
            self.parse(
                b"HTTP/1.1 200 OK\r\n"
                b"Content-Length: 3\r\n"
                b"Transfer-Encoding: chunked\r\n\r\n"
                b"0\r\n\r\n"
            )

    def test_snapshot_schema_validation(self) -> None:
        snapshot = {
            "schema_version": 1,
            "node_id": "node-a",
            "runtime_id": "0123456789abcdef0123456789abcdef",
            "started_at_unix_ms": 1,
            "sequence": 1,
            "health": {"counter_overflow": False, "sequence_overflow": False},
            "servers": [
                {
                    "server_id": "server-a",
                    "listen": "127.0.0.1:8388",
                    "generation": 1,
                    "active": True,
                    "users": [
                        {
                            "identity_kind": "user",
                            "name": "user-a",
                            "generation": 1,
                            "active": True,
                            "tcp_uplink_bytes": 0,
                            "tcp_downlink_bytes": 0,
                            "udp_uplink_bytes": 0,
                            "udp_downlink_bytes": 0,
                        }
                    ],
                }
            ],
        }
        validate_snapshot(snapshot)
        snapshot["servers"][0]["users"][0]["tcp_uplink_bytes"] = True
        with self.assertRaisesRegex(RuntimeError, "traffic counter"):
            validate_snapshot(snapshot)


class StrictClientDeadlineTest(unittest.TestCase):
    def test_rejects_non_finite_timeout_before_connecting(self) -> None:
        for timeout in (math.nan, math.inf, -math.inf):
            with self.subTest(timeout=timeout):
                with self.assertRaisesRegex(RuntimeError, "finite and positive"):
                    USER_STATS_CLIENT.fetch("/path/that/must/not/be-opened", timeout, 1024)

    def test_slow_drip_response_cannot_extend_overall_deadline(self) -> None:
        with tempfile.TemporaryDirectory() as temporary_directory:
            socket_path = Path(temporary_directory) / "slow.sock"
            with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as listener:
                listener.bind(str(socket_path))
                listener.listen(1)

                def serve_slowly() -> None:
                    connection, _ = listener.accept()
                    with connection:
                        connection.recv(4096)
                        response = b"HTTP/1.1 200 OK\r\nContent-Length: 3\r\n\r\n{}\n"
                        for byte in response:
                            try:
                                connection.sendall(bytes((byte,)))
                            except OSError:
                                break
                            time.sleep(0.03)

                server = threading.Thread(target=serve_slowly, daemon=True)
                server.start()
                started = time.monotonic()
                with self.assertRaisesRegex(RuntimeError, "deadline exceeded"):
                    USER_STATS_CLIENT.fetch(str(socket_path), 0.15, 1024)
                elapsed = time.monotonic() - started
                self.assertLess(elapsed, 0.5)
                server.join(timeout=1)
                self.assertFalse(server.is_alive())


class SensitiveScanTest(unittest.TestCase):
    def run_scan(self, root: Path) -> subprocess.CompletedProcess[str]:
        return subprocess.run(
            ["bash", str(PROJECT_ROOT / "scripts" / "check-sensitive.sh"), str(root)],
            check=False,
            capture_output=True,
            text=True,
        )

    def test_match_reports_only_filename_without_secret_contents(self) -> None:
        with tempfile.TemporaryDirectory() as temporary_directory:
            root = Path(temporary_directory)
            secret_value = "DO_NOT_PRINT_THIS_FAKE_VALUE"
            fixture = root / "credential.txt"
            fixture.write_text(f"PrivateKey={secret_value}\n", encoding="utf-8")

            result = self.run_scan(root)
            output = result.stdout + result.stderr
            self.assertNotEqual(result.returncode, 0)
            self.assertTrue(str(fixture) in output, "scanner did not report the matching filename")
            self.assertTrue(secret_value not in output, "scanner exposed matched file contents")
            self.assertTrue("PrivateKey=" not in output, "scanner exposed the matching assignment")

    def test_patch_payload_credentials_are_scanned_without_echoing_them(self) -> None:
        with tempfile.TemporaryDirectory() as temporary_directory:
            root = Path(temporary_directory)
            secret_value = "PATCH_SECRET_VALUE_MUST_NOT_BE_PRINTED"
            fixture = root / "credential.patch"
            fixture.write_text(f"+PrivateKey={secret_value}\n", encoding="utf-8")

            result = self.run_scan(root)
            output = result.stdout + result.stderr
            self.assertNotEqual(result.returncode, 0)
            self.assertIn(str(fixture), output)
            self.assertNotIn(secret_value, output)
            self.assertNotIn("PrivateKey=", output)

    def test_hmac_and_psk_assignments_are_scanned_without_echoing_them(self) -> None:
        fixtures = {
            "hmac.json": '"export_hmac": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"\n',
            "psk.json": '"uPSK": "AAAAAAAAAAAAAAAAAAAAAA=="\n',
            "psk.patch": '+"iPSK": "AQEBAQEBAQEBAQEBAQEBAg=="\n',
        }
        with tempfile.TemporaryDirectory() as temporary_directory:
            root = Path(temporary_directory)
            for name, contents in fixtures.items():
                (root / name).write_text(contents, encoding="utf-8")

            result = self.run_scan(root)
            output = result.stdout + result.stderr
            self.assertNotEqual(result.returncode, 0)
            for name in fixtures:
                self.assertIn(str(root / name), output)
            self.assertNotIn("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", output)
            self.assertNotIn("AAAAAAAAAAAAAAAAAAAAAA==", output)
            self.assertNotIn("AQEBAQEBAQEBAQEBAQEBAg==", output)

    def test_prose_that_mentions_a_pem_marker_is_not_a_secret_record(self) -> None:
        with tempfile.TemporaryDirectory() as temporary_directory:
            root = Path(temporary_directory)
            fixture = root / "guidance.md"
            fixture.write_text(
                "不要提交 -----BEGIN PRIVATE KEY----- 这一标记或其后内容。\n",
                encoding="utf-8",
            )

            result = self.run_scan(root)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(result.stdout, "")
            self.assertEqual(result.stderr, "")

    def test_ignored_files_are_outside_the_precommit_scan_scope(self) -> None:
        with tempfile.TemporaryDirectory() as temporary_directory:
            root = Path(temporary_directory)
            (root / ".gitignore").write_text("deployment.env\n", encoding="utf-8")
            (root / "deployment.env").write_text("Passphrase=ignored-fixture\n", encoding="utf-8")

            result = self.run_scan(root)
            self.assertEqual(result.returncode, 0)
            self.assertTrue(len(result.stdout) == 0, "scanner unexpectedly wrote to stdout")
            self.assertTrue(len(result.stderr) == 0, "scanner unexpectedly wrote to stderr")


if __name__ == "__main__":
    unittest.main()
