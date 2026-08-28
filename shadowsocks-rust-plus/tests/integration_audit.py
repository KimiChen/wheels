#!/usr/bin/env python3
"""Linux auditd smoke integration: start daemon and exercise signed health/lease."""

from __future__ import annotations

import argparse
import grp
import json
import os
import platform
import pwd
import re
import signal
import socket
import struct
import subprocess
import sys
import tempfile
import time
from pathlib import Path

from mock_collector import (
    MockCollector,
    canonical_json,
    strict_json,
    unix_http_request,
    verify_response,
)


def _identity(user_name: str) -> pwd.struct_passwd:
    return pwd.getpwnam(user_name)


def _group_id(group_name: str) -> int:
    return grp.getgrnam(group_name).gr_gid


def _drop_privileges(user_name: str, supplementary_groups: tuple[int, ...] = ()) -> None:
    """Drop a child to the same account model used by the systemd units."""

    user = _identity(user_name)
    groups = set(supplementary_groups)
    groups.add(user.pw_gid)
    os.setgroups(sorted(groups))
    os.setgid(user.pw_gid)
    os.setuid(user.pw_uid)


def _preexec_for(user_name: str, supplementary_groups: tuple[int, ...] = ()):
    return lambda: _drop_privileges(user_name, supplementary_groups)


def _build_event(node_id: str, runtime_id: str) -> bytes:
    return (b'{"schema_version":1,"record_type":"access","event_type":"tcp_target_success",'
            b'"event_id":"' + runtime_id.encode() + b':1","audit_sequence":"1",'
            b'"occurred_at_unix_ms":"1","runtime_monotonic_ms":"1","node_id":"'
            + node_id.encode() + b'","runtime_id":"' + runtime_id.encode()
            + b'","server_id":"server","server_generation":1,"identity_kind":"user",'
            b'"identity_name":"user","identity_generation":1,"transport":"tcp",'
            b'"target":{"kind":"ip","host":"192.0.2.1","normalized_host":"192.0.2.1",'
            b'"port":443,"remote_ip":"192.0.2.1"},"success_evidence":"tcp_bidirectional_payload"}')


def _frame(payload: bytes) -> bytes:
    return struct.pack(">I", len(payload)) + payload


def _receive_json_frame(connection: socket.socket) -> dict[str, object]:
    def receive_exact(size: int) -> bytes:
        chunks: list[bytes] = []
        while size:
            chunk = connection.recv(size)
            if not chunk:
                break
            chunks.append(chunk)
            size -= len(chunk)
        return b"".join(chunks)

    header = receive_exact(4)
    if len(header) != 4:
        raise RuntimeError("auditd ingest response header truncated")
    size = struct.unpack(">I", header)[0]
    if size < 1 or size > 64 * 1024:
        raise RuntimeError(f"auditd ingest response has invalid length: {size}")
    payload = receive_exact(size)
    if len(payload) != size:
        raise RuntimeError("auditd ingest response frame truncated")
    value = strict_json(payload)
    if not isinstance(value, dict):
        raise RuntimeError("auditd ingest response is not an object")
    return value


def _validate_hello_ack(value: dict[str, object]) -> None:
    expected = {"protocol_version": 1, "frame_type": "hello_ack", "status": "ready"}
    if value != expected:
        raise RuntimeError(f"auditd rejected or malformed hello ACK: {value!r}")


def _validate_event_ack(value: dict[str, object], runtime_id: str) -> None:
    if set(value) != {
        "protocol_version",
        "frame_type",
        "event_id",
        "status",
        "spool_epoch",
        "spool_sequence",
    }:
        raise RuntimeError(f"auditd returned malformed event ACK fields: {value!r}")
    if (
        value.get("protocol_version") != 1
        or value.get("frame_type") != "ack"
        or value.get("event_id") != f"{runtime_id}:1"
        or value.get("status") != "stored"
        or not isinstance(value.get("spool_epoch"), str)
        or re.fullmatch(r"[0-9a-f]{32}", str(value["spool_epoch"])) is None
        or not isinstance(value.get("spool_sequence"), str)
        or re.fullmatch(r"[1-9][0-9]*", str(value["spool_sequence"])) is None
    ):
        raise RuntimeError(f"auditd returned invalid event ACK: {value!r}")


def _validate_hello_nack(value: dict[str, object], expected_error: str) -> None:
    expected = {
        "protocol_version": 1,
        "frame_type": "hello_nack",
        "error_code": expected_error,
        "retryable": False,
    }
    if value != expected:
        raise RuntimeError(f"unexpected rejected hello response: {value!r}")


def _run_producer(socket_path: Path, node_id: str, runtime_id: str) -> int:
    """Send one hello and one event from the configured producer identity."""

    event = _build_event(node_id, runtime_id)
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as producer:
        producer.settimeout(3)
        producer.connect(str(socket_path))
        hello = json.dumps(
            {"protocol_version": 1, "frame_type": "hello", "node_id": node_id, "runtime_id": runtime_id},
            separators=(",", ":"),
        ).encode()
        producer.sendall(_frame(hello) + _frame(event))
        _validate_hello_ack(_receive_json_frame(producer))
        _validate_event_ack(_receive_json_frame(producer), runtime_id)
    return 0


def _run_rejected_hello(socket_path: Path, node_id: str, runtime_id: str, expected_error: str) -> int:
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as producer:
        producer.settimeout(3)
        producer.connect(str(socket_path))
        hello = canonical_json(
            {"protocol_version": 1, "frame_type": "hello", "node_id": node_id, "runtime_id": runtime_id}
        )
        producer.sendall(_frame(hello))
        response = _receive_json_frame(producer)
    _validate_hello_nack(response, expected_error)
    return 0


def _validate_health_body(status: int, body: bytes, node_id: str) -> None:
    if status != 200:
        raise RuntimeError(f"auditd health must be healthy after durable ACK, got HTTP {status}")
    health = strict_json(body)
    expected_fields = {
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
    if not isinstance(health, dict) or set(health) != expected_fields:
        raise RuntimeError(f"auditd health body has unexpected fields: {health!r}")
    if health["schema_version"] != 1 or health["node_id"] != node_id or health["status"] != "ok":
        raise RuntimeError(f"auditd health body is not healthy: {health!r}")
    if not isinstance(health["producer_connected"], bool):
        raise RuntimeError("auditd health producer_connected is not boolean")
    if not health["producer_connected"] and (
        health["producer_runtime_id"] is not None or health["last_ingest_at_unix_ms"] is not None
    ):
        raise RuntimeError("disconnected auditd health retained producer identity")
    if not isinstance(health["spool_epoch"], str) or re.fullmatch(
        r"[0-9a-f]{32}", health["spool_epoch"]
    ) is None:
        raise RuntimeError("auditd health spool_epoch is invalid")
    for field in (
        "spool_bytes",
        "max_spool_bytes",
        "sealed_batches",
        "stored_records",
        "storage_rejected_attempts",
        "evicted_unacked_records",
    ):
        value = health[field]
        if not isinstance(value, str) or re.fullmatch(r"0|[1-9][0-9]*", value) is None:
            raise RuntimeError(f"auditd health {field} is not a canonical decimal")
    oldest = health["oldest_unacked_at_unix_ms"]
    if oldest is not None and (not isinstance(oldest, str) or re.fullmatch(r"[1-9][0-9]*", oldest) is None):
        raise RuntimeError("auditd health oldest_unacked_at_unix_ms is invalid")
    if canonical_json(health) != body:
        raise RuntimeError("auditd health body is not canonical JSON")


def _run_collector(socket_path: Path, node_id: str, key: bytes, state_path: Path) -> int:
    """Run lease/ACK/health checks from the dedicated export peer identity."""

    collector = MockCollector(node_id, key, state_path)
    if collector.collect_once(socket_path) != 1:
        raise RuntimeError("auditd did not expose the ingested event as a lease")
    nonce = "0123456789abcdef0123456789abcdef"
    request = collector.build_request("GET", "/v1/audit/healthz", nonce=nonce)
    status, headers, body = unix_http_request(socket_path, request)
    verify_response(key, status=status, headers=headers, body=body,
                    request_nonce=nonce, expected_node=node_id)
    _validate_health_body(status, body, node_id)
    return 0


def _run_role(args: argparse.Namespace) -> int:
    if args.role == "producer":
        return _run_producer(Path(args.socket), args.node_id, args.runtime_id)
    if args.role == "collector":
        key_hex = os.environ.get("SSRP_TEST_AUDIT_KEY", "")
        if len(key_hex) != 64:
            raise RuntimeError("missing integration test HMAC key")
        if args.state is None:
            raise RuntimeError("collector role requires --state")
        return _run_collector(Path(args.socket), args.node_id, bytes.fromhex(key_hex), Path(args.state))
    if args.role == "rejected-hello":
        if args.runtime_id is None or args.expected_error is None:
            raise RuntimeError("rejected-hello role requires runtime id and expected error")
        return _run_rejected_hello(
            Path(args.socket), args.node_id, args.runtime_id, args.expected_error
        )
    raise RuntimeError(f"unknown integration role: {args.role}")


def _prepare_identities() -> tuple[dict[str, pwd.struct_passwd], dict[str, int]]:
    """Require the production identities used by the native Linux gate."""

    if os.geteuid() != 0:
        raise RuntimeError("Linux auditd 集成测试需要 root 以按 systemd 身份降权")
    users: dict[str, pwd.struct_passwd] = {}
    for name in ("shadowsocks-audit", "shadowsocks", "audit-exporter"):
        try:
            users[name] = _identity(name)
        except KeyError:
            raise RuntimeError(f"Linux 主机缺少专用账号 {name!r}")
    if len({item.pw_uid for item in users.values()}) != len(users):
        raise RuntimeError("Linux auditd 集成测试要求 daemon、producer、exporter 使用不同 UID")
    groups: dict[str, int] = {}
    for name in ("shadowsocks-audit", "shadowsocks-audit-ingest", "shadowsocks-audit-export"):
        try:
            groups[name] = _group_id(name)
        except KeyError:
            raise RuntimeError(f"Linux 主机缺少专用组 {name!r}")
    for user_name, group_name in (
        ("shadowsocks", "shadowsocks-audit-ingest"),
        ("audit-exporter", "shadowsocks-audit-export"),
    ):
        member_groups = os.getgrouplist(user_name, users[user_name].pw_gid)
        if groups[group_name] not in member_groups:
            raise RuntimeError(f"账号 {user_name!r} 不在组 {group_name!r}")
    return users, groups


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--source", type=Path)
    parser.add_argument("--auditd-binary", type=Path)
    parser.add_argument("--role", choices=("producer", "collector", "rejected-hello"))
    parser.add_argument("--socket", type=Path)
    parser.add_argument("--node-id")
    parser.add_argument("--runtime-id")
    parser.add_argument("--expected-error", choices=("unauthorized_peer", "node_mismatch"))
    parser.add_argument("--state", type=Path)
    args = parser.parse_args()
    if args.role:
        if args.socket is None or args.node_id is None:
            raise SystemExit("integration role requires --socket and --node-id")
        if args.role in ("producer", "rejected-hello") and args.runtime_id is None:
            raise SystemExit(f"{args.role} role requires --runtime-id")
        return _run_role(args)
    if platform.system() != "Linux":
        print("非 Linux 主机：跳过 auditd 真实运行集成测试（需 Linux CI）。")
        return 0
    if args.source is None:
        raise SystemExit("--source is required for the parent integration test")
    identities = _prepare_identities()
    users, groups = identities
    binary = args.auditd_binary or (args.source / "target" / "debug" / "shadowsocks-auditd")
    if not binary.is_file() or not os.access(binary, os.X_OK):
        raise SystemExit(f"Linux auditd 集成测试缺少可执行文件：{binary}")

    daemon_user = users["shadowsocks-audit"]
    producer_user = users["shadowsocks"]
    exporter_user = users["audit-exporter"]
    daemon_group = groups["shadowsocks-audit"]
    ingest_group = groups["shadowsocks-audit-ingest"]
    export_group = groups["shadowsocks-audit-export"]

    # /tmp and /var/tmp are intentionally group-writable and therefore fail
    # auditd's parent-path checks.  A root-owned directory directly under /run
    # models the production path while retaining automatic cleanup.
    try:
        temp = tempfile.TemporaryDirectory(prefix="ssrp-audit-integration-", dir="/run")
    except (FileNotFoundError, PermissionError) as error:
        raise RuntimeError(f"Linux auditd 集成测试无法创建安全临时目录：{error}") from error
    with temp as temp_path:
        root = Path(temp_path)
        os.chown(root, 0, 0)
        os.chmod(root, 0o755)
        run = root / "run"
        run.mkdir()
        os.chown(run, 0, 0)
        os.chmod(run, 0o755)
        ingest_dir = run / "ingest"
        export_dir = run / "export"
        ingest_dir.mkdir()
        export_dir.mkdir()
        for path, gid in ((ingest_dir, ingest_group), (export_dir, export_group)):
            os.chown(path, daemon_user.pw_uid, gid)
            os.chmod(path, 0o750)
        spool = root / "spool"
        config_dir = root / "config"
        config_dir.mkdir()
        os.chown(config_dir, 0, daemon_group)
        os.chmod(config_dir, 0o750)
        key = config_dir / "export-hmac"
        key.write_text("11" * 32 + "\n", encoding="ascii")
        os.chown(key, daemon_user.pw_uid, daemon_group)
        os.chmod(key, 0o600)
        config = json.loads((Path(__file__).parents[1] / "config/auditd.example.json").read_text(encoding="utf-8"))
        config.update({"producer_user": producer_user.pw_name, "export_peer_user": exporter_user.pw_name,
                       "ingest_socket_path": str(run / "ingest/ingest.sock"),
                       "export_socket_path": str(run / "export/export.sock"),
                       "spool_dir": str(spool), "export_hmac_key_file": str(key)})
        config_path = config_dir / "auditd.json"
        config_path.write_text(json.dumps(config), encoding="utf-8")
        os.chown(config_path, 0, daemon_group)
        os.chmod(config_path, 0o640)
        process = subprocess.Popen(
            [str(binary), "--config", str(config_path)],
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            preexec_fn=_preexec_for(daemon_user.pw_name, (ingest_group, export_group)),
        )
        try:
            socket_path = run / "export/export.sock"
            for _ in range(100):
                if socket_path.exists():
                    break
                if process.poll() is not None:
                    _, stderr = process.communicate()
                    raise RuntimeError(stderr.decode(errors="replace"))
                time.sleep(0.05)
            runtime_id = "0123456789abcdef0123456789abcdef"
            ingest_path = run / "ingest/ingest.sock"
            negative_cases = (
                (producer_user, f"{config['node_id']}-wrong", "node_mismatch"),
                (exporter_user, config["node_id"], "unauthorized_peer"),
            )
            for identity, test_node, expected_error in negative_cases:
                rejected = subprocess.run(
                    [sys.executable, str(Path(__file__).resolve()), "--role", "rejected-hello",
                     "--socket", str(ingest_path), "--node-id", test_node, "--runtime-id", runtime_id,
                     "--expected-error", expected_error],
                    capture_output=True,
                    text=True,
                    timeout=10,
                    preexec_fn=_preexec_for(identity.pw_name, (ingest_group,)),
                )
                if rejected.returncode != 0:
                    raise RuntimeError(
                        f"{expected_error} identity negative failed: {rejected.stderr}"
                    )
            producer = subprocess.run(
                [sys.executable, str(Path(__file__).resolve()), "--role", "producer",
                 "--socket", str(ingest_path), "--node-id", config["node_id"], "--runtime-id", runtime_id],
                capture_output=True,
                text=True,
                timeout=10,
                preexec_fn=_preexec_for(producer_user.pw_name, (ingest_group,)),
            )
            if producer.returncode != 0:
                raise RuntimeError(f"producer role failed: {producer.stderr}")
            collector_state_dir = root / "collector"
            collector_state_dir.mkdir(mode=0o700)
            os.chown(collector_state_dir, exporter_user.pw_uid, exporter_user.pw_gid)
            collector_state = collector_state_dir / "state.json"
            collector = subprocess.run(
                [sys.executable, str(Path(__file__).resolve()), "--role", "collector",
                 "--socket", str(socket_path), "--node-id", config["node_id"],
                 "--state", str(collector_state)],
                capture_output=True,
                text=True,
                timeout=10,
                env={**os.environ, "SSRP_TEST_AUDIT_KEY": "11" * 32},
                preexec_fn=_preexec_for(exporter_user.pw_name, (export_group,)),
            )
            if collector.returncode != 0:
                raise RuntimeError(f"collector role failed: {collector.stderr}")
        finally:
            process.send_signal(signal.SIGTERM)
            process.wait(timeout=10)
    print("auditd health 签名集成测试通过。")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
