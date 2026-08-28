#!/usr/bin/env python3
"""Static guard for panic-prone audit paths in a prepared Rust tree."""

from __future__ import annotations

import argparse
import re
from pathlib import Path


FORBIDDEN = re.compile(r"\b(?:unwrap|expect)\s*\(|\bpanic!\s*\(")
INDEX = re.compile(
    r"\b(?P<name>[A-Za-z_][A-Za-z0-9_]*)"
    r"\[(?P<start>[0-9]+)(?:\.\.(?P<inclusive>=)?(?P<end>[0-9]+))?\]"
)
AUDIT_DIRS = ("crates/shadowsocks-auditd/src",)
FIXED_ARRAY_DECLARATION = (
    r"\b{name}\s*:\s*&?\s*(?:mut\s+)?\[[^\]\n;]+;\s*(?P<typed_len>[0-9]+)\]"
    r"|\blet\s+(?:mut\s+)?{name}\s*=\s*\[[^\]\n;]*;\s*(?P<literal_len>[0-9]+)\]"
)


def _structural_characters(line: str, block_comment_depth: int) -> tuple[str, int]:
    """Return Rust structure outside strings/comments and updated comment depth."""
    result: list[str] = []
    index = 0
    quote: str | None = None
    while index < len(line):
        current = line[index]
        following = line[index + 1] if index + 1 < len(line) else ""
        if block_comment_depth:
            if current == "/" and following == "*":
                block_comment_depth += 1
                index += 2
            elif current == "*" and following == "/":
                block_comment_depth -= 1
                index += 2
            else:
                index += 1
            continue
        if quote is not None:
            if current == "\\":
                index += 2
            elif current == quote:
                quote = None
                index += 1
            else:
                index += 1
            continue
        if current == "/" and following == "/":
            break
        if current == "/" and following == "*":
            block_comment_depth = 1
            index += 2
            continue
        char_literal = current == "'" and (
            (index + 2 < len(line) and line[index + 2] == "'")
            or (following == "\\" and "'" in line[index + 2:index + 7])
        )
        if current == '"' or char_literal:
            quote = current
            index += 1
            continue
        result.append(current)
        index += 1
    return "".join(result), block_comment_depth


def _test_only_lines(lines: list[str]) -> set[int]:
    """Find complete Rust items guarded by a standalone #[cfg(test)] attribute."""
    skipped: set[int] = set()
    cursor = 0
    while cursor < len(lines):
        if lines[cursor].strip() != "#[cfg(test)]":
            cursor += 1
            continue
        start = cursor
        cursor += 1
        depth = 0
        saw_brace = False
        comment_depth = 0
        while cursor < len(lines):
            structural, comment_depth = _structural_characters(lines[cursor], comment_depth)
            depth += structural.count("{") - structural.count("}")
            saw_brace = saw_brace or "{" in structural
            cursor += 1
            if saw_brace and depth == 0:
                break
            if not saw_brace and ";" in structural:
                break
        skipped.update(range(start, cursor))
    return skipped


def _fixed_array_length(lines: list[str], line_index: int, name: str) -> int | None:
    # Rustfmt keeps parameters and local declarations close to their use. Restricting
    # the search avoids treating an identically named array in an earlier item as proof.
    context = "\n".join(lines[:line_index + 1])
    pattern = re.compile(FIXED_ARRAY_DECLARATION.format(name=re.escape(name)))
    matches = list(pattern.finditer(context))
    if not matches:
        return None
    match = matches[-1]
    length = match.group("typed_len") or match.group("literal_len")
    return int(length)


def _fixed_array_index_is_safe(lines: list[str], line_index: int, match: re.Match[str]) -> bool:
    length = _fixed_array_length(lines, line_index, match.group("name"))
    if length is None:
        return False
    start = int(match.group("start"))
    end = match.group("end")
    if end is None:
        return start < length
    end_value = int(end)
    if match.group("inclusive"):
        return start <= end_value < length
    return start <= end_value <= length


def _check_file(path: Path) -> list[str]:
    findings: list[str] = []
    lines = path.read_text(encoding="utf-8").splitlines()
    test_only = _test_only_lines(lines)
    for line_index, line in enumerate(lines):
        if line_index in test_only:
            continue
        number = line_index + 1
        if FORBIDDEN.search(line):
            findings.append(f"{path}:{number}: forbidden panic path")
        context = " ".join(lines[max(0, line_index - 3):line_index + 2])
        guarded = re.search(r"(?:len\(|if |match |checked_|is_some|is_none|validate)", context)
        for match in INDEX.finditer(line):
            if not guarded and not _fixed_array_index_is_safe(lines, line_index, match):
                findings.append(f"{path}:{number}: index lacks nearby bounds guard")
    return findings


def check(root: Path) -> list[str]:
    findings: list[str] = []
    for relative in AUDIT_DIRS:
        directory = root / relative
        if not directory.is_dir():
            continue
        for path in sorted(directory.rglob("*.rs")):
            findings.extend(_check_file(path))
    service_files = list((root / "crates/shadowsocks-service/src").rglob("user_audit.rs"))
    for service_file in service_files:
        if service_file.is_file():
            findings.extend(_check_file(service_file))
    # `user-audit` is a Linux-only feature even when a consumer depends on
    # shadowsocks-service directly (the root crate's auditd dependency is not
    # present in that use case). Keep the compile gate in the prepared source
    # so it cannot silently regress while the rest of this checker remains
    # focused on panic-prone production paths.
    service_lib = root / "crates/shadowsocks-service/src/lib.rs"
    if service_lib.is_file():
        source = service_lib.read_text(encoding="utf-8")
        gate = '#[cfg(all(feature = "user-audit", not(target_os = "linux")))]'
        if gate not in source or "feature `user-audit` is supported on Linux only" not in source:
            findings.append(f"{service_lib}: missing Linux-only user-audit compile gate")
    return findings


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--source", type=Path, required=True)
    args = parser.parse_args()
    findings = check(args.source.resolve())
    if findings:
        for finding in findings:
            print(finding)
        return 1
    print("静态审计通过：user-audit/auditd 生产 Rust 路径无 unwrap/expect/panic。")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
