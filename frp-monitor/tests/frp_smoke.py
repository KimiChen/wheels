#!/usr/bin/env python3
"""Native TCP/UDP/HTTP/STCP matrix against enhanced and actual upstream FRP.

All sockets use dynamic loopback ports. Configurations/tokens are generated into
private temporary directories. The upstream binaries must be separately built
from the exact upstream.lock archive; monitoring-disabled enhanced binaries are
explicitly rejected as upstream inputs.
"""
from __future__ import annotations

import argparse
from contextlib import ExitStack, contextmanager
import http.client
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import secrets
import signal
import socket
import socketserver
import subprocess
import sys
import tempfile
import threading

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "scripts"))
import local

from smoke import (LOOPBACK, PAYLOAD, SmokeFailure, child, child_environment,
                   echo_matches, echo_server, interrupted, port_open,
                   positive_timeout, reserve_port, verify, wait_for, write_private)


class UDPEcho(socketserver.BaseRequestHandler):
    def handle(self) -> None:
        payload, connection = self.request
        connection.sendto(payload, self.client_address)


class HTTPPayload(BaseHTTPRequestHandler):
    def do_GET(self) -> None:
        self.send_response(200)
        self.send_header("Content-Type", "application/octet-stream")
        self.send_header("Content-Length", str(len(PAYLOAD)))
        self.end_headers()
        self.wfile.write(PAYLOAD)

    def log_message(self, *_unused: object) -> None:
        pass


@contextmanager
def local_server(server_class: type, handler: type):
    with server_class((LOOPBACK, 0), handler) as server:
        server.daemon_threads = True
        thread = threading.Thread(target=server.serve_forever,
                                  kwargs={"poll_interval": .05}, daemon=True)
        thread.start()
        try:
            yield server.server_address[1]
        finally:
            server.shutdown()
            thread.join(timeout=2)
            if thread.is_alive():
                raise SmokeFailure("local protocol test target did not stop")


def udp_matches(port: int) -> bool:
    payload = PAYLOAD[:1000]
    try:
        with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as connection:
            connection.settimeout(.4)
            connection.sendto(payload, (LOOPBACK, port))
            data, _ = connection.recvfrom(65535)
            if data != payload:
                raise SmokeFailure("UDP tunnel altered the datagram")
            return True
    except OSError:
        return False


def http_matches(port: int) -> bool:
    connection = http.client.HTTPConnection(LOOPBACK, port, timeout=.5)
    try:
        # DNS is not used; the reserved .invalid name is only a routing header.
        connection.request("GET", "/payload", headers={"Host": "smoke.invalid"})
        response = connection.getresponse()
        data = response.read(len(PAYLOAD) + 1)
        if response.status != 200:
            return False
        if data != PAYLOAD:
            raise SmokeFailure("HTTP tunnel altered the response payload")
        return True
    except (OSError, http.client.HTTPException):
        return False
    finally:
        connection.close()


def is_original(binary: Path) -> bool:
    result = subprocess.run([str(binary), "--monitor-version"],
                            env=child_environment(), stdin=subprocess.DEVNULL,
                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                            timeout=10, check=False)
    return result.returncode != 0


def run_pair(agent: Path, server: Path, label: str, wire: str, enhanced_agent: bool,
             enhanced_server: bool, timeout: float) -> None:
    with tempfile.TemporaryDirectory(prefix="frp-monitor-protocol-") as temporary, ExitStack() as stack:
        directory = Path(temporary).resolve()
        directory.chmod(0o700)
        reservations = [stack.enter_context(reserve_port()) for _ in range(5)]
        control_port, remote_port, http_port, visitor_port, monitor_port = [s.getsockname()[1] for s in reservations]
        udp_reservation = stack.enter_context(socket.socket(socket.AF_INET, socket.SOCK_DGRAM))
        udp_reservation.bind((LOOPBACK, 0))
        udp_remote = udp_reservation.getsockname()[1]
        tcp_local = stack.enter_context(echo_server())
        udp_local = stack.enter_context(local_server(socketserver.UDPServer, UDPEcho))
        http_local = stack.enter_context(local_server(ThreadingHTTPServer, HTTPPayload))
        token, monitor_token, secret = secrets.token_hex(32), secrets.token_urlsafe(32), secrets.token_hex(32)
        common = (f'auth.method = "token"\nauth.token = "{token}"\n'
                  'log.to = "console"\nlog.level = "info"\nlog.disablePrintColor = true\n')
        database, token_file = directory / "control.sqlite", directory / "agent.token"
        local.control_database(database, node_name="Protocol test", token=monitor_token, server_id="protocol")
        write_private(token_file, monitor_token + "\n")
        server_config, agent_config, visitor_config = [directory / name for name in ("frps.toml", "frpc.toml", "visitor.toml")]
        text = (f'bindAddr = "{LOOPBACK}"\nproxyBindAddr = "{LOOPBACK}"\nbindPort = {control_port}\n'
                f'vhostHTTPPort = {http_port}\nallowPorts = [{{single={remote_port}}},{{single={udp_remote}}}]\n' + common)
        if enhanced_server:
            text += (f'\n[monitor]\nenabled = true\nbindAddr = "{LOOPBACK}"\nbindPort = {monitor_port}\n'
                     f'serverID = "protocol"\ndatabaseFile = {json.dumps(str(database))}\n')
        write_private(server_config, text)
        client_common = (f'serverAddr = "{LOOPBACK}"\nserverPort = {control_port}\nuser = ""\n'
                         'loginFailExit = false\ntransport.protocol = "tcp"\ntransport.tls.enable = true\n'
                         f'transport.wireProtocol = "{wire}"\n' + common)
        text = client_common + 'clientID = "1"\n'
        if enhanced_agent:
            # When the server is original this endpoint remains refused. FRP
            # must still forward while independent monitoring keeps retrying.
            text += (f'\n[telemetry]\nenabled = true\nendpoint = "ws://{LOOPBACK}:{monitor_port}/agent/v1/ws"\n'
                     f'tokenFile = {json.dumps(str(token_file))}\nserverID = "protocol"\nallowInsecureLoopback = true\n')
        text += (f'\n[[proxies]]\nname = "echo"\ntype = "tcp"\nlocalIP = "{LOOPBACK}"\nlocalPort = {tcp_local}\nremotePort = {remote_port}\n'
                 f'\n[[proxies]]\nname = "datagram"\ntype = "udp"\nlocalIP = "{LOOPBACK}"\nlocalPort = {udp_local}\nremotePort = {udp_remote}\n'
                 f'\n[[proxies]]\nname = "web"\ntype = "http"\nlocalIP = "{LOOPBACK}"\nlocalPort = {http_local}\ncustomDomains = ["smoke.invalid"]\n'
                 f'\n[[proxies]]\nname = "private-echo"\ntype = "stcp"\nlocalIP = "{LOOPBACK}"\nlocalPort = {tcp_local}\nsecretKey = "{secret}"\n')
        write_private(agent_config, text)
        write_private(visitor_config, client_common + 'clientID = "visitor-client"\n' +
                      f'\n[[visitors]]\nname = "private-visitor"\ntype = "stcp"\nserverName = "private-echo"\n'
                      f'secretKey = "{secret}"\nbindAddr = "{LOOPBACK}"\nbindPort = {visitor_port}\n')
        for binary, config in ((server, server_config), (agent, agent_config), (agent, visitor_config)):
            verify(binary, config, f"{label} {wire} configuration", timeout)
        for reserved in reservations:
            reserved.close()
        udp_reservation.close()
        service = stack.enter_context(child(label + " server", [str(server), "-c", str(server_config)], directory))
        wait_for("FRP control listener", lambda: port_open(control_port), (service,), timeout)
        if enhanced_server:
            wait_for("monitor listener", lambda: port_open(monitor_port), (service,), timeout)
        client = stack.enter_context(child(label + " agent", [str(agent), "-c", str(agent_config)], directory))
        visitor = stack.enter_context(child(label + " visitor", [str(agent), "-c", str(visitor_config)], directory))
        processes = (service, client, visitor)
        for protocol, check in (("TCP", lambda: echo_matches(remote_port)),
                                ("UDP", lambda: udp_matches(udp_remote)),
                                ("HTTP", lambda: http_matches(http_port)),
                                ("STCP", lambda: echo_matches(visitor_port))):
            wait_for(f"{label} {wire} {protocol} forwarding", check, processes, timeout)
        for process in processes:
            process.ensure_running()
        print(f"PASS {label} {wire}: TCP/UDP/HTTP/STCP payloads", flush=True)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("agent", "server", "original-agent", "original-server"):
        parser.add_argument("--" + name, type=Path, required=True)
    parser.add_argument("--wire", choices=("v1", "v2"), action="append")
    parser.add_argument("--timeout", type=positive_timeout, default=30.0)
    args = parser.parse_args(argv)
    binaries = [args.agent, args.server, args.original_agent, args.original_server]
    for binary in binaries:
        if not binary.is_file() or not os.access(binary, os.X_OK):
            parser.error("each binary must be an existing native executable")
    agent, server, original_agent, original_server = [b.resolve() for b in binaries]
    if not is_original(original_agent) or not is_original(original_server):
        raise SmokeFailure("upstream inputs are enhanced binaries; build exact original archive separately")
    for wire in dict.fromkeys(args.wire or ("v1", "v2")):
        run_pair(agent, server, "enhanced-to-enhanced", wire, True, True, args.timeout)
        run_pair(original_agent, server, "original-to-enhanced", wire, False, True, args.timeout)
        run_pair(agent, original_server, "enhanced-to-original", wire, True, False, args.timeout)
    return 0


if __name__ == "__main__":
    signal.signal(signal.SIGTERM, interrupted)
    try:
        raise SystemExit(main())
    except KeyboardInterrupt:
        print("FAIL: interrupted; local processes cleaned up", file=sys.stderr)
        raise SystemExit(130)
    except (SmokeFailure, subprocess.TimeoutExpired) as exc:
        print(f"FAIL: {exc}", file=sys.stderr)
        raise SystemExit(1)
