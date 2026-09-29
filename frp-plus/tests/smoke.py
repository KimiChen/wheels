#!/usr/bin/env python3
"""Loopback-only native FRP baseline checks; Python standard library only."""

from __future__ import annotations

import argparse
import base64
from contextlib import ExitStack, contextmanager
from dataclasses import dataclass, field
from html.parser import HTMLParser
import http.client
import json
import math
import os
from pathlib import Path
import secrets
import select
import signal
import socket
import socketserver
import ssl
import subprocess
import sys
import tempfile
import threading
import time
from typing import Callable, Iterator
from urllib.parse import urljoin, urlsplit


LOOPBACK = "127.0.0.1"
LOGIN_MARKER = b"login to server success"
PAYLOAD = b"frp-plus baseline smoke\x00" + bytes(range(256)) * 256


class SmokeFailure(RuntimeError):
    """A safe diagnostic containing no raw child output or configuration."""


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
        if not isinstance(result, dict) or not isinstance(result.get("nodes"), list):
            raise SmokeFailure("public API returned no node list")
        return result

    def node(self) -> dict:
        nodes = self.snapshot()["nodes"]
        if len(nodes) != 1:
            raise SmokeFailure("public API returned an unexpected node count")
        return nodes[0]

    def history(self) -> dict:
        status, headers, raw = self.get("/api/public/v1/nodes/1/history?window=1h")
        if status != 200 or headers.get("Cache-Control") != "no-store":
            raise SmokeFailure("history API is not an uncached JSON 200 response")
        payload = json.loads(raw)
        if payload.get("node_id") != "1" or payload.get("step_seconds") != 60:
            raise SmokeFailure("history API returned a wrong node or interval")
        return payload

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
            if name in ("cpu", "load"):
                continue
            value = field.get("value")
            if value is not None and (not isinstance(value, str) or not value.isascii() or not value.isdecimal()):
                raise SmokeFailure("public uint64 value is not a decimal string")


@dataclass(frozen=True)
class Dashboard:
    port: int
    user: str = "smoke"
    password: str = field(default_factory=lambda: secrets.token_hex(32), repr=False)


class ScriptReferences(HTMLParser):
    def __init__(self) -> None:
        super().__init__()
        self.sources: list[str] = []

    def handle_starttag(self, tag: str, attrs: list[tuple[str, str | None]]) -> None:
        if tag == "script":
            source = dict(attrs).get("src")
            if source:
                self.sources.append(source)


def dashboard_get(dashboard: Dashboard, path: str, authenticated: bool) -> tuple[int, str, bytes]:
    # HTTPConnection connects directly; proxy environment variables are not used.
    headers = {}
    if authenticated:
        credentials = f"{dashboard.user}:{dashboard.password}".encode("ascii")
        headers["Authorization"] = "Basic " + base64.b64encode(credentials).decode("ascii")
    connection = http.client.HTTPConnection(LOOPBACK, dashboard.port, timeout=2)
    try:
        connection.request("GET", path, headers=headers)
        response = connection.getresponse()
        body = response.read(8 * 1024 * 1024 + 1)
        if len(body) > 8 * 1024 * 1024:
            raise SmokeFailure("Dashboard resource exceeds smoke size limit")
        return response.status, response.getheader("Content-Type", ""), body
    finally:
        connection.close()


def verify_dashboard(dashboard: Dashboard) -> None:
    # The pinned upstream redirects / to its native homepage at /static/.
    homepage = "/static/"
    status, _, _ = dashboard_get(dashboard, homepage, False)
    if status != 401:
        raise SmokeFailure(f"Dashboard anonymous homepage returned {status}, expected 401")
    status, content_type, body = dashboard_get(dashboard, homepage, True)
    if status != 200 or "text/html" not in content_type.lower():
        raise SmokeFailure(f"Dashboard authenticated homepage is not HTML 200 (status {status})")
    references = ScriptReferences()
    try:
        references.feed(body.decode("utf-8"))
    except UnicodeError as exc:
        raise SmokeFailure("Dashboard homepage is not UTF-8 HTML") from exc
    for source in references.sources:
        parsed = urlsplit(source)
        if parsed.scheme or parsed.netloc or not parsed.path.endswith(".js"):
            continue
        resource = urljoin(homepage, source)
        status, content_type, body = dashboard_get(dashboard, resource, True)
        if status == 200 and body and "javascript" in content_type.lower():
            return
    raise SmokeFailure("Dashboard has no readable same-origin JavaScript reference")


def child_environment() -> dict[str, str]:
    # FRP honors proxy environment variables. Do not inherit those, credentials,
    # FRP template inputs, or custom runtime hooks from the invoking shell.
    allowed = ("PATH", "SystemRoot", "WINDIR")
    return {key: os.environ[key] for key in allowed if key in os.environ}


class Child:
    """Drain output without persisting it; retain only known success events."""

    def __init__(self, label: str, argv: list[str], directory: Path):
        self.label = label
        self.logged_in = threading.Event()
        self.forced_stop = False
        self.process = subprocess.Popen(
            argv,
            cwd=directory,
            env=child_environment(),
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            start_new_session=(os.name == "posix"),
        )
        self.reader = threading.Thread(target=self._read_output, daemon=True)
        self.reader.start()

    def _read_output(self) -> None:
        assert self.process.stdout is not None
        tail = b""
        # Chunked reads also bound memory for an unexpectedly long log line.
        while chunk := self.process.stdout.read1(4096):
            combined = tail + chunk
            if LOGIN_MARKER in combined:
                self.logged_in.set()
            tail = combined[-(len(LOGIN_MARKER) - 1):]

    def ensure_running(self) -> None:
        code = self.process.poll()
        if code is not None:
            raise SmokeFailure(f"{self.label} exited unexpectedly (code {code})")

    def _signal(self, sig: int) -> None:
        try:
            if os.name == "posix":
                os.killpg(self.process.pid, sig)
            elif sig == signal.SIGTERM:
                self.process.terminate()
            else:
                self.process.kill()
        except ProcessLookupError:
            pass

    def stop(self) -> None:
        if self.process.poll() is None:
            self._signal(signal.SIGTERM)
            try:
                self.process.wait(timeout=3)
            except subprocess.TimeoutExpired:
                self.forced_stop = True
                self._signal(getattr(signal, "SIGKILL", signal.SIGTERM))
                try:
                    self.process.wait(timeout=3)
                except subprocess.TimeoutExpired as exc:
                    raise SmokeFailure(f"{self.label} did not stop after kill") from exc
        self.reader.join(timeout=1)
        if self.reader.is_alive():
            raise SmokeFailure(f"{self.label} output reader did not stop")
        assert self.process.stdout is not None
        self.process.stdout.close()


@contextmanager
def child(label: str, argv: list[str], directory: Path) -> Iterator[Child]:
    process = Child(label, argv, directory)
    try:
        yield process
    finally:
        process.stop()


def wait_for(
    label: str,
    condition: Callable[[], bool],
    processes: tuple[Child, ...],
    timeout: float,
) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        for process in processes:
            process.ensure_running()
        if condition():
            for process in processes:
                process.ensure_running()
            return
        time.sleep(0.05)
    raise SmokeFailure(f"timed out waiting for {label}")


def reserve_port() -> socket.socket:
    reserved = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    try:
        reserved.bind((LOOPBACK, 0))
    except BaseException:
        reserved.close()
        raise
    return reserved


def port_open(port: int) -> bool:
    try:
        with socket.create_connection((LOOPBACK, port), timeout=0.25):
            return True
    except OSError:
        return False


class EchoHandler(socketserver.BaseRequestHandler):
    def handle(self) -> None:
        self.request.settimeout(1)
        try:
            while data := self.request.recv(16384):
                self.request.sendall(data)
        except OSError:
            pass


class EchoServer(socketserver.ThreadingTCPServer):
    daemon_threads = True
    block_on_close = False


@contextmanager
def echo_server() -> Iterator[int]:
    with EchoServer((LOOPBACK, 0), EchoHandler) as server:
        thread = threading.Thread(
            target=server.serve_forever, kwargs={"poll_interval": 0.05}, daemon=True
        )
        thread.start()
        try:
            yield server.server_address[1]
        finally:
            server.shutdown()
            thread.join(timeout=2)
            if thread.is_alive():
                raise SmokeFailure("echo server did not stop")


def echo_matches(port: int) -> bool:
    try:
        with socket.create_connection((LOOPBACK, port), timeout=0.5) as connection:
            connection.settimeout(0.5)
            connection.sendall(PAYLOAD)
            received = bytearray()
            deadline = time.monotonic() + 1
            while len(received) < len(PAYLOAD) and time.monotonic() < deadline:
                data = connection.recv(min(16384, len(PAYLOAD) - len(received)))
                if not data:
                    break
                received.extend(data)
            if bytes(received) != PAYLOAD:
                raise SmokeFailure("TCP echo returned a truncated or altered payload")
            return True
    except OSError:
        return False


def write_private(path: Path, content: str) -> None:
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(descriptor, "w", encoding="utf-8", newline="\n") as output:
        output.write(content)


def make_configs(
    directory: Path, wire: str, control_port: int, remote_port: int, echo_port: int,
    dashboard: Dashboard | None = None,
) -> tuple[Path, Path, Path]:
    token = secrets.token_hex(32)
    common = (
        'auth.method = "token"\n'
        f'auth.token = "{token}"\n'
        'log.to = "console"\n'
        'log.level = "info"\n'
        'log.disablePrintColor = true\n'
    )
    server = directory / "server.toml"
    no_proxy = directory / "agent-empty.toml"
    tcp = directory / "agent-tcp.toml"
    server_config = (
        f'bindAddr = "{LOOPBACK}"\n'
        f'proxyBindAddr = "{LOOPBACK}"\n'
        f"bindPort = {control_port}\n"
        f"allowPorts = [{{ single = {remote_port} }}]\n"
        + common
    )
    if dashboard is not None:
        server_config += (
            f'webServer.addr = "{LOOPBACK}"\n'
            f"webServer.port = {dashboard.port}\n"
            f'webServer.user = "{dashboard.user}"\n'
            f'webServer.password = "{dashboard.password}"\n'
        )
    write_private(server, server_config)
    client_config = (
        f'serverAddr = "{LOOPBACK}"\n'
        f"serverPort = {control_port}\n"
        "loginFailExit = false\n"
        'transport.protocol = "tcp"\n'
        f'transport.wireProtocol = "{wire}"\n'
        "transport.tls.enable = true\n"
        + common
    )
    write_private(no_proxy, client_config)
    write_private(tcp, client_config + (
        '\n[[proxies]]\n'
        'name = "smoke-echo"\n'
        'type = "tcp"\n'
        f'localIP = "{LOOPBACK}"\n'
        f"localPort = {echo_port}\n"
        f"remotePort = {remote_port}\n"
    ))
    return server, no_proxy, tcp


def verify(binary: Path, config: Path, label: str, timeout: float) -> None:
    with child(label, [str(binary), "verify", "-c", str(config)], config.parent) as check:
        try:
            code = check.process.wait(timeout=timeout)
        except subprocess.TimeoutExpired as exc:
            raise SmokeFailure(f"{label} timed out") from exc
        if code:
            raise SmokeFailure(f"{label} failed (code {code}; raw output suppressed)")


def run_wire(agent: Path, server: Path, wire: str, timeout: float) -> None:
    with tempfile.TemporaryDirectory(prefix="frp-plus-smoke-") as name, ExitStack() as stack:
        directory = Path(name)
        directory.chmod(0o700)
        control = stack.enter_context(reserve_port())
        remote = stack.enter_context(reserve_port())
        dashboard_socket = stack.enter_context(reserve_port())
        dashboard = Dashboard(dashboard_socket.getsockname()[1])
        control_port, remote_port = control.getsockname()[1], remote.getsockname()[1]
        echo_port = stack.enter_context(echo_server())
        server_config, empty_config, tcp_config = make_configs(
            directory, wire, control_port, remote_port, echo_port, dashboard
        )
        for binary, config, label in (
            (server, server_config, "server TOML verify"),
            (agent, empty_config, "empty-agent TOML verify"),
            (agent, tcp_config, "TCP-agent TOML verify"),
        ):
            verify(binary, config, label, timeout)
        print(f"PASS {wire}: server and agent TOML verification", flush=True)

        control.close()
        dashboard_socket.close()
        server_args = [str(server), "-c", str(server_config)]
        service = stack.enter_context(child("server", server_args, directory))
        wait_for("server listener", lambda: port_open(control_port), (service,), timeout)
        wait_for("Dashboard listener", lambda: port_open(dashboard.port), (service,), timeout)
        verify_dashboard(dashboard)
        service.ensure_running()
        print(f"PASS {wire}: Dashboard authentication, homepage and embedded JavaScript", flush=True)
        with child("empty agent", [str(agent), "-c", str(empty_config)], directory) as empty:
            wait_for("empty agent login", empty.logged_in.is_set, (service, empty), timeout)
            # A success marker precedes control startup; also check sustained life.
            time.sleep(0.25)
            empty.ensure_running()
            service.ensure_running()
        if empty.forced_stop:
            raise SmokeFailure("empty agent required forced shutdown")
        print(f"PASS {wire}: authenticated startup without proxies", flush=True)

        remote.close()
        client = stack.enter_context(child("TCP agent", [str(agent), "-c", str(tcp_config)], directory))
        wait_for("TCP echo", lambda: echo_matches(remote_port), (service, client), timeout)
        print(f"PASS {wire}: TCP tunnel preserves binary payload", flush=True)

        client_pid = client.process.pid
        service.stop()
        if service.forced_stop:
            raise SmokeFailure("server required forced shutdown")
        wait_for("old tunnel closure", lambda: not port_open(remote_port), (client,), timeout)
        service = stack.enter_context(child("restarted server", server_args, directory))
        wait_for("restarted server listener", lambda: port_open(control_port), (service, client), timeout)
        wait_for("TCP echo after server restart", lambda: echo_matches(remote_port), (service, client), timeout)
        if client.process.pid != client_pid:
            raise SmokeFailure("agent process changed during server restart")
        print(f"PASS {wire}: same agent reconnects after server restart", flush=True)
        client.stop()
        service.stop()
        if client.forced_stop or service.forced_stop:
            raise SmokeFailure("FRP required forced shutdown")


def positive_timeout(value: str) -> float:
    number = float(value)
    if not math.isfinite(number) or not 1 <= number <= 300:
        raise argparse.ArgumentTypeError("timeout must be between 1 and 300 seconds")
    return number


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--agent", required=True, type=Path, help="native frpc / agent executable")
    parser.add_argument("--server", required=True, type=Path, help="native frps / server executable")
    parser.add_argument("--wire", choices=("v1", "v2"), action="append", help="repeatable; defaults to both")
    parser.add_argument("--timeout", type=positive_timeout, default=30.0, help="seconds per readiness/verify check (default: 30)")
    args = parser.parse_args(argv)
    for label, binary in (("agent", args.agent), ("server", args.server)):
        if not binary.is_file() or not os.access(binary, os.X_OK):
            parser.error(f"{label} must be an existing executable for this host")
    for wire in dict.fromkeys(args.wire or ("v1", "v2")):
        run_wire(args.agent.resolve(), args.server.resolve(), wire, args.timeout)
    print("PASS: native FRP baseline only; monitoring extensions are not exercised", flush=True)
    return 0


def interrupted(_signum: int, _frame: object) -> None:
    raise KeyboardInterrupt


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
    except http.client.HTTPException:
        print("FAIL: invalid local Dashboard HTTP response", file=sys.stderr)
        raise SystemExit(1)
    except OSError as error:
        # Avoid disclosing paths or environment values from exception strings.
        print(f"FAIL: local operating-system error (errno {error.errno})", file=sys.stderr)
        raise SystemExit(1)
