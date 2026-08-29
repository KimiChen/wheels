#!/usr/bin/env python3

from __future__ import annotations

import hashlib
import importlib.util
import json
import os
import platform
import re
import shlex
import shutil
import struct
import subprocess
import sys
import tempfile
import unittest
from unittest import mock
from pathlib import Path


PROJECT_ROOT = Path(__file__).resolve().parent.parent
ARTIFACT_TOOL = PROJECT_ROOT / "scripts" / "release-artifact.py"
BUILD_TOOL = PROJECT_ROOT / "scripts" / "build.sh"
LINUX_BUILD_TOOL = PROJECT_ROOT / "scripts" / "build-linux-release.sh"
SIGN_TOOL = PROJECT_ROOT / "scripts" / "sign-release.sh"
VERIFY_TOOL = PROJECT_ROOT / "scripts" / "verify-release.sh"
VERIFY_ALL_TOOL = PROJECT_ROOT / "scripts" / "verify.sh"
TARGET = "x86_64-unknown-linux-musl"
VERSION = "v1.24.0"
UPSTREAM_COMMIT = "7ee1aa9223ed8f4d34734aac919036c8ad4502c2"
OVERLAY_COMMIT = "a" * 40
EPOCH = 1_787_725_800


class ReleaseArtifactTest(unittest.TestCase):
    def load_artifact_module(self):
        module_name = "ssrp_release_artifact_test_module"
        spec = importlib.util.spec_from_file_location(module_name, ARTIFACT_TOOL)
        self.assertIsNotNone(spec)
        self.assertIsNotNone(spec.loader if spec else None)
        module = importlib.util.module_from_spec(spec)
        sys.modules[module_name] = module
        assert spec is not None and spec.loader is not None
        spec.loader.exec_module(module)
        return module

    def run_command(
        self,
        command: list[str],
        *,
        success: bool = True,
        env: dict[str, str] | None = None,
    ) -> subprocess.CompletedProcess[str]:
        result = subprocess.run(
            command,
            cwd=PROJECT_ROOT,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
            env=env,
        )
        if success and result.returncode != 0:
            self.fail(f"command failed: {' '.join(command)}\n{result.stderr}")
        if not success and result.returncode == 0:
            self.fail(f"command unexpectedly succeeded: {' '.join(command)}")
        return result

    def fake_elf(self, directory: Path, name: str = "ssserver") -> Path:
        payload = bytearray(512)
        payload[:4] = b"\x7fELF"
        payload[4] = 2
        payload[5] = 1
        payload[6] = 1
        struct.pack_into(
            "<HHIQQQIHHHHHH",
            payload,
            16,
            2,
            62,
            1,
            0x400080,
            64,
            0,
            0,
            64,
            56,
            1,
            0,
            0,
            0,
        )
        struct.pack_into(
            "<IIQQQQQQ",
            payload,
            64,
            1,
            5,
            0,
            0x400000,
            0x400000,
            len(payload),
            len(payload),
            0x1000,
        )
        payload[128:] = hashlib.sha256(f"deterministic-{name}-fixture".encode()).digest() * 12
        binary = directory / name
        binary.write_bytes(payload)
        os.chmod(binary, 0o755)
        return binary

    def write_fake_cargo_zigbuild(self, directory: Path) -> Path:
        fake_bin = directory / "fake-bin"
        fake_bin.mkdir()
        tools = {
            "cargo": "#!/bin/sh\nprintf 'cargo 1.97.0 (fixture)\\n'\n",
            "rustc": (
                "#!/bin/sh\n"
                "printf 'rustc 1.97.0 (fixture)\\n'\n"
                "printf 'commit-hash: 2d8144b7880597b6e6d3dfd63a9a9efae3f533d3\\n'\n"
                "printf 'release: 1.97.0\\n'\n"
            ),
            "zig": "#!/bin/sh\nprintf '0.16.0\\n'\n",
            "python3": f"#!/bin/sh\nprintf '{platform.python_version()}\\n'\n",
        }
        for name, payload in tools.items():
            executable = fake_bin / name
            executable.write_text(payload, encoding="utf-8")
            os.chmod(executable, 0o755)
        (fake_bin / "cargo-zigbuild.version").write_text("0.23.0\n", encoding="utf-8")
        cargo_zigbuild = fake_bin / "cargo-zigbuild"
        cargo_zigbuild.write_text(
            f"""#!{sys.executable}
import hashlib
import json
import os
from pathlib import Path
import struct
import sys

arguments = sys.argv[1:]
if arguments == ["--version"]:
    version = Path(__file__).with_name("cargo-zigbuild.version").read_text(encoding="utf-8").strip()
    print("cargo-zigbuild " + version)
    raise SystemExit(0)
if not arguments or arguments[0] != "zigbuild":
    raise SystemExit(91)
expected = ["--locked", "--release", "--features", "user-audit"]
if any(item not in arguments for item in expected):
    raise SystemExit(92)
target = arguments[arguments.index("--target") + 1]
if target != "x86_64-unknown-linux-musl":
    raise SystemExit(93)
bins = [arguments[index + 1] for index, value in enumerate(arguments) if value == "--bin"]
if bins != ["ssserver", "shadowsocks-auditd"]:
    raise SystemExit(94)
source_root = Path(arguments[arguments.index("--manifest-path") + 1]).parent.resolve()
if Path.cwd().resolve() != source_root:
    raise SystemExit(95)
cargo_home = Path(os.environ["CARGO_HOME"])
if any(cargo_home.iterdir()):
    raise SystemExit(96)
if os.environ.get("HOME") != str(cargo_home):
    raise SystemExit(97)

release = Path(os.environ["CARGO_TARGET_DIR"]) / target / "release"
release.mkdir(parents=True)
for name in bins:
    payload = bytearray(512)
    payload[:4] = b"\\x7fELF"
    payload[4:7] = bytes((2, 1, 1))
    struct.pack_into("<HHIQQQIHHHHHH", payload, 16, 2, 62, 1, 0x400080, 64, 0, 0, 64, 56, 1, 0, 0, 0)
    struct.pack_into("<IIQQQQQQ", payload, 64, 1, 5, 0, 0x400000, 0x400000, len(payload), len(payload), 0x1000)
    payload[128:] = hashlib.sha256(f"deterministic-{{name}}-fixture".encode()).digest() * 12
    output = release / name
    output.write_bytes(payload)
    output.chmod(0o755)
fixture_root = source_root.parent
with open(fixture_root / "cargo-invocations.log", "a", encoding="utf-8") as log:
    log.write(" ".join(arguments) + "\\n")
with open(fixture_root / ("environment-" + source_root.name + ".json"), "w", encoding="utf-8") as output:
    json.dump(dict(os.environ), output, sort_keys=True)
""",
            encoding="utf-8",
        )
        os.chmod(cargo_zigbuild, 0o755)
        return fake_bin

    def fake_release_binaries(
        self, directory: Path, *, epoch: int = EPOCH
    ) -> tuple[Path, Path]:
        for suffix in ("a", "b"):
            source_root = directory / f"source-{suffix}"
            source_root.mkdir()
            (source_root / "Cargo.toml").write_text(
                '[workspace]\nresolver = "2"\n', encoding="utf-8"
            )
            (source_root / "Cargo.lock").write_text("version = 4\n", encoding="utf-8")
        for suffix in ("a", "b"):
            (directory / f"cargo-home-{suffix}").mkdir(mode=0o700)
        self.write_fake_cargo_zigbuild(directory)
        self.ensure_build_receipts(directory, epoch)
        release_dir = directory / "target-a" / TARGET / "release"
        return release_dir / "ssserver", release_dir / "shadowsocks-auditd"

    def source_tree_sha256(self, source_root: Path) -> str:
        result = self.run_command(
            [
                sys.executable,
                str(ARTIFACT_TOOL),
                "source-tree-sha256",
                "--source-root",
                str(source_root),
            ]
        )
        digest = result.stdout.strip()
        self.assertRegex(digest, r"\A[0-9a-f]{64}\Z")
        return digest

    def metadata_arguments(self, source_root: Path, epoch: int = EPOCH) -> list[str]:
        return [
            "--version",
            VERSION,
            "--upstream-commit",
            UPSTREAM_COMMIT,
            "--overlay-commit",
            OVERLAY_COMMIT,
            "--source-date-epoch",
            str(epoch),
            "--expected-prepared-tree-sha256",
            self.source_tree_sha256(source_root),
            "--rustc-version",
            "1.97.0",
            "--rustc-commit",
            "2d8144b7880597b6e6d3dfd63a9a9efae3f533d3",
            "--cargo-version",
            "1.97.0",
            "--cargo-zigbuild-version",
            "0.23.0",
            "--zig-version",
            "0.16.0",
            "--python-version",
            platform.python_version(),
        ]

    def ensure_build_receipts(self, root: Path, epoch: int = EPOCH) -> tuple[Path, Path]:
        receipts: list[Path] = []
        environment = os.environ.copy()
        environment["PATH"] = f"{root / 'fake-bin'}{os.pathsep}{environment['PATH']}"
        forbidden = (
            "ARFLAGS",
            "RANLIBFLAGS_x86_64_unknown_linux_musl",
            "TARGET_CC",
            "CARGO_PROFILE_RELEASE_LTO",
            "RUSTC_BOOTSTRAP",
            "CARGO_ALIAS_ZIGBUILD",
            "CARGO_BUILD_RUSTC_WORKSPACE_WRAPPER",
            "SSRP_ARBITRARY_ENV_SENTINEL",
        )
        for name in forbidden:
            environment[name] = "untrusted"
        for suffix in ("a", "b"):
            target_root = root / f"target-{suffix}"
            source_root = root / f"source-{suffix}"
            receipt = target_root / "build-receipt.json"
            self.run_command(
                [
                    sys.executable,
                    str(ARTIFACT_TOOL),
                    "build-and-receipt",
                    "--build-id",
                    f"build-{suffix}",
                    "--source-root",
                    str(source_root),
                    "--target-root",
                    str(target_root),
                    "--cargo-home",
                    str(root / f"cargo-home-{suffix}"),
                    *self.metadata_arguments(source_root, epoch),
                ],
                env=environment,
            )
            receipts.append(receipt)
        invocations = (root / "cargo-invocations.log").read_text(encoding="utf-8").splitlines()
        self.assertEqual(len(invocations), 2)
        for invocation in invocations:
            self.assertIn("--bin ssserver --bin shadowsocks-auditd", invocation)
        configured_names = {
            "CARGO_ENCODED_RUSTFLAGS",
            "CARGO_HOME",
            "CARGO_INCREMENTAL",
            "CARGO_PROFILE_RELEASE_INCREMENTAL",
            "CARGO_TARGET_DIR",
            "HOME",
            "LANG",
            "LC_ALL",
            "SHADOWSOCKS_BUILD_TIME_UTC",
            "SOURCE_DATE_EPOCH",
            "TZ",
            "ZERO_AR_DATE",
        }
        for suffix in ("a", "b"):
            captured = json.loads(
                (root / f"environment-source-{suffix}.json").read_text(encoding="utf-8")
            )
            for name in forbidden:
                self.assertNotIn(name, captured)
            self.assertLessEqual(
                set(captured) - configured_names,
                {"PATH", "RUSTUP_HOME", "__CF_USER_TEXT_ENCODING"},
            )
        return receipts[0], receipts[1]

    def test_runner_uses_direct_version_bound_cargo_zigbuild(self) -> None:
        with tempfile.TemporaryDirectory(prefix="ssrp-release-zigbuild-") as temporary:
            root = Path(temporary)
            source = root / "source-a"
            source.mkdir()
            (source / "Cargo.toml").write_text(
                '[workspace]\nresolver = "2"\n', encoding="utf-8"
            )
            (source / "Cargo.lock").write_text("version = 4\n", encoding="utf-8")
            cargo_home = root / "cargo-home"
            cargo_home.mkdir()
            fake_bin = self.write_fake_cargo_zigbuild(root)
            environment = os.environ.copy()
            environment["PATH"] = f"{fake_bin}{os.pathsep}{environment['PATH']}"
            environment["CARGO_ALIAS_ZIGBUILD"] = "run --package attacker"
            environment["CARGO_BUILD_RUSTC_WORKSPACE_WRAPPER"] = "/tmp/attacker"
            environment["SSRP_ARBITRARY_ENV_SENTINEL"] = "must-not-leak"
            ambient_home = root / "ambient-cargo-home"
            ambient_home.mkdir()
            (ambient_home / "config.toml").write_text(
                '[build]\nrustc-wrapper = "/tmp/attacker"\n', encoding="utf-8"
            )
            environment["CARGO_HOME"] = str(ambient_home)

            command = [
                sys.executable,
                str(ARTIFACT_TOOL),
                "build-and-receipt",
                "--build-id",
                "build-a",
                "--source-root",
                str(source),
                "--target-root",
                str(root / "target-a"),
                "--cargo-home",
                str(cargo_home),
                *self.metadata_arguments(source),
            ]
            self.run_command(command, env=environment)
            invocation = (root / "cargo-invocations.log").read_text(
                encoding="utf-8"
            )
            self.assertTrue(invocation.startswith("zigbuild --manifest-path "))
            receipt = json.loads(
                (root / "target-a" / "build-receipt.json").read_text(encoding="utf-8")
            )
            self.assertEqual(
                receipt["execution"]["command"][:2], ["cargo-zigbuild", "zigbuild"]
            )
            captured = json.loads(
                (root / "environment-source-a.json").read_text(encoding="utf-8")
            )
            self.assertEqual(captured["CARGO_HOME"], str(cargo_home.resolve()))
            self.assertNotIn("CARGO_BUILD_RUSTC_WORKSPACE_WRAPPER", captured)
            self.assertNotIn("SSRP_ARBITRARY_ENV_SENTINEL", captured)

            poisoned_home = root / "poisoned-cargo-home"
            poisoned_home.mkdir()
            (poisoned_home / "config.toml").write_text(
                '[env]\nSSRP_CONFIG_SENTINEL = "injected"\n', encoding="utf-8"
            )
            poisoned = command.copy()
            poisoned[poisoned.index("--target-root") + 1] = str(root / "target-poisoned")
            poisoned[poisoned.index("--cargo-home") + 1] = str(poisoned_home)
            result = self.run_command(poisoned, success=False, env=environment)
            self.assertIn("专用的空目录", result.stderr)

            ancestor_config = root / ".cargo"
            ancestor_config.mkdir()
            (ancestor_config / "config.toml").write_text(
                '[target.x86_64-unknown-linux-musl]\nlinker = "/tmp/attacker"\n',
                encoding="utf-8",
            )
            clean_home = root / "cargo-home-config-test"
            clean_home.mkdir()
            injected = command.copy()
            injected[injected.index("--target-root") + 1] = str(root / "target-config")
            injected[injected.index("--cargo-home") + 1] = str(clean_home)
            result = self.run_command(injected, success=False, env=environment)
            self.assertIn("Cargo config 搜索路径", result.stderr)
            (ancestor_config / "config.toml").unlink()
            ancestor_config.rmdir()

            (fake_bin / "python3").write_text(
                "#!/bin/sh\nprintf '9.9.9\\n'\n", encoding="utf-8"
            )
            os.chmod(fake_bin / "python3", 0o755)
            fake_python = command.copy()
            fake_python[fake_python.index("--target-root") + 1] = str(
                root / "target-fake-python"
            )
            fake_python[fake_python.index("--cargo-home") + 1] = str(clean_home)
            fake_python[fake_python.index("--python-version") + 1] = "9.9.9"
            result = self.run_command(fake_python, success=False, env=environment)
            self.assertIn("当前 Python 解释器版本", result.stderr)
            self.assertFalse(
                (root / "target-fake-python" / "build-receipt.json").exists()
            )

            (fake_bin / "cargo-zigbuild.version").write_text("9.9.9\n", encoding="utf-8")
            mismatch = command.copy()
            mismatch[mismatch.index("--target-root") + 1] = str(root / "target-b")
            result = self.run_command(mismatch, success=False, env=environment)
            self.assertIn("toolchain 不一致", result.stderr)
            self.assertFalse((root / "target-b" / "build-receipt.json").exists())

    def package_multi(
        self,
        server_binary: Path,
        auditd_binary: Path,
        output: Path,
        *,
        success: bool = True,
        epoch: int = EPOCH,
        second_server_override: Path | None = None,
        expected_output_identity: tuple[int, int] | None = None,
    ) -> subprocess.CompletedProcess[str]:
        root = server_binary.parents[3]
        first_source = root / "source-a"
        second_source = root / "source-b"
        first_target = root / "target-a"
        second_target = root / "target-b"
        second_server = second_server_override or (
            second_target / TARGET / "release" / "ssserver"
        )
        second_auditd = second_target / TARGET / "release" / "shadowsocks-auditd"
        first_receipt = first_target / "build-receipt.json"
        second_receipt = second_target / "build-receipt.json"
        return self.run_command(
            [
                sys.executable,
                str(ARTIFACT_TOOL),
                "package-multi",
                "--binary",
                str(server_binary),
                "--auditd-binary",
                str(auditd_binary),
                "--second-binary",
                str(second_server),
                "--second-auditd-binary",
                str(second_auditd),
                "--first-build-receipt",
                str(first_receipt),
                "--second-build-receipt",
                str(second_receipt),
                "--first-source-root",
                str(first_source),
                "--second-source-root",
                str(second_source),
                "--first-target-root",
                str(first_target),
                "--second-target-root",
                str(second_target),
                "--output-dir",
                str(output),
                *(
                    [
                        "--expected-output-device",
                        str(expected_output_identity[0]),
                        "--expected-output-inode",
                        str(expected_output_identity[1]),
                    ]
                    if expected_output_identity is not None
                    else []
                ),
                *self.metadata_arguments(first_source, epoch),
            ],
            success=success,
        )

    def verify_multi(
        self,
        output: Path,
        *,
        success: bool = True,
        expected_prepared_tree_sha256: str | None = None,
    ) -> subprocess.CompletedProcess[str]:
        if expected_prepared_tree_sha256 is None:
            manifest = json.loads((output / "release-manifest.json").read_text(encoding="utf-8"))
            expected_prepared_tree_sha256 = manifest["build"]["prepared_tree_sha256"]
        return self.run_command(
            [
                sys.executable,
                str(ARTIFACT_TOOL),
                "verify-multi",
                "--output-dir",
                str(output),
                "--manifest",
                str(output / "release-manifest.json"),
                "--expected-version",
                VERSION,
                "--expected-upstream-commit",
                UPSTREAM_COMMIT,
                "--expected-overlay-commit",
                OVERLAY_COMMIT,
                "--expected-source-date-epoch",
                str(EPOCH),
                "--expected-prepared-tree-sha256",
                expected_prepared_tree_sha256,
                "--expected-rustc-version",
                "1.97.0",
                "--expected-rustc-commit",
                "2d8144b7880597b6e6d3dfd63a9a9efae3f533d3",
                "--expected-cargo-version",
                "1.97.0",
                "--expected-cargo-zigbuild-version",
                "0.23.0",
                "--expected-zig-version",
                "0.16.0",
                "--expected-python-version",
                platform.python_version(),
            ],
            success=success,
        )

    def release_script_fixture(self, root: Path) -> tuple[Path, Path]:
        fixture_root = root / "release-script-fixture"
        scripts = fixture_root / "scripts"
        packaging = fixture_root / "packaging"
        scripts.mkdir(parents=True)
        packaging.mkdir()
        for name in (
            "build-linux-release.sh",
            "lib.sh",
            "release-artifact.py",
            "sign-release.sh",
            "verify-release.sh",
        ):
            shutil.copy2(PROJECT_ROOT / "scripts" / name, scripts / name)
        shutil.copytree(PROJECT_ROOT / "patches", fixture_root / "patches")
        shutil.copy2(PROJECT_ROOT / "upstream.lock", fixture_root / "upstream.lock")
        toolchain_lock = (PROJECT_ROOT / "packaging" / "release-toolchain.lock").read_text(
            encoding="utf-8"
        )
        toolchain_lock, replacements = re.subn(
            r"(?m)^RELEASE_PYTHON_VERSION=.*$",
            f"RELEASE_PYTHON_VERSION={platform.python_version()}",
            toolchain_lock,
        )
        self.assertEqual(replacements, 1)
        (packaging / "release-toolchain.lock").write_text(
            toolchain_lock, encoding="utf-8"
        )
        return scripts / "sign-release.sh", scripts / "verify-release.sh"

    def test_packaging_is_byte_reproducible_and_verifiable(self) -> None:
        with tempfile.TemporaryDirectory(prefix="ssrp-release-") as temporary:
            root = Path(temporary)
            server, auditd = self.fake_release_binaries(root)
            first = root / "first"
            second = root / "second"
            first.mkdir()
            second.mkdir()
            self.package_multi(server, auditd, first)
            self.package_multi(server, auditd, second)
            for name in (
                "ssserver",
                "ssserver.sha256",
                "shadowsocks-auditd",
                "shadowsocks-auditd.sha256",
                "build-a.receipt.json",
                "build-b.receipt.json",
                "release-manifest.json",
            ):
                self.assertEqual((first / name).read_bytes(), (second / name).read_bytes())
            result = self.verify_multi(first)
            self.assertIn("双二进制", result.stdout)
            wrong_toolchain = self.run_command(
                [
                    sys.executable,
                    str(ARTIFACT_TOOL),
                    "verify-multi",
                    "--output-dir",
                    str(first),
                    "--manifest",
                    str(first / "release-manifest.json"),
                    "--expected-zig-version",
                    "0.15.0",
                ],
                success=False,
            )
            self.assertIn("toolchain.zig_version", wrong_toolchain.stderr)

    def test_packaging_refuses_overwrite_and_preserves_artifacts(self) -> None:
        with tempfile.TemporaryDirectory(prefix="ssrp-release-") as temporary:
            root = Path(temporary)
            server, auditd = self.fake_release_binaries(root)
            output = root / "output"
            output.mkdir()
            self.package_multi(server, auditd, output)
            paths = sorted(output.iterdir())
            originals = [path.read_bytes() for path in paths]
            result = self.package_multi(server, auditd, output, success=False)
            self.assertIn("拒绝覆盖", result.stderr)
            self.assertEqual([path.read_bytes() for path in paths], originals)

    def test_package_requires_live_independent_roots_and_receipts(self) -> None:
        with tempfile.TemporaryDirectory(prefix="ssrp-release-live-evidence-") as temporary:
            root = Path(temporary)
            server, auditd = self.fake_release_binaries(root)
            copied_server = root / "copied-ssserver"
            shutil.copyfile(server, copied_server)
            os.chmod(copied_server, 0o755)
            output = root / "output"
            output.mkdir()
            result = self.package_multi(
                server,
                auditd,
                output,
                success=False,
                second_server_override=copied_server,
            )
            self.assertIn("live target root", result.stderr)
            self.assertEqual(list(output.iterdir()), [])

            (root / "source-b" / "Cargo.lock").write_text(
                "version = 4\n# drift\n", encoding="utf-8"
            )
            result = self.package_multi(server, auditd, output, success=False)
            self.assertIn("源码树摘要", result.stderr)

    def test_verifier_rejects_self_consistent_but_unpinned_source_tree(self) -> None:
        with tempfile.TemporaryDirectory(prefix="ssrp-release-source-anchor-") as temporary:
            root = Path(temporary)
            server, auditd = self.fake_release_binaries(root)
            output = root / "output"
            output.mkdir()
            self.package_multi(server, auditd, output)

            receipts = [
                json.loads((output / f"build-{suffix}.receipt.json").read_text(encoding="utf-8"))
                for suffix in ("a", "b")
            ]
            self.assertEqual(
                receipts[0]["source"]["tree_sha256"],
                receipts[1]["source"]["tree_sha256"],
            )
            result = self.verify_multi(
                output,
                success=False,
                expected_prepared_tree_sha256="f" * 64,
            )
            self.assertIn("prepared_tree_sha256", result.stderr)

    def test_build_receipt_requires_command_execution_and_empty_target(self) -> None:
        with tempfile.TemporaryDirectory(prefix="ssrp-release-build-proof-") as temporary:
            root = Path(temporary)
            source = root / "source-a"
            source.mkdir()
            (source / "Cargo.toml").write_text('[workspace]\nresolver = "2"\n', encoding="utf-8")
            (source / "Cargo.lock").write_text("version = 4\n", encoding="utf-8")
            cargo_home = root / "cargo-home"
            cargo_home.mkdir()
            target = root / "target-a"
            release = target / TARGET / "release"
            release.mkdir(parents=True)
            self.fake_elf(release, "ssserver")
            self.fake_elf(release, "shadowsocks-auditd")

            result = self.run_command(
                [
                    sys.executable,
                    str(ARTIFACT_TOOL),
                    "build-and-receipt",
                    "--build-id",
                    "build-a",
                    "--source-root",
                    str(source),
                    "--target-root",
                    str(target),
                    "--cargo-home",
                    str(cargo_home),
                    *self.metadata_arguments(source),
                ],
                success=False,
            )
            self.assertIn("必须事先不存在", result.stderr)
            self.assertFalse((target / "build-receipt.json").exists())

    def test_release_directory_is_exact_and_bound_to_original_inode(self) -> None:
        with tempfile.TemporaryDirectory(prefix="ssrp-release-directory-") as temporary:
            root = Path(temporary)
            server, auditd = self.fake_release_binaries(root)
            occupied = root / "occupied"
            occupied.mkdir()
            (occupied / "unbound.txt").write_text("not in manifest\n", encoding="utf-8")
            result = self.package_multi(server, auditd, occupied, success=False)
            self.assertIn("未绑定文件", result.stderr)

            output = root / "output"
            output.mkdir()
            metadata = output.stat()
            result = self.package_multi(
                server,
                auditd,
                output,
                success=False,
                expected_output_identity=(metadata.st_dev, metadata.st_ino + 1),
            )
            self.assertIn("inode", result.stderr)
            self.package_multi(
                server,
                auditd,
                output,
                expected_output_identity=(metadata.st_dev, metadata.st_ino),
            )
            (output / "unbound.txt").write_text("not in manifest\n", encoding="utf-8")
            result = self.verify_multi(output, success=False)
            self.assertIn("未绑定", result.stderr)

    def test_package_publish_rejects_extra_member_inserted_during_write(self) -> None:
        module = self.load_artifact_module()
        with tempfile.TemporaryDirectory(prefix="ssrp-package-race-") as temporary:
            output = Path(temporary) / "output"
            output.mkdir()
            payloads = [
                (
                    name,
                    (b"binary" if name in module.MULTI_ARTIFACTS else b"metadata\n"),
                    module._release_file_mode(name),
                )
                for name in sorted(module.UNSIGNED_RELEASE_FILES)
            ]
            directory_fd, identity = module._open_release_directory(output)
            original_write = module._exclusive_write_at
            writes = 0

            def write_and_insert(*args):
                nonlocal writes
                result = original_write(*args)
                writes += 1
                if writes == len(payloads):
                    (output / "unbound.txt").write_text("race\n", encoding="utf-8")
                return result

            try:
                with mock.patch.object(
                    module, "_exclusive_write_at", side_effect=write_and_insert
                ):
                    with self.assertRaisesRegex(module.ArtifactError, "成员集合"):
                        module._publish_release_payloads(
                            output, directory_fd, identity, payloads
                        )
            finally:
                os.close(directory_fd)
            self.assertEqual({path.name for path in output.iterdir()}, {"unbound.txt"})

    def test_package_publish_rejects_directory_replacement_during_write(self) -> None:
        module = self.load_artifact_module()
        with tempfile.TemporaryDirectory(prefix="ssrp-package-dir-race-") as temporary:
            root = Path(temporary)
            output = root / "output"
            displaced = root / "displaced"
            output.mkdir()
            payloads = [
                (
                    name,
                    (b"binary" if name in module.MULTI_ARTIFACTS else b"metadata\n"),
                    module._release_file_mode(name),
                )
                for name in sorted(module.UNSIGNED_RELEASE_FILES)
            ]
            directory_fd, identity = module._open_release_directory(output)
            original_write = module._exclusive_write_at
            writes = 0

            def write_and_replace(*args):
                nonlocal writes
                result = original_write(*args)
                writes += 1
                if writes == len(payloads):
                    output.rename(displaced)
                    output.mkdir()
                return result

            try:
                with mock.patch.object(
                    module, "_exclusive_write_at", side_effect=write_and_replace
                ):
                    with self.assertRaisesRegex(module.ArtifactError, "发布目录路径"):
                        module._publish_release_payloads(
                            output, directory_fd, identity, payloads
                        )
            finally:
                os.close(directory_fd)
            self.assertEqual(list(displaced.iterdir()), [])
            self.assertEqual(list(output.iterdir()), [])

            validated = root / "validated"
            validated_displaced = root / "validated-displaced"
            validated.mkdir()
            validated_path, validated_identity = module._validate_output_dir(validated)
            validated.rename(validated_displaced)
            validated.mkdir()
            with self.assertRaisesRegex(module.ArtifactError, "验证与打开之间"):
                module._open_release_directory(validated_path, validated_identity)
            os.chmod(validated, 0o777)
            with self.assertRaisesRegex(module.ArtifactError, "不得允许"):
                module._open_release_directory(validated)

    def test_verifier_rejects_truncated_elf_header_fixture(self) -> None:
        with tempfile.TemporaryDirectory(prefix="ssrp-release-elf-") as temporary:
            root = Path(temporary)
            server, auditd = self.fake_release_binaries(root)
            output = root / "output"
            output.mkdir()
            self.package_multi(server, auditd, output)
            binary = output / "ssserver"
            binary.write_bytes(b"\x7fELF" + bytes((2, 1, 1)) + bytes(13))
            os.chmod(binary, 0o755)
            result = self.verify_multi(output, success=False)
            self.assertIn("不是 ELF", result.stderr)

    def test_verifier_rejects_bss_only_executable_segment(self) -> None:
        with tempfile.TemporaryDirectory(prefix="ssrp-release-elf-bss-") as temporary:
            root = Path(temporary)
            server, auditd = self.fake_release_binaries(root)
            output = root / "output"
            output.mkdir()
            self.package_multi(server, auditd, output)
            binary = output / "ssserver"
            payload = bytearray(binary.read_bytes())
            struct.pack_into("<Q", payload, 64 + 32, 0)
            binary.write_bytes(payload)
            os.chmod(binary, 0o755)
            result = self.verify_multi(output, success=False)
            self.assertIn("executable PT_LOAD", result.stderr)

    def test_manifest_binds_patch_series(self) -> None:
        with tempfile.TemporaryDirectory(prefix="ssrp-release-series-") as temporary:
            root = Path(temporary)
            server, auditd = self.fake_release_binaries(root)
            output = root / "output"
            output.mkdir()
            self.package_multi(server, auditd, output)
            manifest = output / "release-manifest.json"
            value = json.loads(manifest.read_text(encoding="utf-8"))
            value["patch_series"][0]["sha256"] = "0" * 64
            manifest.write_text(json.dumps(value, sort_keys=True, indent=2) + "\n", encoding="utf-8")
            self.verify_multi(output, success=False)

    def test_legacy_packaging_and_receipt_commands_are_not_available(self) -> None:
        for command in ("package", "verify", "create-build-receipt"):
            result = self.run_command(
                [sys.executable, str(ARTIFACT_TOOL), command],
                success=False,
            )
            self.assertIn("invalid choice", result.stderr)

    def test_signing_scripts_reject_legacy_single_artifact_flags(self) -> None:
        for command in (SIGN_TOOL, VERIFY_TOOL):
            result = self.run_command([str(command), "--archive", "legacy.tar.gz"], success=False)
            self.assertIn("用法", result.stderr)

    def test_multi_packaging_contains_both_binaries_and_checksums(self) -> None:
        with tempfile.TemporaryDirectory(prefix="ssrp-release-multi-") as temporary:
            root = Path(temporary)
            server, auditd = self.fake_release_binaries(root)
            output = root / "output"
            output.mkdir()
            self.package_multi(server, auditd, output)
            self.verify_multi(output)
            expected = {
                "ssserver",
                "ssserver.sha256",
                "shadowsocks-auditd",
                "shadowsocks-auditd.sha256",
                "build-a.receipt.json",
                "build-b.receipt.json",
                "release-manifest.json",
            }
            self.assertEqual({path.name for path in output.iterdir()}, expected)
            manifest = json.loads((output / "release-manifest.json").read_text(encoding="utf-8"))
            build = manifest["build"]
            self.assertEqual(build["generated_at"], "2026-08-26T06:30:00Z")
            self.assertEqual(build["independent_builds"]["count"], 2)
            self.assertEqual(
                [item["build_id"] for item in build["independent_builds"]["receipts"]],
                ["build-a", "build-b"],
            )
            for item in build["independent_builds"]["receipts"]:
                receipt_path = output / item["path"]
                self.assertEqual(hashlib.sha256(receipt_path.read_bytes()).hexdigest(), item["sha256"])
                receipt = json.loads(receipt_path.read_text(encoding="utf-8"))
                self.assertEqual(receipt["build_id"], item["build_id"])
                self.assertEqual(receipt["recipe"]["script"], "scripts/build-linux-release.sh")
                self.assertEqual(receipt["build"]["source_date_epoch"], EPOCH)
            boundary = build["independent_builds"]["evidence_boundary"]
            self.assertIn("cargo-zigbuild-command-execution", boundary["packager_checks"])
            self.assertIn(
                "declared-toolchain-version-execution-binding",
                boundary["packager_checks"],
            )
            self.assertIn(
                "malicious-build-host-or-builder-resistance", boundary["not_attested"]
            )
            self.assertIn(
                "cryptographic-command-execution-attestation",
                boundary["not_attested"],
            )
            self.assertIn(
                "tool-binary-identity-or-trusted-builder-identity",
                boundary["not_attested"],
            )
            self.assertIn(
                "complete-host-environment-or-cargo-config-attestation",
                boundary["not_attested"],
            )
            self.assertIn("trusted-build-timestamp", boundary["not_attested"])
            self.assertIn("separate-host-or-process-isolation", boundary["not_attested"])
            for item in build["independent_builds"]["receipts"]:
                receipt = json.loads((output / item["path"]).read_text(encoding="utf-8"))
                self.assertEqual(receipt["schema_version"], 2)
                self.assertEqual(receipt["execution"]["exit_code"], 0)
                self.assertTrue(receipt["execution"]["target_was_empty"])
            self.assertEqual(
                set(manifest["toolchain"]),
                {
                    "rustc_version",
                    "rustc_commit",
                    "cargo_version",
                    "cargo_zigbuild_version",
                    "zig_version",
                    "python_version",
                },
            )

    def test_multi_verifier_rejects_noncanonical_metadata_and_manifest_location(self) -> None:
        with tempfile.TemporaryDirectory(prefix="ssrp-release-multi-verify-") as temporary:
            root = Path(temporary)
            server, auditd = self.fake_release_binaries(root)
            output = root / "output"
            output.mkdir()
            self.package_multi(server, auditd, output)

            binary = output / "ssserver"
            os.chmod(binary, 0o755)
            manifest = output / "release-manifest.json"
            original_manifest = manifest.read_bytes()
            value = json.loads(original_manifest)
            value["schema_version"] = True
            manifest.write_text(json.dumps(value, sort_keys=True, indent=2) + "\n", encoding="utf-8")
            self.verify_multi(output, success=False)
            manifest.write_bytes(original_manifest)

            os.chmod(binary, 0o754)
            self.verify_multi(output, success=False)
            os.chmod(binary, 0o755)

            outside = root / "outside-manifest.json"
            outside.write_bytes(original_manifest)
            self.run_command(
                [
                    sys.executable,
                    str(ARTIFACT_TOOL),
                    "verify-multi",
                    "--output-dir",
                    str(output),
                    "--manifest",
                    str(outside),
                ],
                success=False,
            )

    def test_verifier_rejects_unverifiable_build_evidence(self) -> None:
        with tempfile.TemporaryDirectory(prefix="ssrp-release-evidence-") as temporary:
            root = Path(temporary)
            server, auditd = self.fake_release_binaries(root)
            output = root / "output"
            output.mkdir()
            self.package_multi(server, auditd, output)
            manifest = output / "release-manifest.json"
            original_manifest = manifest.read_bytes()
            value = json.loads(original_manifest)
            value["build"]["generated_at"] = "2026-08-26T06:30:01Z"
            manifest.write_text(json.dumps(value, sort_keys=True, indent=2) + "\n", encoding="utf-8")
            self.verify_multi(output, success=False)

            value["build"]["generated_at"] = "2026-08-26T06:30:00Z"
            value["build"]["independent_builds"]["receipts"][0]["sha256"] = "0" * 64
            manifest.write_text(json.dumps(value, sort_keys=True, indent=2) + "\n", encoding="utf-8")
            self.verify_multi(output, success=False)
            manifest.write_bytes(original_manifest)

            receipt = output / "build-a.receipt.json"
            receipt_value = json.loads(receipt.read_text(encoding="utf-8"))
            receipt_value["recipe"]["script_sha256"] = "0" * 64
            receipt.write_text(
                json.dumps(receipt_value, sort_keys=True, indent=2) + "\n",
                encoding="utf-8",
            )
            value = json.loads(original_manifest)
            value["build"]["independent_builds"]["receipts"][0]["sha256"] = hashlib.sha256(
                receipt.read_bytes()
            ).hexdigest()
            manifest.write_text(
                json.dumps(value, sort_keys=True, indent=2) + "\n", encoding="utf-8"
            )
            result = self.verify_multi(output, success=False)
            self.assertIn("receipt recipe", result.stderr)

    def test_build_modes_reject_cross_mode_destinations_before_building(self) -> None:
        with tempfile.TemporaryDirectory(prefix="ssrp-release-guards-") as temporary:
            root = Path(temporary)
            fake_bin = root / "bin"
            fake_bin.mkdir()
            invoked = root / "cargo-invoked"
            fake_cargo = fake_bin / "cargo"
            fake_cargo.write_text(
                f"#!/bin/sh\ntouch {shlex.quote(str(invoked))}\nexit 99\n",
                encoding="utf-8",
            )
            os.chmod(fake_cargo, 0o755)
            env = os.environ.copy()
            env["PATH"] = f"{fake_bin}{os.pathsep}{env['PATH']}"

            development = self.run_command(
                [
                    str(BUILD_TOOL),
                    "--source",
                    str(root / "missing-source"),
                    "--output-dir",
                    str(PROJECT_ROOT / "dist" / "nested"),
                ],
                success=False,
                env=env,
            )
            self.assertIn("不得写入 release 目录", development.stderr)
            self.assertFalse(invoked.exists())

            release = self.run_command(
                [
                    str(LINUX_BUILD_TOOL),
                    "--output-dir",
                    str(PROJECT_ROOT / ".cache" / "dev-dist" / "release"),
                ],
                success=False,
                env=env,
            )
            self.assertIn("不得写入开发产物目录", release.stderr)
            self.assertFalse(invoked.exists())

            occupied = root / "occupied-release"
            occupied.mkdir()
            (occupied / "ssserver").write_bytes(b"development artifact")
            early = self.run_command(
                [str(LINUX_BUILD_TOOL), "--output-dir", str(occupied)],
                success=False,
                env=env,
            )
            self.assertIn("必须为空", early.stderr)
            self.assertFalse(invoked.exists())

    def test_release_build_executes_cc_rs_environment_sanitizer(self) -> None:
        script = LINUX_BUILD_TOOL.read_text(encoding="utf-8")
        match = re.search(
            r'(normalized_release_target=".*?\n.*?run_with_clean_release_environment\(\) \{\n.*?\n\})',
            script,
            flags=re.DOTALL,
        )
        self.assertIsNotNone(match)
        function = match.group(1) if match else ""
        normalized = TARGET.replace("-", "_")
        uppercase = normalized.upper()
        target_names: list[str] = []
        for variable in (
            "AR",
            "ARFLAGS",
            "CC",
            "CFLAGS",
            "CXX",
            "CXXFLAGS",
            "RANLIB",
            "RANLIBFLAGS",
        ):
            target_names.extend(
                (
                    variable,
                    f"{variable}_{TARGET}",
                    f"{variable}_{normalized}",
                    f"{variable}_{uppercase}",
                    f"{TARGET}_{variable}",
                    f"{normalized}_{variable}",
                    f"{uppercase}_{variable}",
                    f"TARGET_{variable}",
                    f"HOST_{variable}",
                )
            )
        target_names.extend(
            (
                "CARGO_ALIAS_ZIGBUILD",
                "CROSS_COMPILE",
                "CRATE_CC_NO_DEFAULTS",
                "CC_SHELL_ESCAPED_FLAGS",
                "CC_KNOWN_WRAPPER_CUSTOM",
                f"CARGO_TARGET_{uppercase}_LINKER",
                f"CARGO_TARGET_{uppercase}_RUSTFLAGS",
                f"CARGO_TARGET_{uppercase}_RUNNER",
            )
        )
        contaminated = os.environ.copy()
        for name in target_names:
            contaminated[name] = "untrusted"
        contaminated["SSRP_ENV_SENTINEL"] = "preserved"
        shell = (
            f"RELEASE_TARGET={TARGET}\n"
            f"{function}\n"
            "run_with_clean_release_environment env\n"
        )
        result = self.run_command(["bash", "-c", shell], env=contaminated)
        cleaned = dict(
            line.split("=", 1) for line in result.stdout.splitlines() if "=" in line
        )
        for name in target_names:
            self.assertNotIn(name, cleaned)
        self.assertEqual(cleaned["SSRP_ENV_SENTINEL"], "preserved")
        for suffix in ("CC", "CFLAGS", "AR", "ARFLAGS", "RANLIBFLAGS"):
            self.assertNotIn(
                f"CARGO_TARGET_X86_64_UNKNOWN_LINUX_MUSL_{suffix}", script
            )
        wrapped_builds = re.findall(
            r'run_with_clean_release_environment \\\n\s+"\$SHADOWSOCKS_RUST_PLUS_ROOT/scripts/release-artifact.py" build-and-receipt',
            script,
        )
        self.assertEqual(len(wrapped_builds), 2)
        self.assertNotRegex(script, r"(?m)^\s*cargo zigbuild(?:\s|$)")
        self.assertIn(
            'expected_prepared_tree_sha256="$(lock_value prepared_tree_sha256)"', script
        )
        self.assertEqual(
            script.count(
                '--expected-prepared-tree-sha256 "$expected_prepared_tree_sha256"'
            ),
            2,
        )

        verifier = VERIFY_ALL_TOOL.read_text(encoding="utf-8")
        self.assertIn(
            'expected_prepared_tree_sha256="$(lock_value prepared_tree_sha256)"',
            verifier,
        )
        self.assertIn(
            '"$SHADOWSOCKS_RUST_PLUS_ROOT/scripts/release-artifact.py" source-tree-sha256',
            verifier,
        )
        self.assertIn(
            '[[ "$actual_prepared_tree_sha256" == "$expected_prepared_tree_sha256" ]]',
            verifier,
        )

    def test_verifier_rejects_binary_tampering(self) -> None:
        with tempfile.TemporaryDirectory(prefix="ssrp-release-") as temporary:
            root = Path(temporary)
            server, auditd = self.fake_release_binaries(root)
            output = root / "output"
            output.mkdir()
            self.package_multi(server, auditd, output)
            binary = output / "shadowsocks-auditd"
            tampered = bytearray(binary.read_bytes())
            tampered[-12] ^= 0x01
            binary.write_bytes(tampered)
            result = self.verify_multi(output, success=False)
            self.assertIn("SHA-256 与 manifest 不一致", result.stderr)

    @unittest.skipUnless(shutil.which("openssl"), "openssl is required")
    def test_detached_signature_is_required_and_verified(self) -> None:
        with tempfile.TemporaryDirectory(prefix="ssrp-release-") as temporary:
            root = Path(temporary)
            server, auditd = self.fake_release_binaries(root)
            output = root / "output"
            output.mkdir()
            self.package_multi(server, auditd, output)
            manifest = output / "release-manifest.json"

            private_key = root / "release-private.pem"
            public_key = root / "release-public.pem"
            signature = output / "release-manifest.sig"
            sign_tool, verify_tool = self.release_script_fixture(root)
            real_git = shutil.which("git")
            real_awk = shutil.which("awk")
            real_openssl = shutil.which("openssl")
            self.assertIsNotNone(real_git)
            self.assertIsNotNone(real_awk)
            self.assertIsNotNone(real_openssl)
            test_bin = root / "test-bin"
            test_bin.mkdir()
            (test_bin / "python3").symlink_to(Path(sys.executable).resolve())
            git_wrapper = test_bin / "git"
            git_wrapper.write_text(
                "#!/bin/sh\n"
                'case " $* " in *" status "*)\n'
                '  if [ -n "${SSRP_GIT_STATUS_COUNT_FILE:-}" ]; then\n'
                '    count=0\n'
                '    [ ! -f "$SSRP_GIT_STATUS_COUNT_FILE" ] || count="$(cat "$SSRP_GIT_STATUS_COUNT_FILE")"\n'
                '    count=$((count + 1))\n'
                '    printf "%s\\n" "$count" > "$SSRP_GIT_STATUS_COUNT_FILE"\n'
                '    if [ "$count" -ge "${SSRP_GIT_DIRTY_ON_STATUS_CALL:-999}" ]; then\n'
                '      printf " M scripts/release-artifact.py\\n"\n'
                '    fi\n'
                '  fi\n'
                '  exit 0\n'
                ';; esac\n'
                f'case " $* " in *" rev-parse HEAD "*) printf "%s\\n" {OVERLAY_COMMIT}; exit 0 ;; esac\n'
                f'case " $* " in *" show -s --format=%ct "*) printf "%s\\n" {EPOCH}; exit 0 ;; esac\n'
                f"exec {shlex.quote(real_git or 'git')} \"$@\"\n",
                encoding="utf-8",
            )
            os.chmod(git_wrapper, 0o755)
            awk_wrapper = test_bin / "awk"
            awk_wrapper.write_text(
                "#!/bin/sh\n"
                'case " $* " in *" key=prepared_tree_sha256 "*) '
                'printf "%s\\n" "$SSRP_TEST_PREPARED_TREE_SHA256"; exit 0 ;; esac\n'
                f"exec {shlex.quote(real_awk or 'awk')} \"$@\"\n",
                encoding="utf-8",
            )
            os.chmod(awk_wrapper, 0o755)
            openssl_wrapper = test_bin / "openssl"
            openssl_wrapper.write_text(
                "#!/bin/sh\n"
                f"{shlex.quote(real_openssl or 'openssl')} \"$@\"\n"
                "status=$?\n"
                'if [ "$status" -eq 0 ] && [ -n "${SSRP_SWAP_MANIFEST_FROM:-}" ]; then\n'
                '  mv -- "$SSRP_SWAP_MANIFEST_FROM" "$SSRP_SWAP_MANIFEST_TO" || exit 98\n'
                "fi\n"
                'if [ "$status" -eq 0 ] && [ -n "${SSRP_SWAP_SIGNATURE_FROM:-}" ]; then\n'
                '  mv -- "$SSRP_SWAP_SIGNATURE_FROM" "$SSRP_SWAP_SIGNATURE_TO" || exit 99\n'
                "fi\n"
                'if [ "$status" -eq 0 ] && [ -n "${SSRP_SWAP_ARTIFACT_FROM:-}" ]; then\n'
                '  mv -- "$SSRP_SWAP_ARTIFACT_FROM" "$SSRP_SWAP_ARTIFACT_TO" || exit 97\n'
                "fi\n"
                'if [ "$status" -eq 0 ] && [ -n "${SSRP_INSERT_EXTRA_MEMBER:-}" ]; then\n'
                '  printf "race\\n" > "$SSRP_INSERT_EXTRA_MEMBER" || exit 96\n'
                "fi\n"
                'exit "$status"\n',
                encoding="utf-8",
            )
            os.chmod(openssl_wrapper, 0o755)
            clean_worktree_env = os.environ.copy()
            clean_worktree_env["PATH"] = f"{test_bin}{os.pathsep}{clean_worktree_env['PATH']}"
            clean_worktree_env["SSRP_TEST_PREPARED_TREE_SHA256"] = json.loads(
                manifest.read_text(encoding="utf-8")
            )["build"]["prepared_tree_sha256"]
            self.run_command(
                [
                    "openssl",
                    "genpkey",
                    "-quiet",
                    "-algorithm",
                    "RSA",
                    "-pkeyopt",
                    "rsa_keygen_bits:2048",
                    "-out",
                    str(private_key),
                ],
                env=clean_worktree_env,
            )

            mismatched_head = self.run_command(
                [
                    str(sign_tool),
                    "--release-manifest",
                    str(manifest),
                    "--private-key",
                    str(private_key),
                    "--output",
                    str(output / "release-manifest.sig"),
                    "--overlay-commit",
                    "b" * 40,
                ],
                success=False,
                env=clean_worktree_env,
            )
            self.assertIn("必须等于当前 HEAD", mismatched_head.stderr)
            self.run_command(
                [
                    "openssl",
                    "pkey",
                    "-in",
                    str(private_key),
                    "-pubout",
                    "-out",
                    str(public_key),
                ],
                env=clean_worktree_env,
            )

            wrong_epoch_root = root / "wrong-epoch"
            wrong_epoch_root.mkdir()
            wrong_server, wrong_auditd = self.fake_release_binaries(
                wrong_epoch_root, epoch=EPOCH + 1
            )
            wrong_output = wrong_epoch_root / "output"
            wrong_output.mkdir()
            self.package_multi(
                wrong_server,
                wrong_auditd,
                wrong_output,
                epoch=EPOCH + 1,
            )
            wrong_epoch = self.run_command(
                [
                    str(sign_tool),
                    "--release-manifest",
                    str(wrong_output / "release-manifest.json"),
                    "--private-key",
                    str(private_key),
                    "--output",
                    str(wrong_output / "release-manifest.sig"),
                    "--overlay-commit",
                    OVERLAY_COMMIT,
                ],
                success=False,
                env=clean_worktree_env,
            )
            self.assertIn("source_date_epoch", wrong_epoch.stderr)

            wrong_source_env = clean_worktree_env.copy()
            wrong_source_env["SSRP_TEST_PREPARED_TREE_SHA256"] = "f" * 64
            wrong_source = self.run_command(
                [
                    str(sign_tool),
                    "--release-manifest",
                    str(manifest),
                    "--private-key",
                    str(private_key),
                    "--output",
                    str(signature),
                    "--overlay-commit",
                    OVERLAY_COMMIT,
                ],
                success=False,
                env=wrong_source_env,
            )
            self.assertIn("prepared_tree_sha256", wrong_source.stderr)
            self.assertFalse(signature.exists())

            self.run_command(
                [
                    str(sign_tool),
                    "--release-manifest",
                    str(manifest),
                    "--private-key",
                    str(private_key),
                    "--output",
                    str(signature),
                    "--overlay-commit",
                    OVERLAY_COMMIT,
                ],
                env=clean_worktree_env,
            )
            result = self.run_command(
                [
                    str(verify_tool),
                    "--release-manifest",
                    str(manifest),
                    "--signature",
                    str(signature),
                    "--public-key",
                    str(public_key),
                    "--overlay-commit",
                    OVERLAY_COMMIT,
                ],
                env=clean_worktree_env,
            )
            self.assertIn("签名验证通过", result.stdout)

            overwrite = self.run_command(
                [
                    str(sign_tool),
                    "--release-manifest",
                    str(manifest),
                    "--private-key",
                    str(private_key),
                    "--output",
                    str(signature),
                    "--overlay-commit",
                    OVERLAY_COMMIT,
                ],
                success=False,
                env=clean_worktree_env,
            )
            self.assertIn("拒绝覆盖", overwrite.stderr)

            original_manifest = manifest.read_bytes()
            replacement_value = json.loads(original_manifest)
            replacement_value["build"]["generated_at"] = "2026-08-26T06:30:01Z"
            replacement_manifest = root / "replacement-manifest.json"

            def write_replacement() -> None:
                replacement_manifest.write_text(
                    json.dumps(
                        replacement_value, ensure_ascii=True, indent=2, sort_keys=True
                    )
                    + "\n",
                    encoding="utf-8",
                )

            swap_env = clean_worktree_env.copy()
            swap_env["SSRP_SWAP_MANIFEST_FROM"] = str(replacement_manifest)
            swap_env["SSRP_SWAP_MANIFEST_TO"] = str(manifest)

            write_replacement()
            swapped_verify = self.run_command(
                [
                    str(verify_tool),
                    "--release-manifest",
                    str(manifest),
                    "--signature",
                    str(signature),
                    "--public-key",
                    str(public_key),
                    "--overlay-commit",
                    OVERLAY_COMMIT,
                ],
                success=False,
                env=swap_env,
            )
            self.assertIn(
                "发布成员在操作期间被替换或修改：release-manifest.json",
                swapped_verify.stderr,
            )
            manifest.write_bytes(original_manifest)

            original_signature = signature.read_bytes()
            replacement_signature = root / "replacement-manifest.sig"
            replacement_signature.write_bytes(b"different signature bytes")
            signature_swap_env = clean_worktree_env.copy()
            signature_swap_env["SSRP_SWAP_SIGNATURE_FROM"] = str(replacement_signature)
            signature_swap_env["SSRP_SWAP_SIGNATURE_TO"] = str(signature)
            swapped_signature = self.run_command(
                [
                    str(verify_tool),
                    "--release-manifest",
                    str(manifest),
                    "--signature",
                    str(signature),
                    "--public-key",
                    str(public_key),
                    "--overlay-commit",
                    OVERLAY_COMMIT,
                ],
                success=False,
                env=signature_swap_env,
            )
            self.assertIn(
                "发布成员在操作期间被替换或修改：release-manifest.sig",
                swapped_signature.stderr,
            )
            signature.write_bytes(original_signature)

            server_artifact = output / "ssserver"
            original_server = server_artifact.read_bytes()
            replacement_server = root / "replacement-ssserver"
            replacement_payload = bytearray(original_server)
            replacement_payload[-1] ^= 0x01
            replacement_server.write_bytes(replacement_payload)
            os.chmod(replacement_server, 0o755)
            artifact_swap_env = clean_worktree_env.copy()
            artifact_swap_env["SSRP_SWAP_ARTIFACT_FROM"] = str(replacement_server)
            artifact_swap_env["SSRP_SWAP_ARTIFACT_TO"] = str(server_artifact)
            swapped_artifact = self.run_command(
                [
                    str(verify_tool),
                    "--release-manifest",
                    str(manifest),
                    "--signature",
                    str(signature),
                    "--public-key",
                    str(public_key),
                    "--overlay-commit",
                    OVERLAY_COMMIT,
                ],
                success=False,
                env=artifact_swap_env,
            )
            self.assertIn(
                "发布成员在操作期间被替换或修改：ssserver",
                swapped_artifact.stderr,
            )
            server_artifact.write_bytes(original_server)
            os.chmod(server_artifact, 0o755)

            extra_member = output / "unbound-race.txt"
            extra_env = clean_worktree_env.copy()
            extra_env["SSRP_INSERT_EXTRA_MEMBER"] = str(extra_member)
            inserted_extra = self.run_command(
                [
                    str(verify_tool),
                    "--release-manifest",
                    str(manifest),
                    "--signature",
                    str(signature),
                    "--public-key",
                    str(public_key),
                    "--overlay-commit",
                    OVERLAY_COMMIT,
                ],
                success=False,
                env=extra_env,
            )
            self.assertIn("发布目录成员集合在操作期间发生变化", inserted_extra.stderr)
            extra_member.unlink()

            signature.unlink()
            write_replacement()
            swapped_sign = self.run_command(
                [
                    str(sign_tool),
                    "--release-manifest",
                    str(manifest),
                    "--private-key",
                    str(private_key),
                    "--output",
                    str(signature),
                    "--overlay-commit",
                    OVERLAY_COMMIT,
                ],
                success=False,
                env=swap_env,
            )
            self.assertIn(
                "发布成员在操作期间被替换或修改：release-manifest.json",
                swapped_sign.stderr,
            )
            self.assertFalse(signature.exists())
            manifest.write_bytes(original_manifest)

            sign_drift_env = clean_worktree_env.copy()
            sign_drift_env["SSRP_GIT_STATUS_COUNT_FILE"] = str(
                root / "sign-git-status-count"
            )
            sign_drift_env["SSRP_GIT_DIRTY_ON_STATUS_CALL"] = "2"
            drifted_sign = self.run_command(
                [
                    str(sign_tool),
                    "--release-manifest",
                    str(manifest),
                    "--private-key",
                    str(private_key),
                    "--output",
                    str(signature),
                    "--overlay-commit",
                    OVERLAY_COMMIT,
                ],
                success=False,
                env=sign_drift_env,
            )
            self.assertIn("工作树在签名期间发生变化", drifted_sign.stderr)
            self.assertFalse(signature.exists())

            self.run_command(
                [
                    str(sign_tool),
                    "--release-manifest",
                    str(manifest),
                    "--private-key",
                    str(private_key),
                    "--output",
                    str(signature),
                    "--overlay-commit",
                    OVERLAY_COMMIT,
                ],
                env=clean_worktree_env,
            )

            verify_drift_env = clean_worktree_env.copy()
            verify_drift_env["SSRP_GIT_STATUS_COUNT_FILE"] = str(
                root / "verify-git-status-count"
            )
            verify_drift_env["SSRP_GIT_DIRTY_ON_STATUS_CALL"] = "2"
            drifted_verify = self.run_command(
                [
                    str(verify_tool),
                    "--release-manifest",
                    str(manifest),
                    "--signature",
                    str(signature),
                    "--public-key",
                    str(public_key),
                    "--overlay-commit",
                    OVERLAY_COMMIT,
                ],
                success=False,
                env=verify_drift_env,
            )
            self.assertIn("工作树在验签期间发生变化", drifted_verify.stderr)
            self.assertTrue(signature.exists())

            manifest.write_bytes(manifest.read_bytes() + b" ")
            failed = self.run_command(
                [
                    str(verify_tool),
                    "--release-manifest",
                    str(manifest),
                    "--signature",
                    str(signature),
                    "--public-key",
                    str(public_key),
                    "--overlay-commit",
                    OVERLAY_COMMIT,
                ],
                success=False,
                env=clean_worktree_env,
            )
            self.assertIn("签名验证失败", failed.stderr)


if __name__ == "__main__":
    unittest.main()
