#!/usr/bin/env python3
"""Isolated native control-transport and TLS/authentication regression.

Every connection uses loopback and disposable private files. This checks FRP
control and TCP business payloads, independently of the monitoring transport.
It does not claim cross-NAT behavior or production capacity.
"""
from __future__ import annotations

import argparse
from contextlib import ExitStack, contextmanager
import hashlib
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
import time

from smoke import (Child, LOGIN_MARKER, LOOPBACK, PAYLOAD, SmokeFailure, child_environment,
                   interrupted, port_open,
                   positive_timeout, reserve_port, verify, wait_for, write_private)

TRANSPORTS = ("tcp", "kcp", "quic", "websocket", "wss")
FAILURE_MARKERS = {
    "untrusted_ca": (b"certificate signed by unknown authority",),
    "wrong_name": (b"certificate is valid for", b"cannot validate certificate"),
    "token_mismatch": (b"token in login doesn't match",),
    "client_certificate": (b"certificate required", b"bad certificate"),
}


class ObservedChild(Child):
    """Keep fixed failure evidence, never arbitrary error strings or secrets."""
    def __init__(self, label, argv, directory, failure_markers=None):
        self.failure_markers = failure_markers or FAILURE_MARKERS
        self.failures = {key: threading.Event() for key in self.failure_markers}
        self.registered = threading.Event()
        super().__init__(label, argv, directory)

    def _read_output(self):
        tail = b""
        while chunk := self.process.stdout.read1(4096):
            combined = tail + chunk
            if LOGIN_MARKER in combined:
                self.logged_in.set()
            if b"start proxy success" in combined:
                self.registered.set()
            for kind, markers in self.failure_markers.items():
                if any(marker in combined for marker in markers):
                    self.failures[kind].set()
            tail = combined[-256:]


@contextmanager
def transport_echo_server():
    class Handler(socketserver.BaseRequestHandler):
        def handle(self):
            self.request.settimeout(10)
            try:
                while data := self.request.recv(16384):
                    self.request.sendall(data)
            except OSError:
                pass
    class Server(socketserver.ThreadingTCPServer):
        daemon_threads = True
        block_on_close = False
    with Server((LOOPBACK, 0), Handler) as server:
        thread = threading.Thread(target=server.serve_forever, kwargs={"poll_interval": .05}, daemon=True)
        thread.start()
        try:
            yield server.server_address[1]
        finally:
            server.shutdown()
            thread.join(timeout=2)
            if thread.is_alive():
                raise SmokeFailure("transport echo fixture did not stop")


def require_payload(port, timeout=5):
    # Readiness is verified separately. Once the actual business request starts,
    # any timeout, truncation or corruption fails instead of retrying it away.
    try:
        deadline = time.monotonic() + timeout
        with socket.create_connection((LOOPBACK, port), timeout=timeout) as connection:
            connection.settimeout(timeout)
            connection.sendall(PAYLOAD)
            received = bytearray()
            while len(received) < len(PAYLOAD):
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise TimeoutError()
                connection.settimeout(remaining)
                data = connection.recv(min(16384, len(PAYLOAD) - len(received)))
                if not data:
                    break
                received.extend(data)
            if received != PAYLOAD:
                raise SmokeFailure("transport TCP payload was truncated or altered")
    except OSError as error:
        raise SmokeFailure("transport TCP payload did not finish within its deadline") from error


@contextmanager
def observed_child(label, binary, config, directory, failure_markers=None):
    process = ObservedChild(label, [str(binary), "-c", str(config)], directory, failure_markers)
    try:
        yield process
    finally:
        process.stop()
        if process.forced_stop:
            raise SmokeFailure("transport fixture needed forced process cleanup")


def certificate(directory: Path, name: str):
    cert, key = directory / (name + ".crt"), directory / (name + ".key")
    write_private(cert, "")
    write_private(key, "")
    result = subprocess.run(
        ["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
         "-subj", "/CN=frp-transport-fixture", "-addext", "subjectAltName=IP:127.0.0.1,DNS:transport.invalid",
         "-keyout", str(key), "-out", str(cert)], cwd=directory, env=child_environment(),
        stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
        timeout=30, check=False)
    if result.returncode:
        raise SmokeFailure("disposable transport certificate generation failed")
    cert.chmod(0o600)
    key.chmod(0o600)
    return cert, key


def config_texts(transport, wire, control_port, datagram_port, remote_port,
                 target_port, token, cert, key, *, negative=None, other_cert=None,
                 mux=True, mutual_tls=False, wss_port=None):
    if transport not in TRANSPORTS or wire not in ("v1", "v2"):
        raise ValueError("invalid transport fixture selection")
    common = 'log.to = "console"\nlog.level = "info"\nlog.disablePrintColor = true\n'
    auth = 'auth.method = "token"\nauth.token = ' + json.dumps(token) + '\n'
    if transport == "wss" and not wss_port:
        raise ValueError("WSS requires an explicit TLS termination port")
    ports = f'{{single={remote_port}}}' + (f',{{single={wss_port}}}' if transport == "wss" else '')
    # The pinned upstream frps accepts WS, not WSS. Its own E2E uses a
    # separate native https2http client plugin for TLS termination. The
    # resulting plaintext WS hop is loopback-only and cannot use force=true.
    server = (f'bindAddr = "{LOOPBACK}"\nproxyBindAddr = "{LOOPBACK}"\nbindPort = {control_port}\n'
              f'allowPorts = [{ports}]\ntransport.tcpMux = {str(mux).lower()}\n'
              f'transport.tls.certFile = {json.dumps(str(cert))}\ntransport.tls.keyFile = {json.dumps(str(key))}\n'
              f'transport.tls.force = {str(transport != "wss").lower()}\n' + common + auth)
    if transport in ("kcp", "quic"):
        server += f'{transport}BindPort = {datagram_port}\n'
    if mutual_tls:
        server += f'transport.tls.trustedCaFile = {json.dumps(str(cert))}\n'
    port = datagram_port if transport in ("kcp", "quic") else control_port
    if transport == "wss":
        port = wss_port
    client = (f'serverAddr = "{LOOPBACK}"\nserverPort = {port}\nloginFailExit = true\n'
              f'transport.protocol = "{transport}"\ntransport.wireProtocol = "{wire}"\n'
              f'transport.tcpMux = {str(mux).lower()}\ntransport.tls.enable = true\n'
              f'transport.tls.trustedCaFile = {json.dumps(str(other_cert if negative == "untrusted_ca" else cert))}\n'
              f'transport.tls.serverName = "{"wrong.invalid" if negative == "wrong_name" else "transport.invalid"}"\n'
              + common + (auth.replace(json.dumps(token), json.dumps(token + "-mismatch")) if negative == "token_mismatch" else auth))
    if mutual_tls and negative != "client_certificate":
        client += f'transport.tls.certFile = {json.dumps(str(cert))}\ntransport.tls.keyFile = {json.dumps(str(key))}\n'
    client += (f'\n[[proxies]]\nname = "transport-payload"\ntype = "tcp"\nlocalIP = "{LOOPBACK}"\n'
               f'localPort = {target_port}\nremotePort = {remote_port}\n'
               'transport.useEncryption = true\ntransport.useCompression = true\n')
    return server, client


def require_rejection(client, server, remote_port, expected, timeout):
    # An unrelated crash, verify failure or closed port alone is not evidence of
    # authentication rejection: the expected native failure must be observed.
    wait_for("explicit native authentication rejection", lambda: client.process.poll() is not None,
             (server,), timeout)
    client.reader.join(timeout=1)
    evidence = {"exit_code": client.process.returncode, "logged_in": client.logged_in.is_set(),
                "expected_rejection": client.failures[expected].is_set(), "remote_open": port_open(remote_port)}
    if evidence["logged_in"] or evidence["exit_code"] == 0 or not evidence["expected_rejection"] or evidence["remote_open"]:
        raise SmokeFailure("negative authentication case lacked its expected native rejection: " + json.dumps(evidence))


def run_case(agent, server, transport, wire, timeout, *, negative=None, mux=True, mutual_tls=False):
    label = f"{transport}-{wire}" + ("-via-native-https2http" if transport == "wss" else "") + ("-no-mux" if not mux else "") + ("-mtls" if mutual_tls else "") + ("-" + negative if negative else "")
    with tempfile.TemporaryDirectory(prefix="frp-transport-") as temporary, ExitStack() as stack:
        root = Path(temporary).resolve()
        root.chmod(0o700)
        control = stack.enter_context(reserve_port())
        remote = stack.enter_context(reserve_port())
        wss = stack.enter_context(reserve_port()) if transport == "wss" else None
        wss_port = wss.getsockname()[1] if wss else None
        datagram = stack.enter_context(socket.socket(socket.AF_INET, socket.SOCK_DGRAM))
        datagram.bind((LOOPBACK, 0))
        control_port, remote_port, datagram_port = control.getsockname()[1], remote.getsockname()[1], datagram.getsockname()[1]
        target_port = stack.enter_context(transport_echo_server())
        cert, key = certificate(root, "control")
        other_cert = certificate(root, "untrusted")[0] if negative == "untrusted_ca" else None
        token = secrets.token_hex(32)
        server_text, client_text = config_texts(transport, wire, control_port, datagram_port, remote_port,
                                               target_port, token, cert, key, negative=negative,
                                               other_cert=other_cert, mux=mux, mutual_tls=mutual_tls,
                                               wss_port=wss_port)
        server_config, client_config = root / "server.toml", root / "client.toml"
        write_private(server_config, server_text)
        write_private(client_config, client_text)
        verify(server, server_config, "transport server configuration", timeout)
        verify(agent, client_config, "transport client configuration", timeout)
        control.close()
        datagram.close()
        service = stack.enter_context(observed_child("transport server", server, server_config, root))
        wait_for("transport server startup", lambda: port_open(control_port), (service,), timeout)
        if wss:
            _, bridge_text = config_texts("tcp", wire, control_port, datagram_port, wss_port,
                                         target_port, token, cert, key, mux=mux)
            bridge_text = bridge_text.split("\n[[proxies]]", 1)[0] + (
                '\n[[proxies]]\nname = "wss-termination"\ntype = "tcp"\n'
                f'remotePort = {wss_port}\n[proxies.plugin]\ntype = "https2http"\n'
                f'localAddr = "{LOOPBACK}:{control_port}"\ncrtPath = {json.dumps(str(cert))}\n'
                f'keyPath = {json.dumps(str(key))}\n')
            bridge_config = root / "tls-termination.toml"
            write_private(bridge_config, bridge_text)
            verify(agent, bridge_config, "native WSS termination configuration", timeout)
            wss.close()
            bridge = stack.enter_context(observed_child("native WSS termination", agent, bridge_config, root))
            wait_for("native WSS termination startup", lambda: bridge.logged_in.is_set() and bridge.registered.is_set(), (service, bridge), timeout)
        remote.close()
        client = stack.enter_context(observed_child("transport client", agent, client_config, root))
        if negative:
            require_rejection(client, service, remote_port, negative, timeout)
        else:
            wait_for("transport login and proxy registration", lambda: client.logged_in.is_set() and client.registered.is_set(), (service, client), timeout)
            # Multiple work connections exercise mux/no-mux, not just one login.
            for _ in range(4):
                require_payload(remote_port)
    print("PASS " + label, flush=True)
    return label


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--agent", type=Path, required=True)
    parser.add_argument("--server", type=Path, required=True)
    parser.add_argument("--wire", choices=("v1", "v2"), action="append")
    parser.add_argument("--transport", choices=TRANSPORTS, action="append")
    parser.add_argument("--timeout", type=positive_timeout, default=20)
    parser.add_argument("--positive-only", action="store_true")
    args = parser.parse_args(argv)
    for binary in (args.agent, args.server):
        if not binary.is_file() or not os.access(binary, os.X_OK):
            parser.error("each binary must be an existing native executable")
    agent, server = args.agent.resolve(), args.server.resolve()
    print(json.dumps({"agent_sha256":hashlib.sha256(agent.read_bytes()).hexdigest(),
                      "server_sha256":hashlib.sha256(server.read_bytes()).hexdigest()}), flush=True)
    for wire in dict.fromkeys(args.wire or ("v1", "v2")):
        for transport in dict.fromkeys(args.transport or TRANSPORTS):
            run_case(agent, server, transport, wire, args.timeout)
        if not args.positive_only:
            run_case(agent, server, "tcp", wire, args.timeout, mux=False)
            run_case(agent, server, "tcp", wire, args.timeout, mutual_tls=True)
            for failure in ("untrusted_ca", "wrong_name", "token_mismatch", "client_certificate"):
                # Yamux can race its worker's TLS failure and report only
                # "session shutdown". Direct TLS preserves the verifier's
                # specific rejection evidence; both mux modes have positives.
                run_case(agent, server, "tcp", wire, args.timeout, negative=failure,
                         mux=failure=="token_mismatch", mutual_tls=failure=="client_certificate")
    return 0


if __name__ == "__main__":
    signal.signal(signal.SIGTERM, interrupted)
    try:
        raise SystemExit(main())
    except KeyboardInterrupt:
        print("FAIL: interrupted; isolated processes cleaned up", file=sys.stderr)
        raise SystemExit(130)
    except (SmokeFailure, subprocess.TimeoutExpired) as error:
        print("FAIL: "+str(error), file=sys.stderr)
        raise SystemExit(1)
