#!/usr/bin/env python3
"""Initialize and run a loopback P2 demonstration with private SQLite history."""
from __future__ import annotations

import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import platform
import secrets
import signal
import subprocess
import sys
import tempfile
import time
import uuid

ROOT = Path(__file__).resolve().parents[1]
KEYS = {"FRP_MONITOR_PORT", "FRP_SERVER_PORT", "FRP_MONITOR_INTERVAL_SECONDS", "FRP_MONITOR_RETENTION_DAYS", "FRP_AGENT_NAME", "FRP_AGENT_IFACE"}


def settings(root=ROOT, environment=None):
    env = os.environ if environment is None else environment
    values = {"FRP_MONITOR_PORT": "17401", "FRP_SERVER_PORT": "17000", "FRP_MONITOR_INTERVAL_SECONDS": "1",
              "FRP_MONITOR_RETENTION_DAYS": "7", "FRP_AGENT_NAME": "本地演示节点", "FRP_AGENT_IFACE": ""}
    path = root / ".env"
    if path.exists():
        if path.is_symlink() or not path.is_file():
            raise ValueError(".env must be a regular file")
        for line in path.read_text(encoding="utf-8").splitlines():
            line = line.strip()
            if line.startswith("export "):
                line = line[7:].strip()
            if not line or line.startswith("#") or "=" not in line:
                continue
            key, value = line.split("=", 1)
            key, value = key.strip(), value.strip()
            if key not in KEYS or key in env:
                continue
            if value.startswith(("'", '"')):
                if len(value) < 2 or value[-1] != value[0]:
                    raise ValueError("unclosed quoted configuration value")
                value = value[1:-1]
            else:
                value = value.split(" #", 1)[0].rstrip()
            values[key] = value
    values.update({key: env[key] for key in KEYS if key in env})
    if any("$" in v or "`" in v or any(ord(c) < 32 for c in v) for v in values.values()):
        raise ValueError("configuration must use literal values without control characters")
    for key in ("FRP_MONITOR_PORT", "FRP_SERVER_PORT", "FRP_MONITOR_INTERVAL_SECONDS", "FRP_MONITOR_RETENTION_DAYS"):
        values[key] = int(values[key])
        maximum = 31 if key.endswith("DAYS") else 3600 if key.endswith("SECONDS") else 65535
        if not 1 <= values[key] <= maximum:
            raise ValueError("port or reporting interval out of range")
    if values["FRP_MONITOR_PORT"] == values["FRP_SERVER_PORT"]:
        raise ValueError("FRP and monitoring require different ports")
    if not values["FRP_AGENT_NAME"].strip() or len(values["FRP_AGENT_NAME"].encode()) > 128:
        raise ValueError("node label must contain 1..128 UTF-8 bytes")
    if len(values["FRP_AGENT_IFACE"].encode()) > 1024:
        raise ValueError("interface filter too long")
    return values


def directory(name: str) -> Path:
    path = Path(name)
    if not path.is_absolute():
        path = ROOT / path
    allowed = ROOT / "data"
    if ".." in path.parts or allowed not in path.parents or path == allowed:
        raise ValueError("demo directory must be a child of frp-monitor/data")
    cursor = path
    while cursor != ROOT:
        if cursor.is_symlink():
            raise ValueError("demo paths must not contain symlinks")
        cursor = cursor.parent
    return path


def private(path: Path, text: str):
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "w", encoding="utf-8", newline="\n") as stream:
        stream.write(text)


def initialize(destination: Path, *, plain_http=False, probes=False, config=None):
    c = settings() if config is None else config
    if destination.exists():
        raise ValueError("demo directory exists; use it or select another --directory")
    destination.parent.mkdir(parents=True, exist_ok=True)
    staging = Path(tempfile.mkdtemp(prefix=".init-", dir=destination.parent))
    staging.chmod(0o700)
    try:
        node_id = uuid.uuid4().hex
        token = base64.urlsafe_b64encode(secrets.token_bytes(32)).decode().rstrip("=")
        private(staging / "agent.token", token + "\n")
        private(staging / "frp.token", secrets.token_hex(32) + "\n")
        private(staging / "credentials.json", json.dumps([{"agent_id": node_id, "name": c["FRP_AGENT_NAME"],
                "token_sha256": hashlib.sha256(token.encode()).hexdigest()}], ensure_ascii=False, indent=2) + "\n")
        tasks = [{"id": "local-frp", "name": "本机 FRP 入口", "target": f'127.0.0.1:{c["FRP_SERVER_PORT"]}', "interval": 5}] if probes else []
        private(staging / "probes.json", json.dumps({"version": 1, "nodes": [{"agent_id": node_id, "tasks": tasks}]}, ensure_ascii=False, indent=2) + "\n")
        # Paths in generated configuration refer to the final private directory.
        q = lambda name: json.dumps(str(destination / name), ensure_ascii=False)
        tls_monitor = tls_agent = ""
        if not plain_http:
            try:
                result = subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "30",
                    "-subj", "/CN=frp-monitor-local", "-addext", "subjectAltName=IP:127.0.0.1",
                    "-keyout", str(staging / "local.key"), "-out", str(staging / "local.crt")], capture_output=True, timeout=30)
            except subprocess.TimeoutExpired as exc:
                raise ValueError("local TLS certificate generation timed out") from exc
            if result.returncode:
                raise ValueError("local TLS certificate generation failed (OpenSSL with -addext required)")
            (staging / "local.key").chmod(0o600)
            (staging / "local.crt").chmod(0o600)
            tls_monitor = f"certFile = {q('local.crt')}\nkeyFile = {q('local.key')}\n"
            tls_agent = f"caFile = {q('local.crt')}\n"
        auth = f'auth.method = "token"\nauth.tokenSource.type = "file"\nauth.tokenSource.file.path = {q("frp.token")}\n'
        private(staging / "server.toml", f'bindAddr = "127.0.0.1"\nbindPort = {c["FRP_SERVER_PORT"]}\nproxyBindAddr = "127.0.0.1"\n'
            + auth + f'\n[monitor]\nenabled = true\nbindAddr = "127.0.0.1"\nbindPort = {c["FRP_MONITOR_PORT"]}\nserverID = "local"\n'
            + f'credentialsFile = {q("credentials.json")}\nreportIntervalSeconds = {c["FRP_MONITOR_INTERVAL_SECONDS"]}\n' + tls_monitor)
        with (staging / "server.toml").open("a", encoding="utf-8") as server_config:
            server_config.write(f'databaseFile = {q("history.sqlite")}\nretentionDays = {c["FRP_MONITOR_RETENTION_DAYS"]}\nprobeTasksFile = {q("probes.json")}\n')
        scheme = "ws" if plain_http else "wss"
        private(staging / "agent.toml", f'serverAddr = "127.0.0.1"\nserverPort = {c["FRP_SERVER_PORT"]}\nclientID = "{node_id}"\nloginFailExit = false\n'
            + auth + f'\n[telemetry]\nenabled = true\nserverID = "local"\nendpoint = "{scheme}://127.0.0.1:{c["FRP_MONITOR_PORT"]}/agent/v1/ws"\n'
            + f'tokenFile = {q("agent.token")}\nintervalSeconds = {c["FRP_MONITOR_INTERVAL_SECONDS"]}\niface = {json.dumps(c["FRP_AGENT_IFACE"])}\n'
            + f'allowInsecureLoopback = {str(plain_http).lower()}\nprobeEnabled = {str(probes).lower()}\nprobeAllowPrivate = {str(probes).lower()}\n' + tls_agent)
        private(staging / "local.json", json.dumps({"url": f'{"http" if plain_http else "https"}://127.0.0.1:{c["FRP_MONITOR_PORT"]}/', "id": node_id}) + "\n")
        os.rename(staging, destination)
    finally:
        # Remove only files created by this failed initialization, never an existing installation.
        if staging.exists():
            for entry in staging.iterdir():
                entry.unlink()
            staging.rmdir()
    return destination


def native_binaries():
    system = platform.system().lower()
    machine = {"aarch64": "arm64", "arm64": "arm64", "x86_64": "amd64", "amd64": "amd64"}.get(platform.machine().lower())
    if system not in ("linux", "darwin") or not machine:
        raise ValueError("local demo supports macOS/Linux amd64/arm64")
    folder = ROOT / "dist" / f"{system}-{machine}"
    return folder / "frp-monitor-server", folder / "frp-monitor-agent"


def run_demo(folder: Path):
    binaries = native_binaries()
    environment = {k: v for k, v in os.environ.items() if k in ("PATH", "HOME", "TMPDIR")}
    for binary, config in zip(binaries, ("server.toml", "agent.toml")):
        if not binary.is_file() or not os.access(binary, os.X_OK):
            raise ValueError("native binaries missing; run scripts/frp.py build --native")
        check = subprocess.run([str(binary), "verify", "-c", str(folder / config)], env=environment, capture_output=True)
        if check.returncode:
            raise ValueError("generated FRP configuration verification failed")
    state = json.loads((folder / "local.json").read_text())
    print(f'Local node page: {state["url"]}', flush=True)
    print("Press Ctrl-C to stop both processes. Linux metrics are unsupported on macOS.", flush=True)
    processes = []
    logs = []
    try:
        for binary, name in zip(binaries, ("server", "agent")):
            log = os.fdopen(os.open(folder / f"{name}.log", os.O_WRONLY | os.O_CREAT | os.O_APPEND | os.O_NOFOLLOW, 0o600), "ab")
            os.fchmod(log.fileno(), 0o600)
            logs.append(log)
            processes.append(subprocess.Popen([str(binary), "-c", str(folder / f"{name}.toml")], env=environment,
                stdout=log, stderr=subprocess.STDOUT, start_new_session=True))
        while all(p.poll() is None for p in processes):
            time.sleep(0.2)
        raise ValueError("a local process exited; inspect private data directory logs")
    finally:
        for process in processes:
            if process.poll() is None:
                os.killpg(process.pid, signal.SIGTERM)
        for process in processes:
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                os.killpg(process.pid, signal.SIGKILL)
                process.wait(timeout=5)
        for log in logs:
            log.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("init", "run"))
    parser.add_argument("--directory", default="data/local")
    parser.add_argument("--http", action="store_true", help="init only: explicit plaintext loopback demonstration")
    parser.add_argument("--probes", action="store_true", help="init only: enable a TCP probe to this demonstration's loopback FRP port")
    args = parser.parse_args()
    path = directory(args.directory)
    if args.action == "init":
        initialize(path, plain_http=args.http, probes=args.probes)
        print(f"Initialized private local configuration: {path}")
    else:
        if args.http or args.probes:
            parser.error("--http and --probes apply only to init")
        run_demo(path)


if __name__ == "__main__":
    def stop(*_):
        raise KeyboardInterrupt

    signal.signal(signal.SIGTERM, stop)
    try:
        main()
    except KeyboardInterrupt:
        pass
    except (OSError, ValueError) as exc:
        print(f"frp-monitor local: {exc}", file=sys.stderr)
        raise SystemExit(1)
