#!/usr/bin/env python3
"""Reproducible loopback data-path benchmark for shadowsocks-rust-plus.

On Linux, the benchmark builds and compares three release configurations:

* the exact locked upstream revision without the ``user-audit`` feature;
* plus compiled with ``user-audit`` but without runtime audit configuration;
* the same plus binaries with user statistics and user audit enabled.

The enabled case requires an already running auditd. Its PID, executable and
ingest socket identity are verified before and after the workload, and auditd
RSS is sampled in the same run as the proxy worker outcomes.

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
import pwd
import re
import secrets
import socket
import socketserver
import stat
import statistics
import subprocess
import sys
import tempfile
import threading
import time
import uuid
from dataclasses import dataclass, field
from pathlib import Path
from typing import BinaryIO

from http_unix import json_body, request as http_request, validate_snapshot
from mock_collector import (
    MockCollector,
    canonical_json,
    strict_json,
    unix_http_request,
    verify_response,
)

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
    def __init__(
        self,
        command: list[str],
        log_path: Path,
        identity: pwd.struct_passwd | None = None,
    ) -> None:
        self.log = secure_open_log(log_path)
        environment = os.environ.copy()
        # Upstream traces full configuration objects. Do not inherit a caller's
        # RUST_LOG=trace into a benchmark that uses ephemeral credentials.
        environment["RUST_LOG"] = "off"
        environment["RUST_BACKTRACE"] = "0"
        identity_options: dict[str, object] = {}
        if identity is not None:
            identity_options = {
                "user": identity.pw_uid,
                "group": identity.pw_gid,
                "extra_groups": os.getgrouplist(identity.pw_name, identity.pw_gid),
            }
        try:
            self.process = subprocess.Popen(
                command,
                stdin=subprocess.DEVNULL,
                stdout=self.log,
                stderr=subprocess.STDOUT,
                env=environment,
                **identity_options,
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
    include_auditd: bool = False,
) -> tuple[Path, Path, Path | None]:
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
    if include_auditd:
        command.extend(["--bin", "shadowsocks-auditd"])
    if feature is not None:
        command.extend(["--features", feature])
    if offline:
        command.append("--offline")
    subprocess.run(command, check=True, env=environment)
    server = target / "release" / "ssserver"
    local = target / "release" / "sslocal"
    auditd = target / "release" / "shadowsocks-auditd" if include_auditd else None
    if not server.is_file() or not local.is_file() or (auditd is not None and not auditd.is_file()):
        raise RuntimeError("release build completed without required benchmark binaries")
    return server, local, auditd


def file_sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        while chunk := stream.read(MIB):
            digest.update(chunk)
    return digest.hexdigest()


def process_start_time_ticks(pid: int) -> int:
    try:
        raw = Path(f"/proc/{pid}/stat").read_text(encoding="ascii")
        fields = raw.rsplit(")", 1)[1].split()
        # Field 22 is process start time; fields begins at procfs field 3.
        value = int(fields[19])
    except (FileNotFoundError, OSError, ValueError, IndexError) as error:
        raise RuntimeError(f"cannot read auditd process identity for PID {pid}") from error
    if value < 1:
        raise RuntimeError("auditd process start time is invalid")
    return value


def process_effective_uid(pid: int) -> int:
    try:
        for line in Path(f"/proc/{pid}/status").read_text(encoding="ascii").splitlines():
            if line.startswith("Uid:"):
                values = line.split()[1:]
                return int(values[1])
    except (FileNotFoundError, OSError, ValueError, IndexError) as error:
        raise RuntimeError(f"cannot read auditd credentials for PID {pid}") from error
    raise RuntimeError(f"cannot find auditd credentials for PID {pid}")


@dataclass(frozen=True)
class AuditdProcessEvidence:
    pid: int
    user: str
    uid: int
    ingest_socket: Path
    ingest_socket_device: int
    ingest_socket_inode: int
    executable_path: Path
    executable_sha256: str
    executable_device: int
    executable_inode: int
    process_start_time_ticks: int

    def report(self, run_id: str, peak_rss_kib: int, rss_sample_count: int) -> dict[str, object]:
        return {
            "run_id": run_id,
            "measurement_source": "resource_monitor_pid",
            "pid": self.pid,
            "process_start_time_ticks": self.process_start_time_ticks,
            "user": self.user,
            "uid": self.uid,
            "ingest_socket_path": str(self.ingest_socket),
            "ingest_socket_device": self.ingest_socket_device,
            "ingest_socket_inode": self.ingest_socket_inode,
            "executable_path": str(self.executable_path),
            "executable_sha256": self.executable_sha256,
            "peak_rss_kib": peak_rss_kib,
            "rss_sample_count": rss_sample_count,
        }


def capture_auditd_process(
    pid: int,
    user: str,
    ingest_socket: Path,
) -> AuditdProcessEvidence:
    if platform.system() != "Linux":
        raise RuntimeError("native user-audit benchmark requires Linux")
    if pid < 1:
        raise RuntimeError("--auditd-pid must be positive")
    if not ingest_socket.is_absolute() or os.path.normpath(str(ingest_socket)) != str(ingest_socket):
        raise RuntimeError("--audit-ingest-socket must be a canonical absolute path")
    try:
        account = pwd.getpwnam(user)
    except KeyError as error:
        raise RuntimeError(f"--auditd-user does not exist: {user}") from error
    try:
        socket_metadata = ingest_socket.lstat()
        if ingest_socket.resolve(strict=True) != ingest_socket:
            raise RuntimeError("auditd ingest socket path contains a symbolic link")
    except OSError as error:
        raise RuntimeError(f"cannot inspect auditd ingest socket: {error}") from error
    if not stat.S_ISSOCK(socket_metadata.st_mode):
        raise RuntimeError("--audit-ingest-socket is not a Unix socket")
    if socket_metadata.st_uid != account.pw_uid:
        raise RuntimeError("auditd user does not own the ingest socket")
    if process_effective_uid(pid) != account.pw_uid:
        raise RuntimeError("auditd process effective UID does not match --auditd-user")

    proc_executable = Path(f"/proc/{pid}/exe")
    try:
        linked_path = os.readlink(proc_executable)
        if linked_path.endswith(" (deleted)"):
            raise RuntimeError("auditd executable was deleted after process startup")
        executable_path = Path(linked_path).resolve(strict=True)
        before = proc_executable.stat()
        executable_sha256 = file_sha256(proc_executable)
        after = proc_executable.stat()
    except OSError as error:
        raise RuntimeError(f"cannot inspect auditd executable for PID {pid}: {error}") from error
    if not stat.S_ISREG(after.st_mode) or before.st_dev != after.st_dev or before.st_ino != after.st_ino:
        raise RuntimeError("auditd executable identity changed while it was hashed")
    return AuditdProcessEvidence(
        pid=pid,
        user=user,
        uid=account.pw_uid,
        ingest_socket=ingest_socket,
        ingest_socket_device=socket_metadata.st_dev,
        ingest_socket_inode=socket_metadata.st_ino,
        executable_path=executable_path,
        executable_sha256=executable_sha256,
        executable_device=after.st_dev,
        executable_inode=after.st_ino,
        process_start_time_ticks=process_start_time_ticks(pid),
    )


def assert_same_auditd_process(expected: AuditdProcessEvidence) -> None:
    current = capture_auditd_process(expected.pid, expected.user, expected.ingest_socket)
    if current != expected:
        raise RuntimeError("auditd process, executable or ingest socket changed during benchmark")


AUDIT_HEALTH_FIELDS = {
    "schema_version",
    "node_id",
    "status",
    "producer_connected",
    "producer_runtime_id",
    "last_ingest_at_unix_ms",
    "spool_epoch",
    "spool_bytes",
    "max_spool_bytes",
    "sealed_batches",
    "oldest_unacked_at_unix_ms",
    "stored_records",
    "storage_rejected_attempts",
    "evicted_unacked_records",
}


def _audit_decimal(value: object, field_name: str, *, positive: bool = False) -> int:
    pattern = r"[1-9][0-9]*" if positive else r"0|[1-9][0-9]*"
    if not isinstance(value, str) or re.fullmatch(pattern, value) is None:
        raise RuntimeError(f"auditd health {field_name} is not canonical decimal")
    number = int(value)
    if number > U64_MAX:
        raise RuntimeError(f"auditd health {field_name} exceeds u64")
    return number


def _validate_audit_health(
    status: int,
    body: bytes,
    node_id: str,
) -> dict[str, object]:
    value = strict_json(body)
    if not isinstance(value, dict) or set(value) != AUDIT_HEALTH_FIELDS:
        raise RuntimeError("auditd health response has unexpected fields")
    expected_status = "ok" if status == 200 else "degraded" if status == 503 else None
    if (
        expected_status is None
        or value.get("schema_version") != 1
        or value.get("node_id") != node_id
        or value.get("status") != expected_status
    ):
        raise RuntimeError("auditd health status, schema or node is invalid")
    if canonical_json(value) != body:
        raise RuntimeError("auditd health response is not canonical JSON")
    producer_connected = value.get("producer_connected")
    if type(producer_connected) is not bool:
        raise RuntimeError("auditd health producer_connected is not boolean")
    runtime_id = value.get("producer_runtime_id")
    if runtime_id is not None and (
        not isinstance(runtime_id, str) or re.fullmatch(r"[0-9a-f]{32}", runtime_id) is None
    ):
        raise RuntimeError("auditd health producer_runtime_id is invalid")
    last_ingest_raw = value.get("last_ingest_at_unix_ms")
    last_ingest = (
        None
        if last_ingest_raw is None
        else _audit_decimal(last_ingest_raw, "last_ingest_at_unix_ms", positive=True)
    )
    if not producer_connected and (runtime_id is not None or last_ingest is not None):
        raise RuntimeError("disconnected auditd health retained producer state")
    spool_epoch = value.get("spool_epoch")
    if not isinstance(spool_epoch, str) or re.fullmatch(r"[0-9a-f]{32}", spool_epoch) is None:
        raise RuntimeError("auditd health spool_epoch is invalid")
    oldest_raw = value.get("oldest_unacked_at_unix_ms")
    oldest = (
        None
        if oldest_raw is None
        else _audit_decimal(oldest_raw, "oldest_unacked_at_unix_ms", positive=True)
    )
    normalized: dict[str, object] = {
        "http_status": status,
        "status": expected_status,
        "node_id": node_id,
        "producer_connected": producer_connected,
        "producer_runtime_id": runtime_id,
        "last_ingest_at_unix_ms": last_ingest,
        "spool_epoch": spool_epoch,
        "oldest_unacked_at_unix_ms": oldest,
    }
    for field_name in (
        "spool_bytes",
        "max_spool_bytes",
        "sealed_batches",
        "stored_records",
        "storage_rejected_attempts",
        "evicted_unacked_records",
    ):
        normalized[field_name] = _audit_decimal(value[field_name], field_name)
    return normalized


def _query_audit_health(socket_path: Path, node_id: str, key: bytes) -> dict[str, object]:
    collector = MockCollector(node_id, key)
    nonce = secrets.token_hex(16)
    request = collector.build_request("GET", "/v1/audit/healthz", nonce=nonce)
    status, headers, body = unix_http_request(socket_path, request)
    verify_response(
        key,
        status=status,
        headers=headers,
        body=body,
        request_nonce=nonce,
        expected_node=node_id,
    )
    return _validate_audit_health(status, body, node_id)


def _read_audit_hmac_key(path: Path, expected_uid: int) -> bytes:
    if not path.is_absolute() or os.path.normpath(str(path)) != str(path):
        raise RuntimeError("--audit-hmac-key-file must be a canonical absolute path")
    if path.resolve(strict=True) != path:
        raise RuntimeError("audit HMAC key path contains a symbolic link")
    flags = os.O_RDONLY | os.O_CLOEXEC
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    descriptor = os.open(path, flags)
    try:
        metadata = os.fstat(descriptor)
        if not stat.S_ISREG(metadata.st_mode):
            raise RuntimeError("audit HMAC key is not a regular file")
        if metadata.st_uid != expected_uid or stat.S_IMODE(metadata.st_mode) != 0o600:
            raise RuntimeError("audit HMAC key owner or mode does not match auditd")
        if metadata.st_size not in (64, 65):
            raise RuntimeError("audit HMAC key file must contain exactly 64 hex bytes and optional LF")
        payload = os.read(descriptor, 66)
    finally:
        os.close(descriptor)
    raw = payload[:-1] if payload.endswith(b"\n") else payload
    if len(raw) != 64 or re.fullmatch(rb"[0-9a-f]{64}", raw) is None:
        raise RuntimeError("audit HMAC key must be 64 lowercase hexadecimal bytes")
    return bytes.fromhex(raw.decode("ascii"))


@dataclass(frozen=True)
class AuditHealthClient:
    socket_path: Path
    socket_device: int
    socket_inode: int
    socket_owner_uid: int
    user: str
    uid: int
    gid: int
    supplementary_groups: tuple[int, ...]
    node_id: str
    key: bytes = field(repr=False)

    def assert_socket_identity(self) -> None:
        metadata = self.socket_path.lstat()
        if (
            not stat.S_ISSOCK(metadata.st_mode)
            or metadata.st_dev != self.socket_device
            or metadata.st_ino != self.socket_inode
            or metadata.st_uid != self.socket_owner_uid
        ):
            raise RuntimeError("auditd export socket identity changed during benchmark")

    def query(self) -> dict[str, object]:
        self.assert_socket_identity()
        completed = subprocess.run(
            [
                sys.executable,
                str(Path(__file__).resolve()),
                "--audit-health-helper",
                "--socket",
                str(self.socket_path),
                "--node-id",
                self.node_id,
            ],
            input=self.key.hex(),
            capture_output=True,
            text=True,
            timeout=10,
            check=False,
            user=self.uid,
            group=self.gid,
            extra_groups=list(self.supplementary_groups),
        )
        if completed.returncode != 0:
            raise RuntimeError(
                "signed auditd health query failed as export peer: "
                + completed.stderr.strip()[:500]
            )
        try:
            value = json.loads(completed.stdout)
        except json.JSONDecodeError as error:
            raise RuntimeError("auditd health helper returned invalid JSON") from error
        if not isinstance(value, dict) or value.get("node_id") != self.node_id:
            raise RuntimeError("auditd health helper returned invalid evidence")
        self.assert_socket_identity()
        return value


def capture_audit_health_client(
    socket_path: Path,
    user: str,
    node_id: str,
    key_path: Path,
    auditd_uid: int,
) -> AuditHealthClient:
    if not socket_path.is_absolute() or os.path.normpath(str(socket_path)) != str(socket_path):
        raise RuntimeError("--audit-export-socket must be a canonical absolute path")
    metadata = socket_path.lstat()
    if socket_path.resolve(strict=True) != socket_path or not stat.S_ISSOCK(metadata.st_mode):
        raise RuntimeError("--audit-export-socket is not a canonical Unix socket")
    if metadata.st_uid != auditd_uid:
        raise RuntimeError("auditd user does not own the export socket")
    try:
        account = pwd.getpwnam(user)
    except KeyError as error:
        raise RuntimeError(f"--audit-export-user does not exist: {user}") from error
    if not node_id or len(node_id) > 128 or not node_id.isascii() or not node_id.isprintable():
        raise RuntimeError("--audit-node-id is invalid")
    return AuditHealthClient(
        socket_path=socket_path,
        socket_device=metadata.st_dev,
        socket_inode=metadata.st_ino,
        socket_owner_uid=metadata.st_uid,
        user=user,
        uid=account.pw_uid,
        gid=account.pw_gid,
        supplementary_groups=tuple(os.getgrouplist(account.pw_name, account.pw_gid)),
        node_id=node_id,
        key=_read_audit_hmac_key(key_path, auditd_uid),
    )


def _audit_health_helper_main(arguments: list[str]) -> int:
    parser = argparse.ArgumentParser(add_help=False)
    parser.add_argument("--socket", required=True, type=Path)
    parser.add_argument("--node-id", required=True)
    args = parser.parse_args(arguments)
    key_hex = sys.stdin.read(65)
    if re.fullmatch(r"[0-9a-f]{64}", key_hex) is None:
        raise RuntimeError("health helper received invalid key material")
    health = _query_audit_health(args.socket, args.node_id, bytes.fromhex(key_hex))
    print(json.dumps(health, sort_keys=True, separators=(",", ":")))
    return 0


def build_artifact_report(binaries: tuple[Path, Path, Path | None]) -> dict[str, object]:
    return {
        path.name: {"sha256": file_sha256(path), "bytes": path.stat().st_size}
        for path in binaries
        if path is not None
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
        self.sample_count: dict[str, int] = {name: 0 for name in processes}
        self.peak_total_rss: int | None = None
        self.thread = threading.Thread(target=self._run, daemon=True)

    def _sample(self) -> None:
        values = {name: process_rss_kib(pid) for name, pid in self.processes.items()}
        for name, value in values.items():
            if value is not None:
                self.sample_count[name] += 1
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
            "sample_count": self.sample_count,
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
    extra_processes: dict[str, int] | None = None,
) -> dict[str, object]:
    gate = StartGate(workload.concurrency)
    process_pids = {name: child.pid for name, child in processes.items()}
    for name, pid in (extra_processes or {}).items():
        if name in process_pids:
            raise RuntimeError(f"duplicate monitored process name: {name}")
        process_pids[name] = pid
    monitor = ResourceMonitor(process_pids, monitor_interval)
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
                    name: process_cpu_seconds(pid)
                    for name, pid in process_pids.items()
                }
                started = time.perf_counter_ns()
                gate.release()
                results: list[dict[str, object]] = []
                proxy_attempts = 0
                proxy_successes = 0
                proxy_errors = 0
                worker_errors: list[BaseException] = []
                for future in futures:
                    proxy_attempts += 1
                    try:
                        results.append(future.result(timeout=300))
                    except BaseException as error:
                        proxy_errors += 1
                        worker_errors.append(error)
                    else:
                        proxy_successes += 1
                ended = time.perf_counter_ns()
                cpu_after = {
                    name: process_cpu_seconds(pid)
                    for name, pid in process_pids.items()
                }
                wall_seconds = (ended - started) / 1_000_000_000
                if worker_errors:
                    raise RuntimeError(
                        f"{proxy_errors} of {proxy_attempts} proxy workers failed"
                    ) from worker_errors[0]
            finally:
                gate.release()
    finally:
        resources = (
            monitor.stop()
            if monitor_started
            else {"peak_kib": {}, "sample_count": {}, "peak_combined_kib": None}
        )

    cpu_delta: dict[str, float | None] = {}
    for name in process_pids:
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
        "proxy": {
            "source": "worker_outcomes",
            "attempts": proxy_attempts,
            "successes": proxy_successes,
            "errors": proxy_errors,
        },
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
    for process in ("ssserver", "sslocal", "auditd", "combined"):
        values = complete_metrics(samples, "process_cpu_seconds", process)
        cpu_medians[process] = round(statistics.median(values), 6) if values else None
    rss_maxima: dict[str, int | None] = {}
    rss_sample_count_min: dict[str, int | None] = {}
    for process in ("ssserver", "sslocal", "auditd"):
        values = complete_metrics(samples, "process_rss", "peak_kib", process)
        rss_maxima[process] = int(max(values)) if values else None
        sample_counts = complete_metrics(samples, "process_rss", "sample_count", process)
        rss_sample_count_min[process] = int(min(sample_counts)) if sample_counts else None
    combined_rss = complete_metrics(samples, "process_rss", "peak_combined_kib")
    rss_maxima["combined"] = int(max(combined_rss)) if combined_rss else None
    proxy_attempts = complete_metrics(samples, "proxy", "attempts")
    proxy_successes = complete_metrics(samples, "proxy", "successes")
    proxy_errors = complete_metrics(samples, "proxy", "errors")
    if proxy_attempts is None or proxy_successes is None or proxy_errors is None:
        raise RuntimeError("proxy worker outcomes were not recorded for every sample")
    return {
        "samples": len(samples),
        "wall_seconds": distribution(walls, 6),
        "bidirectional_mib_per_second": distribution(throughput, 3),
        "protocol_bidirectional_mib_per_second": protocol_throughput,
        "process_cpu_seconds_median": cpu_medians,
        "process_peak_rss_kib_max": rss_maxima,
        "process_rss_sample_count_min": rss_sample_count_min,
        "proxy": {
            "source": "worker_outcomes",
            "attempts": int(sum(proxy_attempts)),
            "successes": int(sum(proxy_successes)),
            "errors": int(sum(proxy_errors)),
        },
    }


def server_config(
    server_port: int,
    identity_key: str,
    user_key: str,
    node_id: str,
    statistics_socket: Path | None,
    audit_ingest_socket: Path | None,
    auditd_user: str | None,
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
            "node_id": node_id,
            "socket_path": str(statistics_socket),
            "socket_mode": "0600",
            "read_timeout_ms": 1000,
            "write_timeout_ms": 1000,
            "max_request_bytes": 1024,
            "max_response_bytes": 1_048_576,
            "max_identities": 1,
            "max_concurrent_clients": 1,
        }
    if (audit_ingest_socket is None) != (auditd_user is None):
        raise RuntimeError("audit ingest socket and auditd user must be supplied together")
    if audit_ingest_socket is not None and auditd_user is not None:
        if statistics_socket is None:
            raise RuntimeError("user audit requires user statistics identity metadata")
        config["user_audit"] = {
            "ingest_socket_path": str(audit_ingest_socket),
            "auditd_user": auditd_user,
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


def _audit_ingest_evidence(
    before: dict[str, object],
    after: dict[str, object],
    runtime_id: str,
) -> dict[str, object] | None:
    before_stored = before.get("stored_records")
    after_stored = after.get("stored_records")
    before_ingest = before.get("last_ingest_at_unix_ms")
    after_ingest = after.get("last_ingest_at_unix_ms")
    if (
        before.get("producer_connected") is not False
        or after.get("http_status") != 200
        or after.get("status") != "ok"
        or after.get("producer_connected") is not True
        or after.get("producer_runtime_id") != runtime_id
        or before.get("spool_epoch") != after.get("spool_epoch")
        or type(before_stored) is not int
        or type(after_stored) is not int
        or after_stored <= before_stored
        or type(after_ingest) is not int
        or (type(before_ingest) is int and after_ingest <= before_ingest)
        or after.get("storage_rejected_attempts") != before.get("storage_rejected_attempts")
        or after.get("evicted_unacked_records") != before.get("evicted_unacked_records")
    ):
        return None
    return {
        "source": "signed_health_stored_records_delta",
        "producer_runtime_id": runtime_id,
        "before": before,
        "after": after,
        "stored_records_delta": after_stored - before_stored,
        "last_ingest_advanced": True,
    }


def wait_for_audit_ingest(
    client: AuditHealthClient,
    before: dict[str, object],
    runtime_id: str,
    server: ChildProcess,
    timeout: float = 10.0,
) -> dict[str, object]:
    deadline = time.monotonic() + timeout
    last_health: dict[str, object] | None = None
    while time.monotonic() < deadline:
        server.assert_running("ssserver")
        last_health = client.query()
        evidence = _audit_ingest_evidence(before, last_health, runtime_id)
        if evidence is not None:
            return evidence
        time.sleep(0.2)
    raise RuntimeError(
        "auditd did not prove a signed durable record increment for the benchmark runtime; "
        f"last health={last_health!r}"
    )


def run_case(
    case_name: str,
    binaries: tuple[Path, Path, Path | None],
    echo_port: int,
    identity_key: str,
    user_key: str,
    runtime_statistics: bool,
    runtime_user_audit: bool,
    auditd: AuditdProcessEvidence | None,
    audit_health_client: AuditHealthClient | None,
    producer_identity: pwd.struct_passwd,
    node_id: str,
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
        os.chown(temp, producer_identity.pw_uid, producer_identity.pw_gid)
        server_port = reserve_port()
        local_port = reserve_port()
        statistics_socket = temp / "user-stats.sock" if runtime_statistics else None
        if runtime_user_audit != (auditd is not None and audit_health_client is not None):
            raise RuntimeError("enabled user-audit case requires process and signed health evidence")
        audit_health_before = audit_health_client.query() if audit_health_client is not None else None
        if audit_health_before is not None and audit_health_before.get("producer_connected") is not False:
            raise RuntimeError("benchmark requires a dedicated auditd with no producer connected")
        server_path = temp / "server.json"
        local_path = temp / "local.json"
        secure_write_json(
            server_path,
            server_config(
                server_port,
                identity_key,
                user_key,
                node_id,
                statistics_socket,
                auditd.ingest_socket if auditd is not None else None,
                auditd.user if auditd is not None else None,
            ),
        )
        os.chown(server_path, producer_identity.pw_uid, producer_identity.pw_gid)
        secure_write_json(
            local_path,
            local_config(
                local_port,
                server_port,
                echo_port,
                f"{identity_key}:{user_key}",
            ),
        )
        server = ChildProcess(
            [str(binaries[0]), "-c", str(server_path)],
            temp / "server.log",
            producer_identity,
        )
        local: ChildProcess | None = None
        try:
            if statistics_socket is not None:
                wait_for_socket(statistics_socket, server)
            wait_for_tcp(server_port, [("ssserver", server)])
            local = ChildProcess([str(binaries[1]), "-c", str(local_path)], temp / "local.log")
            processes = {"ssserver": server, "sslocal": local}
            wait_for_tcp(local_port, list(processes.items()))
            time.sleep(0.1)

            statistics_before = (
                query_statistics(statistics_socket) if statistics_socket is not None else None
            )
            counters_before = (
                snapshot_user_counters(statistics_before)
                if statistics_before is not None
                else None
            )

            extra_processes = {"auditd": auditd.pid} if auditd is not None else None
            for _ in range(warmups):
                run_sample(
                    local_port,
                    workload,
                    processes,
                    monitor_interval,
                    extra_processes,
                )
            samples = [
                run_sample(
                    local_port,
                    workload,
                    processes,
                    monitor_interval,
                    extra_processes,
                )
                for _ in range(sample_count)
            ]
            statistics_validation: dict[str, object] | None = None
            runtime_id: str | None = None
            if statistics_socket is not None and counters_before is not None:
                statistics_after = query_statistics(statistics_socket)
                counters_after = snapshot_user_counters(statistics_after)
                runtime_id = str(statistics_after["runtime_id"])
                if statistics_before is None or statistics_before.get("runtime_id") != runtime_id:
                    raise RuntimeError("user-statistics runtime identity changed during benchmark")
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
            audit_ingest: dict[str, object] | None = None
            if auditd is not None:
                if audit_health_client is None or audit_health_before is None or runtime_id is None:
                    raise RuntimeError("enabled audit case lacks health or runtime identity evidence")
                audit_ingest = wait_for_audit_ingest(
                    audit_health_client,
                    audit_health_before,
                    runtime_id,
                    server,
                )
            for label, process in processes.items():
                process.assert_running(label)
            if auditd is not None:
                assert_same_auditd_process(auditd)
            return {
                "name": case_name,
                "runtime_user_stats": runtime_statistics,
                "runtime_user_audit": runtime_user_audit,
                "native_process_measurement": True,
                "producer_user": producer_identity.pw_name,
                "producer_uid": producer_identity.pw_uid,
                "runtime_id": runtime_id,
                "audit_ingest": audit_ingest,
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
    parser.add_argument(
        "--audit-ingest-socket",
        required=True,
        type=Path,
        help="canonical Unix socket of the auditd instance sampled by this run",
    )
    parser.add_argument(
        "--auditd-user",
        required=True,
        help="dedicated account owning the auditd process and ingest socket",
    )
    parser.add_argument(
        "--producer-user",
        required=True,
        help="account used to run benchmark ssserver and accepted by auditd",
    )
    parser.add_argument(
        "--audit-export-socket",
        required=True,
        type=Path,
        help="auditd export socket used for signed before/after health evidence",
    )
    parser.add_argument(
        "--audit-export-user",
        required=True,
        help="account authorized by auditd to query the export socket",
    )
    parser.add_argument(
        "--audit-hmac-key-file",
        required=True,
        type=Path,
        help="auditd HMAC key file; key bytes are never written to the report",
    )
    parser.add_argument(
        "--audit-node-id",
        required=True,
        help="node identifier configured in auditd and user_stats",
    )
    parser.add_argument(
        "--auditd-pid",
        required=True,
        type=int,
        help="PID of the live auditd process sampled by this run",
    )
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

    if platform.system() != "Linux":
        parser.error("native user-audit data-path evidence can only be collected on Linux")
    if os.geteuid() != 0:
        parser.error(
            "native evidence requires root to run ssserver and health queries as distinct service users"
        )

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
    audit_ingest_socket = arguments.audit_ingest_socket.absolute()
    auditd = capture_auditd_process(
        arguments.auditd_pid,
        arguments.auditd_user,
        audit_ingest_socket,
    )
    try:
        producer_identity = pwd.getpwnam(arguments.producer_user)
    except KeyError:
        parser.error(f"--producer-user does not exist: {arguments.producer_user}")
    audit_export_socket = arguments.audit_export_socket.absolute()
    audit_health_client = capture_audit_health_client(
        audit_export_socket,
        arguments.audit_export_user,
        arguments.audit_node_id,
        arguments.audit_hmac_key_file.absolute(),
        auditd.uid,
    )
    if len({auditd.uid, producer_identity.pw_uid, audit_health_client.uid}) != 3:
        parser.error("auditd, producer and export peer must resolve to distinct UIDs")
    run_id = uuid.uuid4().hex

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
            target_root / "plus-user-audit",
            feature="user-audit",
            offline=arguments.offline_build,
            include_auditd=True,
        )
        upstream_artifacts = build_artifact_report(upstream_binaries)
        plus_artifacts = build_artifact_report(plus_binaries)
        built_auditd = plus_binaries[2]
        if built_auditd is None or file_sha256(built_auditd) != auditd.executable_sha256:
            raise RuntimeError(
                "running auditd executable does not match the current plus user-audit build"
            )
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
                    False,
                    None,
                    None,
                    producer_identity,
                    audit_health_client.node_id,
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
                    False,
                    None,
                    None,
                    producer_identity,
                    audit_health_client.node_id,
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
                    True,
                    auditd,
                    audit_health_client,
                    producer_identity,
                    audit_health_client.node_id,
                    workload,
                    arguments.warmups,
                    arguments.samples,
                    arguments.monitor_interval,
                ),
            ]
        assert_same_auditd_process(auditd)
    finally:
        if temporary_target is not None:
            temporary_target.cleanup()

    enabled_aggregate = cases[2]["aggregate"]
    if not isinstance(enabled_aggregate, dict):
        raise RuntimeError("enabled case did not produce aggregate measurements")
    auditd_peak_rss = enabled_aggregate["process_peak_rss_kib_max"].get("auditd")
    auditd_sample_count = enabled_aggregate["process_rss_sample_count_min"].get("auditd")
    if not isinstance(auditd_peak_rss, int) or not isinstance(auditd_sample_count, int):
        raise RuntimeError("enabled case did not collect complete auditd RSS measurements")
    audit_ingest = cases[2].get("audit_ingest")
    if not isinstance(audit_ingest, dict):
        raise RuntimeError("enabled case did not produce signed auditd ingest evidence")
    auditd_report = auditd.report(run_id, auditd_peak_rss, auditd_sample_count)
    auditd_report.update(
        {
            "producer_user": producer_identity.pw_name,
            "producer_uid": producer_identity.pw_uid,
            "export_user": audit_health_client.user,
            "export_uid": audit_health_client.uid,
            "export_socket_path": str(audit_health_client.socket_path),
            "export_socket_device": audit_health_client.socket_device,
            "export_socket_inode": audit_health_client.socket_inode,
            "node_id": audit_health_client.node_id,
            "ingest": audit_ingest,
        }
    )

    report = {
        "schema_version": 1,
        "run_id": run_id,
        "generated_at_utc": dt.datetime.now(dt.timezone.utc).isoformat(),
        "benchmark": "shadowsocks-rust-plus-loopback-data-path",
        "evidence_kind": "native_user_audit_data_path",
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
            "plus_extra_features": ["user-audit"],
            "child_rust_log": "off",
            "artifacts": {
                "upstream": upstream_artifacts,
                "plus_user_audit": plus_artifacts,
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
        "auditd": auditd_report,
    }
    encoded = json.dumps(report, ensure_ascii=False, indent=2) + "\n"
    if arguments.output is None:
        print(encoded, end="")
    else:
        secure_write_report(arguments.output.resolve(), encoded)


if __name__ == "__main__":
    if len(sys.argv) > 1 and sys.argv[1] == "--audit-health-helper":
        raise SystemExit(_audit_health_helper_main(sys.argv[2:]))
    main()
