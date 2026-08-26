#!/usr/bin/env python3
"""Generate and validate the private AEAD-2022 cluster credential source.

The command deliberately never prints iPSK/uPSK values. Secret-bearing output is
written only to an explicit, previously non-existent, mode-0600 file.
"""

from __future__ import annotations

import argparse
import base64
import binascii
import json
import os
import secrets
import stat
import subprocess
import sys
from pathlib import Path
from typing import Any


PROJECT_ROOT = Path(__file__).resolve().parent.parent
SCHEMA_VERSION = 2
METHOD = "2022-blake3-aes-128-gcm"
KEY_BYTES = 16
MAX_USERS = 1_000
MAX_SOURCE_BYTES = 4 * 1024 * 1024
MAX_CONFIG_BYTES = 8 * 1024 * 1024


class ToolError(RuntimeError):
    """Expected, safely reportable command failure."""


def _strict_object(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise ToolError(f"JSON 包含重复字段：{key}")
        result[key] = value
    return result


def _load_json(path: Path, *, max_bytes: int, private: bool) -> Any:
    flags = os.O_RDONLY
    if hasattr(os, "O_CLOEXEC"):
        flags |= os.O_CLOEXEC
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW

    try:
        fd = os.open(path, flags)
    except OSError as exc:
        raise ToolError(f"无法安全打开输入文件 {path}：{exc.strerror}") from exc

    try:
        metadata = os.fstat(fd)
        if not stat.S_ISREG(metadata.st_mode):
            raise ToolError(f"输入路径不是普通文件：{path}")
        if private and metadata.st_mode & 0o077:
            raise ToolError(f"凭据源权限过宽，要求 group/other 无权限：{path}")
        chunks: list[bytes] = []
        remaining = max_bytes + 1
        while remaining:
            chunk = os.read(fd, min(remaining, 64 * 1024))
            if not chunk:
                break
            chunks.append(chunk)
            remaining -= len(chunk)
        raw = b"".join(chunks)
    finally:
        os.close(fd)

    if len(raw) > max_bytes:
        raise ToolError(f"输入文件超过 {max_bytes} 字节上限：{path}")
    try:
        text = raw.decode("utf-8")
    except UnicodeDecodeError as exc:
        raise ToolError(f"输入文件不是有效 UTF-8：{path}") from exc
    try:
        return json.loads(text, object_pairs_hook=_strict_object)
    except ToolError:
        raise
    except json.JSONDecodeError as exc:
        raise ToolError(f"JSON 解析失败 {path}:{exc.lineno}:{exc.colno}") from exc


def _require_exact_keys(value: dict[str, Any], expected: set[str], label: str) -> None:
    actual = set(value)
    if actual != expected:
        missing = sorted(expected - actual)
        unknown = sorted(actual - expected)
        detail: list[str] = []
        if missing:
            detail.append("缺少 " + ", ".join(missing))
        if unknown:
            detail.append("未知 " + ", ".join(unknown))
        raise ToolError(f"{label} 字段不符合契约（{'；'.join(detail)}）")


def _validate_identifier(value: Any, label: str) -> str:
    if not isinstance(value, str) or not value:
        raise ToolError(f"{label} 必须是非空字符串")
    encoded = value.encode("utf-8")
    if len(encoded) > 128 or any(byte < 0x21 or byte > 0x7E for byte in encoded):
        raise ToolError(f"{label} 必须是 1..128 字节的 ASCII 可显示非空白字符")
    return value


def _validate_psk(value: Any, label: str) -> str:
    if not isinstance(value, str):
        raise ToolError(f"{label} 必须是 Base64 字符串")
    try:
        decoded = base64.b64decode(value, validate=True)
    except (ValueError, binascii.Error) as exc:
        raise ToolError(f"{label} 不是合法的标准 Base64") from exc
    if len(decoded) != KEY_BYTES:
        raise ToolError(f"{label} 解码后必须恰好为 {KEY_BYTES} 字节")
    if base64.b64encode(decoded).decode("ascii") != value:
        raise ToolError(f"{label} 必须使用带标准 padding 的规范 Base64")
    return value


def _validate_users(
    value: Any, label: str, *, include_kind: bool
) -> list[dict[str, str]]:
    if not isinstance(value, list) or not value:
        raise ToolError(f"{label} 必须是非空数组")
    if len(value) > MAX_USERS:
        raise ToolError(f"{label} 超过 {MAX_USERS} 个用户上限")

    users: list[dict[str, str]] = []
    seen_names: set[str] = set()
    seen_passwords: set[str] = set()
    for index, entry in enumerate(value):
        entry_label = f"{label}[{index}]"
        if not isinstance(entry, dict):
            raise ToolError(f"{entry_label} 必须是对象")
        expected_keys = {"kind", "name", "password"} if include_kind else {"name", "password"}
        _require_exact_keys(entry, expected_keys, entry_label)
        kind = entry.get("kind")
        if include_kind and kind not in ("formal", "test"):
            raise ToolError(f"{entry_label}.kind 只能是 formal 或 test")
        name = _validate_identifier(entry["name"], f"{entry_label}.name")
        password = _validate_psk(entry["password"], f"{entry_label}.password")
        if name in seen_names:
            raise ToolError(f"{label} 包含重复用户名：{name}")
        if password in seen_passwords:
            raise ToolError(f"{label} 包含重复 uPSK（用户名：{name}）")
        seen_names.add(name)
        seen_passwords.add(password)
        user = {"name": name, "password": password}
        if include_kind:
            user = {"kind": kind, **user}
        users.append(user)
    return users


def _source_sort_key(user: dict[str, str]) -> tuple[int, str]:
    return (0 if user["kind"] == "formal" else 1, user["name"])


def _config_projection(users: list[dict[str, str]]) -> list[dict[str, str]]:
    return [{"name": user["name"], "password": user["password"]} for user in users]


def _validate_source(value: Any) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise ToolError("凭据源顶层必须是对象")
    _require_exact_keys(
        value,
        {"schema_version", "method", "shared_i_psk", "users"},
        "凭据源",
    )
    if type(value["schema_version"]) is not int or value["schema_version"] != SCHEMA_VERSION:
        raise ToolError(f"凭据源 schema_version 必须为 {SCHEMA_VERSION}")
    if value["method"] != METHOD:
        raise ToolError(f"凭据源 method 必须为 {METHOD}")
    shared_i_psk = _validate_psk(value["shared_i_psk"], "凭据源 shared_i_psk")
    users = _validate_users(value["users"], "凭据源 users", include_kind=True)
    if any(user["password"] == shared_i_psk for user in users):
        raise ToolError("共享 iPSK 不得与任何 uPSK 相同")
    return {
        "schema_version": SCHEMA_VERSION,
        "method": METHOD,
        "shared_i_psk": shared_i_psk,
        "users": users,
    }


def _canonical_bytes(value: Any) -> bytes:
    return (json.dumps(value, ensure_ascii=True, indent=2) + "\n").encode("utf-8")


def _path_is_ignored(path: Path) -> bool:
    result = subprocess.run(
        [
            "git",
            "-C",
            str(PROJECT_ROOT),
            "check-ignore",
            "--quiet",
            "--no-index",
            "--",
            str(path),
        ],
        stdin=subprocess.DEVNULL,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
        check=False,
    )
    if result.returncode not in (0, 1):
        raise ToolError("无法确认输出路径的 Git ignore 状态")
    return result.returncode == 0


def _resolved_output(path: Path) -> tuple[Path, Path, str]:
    if not path.name or path.name in (".", ".."):
        raise ToolError("输出路径必须包含文件名")
    try:
        parent = path.parent.resolve(strict=True)
    except OSError as exc:
        raise ToolError(f"输出父目录不存在或无法解析：{path.parent}") from exc
    if not parent.is_dir():
        raise ToolError(f"输出父路径不是目录：{parent}")
    resolved = parent / path.name
    try:
        resolved.relative_to(PROJECT_ROOT)
    except ValueError:
        pass
    else:
        if not _path_is_ignored(resolved):
            raise ToolError("拒绝把凭据写入仓库内未被 ignore 的路径")
    return resolved, parent, path.name


def _write_secret(path: Path, payload: bytes) -> Path:
    resolved, parent, name = _resolved_output(path)
    directory_flags = os.O_RDONLY
    if hasattr(os, "O_DIRECTORY"):
        directory_flags |= os.O_DIRECTORY
    if hasattr(os, "O_CLOEXEC"):
        directory_flags |= os.O_CLOEXEC
    if hasattr(os, "O_NOFOLLOW"):
        directory_flags |= os.O_NOFOLLOW

    try:
        parent_fd = os.open(parent, directory_flags)
    except OSError as exc:
        raise ToolError(f"无法安全打开输出父目录：{parent}") from exc

    created = False
    fd = -1
    previous_umask = os.umask(0o177)
    try:
        parent_metadata = os.fstat(parent_fd)
        if not stat.S_ISDIR(parent_metadata.st_mode):
            raise ToolError(f"输出父路径不是目录：{parent}")
        if parent_metadata.st_uid not in (0, os.geteuid()):
            raise ToolError("输出父目录必须由当前用户或 root 持有")
        if parent_metadata.st_mode & 0o022:
            raise ToolError("输出父目录不得允许 group/other 写入")

        flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
        if hasattr(os, "O_CLOEXEC"):
            flags |= os.O_CLOEXEC
        if hasattr(os, "O_NOFOLLOW"):
            flags |= os.O_NOFOLLOW
        try:
            fd = os.open(name, flags, 0o600, dir_fd=parent_fd)
        except FileExistsError as exc:
            raise ToolError(f"输出已存在，拒绝覆盖：{resolved}") from exc
        except OSError as exc:
            raise ToolError(f"无法创建输出文件 {resolved}：{exc.strerror}") from exc
        created = True
        os.fchmod(fd, 0o600)
        view = memoryview(payload)
        while view:
            written = os.write(fd, view)
            view = view[written:]
        os.fsync(fd)
        metadata = os.fstat(fd)
        if not stat.S_ISREG(metadata.st_mode) or stat.S_IMODE(metadata.st_mode) != 0o600:
            raise ToolError("输出文件类型或权限复核失败")
    except Exception:
        if fd >= 0:
            os.close(fd)
            fd = -1
        if created:
            try:
                os.unlink(name, dir_fd=parent_fd)
            except OSError:
                pass
        raise
    finally:
        os.umask(previous_umask)
        if fd >= 0:
            os.close(fd)
        os.close(parent_fd)
    return resolved


def _new_psk() -> str:
    return base64.b64encode(secrets.token_bytes(KEY_BYTES)).decode("ascii")


def command_generate(args: argparse.Namespace) -> None:
    if not 1 <= args.formal_count <= MAX_USERS:
        raise ToolError(f"--formal-count/--count 必须在 1..{MAX_USERS} 范围内")
    if not 0 <= args.test_count <= MAX_USERS:
        raise ToolError(f"--test-count 必须在 0..{MAX_USERS} 范围内")
    if args.formal_count + args.test_count > MAX_USERS:
        raise ToolError(f"正式与测试账号总数不得超过 {MAX_USERS}")
    if args.start < 0:
        raise ToolError("--start 不得为负数")
    highest = args.start + args.formal_count - 1
    highest_serial = max(highest, args.test_count)
    width = args.width if args.width is not None else max(6, len(str(highest_serial)))
    if width < 1 or width > 32:
        raise ToolError("--width 必须在 1..32 范围内")
    if len(str(highest_serial)) > width:
        raise ToolError("--width 不足以容纳最大用户序号")

    named_kinds = [
        ("formal", f"{args.formal_prefix}{number:0{width}d}")
        for number in range(args.start, highest + 1)
    ]
    named_kinds.extend(
        ("test", f"{args.test_prefix}{number:0{width}d}")
        for number in range(1, args.test_count + 1)
    )
    for index, (_, name) in enumerate(named_kinds):
        _validate_identifier(name, f"生成用户名[{index}]")
    names = [name for _, name in named_kinds]
    if len(names) != len(set(names)):
        raise ToolError("生成规则产生了重复用户名")

    shared_i_psk = _new_psk()
    user_passwords: set[str] = set()
    users: list[dict[str, str]] = []
    for kind, name in named_kinds:
        while True:
            password = _new_psk()
            if password != shared_i_psk and password not in user_passwords:
                break
        user_passwords.add(password)
        users.append({"kind": kind, "name": name, "password": password})
    users.sort(key=_source_sort_key)

    source = {
        "schema_version": SCHEMA_VERSION,
        "method": METHOD,
        "shared_i_psk": shared_i_psk,
        "users": users,
    }
    output = _write_secret(args.output, _canonical_bytes(source))
    print(
        f"已生成 {args.formal_count} 个正式账号、{args.test_count} 个测试账号的私有凭据源："
        f"{output}（0600，未输出密钥）"
    )


def command_normalize(args: argparse.Namespace) -> None:
    source = _validate_source(
        _load_json(args.input, max_bytes=MAX_SOURCE_BYTES, private=True)
    )
    source["users"] = sorted(source["users"], key=_source_sort_key)
    formal_count = sum(user["kind"] == "formal" for user in source["users"])
    test_count = sum(user["kind"] == "test" for user in source["users"])
    output = _write_secret(args.output, _canonical_bytes(source))
    print(
        f"已规范化 {formal_count} 个正式账号、{test_count} 个测试账号："
        f"{output}（0600，未输出密钥）"
    )


def command_render_users(args: argparse.Namespace) -> None:
    source = _validate_source(
        _load_json(args.source, max_bytes=MAX_SOURCE_BYTES, private=True)
    )
    if source["users"] != sorted(source["users"], key=_source_sort_key):
        raise ToolError("凭据源 users[] 尚未按 kind/name 规范排序，请先执行 normalize")
    formal_count = sum(user["kind"] == "formal" for user in source["users"])
    test_count = sum(user["kind"] == "test" for user in source["users"])
    output = _write_secret(args.output, _canonical_bytes(_config_projection(source["users"])))
    print(
        f"已渲染 {formal_count} 个正式账号、{test_count} 个测试账号的 ssserver users[]："
        f"{output}（0600，已剥离 kind，未输出密钥）"
    )


def _single_server_config(path: Path) -> tuple[str, str, dict[str, Any]]:
    value = _load_json(path, max_bytes=MAX_CONFIG_BYTES, private=True)
    if not isinstance(value, dict):
        raise ToolError(f"配置顶层必须是对象：{path}")
    user_stats = value.get("user_stats")
    if not isinstance(user_stats, dict):
        raise ToolError(f"配置缺少 user_stats 对象：{path}")
    node_id = _validate_identifier(user_stats.get("node_id"), f"{path} user_stats.node_id")
    servers = value.get("servers")
    if not isinstance(servers, list) or len(servers) != 1 or not isinstance(servers[0], dict):
        raise ToolError(f"五节点部署配置必须恰好包含一个 servers[] 服务：{path}")
    server = servers[0]
    server_id = _validate_identifier(server.get("id"), f"{path} servers[0].id")
    return node_id, server_id, server


def command_verify_five(args: argparse.Namespace) -> None:
    if len(args.config) != 5:
        raise ToolError("verify-five 必须且只能提供 5 个 --config")
    if not 1 <= args.expected_formal_users <= MAX_USERS:
        raise ToolError(f"--expected-formal-users 必须在 1..{MAX_USERS} 范围内")
    if not 0 <= args.expected_test_users <= MAX_USERS:
        raise ToolError(f"--expected-test-users 必须在 0..{MAX_USERS} 范围内")
    if args.expected_formal_users + args.expected_test_users > MAX_USERS:
        raise ToolError(f"期望的正式与测试账号总数不得超过 {MAX_USERS}")

    source = _validate_source(
        _load_json(args.source, max_bytes=MAX_SOURCE_BYTES, private=True)
    )
    source_users = source["users"]
    formal_count = sum(user["kind"] == "formal" for user in source_users)
    test_count = sum(user["kind"] == "test" for user in source_users)
    if formal_count != args.expected_formal_users or test_count != args.expected_test_users:
        raise ToolError(
            f"凭据源账号数量为 formal={formal_count}, test={test_count}，期望 "
            f"formal={args.expected_formal_users}, test={args.expected_test_users}"
        )
    if source_users != sorted(source_users, key=_source_sort_key):
        raise ToolError("凭据源 users[] 尚未按 kind/name 规范排序，请先执行 normalize")
    expected_users = _config_projection(source_users)

    resolved_configs = [path.resolve(strict=True) for path in args.config]
    if len(resolved_configs) != len(set(resolved_configs)):
        raise ToolError("5 个 --config 必须是互不相同的文件")

    node_ids: set[str] = set()
    server_ids: set[str] = set()
    for path in resolved_configs:
        node_id, server_id, server = _single_server_config(path)
        if node_id in node_ids:
            raise ToolError(f"5 个配置的 node_id 不唯一：{node_id}")
        if server_id in server_ids:
            raise ToolError(f"5 个配置的 server id 不唯一：{server_id}")
        node_ids.add(node_id)
        server_ids.add(server_id)

        if server.get("method") != METHOD:
            raise ToolError(f"配置 method 必须为 {METHOD}：{path}")
        config_i_psk = _validate_psk(server.get("password"), f"{path} servers[0].password")
        if not secrets.compare_digest(config_i_psk, source["shared_i_psk"]):
            raise ToolError(f"配置共享 iPSK 与受控凭据源不一致：{path}")
        config_users = _validate_users(
            server.get("users"), f"{path} servers[0].users", include_kind=False
        )
        if [user["name"] for user in config_users] != [
            user["name"] for user in expected_users
        ]:
            raise ToolError(f"配置 users[] 的用户名或顺序与受控凭据源不一致：{path}")
        for index, (actual, expected) in enumerate(zip(config_users, expected_users)):
            if not secrets.compare_digest(actual["password"], expected["password"]):
                raise ToolError(
                    f"配置 users[{index}] 的 uPSK 与受控凭据源不一致：{path}"
                )

    print(
        f"验证通过：5 个配置、{formal_count} 个正式账号、{test_count} 个测试账号、{METHOD}；"
        "共享 iPSK、users[] 内容与顺序一致，node_id/server id 唯一。"
    )


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        description="安全生成、规范化并校验五节点 AEAD-2022 AES-128-GCM 凭据"
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate = subparsers.add_parser("generate", help="生成新的私有 iPSK/uPSK 凭据源")
    generate.add_argument("--output", required=True, type=Path, help="不存在的显式输出文件")
    generate.add_argument(
        "--formal-count",
        "--count",
        dest="formal_count",
        type=int,
        default=200,
        help="正式账号数，默认 200；--count 是兼容别名",
    )
    generate.add_argument(
        "--formal-prefix",
        "--prefix",
        dest="formal_prefix",
        default="u_",
        help="正式账号序号前缀，默认 u_；--prefix 是兼容别名",
    )
    generate.add_argument("--test-count", type=int, default=4, help="测试账号数，默认 4")
    generate.add_argument("--test-prefix", default="test_", help="测试账号序号前缀")
    generate.add_argument("--start", type=int, default=1, help="起始序号，默认 1")
    generate.add_argument("--width", type=int, help="序号补零宽度，默认至少 6")
    generate.set_defaults(handler=command_generate)

    normalize = subparsers.add_parser("normalize", help="校验并按用户名规范化私有凭据源")
    normalize.add_argument("--input", required=True, type=Path, help="权限不宽于 0600 的凭据源")
    normalize.add_argument("--output", required=True, type=Path, help="不存在的显式输出文件")
    normalize.set_defaults(handler=command_normalize)

    render_users = subparsers.add_parser(
        "render-users", help="从规范私有源渲染仅含 name/password 的 ssserver users[]"
    )
    render_users.add_argument("--source", required=True, type=Path, help="私有规范凭据源")
    render_users.add_argument("--output", required=True, type=Path, help="不存在的显式输出文件")
    render_users.set_defaults(handler=command_render_users)

    verify_five = subparsers.add_parser(
        "verify-five", help="校验受控凭据源与五份单服务配置完全一致"
    )
    verify_five.add_argument("--source", required=True, type=Path, help="私有规范凭据源")
    verify_five.add_argument(
        "--config", required=True, action="append", type=Path, help="节点配置，必须提供 5 次"
    )
    verify_five.add_argument(
        "--expected-formal-users", type=int, default=200, help="期望正式账号数，默认 200"
    )
    verify_five.add_argument(
        "--expected-test-users", type=int, default=4, help="期望测试账号数，默认 4"
    )
    verify_five.set_defaults(handler=command_verify_five)
    return parser


def main() -> int:
    parser = build_parser()
    args = parser.parse_args()
    try:
        args.handler(args)
    except (ToolError, OSError) as exc:
        print(f"错误：{exc}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
