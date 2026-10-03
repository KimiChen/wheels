"""Checks for smoke harness failure handling, without requiring FRP binaries."""

from contextlib import contextmanager, redirect_stdout
import io
import os
from pathlib import Path
import stat
import sys
import tempfile
import unittest
from unittest import mock

import smoke


ECHO_FIXTURE_PAYLOAD = b"smoke-helper\x00binary\xffpayload"


@contextmanager
def scripted_echo(chunks, clock=None):
    connection = mock.MagicMock()
    connection.__enter__.return_value = connection
    connection.recv.side_effect = chunks
    with mock.patch.object(smoke, "PAYLOAD", ECHO_FIXTURE_PAYLOAD), \
            mock.patch.object(smoke.socket, "create_connection", return_value=connection), \
            mock.patch.object(smoke.time, "monotonic", side_effect=clock, return_value=0):
        yield connection


class HarnessTests(unittest.TestCase):
    def test_child_does_not_inherit_proxy_or_secret_environment(self):
        with mock.patch.dict(os.environ, {
            "HTTPS_PROXY": "http://synthetic.invalid",
            "http_proxy": "http://synthetic.invalid",
            "FRP_TOKEN": "synthetic-secret",
            "GODEBUG": "synthetic-hook",
        }):
            environment = smoke.child_environment()
        for key in ("HTTPS_PROXY", "http_proxy", "FRP_TOKEN", "GODEBUG"):
            self.assertNotIn(key, environment)

    def test_configs_are_private_and_do_not_reuse_credentials(self):
        with tempfile.TemporaryDirectory() as name:
            root = Path(name)
            first, second = root / "one", root / "two"
            first.mkdir()
            second.mkdir()
            configs = smoke.make_configs(first, "v2", 1111, 2222, 3333)
            other = smoke.make_configs(second, "v2", 1111, 2222, 3333)
            for config in configs:
                if os.name == "posix":
                    self.assertEqual(stat.S_IMODE(config.stat().st_mode), 0o600)
            self.assertNotEqual(configs[0].read_text(), other[0].read_text())

    def test_wait_fails_on_early_exit_instead_of_accepting_stale_marker(self):
        with tempfile.TemporaryDirectory() as name:
            with smoke.child("synthetic process", [sys.executable, "-c", "raise SystemExit(7)"], Path(name)) as process:
                process.process.wait(timeout=5)
                with self.assertRaisesRegex(smoke.SmokeFailure, "code 7"):
                    smoke.wait_for("ready", lambda: True, (process,), 1)

    def test_timeout_is_a_failure_and_context_reaps_child(self):
        with tempfile.TemporaryDirectory() as name:
            with self.assertRaisesRegex(smoke.SmokeFailure, "timed out"):
                with smoke.child("synthetic process", [sys.executable, "-c", "import time; time.sleep(30)"], Path(name)) as process:
                    smoke.wait_for("never ready", lambda: False, (process,), 0.1)
            self.assertIsNotNone(process.process.poll())

    @unittest.skipUnless(os.name == "posix", "POSIX termination semantics")
    def test_hung_shutdown_escalates_and_child_output_is_not_printed(self):
        program = (
            "import signal, time; "
            "signal.signal(signal.SIGTERM, signal.SIG_IGN); "
            "print('synthetic-secret login to server success', flush=True); "
            "time.sleep(30)"
        )
        output = io.StringIO()
        with tempfile.TemporaryDirectory() as name, redirect_stdout(output):
            with smoke.child("synthetic process", [sys.executable, "-c", program], Path(name)) as process:
                smoke.wait_for("ready", process.logged_in.is_set, (process,), 5)
            self.assertTrue(process.forced_stop)
            self.assertIsNotNone(process.process.poll())
        self.assertEqual(output.getvalue(), "")

    def test_echo_checks_full_binary_payload(self):
        with smoke.echo_server() as port:
            self.assertTrue(smoke.echo_matches(port))

    def test_echo_complete_and_segmented_payloads_are_ready(self):
        payload = ECHO_FIXTURE_PAYLOAD
        for chunks in ([payload], [payload[:1], payload[1:7], payload[7:]]):
            with self.subTest(chunks=len(chunks)), scripted_echo(chunks) as connection:
                self.assertTrue(smoke.echo_matches(1))
                connection.sendall.assert_called_once_with(payload)
                self.assertEqual(connection.recv.call_count, len(chunks))

    def test_echo_empty_or_valid_prefix_eof_is_not_ready(self):
        for prefix in (b"", ECHO_FIXTURE_PAYLOAD[:1], ECHO_FIXTURE_PAYLOAD[:-1]):
            chunks = [prefix, b""] if prefix else [b""]
            with self.subTest(prefix_length=len(prefix)), scripted_echo(chunks):
                self.assertFalse(smoke.echo_matches(1))

    def test_echo_valid_prefix_deadline_is_not_ready(self):
        with scripted_echo([ECHO_FIXTURE_PAYLOAD[:5]], clock=[0, .1, 1.1]) as connection:
            self.assertFalse(smoke.echo_matches(1))
            self.assertEqual(connection.recv.call_count, 1)

    def test_echo_timeout_or_reset_after_valid_prefix_is_not_ready(self):
        for error in (TimeoutError(), ConnectionResetError()):
            with self.subTest(error=type(error).__name__), scripted_echo([ECHO_FIXTURE_PAYLOAD[:5], error]):
                self.assertFalse(smoke.echo_matches(1))

    def test_echo_altered_chunk_fails_before_later_network_error(self):
        for prefix in (b"", ECHO_FIXTURE_PAYLOAD[:5]):
            chunks = ([prefix] if prefix else []) + [b"incorrect", ConnectionResetError()]
            with self.subTest(prefix_length=len(prefix)), scripted_echo(chunks) as connection:
                with self.assertRaisesRegex(smoke.SmokeFailure, "altered payload"):
                    smoke.echo_matches(1)
                self.assertEqual(connection.recv.call_count, 2 if prefix else 1)

    def test_echo_full_length_altered_payload_is_fatal(self):
        payload = ECHO_FIXTURE_PAYLOAD
        with scripted_echo([payload[:-1] + bytes([payload[-1] ^ 1])]):
            with self.assertRaisesRegex(smoke.SmokeFailure, "altered payload"):
                smoke.echo_matches(1)

    def test_monitor_steady_traffic_does_not_retry_incomplete_echo(self):
        import monitor_smoke
        with scripted_echo([ECHO_FIXTURE_PAYLOAD[:5], b""]) as connection:
            with self.assertRaisesRegex(smoke.SmokeFailure, "native FRP forwarding failed"):
                monitor_smoke.exercise_tunnel(1, 1, ())
            self.assertEqual(connection.recv.call_count, 2)

    def test_bridge_can_interrupt_and_restart_the_same_endpoint(self):
        with smoke.echo_server() as port:
            bridge = smoke.Bridge(port)
            try:
                bridge.start()
                endpoint = bridge.port
                self.assertTrue(smoke.echo_matches(endpoint))
                bridge.stop()
                self.assertFalse(smoke.port_open(endpoint))
                bridge.start()
                self.assertEqual(bridge.port, endpoint)
                self.assertTrue(smoke.echo_matches(endpoint))
            finally:
                bridge.stop()

    def test_public_snapshot_supports_last_node_revocation(self):
        api = object.__new__(smoke.PublicAPI)
        with mock.patch.object(api, "get", return_value=(200, {"Cache-Control": "no-store"}, b'{"nodes":[]}')):
            self.assertEqual(api.snapshot()["nodes"], [])
            with self.assertRaises(smoke.SmokeFailure):
                api.node()
        for body in (b'[]', b'{}', b'{"nodes":null}'):
            with mock.patch.object(api, "get", return_value=(200, {"Cache-Control": "no-store"}, body)):
                with self.assertRaises(smoke.SmokeFailure):
                    api.snapshot()

    def test_redaction_rejects_nested_secrets_and_imprecise_uint64(self):
        for payload in ({"nested": [{"token": "hidden"}]}, {"note": "private-value"},
                        {"nodes": [{"metrics": {"mem_used": {"value": 9007199254740993}}}]}):
            with self.assertRaises(smoke.SmokeFailure):
                smoke.assert_redacted(payload, ("private-value",))
        smoke.assert_redacted({"nodes": [{"metrics": {"mem_used": {"value": "9007199254740993"}}}]}, ())


if __name__ == "__main__":
    unittest.main()
