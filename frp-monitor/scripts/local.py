#!/usr/bin/env python3
"""Initialize and run a loopback demonstration with private history and administration."""
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
import sqlite3
import stat
from contextlib import closing
from urllib.parse import urlsplit

ROOT = Path(__file__).resolve().parents[1]
KEYS = {"FRP_MONITOR_PORT", "FRP_SERVER_PORT", "FRP_MONITOR_INTERVAL_SECONDS", "FRP_MONITOR_RETENTION_DAYS", "FRP_AGENT_NAME", "FRP_AGENT_IFACE", "FRP_MONITOR_HISTORY_DATA_PATH", "FRP_GITHUB_CLIENT_ID", "FRP_GITHUB_CLIENT_SECRET_FILE", "FRP_GITHUB_CALLBACK_URL", "FRP_GITHUB_ADMIN_USERS"}


def settings(root=ROOT, environment=None):
    env = os.environ if environment is None else environment
    values = {"FRP_MONITOR_PORT": "17401", "FRP_SERVER_PORT": "17000", "FRP_MONITOR_INTERVAL_SECONDS": "1",
              "FRP_MONITOR_RETENTION_DAYS": "7", "FRP_AGENT_NAME": "本地演示节点", "FRP_AGENT_IFACE": "", "FRP_MONITOR_HISTORY_DATA_PATH": "",
              "FRP_GITHUB_CLIENT_ID": "", "FRP_GITHUB_CLIENT_SECRET_FILE": "", "FRP_GITHUB_CALLBACK_URL": "", "FRP_GITHUB_ADMIN_USERS": ""}
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
        maximum = 365 if key.endswith("DAYS") else 3600 if key.endswith("SECONDS") else 65535
        if not 1 <= values[key] <= maximum:
            raise ValueError("port or reporting interval out of range")
    if values["FRP_MONITOR_PORT"] == values["FRP_SERVER_PORT"]:
        raise ValueError("FRP and monitoring require different ports")
    if not values["FRP_AGENT_NAME"].strip() or len(values["FRP_AGENT_NAME"].encode()) > 128:
        raise ValueError("node label must contain 1..128 UTF-8 bytes")
    if len(values["FRP_AGENT_IFACE"].encode()) > 1024:
        raise ValueError("interface filter too long")
    history_path = values["FRP_MONITOR_HISTORY_DATA_PATH"].strip()
    if history_path and (".." in Path(history_path).parts or history_path == "."):
        raise ValueError("history directory must not contain parent traversal")
    values["FRP_MONITOR_HISTORY_DATA_PATH"] = history_path
    users = [login.strip() for login in values["FRP_GITHUB_ADMIN_USERS"].split(",") if login.strip()]
    import re
    if any(not re.fullmatch(r"[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?", login) for login in users):
        raise ValueError("GitHub administrators must be account login names")
    values["FRP_GITHUB_ADMIN_USERS"] = list(dict.fromkeys(login.lower() for login in users))
    oauth = [values[key] for key in ("FRP_GITHUB_CLIENT_ID", "FRP_GITHUB_CLIENT_SECRET_FILE", "FRP_GITHUB_CALLBACK_URL", "FRP_GITHUB_ADMIN_USERS")]
    if any(oauth) and not all(oauth):
        raise ValueError("GitHub login requires client ID, secret file, callback URL and administrator users together")
    if values["FRP_GITHUB_CALLBACK_URL"]:
        callback = urlsplit(values["FRP_GITHUB_CALLBACK_URL"])
        if callback.scheme != "https" or not callback.hostname or callback.username or callback.password or callback.query or callback.fragment or callback.path != "/api/admin/v1/auth/github/callback":
            raise ValueError("GitHub callback must be an HTTPS URL ending in /api/admin/v1/auth/github/callback")
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


def _history_directory(value, destination, *, check_files=True):
    """Resolve optional history inside the private runtime, without creating it."""
    if not value:
        return ""
    requested = Path(value)
    if ".." in requested.parts:
        raise ValueError("history directory must not contain parent traversal")
    path = requested if requested.is_absolute() else destination / requested
    if path == destination or destination not in path.parents:
        raise ValueError("history directory must be inside the private runtime directory")
    relative = path.relative_to(destination)
    reserved = {"server.toml", "agent.toml", "control.sqlite", "control.sqlite-wal", "control.sqlite-shm",
                "installation.json", "local.json", "github.secret", "agent.token", "frp.token",
                "tls.crt", "tls.key", "local.crt", "local.key", "ca.crt"}
    if relative.parts[0] in reserved:
        raise ValueError("history directory conflicts with a managed runtime file")
    current = path
    while check_files and current != destination:
        if current.is_symlink() or (current.exists() and not current.is_dir()):
            raise ValueError("history directory must not contain links or regular files")
        if current.exists() and stat.S_IMODE(current.stat().st_mode) != 0o700:
            raise ValueError("history directory must be private (0700)")
        current = current.parent
    return str(path)


def control_database(path: Path, *, node_name=None, token=None, server_id="local", tasks=None):
    """Create the current schema, with an optional first node and probe document."""
    private(path, "")
    with closing(sqlite3.connect(path)) as database:
        database.executescript((ROOT / "monitor/control/schema.sql").read_text(encoding="utf-8"))
        node_id = None
        with database:
            if node_name is not None:
                if not token:
                    raise ValueError("node token is required")
                now = time.time_ns() // 1_000_000
                cursor = database.execute("INSERT INTO nodes(name,token_sha256,created_at_ms,updated_at_ms) VALUES(?,?,?,?)", (node_name, hashlib.sha256(token.encode()).hexdigest(), now, now))
                node_id = str(cursor.lastrowid)
                binding = {"server_id": server_id, "user": "", "raw_client_id": node_id}
                database.execute("UPDATE nodes SET frp_binding=? WHERE id=?", (json.dumps(binding), int(node_id)))
                document = {"version": 1, "nodes": [{"agent_id": node_id, "tasks": tasks or []}]}
                database.execute("UPDATE settings SET probe_json=? WHERE id=1", (json.dumps(document, ensure_ascii=False),))
        if database.execute("PRAGMA integrity_check").fetchone() != ("ok",):
            raise ValueError("control database initialization failed")
    return node_id


def github_config(config, staging, destination):
    """Copy a configured OAuth secret without writing it into TOML or stdout."""
    secret_path = config["FRP_GITHUB_CLIENT_SECRET_FILE"]
    target = ""
    if secret_path:
        source = Path(secret_path).expanduser()
        if not source.is_absolute():
            source = ROOT / source
        if any(part.is_symlink() for part in (source, *source.parents)):
            raise ValueError("GitHub secret path must not contain symlinks")
        fd = os.open(source, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
        with os.fdopen(fd, "rb") as stream:
            info = os.fstat(stream.fileno())
            if not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode) != 0o600 or info.st_size > 4096:
                raise ValueError("GitHub secret must be a regular 0600 file")
            secret = stream.read(4097).decode("utf-8").strip()
        if not secret or len(secret) > 4096 or any(ord(c) < 33 for c in secret):
            raise ValueError("GitHub secret file contains an invalid value")
        private(staging / "github.secret", secret + "\n")
        target = str(destination / "github.secret")
    return (f'githubClientID = {json.dumps(config["FRP_GITHUB_CLIENT_ID"])}\n'
            f'githubClientSecretFile = {json.dumps(target)}\n'
            f'githubCallbackURL = {json.dumps(config["FRP_GITHUB_CALLBACK_URL"])}\n'
            f'githubAdminUsers = {json.dumps(config["FRP_GITHUB_ADMIN_USERS"])}\n')


def initialize(destination: Path, *, plain_http=False, probes=False, config=None):
    c = settings() if config is None else config
    if destination.exists():
        raise ValueError("demo directory exists; use it or select another --directory")
    destination.parent.mkdir(parents=True, exist_ok=True)
    staging = Path(tempfile.mkdtemp(prefix=".init-", dir=destination.parent))
    staging.chmod(0o700)
    try:
        token = base64.urlsafe_b64encode(secrets.token_bytes(32)).decode().rstrip("=")
        private(staging / "agent.token", token + "\n")
        private(staging / "frp.token", secrets.token_hex(32) + "\n")
        tasks = [{"id": "local-frp", "name": "本机 FRP 入口", "target": f'127.0.0.1:{c["FRP_SERVER_PORT"]}', "interval": 5}] if probes else []
        node_id = control_database(staging / "control.sqlite", node_name=c["FRP_AGENT_NAME"], token=token, tasks=tasks)
        oauth = github_config(c, staging, destination)
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
            + f'databaseFile = {q("control.sqlite")}\nreportIntervalSeconds = {c["FRP_MONITOR_INTERVAL_SECONDS"]}\n' + tls_monitor + oauth)
        with (staging / "server.toml").open("a", encoding="utf-8") as server_config:
            server_config.write(f'historyDataPath = {json.dumps(_history_directory(c["FRP_MONITOR_HISTORY_DATA_PATH"], destination))}\nretentionDays = {c["FRP_MONITOR_RETENTION_DAYS"]}\n')
        scheme = "ws" if plain_http else "wss"
        private(staging / "agent.toml", f'serverAddr = "127.0.0.1"\nserverPort = {c["FRP_SERVER_PORT"]}\nclientID = "{node_id}"\nloginFailExit = false\n'
            + auth + f'\n[telemetry]\nenabled = true\nserverID = "local"\nendpoint = "{scheme}://127.0.0.1:{c["FRP_MONITOR_PORT"]}/agent/v1/ws"\n'
            + f'tokenFile = {q("agent.token")}\nintervalSeconds = {c["FRP_MONITOR_INTERVAL_SECONDS"]}\niface = {json.dumps(c["FRP_AGENT_IFACE"])}\n'
            + f'allowInsecureLoopback = {str(plain_http).lower()}\nprobeEnabled = {str(probes).lower()}\nprobeAllowPrivate = {str(probes).lower()}\n' + tls_agent)
        private(staging / "local.json", json.dumps({"url": f'{"http" if plain_http else "https"}://127.0.0.1:{c["FRP_MONITOR_PORT"]}/', "id": node_id}) + "\n")
        private(staging / "installation.json", json.dumps({"format": 2, "roles": ["server", "agent"]}) + "\n")
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
    print(f'Administration: {state["url"]}admin/ (GitHub login; configure an OAuth app and administrator account allowlist)', flush=True)
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
    except (OSError, ValueError, sqlite3.Error) as exc:
        print(f"frp-monitor local initialization failed ({type(exc).__name__}); check configuration and private file permissions.", file=sys.stderr)
        raise SystemExit(1)
