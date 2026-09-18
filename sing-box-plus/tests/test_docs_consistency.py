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


# json:"name" 与 json:"name,omitempty" 都要认。只认前者的正则会让加了 omitempty
# 的字段在两个方向上同时隐身——恰好是需要被检查的那一刻失效。
JSON_TAG = re.compile(r'json:"([a-z_]+)(?:,[^"]*)?"')


def go_struct_fields(relative: str) -> dict:
    """按结构体收集 (Go 字段名, json 标签)：{结构体名: [(字段, 标签), ...]}。"""
    source = read(relative)
    result = {}
    for match in re.finditer(r"(?ms)^type (\w+) struct \{(.*?)^\}", source):
        fields = []
        for line in match.group(2).splitlines():
            tag = JSON_TAG.search(line)
            name = re.match(r"\s*([A-Z]\w*)\s", line)
            if tag and name:
                fields.append((name.group(1), tag.group(1)))
        result[match.group(1)] = fields
    return result


def go_struct_tags(relative: str) -> dict:
    """按结构体收集 json 标签：{结构体名: {标签, ...}}。"""
    source = read(relative)
    result = {}
    for match in re.finditer(r"(?ms)^type (\w+) struct \{(.*?)^\}", source):
        result[match.group(1)] = set(JSON_TAG.findall(match.group(2)))
    return result


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
    def test_schema_version_is_three_everywhere(self):
        source = read("internal/userstats/snapshot.go")
        self.assertIn("SchemaVersion = 3", source)
        model = read("tests/settlement_model.py")
        self.assertIn("SCHEMA_VERSION = 3", model)
        api = read("docs/API.md")
        self.assertIn("GET /v3/snapshot", api)
        self.assertIn("PUT /v3/quota", api)
        # 历史路径必须显式说明恒为 404，否则未同步升级的下游会以为可以直接切过来。
        self.assertIn("/v1/snapshot", api)
        self.assertIn("/v2/snapshot", api)

    def test_snapshot_key_sets_match_between_go_and_python(self):
        """逐结构体双向比对。

        单向比对（只查 Go 侧是否缺字段）挡不住真正危险的那个方向：Go 侧新增一个字段，
        本检查照样通过，而 settlement_model.parse_snapshot 要求键集严格相等，
        于是每个真实 collector 会拒绝每一份快照——门禁全绿，控制面全灭。
        逐结构体而非取并集，则字段在结构体之间挪动也会被抓到。
        """
        import sys

        sys.path.insert(0, str(ROOT / "tests"))
        from settlement_model import HEALTH_KEYS, INBOUND_KEYS, SNAPSHOT_KEYS, USER_KEYS

        structs = go_struct_tags("internal/userstats/snapshot.go")
        for name, expected in (("Health", HEALTH_KEYS), ("SnapshotUser", USER_KEYS),
                               ("SnapshotInbound", INBOUND_KEYS), ("Snapshot", SNAPSHOT_KEYS)):
            self.assertIn(name, structs, f"snapshot.go 中找不到结构体 {name}")
            self.assertEqual(structs[name], set(expected), f"{name} 与 Python 侧键集不一致")


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


class DeadOptionTest(unittest.TestCase):
    """用户配置里出现的键，必须真的被读取。

    socket_group 在 2026-09 之前是个反例：它被声明、被解码、通过校验，然后什么都不做。
    这在一个「未知字段硬失败」的项目里尤其有害——键被接受，于是运维有理由相信它生效了。
    纯文本的文档一致性检查抓不到这类问题，只有「字段是否在 options.go 之外被引用过」能抓到。
    """

    OPTION_STRUCTS = ("Options", "AccessLogOptions", "QuotaControlOptions")

    def test_every_option_field_is_read_somewhere(self):
        structs = go_struct_fields("internal/userstats/options.go")
        sources = [path for path in (ROOT / "internal").rglob("*.go")
                   if path.name != "options.go"]
        bodies = {path: path.read_text(encoding="utf-8") for path in sources}
        dead = []
        for struct in self.OPTION_STRUCTS:
            self.assertIn(struct, structs, f"options.go 中找不到结构体 {struct}")
            for field, tag in structs[struct]:
                pattern = re.compile(r"\b" + re.escape(field) + r"\b")
                if not any(pattern.search(body) for body in bodies.values()):
                    dead.append(f"{struct}.{field}（json:\"{tag}\"）")
        self.assertFalse(dead, "以下配置项被声明却从未被读取：" + "，".join(dead))


if __name__ == "__main__":
    unittest.main()
