#!/usr/bin/env python3
"""Benchmark release-build exporter snapshots at several identity counts."""

from __future__ import annotations

import argparse
import base64
import json
import os
import platform
import secrets
import socket
import statistics
import subprocess
import tempfile
import time
from pathlib import Path

from http_unix import json_body, request as http_request, validate_snapshot


def key() -> str:
    return base64.b64encode(secrets.token_bytes(16)).decode("ascii")


def free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as probe:
        probe.bind(("127.0.0.1", 0))
        return int(probe.getsockname()[1])


def query(path: Path) -> bytes:
    response = http_request(path, max_body_bytes=16 * 1024 * 1024)
    if response.status != 200:
        raise RuntimeError(f"snapshot exporter returned HTTP {response.status}")
    if response.headers.get("cache-control", "").lower() != "no-store":
        raise RuntimeError("snapshot exporter omitted Cache-Control: no-store")
    if response.headers.get("connection", "").lower() != "close":
        raise RuntimeError("snapshot exporter omitted Connection: close")
    if not response.body.endswith(b"\n"):
        raise RuntimeError("snapshot exporter omitted the JSON newline terminator")
    validate_snapshot(json_body(response))
    return response.body


def wait_for_socket(path: Path, process: subprocess.Popen[bytes]) -> None:
    deadline = time.monotonic() + 15
    while time.monotonic() < deadline:
        if process.poll() is not None:
            raise RuntimeError(f"ssserver exited with status {process.returncode}")
        if path.exists():
            return
        time.sleep(0.05)
    raise TimeoutError("exporter socket did not become ready")


def rss_kib(process_id: int) -> int | None:
    try:
        value = subprocess.check_output(
            ["ps", "-o", "rss=", "-p", str(process_id)],
            text=True,
            stderr=subprocess.DEVNULL,
        ).strip()
        return int(value) if value else None
    except (OSError, subprocess.CalledProcessError, ValueError):
        return None


def percentile(values: list[float], fraction: float) -> float:
    ordered = sorted(values)
    index = min(len(ordered) - 1, max(0, int((len(ordered) - 1) * fraction)))
    return ordered[index]


def run_case(binary: Path, identity_count: int, samples: int) -> dict[str, object]:
    with tempfile.TemporaryDirectory(prefix=f"ssrp-bench-{identity_count}-") as temp_name:
        temp = Path(temp_name).resolve()
        temp.chmod(0o700)
        socket_path = temp / "stats.sock"
        config_path = temp / "server.json"
        config = {
            "user_stats": {
                "node_id": "benchmark-node",
                "socket_path": str(socket_path),
                "socket_mode": "0600",
                "read_timeout_ms": 1000,
                "write_timeout_ms": 1000,
                "max_request_bytes": 1024,
                "max_response_bytes": 16 * 1024 * 1024,
                "max_identities": identity_count,
                "max_concurrent_clients": 4,
            },
            "servers": [
                {
                    "id": "benchmark-entry",
                    "server": "127.0.0.1",
                    "server_port": free_port(),
                    "method": "2022-blake3-aes-128-gcm",
                    "password": key(),
                    "mode": "tcp_and_udp",
                    "users": [
                        {"name": f"user-{number:04d}", "password": key()}
                        for number in range(identity_count)
                    ],
                }
            ],
        }
        config_path.write_text(json.dumps(config), encoding="utf-8")
        config_path.chmod(0o600)
        with (temp / "server.log").open("wb") as log:
            process = subprocess.Popen(
                [str(binary), "-c", str(config_path)],
                stdin=subprocess.DEVNULL,
                stdout=log,
                stderr=subprocess.STDOUT,
            )
            try:
                wait_for_socket(socket_path, process)
                for _ in range(3):
                    query(socket_path)
                latencies_ms: list[float] = []
                response = b""
                for _ in range(samples):
                    started = time.perf_counter_ns()
                    response = query(socket_path)
                    latencies_ms.append((time.perf_counter_ns() - started) / 1_000_000)
                snapshot = json.loads(response)
                assert len(snapshot["servers"][0]["users"]) == identity_count
                assert response.endswith(b"\n")
                assert max(latencies_ms) < 1000
                return {
                    "identities": identity_count,
                    "samples": samples,
                    "response_bytes": len(response),
                    "latency_ms": {
                        "min": round(min(latencies_ms), 3),
                        "median": round(statistics.median(latencies_ms), 3),
                        "p95": round(percentile(latencies_ms, 0.95), 3),
                        "max": round(max(latencies_ms), 3),
                    },
                    "rss_kib": rss_kib(process.pid),
                }
            finally:
                if process.poll() is None:
                    process.terminate()
                    try:
                        process.wait(timeout=5)
                    except subprocess.TimeoutExpired:
                        process.kill()
                        process.wait(timeout=5)


def command_version(command: list[str]) -> str | None:
    try:
        return subprocess.check_output(command, text=True, stderr=subprocess.STDOUT).strip()
    except (OSError, subprocess.CalledProcessError):
        return None


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--source", required=True, type=Path)
    parser.add_argument("--target", type=Path)
    parser.add_argument("--samples", type=int, default=25)
    parser.add_argument("--identities", type=int, nargs="+", default=[100, 500, 1000])
    parser.add_argument("--output", type=Path)
    arguments = parser.parse_args()
    if arguments.samples < 1 or any(count < 1 for count in arguments.identities):
        parser.error("samples and identity counts must be positive")

    source = arguments.source.resolve()
    target = (arguments.target or Path(os.environ.get("CARGO_TARGET_DIR", source / "target"))).resolve()
    environment = os.environ.copy()
    environment["CARGO_TARGET_DIR"] = str(target)
    subprocess.run(
        [
            "cargo",
            "build",
            "--manifest-path",
            str(source / "Cargo.toml"),
            "--locked",
            "--release",
            "--features",
            "user-stats",
            "--bin",
            "ssserver",
        ],
        check=True,
        env=environment,
    )
    binary = target / "release" / "ssserver"
    report = {
        "schema_version": 1,
        "environment": {
            "platform": platform.platform(),
            "machine": platform.machine(),
            "processor": platform.processor(),
            "python": platform.python_version(),
            "rustc": command_version(["rustc", "--version"]),
            "cargo": command_version(["cargo", "--version"]),
        },
        "profile": "release",
        "cases": [run_case(binary, count, arguments.samples) for count in arguments.identities],
    }
    encoded = json.dumps(report, ensure_ascii=False, indent=2) + "\n"
    if arguments.output:
        arguments.output.write_text(encoded, encoding="utf-8")
    else:
        print(encoded, end="")


if __name__ == "__main__":
    main()
