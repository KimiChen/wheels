#!/usr/bin/env python3
"""Real native managed-Store acceptance through a synthetic TLS controller.

The enhanced frpc/frps binaries and their forwarding/runtime adapters are real.
The management peer is a bounded, authenticated loopback TLS WebSocket fixture;
this is NOT a real GitHub OAuth, monitor administrator API or browser E2E test.
All credentials and configurations are disposable private temporary material.
Only Python's standard library, OpenSSL and existing smoke helpers are used.
"""
from __future__ import annotations

import argparse
import base64
from contextlib import ExitStack, contextmanager
import copy
from datetime import datetime, timezone
import hashlib
import hmac
import http.client
from http.server import ThreadingHTTPServer
import json
import os
from pathlib import Path
import secrets
import signal
import socket
import socketserver
import ssl
import subprocess
import sys
import tempfile
import threading
import time
import uuid

from frp_detail_smoke import MAX_FRAME, replace_private, echo_tick
from frp_smoke import HTTPPayload, UDPEcho, http_matches, local_server, udp_matches
from smoke import (LOOPBACK, PAYLOAD, SmokeFailure, child, child_environment,
                   echo_matches, interrupted, port_open,
                   positive_timeout, reserve_port, verify, wait_for, write_private)


CAPABILITIES = ["metrics.v1", "frp.v1", "frp.detail.v1", "config.manage.v1"]
MAX_PENDING = 16
MAX_HEADER = 16384
EMPTY_STORE = '{"proxies":[],"visitors":[]}\n'
TLS_NAME = "smoke.invalid"
MUX_NAME = "mux.smoke.invalid"
SOAK_RSS_LIMIT_KIB = 512 * 1024
SOAK_REPORT_GAP = 8.0


class PersistentEcho(socketserver.BaseRequestHandler):
    """Keep a real business connection across rate-limited management retries."""

    def handle(self):
        self.request.settimeout(15)
        try:
            while data := self.request.recv(16384):
                self.request.sendall(data)
        except OSError:
            pass


@contextmanager
def tls_backend(certificate, key):
    """Terminate actual HTTPS only behind the native SNI passthrough proxy."""
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    context.minimum_version = ssl.TLSVersion.TLSv1_2
    context.load_cert_chain(certificate, key)
    names = set()
    lock = threading.Lock()

    def observe(_connection, name, _context):
        with lock:
            names.add(name)

    def received_name():
        with lock:
            return TLS_NAME in names

    context.set_servername_callback(observe)
    with ThreadingHTTPServer((LOOPBACK, 0), HTTPPayload) as server:
        server.daemon_threads = True
        server.socket = context.wrap_socket(server.socket, server_side=True)
        thread = threading.Thread(target=server.serve_forever,
                                  kwargs={"poll_interval": .05}, daemon=True)
        thread.start()
        try:
            yield server.server_address[1], received_name
        finally:
            server.shutdown()
            thread.join(timeout=2)
            if thread.is_alive():
                raise SmokeFailure("local TLS target did not stop")


def https_matches(port, certificate, hostname=TLS_NAME):
    # Connect to loopback directly: the reserved hostname is TLS SNI and a
    # verified certificate identity, never a DNS lookup or external endpoint.
    context = ssl.create_default_context(cafile=str(certificate))
    try:
        with socket.create_connection((LOOPBACK, port), timeout=.8) as raw:
            with context.wrap_socket(raw, server_hostname=hostname) as connection:
                connection.sendall((f"GET /payload HTTP/1.1\r\nHost: {hostname}\r\n"
                                    "Connection: close\r\n\r\n").encode())
                with http.client.HTTPResponse(connection) as response:
                    response.begin()
                    data = response.read(len(PAYLOAD) + 1)
                    if response.status != 200:
                        return False
                    if data != PAYLOAD:
                        raise SmokeFailure("HTTPS tunnel altered the response payload")
                    return True
    except (OSError, http.client.HTTPException):
        return False


def connect_matches(port, hostname=MUX_NAME):
    try:
        with socket.create_connection((LOOPBACK, port), timeout=.8) as connection:
            connection.sendall((f"CONNECT {hostname}:443 HTTP/1.1\r\n"
                                f"Host: {hostname}:443\r\n\r\n").encode())
            header = bytearray()
            while not header.endswith(b"\r\n\r\n"):
                value = connection.recv(1)
                if not value:
                    return False
                header.extend(value)
                if len(header) > MAX_HEADER:
                    raise SmokeFailure("CONNECT response header exceeded limit")
            if not bytes(header).split(b"\r\n", 1)[0].startswith(b"HTTP/1.1 200 "):
                return False
            connection.sendall(PAYLOAD)
            data = bytearray()
            while len(data) < len(PAYLOAD):
                value = connection.recv(min(16384, len(PAYLOAD) - len(data)))
                if not value:
                    return False
                data.extend(value)
            if data != PAYLOAD:
                raise SmokeFailure("CONNECT tunnel altered the echoed payload")
            return True
    except OSError:
        return False


def ws_frame(payload: bytes, opcode: int = 1) -> bytes:
    if len(payload) > MAX_FRAME or (opcode >= 8 and len(payload) > 125):
        raise SmokeFailure("controller outbound frame exceeded limit")
    prefix = bytes([128 | opcode])
    if len(payload) < 126:
        return prefix + bytes([len(payload)]) + payload
    if len(payload) < 65536:
        return prefix + b"\x7e" + len(payload).to_bytes(2, "big") + payload
    return prefix + b"\x7f" + len(payload).to_bytes(8, "big") + payload


class ControlFrames:
    """Bounded masked-client parser, including fragments and immediate ping/pong."""

    def __init__(self, observe, control):
        self.observe, self.control = observe, control
        self.buffer = bytearray()
        self.fragment = None

    def feed(self, chunk: bytes) -> None:
        # The socket reads at most 64 KiB; also bound direct/test callers.
        if len(chunk) > MAX_FRAME + 14 or len(self.buffer) + len(chunk) > 2 * MAX_FRAME + 28:
            raise SmokeFailure("controller receive buffer exceeded limit")
        self.buffer.extend(chunk)
        while len(self.buffer) >= 2:
            first, second = self.buffer[:2]
            opcode, final = first & 15, bool(first & 128)
            if first & 112 or not second & 128 or opcode not in (0, 1, 8, 9, 10):
                raise SmokeFailure("controller received invalid WebSocket framing")
            length, offset = second & 127, 2
            extra = 2 if length == 126 else 8 if length == 127 else 0
            if len(self.buffer) < offset + extra:
                return
            if extra:
                length = int.from_bytes(self.buffer[offset:offset + extra], "big")
                offset += extra
            if length > MAX_FRAME or (opcode >= 8 and (length > 125 or not final)):
                raise SmokeFailure("controller inbound frame exceeded limit")
            if len(self.buffer) < offset + 4 + length:
                return
            mask = self.buffer[offset:offset + 4]
            offset += 4
            payload = bytes(value ^ mask[index % 4] for index, value in enumerate(self.buffer[offset:offset + length]))
            del self.buffer[:offset + length]
            if opcode >= 8:
                self.control(opcode, payload)
                continue
            if opcode == 1:
                if self.fragment is not None:
                    raise SmokeFailure("controller received overlapping messages")
                self.fragment = bytearray()
            elif self.fragment is None:
                raise SmokeFailure("controller received orphan continuation")
            self.fragment.extend(payload)
            if len(self.fragment) > MAX_FRAME:
                raise SmokeFailure("controller inbound message exceeded limit")
            if final:
                try:
                    value = json.loads(self.fragment)
                except (UnicodeError, json.JSONDecodeError, RecursionError) as exc:
                    raise SmokeFailure("controller received invalid JSON") from exc
                self.fragment = None
                self.observe(value)


def authenticate_upgrade(raw: bytes, token: str) -> tuple[int, str]:
    """Validate credentials before advertising any management capability."""
    if len(raw) > MAX_HEADER or not raw.endswith(b"\r\n\r\n"):
        return 400, ""
    try:
        lines = raw.decode("ascii").split("\r\n")
        if lines[0] != "GET /agent/v1/ws HTTP/1.1":
            return 400, ""
        headers = {}
        for line in lines[1:-2]:
            key, value = line.split(":", 1)
            key = key.lower()
            if key in headers:
                return 400, ""
            headers[key] = value.strip()
        if not hmac.compare_digest(headers.get("authorization", ""), "Bearer " + token):
            return 401, ""
        key = headers.get("sec-websocket-key", "")
        if (headers.get("upgrade", "").lower() != "websocket"
                or "upgrade" not in [item.strip() for item in headers.get("connection", "").lower().split(",")]
                or headers.get("sec-websocket-version") != "13"
                or len(base64.b64decode(key, validate=True)) != 16):
            return 400, ""
        return 101, base64.b64encode(hashlib.sha1((key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").encode()).digest()).decode()
    except (ValueError, UnicodeError):
        return 400, ""


class Controller:
    """One receiver, one active connection, bounded in-flight result storage."""

    def __init__(self, certificate: Path, key: Path, token: str, hidden=()):
        self.context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        self.context.minimum_version = ssl.TLSVersion.TLSv1_2
        self.context.load_cert_chain(certificate, key)
        self.token, self.hidden = token, tuple(hidden)
        self.lock, self.write_lock = threading.RLock(), threading.Lock()
        self.listener = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        self.listener.bind((LOOPBACK, 0))
        self.listener.listen(4)
        self.listener.settimeout(.2)
        self.port = self.listener.getsockname()[1]
        self.connection = None
        self.enabled, self.stopped, self.fault = True, False, None
        self.session, self.sequence, self.down_sequence = None, 0, 1
        self.sessions, self.reports, self.results_seen = 0, 0, 0
        self.last_report_at, self.pending_peak = 0.0, 0
        self.detail, self.pending = None, {}
        self.last_sent = 0.0
        self.thread = threading.Thread(target=self._accept, daemon=True)
        self.thread.start()

    def _write(self, connection, payload, opcode=1):
        with self.write_lock:
            connection.sendall(ws_frame(payload, opcode))

    def _observe(self, frame, connection):
        serialized = json.dumps(frame, ensure_ascii=False)
        if any(value and value in serialized for value in self.hidden):
            raise SmokeFailure("native uplink leaked private material")
        if not isinstance(frame, dict) or frame.get("jsonrpc") != "2.0" or not isinstance(frame.get("params"), dict):
            raise SmokeFailure("controller received invalid RPC envelope")
        method, params = frame.get("method"), frame["params"]
        session, sequence = params.get("session_id"), params.get("sequence")
        if params.get("schema") != 1 or not isinstance(session, str) or not session or type(sequence) is not int or not 0 < sequence < 2 ** 64:
            raise SmokeFailure("controller received invalid session metadata")
        with self.lock:
            if method == "hello":
                if self.session is not None or sequence != 1 or not set(CAPABILITIES).issubset(params.get("capabilities", [])):
                    raise SmokeFailure("native management capabilities were not offered")
                reply = {"jsonrpc": "2.0", "id": "hello", "result": {"schema": 1, "session_id": session,
                         "capabilities": CAPABILITIES, "report_interval": 1}}
                self._write(connection, json.dumps(reply).encode())
                self.session, self.sequence, self.down_sequence = session, 1, 1
                self.sessions += 1
                return
            if session != self.session or sequence <= self.sequence:
                raise SmokeFailure("metrics, detail and control results did not share increasing sequence")
            self.sequence = sequence
            if method == "report":
                if not isinstance(params.get("metrics"), dict):
                    raise SmokeFailure("native host metrics stopped carrying measurements")
                self.reports += 1
                self.last_report_at = time.monotonic()
            elif method == "frp.detail":
                if not isinstance(params.get("detail"), dict):
                    raise SmokeFailure("native detail payload is invalid")
                self.detail = copy.deepcopy(params["detail"])
            elif method == "config.result":
                request = params.get("request_id")
                if request not in self.pending or self.pending[request] is not None:
                    raise SmokeFailure("controller received unexpected or duplicate result")
                self.pending[request] = copy.deepcopy(params)
                self.results_seen += 1
            else:
                raise SmokeFailure("controller received unnegotiated method")

    def _accept(self):
        while not self.stopped:
            try:
                raw, _ = self.listener.accept()
            except socket.timeout:
                continue
            except OSError:
                return
            connection = None
            try:
                raw.settimeout(2)
                if not self.enabled:
                    continue
                connection = self.context.wrap_socket(raw, server_side=True)
                header = bytearray()
                while b"\r\n\r\n" not in header and len(header) <= MAX_HEADER:
                    data = connection.recv(4096)
                    if not data:
                        break
                    header.extend(data)
                end = header.find(b"\r\n\r\n")
                status, accept = authenticate_upgrade(bytes(header[:end + 4]) if end >= 0 else bytes(header), self.token)
                if status != 101:
                    connection.sendall(f"HTTP/1.1 {status} Rejected\r\nContent-Length: 0\r\nConnection: close\r\n\r\n".encode())
                    continue
                connection.sendall(("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"
                                    f"Sec-WebSocket-Accept: {accept}\r\nX-Frp-Plus-Capabilities: frp.detail.v1, config.manage.v1\r\n\r\n").encode())
                with self.lock:
                    self.connection, self.session, self.detail = connection, None, None
                    self.pending.clear()
                connection.settimeout(.25)

                def control(opcode, payload):
                    if opcode == 9:
                        self._write(connection, payload, 10)
                    elif opcode == 8:
                        raise ConnectionAbortedError()

                parser = ControlFrames(lambda frame: self._observe(frame, connection), control)
                parser.feed(bytes(header[end + 4:]))
                header.clear()
                while self.enabled and not self.stopped:
                    try:
                        data = connection.recv(65536)
                    except socket.timeout:
                        continue
                    if not data:
                        break
                    parser.feed(data)
            except SmokeFailure as exc:
                with self.lock:
                    self.fault = str(exc)
            except (OSError, ValueError):
                pass
            finally:
                with self.lock:
                    if self.connection is connection:
                        self.connection, self.session, self.detail = None, None, None
                        self.pending.clear()
                if connection is not None:
                    connection.close()
                raw.close()

    def snapshot(self):
        with self.lock:
            if self.fault:
                raise SmokeFailure(self.fault)
            return {"session": self.session, "sessions": self.sessions, "reports": self.reports,
                    "results": self.results_seen, "detail": copy.deepcopy(self.detail),
                    "sequence": self.sequence, "last_report_at": self.last_report_at,
                    "pending": len(self.pending), "pending_peak": self.pending_peak}

    def send(self, action, service_id="", *, command_seconds=10, **fields):
        # Agent downlink is a token bucket of 1 command/s, burst 4. Pacing makes
        # correctness independent of how quickly the local machine replies.
        pause = 1.05 - (time.monotonic() - self.last_sent)
        if pause > 0:
            time.sleep(pause)
        with self.lock:
            self.snapshot()
            if self.connection is None or self.session is None:
                raise SmokeFailure("controller has no authenticated agent session")
            if len(self.pending) >= MAX_PENDING:
                raise SmokeFailure("controller pending result limit exceeded")
            self.down_sequence += 1
            command = make_command(action, service_id, self.session, self.down_sequence,
                                   command_seconds=command_seconds, **fields)
            request = command["request_id"]
            self.pending[request] = None
            self.pending_peak = max(self.pending_peak, len(self.pending))
            try:
                self._write(self.connection, json.dumps({"jsonrpc": "2.0", "method": "config.command", "params": command}).encode())
            except OSError as exc:
                self.pending.pop(request, None)
                raise SmokeFailure("controller command connection was lost") from exc
            self.last_sent = time.monotonic()
            return request

    def receive(self, request, processes, timeout):
        result = []

        def available():
            with self.lock:
                self.snapshot()
                if request not in self.pending:
                    raise SmokeFailure("controller result session was lost")
                if self.pending[request] is not None:
                    result.append(self.pending.pop(request))
                    return True
                return False

        wait_for("native configuration result", available, processes, timeout)
        return result[0]

    def request(self, action, service_id, processes, timeout, **fields):
        return self.receive(self.send(action, service_id, **fields), processes, timeout)

    def disconnect(self):
        with self.lock:
            self.enabled = False
            connection = self.connection
        if connection is not None:
            try:
                connection.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass

    def resume(self):
        self.enabled = True

    def stop(self):
        self.stopped = True
        self.disconnect()
        self.listener.close()
        self.thread.join(timeout=3)
        if self.thread.is_alive():
            raise SmokeFailure("TLS controller did not stop")


def make_command(action, service_id, session, sequence, *, command_seconds=10, **fields):
    now = datetime.now(timezone.utc)
    command = {"schema": 1, "session_id": session, "sequence": sequence,
               "collected_at": now.isoformat(timespec="milliseconds").replace("+00:00", "Z"),
               "request_id": str(uuid.uuid4()), "service_id": service_id, "action": action,
               "operation_id": "", "base_revision": "", "context_revision": "", "candidate_digest": "",
               "idempotency_key": "", "deadline_at_ms": int(now.timestamp() * 1000) + int(command_seconds * 1000),
               "operation_deadline_at_ms": 0, "changes": []}
    command.update(fields)
    return command


def change(name, kind="proxy", type_="tcp", fields=None, reference=None):
    return {"operation": "create", "kind": kind, "name": name, "type": type_,
            "fields": [{"path": key, "value": value} for key, value in (fields or {}).items()],
            "secrets": [{"path": "secretKey", "mode": "reference", "reference": reference}] if reference else []}


def operation_fields(prepared):
    operation = prepared.get("operation") or {}
    return {key: operation.get(key, "") for key in ("operation_id", "base_revision", "context_revision", "candidate_digest")}


def expect_operation(result, state):
    operation = result.get("operation") or {}
    expected_code = "conflict" if state == "conflict" else "ok"
    if result.get("code") != expected_code or operation.get("state") != state:
        # Only closed-vocabulary expectations are logged, never server data.
        raise SmokeFailure(f"native transaction did not reach expected {state} state")
    if state in ("confirmed", "rolled_back"):
        if (not all(operation.get(key) is True for key in ("runtime_loaded", "resources_ready"))
                or any(operation.get(key) is not (state == "confirmed") for key in ("store_persisted", "runtime_applied"))):
            raise SmokeFailure("completed transaction omitted actual persistence/runtime/resource facts")
        if operation.get("business_checked") is not False:
            raise SmokeFailure("native adapter falsely claimed application business verification")
    return operation


def tls_material(root: Path):
    certificate, key = root / "controller.crt", root / "controller.key"
    write_private(certificate, "")
    write_private(key, "")
    result = subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
                             "-subj", "/CN=frp-plus-config-smoke", "-addext", "subjectAltName=IP:127.0.0.1,DNS:smoke.invalid",
                             "-keyout", str(key), "-out", str(certificate)], cwd=root, env=child_environment(),
                            stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                            timeout=30, check=False)
    if result.returncode:
        raise SmokeFailure("disposable TLS certificate generation failed")
    certificate.chmod(0o600)
    key.chmod(0o600)
    return certificate, key


def reject_unauthenticated(controller, certificate):
    context = ssl.create_default_context(cafile=str(certificate))
    connection = http.client.HTTPSConnection(LOOPBACK, controller.port, context=context, timeout=3)
    try:
        connection.request("GET", "/agent/v1/ws", headers={"Authorization": "Bearer invalid-disposable-token",
                           "Upgrade": "websocket", "Connection": "Upgrade", "Sec-WebSocket-Version": "13",
                           "Sec-WebSocket-Key": base64.b64encode(secrets.token_bytes(16)).decode()})
        response = connection.getresponse()
        response.read(1024)
        if response.status != 401 or response.getheader("X-Frp-Plus-Capabilities"):
            raise SmokeFailure("synthetic TLS controller did not enforce Bearer authentication")
    finally:
        connection.close()


def dashboard_request(port, password, method, path, body=None, *, json_response=False):
    connection = http.client.HTTPConnection(LOOPBACK, port, timeout=3)
    try:
        connection.request(method, path, body=body, headers={"Authorization": "Basic " + base64.b64encode(("smoke:" + password).encode()).decode(),
                           "Content-Type": "application/json"})
        response = connection.getresponse()
        raw = response.read(MAX_FRAME + 1)
        if len(raw) > MAX_FRAME:
            raise SmokeFailure("native dashboard response exceeded limit")
        if json_response:
            if response.status != 200:
                raise SmokeFailure("native dashboard status request failed")
            return json.loads(raw)
        return response.status
    finally:
        connection.close()


def process_stopped(pid):
    result = subprocess.run(["ps", "-o", "stat=", "-p", str(pid)], stdin=subprocess.DEVNULL,
                            stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, timeout=2,
                            env=child_environment(), check=False)
    state = result.stdout.strip()
    if result.returncode or not state:
        raise SmokeFailure("native server suspension could not be observed")
    return state.startswith(b"T")


@contextmanager
def suspended_server(service):
    """Hold registration replies without manufacturing a native start error."""
    if os.name != "posix" or not hasattr(signal, "SIGSTOP"):
        raise SmokeFailure("deadline acceptance requires local process suspension support")
    service.ensure_running()
    try:
        service._signal(signal.SIGSTOP)
        wait_for("observed native server suspension", lambda: process_stopped(service.process.pid), (service,), 3)
        yield
    finally:
        # Resume before Child.stop sends TERM, including setup/body failures.
        # These are process groups created only for this disposable fixture.
        service._signal(signal.SIGCONT)


def journal_state(root, operation_id):
    name = hashlib.sha256(operation_id.encode()).hexdigest() + ".json"
    try:
        raw = (root / "operations" / name).read_bytes()
        if len(raw) > 16384:
            raise SmokeFailure("local transaction journal exceeded limit")
        return json.loads(raw).get("state")
    except FileNotFoundError:
        return None


def store_bytes(path):
    raw = path.read_bytes()
    if len(raw) > 1024 * 1024:
        raise SmokeFailure("native Store exceeded smoke size limit")
    return raw


def verify_private_tree(root):
    for path in (root, *root.rglob("*")):
        if path.is_symlink():
            raise SmokeFailure("managed transaction created a symlink")
        mode = path.stat().st_mode & 0o777
        if mode != (0o700 if path.is_dir() else 0o600):
            raise SmokeFailure("managed transaction material permissions are not private")


def soak_duration(value):
    try:
        seconds = int(value)
    except (TypeError, ValueError) as exc:
        raise argparse.ArgumentTypeError("soak duration must be 0 or 30..3600 whole seconds") from exc
    if seconds != 0 and not 30 <= seconds <= 3600:
        raise argparse.ArgumentTypeError("soak duration must be 0 or 30..3600 whole seconds")
    return seconds


def process_rss_kib(pid):
    # Both macOS and Linux ps report RSS in KiB. Never request argv/environment,
    # and fail explicitly if this test host cannot provide a numeric sample.
    try:
        result = subprocess.run(["ps", "-o", "rss=", "-p", str(pid)],
                                stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                                stderr=subprocess.DEVNULL, timeout=2,
                                env=child_environment(), check=False)
    except (OSError, subprocess.TimeoutExpired) as exc:
        raise SmokeFailure("soak Agent RSS sampling unavailable") from exc
    raw = result.stdout.strip()
    if result.returncode or not raw.isdigit() or len(raw) > 12 or int(raw) <= 0:
        raise SmokeFailure("soak Agent RSS sampling unavailable")
    return int(raw)


class SoakWitness:
    """One never-reconnected TCP stream and a separately sampled health gate."""

    def __init__(self, port, controller, processes, agent_pid):
        self.controller, self.processes, self.agent_pid = controller, processes, agent_pid
        self.initial = controller.snapshot()
        if not self.initial["session"]:
            raise SmokeFailure("soak requires an authenticated monitoring session")
        self.started = time.monotonic()
        self.stop_event, self.lock = threading.Event(), threading.Lock()
        self.fault, self.ticks, self.rss_peak_kib = None, 0, 0
        self.connection = socket.create_connection((LOOPBACK, port), timeout=1)
        self.connection.settimeout(1)
        self.threads = [threading.Thread(target=self._echo, daemon=True),
                        threading.Thread(target=self._health, daemon=True)]
        for thread in self.threads:
            thread.start()

    def _fail(self, message):
        with self.lock:
            if self.fault is None:
                self.fault = message
        self.stop_event.set()

    def _echo(self):
        next_tick = time.monotonic()
        while not self.stop_event.is_set():
            try:
                echo_tick(self.connection)
                with self.lock:
                    self.ticks += 1
            except (OSError, SmokeFailure):
                if not self.stop_event.is_set():
                    self._fail("soak unedited TCP stream was interrupted or corrupted")
                return
            next_tick += .2
            # Avoid a burst after scheduler delay; the goal is sustained traffic.
            next_tick = max(next_tick, time.monotonic())
            self.stop_event.wait(max(0, next_tick - time.monotonic()))

    def _health(self):
        while not self.stop_event.is_set():
            try:
                for process in self.processes:
                    process.ensure_running()
                state = self.controller.snapshot()
                if state["session"] != self.initial["session"] or state["sessions"] != self.initial["sessions"]:
                    raise SmokeFailure("soak monitoring session changed")
                if time.monotonic() - state["last_report_at"] > SOAK_REPORT_GAP:
                    raise SmokeFailure("soak monitoring reports stopped advancing")
                if state["pending"] > 1 or state["pending_peak"] > 1:
                    raise SmokeFailure("soak pending control results exceeded single-request bound")
                rss = process_rss_kib(self.agent_pid)
                with self.lock:
                    self.rss_peak_kib = max(self.rss_peak_kib, rss)
                if rss > SOAK_RSS_LIMIT_KIB:
                    raise SmokeFailure("soak Agent RSS exceeded the test-only 512 MiB ceiling")
            except SmokeFailure as exc:
                self._fail(str(exc))
                return
            self.stop_event.wait(1)

    def check(self):
        with self.lock:
            if self.fault:
                raise SmokeFailure(self.fault)

    def finish(self, seconds):
        self.check()
        state = self.controller.snapshot()
        with self.lock:
            ticks, rss = self.ticks, self.rss_peak_kib
        if time.monotonic() - self.started < seconds or ticks < seconds * 2 or rss <= 0:
            raise SmokeFailure("soak did not collect enough persistent traffic or RSS evidence")
        if state["reports"] - self.initial["reports"] < seconds // 2 or state["sequence"] <= self.initial["sequence"]:
            raise SmokeFailure("soak did not collect enough advancing same-session reports")
        if state["pending"] != 0 or state["session"] != self.initial["session"] or state["sessions"] != self.initial["sessions"]:
            raise SmokeFailure("soak ended with a pending request or changed session")
        return {"seconds": time.monotonic() - self.started, "ticks": ticks,
                "reports": state["reports"] - self.initial["reports"],
                "rss_peak_kib": rss, "pending_peak": state["pending_peak"]}

    def __enter__(self):
        return self

    def __exit__(self, *_):
        self.stop_event.set()
        try:
            self.connection.shutdown(socket.SHUT_RDWR)
        except OSError:
            pass
        self.connection.close()
        for thread in self.threads:
            thread.join(timeout=3)
            if thread.is_alive():
                raise SmokeFailure("soak witness thread did not stop within its deadline")
        self.check()


def run(agent: Path, server: Path, wire: str, timeout: float, soak_seconds: int = 0):
    cache = Path(__file__).resolve().parents[1] / ".cache"
    cache.mkdir(exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="config-smoke-", dir=cache) as temporary, ExitStack() as stack:
        root = Path(temporary).resolve(strict=True)
        root.chmod(0o700)
        managed = root / "managed"
        managed.mkdir(mode=0o700)
        store = managed / "store.json"
        write_private(store, EMPTY_STORE)
        old = store_bytes(store)
        certificate, key = tls_material(root)
        control_token, frp_token, native_secret, password = [secrets.token_hex(32) for _ in range(4)]
        write_private(root / "agent.token", control_token + "\n")
        controller = Controller(certificate, key, control_token, (control_token, frp_token, native_secret, password, str(root)))
        stack.callback(controller.stop)
        reject_unauthenticated(controller, certificate)
        reserved = [stack.enter_context(reserve_port()) for _ in range(8)]
        (control_port, http_port, visitor_port, dashboard_port, remote_port,
         https_port, mux_port, xtcp_port) = [item.getsockname()[1] for item in reserved]
        udp_sockets = [stack.enter_context(socket.socket(socket.AF_INET, socket.SOCK_DGRAM)) for _ in range(3)]
        for connection in udp_sockets:
            connection.bind((LOOPBACK, 0))
        udp_port, sudp_port, stun_port = [connection.getsockname()[1] for connection in udp_sockets]
        tcp_local = stack.enter_context(local_server(socketserver.ThreadingTCPServer, PersistentEcho))
        udp_local = stack.enter_context(local_server(socketserver.UDPServer, UDPEcho))
        http_local = stack.enter_context(local_server(ThreadingHTTPServer, HTTPPayload))
        https_local, received_sni = stack.enter_context(tls_backend(certificate, key))
        common = f'auth.method = "token"\nauth.token = "{frp_token}"\nlog.to = "console"\nlog.level = "warn"\n'
        server_path, agent_path = root / "server.toml", root / "agent.toml"
        write_private(server_path, f'bindAddr = "{LOOPBACK}"\nproxyBindAddr = "{LOOPBACK}"\nbindPort = {control_port}\n'
                      f'vhostHTTPPort = {http_port}\nvhostHTTPSPort = {https_port}\n'
                      f'tcpmuxHTTPConnectPort = {mux_port}\n' + common)
        main = (f'serverAddr = "{LOOPBACK}"\nserverPort = {control_port}\nclientID = "1"\nloginFailExit = false\n'
                f'transport.protocol = "tcp"\ntransport.wireProtocol = "{wire}"\ntransport.tls.enable = true\n'
                f'natHoleStunServer = "{LOOPBACK}:{stun_port}"\n'
                f'store.path = {json.dumps(str(store))}\nwebServer.addr = "{LOOPBACK}"\nwebServer.port = {dashboard_port}\n'
                f'webServer.user = "smoke"\nwebServer.password = "{password}"\n' + common +
                f'\n[telemetry]\nenabled = true\nendpoint = "wss://{LOOPBACK}:{controller.port}/agent/v1/ws"\n'
                f'tokenFile = {json.dumps(str(root / "agent.token"))}\ncaFile = {json.dumps(str(certificate))}\n'
                'serverID = "config-smoke"\nintervalSeconds = 1\n'
                f'\n[telemetry.configManagement]\nenabled = true\nroot = {json.dumps(str(managed))}\n')
        write_private(agent_path, main)
        for binary, config in ((agent, agent_path), (server, server_path)):
            verify(binary, config, "managed smoke configuration", timeout)
        for reservation in reserved:
            reservation.close()
        for connection in udp_sockets[:2]:
            connection.close()
        service = stack.enter_context(child("managed FRP server", [str(server), "-c", str(server_path)], root))
        wait_for("native server listeners", lambda: all(port_open(port) for port in
                 (control_port, http_port, https_port, mux_port)), (service,), timeout)
        client = stack.enter_context(child("managed FRP agent", [str(agent), "-c", str(agent_path)], root))
        processes = (service, client)
        wait_for("authenticated management session", lambda: bool(controller.snapshot()["session"]) and port_open(dashboard_port), processes, timeout)

        def request(action, service_id="", **fields):
            return controller.request(action, service_id, processes, timeout, **fields)

        def inspect():
            result = request("inspect")
            if result.get("code") != "ok" or (result.get("inventory") or {}).get("state") != "ready":
                raise SmokeFailure("native managed inventory is not writable and ready")
            return result

        def prepare(changes, ttl=120):
            inventory = inspect()
            intent = {"operation_id": str(uuid.uuid4()), "idempotency_key": str(uuid.uuid4()),
                      "base_revision": inventory["inventory"]["revision"], "changes": changes,
                      "operation_deadline_at_ms": int(time.time() * 1000) + ttl * 1000}
            result = request("prepare", inventory["service_id"], command_seconds=min(10, ttl - 2), **intent)
            expect_operation(result, "prepared")
            if not isinstance(result.get("preview"), dict):
                raise SmokeFailure("native prepare omitted its safe review preview")
            return result, intent

        initial = inspect()
        service_id = initial["service_id"]
        if initial["inventory"]["objects"]:
            raise SmokeFailure("isolated smoke did not start with an empty Store")
        first_session = controller.snapshot()
        reference = str(uuid.uuid4())
        uploaded = request("secret", service_id, secret={"reference": reference, "value": native_secret})
        if uploaded.get("code") != "ok" or uploaded.get("secret_reference") != reference:
            raise SmokeFailure("private local secret reference could not be created")
        if request("secret", service_id, secret={"reference": reference, "value": native_secret}).get("code") != "ok":
            raise SmokeFailure("identical secret reference retry was not idempotent")
        if request("secret", service_id, secret={"reference": reference, "value": "different-disposable-value"}).get("code") != "conflict":
            raise SmokeFailure("existing private secret reference was mutable")

        changes = [change("managed-tcp", fields={"localIP": LOOPBACK, "localPort": tcp_local, "remotePort": remote_port}),
                   change("managed-udp", type_="udp", fields={"localIP": LOOPBACK, "localPort": udp_local, "remotePort": udp_port}),
                   change("managed-http", type_="http", fields={"localIP": LOOPBACK, "localPort": http_local, "customDomains": ["smoke.invalid"]}),
                   change("managed-private", type_="stcp", fields={"localIP": LOOPBACK, "localPort": tcp_local}, reference=reference),
                   change("managed-visitor", "visitor", "stcp", {"serverName": "managed-private", "bindAddr": LOOPBACK, "bindPort": visitor_port}, reference),
                   change("managed-https", type_="https", fields={"localIP": LOOPBACK, "localPort": https_local, "customDomains": [TLS_NAME]}),
                   change("managed-mux", type_="tcpmux", fields={"localIP": LOOPBACK, "localPort": tcp_local, "customDomains": [MUX_NAME], "multiplexer": "httpconnect"}),
                   change("managed-datagram", type_="sudp", fields={"localIP": LOOPBACK, "localPort": udp_local}, reference=reference),
                   change("managed-sudp-visitor", "visitor", "sudp", {"serverName": "managed-datagram", "bindAddr": LOOPBACK, "bindPort": sudp_port}, reference),
                   change("managed-xtcp", type_="xtcp", fields={"localIP": LOOPBACK, "localPort": tcp_local}, reference=reference),
                   change("managed-xtcp-visitor", "visitor", "xtcp", {"serverName": "absent-xtcp", "bindAddr": LOOPBACK, "bindPort": xtcp_port,
                          "protocol": "quic", "keepTunnelOpen": False, "fallbackTo": "managed-visitor", "fallbackTimeoutMs": 200}, reference)]
        prepared, intent = prepare(changes)
        if (store_bytes(store) != old or any(port_open(port) for port in (remote_port, visitor_port, xtcp_port))
                or http_matches(http_port) or https_matches(https_port, certificate)
                or connect_matches(mux_port) or udp_matches(sudp_port)):
            raise SmokeFailure("prepare changed Store bytes or native forwarding")
        replay = request("prepare", service_id, **intent)
        if expect_operation(replay, "prepared") != prepared["operation"]:
            raise SmokeFailure("identical prepare retry changed the durable transaction")
        applied = request("apply", service_id, **operation_fields(prepared))
        expect_operation(applied, "confirmed")
        queried = request("query", service_id, **operation_fields(prepared))
        expect_operation(queried, "confirmed")
        wait_for("managed TCP forwarding", lambda: echo_matches(remote_port), processes, timeout)
        wait_for("managed UDP forwarding", lambda: udp_matches(udp_port), processes, timeout)
        wait_for("managed HTTP forwarding", lambda: http_matches(http_port), processes, timeout)
        wait_for("managed STCP visitor forwarding", lambda: echo_matches(visitor_port), processes, timeout)
        wait_for("managed HTTPS with verified certificate and SNI", lambda: https_matches(https_port, certificate) and received_sni(), processes, timeout)
        wait_for("managed TCPMUX CONNECT forwarding", lambda: connect_matches(mux_port), processes, timeout)
        wait_for("managed SUDP visitor datagram forwarding", lambda: udp_matches(sudp_port), processes, timeout)
        # The configured XTCP proxy is registered, but the Visitor intentionally
        # targets a different absent peer. Native PreCheck rejects it before STUN
        # so the test can prove actual STCP fallback without claiming NAT/P2P.
        with socket.create_connection((LOOPBACK, xtcp_port), timeout=2) as fallback:
            fallback.settimeout(2)

            def fallback_active():
                echo_tick(fallback)
                detail = controller.snapshot()["detail"] or {}
                proxy = next((item for item in detail.get("proxies", []) if item["name"] == "managed-xtcp"), {})
                visitor = next((item for item in detail.get("visitors", []) if item["name"] == "managed-xtcp-visitor"), {})
                return (proxy.get("status") == "running" and visitor.get("fallback_state") == "active"
                        and visitor.get("p2p_state") == "failed" and visitor.get("remote_state") == "error")

            wait_for("managed XTCP registration and actual STCP fallback", fallback_active, processes, timeout)
        print(f"PASS config {wire}: TLS auth, private secret reference, prepare isolation, confirmed 8 Proxy/3 Visitor types", flush=True)
        print(f"PASS config {wire}: TCP/UDP/HTTP/STCP, HTTPS CA+SNI, TCPMUX CONNECT, SUDP datagrams; XTCP uses actual STCP fallback (P2P failed)", flush=True)

        held = stack.enter_context(socket.create_connection((LOOPBACK, remote_port), timeout=1))
        held.settimeout(1)
        echo_tick(held)
        fingerprint = (store_bytes(store), store.stat().st_mtime_ns)
        for action, fields in (("apply", operation_fields(prepared)), ("prepare", intent)):
            if expect_operation(request(action, service_id, **fields), "confirmed") != queried["operation"]:
                raise SmokeFailure("completed command retry changed transaction facts")
            echo_tick(held)
        if fingerprint != (store_bytes(store), store.stat().st_mtime_ns):
            raise SmokeFailure("completed command retry rewrote the native Store")
        inventory = inspect()
        if len(inventory["inventory"]["objects"]) != 11 or any(not item.get("writable") or item.get("source") != "store" for item in inventory["inventory"]["objects"]):
            raise SmokeFailure("managed native objects did not have actual Store ownership")
        secret_objects = [item for item in inventory["inventory"]["objects"] if item["type"] in ("stcp", "sudp", "xtcp")]
        if len(secret_objects) != 6 or any(not any(value == {"path": "secretKey", "present": True} for value in item["secrets"]) for item in secret_objects):
            raise SmokeFailure("managed inventory omitted redacted secret presence")

        for valid in changes:
            invalid = copy.deepcopy(valid)
            invalid["name"] = "invalid-" + valid["kind"] + "-" + valid["type"]
            invalid_path = "transport.bandwidthLimitMode" if valid["kind"] == "proxy" else "serverName"
            invalid["fields"] = [field for field in invalid["fields"] if field["path"] != invalid_path]
            invalid["fields"].append({"path": invalid_path, "value": "invalid-mode" if valid["kind"] == "proxy" else ""})
            operation_id = str(uuid.uuid4())
            rejected = request("prepare", service_id, operation_id=operation_id, idempotency_key=str(uuid.uuid4()),
                               base_revision=inventory["inventory"]["revision"], operation_deadline_at_ms=int(time.time() * 1000) + 120000,
                               changes=[invalid])
            if (rejected.get("code") != "validation_failed" or rejected.get("operation")
                    or journal_state(managed, operation_id) is not None
                    or fingerprint != (store_bytes(store), store.stat().st_mtime_ns)):
                raise SmokeFailure("invalid native type candidate changed Store or created a transaction")
            echo_tick(held)
        print(f"PASS config {wire}: all 11 native types reject invalid candidates without journal, Store write or live TCP interruption", flush=True)

        if dashboard_request(dashboard_port, password, "GET", "/api/status") != 200:
            raise SmokeFailure("native dashboard read API became unavailable")
        proxy_body = json.dumps({"name": "managed-tcp", "type": "tcp", "tcp": {"localIP": LOOPBACK, "localPort": tcp_local, "remotePort": remote_port}})
        visitor_body = json.dumps({"name": "managed-visitor", "type": "stcp", "stcp": {"serverName": "managed-private", "secretKey": native_secret, "bindAddr": LOOPBACK, "bindPort": visitor_port}})
        new_proxy = json.dumps({"name": "dashboard-bypass-proxy", "type": "tcp", "tcp": {"localIP": LOOPBACK, "localPort": tcp_local, "remotePort": 0}})
        new_visitor = json.dumps({"name": "dashboard-bypass-visitor", "type": "stcp", "stcp": {"serverName": "managed-private", "secretKey": native_secret, "bindAddr": LOOPBACK, "bindPort": -1}})
        for method, path, body in (("PUT", "/api/config", main), ("GET", "/api/reload", None),
                                   ("POST", "/api/store/proxies", new_proxy),
                                   ("PUT", "/api/store/proxies/managed-tcp", proxy_body),
                                   ("DELETE", "/api/store/proxies/managed-tcp", None),
                                   ("POST", "/api/store/visitors", new_visitor),
                                   ("PUT", "/api/store/visitors/managed-visitor", visitor_body),
                                   ("DELETE", "/api/store/visitors/managed-visitor", None)):
            if dashboard_request(dashboard_port, password, method, path, body) != 409:
                raise SmokeFailure("managed mode did not reject a native dashboard write")
        if fingerprint != (store_bytes(store), store.stat().st_mtime_ns) or agent_path.read_text() != main:
            raise SmokeFailure("rejected native dashboard write changed local configuration")
        echo_tick(held)
        held.close()
        expect_operation(request("rollback", service_id, **operation_fields(prepared)), "rolled_back")
        if store_bytes(store) != old:
            raise SmokeFailure("explicit rollback did not restore exact original Store bytes")
        wait_for("removed native forwarding resources", lambda: not any(port_open(port) for port in
                 (remote_port, visitor_port, xtcp_port)) and not udp_matches(udp_port) and not udp_matches(sudp_port)
                 and not http_matches(http_port) and not https_matches(https_port, certificate)
                 and not connect_matches(mux_port), processes, timeout)
        print(f"PASS config {wire}: idempotent retries, redacted inventory, Dashboard write exclusion, exact rollback", flush=True)

        # Exercise actual edits against registered resources, with each prior
        # byte image retained so reverse-order rollback proves exact restoration.
        transactions = []

        def apply_edit(edits):
            before = store_bytes(store)
            candidate, _ = prepare(edits)
            expect_operation(request("apply", service_id, **operation_fields(candidate)), "confirmed")
            transactions.append((candidate, before))
            return candidate

        def instruction(item, action, fields=None):
            return {"operation": action, "kind": item["kind"], "name": item["name"], "type": "",
                    "fields": [{"path": key, "value": value} for key, value in (fields or {}).items()], "secrets": []}

        def forwarding_ready():
            return (echo_matches(remote_port) and udp_matches(udp_port) and http_matches(http_port)
                    and echo_matches(visitor_port) and https_matches(https_port, certificate)
                    and connect_matches(mux_port) and udp_matches(sudp_port) and echo_matches(xtcp_port))

        disabled = copy.deepcopy(changes)
        for item in disabled:
            item["fields"].append({"path": "enabled", "value": False})
        apply_edit(disabled)
        inactive = inspect()["inventory"]["objects"]
        if len(inactive) != 11 or any(item["active"] for item in inactive):
            raise SmokeFailure("disabled creation lost objects or reported active resources")
        if any(port_open(port) for port in (remote_port, visitor_port, xtcp_port)) or udp_matches(sudp_port):
            raise SmokeFailure("disabled native objects opened forwarding listeners")
        apply_edit([instruction(item, "enable") for item in changes])
        wait_for("all enabled native forwarding types", forwarding_ready, processes, timeout)
        apply_edit([instruction(item, "update", {"transport.useCompression": True}) for item in changes])
        wait_for("all edited native forwarding types", forwarding_ready, processes, timeout)


        if soak_seconds:
            before_soak = store_bytes(store)
            with SoakWitness(remote_port, controller, processes, client.process.pid) as witness:
                rounds = 0
                while time.monotonic() - witness.started < soak_seconds or rounds < 2:
                    witness.check()
                    candidate, _ = prepare([instruction(next(item for item in changes if item["name"] == "managed-http"),
                                                       "update", {"transport.useCompression": False})])
                    if store_bytes(store) != before_soak:
                        raise SmokeFailure("soak prepare modified the Store")
                    expect_operation(request("apply", service_id, **operation_fields(candidate)), "confirmed")
                    wait_for("all native business paths during soak apply", forwarding_ready, processes, timeout)
                    witness.check()
                    expect_operation(request("rollback", service_id, **operation_fields(candidate)), "rolled_back")
                    if store_bytes(store) != before_soak:
                        raise SmokeFailure("soak rollback did not restore exact Store bytes")
                    wait_for("all native business paths after soak rollback", forwarding_ready, processes, timeout)
                    rounds += 1
                    witness.check()
                stats = witness.finish(soak_seconds)
            if store_bytes(store) != before_soak:
                raise SmokeFailure("soak ended with an altered Store")
            print(f"PASS config {wire}: bounded soak {stats['seconds']:.1f}s, {rounds} complete HTTP apply/rollback cycles, "
                  f"{stats['ticks']} same-TCP echoes, {stats['reports']} same-session reports, "
                  f"pending peak {stats['pending_peak']}; sampled Agent RSS peak {stats['rss_peak_kib']} KiB "
                  f"<= test-only {SOAK_RSS_LIMIT_KIB} KiB (not a production capacity claim)", flush=True)

        clone_socket = stack.enter_context(socket.socket(socket.AF_INET, socket.SOCK_DGRAM))
        clone_socket.bind((LOOPBACK, 0))
        clone_port = clone_socket.getsockname()[1]
        clone_socket.close()
        clones = [change("copied-datagram", type_="sudp"),
                  change("copied-sudp-visitor", "visitor", "sudp", {"serverName": "copied-datagram", "bindPort": clone_port})]
        for item, source in zip(clones, ("managed-datagram", "managed-sudp-visitor")):
            item["clone_from"] = source
        apply_edit(clones)
        wait_for("copied SUDP secret and compressed datagram", lambda: udp_matches(clone_port), processes, timeout)
        cloned = [item for item in inspect()["inventory"]["objects"] if item["name"].startswith("copied-")]
        if len(cloned) != 2 or any(not any(secret == {"path": "secretKey", "present": True} for secret in item["secrets"])
                                   or not any(field == {"path": "transport.useCompression", "value": True} for field in item["fields"])
                                   for item in cloned):
            raise SmokeFailure("copy lost local secret or unedited transport setting")
        renamed = [change("renamed-datagram", type_="sudp"),
                   change("renamed-sudp-visitor", "visitor", "sudp", {"serverName": "renamed-datagram"})]
        for item, source in zip(renamed, ("copied-datagram", "copied-sudp-visitor")):
            item["clone_from"] = source
        apply_edit([instruction(item, "delete") for item in clones] + renamed)
        wait_for("renamed SUDP preserved private datagram", lambda: udp_matches(clone_port), processes, timeout)
        if any(item["name"].startswith("copied-") for item in inspect()["inventory"]["objects"]):
            raise SmokeFailure("rename left old Store objects behind")
        apply_edit([instruction(item, "delete") for item in renamed])
        wait_for("deleted SUDP visitor listener", lambda: not udp_matches(clone_port), processes, timeout)
        wait_for("unrelated forwarding after delete", forwarding_ready, processes, timeout)
        for candidate, before in reversed(transactions):
            expect_operation(request("rollback", service_id, **operation_fields(candidate)), "rolled_back")
            if store_bytes(store) != before:
                raise SmokeFailure("edit sequence rollback did not restore its exact prior Store bytes")
        if store_bytes(store) != old or inspect()["inventory"]["objects"]:
            raise SmokeFailure("edit sequence did not return to the empty original Store")
        print(f"PASS config {wire}: all-type disable/enable/update, SUDP copy/rename/delete preserves secret, reverse rollback restores each exact Store", flush=True)

        inventory = inspect()
        invalid = request("prepare", service_id, operation_id=str(uuid.uuid4()), idempotency_key=str(uuid.uuid4()),
                          base_revision=inventory["inventory"]["revision"], operation_deadline_at_ms=int(time.time() * 1000) + 120000,
                          changes=[change("invalid-candidate", fields={"localPort": "not-a-port", "remotePort": 0})])
        if invalid.get("code") != "invalid_field" or invalid.get("operation") or store_bytes(store) != old:
            raise SmokeFailure("invalid native candidate changed the Store or created an operation")
        drifted, _ = prepare([change("drift-check", fields={"localPort": tcp_local, "remotePort": 0})])
        external = EMPTY_STORE + " \n"
        replace_private(store, external)
        expect_operation(request("apply", service_id, **operation_fields(drifted)), "conflict")
        if store_bytes(store) != external.encode():
            raise SmokeFailure("Store drift conflict overwrote external bytes")
        replace_private(store, EMPTY_STORE)
        drifted, _ = prepare([change("context-check", fields={"localPort": tcp_local, "remotePort": 0})])
        replace_private(agent_path, main + "\n# disposable external source change\n")
        expect_operation(request("apply", service_id, **operation_fields(drifted)), "conflict")
        if store_bytes(store) != old:
            raise SmokeFailure("source context drift wrote a candidate Store")
        replace_private(agent_path, main)
        steady = controller.snapshot()
        if steady["session"] != first_session["session"] or steady["reports"] <= first_session["reports"]:
            raise SmokeFailure("configuration control interrupted the existing metrics session")
        print(f"PASS config {wire}: invalid candidate, Store CAS drift and file context drift rejected before write", flush=True)

        unavailable = [change("registration-waiting", fields={"localIP": LOOPBACK, "localPort": tcp_local, "remotePort": 0})]

        def waiting_for_registration(operation):
            if (journal_state(managed, operation["operation"]["operation_id"]) != "verifying"
                    or store_bytes(store) == old):
                return False
            statuses = dashboard_request(dashboard_port, password, "GET", "/api/status", json_response=True)
            return any(item.get("name") == "registration-waiting" and item.get("status") == "wait start"
                       for item in statuses.get("tcp", []))

        timed, _ = prepare(unavailable, ttl=9)
        with suspended_server(service):
            reports = controller.snapshot()["reports"]
            controller.send("apply", service_id, command_seconds=5, **operation_fields(timed))
            wait_for("native WaitStart with independent live monitoring", lambda: waiting_for_registration(timed)
                     and controller.snapshot()["reports"] > reports, processes, timeout)
            controller.disconnect()
            wait_for("Agent local watchdog rollback without controller", lambda: journal_state(managed, timed["operation"]["operation_id"]) == "rolled_back" and store_bytes(store) == old, processes, timeout)
        controller.resume()
        wait_for("management reconnection", lambda: bool(controller.snapshot()["session"]), processes, timeout)
        recovered = inspect()
        if recovered["service_id"] != service_id:
            raise SmokeFailure("service identity changed on telemetry reconnect")
        expect_operation(request("query", service_id, **operation_fields(timed)), "rolled_back")
        print(f"PASS config {wire}: real native WaitStart, controller loss, Agent deadline and autonomous rollback", flush=True)

        killed, _ = prepare(unavailable)
        with suspended_server(service):
            controller.send("apply", service_id, command_seconds=5, **operation_fields(killed))
            wait_for("durable native WaitStart before crash", lambda: waiting_for_registration(killed), processes, timeout)
            controller.disconnect()
            client._signal(signal.SIGKILL)
            client.process.wait(timeout=3)
            client.stop()
        client = stack.enter_context(child("restarted managed FRP agent", [str(agent), "-c", str(agent_path)], root))
        processes = (service, client)
        wait_for("offline startup recovery before native Store load", lambda: store_bytes(store) == old and journal_state(managed, killed["operation"]["operation_id"]) == "rolled_back" and port_open(dashboard_port), processes, timeout)
        controller.resume()
        wait_for("recovered management session", lambda: bool(controller.snapshot()["session"]), processes, timeout)
        if inspect()["service_id"] != service_id:
            raise SmokeFailure("managed service identity changed after ordinary restart")
        expect_operation(request("query", service_id, **operation_fields(killed)), "rolled_back")
        verify_private_tree(managed)
        before_reports = controller.snapshot()["reports"]
        wait_for("continuing host metrics after recovery", lambda: controller.snapshot()["reports"] > before_reports, processes, timeout)
        final = controller.snapshot()
        if final["reports"] <= first_session["reports"] + 3 or final["results"] < 15:
            raise SmokeFailure("management commands starved native host metrics")
        print(f"PASS config {wire}: SIGKILL recovery without controller, persistent identity, private materials, shared sequences and metrics", flush=True)
        for process in (client, service):
            process.stop()
            if process.forced_stop:
                raise SmokeFailure("managed smoke process required forced shutdown")


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--agent", type=Path, required=True)
    parser.add_argument("--server", type=Path, required=True)
    parser.add_argument("--wire", choices=("v1", "v2"), action="append")
    parser.add_argument("--timeout", type=positive_timeout, default=35.0)
    parser.add_argument("--soak-seconds", type=soak_duration, default=0,
                        help="optional bounded load gate: 0 disables, 30..3600 seconds per wire")
    args = parser.parse_args(argv)
    for binary in (args.agent, args.server):
        if not binary.is_file() or not os.access(binary, os.X_OK):
            parser.error("agent and server must be existing native executables")
    for wire in dict.fromkeys(args.wire or ("v1", "v2")):
        run(args.agent.resolve(), args.server.resolve(), wire, args.timeout, args.soak_seconds)
    return 0


if __name__ == "__main__":
    signal.signal(signal.SIGTERM, interrupted)
    try:
        raise SystemExit(main())
    except KeyboardInterrupt:
        print("FAIL: interrupted; local processes cleaned up", file=sys.stderr)
        raise SystemExit(130)
    except SmokeFailure as exc:
        print(f"FAIL: {exc}", file=sys.stderr)
        raise SystemExit(1)
    except (OSError, ValueError, http.client.HTTPException, subprocess.TimeoutExpired):
        print("FAIL: managed smoke I/O failed; private diagnostics suppressed", file=sys.stderr)
        raise SystemExit(1)
