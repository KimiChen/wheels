#!/usr/bin/env python3
"""Build the genuine pre-detail Plus revision in an isolated ignored directory."""
from __future__ import annotations

import argparse
import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile

from build_original import frp

ROOT = Path(__file__).resolve().parents[1]
BASELINE_COMMIT = "23851a2298eac85e384c8ba61cb9ba89710d24e5"


def baseline_archive(root: Path = ROOT) -> bytes:
    repository = Path(frp.run(["git", "rev-parse", "--show-toplevel"], cwd=root, env=frp.clean_environment()))
    prefix = root.resolve().relative_to(repository.resolve()).as_posix()
    tree = BASELINE_COMMIT if prefix == "." else BASELINE_COMMIT + ":" + prefix
    epoch = frp.run(["git", "show", "-s", "--format=%ct", BASELINE_COMMIT], cwd=repository, env=frp.clean_environment())
    # A tree object otherwise receives the current wall clock time, changing
    # the archive hash between build and verification despite identical files.
    result = subprocess.run(["git", "archive", "--format=tar", "--mtime=@" + epoch, tree], cwd=repository,
                            env=frp.clean_environment(), stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=False)
    if result.returncode:
        raise frp.PipelineError("Cannot archive the fixed pre-detail Plus revision")
    return result.stdout


def verify_baseline(agent: Path, server: Path, manifest_path: Path, root: Path = ROOT) -> dict:
    """Verify local Git/archive/build consistency; this is not a signed attestation."""
    manifest = json.loads(frp.read_regular(manifest_path))
    archive = baseline_archive(root)
    baseline = manifest.get("plus_baseline", {}) if isinstance(manifest, dict) else {}
    if (not isinstance(baseline, dict) or baseline.get("commit") != BASELINE_COMMIT or baseline.get("source_verified_clean") is not True
            or baseline.get("archive_sha256") != frp.digest(archive)):
        raise frp.PipelineError("Old Plus manifest does not match the fixed Git archive")
    # Confirm that the patch/overlay inventory really belongs to that archive.
    with tempfile.TemporaryDirectory(prefix="frp-plus-baseline-verify-") as temp:
        archived = Path(temp)
        frp.extract_archive(archive, archived)
        expected_patches = {name: frp.digest(data) for name, data in frp.patch_inputs(archived)}
        expected_overlays = {name: frp.digest(data) for name, (data, _) in frp.overlay_inputs(archived).items()}
        if (manifest.get("upstream") != frp.load_lock(archived) or manifest.get("patches") != expected_patches
                or manifest.get("overlays") != expected_overlays):
            raise frp.PipelineError("Old Plus inputs differ from the fixed revision")
    hashes = manifest.get("binaries", {})
    if not isinstance(hashes, dict):
        raise frp.PipelineError("Old Plus binary inventory is invalid")
    for name, path in (("frp-plus-agent", agent), ("frp-plus-server", server)):
        if frp.digest(frp.read_regular(path)) != hashes.get(name):
            raise frp.PipelineError("Old Plus executable does not match its build manifest")
    return manifest


def build(go: Path, root: Path = ROOT) -> dict:
    owner = frp.Pipeline(root, {"FRP_MONITOR_CACHE_DIR": ".cache/compat-baseline", "FRP_MONITOR_OUTPUT_DIR": "dist"})
    with owner.exclusive():
        project = frp.safe_directory(root, str(owner.cache / "project"), ".cache")
        archive = baseline_archive(root)
        staging = Path(tempfile.mkdtemp(prefix=".project-", dir=owner.cache))
        try:
            frp.extract_archive(archive, staging)
            files = {path.relative_to(staging).as_posix(): frp.digest(frp.read_regular(path))
                     for path in sorted(staging.rglob("*")) if path.is_file()}
            provenance = {"commit": BASELINE_COMMIT, "archive_sha256": frp.digest(archive),
                          "source_files": files, "source_verified_clean": True}
            (staging / ".compat-baseline.json").write_text(json.dumps(provenance, sort_keys=True), encoding="utf-8")
            if project.exists():
                old = json.loads(frp.read_regular(project / ".compat-baseline.json"))
                if old.get("commit") != BASELINE_COMMIT:
                    raise frp.PipelineError("Refusing to replace an unowned compatibility source")
                shutil.rmtree(project)
            os.replace(staging, project)
        finally:
            if staging.exists():
                shutil.rmtree(staging)
        spec = importlib.util.spec_from_file_location("legacy_plus_pipeline", project / "scripts/frp.py")
        legacy = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(legacy)

        class BaselinePipeline(legacy.Pipeline):
            def go_environment(self):
                env = super().go_environment()
                env.pop("GOROOT", None)
                env.update({"PATH": str(go.parent) + os.pathsep + os.environ.get("PATH", ""),
                            "GOCACHE": str(frp.safe_directory(root, ".cache/go-build", ".cache")),
                            "GOMODCACHE": str(frp.safe_directory(root, ".cache/go-mod", ".cache")),
                            "GOPROXY": "https://proxy.golang.org,direct", "GOSUMDB": "sum.golang.org"})
                return env

        env = {"FRP_MONITOR_CACHE_DIR": ".cache", "FRP_MONITOR_OUTPUT_DIR": "dist"}
        mirror = root / ".cache/upstream/repository.git"
        if mirror.is_dir() and not mirror.is_symlink():
            env["FRP_MONITOR_UPSTREAM_MIRROR"] = str(mirror)
        pipeline = BaselinePipeline(project, env)
        # The archived web_assets implementation rechecks input and output hashes.
        assets = root / ".cache/web-assets"
        if assets.is_dir():
            if any(path.is_symlink() for path in (assets, *assets.rglob("*"))):
                raise frp.PipelineError("Dashboard cache must not contain symlinks")
            shutil.copytree(assets, pipeline.cache / "web-assets")
        try:
            with pipeline.exclusive():
                result = pipeline.build(native=True)
        except legacy.PipelineError as exc:
            raise frp.PipelineError(str(exc)) from exc
        for relative, expected in files.items():
            if frp.digest(frp.read_regular(project / relative)) != expected:
                raise frp.PipelineError("Archived Plus source changed during baseline build")
        for directory in result["output_dirs"]:
            path = Path(directory) / "BUILD.json"
            manifest = json.loads(frp.read_regular(path))
            manifest["plus_baseline"] = provenance
            manifest["baseline_builder_sha256"] = frp.digest(frp.read_regular(Path(__file__)))
            path.write_text(json.dumps(manifest, sort_keys=True, indent=2) + "\n", encoding="utf-8")
            verify_baseline(Path(directory) / "frp-plus-agent", Path(directory) / "frp-plus-server", path, root)
        return {**result, "plus_commit": BASELINE_COMMIT, "archive_sha256": provenance["archive_sha256"],
                "source_verified_clean": True}


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--go", default=str(ROOT / ".cache/toolchains/go1.26.6/bin/go"))
    args = parser.parse_args(argv)
    try:
        executable = shutil.which(args.go)
        if executable is None:
            raise frp.PipelineError("Go executable not found")
        print(json.dumps(build(Path(executable).resolve()), sort_keys=True, indent=2))
        return 0
    except (frp.PipelineError, OSError, ValueError) as exc:
        print(f"compat-baseline: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
