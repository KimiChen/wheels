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
SERVICE_RELAY_WIRING_FILES = (
    "crates/shadowsocks-service/src/server/tcprelay.rs",
    "crates/shadowsocks-service/src/server/udprelay.rs",
    "crates/shadowsocks-service/src/server/context.rs",
)
REQUIRED_PRODUCTION_FILES = (
    "Cargo.toml",
    "Cargo.lock",
    "crates/shadowsocks-audit-protocol/Cargo.toml",
    "crates/shadowsocks-audit-protocol/src/lib.rs",
    "crates/shadowsocks-auditd/Cargo.toml",
    "crates/shadowsocks-auditd/src/lib.rs",
    "crates/shadowsocks-auditd/src/config.rs",
    "crates/shadowsocks-auditd/src/export.rs",
    "crates/shadowsocks-auditd/src/ingest.rs",
    "crates/shadowsocks-auditd/src/protocol.rs",
    "crates/shadowsocks-auditd/src/spool.rs",
    "crates/shadowsocks-service/Cargo.toml",
    "crates/shadowsocks-service/src/config.rs",
    "crates/shadowsocks-service/src/lib.rs",
    "crates/shadowsocks-service/src/server/mod.rs",
    "crates/shadowsocks-service/src/server/server.rs",
    "crates/shadowsocks-service/src/server/user_audit.rs",
    "crates/shadowsocks-service/src/server/user_stats.rs",
    "crates/shadowsocks-service/src/server/tcprelay.rs",
    "crates/shadowsocks-service/src/server/udprelay.rs",
    "crates/shadowsocks-service/src/server/context.rs",
)
AUDIT_MARKER = re.compile(
    r"\b(?:user_audit|audit_emitter|AuditEmitter|AuditTarget|AuditRecord|audit_event|user-audit)\b"
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
    """Find complete Rust items whose cfg predicate includes ``test``."""
    skipped: set[int] = set()
    cursor = 0
    while cursor < len(lines):
        attribute = lines[cursor].strip()
        if re.fullmatch(r"#\[cfg\([^\]]*\btest\b[^\]]*\)\]", attribute) is None:
            cursor += 1
            continue
        start = cursor
        cursor += 1
        depth = 0
        saw_brace = False
        comment_depth = 0
        raw_hashes: int | None = None
        while cursor < len(lines):
            structural, comment_depth, raw_hashes = _structural_characters_for_item(
                lines[cursor], comment_depth, raw_hashes
            )
            depth += structural.count("{") - structural.count("}")
            saw_brace = saw_brace or "{" in structural
            cursor += 1
            if saw_brace and depth == 0:
                break
            if not saw_brace and ";" in structural:
                break
        skipped.update(range(start, cursor))
    return skipped


def _structural_characters_for_item(
    line: str, block_comment_depth: int, raw_hashes: int | None
) -> tuple[str, int, int | None]:
    """Count braces for cfg(test) items while ignoring multiline raw strings."""

    result: list[str] = []
    index = 0
    quote: str | None = None
    while index < len(line):
        if raw_hashes is not None:
            closing = '"' + ("#" * raw_hashes)
            end = line.find(closing, index)
            if end < 0:
                return "".join(result), block_comment_depth, raw_hashes
            raw_hashes = None
            index = end + len(closing)
            continue
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
        # Raw strings may be prefixed with `b`; consume the complete delimiter
        # and carry its hash count across lines.
        raw_start = index
        if current == "b" and following == "r":
            raw_start += 1
        if current == "r" or (current == "b" and following == "r"):
            marker = raw_start + 1
            while marker < len(line) and line[marker] == "#":
                marker += 1
            if marker < len(line) and line[marker] == '"':
                raw_hashes = marker - (raw_start + 1)
                index = marker + 1
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
    return "".join(result), block_comment_depth, raw_hashes


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


def _check_wiring_file_functions(path: Path) -> list[str]:
    """Check complete functions that contain user-audit relay wiring.

    Looking only at the marker line misses a panic or an unchecked index on the
    following line.  We first identify the enclosing Rust function for every
    marker, then apply the same structural checks used for dedicated audit
    files to that whole function.  Unrelated upstream functions remain out of
    scope, keeping this guard useful on the large relay modules.
    """

    findings: list[str] = []
    lines = path.read_text(encoding="utf-8").splitlines()
    test_only = _test_only_lines(lines)
    structural_lines: list[str] = []
    comment_depth = 0
    for line in lines:
        structural, comment_depth = _structural_characters(line, comment_depth)
        structural_lines.append(structural)

    depth_before: list[int] = []
    depth = 0
    for structural in structural_lines:
        depth_before.append(depth)
        depth += structural.count("{") - structural.count("}")

    function_starts = [
        index
        for index, structural in enumerate(structural_lines)
        if re.search(r"\b(?:async\s+)?fn\s+[A-Za-z_][A-Za-z0-9_]*", structural)
    ]
    audited_lines: set[int] = set()
    for marker_index, structural in enumerate(structural_lines):
        raw_attribute = lines[marker_index].strip()
        audit_cfg = raw_attribute in {
            '#[cfg(feature = "user-audit")]',
            '#[cfg(all(feature = "user-audit", target_os = "linux"))]',
        }
        if AUDIT_MARKER.search(structural) is None and not audit_cfg:
            continue
        candidates = [
            start
            for start in function_starts
            if start <= marker_index and depth_before[start] <= depth_before[marker_index]
        ]
        start = candidates[-1] if candidates else marker_index
        baseline_depth = depth_before[start]
        saw_brace = False
        end = start
        for cursor in range(start, len(structural_lines)):
            current = structural_lines[cursor]
            saw_brace = saw_brace or "{" in current
            end = cursor
            if saw_brace and depth_before[cursor] <= baseline_depth and cursor > start:
                break
        # A marker in an attribute/closure without a discoverable function is
        # still checked locally instead of silently escaping the guard.
        if not candidates:
            start = max(0, marker_index - 8)
            end = min(len(lines) - 1, marker_index + 8)
        audited_lines.update(range(start, end + 1))

    for line_index in sorted(audited_lines):
        if line_index in test_only:
            continue
        structural = structural_lines[line_index]
        number = line_index + 1
        if FORBIDDEN.search(structural):
            findings.append(f"{path}:{number}: forbidden panic path in user-audit wiring")
        for match in INDEX.finditer(structural):
            if not _index_is_proven_safe(structural_lines, line_index, match):
                findings.append(f"{path}:{number}: direct index in user-audit wiring")
    return findings


def _check_relay_wiring_file(path: Path) -> list[str]:
    """Audit complete ``cfg(user-audit)`` items in relay/context modules.

    Relay modules contain a large amount of unrelated upstream code with
    deliberate indexing patterns. Feature attributes provide a precise
    boundary for audit-specific items; checking each complete item catches a
    panic or unchecked index introduced after the attribute line.
    """

    findings: list[str] = []
    lines = path.read_text(encoding="utf-8").splitlines()
    test_only = _test_only_lines(lines)
    structural_lines: list[str] = []
    comment_depth = 0
    for line in lines:
        structural, comment_depth = _structural_characters(line, comment_depth)
        structural_lines.append(structural)

    audited_lines: set[int] = set()
    cursor = 0
    while cursor < len(lines):
        attribute = lines[cursor].strip()
        if attribute not in {
            '#[cfg(feature = "user-audit")]',
            '#[cfg(all(feature = "user-audit", target_os = "linux"))]',
        }:
            cursor += 1
            continue
        start = cursor
        cursor += 1
        # A cfg attribute can guard a struct field or a local binding rather
        # than a whole function. Stop at its comma/semicolon so a later
        # function brace cannot make the selected range span the enclosing item.
        while cursor < len(lines) and not structural_lines[cursor].strip():
            cursor += 1
        first_item = structural_lines[cursor].strip() if cursor < len(lines) else ""
        item_has_body = bool(re.search(r"\b(?:fn|struct|enum|impl|trait|mod)\b", first_item)) or "{" in first_item
        if not item_has_body:
            while cursor < len(lines):
                current = structural_lines[cursor]
                cursor += 1
                if ";" in current or "," in current:
                    break
            audited_lines.update(range(start, cursor))
            continue
        depth = 0
        saw_brace = False
        while cursor < len(lines):
            current = structural_lines[cursor]
            depth += current.count("{") - current.count("}")
            saw_brace = saw_brace or "{" in current
            cursor += 1
            if saw_brace and depth == 0:
                break
            if not saw_brace and ";" in current:
                break
        audited_lines.update(range(start, cursor))

    # Some call sites are inside a larger feature-gated function without a
    # nested cfg attribute. Include a short neighborhood around explicit audit
    # markers so a newly added unchecked operation cannot hide on the next line.
    for index, structural in enumerate(structural_lines):
        if AUDIT_MARKER.search(structural) is not None:
            audited_lines.update(range(max(0, index - 2), min(len(lines), index + 3)))

    for line_index in sorted(audited_lines):
        if line_index in test_only:
            continue
        structural = structural_lines[line_index]
        number = line_index + 1
        if FORBIDDEN.search(structural):
            findings.append(f"{path}:{number}: forbidden panic path in user-audit wiring")
        for match in INDEX.finditer(structural):
            if not _index_is_proven_safe(structural_lines, line_index, match):
                findings.append(f"{path}:{number}: direct index in user-audit wiring")
    return findings


def _check_wiring_file(path: Path) -> list[str]:
    """Check only lines that explicitly contain the audit feature wiring."""

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


def check(root: Path, *, require_complete: bool = True) -> list[str]:
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
    for relative in SERVICE_RELAY_WIRING_FILES:
        service_file = root / relative
        if service_file.is_file():
            # Relay audit code is commonly a local cfg-gated block inside a
            # larger function. Checking only the attribute item or a marker
            # neighborhood misses later statements in multiline if-let/match
            # blocks, so audit every complete function containing a marker.
            findings.extend(_check_wiring_file_functions(service_file))
            scanned.add(service_file)
    if not scanned:
        findings.append(f"{root}: no audit production Rust sources found")
    if require_complete:
        for relative in REQUIRED_PRODUCTION_FILES:
            if not (root / relative).is_file():
                findings.append(f"{root / relative}: required audit production source is missing")
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
    parser.add_argument(
        "--allow-partial",
        action="store_true",
        help="仅用于单元测试的最小临时树；正式检查默认拒绝缺失源文件",
    )
    args = parser.parse_args()
    findings = check(args.source.resolve(), require_complete=not args.allow_partial)
    if findings:
        for finding in findings:
            print(finding)
        return 1
    print("静态审计通过：audit protocol/auditd/service 接线路径无禁止 panic 或未证明安全的直接索引。")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
