"""执行真实发布脚本的工具链门禁；假编译器不下载或编译任何依赖。"""

from __future__ import annotations

import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest


ROOT = Path(__file__).resolve().parent.parent


class ReleaseToolchainTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="release-toolchain-test-")
        self.addCleanup(self.temporary.cleanup)
        self.base = Path(self.temporary.name).resolve()
        # 保留 monorepo 布局，实际执行 git archive 与两架构、两次独立路径构建。
        self.repository = self.base / "repository"
        self.project = self.repository / "sing-box-plus"
        self.scripts = self.project / "scripts"
        self.scripts.mkdir(parents=True)
        for name in ("build-linux-release.sh", "lib.sh", "release-artifact.py"):
            shutil.copy2(ROOT / "scripts" / name, self.scripts / name)

        self.source = self.base / "source"
        (self.source / "release").mkdir(parents=True)
        (self.source / "release" / "LDFLAGS").write_text("-checklinkname=0\n", encoding="utf-8")
        tree_hash = subprocess.check_output(
            [sys.executable, str(self.scripts / "release-artifact.py"), "source-tree-sha256",
             "--source-root", str(self.source)], text=True,
        ).strip()
        self.lock = dict(
            line.split("=", 1) for line in (ROOT / "upstream.lock").read_text(encoding="utf-8").splitlines()
            if line and not line.startswith("#")
        )
        self.lock["prepared_tree_sha256"] = tree_hash
        self.version = self.lock["go_verified"]
        (self.project / "go.mod").write_text("module example.com/release-test\ngo 1.25.5\n", encoding="utf-8")
        self.write_executable(self.scripts / "prepare-source.sh", """#!/bin/sh
set -eu
printf '%s\n' "$GOTOOLCHAIN" > "$PREPARE_MARKER"
cp -R "$SOURCE_FIXTURE" "$1"
""")
        self.fake_bin = self.base / "fake-bin"
        self.fake_bin.mkdir()
        self.write_executable(self.fake_bin / "go", f"""#!{sys.executable}
import json
import os
from pathlib import Path
import sys

arguments = sys.argv[1:]
with open(os.environ["GO_CALLS"], "a", encoding="utf-8") as output:
    output.write(json.dumps({{"args": arguments, "toolchain": os.environ.get("GOTOOLCHAIN")}}) + "\\n")
if arguments == ["env", "GOVERSION"]:
    print(os.environ["FAKE_GOVERSION"])
elif arguments and arguments[0] == "build":
    destination = Path(arguments[arguments.index("-o") + 1])
    destination.write_text("fixture-" + os.environ["GOARCH"], encoding="utf-8")
else:
    raise SystemExit("unexpected go command")
""")
        self.calls = self.base / "go-calls.jsonl"
        self.marker = self.base / "prepare-marker"
        self.output = self.base / "release"
        self.env = dict(os.environ, PATH=str(self.fake_bin) + os.pathsep + os.environ["PATH"],
                        GO_CALLS=str(self.calls), SOURCE_FIXTURE=str(self.source),
                        PREPARE_MARKER=str(self.marker), FAKE_GOVERSION="go" + self.version,
                        GOTOOLCHAIN="auto", SING_BOX_PLUS_NO_DOTENV="1")
        subprocess.run(["git", "init", "--quiet", str(self.repository)], check=True)

    def write_executable(self, path, contents):
        path.write_text(contents, encoding="utf-8")
        path.chmod(0o755)

    def build(self):
        (self.project / "upstream.lock").write_text(
            "".join(f"{key}={value}\n" for key, value in self.lock.items()), encoding="utf-8",
        )
        subprocess.run(["git", "-C", str(self.repository), "add", "."], check=True)
        subprocess.run(
            ["git", "-C", str(self.repository), "-c", "user.name=Release Gate Test",
             "-c", "user.email=fixture@example.com", "-c", "commit.gpgsign=false",
             "-c", "core.hooksPath=/dev/null", "commit", "--quiet", "-m", "test fixture"],
            check=True,
        )
        return subprocess.run(
            ["bash", str(self.scripts / "build-linux-release.sh"), str(self.output), "0.0.0-test"],
            cwd=self.project, env=self.env, text=True, capture_output=True, timeout=30,
        )

    def assert_refused_before_source(self, result):
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertFalse(self.marker.exists(), "工具链拒绝前不应准备源码")
        self.assertFalse(self.output.exists(), "工具链拒绝前不应创建发布目录")
        calls = [json.loads(line) for line in self.calls.read_text(encoding="utf-8").splitlines()]
        self.assertEqual([call["args"] for call in calls], [["env", "GOVERSION"]])
        self.assertTrue(all(call["toolchain"] == "local" for call in calls))

    def test_exact_version_releases_both_targets_with_local_toolchain(self):
        self.env["GOTOOLCHAIN"] = "go9.99.9+auto"
        result = self.build()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.marker.read_text(encoding="utf-8").strip(), "local")
        calls = [json.loads(line) for line in self.calls.read_text(encoding="utf-8").splitlines()]
        self.assertEqual(len(calls), 5)  # GOVERSION + 两架构各两次构建
        self.assertTrue(all(call["toolchain"] == "local" for call in calls))
        manifest = json.loads((self.output / "manifest.json").read_text(encoding="utf-8"))
        self.assertEqual(manifest["go_version"], "go" + self.version)
        self.assertEqual(manifest["targets"], ["linux/amd64", "linux/arm64"])
        self.assertEqual(len(manifest["artifacts"]), 2)
        self.assertTrue((self.output / "SHA256SUMS").is_file())

    def test_older_toolchain_is_rejected(self):
        self.env["FAKE_GOVERSION"] = "go1.25.0"
        result = self.build()
        self.assert_refused_before_source(result)
        self.assertIn("发布要求", result.stderr)

    def test_newer_toolchain_is_also_rejected(self):
        self.env["FAKE_GOVERSION"] = "go9.99.9"
        result = self.build()
        self.assert_refused_before_source(result)
        self.assertIn("发布要求", result.stderr)

    def test_missing_verified_version_is_rejected(self):
        del self.lock["go_verified"]
        result = self.build()
        self.assert_refused_before_source(result)
        self.assertIn("缺少字段：go_verified", result.stderr)

    def test_minor_only_verified_version_is_rejected(self):
        self.lock["go_verified"] = "1.26"
        self.env["FAKE_GOVERSION"] = "go1.26"
        result = self.build()
        self.assert_refused_before_source(result)
        self.assertIn("必须是明确的 Go 补丁版本", result.stderr)


if __name__ == "__main__":
    unittest.main()
