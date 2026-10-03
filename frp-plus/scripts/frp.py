#!/usr/bin/env python3
"""Prepare, test and build the pinned FRP Plus overlay."""
from __future__ import annotations

import argparse
import contextlib
import gzip
import hashlib
import io
import json
import os
from pathlib import Path
import re
import shutil
import stat
import subprocess
import sys
import tarfile
import tempfile
import tomllib

ROOT = Path(__file__).resolve().parents[1]
OFFICIAL_REPOSITORY = "https://github.com/fatedier/frp.git"
OVERLAYS = ("agent", "monitor", "shared", "web")
CONFIG_KEYS = {"FRP_MONITOR_CACHE_DIR", "FRP_MONITOR_OUTPUT_DIR", "FRP_MONITOR_UPSTREAM_MIRROR"}
BASELINE = "P4 SQLite node control, GitHub administration and optional embedded TSDB"
LOCK_KEYS = {"schema_version", "repository", "tag", "tag_object", "commit", "license"}
NATIVE_TESTS = ("./pkg/config/...", "./pkg/msg/...", "./pkg/util/...", "./pkg/metrics/...", "./client/...", "./server/...", "./extension/frpmonitor/...")


class PipelineError(Exception):
    pass


def read_regular(path: Path) -> bytes:
    if path.is_symlink() or not path.is_file():
        raise PipelineError(f"Expected a regular, non-symlink file: {path}")
    # Recheck the opened object, without following a replacement link or
    # blocking if the pathname was replaced by a FIFO after the first check.
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, "rb") as stream:
        if not stat.S_ISREG(os.fstat(stream.fileno()).st_mode):
            raise PipelineError(f"Expected a regular file: {path}")
        return stream.read()


def load_lock(root: Path) -> dict:
    try:
        value = tomllib.loads(read_regular(root / "upstream.lock").decode("utf-8"))
    except (UnicodeError, tomllib.TOMLDecodeError) as exc:
        raise PipelineError("Invalid UTF-8 TOML upstream.lock") from exc
    if set(value) != LOCK_KEYS or type(value.get("schema_version")) is not int or value["schema_version"] != 1:
        raise PipelineError("upstream.lock must contain exactly the schema_version=1 fields")
    if value["repository"] != OFFICIAL_REPOSITORY or value["license"] != "Apache-2.0":
        raise PipelineError("Unexpected upstream repository or license")
    if not isinstance(value["tag"], str) or not re.fullmatch(r"v[0-9]+\.[0-9]+\.[0-9]+", value["tag"]):
        raise PipelineError("Invalid upstream version tag")
    for field in ("tag_object", "commit"):
        if not isinstance(value[field], str) or not re.fullmatch(r"[0-9a-f]{40}", value[field]):
            raise PipelineError(f"Invalid full Git object ID: {field}")
    return value


def read_config(root: Path, environ: dict | None = None) -> dict:
    """Read only literal allowlisted assignments. Never source or interpolate .env."""
    env = os.environ if environ is None else environ
    values = {}
    path = root / ".env"
    if path.exists() or path.is_symlink():
        for line in read_regular(path).decode("utf-8").splitlines():
            line = line.strip()
            if line.startswith("export "):
                line = line[7:].lstrip()
            if not line or line.startswith("#") or "=" not in line:
                continue
            key, value = line.split("=", 1)
            key = key.strip()
            if key not in CONFIG_KEYS or key in env:
                continue
            value = value.strip()
            if value.startswith(("'", '"')):
                if len(value) < 2 or value[-1] != value[0]:
                    raise PipelineError(f"Invalid quoted literal for {key}")
                value = value[1:-1]
            else:
                value = value.split(" #", 1)[0].rstrip()
            values[key] = value
    values.update({key: env[key] for key in CONFIG_KEYS if key in env})
    if values.get("FRP_MONITOR_UPSTREAM_MIRROR") == "":
        values.pop("FRP_MONITOR_UPSTREAM_MIRROR")
    for key, value in values.items():
        if not value or any(ord(char) < 32 for char in value) or "$" in value or "`" in value:
            raise PipelineError(f"{key} must be a nonempty literal without shell expansion")
    return values


def safe_directory(root: Path, raw: str, area: str) -> Path:
    path = Path(raw)
    if not path.is_absolute():
        path = root / path
    # Keep all configurable generated paths within the already ignored project areas.
    permitted = root / area
    if path != permitted and permitted not in path.parents:
        raise PipelineError(f"Generated directory must be inside {permitted}")
    if ".." in path.parts:
        raise PipelineError("Parent traversal is not allowed in generated directories")
    cursor = path
    while cursor != root:
        if cursor.is_symlink():
            raise PipelineError(f"Symlink is not allowed in generated directory: {cursor}")
        cursor = cursor.parent
    return path


def clean_environment() -> dict:
    env = dict(os.environ)
    for name in list(env):
        if name.startswith("GIT_"):
            env.pop(name)
    env.update({"GIT_TERMINAL_PROMPT": "0", "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": os.devnull, "GIT_NO_REPLACE_OBJECTS": "1"})
    return env


def run(command: list[str], *, cwd: Path | None = None, env: dict | None = None, capture: bool = True) -> str:
    result = subprocess.run(command, cwd=cwd, env=env, stdout=subprocess.PIPE if capture else sys.stderr,
                            stderr=subprocess.PIPE if capture else sys.stderr, text=True)
    if result.returncode:
        # Commands never contain token values. Do not echo local .env contents.
        detail = (result.stderr or "").strip()
        raise PipelineError(f"{command[0]} {command[1] if len(command) > 1 else ''} failed ({result.returncode})"
                            + (f": {detail}" if detail else ""))
    return result.stdout.strip() if capture else ""


def digest(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def patch_inputs(root: Path) -> list[tuple[str, bytes]]:
    result = []
    seen = set()
    if (root / "patches").is_symlink():
        raise PipelineError("Patch directory must not be a symlink")
    for line in read_regular(root / "patches" / "series").decode("utf-8").splitlines():
        name = line.strip()
        if not name or name.startswith("#"):
            continue
        if not re.fullmatch(r"[0-9]{4}-[A-Za-z0-9][A-Za-z0-9._-]*\.patch", name) or name in seen:
            raise PipelineError("Invalid or duplicate patch filename in patches/series")
        seen.add(name)
        result.append((name, read_regular(root / "patches" / name)))
    return result


def overlay_inputs(root: Path) -> dict[str, tuple[bytes, int]]:
    result = {}
    for name in OVERLAYS:
        directory = root / name
        if directory.is_symlink() or not directory.is_dir():
            raise PipelineError(f"Missing or symlink overlay directory: {name}")
        for base, dirs, files in os.walk(directory, followlinks=False):
            for item in sorted(dirs + files):
                path = Path(base) / item
                if path.is_symlink():
                    raise PipelineError(f"Overlay symlinks are not allowed: {path.relative_to(root)}")
                if item.startswith(".env") or item in {".git", "node_modules", "__pycache__", "go.mod", "go.sum", "go.work", "go.work.sum"} or item.endswith(".pyc"):
                    raise PipelineError(f"Unexpected local configuration, dependency or nested module in overlay: {path.relative_to(root)}")
            for item in sorted(files):
                path = Path(base) / item
                relative = path.relative_to(root).as_posix()
                result[relative] = (read_regular(path), 0o755 if path.stat().st_mode & 0o111 else 0o644)
    return result


def extract_archive(data: bytes, destination: Path) -> None:
    """Extract Git archive files without trusting archive path or link metadata."""
    with tarfile.open(fileobj=io.BytesIO(data), mode="r:") as archive:
        links = []
        for member in archive:
            name = Path(member.name)
            if not name.parts or name.is_absolute() or ".." in name.parts or name.parts[0] == ".git":
                raise PipelineError("Unsafe upstream archive member")
            target = destination / name
            if member.isdir():
                target.mkdir(parents=True, exist_ok=True)
            elif member.isfile():
                target.parent.mkdir(parents=True, exist_ok=True)
                source = archive.extractfile(member)
                if source is None:
                    raise PipelineError("Unreadable upstream archive member")
                target.write_bytes(source.read())
                target.chmod(0o755 if member.mode & 0o111 else 0o644)
            elif member.issym():
                link = Path(member.linkname)
                if link.is_absolute() or ".." in link.parts:
                    raise PipelineError("Unsafe upstream archive symlink")
                links.append((target, target.parent / link))
            else:
                raise PipelineError("Upstream archive hard links and special files are not allowed")
        # Materialize safe internal documentation links after extracting regular files.
        # The prepared source itself contains no symlinks to follow on subsequent writes.
        for target, source in links:
            data = read_regular(source)
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(data)
            target.chmod(0o644)


class Pipeline:
    def __init__(self, root: Path = ROOT, environ: dict | None = None):
        self.root = root.resolve()
        self.lock = load_lock(self.root)
        config = read_config(self.root, environ)
        self.cache = safe_directory(self.root, config.get("FRP_MONITOR_CACHE_DIR", ".cache"), ".cache")
        for directory in ("upstream", "go-build", "go-mod", "npm", "web-assets"):
            if (self.cache / directory).is_symlink():
                raise PipelineError("Generated cache subdirectories must not be symlinks")
        self.output = safe_directory(self.root, config.get("FRP_MONITOR_OUTPUT_DIR", "dist"), "dist")
        self.origin = config.get("FRP_MONITOR_UPSTREAM_MIRROR", self.lock["repository"])
        if self.origin != OFFICIAL_REPOSITORY:
            # Mirrors may be local Git repositories or HTTPS repositories without credentials.
            from urllib.parse import urlparse
            parsed = urlparse(self.origin)
            if parsed.scheme:
                if parsed.scheme != "https" or not parsed.hostname or parsed.username or parsed.password or parsed.query or parsed.fragment:
                    raise PipelineError("Mirror must be a local path or an HTTPS URL without credentials")
            else:
                mirror = Path(self.origin)
                self.origin = str((self.root / mirror).resolve() if not mirror.is_absolute() else mirror.resolve())
                if not Path(self.origin).is_dir():
                    raise PipelineError("Local upstream mirror does not exist")
        self.git_env = clean_environment()
        self.git_env["GIT_CEILING_DIRECTORIES"] = str(self.cache)
        self.source = self.cache / "upstream" / "worktree"
        self.repository = self.cache / "upstream" / "repository.git"

    @contextlib.contextmanager
    def exclusive(self):
        import fcntl
        self.cache.mkdir(parents=True, exist_ok=True)
        lock = self.cache / ".pipeline.lock"
        if lock.is_symlink():
            raise PipelineError("Cache lock must not be a symlink")
        with lock.open("a") as handle:
            try:
                fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError as exc:
                raise PipelineError("Another frp-plus pipeline is using this cache") from exc
            yield

    def git(self, *args: str) -> str:
        return run(["git", "--git-dir", str(self.repository), *args], env=self.git_env)

    def verify_identity(self) -> None:
        tag = "refs/tags/" + self.lock["tag"]
        if self.git("rev-parse", "--verify", tag) != self.lock["tag_object"]:
            raise PipelineError("Upstream tag object does not match upstream.lock")
        if self.git("cat-file", "-t", self.lock["tag_object"]) != "tag":
            raise PipelineError("Pinned upstream tag must be an annotated tag")
        headers = self.git("cat-file", "-p", self.lock["tag_object"]).split("\n\n", 1)[0].splitlines()
        if ("tag " + self.lock["tag"]) not in headers or "type commit" not in headers or ("object " + self.lock["commit"]) not in headers:
            raise PipelineError("Annotated tag metadata does not match the locked tag and commit")
        if self.git("rev-parse", "--verify", tag + "^{commit}") != self.lock["commit"]:
            raise PipelineError("Upstream tag resolves to a different commit")
        if self.git("cat-file", "-t", self.lock["commit"]) != "commit":
            raise PipelineError("Pinned upstream commit has an unexpected type")

    def fetch(self) -> None:
        if self.repository.is_symlink() or self.repository.parent.is_symlink():
            raise PipelineError("Upstream cache must not be a symlink")
        if not self.repository.exists():
            self.repository.parent.mkdir(parents=True, exist_ok=True)
            run(["git", "init", "--bare", str(self.repository)], env=self.git_env)
        try:
            self.verify_identity()
            return
        except PipelineError:
            pass
        tag = "refs/tags/" + self.lock["tag"]
        print("Fetching pinned FRP source...", file=sys.stderr, flush=True)
        self.git("fetch", "--depth=1", "--no-tags", "--force", self.origin, "+" + tag + ":" + tag)
        self.verify_identity()

    def discard_source(self) -> None:
        if self.source.parent.is_symlink():
            raise PipelineError("Upstream cache must not be a symlink")
        if self.source.is_symlink():
            raise PipelineError("Prepared source must not be a symlink")
        if self.source.exists():
            if not (self.source / ".frp-monitor.json").is_file():
                raise PipelineError("Refusing to replace a source tree without pipeline metadata")
            shutil.rmtree(self.source)

    def prepare(self) -> dict:
        # Remove the old published tree first: a failed prepare never leaves a stale usable tree.
        self.discard_source()
        patches = patch_inputs(self.root)
        overlays = overlay_inputs(self.root)
        self.fetch()
        epoch = int(self.git("show", "-s", "--format=%ct", self.lock["commit"]))
        metadata = {"schema_version": 1, "upstream": self.lock, "implementation": BASELINE,
                    "pipeline_sha256": digest(read_regular(Path(__file__))),
                    "source_date_epoch": epoch,
                    "patches": {name: digest(data) for name, data in patches},
                    "overlays": {name: digest(data) for name, (data, _) in sorted(overlays.items())}}
        staging = Path(tempfile.mkdtemp(prefix=".prepare-", dir=self.repository.parent))
        try:
            archive = subprocess.run(["git", "--git-dir", str(self.repository), "archive", self.lock["commit"]],
                                     env=self.git_env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=False)
            if archive.returncode:
                raise PipelineError("Cannot archive pinned upstream source")
            extract_archive(archive.stdout, staging)
            for name, data in patches:
                descriptor, filename = tempfile.mkstemp(prefix=".applying-", suffix=".patch", dir=self.repository.parent)
                patch_file = Path(filename)
                try:
                    with os.fdopen(descriptor, "wb") as stream:
                        stream.write(data)
                    run(["git", "apply", "--check", str(patch_file)], cwd=staging, env=self.git_env)
                    run(["git", "apply", str(patch_file)], cwd=staging, env=self.git_env)
                finally:
                    patch_file.unlink(missing_ok=True)
            module = read_regular(staging / "go.mod").decode("utf-8")
            if not re.search(r"^module github\.com/fatedier/frp\s*$", module, re.MULTILINE):
                raise PipelineError("Unexpected upstream Go module")
            if any(path.is_symlink() for path in staging.rglob("*")):
                raise PipelineError("Patches must not introduce symlinks into the prepared source")
            destination = staging / "extension" / "frpmonitor"
            if destination.exists():
                raise PipelineError("Upstream or patches already contain the reserved overlay destination")
            for name, (data, mode) in overlays.items():
                target = destination / name
                target.parent.mkdir(parents=True, exist_ok=True)
                target.write_bytes(data)
                target.chmod(mode)
            (staging / ".frp-monitor.json").write_text(json.dumps(metadata, sort_keys=True, indent=2) + "\n", encoding="utf-8")
            os.replace(staging, self.source)
        finally:
            if staging.exists():
                shutil.rmtree(staging)
        return {"source_dir": str(self.source), **metadata}

    def go_environment(self) -> dict:
        env = dict(os.environ)
        # Do not let ambient Go flags, workspaces, module fetch/verification settings
        # or automatic toolchain downloads change the build.
        for name in ("GOOS", "GOARCH", "GOAMD64", "GOARM", "GOARM64", "GO386", "GOMIPS", "GOMIPS64", "GOPPC64", "GORISCV64",
                     "GOEXPERIMENT", "GOFIPS140", "GODEBUG", "GOFLAGS", "GOWORK", "GOENV", "GOTOOLCHAIN",
                     "GOPATH", "GOPROXY", "GOSUMDB", "GONOPROXY", "GONOSUMDB", "GOPRIVATE", "GOVCS"):
            env.pop(name, None)
        env.update({"CGO_ENABLED": "0", "GOTOOLCHAIN": "local", "GOWORK": "off", "GOENV": "off", "GOFLAGS": "",
                    "GOCACHE": str(self.cache / "go-build"), "GOMODCACHE": str(self.cache / "go-mod"),
                    "FRP_MONITOR_COLLECT_FIXTURES": str(self.root / "tests/fixtures/collect/cases.json")})
        return env

    def test(self) -> dict:
        prepared = self.prepare()
        env = self.go_environment()
        run(["go", "test", "-mod=readonly", "-trimpath", "-buildvcs=false", *NATIVE_TESTS], cwd=self.source, env=env, capture=False)
        return {"source_dir": prepared["source_dir"], "implementation": BASELINE, "tested": list(NATIVE_TESTS)}

    def web_assets(self) -> dict:
        """Build and verify both native dashboards; never silently select the noweb build tag."""
        node = run(["node", "--version"])
        npm = run(["npm", "--version"])
        inputs = {}
        for path in sorted((self.source / "web").rglob("*")):
            if {"node_modules", "dist"}.intersection(path.relative_to(self.source / "web").parts):
                continue
            if path.is_file():
                inputs[path.relative_to(self.source).as_posix()] = digest(read_regular(path))
        key = digest(json.dumps({"files": inputs, "node": node, "npm": npm}, sort_keys=True).encode())
        parent = self.cache / "web-assets"
        if parent.is_symlink():
            raise PipelineError("Web asset cache must not be a symlink")
        parent.mkdir(parents=True, exist_ok=True)
        cached = parent / key

        def inventory(directory: Path) -> dict:
            hashes = {}
            for workspace in ("frpc", "frps"):
                web = directory / workspace
                if web.is_symlink() or not (web / "index.html").is_file():
                    raise PipelineError("Native dashboard cache is incomplete")
                for path in sorted(web.rglob("*")):
                    if path.is_symlink():
                        raise PipelineError("Web asset symlinks are not allowed")
                    if path.is_file():
                        hashes[path.relative_to(directory).as_posix()] = digest(read_regular(path))
            return hashes

        if cached.is_symlink():
            raise PipelineError("Web asset cache entry must not be a symlink")
        valid = False
        if cached.exists():
            try:
                manifest = json.loads(read_regular(cached / "ASSETS.json"))
                valid = isinstance(manifest, dict) and manifest["input_sha256"] == key and manifest["files"] == inventory(cached)
            except (PipelineError, KeyError, ValueError, OSError):
                pass
            if not valid:
                shutil.rmtree(cached)
        if not valid:
            print("Building original frpc and frps dashboard resources...", file=sys.stderr, flush=True)
            npm_env = dict(os.environ)
            npm_env.pop("NODE_OPTIONS", None)
            npm_env["NODE_ENV"] = "development"
            npm_env["npm_config_cache"] = str(self.cache / "npm")
            npm_env["npm_config_update_notifier"] = "false"
            run(["npm", "ci", "--no-audit", "--no-fund"], cwd=self.source / "web", env=npm_env, capture=False)
            for workspace in ("frpc", "frps"):
                run(["npm", "run", "build", "--workspace", workspace], cwd=self.source / "web", env=npm_env, capture=False)
            temporary = Path(tempfile.mkdtemp(prefix=".assets-", dir=parent))
            try:
                for workspace in ("frpc", "frps"):
                    shutil.copytree(self.source / "web" / workspace / "dist", temporary / workspace)
                manifest = {"input_sha256": key, "node_version": node, "npm_version": npm, "files": inventory(temporary)}
                (temporary / "ASSETS.json").write_text(json.dumps(manifest, sort_keys=True, indent=2) + "\n", encoding="utf-8")
                os.replace(temporary, cached)
            finally:
                if temporary.exists():
                    shutil.rmtree(temporary)
        for workspace in ("frpc", "frps"):
            destination = self.source / "web" / workspace / "dist"
            if destination.exists():
                shutil.rmtree(destination)
            shutil.copytree(cached / workspace, destination)
        return manifest

    def build(self, targets: list[str] | None = None, native: bool = False) -> dict:
        prepared = self.prepare()
        env = self.go_environment()
        toolchain = run(["go", "env", "GOVERSION"], cwd=self.source, env=env)
        if native:
            targets = [run(["go", "env", "GOHOSTOS"], cwd=self.source, env=env) + "/" +
                       run(["go", "env", "GOHOSTARCH"], cwd=self.source, env=env)]
        elif not targets:
            targets = ["linux/amd64", "linux/arm64"]
        if any(not re.fullmatch(r"(linux|darwin)/(amd64|arm64)", target) for target in targets):
            raise PipelineError("Supported targets are Linux/macOS amd64/arm64")
        assets = self.web_assets()
        self.output.mkdir(parents=True, exist_ok=True)
        products = []
        for target in dict.fromkeys(targets):
            goos, goarch = target.split("/")
            destination = self.output / f"{goos}-{goarch}"
            if destination.is_symlink() or (destination.exists() and not (destination / "BUILD.json").is_file()):
                raise PipelineError("Refusing to replace an unowned build output directory")
            staging = Path(tempfile.mkdtemp(prefix=".build-", dir=self.output))
            try:
                target_env = {**env, "GOOS": goos, "GOARCH": goarch, "SOURCE_DATE_EPOCH": str(prepared["source_date_epoch"])}
                for name, command in (("frp-plus-agent", "frpc"), ("frp-plus-server", "frps")):
                    print(f"Building {name} ({target}, P4 monitoring)...", file=sys.stderr, flush=True)
                    run(["go", "build", "-mod=readonly", "-trimpath", "-buildvcs=false", "-ldflags=-buildid=",
                         "-tags", command, "-o", str(staging / name), "./cmd/" + command], cwd=self.source, env=target_env, capture=False)
                manifest = {key: value for key, value in prepared.items() if key != "source_dir"}
                manifest.update({"target": target, "go_version": toolchain, "native_web_assets": assets,
                                 "binaries": {name: digest(read_regular(staging / name)) for name in ("frp-plus-agent", "frp-plus-server")}})
                (staging / "BUILD.json").write_text(json.dumps(manifest, sort_keys=True, indent=2) + "\n", encoding="utf-8")
                if destination.exists():
                    shutil.rmtree(destination)
                os.replace(staging, destination)
                products.append(str(destination))
            finally:
                if staging.exists():
                    shutil.rmtree(staging)
        return {"implementation": BASELINE, "go_version": toolchain, "output_dirs": products}

    def package(self) -> dict:
        built = self.build()
        packages = []
        for directory in built["output_dirs"]:
            source = Path(directory)
            manifest = json.loads(read_regular(source / "BUILD.json"))
            payload = {name: read_regular(source / name) for name in ("frp-plus-agent", "frp-plus-server", "BUILD.json")}
            payload.update({name: read_regular(self.root / name) for name in ("LICENSE", "THIRD_PARTY_NOTICES.md", "upstream.lock")})
            payload["LICENSE.monitor-probe"] = read_regular(self.root / "agent/collect/LICENSE.monitor-probe")
            license_dir = self.root / "monitor/store/licenses"
            license_names = {"LICENSE.txt", "LICENSE.libyaml", "NOTICE", "LICENSE", "LICENSE-SQLITE", "LICENSE-SQLITE_VEC", "LICENSE-3RD-PARTY.md", "AUTHORS", "PATENTS", "SOURCES.md", "TSDB-SOURCES.md"}
            if license_dir.is_symlink() or not license_dir.is_dir():
                raise PipelineError("Missing dependency license directory: monitor/store/licenses")
            for license_path in sorted(license_dir.rglob("*")):
                if license_path.name in license_names:
                    payload["licenses/" + license_path.relative_to(license_dir).as_posix()] = read_regular(license_path)
            payload["README.txt"] = ("frp-plus P4: " + BASELINE + ".\nThese executables retain native frpc/frps CLI and configuration.\n"
                                      "Monitoring is opt-in via [telemetry]/[monitor]; use private credentials and verified TLS.\n"
                                      "SQLite control storage is required; embedded history is optional. Administration uses GitHub OAuth only.\n"
                                      "Operations and systemd instructions: packaging/README.md; tools require Python 3.11+.\n"
                                      "Build provenance and exact Go version: BUILD.json.\n").encode("utf-8")
            for name in ("scripts/ops.py", "scripts/local.py", "packaging/README.md", "packaging/nginx.conf.example", ".env.example"):
                payload[name] = read_regular(self.root / name)
            payload["monitor/control/schema.sql"] = read_regular(self.root / "monitor/control/schema.sql")
            payload["SHA256SUMS"] = "".join(f"{digest(data)}  {name}\n" for name, data in sorted(payload.items())).encode("utf-8")
            basename = f"frp-plus-{self.lock['tag']}-{manifest['target'].replace('/', '-')}"
            path = self.output / (basename + ".tar.gz")
            if path.is_symlink():
                raise PipelineError("Package destination must not be a symlink")
            descriptor, temporary = tempfile.mkstemp(prefix=".package-", dir=self.output)
            try:
                with os.fdopen(descriptor, "wb") as stream:
                    with gzip.GzipFile(filename="", mode="wb", fileobj=stream, mtime=0) as zipped:
                        with tarfile.open(fileobj=zipped, mode="w") as archive:
                            for name, data in sorted(payload.items()):
                                info = tarfile.TarInfo(basename + "/" + name)
                                info.size = len(data)
                                info.mode = 0o755 if name in ("frp-plus-agent", "frp-plus-server") else 0o644
                                info.mtime = manifest["source_date_epoch"]
                                archive.addfile(info, io.BytesIO(data))
                os.replace(temporary, path)
            finally:
                Path(temporary).unlink(missing_ok=True)
            packages.append(str(path))
        checksum = self.output / "SHA256SUMS"
        if checksum.is_symlink():
            raise PipelineError("Checksum destination must not be a symlink")
        checksum.write_text("".join(f"{digest(Path(path).read_bytes())}  {Path(path).name}\n" for path in packages), encoding="utf-8")
        return {"implementation": BASELINE, "packages": packages, "checksums": str(checksum)}


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    subparsers = parser.add_subparsers(dest="command", required=True)
    for name in ("prepare", "test", "package"):
        subparsers.add_parser(name)
    build = subparsers.add_parser("build")
    group = build.add_mutually_exclusive_group()
    group.add_argument("--native", action="store_true", help="Build binaries for the current host for smoke tests")
    group.add_argument("--target", action="append", help="Target OS/architecture (default: both Linux architectures)")
    args = parser.parse_args(argv)
    try:
        pipeline = Pipeline()
        with pipeline.exclusive():
            result = pipeline.build(args.target, args.native) if args.command == "build" else getattr(pipeline, args.command)()
        print(json.dumps(result, sort_keys=True, indent=2))
        return 0
    except (PipelineError, OSError, UnicodeError, ValueError, tarfile.TarError) as exc:
        print(f"frp-plus: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
