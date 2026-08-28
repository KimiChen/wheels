#!/usr/bin/env python3
"""Create and verify deterministic Linux x86_64 release archives."""

from __future__ import annotations

import argparse
import gzip
import hashlib
import io
import json
import os
import re
import stat
import struct
import sys
import tarfile
from pathlib import Path
from typing import Any


TARGET = "x86_64-unknown-linux-musl"
MAX_BINARY_BYTES = 256 * 1024 * 1024
MAX_MANIFEST_BYTES = 256 * 1024
MAX_ARCHIVE_BYTES = 256 * 1024 * 1024
COMMIT_PATTERN = re.compile(r"[0-9a-f]{40}\Z")
SHA256_PATTERN = re.compile(r"[0-9a-f]{64}\Z")
VERSION_PATTERN = re.compile(r"v[0-9A-Za-z][0-9A-Za-z.+_-]{0,63}\Z")
TOOL_VERSION_PATTERN = re.compile(r"[0-9]+(?:\.[0-9A-Za-z+-]+)+\Z")


class ArtifactError(RuntimeError):
    """Expected validation or packaging failure."""


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
    if len(payload) < 20 or payload[:4] != b"\x7fELF":
        raise ArtifactError(f"{label} 不是 ELF")
    if payload[4] != 2:
        raise ArtifactError(f"{label} 不是 ELF64")
    if payload[5] != 1:
        raise ArtifactError(f"{label} 不是 little-endian ELF")
    if payload[6] != 1:
        raise ArtifactError(f"{label} 的 ELF ident version 错误")
    elf_type = struct.unpack_from("<H", payload, 16)[0]
    if elf_type not in (2, 3):
        raise ArtifactError(f"{label} 不是 ELF executable/shared-object")
    machine = struct.unpack_from("<H", payload, 18)[0]
    if machine != 62:
        raise ArtifactError(f"{label} 的 ELF machine 不是 x86_64")


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


def _parse_manifest(payload: bytes) -> dict[str, Any]:
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
    if not isinstance(value, dict):
        raise ArtifactError("manifest 顶层必须是对象")
    _exact_keys(value, {"schema_version", "artifact", "build", "toolchain", "patch_series"}, "manifest")
    if value["patch_series"] != _patch_series():
        raise ArtifactError("manifest patch_series 与仓库不一致")
    if type(value["schema_version"]) is not int or value["schema_version"] != 1:
        raise ArtifactError("manifest schema_version 必须为 1")

    artifact = value["artifact"]
    if not isinstance(artifact, dict):
        raise ArtifactError("manifest.artifact 必须是对象")
    _exact_keys(
        artifact,
        {"name", "bytes", "sha256", "format", "architecture"},
        "manifest.artifact",
    )
    if artifact["name"] != "ssserver":
        raise ArtifactError("manifest artifact.name 必须为 ssserver")
    if type(artifact["bytes"]) is not int or not 1 <= artifact["bytes"] <= MAX_BINARY_BYTES:
        raise ArtifactError("manifest artifact.bytes 必须是正整数")
    if not isinstance(artifact["sha256"], str) or not SHA256_PATTERN.fullmatch(
        artifact["sha256"]
    ):
        raise ArtifactError("manifest artifact.sha256 格式错误")
    if artifact["format"] != "ELF64" or artifact["architecture"] != "x86_64":
        raise ArtifactError("manifest artifact ELF 格式或架构错误")

    build = value["build"]
    if not isinstance(build, dict):
        raise ArtifactError("manifest.build 必须是对象")
    _exact_keys(
        build,
        {
            "version",
            "upstream_commit",
            "overlay_commit",
            "target",
            "source_date_epoch",
            "independent_builds",
        },
        "manifest.build",
    )
    if not isinstance(build["version"], str) or not VERSION_PATTERN.fullmatch(build["version"]):
        raise ArtifactError("manifest build.version 格式错误")
    for field in ("upstream_commit", "overlay_commit"):
        if not isinstance(build[field], str) or not COMMIT_PATTERN.fullmatch(build[field]):
            raise ArtifactError(f"manifest build.{field} 格式错误")
    if build["target"] != TARGET:
        raise ArtifactError(f"manifest build.target 必须为 {TARGET}")
    if (
        type(build["source_date_epoch"]) is not int
        or not 1 <= build["source_date_epoch"] <= 0xFFFFFFFF
    ):
        raise ArtifactError("manifest build.source_date_epoch 必须是正整数")
    if build["independent_builds"] != 2:
        raise ArtifactError("manifest 必须记录 2 次独立构建通过一致性检查")

    toolchain = value["toolchain"]
    if not isinstance(toolchain, dict):
        raise ArtifactError("manifest.toolchain 必须是对象")
    _exact_keys(
        toolchain,
        {
            "rustc_version",
            "rustc_commit",
            "cargo_version",
            "cargo_zigbuild_version",
            "zig_version",
            "python_version",
            "zlib_version",
        },
        "manifest.toolchain",
    )
    for field, item in toolchain.items():
        if not isinstance(item, str) or not item or len(item) > 256 or "\n" in item:
            raise ArtifactError(f"manifest toolchain.{field} 格式错误")
    if not COMMIT_PATTERN.fullmatch(toolchain["rustc_commit"]):
        raise ArtifactError("manifest toolchain.rustc_commit 格式错误")
    for field in (
        "rustc_version",
        "cargo_version",
        "cargo_zigbuild_version",
        "zig_version",
        "python_version",
        "zlib_version",
    ):
        if not TOOL_VERSION_PATTERN.fullmatch(toolchain[field]):
            raise ArtifactError(f"manifest toolchain.{field} 版本格式错误")
    return value


def _canonical_manifest(value: dict[str, Any]) -> bytes:
    return (json.dumps(value, ensure_ascii=True, indent=2, sort_keys=True) + "\n").encode(
        "utf-8"
    )


def _tar_info(name: str, payload: bytes, mode: int, epoch: int) -> tarfile.TarInfo:
    info = tarfile.TarInfo(name=name)
    info.size = len(payload)
    info.mode = mode
    info.mtime = epoch
    info.uid = 0
    info.gid = 0
    info.uname = "root"
    info.gname = "root"
    info.type = tarfile.REGTYPE
    return info


def _deterministic_archive(
    root_name: str, binary: bytes, manifest: bytes, epoch: int
) -> bytes:
    tar_buffer = io.BytesIO()
    entries = [
        (f"{root_name}/manifest.json", manifest, 0o644),
        (f"{root_name}/ssserver", binary, 0o755),
    ]
    with tarfile.open(fileobj=tar_buffer, mode="w", format=tarfile.USTAR_FORMAT) as archive:
        for name, payload, mode in entries:
            archive.addfile(_tar_info(name, payload, mode, epoch), io.BytesIO(payload))

    gzip_buffer = io.BytesIO()
    with gzip.GzipFile(
        filename="", mode="wb", compresslevel=9, fileobj=gzip_buffer, mtime=epoch
    ) as compressed:
        compressed.write(tar_buffer.getvalue())
    return gzip_buffer.getvalue()


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


def command_package(args: argparse.Namespace) -> None:
    if not VERSION_PATTERN.fullmatch(args.version):
        raise ArtifactError("--version 格式错误")
    for label, commit in (
        ("--upstream-commit", args.upstream_commit),
        ("--overlay-commit", args.overlay_commit),
    ):
        if not COMMIT_PATTERN.fullmatch(commit):
            raise ArtifactError(f"{label} 必须是 40 位小写十六进制 commit")
    if not 1 <= args.source_date_epoch <= 0xFFFFFFFF:
        raise ArtifactError("--source-date-epoch 必须在 1..4294967295 范围内")

    binary, metadata = _read_regular(args.binary, MAX_BINARY_BYTES)
    if not metadata.st_mode & 0o111:
        raise ArtifactError("ssserver 输入文件没有可执行位")
    _require_elf_x86_64(binary, "ssserver")

    manifest_value = {
        "schema_version": 1,
        "artifact": {
            "name": "ssserver",
            "bytes": len(binary),
            "sha256": _sha256(binary),
            "format": "ELF64",
            "architecture": "x86_64",
        },
        "build": {
            "version": args.version,
            "upstream_commit": args.upstream_commit,
            "overlay_commit": args.overlay_commit,
            "target": TARGET,
            "source_date_epoch": args.source_date_epoch,
            "independent_builds": 2,
        },
        "toolchain": {
            "rustc_version": args.rustc_version,
            "rustc_commit": args.rustc_commit,
            "cargo_version": args.cargo_version,
            "cargo_zigbuild_version": args.cargo_zigbuild_version,
            "zig_version": args.zig_version,
            "python_version": args.python_version,
            "zlib_version": args.zlib_version,
        },
        "patch_series": _patch_series(),
    }
    _parse_manifest(_canonical_manifest(manifest_value))
    manifest = _canonical_manifest(manifest_value)

    root_name = f"shadowsocks-rust-plus-{args.version}-{TARGET}"
    archive_name = f"{root_name}.tar.gz"
    manifest_name = f"{root_name}.manifest.json"
    checksum_name = f"{archive_name}.sha256"
    archive = _deterministic_archive(root_name, binary, manifest, args.source_date_epoch)
    checksum = f"{_sha256(archive)}  {archive_name}\n".encode("ascii")

    output_dir = args.output_dir.resolve(strict=True)
    if not output_dir.is_dir():
        raise ArtifactError(f"输出路径不是目录：{output_dir}")
    output_metadata = output_dir.stat()
    if output_metadata.st_uid not in (0, os.geteuid()) or output_metadata.st_mode & 0o022:
        raise ArtifactError("输出目录必须由当前用户/root 持有且不得允许 group/other 写入")
    paths = [
        (output_dir / archive_name, archive, 0o644),
        (output_dir / manifest_name, manifest, 0o644),
        (output_dir / checksum_name, checksum, 0o644),
    ]
    for path, _, _ in paths:
        if os.path.lexists(path):
            raise ArtifactError(f"输出已存在，拒绝覆盖：{path}")

    created: list[Path] = []
    try:
        for path, payload, mode in paths:
            _exclusive_write(path, payload, mode)
            created.append(path)
    except Exception:
        for path in created:
            try:
                path.unlink()
            except OSError:
                pass
        raise

    print(f"Linux x86_64 发布包：{paths[0][0]}")
    print(f"发布 manifest：{paths[1][0]}")
    print(f"发布包 SHA-256：{paths[2][0]}")


def _check_expected(manifest: dict[str, Any], args: argparse.Namespace) -> None:
    build = manifest["build"]
    expected = {
        "version": args.expected_version,
        "upstream_commit": args.expected_upstream_commit,
        "overlay_commit": args.expected_overlay_commit,
    }
    for field, value in expected.items():
        if value is not None and build[field] != value:
            raise ArtifactError(f"manifest {field} 与期望值不一致")
    toolchain = manifest["toolchain"]
    expected_toolchain = {
        "rustc_version": args.expected_rustc_version,
        "rustc_commit": args.expected_rustc_commit,
        "cargo_version": args.expected_cargo_version,
        "cargo_zigbuild_version": args.expected_cargo_zigbuild_version,
        "zig_version": args.expected_zig_version,
        "python_version": args.expected_python_version,
        "zlib_version": args.expected_zlib_version,
    }
    for field, value in expected_toolchain.items():
        if value is not None and toolchain[field] != value:
            raise ArtifactError(f"manifest toolchain.{field} 与锁定值不一致")


def command_verify(args: argparse.Namespace) -> None:
    archive, _ = _read_regular(args.archive, MAX_ARCHIVE_BYTES)
    manifest_payload, _ = _read_regular(args.manifest, MAX_MANIFEST_BYTES)
    checksum_payload, _ = _read_regular(args.checksum, 1024)
    manifest = _parse_manifest(manifest_payload)
    if manifest_payload != _canonical_manifest(manifest):
        raise ArtifactError("manifest 不是规范 JSON 编码")
    _check_expected(manifest, args)

    try:
        checksum_line = checksum_payload.decode("ascii")
    except UnicodeDecodeError as exc:
        raise ArtifactError("SHA-256 文件不是 ASCII") from exc
    expected_line = f"{_sha256(archive)}  {args.archive.name}\n"
    if checksum_line != expected_line:
        raise ArtifactError("发布包 SHA-256 校验失败或校验文件格式错误")

    if len(archive) < 10 or archive[:4] != b"\x1f\x8b\x08\x00":
        raise ArtifactError("发布包不是无可变文件名字段的规范 gzip")
    gzip_epoch = struct.unpack_from("<I", archive, 4)[0]
    if gzip_epoch != manifest["build"]["source_date_epoch"]:
        raise ArtifactError("gzip mtime 与 manifest source_date_epoch 不一致")

    root_name = f"shadowsocks-rust-plus-{manifest['build']['version']}-{TARGET}"
    expected_names = [f"{root_name}/manifest.json", f"{root_name}/ssserver"]
    try:
        with tarfile.open(fileobj=io.BytesIO(archive), mode="r:gz") as package:
            members = package.getmembers()
            if [member.name for member in members] != expected_names:
                raise ArtifactError("发布包成员列表、顺序或根目录不符合契约")
            contents: dict[str, bytes] = {}
            for member in members:
                expected_mode = 0o644 if member.name.endswith("manifest.json") else 0o755
                if not member.isfile() or member.mode != expected_mode:
                    raise ArtifactError(f"发布包成员类型或权限错误：{member.name}")
                if (
                    member.uid != 0
                    or member.gid != 0
                    or member.uname != "root"
                    or member.gname != "root"
                    or member.mtime != manifest["build"]["source_date_epoch"]
                ):
                    raise ArtifactError(f"发布包成员元数据不可复现：{member.name}")
                expected_size = (
                    len(manifest_payload)
                    if member.name.endswith("manifest.json")
                    else manifest["artifact"]["bytes"]
                )
                if member.size != expected_size:
                    raise ArtifactError(f"发布包成员大小与 manifest 不一致：{member.name}")
                extracted = package.extractfile(member)
                if extracted is None:
                    raise ArtifactError(f"无法读取发布包成员：{member.name}")
                contents[member.name] = extracted.read()
    except (tarfile.TarError, OSError) as exc:
        raise ArtifactError("发布包 tar/gzip 解析失败") from exc

    if contents[expected_names[0]] != manifest_payload:
        raise ArtifactError("发布包内外 manifest 不一致")
    binary = contents[expected_names[1]]
    _require_elf_x86_64(binary, "发布包 ssserver")
    artifact = manifest["artifact"]
    if len(binary) != artifact["bytes"] or _sha256(binary) != artifact["sha256"]:
        raise ArtifactError("ssserver 大小或 SHA-256 与 manifest 不一致")
    print(
        "发布包结构、ELF x86_64、manifest 来源字段、二进制 SHA-256 与归档 SHA-256 均通过。"
    )


MULTI_ARTIFACTS = ("ssserver", "shadowsocks-auditd")


def _read_executable(path: Path, label: str) -> bytes:
    payload, metadata = _read_regular(path, MAX_BINARY_BYTES)
    if not metadata.st_mode & 0o111:
        raise ArtifactError(f"{label} 输入文件没有可执行位")
    _require_elf_x86_64(payload, label)
    return payload


def _build_metadata(args: argparse.Namespace) -> dict[str, Any]:
    if not VERSION_PATTERN.fullmatch(args.version):
        raise ArtifactError("--version 格式错误")
    for label, commit in (
        ("--upstream-commit", args.upstream_commit),
        ("--overlay-commit", args.overlay_commit),
    ):
        if not COMMIT_PATTERN.fullmatch(commit):
            raise ArtifactError(f"{label} 必须是 40 位小写十六进制 commit")
    if not 1 <= args.source_date_epoch <= 0xFFFFFFFF:
        raise ArtifactError("--source-date-epoch 必须在 1..4294967295 范围内")
    return {
        "version": args.version,
        "upstream_commit": args.upstream_commit,
        "overlay_commit": args.overlay_commit,
        "target": TARGET,
        "source_date_epoch": args.source_date_epoch,
        "independent_builds": 2,
    }


def _toolchain_metadata(args: argparse.Namespace) -> dict[str, str]:
    return {
        "rustc_version": args.rustc_version,
        "rustc_commit": args.rustc_commit,
        "cargo_version": args.cargo_version,
        "cargo_zigbuild_version": args.cargo_zigbuild_version,
        "zig_version": args.zig_version,
        "python_version": args.python_version,
        "zlib_version": args.zlib_version,
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
        {"version", "upstream_commit", "overlay_commit", "target", "source_date_epoch", "independent_builds"},
        "manifest.build",
    )
    if not isinstance(build["version"], str) or not VERSION_PATTERN.fullmatch(build["version"]):
        raise ArtifactError("multi manifest build.version 格式错误")
    for field in ("upstream_commit", "overlay_commit"):
        if not isinstance(build[field], str) or not COMMIT_PATTERN.fullmatch(build[field]):
            raise ArtifactError(f"multi manifest build.{field} 格式错误")
    if (
        build["target"] != TARGET
        or type(build["independent_builds"]) is not int
        or build["independent_builds"] != 2
    ):
        raise ArtifactError("multi manifest build 字段错误")
    if type(build["source_date_epoch"]) is not int or not 1 <= build["source_date_epoch"] <= 0xFFFFFFFF:
        raise ArtifactError("multi manifest source_date_epoch 格式错误")
    toolchain = value.get("toolchain")
    if not isinstance(toolchain, dict):
        raise ArtifactError("multi manifest.toolchain 必须是对象")
    expected = {
        "rustc_version",
        "rustc_commit",
        "cargo_version",
        "cargo_zigbuild_version",
        "zig_version",
        "python_version",
        "zlib_version",
    }
    _exact_keys(toolchain, expected, "manifest.toolchain")
    for field, item in toolchain.items():
        if not isinstance(item, str) or not item or len(item) > 256 or "\n" in item:
            raise ArtifactError(f"multi manifest toolchain.{field} 格式错误")
    if not COMMIT_PATTERN.fullmatch(toolchain["rustc_commit"]):
        raise ArtifactError("multi manifest rustc_commit 格式错误")
    for field in expected - {"rustc_commit"}:
        if not TOOL_VERSION_PATTERN.fullmatch(toolchain[field]):
            raise ArtifactError(f"multi manifest toolchain.{field} 版本格式错误")


def _validate_output_dir(path: Path) -> Path:
    output_dir = path.resolve(strict=True)
    if not output_dir.is_dir():
        raise ArtifactError(f"输出路径不是目录：{output_dir}")
    metadata = output_dir.stat()
    if metadata.st_uid not in (0, os.geteuid()) or metadata.st_mode & 0o022:
        raise ArtifactError("输出目录必须由当前用户/root 持有且不得允许 group/other 写入")
    return output_dir


def command_package_multi(args: argparse.Namespace) -> None:
    build = _build_metadata(args)
    binaries = {
        "ssserver": _read_executable(args.binary, "ssserver"),
        "shadowsocks-auditd": _read_executable(args.auditd_binary, "shadowsocks-auditd"),
    }
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
    output_dir = _validate_output_dir(args.output_dir)
    payloads: list[tuple[Path, bytes, int]] = []
    for name in MULTI_ARTIFACTS:
        payloads.append((output_dir / name, binaries[name], 0o755))
        checksum = f"{_sha256(binaries[name])}  {name}\n".encode("ascii")
        payloads.append((output_dir / f"{name}.sha256", checksum, 0o644))
    payloads.append((output_dir / "release-manifest.json", manifest, 0o644))
    for path, _, _ in payloads:
        if os.path.lexists(path):
            raise ArtifactError(f"输出已存在，拒绝覆盖：{path}")
    created: list[Path] = []
    try:
        for path, payload, mode in payloads:
            _exclusive_write(path, payload, mode)
            created.append(path)
    except Exception:
        for path in created:
            try:
                path.unlink()
            except OSError:
                pass
        raise
    print(f"双二进制发布产物已生成：{output_dir}")
    print("发布 manifest：" + str(output_dir / "release-manifest.json"))


def _read_checksum(path: Path, binary_name: str, expected_hash: str) -> None:
    payload, metadata = _read_regular(path, 1024)
    if stat.S_IMODE(metadata.st_mode) != 0o644:
        raise ArtifactError(f"{binary_name} SHA-256 文件权限必须为 0644")
    try:
        line = payload.decode("ascii")
    except UnicodeDecodeError as exc:
        raise ArtifactError(f"{binary_name} SHA-256 文件不是 ASCII") from exc
    if line != f"{expected_hash}  {binary_name}\n":
        raise ArtifactError(f"{binary_name} SHA-256 校验失败或格式错误")


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
    toolchain = manifest["toolchain"]
    expected_toolchain = {
        "rustc_version": args.expected_rustc_version,
        "rustc_commit": args.expected_rustc_commit,
        "cargo_version": args.expected_cargo_version,
        "cargo_zigbuild_version": args.expected_cargo_zigbuild_version,
        "zig_version": args.expected_zig_version,
        "python_version": args.expected_python_version,
        "zlib_version": args.expected_zlib_version,
    }
    for field, value in expected_toolchain.items():
        if value is not None and toolchain[field] != value:
            raise ArtifactError(f"multi manifest toolchain.{field} 与期望值不一致")


def command_verify_multi(args: argparse.Namespace) -> None:
    output_dir = _validate_output_dir(args.output_dir)
    if args.manifest.is_symlink():
        raise ArtifactError("release-manifest.json 不能是符号链接")
    manifest_path = args.manifest.resolve(strict=True)
    expected_manifest_path = (output_dir / "release-manifest.json").resolve()
    if manifest_path != expected_manifest_path:
        raise ArtifactError("manifest 必须位于 output-dir/release-manifest.json")
    manifest_payload, manifest_metadata = _read_regular(manifest_path, MAX_MANIFEST_BYTES)
    if stat.S_IMODE(manifest_metadata.st_mode) != 0o644:
        raise ArtifactError("release-manifest.json 权限必须为 0644")
    manifest = _parse_multi_manifest(manifest_payload)
    if manifest_payload != _multi_manifest(manifest):
        raise ArtifactError("release-manifest.json 不是规范 JSON 编码")
    _check_multi_expected(manifest, args)
    artifacts = {item["name"]: item for item in manifest["artifacts"]}
    for name in MULTI_ARTIFACTS:
        payload, metadata = _read_regular(output_dir / name, MAX_BINARY_BYTES)
        if stat.S_IMODE(metadata.st_mode) != 0o755:
            raise ArtifactError(f"{name} 权限必须为 0755")
        _require_elf_x86_64(payload, name)
        expected = artifacts[name]
        if len(payload) != expected["bytes"] or _sha256(payload) != expected["sha256"]:
            raise ArtifactError(f"{name} 大小或 SHA-256 与 manifest 不一致")
        _read_checksum(args.output_dir / f"{name}.sha256", name, expected["sha256"])
    print("双二进制发布产物、manifest 与 SHA-256 全部通过。")


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="创建或校验可复现 Linux x86_64 发布包")
    subparsers = parser.add_subparsers(dest="command", required=True)

    package = subparsers.add_parser("package")
    package.add_argument("--binary", required=True, type=Path)
    package.add_argument("--output-dir", required=True, type=Path)
    package.add_argument("--version", required=True)
    package.add_argument("--upstream-commit", required=True)
    package.add_argument("--overlay-commit", required=True)
    package.add_argument("--source-date-epoch", required=True, type=int)
    package.add_argument("--rustc-version", required=True)
    package.add_argument("--rustc-commit", required=True)
    package.add_argument("--cargo-version", required=True)
    package.add_argument("--cargo-zigbuild-version", required=True)
    package.add_argument("--zig-version", required=True)
    package.add_argument("--python-version", required=True)
    package.add_argument("--zlib-version", required=True)
    package.set_defaults(handler=command_package)

    package_multi = subparsers.add_parser(
        "package-multi",
        help="打包 ssserver 与 shadowsocks-auditd 双二进制发布产物",
    )
    package_multi.add_argument("--binary", required=True, type=Path)
    package_multi.add_argument("--auditd-binary", required=True, type=Path)
    package_multi.add_argument("--output-dir", required=True, type=Path)
    package_multi.add_argument("--version", required=True)
    package_multi.add_argument("--upstream-commit", required=True)
    package_multi.add_argument("--overlay-commit", required=True)
    package_multi.add_argument("--source-date-epoch", required=True, type=int)
    package_multi.add_argument("--rustc-version", required=True)
    package_multi.add_argument("--rustc-commit", required=True)
    package_multi.add_argument("--cargo-version", required=True)
    package_multi.add_argument("--cargo-zigbuild-version", required=True)
    package_multi.add_argument("--zig-version", required=True)
    package_multi.add_argument("--python-version", required=True)
    package_multi.add_argument("--zlib-version", required=True)
    package_multi.set_defaults(handler=command_package_multi)

    verify = subparsers.add_parser("verify")
    verify.add_argument("--archive", required=True, type=Path)
    verify.add_argument("--manifest", required=True, type=Path)
    verify.add_argument("--checksum", required=True, type=Path)
    verify.add_argument("--expected-version")
    verify.add_argument("--expected-upstream-commit")
    verify.add_argument("--expected-overlay-commit")
    verify.add_argument("--expected-rustc-version")
    verify.add_argument("--expected-rustc-commit")
    verify.add_argument("--expected-cargo-version")
    verify.add_argument("--expected-cargo-zigbuild-version")
    verify.add_argument("--expected-zig-version")
    verify.add_argument("--expected-python-version")
    verify.add_argument("--expected-zlib-version")
    verify.set_defaults(handler=command_verify)

    verify_multi = subparsers.add_parser(
        "verify-multi",
        help="校验双二进制发布产物",
    )
    verify_multi.add_argument("--output-dir", required=True, type=Path)
    verify_multi.add_argument("--manifest", required=True, type=Path)
    verify_multi.add_argument("--expected-version")
    verify_multi.add_argument("--expected-upstream-commit")
    verify_multi.add_argument("--expected-overlay-commit")
    verify_multi.add_argument("--expected-rustc-version")
    verify_multi.add_argument("--expected-rustc-commit")
    verify_multi.add_argument("--expected-cargo-version")
    verify_multi.add_argument("--expected-cargo-zigbuild-version")
    verify_multi.add_argument("--expected-zig-version")
    verify_multi.add_argument("--expected-python-version")
    verify_multi.add_argument("--expected-zlib-version")
    verify_multi.set_defaults(handler=command_verify_multi)
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
