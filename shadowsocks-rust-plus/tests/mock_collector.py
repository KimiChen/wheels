#!/usr/bin/env python3
"""Small protocol-faithful collector used by auditd integration tests.

The collector intentionally has no external dependencies.  It keeps raw event
JSON bytes when possible, verifies the response MAC before parsing a lease, and
uses a JSON state file only when the caller asks for persistence.  It is not a
production collector or a replacement for a controller database.
"""

from __future__ import annotations

import argparse
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
    _field(timestamp, POSITIVE_DECIMAL, "timestamp")
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
) -> ResponseMetadata:
    """Verify response digest, node and MAC before exposing response content."""

    metadata = parse_response_metadata(status, headers, request_nonce)
    if not hmac.compare_digest(metadata.node_id, expected_node):
        raise CollectorError("response node mismatch")
    if not hmac.compare_digest(metadata.response_sha256, sha256_hex(body)):
        raise CollectorError("response body digest mismatch")
    if status == 200:
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


def parse_lease(body: bytes, metadata: ResponseMetadata) -> list[dict[str, Any]]:
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
        _exact_keys(
            record,
            {
                "spool_schema_version",
                "spool_epoch",
                "spool_sequence",
                "received_at_unix_ms",
                "event_payload_sha256",
                "event",
            },
            "wrapper",
        )
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
        if not hmac.compare_digest(payload_hash, sha256_hex(event_bytes)):
            raise CollectorError("event payload digest mismatch")
        if epoch != metadata.spool_epoch:
            raise CollectorError("spool epoch mismatch")
        records.append(record)
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
    return records


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


def _secure_state_write(path: Path, value: dict[str, Any]) -> None:
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    path.parent.chmod(0o700)
    encoded = canonical_json(value) + b"\n"
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    try:
        os.fchmod(descriptor, 0o600)
        with os.fdopen(descriptor, "wb") as stream:
            stream.write(encoded)
            stream.flush()
            os.fsync(stream.fileno())
    except BaseException:
        try:
            os.close(descriptor)
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
        self.conflicts: list[dict[str, Any]] = []
        if state_path is not None and state_path.exists():
            self._load_state(state_path)

    def _load_state(self, path: Path) -> None:
        payload = path.read_bytes()
        value = strict_json(payload)
        if not isinstance(value, dict):
            raise CollectorError("collector state must be an object")
        _exact_keys(value, {"schema_version", "node_id", "events", "batches", "conflicts"}, "collector state")
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
        conflicts = value["conflicts"]
        if not isinstance(conflicts, list):
            raise CollectorError("collector conflict state is invalid")
        self.conflicts = conflicts

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
            "conflicts": self.conflicts,
        }
        _secure_state_write(self.state_path, value)

    def accept_records(self, records: Iterable[dict[str, Any]], metadata: ResponseMetadata) -> None:
        """Durably accept records, isolating event/batch conflicts atomically."""

        batch_id = metadata.batch_id
        batch_hash = metadata.lease_body_sha256
        batch_metadata = {
            "spool_epoch": metadata.spool_epoch,
            "first_sequence": metadata.first_sequence,
            "last_sequence": metadata.last_sequence,
            "event_count": metadata.event_count,
        }
        existing_batch = self.batches.get(batch_id)
        if existing_batch is not None and existing_batch[0] != batch_hash:
            self.conflicts.append({"kind": "batch_id_conflict", "batch_id": batch_id})
            self._save_state()
            raise CollectorError("batch_id_conflict")
        staged: list[tuple[str, str, dict[str, Any]]] = []
        for wrapper in records:
            event = wrapper["event"]
            event_id = event.get("event_id")
            if not isinstance(event_id, str):
                raise CollectorError("event has no event_id")
            # The protocol hashes the exact event bytes carried in the wrapper;
            # do not reserialize an object and accidentally change escaping.
            event_hash = _field(wrapper["event_payload_sha256"], HEX64, "event payload digest")
            previous = self.events.get(event_id)
            if previous is not None and previous[0] != event_hash:
                self.conflicts.append({"kind": "event_payload_conflict", "event_id": event_id})
                self._save_state()
                raise CollectorError("event_payload_conflict")
            if previous is None:
                staged.append((event_id, event_hash, event))
        for event_id, event_hash, event in staged:
            self.events[event_id] = (event_hash, event)
        self.batches[batch_id] = (batch_hash, batch_metadata)
        self._save_state()

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
    content_length = headers.get("Content-Length")
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
    parser.add_argument("--state", type=Path)
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
