#!/usr/bin/env python3
"""Private configuration, consistent SQLite backups, restore and systemd unit rendering."""
from __future__ import annotations

import argparse
from contextlib import closing
import hashlib
import io
import ipaddress
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import sqlite3
import stat
import sys
import tarfile
import tempfile
import time
import tomllib
from urllib.parse import urlsplit

from local import (private, read_private, settings, control_database, github_config,
                   _history_directory, toml_value, render_auth, render_monitor, render_telemetry)

FILES = frozenset(('server.toml', 'agent.toml', 'github.secret', 'frp.token', 'agent.token',
                   'local.crt', 'local.key', 'tls.crt', 'tls.key', 'ca.crt', 'local.json',
                   'installation.json', 'control.sqlite'))
MAX_FILE = 1024 * 1024 * 1024
MAX_TOTAL = 2 * MAX_FILE
PATH_KEYS = frozenset(('path', 'tokenFile', 'caFile', 'certFile', 'keyFile',
                       'githubClientSecretFile', 'databaseFile', 'historyDataPath'))


def path_without_links(value):
    path = Path(os.path.abspath(value))
    for part in (path, *path.parents):
        if part.is_symlink():
            raise ValueError('paths must not contain symlinks')
    return path


def private_directory(value):
    path = path_without_links(value)
    if not path.is_dir() or stat.S_IMODE(path.stat().st_mode) != 0o700:
        raise ValueError('runtime directory must exist with mode 0700')
    return path


def literal(value, name, maximum=128, empty=False):
    # Reject all whitespace (including spaces and tabs), not just control characters.
    if (not empty and not value.strip()) or len(value.encode()) > maximum or any(c.isspace() or ord(c) < 32 for c in value):
        raise ValueError('invalid ' + name)
    return toml_value(value)


def new_directory(value, write):
    destination = path_without_links(value)
    if destination.exists() or not destination.parent.is_dir():
        raise ValueError('destination must be new, with an existing parent directory')
    stage = Path(tempfile.mkdtemp(prefix='.frp-init-', dir=destination.parent))
    try:
        write(stage, destination)
        if destination.exists():
            raise ValueError('destination already exists')
        os.rename(stage, destination)
    finally:
        if stage.exists():
            shutil.rmtree(stage)
    return destination


def server_init(args):
    config = settings()
    literal(args.server_id, 'server ID')
    address = str(ipaddress.ip_address(args.bind))
    cert = read_private(path_without_links(args.tls_cert))
    key = read_private(path_without_links(args.tls_key))
    def write(stage, final):
        private(stage / 'frp.token', secrets.token_hex(32) + '\n')
        control_database(stage / 'control.sqlite')
        oauth = github_config(config, stage, final)
        private(stage / 'tls.crt', cert.decode('utf-8'))
        private(stage / 'tls.key', key.decode('utf-8'))
        # Verify the exact bytes being installed, even if a certificate renewal
        # replaced the original paths after read_private returned.
        import ssl
        ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER).load_cert_chain(stage / 'tls.crt', stage / 'tls.key')
        private(stage / 'server.toml', f'bindAddr = {toml_value(address)}\nbindPort = {config["FRP_SERVER_PORT"]}\n'
                + render_auth(final) + render_monitor(config, final, bind=address, server_id=args.server_id,
                    cert_file="tls.crt", key_file="tls.key", oauth=oauth))
        private(stage / 'installation.json', '{"format":2,"roles":["server"]}\n')
    return new_directory(args.directory, write)


def agent_init(args):
    config = settings()
    literal(args.monitor_url, 'monitor URL', maximum=2048)
    endpoint = urlsplit(args.monitor_url)
    if (endpoint.scheme != 'wss' or not endpoint.hostname or endpoint.path != '/agent/v1/ws'
            or endpoint.username or endpoint.password or endpoint.query or endpoint.fragment):
        raise ValueError('monitor URL must be wss://host:port/agent/v1/ws without credentials/query/fragment')
    if endpoint.port is not None and not 1 <= endpoint.port <= 65535:
        raise ValueError('invalid monitor port')
    for value, name in ((args.server_id, 'server ID'), (args.client_id, 'client ID'), (args.server_addr, 'FRP server address')):
        literal(value, name)
    literal(args.user, 'FRP user', empty=True)
    token = read_private(path_without_links(args.agent_token)).decode('ascii').strip()
    if not re.fullmatch(r'[A-Za-z0-9_-]{43,256}', token):
        raise ValueError('agent token must be a generated high-entropy token')
    frp = read_private(path_without_links(args.frp_token)).decode('utf-8').strip()
    literal(frp, 'FRP token', maximum=4096)
    ca = read_private(path_without_links(args.ca)) if args.ca else None
    def write(stage, final):
        private(stage / 'agent.token', token + '\n')
        private(stage / 'frp.token', frp + '\n')
        private(stage / 'agent.toml', f'serverAddr = {toml_value(args.server_addr)}\nserverPort = {config["FRP_SERVER_PORT"]}\n'
                f'clientID = {toml_value(args.client_id)}\nuser = {toml_value(args.user)}\nloginFailExit = false\n'
                + render_auth(final) + render_telemetry(config, final, endpoint=args.monitor_url,
                    server_id=args.server_id, ca_file="ca.crt" if ca else "", probes=args.probes,
                    allow_private_probes=args.allow_private_probes))
        if ca:
            private(stage / 'ca.crt', ca.decode('utf-8'))
        private(stage / 'installation.json', '{"format":2,"roles":["agent"]}\n')
    if args.allow_private_probes and not args.probes:
        raise ValueError('--allow-private-probes requires --probes')
    return new_directory(args.directory, write)


def managed_files(folder, reference_directory=None):
    reference_directory = folder if reference_directory is None else reference_directory
    metadata = json.loads(read_private(folder / 'installation.json'))
    if not isinstance(metadata, dict):
        raise ValueError('invalid installation metadata')
    roles = metadata.get('roles')
    if (metadata.get('format') != 2 or not isinstance(roles, list) or not roles
            or any(role not in ('server', 'agent') for role in roles)):
        raise ValueError('requires configuration created by local.py or ops.py')
    files = {}
    for name in sorted(FILES - {'control.sqlite'}):
        path = folder / name
        if path.exists() or path.is_symlink():
            files[name] = read_private(path)
    # Reject external/include paths: a restore must not silently depend on files absent from its backup.
    def check(value):
        if isinstance(value, dict):
            for key, item in value.items():
                if key == 'includes' and item:
                    raise ValueError('backup does not support external includes')
                if key in PATH_KEYS and isinstance(item, str) and item:
                    path = Path(item)
                    if key == 'historyDataPath':
                        history = Path(_history_directory(item, reference_directory, check_files=False))
                        _history_directory(str(folder / history.relative_to(reference_directory)), folder)
                        continue
                    if path.parent != reference_directory or path.name not in FILES:
                        raise ValueError('backup requires all runtime file references inside its directory')
                    if path.name != 'control.sqlite' and path.name not in files:
                        raise ValueError('referenced private file is missing')
                check(item)
        elif isinstance(value, list):
            for item in value:
                check(item)
    for role in roles:
        check(tomllib.loads(files[role + '.toml'].decode('utf-8')))
    if 'server' in roles and not (folder / 'control.sqlite').is_file():
        raise ValueError('control database is required for server installations')
    return files


def database_snapshot(source, target):
    # WAL-aware online backup; never copy the live database/WAL files separately.
    fd = os.open(source, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    try:
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode) != 0o600:
            raise ValueError('database must be a regular 0600 file')
    finally:
        os.close(fd)
    private(target, '')
    deadline = time.monotonic() + 30
    with closing(sqlite3.connect(source.as_uri() + '?mode=ro', uri=True, timeout=2)) as reader:
        page_size = reader.execute('PRAGMA page_size').fetchone()[0]
        if reader.execute('PRAGMA page_count').fetchone()[0] * page_size > MAX_FILE:
            raise ValueError('database backup exceeds size budget')
        def progress(status, remaining, total):
            if time.monotonic() > deadline or total * page_size > MAX_FILE:
                raise ValueError('database backup exceeds time/size budget')
        with closing(sqlite3.connect(target, timeout=2)) as writer:
            reader.backup(writer, pages=128, progress=progress, sleep=0.05)
            integrity_check(writer, deadline)
            writer.execute('PRAGMA journal_mode=DELETE')
    if target.stat().st_size > MAX_FILE:
        raise ValueError('database backup exceeds size budget')


def integrity_check(connection, deadline):
    connection.set_progress_handler(lambda: int(time.monotonic() > deadline), 10000)
    try:
        if time.monotonic() > deadline or connection.execute('PRAGMA integrity_check').fetchone() != ('ok',):
            raise ValueError('database integrity check failed or exceeded its time budget')
    finally:
        connection.set_progress_handler(None, 0)


def remap_paths(text, old, final):
    """Rewrite only generated path assignments, never equal non-path strings.

    Generated files use one-line JSON-compatible quoted TOML strings. Parse each
    proposed replacement to preserve this constraint; reject unsupported manual
    path edits when managed_files subsequently validates the restored result.
    """
    names = '|'.join(sorted(PATH_KEYS))
    assignment = re.compile(r'^(\s*(?:[A-Za-z0-9_-]+\.)*(?:' + names + r')\s*=\s*)("(?:[^"\\]|\\.)*")(\s*(?:#.*)?)$')
    replacements = {str(Path(old) / name): str(final / name) for name in FILES}
    document = tomllib.loads(text)
    history = document.get('monitor', {}).get('historyDataPath', '')
    if history:
        history = Path(_history_directory(history, Path(old), check_files=False))
        replacements[str(history)] = str(final / history.relative_to(old))
    lines = []
    for line in text.splitlines(keepends=True):
        ending = '\n' if line.endswith('\n') else ''
        match = assignment.fullmatch(line.removesuffix('\n'))
        if match:
            value = tomllib.loads('value = ' + match[2])['value']
            if value in replacements:
                line = match[1] + toml_value(replacements[value]) + match[3] + ending
        lines.append(line)
    result = ''.join(lines)
    def remap(value):
        if isinstance(value, dict):
            return {key: replacements.get(item, item) if key in PATH_KEYS and isinstance(item, str)
                    else remap(item) for key, item in value.items()}
        if isinstance(value, list):
            return [remap(item) for item in value]
        return value
    # A manual multiline string can contain text resembling an assignment.
    # Never let a lexical replacement silently alter that non-path content.
    if tomllib.loads(result) != remap(tomllib.loads(text)):
        raise ValueError('unsupported manual TOML path formatting')
    return result


def backup(directory, output):
    folder = private_directory(directory)
    output = path_without_links(output)
    private_directory(output.parent)
    if output.exists() or output == folder or folder in output.parents:
        raise ValueError('backup output must be new and outside the runtime directory')
    files = managed_files(folder)
    with tempfile.TemporaryDirectory(prefix='.frp-backup-', dir=output.parent) as temporary:
        temporary = Path(temporary)
        if (folder / 'control.sqlite').exists() or (folder / 'control.sqlite').is_symlink():
            database_snapshot(folder / 'control.sqlite', temporary / 'control.sqlite')
        manifest = {'format': 2, 'original_directory': str(folder), 'created_at': int(time.time()), 'sha256': {}}
        payload = {**files}
        if (temporary / 'control.sqlite').exists():
            payload['control.sqlite'] = temporary / 'control.sqlite'
        staged = temporary / 'backup.tar.gz'
        private(staged, '')
        with tarfile.open(staged, 'w:gz') as archive:
            total = 0
            for name, value in sorted(payload.items()):
                size = value.stat().st_size if isinstance(value, Path) else len(value)
                total += size
                if total > MAX_TOTAL:
                    raise ValueError('backup exceeds size budget')
                digest = hashlib.sha256()
                source = value.open('rb') if isinstance(value, Path) else io.BytesIO(value)
                with source:
                    while chunk := source.read(1024 * 1024):
                        digest.update(chunk)
                    source.seek(0)
                    info = tarfile.TarInfo(name)
                    info.size, info.mode = size, 0o600
                    archive.addfile(info, source)
                manifest['sha256'][name] = digest.hexdigest()
            data = json.dumps(manifest, sort_keys=True).encode()
            info = tarfile.TarInfo('BACKUP.json')
            info.size, info.mode = len(data), 0o600
            archive.addfile(info, io.BytesIO(data))
        # Link only a completely written archive, never overwrite an existing backup.
        os.link(staged, output)
    return output


def restore(archive_path, directory):
    archive_path = path_without_links(archive_path)
    fd = os.open(archive_path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    info = os.fstat(fd)
    if not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode) != 0o600:
        os.close(fd)
        raise ValueError('backup must be a regular 0600 file')
    def write(stage, final):
        with os.fdopen(os.dup(fd), 'rb') as source, tarfile.open(fileobj=source, mode='r:gz') as archive:
            names, total = set(), 0
            for member in archive:
                total += member.size
                if (not member.isfile() or member.name not in FILES | {'BACKUP.json'} or member.name in names
                        or member.size > MAX_FILE or total > MAX_TOTAL):
                    raise ValueError('invalid backup inventory')
                names.add(member.name)
                if member.name != 'control.sqlite' and member.size > 1024 * 1024:
                    raise ValueError('configuration exceeds size limit')
                with archive.extractfile(member) as source:
                    dest = os.open(stage / member.name, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
                    with os.fdopen(dest, 'wb') as target:
                        shutil.copyfileobj(source, target, 1024 * 1024)
        manifest = json.loads(read_private(stage / 'BACKUP.json'))
        if not isinstance(manifest, dict):
            raise ValueError('invalid backup manifest')
        hashes = manifest.get('sha256')
        old = manifest.get('original_directory')
        if (manifest.get('format') != 2 or not isinstance(old, str) or not Path(old).is_absolute()
                or not isinstance(hashes, dict) or set(hashes) != names - {'BACKUP.json'}):
            raise ValueError('invalid backup manifest')
        for name, expected in hashes.items():
            with (stage / name).open('rb') as source:
                if hashlib.file_digest(source, 'sha256').hexdigest() != expected:
                    raise ValueError('backup checksum mismatch')
        db = stage / 'control.sqlite'
        if db.exists():
            with closing(sqlite3.connect(db.as_uri() + '?mode=ro', uri=True)) as connection:
                integrity_check(connection, time.monotonic() + 30)
                # Database identity constants; must match monitor/control/schema.sql
                # and the startup check in monitor/control/store.go.
                if connection.execute('PRAGMA application_id').fetchone() != (1179798836,) or connection.execute('PRAGMA user_version').fetchone()[0] not in (4, 5, 6):
                    raise ValueError('unsupported control database schema')
        # Generated TOML uses JSON-compatible quoted strings for file paths.
        for name in ('server.toml', 'agent.toml'):
            path = stage / name
            if path.exists():
                text = path.read_text(encoding='utf-8')
                text = remap_paths(text, old, final)
                tomllib.loads(text)
                path.write_text(text, encoding='utf-8')
        (stage / 'BACKUP.json').unlink()
        managed_files(stage, final)
    try:
        return new_directory(directory, write)
    finally:
        os.close(fd)


def systemd_unit(role, directory, binary_directory, user='frp-plus'):
    # Restrict characters to avoid systemd specifier/environment/line injection.
    for value in (directory, binary_directory):
        if not re.fullmatch(r'/[A-Za-z0-9_./-]+', value) or '..' in Path(value).parts:
            raise ValueError('systemd paths require absolute paths without whitespace or specifiers')
    if not re.fullmatch(r'[a-z_][a-z0-9_-]{0,30}', user):
        raise ValueError('invalid systemd user')
    if role not in ('server', 'agent'):
        raise ValueError('invalid service role')
    filesystem_policy = f'ProtectSystem=strict\nProtectHome=true\nReadWritePaths={directory}\n' if role == 'server' else ''
    return f'''[Unit]
Description=FRP Plus {role}
Wants=network-online.target
After=network-online.target
StartLimitIntervalSec=60
StartLimitBurst=5

[Service]
Type=simple
User={user}
Group={user}
UMask=0077
WorkingDirectory={directory}
ExecStartPre={binary_directory}/frp-plus-{role} verify -c {directory}/{role}.toml
ExecStart={binary_directory}/frp-plus-{role} -c {directory}/{role}.toml
Restart=on-failure
RestartSec=5
TimeoutStopSec=10
KillSignal=SIGTERM
NoNewPrivileges=true
{filesystem_policy}RestrictSUIDSGID=true
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX AF_NETLINK
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
'''


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest='action', required=True)
    server = commands.add_parser('server-init')
    server.add_argument('--directory', required=True)
    server.add_argument('--tls-cert', required=True)
    server.add_argument('--tls-key', required=True)
    server.add_argument('--server-id', default='default')
    server.add_argument('--bind', default='0.0.0.0')
    agent = commands.add_parser('agent-init')
    agent.add_argument('--directory', required=True)
    agent.add_argument('--server-addr', required=True)
    agent.add_argument('--monitor-url', required=True)
    agent.add_argument('--agent-token', required=True)
    agent.add_argument('--frp-token', required=True)
    agent.add_argument('--client-id', required=True)
    agent.add_argument('--server-id', default='default')
    agent.add_argument('--user', default='')
    agent.add_argument('--ca')
    agent.add_argument('--probes', action='store_true')
    agent.add_argument('--allow-private-probes', action='store_true')
    save = commands.add_parser('backup')
    save.add_argument('--directory', required=True)
    save.add_argument('--output', required=True)
    load = commands.add_parser('restore')
    load.add_argument('--archive', required=True)
    load.add_argument('--directory', required=True)
    unit = commands.add_parser('systemd')
    unit.add_argument('--role', choices=('server', 'agent'), required=True)
    unit.add_argument('--directory', default='/var/lib/frp-plus')
    unit.add_argument('--binary-directory', default='/opt/frp-plus')
    unit.add_argument('--user', default='frp-plus')
    args = parser.parse_args()
    try:
        if args.action == 'systemd':
            print(systemd_unit(args.role, args.directory, args.binary_directory, args.user), end='')
        else:
            result = (server_init(args) if args.action == 'server-init' else agent_init(args) if args.action == 'agent-init'
                      else backup(args.directory, args.output) if args.action == 'backup' else restore(args.archive, args.directory))
            print(f'Created private {args.action} output: {result}')
    except (OSError, ValueError, KeyError, TypeError, sqlite3.Error, tarfile.TarError) as exc:
        # Never echo paths/configuration details from parser/SQLite errors containing credentials.
        print(f'frp-plus operations failed ({type(exc).__name__}); check input format, permissions and destination.', file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
