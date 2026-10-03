#!/usr/bin/env python3
"""Real FRP detail adapter acceptance using a transparent loopback WS relay.

Runs enhanced agent/server binaries, captures the independently negotiated detail
frames, and checks the real public monitor API. Private admin API authentication
is covered separately by Go tests; this is not a GitHub OAuth end-to-end test.
All credentials/configuration are disposable, and child output is discarded.
Only Python's standard library and the repository's smoke helpers are required.
"""
from __future__ import annotations

import argparse
from contextlib import ExitStack
import copy
import http.client
from http.server import ThreadingHTTPServer
import json
import os
from pathlib import Path
import secrets
import select
import signal
import socket
import socketserver
import subprocess
import sys
import tempfile
import threading

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "scripts"))
import local
from frp_smoke import HTTPPayload, UDPEcho, http_matches, local_server, udp_matches
from smoke import (Bridge, LOOPBACK, PublicAPI, SmokeFailure, assert_redacted,
                   child, echo_matches, echo_server, interrupted, port_open,
                   positive_timeout, reserve_port, verify, wait_for, write_private)


MAX_FRAME = 256 * 1024


class ClientFrames:
    """Bounded incremental decoder for masked client WS messages after HTTP.

    The HTTP request (including its Authorization header) is discarded as soon
    as the header terminator arrives. Neither it nor any raw frames are logged.
    Fragmentation/control frames are supported because large Gorilla writes can
    split one JSON message across several frames.
    """

    def __init__(self, observe):
        self.observe = observe
        self.buffer = bytearray()
        self.upgraded = False
        self.fragment: bytearray | None = None

    def feed(self, data: bytes) -> None:
        self.buffer.extend(data)
        if not self.upgraded:
            end = self.buffer.find(b"\r\n\r\n")
            if end < 0:
                if len(self.buffer) > 16384:
                    raise SmokeFailure("capture HTTP header exceeded limit")
                return
            if end > 16384 or not self.buffer.startswith(b"GET /agent/v1/ws "):
                raise SmokeFailure("capture received an unexpected HTTP upgrade")
            del self.buffer[:end + 4]
            self.upgraded = True
        while len(self.buffer) >= 2:
            first, second = self.buffer[:2]
            final, opcode = bool(first & 128), first & 15
            if first & 112 or not second & 128 or opcode not in (0, 1, 8, 9, 10):
                raise SmokeFailure("capture received an invalid WS frame")
            length, offset = second & 127, 2
            extra = 2 if length == 126 else 8 if length == 127 else 0
            if len(self.buffer) < offset + extra:
                return
            if extra:
                length = int.from_bytes(self.buffer[offset:offset + extra], "big")
                offset += extra
            if length > MAX_FRAME or (opcode >= 8 and (length > 125 or not final)):
                raise SmokeFailure("capture WS frame exceeded limit")
            if len(self.buffer) < offset + 4 + length:
                return
            mask = self.buffer[offset:offset + 4]
            offset += 4
            payload = bytes(value ^ mask[i % 4] for i, value in enumerate(self.buffer[offset:offset + length]))
            del self.buffer[:offset + length]
            if opcode >= 8:
                continue
            if opcode == 1:
                if self.fragment is not None:
                    raise SmokeFailure("capture received overlapping WS messages")
                self.fragment = bytearray()
            elif self.fragment is None:
                raise SmokeFailure("capture received an unexpected WS continuation")
            self.fragment.extend(payload)
            if len(self.fragment) > MAX_FRAME:
                raise SmokeFailure("capture WS message exceeded limit")
            if final:
                try:
                    frame = json.loads(self.fragment)
                except (UnicodeError, json.JSONDecodeError) as exc:
                    raise SmokeFailure("capture received invalid JSON") from exc
                self.fragment = None
                self.observe(frame)


class Capture:
    """Keep only the newest detail, report counters and session sequence."""

    def __init__(self):
        self.lock = threading.Lock()
        self.session = None
        self.sequence = 0
        self.report_count = 0
        self.detail_count = 0
        self.detail = None
        self.fault = None

    def fail(self, reason: str) -> None:
        with self.lock:
            self.fault = reason

    def observe(self, frame: dict) -> None:
        if not isinstance(frame, dict) or frame.get("jsonrpc") != "2.0" or not isinstance(frame.get("params"), dict):
            raise SmokeFailure("capture received an invalid RPC envelope")
        method, params = frame.get("method"), frame["params"]
        session, sequence = params.get("session_id"), params.get("sequence")
        if not isinstance(session, str) or not session or type(sequence) is not int or not 0 < sequence < 2 ** 64:
            raise SmokeFailure("capture received invalid sequence metadata")
        with self.lock:
            if method == "hello":
                if sequence != 1 or "frp.detail.v1" not in params.get("capabilities", []):
                    raise SmokeFailure("detail capability was not negotiated")
                self.session, self.sequence, self.detail = session, 1, None
                return
            if session != self.session or sequence <= self.sequence:
                raise SmokeFailure("capture sequence did not increase within its session")
            self.sequence = sequence
            if method == "report":
                if isinstance(params.get("metrics"), dict):
                    self.report_count += 1
            elif method == "frp.detail":
                if not isinstance(params.get("detail"), dict):
                    raise SmokeFailure("capture detail was not an object")
                self.detail = params["detail"]
                self.detail_count += 1

    def snapshot(self) -> dict:
        with self.lock:
            if self.fault:
                raise SmokeFailure(self.fault)
            return {"detail": copy.deepcopy(self.detail), "reports": self.report_count,
                    "details": self.detail_count, "session": self.session}


class CaptureBridge(Bridge):
    """Forward unchanged bytes to the real monitor; inspect client frames only."""

    def __init__(self, target: int, capture: Capture):
        super().__init__(target)
        self.capture = capture

    def _relay(self, client: socket.socket, stop: threading.Event) -> None:
        upstream = None
        parser = ClientFrames(self.capture.observe)
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
                    if source is client:
                        parser.feed(data)
                    (upstream if source is client else client).sendall(data)
        except SmokeFailure as exc:
            self.capture.fail(str(exc))
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


class LoopbackPublicAPI(PublicAPI):
    def __init__(self, port: int):
        self.port = port

    def connection(self):
        return http.client.HTTPConnection(LOOPBACK, self.port, timeout=3)


def replace_private(path: Path, text: str) -> None:
    staged = path.with_name(path.name + ".next")
    write_private(staged, text)
    staged.replace(path)


def check_private_fields(payload: dict, hidden: tuple[str, ...]) -> None:
    assert_redacted(payload, hidden)
    forbidden = {"frp_detail", "service_id", "configuration", "visitors", "endpoints",
                 "control_error", "shadowed_sources", "bind_endpoint", "plugin_type"}
    def walk(value):
        if isinstance(value, dict):
            if forbidden.intersection(value):
                raise SmokeFailure("public API exposed private FRP detail")
            for nested in value.values():
                walk(nested)
        elif isinstance(value, list):
            for nested in value:
                walk(nested)
    walk(payload)


def item(detail: dict | None, collection: str, name: str) -> dict:
    if not detail or detail.get("state") != "ready":
        return {}
    return next((entry for entry in detail.get(collection, []) if entry.get("name") == name), {})


def endpoint(proxy: dict, kind: str, source: str) -> dict:
    return next((entry for entry in proxy.get("endpoints", [])
                 if entry.get("kind") == kind and entry.get("source") == source), {})


def echo_tick(connection: socket.socket) -> None:
    payload = b"visitor live connection"
    connection.sendall(payload)
    received = bytearray()
    while len(received) < len(payload):
        chunk = connection.recv(len(payload) - len(received))
        if not chunk:
            raise SmokeFailure("Visitor connection closed prematurely")
        received.extend(chunk)
    if received != payload:
        raise SmokeFailure("Visitor connection altered payload")


def reload_native(agent: Path, path: Path, timeout: float) -> None:
    verify(agent, path, "reloaded detail smoke configuration", timeout)
    with child("native FRP reload", [str(agent), "reload", "-c", str(path)], path.parent) as process:
        try:
            if process.process.wait(timeout=timeout):
                raise SmokeFailure("native FRP file reload failed")
        except subprocess.TimeoutExpired as exc:
            raise SmokeFailure("native FRP file reload timed out") from exc


def run(agent: Path, server: Path, wire: str, timeout: float) -> None:
    with tempfile.TemporaryDirectory(prefix="frp-plus-detail-") as temporary, ExitStack() as stack:
        root = Path(temporary).resolve()
        root.chmod(0o700)
        reserved = [stack.enter_context(reserve_port()) for _ in range(6)]
        control_port, monitor_port, http_port, visitor_port, dashboard_port, xtcp_port = [s.getsockname()[1] for s in reserved]
        refused = stack.enter_context(reserve_port())
        refused_port = refused.getsockname()[1]
        udp_reserved = stack.enter_context(socket.socket(socket.AF_INET, socket.SOCK_DGRAM))
        udp_reserved.bind((LOOPBACK, 0))
        sudp_port = udp_reserved.getsockname()[1]
        stun_reserved = stack.enter_context(socket.socket(socket.AF_INET, socket.SOCK_DGRAM))
        stun_reserved.bind((LOOPBACK, 0))
        stun_port = stun_reserved.getsockname()[1]
        tcp_local = stack.enter_context(echo_server())
        http_local = stack.enter_context(local_server(ThreadingHTTPServer, HTTPPayload))
        udp_local = stack.enter_context(local_server(socketserver.UDPServer, UDPEcho))
        settings = local.settings(root, {"FRP_SERVER_PORT": str(control_port), "FRP_MONITOR_PORT": str(monitor_port),
                                         "FRP_AGENT_NAME": "Detail smoke public node"})
        folder = local.initialize(root / "runtime", plain_http=True, config=settings)
        capture = Capture()
        bridge = CaptureBridge(monitor_port, capture)
        bridge.start()
        stack.callback(bridge.stop)
        secret, password = secrets.token_hex(32), secrets.token_hex(32)
        hidden = (secret, password, (folder / "agent.token").read_text().strip(),
                  (folder / "frp.token").read_text().strip(), str(folder),
                  "dynamic-file", "include-web", "store-tcp", "shadowed", "late-service", "private-visitor",
                  "datagram-private", "datagram-visitor", "fallback-visitor", "absent-xtcp", LOOPBACK)
        server_path, agent_path = folder / "server.toml", folder / "agent.toml"
        include_path, store_path = folder / "include.toml", folder / "store.json"
        replace_private(server_path, f"vhostHTTPPort = {http_port}\n" + server_path.read_text())
        write_private(include_path, f'[[proxies]]\nname = "include-web"\ntype = "http"\nlocalIP = "{LOOPBACK}"\n'
                      f'localPort = {http_local}\ncustomDomains = ["smoke.invalid"]\n')
        store = {"proxies": [{"name": name, "type": "tcp", "localIP": LOOPBACK, "localPort": tcp_local, "remotePort": 0}
                              for name in ("store-tcp", "shadowed")], "visitors": []}
        write_private(store_path, json.dumps(store) + "\n")
        main = (f'includes = [{json.dumps(str(include_path))}]\nstore.path = {json.dumps(str(store_path))}\n'
                f'webServer.addr = "{LOOPBACK}"\nwebServer.port = {dashboard_port}\nwebServer.user = "smoke"\n'
                f'webServer.password = "{password}"\ntransport.protocol = "tcp"\ntransport.wireProtocol = "{wire}"\n'
                f'natHoleStunServer = "{LOOPBACK}:{stun_port}"\ntransport.tls.enable = true\n' + agent_path.read_text())
        main = main.replace(f':{monitor_port}/agent/v1/ws', f':{bridge.port}/agent/v1/ws')
        for name in ("dynamic-file", "shadowed"):
            local_port = refused_port if name == "shadowed" else tcp_local
            main += (f'\n[[proxies]]\nname = "{name}"\ntype = "tcp"\nlocalIP = "{LOOPBACK}"\n'
                     f'localPort = {local_port}\nremotePort = 0\n')
        main += (f'\n[[visitors]]\nname = "private-visitor"\ntype = "stcp"\nserverName = "late-service"\n'
                 f'secretKey = "{secret}"\nbindAddr = "{LOOPBACK}"\nbindPort = {visitor_port}\n')
        main += (f'\n[[proxies]]\nname = "datagram-private"\ntype = "sudp"\nlocalIP = "{LOOPBACK}"\n'
                 f'localPort = {udp_local}\nsecretKey = "{secret}"\n'
                 f'\n[[visitors]]\nname = "datagram-visitor"\ntype = "sudp"\nserverName = "datagram-private"\n'
                 f'secretKey = "{secret}"\nbindAddr = "{LOOPBACK}"\nbindPort = {sudp_port}\n'
                 f'\n[[visitors]]\nname = "fallback-visitor"\ntype = "xtcp"\nserverName = "absent-xtcp"\n'
                 f'secretKey = "{secret}"\nbindAddr = "{LOOPBACK}"\nbindPort = {xtcp_port}\n'
                 'protocol = "quic"\nkeepTunnelOpen = false\nfallbackTo = "private-visitor"\nfallbackTimeoutMs = 200\n')
        replace_private(agent_path, main)
        for binary, path in ((server, server_path), (agent, agent_path)):
            verify(binary, path, "detail smoke configuration", timeout)
        for reservation in reserved:
            reservation.close()
        udp_reserved.close()
        service = stack.enter_context(child("detail server", [str(server), "-c", str(server_path)], folder))
        wait_for("detail server listeners", lambda: port_open(control_port) and port_open(monitor_port), (service,), timeout)
        client = stack.enter_context(child("detail agent", [str(agent), "-c", str(agent_path)], folder))
        processes = (service, client)
        api = LoopbackPublicAPI(monitor_port)

        def detail():
            return capture.snapshot()["detail"]

        def ready_detail(label):
            observed = []
            def ready():
                current = detail()
                if current and current.get("state") == "ready":
                    observed.append(current)
                    return True
                return False
            wait_for(label, ready, processes, timeout)
            return observed[0]

        def initial_ready():
            current = detail()
            expected = ("dynamic-file", "include-web", "store-tcp", "shadowed", "datagram-private")
            return (all(item(current, "proxies", name).get("status") == "running" for name in expected)
                    and all(item(current, "visitors", name).get("local_state") == "listening"
                            for name in ("private-visitor", "datagram-visitor", "fallback-visitor")))

        wait_for("initial private FRP detail", initial_ready, processes, timeout)
        initial = ready_detail("ready initial detail after native sampling")
        if any(private in json.dumps(initial) for private in hidden[:5]):
            raise SmokeFailure("private detail copied native credentials or configuration paths")
        config = initial["configuration"]
        if (config.get("source") != "mixed" or config.get("native_entry") != "store"
                or not config.get("revision") or not config.get("read_at") or config.get("dashboard_enabled") is not True):
            raise SmokeFailure("native configuration metadata is incomplete")
        for name, origin in (("dynamic-file", "file"), ("include-web", "include"), ("store-tcp", "store"), ("shadowed", "store")):
            proxy = item(initial, "proxies", name)
            if proxy.get("source") != origin:
                raise SmokeFailure("native proxy source did not match its effective configuration")
        shadowed = item(initial, "proxies", "shadowed")
        if shadowed.get("source_state") != "active" or "file" not in shadowed.get("shadowed_sources", []):
            raise SmokeFailure("Store precedence over file was not visible")
        transport = initial["transport"]
        if (transport.get("protocol") != "tcp" or transport.get("wire_protocol") != wire
                or transport.get("source") != "configured" or transport.get("tls") is not True):
            raise SmokeFailure("native transport and wire details were conflated")
        visitor = item(initial, "visitors", "private-visitor")
        if visitor.get("remote_state") != "unknown" or visitor.get("error") is not None:
            raise SmokeFailure("local Visitor listener falsely asserted remote connectivity")
        if visitor.get("bind_endpoint", {}).get("source") != "observed":
            raise SmokeFailure("Visitor actual local listener was not observed")
        for name in ("datagram-visitor", "fallback-visitor"):
            current = item(initial, "visitors", name)
            if current.get("remote_state") != "unknown" or current.get("source") != "file":
                raise SmokeFailure("idle private Visitor invented remote activity or a source")
        xtcp = item(initial, "visitors", "fallback-visitor")
        if xtcp.get("protocol") != "quic" or xtcp.get("p2p_state") != "unknown" or xtcp.get("fallback_state") != "available":
            raise SmokeFailure("XTCP configured transport was conflated with observed activity")
        sudp = item(initial, "visitors", "datagram-visitor")
        if sudp.get("bind_endpoint", {}).get("kind") != "udp" or sudp.get("protocol") != "tcp":
            raise SmokeFailure("SUDP local datagram listener was conflated with control transport")
        dynamic = item(initial, "proxies", "dynamic-file")
        configured, observed = endpoint(dynamic, "tcp", "configured"), endpoint(dynamic, "tcp", "observed")
        # Native frpc reports TCP RemoteAddr as :port; an unknown observed host
        # must stay empty. The test's server config independently pins loopback.
        if configured.get("port") != 0 or not observed.get("port") or observed.get("host") not in ("", LOOPBACK):
            raise SmokeFailure("dynamic TCP request and actual allocated port were not distinguished")
        wait_for("observed dynamic TCP forwarding", lambda: echo_matches(observed["port"]), processes, timeout)
        wait_for("included HTTP forwarding", lambda: http_matches(http_port), processes, timeout)
        http_endpoint = endpoint(item(initial, "proxies", "include-web"), "http", "observed")
        if http_endpoint.get("host") != "smoke.invalid" or http_endpoint.get("port") != http_port:
            raise SmokeFailure("HTTP observed endpoint did not match its registered listener")
        for name in ("store-tcp", "shadowed"):
            target = endpoint(item(initial, "proxies", name), "tcp", "observed").get("port")
            wait_for("Store TCP forwarding", lambda port=target: bool(port) and echo_matches(port), processes, timeout)
        wait_for("fresh public metrics", lambda: api.node().get("freshness") == "fresh", processes, timeout)
        before_failure = capture.snapshot()
        before_metrics = api.node().get("metrics_at")
        print(f"PASS detail {wire}: file/include/Store priority, dynamic TCP and HTTP endpoints", flush=True)

        # This is the first local Visitor connection: testing port_open earlier
        # would itself initiate the remote attempt and invalidate the assertion.
        with socket.create_connection((LOOPBACK, visitor_port), timeout=2) as failed:
            failed.settimeout(2)
            failed.sendall(b"expected remote rejection")
            try:
                if failed.recv(128):
                    raise SmokeFailure("absent STCP service unexpectedly forwarded data")
            except (ConnectionResetError, socket.timeout):
                pass

        def visitor_failed():
            state = item(detail(), "visitors", "private-visitor")
            error = state.get("error") or {}
            return state.get("remote_state") == "error" and error.get("code") == "peer_failed" and error.get("at") and not error.get("recovered_at")

        wait_for("Visitor peer failure observation", visitor_failed, processes, timeout)
        failure_at = item(ready_detail("ready Visitor failure detail"), "visitors", "private-visitor")["error"]["at"]
        wait_for("host metrics during Visitor failure", lambda: capture.snapshot()["reports"] > before_failure["reports"]
                 and api.node().get("metrics_at") != before_metrics, processes, timeout)
        main += (f'\n[[proxies]]\nname = "late-service"\ntype = "stcp"\nlocalIP = "{LOOPBACK}"\n'
                 f'localPort = {tcp_local}\nsecretKey = "{secret}"\n')
        replace_private(agent_path, main)
        reload_native(agent, agent_path, timeout)
        wait_for("reloaded STCP service", lambda: item(detail(), "proxies", "late-service").get("status") == "running", processes, timeout)
        current = ready_detail("ready reloaded detail after native sampling")
        if current["configuration"].get("revision") == config["revision"] or current["configuration"].get("read_at") == config["read_at"]:
            raise SmokeFailure("native reload did not refresh source revision and read time")
        private_endpoint = endpoint(item(current, "proxies", "late-service"), "visitor", "observed")
        if not private_endpoint or private_endpoint.get("host") or private_endpoint.get("port") is not None:
            raise SmokeFailure("private STCP proxy invented a public listener")
        with socket.create_connection((LOOPBACK, visitor_port), timeout=2) as connected:
            connected.settimeout(2)
            def recovered():
                # Keep the connection active across at least one detail sample.
                echo_tick(connected)
                state = item(detail(), "visitors", "private-visitor")
                error = state.get("error") or {}
                return (state.get("remote_state") == "connected" and error.get("at") == failure_at
                        and error.get("recovered_at") and error["recovered_at"] >= failure_at)
            wait_for("Visitor live connection and error recovery", recovered, processes, timeout)
        wait_for("Visitor closed connection", lambda: item(detail(), "visitors", "private-visitor").get("remote_state") == "closed", processes, timeout)
        print(f"PASS detail {wire}: Visitor unknown/error/connected/closed, native reload and recovery times", flush=True)

        wait_for("SUDP datagram forwarding", lambda: udp_matches(sudp_port), processes, timeout)
        def sudp_active():
            state = item(detail(), "visitors", "datagram-visitor")
            return state.get("remote_state") == "connected" and state.get("local_state") == "listening"
        # Closing the caller's UDP socket does not terminate SUDP's remote work
        # connection. Verify its independent lifetime before closing the proxy.
        wait_for("SUDP remote work connection", sudp_active, processes, timeout)
        main = main.replace('name = "datagram-private"\ntype = "sudp"',
                            'name = "datagram-private"\ntype = "sudp"\nenabled = false', 1)
        replace_private(agent_path, main)
        reload_native(agent, agent_path, timeout)
        def sudp_closed():
            state = item(detail(), "visitors", "datagram-visitor")
            return (state.get("remote_state") == "closed" and state.get("local_state") == "listening"
                    and item(detail(), "proxies", "datagram-private").get("status") == "disabled")
        wait_for("SUDP remote close with local listener retained", sudp_closed, processes, timeout)
        print(f"PASS detail {wire}: SUDP datagram, independent local listener and remote connected/closed", flush=True)

        # The target XTCP proxy does not exist, so native PreCheck fails before
        # STUN discovery. Even an unexpected discovery attempt stays loopback.
        # With empty native user, fallbackTo is the exact unprefixed Visitor
        # name. Keep one real echoed stream open to observe active fallback.
        with socket.create_connection((LOOPBACK, xtcp_port), timeout=2) as fallback:
            fallback.settimeout(2)
            def fallback_active():
                echo_tick(fallback)
                current = detail()
                state = item(current, "visitors", "fallback-visitor")
                target = item(current, "visitors", "private-visitor")
                error = state.get("error") or {}
                return (state.get("fallback_state") == "active" and state.get("p2p_state") == "failed"
                        and state.get("remote_state") == "error" and state.get("local_state") == "listening"
                        and state.get("protocol") == "quic" and error.get("code") == "nat_traversal_failed"
                        and error.get("at") and not error.get("recovered_at")
                        and target.get("remote_state") == "connected")
            wait_for("XTCP actual STCP fallback and P2P failure", fallback_active, processes, timeout)
        def fallback_closed():
            current = detail()
            state = item(current, "visitors", "fallback-visitor")
            return (state.get("fallback_state") == "available" and state.get("p2p_state") == "failed"
                    and item(current, "visitors", "private-visitor").get("remote_state") == "closed")
        wait_for("XTCP fallback connection close", fallback_closed, processes, timeout)
        print(f"PASS detail {wire}: XTCP failed P2P, real STCP fallback active and returned to available", flush=True)

        for public in (api.snapshot(), api.event()):
            check_private_fields(public, hidden)
        # OAuth is deliberately unconfigured here: production hides all admin
        # APIs with 404. Authenticated access and 401 are exercised by Go tests.
        if api.get("/api/admin/v1/nodes/1/frp-detail")[0] != 404:
            raise SmokeFailure("unconfigured admin detail API was unexpectedly exposed")
        final = capture.snapshot()
        if final["session"] != before_failure["session"] or final["reports"] <= before_failure["reports"] or final["details"] < 4:
            raise SmokeFailure("native detail activity interrupted the telemetry session")
        if api.node().get("freshness") != "fresh":
            raise SmokeFailure("host metrics did not remain fresh after native detail activity")
        print(f"PASS detail {wire}: monotonic frames, continuing host reports, public JSON/SSE privacy", flush=True)
        for process in (client, service):
            process.stop()
            if process.forced_stop:
                raise SmokeFailure("detail smoke process required forced shutdown")


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--agent", type=Path, required=True)
    parser.add_argument("--server", type=Path, required=True)
    parser.add_argument("--wire", choices=("v1", "v2"), action="append")
    parser.add_argument("--timeout", type=positive_timeout, default=30.0)
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
    except (OSError, ValueError, http.client.HTTPException):
        print("FAIL: local detail smoke I/O failed; private diagnostics suppressed", file=sys.stderr)
        raise SystemExit(1)
