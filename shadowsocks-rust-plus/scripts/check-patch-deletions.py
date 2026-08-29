#!/usr/bin/env python3
"""Reject malformed or unresolvable file-deletion stanzas in Git patches."""

from __future__ import annotations

import argparse
import os
import shlex
from dataclasses import dataclass
from pathlib import Path, PurePosixPath


class PatchDeletionError(ValueError):
    pass


@dataclass
class Stanza:
    line: int
    old_path: str
    deleted: bool = False
    has_payload: bool = False


def _diff_old_path(line: str, number: int) -> str:
    try:
        fields = shlex.split(line, posix=True)
    except ValueError as error:
        raise PatchDeletionError(f"line {number}: malformed diff header: {error}") from error
    if len(fields) != 4 or fields[:2] != ["diff", "--git"]:
        raise PatchDeletionError(f"line {number}: malformed diff --git header")
    old_path = fields[2]
    if not old_path.startswith("a/"):
        raise PatchDeletionError(f"line {number}: deletion source path must start with a/")
    relative = old_path[2:]
    pure = PurePosixPath(relative)
    if not relative or pure.is_absolute() or any(part in ("", ".", "..") for part in pure.parts):
        raise PatchDeletionError(f"line {number}: unsafe deletion source path {relative!r}")
    return relative


def validate_patch(path: Path, source_root: Path | None = None) -> list[str]:
    try:
        lines = path.read_text(encoding="utf-8").splitlines()
    except (OSError, UnicodeDecodeError) as error:
        raise PatchDeletionError(f"cannot read patch: {error}") from error

    deletions: list[str] = []
    current: Stanza | None = None

    def finish() -> None:
        if current is None or not current.deleted:
            return
        if not current.has_payload:
            raise PatchDeletionError(
                f"line {current.line}: deletion of {current.old_path!r} has no hunk or binary payload"
            )
        if source_root is not None and not os.path.lexists(source_root / current.old_path):
            raise PatchDeletionError(
                f"line {current.line}: deletion target does not exist in current source tree: {current.old_path}"
            )
        deletions.append(current.old_path)

    for number, line in enumerate(lines, 1):
        if line.startswith("diff --git "):
            finish()
            current = Stanza(number, _diff_old_path(line, number))
            continue
        if current is None:
            continue
        if line.startswith("deleted file mode "):
            current.deleted = True
        elif current.deleted and (line.startswith("@@ ") or line == "GIT binary patch"):
            current.has_payload = True
    finish()
    return deletions


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("patch", type=Path)
    parser.add_argument("--source-root", type=Path)
    args = parser.parse_args()
    source_root = args.source_root.resolve(strict=True) if args.source_root is not None else None
    try:
        validate_patch(args.patch, source_root)
    except (PatchDeletionError, OSError) as error:
        parser.exit(1, f"{args.patch}: {error}\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
