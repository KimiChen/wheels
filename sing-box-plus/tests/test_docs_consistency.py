"""文档与实现的一致性门禁。

文档漂移的代价不是「读起来不准」，而是运维照着一份过期的契约去接线。
这里只查那些一旦不一致就会造成误接的硬事实。
"""

from __future__ import annotations

import json
import re
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent


def read(relative: str) -> str:
    return (ROOT / relative).read_text(encoding="utf-8")


class UpstreamLockConsistencyTest(unittest.TestCase):
    def setUp(self) -> None:
        self.lock = {}
        for line in read("upstream.lock").splitlines():
            if "=" in line and not line.startswith("#"):
                key, _, value = line.partition("=")
                self.lock[key] = value

    def test_lock_has_required_fields(self):
        for key in (
            "schema_version", "repository", "tag", "commit", "prepared_tree_sha256",
            "commit_date", "fetched_at", "license", "go_minimum", "track",
        ):
            self.assertIn(key, self.lock, key)
        self.assertRegex(self.lock["commit"], r"^[0-9a-f]{40}$")
        self.assertRegex(self.lock["prepared_tree_sha256"], r"^[0-9a-f]{64}$")

    def test_commit_matches_copied_files_lock(self):
        header = read("cmd/sing-box-plus/copied-files.lock")
        self.assertIn(f"upstream_commit={self.lock['commit']}", header)

    def test_commit_appears_in_docs_and_notices(self):
        for relative in ("docs/UPSTREAM_BASELINE.md", "THIRD_PARTY_NOTICES.md"):
            self.assertIn(self.lock["commit"], read(relative), relative)

    def test_copied_files_lock_lists_nine_files(self):
        rows = [
            line for line in read("cmd/sing-box-plus/copied-files.lock").splitlines()
            if line.strip() and not line.startswith("#")
        ]
        self.assertEqual(len(rows), 9)
        for row in rows:
            name, upstream, overlay = row.split(" ")
            self.assertTrue((ROOT / "cmd/sing-box-plus" / name).is_file(), name)
            self.assertRegex(upstream, r"^[0-9a-f]{64}$")
            self.assertRegex(overlay, r"^[0-9a-f]{64}$")


class SchemaConsistencyTest(unittest.TestCase):
    def test_schema_version_is_two_everywhere(self):
        source = read("internal/userstats/snapshot.go")
        self.assertIn("SchemaVersion = 2", source)
        model = read("tests/settlement_model.py")
        self.assertIn("SCHEMA_VERSION = 2", model)
        api = read("docs/API.md")
        self.assertIn("GET /v2/snapshot", api)
        self.assertIn("PUT /v2/quota", api)
        # 本项目不提供 /v1/snapshot：文档必须显式说明它恒为 404，
        # 否则已对接 shadowsocks-rust-plus 的下游会以为可以直接切过来。
        self.assertIn("/v1/snapshot", api)

    def test_snapshot_key_sets_match_between_go_and_python(self):
        source = read("internal/userstats/snapshot.go")
        json_tags = set(re.findall(r'json:"([a-z_]+)"', source))
        import sys

        sys.path.insert(0, str(ROOT / "tests"))
        from settlement_model import HEALTH_KEYS, INBOUND_KEYS, SNAPSHOT_KEYS, USER_KEYS

        expected = SNAPSHOT_KEYS | HEALTH_KEYS | INBOUND_KEYS | USER_KEYS
        missing = expected - json_tags
        self.assertFalse(missing, f"Go 侧缺少字段：{sorted(missing)}")


class ConfigExampleTest(unittest.TestCase):
    def test_example_is_valid_json_and_desensitised(self):
        text = read("config/server.example.json")
        config = json.loads(text)
        self.assertEqual(len(config["services"]), 1)
        service = config["services"][0]
        self.assertEqual(service["type"], "user_stats")
        # 两个 socket 不得同路径——配置校验会拒绝，示例更不能示范错误做法。
        self.assertNotEqual(service["listen_path"], service["quota_control"]["listen_path"])
        # 示例里不能出现可直接使用的凭据。
        self.assertIn("REPLACE_WITH", text)
        self.assertIn("example.invalid", text)

    def test_example_inbounds_are_whitelisted(self):
        config = json.loads(read("config/server.example.json"))
        types = {inbound["type"] for inbound in config["inbounds"]}
        self.assertTrue(types <= {"vless", "shadowsocks"}, types)
        listed = set(config["services"][0]["inbounds"])
        tags = {inbound["tag"] for inbound in config["inbounds"]}
        self.assertTrue(listed <= tags, "user_stats.inbounds 指向了不存在的 inbound")
        for inbound in config["inbounds"]:
            self.assertGreater(inbound["listen_port"], 0)


class DocsPresenceTest(unittest.TestCase):
    def test_six_docs_exist(self):
        for name in (
            "API.md", "ARCHITECTURE.md", "OPERATIONS.md",
            "UPSTREAM_BASELINE.md", "PERFORMANCE.md", "ACCESS_AUDIT.md",
        ):
            self.assertTrue((ROOT / "docs" / name).is_file(), name)

    def test_operations_documents_the_two_known_differences(self):
        operations = read("docs/OPERATIONS.md")
        self.assertIn("连接建立后被重置", operations)
        self.assertIn("仍会向目的地拨号", operations)
        self.assertIn("不要用官方或 Homebrew 的 sing-box 校验", operations)
        self.assertIn("logrotate", operations)


if __name__ == "__main__":
    unittest.main()
