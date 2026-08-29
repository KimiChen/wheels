#!/usr/bin/env python3
"""Reject malformed or unresolvable file-deletion stanzas in Git patches."""

from __future__ import annotations

import argparse
import os
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


DIFF_HEADER_PREFIX = "diff --git "
C_QUOTE_ESCAPES = {
    "a": 0x07,
    "b": 0x08,
    "f": 0x0C,
    "n": 0x0A,
    "r": 0x0D,
    "t": 0x09,
    "v": 0x0B,
    "\\": 0x5C,
    '"': 0x22,
}


def _decode_c_quoted(token: str, number: int) -> str:
    """Decode Git's C-style quoting (octal byte escapes) back into a path."""
    raw = bytearray()
    index = 0
    while index < len(token):
        char = token[index]
        if char != "\\":
            raw.extend(char.encode("utf-8"))
            index += 1
            continue
        index += 1
        if index >= len(token):
            raise PatchDeletionError(f"line {number}: truncated escape in diff --git header")
        char = token[index]
        if char in C_QUOTE_ESCAPES:
            raw.append(C_QUOTE_ESCAPES[char])
            index += 1
            continue
        digits = token[index : index + 3]
        if len(digits) != 3 or any(digit not in "01234567" for digit in digits):
            raise PatchDeletionError(f"line {number}: bad escape in diff --git header")
        raw.append(int(digits, 8))
        index += 3
    try:
        return raw.decode("utf-8")
    except UnicodeDecodeError as error:
        raise PatchDeletionError(f"line {number}: diff --git path is not UTF-8") from error


def _scan_quoted(text: str, number: int) -> tuple[str, str]:
    """Split a leading C-quoted token from ``text`` and decode it."""
    index = 1
    while index < len(text):
        char = text[index]
        if char == "\\":
            index += 2
            continue
        if char == '"':
            return _decode_c_quoted(text[1:index], number), text[index + 1 :]
        index += 1
    raise PatchDeletionError(f"line {number}: unterminated quoted path in diff --git header")


def _split_diff_header(rest: str, number: int) -> tuple[str, str]:
    if rest.startswith('"'):
        # Git quotes both names as a pair whenever either one needs quoting.
        old_path, remainder = _scan_quoted(rest, number)
        if not remainder.startswith(' "'):
            raise PatchDeletionError(f"line {number}: malformed diff --git header")
        new_path, trailing = _scan_quoted(remainder[1:], number)
        if trailing:
            raise PatchDeletionError(f"line {number}: malformed diff --git header")
        return old_path, new_path
    # An unquoted header is ambiguous when a path contains spaces; recover the
    # split by requiring a single `a/... b/...` cut, preferring the equal-path
    # one that every deletion stanza produces.
    candidates = [
        (rest[:index], rest[index + 1 :])
        for index, char in enumerate(rest)
        if char == " "
        and rest[:index].startswith("a/")
        and rest[index + 1 :].startswith("b/")
    ]
    equal = [pair for pair in candidates if pair[0][2:] == pair[1][2:]]
    if len(equal) == 1:
        return equal[0]
    if len(candidates) == 1:
        return candidates[0]
    raise PatchDeletionError(f"line {number}: malformed diff --git header")


def _diff_old_path(line: str, number: int) -> str:
    if not line.startswith(DIFF_HEADER_PREFIX):
        raise PatchDeletionError(f"line {number}: malformed diff --git header")
    old_path, new_path = _split_diff_header(line[len(DIFF_HEADER_PREFIX) :], number)
    if not old_path.startswith("a/") or not new_path.startswith("b/"):
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
