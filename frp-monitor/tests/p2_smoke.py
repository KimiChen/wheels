#!/usr/bin/env python3
"""Loopback P2 binary acceptance: TCP probes, history, restart and fault isolation.

Only local processes and dynamically allocated loopback ports are used. Secrets,
TLS material, SQLite data and private task targets live in a temporary directory.
The test requires the P2 graceful shutdown path to persist partial minute buckets.
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
import signal
import socket
import sys
import tempfile
import threading
import time

from p1_smoke import PublicAPI, assert_redacted, generate_certificate
from smoke import (Child, LOOPBACK, SmokeFailure, child, echo_matches, echo_server,
                   interrupted, port_open, positive_timeout, reserve_port, verify,
                   wait_for, write_private)


NODE_ID = "p2-smoke-node"


class CountedTarget:
    """Count TCP handshakes without reading or emitting application data."""

    def __init__(self) -> None:
        self.listener = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        self.listener.bind((LOOPBACK, 0))
        self.listener.listen(16)
        self.listener.settimeout(.1)
        self.port = self.listener.getsockname()[1]
        self.lock = threading.Lock()
        self.accepted = 0
        self.stopped = threading.Event()
        self.thread = threading.Thread(target=self._run, daemon=True)

    def _run(self) -> None:
        while not self.stopped.is_set():
            try:
                connection, _ = self.listener.accept()
            except socket.timeout:
                continue
            except OSError:
                return
            with connection, self.lock:
                self.accepted += 1

    def count(self) -> int:
        with self.lock:
            return self.accepted

    def __enter__(self) -> CountedTarget:
        self.thread.start()
        return self

    def __exit__(self, *unused: object) -> None:
        self.stopped.set()
        self.listener.close()
        self.thread.join(timeout=2)
        if self.thread.is_alive():
            raise SmokeFailure("probe test target did not stop")


class HistoryAPI(PublicAPI):
    def history(self) -> dict:
        status, headers, raw = self.get(f"/api/public/v1/nodes/{NODE_ID}/history?window=1h")
        if status != 200 or headers.get("Cache-Control") != "no-store":
            raise SmokeFailure("history API is not an uncached JSON 200 response")
        payload = json.loads(raw)
        if payload.get("node_id") != NODE_ID or payload.get("step_seconds") != 60:
            raise SmokeFailure("history API returned a wrong node or interval")
        return payload


def stop_cleanly(process: Child) -> None:
    process.stop()
    if process.forced_stop:
        raise SmokeFailure("P2 process required forced shutdown")


def exercise_tunnel(seconds: float, port: int, processes: tuple[Child, ...]) -> None:
    """Keep asserting FRP health while a task update has time to take effect."""
    until = time.monotonic() + seconds
    while time.monotonic() < until:
        for process in processes:
            process.ensure_running()
        if not echo_matches(port):
            raise SmokeFailure("native FRP forwarding failed during P2 activity")
        time.sleep(.1)


def run(agent: Path, monitor: Path, timeout: float) -> None:
    with tempfile.TemporaryDirectory(prefix="frp-monitor-p2-") as temporary, ExitStack() as stack:
        # Resolve macOS /var symlinks before passing the private SQLite path.
        directory = Path(temporary).resolve()
        directory.chmod(0o700)
        reservations = [stack.enter_context(reserve_port()) for _ in range(4)]
        control, remote, monitor_socket, refused = reservations
        control_port, remote_port, monitor_port, refused_port = [s.getsockname()[1] for s in reservations]
        echo_port = stack.enter_context(echo_server())
        target = stack.enter_context(CountedTarget())
        certificate, key = generate_certificate(directory)
        token, frp_token = secrets.token_urlsafe(32), secrets.token_hex(32)
        token_file, credentials = directory / "agent.token", directory / "credentials.json"
        task_file, database = directory / "probe-tasks.json", directory / "history" / "monitor.sqlite"
        write_private(token_file, token + "\n")
        write_private(credentials, json.dumps([{"agent_id": NODE_ID, "name": "P2 public node", "token_sha256": hashlib.sha256(token.encode()).hexdigest()}]))

        def tasks(version: int, port: int | None, name: str = "TCP demo") -> None:
            configured = [] if port is None else [{"id": "smoke-probe", "name": name, "target": f"{LOOPBACK}:{port}", "interval": 5}]
            staging = task_file.with_suffix(".next")
            write_private(staging, json.dumps({"version": version, "nodes": [{"agent_id": NODE_ID, "tasks": configured}]}))
            staging.replace(task_file)

        tasks(1, target.port)
        common = f'auth.method = "token"\nauth.token = "{frp_token}"\nlog.to = "console"\nlog.level = "info"\nlog.disablePrintColor = true\n'
        server_config = directory / "server.toml"

        def configure_server(path: Path) -> None:
            staging = server_config.with_suffix(".next")
            write_private(staging, f'bindAddr = "{LOOPBACK}"\nproxyBindAddr = "{LOOPBACK}"\nbindPort = {control_port}\nallowPorts = [{{single={remote_port}}}]\n' + common +
                          f'\n[monitor]\nenabled = true\nbindAddr = "{LOOPBACK}"\nbindPort = {monitor_port}\nserverID = "p2-smoke"\ncertFile = {json.dumps(str(certificate))}\nkeyFile = {json.dumps(str(key))}\ncredentialsFile = {json.dumps(str(credentials))}\nreportIntervalSeconds = 1\ndatabaseFile = {json.dumps(str(path))}\nretentionDays = 7\nprobeTasksFile = {json.dumps(str(task_file))}\n')
            staging.replace(server_config)

        configure_server(database)
        client_config = directory / "agent.toml"
        write_private(client_config, f'serverAddr = "{LOOPBACK}"\nserverPort = {control_port}\nloginFailExit = false\nclientID = "private-p2-client"\ntransport.protocol = "tcp"\ntransport.wireProtocol = "v2"\ntransport.tls.enable = true\n' + common +
                      f'\n[telemetry]\nenabled = true\nendpoint = "wss://{LOOPBACK}:{monitor_port}/agent/v1/ws"\ntokenFile = {json.dumps(str(token_file))}\ncaFile = {json.dumps(str(certificate))}\nserverID = "p2-smoke"\nintervalSeconds = 1\nprobeEnabled = true\nprobeAllowPrivate = true\n' +
                      f'\n[[proxies]]\nname = "private-p2-tunnel"\ntype = "tcp"\nlocalIP = "{LOOPBACK}"\nlocalPort = {echo_port}\nremotePort = {remote_port}\n')
        verify(monitor, server_config, "P2 server TOML verification", timeout)
        verify(agent, client_config, "P2 agent TOML verification", timeout)
        control.close(); remote.close(); monitor_socket.close()
        api = HistoryAPI(monitor_port, certificate)
        hidden = (token, frp_token, "private-p2-client", "private-p2-tunnel", LOOPBACK, str(task_file), str(database))

        def launch_server() -> Child:
            instance = stack.enter_context(child("P2 monitor", [str(monitor), "-c", str(server_config)], directory))
            wait_for("P2 listeners", lambda: port_open(control_port) and port_open(monitor_port), (instance,), timeout)
            wait_for("P2 public listener readiness", lambda: api.node()["session"] == "waiting", (instance,), timeout)
            return instance

        def launch_agent(server: Child) -> Child:
            instance = stack.enter_context(child("P2 agent", [str(agent), "-c", str(client_config)], directory))
            wait_for("P2 FRP forwarding", lambda: echo_matches(remote_port), (server, instance), timeout)
            wait_for("P2 live monitoring", lambda: api.node()["session"] == "online" and api.node()["freshness"] == "fresh", (server, instance), timeout)
            return instance

        server = launch_server()
        client = launch_agent(server)
        wait_for("initial TCP probe handshakes", lambda: target.count() >= 2, (server, client), timeout)
        initial = api.history()
        if initial["storage"]["state"] != "ready" or initial["probes_state"] != "ready":
            raise SmokeFailure("P2 storage or probe negotiation is not ready")
        assert_redacted(initial, hidden)
        assert_redacted(api.snapshot(), hidden)
        assert_redacted(api.event(), hidden)
        print("PASS P2: real loopback TCP probes, verified WSS, FRP echo and public redaction", flush=True)

        stop_cleanly(client); stop_cleanly(server)
        server = launch_server()
        restored = api.history()
        probe = restored["probes"][0]
        if probe["samples"] < 1 or probe["failures"] != 0 or probe["latency_ms"] is None or probe["latency_ms"] < 0:
            raise SmokeFailure("graceful restart lost successful probe history")
        if not any(point["samples"] > 0 for point in restored["points"]):
            raise SmokeFailure("graceful restart lost host report history")
        if not any(point["samples"] > 0 and point["latency_ms"] is not None for point in probe["points"]):
            raise SmokeFailure("probe history has no mean latency bucket")
        node = api.node()
        if node["session"] != "waiting" or node["metrics"] is not None or restored["probes_state"] != "waiting":
            raise SmokeFailure("historical state incorrectly restored a live session")
        assert_redacted(restored, hidden)
        if database.stat().st_mode & 0o077:
            raise SmokeFailure("SQLite history permissions are public")
        print("PASS P2: partial-minute history survives graceful restart; node remains waiting", flush=True)

        client = launch_agent(server)
        tasks(2, refused_port, "Refused TCP demo")
        wait_for("versioned target replacement", lambda: api.history()["probes"][0]["name"] == "Refused TCP demo", (server, client), timeout)
        if api.history()["probes"][0]["samples"] != 0:
            raise SmokeFailure("new target inherited the old target's history")
        exercise_tunnel(7, remote_port, (server, client))
        stop_cleanly(client); stop_cleanly(server)
        server = launch_server()
        rejected = api.history()["probes"][0]
        if rejected["samples"] < 1 or rejected["failures"] != rejected["samples"] or rejected["latency_ms"] != -1 or rejected["failure_rate"] != 100:
            raise SmokeFailure("refused TCP endpoint did not persist failed probe samples")
        if any(point["latency_ms"] is not None for point in rejected["points"]):
            raise SmokeFailure("failed probes fabricated a successful mean latency")
        print("PASS P2: task replacement separates target history; refusal produces failure samples", flush=True)

        client = launch_agent(server)
        previous = target.count()
        tasks(3, target.port)
        wait_for("restored TCP target", lambda: target.count() > previous, (server, client), timeout)
        tasks(4, None)
        wait_for("empty task list", lambda: api.history()["probes"] == [] and api.history()["probes_state"] == "disabled", (server, client), timeout)
        exercise_tunnel(2, remote_port, (server, client))
        after_cancel = target.count()
        exercise_tunnel(6, remote_port, (server, client))
        if target.count() != after_cancel:
            raise SmokeFailure("cleared probe task continued to dial")
        print("PASS P2: full-list clear cancels TCP probing while FRP continues forwarding", flush=True)

        stop_cleanly(client); stop_cleanly(server)
        broken = directory / "invalid-database-directory"
        broken.mkdir(mode=0o700)
        configure_server(broken)
        server = launch_server()
        client = launch_agent(server)
        if api.history()["storage"]["state"] != "degraded":
            raise SmokeFailure("unavailable SQLite storage did not expose degraded state")
        exercise_tunnel(2, remote_port, (server, client))
        assert_redacted(api.history(), hidden + (str(broken),))
        if api.get(f"/api/public/v1/nodes/{NODE_ID}/history?window=invalid")[0] != 400:
            raise SmokeFailure("unbounded history window was accepted")
        print("PASS P2: storage initialization failure leaves realtime monitoring and FRP operational", flush=True)
        stop_cleanly(client); stop_cleanly(server)


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
    print("PASS: P2 binary integration complete; production capacity and Linux live collection are outside this check", flush=True)
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
