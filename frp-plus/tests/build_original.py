#!/usr/bin/env python3
"""Build genuine upstream frpc/frps and dashboards for local interoperability tests.

Only the exact upstream.lock Git archive is used. No Plus patch or overlay is
read or applied. All generated files remain under ignored .cache/original.
"""
from __future__ import annotations

import argparse
import importlib.util
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile

ROOT = Path(__file__).resolve().parents[1]
SPEC = importlib.util.spec_from_file_location("original_frp_pipeline", ROOT / "scripts" / "frp.py")
frp = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(frp)
MARKER = ".original-frp.json"
IMPLEMENTATION = "unmodified upstream Git archive with native dashboards"


def verify_original_binaries(agent: Path, server: Path, manifest_path: Path, root: Path = ROOT) -> dict:
    """Check local provenance consistency, not a signature or a trusted attestation."""
    manifest = json.loads(frp.read_regular(manifest_path))
    if not isinstance(manifest, dict) or type(manifest.get("schema_version")) is not int or manifest["schema_version"] != 1:
        raise frp.PipelineError("Original build manifest has an unsupported schema")
    if manifest.get("implementation") != IMPLEMENTATION or manifest.get("upstream") != frp.load_lock(root):
        raise frp.PipelineError("Original build manifest does not match upstream.lock")
    if manifest.get("patches") != {} or manifest.get("overlays") != {} or manifest.get("source_verified_clean") is not True:
        raise frp.PipelineError("Original build manifest must declare clean source without patches or overlays")

    def is_sha256(value: object) -> bool:
        return isinstance(value, str) and re.fullmatch(r"[0-9a-f]{64}", value) is not None

    files = manifest.get("source_files")
    if not isinstance(files, dict) or "go.mod" not in files or any(not is_sha256(value) for value in files.values()):
        raise frp.PipelineError("Original source hash inventory is missing or invalid")
    source_sha = frp.digest(json.dumps(files, sort_keys=True).encode())
    if manifest.get("source_sha256") != source_sha or not is_sha256(manifest.get("archive_sha256")):
        raise frp.PipelineError("Original source hash inventory is inconsistent")
    assets = manifest.get("native_web_assets")
    if not isinstance(assets, dict) or not is_sha256(assets.get("input_sha256")):
        raise frp.PipelineError("Original native Dashboard provenance is missing")
    web_files = assets.get("files")
    if (not isinstance(web_files, dict) or not {"frpc/index.html", "frps/index.html"}.issubset(web_files)
            or any(not is_sha256(value) for value in web_files.values())):
        raise frp.PipelineError("Original native Dashboard inventory is incomplete")
    binaries = manifest.get("binaries")
    if not isinstance(binaries, dict) or set(binaries) != {"frpc", "frps"}:
        raise frp.PipelineError("Original binary inventory must contain frpc and frps")
    for name, path in (("frpc", agent), ("frps", server)):
        if not is_sha256(binaries[name]) or frp.digest(frp.read_regular(path)) != binaries[name]:
            raise frp.PipelineError(f"Original {name} hash does not match its build manifest")
    return manifest


def source_inventory(directory: Path) -> dict[str, str]:
    """Hash source, excluding only this builder's marker and generated web output."""
    hashes = {}
    for base, dirs, files in os.walk(directory, followlinks=False):
        relative = Path(base).relative_to(directory)
        for name in list(dirs):
            if relative.parts and relative.parts[0] == "web" and name in {"node_modules", "dist"}:
                dirs.remove(name)
            elif (Path(base) / name).is_symlink():
                raise frp.PipelineError("Original source directories must not be symlinks")
        for name in sorted(files):
            path = Path(base) / name
            key = path.relative_to(directory).as_posix()
            if key != MARKER:
                hashes[key] = frp.digest(frp.read_regular(path))
    return dict(sorted(hashes.items()))


class OriginalPipeline(frp.Pipeline):
    def __init__(self, root: Path = ROOT, environ: dict | None = None):
        env = dict(os.environ if environ is None else environ)
        # Override enhanced pipeline destinations even when .env configures them.
        env.update({"FRP_MONITOR_CACHE_DIR": ".cache/original", "FRP_MONITOR_OUTPUT_DIR": "dist"})
        super().__init__(root, env)
        self.source = frp.safe_directory(self.root, str(self.cache / "source"), ".cache")
        self.output = frp.safe_directory(self.root, str(self.cache / "bin"), ".cache")
        mirror = self.root / ".cache/upstream/repository.git"
        if self.origin == frp.OFFICIAL_REPOSITORY and mirror.is_dir() and not mirror.is_symlink():
            # Reuse only Git objects, never the enhanced assembled working tree.
            self.origin = str(mirror)

    def prepare_original(self) -> dict:
        self.fetch()
        archive = subprocess.run(["git", "--git-dir", str(self.repository), "archive", self.lock["commit"]],
                                 env=self.git_env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=False)
        if archive.returncode:
            raise frp.PipelineError("Cannot archive pinned upstream source")
        staging = Path(tempfile.mkdtemp(prefix=".source-", dir=self.cache))
        try:
            frp.extract_archive(archive.stdout, staging)
            module = frp.read_regular(staging / "go.mod").decode("utf-8")
            if not re.search(r"^module github\.com/fatedier/frp\s*$", module, re.MULTILINE):
                raise frp.PipelineError("Unexpected original Go module")
            if (staging / "extension/frpmonitor").exists() or (staging / MARKER).exists():
                raise frp.PipelineError("Original archive contains a reserved Plus path")
            files = source_inventory(staging)
            metadata = {
                "schema_version": 1, "implementation": IMPLEMENTATION,
                "upstream": self.lock, "patches": {}, "overlays": {},
                "archive_sha256": frp.digest(archive.stdout),
                "source_sha256": frp.digest(json.dumps(files, sort_keys=True).encode()),
                "source_files": files,
                "source_verified_clean": True,
                "source_format": "git archive; safe internal documentation links materialized",
                "source_date_epoch": int(self.git("show", "-s", "--format=%ct", self.lock["commit"])),
                "builder_sha256": frp.digest(frp.read_regular(Path(__file__))),
                "pipeline_sha256": frp.digest(frp.read_regular(ROOT / "scripts/frp.py")),
            }
            (staging / MARKER).write_text(json.dumps(metadata, sort_keys=True, indent=2) + "\n", encoding="utf-8")
            self.discard_original_source()
            os.replace(staging, self.source)
        finally:
            if staging.exists():
                shutil.rmtree(staging)
        return metadata

    def discard_original_source(self) -> None:
        if self.source.is_symlink():
            raise frp.PipelineError("Original source must not be a symlink")
        if self.source.exists():
            marker = json.loads(frp.read_regular(self.source / MARKER))
            if marker.get("implementation") != IMPLEMENTATION:
                raise frp.PipelineError("Refusing to replace an unowned original source tree")
            shutil.rmtree(self.source)

    def verify_source(self, metadata: dict) -> None:
        if source_inventory(self.source) != metadata["source_files"]:
            raise frp.PipelineError("Original source changed after archive extraction")

    def go_environment(self) -> dict:
        env = super().go_environment()
        for name in ("GOROOT", "GOTOOLDIR", "FRP_MONITOR_COLLECT_FIXTURES"):
            env.pop(name, None)
        # Go's caches support simultaneous builds. Share downloads without sharing source.
        env.update({"GOCACHE": str(frp.safe_directory(self.root, ".cache/go-build", ".cache")),
                    "GOMODCACHE": str(frp.safe_directory(self.root, ".cache/go-mod", ".cache")),
                    "GOPROXY": "https://proxy.golang.org,direct", "GOSUMDB": "sum.golang.org"})
        return env

    def build_original(self, go: str) -> dict:
        metadata = self.prepare_original()
        env = self.go_environment()
        toolchain = frp.run([go, "env", "-json", "GOVERSION", "GOHOSTOS", "GOHOSTARCH", "GOROOT"],
                            cwd=self.source, env=env)
        toolchain = json.loads(toolchain)
        target = toolchain["GOHOSTOS"] + "/" + toolchain["GOHOSTARCH"]
        if not re.fullmatch(r"(linux|darwin)/(amd64|arm64)", target):
            raise frp.PipelineError("Original test builds support Linux/macOS amd64/arm64")
        assets = self.web_assets()
        self.verify_source(metadata)
        self.output.mkdir(parents=True, exist_ok=True)
        destination = self.output / target.replace("/", "-")
        if destination.is_symlink():
            raise frp.PipelineError("Original binary directory must not be a symlink")
        if destination.exists():
            previous = json.loads(frp.read_regular(destination / "BUILD.json"))
            if previous.get("implementation") != IMPLEMENTATION:
                raise frp.PipelineError("Refusing to replace unowned original binaries")
        staging = Path(tempfile.mkdtemp(prefix=".build-", dir=self.output))
        try:
            env.update({"GOOS": toolchain["GOHOSTOS"], "GOARCH": toolchain["GOHOSTARCH"],
                        "SOURCE_DATE_EPOCH": str(metadata["source_date_epoch"])})
            flags = ["-mod=readonly", "-trimpath", "-buildvcs=false", "-ldflags=-buildid="]
            for name in ("frpc", "frps"):
                print(f"Building original {name} ({target})...", file=sys.stderr, flush=True)
                frp.run([go, "build", *flags, "-tags", name, "-o", str(staging / name), "./cmd/" + name],
                        cwd=self.source, env=env, capture=False)
            self.verify_source(metadata)
            fixed_keys = ("CGO_ENABLED", "GOENV", "GOFLAGS", "GOTOOLCHAIN", "GOWORK", "GOPROXY", "GOSUMDB", "GOOS", "GOARCH")
            manifest = {**metadata, "target": target, "go_version": toolchain["GOVERSION"],
                        "toolchain": {**toolchain, "binary_sha256": frp.digest(frp.read_regular(Path(go)))},
                        "go_environment": {key: env[key] for key in fixed_keys}, "go_build_flags": flags,
                        "native_web_assets": assets,
                        "binaries": {name: frp.digest(frp.read_regular(staging / name)) for name in ("frpc", "frps")}}
            (staging / "BUILD.json").write_text(json.dumps(manifest, sort_keys=True, indent=2) + "\n", encoding="utf-8")
            if destination.exists():
                shutil.rmtree(destination)
            os.replace(staging, destination)
        finally:
            if staging.exists():
                shutil.rmtree(staging)
        return {"implementation": IMPLEMENTATION, "commit": self.lock["commit"], "go_version": toolchain["GOVERSION"],
                "source_verified_clean": True, "source_dir": str(self.source), "output_dir": str(destination),
                "manifest": str(destination / "BUILD.json"), "binaries": manifest["binaries"]}


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    bundled = ROOT / ".cache/toolchains/go1.26.6/bin/go"
    parser.add_argument("--go", default=str(bundled) if bundled.is_file() else "go", help="Go executable (no automatic toolchain download)")
    args = parser.parse_args(argv)
    try:
        executable = shutil.which(args.go)
        if executable is None:
            raise frp.PipelineError("Go executable not found")
        go = str(Path(executable).resolve())
        pipeline = OriginalPipeline()
        with pipeline.exclusive():
            result = pipeline.build_original(go)
        print(json.dumps(result, sort_keys=True, indent=2))
        return 0
    except (frp.PipelineError, OSError, ValueError, tarfile.TarError) as exc:
        print(f"original-frp: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
