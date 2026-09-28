"""Offline tests of build input validation, atomic preparation, and release contents."""
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest
from contextlib import redirect_stderr
from unittest import mock

SPEC = importlib.util.spec_from_file_location("frp_pipeline", Path(__file__).resolve().parents[1] / "scripts" / "frp.py")
frp = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(frp)


class PipelineTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.base = Path(self.temporary.name)
        self.root = self.base / "project"
        self.root.mkdir()
        self.upstream = self.base / "mirror"
        self.upstream.mkdir()
        self.git_env = {**frp.clean_environment(), "GIT_AUTHOR_NAME": "Fixture", "GIT_AUTHOR_EMAIL": "fixture@example.invalid",
                        "GIT_COMMITTER_NAME": "Fixture", "GIT_COMMITTER_EMAIL": "fixture@example.invalid"}
        self.git("init", "--initial-branch=main")
        (self.upstream / "go.mod").write_text("module github.com/fatedier/frp\n\ngo 1.25.0\n")
        (self.upstream / "fixture.txt").write_text("original\n")
        self.git("add", ".")
        self.git("commit", "-m", "Local upstream fixture")
        self.git("tag", "-a", "v1.2.3", "-m", "Annotated fixture release")
        self.commit = self.git("rev-parse", "HEAD")
        self.tag = self.git("rev-parse", "refs/tags/v1.2.3")
        self.lock = {"schema_version": 1, "repository": frp.OFFICIAL_REPOSITORY, "tag": "v1.2.3",
                     "tag_object": self.tag, "commit": self.commit, "license": "Apache-2.0"}
        self.write_lock()
        (self.root / "patches").mkdir()
        (self.root / "patches" / "series").write_text("# Empty baseline\n")
        for name in frp.OVERLAYS:
            (self.root / name).mkdir()
            (self.root / name / "README.md").write_text(name + "\n")
        for name in ("LICENSE", "THIRD_PARTY_NOTICES.md"):
            (self.root / name).write_text(name + "\n")
        self.env = {"FRP_MONITOR_UPSTREAM_MIRROR": str(self.upstream)}

    def git(self, *args):
        return subprocess.run(["git", *args], cwd=self.upstream, env=self.git_env, check=True,
                              capture_output=True, text=True).stdout.strip()

    def write_lock(self):
        text = "".join(f"{name} = {json.dumps(value)}\n" for name, value in self.lock.items())
        (self.root / "upstream.lock").write_text(text)

    def pipeline(self):
        return frp.Pipeline(self.root, self.env)

    def test_repeat_prepare_replaces_edits_and_refreshes_only_explicit_overlays(self):
        (self.root / "unrelated-secret.txt").write_text("do-not-copy")
        pipeline = self.pipeline()
        first = pipeline.prepare()
        (pipeline.source / "fixture.txt").write_text("modified cache\n")
        (self.root / "shared" / "README.md").write_text("updated shared\n")
        second = pipeline.prepare()
        self.assertEqual((pipeline.source / "fixture.txt").read_text(), "original\n")
        self.assertEqual((pipeline.source / "extension/frpmonitor/shared/README.md").read_text(), "updated shared\n")
        self.assertNotEqual(first["overlays"], second["overlays"])
        self.assertFalse((pipeline.source / "unrelated-secret.txt").exists())
        self.assertFalse((pipeline.source / ".git").exists())
        self.assertEqual(set(path.name for path in (pipeline.source / "extension/frpmonitor").iterdir()), set(frp.OVERLAYS))

    def test_lock_rejects_unknown_fields_invalid_ids_and_command_strings(self):
        for field, value in (("commit", "HEAD"), ("tag", "v1.2.3; touch pwned"),
                             ("repository", "https://example.invalid/frp.git"),
                             ("schema_version", True), ("surprise", "command")):
            with self.subTest(field=field):
                original = dict(self.lock)
                self.lock[field] = value
                self.write_lock()
                with self.assertRaises(frp.PipelineError):
                    self.pipeline()
                self.lock = original
        self.assertFalse((self.root / ".cache").exists())

    def test_tag_object_and_resolved_commit_must_both_match(self):
        for field in ("tag_object", "commit"):
            with self.subTest(field=field):
                original = self.lock[field]
                self.lock[field] = "0" * 40
                self.write_lock()
                pipeline = self.pipeline()
                with self.assertRaises(frp.PipelineError):
                    pipeline.prepare()
                self.assertFalse(pipeline.source.exists())
                self.lock[field] = original

    def test_literal_dotenv_allowlist_and_environment_precedence(self):
        (self.root / ".env").write_text("IGNORED=$(touch should-not-exist)\n"
                                       "FRP_MONITOR_CACHE_DIR='.cache/nested cache'\n"
                                       "FRP_MONITOR_OUTPUT_DIR=dist/from-file # comment\n")
        values = frp.read_config(self.root, {"FRP_MONITOR_OUTPUT_DIR": "dist/from-environment"})
        self.assertEqual(values, {"FRP_MONITOR_CACHE_DIR": ".cache/nested cache", "FRP_MONITOR_OUTPUT_DIR": "dist/from-environment"})
        self.assertFalse((self.root / "should-not-exist").exists())
        (self.root / ".env").write_text("FRP_MONITOR_CACHE_DIR=$(touch should-not-exist)\n")
        with self.assertRaises(frp.PipelineError):
            frp.read_config(self.root, {})

    @unittest.skipUnless(hasattr(os, 'mkfifo'), 'requires Unix FIFO')
    def test_regular_reader_rechecks_opened_file_after_fifo_replacement(self):
        path = self.root / 'input'
        path.write_bytes(b'regular input')
        self.assertEqual(frp.read_regular(path), b'regular input')
        original_open = os.open

        def replace_before_open(value, flags, *args, **kwargs):
            self.assertTrue(flags & os.O_NONBLOCK, 'FIFO replacement must not block')
            self.assertTrue(flags & os.O_NOFOLLOW)
            path.unlink()
            os.mkfifo(path, 0o600)
            return original_open(value, flags, *args, **kwargs)

        with mock.patch.object(frp.os, 'open', side_effect=replace_before_open), self.assertRaises(frp.PipelineError):
            frp.read_regular(path)

    def test_regular_reader_rejects_link_replacement_after_path_check(self):
        path, target = self.root / 'input', self.root / 'outside'
        path.write_bytes(b'original')
        target.write_bytes(b'private target')
        original_open = os.open

        def replace_before_open(value, flags, *args, **kwargs):
            path.unlink()
            path.symlink_to(target)
            return original_open(value, flags, *args, **kwargs)

        with mock.patch.object(frp.os, 'open', side_effect=replace_before_open), self.assertRaises(OSError):
            frp.read_regular(path)
        self.assertEqual(target.read_bytes(), b'private target')

    def test_cli_reports_corrupt_tar_without_a_traceback(self):
        errors = io.StringIO()
        with mock.patch.object(frp, 'Pipeline') as constructor, redirect_stderr(errors):
            constructor.return_value.prepare.side_effect = tarfile.ReadError('corrupt fixture archive')
            self.assertEqual(frp.main(['prepare']), 1)
        self.assertIn('frp-monitor: corrupt fixture archive', errors.getvalue())
        self.assertNotIn('Traceback', errors.getvalue())

    def test_empty_optional_mirror_uses_official_repository(self):
        (self.root / ".env").write_text("FRP_MONITOR_UPSTREAM_MIRROR=\n")
        self.assertEqual(frp.Pipeline(self.root, {}).origin, frp.OFFICIAL_REPOSITORY)
        self.assertEqual(frp.Pipeline(self.root, {"FRP_MONITOR_UPSTREAM_MIRROR": ""}).origin, frp.OFFICIAL_REPOSITORY)

    def test_generated_path_traversal_and_symlinks_are_rejected(self):
        for value in (".", "../outside", ".cache/../../outside", str(self.base / "outside")):
            with self.subTest(value=value), self.assertRaises(frp.PipelineError):
                frp.Pipeline(self.root, {**self.env, "FRP_MONITOR_CACHE_DIR": value})
        (self.root / ".cache").mkdir()
        outside = self.base / "outside"
        outside.mkdir()
        (self.root / ".cache" / "upstream").symlink_to(outside, target_is_directory=True)
        with self.assertRaises(frp.PipelineError):
            self.pipeline().prepare()
        self.assertEqual(list(outside.iterdir()), [])

    def test_overlay_symlink_secret_and_nested_module_are_rejected(self):
        secret = self.base / "secret"
        secret.write_text("do-not-copy")
        linked = self.root / "web" / "asset"
        linked.symlink_to(secret)
        with self.assertRaises(frp.PipelineError):
            self.pipeline().prepare()
        linked.unlink()
        for name in (".env", "go.mod"):
            path = self.root / "agent" / name
            path.write_text("not allowed\n")
            with self.subTest(name=name), self.assertRaises(frp.PipelineError):
                self.pipeline().prepare()
            path.unlink()

    def test_patch_paths_and_duplicates_are_rejected(self):
        for series in ("../escape.patch\n", "/absolute.patch\n", "0001-one.patch\n0001-one.patch\n"):
            (self.root / "patches" / "series").write_text(series)
            (self.root / "patches" / "0001-one.patch").write_text("dummy\n")
            with self.subTest(series=series), self.assertRaises(frp.PipelineError):
                self.pipeline().prepare()

    def test_patch_applies_then_failure_publishes_no_dirty_or_stale_tree(self):
        patch = self.root / "patches" / "0001-fixture.patch"
        patch.write_text("diff --git a/fixture.txt b/fixture.txt\n--- a/fixture.txt\n+++ b/fixture.txt\n@@ -1 +1 @@\n-original\n+patched\n")
        (self.root / "patches" / "series").write_text(patch.name + "\n")
        pipeline = self.pipeline()
        pipeline.prepare()
        self.assertEqual((pipeline.source / "fixture.txt").read_text(), "patched\n")
        patch.write_text(patch.read_text().replace("-original", "-not-present"))
        with self.assertRaises(frp.PipelineError):
            pipeline.prepare()
        self.assertFalse(pipeline.source.exists())
        self.assertFalse(list(pipeline.source.parent.glob(".prepare-*")))
        self.assertFalse(list(pipeline.source.parent.glob(".applying-*")))

    def test_archive_rejects_path_traversal_and_links(self):
        for name, kind in (("../outside", tarfile.REGTYPE), ("/absolute", tarfile.REGTYPE), ("link", tarfile.SYMTYPE)):
            stream = io.BytesIO()
            with tarfile.open(fileobj=stream, mode="w") as archive:
                info = tarfile.TarInfo(name)
                info.type = kind
                info.linkname = "../outside"
                archive.addfile(info)
            with self.subTest(name=name), self.assertRaises(frp.PipelineError):
                frp.extract_archive(stream.getvalue(), self.root)

    def test_safe_internal_archive_symlink_is_materialized(self):
        stream = io.BytesIO()
        with tarfile.open(fileobj=stream, mode="w") as archive:
            info = tarfile.TarInfo("AGENTS.md")
            info.size = 7
            archive.addfile(info, io.BytesIO(b"fixture"))
            link = tarfile.TarInfo("CLAUDE.md")
            link.type = tarfile.SYMTYPE
            link.linkname = "AGENTS.md"
            archive.addfile(link)
        frp.extract_archive(stream.getvalue(), self.root)
        self.assertEqual((self.root / "CLAUDE.md").read_bytes(), b"fixture")
        self.assertFalse((self.root / "CLAUDE.md").is_symlink())

    def test_git_replace_refs_cannot_change_pinned_archive(self):
        pipeline = self.pipeline()
        pipeline.prepare()
        (self.upstream / "fixture.txt").write_text("replacement\n")
        self.git("add", "fixture.txt")
        self.git("commit", "-m", "Replacement fixture")
        replacement = self.git("rev-parse", "HEAD")
        pipeline.git("fetch", str(self.upstream), "HEAD")
        pipeline.git("replace", self.commit, replacement)
        pipeline.prepare()
        self.assertEqual((pipeline.source / "fixture.txt").read_text(), "original\n")

    def test_web_cache_validates_both_complete_asset_trees(self):
        pipeline = self.pipeline()
        pipeline.prepare()
        web = pipeline.source / "web"
        web.mkdir()
        (web / "package-lock.json").write_text("{}\n")
        builds = []

        def fake_run(command, **kwargs):
            if command == ["node", "--version"]:
                return "v26.5.0"
            if command == ["npm", "--version"]:
                return "11.17.0"
            if command[:3] == ["npm", "run", "build"]:
                workspace = command[-1]
                builds.append(workspace)
                destination = web / workspace / "dist"
                destination.mkdir(parents=True, exist_ok=True)
                (destination / "index.html").write_text("<html>fixture</html>")
                (destination / "app.js").write_text("console.log('fixture')")
            return ""

        with mock.patch.object(frp, "run", side_effect=fake_run):
            first = pipeline.web_assets()
            self.assertEqual(builds, ["frpc", "frps"])
            second = pipeline.web_assets()
            self.assertEqual(first, second)
            self.assertEqual(builds, ["frpc", "frps"])
            cached = pipeline.cache / "web-assets" / first["input_sha256"]
            (cached / "frps" / "app.js").write_text("corrupted")
            pipeline.web_assets()
            self.assertEqual(builds, ["frpc", "frps", "frpc", "frps"])
            (cached / "frpc" / "unexpected.js").write_text("extra file")
            pipeline.web_assets()
            self.assertEqual(len(builds), 6)

    def test_go_environment_drops_ambient_module_fetch_settings(self):
        ambient = {"GOPATH": "/tmp/ambient-gopath", "GOPROXY": "https://proxy.invalid", "GOSUMDB": "off",
                   "GONOPROXY": "*.internal", "GONOSUMDB": "*.internal", "GOPRIVATE": "*.internal", "GOVCS": "off"}
        with mock.patch.dict(os.environ, ambient):
            env = self.pipeline().go_environment()
        for name in ambient:
            self.assertNotIn(name, env)
        self.assertEqual(env["GOWORK"], "off")
        self.assertEqual(env["GOFLAGS"], "")

    def test_package_requires_dependency_license_directory(self):
        pipeline = self.pipeline()
        license_path = self.root / "agent/collect/LICENSE.monitor-probe"
        license_path.parent.mkdir(parents=True, exist_ok=True)
        license_path.write_text("fixture MIT license\n")
        for name in ("scripts/ops.py", "scripts/local.py", "packaging/README.md", "packaging/nginx.conf.example", ".env.example", "monitor/control/schema.sql"):
            path = self.root / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text("public operations fixture\n")
        pipeline.output.mkdir()
        directory = pipeline.output / "linux-amd64"
        directory.mkdir()
        for name in ("frp-monitor-agent", "frp-monitor-server"):
            (directory / name).write_bytes(b"fixture-binary")
        (directory / "BUILD.json").write_text(json.dumps({"target": "linux/amd64", "source_date_epoch": 1234567890}))
        # Without monitor/store/licenses, packaging must fail rather than omit licenses.
        with mock.patch.object(pipeline, "build", return_value={"output_dirs": [str(directory)]}):
            with self.assertRaises(frp.PipelineError):
                pipeline.package()

    def test_packages_are_deterministic_allowlisted_and_checksummed(self):
        pipeline = self.pipeline()
        for name in ("scripts/ops.py", "scripts/local.py", "packaging/README.md", "packaging/nginx.conf.example", ".env.example", "monitor/control/schema.sql"):
            path = self.root / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text("public operations fixture\n")
        license_path = self.root / "agent/collect/LICENSE.monitor-probe"
        license_path.parent.mkdir(parents=True, exist_ok=True)
        license_path.write_text("fixture MIT license\n")
        sqlite_license = self.root / "monitor/store/licenses/sqlite/LICENSE"
        sqlite_license.parent.mkdir(parents=True)
        sqlite_license.write_text("fixture BSD license\n")
        (sqlite_license.parent / "private.env").write_text("do-not-package")
        vm_license = self.root / "monitor/store/licenses/VictoriaMetrics/LICENSE"
        vm_license.parent.mkdir(parents=True)
        vm_license.write_text("fixture Apache license\n")
        for name in ("NOTICE", "LICENSE.txt", "LICENSE.libyaml", "TSDB-SOURCES.md"):
            (vm_license.parent / name).write_text("fixture extra notice\n")
        (self.root / ".env").write_text("PRIVATE_KEY=do-not-package\n")
        pipeline.output.mkdir()
        dirs = []
        for target in ("linux/amd64", "linux/arm64"):
            directory = pipeline.output / target.replace("/", "-")
            directory.mkdir()
            for name in ("frp-monitor-agent", "frp-monitor-server"):
                (directory / name).write_bytes(b"fixture-binary")
            (directory / "BUILD.json").write_text(json.dumps({"target": target, "source_date_epoch": 1234567890}))
            (directory / ".env").write_text("do-not-package")
            dirs.append(str(directory))
        with mock.patch.object(pipeline, "build", return_value={"output_dirs": dirs}):
            first = pipeline.package()
            original = [Path(path).read_bytes() for path in first["packages"]]
            second = pipeline.package()
        self.assertEqual(original, [Path(path).read_bytes() for path in second["packages"]])
        for package in first["packages"]:
            with tarfile.open(package, "r:gz") as archive:
                members = {"/".join(Path(member.name).parts[1:]): archive.extractfile(member).read() for member in archive.getmembers()}
            self.assertNotIn(".env", members)
            self.assertIn("scripts/ops.py", members)
            self.assertIn("packaging/README.md", members)
            self.assertEqual(members["packaging/nginx.conf.example"], b"public operations fixture\n")
            self.assertEqual(members["monitor/control/schema.sql"], b"public operations fixture\n")
            self.assertEqual(members["LICENSE.monitor-probe"], b"fixture MIT license\n")
            self.assertEqual(members["licenses/sqlite/LICENSE"], b"fixture BSD license\n")
            self.assertEqual(members["licenses/VictoriaMetrics/LICENSE"], b"fixture Apache license\n")
            for name in ("NOTICE", "LICENSE.txt", "LICENSE.libyaml", "TSDB-SOURCES.md"):
                self.assertEqual(members["licenses/VictoriaMetrics/" + name], b"fixture extra notice\n")
            self.assertNotIn(b"do-not-package", b"".join(members.values()))
            for line in members["SHA256SUMS"].decode().splitlines():
                checksum, name = line.split("  ", 1)
                self.assertEqual(checksum, hashlib.sha256(members[name]).hexdigest())


if __name__ == "__main__":
    unittest.main()
