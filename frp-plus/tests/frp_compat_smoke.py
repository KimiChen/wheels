#!/usr/bin/env python3
"""Exercise genuine old/new Plus binaries over private loopback TLS and FRP TCP.

The old pair must match build_compat_baseline.py's fixed Git archive manifest.
Four real endpoint combinations must maintain one monitoring connection, fresh
public metrics, advancing report times, and unchanged TCP payloads. No login
bypass, production credentials, or remote service is used.
"""
from __future__ import annotations

import argparse
from contextlib import ExitStack, closing, contextmanager
import importlib.util
import json
import os
from pathlib import Path
import signal
import sqlite3
import sys
import tempfile
import threading
import time

from build_compat_baseline import BASELINE_COMMIT, baseline_archive, verify_baseline, frp
from smoke import (Bridge, LOOPBACK, PublicAPI, SmokeFailure, child, echo_matches,
                   echo_server, interrupted, port_open, positive_timeout,
                   reserve_port, verify, wait_for, write_private)

@contextmanager
def baseline_initializer(manifest: dict):
    """Load the initializer and its schema from the verified old Git archive.

    All pairs start with a genuine old database. The new server performs its
    normal upgrade; no current-schema database is presented to the old server.
    The mutable baseline build cache is never used as the source of this code.
    """
    archive = baseline_archive()
    provenance = manifest.get("plus_baseline", {})
    if (provenance.get("commit") != BASELINE_COMMIT or provenance.get("source_verified_clean") is not True
            or provenance.get("archive_sha256") != frp.digest(archive)):
        raise SmokeFailure("compatibility initializer does not match the verified old Git archive")
    with tempfile.TemporaryDirectory(prefix="frp-plus-compat-source-") as temporary:
        source = Path(temporary)
        frp.extract_archive(archive, source)
        # Both paths must be regular archived inputs before executing local code.
        frp.read_regular(source / "scripts/local.py")
        frp.read_regular(source / "monitor/control/schema.sql")
        spec = importlib.util.spec_from_file_location("compat_baseline_local", source / "scripts/local.py")
        initializer = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(initializer)
        yield initializer


def database_version(path: Path) -> int:
    with closing(sqlite3.connect(path.as_uri() + "?mode=ro", uri=True)) as database:
        return database.execute("PRAGMA user_version").fetchone()[0]


class CountedBridge(Bridge):
    """Count monitoring TCP sessions without inspecting private TLS traffic."""

    def __init__(self, target: int):
        super().__init__(target)
        self.accepted = 0
        self.closed = 0

    def _relay(self, client, stop: threading.Event):
        with self.lock:
            self.accepted += 1
        try:
            super()._relay(client, stop)
        finally:
            with self.lock:
                self.closed += 1

    def counts(self):
        with self.lock:
            return self.accepted, self.closed


def require_fresh(node: dict, counts: tuple[int, int]) -> None:
    if counts != (1, 0):
        raise SmokeFailure("monitoring connection closed or reconnected during compatibility observation")
    if node.get("session") != "online" or node.get("freshness") != "fresh" or node.get("metrics") is None:
        raise SmokeFailure("monitoring did not remain continuously online and fresh")
    if node.get("frp", {}).get("control_state") != "connected" or node.get("frp", {}).get("proxy_total", 0) < 1:
        raise SmokeFailure("public FRP observation lost the connected native tunnel")
    if not node.get("metrics_at") or not node.get("last_seen"):
        raise SmokeFailure("fresh public metrics have no report timestamps")


def run_pair(agent: Path, server: Path, label: str, timeout: float, duration: float,
             initializer, *, expect_upgrade: bool) -> None:
    with tempfile.TemporaryDirectory(prefix="frp-plus-compat-") as temporary, ExitStack() as stack:
        root = Path(temporary).resolve()
        reservations = [stack.enter_context(reserve_port()) for _ in range(3)]
        control_port, monitor_port, remote_port = [sock.getsockname()[1] for sock in reservations]
        echo_port = stack.enter_context(echo_server())
        settings = initializer.settings(root, {"FRP_SERVER_PORT": str(control_port), "FRP_MONITOR_PORT": str(monitor_port),
                                         "FRP_MONITOR_INTERVAL_SECONDS": "1", "FRP_AGENT_NAME": "Compatibility node"})
        folder = initializer.initialize(root / "runtime", probes=True, config=settings)
        initial_schema = database_version(folder / "control.sqlite")
        bridge = CountedBridge(monitor_port)
        bridge.start()
        stack.callback(bridge.stop)
        server_config = folder / "compat-server.toml"
        write_private(server_config, f'allowPorts = [{{single={remote_port}}}]\n' + (folder / "server.toml").read_text())
        agent_config = folder / "compat-agent.toml"
        config = (folder / "agent.toml").read_text().replace(
            f':{monitor_port}/agent/v1/ws', f':{bridge.port}/agent/v1/ws')
        config += (f'\n[[proxies]]\nname = "compat-echo"\ntype = "tcp"\nlocalIP = "{LOOPBACK}"\n'
                   f'localPort = {echo_port}\nremotePort = {remote_port}\n')
        write_private(agent_config, config)
        verify(server, server_config, label + " server config", timeout)
        verify(agent, agent_config, label + " agent config", timeout)
        for sock in reservations:
            sock.close()
        service = stack.enter_context(child(label + " server", [str(server), "-c", str(server_config)], folder))
        wait_for(label + " listeners", lambda: port_open(control_port) and port_open(monitor_port), (service,), timeout)
        running_schema = database_version(folder / "control.sqlite")
        if ((expect_upgrade and running_schema <= initial_schema)
                or (not expect_upgrade and running_schema != initial_schema)):
            raise SmokeFailure("compatibility database did not follow the expected old-to-current upgrade path")
        client = stack.enter_context(child(label + " agent", [str(agent), "-c", str(agent_config)], folder))
        processes = (service, client)
        api = PublicAPI(monitor_port, folder / "local.crt")
        wait_for(label + " TCP", lambda: echo_matches(remote_port), processes, timeout)

        def ready():
            node = api.node()
            return (node.get("session") == "online" and node.get("freshness") == "fresh"
                    and node.get("metrics_at") is not None and node.get("frp", {}).get("control_state") == "connected"
                    and node.get("frp", {}).get("proxy_total", 0) >= 1)

        wait_for(label + " initial metrics", ready, processes, timeout)
        metric_times, seen_times = set(), set()
        echoes = 0
        until = time.monotonic() + duration
        while time.monotonic() < until:
            for process in processes:
                process.ensure_running()
            node = api.node()
            require_fresh(node, bridge.counts())
            metric_times.add(node["metrics_at"])
            seen_times.add(node["last_seen"])
            if not echo_matches(remote_port):
                raise SmokeFailure("native TCP forwarding failed during compatibility observation")
            echoes += 1
            time.sleep(.2)
        require_fresh(api.node(), bridge.counts())
        if len(metric_times) < 4 or len(seen_times) < 4 or echoes < 10:
            raise SmokeFailure("insufficient fresh report progression or TCP observations")
        print(f"PASS {label}: {len(metric_times)} fresh metrics, {echoes} TCP payloads, 1 monitor connection, "
              f"0 reconnects, schema {initial_schema}->{running_schema} ({duration:g}s)", flush=True)


def verify_new_pair(agent: Path, server: Path) -> dict:
    if agent.parent != server.parent:
        raise SmokeFailure("new Plus binaries must share their BUILD.json directory")
    manifest = json.loads(frp.read_regular(agent.parent / "BUILD.json"))
    if not isinstance(manifest, dict) or not isinstance(manifest.get("binaries"), dict):
        raise SmokeFailure("new Plus build manifest is invalid")
    for name, binary in (("frp-plus-agent", agent), ("frp-plus-server", server)):
        if frp.digest(frp.read_regular(binary)) != manifest.get("binaries", {}).get(name):
            raise SmokeFailure("new Plus executable does not match its BUILD.json")
    if not manifest.get("patches", {}).get("0005-frp-observation.patch"):
        raise SmokeFailure("new Plus build has no native FRP detail patch")
    return manifest


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("agent", "server", "old-agent", "old-server"):
        parser.add_argument("--" + name, type=Path, required=True)
    parser.add_argument("--old-manifest", type=Path, help="Defaults to BUILD.json beside both old binaries")
    parser.add_argument("--timeout", type=positive_timeout, default=30.0)
    parser.add_argument("--duration", type=positive_timeout, default=12.0, help="Continuous observation seconds per pair, at least 5")
    args = parser.parse_args(argv)
    if args.duration < 5:
        parser.error("--duration must be at least 5 seconds")
    binaries = [args.agent, args.server, args.old_agent, args.old_server]
    if any(not path.is_file() or not os.access(path, os.X_OK) for path in binaries):
        parser.error("all inputs must be executable native binaries")
    agent, server, old_agent, old_server = [path.resolve() for path in binaries]
    if args.old_manifest is None and old_agent.parent != old_server.parent:
        parser.error("separate old binary directories require --old-manifest")
    try:
        old_manifest = verify_baseline(old_agent, old_server, args.old_manifest or old_agent.parent / "BUILD.json")
        new_manifest = verify_new_pair(agent, server)
    except (frp.PipelineError, OSError, ValueError) as exc:
        raise SmokeFailure(f"compatibility input provenance check failed: {exc}") from exc
    if any(new_manifest["binaries"][name] == old_manifest["binaries"][name] for name in ("frp-plus-agent", "frp-plus-server")):
        raise SmokeFailure("old and new compatibility inputs must be different builds")
    print(f"PASS old Plus provenance: {BASELINE_COMMIT}; exact Git archive inputs and binary hashes match", flush=True)
    with baseline_initializer(old_manifest) as initializer:
        for a, s, label, upgrade in ((old_agent, old_server, "old-to-old", False), (agent, old_server, "new-to-old", False),
                                    (old_agent, server, "old-to-new", True), (agent, server, "new-to-new", True)):
            run_pair(a, s, label, args.timeout, args.duration, initializer, expect_upgrade=upgrade)
    return 0


if __name__ == "__main__":
    signal.signal(signal.SIGTERM, interrupted)
    try:
        raise SystemExit(main())
    except KeyboardInterrupt:
        print("FAIL compatibility: interrupted; local processes cleaned up", file=sys.stderr)
        raise SystemExit(130)
    except (SmokeFailure, OSError, ValueError) as exc:
        print(f"FAIL compatibility: {exc}", file=sys.stderr)
        raise SystemExit(1)
