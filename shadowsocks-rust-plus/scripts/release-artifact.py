#!/usr/bin/env python3
"""Create and verify deterministic Linux x86_64 dual-binary release directories."""

from __future__ import annotations

import argparse
from dataclasses import dataclass
from datetime import datetime, timezone
import hashlib
import json
import os
import platform
import re
import shutil
import stat
import struct
import subprocess
import sys
import tempfile
from pathlib import Path
from typing import Any


TARGET = "x86_64-unknown-linux-musl"
MAX_BINARY_BYTES = 256 * 1024 * 1024
MAX_MANIFEST_BYTES = 256 * 1024
MAX_RECEIPT_BYTES = 256 * 1024
COMMIT_PATTERN = re.compile(r"[0-9a-f]{40}\Z")
SHA256_PATTERN = re.compile(r"[0-9a-f]{64}\Z")
VERSION_PATTERN = re.compile(r"v[0-9A-Za-z][0-9A-Za-z.+_-]{0,63}\Z")
TOOL_VERSION_PATTERN = re.compile(r"[0-9]+(?:\.[0-9A-Za-z+-]+)+\Z")
BUILD_IDS = ("build-a", "build-b")
# Recorded verbatim in the receipt: they are decided by the build host and they
# decide which toolchain binaries the build actually resolves.
RECIPE_HOST_ENVIRONMENT = ("PATH", "RUSTUP_HOME")
RECIPE_OPTIONAL_ENVIRONMENT = ("RUSTUP_HOME",)
EVIDENCE_BOUNDARY = {
    "packager_checks": [
        "cargo-zigbuild-command-execution",
        "cargo-zigbuild-version-execution-binding",
        "declared-toolchain-version-execution-binding",
        "pinned-prepared-source-tree-digest",
        "sanitized-cargo-rust-and-cc-environment",
        "source-root-working-directory",
        "isolated-empty-cargo-home-and-config-search",
        "empty-target-root-before-build",
        "distinct-live-source-and-target-roots",
        "live-source-tree-and-artifact-digests",
    ],
    "verifier_checks": [
        "canonical-receipt-and-manifest-hash-binding",
        "pinned-prepared-source-tree-digest",
        "source-tree-digest-equality-between-builds",
        "artifact-byte-equality-and-digest-binding",
        "declared-toolchain-and-recipe-consistency",
    ],
    "not_attested": [
        "malicious-build-host-or-builder-resistance",
        "cryptographic-command-execution-attestation",
        "tool-binary-identity-or-trusted-builder-identity",
        "complete-host-environment-or-cargo-config-attestation",
        "trusted-build-timestamp",
        "separate-host-or-process-isolation",
    ],
}


class ArtifactError(RuntimeError):
    """Expected validation or packaging failure."""


@dataclass(frozen=True)
class FileSnapshot:
    payload: bytes
    mode: int
    identity: tuple[int, int]


@dataclass(frozen=True)
class ReleaseDirectorySnapshot:
    directory_identity: tuple[int, int]
    files: dict[str, FileSnapshot]


def _strict_object(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise ArtifactError(f"manifest 包含重复字段：{key}")
        result[key] = value
    return result


def _sha256(payload: bytes) -> str:
    return hashlib.sha256(payload).hexdigest()


def _patch_series() -> list[dict[str, str]]:
    series_path = Path(__file__).resolve().parent.parent / "patches" / "series"
    try:
        lines = series_path.read_text(encoding="utf-8").splitlines()
    except OSError as exc:
        raise ArtifactError(f"无法读取补丁 series：{exc}") from exc
    series = [line.strip() for line in lines if line.strip() and not line.lstrip().startswith("#")]
    if not series or any("/" in item or "\\" in item for item in series):
        raise ArtifactError("补丁 series 为空或包含非法路径")
    if len(series) != len(set(series)):
        raise ArtifactError("补丁 series 包含重复项")
    for item in series:
        if not (series_path.parent / item).is_file():
            raise ArtifactError(f"补丁 series 引用不存在文件：{item}")
    return [
        {"name": item, "sha256": _sha256((series_path.parent / item).read_bytes())}
        for item in series
    ]


def _read_regular(path: Path, limit: int) -> tuple[bytes, os.stat_result]:
    flags = os.O_RDONLY
    if hasattr(os, "O_CLOEXEC"):
        flags |= os.O_CLOEXEC
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    try:
        fd = os.open(path, flags)
    except OSError as exc:
        raise ArtifactError(f"无法安全打开 {path}：{exc.strerror}") from exc
    try:
        metadata = os.fstat(fd)
        if not stat.S_ISREG(metadata.st_mode):
            raise ArtifactError(f"路径不是普通文件：{path}")
        chunks: list[bytes] = []
        remaining = limit + 1
        while remaining:
            chunk = os.read(fd, min(remaining, 1024 * 1024))
            if not chunk:
                break
            chunks.append(chunk)
            remaining -= len(chunk)
        payload = b"".join(chunks)
    finally:
        os.close(fd)
    if len(payload) > limit:
        raise ArtifactError(f"文件超过 {limit} 字节上限：{path}")
    return payload, metadata


def _require_elf_x86_64(payload: bytes, label: str) -> None:
    if len(payload) < 64 or payload[:4] != b"\x7fELF":
        raise ArtifactError(f"{label} 不是 ELF")
    if payload[4] != 2:
        raise ArtifactError(f"{label} 不是 ELF64")
    if payload[5] != 1:
        raise ArtifactError(f"{label} 不是 little-endian ELF")
    if payload[6] != 1:
        raise ArtifactError(f"{label} 的 ELF ident version 错误")
    (
        elf_type,
        machine,
        elf_version,
        entry,
        program_offset,
        _section_offset,
        _flags,
        header_size,
        program_entry_size,
        program_count,
        _section_entry_size,
        _section_count,
        _section_name_index,
    ) = struct.unpack_from("<HHIQQQIHHHHHH", payload, 16)
    if elf_type not in (2, 3):
        raise ArtifactError(f"{label} 不是 ELF executable/shared-object")
    if machine != 62:
        raise ArtifactError(f"{label} 的 ELF machine 不是 x86_64")
    if elf_version != 1 or header_size != 64:
        raise ArtifactError(f"{label} 的 ELF header 无效")
    if program_count < 1 or program_entry_size != 56 or program_offset < header_size:
        raise ArtifactError(f"{label} 缺少有效 program header table")
    program_table_end = program_offset + program_entry_size * program_count
    if program_table_end > len(payload):
        raise ArtifactError(f"{label} 的 program header table 越界")
    executable_load = False
    entry_in_executable_load = False
    for index in range(program_count):
        offset = program_offset + index * program_entry_size
        (
            segment_type,
            segment_flags,
            file_offset,
            virtual_address,
            _physical_address,
            file_size,
            memory_size,
            alignment,
        ) = struct.unpack_from("<IIQQQQQQ", payload, offset)
        if segment_type != 1:
            continue
        if file_size > memory_size or file_offset + file_size > len(payload):
            raise ArtifactError(f"{label} 的 PT_LOAD 文件范围无效")
        if alignment not in (0, 1) and alignment & (alignment - 1):
            raise ArtifactError(f"{label} 的 PT_LOAD alignment 无效")
        if segment_flags & 0x1 and file_size > 0:
            executable_load = True
            if virtual_address <= entry < virtual_address + file_size:
                entry_in_executable_load = True
    if not executable_load or not entry_in_executable_load:
        raise ArtifactError(f"{label} 缺少包含 entry point 的 executable PT_LOAD")


def _exact_keys(value: dict[str, Any], expected: set[str], label: str) -> None:
    actual = set(value)
    if actual != expected:
        missing = sorted(expected - actual)
        unknown = sorted(actual - expected)
        detail: list[str] = []
        if missing:
            detail.append("缺少 " + ", ".join(missing))
        if unknown:
            detail.append("未知 " + ", ".join(unknown))
        raise ArtifactError(f"{label} 字段错误（{'；'.join(detail)}）")


def _canonical_manifest(value: dict[str, Any]) -> bytes:
    return (json.dumps(value, ensure_ascii=True, indent=2, sort_keys=True) + "\n").encode(
        "utf-8"
    )


def _exclusive_write(path: Path, payload: bytes, mode: int) -> None:
    if not path.parent.is_dir():
        raise ArtifactError(f"输出目录不存在：{path.parent}")
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
    if hasattr(os, "O_CLOEXEC"):
        flags |= os.O_CLOEXEC
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    previous_umask = os.umask(0o022)
    fd = -1
    created = False
    try:
        try:
            fd = os.open(path, flags, mode)
        except FileExistsError as exc:
            raise ArtifactError(f"输出已存在，拒绝覆盖：{path}") from exc
        created = True
        view = memoryview(payload)
        while view:
            written = os.write(fd, view)
            view = view[written:]
        os.fchmod(fd, mode)
        os.fsync(fd)
    except Exception:
        if fd >= 0:
            os.close(fd)
            fd = -1
        if created:
            try:
                path.unlink()
            except OSError:
                pass
        raise
    finally:
        os.umask(previous_umask)
        if fd >= 0:
            os.close(fd)


def _exclusive_write_at(
    directory_fd: int, name: str, payload: bytes, mode: int
) -> tuple[int, int]:
    if not name or "/" in name or name in (".", ".."):
        raise ArtifactError(f"输出文件名非法：{name}")
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
    if hasattr(os, "O_CLOEXEC"):
        flags |= os.O_CLOEXEC
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    previous_umask = os.umask(0o022)
    fd = -1
    created = False
    try:
        try:
            fd = os.open(name, flags, mode, dir_fd=directory_fd)
        except FileExistsError as exc:
            raise ArtifactError(f"输出已存在，拒绝覆盖：{name}") from exc
        created = True
        view = memoryview(payload)
        while view:
            written = os.write(fd, view)
            view = view[written:]
        os.fchmod(fd, mode)
        os.fsync(fd)
        metadata = os.fstat(fd)
        return metadata.st_dev, metadata.st_ino
    except Exception:
        if fd >= 0:
            os.close(fd)
            fd = -1
        if created:
            try:
                os.unlink(name, dir_fd=directory_fd)
            except OSError:
                pass
        raise
    finally:
        os.umask(previous_umask)
        if fd >= 0:
            os.close(fd)


MULTI_ARTIFACTS = ("ssserver", "shadowsocks-auditd")
UNSIGNED_RELEASE_FILES = frozenset(
    {
        "ssserver",
        "ssserver.sha256",
        "shadowsocks-auditd",
        "shadowsocks-auditd.sha256",
        "build-a.receipt.json",
        "build-b.receipt.json",
        "release-manifest.json",
    }
)


def _toolchain_metadata(args: argparse.Namespace) -> dict[str, str]:
    return {
        "rustc_version": args.rustc_version,
        "rustc_commit": args.rustc_commit,
        "cargo_version": args.cargo_version,
        "cargo_zigbuild_version": args.cargo_zigbuild_version,
        "zig_version": args.zig_version,
        "python_version": args.python_version,
    }


def _validate_toolchain(toolchain: dict[str, Any], label: str) -> None:
    expected = {
        "rustc_version",
        "rustc_commit",
        "cargo_version",
        "cargo_zigbuild_version",
        "zig_version",
        "python_version",
    }
    _exact_keys(toolchain, expected, label)
    for field, item in toolchain.items():
        if not isinstance(item, str) or not item or len(item) > 256 or "\n" in item:
            raise ArtifactError(f"{label}.{field} 格式错误")
    if not COMMIT_PATTERN.fullmatch(toolchain["rustc_commit"]):
        raise ArtifactError(f"{label}.rustc_commit 格式错误")
    for field in expected - {"rustc_commit"}:
        if not TOOL_VERSION_PATTERN.fullmatch(toolchain[field]):
            raise ArtifactError(f"{label}.{field} 版本格式错误")


def _validate_common_build_values(
    version: str,
    upstream_commit: str,
    overlay_commit: str,
    source_date_epoch: int,
) -> None:
    if not isinstance(version, str) or not VERSION_PATTERN.fullmatch(version):
        raise ArtifactError("--version 格式错误")
    for label, commit in (
        ("--upstream-commit", upstream_commit),
        ("--overlay-commit", overlay_commit),
    ):
        if not isinstance(commit, str) or not COMMIT_PATTERN.fullmatch(commit):
            raise ArtifactError(f"{label} 必须是 40 位小写十六进制 commit")
    if type(source_date_epoch) is not int or not 1 <= source_date_epoch <= 0xFFFFFFFF:
        raise ArtifactError("--source-date-epoch 必须在 1..4294967295 范围内")


def _validate_prepared_tree_sha256(value: str, label: str) -> None:
    if not isinstance(value, str) or not SHA256_PATTERN.fullmatch(value):
        raise ArtifactError(f"{label} 必须是 64 位小写十六进制 SHA-256")


def _tree_sha256(root: Path) -> str:
    """Hash a prepared source tree without following links or host paths."""
    root = root.resolve(strict=True)
    if not root.is_dir():
        raise ArtifactError(f"源码根不是目录：{root}")
    digest = hashlib.sha256()
    entries = sorted(root.rglob("*"), key=lambda path: path.relative_to(root).as_posix())
    for path in entries:
        relative = path.relative_to(root).as_posix().encode("utf-8")
        metadata = path.lstat()
        if stat.S_ISDIR(metadata.st_mode):
            kind = b"directory"
            payload = b""
        elif stat.S_ISREG(metadata.st_mode):
            kind = b"file"
            payload, _ = _read_regular(path, MAX_BINARY_BYTES)
        elif stat.S_ISLNK(metadata.st_mode):
            kind = b"symlink"
            payload = os.readlink(path).encode("utf-8")
        else:
            raise ArtifactError(f"源码树包含不支持的文件类型：{path}")
        executable = b"1" if metadata.st_mode & 0o111 else b"0"
        for item in (kind, relative, executable, str(len(payload)).encode("ascii"), payload):
            digest.update(str(len(item)).encode("ascii"))
            digest.update(b":")
            digest.update(item)
            digest.update(b"\0")
    return digest.hexdigest()


def _encoded_rustflags(source_root: str, target_root: str, cargo_home: str) -> str:
    return "\x1f".join(
        (
            f"--remap-path-prefix={source_root}=/usr/src/shadowsocks-rust",
            f"--remap-path-prefix={target_root}=/usr/src/target",
            f"--remap-path-prefix={cargo_home}=/usr/local/cargo",
            "-C",
            "link-arg=-Wl,--build-id=none",
            "-C",
            "strip=symbols",
        )
    )


def _recipe_declaration() -> dict[str, Any]:
    """Pinned part of the recipe: it never depends on the build host."""
    script_path = Path(__file__).resolve().parent / "build-linux-release.sh"
    script_payload, _ = _read_regular(script_path, MAX_MANIFEST_BYTES)
    return {
        "id": "linux-musl-user-audit-v2",
        "script": "scripts/build-linux-release.sh",
        "script_sha256": _sha256(script_payload),
        "working_directory": "{source_root}",
        "command_template": [
            "cargo-zigbuild",
            "zigbuild",
            "--manifest-path",
            "{source_root}/Cargo.toml",
            "--locked",
            "--release",
            "--target",
            TARGET,
            "--features",
            "user-audit",
            "--bin",
            "ssserver",
            "--bin",
            "shadowsocks-auditd",
        ],
    }


def _recipe_environment_template(source_date_epoch: int) -> dict[str, str]:
    """Environment variables the release recipe pins to a fixed value."""
    return {
        "CARGO_ENCODED_RUSTFLAGS": _encoded_rustflags(
            "{source_root}", "{target_root}", "{cargo_home}"
        ),
        "CARGO_HOME": "{cargo_home}",
        "CARGO_INCREMENTAL": "0",
        "CARGO_PROFILE_RELEASE_INCREMENTAL": "false",
        "CARGO_TARGET_DIR": "{target_root}",
        "HOME": "{cargo_home}",
        "LANG": "C",
        "LC_ALL": "C",
        "SHADOWSOCKS_BUILD_TIME_UTC": _epoch_timestamp(source_date_epoch),
        "SOURCE_DATE_EPOCH": str(source_date_epoch),
        "TZ": "UTC",
        "ZERO_AR_DATE": "1",
    }


def _observed_recipe_environment(
    environment: dict[str, str],
    source_root: Path,
    target_root: Path,
    cargo_home: Path,
) -> dict[str, str]:
    """Record the environment the build actually ran with.

    Only the three per-build absolute roots are folded back into the recipe
    placeholders, so both independent builds record the same recipe while the
    host-determined PATH and RUSTUP_HOME — which decide *which* toolchain
    binaries the build resolves — are recorded verbatim.
    """
    placeholders = sorted(
        (
            (str(source_root), "{source_root}"),
            (str(target_root), "{target_root}"),
            (str(cargo_home), "{cargo_home}"),
        ),
        key=lambda item: len(item[0]),
        reverse=True,
    )
    observed: dict[str, str] = {}
    for key, value in environment.items():
        for actual, placeholder in placeholders:
            value = value.replace(actual, placeholder)
        observed[key] = value
    return observed


def _observed_execution_command(command: list[str], source_root: Path) -> list[str]:
    """Record the argv actually executed, keeping the resolved tool path."""
    return [
        command[0],
        *(item.replace(str(source_root), "{source_root}") for item in command[1:]),
    ]


def _recipe_metadata(environment: dict[str, str]) -> dict[str, Any]:
    return {**_recipe_declaration(), "environment": dict(environment)}


def _validate_recipe(recipe: Any, source_date_epoch: int) -> None:
    if not isinstance(recipe, dict):
        raise ArtifactError("build receipt recipe 必须是对象")
    declaration = _recipe_declaration()
    _exact_keys(recipe, set(declaration) | {"environment"}, "build receipt recipe")
    for key, expected in declaration.items():
        if recipe[key] != expected:
            raise ArtifactError("build receipt recipe 与当前发布脚本不一致")
    environment = recipe["environment"]
    if not isinstance(environment, dict):
        raise ArtifactError("build receipt recipe.environment 必须是对象")
    template = _recipe_environment_template(source_date_epoch)
    _exact_keys(
        environment,
        set(template) | {"PATH"} | (set(environment) & set(RECIPE_OPTIONAL_ENVIRONMENT)),
        "build receipt recipe.environment",
    )
    for key, expected in template.items():
        if environment[key] != expected:
            raise ArtifactError(f"build receipt recipe.environment.{key} 与发布配方不一致")
    for key in RECIPE_HOST_ENVIRONMENT:
        if key in environment and (
            not isinstance(environment[key], str) or not environment[key]
        ):
            raise ArtifactError(f"build receipt recipe.environment.{key} 必须是非空字符串")


def _absolute_tool_path(candidate: Path) -> Path:
    """Absolutise a PATH hit without collapsing a proxy symlink.

    `Path.resolve()` would rewrite rustup's `bin/cargo -> rustup` link to the
    proxy binary, and the proxy picks its behaviour from argv[0]; executing the
    resolved path runs `rustup` instead of `cargo`.  Only the directory part is
    resolved so the result stays absolute and symlinked *directories* still
    cannot hide the tool.
    """

    if candidate.is_absolute():
        return candidate.parent.resolve(strict=True) / candidate.name
    return candidate.resolve(strict=True)


def _resolve_cargo_zigbuild(environment: dict[str, str]) -> Path:
    resolved = shutil.which("cargo-zigbuild", path=environment.get("PATH"))
    if resolved is None:
        raise ArtifactError("PATH 中找不到 cargo-zigbuild")
    try:
        # Keep the PATH spelling: rustup ships `cargo`/`rustc` as symlinks to the
        # `rustup` proxy, which decides which tool to be from argv[0].  Collapsing
        # the link would execute `rustup` itself.  `stat()` still follows the link,
        # so the regular-file, exec-bit and inode checks describe the real target.
        executable = _absolute_tool_path(Path(resolved))
        metadata = executable.stat()
    except OSError as exc:
        raise ArtifactError(f"无法解析 cargo-zigbuild：{exc}") from exc
    if not executable.is_absolute() or not stat.S_ISREG(metadata.st_mode):
        raise ArtifactError("cargo-zigbuild 必须解析为绝对普通文件")
    if metadata.st_mode & 0o111 == 0:
        raise ArtifactError("cargo-zigbuild 没有可执行位")
    return executable


def _verify_cargo_zigbuild_version(
    executable: Path,
    expected_version: str,
    environment: dict[str, str],
    source_root: Path,
) -> tuple[int, int]:
    try:
        before = executable.stat()
        completed = subprocess.run(
            [str(executable), "--version"],
            env=environment,
            cwd=source_root,
            check=False,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        )
        after = executable.stat()
    except OSError as exc:
        raise ArtifactError(f"无法执行 cargo-zigbuild --version：{exc}") from exc
    if (before.st_dev, before.st_ino) != (after.st_dev, after.st_ino):
        raise ArtifactError("cargo-zigbuild 在版本校验期间被替换")
    if completed.returncode != 0:
        raise ArtifactError(
            f"cargo-zigbuild --version 失败，exit code={completed.returncode}"
        )
    match = re.fullmatch(r"cargo-zigbuild ([^\s]+)", completed.stdout.strip())
    if match is None or not TOOL_VERSION_PATTERN.fullmatch(match.group(1)):
        raise ArtifactError("cargo-zigbuild --version 输出格式错误")
    actual_version = match.group(1)
    if actual_version != expected_version:
        raise ArtifactError(
            "cargo-zigbuild 版本与 build receipt toolchain 不一致："
            f"期望 {expected_version}，实际 {actual_version}"
        )
    return before.st_dev, before.st_ino


def _resolve_build_tool(name: str, environment: dict[str, str]) -> Path:
    resolved = shutil.which(name, path=environment.get("PATH"))
    if resolved is None:
        raise ArtifactError(f"PATH 中找不到 {name}")
    try:
        # See _resolve_cargo_zigbuild: preserve the invoked name so rustup proxies
        # dispatch correctly, while stat() still describes the real target.
        executable = _absolute_tool_path(Path(resolved))
        metadata = executable.stat()
    except OSError as exc:
        raise ArtifactError(f"无法解析 {name}：{exc}") from exc
    if not executable.is_absolute() or not stat.S_ISREG(metadata.st_mode):
        raise ArtifactError(f"{name} 必须解析为绝对普通文件")
    if metadata.st_mode & 0o111 == 0:
        raise ArtifactError(f"{name} 没有可执行位")
    return executable


def _run_tool_version(
    executable: Path,
    arguments: list[str],
    environment: dict[str, str],
    source_root: Path,
    label: str,
) -> tuple[str, tuple[int, int]]:
    try:
        before = executable.stat()
        completed = subprocess.run(
            [str(executable), *arguments],
            env=environment,
            cwd=source_root,
            check=False,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        )
        after = executable.stat()
    except OSError as exc:
        raise ArtifactError(f"无法执行 {label} 版本校验：{exc}") from exc
    identity = (before.st_dev, before.st_ino)
    if identity != (after.st_dev, after.st_ino):
        raise ArtifactError(f"{label} 在版本校验期间被替换")
    if completed.returncode != 0:
        raise ArtifactError(f"{label} 版本校验失败，exit code={completed.returncode}")
    return completed.stdout.strip(), identity


def _verify_declared_toolchain(
    args: argparse.Namespace,
    environment: dict[str, str],
    source_root: Path,
) -> dict[Path, tuple[int, int]]:
    identities: dict[Path, tuple[int, int]] = {}

    cargo = _resolve_build_tool("cargo", environment)
    cargo_output, identities[cargo] = _run_tool_version(
        cargo, ["-V"], environment, source_root, "cargo"
    )
    cargo_match = re.fullmatch(r"cargo ([^\s]+)(?: .*)?", cargo_output)
    if cargo_match is None or cargo_match.group(1) != args.cargo_version:
        raise ArtifactError("cargo 实际版本与 build receipt toolchain 不一致")

    rustc = _resolve_build_tool("rustc", environment)
    rustc_output, identities[rustc] = _run_tool_version(
        rustc, ["-Vv"], environment, source_root, "rustc"
    )
    rustc_fields = {}
    for line in rustc_output.splitlines():
        if ": " in line:
            key, value = line.split(": ", 1)
            rustc_fields[key] = value
    if (
        rustc_fields.get("release") != args.rustc_version
        or rustc_fields.get("commit-hash") != args.rustc_commit
    ):
        raise ArtifactError("rustc 实际版本/commit 与 build receipt toolchain 不一致")

    zig = _resolve_build_tool("zig", environment)
    zig_output, identities[zig] = _run_tool_version(
        zig, ["version"], environment, source_root, "zig"
    )
    if zig_output != args.zig_version:
        raise ArtifactError("zig 实际版本与 build receipt toolchain 不一致")

    try:
        python = Path(sys.executable).resolve(strict=True)
        python_metadata = python.stat()
    except OSError as exc:
        raise ArtifactError(f"无法解析当前 Python 解释器：{exc}") from exc
    if not stat.S_ISREG(python_metadata.st_mode) or python_metadata.st_mode & 0o111 == 0:
        raise ArtifactError("当前 Python 解释器必须是可执行普通文件")
    identities[python] = (python_metadata.st_dev, python_metadata.st_ino)
    if platform.python_version() != args.python_version:
        raise ArtifactError("当前 Python 解释器版本与 build receipt toolchain 不一致")
    return identities


def _assert_tool_identities(identities: dict[Path, tuple[int, int]]) -> None:
    for executable, expected in identities.items():
        try:
            metadata = executable.stat()
        except OSError as exc:
            raise ArtifactError(f"构建工具在构建期间消失：{executable}") from exc
        if (metadata.st_dev, metadata.st_ino) != expected:
            raise ArtifactError(f"构建工具在构建期间被替换：{executable}")


def _normalized_build_command(executable: Path, source_root: Path) -> list[str]:
    return [
        str(executable),
        "zigbuild",
        "--manifest-path",
        str(source_root / "Cargo.toml"),
        "--locked",
        "--release",
        "--target",
        TARGET,
        "--features",
        "user-audit",
        "--bin",
        "ssserver",
        "--bin",
        "shadowsocks-auditd",
    ]


def _release_build_environment(
    source_root: Path,
    target_root: Path,
    cargo_home: Path,
    source_date_epoch: int,
) -> dict[str, str]:
    encoded_flags = _encoded_rustflags(str(source_root), str(target_root), str(cargo_home))
    path = os.environ.get("PATH")
    if not path:
        raise ArtifactError("构建环境缺少 PATH")
    environment = {"PATH": path}
    rustup_home = os.environ.get("RUSTUP_HOME")
    if rustup_home is None and os.environ.get("HOME"):
        default_rustup_home = Path(os.environ["HOME"]) / ".rustup"
        if default_rustup_home.is_dir():
            rustup_home = str(default_rustup_home)
    if rustup_home:
        resolved_rustup_home = _resolved_directory(Path(rustup_home), "rustup home")
        rustup_metadata = resolved_rustup_home.stat(follow_symlinks=False)
        if rustup_metadata.st_uid not in (0, os.geteuid()) or rustup_metadata.st_mode & 0o022:
            raise ArtifactError(
                "rustup home 必须由当前用户/root 持有且不得允许 group/other 写入"
            )
        environment["RUSTUP_HOME"] = str(resolved_rustup_home)
    environment.update(
        {
            "CARGO_ENCODED_RUSTFLAGS": encoded_flags,
            "CARGO_INCREMENTAL": "0",
            "CARGO_HOME": str(cargo_home),
            "CARGO_PROFILE_RELEASE_INCREMENTAL": "false",
            "CARGO_TARGET_DIR": str(target_root),
            "LANG": "C",
            "LC_ALL": "C",
            "HOME": str(cargo_home),
            "SHADOWSOCKS_BUILD_TIME_UTC": _epoch_timestamp(source_date_epoch),
            "SOURCE_DATE_EPOCH": str(source_date_epoch),
            "TZ": "UTC",
            "ZERO_AR_DATE": "1",
        }
    )
    return environment


def _require_isolated_cargo_home(cargo_home: Path) -> None:
    metadata = cargo_home.stat(follow_symlinks=False)
    if metadata.st_uid not in (0, os.geteuid()) or metadata.st_mode & 0o022:
        raise ArtifactError(
            "cargo home 必须由当前用户/root 持有且不得允许 group/other 写入"
        )
    try:
        with os.scandir(cargo_home) as entries:
            first_entry = next(entries, None)
    except OSError as exc:
        raise ArtifactError(f"无法枚举 cargo home：{exc}") from exc
    if first_entry is not None:
        raise ArtifactError("cargo home 必须是本次构建专用的空目录")


def _reject_cargo_config_search_path(source_root: Path, cargo_home: Path) -> None:
    search_roots = (source_root, *source_root.parents, cargo_home)
    for root in search_roots:
        cargo_config_root = root / ".cargo" if root != cargo_home else root
        for name in ("config", "config.toml"):
            candidate = cargo_config_root / name
            if os.path.lexists(candidate):
                raise ArtifactError(
                    "Cargo config 搜索路径必须为空，拒绝未记录的构建注入："
                    f"{candidate}"
                )


def _receipt_artifacts(build_id: str, binaries: dict[str, bytes]) -> list[dict[str, Any]]:
    return [
        {
            "name": name,
            "path": f"target-{build_id[-1]}/{TARGET}/release/{name}",
            "bytes": len(binaries[name]),
            "sha256": _sha256(binaries[name]),
        }
        for name in MULTI_ARTIFACTS
    ]


def _read_executable(path: Path, label: str) -> bytes:
    payload, metadata = _read_regular(path, MAX_BINARY_BYTES)
    if not metadata.st_mode & 0o111:
        raise ArtifactError(f"{label} 输入文件没有可执行位")
    _require_elf_x86_64(payload, label)
    return payload


def _reject_same_input(first: Path, second: Path, label: str) -> None:
    """Require the two reproducibility inputs to be distinct filesystem inodes."""
    try:
        first_stat = os.stat(first, follow_symlinks=False)
        second_stat = os.stat(second, follow_symlinks=False)
    except OSError:
        # Let _read_executable report the more specific missing/unsafe-path
        # error below.
        return
    if os.path.samestat(first_stat, second_stat):
        raise ArtifactError(f"{label} 两次独立构建必须来自不同文件")


def _epoch_timestamp(epoch: int) -> str:
    try:
        return datetime.fromtimestamp(epoch, timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    except (OverflowError, OSError, ValueError) as exc:
        raise ArtifactError("--source-date-epoch 无法转换为 UTC 时间") from exc


def _parse_build_receipt(payload: bytes, expected_build_id: str | None = None) -> dict[str, Any]:
    try:
        value = json.loads(payload.decode("utf-8"), object_pairs_hook=_strict_object)
    except UnicodeDecodeError as exc:
        raise ArtifactError("build receipt 不是有效 UTF-8") from exc
    except ArtifactError:
        raise
    except json.JSONDecodeError as exc:
        raise ArtifactError(f"build receipt JSON 解析失败：{exc.lineno}:{exc.colno}") from exc
    if not isinstance(value, dict):
        raise ArtifactError("build receipt 必须是对象")
    _exact_keys(
        value,
        {
            "schema_version",
            "build_id",
            "source",
            "target",
            "build",
            "toolchain",
            "recipe",
            "execution",
            "artifacts",
            "evidence_boundary",
        },
        "build receipt",
    )
    if type(value["schema_version"]) is not int or value["schema_version"] != 2:
        raise ArtifactError("build receipt schema_version 必须为 2")
    build_id = value["build_id"]
    if build_id not in BUILD_IDS or (
        expected_build_id is not None and build_id != expected_build_id
    ):
        raise ArtifactError("build receipt build_id 错误")
    suffix = build_id[-1]

    source = value["source"]
    if not isinstance(source, dict):
        raise ArtifactError("build receipt source 必须是对象")
    _exact_keys(source, {"root", "tree_sha256"}, "build receipt source")
    if source["root"] != f"source-{suffix}":
        raise ArtifactError("build receipt source.root 错误")
    if not isinstance(source["tree_sha256"], str) or not SHA256_PATTERN.fullmatch(
        source["tree_sha256"]
    ):
        raise ArtifactError("build receipt source.tree_sha256 格式错误")

    target = value["target"]
    if not isinstance(target, dict):
        raise ArtifactError("build receipt target 必须是对象")
    _exact_keys(target, {"root", "triple"}, "build receipt target")
    if target != {"root": f"target-{suffix}", "triple": TARGET}:
        raise ArtifactError("build receipt target 错误")

    build = value["build"]
    if not isinstance(build, dict):
        raise ArtifactError("build receipt build 必须是对象")
    _exact_keys(
        build,
        {"version", "upstream_commit", "overlay_commit", "source_date_epoch"},
        "build receipt build",
    )
    if type(build["source_date_epoch"]) is not int:
        raise ArtifactError("build receipt source_date_epoch 格式错误")
    _validate_common_build_values(
        build["version"],
        build["upstream_commit"],
        build["overlay_commit"],
        build["source_date_epoch"],
    )

    toolchain = value["toolchain"]
    if not isinstance(toolchain, dict):
        raise ArtifactError("build receipt toolchain 必须是对象")
    _validate_toolchain(toolchain, "build receipt toolchain")
    _validate_recipe(value["recipe"], build["source_date_epoch"])
    execution = value["execution"]
    if not isinstance(execution, dict):
        raise ArtifactError("build receipt execution 必须是对象")
    _exact_keys(
        execution,
        {"runner", "command", "exit_code", "target_was_empty"},
        "build receipt execution",
    )
    command_template = value["recipe"]["command_template"]
    command = execution["command"]
    if (
        not isinstance(command, list)
        or len(command) != len(command_template)
        or not all(isinstance(item, str) for item in command)
    ):
        raise ArtifactError("build receipt execution.command 格式错误")
    executable = command[0]
    if not executable.startswith("/") or os.path.basename(executable) != "cargo-zigbuild":
        raise ArtifactError(
            "build receipt execution.command 必须记录实际执行的 cargo-zigbuild 绝对路径"
        )
    expected_execution = {
        "runner": "scripts/release-artifact.py build-and-receipt",
        "command": [executable, *command_template[1:]],
        "exit_code": 0,
        "target_was_empty": True,
    }
    if execution != expected_execution:
        raise ArtifactError("build receipt execution 不是受支持的成功构建记录")
    if value["evidence_boundary"] != EVIDENCE_BOUNDARY:
        raise ArtifactError("build receipt evidence_boundary 错误")

    artifacts = value["artifacts"]
    if not isinstance(artifacts, list) or len(artifacts) != len(MULTI_ARTIFACTS):
        raise ArtifactError("build receipt artifacts 必须包含两个对象")
    names: list[str] = []
    for artifact in artifacts:
        if not isinstance(artifact, dict):
            raise ArtifactError("build receipt artifact 必须是对象")
        _exact_keys(artifact, {"name", "path", "bytes", "sha256"}, "build receipt artifact")
        name = artifact["name"]
        if name not in MULTI_ARTIFACTS or name in names:
            raise ArtifactError("build receipt artifact.name 错误或重复")
        names.append(name)
        if artifact["path"] != f"target-{suffix}/{TARGET}/release/{name}":
            raise ArtifactError("build receipt artifact.path 错误")
        if type(artifact["bytes"]) is not int or not 1 <= artifact["bytes"] <= MAX_BINARY_BYTES:
            raise ArtifactError("build receipt artifact.bytes 格式错误")
        if not isinstance(artifact["sha256"], str) or not SHA256_PATTERN.fullmatch(
            artifact["sha256"]
        ):
            raise ArtifactError("build receipt artifact.sha256 格式错误")
    if names != list(MULTI_ARTIFACTS):
        raise ArtifactError("build receipt artifact 顺序错误")
    return value


def _read_build_receipt(path: Path, expected_build_id: str) -> tuple[bytes, dict[str, Any]]:
    payload, metadata = _read_regular(path, MAX_RECEIPT_BYTES)
    if stat.S_IMODE(metadata.st_mode) != 0o644:
        raise ArtifactError(f"{expected_build_id} receipt 权限必须为 0644")
    value = _parse_build_receipt(payload, expected_build_id)
    if payload != _canonical_manifest(value):
        raise ArtifactError(f"{expected_build_id} receipt 不是规范 JSON 编码")
    return payload, value


def _resolved_directory(path: Path, label: str) -> Path:
    if path.is_symlink():
        raise ArtifactError(f"{label} 不能是符号链接")
    resolved = path.resolve(strict=True)
    if not resolved.is_dir():
        raise ArtifactError(f"{label} 不是目录：{resolved}")
    return resolved


def _reject_shared_roots(roots: list[tuple[str, Path]]) -> None:
    for index, (first_label, first) in enumerate(roots):
        for second_label, second in roots[index + 1 :]:
            if first == second or first in second.parents or second in first.parents:
                raise ArtifactError(f"{first_label} 与 {second_label} 必须是互不重叠的目录")


def _validate_receipt_against_live_build(
    receipt: dict[str, Any],
    args: argparse.Namespace,
    source_root: Path,
    target_root: Path,
    binaries: dict[str, bytes],
    binary_paths: dict[str, Path],
) -> None:
    build = receipt["build"]
    expected_build = {
        "version": args.version,
        "upstream_commit": args.upstream_commit,
        "overlay_commit": args.overlay_commit,
        "source_date_epoch": args.source_date_epoch,
    }
    if build != expected_build:
        raise ArtifactError(f"{receipt['build_id']} receipt build 声明与打包参数不一致")
    if receipt["toolchain"] != _toolchain_metadata(args):
        raise ArtifactError(f"{receipt['build_id']} receipt toolchain 与打包参数不一致")
    if receipt["source"]["tree_sha256"] != _tree_sha256(source_root):
        raise ArtifactError(f"{receipt['build_id']} receipt 源码树摘要与 live source root 不一致")
    if receipt["source"]["tree_sha256"] != args.expected_prepared_tree_sha256:
        raise ArtifactError(f"{receipt['build_id']} receipt 源码树摘要与锁定准备源码不一致")
    expected_artifacts = {item["name"]: item for item in receipt["artifacts"]}
    for name in MULTI_ARTIFACTS:
        expected_path = target_root / TARGET / "release" / name
        if binary_paths[name].resolve(strict=True) != expected_path.resolve(strict=True):
            raise ArtifactError(f"{receipt['build_id']} 的 {name} 不位于声明的 live target root")
        artifact = expected_artifacts[name]
        if artifact["bytes"] != len(binaries[name]) or artifact["sha256"] != _sha256(
            binaries[name]
        ):
            raise ArtifactError(f"{receipt['build_id']} receipt 未绑定 live {name}")


def _independent_build_metadata(receipt_payloads: dict[str, bytes]) -> dict[str, Any]:
    return {
        "count": 2,
        "evidence_boundary": EVIDENCE_BOUNDARY,
        "receipts": [
            {
                "build_id": build_id,
                "path": f"{build_id}.receipt.json",
                "sha256": _sha256(receipt_payloads[build_id]),
            }
            for build_id in BUILD_IDS
        ],
    }


def _build_metadata(
    args: argparse.Namespace,
    receipt_payloads: dict[str, bytes],
) -> dict[str, Any]:
    _validate_common_build_values(
        args.version, args.upstream_commit, args.overlay_commit, args.source_date_epoch
    )
    return {
        "version": args.version,
        "upstream_commit": args.upstream_commit,
        "overlay_commit": args.overlay_commit,
        "target": TARGET,
        "prepared_tree_sha256": args.expected_prepared_tree_sha256,
        "source_date_epoch": args.source_date_epoch,
        "generated_at": _epoch_timestamp(args.source_date_epoch),
        "independent_builds": _independent_build_metadata(receipt_payloads),
    }


def _multi_manifest(value: dict[str, Any]) -> bytes:
    return _canonical_manifest(value)


def _parse_multi_manifest(payload: bytes) -> dict[str, Any]:
    value = _parse_manifest_multi_common(payload)
    if set(value) != {"schema_version", "artifacts", "build", "toolchain", "patch_series"}:
        raise ArtifactError("multi manifest 顶层字段错误")
    if value["patch_series"] != _patch_series():
        raise ArtifactError("multi manifest patch_series 与仓库不一致")
    artifacts = value["artifacts"]
    if not isinstance(artifacts, list) or len(artifacts) != len(MULTI_ARTIFACTS):
        raise ArtifactError("multi manifest 必须包含两个 artifacts")
    names: list[str] = []
    for artifact in artifacts:
        if not isinstance(artifact, dict):
            raise ArtifactError("multi manifest artifact 必须是对象")
        _exact_keys(artifact, {"name", "bytes", "sha256", "format", "architecture"}, "manifest.artifact")
        name = artifact["name"]
        if name not in MULTI_ARTIFACTS or name in names:
            raise ArtifactError("multi manifest artifact.name 错误或重复")
        names.append(name)
        if type(artifact["bytes"]) is not int or not 1 <= artifact["bytes"] <= MAX_BINARY_BYTES:
            raise ArtifactError("multi manifest artifact.bytes 必须是正整数")
        if not isinstance(artifact["sha256"], str) or not SHA256_PATTERN.fullmatch(artifact["sha256"]):
            raise ArtifactError("multi manifest artifact.sha256 格式错误")
        if artifact["format"] != "ELF64" or artifact["architecture"] != "x86_64":
            raise ArtifactError("multi manifest artifact ELF 格式或架构错误")
    if names != list(MULTI_ARTIFACTS):
        raise ArtifactError("multi manifest artifact 顺序错误")
    _validate_multi_build_and_toolchain(value)
    return value


def _parse_manifest_multi_common(payload: bytes) -> dict[str, Any]:
    try:
        text = payload.decode("utf-8")
    except UnicodeDecodeError as exc:
        raise ArtifactError("manifest 不是有效 UTF-8") from exc
    try:
        value = json.loads(text, object_pairs_hook=_strict_object)
    except ArtifactError:
        raise
    except json.JSONDecodeError as exc:
        raise ArtifactError(f"manifest JSON 解析失败：{exc.lineno}:{exc.colno}") from exc
    if (
        not isinstance(value, dict)
        or type(value.get("schema_version")) is not int
        or value.get("schema_version") != 1
    ):
        raise ArtifactError("multi manifest schema_version 必须为 1")
    return value


def _validate_multi_build_and_toolchain(value: dict[str, Any]) -> None:
    build = value.get("build")
    if not isinstance(build, dict):
        raise ArtifactError("multi manifest.build 必须是对象")
    _exact_keys(
        build,
        {
            "version",
            "upstream_commit",
            "overlay_commit",
            "target",
            "prepared_tree_sha256",
            "source_date_epoch",
            "generated_at",
            "independent_builds",
        },
        "manifest.build",
    )
    if not isinstance(build["version"], str) or not VERSION_PATTERN.fullmatch(build["version"]):
        raise ArtifactError("multi manifest build.version 格式错误")
    for field in ("upstream_commit", "overlay_commit"):
        if not isinstance(build[field], str) or not COMMIT_PATTERN.fullmatch(build[field]):
            raise ArtifactError(f"multi manifest build.{field} 格式错误")
    if build["target"] != TARGET:
        raise ArtifactError("multi manifest build.target 错误")
    _validate_prepared_tree_sha256(
        build["prepared_tree_sha256"], "multi manifest build.prepared_tree_sha256"
    )
    if type(build["source_date_epoch"]) is not int or not 1 <= build["source_date_epoch"] <= 0xFFFFFFFF:
        raise ArtifactError("multi manifest source_date_epoch 格式错误")
    if build["generated_at"] != _epoch_timestamp(build["source_date_epoch"]):
        raise ArtifactError("multi manifest generated_at 与 source_date_epoch 不一致")
    independent = build["independent_builds"]
    if not isinstance(independent, dict):
        raise ArtifactError("multi manifest independent_builds 必须是对象")
    _exact_keys(
        independent,
        {"count", "evidence_boundary", "receipts"},
        "manifest.build.independent_builds",
    )
    if type(independent["count"]) is not int or independent["count"] != 2:
        raise ArtifactError("multi manifest independent_builds.count 必须为 2")
    if independent["evidence_boundary"] != EVIDENCE_BOUNDARY:
        raise ArtifactError("multi manifest independent_builds.evidence_boundary 错误")
    receipts = independent["receipts"]
    if not isinstance(receipts, list) or len(receipts) != len(BUILD_IDS):
        raise ArtifactError("multi manifest independent_builds.receipts 必须包含两个对象")
    build_ids: list[str] = []
    for item in receipts:
        if not isinstance(item, dict):
            raise ArtifactError("multi manifest independent build receipt 必须是对象")
        _exact_keys(item, {"build_id", "path", "sha256"}, "manifest.independent_build_receipt")
        build_id = item["build_id"]
        if build_id not in BUILD_IDS or build_id in build_ids:
            raise ArtifactError("multi manifest independent build receipt.build_id 错误或重复")
        build_ids.append(build_id)
        if item["path"] != f"{build_id}.receipt.json":
            raise ArtifactError("multi manifest independent build receipt.path 错误")
        if not isinstance(item["sha256"], str) or not SHA256_PATTERN.fullmatch(item["sha256"]):
            raise ArtifactError("multi manifest independent build receipt.sha256 格式错误")
    if build_ids != list(BUILD_IDS):
        raise ArtifactError("multi manifest independent build receipt 顺序错误")
    toolchain = value.get("toolchain")
    if not isinstance(toolchain, dict):
        raise ArtifactError("multi manifest.toolchain 必须是对象")
    _validate_toolchain(toolchain, "multi manifest toolchain")


def _validate_output_dir(
    path: Path,
    *,
    expected_device: int | None = None,
    expected_inode: int | None = None,
) -> tuple[Path, tuple[int, int]]:
    if path.is_symlink():
        raise ArtifactError("输出目录不能是符号链接")
    output_dir = path.resolve(strict=True)
    if not output_dir.is_dir():
        raise ArtifactError(f"输出路径不是目录：{output_dir}")
    metadata = output_dir.stat(follow_symlinks=False)
    if metadata.st_uid not in (0, os.geteuid()) or metadata.st_mode & 0o022:
        raise ArtifactError("输出目录必须由当前用户/root 持有且不得允许 group/other 写入")
    if expected_device is not None and metadata.st_dev != expected_device:
        raise ArtifactError("输出目录 device 已在构建期间变化")
    if expected_inode is not None and metadata.st_ino != expected_inode:
        raise ArtifactError("输出目录 inode 已在构建期间变化")
    return output_dir, (metadata.st_dev, metadata.st_ino)


def _directory_entries(path: Path) -> set[str]:
    try:
        return {entry.name for entry in os.scandir(path)}
    except OSError as exc:
        raise ArtifactError(f"无法枚举输出目录：{exc}") from exc


def _open_release_directory(
    output_dir: Path, expected_identity: tuple[int, int] | None = None
) -> tuple[int, tuple[int, int]]:
    flags = os.O_RDONLY
    if hasattr(os, "O_DIRECTORY"):
        flags |= os.O_DIRECTORY
    if hasattr(os, "O_CLOEXEC"):
        flags |= os.O_CLOEXEC
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    try:
        directory_fd = os.open(output_dir, flags)
        metadata = os.fstat(directory_fd)
    except OSError as exc:
        raise ArtifactError(f"无法安全打开发布目录：{exc}") from exc
    if not stat.S_ISDIR(metadata.st_mode):
        os.close(directory_fd)
        raise ArtifactError("发布目录路径不是普通目录")
    if metadata.st_uid not in (0, os.geteuid()) or metadata.st_mode & 0o022:
        os.close(directory_fd)
        raise ArtifactError("输出目录必须由当前用户/root 持有且不得允许 group/other 写入")
    identity = (metadata.st_dev, metadata.st_ino)
    if expected_identity is not None and identity != expected_identity:
        os.close(directory_fd)
        raise ArtifactError("发布目录路径在验证与打开之间被替换")
    try:
        _assert_release_directory_path_identity(output_dir, identity)
    except Exception:
        os.close(directory_fd)
        raise
    return directory_fd, identity


def _directory_entries_at(directory_fd: int) -> set[str]:
    try:
        return set(os.listdir(directory_fd))
    except OSError as exc:
        raise ArtifactError(f"无法枚举已打开的发布目录：{exc}") from exc


def _release_file_limit(name: str) -> int:
    if name in MULTI_ARTIFACTS:
        return MAX_BINARY_BYTES
    if name.endswith(".sha256"):
        return 1024
    if name.endswith(".receipt.json"):
        return MAX_RECEIPT_BYTES
    return MAX_MANIFEST_BYTES


def _release_file_mode(name: str) -> int:
    return 0o755 if name in MULTI_ARTIFACTS else 0o644


def _read_regular_at(
    directory_fd: int, name: str, limit: int
) -> tuple[bytes, os.stat_result]:
    if not name or "/" in name or name in (".", ".."):
        raise ArtifactError(f"发布成员名称非法：{name}")
    flags = os.O_RDONLY
    if hasattr(os, "O_CLOEXEC"):
        flags |= os.O_CLOEXEC
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    try:
        fd = os.open(name, flags, dir_fd=directory_fd)
    except OSError as exc:
        raise ArtifactError(f"无法安全打开发布成员 {name}：{exc.strerror}") from exc
    try:
        metadata = os.fstat(fd)
        if not stat.S_ISREG(metadata.st_mode):
            raise ArtifactError(f"发布成员不是普通文件：{name}")
        chunks: list[bytes] = []
        remaining = limit + 1
        while remaining:
            chunk = os.read(fd, min(remaining, 1024 * 1024))
            if not chunk:
                break
            chunks.append(chunk)
            remaining -= len(chunk)
        payload = b"".join(chunks)
    finally:
        os.close(fd)
    if len(payload) > limit:
        raise ArtifactError(f"发布成员超过 {limit} 字节上限：{name}")
    return payload, metadata


def _assert_release_directory_path_identity(
    output_dir: Path, expected_identity: tuple[int, int]
) -> None:
    try:
        metadata = output_dir.stat(follow_symlinks=False)
    except OSError as exc:
        raise ArtifactError("发布目录路径在操作期间消失或被替换") from exc
    if not stat.S_ISDIR(metadata.st_mode) or (
        metadata.st_dev,
        metadata.st_ino,
    ) != expected_identity:
        raise ArtifactError("发布目录路径在操作期间被替换")


def _assert_release_directory_snapshot(
    output_dir: Path,
    directory_fd: int,
    snapshot: ReleaseDirectorySnapshot,
) -> None:
    current_directory = os.fstat(directory_fd)
    if (current_directory.st_dev, current_directory.st_ino) != snapshot.directory_identity:
        raise ArtifactError("已打开的发布目录身份发生变化")
    _assert_release_directory_path_identity(output_dir, snapshot.directory_identity)
    expected_names = set(snapshot.files)
    if _directory_entries_at(directory_fd) != expected_names:
        raise ArtifactError("发布目录成员集合在操作期间发生变化")
    for name, expected in snapshot.files.items():
        payload, metadata = _read_regular_at(directory_fd, name, _release_file_limit(name))
        if (
            (metadata.st_dev, metadata.st_ino) != expected.identity
            or stat.S_IMODE(metadata.st_mode) != expected.mode
            or payload != expected.payload
        ):
            raise ArtifactError(f"发布成员在操作期间被替换或修改：{name}")


def _capture_release_directory_snapshot(
    output_dir: Path,
    directory_fd: int,
    expected_names: frozenset[str],
) -> ReleaseDirectorySnapshot:
    directory_metadata = os.fstat(directory_fd)
    directory_identity = (directory_metadata.st_dev, directory_metadata.st_ino)
    _assert_release_directory_path_identity(output_dir, directory_identity)
    entries = _directory_entries_at(directory_fd)
    if entries != set(expected_names):
        extras = sorted(entries - set(expected_names))
        missing = sorted(set(expected_names) - entries)
        detail = []
        if missing:
            detail.append("缺少 " + ", ".join(missing))
        if extras:
            detail.append("未绑定 " + ", ".join(extras))
        raise ArtifactError("发布目录成员集合错误（" + "；".join(detail) + "）")
    files: dict[str, FileSnapshot] = {}
    for name in sorted(expected_names):
        payload, metadata = _read_regular_at(directory_fd, name, _release_file_limit(name))
        mode = stat.S_IMODE(metadata.st_mode)
        expected_mode = _release_file_mode(name)
        if mode != expected_mode:
            raise ArtifactError(f"发布成员 {name} 权限必须为 {expected_mode:04o}")
        files[name] = FileSnapshot(
            payload=payload,
            mode=mode,
            identity=(metadata.st_dev, metadata.st_ino),
        )
    snapshot = ReleaseDirectorySnapshot(directory_identity, files)
    _assert_release_directory_snapshot(output_dir, directory_fd, snapshot)
    return snapshot


def _require_release_directory_entries(path: Path, *, packaging: bool) -> None:
    entries = _directory_entries(path)
    if packaging:
        if entries:
            raise ArtifactError(
                "发布输出目录必须为空，拒绝覆盖或保留未绑定文件："
                + ", ".join(sorted(entries))
            )
        return
    allowed = (UNSIGNED_RELEASE_FILES, UNSIGNED_RELEASE_FILES | {"release-manifest.sig"})
    if entries not in allowed:
        extras = sorted(entries - (UNSIGNED_RELEASE_FILES | {"release-manifest.sig"}))
        missing = sorted(UNSIGNED_RELEASE_FILES - entries)
        detail: list[str] = []
        if missing:
            detail.append("缺少 " + ", ".join(missing))
        if extras:
            detail.append("未绑定 " + ", ".join(extras))
        raise ArtifactError("发布目录成员集合错误（" + "；".join(detail) + "）")


def _unlink_created_release_files(
    directory_fd: int, created: dict[str, tuple[int, int]]
) -> None:
    for name, identity in created.items():
        try:
            metadata = os.stat(name, dir_fd=directory_fd, follow_symlinks=False)
            if stat.S_ISREG(metadata.st_mode) and (
                metadata.st_dev,
                metadata.st_ino,
            ) == identity:
                os.unlink(name, dir_fd=directory_fd)
        except OSError:
            pass
    try:
        os.fsync(directory_fd)
    except OSError:
        pass


def _publish_release_payloads(
    output_dir: Path,
    directory_fd: int,
    directory_identity: tuple[int, int],
    payloads: list[tuple[str, bytes, int]],
) -> ReleaseDirectorySnapshot:
    if _directory_entries_at(directory_fd):
        raise ArtifactError("发布输出目录必须为空，拒绝覆盖或保留未绑定文件")
    _assert_release_directory_path_identity(output_dir, directory_identity)
    created: dict[str, tuple[int, int]] = {}
    expected = {name: (payload, mode) for name, payload, mode in payloads}
    if len(expected) != len(payloads) or set(expected) != set(UNSIGNED_RELEASE_FILES):
        raise ArtifactError("内部发布 payload 成员集合错误")
    try:
        for name, payload, mode in payloads:
            created[name] = _exclusive_write_at(directory_fd, name, payload, mode)
        os.fsync(directory_fd)
        snapshot = _capture_release_directory_snapshot(
            output_dir, directory_fd, UNSIGNED_RELEASE_FILES
        )
        for name, (payload, mode) in expected.items():
            actual = snapshot.files[name]
            if (
                actual.payload != payload
                or actual.mode != mode
                or actual.identity != created[name]
            ):
                raise ArtifactError(f"写入后的发布成员与预期不一致：{name}")
        return snapshot
    except Exception:
        _unlink_created_release_files(directory_fd, created)
        raise


def command_build_and_receipt(args: argparse.Namespace) -> None:
    _validate_common_build_values(
        args.version, args.upstream_commit, args.overlay_commit, args.source_date_epoch
    )
    toolchain = _toolchain_metadata(args)
    _validate_toolchain(toolchain, "build receipt toolchain")
    _validate_prepared_tree_sha256(
        args.expected_prepared_tree_sha256, "--expected-prepared-tree-sha256"
    )
    source_root = _resolved_directory(args.source_root, "source root")
    cargo_home = _resolved_directory(args.cargo_home, "cargo home")
    _require_isolated_cargo_home(cargo_home)
    _reject_cargo_config_search_path(source_root, cargo_home)
    target_path = args.target_root
    if target_path.is_symlink() or os.path.lexists(target_path):
        raise ArtifactError("target root 必须事先不存在，禁止复用或预置构建产物")
    target_parent = _resolved_directory(target_path.parent, "target root parent")
    target_path = target_parent / target_path.name
    try:
        target_path.mkdir(mode=0o700)
    except OSError as exc:
        raise ArtifactError(f"无法创建独立 target root：{exc}") from exc
    target_root = _resolved_directory(target_path, "target root")
    _reject_shared_roots(
        [
            ("source root", source_root),
            ("target root", target_root),
            ("cargo home", cargo_home),
        ]
    )
    expected_receipt_path = target_root / "build-receipt.json"
    source_digest_before = _tree_sha256(source_root)
    if source_digest_before != args.expected_prepared_tree_sha256:
        raise ArtifactError("准备源码树 SHA-256 与锁定摘要不一致")

    environment = _release_build_environment(
        source_root, target_root, cargo_home, args.source_date_epoch
    )
    cargo_zigbuild = _resolve_cargo_zigbuild(environment)
    executable_identity = _verify_cargo_zigbuild_version(
        cargo_zigbuild,
        args.cargo_zigbuild_version,
        environment,
        source_root,
    )
    tool_identities = _verify_declared_toolchain(args, environment, source_root)
    tool_identities[cargo_zigbuild] = executable_identity
    _require_isolated_cargo_home(cargo_home)
    _reject_cargo_config_search_path(source_root, cargo_home)
    command = _normalized_build_command(cargo_zigbuild, source_root)
    if (cargo_zigbuild.stat().st_dev, cargo_zigbuild.stat().st_ino) != executable_identity:
        raise ArtifactError("cargo-zigbuild 在构建前被替换")
    try:
        completed = subprocess.run(
            command,
            env=environment,
            cwd=source_root,
            check=False,
        )
    except OSError as exc:
        raise ArtifactError(f"无法执行 cargo-zigbuild：{exc}") from exc
    if completed.returncode != 0:
        raise ArtifactError(f"cargo-zigbuild 失败，exit code={completed.returncode}")
    _assert_tool_identities(tool_identities)
    if _tree_sha256(source_root) != source_digest_before:
        raise ArtifactError("cargo-zigbuild 修改了准备源码树，拒绝生成 receipt")

    binaries: dict[str, bytes] = {}
    binary_paths = {
        name: target_root / TARGET / "release" / name for name in MULTI_ARTIFACTS
    }
    for name, path in binary_paths.items():
        binaries[name] = _read_executable(path, name)

    suffix = args.build_id[-1]
    receipt_value = {
        "schema_version": 2,
        "build_id": args.build_id,
        "source": {
            "root": f"source-{suffix}",
            "tree_sha256": source_digest_before,
        },
        "target": {"root": f"target-{suffix}", "triple": TARGET},
        "build": {
            "version": args.version,
            "upstream_commit": args.upstream_commit,
            "overlay_commit": args.overlay_commit,
            "source_date_epoch": args.source_date_epoch,
        },
        "toolchain": toolchain,
        "recipe": _recipe_metadata(
            _observed_recipe_environment(environment, source_root, target_root, cargo_home)
        ),
        "execution": {
            "runner": "scripts/release-artifact.py build-and-receipt",
            "command": _observed_execution_command(command, source_root),
            "exit_code": completed.returncode,
            "target_was_empty": True,
        },
        "artifacts": _receipt_artifacts(args.build_id, binaries),
        "evidence_boundary": EVIDENCE_BOUNDARY,
    }
    payload = _canonical_manifest(receipt_value)
    parsed = _parse_build_receipt(payload, args.build_id)
    if payload != _canonical_manifest(parsed):
        raise ArtifactError("生成的 build receipt 不是规范 JSON")
    output = expected_receipt_path
    if output.is_symlink() or os.path.lexists(output):
        raise ArtifactError(f"receipt 输出已存在，拒绝覆盖：{output}")
    _exclusive_write(output, payload, 0o644)
    print(f"cargo-zigbuild 成功并生成 {args.build_id} canonical build receipt：{output}")


def command_package_multi(args: argparse.Namespace) -> None:
    _validate_prepared_tree_sha256(
        args.expected_prepared_tree_sha256, "--expected-prepared-tree-sha256"
    )
    _reject_same_input(args.binary, args.second_binary, "ssserver")
    _reject_same_input(args.auditd_binary, args.second_auditd_binary, "shadowsocks-auditd")
    _reject_same_input(args.first_build_receipt, args.second_build_receipt, "build receipt")
    binaries = {
        "ssserver": _read_executable(args.binary, "ssserver"),
        "shadowsocks-auditd": _read_executable(args.auditd_binary, "shadowsocks-auditd"),
    }
    independent_binaries = {
        "ssserver": _read_executable(args.second_binary, "second ssserver"),
        "shadowsocks-auditd": _read_executable(args.second_auditd_binary, "second shadowsocks-auditd"),
    }
    for name in MULTI_ARTIFACTS:
        if binaries[name] != independent_binaries[name]:
            raise ArtifactError(f"{name} 两次独立构建字节不一致")

    source_roots = {
        "build-a": _resolved_directory(args.first_source_root, "build-a source root"),
        "build-b": _resolved_directory(args.second_source_root, "build-b source root"),
    }
    target_roots = {
        "build-a": _resolved_directory(args.first_target_root, "build-a target root"),
        "build-b": _resolved_directory(args.second_target_root, "build-b target root"),
    }
    _reject_shared_roots(
        [
            ("build-a source root", source_roots["build-a"]),
            ("build-b source root", source_roots["build-b"]),
            ("build-a target root", target_roots["build-a"]),
            ("build-b target root", target_roots["build-b"]),
        ]
    )
    receipt_paths = {
        "build-a": args.first_build_receipt,
        "build-b": args.second_build_receipt,
    }
    receipt_payloads: dict[str, bytes] = {}
    receipts: dict[str, dict[str, Any]] = {}
    for build_id in BUILD_IDS:
        expected_receipt_path = target_roots[build_id] / "build-receipt.json"
        if receipt_paths[build_id].resolve(strict=True) != expected_receipt_path:
            raise ArtifactError(f"{build_id} receipt 不位于声明的 live target root")
        payload, receipt = _read_build_receipt(receipt_paths[build_id], build_id)
        receipt_payloads[build_id] = payload
        receipts[build_id] = receipt
    _validate_receipt_against_live_build(
        receipts["build-a"],
        args,
        source_roots["build-a"],
        target_roots["build-a"],
        binaries,
        {"ssserver": args.binary, "shadowsocks-auditd": args.auditd_binary},
    )
    _validate_receipt_against_live_build(
        receipts["build-b"],
        args,
        source_roots["build-b"],
        target_roots["build-b"],
        independent_binaries,
        {
            "ssserver": args.second_binary,
            "shadowsocks-auditd": args.second_auditd_binary,
        },
    )
    if receipts["build-a"]["source"]["tree_sha256"] != receipts["build-b"]["source"][
        "tree_sha256"
    ]:
        raise ArtifactError("两次独立构建的准备源码树摘要不一致")

    build = _build_metadata(args, receipt_payloads)
    manifest_value = {
        "schema_version": 1,
        "artifacts": [
            {
                "name": name,
                "bytes": len(binaries[name]),
                "sha256": _sha256(binaries[name]),
                "format": "ELF64",
                "architecture": "x86_64",
            }
            for name in MULTI_ARTIFACTS
        ],
        "build": build,
        "toolchain": _toolchain_metadata(args),
        "patch_series": _patch_series(),
    }
    manifest = _multi_manifest(manifest_value)
    _parse_multi_manifest(manifest)
    output_dir, validated_directory_identity = _validate_output_dir(
        args.output_dir,
        expected_device=args.expected_output_device,
        expected_inode=args.expected_output_inode,
    )
    _require_release_directory_entries(output_dir, packaging=True)
    payloads: list[tuple[str, bytes, int]] = []
    for name in MULTI_ARTIFACTS:
        payloads.append((name, binaries[name], 0o755))
        checksum = f"{_sha256(binaries[name])}  {name}\n".encode("ascii")
        payloads.append((f"{name}.sha256", checksum, 0o644))
    for build_id in BUILD_IDS:
        payloads.append(
            (f"{build_id}.receipt.json", receipt_payloads[build_id], 0o644)
        )
    payloads.append(("release-manifest.json", manifest, 0o644))
    directory_fd, directory_identity = _open_release_directory(
        output_dir, validated_directory_identity
    )
    try:
        if (
            args.expected_output_device is not None
            and directory_identity[0] != args.expected_output_device
        ):
            raise ArtifactError("输出目录 device 已在写入前变化")
        if (
            args.expected_output_inode is not None
            and directory_identity[1] != args.expected_output_inode
        ):
            raise ArtifactError("输出目录 inode 已在写入前变化")
        _publish_release_payloads(
            output_dir, directory_fd, directory_identity, payloads
        )
    finally:
        os.close(directory_fd)
    print(f"双二进制发布产物已生成：{output_dir}")
    print("发布 manifest：" + str(output_dir / "release-manifest.json"))


def _check_multi_expected(manifest: dict[str, Any], args: argparse.Namespace) -> None:
    build = manifest["build"]
    expected = {
        "version": args.expected_version,
        "upstream_commit": args.expected_upstream_commit,
        "overlay_commit": args.expected_overlay_commit,
    }
    for field, value in expected.items():
        if value is not None and build[field] != value:
            raise ArtifactError(f"multi manifest {field} 与期望值不一致")
    if (
        args.expected_source_date_epoch is not None
        and build["source_date_epoch"] != args.expected_source_date_epoch
    ):
        raise ArtifactError("multi manifest source_date_epoch 与期望值不一致")
    if (
        args.expected_prepared_tree_sha256 is not None
        and build["prepared_tree_sha256"] != args.expected_prepared_tree_sha256
    ):
        raise ArtifactError("multi manifest prepared_tree_sha256 与期望值不一致")
    toolchain = manifest["toolchain"]
    expected_toolchain = {
        "rustc_version": args.expected_rustc_version,
        "rustc_commit": args.expected_rustc_commit,
        "cargo_version": args.expected_cargo_version,
        "cargo_zigbuild_version": args.expected_cargo_zigbuild_version,
        "zig_version": args.expected_zig_version,
        "python_version": args.expected_python_version,
    }
    for field, value in expected_toolchain.items():
        if value is not None and toolchain[field] != value:
            raise ArtifactError(f"multi manifest toolchain.{field} 与期望值不一致")


def _verify_packaged_receipts(
    snapshot: ReleaseDirectorySnapshot, manifest: dict[str, Any]
) -> None:
    independent = manifest["build"]["independent_builds"]
    receipt_references = {item["build_id"]: item for item in independent["receipts"]}
    manifest_artifacts = {item["name"]: item for item in manifest["artifacts"]}
    source_digests: list[str] = []
    expected_build = {
        field: manifest["build"][field]
        for field in ("version", "upstream_commit", "overlay_commit", "source_date_epoch")
    }
    for build_id in BUILD_IDS:
        reference = receipt_references[build_id]
        payload = snapshot.files[reference["path"]].payload
        receipt = _parse_build_receipt(payload, build_id)
        if payload != _canonical_manifest(receipt):
            raise ArtifactError(f"{build_id} receipt 不是规范 JSON 编码")
        if _sha256(payload) != reference["sha256"]:
            raise ArtifactError(f"{build_id} receipt SHA-256 与 manifest 不一致")
        if receipt["build"] != expected_build:
            raise ArtifactError(f"{build_id} receipt build 声明与 manifest 不一致")
        if receipt["toolchain"] != manifest["toolchain"]:
            raise ArtifactError(f"{build_id} receipt toolchain 与 manifest 不一致")
        source_digests.append(receipt["source"]["tree_sha256"])
        receipt_artifacts = {item["name"]: item for item in receipt["artifacts"]}
        for name in MULTI_ARTIFACTS:
            expected_artifact = manifest_artifacts[name]
            receipt_artifact = receipt_artifacts[name]
            if (
                receipt_artifact["bytes"] != expected_artifact["bytes"]
                or receipt_artifact["sha256"] != expected_artifact["sha256"]
            ):
                raise ArtifactError(f"{build_id} receipt 的 {name} 与 manifest 不一致")
    if len(set(source_digests)) != 1:
        raise ArtifactError("两份 build receipt 的源码树摘要不一致")
    if source_digests[0] != manifest["build"]["prepared_tree_sha256"]:
        raise ArtifactError("build receipt 源码树摘要与 manifest 锁定准备源码不一致")


def _validate_multi_snapshot(
    snapshot: ReleaseDirectorySnapshot, args: argparse.Namespace
) -> None:
    manifest_payload = snapshot.files["release-manifest.json"].payload
    manifest = _parse_multi_manifest(manifest_payload)
    if manifest_payload != _multi_manifest(manifest):
        raise ArtifactError("release-manifest.json 不是规范 JSON 编码")
    _check_multi_expected(manifest, args)
    artifacts = {item["name"]: item for item in manifest["artifacts"]}
    for name in MULTI_ARTIFACTS:
        payload = snapshot.files[name].payload
        _require_elf_x86_64(payload, name)
        expected = artifacts[name]
        if len(payload) != expected["bytes"] or _sha256(payload) != expected["sha256"]:
            raise ArtifactError(f"{name} 大小或 SHA-256 与 manifest 不一致")
        checksum_payload = snapshot.files[f"{name}.sha256"].payload
        try:
            checksum_line = checksum_payload.decode("ascii")
        except UnicodeDecodeError as exc:
            raise ArtifactError(f"{name} SHA-256 文件不是 ASCII") from exc
        if checksum_line != f"{expected['sha256']}  {name}\n":
            raise ArtifactError(f"{name} SHA-256 校验失败或格式错误")
    _verify_packaged_receipts(snapshot, manifest)


def _openssl_fd_path(fd: int) -> str:
    if Path("/dev/fd").is_dir():
        return f"/dev/fd/{fd}"
    if Path("/proc/self/fd").is_dir():
        return f"/proc/self/fd/{fd}"
    raise ArtifactError("系统不支持安全传递已打开的密钥/签名文件")


def _resolve_openssl() -> tuple[Path, tuple[int, int]]:
    openssl = _resolve_build_tool("openssl", {"PATH": os.environ.get("PATH", "")})
    metadata = openssl.stat()
    return openssl, (metadata.st_dev, metadata.st_ino)


def _assert_executable_identity(executable: Path, identity: tuple[int, int]) -> None:
    metadata = executable.stat()
    if (metadata.st_dev, metadata.st_ino) != identity:
        raise ArtifactError(f"命令在执行期间被替换：{executable}")


def _sign_snapshot(manifest_payload: bytes, private_key: Path) -> bytes:
    key_payload, key_metadata = _read_regular(private_key, MAX_MANIFEST_BYTES)
    if key_metadata.st_mode & 0o077:
        raise ArtifactError("发布私钥不得授予 group/other 任何权限")
    try:
        with tempfile.TemporaryFile() as key_snapshot:
            key_snapshot.write(key_payload)
            key_snapshot.flush()
            key_snapshot.seek(0)
            key_fd = key_snapshot.fileno()
            openssl, openssl_identity = _resolve_openssl()
            completed = subprocess.run(
                [
                    str(openssl),
                    "dgst",
                    "-sha256",
                    "-sign",
                    _openssl_fd_path(key_fd),
                ],
                input=manifest_payload,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                check=False,
                pass_fds=(key_fd,),
            )
            _assert_executable_identity(openssl, openssl_identity)
    except OSError as exc:
        raise ArtifactError(f"manifest 签名失败：{exc}") from exc
    if completed.returncode != 0 or not completed.stdout:
        raise ArtifactError("manifest 签名失败")
    if len(completed.stdout) > MAX_MANIFEST_BYTES:
        raise ArtifactError("manifest 签名输出异常过大")
    return completed.stdout


def _verify_snapshot_signature(
    manifest_payload: bytes, signature_payload: bytes, public_key: Path
) -> None:
    public_key_payload, _ = _read_regular(public_key, MAX_MANIFEST_BYTES)
    try:
        with (
            tempfile.TemporaryFile() as signature_snapshot,
            tempfile.TemporaryFile() as public_key_snapshot,
        ):
            signature_snapshot.write(signature_payload)
            signature_snapshot.flush()
            signature_snapshot.seek(0)
            public_key_snapshot.write(public_key_payload)
            public_key_snapshot.flush()
            public_key_snapshot.seek(0)
            signature_fd = signature_snapshot.fileno()
            public_key_fd = public_key_snapshot.fileno()
            openssl, openssl_identity = _resolve_openssl()
            completed = subprocess.run(
                [
                    str(openssl),
                    "dgst",
                    "-sha256",
                    "-verify",
                    _openssl_fd_path(public_key_fd),
                    "-signature",
                    _openssl_fd_path(signature_fd),
                ],
                input=manifest_payload,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                check=False,
                pass_fds=(signature_fd, public_key_fd),
            )
            _assert_executable_identity(openssl, openssl_identity)
    except OSError as exc:
        raise ArtifactError(f"manifest detached 签名验证失败：{exc}") from exc
    if completed.returncode != 0:
        raise ArtifactError("manifest detached 签名验证失败")


def _validate_expected_signature_path(output_dir: Path, path: Path) -> Path:
    try:
        parent = path.parent.resolve(strict=True)
    except OSError as exc:
        raise ArtifactError(f"无法解析签名父目录：{exc}") from exc
    if path.name != "release-manifest.sig" or parent != output_dir:
        raise ArtifactError("签名必须位于 output-dir/release-manifest.sig")
    return output_dir / "release-manifest.sig"


def _validate_expected_manifest_path(output_dir: Path, path: Path) -> None:
    try:
        parent = path.parent.resolve(strict=True)
    except OSError as exc:
        raise ArtifactError(f"无法解析 manifest 父目录：{exc}") from exc
    if path.name != "release-manifest.json" or parent != output_dir:
        raise ArtifactError("manifest 必须位于 output-dir/release-manifest.json")


def _release_snapshot_names(directory_fd: int) -> frozenset[str]:
    entries = _directory_entries_at(directory_fd)
    if "release-manifest.sig" in entries:
        return UNSIGNED_RELEASE_FILES | {"release-manifest.sig"}
    return UNSIGNED_RELEASE_FILES


def command_verify_multi(args: argparse.Namespace) -> None:
    if args.expected_prepared_tree_sha256 is not None:
        _validate_prepared_tree_sha256(
            args.expected_prepared_tree_sha256, "--expected-prepared-tree-sha256"
        )
    output_dir, directory_identity = _validate_output_dir(args.output_dir)
    _validate_expected_manifest_path(output_dir, args.manifest)
    directory_fd, _ = _open_release_directory(output_dir, directory_identity)
    try:
        snapshot = _capture_release_directory_snapshot(
            output_dir, directory_fd, _release_snapshot_names(directory_fd)
        )
        _validate_multi_snapshot(snapshot, args)
        _assert_release_directory_snapshot(output_dir, directory_fd, snapshot)
    finally:
        os.close(directory_fd)
    print("双二进制发布产物、build receipts、manifest 与 SHA-256 全部通过。")


def command_sign_multi(args: argparse.Namespace) -> None:
    if args.expected_prepared_tree_sha256 is not None:
        _validate_prepared_tree_sha256(
            args.expected_prepared_tree_sha256, "--expected-prepared-tree-sha256"
        )
    output_dir, directory_identity = _validate_output_dir(args.output_dir)
    _validate_expected_manifest_path(output_dir, args.manifest)
    signature_path = _validate_expected_signature_path(output_dir, args.signature_output)
    directory_fd, _ = _open_release_directory(output_dir, directory_identity)
    created_signature: dict[str, tuple[int, int]] = {}
    try:
        unsigned_snapshot = _capture_release_directory_snapshot(
            output_dir, directory_fd, UNSIGNED_RELEASE_FILES
        )
        _validate_multi_snapshot(unsigned_snapshot, args)
        manifest_payload = unsigned_snapshot.files["release-manifest.json"].payload
        signature_payload = _sign_snapshot(manifest_payload, args.private_key)
        _assert_release_directory_snapshot(output_dir, directory_fd, unsigned_snapshot)
        created_signature["release-manifest.sig"] = _exclusive_write_at(
            directory_fd, "release-manifest.sig", signature_payload, 0o644
        )
        os.fsync(directory_fd)
        signed_snapshot = _capture_release_directory_snapshot(
            output_dir,
            directory_fd,
            UNSIGNED_RELEASE_FILES | {"release-manifest.sig"},
        )
        for name, original in unsigned_snapshot.files.items():
            if signed_snapshot.files[name] != original:
                raise ArtifactError(f"签名期间发布成员发生变化：{name}")
        signature_snapshot = signed_snapshot.files["release-manifest.sig"]
        if (
            signature_snapshot.payload != signature_payload
            or signature_snapshot.identity
            != created_signature["release-manifest.sig"]
        ):
            raise ArtifactError("发布签名写入后与内存结果不一致")
    except Exception:
        _unlink_created_release_files(directory_fd, created_signature)
        raise
    finally:
        os.close(directory_fd)
    print(f"已生成 detached SHA-256 签名：{signature_path}")


def command_verify_signed_multi(args: argparse.Namespace) -> None:
    if args.expected_prepared_tree_sha256 is not None:
        _validate_prepared_tree_sha256(
            args.expected_prepared_tree_sha256, "--expected-prepared-tree-sha256"
        )
    output_dir, directory_identity = _validate_output_dir(args.output_dir)
    _validate_expected_manifest_path(output_dir, args.manifest)
    signature_path = _validate_expected_signature_path(output_dir, args.signature)
    directory_fd, _ = _open_release_directory(output_dir, directory_identity)
    try:
        snapshot = _capture_release_directory_snapshot(
            output_dir,
            directory_fd,
            UNSIGNED_RELEASE_FILES | {"release-manifest.sig"},
        )
        manifest_payload = snapshot.files["release-manifest.json"].payload
        _verify_snapshot_signature(
            manifest_payload,
            snapshot.files[signature_path.name].payload,
            args.public_key,
        )
        _validate_multi_snapshot(snapshot, args)
        _assert_release_directory_snapshot(output_dir, directory_fd, snapshot)
    finally:
        os.close(directory_fd)
    print("签名验证通过；双二进制发布产物、来源与 SHA-256 全部匹配。")


def command_source_tree_sha256(args: argparse.Namespace) -> None:
    source_root = _resolved_directory(args.source_root, "source root")
    print(_tree_sha256(source_root))


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="创建或校验可复现 Linux x86_64 双二进制发布目录")
    subparsers = parser.add_subparsers(dest="command", required=True)

    source_tree_sha256 = subparsers.add_parser(
        "source-tree-sha256",
        help="计算规范准备源码树 SHA-256",
    )
    source_tree_sha256.add_argument("--source-root", required=True, type=Path)
    source_tree_sha256.set_defaults(handler=command_source_tree_sha256)

    build_receipt = subparsers.add_parser(
        "build-and-receipt",
        help="执行一次独立 cargo-zigbuild 并生成 canonical provenance receipt",
    )
    build_receipt.add_argument("--build-id", required=True, choices=BUILD_IDS)
    build_receipt.add_argument("--source-root", required=True, type=Path)
    build_receipt.add_argument("--target-root", required=True, type=Path)
    build_receipt.add_argument("--cargo-home", required=True, type=Path)
    build_receipt.add_argument("--version", required=True)
    build_receipt.add_argument("--upstream-commit", required=True)
    build_receipt.add_argument("--overlay-commit", required=True)
    build_receipt.add_argument("--source-date-epoch", required=True, type=int)
    build_receipt.add_argument("--expected-prepared-tree-sha256", required=True)
    build_receipt.add_argument("--rustc-version", required=True)
    build_receipt.add_argument("--rustc-commit", required=True)
    build_receipt.add_argument("--cargo-version", required=True)
    build_receipt.add_argument("--cargo-zigbuild-version", required=True)
    build_receipt.add_argument("--zig-version", required=True)
    build_receipt.add_argument("--python-version", required=True)
    build_receipt.set_defaults(handler=command_build_and_receipt)

    package_multi = subparsers.add_parser(
        "package-multi",
        help="打包 ssserver 与 shadowsocks-auditd 双二进制发布产物",
    )
    package_multi.add_argument("--binary", required=True, type=Path)
    package_multi.add_argument("--auditd-binary", required=True, type=Path)
    package_multi.add_argument("--second-binary", required=True, type=Path)
    package_multi.add_argument("--second-auditd-binary", required=True, type=Path)
    package_multi.add_argument("--first-build-receipt", required=True, type=Path)
    package_multi.add_argument("--second-build-receipt", required=True, type=Path)
    package_multi.add_argument("--first-source-root", required=True, type=Path)
    package_multi.add_argument("--second-source-root", required=True, type=Path)
    package_multi.add_argument("--first-target-root", required=True, type=Path)
    package_multi.add_argument("--second-target-root", required=True, type=Path)
    package_multi.add_argument("--output-dir", required=True, type=Path)
    package_multi.add_argument("--expected-output-device", type=int)
    package_multi.add_argument("--expected-output-inode", type=int)
    package_multi.add_argument("--version", required=True)
    package_multi.add_argument("--upstream-commit", required=True)
    package_multi.add_argument("--overlay-commit", required=True)
    package_multi.add_argument("--source-date-epoch", required=True, type=int)
    package_multi.add_argument("--expected-prepared-tree-sha256", required=True)
    package_multi.add_argument("--rustc-version", required=True)
    package_multi.add_argument("--rustc-commit", required=True)
    package_multi.add_argument("--cargo-version", required=True)
    package_multi.add_argument("--cargo-zigbuild-version", required=True)
    package_multi.add_argument("--zig-version", required=True)
    package_multi.add_argument("--python-version", required=True)
    package_multi.set_defaults(handler=command_package_multi)

    verify_multi = subparsers.add_parser(
        "verify-multi",
        help="校验双二进制发布产物",
    )
    verify_multi.add_argument("--output-dir", required=True, type=Path)
    verify_multi.add_argument("--manifest", required=True, type=Path)
    verify_multi.add_argument("--expected-version")
    verify_multi.add_argument("--expected-upstream-commit")
    verify_multi.add_argument("--expected-overlay-commit")
    verify_multi.add_argument("--expected-source-date-epoch", type=int)
    verify_multi.add_argument("--expected-prepared-tree-sha256")
    verify_multi.add_argument("--expected-rustc-version")
    verify_multi.add_argument("--expected-rustc-commit")
    verify_multi.add_argument("--expected-cargo-version")
    verify_multi.add_argument("--expected-cargo-zigbuild-version")
    verify_multi.add_argument("--expected-zig-version")
    verify_multi.add_argument("--expected-python-version")
    verify_multi.set_defaults(handler=command_verify_multi)

    sign_multi = subparsers.add_parser(
        "sign-multi",
        help="校验双二进制发布产物并对同一 manifest 快照签名",
    )
    sign_multi.add_argument("--output-dir", required=True, type=Path)
    sign_multi.add_argument("--manifest", required=True, type=Path)
    sign_multi.add_argument("--private-key", required=True, type=Path)
    sign_multi.add_argument("--signature-output", required=True, type=Path)
    sign_multi.add_argument("--expected-version")
    sign_multi.add_argument("--expected-upstream-commit")
    sign_multi.add_argument("--expected-overlay-commit")
    sign_multi.add_argument("--expected-source-date-epoch", type=int)
    sign_multi.add_argument("--expected-prepared-tree-sha256")
    sign_multi.add_argument("--expected-rustc-version")
    sign_multi.add_argument("--expected-rustc-commit")
    sign_multi.add_argument("--expected-cargo-version")
    sign_multi.add_argument("--expected-cargo-zigbuild-version")
    sign_multi.add_argument("--expected-zig-version")
    sign_multi.add_argument("--expected-python-version")
    sign_multi.set_defaults(handler=command_sign_multi)

    verify_signed_multi = subparsers.add_parser(
        "verify-signed-multi",
        help="用同一 manifest 快照验签并校验双二进制发布产物",
    )
    verify_signed_multi.add_argument("--output-dir", required=True, type=Path)
    verify_signed_multi.add_argument("--manifest", required=True, type=Path)
    verify_signed_multi.add_argument("--signature", required=True, type=Path)
    verify_signed_multi.add_argument("--public-key", required=True, type=Path)
    verify_signed_multi.add_argument("--expected-version")
    verify_signed_multi.add_argument("--expected-upstream-commit")
    verify_signed_multi.add_argument("--expected-overlay-commit")
    verify_signed_multi.add_argument("--expected-source-date-epoch", type=int)
    verify_signed_multi.add_argument("--expected-prepared-tree-sha256")
    verify_signed_multi.add_argument("--expected-rustc-version")
    verify_signed_multi.add_argument("--expected-rustc-commit")
    verify_signed_multi.add_argument("--expected-cargo-version")
    verify_signed_multi.add_argument("--expected-cargo-zigbuild-version")
    verify_signed_multi.add_argument("--expected-zig-version")
    verify_signed_multi.add_argument("--expected-python-version")
    verify_signed_multi.set_defaults(handler=command_verify_signed_multi)
    return parser


def main() -> int:
    parser = build_parser()
    args = parser.parse_args()
    try:
        args.handler(args)
    except (ArtifactError, OSError) as exc:
        print(f"错误：{exc}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
