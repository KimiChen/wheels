#!/usr/bin/env python3
"""Static guard for panic-prone audit paths in a prepared Rust tree."""

from __future__ import annotations

import argparse
import re
from pathlib import Path


FORBIDDEN = re.compile(
    r"\b(?:unwrap|expect)\s*\(|\b(?:panic|unreachable|todo|unimplemented)!\s*\("
)
INDEX = re.compile(
    r"\b(?P<name>[A-Za-z_][A-Za-z0-9_]*)\[(?P<expression>[^\]\n]+)\]"
)
AUDIT_DIRS = (
    "crates/shadowsocks-auditd/src",
    "crates/shadowsocks-audit-protocol/src",
)
SERVICE_AUDIT_FILES = ("crates/shadowsocks-service/src/server/user_audit.rs",)
SERVICE_WIRING_FILES = (
    "crates/shadowsocks-service/src/config.rs",
    "crates/shadowsocks-service/src/lib.rs",
    "crates/shadowsocks-service/src/server/mod.rs",
)
FIXED_ARRAY_DECLARATION = (
    r"\b{name}\s*:\s*&?\s*(?:mut\s+)?\[[^\]\n;]+;\s*(?P<typed_len>[A-Z_][A-Z0-9_]*(?:\.len\(\))?|[0-9]+)\]"
    r"|\blet\s+(?:mut\s+)?{name}\s*=\s*\[[^\]\n;]*;\s*(?P<literal_len>[A-Z_][A-Z0-9_]*|[0-9]+)\]"
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
    if length.isdecimal():
        return int(length)
    if length.endswith(".len()"):
        array_name = length.removesuffix(".len()")
        declaration = re.search(
            rf"\bconst\s+{re.escape(array_name)}\s*:\s*\[[^\]\n;]+;\s*([0-9]+)\]",
            context,
        )
    else:
        declaration = re.search(
            rf"\bconst\s+{re.escape(length)}\s*:\s*usize\s*=\s*([0-9]+)",
            context,
        )
    return int(declaration.group(1)) if declaration is not None else None


def _fixed_array_index_is_safe(lines: list[str], line_index: int, match: re.Match[str]) -> bool:
    length = _fixed_array_length(lines, line_index, match.group("name"))
    if length is None:
        return False
    expression = match.group("expression").strip()
    if re.fullmatch(r"[0-9]+", expression):
        return int(expression) < length
    range_match = re.fullmatch(r"(?P<start>[0-9]*)\.\.(?P<inclusive>=)?(?P<end>[0-9]*)", expression)
    if range_match is None:
        return False
    start = int(range_match.group("start") or 0)
    end_text = range_match.group("end")
    if not end_text:
        return not range_match.group("inclusive") and start <= length
    end = int(end_text)
    if range_match.group("inclusive"):
        return start <= end < length
    return start <= end <= length


def _index_is_proven_safe(lines: list[str], line_index: int, match: re.Match[str]) -> bool:
    length = _fixed_array_length(lines, line_index, match.group("name"))
    expression = match.group("expression").strip()
    literal_expression = re.fullmatch(r"[0-9]+|[0-9]*\.\.?=?[0-9]*", expression) is not None
    if length is not None and literal_expression:
        # A known array bound is authoritative. Contextual guard words must
        # never suppress a compile-time out-of-bounds index.
        return _fixed_array_index_is_safe(lines, line_index, match)

    name = re.escape(match.group("name"))
    context = "\n".join(lines[max(0, line_index - 256):line_index + 1])
    if re.search(
        rf"\b{name}\s*(?:\n\s*)?\.(?:len|starts_with|strip_prefix|find|windows|iter)\s*\(",
        context,
    ):
        return True
    if re.search(rf"\b(?:validate|check)[A-Za-z0-9_]*\s*\([^;\n]*\b{name}\b", context):
        return True
    if re.search(rf"\.read\s*\(\s*&mut\s+{name}\b", context):
        return True
    if re.search(rf"\b{name}\.len\(\)", expression):
        return True

    # `slice.windows(N)` yields subslices of exactly N elements.
    if expression.isdecimal():
        window = re.search(rf"\.windows\(([0-9]+)\).*\|\s*{name}\s*\|", context, re.DOTALL)
        if window is not None and int(expression) < int(window.group(1)):
            return True

    if length is not None:
        modulo = re.search(
            rf"\blet\s+{re.escape(expression)}\s*=.*%\s*(?:[A-Z_][A-Z0-9_]*|[A-Za-z_][A-Za-z0-9_]*\.len\(\))",
            context,
        )
        if modulo is not None:
            return True
        if expression.endswith(".index()"):
            index_functions = list(re.finditer(r"\bfn\s+index\s*\([^)]*\)\s*->\s*usize\s*\{", "\n".join(lines[:line_index + 1])))
            if index_functions:
                function_tail = "\n".join(lines[:line_index + 1])[index_functions[-1].end():]
                function_body = function_tail.split("\n    }", 1)[0]
                arms = [int(value) for value in re.findall(r"=>\s*([0-9]+)\b", function_body)]
                if arms and max(arms) < length:
                    return True
    return False


def _check_file(path: Path) -> list[str]:
    findings: list[str] = []
    lines = path.read_text(encoding="utf-8").splitlines()
    test_only = _test_only_lines(lines)
    structural_lines: list[str] = []
    comment_depth = 0
    for line in lines:
        structural, comment_depth = _structural_characters(line, comment_depth)
        structural_lines.append(structural)
    for line_index, line in enumerate(lines):
        if line_index in test_only:
            continue
        number = line_index + 1
        structural = structural_lines[line_index]
        if FORBIDDEN.search(structural):
            findings.append(f"{path}:{number}: forbidden panic path")
        for match in INDEX.finditer(structural):
            if not _index_is_proven_safe(structural_lines, line_index, match):
                findings.append(f"{path}:{number}: direct index is not statically bounded")
    return findings


def _check_wiring_file(path: Path) -> list[str]:
    """Check the feature wiring statements without auditing unrelated upstream code."""

    findings: list[str] = []
    lines = path.read_text(encoding="utf-8").splitlines()
    test_only = _test_only_lines(lines)
    comment_depth = 0
    for line_index, line in enumerate(lines):
        structural, comment_depth = _structural_characters(line, comment_depth)
        if line_index in test_only:
            continue
        number = line_index + 1
        if "user_audit" not in structural and "user-audit" not in structural:
            continue
        if FORBIDDEN.search(structural):
            findings.append(f"{path}:{number}: forbidden panic path in user-audit wiring")
        if INDEX.search(structural):
            findings.append(f"{path}:{number}: direct index in user-audit wiring")
    return findings


def check(root: Path) -> list[str]:
    findings: list[str] = []
    if not root.is_dir():
        return [f"{root}: source root does not exist or is not a directory"]
    scanned: set[Path] = set()
    for relative in AUDIT_DIRS:
        directory = root / relative
        if not directory.is_dir():
            continue
        for path in sorted(directory.rglob("*.rs")):
            findings.extend(_check_file(path))
            scanned.add(path)
    for relative in SERVICE_AUDIT_FILES:
        service_file = root / relative
        if service_file.is_file():
            findings.extend(_check_file(service_file))
            scanned.add(service_file)
    for relative in SERVICE_WIRING_FILES:
        service_file = root / relative
        if service_file.is_file():
            findings.extend(_check_wiring_file(service_file))
            scanned.add(service_file)
    if not scanned:
        findings.append(f"{root}: no audit production Rust sources found")
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
    print("静态审计通过：audit protocol/auditd/service 接线路径无禁止 panic 或未证明安全的直接索引。")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
