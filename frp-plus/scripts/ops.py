#!/usr/bin/env python3
"""Private configuration, consistent SQLite backups, restore and systemd unit rendering."""
from __future__ import annotations

import argparse
from contextlib import closing, contextmanager
import gzip
import hashlib
import io
import ipaddress
import json
import os
from pathlib import Path
import re
import selectors
import secrets
import shutil
import sqlite3
import stat
import subprocess
import sys
import tarfile
import tempfile
import time
import tomllib
from urllib.parse import urlsplit

from local import (private, read_private, settings, control_database, github_config,
                   _history_directory, toml_value, render_auth, render_monitor, render_telemetry,
                   initialize_managed_store, installation_metadata, native_binaries)

FILES = frozenset(('server.toml', 'agent.toml', 'github.secret', 'frp.token', 'agent.token',
                   'local.crt', 'local.key', 'tls.crt', 'tls.key', 'ca.crt', 'local.json',
                   'installation.json', 'control.sqlite'))
MAX_FILE = 1024 * 1024 * 1024
MAX_TOTAL = 2 * MAX_FILE
MANAGED_MAX_FILES = 8192
MANAGED_MAX_BYTES = 64 * 1024 * 1024
MANAGED_MAX_FILE = 1024 * 1024
MAINTENANCE_TIMEOUT = 60
MANAGED_CONTEXT = frozenset(('installation.json', 'agent.toml', 'agent.token', 'frp.token',
                             'ca.crt', 'tls.crt', 'tls.key', 'local.crt', 'local.key'))
HEX_DIGEST = re.compile(r'[0-9a-f]{64}')
UUID = re.compile(r'[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}')
PATH_KEYS = frozenset(('path', 'tokenFile', 'caFile', 'certFile', 'keyFile',
                       'githubClientSecretFile', 'databaseFile', 'historyDataPath'))


class BackupTarInfo(tarfile.TarInfo):
    @classmethod
    def fromtarfile(cls, archive):
        # Python versions differ in whether TarInfo's internal parser calls
        # the public frombuf override. Validate the raw header explicitly.
        member = cls.frombuf(archive.fileobj.read(tarfile.BLOCKSIZE), archive.encoding, archive.errors)
        member.offset = archive.fileobj.tell() - tarfile.BLOCKSIZE
        return member._proc_member(archive)

    @classmethod
    def frombuf(cls, buffer, encoding, errors):
        member = super().frombuf(buffer, encoding, errors)
        # Fixed ASCII backup names fit USTAR. Reject extension headers before
        # tarfile reads their attacker-sized PAX/GNU metadata into memory.
        if member.type not in (tarfile.REGTYPE, tarfile.AREGTYPE):
            raise ValueError('backup permits only regular USTAR members')
        return member


class BackupReader:
    def __init__(self, source, maximum):
        self.source, self.remaining = source, maximum
        self.deadline = time.monotonic() + 30

    def read(self, size=-1):
        if time.monotonic() >= self.deadline:
            raise ValueError('backup decompression exceeded its time budget')
        size = 65536 if size < 0 else min(size, 65536)
        data = self.source.read(min(size, self.remaining + 1))
        self.remaining -= len(data)
        if self.remaining < 0:
            raise ValueError('backup decompression exceeded its size budget')
        return data


@contextmanager
def read_archive(fd, maximum):
    os.lseek(fd, 0, os.SEEK_SET)
    with os.fdopen(os.dup(fd), 'rb') as source, gzip.GzipFile(fileobj=source, mode='rb') as decoded:
        bounded = BackupReader(decoded, maximum + (MANAGED_MAX_FILES + 2) * 1024)
        with tarfile.open(fileobj=bounded, mode='r|', tarinfo=BackupTarInfo) as archive:
            yield archive


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
        private(stage / 'installation.json', installation_metadata(['server']))
    return new_directory(args.directory, write)


def agent_init(args):
    config = settings()
    manage_config = getattr(args, 'manage_config', None)
    if manage_config is None:
        manage_config = config.get('FRP_CONFIG_MANAGEMENT_ENABLED', False)
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
        managed_store = initialize_managed_store(stage, final, manage_config)
        private(stage / 'agent.toml', f'serverAddr = {toml_value(args.server_addr)}\nserverPort = {config["FRP_SERVER_PORT"]}\n'
                f'clientID = {toml_value(args.client_id)}\nuser = {toml_value(args.user)}\nloginFailExit = false\n'
                + managed_store + render_auth(final) + render_telemetry(config, final, endpoint=args.monitor_url,
                    server_id=args.server_id, ca_file="ca.crt" if ca else "", probes=args.probes,
                    allow_private_probes=args.allow_private_probes, manage_config=manage_config))
        if ca:
            private(stage / 'ca.crt', ca.decode('utf-8'))
        private(stage / 'installation.json', installation_metadata(['agent'], manage_config))
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



def strict_json(data):
    def unique(pairs):
        result = {}
        for key, value in pairs:
            if key in result:
                raise ValueError('duplicate JSON field')
            result[key] = value
        return result
    def invalid_constant(_):
        raise ValueError('non-JSON numeric value')
    try:
        result = json.loads(data, object_pairs_hook=unique, parse_constant=invalid_constant)
    except RecursionError as exc:
        raise ValueError('JSON nesting exceeds the limit') from exc
    pending = [(result, 0)]
    while pending:
        value, depth = pending.pop()
        if depth > 16:
            raise ValueError('JSON nesting exceeds the limit')
        if isinstance(value, dict):
            pending.extend((item, depth + 1) for item in value.values())
        elif isinstance(value, list):
            pending.extend((item, depth + 1) for item in value)
    return result


def managed_profile(folder):
    metadata = strict_json(read_private(folder / 'installation.json'))
    if not isinstance(metadata, dict):
        raise ValueError('invalid installation metadata')
    declared = metadata.get('managed')
    if declared is None:
        return False
    if (metadata.get('format') != 2 or metadata.get('roles') != ['agent']
            or declared != {'version': 1, 'root': 'managed', 'store': 'store.json'}):
        raise ValueError('managed backup supports only generated single-Agent installations')
    return True


def maintenance(arguments, folder, binary=None):
    """Invoke the native lease holder, bounding and suppressing private diagnostics."""
    executable = path_without_links(binary or native_binaries()[1])
    info = executable.stat()
    if not stat.S_ISREG(info.st_mode) or not os.access(executable, os.X_OK) or info.st_mode & 0o022:
        raise ValueError('managed maintenance requires a trusted native Agent binary')
    command = [str(executable), 'managed-maintenance', *arguments, '--offline']
    process = subprocess.Popen(command, cwd=folder, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    output, size = bytearray(), 0
    deadline = time.monotonic() + MAINTENANCE_TIMEOUT
    try:
        with selectors.DefaultSelector() as selector:
            selector.register(process.stdout, selectors.EVENT_READ, True)
            selector.register(process.stderr, selectors.EVENT_READ, False)
            while selector.get_map():
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise ValueError('managed maintenance exceeded its time budget')
                for key, _ in selector.select(min(remaining, 0.2)):
                    chunk = os.read(key.fileobj.fileno(), 4096)
                    if not chunk:
                        selector.unregister(key.fileobj)
                        continue
                    size += len(chunk)
                    if size > 65536:
                        raise ValueError('managed maintenance exceeded its output budget')
                    if key.data:
                        output.extend(chunk)
            process.wait(timeout=max(0.001, deadline - time.monotonic()))
        if process.returncode != 0 or len(output) > 8192:
            raise ValueError('managed maintenance failed; runtime remains gated when recovery is incomplete')
        result = strict_json(output)
        if not isinstance(result, dict) or result.get('code') != 'ok':
            raise ValueError('managed maintenance did not confirm success')
        return result
    except (subprocess.TimeoutExpired, json.JSONDecodeError) as exc:
        raise ValueError('managed maintenance did not complete safely') from exc
    finally:
        if process.poll() is None:
            process.kill()
            process.wait(timeout=5)
        process.stdout.close()
        process.stderr.close()


def checkpoint_path(name):
    if not isinstance(name, str):
        return False
    parts = name.split('/')
    if len(parts) == 2 and parts[0] == 'context':
        return parts[1] in MANAGED_CONTEXT
    if len(parts) == 2 and parts[0] == 'managed':
        return parts[1] in ('store.json', 'identity.json', 'restore.json')
    if len(parts) == 3 and parts[:2] == ['managed', 'operations']:
        return re.fullmatch(r'[0-9a-f]{64}\.(json|old|new)', parts[2]) is not None
    if len(parts) == 3 and parts[:2] == ['managed', 'secrets']:
        return parts[2].endswith('.secret') and UUID.fullmatch(parts[2][:-7]) is not None
    return False


def checkpoint_files(directory, expected_directory=None):
    """Validate a closed private checkpoint before packaging or invoking restore."""
    private_directory(directory)
    if directory.stat().st_uid != os.geteuid():
        raise ValueError('managed checkpoint owner does not match the maintenance user')
    raw = read_private(directory / 'CHECKPOINT.json', MANAGED_MAX_FILE)
    manifest_info = (directory / 'CHECKPOINT.json').stat()
    if manifest_info.st_nlink != 1 or manifest_info.st_uid != os.geteuid():
        raise ValueError('managed checkpoint manifest is not private')
    manifest = strict_json(raw)
    if not isinstance(manifest, dict):
        raise ValueError('invalid managed checkpoint')
    original = manifest.get('config_file')
    if (manifest.get('version') != 1 or manifest.get('kind') != 'frp-managed-checkpoint'
            or not isinstance(original, str) or not Path(original).is_absolute()
            or Path(original).name != 'agent.toml' or str(Path(original)) != original):
        raise ValueError('invalid managed checkpoint identity')
    folder = Path(original).parent
    if (manifest.get('root') != str(folder / 'managed') or manifest.get('cwd') != str(folder)
            or manifest.get('store_name') != 'store.json'
            or (expected_directory is not None and folder != expected_directory)
            or not isinstance(manifest.get('files'), list)
            or not 1 <= len(manifest['files']) <= MANAGED_MAX_FILES):
        raise ValueError('managed checkpoint has an unsupported layout')
    for field in ('id', 'service_id'):
        if not isinstance(manifest.get(field), str) or UUID.fullmatch(manifest[field]) is None:
            raise ValueError('invalid managed checkpoint identity')
    for field in ('context_revision', 'store_digest'):
        if not isinstance(manifest.get(field), str) or HEX_DIGEST.fullmatch(manifest[field]) is None:
            raise ValueError('invalid managed checkpoint revision')
    if not isinstance(manifest.get('store_exists'), bool):
        raise ValueError('invalid managed checkpoint Store state')
    payload, total = {'CHECKPOINT.json': raw}, 0
    for entry in manifest['files']:
        if (not isinstance(entry, dict) or not checkpoint_path(entry.get('path'))
                or entry['path'] in payload or type(entry.get('size')) is not int
                or not 0 <= entry['size'] <= MANAGED_MAX_FILE
                or type(entry.get('mtime_ns')) is not int or not 0 < entry['mtime_ns'] < 2**63
                or not isinstance(entry.get('sha256'), str) or HEX_DIGEST.fullmatch(entry['sha256']) is None):
            raise ValueError('invalid managed checkpoint file inventory')
        total += entry['size']
        if total > MANAGED_MAX_BYTES:
            raise ValueError('managed checkpoint exceeds size budget')
        path = directory / entry['path']
        data = read_private(path, MANAGED_MAX_FILE)
        info = path.stat()
        if info.st_nlink != 1 or info.st_uid != os.geteuid() or len(data) != entry['size'] or hashlib.sha256(data).hexdigest() != entry['sha256']:
            raise ValueError('managed checkpoint file verification failed')
        payload[entry['path']] = data
    actual = set()
    for root, dirs, files in os.walk(directory, followlinks=False):
        for name in dirs:
            subdirectory = Path(root) / name
            relative = subdirectory.relative_to(directory).as_posix()
            if relative not in ('context', 'managed', 'managed/operations', 'managed/secrets'):
                raise ValueError('unexpected managed checkpoint directory')
            private_directory(subdirectory)
        for name in files:
            actual.add((Path(root) / name).relative_to(directory).as_posix())
            if len(actual) > MANAGED_MAX_FILES + 1:
                raise ValueError('managed checkpoint exceeds file budget')
    if actual != set(payload) or not {'context/installation.json', 'context/agent.toml', 'managed/identity.json'} <= actual:
        raise ValueError('managed checkpoint is incomplete')
    metadata = strict_json(payload['context/installation.json'])
    if metadata != {'format': 2, 'roles': ['agent'], 'managed': {'version': 1, 'root': 'managed', 'store': 'store.json'}}:
        raise ValueError('unsupported managed checkpoint installation')
    if ('managed/store.json' in actual) != manifest['store_exists']:
        raise ValueError('managed checkpoint Store presence is inconsistent')
    return manifest, payload, hashlib.sha256(raw).hexdigest()


def sync_file(path):
    with path.open('rb') as source:
        os.fsync(source.fileno())


def sync_directory(path):
    fd = os.open(path, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def managed_backup(folder, output, *, offline=False, agent_binary=None):
    if not offline:
        raise ValueError('managed backup requires explicit --offline maintenance acknowledgement')
    with tempfile.TemporaryDirectory(prefix='.frp-checkpoint-', dir=output.parent) as temporary:
        temporary = Path(temporary)
        checkpoint = temporary / 'checkpoint'
        result = maintenance(['checkpoint', '--config', str(folder / 'agent.toml'), '--output', str(checkpoint)], folder, agent_binary)
        manifest, payload, digest = checkpoint_files(checkpoint, folder)
        if (result.get('manifest_digest') != digest or result.get('checkpoint_id') != manifest['id']
                or result.get('file_count') != len(manifest['files'])
                or result.get('total_bytes') != sum(item['size'] for item in manifest['files'])):
            raise ValueError('managed checkpoint response does not match durable material')
        archive_manifest = {'format': 3, 'kind': 'frp-managed-agent', 'original_directory': str(folder),
                            'checkpoint_sha256': digest, 'created_at': int(time.time())}
        payload = {'checkpoint/' + name: data for name, data in payload.items()}
        payload['BACKUP.json'] = json.dumps(archive_manifest, sort_keys=True).encode()
        staged = temporary / 'backup.tar.gz'
        private(staged, '')
        with tarfile.open(staged, 'w:gz', format=tarfile.USTAR_FORMAT) as archive:
            for name, data in sorted(payload.items()):
                info = tarfile.TarInfo(name)
                info.size, info.mode = len(data), 0o600
                archive.addfile(info, io.BytesIO(data))
        sync_file(staged)
        os.link(staged, output)
        sync_directory(output.parent)
    return output


def archive_format(fd):
    with read_archive(fd, MAX_TOTAL) as archive:
        total, count = 0, 0
        deadline = time.monotonic() + 30
        for member in archive:
            count, total = count + 1, total + member.size
            if count > MANAGED_MAX_FILES + 2 or total > MAX_TOTAL or time.monotonic() > deadline:
                raise ValueError('backup inventory exceeds limits')
            if (member.name not in FILES | {'BACKUP.json', 'checkpoint/CHECKPOINT.json'}
                    and not (member.name.startswith('checkpoint/') and checkpoint_path(member.name.removeprefix('checkpoint/')))):
                raise ValueError('invalid backup inventory')
            if member.size < 0 or member.size > (MAX_FILE if member.name == 'control.sqlite' else MANAGED_MAX_FILE):
                raise ValueError('backup member exceeds size limit')
            if member.name == 'BACKUP.json':
                if not member.isfile() or not 0 <= member.size <= MANAGED_MAX_FILE:
                    raise ValueError('invalid backup manifest')
                with archive.extractfile(member) as value:
                    manifest = strict_json(value.read(MANAGED_MAX_FILE + 1))
                if not isinstance(manifest, dict) or manifest.get('format') not in (2, 3):
                    raise ValueError('unsupported backup format')
                return manifest['format']
    raise ValueError('backup manifest is missing')


def managed_restore(fd, folder, *, offline=False, agent_binary=None):
    if not offline:
        raise ValueError('managed restore requires explicit --offline maintenance acknowledgement')
    folder = private_directory(folder)
    with tempfile.TemporaryDirectory(prefix='.frp-restore-', dir=folder.parent) as temporary:
        temporary = Path(temporary)
        with read_archive(fd, MANAGED_MAX_BYTES + 2 * MANAGED_MAX_FILE) as archive:
            names, total = set(), 0
            deadline = time.monotonic() + 30
            for member in archive:
                total += member.size
                relative = member.name.removeprefix('checkpoint/')
                allowed = (member.name == 'BACKUP.json' or member.name == 'checkpoint/CHECKPOINT.json'
                           or member.name.startswith('checkpoint/') and checkpoint_path(relative))
                if (not member.isfile() or not allowed or member.name in names or member.mode != 0o600
                        or not 0 <= member.size <= MANAGED_MAX_FILE or len(names) >= MANAGED_MAX_FILES + 2
                        or total > MANAGED_MAX_BYTES + 2 * MANAGED_MAX_FILE or time.monotonic() > deadline):
                    raise ValueError('invalid managed backup inventory')
                names.add(member.name)
                target = temporary / member.name
                parent = temporary
                for component in Path(member.name).parts[:-1]:
                    parent /= component
                    parent.mkdir(mode=0o700, exist_ok=True)
                with archive.extractfile(member) as value:
                    dest = os.open(target, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
                    with os.fdopen(dest, 'wb') as output:
                        shutil.copyfileobj(value, output, 64 * 1024)
        archive_manifest = strict_json(read_private(temporary / 'BACKUP.json'))
        expected_keys = {'format', 'kind', 'original_directory', 'checkpoint_sha256', 'created_at'}
        if (not isinstance(archive_manifest, dict) or set(archive_manifest) != expected_keys
                or archive_manifest.get('format') != 3 or archive_manifest.get('kind') != 'frp-managed-agent'
                or archive_manifest.get('original_directory') != str(folder)):
            raise ValueError('managed restore requires the original absolute installation directory')
        checkpoint = temporary / 'checkpoint'
        _, _, digest = checkpoint_files(checkpoint, folder)
        if digest != archive_manifest['checkpoint_sha256']:
            raise ValueError('managed checkpoint manifest checksum mismatch')
        result = maintenance(['restore-install', '--checkpoint', str(checkpoint), '--root', str(folder / 'managed'),
                              '--manifest-digest', digest], folder, agent_binary)
        return restore_result(result, 'pending', digest)


def restore_result(result, state, digest):
    if (result.get('state') != state or result.get('manifest_digest') != digest
            or not isinstance(result.get('epoch'), str) or UUID.fullmatch(result['epoch']) is None
            or not isinstance(result.get('service_id'), str) or UUID.fullmatch(result['service_id']) is None):
        raise ValueError('managed restore did not confirm the required gate state')
    return {key: result[key] for key in ('code', 'epoch', 'service_id', 'manifest_digest', 'state')}


def restore_confirm(directory, epoch, manifest_digest, *, offline=False, agent_binary=None):
    if not offline:
        raise ValueError('managed restore confirmation requires explicit --offline acknowledgement')
    folder = private_directory(directory)
    if not isinstance(epoch, str) or UUID.fullmatch(epoch) is None or not isinstance(manifest_digest, str) or HEX_DIGEST.fullmatch(manifest_digest) is None:
        raise ValueError('invalid managed restore confirmation identity')
    result = maintenance(['restore-confirm', '--config', str(folder / 'agent.toml'), '--epoch', epoch,
                          '--manifest-digest', manifest_digest], folder, agent_binary)
    return restore_result(result, 'confirmed', manifest_digest)

def backup(directory, output, *, offline=False, agent_binary=None):
    folder = private_directory(directory)
    output = path_without_links(output)
    private_directory(output.parent)
    if output.exists() or output == folder or folder in output.parents:
        raise ValueError('backup output must be new and outside the runtime directory')
    if managed_profile(folder):
        return managed_backup(folder, output, offline=offline, agent_binary=agent_binary)
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
        with tarfile.open(staged, 'w:gz', format=tarfile.USTAR_FORMAT) as archive:
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


def restore(archive_path, directory, *, offline=False, agent_binary=None):
    archive_path = path_without_links(archive_path)
    fd = os.open(archive_path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    info = os.fstat(fd)
    if not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode) != 0o600:
        os.close(fd)
        raise ValueError('backup must be a regular 0600 file')
    def write(stage, final):
        with read_archive(fd, MAX_TOTAL) as archive:
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
                if connection.execute('PRAGMA application_id').fetchone() != (1179798836,) or connection.execute('PRAGMA user_version').fetchone()[0] not in (4, 5, 6, 7, 8, 9, 10):
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
        if archive_format(fd) == 3:
            return managed_restore(fd, directory, offline=offline, agent_binary=agent_binary)
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
    agent.add_argument('--manage-config', action=argparse.BooleanOptionalAction, default=None,
                       help='opt in to managed Store configuration (default: .env or disabled)')
    save = commands.add_parser('backup')
    save.add_argument('--directory', required=True)
    save.add_argument('--output', required=True)
    save.add_argument('--offline', action='store_true', help='acknowledge stopped Agent maintenance; native lease still enforced')
    save.add_argument('--agent-binary', help='native Agent with managed-maintenance support')
    load = commands.add_parser('restore')
    load.add_argument('--archive', required=True)
    load.add_argument('--directory', required=True)
    load.add_argument('--offline', action='store_true', help='required for managed restore to the original directory')
    load.add_argument('--agent-binary', help='native Agent with managed-maintenance support')
    confirm = commands.add_parser('restore-confirm')
    confirm.add_argument('--directory', required=True)
    confirm.add_argument('--epoch', required=True)
    confirm.add_argument('--manifest-digest', required=True)
    confirm.add_argument('--offline', action='store_true', help='required after runtime verification and administrator takeover')
    confirm.add_argument('--agent-binary', help='native Agent with managed-maintenance support')
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
                      else backup(args.directory, args.output, offline=args.offline, agent_binary=args.agent_binary) if args.action == 'backup'
                      else restore(args.archive, args.directory, offline=args.offline, agent_binary=args.agent_binary) if args.action == 'restore'
                      else restore_confirm(args.directory, args.epoch, args.manifest_digest,
                                           offline=args.offline, agent_binary=args.agent_binary))
            if isinstance(result, dict):
                print(json.dumps(result, sort_keys=True))
            else:
                print(f'Created private {args.action} output.')
    except (OSError, ValueError, KeyError, TypeError, sqlite3.Error, tarfile.TarError) as exc:
        # Never echo paths/configuration details from parser/SQLite errors containing credentials.
        print(f'frp-plus operations failed ({type(exc).__name__}); check input format, permissions and destination.', file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
