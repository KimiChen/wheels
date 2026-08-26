#!/usr/bin/env python3
"""End-to-end EIH user-statistics test using real ssserver and sslocal."""

from __future__ import annotations

import argparse
import base64
import concurrent.futures
import json
import os
import re
import secrets
import socket
import socketserver
import stat
import subprocess
import tempfile
import threading
import time
from pathlib import Path

from http_unix import (
    HEALTH_REQUEST,
    SNAPSHOT_REQUEST,
    HttpResponse,
    build_request,
    json_body,
    receive_response,
    request as http_request,
    validate_snapshot,
)

METHOD = "2022-blake3-aes-128-gcm"


def random_key() -> str:
    return base64.b64encode(secrets.token_bytes(16)).decode("ascii")


def reserve_port() -> int:
    for _ in range(100):
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as tcp, socket.socket(
            socket.AF_INET, socket.SOCK_DGRAM
        ) as udp:
            tcp.bind(("127.0.0.1", 0))
            port = int(tcp.getsockname()[1])
            try:
                udp.bind(("127.0.0.1", port))
            except OSError:
                continue
            return port
    raise RuntimeError("could not reserve a TCP/UDP loopback port")


class EchoHandler(socketserver.BaseRequestHandler):
    def handle(self) -> None:
        while True:
            data = self.request.recv(64 * 1024)
            if not data:
                return
            self.request.sendall(data)


class ThreadedTcpServer(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True


class EchoServices:
    def __init__(self) -> None:
        for _ in range(100):
            tcp = ThreadedTcpServer(("127.0.0.1", 0), EchoHandler)
            port = int(tcp.server_address[1])
            udp = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
            try:
                udp.bind(("127.0.0.1", port))
            except OSError:
                udp.close()
                tcp.server_close()
                continue
            self.tcp = tcp
            self.port = port
            self.udp = udp
            break
        else:
            raise RuntimeError("could not bind TCP/UDP echo services to one port")
        self.udp.settimeout(0.2)
        self.stop_event = threading.Event()
        self.tcp_thread = threading.Thread(target=self.tcp.serve_forever, daemon=True)
        self.udp_thread = threading.Thread(target=self._serve_udp, daemon=True)

    def _serve_udp(self) -> None:
        while not self.stop_event.is_set():
            try:
                data, peer = self.udp.recvfrom(65_535)
            except socket.timeout:
                continue
            except OSError:
                return
            self.udp.sendto(data, peer)

    def __enter__(self) -> "EchoServices":
        self.tcp_thread.start()
        self.udp_thread.start()
        return self

    def __exit__(self, *_: object) -> None:
        self.stop_event.set()
        self.tcp.shutdown()
        self.tcp.server_close()
        self.udp.close()
        self.tcp_thread.join(timeout=2)
        self.udp_thread.join(timeout=2)


class ChildProcess:
    def __init__(self, command: list[str], log_path: Path, redactions: list[str] | None = None) -> None:
        self.log_path = log_path
        self.redactions = redactions or []
        self.log_file = log_path.open("wb")
        self.process = subprocess.Popen(
            command,
            stdin=subprocess.DEVNULL,
            stdout=self.log_file,
            stderr=subprocess.STDOUT,
        )

    def assert_running(self, label: str) -> None:
        return_code = self.process.poll()
        if return_code is not None:
            log_text = self.log_path.read_text(encoding="utf-8", errors="replace")
            for value in self.redactions:
                log_text = log_text.replace(value, "<redacted>")
            log_text = re.sub(r"(?<![A-Za-z0-9+/])[A-Za-z0-9+/]{20,}={0,2}(?![A-Za-z0-9+/])", "<redacted>", log_text)
            log_text = re.sub(r"(?<![0-9A-Fa-f])[0-9A-Fa-f]{32,}(?![0-9A-Fa-f])", "<redacted>", log_text)
            raise AssertionError(
                f"{label} exited unexpectedly with status {return_code}; "
                f"sanitized log tail:\n{log_text[-4000:]}"
            )

    def stop(self) -> None:
        if self.process.poll() is None:
            self.process.terminate()
            try:
                self.process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait(timeout=5)
        self.log_file.close()


def wait_for_tcp(port: int, processes: list[tuple[str, ChildProcess]], timeout: float = 10.0) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        for label, process in processes:
            process.assert_running(label)
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as probe:
            probe.settimeout(0.1)
            if probe.connect_ex(("127.0.0.1", port)) == 0:
                return
        time.sleep(0.05)
    raise AssertionError(f"TCP port {port} did not become ready")


def wait_for_path(path: Path, process: ChildProcess, timeout: float = 10.0) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        process.assert_running("ssserver")
        if path.exists():
            return
        time.sleep(0.05)
    raise AssertionError("user-statistics socket did not become ready")


def assert_common_headers(response: HttpResponse) -> None:
    assert response.headers.get("connection", "").lower() == "close"
    assert response.headers.get("cache-control", "").lower() == "no-store"
    assert "content-length" in response.headers
    assert int(response.headers["content-length"]) == len(response.body)
    assert response.body.endswith(b"\n")


def request_exporter(path: Path) -> dict[str, object]:
    response = http_request(path, SNAPSHOT_REQUEST, max_body_bytes=1_048_576)
    assert response.status == 200
    assert_common_headers(response)
    snapshot = json_body(response)
    validate_snapshot(snapshot)
    return snapshot


def assert_error_response(response: HttpResponse, status: int, code: str) -> None:
    assert response.status == status
    assert_common_headers(response)
    assert json_body(response) == {"schema_version": 1, "error": {"code": code}}


def assert_incomplete_request_times_out(path: Path) -> None:
    started = time.monotonic()
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as client:
        client.settimeout(3)
        client.connect(str(path))
        client.sendall(b"GET /v1/snapshot HTTP/1.1\r\nHost: local")
        try:
            response = receive_response(client, 4096)
        except RuntimeError as error:
            assert "ended before" in str(error)
        else:
            assert_error_response(response, 408, "read_timeout")
    elapsed = time.monotonic() - started
    assert 0.5 <= elapsed < 3, f"incomplete request closed outside timeout window: {elapsed:.3f}s"


def assert_protocol_rejected(
    path: Path,
    payload: bytes,
    status: int,
    code: str,
) -> None:
    response = http_request(
        path,
        payload,
        max_body_bytes=4096,
    )
    assert_error_response(response, status, code)


def transfer_tcp(port: int, payload: bytes) -> None:
    with socket.create_connection(("127.0.0.1", port), timeout=5) as stream:
        stream.settimeout(5)
        stream.sendall(payload)
        received = bytearray()
        while len(received) < len(payload):
            chunk = stream.recv(len(payload) - len(received))
            if not chunk:
                break
            received.extend(chunk)
    assert bytes(received) == payload


def transfer_udp(port: int, payloads: list[bytes]) -> None:
    with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as client:
        client.settimeout(5)
        for payload in payloads:
            client.sendto(payload, ("127.0.0.1", port))
            response, _ = client.recvfrom(65_535)
            assert response == payload


def write_json(path: Path, value: object) -> None:
    path.write_text(json.dumps(value, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    path.chmod(0o600)


def local_config(port: int, server_port: int, echo_port: int, password: str) -> dict[str, object]:
    return {
        "locals": [
            {
                "local_address": "127.0.0.1",
                "local_port": port,
                "protocol": "tunnel",
                "forward_address": "127.0.0.1",
                "forward_port": echo_port,
                "mode": "tcp_and_udp",
            }
        ],
        "server": "127.0.0.1",
        "server_port": server_port,
        "method": METHOD,
        "password": password,
        "mode": "tcp_and_udp",
    }


def find_users(snapshot: dict[str, object]) -> dict[str, dict[str, object]]:
    servers = snapshot["servers"]
    assert isinstance(servers, list) and len(servers) == 1
    users = servers[0]["users"]
    assert isinstance(users, list)
    return {str(user["name"]): user for user in users}


def expected_counters(tcp_size: int, udp_sizes: list[int]) -> dict[str, int]:
    udp_total = sum(udp_sizes)
    return {
        "tcp_uplink_bytes": tcp_size,
        "tcp_downlink_bytes": tcp_size,
        "udp_uplink_bytes": udp_total,
        "udp_downlink_bytes": udp_total,
    }


def wait_for_counters(
    socket_path: Path,
    expected: dict[str, dict[str, int]],
    timeout: float = 5.0,
) -> dict[str, object]:
    deadline = time.monotonic() + timeout
    snapshot: dict[str, object] = {}
    while time.monotonic() < deadline:
        snapshot = request_exporter(socket_path)
        users = find_users(snapshot)
        if all(
            all(int(users[name][field]) == value for field, value in counters.items())
            for name, counters in expected.items()
        ):
            return snapshot
        time.sleep(0.05)
    raise AssertionError("counters did not settle to expected values before timeout")


def build(source: Path, target: Path) -> tuple[Path, Path]:
    environment = os.environ.copy()
    environment["CARGO_TARGET_DIR"] = str(target)
    subprocess.run(
        [
            "cargo",
            "build",
            "--manifest-path",
            str(source / "Cargo.toml"),
            "--locked",
            "--features",
            "user-stats",
            "--bin",
            "ssserver",
            "--bin",
            "sslocal",
        ],
        check=True,
        env=environment,
    )
    return target / "debug" / "ssserver", target / "debug" / "sslocal"


def run(source: Path) -> None:
    target = Path(os.environ.get("CARGO_TARGET_DIR", source / "target"))
    ssserver, sslocal = build(source, target)

    with tempfile.TemporaryDirectory(prefix="ssrp-it-") as temp_name, EchoServices() as echo:
        temp = Path(temp_name).resolve()
        temp.chmod(0o700)
        socket_path = temp / "user-stats.sock"
        server_port = reserve_port()
        exercised_users = ("user-a", "user-b")
        user_ports = {name: reserve_port() for name in exercised_users}
        ipsk = random_key()
        user_keys = {name: random_key() for name in exercised_users}
        user_keys.update({f"user-{number:03d}": random_key() for number in range(2, 100)})
        invalid_key = random_key()

        server_config = {
            "user_stats": {
                "node_id": "integration-node",
                "socket_path": str(socket_path),
                "socket_mode": "0600",
                "read_timeout_ms": 1000,
                "write_timeout_ms": 1000,
                "max_request_bytes": 1024,
                "max_response_bytes": 1_048_576,
                "max_identities": 100,
                "max_concurrent_clients": 4,
            },
            "servers": [
                {
                    "id": "integration-entry",
                    "server": "127.0.0.1",
                    "server_port": server_port,
                    "method": METHOD,
                    "password": ipsk,
                    "mode": "tcp_and_udp",
                    "users": [
                        {"name": name, "password": key}
                        for name, key in sorted(user_keys.items())
                    ],
                }
            ],
        }
        server_config_path = temp / "server.json"
        write_json(server_config_path, server_config)

        local_paths: dict[str, Path] = {}
        for name, port in user_ports.items():
            path = temp / f"{name}.json"
            write_json(path, local_config(port, server_port, echo.port, f"{ipsk}:{user_keys[name]}"))
            local_paths[name] = path

        processes: list[ChildProcess] = []
        try:
            all_secrets = [ipsk, *user_keys.values(), invalid_key]
            server = ChildProcess(
                [str(ssserver), "-vvv", "-c", str(server_config_path)],
                temp / "server.log",
                all_secrets,
            )
            processes.append(server)
            wait_for_path(socket_path, server)
            wait_for_tcp(server_port, [("ssserver", server)])
            assert stat.S_IMODE(socket_path.stat().st_mode) == 0o600

            first_snapshot = request_exporter(socket_path)
            first_sequence = int(first_snapshot["sequence"])
            health_response = http_request(socket_path, HEALTH_REQUEST, max_body_bytes=4096)
            assert health_response.status == 200
            assert_common_headers(health_response)
            assert json_body(health_response) == {"schema_version": 1, "status": "ok"}
            after_health = request_exporter(socket_path)
            assert int(after_health["sequence"]) == first_sequence + 1

            not_found = http_request(socket_path, build_request(target="/missing"), max_body_bytes=4096)
            assert_error_response(not_found, 404, "not_found")

            method_not_allowed = http_request(
                socket_path,
                build_request(method="POST"),
                max_body_bytes=4096,
            )
            assert_error_response(method_not_allowed, 405, "method_not_allowed")
            assert method_not_allowed.headers.get("allow") == "GET"

            query_rejected = http_request(
                socket_path,
                build_request(target="/v1/snapshot?detail=1"),
                max_body_bytes=4096,
            )
            assert_error_response(query_rejected, 400, "invalid_request")

            declared_body_rejected = http_request(
                socket_path,
                build_request(body=b"{}"),
                max_body_bytes=4096,
            )
            assert_error_response(declared_body_rejected, 400, "invalid_request")

            chunked_body_request = (
                b"GET /v1/snapshot HTTP/1.1\r\n"
                b"Host: localhost\r\n"
                b"Transfer-Encoding: chunked\r\n"
                b"Connection: close\r\n\r\n"
                b"1\r\nx\r\n0\r\n\r\n"
            )
            framed_body_rejected = http_request(
                socket_path,
                chunked_body_request,
                max_body_bytes=4096,
            )
            assert_error_response(framed_body_rejected, 400, "invalid_request")

            oversized_metadata = http_request(
                socket_path,
                build_request(extra_headers=(("X-Oversized", "x" * 1_100),)),
                max_body_bytes=4096,
            )
            assert_error_response(oversized_metadata, 413, "request_too_large")

            assert_protocol_rejected(
                socket_path,
                b"GET /v1/snapshot HTTP/1.1\r\nConnection: close\r\n\r\n",
                400,
                "invalid_request",
            )
            assert_protocol_rejected(
                socket_path,
                b"GET /v1/snapshot HTTP/1.1\r\nHost: /\r\nConnection: close\r\n\r\n",
                400,
                "invalid_request",
            )
            assert_protocol_rejected(
                socket_path,
                (
                    b"GET /v1/snapshot HTTP/1.1\r\n"
                    b"Host: localhost\r\nHost: duplicate\r\nConnection: close\r\n\r\n"
                ),
                400,
                "invalid_request",
            )
            assert_protocol_rejected(
                socket_path,
                b"GET /v1/snapshot HTTP/1.1\r\nHost: localhost\r\nContent-Length: nope\r\n\r\n",
                400,
                "invalid_request",
            )
            assert_protocol_rejected(
                socket_path,
                b"GET /v1/snapshot HTTP/1.1\r\nHost: localhost\r\nTransfer-Encoding: gzip\r\n\r\n",
                400,
                "invalid_request",
            )
            assert_protocol_rejected(
                socket_path,
                build_request(target="http://localhost/v1/snapshot"),
                400,
                "invalid_request",
            )
            assert_protocol_rejected(
                socket_path,
                b"GET /v1/snapshot HTTP/1.0\r\nHost: localhost\r\nConnection: close\r\n\r\n",
                505,
                "http_version_not_supported",
            )
            assert_protocol_rejected(
                socket_path,
                b"GET /v1/snapshot HTTP/1.2\r\nHost: localhost\r\nConnection: close\r\n\r\n",
                505,
                "http_version_not_supported",
            )

            assert_incomplete_request_times_out(socket_path)
            server.assert_running("ssserver after HTTP rejection tests")
            after_rejections = request_exporter(socket_path)
            assert int(after_rejections["sequence"]) == int(after_health["sequence"]) + 1

            locals_by_name: dict[str, ChildProcess] = {}
            for name in sorted(user_ports):
                process = ChildProcess(
                    [str(sslocal), "-vvv", "-c", str(local_paths[name])],
                    temp / f"{name}.log",
                    [ipsk, user_keys[name]],
                )
                processes.append(process)
                locals_by_name[name] = process
                wait_for_tcp(user_ports[name], [("ssserver", server), (name, process)])

            time.sleep(0.25)
            tcp_payloads = {
                "user-a": secrets.token_bytes(12_345),
                "user-b": secrets.token_bytes(23_456),
            }
            udp_payloads = {
                "user-a": [secrets.token_bytes(987), secrets.token_bytes(1_021)],
                "user-b": [secrets.token_bytes(701), secrets.token_bytes(1_111), secrets.token_bytes(333)],
            }
            def exercise(name: str) -> None:
                transfer_tcp(user_ports[name], tcp_payloads[name])
                transfer_udp(user_ports[name], udp_payloads[name])

            with concurrent.futures.ThreadPoolExecutor(max_workers=2) as executor:
                futures = [executor.submit(exercise, name) for name in sorted(user_ports)]
                for future in futures:
                    future.result()

            expected = {
                name: expected_counters(len(tcp_payloads[name]), [len(item) for item in udp_payloads[name]])
                for name in user_ports
            }
            snapshot = wait_for_counters(socket_path, expected)
            assert snapshot["schema_version"] == 1
            assert snapshot["node_id"] == "integration-node"
            assert snapshot["health"] == {"counter_overflow": False, "sequence_overflow": False}
            users = find_users(snapshot)
            assert len(users) == 100
            assert list(users) == sorted(users)
            assert all(user["identity_kind"] == "user" for user in users.values())
            assert all(user["generation"] == 1 and user["active"] is True for user in users.values())

            serialized = json.dumps(snapshot, sort_keys=True)
            for secret_value in [ipsk, *user_keys.values(), invalid_key]:
                assert secret_value not in serialized
            assert str(echo.port) not in serialized

            before_invalid = wait_for_counters(socket_path, expected)
            invalid_port = reserve_port()
            invalid_path = temp / "invalid.json"
            write_json(invalid_path, local_config(invalid_port, server_port, echo.port, f"{ipsk}:{invalid_key}"))
            invalid_local = ChildProcess(
                [str(sslocal), "-vvv", "-c", str(invalid_path)],
                temp / "invalid.log",
                [ipsk, invalid_key],
            )
            processes.append(invalid_local)
            wait_for_tcp(invalid_port, [("ssserver", server), ("invalid sslocal", invalid_local)])
            try:
                transfer_tcp(invalid_port, secrets.token_bytes(257))
                raise AssertionError("unknown EIH user unexpectedly completed a TCP transfer")
            except (OSError, AssertionError) as error:
                if isinstance(error, AssertionError) and str(error).startswith("unknown EIH"):
                    raise
            try:
                transfer_udp(invalid_port, [secrets.token_bytes(389)])
                raise AssertionError("unknown EIH user unexpectedly completed a UDP transfer")
            except (OSError, AssertionError) as error:
                if isinstance(error, AssertionError) and str(error).startswith("unknown EIH"):
                    raise
            time.sleep(0.25)
            after_invalid = request_exporter(socket_path)
            assert {
                name: {field: int(find_users(after_invalid)[name][field]) for field in counters}
                for name, counters in expected.items()
            } == expected
            assert int(after_invalid["sequence"]) > int(before_invalid["sequence"])

            first_runtime_id = str(after_invalid["runtime_id"])
            for process in list(processes[1:]):
                process.stop()
            processes = [server]
            server.stop()
            processes = []

            deadline = time.monotonic() + 5
            while socket_path.exists() and time.monotonic() < deadline:
                time.sleep(0.05)

            restarted = ChildProcess(
                [str(ssserver), "-vvv", "-c", str(server_config_path)],
                temp / "server-restarted.log",
                all_secrets,
            )
            processes.append(restarted)
            wait_for_path(socket_path, restarted)
            restarted_snapshot = request_exporter(socket_path)
            assert restarted_snapshot["runtime_id"] != first_runtime_id
            assert int(restarted_snapshot["sequence"]) == 1
            for user in find_users(restarted_snapshot).values():
                for field in expected_counters(0, []):
                    assert int(user[field]) == 0
        finally:
            for process in reversed(processes):
                process.stop()

        for log_path in temp.glob("*.log"):
            log_text = log_path.read_text(encoding="utf-8", errors="replace")
            for secret_value in [ipsk, *user_keys.values(), invalid_key]:
                assert secret_value not in log_text, f"credential leaked into {log_path.name}"


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--source", required=True, type=Path)
    arguments = parser.parse_args()
    source = arguments.source.resolve()
    if not (source / "Cargo.toml").is_file():
        parser.error("--source must point to a prepared shadowsocks-rust tree")
    run(source)
    print("user statistics integration test passed")


if __name__ == "__main__":
    main()
