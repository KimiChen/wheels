#!/usr/bin/env python3
"""Loopback monitor acceptance with current SQLite control and optional history.

Uses real binaries, TLS, TCP probes and forwarding. SQLite changes simulate local
operator edits and exercise production reload; GitHub OAuth authorization remains
covered by Go tests. No test login endpoint, .env secrets or external host is used.
"""
from __future__ import annotations

import argparse
from contextlib import ExitStack, closing
import hashlib
import http.client
import json
import os
from pathlib import Path
import re
import secrets
import signal
import socket
import sqlite3
import ssl
import sys
import tempfile
import threading
import time

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "scripts"))
import local
from smoke import (Bridge, Child, LOOPBACK, PublicAPI, SmokeFailure, assert_redacted,
                   child, echo_matches, echo_server, interrupted, port_open,
                   positive_timeout, reserve_port, verify, wait_for, write_private)


class CountedTarget:
    """Count actual TCP handshakes to prove task replacement and cancellation."""

    def __init__(self):
        self.listener = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        self.listener.bind((LOOPBACK, 0))
        self.listener.listen(16)
        self.listener.settimeout(.1)
        self.port = self.listener.getsockname()[1]
        self.lock = threading.Lock()
        self.accepted = 0
        self.stopped = threading.Event()
        self.thread = threading.Thread(target=self._run, daemon=True)

    def _run(self):
        while not self.stopped.is_set():
            try:
                connection, _ = self.listener.accept()
            except socket.timeout:
                continue
            except OSError:
                return
            with connection, self.lock:
                self.accepted += 1

    def count(self):
        with self.lock:
            return self.accepted

    def __enter__(self):
        self.thread.start()
        return self

    def __exit__(self, *_unused):
        self.stopped.set()
        self.listener.close()
        self.thread.join(timeout=2)
        if self.thread.is_alive():
            raise SmokeFailure("probe test target did not stop")


def replace_private(path: Path, content: str):
    staging = path.with_name(path.name + ".next")
    write_private(staging, content)
    staging.replace(path)


def stop_cleanly(*processes: Child):
    for process in processes:
        process.stop()
        if process.forced_stop:
            raise SmokeFailure("process required forced shutdown")


def exercise_tunnel(seconds: float, port: int, processes: tuple[Child, ...]):
    until = time.monotonic() + seconds
    while time.monotonic() < until:
        for process in processes:
            process.ensure_running()
        if not echo_matches(port):
            raise SmokeFailure("native FRP forwarding failed during monitor activity")
        time.sleep(.1)


def run(agent: Path, monitor: Path, timeout: float, history: bool = False):
    with tempfile.TemporaryDirectory(prefix="frp-monitor-smoke-") as temporary, ExitStack() as stack:
        root = Path(temporary).resolve()
        reservations = [stack.enter_context(reserve_port()) for _ in range(4)]
        control_socket, monitor_socket, remote_socket, refused_socket = reservations
        control_port, monitor_port, remote_port, refused_port = [sock.getsockname()[1] for sock in reservations]
        echo_port = stack.enter_context(echo_server())
        target = stack.enter_context(CountedTarget())
        config = local.settings(root, {"FRP_SERVER_PORT": str(control_port), "FRP_MONITOR_PORT": str(monitor_port),
                                      "FRP_MONITOR_HISTORY_DATA_PATH": "history" if history else "", "FRP_AGENT_NAME": "Smoke public node"})
        folder = local.initialize(root / "runtime", probes=True, config=config)
        token_file, database_path = folder / "agent.token", folder / "control.sqlite"
        token = token_file.read_text().strip()
        private_note, private_client_id = "private-smoke-note", "private-smoke-client"
        hidden = (token, (folder / "frp.token").read_text().strip(), private_note, private_client_id, LOOPBACK, "private-smoke-echo")

        def update(sql, parameters=()):
            with closing(sqlite3.connect(database_path)) as database, database:
                database.execute(sql, parameters)

        def tasks(version, port, name="TCP demo"):
            configured = [] if port is None else [{"id": "smoke-probe", "name": name, "target": f"{LOOPBACK}:{port}", "interval": 5}]
            document = {"version": version, "nodes": [{"agent_id": "1", "tasks": configured}]}
            update("UPDATE settings SET probe_json=? WHERE id=1", (json.dumps(document),))

        update("UPDATE nodes SET private_note=?,traffic_adjustment_bytes=?,traffic_quota_bytes=?,traffic_reset_mode='manual',frp_binding=? WHERE id=1",
               (private_note, "161061273600", "1099511627776", json.dumps({"server_id": "local", "user": "", "raw_client_id": private_client_id})))
        tasks(1, target.port)
        telemetry_bridge, frp_bridge = Bridge(monitor_port), Bridge(control_port)
        for bridge in (telemetry_bridge, frp_bridge):
            bridge.start()
            stack.callback(bridge.stop)
        server_config = folder / "server.toml"
        replace_private(server_config, f'allowPorts = [{{single={remote_port}}}]\n' + server_config.read_text())
        base_agent = (folder / "agent.toml").read_text().replace('clientID = "1"', f'clientID = "{private_client_id}"')
        base_agent = base_agent.replace(f':{monitor_port}/agent/v1/ws', f':{telemetry_bridge.port}/agent/v1/ws')

        def agent_config(name, port, proxy=False):
            path = folder / name
            text = base_agent.replace(f'serverPort = {control_port}\n', f'serverPort = {port}\n')
            if proxy:
                text += f'\n[[proxies]]\nname = "private-smoke-echo"\ntype = "tcp"\nlocalIP = "{LOOPBACK}"\nlocalPort = {echo_port}\nremotePort = {remote_port}\n'
            write_private(path, text)
            verify(agent, path, "monitor agent configuration", timeout)
            return path

        failed_config = agent_config("agent-unavailable.toml", refused_port)
        empty_config = agent_config("agent-empty.toml", frp_bridge.port)
        client_config = agent_config("agent-tunnel.toml", frp_bridge.port, True)
        verify(monitor, server_config, "monitor server configuration", timeout)
        control_socket.close()
        monitor_socket.close()
        api = PublicAPI(monitor_port, folder / "local.crt")

        def launch_server():
            instance = stack.enter_context(child("monitor", [str(monitor), "-c", str(server_config)], folder))
            wait_for("FRP and monitoring listeners", lambda: port_open(control_port) and port_open(monitor_port), (instance,), timeout)
            return instance

        def online():
            node = api.node()
            return node["session"] == "online" and node["freshness"] == "fresh"

        def launch_agent(server):
            instance = stack.enter_context(child("agent", [str(agent), "-c", str(client_config)], folder))
            wait_for("FRP tunnel", lambda: echo_matches(remote_port), (server, instance), timeout)
            wait_for("fresh monitoring", online, (server, instance), timeout)
            return instance

        server = launch_server()
        node = api.node()
        if node["id"] != "1" or node["session"] != "waiting" or node.get("billing") is not None:
            raise SmokeFailure("new numeric node or public billing policy mismatch")
        if node.get("traffic_plan", {}).get("used_bytes") != "161061273600" or "traffic_today" not in node:
            raise SmokeFailure("current traffic requires persisted calibration without history")
        untrusted = http.client.HTTPSConnection(LOOPBACK, monitor_port, timeout=3)
        try:
            try:
                untrusted.request("GET", "/api/public/v1/nodes")
                untrusted.getresponse()
            except ssl.SSLCertVerificationError:
                pass
            else:
                raise SmokeFailure("untrusted monitoring TLS certificate was accepted")
        finally:
            untrusted.close()
        upgrade = {"Connection": "Upgrade", "Upgrade": "websocket", "Sec-WebSocket-Version": "13", "Sec-WebSocket-Key": "c21va2UtdGVzdC1ub25jZQ=="}
        for bearer in (None, secrets.token_urlsafe(32)):
            headers = upgrade | ({"Authorization": "Bearer " + bearer} if bearer else {})
            if api.get("/agent/v1/ws", headers)[0] != 401:
                raise SmokeFailure("missing or invalid node credential was accepted")
        status, _, body = api.get("/api/admin/v1/auth")
        if status != 200 or json.loads(body) != {"provider": "github", "enabled": False}:
            raise SmokeFailure("unconfigured GitHub login must be explicit")
        for path in ("/node/1", "/src/node-settings.mjs", "/admin/"):
            if api.get(path)[0] != 200:
                raise SmokeFailure("public or administrator shell is unavailable")
        for path in ("/node/01", "/README.md", "/handler.go", "/.env", "/assets/", "/src/", "/api/admin/v1/login"):
            if api.get(path)[0] != 404:
                raise SmokeFailure("invalid route or private resource is exposed")
        if api.get("/api/admin/v1/session")[0] not in (401, 404):
            raise SmokeFailure("administrator session accessible anonymously")
        status, headers, body = api.get("/")
        if status != 200 or b"/src/app.mjs" not in body or "script-src 'self'" not in headers.get("Content-Security-Policy", ""):
            raise SmokeFailure("embedded public page or CSP is missing")
        print("PASS monitor: numeric node, private TLS, GitHub-only login and static/public access rules", flush=True)

        with child("FRP unavailable agent", [str(agent), "-c", str(failed_config)], folder) as failed:
            wait_for("monitoring before first FRP login", lambda: online() and api.node()["frp"]["control_state"] in ("connecting", "disconnected"), (server, failed), timeout)
        with child("no-proxy agent", [str(agent), "-c", str(empty_config)], folder) as empty:
            wait_for("connected FRP without proxies", lambda: online() and api.node()["frp"]["control_state"] == "connected" and api.node()["frp"]["proxy_total"] == 0, (server, empty), timeout)
        remote_socket.close()
        client = launch_agent(server)
        wait_for("real TCP probes", lambda: target.count() >= 2, (server, client), timeout)
        for payload in (api.snapshot(), api.event(), api.history()):
            assert_redacted(payload, hidden)
        initial = api.history()
        if initial["storage"]["state"] != ("ready" if history else "disabled") or initial["probes_state"] != "ready":
            raise SmokeFailure("optional history or probe negotiation state mismatch")
        if not history and (folder / "history").exists():
            raise SmokeFailure("disabled history created a TSDB directory")
        # Linux automatic-interface samples establish a durable baseline; macOS
        # intentionally exposes unsupported network fields.
        if api.node().get("metrics", {}).get("net_rx_total", {}).get("quality") == "ok":
            def baseline():
                with closing(sqlite3.connect(database_path.as_uri() + "?mode=ro", uri=True)) as database:
                    row = database.execute("SELECT counter_interface,counter_rx_bytes FROM nodes WHERE id=1").fetchone()
                    return row is not None and row[0] == "" and row[1] is not None
            wait_for("automatic-interface counter baseline", baseline, (server, client), timeout)

        frp_bridge.stop()
        wait_for("independent FRP disconnect", lambda: online() and api.node()["frp"]["control_state"] == "disconnected", (server, client), timeout)
        frp_bridge.start()
        wait_for("FRP reconnect", lambda: echo_matches(remote_port) and api.node()["frp"]["control_state"] == "connected", (server, client), timeout)
        telemetry_bridge.stop()
        wait_for("independent monitoring disconnect", lambda: api.node()["session"] == "offline", (server, client), timeout)
        exercise_tunnel(1, remote_port, (server, client))
        wait_for("stale monitoring", lambda: api.node()["freshness"] == "stale", (server, client), max(timeout, 15))
        telemetry_bridge.start()
        wait_for("monitoring reconnect", online, (server, client), timeout)
        exercise_tunnel(1, remote_port, (server, client))
        print("PASS monitor: independent FRP/monitor disconnect, staleness and reconnect preserve the other service", flush=True)

        stop_cleanly(client, server)
        with closing(sqlite3.connect(database_path)) as database:
            if database.execute("PRAGMA integrity_check").fetchone() != ("ok",):
                raise SmokeFailure("control database integrity failure")
            if database.execute("SELECT traffic_adjustment_bytes FROM nodes WHERE id=1").fetchone() != ("161061273600",):
                raise SmokeFailure("shutdown lost manual quota calibration")
        server = launch_server()
        if api.node()["session"] != "waiting" or api.node()["metrics"] is not None or int(api.node()["traffic_plan"]["used_bytes"]) < 161061273600:
            raise SmokeFailure("restart fabricated live state or lost persisted quota usage")
        if history:
            restored = api.history()
            probe = restored["probes"][0]
            if probe["samples"] < 1 or probe["failures"] != 0 or probe["latency_ms"] is None or not any(point["samples"] for point in restored["points"]):
                raise SmokeFailure("graceful restart lost host or successful probe history")
            if not any(point["latency_ms"] is not None for point in probe["points"]):
                raise SmokeFailure("successful probe history has no latency bucket")
        print("PASS monitor: SQLite calibration and optional host/probe history survive graceful restart", flush=True)

        client = launch_agent(server)
        tasks(2, refused_port, "Refused TCP demo")
        wait_for("replaced probe target", lambda: api.history()["probes"][0]["name"] == "Refused TCP demo", (server, client), timeout)
        if history:
            wait_for("failed probe history", lambda: api.history()["probes"][0]["samples"] > 0, (server, client), max(timeout, 30))
            probe = api.history()["probes"][0]
            if probe["samples"] != probe["failures"] or probe["latency_ms"] != -1 or probe["failure_rate"] != 100 or any(point["latency_ms"] is not None for point in probe["points"]):
                raise SmokeFailure("refused target inherited successful results or fabricated latency")
        previous = target.count()
        tasks(3, target.port)
        wait_for("restored TCP target", lambda: target.count() > previous, (server, client), timeout)
        tasks(4, None)
        wait_for("empty probe task list", lambda: api.history()["probes"] == [] and api.history()["probes_state"] == "disabled", (server, client), timeout)
        exercise_tunnel(2, remote_port, (server, client))
        after_cancel = target.count()
        exercise_tunnel(6, remote_port, (server, client))
        if target.count() != after_cancel:
            raise SmokeFailure("cleared probe task continued dialing")
        print("PASS monitor: SQLite task replacement, refused TCP target and full-list cancellation", flush=True)

        rotated = secrets.token_urlsafe(32)
        update("UPDATE nodes SET token_sha256=?,config_revision=config_revision+1 WHERE id=1", (hashlib.sha256(rotated.encode()).hexdigest(),))
        wait_for("rotation closes old monitoring session", lambda: api.node()["session"] == "waiting", (server, client), timeout)
        if api.get("/agent/v1/ws", upgrade | {"Authorization": "Bearer " + token})[0] != 401:
            raise SmokeFailure("rotated credential remains authorized")
        exercise_tunnel(1, remote_port, (server, client))
        stop_cleanly(client)
        replace_private(token_file, rotated + "\n")
        client = launch_agent(server)
        assert_redacted(api.snapshot(), hidden + (rotated,))
        print("PASS monitor: SQLite credential rotation revokes old monitoring access while FRP continues", flush=True)

        # Only optional TSDB initialization may degrade. A control DB failure is
        # covered separately because that database is mandatory.
        stop_cleanly(client, server)
        broken = folder / "invalid-history"
        write_private(broken, "not a directory\n")
        replace_private(server_config, re.sub(r'^historyDataPath = .*$', f'historyDataPath = {json.dumps(str(broken))}', server_config.read_text(), flags=re.MULTILINE))
        server = launch_server()
        client = launch_agent(server)
        if api.history()["storage"]["state"] != "degraded":
            raise SmokeFailure("unavailable optional history did not expose degraded state")
        exercise_tunnel(1, remote_port, (server, client))
        assert_redacted(api.history(), hidden + (rotated, str(broken)))
        if api.get("/api/public/v1/nodes/1/history?window=invalid")[0] != 400:
            raise SmokeFailure("unbounded history window was accepted")
        update("DELETE FROM nodes WHERE id=1")
        update("UPDATE settings SET probe_json=? WHERE id=1", (json.dumps({"version": 5, "nodes": []}),))
        wait_for("revoked node disappears", lambda: api.snapshot()["nodes"] == [], (server, client), timeout)
        if api.get("/api/public/v1/nodes/1/history?window=1h")[0] != 404:
            raise SmokeFailure("revoked node history remains public")
        exercise_tunnel(1, remote_port, (server, client))
        stop_cleanly(client, server)
        server = launch_server()
        if api.snapshot()["nodes"] != []:
            raise SmokeFailure("revoked node reappeared after restart")
        stop_cleanly(server)
        print(f"PASS monitor: optional-history failure and node revocation preserve FRP; history {'enabled' if history else 'disabled'} acceptance complete", flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--agent", type=Path, required=True)
    parser.add_argument("--server", type=Path, required=True)
    parser.add_argument("--timeout", type=positive_timeout, default=30)
    parser.add_argument("--history", action="store_true", help="also verify embedded TSDB host/probe persistence")
    args = parser.parse_args()
    for binary in (args.agent, args.server):
        if not binary.is_file() or not os.access(binary, os.X_OK):
            parser.error("agent and server must be executable binaries for this host")
    for sig in (signal.SIGTERM, signal.SIGINT):
        signal.signal(sig, interrupted)
    try:
        run(args.agent.resolve(), args.server.resolve(), args.timeout, args.history)
    except KeyboardInterrupt:
        print("FAIL monitor: interrupted; test processes cleaned up", file=sys.stderr)
        return 130
    except (SmokeFailure, OSError, ValueError, sqlite3.Error, http.client.HTTPException) as exc:
        print(f"FAIL monitor: {exc}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
