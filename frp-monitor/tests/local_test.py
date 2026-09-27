"""Check credential initialization, generated configuration and path confinement."""
import hashlib
import importlib.util
import json
import os
import sqlite3
from contextlib import closing
from pathlib import Path
import tempfile
import tomllib
import unittest
from unittest import mock

SPEC = importlib.util.spec_from_file_location("local_demo", Path(__file__).resolve().parents[1] / "scripts/local.py")
local = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(local)


class LocalTests(unittest.TestCase):
    def test_private_initialization_is_consistent_and_does_not_overwrite(self):
        with tempfile.TemporaryDirectory() as root:
            folder = Path(root) / "demo"
            config = local.settings(Path(root), {})
            local.initialize(folder, plain_http=True, config=config)
            token = (folder / "agent.token").read_text().strip()
            with closing(sqlite3.connect(folder / "control.sqlite")) as database:
                database.row_factory = sqlite3.Row
                credentials = dict(database.execute("SELECT * FROM nodes").fetchone())
                self.assertEqual(database.execute("PRAGMA user_version").fetchone()[0], 4)
            agent = tomllib.loads((folder / "agent.toml").read_text())
            server = tomllib.loads((folder / "server.toml").read_text())
            self.assertEqual(credentials["token_sha256"], hashlib.sha256(token.encode()).hexdigest())
            self.assertEqual(str(credentials["id"]), agent["clientID"])
            self.assertEqual(agent["telemetry"]["tokenFile"], str(folder / "agent.token"))
            self.assertEqual(credentials["id"], 1)
            self.assertNotIn("credentialsFile", server["monitor"])
            self.assertEqual(server["monitor"]["githubAdminUsers"], [])
            self.assertEqual(server["monitor"]["historyDataPath"], "")
            self.assertNotIn("historyEnabled", server["monitor"])
            self.assertFalse((folder / "history").exists())
            for old in ("admin.token", "admin.json", "credentials.json", "probes.json"):
                self.assertFalse((folder / old).exists())
            self.assertTrue(agent["telemetry"]["allowInsecureLoopback"])
            self.assertFalse(agent["telemetry"]["probeEnabled"])
            self.assertEqual(server["monitor"]["retentionDays"], 7)
            self.assertEqual(server["monitor"]["databaseFile"], str(folder / "control.sqlite"))
            self.assertNotIn(token, (folder / "agent.toml").read_text())
            self.assertNotEqual(token, (folder / "frp.token").read_text().strip())
            self.assertEqual(folder.stat().st_mode & 0o777, 0o700)
            for path in folder.iterdir():
                self.assertEqual(path.stat().st_mode & 0o777, 0o600)
            with self.assertRaises(ValueError):
                local.initialize(folder, plain_http=True, config=config)
            self.assertEqual(token, (folder / "agent.token").read_text().strip())

    def test_env_is_literal_allowlisted_and_process_values_win(self):
        with tempfile.TemporaryDirectory() as root:
            root = Path(root)
            (root / ".env").write_text('PRIVATE_KEY=$(never)\nFRP_AGENT_NAME="测试节点"\nFRP_MONITOR_PORT=17402\n')
            config = local.settings(root, {"FRP_MONITOR_PORT": "17403"})
            self.assertEqual(config["FRP_MONITOR_PORT"], 17403)
            self.assertEqual(config["FRP_AGENT_NAME"], "测试节点")
            self.assertNotIn("PRIVATE_KEY", config)
            with self.assertRaises(ValueError):
                local.settings(root, {"FRP_AGENT_NAME": "$(not-executed)"})

    def test_probe_initialization_requires_explicit_opt_in(self):
        with tempfile.TemporaryDirectory() as root:
            root = Path(root)
            folder = root / "demo"
            local.initialize(folder, plain_http=True, probes=True, config=local.settings(root, {}))
            config = tomllib.loads((folder / "agent.toml").read_text())
            self.assertTrue(config["telemetry"]["probeEnabled"])
            self.assertTrue(config["telemetry"]["probeAllowPrivate"])
            with closing(sqlite3.connect(folder / "control.sqlite")) as database:
                document = json.loads(database.execute("SELECT probe_json FROM settings").fetchone()[0])
            tasks = document["nodes"][0]["tasks"]
            self.assertEqual(document["nodes"][0]["agent_id"], "1")
            self.assertEqual(tasks[0]["target"], "127.0.0.1:17000")

    def test_github_login_copies_secret_and_uses_allowlist_without_token_fallback(self):
        with tempfile.TemporaryDirectory() as root:
            root = Path(root).resolve()
            secret = root / "oauth🛰secret"
            local.private(secret, "example-client-secret\n")
            env = {"FRP_GITHUB_CLIENT_ID": "example-client-id", "FRP_GITHUB_CLIENT_SECRET_FILE": str(secret),
                   "FRP_GITHUB_CALLBACK_URL": "https://monitor.example.invalid/api/admin/v1/auth/github/callback", "FRP_GITHUB_ADMIN_USERS": "ExampleAdmin,second-admin,ExampleAdmin", "FRP_MONITOR_HISTORY_DATA_PATH": "history🛰", "FRP_MONITOR_RETENTION_DAYS": "365", "FRP_AGENT_IFACE": "eth🛰"}
            config = local.settings(root, env)
            folder = root / "demo🛰"
            local.initialize(folder, plain_http=True, config=config)
            content = (folder / "server.toml").read_text()
            monitor = tomllib.loads(content)["monitor"]
            self.assertEqual(monitor["githubAdminUsers"], ["exampleadmin", "second-admin"])
            self.assertEqual(monitor["githubClientSecretFile"], str(folder / "github.secret"))
            self.assertEqual((folder / "github.secret").stat().st_mode & 0o777, 0o600)
            self.assertNotIn("example-client-secret", content)
            self.assertEqual(monitor["historyDataPath"], str(folder / "history🛰"))
            self.assertFalse((folder / "history🛰").exists())
            agent = tomllib.loads((folder / "agent.toml").read_text())
            self.assertEqual(agent["telemetry"]["iface"], "eth🛰")
            self.assertEqual(agent["auth"]["tokenSource"]["file"]["path"], str(folder / "frp.token"))
            for changed in ({"FRP_GITHUB_CLIENT_ID": ""}, {"FRP_GITHUB_ADMIN_USERS": "bad login"}, {"FRP_GITHUB_CALLBACK_URL": "http://example.invalid/callback"}, {"FRP_MONITOR_HISTORY_DATA_PATH": "../outside"}):
                with self.subTest(changed=changed), self.assertRaises(ValueError):
                    local.settings(root, {**env, **changed})

    def test_history_path_is_optional_and_confined_to_private_runtime(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            destination = root / "demo"
            self.assertEqual(local._history_directory("", destination), "")
            self.assertEqual(local._history_directory("history", destination), str(destination / "history"))
            self.assertEqual(local._history_directory(str(destination / "metrics/history"), destination), str(destination / "metrics/history"))
            for name in ("..", "../outside", ".", str(root / "outside"), "control.sqlite", "agent.token/cache"):
                with self.subTest(name=name), self.assertRaises(ValueError):
                    local._history_directory(name, destination)
            destination.mkdir(mode=0o700)
            (destination / "linked").symlink_to(root, target_is_directory=True)
            (destination / "public").mkdir(mode=0o755)
            (destination / "file").write_text("not a directory")
            for name in ("linked/cache", "public", "file"):
                with self.subTest(name=name), self.assertRaises(ValueError):
                    local._history_directory(name, destination)

    def test_github_secret_matches_server_byte_limits_and_whitespace_rules(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            source = root / "oauth-secret"
            config = {**local.settings(root, {}), "FRP_GITHUB_CLIENT_SECRET_FILE": str(source)}
            cases = [("a" * 8, True), ("a" * 256, True), ("密钥AB", True), ("  abcdefgh\n", True),
                     ("a" * 7, False), ("a" * 257, False), ("密" * 86, False),
                     ("abcd efgh", False), ("abcd\tefgh", False), ("abcd\nefgh", False), ("abcd\0efgh", False)]
            for index, (value, valid) in enumerate(cases):
                with self.subTest(value_length=len(value.encode()), valid=valid):
                    source.write_text(value, encoding="utf-8")
                    source.chmod(0o600)
                    staging = root / str(index)
                    staging.mkdir(mode=0o700)
                    if valid:
                        local.github_config(config, staging, staging)
                        self.assertEqual((staging / "github.secret").read_text(), value.strip() + "\n")
                    else:
                        with self.assertRaises(ValueError):
                            local.github_config(config, staging, staging)
                        self.assertFalse((staging / "github.secret").exists())

    def test_private_reader_rejects_oversize_public_and_linked_inputs(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            source = root / "input"
            local.private(source, "12345678")
            self.assertEqual(local.read_private(source, 8), b"12345678")
            with self.assertRaises(ValueError):
                local.read_private(source, 7)
            source.chmod(0o644)
            with self.assertRaises(ValueError):
                local.read_private(source)
            source.chmod(0o600)
            (root / "linked").symlink_to(source)
            (root / "parent").symlink_to(root, target_is_directory=True)
            for path in (root / "linked", root / "parent/input", root):
                with self.subTest(path=path), self.assertRaises(ValueError):
                    local.read_private(path)

    def test_paths_stay_in_private_data_and_symlinks_rejected(self):
        with tempfile.TemporaryDirectory() as root, mock.patch.object(local, "ROOT", Path(root)):
            (Path(root) / "data").mkdir()
            (Path(root) / "data" / "link").symlink_to(Path(root), target_is_directory=True)
            for name in ("../outside", "data/../other", "data", "/tmp/outside", "data/link/demo"):
                with self.subTest(name=name), self.assertRaises(ValueError):
                    local.directory(name)
            self.assertEqual(local.directory("data/demo"), Path(root) / "data/demo")

    def test_tls_generation_failure_removes_only_its_staging_directory(self):
        with tempfile.TemporaryDirectory() as root:
            root = Path(root)
            keep = root / "keep"
            keep.write_text("existing")
            with mock.patch.object(local.subprocess, "run", return_value=mock.Mock(returncode=1)):
                with self.assertRaises(ValueError):
                    local.initialize(root / "demo", config=local.settings(root, {}))
            self.assertEqual(list(root.iterdir()), [keep])

    def test_run_demo_verifies_with_timeout_and_survives_cleanup_race(self):
        with tempfile.TemporaryDirectory() as root:
            root = Path(root)
            folder = root / "demo"
            folder.mkdir()
            (folder / "local.json").write_text(json.dumps({"url": "https://127.0.0.1:17401/", "id": "1"}) + "\n")
            binaries = []
            for name in ("server", "agent"):
                binary = root / name
                binary.write_text("#!/bin/sh\n")
                binary.chmod(0o755)
                binaries.append(binary)
            runs = []

            class Raced:
                """Exited in the main loop, then again gone when the cleanup signals it."""
                pid = 424242

                def __init__(self):
                    self.polls = 0

                def poll(self):
                    self.polls += 1
                    return 1 if self.polls == 1 else None

                def wait(self, timeout=None):
                    return 1

            def fake_run(*args, **kwargs):
                runs.append(kwargs)
                return mock.Mock(returncode=0)

            killpg = mock.Mock(side_effect=ProcessLookupError)
            with mock.patch.object(local, "native_binaries", return_value=tuple(binaries)), \
                    mock.patch.object(local.subprocess, "run", side_effect=fake_run), \
                    mock.patch.object(local.subprocess, "Popen", side_effect=lambda *a, **k: Raced()), \
                    mock.patch.object(local.os, "killpg", killpg), \
                    mock.patch("builtins.print"):
                with self.assertRaises(ValueError):
                    local.run_demo(folder)
            self.assertEqual(len(runs), 2)
            self.assertTrue(all(call.get("timeout") == 30 for call in runs))
            killpg.assert_called()

    def test_run_demo_verify_timeout_is_an_initialization_error(self):
        with tempfile.TemporaryDirectory() as root:
            root = Path(root)
            folder = root / "demo"
            folder.mkdir()
            binary = root / "server"
            binary.write_text("#!/bin/sh\n")
            binary.chmod(0o755)
            with mock.patch.object(local, "native_binaries", return_value=(binary, binary)), \
                    mock.patch.object(local.subprocess, "run", side_effect=local.subprocess.TimeoutExpired("verify", 30)), \
                    mock.patch("builtins.print"):
                with self.assertRaises(ValueError):
                    local.run_demo(folder)


if __name__ == "__main__":
    unittest.main()
