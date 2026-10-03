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
from contextlib import ExitStack
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
from smoke import (LOOPBACK, SmokeFailure, child, child_environment,
                   echo_matches, interrupted, port_open,
                   positive_timeout, reserve_port, verify, wait_for, write_private)


CAPABILITIES = ["metrics.v1", "frp.v1", "frp.detail.v1", "config.manage.v1"]
MAX_PENDING = 16
MAX_HEADER = 16384
EMPTY_STORE = '{"proxies":[],"visitors":[]}\n'


class PersistentEcho(socketserver.BaseRequestHandler):
    """Keep a real business connection across rate-limited management retries."""

    def handle(self):
        self.request.settimeout(15)
        try:
            while data := self.request.recv(16384):
                self.request.sendall(data)
        except OSError:
            pass


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
                    "results": self.results_seen, "detail": copy.deepcopy(self.detail)}

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
                             "-subj", "/CN=frp-plus-config-smoke", "-addext", "subjectAltName=IP:127.0.0.1",
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


def dashboard_request(port, password, method, path, body=None):
    connection = http.client.HTTPConnection(LOOPBACK, port, timeout=3)
    try:
        connection.request(method, path, body=body, headers={"Authorization": "Basic " + base64.b64encode(("smoke:" + password).encode()).decode(),
                           "Content-Type": "application/json"})
        response = connection.getresponse()
        if len(response.read(MAX_FRAME + 1)) > MAX_FRAME:
            raise SmokeFailure("native dashboard response exceeded limit")
        return response.status
    finally:
        connection.close()


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


def run(agent: Path, server: Path, wire: str, timeout: float):
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
        reserved = [stack.enter_context(reserve_port()) for _ in range(5)]
        control_port, http_port, visitor_port, dashboard_port, remote_port = [item.getsockname()[1] for item in reserved]
        udp_socket = stack.enter_context(socket.socket(socket.AF_INET, socket.SOCK_DGRAM))
        udp_socket.bind((LOOPBACK, 0))
        udp_port = udp_socket.getsockname()[1]
        # A live listener is a deterministic native registration conflict.
        occupied = stack.enter_context(reserve_port())
        occupied.listen(1)
        occupied_port = occupied.getsockname()[1]
        tcp_local = stack.enter_context(local_server(socketserver.ThreadingTCPServer, PersistentEcho))
        udp_local = stack.enter_context(local_server(socketserver.UDPServer, UDPEcho))
        http_local = stack.enter_context(local_server(ThreadingHTTPServer, HTTPPayload))
        common = f'auth.method = "token"\nauth.token = "{frp_token}"\nlog.to = "console"\nlog.level = "warn"\n'
        server_path, agent_path = root / "server.toml", root / "agent.toml"
        write_private(server_path, f'bindAddr = "{LOOPBACK}"\nproxyBindAddr = "{LOOPBACK}"\nbindPort = {control_port}\n'
                      f'vhostHTTPPort = {http_port}\n' + common)
        main = (f'serverAddr = "{LOOPBACK}"\nserverPort = {control_port}\nclientID = "1"\nloginFailExit = false\n'
                f'transport.protocol = "tcp"\ntransport.wireProtocol = "{wire}"\ntransport.tls.enable = true\n'
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
        udp_socket.close()
        service = stack.enter_context(child("managed FRP server", [str(server), "-c", str(server_path)], root))
        wait_for("native server listeners", lambda: port_open(control_port) and port_open(http_port), (service,), timeout)
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
                   change("managed-visitor", "visitor", "stcp", {"serverName": "managed-private", "bindAddr": LOOPBACK, "bindPort": visitor_port}, reference)]
        prepared, intent = prepare(changes)
        if store_bytes(store) != old or port_open(remote_port) or port_open(visitor_port) or http_matches(http_port):
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
        print(f"PASS config {wire}: TLS auth, private secret reference, prepare isolation, confirmed TCP/UDP/HTTP/STCP", flush=True)

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
        if len(inventory["inventory"]["objects"]) != 5 or any(not item.get("writable") or item.get("source") != "store" for item in inventory["inventory"]["objects"]):
            raise SmokeFailure("managed native objects did not have actual Store ownership")
        secret_objects = [item for item in inventory["inventory"]["objects"] if item["type"] == "stcp"]
        if len(secret_objects) != 2 or any(not any(value == {"path": "secretKey", "present": True} for value in item["secrets"]) for item in secret_objects):
            raise SmokeFailure("managed inventory omitted redacted secret presence")

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
        wait_for("removed native forwarding resources", lambda: not port_open(remote_port) and not port_open(visitor_port) and not udp_matches(udp_port) and not http_matches(http_port), processes, timeout)
        print(f"PASS config {wire}: idempotent retries, redacted inventory, Dashboard write exclusion, exact rollback", flush=True)

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

        unavailable = [change("registration-blocked", fields={"localIP": LOOPBACK, "localPort": tcp_local, "remotePort": occupied_port})]
        timed, _ = prepare(unavailable, ttl=9)
        controller.send("apply", service_id, command_seconds=5, **operation_fields(timed))
        wait_for("unconfirmed native candidate", lambda: journal_state(managed, timed["operation"]["operation_id"]) == "verifying" and store_bytes(store) != old, processes, timeout)
        controller.disconnect()
        wait_for("Agent local watchdog rollback without controller", lambda: journal_state(managed, timed["operation"]["operation_id"]) == "rolled_back" and store_bytes(store) == old, processes, timeout)
        controller.resume()
        wait_for("management reconnection", lambda: bool(controller.snapshot()["session"]), processes, timeout)
        recovered = inspect()
        if recovered["service_id"] != service_id:
            raise SmokeFailure("service identity changed on telemetry reconnect")
        expect_operation(request("query", service_id, **operation_fields(timed)), "rolled_back")
        print(f"PASS config {wire}: controller loss during unready apply, Agent deadline and autonomous rollback", flush=True)

        killed, _ = prepare(unavailable)
        controller.send("apply", service_id, command_seconds=5, **operation_fields(killed))
        wait_for("durable native transaction before crash", lambda: journal_state(managed, killed["operation"]["operation_id"]) == "verifying" and store_bytes(store) != old, processes, timeout)
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
    args = parser.parse_args(argv)
    for binary in (args.agent, args.server):
        if not binary.is_file() or not os.access(binary, os.X_OK):
            parser.error("agent and server must be existing native executables")
    for wire in dict.fromkeys(args.wire or ("v1", "v2")):
        run(args.agent.resolve(), args.server.resolve(), wire, args.timeout)
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
