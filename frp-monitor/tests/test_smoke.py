"""Checks for smoke harness failure handling, without requiring FRP binaries."""

from contextlib import redirect_stdout
import io
import os
from pathlib import Path
import stat
import sys
import tempfile
import unittest
from unittest import mock

import smoke


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


if __name__ == "__main__":
    unittest.main()
