#!/usr/bin/env python3
"""Small protocol-faithful collector used by auditd integration tests.

The collector intentionally has no external dependencies.  It keeps raw event
JSON bytes when possible, verifies the response MAC before parsing a lease, and
uses a JSON state file only when the caller asks for persistence.  It is not a
production collector or a replacement for a controller database.
"""

from __future__ import annotations

import argparse
import copy
import hashlib
import hmac
import json
import os
import re
import socket
import stat
import tempfile
import time
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterable


SCHEMA_VERSION = 1
MAX_BODY_BYTES = 8 * 1024 * 1024
MAX_REQUEST_BYTES = 4096
HEX32 = re.compile(r"[0-9a-f]{32}\Z")
HEX64 = re.compile(r"[0-9a-f]{64}\Z")
DECIMAL = re.compile(r"0|[1-9][0-9]*\Z")
POSITIVE_DECIMAL = re.compile(r"[1-9][0-9]*\Z")
IDENTIFIER = re.compile(r"[!-~]{1,128}\Z")


class CollectorError(ValueError):
    """A protocol or durable-state validation failure."""


class ParsedLease(list[dict[str, Any]]):
    """Parsed records plus the exact raw NDJSON wrapper digest for each row.

    The wire-level sequence idempotency key is defined over the wrapper bytes,
    not over the event payload digest.  Keeping the sidecar out of each record
    preserves the protocol object's exact field set while allowing the durable
    collector path to compare the original bytes across retries.
    """

    def __init__(self, records: Iterable[dict[str, Any]], wrapper_hashes: Iterable[str]) -> None:
        super().__init__(records)
        self.wrapper_hashes = tuple(wrapper_hashes)
        if len(self) != len(self.wrapper_hashes):
            raise CollectorError("parsed lease wrapper hash count mismatch")


def _duplicate_key_guard(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    value: dict[str, Any] = {}
    for key, item in pairs:
        if key in value:
            raise CollectorError(f"duplicate JSON key: {key}")
        value[key] = item
    return value


def _reject_constant(value: str) -> None:
    raise CollectorError(f"non-finite JSON number: {value}")


def strict_json(payload: bytes | str) -> Any:
    """Decode one complete UTF-8 JSON value with duplicate-key rejection."""

    if isinstance(payload, bytes):
        try:
            text = payload.decode("utf-8")
        except UnicodeDecodeError as exc:
            raise CollectorError("payload is not UTF-8") from exc
    else:
        text = payload
    if text.startswith("\ufeff"):
        raise CollectorError("BOM is not permitted")
    try:
        return json.loads(
            text,
            object_pairs_hook=_duplicate_key_guard,
            parse_constant=_reject_constant,
        )
    except CollectorError:
        raise
    except json.JSONDecodeError as exc:
        raise CollectorError("invalid JSON") from exc


def sha256_hex(payload: bytes) -> str:
    return hashlib.sha256(payload).hexdigest()


def _field(value: Any, pattern: re.Pattern[str], name: str) -> str:
    if not isinstance(value, str) or pattern.fullmatch(value) is None:
        raise CollectorError(f"invalid {name}")
    return value


def _header(headers: dict[str, str], name: str) -> str:
    lowered = name.lower()
    matches = [value for key, value in headers.items() if key.lower() == lowered]
    if len(matches) != 1:
        raise CollectorError(f"missing or duplicate header: {name}")
    return matches[0]


def canonical_request(
    method: str,
    path: str,
    node_id: str,
    timestamp: str,
    nonce: str,
    body_sha256: str,
) -> bytes:
    """Return the v1 request signing bytes (without a trailing LF)."""

    if method not in {"GET", "POST"} or not path.startswith("/") or "?" in path:
        raise CollectorError("invalid request target")
    if any("\r" in value or "\n" in value for value in (method, path, node_id, timestamp, nonce, body_sha256)):
        raise CollectorError("request signing field contains CR/LF")
    _field(node_id, IDENTIFIER, "node")
    _field(timestamp, DECIMAL, "timestamp")
    _field(nonce, HEX32, "nonce")
    _field(body_sha256, HEX64, "body digest")
    return "\n".join(
        ("SHADOWSOCKS-AUDIT-V1", method, path, node_id, timestamp, nonce, body_sha256)
    ).encode("utf-8")


def request_mac(key: bytes, canonical: bytes) -> str:
    if len(key) != 32:
        raise CollectorError("HMAC key must be 32 bytes")
    return hmac.new(key, canonical, hashlib.sha256).hexdigest()


def signed_request_headers(
    key: bytes,
    method: str,
    path: str,
    node_id: str,
    *,
    timestamp: int | str | None = None,
    nonce: str | None = None,
    body: bytes = b"",
) -> dict[str, str]:
    """Build the security headers for an export request."""

    timestamp_text = str(int(time.time()) if timestamp is None else timestamp)
    if nonce is None:
        nonce = os.urandom(16).hex()
    digest = sha256_hex(body)
    canonical = canonical_request(method, path, node_id, timestamp_text, nonce, digest)
    return {
        "X-Shadowsocks-Audit-Node": node_id,
        "X-Shadowsocks-Audit-Timestamp": timestamp_text,
        "X-Shadowsocks-Audit-Nonce": nonce,
        "X-Shadowsocks-Audit-Content-SHA256": digest,
        "Authorization": "Shadowsocks-Audit-HMAC-SHA256 " + request_mac(key, canonical),
    }


@dataclass(frozen=True)
class ResponseMetadata:
    status: int
    content_type: str
    schema: str
    lease_body_sha256: str
    node_id: str
    batch_id: str
    spool_epoch: str
    first_sequence: str
    last_sequence: str
    event_count: str
    response_sha256: str
    response_mac: str
    request_nonce: str


def canonical_response(metadata: ResponseMetadata) -> bytes:
    """Return response signing bytes, excluding a trailing LF."""

    fields = (
        "SHADOWSOCKS-AUDIT-RESPONSE-V1",
        metadata.request_nonce,
        str(metadata.status),
        metadata.content_type,
        metadata.schema,
        metadata.lease_body_sha256,
        metadata.node_id,
        metadata.batch_id,
        metadata.spool_epoch,
        metadata.first_sequence,
        metadata.last_sequence,
        metadata.event_count,
        metadata.response_sha256,
    )
    if any("\r" in value or "\n" in value for value in fields):
        raise CollectorError("response signing field contains CR/LF")
    return "\n".join(fields).encode("utf-8")


def parse_response_metadata(status: int, headers: dict[str, str], nonce: str) -> ResponseMetadata:
    """Extract signed response metadata using case-insensitive header names."""

    def optional(name: str) -> str:
        values = [value for key, value in headers.items() if key.lower() == name.lower()]
        if len(values) > 1:
            raise CollectorError(f"duplicate response header: {name}")
        return values[0] if values else ""

    response_sha = _field(_header(headers, "X-Shadowsocks-Audit-Response-SHA256"), HEX64, "response digest")
    response_mac = _field(_header(headers, "X-Shadowsocks-Audit-Response-MAC"), HEX64, "response MAC")
    node = _header(headers, "X-Shadowsocks-Audit-Node")
    return ResponseMetadata(
        status=status,
        content_type=optional("Content-Type"),
        schema=optional("X-Shadowsocks-Audit-Schema"),
        lease_body_sha256=optional("X-Shadowsocks-Audit-Body-SHA256"),
        node_id=node,
        batch_id=optional("X-Shadowsocks-Audit-Batch-Id"),
        spool_epoch=optional("X-Shadowsocks-Audit-Spool-Epoch"),
        first_sequence=optional("X-Shadowsocks-Audit-First-Sequence"),
        last_sequence=optional("X-Shadowsocks-Audit-Last-Sequence"),
        event_count=optional("X-Shadowsocks-Audit-Event-Count"),
        response_sha256=response_sha,
        response_mac=response_mac,
        request_nonce=nonce,
    )


def verify_response(
    key: bytes,
    *,
    status: int,
    headers: dict[str, str],
    body: bytes,
    request_nonce: str,
    expected_node: str,
    require_lease_body_digest: bool = False,
) -> ResponseMetadata:
    """Verify response digest, node and MAC before exposing response content."""

    metadata = parse_response_metadata(status, headers, request_nonce)
    if not hmac.compare_digest(metadata.node_id, expected_node):
        raise CollectorError("response node mismatch")
    if not hmac.compare_digest(metadata.response_sha256, sha256_hex(body)):
        raise CollectorError("response body digest mismatch")
    if require_lease_body_digest:
        if not metadata.lease_body_sha256:
            raise CollectorError("lease response lacks body digest")
        if not hmac.compare_digest(metadata.lease_body_sha256, sha256_hex(body)):
            raise CollectorError("lease body digest mismatch")
    expected_mac = request_mac(key, canonical_response(metadata))
    if not hmac.compare_digest(metadata.response_mac, expected_mac):
        raise CollectorError("response MAC mismatch")
    return metadata


def _exact_keys(value: dict[str, Any], expected: set[str], label: str) -> None:
    actual = set(value)
    if actual != expected:
        raise CollectorError(f"{label} has unexpected fields")


def _strict_decimal(value: Any, *, positive: bool = False) -> str:
    return _field(value, POSITIVE_DECIMAL if positive else DECIMAL, "decimal")


def parse_lease(body: bytes, metadata: ResponseMetadata) -> ParsedLease:
    """Parse and validate a signed NDJSON lease after MAC verification."""

    if metadata.status != 200:
        raise CollectorError("lease response is not HTTP 200")
    if metadata.content_type != "application/x-ndjson":
        raise CollectorError("lease content type mismatch")
    if metadata.schema != "1":
        raise CollectorError("lease schema mismatch")
    _field(metadata.spool_epoch, HEX32, "spool epoch")
    _field(metadata.batch_id, HEX32, "batch id")
    _strict_decimal(metadata.first_sequence, positive=True)
    _strict_decimal(metadata.last_sequence, positive=True)
    _strict_decimal(metadata.event_count, positive=True)
    if len(body) > MAX_BODY_BYTES:
        raise CollectorError("lease exceeds size limit")
    if not body or not body.endswith(b"\n"):
        raise CollectorError("lease must be newline terminated")
    records: list[dict[str, Any]] = []
    wrapper_hashes: list[str] = []
    previous: int | None = None
    for line in body.splitlines(keepends=True):
        if not line.endswith(b"\n") or line.endswith(b"\r\n"):
            raise CollectorError("invalid NDJSON line ending")
        raw_line = line[:-1]
        if not raw_line or raw_line[:1] in b" \t\r\n" or raw_line[-1:] in b" \t\r\n":
            raise CollectorError("wrapper has non-canonical whitespace")
        record = strict_json(raw_line)
        if not isinstance(record, dict):
            raise CollectorError("lease record must be an object")
        wrapper_fields = (
            "spool_schema_version",
            "spool_epoch",
            "spool_sequence",
            "received_at_unix_ms",
            "event_payload_sha256",
            "event",
        )
        _exact_keys(
            record,
            set(wrapper_fields),
            "wrapper",
        )
        canonical_wrapper = {name: record[name] for name in wrapper_fields}
        if canonical_json(canonical_wrapper) != raw_line:
            raise CollectorError("wrapper is not canonical JSON")
        if record["spool_schema_version"] != 1:
            raise CollectorError("unsupported spool schema")
        epoch = _field(record["spool_epoch"], HEX32, "spool epoch")
        sequence = _strict_decimal(record["spool_sequence"], positive=True)
        try:
            sequence_int = int(sequence)
        except ValueError as exc:
            raise CollectorError("spool sequence is out of range") from exc
        if previous is not None and sequence_int != previous + 1:
            raise CollectorError("spool sequence is not contiguous")
        previous = sequence_int
        _strict_decimal(record["received_at_unix_ms"], positive=True)
        payload_hash = _field(record["event_payload_sha256"], HEX64, "event payload digest")
        event = record["event"]
        if not isinstance(event, dict):
            raise CollectorError("event must be an object")
        event_bytes = _raw_object_field(raw_line, "event")
        if _canonical_event(event) != event_bytes:
            raise CollectorError("embedded event is not canonical JSON")
        if not hmac.compare_digest(payload_hash, sha256_hex(event_bytes)):
            raise CollectorError("event payload digest mismatch")
        if epoch != metadata.spool_epoch:
            raise CollectorError("spool epoch mismatch")
        records.append(record)
        # Include the required LF: this is the exact wrapper row carried by
        # the signed NDJSON body and therefore the correct sequence key.
        wrapper_hashes.append(sha256_hex(line))
    try:
        expected_count = int(metadata.event_count)
    except ValueError as exc:
        raise CollectorError("event count is out of range") from exc
    if len(records) != expected_count:
        raise CollectorError("event count mismatch")
    if records:
        if records[0]["spool_sequence"] != metadata.first_sequence:
            raise CollectorError("first sequence mismatch")
        if records[-1]["spool_sequence"] != metadata.last_sequence:
            raise CollectorError("last sequence mismatch")
    return ParsedLease(records, wrapper_hashes)


def _raw_object_field(raw_object: bytes, wanted_key: str) -> bytes:
    """Extract one top-level JSON value without changing its raw escaping."""

    try:
        text = raw_object.decode("utf-8")
    except UnicodeDecodeError as exc:
        raise CollectorError("wrapper is not UTF-8") from exc
    decoder = json.JSONDecoder(parse_constant=_reject_constant)
    index = 0

    def skip_space(position: int) -> int:
        while position < len(text) and text[position] in " \t\r\n":
            position += 1
        return position

    index = skip_space(index)
    if index >= len(text) or text[index] != "{":
        raise CollectorError("wrapper is not an object")
    index += 1
    found: bytes | None = None
    while True:
        index = skip_space(index)
        if index < len(text) and text[index] == "}":
            index += 1
            break
        try:
            key, key_end = decoder.raw_decode(text, index)
        except json.JSONDecodeError as exc:
            raise CollectorError("invalid wrapper key") from exc
        if not isinstance(key, str):
            raise CollectorError("wrapper key is not a string")
        index = skip_space(key_end)
        if index >= len(text) or text[index] != ":":
            raise CollectorError("wrapper key lacks colon")
        index = skip_space(index + 1)
        value_start = index
        try:
            _, value_end = decoder.raw_decode(text, index)
        except json.JSONDecodeError as exc:
            raise CollectorError("invalid wrapper value") from exc
        if key == wanted_key:
            if found is not None:
                raise CollectorError("duplicate wrapper event field")
            found = text[value_start:value_end].encode("utf-8")
        index = skip_space(value_end)
        if index < len(text) and text[index] == ",":
            index += 1
            continue
        if index < len(text) and text[index] == "}":
            index += 1
            break
        raise CollectorError("invalid wrapper delimiter")
    if skip_space(index) != len(text) or found is None:
        raise CollectorError("wrapper event field missing")
    return found


def canonical_json(value: Any) -> bytes:
    """Encode an already parsed object with the collector's deterministic form."""

    try:
        encoded = json.dumps(value, ensure_ascii=False, separators=(",", ":"), allow_nan=False)
    except (TypeError, ValueError) as exc:
        raise CollectorError("event cannot be encoded") from exc
    return encoded.encode("utf-8")


def _canonical_event(event: dict[str, Any]) -> bytes:
    event_type = event.get("event_type")
    common_access = (
        "schema_version",
        "record_type",
        "event_type",
        "event_id",
        "audit_sequence",
        "occurred_at_unix_ms",
        "runtime_monotonic_ms",
        "node_id",
        "runtime_id",
        "server_id",
        "server_generation",
        "identity_kind",
        "identity_name",
        "identity_generation",
        "transport",
    )
    orders = {
        "tcp_target_success": common_access + ("target", "success_evidence"),
        "udp_target_success": common_access + ("association_id", "target", "success_evidence"),
        "producer_gap": (
            "schema_version",
            "record_type",
            "event_type",
            "event_id",
            "audit_sequence",
            "occurred_at_unix_ms",
            "node_id",
            "runtime_id",
            "reason",
            "permanent_nack_code",
            "dropped_events",
            "first_dropped_sequence",
            "last_dropped_sequence",
            "first_seen_unix_ms",
            "last_seen_unix_ms",
        ),
        "udp_window_contention": (
            "schema_version",
            "record_type",
            "event_type",
            "event_id",
            "audit_sequence",
            "occurred_at_unix_ms",
            "node_id",
            "runtime_id",
            "skipped_successful_datagrams",
            "first_seen_unix_ms",
            "last_seen_unix_ms",
        ),
        "spool_gap": (
            "schema_version",
            "record_type",
            "event_type",
            "event_id",
            "occurred_at_unix_ms",
            "node_id",
            "spool_epoch",
            "lost_spool_epoch",
            "reason",
            "first_lost_spool_sequence",
            "last_lost_spool_sequence",
            "lost_events",
            "lost_bytes",
            "lost_batch_id",
        ),
    }
    order = orders.get(event_type)
    if order is None:
        # Compatibility for unit-level idempotency fixtures that do not model a
        # complete wire event. Real protocol events always select a fixed order.
        return canonical_json(event)
    _exact_keys(event, set(order), "event")
    ordered = {name: event[name] for name in order}
    if event_type in {"tcp_target_success", "udp_target_success"}:
        target = event["target"]
        if not isinstance(target, dict):
            raise CollectorError("event target must be an object")
        target_order = ("kind", "host", "normalized_host", "port", "remote_ip")
        _exact_keys(target, set(target_order), "event target")
        ordered["target"] = {name: target[name] for name in target_order}
    return canonical_json(ordered)


def _secure_state_write(path: Path, value: dict[str, Any]) -> None:
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    path.parent.chmod(0o700)
    encoded = canonical_json(value) + b"\n"
    # A process crash can leave an uncommitted temporary state file. Remove
    # only the exact private naming pattern before creating the next snapshot;
    # never follow a symlink or sweep unrelated collector data.
    for stale in path.parent.glob(f".{path.name}.tmp*"):
        try:
            metadata = stale.lstat()
        except FileNotFoundError:
            continue
        if stat.S_ISREG(metadata.st_mode):
            stale.unlink()
    directory_fd = os.open(path.parent, os.O_RDONLY)
    try:
        os.fsync(directory_fd)
    finally:
        os.close(directory_fd)
    temporary = path.with_name(f".{path.name}.tmp")
    descriptor = os.open(
        temporary,
        os.O_WRONLY | os.O_CREAT | os.O_EXCL,
        0o600,
    )
    try:
        os.fchmod(descriptor, 0o600)
        stream = os.fdopen(descriptor, "wb")
        # Ownership transfers to the file object.  If a later operation
        # fails, do not close the descriptor a second time (or close a
        # different descriptor after its number has been reused).
        descriptor = -1
        with stream:
            stream.write(encoded)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, path)
        directory_fd = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(directory_fd)
        finally:
            os.close(directory_fd)
    except BaseException:
        if descriptor >= 0:
            try:
                os.close(descriptor)
            except OSError:
                pass
        try:
            temporary.unlink()
        except OSError:
            pass
        raise


class MockCollector:
    """In-memory collector state with optional durable idempotency records."""

    def __init__(self, node_id: str, key: bytes, state_path: Path | None = None) -> None:
        if not node_id or "\r" in node_id or "\n" in node_id:
            raise CollectorError("invalid node id")
        if len(key) != 32:
            raise CollectorError("HMAC key must be 32 bytes")
        self.node_id = node_id
        self.key = bytes(key)
        self.state_path = state_path
        self.events: dict[str, tuple[str, dict[str, Any]]] = {}
        self.batches: dict[str, tuple[str, dict[str, Any]]] = {}
        self.sequences: dict[str, str] = {}
        self.conflicts: list[dict[str, Any]] = []
        # Incoming material that conflicts with an accepted key is retained
        # separately. It is evidence, not a replacement for the first value.
        self.isolated_batches: dict[str, dict[str, Any]] = {}
        self.isolated_wrappers: list[dict[str, Any]] = []
        self.diagnostics: list[dict[str, Any]] = []
        self.gaps: list[dict[str, Any]] = []
        if state_path is not None and state_path.exists():
            self._load_state(state_path)

    def _load_state(self, path: Path) -> None:
        payload = path.read_bytes()
        value = strict_json(payload)
        if not isinstance(value, dict):
            raise CollectorError("collector state must be an object")
        keys = set(value)
        required = {"schema_version", "node_id", "events", "batches", "conflicts"}
        allowed = set(required)
        allowed.update({"sequences", "diagnostics", "gaps", "isolated_batches", "isolated_wrappers"})
        if not required <= keys or not keys <= allowed:
            raise CollectorError("collector state has missing or unexpected fields")
        if value["schema_version"] != 1 or value["node_id"] != self.node_id:
            raise CollectorError("collector state identity mismatch")
        if not isinstance(value["events"], dict) or not isinstance(value["batches"], dict):
            raise CollectorError("collector state maps are invalid")
        for event_id, item in value["events"].items():
            if not isinstance(item, dict) or set(item) != {"hash", "event"}:
                raise CollectorError("collector event state is invalid")
            self.events[event_id] = (str(item["hash"]), item["event"])
        for batch_id, item in value["batches"].items():
            if not isinstance(item, dict) or set(item) != {"hash", "metadata"}:
                raise CollectorError("collector batch state is invalid")
            self.batches[batch_id] = (str(item["hash"]), item["metadata"])
        for sequence_id, digest in value.get("sequences", {}).items():
            if not isinstance(sequence_id, str) or not isinstance(digest, str):
                raise CollectorError("collector sequence state is invalid")
            self.sequences[sequence_id] = digest
        conflicts = value["conflicts"]
        if not isinstance(conflicts, list):
            raise CollectorError("collector conflict state is invalid")
        self.conflicts = conflicts
        isolated_batches = value.get("isolated_batches", {})
        if not isinstance(isolated_batches, dict):
            raise CollectorError("collector isolated batch state is invalid")
        self.isolated_batches = isolated_batches
        isolated_wrappers = value.get("isolated_wrappers", [])
        if not isinstance(isolated_wrappers, list) or not all(isinstance(item, dict) for item in isolated_wrappers):
            raise CollectorError("collector isolated wrapper state is invalid")
        self.isolated_wrappers = isolated_wrappers
        for field, target in (("diagnostics", self.diagnostics), ("gaps", self.gaps)):
            values = value.get(field, [])
            if not isinstance(values, list) or not all(isinstance(item, dict) for item in values):
                raise CollectorError(f"collector {field} state is invalid")
            target.extend(values)

    def _save_state(self) -> None:
        if self.state_path is None:
            return
        value = {
            "schema_version": 1,
            "node_id": self.node_id,
            "events": {
                key: {"hash": digest, "event": event}
                for key, (digest, event) in sorted(self.events.items())
            },
            "batches": {
                key: {"hash": digest, "metadata": metadata}
                for key, (digest, metadata) in sorted(self.batches.items())
            },
            "sequences": dict(sorted(self.sequences.items())),
            "conflicts": self.conflicts,
            "diagnostics": self.diagnostics,
            "gaps": self.gaps,
            "isolated_batches": self.isolated_batches,
            "isolated_wrappers": self.isolated_wrappers,
        }
        _secure_state_write(self.state_path, value)

    def _durable_mutation(self, callback: Any) -> None:
        """Apply a state mutation only if its durable snapshot succeeds."""

        snapshot = (
            copy.deepcopy(self.events),
            copy.deepcopy(self.batches),
            copy.deepcopy(self.sequences),
            copy.deepcopy(self.conflicts),
            copy.deepcopy(self.diagnostics),
            copy.deepcopy(self.gaps),
            copy.deepcopy(self.isolated_batches),
            copy.deepcopy(self.isolated_wrappers),
        )
        try:
            callback()
            self._save_state()
        except BaseException:
            (
                self.events,
                self.batches,
                self.sequences,
                self.conflicts,
                self.diagnostics,
                self.gaps,
                self.isolated_batches,
                self.isolated_wrappers,
            ) = snapshot
            raise

    def record_diagnostic(self, kind: str, **fields: Any) -> None:
        """Record a producer/audit diagnostic and make it visible to health."""

        if (
            not kind
            or "\r" in kind
            or "\n" in kind
            or any("\r" in str(value) or "\n" in str(value) for value in fields.values())
        ):
            raise CollectorError("invalid diagnostic")
        self._durable_mutation(lambda: self.diagnostics.append({"kind": kind, **fields}))

    def record_gap(self, kind: str, **fields: Any) -> None:
        """Record a loss gap; gaps remain observable until explicitly cleared."""

        if kind not in {"spool_gap", "producer_gap", "batch_evicted"}:
            raise CollectorError("invalid gap kind")
        self._durable_mutation(lambda: self.gaps.append({"kind": kind, **fields}))

    def health(self) -> dict[str, Any]:
        """Return a collector-facing health summary with actionable alerts."""

        alerts: list[str] = []
        if self.conflicts:
            alerts.append("idempotency_conflict")
        if self.diagnostics:
            alerts.extend(sorted({str(item["kind"]) for item in self.diagnostics}))
        if self.gaps:
            alerts.extend(sorted({str(item["kind"]) for item in self.gaps}))
        return {
            "schema_version": 1,
            "node_id": self.node_id,
            "status": "degraded" if alerts else "ok",
            "event_count": len(self.events),
            "batch_count": len(self.batches),
            "conflict_count": len(self.conflicts),
            "diagnostic_count": len(self.diagnostics),
            "spool_gap_count": sum(item["kind"] == "spool_gap" for item in self.gaps),
            "producer_gap_count": sum(item["kind"] == "producer_gap" for item in self.gaps),
            "batch_evicted_count": sum(item["kind"] == "batch_evicted" for item in self.gaps),
            "isolated_batch_count": len(self.isolated_batches),
            "isolated_wrapper_count": len(self.isolated_wrappers),
            "alerts": sorted(set(alerts)),
        }

    def accept_records(self, records: Iterable[dict[str, Any]], metadata: ResponseMetadata) -> None:
        """Durably accept records, isolating event/batch conflicts atomically."""

        raw_wrapper_hashes = getattr(records, "wrapper_hashes", None)
        records_list = list(records)
        if raw_wrapper_hashes is not None and len(raw_wrapper_hashes) != len(records_list):
            raise CollectorError("parsed lease wrapper hash count mismatch")
        if raw_wrapper_hashes is None:
            # This compatibility path is only for callers that already hold
            # parsed records (the wire-facing collect_once path always uses
            # ParsedLease).  There are no raw bytes left to hash, so use the
            # collector's deterministic representation rather than the event
            # payload hash, which would collapse distinct wrappers.
            wrapper_hashes = [sha256_hex(canonical_json(wrapper)) for wrapper in records_list]
        else:
            wrapper_hashes = list(raw_wrapper_hashes)
        batch_id = metadata.batch_id
        batch_hash = metadata.lease_body_sha256
        batch_metadata = {
            "spool_epoch": metadata.spool_epoch,
            "first_sequence": metadata.first_sequence,
            "last_sequence": metadata.last_sequence,
            "event_count": metadata.event_count,
        }
        isolation_key = self._batch_isolation_key(batch_id, batch_hash, batch_metadata)
        existing_batch = self.batches.get(batch_id)
        if existing_batch is not None:
            existing_hash, existing_metadata = existing_batch
            if existing_hash != batch_hash or existing_metadata != batch_metadata:
                if isolation_key in self.isolated_batches:
                    # The conflicting raw batch was already durably isolated
                    # and ACKed by the caller; do not create an unbounded
                    # duplicate conflict record on a retry.
                    return
                def isolate_batch() -> None:
                    self.conflicts.append({"kind": "batch_id_conflict", "batch_id": batch_id})
                    self.isolated_batches[isolation_key] = {
                        "batch_id": batch_id,
                        "body_sha256": batch_hash,
                        "metadata": batch_metadata,
                        "records": copy.deepcopy(records_list),
                    }

                # A batch identity conflict is all-or-nothing: no incoming
                # record may enter the accepted maps.
                self._durable_mutation(isolate_batch)
                return
            # A byte-identical batch replay is fully idempotent.
            return
        staged: list[tuple[str, str, dict[str, Any]]] = []
        staged_by_event: dict[str, str] = {}
        staged_sequences: dict[str, str] = {}
        def stage() -> None:
            for index, wrapper in enumerate(records_list):
                self._stage_wrapper(
                    wrapper,
                    batch_metadata["spool_epoch"],
                    wrapper_hashes[index],
                    staged,
                    staged_by_event,
                    staged_sequences,
                )
            for event_id, event_hash, event in staged:
                self.events[event_id] = (event_hash, event)
            for sequence_id, wrapper_hash in staged_sequences.items():
                self.sequences[sequence_id] = wrapper_hash
            self.batches[batch_id] = (batch_hash, batch_metadata)

        self._durable_mutation(stage)

    @staticmethod
    def _batch_isolation_key(batch_id: str, body_hash: str, metadata: dict[str, str]) -> str:
        metadata_hash = sha256_hex(canonical_json(metadata))
        return f"{batch_id}:{body_hash}:{metadata_hash}"

    def _stage_wrapper(
        self,
        wrapper: dict[str, Any],
        spool_epoch: str,
        wrapper_hash: str,
        staged: list[tuple[str, str, dict[str, Any]]],
        staged_by_event: dict[str, str],
        staged_sequences: dict[str, str],
    ) -> None:
        event = wrapper.get("event")
        if not isinstance(event, dict):
            raise CollectorError("event must be an object")
        event_id = event.get("event_id")
        if not isinstance(event_id, str):
            raise CollectorError("event has no event_id")
        # The protocol hashes the exact event bytes carried in the wrapper;
        # do not reserialize an object and accidentally change escaping.
        event_hash = _field(wrapper.get("event_payload_sha256"), HEX64, "event payload digest")
        sequence = _strict_decimal(wrapper.get("spool_sequence"), positive=True)
        wrapper_hash = _field(wrapper_hash, HEX64, "wrapper digest")
        sequence_id = f"{self.node_id}:{spool_epoch}:{sequence}"
        sequence_hash = self.sequences.get(sequence_id)
        if sequence_hash is None:
            sequence_hash = staged_sequences.get(sequence_id)
        if sequence_hash is not None and sequence_hash != wrapper_hash:
            self.conflicts.append(
                {"kind": "spool_sequence_conflict", "spool_epoch": spool_epoch, "spool_sequence": sequence}
            )
            self.isolated_wrappers.append(copy.deepcopy(wrapper))
            return
        previous = self.events.get(event_id)
        if previous is not None and previous[0] != event_hash:
            self.conflicts.append({"kind": "event_payload_conflict", "event_id": event_id})
            self.isolated_wrappers.append(copy.deepcopy(wrapper))
            return
        staged_hash = staged_by_event.get(event_id)
        if staged_hash is not None and staged_hash != event_hash:
            self.conflicts.append({"kind": "event_payload_conflict", "event_id": event_id})
            self.isolated_wrappers.append(copy.deepcopy(wrapper))
            return
        if previous is None and staged_hash is None:
            staged.append((event_id, event_hash, copy.deepcopy(event)))
            staged_by_event[event_id] = event_hash
        if sequence_hash is None:
            staged_sequences[sequence_id] = wrapper_hash

    def ack_body(self, metadata: ResponseMetadata) -> bytes:
        return canonical_json(
            {"schema_version": 1, "batch_id": metadata.batch_id, "body_sha256": metadata.lease_body_sha256}
        )

    def build_request(self, method: str, path: str, body: bytes = b"", *, nonce: str | None = None) -> bytes:
        if len(body) > MAX_REQUEST_BYTES:
            raise CollectorError("request body exceeds limit")
        headers = signed_request_headers(self.key, method, path, self.node_id, nonce=nonce, body=body)
        lines = [f"{method} {path} HTTP/1.1", "Host: auditd", "Connection: close"]
        if method == "POST":
            lines.extend(("Content-Type: application/json", f"Content-Length: {len(body)}"))
        lines.extend(f"{name}: {value}" for name, value in headers.items())
        return ("\r\n".join(lines) + "\r\n\r\n").encode("ascii") + body

    def collect_once(self, socket_path: Path) -> int:
        """Lease, verify, durably accept, and ACK one batch from an auditd UDS."""

        if self.state_path is None:
            raise CollectorError("collect_once requires a durable state path before ACK")
        request_body = b'{"schema_version":1}'
        nonce = os.urandom(16).hex()
        request = self.build_request("POST", "/v1/audit/lease", request_body, nonce=nonce)
        status, headers, body = unix_http_request(socket_path, request)
        metadata = verify_response(
            self.key,
            status=status,
            headers=headers,
            body=body,
            request_nonce=nonce,
            expected_node=self.node_id,
            require_lease_body_digest=(status == 200),
        )
        if status == 204:
            return 0
        records = parse_lease(body, metadata)
        self.accept_records(records, metadata)
        ack_body = self.ack_body(metadata)
        ack_nonce = os.urandom(16).hex()
        ack_request = self.build_request("POST", "/v1/audit/ack", ack_body, nonce=ack_nonce)
        ack_status, ack_headers, ack_response = unix_http_request(socket_path, ack_request)
        verify_response(
            self.key,
            status=ack_status,
            headers=ack_headers,
            body=ack_response,
            request_nonce=ack_nonce,
            expected_node=self.node_id,
        )
        if ack_status != 200 or strict_json(ack_response) != {"schema_version": 1, "status": "acked"}:
            raise CollectorError("auditd did not acknowledge batch")
        return len(records)


def _parse_http_response(payload: bytes) -> tuple[int, dict[str, str], bytes]:
    marker = payload.find(b"\r\n\r\n")
    if marker < 0:
        raise CollectorError("response headers are incomplete")
    head = payload[:marker].decode("ascii")
    body = payload[marker + 4 :]
    lines = head.split("\r\n")
    if not lines or not lines[0].startswith("HTTP/1.1 "):
        raise CollectorError("response is not HTTP/1.1")
    try:
        status = int(lines[0].split(" ", 2)[1])
    except (IndexError, ValueError) as exc:
        raise CollectorError("invalid HTTP status") from exc
    headers: dict[str, str] = {}
    for line in lines[1:]:
        if ": " not in line:
            raise CollectorError("invalid response header")
        name, value = line.split(": ", 1)
        if name.lower() in {key.lower() for key in headers}:
            raise CollectorError("duplicate response header")
        headers[name] = value
    content_length = next(
        (value for name, value in headers.items() if name.lower() == "content-length"),
        None,
    )
    if content_length is not None:
        try:
            expected = int(content_length)
        except ValueError as exc:
            raise CollectorError("invalid Content-Length") from exc
        if expected != len(body):
            raise CollectorError("response body length mismatch")
    elif status != 204:
        raise CollectorError("response lacks Content-Length")
    return status, headers, body


def unix_http_request(socket_path: Path, request: bytes, timeout: float = 5.0) -> tuple[int, dict[str, str], bytes]:
    if len(request) > MAX_REQUEST_BYTES + 2048:
        raise CollectorError("request exceeds collector limit")
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as client:
        client.settimeout(timeout)
        client.connect(str(socket_path))
        client.sendall(request)
        chunks: list[bytes] = []
        total = 0
        while True:
            chunk = client.recv(64 * 1024)
            if not chunk:
                break
            chunks.append(chunk)
            total += len(chunk)
            if total > MAX_BODY_BYTES + 64 * 1024:
                raise CollectorError("response exceeds collector limit")
    return _parse_http_response(b"".join(chunks))


def _read_key(path: Path) -> bytes:
    payload = path.read_bytes()
    if payload.endswith(b"\n"):
        payload = payload[:-1]
    if len(payload) != 64 or HEX64.fullmatch(payload.decode("ascii", errors="ignore")) is None:
        raise CollectorError("HMAC key file must contain 64 lowercase hex characters")
    return bytes.fromhex(payload.decode("ascii"))


def main() -> int:
    parser = argparse.ArgumentParser(description="one-shot shadowsocks audit export collector")
    parser.add_argument("--socket", required=True, type=Path)
    parser.add_argument("--node", required=True)
    parser.add_argument("--key-file", required=True, type=Path)
    parser.add_argument("--state", type=Path, required=True)
    args = parser.parse_args()
    try:
        collector = MockCollector(args.node, _read_key(args.key_file), args.state)
        count = collector.collect_once(args.socket)
    except (CollectorError, OSError) as exc:
        # Deliberately omit request/response bodies and key material from CLI errors.
        print(f"collector failed: {exc}", file=os.sys.stderr)
        return 1
    print(f"accepted {count} audit records")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
