#!/usr/bin/env python3
"""Reproducible loopback data-path benchmark for shadowsocks-rust-plus.

The benchmark builds and compares three release configurations:

* the exact locked upstream revision without the ``user-stats`` feature;
* plus compiled with ``user-stats`` but without runtime statistics configuration;
* the same plus binaries with runtime statistics enabled.

No public network endpoint is used by the benchmark workload. All credentials are
created in memory for one invocation, written only to mode-0600 files below a
mode-0700 temporary directory, and never included in the JSON report or emitted
by this script. Child output is confined to mode-0600 temporary logs.
"""

from __future__ import annotations

import argparse
import base64
import concurrent.futures
import datetime as dt
import hashlib
import json
import math
import os
import platform
import secrets
import socket
import socketserver
import stat
import statistics
import subprocess
import tempfile
import threading
import time
from dataclasses import dataclass
from pathlib import Path
from typing import BinaryIO

from http_unix import json_body, request as http_request, validate_snapshot

LOCKED_UPSTREAM_COMMIT = "7ee1aa9223ed8f4d34734aac919036c8ad4502c2"
METHOD = "2022-blake3-aes-128-gcm"
MIB = 1024 * 1024
U64_MAX = (1 << 64) - 1
COUNTER_FIELDS = (
    "tcp_uplink_bytes",
    "tcp_downlink_bytes",
    "udp_uplink_bytes",
    "udp_downlink_bytes",
)


def random_key() -> str:
    """Return a runtime-only SIP022 128-bit key."""

    return base64.b64encode(secrets.token_bytes(16)).decode("ascii")


def secure_write_json(path: Path, value: object) -> None:
    """Create a JSON file without a world-readable creation window."""

    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    try:
        os.fchmod(descriptor, 0o600)
        with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
            json.dump(value, stream, ensure_ascii=False, separators=(",", ":"))
            stream.write("\n")
    except BaseException:
        try:
            os.close(descriptor)
        except OSError:
            pass
        raise


def secure_open_log(path: Path) -> BinaryIO:
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    os.fchmod(descriptor, 0o600)
    return os.fdopen(descriptor, "wb")


def secure_write_report(path: Path, encoded: str) -> None:
    flags = os.O_WRONLY | os.O_CREAT | os.O_TRUNC
    descriptor = os.open(path, flags, 0o600)
    try:
        os.fchmod(descriptor, 0o600)
        with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
            stream.write(encoded)
    except BaseException:
        try:
            os.close(descriptor)
        except OSError:
            pass
        raise


def reserve_port() -> int:
    for _ in range(32):
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as tcp_probe:
            tcp_probe.bind(("127.0.0.1", 0))
            port = int(tcp_probe.getsockname()[1])
            with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as udp_probe:
                try:
                    udp_probe.bind(("127.0.0.1", port))
                except OSError:
                    continue
                return port
    raise RuntimeError("could not reserve a port available for both TCP and UDP")


class EchoHandler(socketserver.BaseRequestHandler):
    def handle(self) -> None:
        while True:
            data = self.request.recv(256 * 1024)
            if not data:
                return
            self.request.sendall(data)


class ThreadedTcpServer(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True


class EchoServices:
    """TCP and UDP echo services sharing one loopback port."""

    def __init__(self) -> None:
        for _ in range(32):
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
            raise RuntimeError("could not bind TCP and UDP echo services to one port")
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

    def __enter__(self) -> EchoServices:
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
    def __init__(self, command: list[str], log_path: Path) -> None:
        self.log = secure_open_log(log_path)
        environment = os.environ.copy()
        # Upstream traces full configuration objects. Do not inherit a caller's
        # RUST_LOG=trace into a benchmark that uses ephemeral credentials.
        environment["RUST_LOG"] = "off"
        environment["RUST_BACKTRACE"] = "0"
        try:
            self.process = subprocess.Popen(
                command,
                stdin=subprocess.DEVNULL,
                stdout=self.log,
                stderr=subprocess.STDOUT,
                env=environment,
            )
        except BaseException:
            self.log.close()
            raise

    @property
    def pid(self) -> int:
        return self.process.pid

    def assert_running(self, label: str) -> None:
        status = self.process.poll()
        if status is not None:
            raise RuntimeError(f"{label} exited unexpectedly with status {status}")

    def stop(self) -> None:
        if self.process.poll() is None:
            self.process.terminate()
            try:
                self.process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait(timeout=5)
        self.log.close()


def wait_for_tcp(
    port: int,
    processes: list[tuple[str, ChildProcess]],
    timeout: float = 15.0,
) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        for label, process in processes:
            process.assert_running(label)
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as probe:
            probe.settimeout(0.1)
            if probe.connect_ex(("127.0.0.1", port)) == 0:
                return
        time.sleep(0.05)
    raise TimeoutError(f"TCP port {port} did not become ready")


def wait_for_socket(
    path: Path,
    process: ChildProcess,
    timeout: float = 15.0,
) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        process.assert_running("ssserver")
        if path.exists():
            path_stat = path.stat()
            if not stat.S_ISSOCK(path_stat.st_mode):
                raise RuntimeError("statistics path exists but is not a Unix socket")
            mode = stat.S_IMODE(path_stat.st_mode)
            if mode != 0o600:
                raise RuntimeError(f"statistics socket mode is {mode:04o}, expected 0600")
            return
        time.sleep(0.05)
    raise TimeoutError("statistics socket did not become ready")


def query_statistics(path: Path) -> dict[str, object]:
    response = http_request(path, max_body_bytes=MIB)
    if response.status != 200:
        raise RuntimeError(f"statistics exporter returned HTTP {response.status}")
    if response.headers.get("cache-control", "").lower() != "no-store":
        raise RuntimeError("statistics exporter omitted Cache-Control: no-store")
    if response.headers.get("connection", "").lower() != "close":
        raise RuntimeError("statistics exporter omitted Connection: close")
    if not response.body.endswith(b"\n"):
        raise RuntimeError("statistics exporter omitted the JSON newline terminator")
    snapshot = json_body(response)
    validate_snapshot(snapshot)
    return snapshot


def snapshot_user_counters(snapshot: dict[str, object]) -> dict[str, int]:
    servers = snapshot.get("servers")
    if not isinstance(servers, list) or len(servers) != 1:
        raise RuntimeError("statistics snapshot did not contain exactly one server")
    server = servers[0]
    if not isinstance(server, dict):
        raise RuntimeError("statistics snapshot server entry was not an object")
    users = server.get("users")
    if not isinstance(users, list) or len(users) != 1:
        raise RuntimeError("statistics snapshot did not contain exactly one user")
    user = users[0]
    if not isinstance(user, dict) or user.get("name") != "benchmark-user":
        raise RuntimeError("statistics snapshot did not contain the benchmark user")
    counters: dict[str, int] = {}
    for field in COUNTER_FIELDS:
        value = user.get(field)
        if not isinstance(value, int) or isinstance(value, bool) or value < 0:
            raise RuntimeError(f"statistics field {field} was not a non-negative integer")
        counters[field] = value
    return counters


def command_output(command: list[str]) -> str | None:
    try:
        return subprocess.check_output(
            command,
            text=True,
            stderr=subprocess.STDOUT,
        ).strip()
    except (OSError, subprocess.CalledProcessError):
        return None


def git_revision(source: Path) -> str | None:
    try:
        return subprocess.check_output(
            ["git", "-C", str(source), "rev-parse", "HEAD"],
            text=True,
            stderr=subprocess.DEVNULL,
        ).strip()
    except (OSError, subprocess.CalledProcessError):
        return None


def git_worktree_state(source: Path) -> str | None:
    if git_revision(source) is None:
        return None
    try:
        status = subprocess.check_output(
            ["git", "-C", str(source), "status", "--porcelain", "--untracked-files=all"],
            text=True,
            stderr=subprocess.DEVNULL,
        )
        return "modified" if status else "clean"
    except (OSError, subprocess.CalledProcessError):
        return None


def project_locked_commit() -> str:
    lock_path = Path(__file__).resolve().parents[1] / "upstream.lock"
    try:
        values = dict(
            line.split("=", 1)
            for line in lock_path.read_text(encoding="utf-8").splitlines()
            if line and not line.startswith("#") and "=" in line
        )
        commit = values["commit"]
    except (FileNotFoundError, OSError, KeyError, ValueError) as error:
        raise RuntimeError("cannot read the project's upstream.lock commit") from error
    if commit != LOCKED_UPSTREAM_COMMIT:
        raise RuntimeError(
            "benchmark lock constant disagrees with the project's upstream.lock"
        )
    return commit


def assert_locked_upstream(source: Path, expected: str) -> None:
    revision = git_revision(source)
    if revision is None:
        raise RuntimeError(
            "--upstream-source must be a Git checkout so its locked revision can be verified"
        )
    if revision != expected:
        raise RuntimeError(
            f"upstream revision is {revision}, expected locked revision {expected}"
        )
    status = subprocess.check_output(
        ["git", "-C", str(source), "status", "--porcelain", "--untracked-files=all"],
        text=True,
    )
    if status:
        raise RuntimeError("upstream checkout contains modifications or untracked files")


def assert_plus_base(source: Path, expected: str) -> dict[str, object]:
    revision = git_revision(source)
    if revision is None:
        raise RuntimeError(
            "--plus-source must be a Git checkout/worktree so its upstream base can be verified"
        )
    result = subprocess.run(
        ["git", "-C", str(source), "merge-base", "--is-ancestor", expected, "HEAD"],
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
        check=False,
    )
    if result.returncode != 0:
        raise RuntimeError(
            "plus HEAD is not verifiably derived from the project's locked upstream commit"
        )
    worktree_state = git_worktree_state(source)
    if worktree_state != "clean":
        raise RuntimeError("plus checkout contains modifications or untracked files")
    return {
        "method": "git_merge_base_is_ancestor",
        "base_commit": expected,
        "head_revision": revision,
        "worktree_state": worktree_state,
    }


def build_binaries(
    source: Path,
    target: Path,
    feature: str | None,
    offline: bool,
) -> tuple[Path, Path]:
    environment = os.environ.copy()
    environment["CARGO_TARGET_DIR"] = str(target)
    command = [
        "cargo",
        "build",
        "--manifest-path",
        str(source / "Cargo.toml"),
        "--locked",
        "--release",
        "--bin",
        "ssserver",
        "--bin",
        "sslocal",
    ]
    if feature is not None:
        command.extend(["--features", feature])
    if offline:
        command.append("--offline")
    subprocess.run(command, check=True, env=environment)
    server = target / "release" / "ssserver"
    local = target / "release" / "sslocal"
    if not server.is_file() or not local.is_file():
        raise RuntimeError("release build completed without ssserver/sslocal")
    return server, local


def file_sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        while chunk := stream.read(MIB):
            digest.update(chunk)
    return digest.hexdigest()


def build_artifact_report(binaries: tuple[Path, Path]) -> dict[str, object]:
    return {
        path.name: {"sha256": file_sha256(path), "bytes": path.stat().st_size}
        for path in binaries
    }


def system_memory_bytes() -> int | None:
    try:
        return int(os.sysconf("SC_PHYS_PAGES")) * int(os.sysconf("SC_PAGE_SIZE"))
    except (AttributeError, OSError, ValueError):
        return None


def cpu_model() -> str | None:
    reported = platform.processor().strip()
    if reported:
        return reported
    for command in (
        ["sysctl", "-n", "machdep.cpu.brand_string"],
        ["sysctl", "-n", "hw.model"],
    ):
        value = command_output(command)
        if value:
            return value
    try:
        for line in Path("/proc/cpuinfo").read_text(encoding="utf-8").splitlines():
            if line.lower().startswith(("model name", "hardware")):
                return line.split(":", 1)[-1].strip() or None
    except (FileNotFoundError, OSError):
        pass
    return None


def process_cpu_seconds(pid: int) -> float | None:
    """Return cumulative user+system CPU time, preferring procfs."""

    try:
        raw = Path(f"/proc/{pid}/stat").read_text(encoding="ascii")
        fields = raw.rsplit(")", 1)[1].split()
        ticks = int(fields[11]) + int(fields[12])
        return ticks / float(os.sysconf("SC_CLK_TCK"))
    except (FileNotFoundError, OSError, ValueError, IndexError):
        pass

    output = command_output(["ps", "-o", "time=", "-p", str(pid)])
    if not output:
        return None
    try:
        day_parts = output.strip().split("-")
        days = int(day_parts[0]) if len(day_parts) == 2 else 0
        clock = day_parts[-1].split(":")
        if len(clock) == 3:
            hours, minutes, seconds = clock
        elif len(clock) == 2:
            hours, (minutes, seconds) = "0", clock
        else:
            return None
        return days * 86_400 + int(hours) * 3_600 + int(minutes) * 60 + float(seconds)
    except ValueError:
        return None


def process_rss_kib(pid: int) -> int | None:
    try:
        fields = Path(f"/proc/{pid}/statm").read_text(encoding="ascii").split()
        pages = int(fields[1])
        return pages * int(os.sysconf("SC_PAGE_SIZE")) // 1024
    except (FileNotFoundError, OSError, ValueError, IndexError):
        pass

    output = command_output(["ps", "-o", "rss=", "-p", str(pid)])
    if not output:
        return None
    try:
        return int(output)
    except ValueError:
        return None


class ResourceMonitor:
    def __init__(self, processes: dict[str, int], interval: float) -> None:
        self.processes = processes
        self.interval = interval
        self.stop_event = threading.Event()
        self.peak_rss: dict[str, int | None] = {name: None for name in processes}
        self.peak_total_rss: int | None = None
        self.thread = threading.Thread(target=self._run, daemon=True)

    def _sample(self) -> None:
        values = {name: process_rss_kib(pid) for name, pid in self.processes.items()}
        for name, value in values.items():
            if value is not None:
                prior = self.peak_rss[name]
                self.peak_rss[name] = value if prior is None else max(prior, value)
        present = [value for value in values.values() if value is not None]
        if len(present) == len(values):
            total = sum(present)
            self.peak_total_rss = (
                total if self.peak_total_rss is None else max(self.peak_total_rss, total)
            )

    def _run(self) -> None:
        while not self.stop_event.wait(self.interval):
            self._sample()

    def start(self) -> None:
        self._sample()
        self.thread.start()

    def stop(self) -> dict[str, object]:
        self.stop_event.set()
        self.thread.join(timeout=max(1.0, self.interval * 2))
        self._sample()
        return {
            "peak_kib": self.peak_rss,
            "peak_combined_kib": self.peak_total_rss,
        }


class StartGate:
    """Exclude worker socket setup from the measured interval."""

    def __init__(self, expected: int) -> None:
        self.expected = expected
        self.ready = 0
        self.condition = threading.Condition()
        self.release_event = threading.Event()

    def worker_ready(self) -> None:
        with self.condition:
            self.ready += 1
            self.condition.notify_all()
        if not self.release_event.wait(timeout=30):
            raise TimeoutError("benchmark start gate was not released")

    def wait_until_ready(
        self,
        futures: list[concurrent.futures.Future[dict[str, object]]],
    ) -> None:
        deadline = time.monotonic() + 30
        with self.condition:
            while self.ready < self.expected:
                failed = next((future for future in futures if future.done()), None)
                if failed is not None:
                    failed.result()
                    raise RuntimeError("a worker exited before reaching the start gate")
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise TimeoutError("workers did not become ready")
                self.condition.wait(timeout=min(0.05, remaining))

    def release(self) -> None:
        self.release_event.set()


def tcp_worker(
    worker_id: int,
    port: int,
    total_bytes: int,
    chunk_size: int,
    gate: StartGate,
) -> dict[str, object]:
    pattern = hashlib.sha256(f"ssrp-tcp-{worker_id}".encode("ascii")).digest()
    chunk = (pattern * math.ceil(chunk_size / len(pattern)))[:chunk_size]
    sent_digest = hashlib.sha256()
    received_digest = hashlib.sha256()
    received = 0
    receiver_error: list[BaseException] = []

    with socket.create_connection(("127.0.0.1", port), timeout=10) as stream:
        stream.settimeout(30)

        def receive() -> None:
            nonlocal received
            try:
                while received < total_bytes:
                    data = stream.recv(min(256 * 1024, total_bytes - received))
                    if not data:
                        raise ConnectionError("TCP echo ended before all bytes returned")
                    received_digest.update(data)
                    received += len(data)
            except BaseException as error:
                receiver_error.append(error)

        receiver = threading.Thread(target=receive, daemon=True)
        receiver.start()
        gate.worker_ready()
        started = time.perf_counter_ns()
        remaining = total_bytes
        while remaining:
            data = chunk if remaining >= len(chunk) else chunk[:remaining]
            stream.sendall(data)
            sent_digest.update(data)
            remaining -= len(data)
        receiver.join(timeout=30)
        elapsed = (time.perf_counter_ns() - started) / 1_000_000_000
        if receiver.is_alive():
            raise TimeoutError("TCP echo receiver did not finish")
        if receiver_error:
            raise receiver_error[0]
        if received != total_bytes or received_digest.digest() != sent_digest.digest():
            raise RuntimeError("TCP echo integrity check failed")
    return {"protocol": "tcp", "bytes_each_direction": total_bytes, "seconds": elapsed}


def udp_worker(
    worker_id: int,
    port: int,
    datagrams: int,
    payload_size: int,
    gate: StartGate,
) -> dict[str, object]:
    prefix = hashlib.sha256(f"ssrp-udp-{worker_id}".encode("ascii")).digest()
    filler_length = payload_size - 8
    filler = (prefix * math.ceil(filler_length / len(prefix)))[:filler_length]
    transferred = 0
    with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as client:
        client.settimeout(10)
        client.connect(("127.0.0.1", port))
        gate.worker_ready()
        started = time.perf_counter_ns()
        for sequence in range(datagrams):
            payload = sequence.to_bytes(8, "big") + filler
            if client.send(payload) != len(payload):
                raise RuntimeError("UDP datagram was only partially sent")
            response = client.recv(65_535)
            if response != payload:
                raise RuntimeError("UDP echo integrity check failed")
            transferred += len(payload)
        elapsed = (time.perf_counter_ns() - started) / 1_000_000_000
    return {"protocol": "udp", "bytes_each_direction": transferred, "seconds": elapsed}


@dataclass(frozen=True)
class Workload:
    tcp_workers: int
    udp_workers: int
    tcp_bytes_per_worker: int
    tcp_chunk_bytes: int
    udp_datagrams_per_worker: int
    udp_payload_bytes: int

    @property
    def concurrency(self) -> int:
        return self.tcp_workers + self.udp_workers

    def report(self) -> dict[str, object]:
        tcp_bytes = self.tcp_workers * self.tcp_bytes_per_worker
        udp_bytes = self.udp_workers * self.udp_datagrams_per_worker * self.udp_payload_bytes
        total = tcp_bytes + udp_bytes
        return {
            "concurrency": {
                "total": self.concurrency,
                "tcp_workers": self.tcp_workers,
                "udp_workers": self.udp_workers,
            },
            "tcp": {
                "chunk_payload_bytes": self.tcp_chunk_bytes,
                "bytes_per_worker_per_direction": self.tcp_bytes_per_worker,
                "bytes_per_sample_per_direction": tcp_bytes,
            },
            "udp": {
                "datagram_payload_bytes": self.udp_payload_bytes,
                "datagrams_per_worker": self.udp_datagrams_per_worker,
                "bytes_per_sample_per_direction": udp_bytes,
            },
            "offered_payload_ratio": {
                "tcp": round(tcp_bytes / total, 6),
                "udp": round(udp_bytes / total, 6),
            },
        }

    def expected_counter_delta(self, iterations: int) -> dict[str, int]:
        tcp_bytes = self.tcp_workers * self.tcp_bytes_per_worker * iterations
        udp_bytes = (
            self.udp_workers
            * self.udp_datagrams_per_worker
            * self.udp_payload_bytes
            * iterations
        )
        return {
            "tcp_uplink_bytes": tcp_bytes,
            "tcp_downlink_bytes": tcp_bytes,
            "udp_uplink_bytes": udp_bytes,
            "udp_downlink_bytes": udp_bytes,
        }


def run_sample(
    local_port: int,
    workload: Workload,
    processes: dict[str, ChildProcess],
    monitor_interval: float,
) -> dict[str, object]:
    gate = StartGate(workload.concurrency)
    monitor = ResourceMonitor(
        {name: child.pid for name, child in processes.items()},
        monitor_interval,
    )
    monitor_started = False
    try:
        with concurrent.futures.ThreadPoolExecutor(max_workers=workload.concurrency) as pool:
            futures: list[concurrent.futures.Future[dict[str, object]]] = []
            for worker_id in range(workload.tcp_workers):
                futures.append(
                    pool.submit(
                        tcp_worker,
                        worker_id,
                        local_port,
                        workload.tcp_bytes_per_worker,
                        workload.tcp_chunk_bytes,
                        gate,
                    )
                )
            for worker_id in range(workload.udp_workers):
                futures.append(
                    pool.submit(
                        udp_worker,
                        worker_id,
                        local_port,
                        workload.udp_datagrams_per_worker,
                        workload.udp_payload_bytes,
                        gate,
                    )
                )
            try:
                gate.wait_until_ready(futures)
                monitor.start()
                monitor_started = True
                cpu_before = {
                    name: process_cpu_seconds(child.pid)
                    for name, child in processes.items()
                }
                started = time.perf_counter_ns()
                gate.release()
                results = [future.result(timeout=300) for future in futures]
                ended = time.perf_counter_ns()
                cpu_after = {
                    name: process_cpu_seconds(child.pid)
                    for name, child in processes.items()
                }
                wall_seconds = (ended - started) / 1_000_000_000
            finally:
                gate.release()
    finally:
        resources = (
            monitor.stop()
            if monitor_started
            else {"peak_kib": {}, "peak_combined_kib": None}
        )

    cpu_delta: dict[str, float | None] = {}
    for name in processes:
        before, after = cpu_before[name], cpu_after[name]
        cpu_delta[name] = (
            round(max(0.0, after - before), 6)
            if before is not None and after is not None
            else None
        )
    available_cpu = [value for value in cpu_delta.values() if value is not None]
    combined_cpu = sum(available_cpu) if len(available_cpu) == len(cpu_delta) else None

    protocol: dict[str, dict[str, float | int]] = {}
    for name in ("tcp", "udp"):
        selected = [result for result in results if result["protocol"] == name]
        bytes_each_direction = sum(int(result["bytes_each_direction"]) for result in selected)
        protocol_wall = max(float(result["seconds"]) for result in selected)
        protocol[name] = {
            "uplink_payload_bytes": bytes_each_direction,
            "downlink_payload_bytes": bytes_each_direction,
            "worker_wall_seconds_max": round(protocol_wall, 6),
            "bidirectional_mib_per_second": round(
                (bytes_each_direction * 2) / MIB / protocol_wall,
                3,
            ),
        }
    total_payload = sum(
        int(metrics["uplink_payload_bytes"]) + int(metrics["downlink_payload_bytes"])
        for metrics in protocol.values()
    )
    return {
        "wall_seconds": round(wall_seconds, 6),
        "bidirectional_payload_bytes": total_payload,
        "bidirectional_mib_per_second": round(total_payload / MIB / wall_seconds, 3),
        "protocols": protocol,
        "process_cpu_seconds": {
            **cpu_delta,
            "combined": round(combined_cpu, 6) if combined_cpu is not None else None,
            "combined_percent_of_wall": (
                round(combined_cpu / wall_seconds * 100, 2)
                if combined_cpu is not None
                else None
            ),
        },
        "process_rss": resources,
    }


def nested_metric(record: dict[str, object], *keys: str) -> float | None:
    value: object = record
    for key in keys:
        if not isinstance(value, dict) or key not in value:
            return None
        value = value[key]
    if isinstance(value, (int, float)):
        return float(value)
    return None


def complete_metrics(samples: list[dict[str, object]], *keys: str) -> list[float] | None:
    values = [nested_metric(sample, *keys) for sample in samples]
    if any(value is None for value in values):
        return None
    return [value for value in values if value is not None]


def distribution(values: list[float], digits: int) -> dict[str, float]:
    return {
        "median": round(statistics.median(values), digits),
        "min": round(min(values), digits),
        "max": round(max(values), digits),
    }


def aggregate_samples(samples: list[dict[str, object]]) -> dict[str, object]:
    walls = complete_metrics(samples, "wall_seconds")
    throughput = complete_metrics(samples, "bidirectional_mib_per_second")
    assert walls is not None and throughput is not None
    protocol_throughput = {
        protocol: distribution(values, 3)
        for protocol in ("tcp", "udp")
        if (
            values := complete_metrics(
                samples,
                "protocols",
                protocol,
                "bidirectional_mib_per_second",
            )
        )
        is not None
    }
    cpu_medians: dict[str, float | None] = {}
    for process in ("ssserver", "sslocal", "combined"):
        values = complete_metrics(samples, "process_cpu_seconds", process)
        cpu_medians[process] = round(statistics.median(values), 6) if values else None
    rss_maxima: dict[str, int | None] = {}
    for process in ("ssserver", "sslocal"):
        values = complete_metrics(samples, "process_rss", "peak_kib", process)
        rss_maxima[process] = int(max(values)) if values else None
    combined_rss = complete_metrics(samples, "process_rss", "peak_combined_kib")
    rss_maxima["combined"] = int(max(combined_rss)) if combined_rss else None
    return {
        "samples": len(samples),
        "wall_seconds": distribution(walls, 6),
        "bidirectional_mib_per_second": distribution(throughput, 3),
        "protocol_bidirectional_mib_per_second": protocol_throughput,
        "process_cpu_seconds_median": cpu_medians,
        "process_peak_rss_kib_max": rss_maxima,
    }


def server_config(
    server_port: int,
    identity_key: str,
    user_key: str,
    statistics_socket: Path | None,
) -> dict[str, object]:
    server: dict[str, object] = {
        "server": "127.0.0.1",
        "server_port": server_port,
        "method": METHOD,
        "password": identity_key,
        "mode": "tcp_and_udp",
        "users": [{"name": "benchmark-user", "password": user_key}],
    }
    config: dict[str, object] = {"servers": [server]}
    if statistics_socket is not None:
        server["id"] = "benchmark-entry"
        config["user_stats"] = {
            "node_id": "benchmark-node",
            "socket_path": str(statistics_socket),
            "socket_mode": "0600",
            "read_timeout_ms": 1000,
            "write_timeout_ms": 1000,
            "max_request_bytes": 1024,
            "max_response_bytes": 1_048_576,
            "max_identities": 1,
            "max_concurrent_clients": 1,
        }
    return config


def local_config(
    local_port: int,
    server_port: int,
    echo_port: int,
    password: str,
) -> dict[str, object]:
    return {
        "locals": [
            {
                "local_address": "127.0.0.1",
                "local_port": local_port,
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


def run_case(
    case_name: str,
    binaries: tuple[Path, Path],
    echo_port: int,
    identity_key: str,
    user_key: str,
    runtime_statistics: bool,
    workload: Workload,
    warmups: int,
    sample_count: int,
    monitor_interval: float,
) -> dict[str, object]:
    # Keep the private directory name short: Darwin's sockaddr_un path is only
    # 104 bytes and its per-user temporary-directory prefix is already long.
    with tempfile.TemporaryDirectory(prefix="sdp-") as temp_name:
        temp = Path(temp_name).resolve()
        temp.chmod(0o700)
        server_port = reserve_port()
        local_port = reserve_port()
        statistics_socket = temp / "user-stats.sock" if runtime_statistics else None
        server_path = temp / "server.json"
        local_path = temp / "local.json"
        secure_write_json(
            server_path,
            server_config(server_port, identity_key, user_key, statistics_socket),
        )
        secure_write_json(
            local_path,
            local_config(
                local_port,
                server_port,
                echo_port,
                f"{identity_key}:{user_key}",
            ),
        )
        server = ChildProcess([str(binaries[0]), "-c", str(server_path)], temp / "server.log")
        local: ChildProcess | None = None
        try:
            if statistics_socket is not None:
                wait_for_socket(statistics_socket, server)
            wait_for_tcp(server_port, [("ssserver", server)])
            local = ChildProcess([str(binaries[1]), "-c", str(local_path)], temp / "local.log")
            processes = {"ssserver": server, "sslocal": local}
            wait_for_tcp(local_port, list(processes.items()))
            time.sleep(0.1)

            counters_before = (
                snapshot_user_counters(query_statistics(statistics_socket))
                if statistics_socket is not None
                else None
            )

            for _ in range(warmups):
                run_sample(local_port, workload, processes, monitor_interval)
            samples = [
                run_sample(local_port, workload, processes, monitor_interval)
                for _ in range(sample_count)
            ]
            statistics_validation: dict[str, object] | None = None
            if statistics_socket is not None and counters_before is not None:
                counters_after = snapshot_user_counters(query_statistics(statistics_socket))
                observed_delta = {
                    field: counters_after[field] - counters_before[field]
                    for field in COUNTER_FIELDS
                }
                expected_delta = workload.expected_counter_delta(warmups + sample_count)
                if observed_delta != expected_delta:
                    mismatches = [
                        field
                        for field in COUNTER_FIELDS
                        if observed_delta[field] != expected_delta[field]
                    ]
                    raise RuntimeError(
                        "runtime user-statistics counter mismatch: " + ", ".join(mismatches)
                    )
                statistics_validation = {
                    "matched": True,
                    "expected_delta": expected_delta,
                    "observed_delta": observed_delta,
                }
            for label, process in processes.items():
                process.assert_running(label)
            return {
                "name": case_name,
                "runtime_user_stats": runtime_statistics,
                "runtime_user_stats_validation": statistics_validation,
                "aggregate": aggregate_samples(samples),
                "measurements": samples,
            }
        finally:
            if local is not None:
                local.stop()
            server.stop()


def validate_positive(parser: argparse.ArgumentParser, name: str, value: int) -> None:
    if value < 1:
        parser.error(f"{name} must be positive")


def main() -> None:
    parser = argparse.ArgumentParser(
        description="Compare upstream and plus TCP/UDP loopback data paths.",
        formatter_class=argparse.ArgumentDefaultsHelpFormatter,
        epilog=(
            "Short smoke workload: --samples 1 --warmups 0 --tcp-workers 1 "
            "--udp-workers 1 --tcp-mib-per-worker 1 --udp-datagrams-per-worker 20"
        ),
    )
    parser.add_argument(
        "--upstream-source",
        required=True,
        type=Path,
        help="clean Git checkout at the exact project-locked upstream commit",
    )
    parser.add_argument(
        "--plus-source",
        required=True,
        type=Path,
        help="Git checkout/worktree derived from the same locked upstream commit",
    )
    parser.add_argument("--target-root", type=Path)
    parser.add_argument("--output", type=Path)
    parser.add_argument("--samples", type=int, default=5)
    parser.add_argument("--warmups", type=int, default=1)
    parser.add_argument("--tcp-workers", type=int, default=4)
    parser.add_argument("--udp-workers", type=int, default=4)
    parser.add_argument("--tcp-mib-per-worker", type=int, default=32)
    parser.add_argument("--tcp-chunk-bytes", type=int, default=65_536)
    parser.add_argument("--udp-datagrams-per-worker", type=int, default=2_000)
    parser.add_argument("--udp-payload-bytes", type=int, default=1_200)
    parser.add_argument("--monitor-interval", type=float, default=0.2)
    parser.add_argument("--offline-build", action="store_true")
    arguments = parser.parse_args()

    for name in (
        "samples",
        "tcp_workers",
        "udp_workers",
        "tcp_mib_per_worker",
        "tcp_chunk_bytes",
        "udp_datagrams_per_worker",
        "udp_payload_bytes",
    ):
        validate_positive(parser, name, int(getattr(arguments, name)))
    if arguments.warmups < 0:
        parser.error("warmups cannot be negative")
    if not 8 <= arguments.udp_payload_bytes <= 65_507:
        parser.error("udp-payload-bytes must be between 8 and 65507")
    if not math.isfinite(arguments.monitor_interval) or arguments.monitor_interval <= 0:
        parser.error("monitor-interval must be positive")

    upstream = arguments.upstream_source.resolve()
    plus = arguments.plus_source.resolve()
    for label, source in (("upstream", upstream), ("plus", plus)):
        if not (source / "Cargo.toml").is_file():
            parser.error(f"{label} source does not contain Cargo.toml")
    locked_commit = project_locked_commit()
    assert_locked_upstream(upstream, locked_commit)
    plus_base_verification = assert_plus_base(plus, locked_commit)

    workload = Workload(
        tcp_workers=arguments.tcp_workers,
        udp_workers=arguments.udp_workers,
        tcp_bytes_per_worker=arguments.tcp_mib_per_worker * MIB,
        tcp_chunk_bytes=arguments.tcp_chunk_bytes,
        udp_datagrams_per_worker=arguments.udp_datagrams_per_worker,
        udp_payload_bytes=arguments.udp_payload_bytes,
    )
    if any(
        value > U64_MAX
        for value in workload.expected_counter_delta(arguments.warmups + arguments.samples).values()
    ):
        parser.error("workload would exceed the user-statistics u64 counter range")
    temporary_target: tempfile.TemporaryDirectory[str] | None = None
    if arguments.target_root is None:
        temporary_target = tempfile.TemporaryDirectory(prefix="ssrp-data-path-build-")
        target_root = Path(temporary_target.name)
        target_root.chmod(0o700)
    else:
        target_root = arguments.target_root.resolve()
        target_root.mkdir(parents=True, exist_ok=True)

    try:
        upstream_binaries = build_binaries(
            upstream,
            target_root / "upstream",
            feature=None,
            offline=arguments.offline_build,
        )
        plus_binaries = build_binaries(
            plus,
            target_root / "plus-user-stats",
            feature="user-stats",
            offline=arguments.offline_build,
        )
        upstream_artifacts = build_artifact_report(upstream_binaries)
        plus_artifacts = build_artifact_report(plus_binaries)
        identity_key = random_key()
        user_key = random_key()
        with EchoServices() as echo:
            cases = [
                run_case(
                    "locked_upstream",
                    upstream_binaries,
                    echo.port,
                    identity_key,
                    user_key,
                    False,
                    workload,
                    arguments.warmups,
                    arguments.samples,
                    arguments.monitor_interval,
                ),
                run_case(
                    "plus_compiled_runtime_disabled",
                    plus_binaries,
                    echo.port,
                    identity_key,
                    user_key,
                    False,
                    workload,
                    arguments.warmups,
                    arguments.samples,
                    arguments.monitor_interval,
                ),
                run_case(
                    "plus_runtime_enabled",
                    plus_binaries,
                    echo.port,
                    identity_key,
                    user_key,
                    True,
                    workload,
                    arguments.warmups,
                    arguments.samples,
                    arguments.monitor_interval,
                ),
            ]
    finally:
        if temporary_target is not None:
            temporary_target.cleanup()

    report = {
        "schema_version": 1,
        "generated_at_utc": dt.datetime.now(dt.timezone.utc).isoformat(),
        "benchmark": "shadowsocks-rust-plus-loopback-data-path",
        "workload_network_scope": "IPv4 loopback only",
        "build": {
            "profile": "release",
            "locked": True,
            "cargo_offline": arguments.offline_build,
            "upstream_revision": locked_commit,
            "plus_revision": git_revision(plus),
            "plus_worktree_state": git_worktree_state(plus),
            "plus_base_verification": plus_base_verification,
            "upstream_extra_features": [],
            "plus_extra_features": ["user-stats"],
            "child_rust_log": "off",
            "artifacts": {
                "upstream": upstream_artifacts,
                "plus_user_stats": plus_artifacts,
            },
        },
        "environment": {
            "platform": platform.platform(),
            "system": platform.system(),
            "release": platform.release(),
            "machine": platform.machine(),
            "processor": cpu_model(),
            "logical_cpu_count": os.cpu_count(),
            "memory_bytes": system_memory_bytes(),
            "python": platform.python_version(),
            "rustc": command_output(["rustc", "--version"]),
            "rustc_verbose": command_output(["rustc", "--version", "--verbose"]),
            "cargo": command_output(["cargo", "--version"]),
        },
        "method": METHOD,
        "workload": workload.report(),
        "sampling": {
            "warmups_per_case": arguments.warmups,
            "measured_samples_per_case": arguments.samples,
            "resource_monitor_interval_seconds": arguments.monitor_interval,
            "case_order": [case["name"] for case in cases],
            "connection_model": {
                "tcp": (
                    "new local tunnel socket per worker/sample; local connect is outside "
                    "the timer, "
                    "while remote proxy setup may overlap the measured transfer"
                ),
                "udp": (
                    "new connected local UDP socket per worker/sample; first-packet association "
                    "setup is inside the timer"
                ),
            },
        },
        "cases": cases,
    }
    encoded = json.dumps(report, ensure_ascii=False, indent=2) + "\n"
    if arguments.output is None:
        print(encoded, end="")
    else:
        secure_write_report(arguments.output.resolve(), encoded)


if __name__ == "__main__":
    main()
