#!/usr/bin/env python3

from __future__ import annotations

import base64
import json
import os
import stat
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path


PROJECT_ROOT = Path(__file__).resolve().parent.parent
TOOL = PROJECT_ROOT / "scripts" / "cluster-users.py"


class ClusterUsersToolTest(unittest.TestCase):
    def run_tool(self, *arguments: str, success: bool = True) -> subprocess.CompletedProcess[str]:
        result = subprocess.run(
            [sys.executable, str(TOOL), *arguments],
            cwd=PROJECT_ROOT,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
        )
        if success and result.returncode != 0:
            self.fail(f"tool failed: {result.stderr}")
        if not success and result.returncode == 0:
            self.fail(f"tool unexpectedly succeeded: {result.stdout}")
        return result

    def generate(
        self, directory: Path, *, formal_count: int = 205, test_count: int = 4
    ) -> tuple[Path, dict[str, object]]:
        output = directory / "cluster-users.json"
        result = self.run_tool(
            "generate",
            "--output",
            str(output),
            "--formal-count",
            str(formal_count),
            "--test-count",
            str(test_count),
        )
        payload = json.loads(output.read_text(encoding="utf-8"))
        self.assertEqual(stat.S_IMODE(output.stat().st_mode), 0o600)
        self.assertNotIn(payload["shared_i_psk"], result.stdout)
        for user in payload["users"]:
            self.assertNotIn(user["password"], result.stdout)
        return output, payload

    def write_five_configs(self, directory: Path, payload: dict[str, object]) -> list[Path]:
        configs: list[Path] = []
        for index in range(5):
            config = {
                "user_stats": {
                    "node_id": f"node-{index + 1:02d}",
                    "socket_path": "/run/shadowsocks-rust-plus/user-stats.sock",
                },
                "servers": [
                    {
                        "id": f"service-{index + 1:02d}",
                        "server": "0.0.0.0",
                        "server_port": 19999,
                        "method": payload["method"],
                        "password": payload["shared_i_psk"],
                        "mode": "tcp_and_udp",
                        "users": [
                            {"name": user["name"], "password": user["password"]}
                            for user in payload["users"]
                        ],
                    }
                ],
            }
            path = directory / f"node-{index + 1}.json"
            path.write_text(json.dumps(config), encoding="utf-8")
            os.chmod(path, 0o600)
            configs.append(path)
        return configs

    def verify_args(
        self, source: Path, configs: list[Path], formal_count: int, test_count: int = 4
    ) -> list[str]:
        arguments = [
            "verify-five",
            "--source",
            str(source),
            "--expected-formal-users",
            str(formal_count),
            "--expected-test-users",
            str(test_count),
        ]
        for config in configs:
            arguments.extend(("--config", str(config)))
        return arguments

    def test_generate_205_formal_plus_test_users_is_private_unique_and_valid(self) -> None:
        with tempfile.TemporaryDirectory(prefix="ssrp-users-") as temporary:
            source, payload = self.generate(Path(temporary), formal_count=205, test_count=4)
            self.assertEqual(payload["schema_version"], 2)
            self.assertEqual(payload["method"], "2022-blake3-aes-128-gcm")
            self.assertEqual(len(payload["users"]), 209)
            self.assertEqual(
                sum(user["kind"] == "formal" for user in payload["users"]), 205
            )
            self.assertEqual(sum(user["kind"] == "test" for user in payload["users"]), 4)
            names = [user["name"] for user in payload["users"]]
            passwords = [user["password"] for user in payload["users"]]
            self.assertEqual(len(base64.b64decode(payload["shared_i_psk"], validate=True)), 16)
            for password in passwords:
                self.assertEqual(len(base64.b64decode(password, validate=True)), 16)
            self.assertEqual(names[0], "u_000001")
            self.assertEqual(names[204], "u_000205")
            self.assertEqual(names[205], "test_000001")
            self.assertEqual(names[-1], "test_000004")
            self.assertEqual(len(names), len(set(names)))
            self.assertEqual(len(passwords), len(set(passwords)))
            self.assertNotIn(payload["shared_i_psk"], passwords)
            self.assertTrue(source.exists())

    def test_refuses_overwrite_without_changing_existing_file(self) -> None:
        with tempfile.TemporaryDirectory(prefix="ssrp-users-") as temporary:
            directory = Path(temporary)
            source, _ = self.generate(directory, formal_count=200)
            original = source.read_bytes()
            result = self.run_tool(
                "generate", "--output", str(source), "--count", "200", success=False
            )
            self.assertIn("拒绝覆盖", result.stderr)
            self.assertEqual(source.read_bytes(), original)
            self.assertEqual(stat.S_IMODE(source.stat().st_mode), 0o600)

    def test_refuses_unignored_secret_target_inside_repository(self) -> None:
        target = PROJECT_ROOT / "config" / "cluster-users.private.json"
        self.assertFalse(target.exists())
        result = self.run_tool(
            "generate", "--output", str(target), "--count", "200", success=False
        )
        self.assertIn("未被 ignore", result.stderr)
        self.assertFalse(target.exists())

    def test_normalize_sorts_users_and_preserves_private_mode(self) -> None:
        with tempfile.TemporaryDirectory(prefix="ssrp-users-") as temporary:
            directory = Path(temporary)
            source, payload = self.generate(directory, formal_count=205)
            payload["users"] = list(reversed(payload["users"]))
            source.write_text(json.dumps(payload), encoding="utf-8")
            os.chmod(source, 0o600)
            output = directory / "cluster-users.normalized.json"
            result = self.run_tool(
                "normalize", "--input", str(source), "--output", str(output)
            )
            normalized = json.loads(output.read_text(encoding="utf-8"))
            ordering = [(user["kind"], user["name"]) for user in normalized["users"]]
            self.assertEqual(
                ordering,
                sorted(ordering, key=lambda item: (0 if item[0] == "formal" else 1, item[1])),
            )
            self.assertEqual(stat.S_IMODE(output.stat().st_mode), 0o600)
            self.assertNotIn(normalized["shared_i_psk"], result.stdout)

    def test_normalize_rejects_group_readable_source(self) -> None:
        with tempfile.TemporaryDirectory(prefix="ssrp-users-") as temporary:
            directory = Path(temporary)
            source, _ = self.generate(directory, formal_count=200)
            os.chmod(source, 0o640)
            output = directory / "must-not-exist.json"
            result = self.run_tool(
                "normalize",
                "--input",
                str(source),
                "--output",
                str(output),
                success=False,
            )
            self.assertIn("权限过宽", result.stderr)
            self.assertFalse(output.exists())

    def test_render_users_strips_kind_and_keeps_canonical_order(self) -> None:
        with tempfile.TemporaryDirectory(prefix="ssrp-users-") as temporary:
            directory = Path(temporary)
            source, payload = self.generate(directory, formal_count=205, test_count=4)
            output = directory / "ssserver-users.json"
            result = self.run_tool(
                "render-users", "--source", str(source), "--output", str(output)
            )
            rendered = json.loads(output.read_text(encoding="utf-8"))
            self.assertEqual(len(rendered), 209)
            self.assertTrue(all(set(user) == {"name", "password"} for user in rendered))
            self.assertEqual(
                rendered,
                [
                    {"name": user["name"], "password": user["password"]}
                    for user in payload["users"]
                ],
            )
            self.assertEqual(stat.S_IMODE(output.stat().st_mode), 0o600)
            self.assertNotIn(payload["shared_i_psk"], result.stdout)

    def test_verify_five_accepts_identical_canonical_users(self) -> None:
        with tempfile.TemporaryDirectory(prefix="ssrp-users-") as temporary:
            directory = Path(temporary)
            source, payload = self.generate(directory, formal_count=205)
            configs = self.write_five_configs(directory, payload)
            result = self.run_tool(*self.verify_args(source, configs, 205, 4))
            self.assertIn("205 个正式账号、4 个测试账号", result.stdout)
            self.assertNotIn(payload["shared_i_psk"], result.stdout)
            wrong_counts = self.run_tool(
                *self.verify_args(source, configs, 205, 3), success=False
            )
            self.assertIn("formal=205, test=4", wrong_counts.stderr)

    def test_verify_five_rejects_order_and_secret_drift_without_leaking(self) -> None:
        with tempfile.TemporaryDirectory(prefix="ssrp-users-") as temporary:
            directory = Path(temporary)
            source, payload = self.generate(directory, formal_count=205)
            configs = self.write_five_configs(directory, payload)

            drifted = json.loads(configs[4].read_text(encoding="utf-8"))
            drifted["servers"][0]["users"][0], drifted["servers"][0]["users"][1] = (
                drifted["servers"][0]["users"][1],
                drifted["servers"][0]["users"][0],
            )
            configs[4].write_text(json.dumps(drifted), encoding="utf-8")
            result = self.run_tool(
                *self.verify_args(source, configs, 205, 4), success=False
            )
            self.assertIn("用户名或顺序", result.stderr)
            self.assertNotIn(payload["shared_i_psk"], result.stderr)
            for user in payload["users"]:
                self.assertNotIn(user["password"], result.stderr)

    def test_verify_five_rejects_duplicate_node_or_service_ids(self) -> None:
        with tempfile.TemporaryDirectory(prefix="ssrp-users-") as temporary:
            directory = Path(temporary)
            source, payload = self.generate(directory, formal_count=200)
            configs = self.write_five_configs(directory, payload)
            duplicate = json.loads(configs[1].read_text(encoding="utf-8"))
            duplicate["user_stats"]["node_id"] = "node-01"
            configs[1].write_text(json.dumps(duplicate), encoding="utf-8")
            result = self.run_tool(
                *self.verify_args(source, configs, 200, 4), success=False
            )
            self.assertIn("node_id 不唯一", result.stderr)

    def test_verify_five_rejects_valid_but_different_upsk(self) -> None:
        with tempfile.TemporaryDirectory(prefix="ssrp-users-") as temporary:
            directory = Path(temporary)
            source, payload = self.generate(directory, formal_count=200)
            configs = self.write_five_configs(directory, payload)
            drifted = json.loads(configs[3].read_text(encoding="utf-8"))
            replacement = base64.b64encode(b"test-drift-value").decode("ascii")
            self.assertEqual(len(base64.b64decode(replacement)), 16)
            drifted["servers"][0]["users"][17]["password"] = replacement
            configs[3].write_text(json.dumps(drifted), encoding="utf-8")
            result = self.run_tool(
                *self.verify_args(source, configs, 200, 4), success=False
            )
            self.assertIn("uPSK", result.stderr)
            self.assertNotIn(replacement, result.stderr)

    def test_verify_five_rejects_group_readable_node_config(self) -> None:
        with tempfile.TemporaryDirectory(prefix="ssrp-users-") as temporary:
            directory = Path(temporary)
            source, payload = self.generate(directory, formal_count=200)
            configs = self.write_five_configs(directory, payload)
            os.chmod(configs[2], 0o640)
            result = self.run_tool(
                *self.verify_args(source, configs, 200, 4), success=False
            )
            self.assertIn("权限过宽", result.stderr)


if __name__ == "__main__":
    unittest.main()
