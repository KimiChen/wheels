#!/usr/bin/env python3
"""P4 binary acceptance: fresh SQLite control, numeric IDs and optional embedded history.

All configuration is created in a private temporary loopback installation. GitHub
OAuth flow and administrator authorization are tested by Go tests; this smoke does
not add a token-login fallback or contact GitHub.
"""
from __future__ import annotations

import argparse
from contextlib import ExitStack, closing
import json
import os
from pathlib import Path
import signal
import sqlite3
import sys
import tempfile

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "scripts"))
import local
from p1_smoke import PublicAPI, assert_redacted
from smoke import SmokeFailure, child, interrupted, port_open, positive_timeout, reserve_port, verify, wait_for


def run(agent: Path, monitor: Path, timeout: float, history: bool = False):
    with tempfile.TemporaryDirectory(prefix="frp-monitor-p4-") as temporary, ExitStack() as stack:
        root = Path(temporary).resolve()
        reservations = [stack.enter_context(reserve_port()) for _ in range(2)]
        control_port, monitor_port = [sock.getsockname()[1] for sock in reservations]
        config = local.settings(root, {"FRP_SERVER_PORT": str(control_port), "FRP_MONITOR_PORT": str(monitor_port),
                                      "FRP_MONITOR_HISTORY_DATA_PATH": "history" if history else "", "FRP_AGENT_NAME": "P4 public node"})
        folder = local.initialize(root / "runtime", probes=True, config=config)
        token = (folder / "agent.token").read_text().strip()
        private_note = "private-p4-note"
        with closing(sqlite3.connect(folder / "control.sqlite")) as database, database:
            database.execute("UPDATE nodes SET private_note=?,traffic_adjustment_bytes=?,traffic_quota_bytes=?,traffic_reset_mode='manual' WHERE id=1",
                             (private_note, "161061273600", "1099511627776"))
        for binary, filename in ((monitor, "server.toml"), (agent, "agent.toml")):
            verify(binary, folder / filename, "P4 configuration verification", timeout)
        for sock in reservations:
            sock.close()
        server = stack.enter_context(child("P4 monitor", [str(monitor), "-c", str(folder / "server.toml")], folder))
        wait_for("P4 listeners", lambda: port_open(control_port) and port_open(monitor_port), (server,), timeout)
        api = PublicAPI(monitor_port, folder / "local.crt")
        node = api.node()
        if node["id"] != "1" or node["session"] != "waiting":
            raise SmokeFailure("new numeric node must wait for its agent")
        if node.get("billing") is not None or node.get("traffic_plan", {}).get("used_bytes") != "161061273600":
            raise SmokeFailure("public policy or persisted quota calibration mismatch")
        if "traffic_today" not in node:
            raise SmokeFailure("current daily traffic missing without history dependency")
        auth_status, _, auth_body = api.get("/api/admin/v1/auth")
        if auth_status != 200 or json.loads(auth_body) != {"provider": "github", "enabled": False}:
            raise SmokeFailure("unconfigured GitHub login must be explicit")
        for path in ("/node/1", "/src/node-settings.mjs", "/admin/"):
            if api.get(path)[0] != 200:
                raise SmokeFailure("P4 public shell is unavailable")
        if api.get("/node/01")[0] != 404:
            raise SmokeFailure("noncanonical numeric detail route was accepted")
        if api.get("/api/admin/v1/session")[0] not in (401, 404):
            raise SmokeFailure("administrator session accessible anonymously")
        client = stack.enter_context(child("P4 agent", [str(agent), "-c", str(folder / "agent.toml")], folder))
        wait_for("P4 telemetry", lambda: api.node()["session"] == "online" and api.node()["freshness"] == "fresh", (server, client), timeout)
        # The default automatic interface filter is the valid empty string, not
        # missing identity. Linux network samples must still establish a durable
        # accounting baseline; macOS intentionally reports unsupported metrics.
        if api.node().get("metrics", {}).get("net_rx_total", {}).get("quality") == "ok":
            def baseline():
                with closing(sqlite3.connect((folder / "control.sqlite").as_uri() + "?mode=ro", uri=True)) as database:
                    row = database.execute("SELECT counter_interface,counter_rx_bytes FROM nodes WHERE id=1").fetchone()
                    return row is not None and row[0] == "" and row[1] is not None
            wait_for("default automatic-interface counter baseline", baseline, (server, client), timeout)
        assert_redacted(api.snapshot(), (token, private_note))
        assert_redacted(api.event(), (token, private_note))
        status, _, body = api.get("/api/public/v1/nodes/1/history?window=1h")
        data = json.loads(body)
        if status != 200 or data.get("storage", {}).get("state") != ("ready" if history else "disabled"):
            raise SmokeFailure("optional history state mismatch")
        if history:
            def samples():
                status, _, body = api.get("/api/public/v1/nodes/1/history?window=1h")
                data = json.loads(body)
                return status == 200 and (bool(data.get("points")) or any(probe.get("samples", 0) for probe in data.get("probes", [])))
            wait_for("embedded TSDB samples", samples, (server, client), max(timeout, 30))
        else:
            if (folder / "history").exists():
                raise SmokeFailure("disabled history created a TSDB directory")
        if int(api.node()["traffic_plan"]["used_bytes"]) < 161061273600:
            raise SmokeFailure("agent reports overwrote manual quota calibration")
        client.stop()
        server.stop()
        if client.forced_stop or server.forced_stop:
            raise SmokeFailure("P4 processes required forced shutdown")
        with closing(sqlite3.connect(folder / "control.sqlite")) as database:
            if database.execute("PRAGMA integrity_check").fetchone() != ("ok",):
                raise SmokeFailure("control database integrity failure")
            if database.execute("SELECT traffic_adjustment_bytes FROM nodes WHERE id=1").fetchone() != ("161061273600",):
                raise SmokeFailure("shutdown lost quota calibration")
        restarted = stack.enter_context(child("P4 restarted monitor", [str(monitor), "-c", str(folder / "server.toml")], folder))
        wait_for("restarted monitoring listener", lambda: port_open(monitor_port), (restarted,), timeout)
        if api.node()["session"] == "online" or int(api.node()["traffic_plan"]["used_bytes"]) < 161061273600:
            raise SmokeFailure("restart fabricated live state or lost persisted quota usage")
        restarted.stop()
        if restarted.forced_stop:
            raise SmokeFailure("restarted P4 process required forced shutdown")
        print(f"PASS P4: numeric node, current SQLite counters, GitHub-only login, redacted live JSON/SSE, restart recovery; history {'enabled' if history else 'disabled'}", flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--agent", type=Path, required=True)
    parser.add_argument("--server", type=Path, required=True)
    parser.add_argument("--timeout", type=positive_timeout, default=30)
    parser.add_argument("--history", action="store_true", help="also verify enabled embedded TSDB sampling")
    args = parser.parse_args()
    for binary in (args.agent, args.server):
        if not binary.is_file() or not os.access(binary, os.X_OK):
            parser.error("agent and server must be executable binaries for this host")
    for sig in (signal.SIGTERM, signal.SIGINT):
        signal.signal(sig, interrupted)
    try:
        run(args.agent.resolve(), args.server.resolve(), args.timeout, args.history)
    except (SmokeFailure, OSError, ValueError, sqlite3.Error) as exc:
        print(f"FAIL P4: {exc}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
