"""Offline provenance and isolation checks for the genuine upstream test build."""
import importlib.util
import copy
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest import mock

SPEC = importlib.util.spec_from_file_location("build_original", Path(__file__).with_name("build_original.py"))
original = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(original)
frp = original.frp


class OriginalBuildTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        base = Path(self.temporary.name).resolve()
        self.root, self.upstream = base / "project", base / "upstream"
        self.root.mkdir()
        self.upstream.mkdir()
        self.env = {**frp.clean_environment(), "GIT_AUTHOR_NAME": "Fixture", "GIT_AUTHOR_EMAIL": "fixture@example.invalid",
                    "GIT_COMMITTER_NAME": "Fixture", "GIT_COMMITTER_EMAIL": "fixture@example.invalid"}
        self.git("init", "--initial-branch=main")
        (self.upstream / "go.mod").write_text("module github.com/fatedier/frp\n\ngo 1.25.0\n", encoding="utf-8")
        (self.upstream / "fixture.go").write_text("package fixture\n", encoding="utf-8")
        self.git("add", ".")
        self.git("commit", "-m", "Fixture upstream")
        self.git("tag", "-a", "v1.2.3", "-m", "Fixture release")
        self.lock = {"schema_version": 1, "repository": frp.OFFICIAL_REPOSITORY, "tag": "v1.2.3",
                     "tag_object": self.git("rev-parse", "refs/tags/v1.2.3"),
                     "commit": self.git("rev-parse", "HEAD"), "license": "Apache-2.0"}
        self.write_lock()

    def git(self, *args):
        return subprocess.run(["git", *args], cwd=self.upstream, env=self.env, check=True,
                              capture_output=True, text=True).stdout.strip()

    def write_lock(self):
        (self.root / "upstream.lock").write_text("".join(f"{key} = {json.dumps(value)}\n" for key, value in self.lock.items()), encoding="utf-8")

    def pipeline(self):
        return original.OriginalPipeline(self.root, {"FRP_MONITOR_UPSTREAM_MIRROR": str(self.upstream),
                                                     "FRP_MONITOR_CACHE_DIR": ".cache/other", "FRP_MONITOR_OUTPUT_DIR": "dist/other"})

    def test_archive_is_original_and_main_workspace_is_untouched(self):
        main_source = self.root / ".cache/upstream/worktree"
        main_source.mkdir(parents=True)
        (main_source / "fixture.go").write_text("enhanced\n", encoding="utf-8")
        pipeline = self.pipeline()
        with mock.patch.object(frp.Pipeline, "prepare", side_effect=AssertionError("must not prepare overlays")), \
             mock.patch.object(frp, "patch_inputs", side_effect=AssertionError("must not read patches")), \
             mock.patch.object(frp, "overlay_inputs", side_effect=AssertionError("must not read overlays")):
            with pipeline.exclusive():
                metadata = pipeline.prepare_original()
        self.assertEqual(pipeline.cache, self.root / ".cache/original")
        self.assertEqual((pipeline.source / "fixture.go").read_text(), "package fixture\n")
        self.assertEqual((main_source / "fixture.go").read_text(), "enhanced\n")
        self.assertEqual(metadata["upstream"]["commit"], self.lock["commit"])
        self.assertEqual(metadata["patches"], {})
        self.assertEqual(metadata["overlays"], {})
        self.assertFalse((pipeline.source / "extension").exists())
        pipeline.verify_source(metadata)

    def test_source_edits_and_injected_build_inputs_are_detected(self):
        pipeline = self.pipeline()
        with pipeline.exclusive():
            metadata = pipeline.prepare_original()
            (pipeline.source / "injected.go").write_text("package fixture\n", encoding="utf-8")
            with self.assertRaises(frp.PipelineError):
                pipeline.verify_source(metadata)
            (pipeline.source / "injected.go").unlink()
            (pipeline.source / "fixture.go").write_text("changed\n", encoding="utf-8")
            with self.assertRaises(frp.PipelineError):
                pipeline.verify_source(metadata)
            restored = pipeline.prepare_original()
            pipeline.verify_source(restored)
            self.assertEqual(restored["source_sha256"], metadata["source_sha256"])

    def test_wrong_tag_or_commit_never_publishes_source(self):
        self.lock["commit"] = "0" * 40
        self.write_lock()
        pipeline = self.pipeline()
        with pipeline.exclusive(), self.assertRaises(frp.PipelineError):
            pipeline.prepare_original()
        self.assertFalse(pipeline.source.exists())

    def test_original_source_symlinks_and_unowned_directories_are_rejected(self):
        pipeline = self.pipeline()
        pipeline.cache.mkdir(parents=True)
        pipeline.source.symlink_to(self.upstream, target_is_directory=True)
        with self.assertRaises(frp.PipelineError):
            pipeline.discard_original_source()
        pipeline.source.unlink()
        pipeline.source.mkdir()
        with self.assertRaises(frp.PipelineError):
            pipeline.discard_original_source()
        self.assertEqual((self.upstream / "fixture.go").read_text(), "package fixture\n")

    def test_module_environment_ignores_ambient_overrides(self):
        pipeline = self.pipeline()
        with mock.patch.dict(os.environ, {"GOROOT": "/invalid", "GOTOOLCHAIN": "auto", "GOFLAGS": "-mod=mod",
                                          "GOPROXY": "https://invalid.invalid", "GOWORK": "/invalid/go.work"}):
            env = pipeline.go_environment()
        self.assertNotIn("GOROOT", env)
        self.assertEqual(env["GOFLAGS"], "")
        self.assertEqual(env["GOENV"], "off")
        self.assertEqual(env["GOWORK"], "off")
        self.assertEqual(env["GOTOOLCHAIN"], "local")
        self.assertEqual(env["GOPROXY"], "https://proxy.golang.org,direct")
        self.assertEqual(env["GOMODCACHE"], str(self.root / ".cache/go-mod"))

    def manifest_fixture(self):
        pipeline = self.pipeline()
        with pipeline.exclusive():
            metadata = pipeline.prepare_original()
        agent, server = self.root / "frpc", self.root / "frps"
        agent.write_bytes(b"original-agent-fixture")
        server.write_bytes(b"original-server-fixture")
        metadata.update({"native_web_assets": {"input_sha256": "1" * 64,
                                               "files": {"frpc/index.html": "2" * 64, "frps/index.html": "3" * 64}},
                         "binaries": {"frpc": frp.digest(agent.read_bytes()), "frps": frp.digest(server.read_bytes())}})
        manifest = self.root / "BUILD.json"
        manifest.write_text(json.dumps(metadata), encoding="utf-8")
        return agent, server, manifest, metadata

    def test_manifest_accepts_matching_files_at_separate_paths(self):
        agent, server, manifest, metadata = self.manifest_fixture()
        relocated = self.root / "separate" / "original-server"
        relocated.parent.mkdir()
        server.rename(relocated)
        verified = original.verify_original_binaries(agent, relocated, manifest, self.root)
        self.assertEqual(verified["upstream"], self.lock)
        self.assertEqual(verified["binaries"], metadata["binaries"])

    def test_manifest_rejects_wrong_identity_dirty_source_or_inconsistent_hashes(self):
        agent, server, manifest, metadata = self.manifest_fixture()
        mutations = (
            lambda value: value["upstream"].update(commit="0" * 40),
            lambda value: value["upstream"].update(tag_object="0" * 40),
            lambda value: value.update(patches={"0001.patch": "1" * 64}),
            lambda value: value.update(overlays={"agent/file.go": "1" * 64}),
            lambda value: value.update(source_verified_clean=False),
            lambda value: value.update(source_sha256="0" * 64),
            lambda value: value["source_files"].update({"injected.go": "0" * 64}),
            lambda value: value["native_web_assets"]["files"].pop("frpc/index.html"),
            lambda value: value["binaries"].update(frpc="0" * 64),
            lambda value: value["binaries"].update(frps="0" * 64),
        )
        for index, change in enumerate(mutations):
            changed = copy.deepcopy(metadata)
            change(changed)
            manifest.write_text(json.dumps(changed), encoding="utf-8")
            with self.subTest(case=index), self.assertRaises(frp.PipelineError):
                original.verify_original_binaries(agent, server, manifest, self.root)
        manifest.write_text(json.dumps(metadata), encoding="utf-8")
        agent.write_bytes(b"changed executable")
        with self.assertRaises(frp.PipelineError):
            original.verify_original_binaries(agent, server, manifest, self.root)

    def test_smoke_checks_provenance_before_executing_original_inputs(self):
        import frp_smoke
        agent, server, manifest, _ = self.manifest_fixture()
        agent.chmod(0o755)
        server.chmod(0o755)
        # The local fixture is intentionally not the real project's locked commit.
        with mock.patch.object(frp_smoke, "is_original", side_effect=AssertionError("must not execute")), \
             mock.patch.object(frp_smoke, "run_pair", side_effect=AssertionError("must not launch")), \
             self.assertRaises(frp_smoke.SmokeFailure):
            frp_smoke.main(["--agent", str(agent), "--server", str(server), "--original-agent", str(agent),
                            "--original-server", str(server), "--original-manifest", str(manifest)])


if __name__ == "__main__":
    unittest.main()
