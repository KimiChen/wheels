#!/usr/bin/env python3

from __future__ import annotations

import hashlib
import json
import os
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
TARGET = "x86_64-unknown-linux-musl"
EPOCH = 1_787_725_800


class ReleaseArtifactTest(unittest.TestCase):
    def run_command(
        self, command: list[str], *, success: bool = True
    ) -> subprocess.CompletedProcess[str]:
        result = subprocess.run(
            command,
            cwd=PROJECT_ROOT,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
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

    def package(self, binary: Path, output: Path, *, success: bool = True) -> subprocess.CompletedProcess[str]:
        return self.run_command(
            [
                sys.executable,
                str(ARTIFACT_TOOL),
                "package",
                "--binary",
                str(binary),
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
                "--zlib-version",
                "1.2.12",
            ],
            success=success,
        )

    def package_multi(
        self,
        server_binary: Path,
        auditd_binary: Path,
        output: Path,
        *,
        success: bool = True,
    ) -> subprocess.CompletedProcess[str]:
        return self.run_command(
            [
                sys.executable,
                str(ARTIFACT_TOOL),
                "package-multi",
                "--binary",
                str(server_binary),
                "--auditd-binary",
                str(auditd_binary),
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
                "--zlib-version",
                "1.2.12",
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
                "--expected-zlib-version",
                "1.2.12",
            ],
            success=success,
        )

    def artifact_paths(self, output: Path) -> tuple[Path, Path, Path]:
        stem = f"shadowsocks-rust-plus-{VERSION}-{TARGET}"
        archive = output / f"{stem}.tar.gz"
        manifest = output / f"{stem}.manifest.json"
        checksum = output / f"{stem}.tar.gz.sha256"
        return archive, manifest, checksum

    def verify_artifact(
        self, archive: Path, manifest: Path, checksum: Path, *, success: bool = True
    ) -> subprocess.CompletedProcess[str]:
        return self.run_command(
            [
                sys.executable,
                str(ARTIFACT_TOOL),
                "verify",
                "--archive",
                str(archive),
                "--manifest",
                str(manifest),
                "--checksum",
                str(checksum),
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
                "--expected-zlib-version",
                "1.2.12",
            ],
            success=success,
        )

    def test_packaging_is_byte_reproducible_and_verifiable(self) -> None:
        with tempfile.TemporaryDirectory(prefix="ssrp-release-") as temporary:
            root = Path(temporary)
            binary = self.fake_elf(root)
            first = root / "first"
            second = root / "second"
            first.mkdir()
            second.mkdir()
            self.package(binary, first)
            self.package(binary, second)
            first_paths = self.artifact_paths(first)
            second_paths = self.artifact_paths(second)
            for left, right in zip(first_paths, second_paths):
                self.assertEqual(left.read_bytes(), right.read_bytes())
            result = self.verify_artifact(*first_paths)
            self.assertIn("ELF x86_64", result.stdout)
            wrong_toolchain = self.run_command(
                [
                    sys.executable,
                    str(ARTIFACT_TOOL),
                    "verify",
                    "--archive",
                    str(first_paths[0]),
                    "--manifest",
                    str(first_paths[1]),
                    "--checksum",
                    str(first_paths[2]),
                    "--expected-zig-version",
                    "0.15.0",
                ],
                success=False,
            )
            self.assertIn("toolchain.zig_version", wrong_toolchain.stderr)

    def test_packaging_refuses_overwrite_and_preserves_artifacts(self) -> None:
        with tempfile.TemporaryDirectory(prefix="ssrp-release-") as temporary:
            root = Path(temporary)
            binary = self.fake_elf(root)
            output = root / "output"
            output.mkdir()
            self.package(binary, output)
            paths = self.artifact_paths(output)
            originals = [path.read_bytes() for path in paths]
            result = self.package(binary, output, success=False)
            self.assertIn("拒绝覆盖", result.stderr)
            self.assertEqual([path.read_bytes() for path in paths], originals)

    def test_manifest_binds_patch_series(self) -> None:
        with tempfile.TemporaryDirectory(prefix="ssrp-release-series-") as temporary:
            root = Path(temporary)
            binary = self.fake_elf(root)
            output = root / "output"
            output.mkdir()
            self.package(binary, output)
            archive, manifest, checksum = self.artifact_paths(output)
            value = json.loads(manifest.read_text(encoding="utf-8"))
            value["patch_series"][0]["sha256"] = "0" * 64
            manifest.write_text(json.dumps(value, sort_keys=True, indent=2) + "\n", encoding="utf-8")
            self.verify_artifact(archive, manifest, checksum, success=False)

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

    def test_verifier_rejects_archive_tampering(self) -> None:
        with tempfile.TemporaryDirectory(prefix="ssrp-release-") as temporary:
            root = Path(temporary)
            binary = self.fake_elf(root)
            output = root / "output"
            output.mkdir()
            self.package(binary, output)
            archive, manifest, checksum = self.artifact_paths(output)
            tampered = bytearray(archive.read_bytes())
            tampered[-12] ^= 0x01
            archive.write_bytes(tampered)
            result = self.verify_artifact(archive, manifest, checksum, success=False)
            self.assertIn("SHA-256 校验失败", result.stderr)

    @unittest.skipUnless(shutil.which("openssl"), "openssl is required")
    def test_detached_signature_is_required_and_verified(self) -> None:
        with tempfile.TemporaryDirectory(prefix="ssrp-release-") as temporary:
            root = Path(temporary)
            binary = self.fake_elf(root)
            output = root / "output"
            output.mkdir()
            self.package(binary, output)
            archive, manifest, checksum = self.artifact_paths(output)

            private_key = root / "release-private.pem"
            public_key = root / "release-public.pem"
            signature = output / "manifest.sig"
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
                ]
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
                ]
            )
            self.run_command(
                [
                    str(SIGN_TOOL),
                    "--archive",
                    str(archive),
                    "--manifest",
                    str(manifest),
                    "--checksum",
                    str(checksum),
                    "--private-key",
                    str(private_key),
                    "--output",
                    str(signature),
                    "--overlay-commit",
                    OVERLAY_COMMIT,
                ]
            )
            result = self.run_command(
                [
                    str(VERIFY_TOOL),
                    "--archive",
                    str(archive),
                    "--manifest",
                    str(manifest),
                    "--checksum",
                    str(checksum),
                    "--signature",
                    str(signature),
                    "--public-key",
                    str(public_key),
                    "--overlay-commit",
                    OVERLAY_COMMIT,
                ]
            )
            self.assertIn("签名验证通过", result.stdout)

            overwrite = self.run_command(
                [
                    str(SIGN_TOOL),
                    "--archive",
                    str(archive),
                    "--manifest",
                    str(manifest),
                    "--checksum",
                    str(checksum),
                    "--private-key",
                    str(private_key),
                    "--output",
                    str(signature),
                    "--overlay-commit",
                    OVERLAY_COMMIT,
                ],
                success=False,
            )
            self.assertIn("拒绝覆盖", overwrite.stderr)

            manifest.write_bytes(manifest.read_bytes() + b" ")
            failed = self.run_command(
                [
                    str(VERIFY_TOOL),
                    "--archive",
                    str(archive),
                    "--manifest",
                    str(manifest),
                    "--checksum",
                    str(checksum),
                    "--signature",
                    str(signature),
                    "--public-key",
                    str(public_key),
                    "--overlay-commit",
                    OVERLAY_COMMIT,
                ],
                success=False,
            )
            self.assertIn("签名验证失败", failed.stderr)


if __name__ == "__main__":
    unittest.main()
