#!/usr/bin/env python3
"""发布产物工具：规范源码树哈希、复制文件漂移门禁、manifest 生成与验签输入。

刻意保持小：本项目是零补丁 wrapper，Go 构建本身可复现，不需要参考实现那套
两千行的 provenance 机器。这里只做四件必须精确的事。
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import stat
import sys
from pathlib import Path

MAX_FILE_BYTES = 64 * 1024 * 1024


class ArtifactError(Exception):
    pass


def _tree_sha256(root: Path) -> str:
    """对准备好的源码树取哈希，不跟随链接、不混入宿主路径。

    条目按相对路径排序，每一项把「类型、相对路径、可执行位、长度、内容」都写进
    长度前缀的摘要流——不写长度前缀的话，相邻字段可以互相借位，两棵不同的树能撞出同一个值。
    """
    root = root.resolve(strict=True)
    if not root.is_dir():
        raise ArtifactError(f"源码根不是目录：{root}")
    digest = hashlib.sha256()
    entries = sorted(root.rglob("*"), key=lambda path: path.relative_to(root).as_posix())
    for path in entries:
        relative = path.relative_to(root).as_posix().encode("utf-8")
        metadata = path.lstat()
        if stat.S_ISDIR(metadata.st_mode):
            kind, payload = b"directory", b""
        elif stat.S_ISREG(metadata.st_mode):
            if metadata.st_size > MAX_FILE_BYTES:
                raise ArtifactError(f"源码树含超大文件：{path}")
            kind, payload = b"file", path.read_bytes()
        elif stat.S_ISLNK(metadata.st_mode):
            kind, payload = b"symlink", os.readlink(path).encode("utf-8")
        else:
            raise ArtifactError(f"源码树包含不支持的文件类型：{path}")
        executable = b"1" if metadata.st_mode & 0o111 else b"0"
        for item in (kind, relative, executable, str(len(payload)).encode("ascii"), payload):
            digest.update(str(len(item)).encode("ascii"))
            digest.update(b":")
            digest.update(item)
            digest.update(b"\0")
    return digest.hexdigest()


def _sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _read_copied_lock(path: Path) -> list[tuple[str, str, str]]:
    rows: list[tuple[str, str, str]] = []
    for line in path.read_text(encoding="utf-8").splitlines():
        if not line.strip() or line.startswith("#"):
            continue
        parts = line.split(" ")
        if len(parts) != 3:
            raise ArtifactError(f"copied-files.lock 行格式错误：{line}")
        rows.append((parts[0], parts[1], parts[2]))
    if not rows:
        raise ArtifactError("copied-files.lock 没有任何条目")
    return rows


def command_source_tree_sha256(args: argparse.Namespace) -> int:
    print(_tree_sha256(args.source_root))
    return 0


def command_copied_files_check(args: argparse.Namespace) -> int:
    """复制文件漂移门禁（README §4.7、§9.2 第 8 条）。

    两个方向都要查：
      * 上游侧——新准备的源码树里同名文件的哈希必须与 lock 记录一致，否则说明上游在
        钉定 commit 之外发生了变化（或 lock 记错了版本）；
      * overlay 侧——本仓库的副本必须与 lock 记录一致，否则说明有人改了复制文件却没登记。
    只查其中一侧都会漏掉另一类漂移。
    """
    rows = _read_copied_lock(args.lock)
    upstream_dir = args.source_root / "cmd" / "sing-box"
    failures: list[str] = []
    for name, upstream_hash, overlay_hash in rows:
        upstream_path = upstream_dir / name
        overlay_path = args.overlay_root / name
        if not upstream_path.is_file():
            failures.append(f"上游缺少 {name}：钉定版本的 cmd/sing-box 已不含该文件")
            continue
        actual_upstream = _sha256_file(upstream_path)
        if actual_upstream != upstream_hash:
            failures.append(
                f"{name} 的上游内容已漂移：lock={upstream_hash[:12]} 实际={actual_upstream[:12]}"
            )
        if not overlay_path.is_file():
            failures.append(f"overlay 缺少复制文件：{name}")
            continue
        actual_overlay = _sha256_file(overlay_path)
        if actual_overlay != overlay_hash:
            failures.append(
                f"{name} 的 overlay 副本被改动但未登记：lock={overlay_hash[:12]} 实际={actual_overlay[:12]}"
            )
    extra = sorted(
        path.name
        for path in args.overlay_root.glob("cmd*.go")
        if path.name not in {row[0] for row in rows}
    )
    if extra:
        failures.append("overlay 里出现未登记的 cmd*.go：" + ", ".join(extra))
    if failures:
        for failure in failures:
            print(f"错误：{failure}", file=sys.stderr)
        return 1
    print(f"复制文件门禁通过：{len(rows)} 个文件与钉定版本一致")
    return 0


def command_manifest(args: argparse.Namespace) -> int:
    """为发布产物生成 manifest。内容全部可从产物本身与 upstream.lock 复算。"""
    artifacts = []
    for path in sorted(args.artifact):
        resolved = Path(path)
        if not resolved.is_file():
            raise ArtifactError(f"产物不存在：{resolved}")
        artifacts.append(
            {
                "name": resolved.name,
                "size": resolved.stat().st_size,
                "sha256": _sha256_file(resolved),
            }
        )
    manifest = {
        "schema_version": 1,
        "project": "sing-box-plus",
        "version": args.version,
        "upstream_tag": args.upstream_tag,
        "upstream_commit": args.upstream_commit,
        "prepared_tree_sha256": args.prepared_tree_sha256,
        "build_tags": args.build_tags,
        "go_version": args.go_version,
        "targets": args.target,
        "artifacts": artifacts,
    }
    output = json.dumps(manifest, ensure_ascii=False, indent=2, sort_keys=True) + "\n"
    args.output.write_text(output, encoding="utf-8")
    print(f"manifest 已写入：{args.output}")
    return 0


def command_verify_manifest(args: argparse.Namespace) -> int:
    manifest = json.loads(args.manifest.read_text(encoding="utf-8"))
    if manifest.get("schema_version") != 1:
        raise ArtifactError("manifest schema_version 不是 1")
    directory = args.manifest.parent
    for artifact in manifest["artifacts"]:
        path = directory / artifact["name"]
        if not path.is_file():
            raise ArtifactError(f"manifest 引用的产物不存在：{path}")
        actual = _sha256_file(path)
        if actual != artifact["sha256"]:
            raise ArtifactError(
                f"{artifact['name']} 哈希不符：manifest={artifact['sha256'][:12]} 实际={actual[:12]}"
            )
        if path.stat().st_size != artifact["size"]:
            raise ArtifactError(f"{artifact['name']} 大小不符")
    print(f"manifest 校验通过：{len(manifest['artifacts'])} 个产物")
    return 0


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(description="sing-box-plus 发布产物工具")
    subparsers = parser.add_subparsers(dest="command", required=True)

    tree = subparsers.add_parser("source-tree-sha256", help="计算规范准备源码树 SHA-256")
    tree.add_argument("--source-root", required=True, type=Path)
    tree.set_defaults(handler=command_source_tree_sha256)

    copied = subparsers.add_parser("copied-files-check", help="复制文件漂移门禁")
    copied.add_argument("--source-root", required=True, type=Path)
    copied.add_argument("--overlay-root", required=True, type=Path)
    copied.add_argument("--lock", required=True, type=Path)
    copied.set_defaults(handler=command_copied_files_check)

    manifest = subparsers.add_parser("manifest", help="生成发布 manifest")
    manifest.add_argument("--version", required=True)
    manifest.add_argument("--upstream-tag", required=True)
    manifest.add_argument("--upstream-commit", required=True)
    manifest.add_argument("--prepared-tree-sha256", required=True)
    manifest.add_argument("--build-tags", required=True)
    manifest.add_argument("--go-version", required=True)
    manifest.add_argument("--target", action="append", required=True)
    manifest.add_argument("--artifact", action="append", required=True)
    manifest.add_argument("--output", required=True, type=Path)
    manifest.set_defaults(handler=command_manifest)

    verify = subparsers.add_parser("verify-manifest", help="校验发布 manifest 与产物一致")
    verify.add_argument("--manifest", required=True, type=Path)
    verify.set_defaults(handler=command_verify_manifest)

    args = parser.parse_args(argv)
    try:
        return args.handler(args)
    except ArtifactError as error:
        print(f"错误：{error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
