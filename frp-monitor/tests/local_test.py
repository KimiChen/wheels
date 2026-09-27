"""Check credential initialization, generated configuration and path confinement."""
import hashlib
import importlib.util
import json
import os
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
            credentials = json.loads((folder / "credentials.json").read_text())[0]
            agent = tomllib.loads((folder / "agent.toml").read_text())
            server = tomllib.loads((folder / "server.toml").read_text())
            self.assertEqual(credentials["token_sha256"], hashlib.sha256(token.encode()).hexdigest())
            self.assertEqual(credentials["agent_id"], agent["clientID"])
            self.assertEqual(agent["telemetry"]["tokenFile"], str(folder / "agent.token"))
            self.assertEqual(server["monitor"]["credentialsFile"], str(folder / "credentials.json"))
            self.assertTrue(agent["telemetry"]["allowInsecureLoopback"])
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


if __name__ == "__main__":
    unittest.main()
