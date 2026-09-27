#!/usr/bin/env python3
"""Loopback P1 integration: verified WSS, public DTO/SSE and FRP fault isolation.

Only stdlib + the locally installed OpenSSL are needed. Every token, certificate
and configuration is generated into a private temporary directory and removed.
No actual infrastructure, .env credentials, DNS endpoint or external port is used.
"""
from __future__ import annotations

import argparse
from contextlib import ExitStack
import hashlib
import http.client
import json
import os
from pathlib import Path
import secrets
import select
import signal
import socket
import ssl
import subprocess
import sys
import tempfile
import threading
import time

from smoke import (Child, LOOPBACK, SmokeFailure, child, child_environment, echo_matches,
                   echo_server, interrupted, port_open, positive_timeout, reserve_port,
                   verify, wait_for, write_private)


class Bridge:
    """A disposable byte bridge; dropping it interrupts one transport only."""

    def __init__(self, target: int):
        self.target = target
        self.port = 0
        self.listener: socket.socket | None = None
        self.connections: set[socket.socket] = set()
        self.lock = threading.Lock()
        self.thread: threading.Thread | None = None
        self.stop_event = threading.Event()

    def start(self) -> None:
        if self.listener is not None:
            raise SmokeFailure("test bridge already started")
        listener = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        listener.bind((LOOPBACK, self.port))
        listener.listen(16)
        listener.settimeout(.2)
        self.port = listener.getsockname()[1]
        self.listener = listener
        self.stop_event = threading.Event()
        self.thread = threading.Thread(target=self._accept, args=(listener, self.stop_event), daemon=True)
        self.thread.start()

    def _accept(self, listener: socket.socket, stop: threading.Event) -> None:
        while not stop.is_set():
            try:
                client, _ = listener.accept()
            except socket.timeout:
                continue
            except OSError:
                return
            threading.Thread(target=self._relay, args=(client, stop), daemon=True).start()

    def _relay(self, client: socket.socket, stop: threading.Event) -> None:
        upstream = None
        try:
            upstream = socket.create_connection((LOOPBACK, self.target), timeout=1)
            client.settimeout(1)
            with self.lock:
                if stop.is_set():
                    return
                self.connections.update((client, upstream))
            while not stop.is_set():
                readable, _, _ = select.select((client, upstream), (), (), .2)
                for source in readable:
                    data = source.recv(65536)
                    if not data:
                        return
                    (upstream if source is client else client).sendall(data)
        except (OSError, ValueError):
            pass
        finally:
            with self.lock:
                self.connections.discard(client)
                if upstream:
                    self.connections.discard(upstream)
            client.close()
            if upstream:
                upstream.close()

    def stop(self) -> None:
        self.stop_event.set()
        if self.listener:
            self.listener.close()
            self.listener = None
        with self.lock:
            active = list(self.connections)
        for connection in active:
            try:
                connection.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass
            connection.close()
        if self.thread:
            self.thread.join(timeout=2)
            if self.thread.is_alive():
                raise SmokeFailure("test bridge did not stop")
            self.thread = None


class PublicAPI:
    def __init__(self, port: int, certificate: Path):
        self.port = port
        self.context = ssl.create_default_context(cafile=str(certificate))

    def connection(self) -> http.client.HTTPSConnection:
        return http.client.HTTPSConnection(LOOPBACK, self.port, context=self.context, timeout=3)

    def get(self, path: str, headers: dict[str, str] | None = None) -> tuple[int, dict[str, str], bytes]:
        connection = self.connection()
        try:
            connection.request("GET", path, headers=headers or {})
            response = connection.getresponse()
            body = response.read(2 * 1024 * 1024 + 1)
            if len(body) > 2 * 1024 * 1024:
                raise SmokeFailure("public response exceeded smoke limit")
            return response.status, dict(response.getheaders()), body
        finally:
            connection.close()

    def snapshot(self) -> dict:
        status, headers, body = self.get("/api/public/v1/nodes")
        if status != 200 or headers.get("Cache-Control") != "no-store":
            raise SmokeFailure("public API is not an uncached JSON 200 response")
        try:
            result = json.loads(body)
        except (UnicodeError, json.JSONDecodeError) as exc:
            raise SmokeFailure("public API returned malformed JSON") from exc
        if len(result.get("nodes", [])) != 1:
            raise SmokeFailure("public API returned an unexpected node count")
        return result

    def node(self) -> dict:
        return self.snapshot()["nodes"][0]

    def event(self) -> dict:
        connection = self.connection()
        try:
            connection.request("GET", "/events/public")
            response = connection.getresponse()
            if response.status != 200 or response.getheader("Content-Type", "").split(";")[0] != "text/event-stream":
                raise SmokeFailure("public SSE did not open a stream")
            event = ""
            for _ in range(20):
                line = response.readline(512 * 1024)
                if line.startswith(b"event:"):
                    event = line[6:].strip().decode("ascii")
                if line.startswith(b"data:"):
                    if event != "snapshot":
                        raise SmokeFailure("public SSE event is not a snapshot")
                    return json.loads(line[5:])
            raise SmokeFailure("public SSE did not send its initial snapshot")
        finally:
            connection.close()


def assert_redacted(payload: dict, secrets_to_hide: tuple[str, ...]) -> None:
    forbidden = {"hostname", "ipv4", "ipv6", "kernel", "boot_id", "iface", "raw_client_id", "local_target", "association", "facts", "session_id", "token", "token_sha256"}
    def walk(value: object) -> None:
        if isinstance(value, dict):
            if forbidden.intersection(value):
                raise SmokeFailure("public response leaked an internal field")
            for nested in value.values():
                walk(nested)
        elif isinstance(value, list):
            for nested in value:
                walk(nested)
    walk(payload)
    encoded = json.dumps(payload, ensure_ascii=False)
    if any(secret in encoded for secret in secrets_to_hide):
        raise SmokeFailure("public response leaked a private test value")
    for node in payload.get("nodes", []):
        metrics = node.get("metrics") or {}
        for name, field in metrics.items():
            if name in ("scope", "cpu", "load"):
                continue
            value = field.get("value")
            if value is not None and (not isinstance(value, str) or not value.isascii() or not value.isdecimal()):
                raise SmokeFailure("public uint64 value is not a decimal string")


def generate_certificate(directory: Path) -> tuple[Path, Path]:
    certificate, key = directory / "loopback.crt", directory / "loopback.key"
    config = directory / "openssl.cnf"
    write_private(config, "[req]\nprompt=no\ndistinguished_name=dn\nx509_extensions=ext\n[dn]\nCN=localhost\n[ext]\nsubjectAltName=IP:127.0.0.1\nbasicConstraints=critical,CA:TRUE\nkeyUsage=critical,digitalSignature,keyEncipherment,keyCertSign\nextendedKeyUsage=serverAuth\n")
    try:
        result = subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1", "-config", str(config), "-keyout", str(key), "-out", str(certificate)],
                                env=child_environment(), stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=30)
    except subprocess.TimeoutExpired as exc:
        raise SmokeFailure("loopback certificate generation timed out") from exc
    if result.returncode:
        raise SmokeFailure("loopback certificate generation failed")
    key.chmod(0o600)
    certificate.chmod(0o600)
    return certificate, key


def run(agent: Path, monitor: Path, timeout: float) -> None:
    with tempfile.TemporaryDirectory(prefix="frp-monitor-p1-") as temporary, ExitStack() as stack:
        directory = Path(temporary)
        directory.chmod(0o700)
        reservations = [stack.enter_context(reserve_port()) for _ in range(4)]
        control, remote, monitor_socket, unavailable = reservations
        control_port, remote_port, monitor_port, unavailable_port = [s.getsockname()[1] for s in reservations]
        echo_port = stack.enter_context(echo_server())
        certificate, key = generate_certificate(directory)
        token, frp_token = secrets.token_urlsafe(32), secrets.token_hex(32)
        private_client_id, private_user = "private-smoke-client-id", "private-smoke-user"
        token_file, credentials = directory / "agent.token", directory / "credentials.json"
        write_private(token_file, token + "\n")
        write_private(credentials, json.dumps([{"agent_id": "smoke-node-01", "name": "Smoke public node", "token_sha256": hashlib.sha256(token.encode()).hexdigest()}]))
        common = f'auth.method = "token"\nauth.token = "{frp_token}"\nlog.to = "console"\nlog.level = "info"\nlog.disablePrintColor = true\n'
        server_config = directory / "monitor.toml"
        write_private(server_config, f'bindAddr = "{LOOPBACK}"\nproxyBindAddr = "{LOOPBACK}"\nbindPort = {control_port}\nallowPorts = [{{single={remote_port}}}]\n' + common +
                      f'\n[monitor]\nenabled = true\nbindAddr = "{LOOPBACK}"\nbindPort = {monitor_port}\nserverID = "smoke-server"\ncertFile = {json.dumps(str(certificate))}\nkeyFile = {json.dumps(str(key))}\ncredentialsFile = {json.dumps(str(credentials))}\nreportIntervalSeconds = 1\n')
        telemetry_bridge, frp_bridge = Bridge(monitor_port), Bridge(control_port)
        for bridge in (telemetry_bridge, frp_bridge):
            bridge.start()
            stack.callback(bridge.stop)
        def agent_config(name: str, port: int, proxy: bool = False) -> Path:
            target = directory / name
            text = (f'serverAddr = "{LOOPBACK}"\nserverPort = {port}\nloginFailExit = false\nuser = "{private_user}"\n'
                    f'clientID = "{private_client_id}"\ntransport.protocol = "tcp"\ntransport.wireProtocol = "v2"\ntransport.tls.enable = true\n' + common +
                    f'\n[telemetry]\nenabled = true\nendpoint = "wss://{LOOPBACK}:{telemetry_bridge.port}/agent/v1/ws"\ntokenFile = {json.dumps(str(token_file))}\ncaFile = {json.dumps(str(certificate))}\nserverID = "smoke-server"\nintervalSeconds = 1\n')
            if proxy:
                text += f'\n[[proxies]]\nname = "private-smoke-echo"\ntype = "tcp"\nlocalIP = "{LOOPBACK}"\nlocalPort = {echo_port}\nremotePort = {remote_port}\n'
            write_private(target, text)
            return target
        failed_config = agent_config("agent-frp-unavailable.toml", unavailable_port)
        empty_config = agent_config("agent-empty.toml", frp_bridge.port)
        tcp_config = agent_config("agent-tcp.toml", frp_bridge.port, True)
        for binary, config in ((monitor, server_config), (agent, failed_config), (agent, empty_config), (agent, tcp_config)):
            verify(binary, config, "P1 TOML verification", timeout)
        control.close(); monitor_socket.close()
        server = stack.enter_context(child("P1 monitor", [str(monitor), "-c", str(server_config)], directory))
        wait_for("FRP and WSS listeners", lambda: port_open(control_port) and port_open(monitor_port), (server,), timeout)
        api = PublicAPI(monitor_port, certificate)
        if api.node()["session"] != "waiting":
            raise SmokeFailure("unconnected credential incorrectly appears online")
        # The monitor cert must validate with the configured CA and fail otherwise.
        untrusted = http.client.HTTPSConnection(LOOPBACK, monitor_port, timeout=3)
        try:
            try:
                untrusted.request("GET", "/api/public/v1/nodes")
                untrusted.getresponse()
            except ssl.SSLCertVerificationError:
                pass
            else:
                raise SmokeFailure("untrusted monitor TLS certificate was accepted")
        finally:
            untrusted.close()
        upgrade = {"Connection": "Upgrade", "Upgrade": "websocket", "Sec-WebSocket-Version": "13", "Sec-WebSocket-Key": "c21va2UtdGVzdC1ub25jZQ=="}
        for bearer in (None, secrets.token_urlsafe(32)):
            headers = dict(upgrade)
            if bearer:
                headers["Authorization"] = "Bearer " + bearer
            if api.get("/agent/v1/ws", headers)[0] != 401:
                raise SmokeFailure("missing or invalid node credential was accepted")
        print("PASS P1: trusted WSS certificate; untrusted TLS and invalid credentials rejected", flush=True)
        status, headers, body = api.get("/")
        if status != 200 or b"/src/app.mjs" not in body or "script-src 'self'" not in headers.get("Content-Security-Policy", ""):
            raise SmokeFailure("embedded public page or its CSP is missing")
        for path in ("/README.md", "/handler.go", "/.env", "/admin", "/assets/", "/src/"):
            if api.get(path)[0] != 404:
                raise SmokeFailure("static handler exposed a non-public resource")
        print("PASS P1: embedded public page, CSP, static allowlist and absent admin routes", flush=True)
        with child("FRP unavailable agent", [str(agent), "-c", str(failed_config)], directory) as failed:
            wait_for("monitoring before first FRP login", lambda: api.node()["session"] == "online" and api.node()["freshness"] == "fresh" and api.node()["frp"]["control_state"] in ("connecting", "disconnected"), (server, failed), timeout)
            assert_redacted(api.snapshot(), (token, frp_token, private_client_id, private_user, LOOPBACK))
        print("PASS P1: telemetry remains online and fresh while initial FRP login fails", flush=True)
        with child("no-proxy P1 agent", [str(agent), "-c", str(empty_config)], directory) as empty:
            wait_for("no-proxy FRP and monitoring login", lambda: api.node()["session"] == "online" and api.node()["freshness"] == "fresh" and api.node()["frp"] == {"control_state": "connected", "proxy_total": 0, "proxy_running": 0}, (server, empty), timeout)
        print("PASS P1: authenticated agent without proxies reports connected FRP and zero tunnels", flush=True)
        remote.close()
        client = stack.enter_context(child("TCP P1 agent", [str(agent), "-c", str(tcp_config)], directory))
        client_pid = client.process.pid
        wait_for("P1 TCP echo", lambda: echo_matches(remote_port), (server, client), timeout)
        wait_for("running proxy report", lambda: api.node()["frp"]["proxy_running"] == 1 and api.node()["session"] == "online", (server, client), timeout)
        assert_redacted(api.snapshot(), (token, frp_token, private_client_id, private_user, LOOPBACK, "private-smoke-echo"))
        assert_redacted(api.event(), (token, frp_token, private_client_id, private_user, LOOPBACK, "private-smoke-echo"))
        print("PASS P1: TCP payload, public JSON/SSE redaction and decimal uint64 transport", flush=True)
        frp_bridge.stop()
        wait_for("independent FRP disconnect", lambda: api.node()["session"] == "online" and api.node()["freshness"] == "fresh" and api.node()["frp"]["control_state"] == "disconnected", (server, client), timeout)
        frp_bridge.start()
        wait_for("FRP reconnect without agent restart", lambda: echo_matches(remote_port), (server, client), timeout)
        wait_for("FRP reconnect report", lambda: api.node()["frp"]["control_state"] == "connected", (server, client), timeout)
        print("PASS P1: FRP disconnect/reconnect leaves monitoring online and fresh", flush=True)
        telemetry_bridge.stop()
        wait_for("independent monitoring disconnect", lambda: api.node()["session"] == "offline", (server, client), timeout)
        if not echo_matches(remote_port):
            raise SmokeFailure("monitoring interruption stopped the native FRP tunnel")
        wait_for("stale monitoring snapshot", lambda: api.node()["freshness"] == "stale", (server, client), max(timeout, 15))
        telemetry_bridge.start()
        wait_for("monitoring reconnect without agent restart", lambda: api.node()["session"] == "online" and api.node()["freshness"] == "fresh", (server, client), timeout)
        if client.process.pid != client_pid or not echo_matches(remote_port):
            raise SmokeFailure("agent restarted or tunnel failed across monitoring reconnect")
        print("PASS P1: monitoring disconnect/staleness/reconnect leaves native FRP forwarding intact", flush=True)
        client.stop(); server.stop()
        if client.forced_stop or server.forced_stop:
            raise SmokeFailure("P1 process required forced shutdown")


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--agent", type=Path, required=True)
    parser.add_argument("--server", type=Path, required=True)
    parser.add_argument("--timeout", type=positive_timeout, default=30.0)
    args = parser.parse_args(argv)
    for label, path in (("agent", args.agent), ("server", args.server)):
        if not path.is_file() or not os.access(path, os.X_OK):
            parser.error(f"{label} must be an executable for this host")
    run(args.agent.resolve(), args.server.resolve(), args.timeout)
    print("PASS: P1 local integration complete; deployment/history/capacity are outside this check", flush=True)
    return 0


if __name__ == "__main__":
    signal.signal(signal.SIGTERM, interrupted)
    try:
        raise SystemExit(main())
    except KeyboardInterrupt:
        print("FAIL: interrupted; test processes cleaned up", file=sys.stderr)
        raise SystemExit(130)
    except SmokeFailure as error:
        print(f"FAIL: {error}", file=sys.stderr)
        raise SystemExit(1)
    except (http.client.HTTPException, json.JSONDecodeError, UnicodeError):
        print("FAIL: invalid local monitoring response", file=sys.stderr)
        raise SystemExit(1)
    except OSError as error:
        print(f"FAIL: local operating-system error (errno {error.errno})", file=sys.stderr)
        raise SystemExit(1)
