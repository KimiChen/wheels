#!/usr/bin/env python3
"""Strict HTTP/1.1-over-Unix client for the v1 user-statistics snapshot."""

from __future__ import annotations

import argparse
import json
import math
import socket
import sys
import time


REQUEST = (
    b"GET /v1/snapshot HTTP/1.1\r\n"
    b"Host: localhost\r\n"
    b"Accept: application/json\r\n"
    b"Connection: close\r\n"
    b"\r\n"
)
MAX_HEADER_BYTES = 64 * 1024
HTTP_TOKEN_CHARACTERS = frozenset("!#$%&'*+-.^_`|~0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz")
U64_MAX = (1 << 64) - 1
COUNTER_FIELDS = (
    "tcp_uplink_bytes",
    "tcp_downlink_bytes",
    "udp_uplink_bytes",
    "udp_downlink_bytes",
)


def _remaining_timeout(deadline: float) -> float:
    remaining = deadline - time.monotonic()
    if remaining <= 0:
        raise RuntimeError("HTTP request deadline exceeded")
    return remaining


def _recv(client: socket.socket, size: int, deadline: float) -> bytes:
    client.settimeout(_remaining_timeout(deadline))
    try:
        return client.recv(size)
    except TimeoutError as error:
        raise RuntimeError("HTTP request deadline exceeded") from error


def _receive_more(
    client: socket.socket,
    payload: bytearray,
    max_wire_bytes: int,
    deadline: float,
) -> None:
    if len(payload) > max_wire_bytes:
        raise RuntimeError("HTTP response framing exceeds its allowed size")
    chunk = _recv(client, min(65_536, max_wire_bytes + 1 - len(payload)), deadline)
    if not chunk:
        raise RuntimeError("HTTP response ended before its body was complete")
    payload.extend(chunk)
    if len(payload) > max_wire_bytes:
        raise RuntimeError("HTTP response framing exceeds its allowed size")


def _receive_chunked(client: socket.socket, prefix: bytes, max_bytes: int, deadline: float) -> bytes:
    decoded = bytearray()
    payload = bytearray(prefix)
    cursor = 0
    max_wire_bytes = max_bytes + MAX_HEADER_BYTES
    if len(payload) > max_wire_bytes:
        raise RuntimeError("HTTP response framing exceeds its allowed size")
    while True:
        line_end = payload.find(b"\r\n", cursor)
        while line_end < 0:
            _receive_more(client, payload, max_wire_bytes, deadline)
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
                    _receive_more(client, payload, max_wire_bytes, deadline)
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
            _receive_more(client, payload, max_wire_bytes, deadline)
        if payload[chunk_end:chunk_end + 2] != b"\r\n":
            raise RuntimeError("invalid HTTP chunk terminator")
        decoded.extend(payload[cursor:chunk_end])
        if len(decoded) > max_bytes:
            raise RuntimeError(f"response exceeds {max_bytes} bytes")
        cursor = chunk_end + 2


def _receive_http_response(
    client: socket.socket,
    max_bytes: int,
    deadline: float,
) -> tuple[int, dict[str, str], bytes]:
    received = bytearray()
    while b"\r\n\r\n" not in received:
        chunk = _recv(client, min(16_384, MAX_HEADER_BYTES + 1 - len(received)), deadline)
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
        raise RuntimeError("invalid HTTP/1.1 status line")
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
        if declared_length > max_bytes:
            raise RuntimeError(f"response exceeds {max_bytes} bytes")

    transfer_encoding = headers.get("transfer-encoding")
    if transfer_encoding is not None and declared_length is not None:
        raise RuntimeError("HTTP response contains both Transfer-Encoding and Content-Length")
    if transfer_encoding is not None and transfer_encoding.lower() != "chunked":
        raise RuntimeError("unsupported HTTP Transfer-Encoding")

    if transfer_encoding is not None:
        body = _receive_chunked(client, body_prefix, max_bytes, deadline)
    elif declared_length is not None:
        body = bytearray(body_prefix)
        if len(body) > declared_length:
            raise RuntimeError("HTTP response length does not match Content-Length")
        while len(body) < declared_length:
            chunk = _recv(client, min(65_536, declared_length - len(body)), deadline)
            if not chunk:
                raise RuntimeError("HTTP response length does not match Content-Length")
            body.extend(chunk)
        body = bytes(body)
    else:
        body = bytearray(body_prefix)
        while True:
            if len(body) > max_bytes:
                raise RuntimeError(f"response exceeds {max_bytes} bytes")
            chunk = _recv(client, min(65_536, max_bytes + 1 - len(body)), deadline)
            if not chunk:
                break
            body.extend(chunk)
        body = bytes(body)

    return status, headers, body


def _is_u64(value: object, *, positive: bool = False) -> bool:
    return type(value) is int and (value > 0 if positive else value >= 0) and value <= U64_MAX


def _validate_snapshot(snapshot: dict) -> None:
    if snapshot.get("schema_version") != 1 or type(snapshot.get("schema_version")) is not int:
        raise RuntimeError("unsupported or missing schema_version")
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


def fetch(socket_path: str, timeout: float, max_bytes: int) -> dict:
    if not math.isfinite(timeout) or timeout <= 0:
        raise RuntimeError("timeout must be finite and positive")
    if max_bytes <= 0:
        raise RuntimeError("max-bytes must be positive")
    deadline = time.monotonic() + timeout
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as client:
        client.settimeout(_remaining_timeout(deadline))
        try:
            client.connect(socket_path)
            client.settimeout(_remaining_timeout(deadline))
            client.sendall(REQUEST)
        except TimeoutError as error:
            raise RuntimeError("HTTP request deadline exceeded") from error
        status, headers, payload = _receive_http_response(client, max_bytes, deadline)

    if status != 200:
        raise RuntimeError(f"exporter returned HTTP {status}")
    if headers.get("connection", "").lower() != "close":
        raise RuntimeError("exporter omitted Connection: close")
    if headers.get("cache-control", "").lower() != "no-store":
        raise RuntimeError("exporter omitted Cache-Control: no-store")
    media_type = headers.get("content-type", "").split(";", 1)[0].strip().lower()
    if media_type != "application/json":
        raise RuntimeError("exporter returned a non-JSON Content-Type")
    if not payload.endswith(b"\n"):
        raise RuntimeError("exporter response is not newline terminated")
    try:
        response = json.loads(payload)
    except (UnicodeDecodeError, json.JSONDecodeError) as error:
        raise RuntimeError(f"invalid JSON response: {error}") from error

    if not isinstance(response, dict):
        raise RuntimeError("HTTP JSON response is not an object")
    if "error" in response:
        raise RuntimeError("exporter returned an error response")
    _validate_snapshot(response)
    return response


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("socket_path")
    parser.add_argument("--timeout", type=float, default=2.0)
    parser.add_argument("--max-bytes", type=int, default=16 * 1024 * 1024)
    parser.add_argument("--compact", action="store_true")
    parser.add_argument("--require-healthy", action="store_true")
    args = parser.parse_args()

    try:
        response = fetch(args.socket_path, args.timeout, args.max_bytes)
        if args.require_healthy:
            health = response.get("health")
            if health != {"counter_overflow": False, "sequence_overflow": False}:
                raise RuntimeError("snapshot health is not acceptable")
    except (OSError, RuntimeError) as error:
        print(f"user-stats-client: {error}", file=sys.stderr)
        return 1

    if args.compact:
        json.dump(response, sys.stdout, ensure_ascii=False, separators=(",", ":"))
    else:
        json.dump(response, sys.stdout, ensure_ascii=False, indent=2, sort_keys=True)
    sys.stdout.write("\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
