#!/usr/bin/env python3

from __future__ import annotations

import hashlib
import json
import os
import shlex
import shutil
import struct
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path


PROJECT_ROOT = Path(__file__).resolve().parent.parent
ARTIFACT_TOOL = PROJECT_ROOT / "scripts" / "release-artifact.py"
SIGN_TOOL = PROJECT_ROOT / "scripts" / "sign-release.sh"
VERIFY_TOOL = PROJECT_ROOT / "scripts" / "verify-release.sh"
VERSION = "v1.24.0"
UPSTREAM_COMMIT = "7ee1aa9223ed8f4d34734aac919036c8ad4502c2"
OVERLAY_COMMIT = "a" * 40
EPOCH = 1_787_725_800


class ReleaseArtifactTest(unittest.TestCase):
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

    def fake_elf(self, directory: Path) -> Path:
        payload = bytearray(256)
        payload[:4] = b"\x7fELF"
        payload[4] = 2
        payload[5] = 1
        payload[6] = 1
        struct.pack_into("<H", payload, 16, 2)
        struct.pack_into("<H", payload, 18, 62)
        payload[64:] = hashlib.sha256(b"deterministic-ssserver-fixture").digest() * 6
        binary = directory / "ssserver"
        binary.write_bytes(payload)
        os.chmod(binary, 0o755)
        return binary

    def fake_release_binaries(self, directory: Path) -> tuple[Path, Path]:
        server = self.fake_elf(directory)
        auditd = directory / "shadowsocks-auditd"
        auditd.write_bytes(server.read_bytes())
        os.chmod(auditd, 0o755)
        return server, auditd

    def package_multi(
        self,
        server_binary: Path,
        auditd_binary: Path,
        output: Path,
        *,
        success: bool = True,
    ) -> subprocess.CompletedProcess[str]:
        second_server = server_binary.parent / f".{server_binary.name}.independent"
        second_auditd = auditd_binary.parent / f".{auditd_binary.name}.independent"
        shutil.copyfile(server_binary, second_server)
        shutil.copyfile(auditd_binary, second_auditd)
        os.chmod(second_server, 0o755)
        os.chmod(second_auditd, 0o755)
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
                "--output-dir",
                str(output),
                "--version",
                VERSION,
                "--upstream-commit",
                UPSTREAM_COMMIT,
                "--overlay-commit",
                OVERLAY_COMMIT,
                "--source-date-epoch",
                str(EPOCH),
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
                "3.14.6",
            ],
            success=success,
        )

    def verify_multi(self, output: Path, *, success: bool = True) -> subprocess.CompletedProcess[str]:
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
                "3.14.6",
            ],
            success=success,
        )

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

    def test_single_artifact_commands_are_not_available(self) -> None:
        for command in ("package", "verify"):
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
            input_dir = root / "input"
            input_dir.mkdir()
            server = self.fake_elf(input_dir)
            auditd = input_dir / "shadowsocks-auditd"
            auditd.write_bytes(server.read_bytes())
            os.chmod(auditd, 0o755)
            output = root / "output"
            output.mkdir()
            self.package_multi(server, auditd, output)
            self.verify_multi(output)
            expected = {
                "ssserver",
                "ssserver.sha256",
                "shadowsocks-auditd",
                "shadowsocks-auditd.sha256",
                "release-manifest.json",
            }
            self.assertEqual({path.name for path in output.iterdir()}, expected)
            manifest = json.loads((output / "release-manifest.json").read_text(encoding="utf-8"))
            build = manifest["build"]
            self.assertEqual(build["generated_at"], "2026-08-26T06:30:00Z")
            self.assertEqual(build["independent_builds"]["count"], 2)
            self.assertEqual(
                [item["name"] for item in build["independent_builds"]["artifacts"]],
                ["ssserver", "shadowsocks-auditd"],
            )
            for item in build["independent_builds"]["artifacts"]:
                self.assertEqual(item["first"]["path"], f"build-a/{item['name']}")
                self.assertEqual(item["second"]["path"], f"build-b/{item['name']}")
                self.assertEqual(item["first"]["sha256"], next(a["sha256"] for a in manifest["artifacts"] if a["name"] == item["name"]))
                self.assertEqual(item["second"]["sha256"], item["first"]["sha256"])
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
            input_dir = root / "input"
            input_dir.mkdir()
            server = self.fake_elf(input_dir)
            auditd = input_dir / "shadowsocks-auditd"
            auditd.write_bytes(server.read_bytes())
            os.chmod(auditd, 0o755)
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
            value["build"]["independent_builds"]["artifacts"][0]["second"]["sha256"] = "0" * 64
            manifest.write_text(json.dumps(value, sort_keys=True, indent=2) + "\n", encoding="utf-8")
            self.verify_multi(output, success=False)
            manifest.write_bytes(original_manifest)

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
            real_git = shutil.which("git")
            self.assertIsNotNone(real_git)
            test_bin = root / "test-bin"
            test_bin.mkdir()
            git_wrapper = test_bin / "git"
            git_wrapper.write_text(
                "#!/bin/sh\n"
                'case " $* " in *" status "*) exit 0 ;; esac\n'
                f"exec {shlex.quote(real_git or 'git')} \"$@\"\n",
                encoding="utf-8",
            )
            os.chmod(git_wrapper, 0o755)
            clean_worktree_env = os.environ.copy()
            clean_worktree_env["PATH"] = f"{test_bin}{os.pathsep}{clean_worktree_env['PATH']}"
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
            self.run_command(
                [
                    str(SIGN_TOOL),
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
                    str(VERIFY_TOOL),
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
                    str(SIGN_TOOL),
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

            manifest.write_bytes(manifest.read_bytes() + b" ")
            failed = self.run_command(
                [
                    str(VERIFY_TOOL),
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
