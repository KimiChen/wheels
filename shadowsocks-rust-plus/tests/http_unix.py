"""Strict, dependency-free HTTP/1.1-over-Unix helpers for test programs."""

from __future__ import annotations

import json
import socket
from dataclasses import dataclass
from pathlib import Path


MAX_HEADER_BYTES = 64 * 1024
HTTP_TOKEN_CHARACTERS = frozenset("!#$%&'*+-.^_`|~0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz")
U64_MAX = (1 << 64) - 1
COUNTER_FIELDS = (
    "tcp_uplink_bytes",
    "tcp_downlink_bytes",
    "udp_uplink_bytes",
    "udp_downlink_bytes",
)


@dataclass(frozen=True)
class HttpResponse:
    status: int
    headers: dict[str, str]
    body: bytes


def build_request(
    method: str = "GET",
    target: str = "/v1/snapshot",
    body: bytes = b"",
    extra_headers: tuple[tuple[str, str], ...] = (),
) -> bytes:
    lines = [
        f"{method} {target} HTTP/1.1",
        "Host: localhost",
        "Accept: application/json",
        "Connection: close",
    ]
    if body:
        lines.append(f"Content-Length: {len(body)}")
        lines.append("Content-Type: application/json")
    lines.extend(f"{name}: {value}" for name, value in extra_headers)
    return ("\r\n".join(lines) + "\r\n\r\n").encode("ascii") + body


SNAPSHOT_REQUEST = build_request()
HEALTH_REQUEST = build_request(target="/healthz")


def _receive_more(client: socket.socket, payload: bytearray, max_wire_bytes: int) -> None:
    if len(payload) > max_wire_bytes:
        raise RuntimeError("HTTP response framing exceeds its allowed size")
    chunk = client.recv(min(65_536, max_wire_bytes + 1 - len(payload)))
    if not chunk:
        raise RuntimeError("HTTP response ended before its body was complete")
    payload.extend(chunk)
    if len(payload) > max_wire_bytes:
        raise RuntimeError("HTTP response framing exceeds its allowed size")


def _receive_chunked(client: socket.socket, prefix: bytes, max_body_bytes: int) -> bytes:
    decoded = bytearray()
    payload = bytearray(prefix)
    cursor = 0
    max_wire_bytes = max_body_bytes + MAX_HEADER_BYTES
    if len(payload) > max_wire_bytes:
        raise RuntimeError("HTTP response framing exceeds its allowed size")
    while True:
        line_end = payload.find(b"\r\n", cursor)
        while line_end < 0:
            _receive_more(client, payload, max_wire_bytes)
            line_end = payload.find(b"\r\n", cursor)
        size_field = payload[cursor:line_end].split(b";", 1)[0].strip()
        if not size_field or any(byte not in b"0123456789abcdefABCDEF" for byte in size_field):
            raise RuntimeError("invalid HTTP chunk size")
        chunk_size = int(size_field, 16)
        cursor = line_end + 2
        if chunk_size == 0:
            while True:
                trailer_end = payload.find(b"\r\n", cursor)
                while trailer_end < 0:
                    _receive_more(client, payload, max_wire_bytes)
                    trailer_end = payload.find(b"\r\n", cursor)
                trailer = payload[cursor:trailer_end]
                cursor = trailer_end + 2
                if not trailer:
                    if cursor != len(payload):
                        raise RuntimeError("bytes follow the final HTTP chunk")
                    return bytes(decoded)
                if trailer[:1] in (b" ", b"\t") or b":" not in trailer:
                    raise RuntimeError("invalid HTTP trailer")
        chunk_end = cursor + chunk_size
        while chunk_end + 2 > len(payload):
            _receive_more(client, payload, max_wire_bytes)
        if payload[chunk_end:chunk_end + 2] != b"\r\n":
            raise RuntimeError("invalid HTTP chunk terminator")
        decoded.extend(payload[cursor:chunk_end])
        if len(decoded) > max_body_bytes:
            raise RuntimeError("HTTP response body exceeds its allowed size")
        cursor = chunk_end + 2


def receive_response(
    client: socket.socket,
    max_body_bytes: int,
) -> HttpResponse:
    if max_body_bytes <= 0:
        raise ValueError("max_body_bytes must be positive")

    received = bytearray()
    while b"\r\n\r\n" not in received:
        chunk = client.recv(min(16_384, MAX_HEADER_BYTES + 1 - len(received)))
        if not chunk:
            raise RuntimeError("HTTP response ended before its headers were complete")
        received.extend(chunk)
        if len(received) > MAX_HEADER_BYTES:
            raise RuntimeError("HTTP response headers are too large")

    header_block, body_prefix = bytes(received).split(b"\r\n\r\n", 1)
    try:
        header_lines = header_block.decode("ascii").split("\r\n")
    except UnicodeDecodeError as error:
        raise RuntimeError("HTTP response headers are not ASCII") from error

    status_parts = header_lines[0].split(" ", 2)
    if (
        len(status_parts) < 2
        or status_parts[0] != "HTTP/1.1"
        or len(status_parts[1]) != 3
        or not status_parts[1].isdigit()
    ):
        raise RuntimeError("invalid HTTP status line")
    status = int(status_parts[1])
    if status < 100 or status > 599:
        raise RuntimeError("invalid HTTP status code")

    headers: dict[str, str] = {}
    for line in header_lines[1:]:
        if not line or line[0] in " \t" or ":" not in line:
            raise RuntimeError("invalid HTTP response header")
        name, value = line.split(":", 1)
        if not name or any(character not in HTTP_TOKEN_CHARACTERS for character in name):
            raise RuntimeError("invalid HTTP response header name")
        if any(ord(character) < 32 and character != "\t" or ord(character) == 127 for character in value):
            raise RuntimeError("invalid HTTP response header value")
        normalized = name.lower()
        if normalized in headers:
            raise RuntimeError(f"duplicate HTTP response header: {normalized}")
        headers[normalized] = value.strip()

    declared_length: int | None = None
    if "content-length" in headers:
        raw_length = headers["content-length"]
        if not raw_length.isascii() or not raw_length.isdecimal():
            raise RuntimeError("invalid HTTP Content-Length")
        declared_length = int(raw_length)
        if declared_length > max_body_bytes:
            raise RuntimeError("HTTP response body exceeds its allowed size")

    transfer_encoding = headers.get("transfer-encoding")
    if transfer_encoding is not None and declared_length is not None:
        raise RuntimeError("HTTP response contains both Transfer-Encoding and Content-Length")
    if transfer_encoding is not None and transfer_encoding.lower() != "chunked":
        raise RuntimeError("unsupported HTTP Transfer-Encoding")

    if transfer_encoding is not None:
        body = _receive_chunked(client, body_prefix, max_body_bytes)
    elif declared_length is not None:
        body = bytearray(body_prefix)
        if len(body) > declared_length:
            raise RuntimeError("HTTP response length does not match Content-Length")
        while len(body) < declared_length:
            chunk = client.recv(min(65_536, declared_length - len(body)))
            if not chunk:
                raise RuntimeError("HTTP response length does not match Content-Length")
            body.extend(chunk)
        body = bytes(body)
    else:
        body = bytearray(body_prefix)
        while True:
            if len(body) > max_body_bytes:
                raise RuntimeError("HTTP response body exceeds its allowed size")
            chunk = client.recv(min(65_536, max_body_bytes + 1 - len(body)))
            if not chunk:
                break
            body.extend(chunk)
        body = bytes(body)

    return HttpResponse(status=status, headers=headers, body=body)


def request(
    path: Path,
    payload: bytes = SNAPSHOT_REQUEST,
    *,
    timeout: float = 3.0,
    max_body_bytes: int = 16 * 1024 * 1024,
) -> HttpResponse:
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as client:
        client.settimeout(timeout)
        client.connect(str(path))
        client.sendall(payload)
        return receive_response(client, max_body_bytes)


def json_body(response: HttpResponse) -> dict[str, object]:
    media_type = response.headers.get("content-type", "").split(";", 1)[0].strip().lower()
    if media_type != "application/json":
        raise RuntimeError("HTTP response has a non-JSON Content-Type")
    try:
        decoded = json.loads(response.body)
    except (UnicodeDecodeError, json.JSONDecodeError) as error:
        raise RuntimeError("HTTP response contains invalid JSON") from error
    if not isinstance(decoded, dict):
        raise RuntimeError("HTTP JSON response is not an object")
    return decoded


def _is_u64(value: object, *, positive: bool = False) -> bool:
    return type(value) is int and (value > 0 if positive else value >= 0) and value <= U64_MAX


def validate_snapshot(snapshot: dict[str, object]) -> None:
    if snapshot.get("schema_version") != 1 or type(snapshot.get("schema_version")) is not int:
        raise RuntimeError("unsupported or missing snapshot schema_version")
    if not isinstance(snapshot.get("node_id"), str) or not snapshot["node_id"]:
        raise RuntimeError("snapshot has an invalid node_id")
    runtime_id = snapshot.get("runtime_id")
    if (
        not isinstance(runtime_id, str)
        or len(runtime_id) != 32
        or any(character not in "0123456789abcdef" for character in runtime_id)
    ):
        raise RuntimeError("snapshot has an invalid runtime_id")
    if not _is_u64(snapshot.get("started_at_unix_ms"), positive=True):
        raise RuntimeError("snapshot has an invalid started_at_unix_ms")
    if not _is_u64(snapshot.get("sequence"), positive=True):
        raise RuntimeError("snapshot has an invalid sequence")

    health = snapshot.get("health")
    if not isinstance(health, dict) or any(
        type(health.get(field)) is not bool for field in ("counter_overflow", "sequence_overflow")
    ):
        raise RuntimeError("snapshot has an invalid health object")

    servers = snapshot.get("servers")
    if not isinstance(servers, list):
        raise RuntimeError("snapshot has an invalid servers array")
    for server in servers:
        if not isinstance(server, dict):
            raise RuntimeError("snapshot contains an invalid server object")
        if not isinstance(server.get("server_id"), str) or not server["server_id"]:
            raise RuntimeError("snapshot contains an invalid server_id")
        if not isinstance(server.get("listen"), str) or not server["listen"]:
            raise RuntimeError("snapshot contains an invalid listen address")
        if not _is_u64(server.get("generation"), positive=True) or type(server.get("active")) is not bool:
            raise RuntimeError("snapshot contains invalid server lifecycle fields")
        users = server.get("users")
        if not isinstance(users, list):
            raise RuntimeError("snapshot contains an invalid users array")
        for user in users:
            if not isinstance(user, dict):
                raise RuntimeError("snapshot contains an invalid user object")
            if user.get("identity_kind") != "user":
                raise RuntimeError("snapshot contains an unsupported identity_kind")
            if not isinstance(user.get("name"), str) or not user["name"]:
                raise RuntimeError("snapshot contains an invalid identity name")
            if not _is_u64(user.get("generation"), positive=True) or type(user.get("active")) is not bool:
                raise RuntimeError("snapshot contains invalid identity lifecycle fields")
            if any(not _is_u64(user.get(field)) for field in COUNTER_FIELDS):
                raise RuntimeError("snapshot contains an invalid traffic counter")
