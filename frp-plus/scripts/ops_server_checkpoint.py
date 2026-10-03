"""Explicit-policy server checkpoints. Archives are data, never path authority.

The native helper proves FRP dependency closure and equivalent path relocation.
This module snapshots stopped SQLite/WAL and TSDB, and owns bounded publication.
"""
from __future__ import annotations

from contextlib import closing, contextmanager
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import selectors
import sqlite3
import stat
import subprocess
import tempfile
import time
import uuid

import ops_history
from ops_checkpoint import SERVER_KIND, relative, restore_parent, validate_entries, write_archive

CONTEXT_KEYS = {'kind', 'version', 'config_file', 'cwd', 'policy_digest', 'context_revision', 'database_file',
                'history_path', 'history_enabled', 'log_path', 'template_requirements', 'files'}
POLICY_KEYS = {'version', 'config_file', 'working_dir', 'roots', 'files'}
MAX_PLAN = 8 * 1024 * 1024
ACTIVE_STATES = ('draft', 'validated', 'prepared', 'applying', 'verifying', 'outcome_unknown', 'rolling_back', 'rollback_failed')


def encoded(value): return json.dumps(value, sort_keys=True, separators=(',', ':')).encode()
def sha(data): return hashlib.sha256(data).hexdigest()
def within(path, root): return path == root or root in path.parents
def absolute(value):
    return (isinstance(value, str) and value.startswith("/") and not value.startswith("//")
            and len(value.encode()) <= 4096 and os.path.normpath(value) == value
            and not any(ord(c) < 32 for c in value))


def private_bytes(api, path, maximum):
    path = api.path_without_links(path)
    raw = api.read_private(path, maximum)
    info = path.stat()
    if info.st_uid != os.geteuid() or info.st_nlink != 1 or stat.S_IMODE(info.st_mode) != 0o600:
        raise ValueError('server checkpoint material is not private')
    return raw


def maintenance(api, arguments, folder, binary):
    if binary is None: raise ValueError('explicit server backup requires a native server binary')
    executable = api.path_without_links(binary); info = executable.stat()
    if not stat.S_ISREG(info.st_mode) or info.st_mode & 0o022 or not os.access(executable, os.X_OK):
        raise ValueError('server maintenance requires a trusted executable')
    process = subprocess.Popen([str(executable), 'server-maintenance', *arguments, '--offline'], cwd=folder,
                               stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    output = bytearray(); size = 0; deadline = time.monotonic() + api.MAINTENANCE_TIMEOUT
    try:
        with selectors.DefaultSelector() as selector:
            selector.register(process.stdout, selectors.EVENT_READ, True)
            selector.register(process.stderr, selectors.EVENT_READ, False)
            while selector.get_map():
                remaining = deadline - time.monotonic()
                if remaining <= 0: raise ValueError('server maintenance exceeded time budget')
                for key, _ in selector.select(min(.2, remaining)):
                    chunk = os.read(key.fileobj.fileno(), 4096)
                    if not chunk: selector.unregister(key.fileobj); continue
                    size += len(chunk)
                    if size > 65536: raise ValueError('server maintenance exceeded output budget')
                    if key.data: output.extend(chunk)
            process.wait(timeout=max(.001, deadline - time.monotonic()))
        if process.returncode != 0 or len(output) > 8192: raise ValueError('server maintenance failed')
        result = api.strict_json(output)
        if not isinstance(result, dict) or result.get('code') != 'ok': raise ValueError('server maintenance did not confirm success')
        return result
    except subprocess.TimeoutExpired as error:
        raise ValueError('server maintenance did not complete') from error
    finally:
        if process.poll() is None: process.kill(); process.wait(timeout=5)
        process.stdout.close(); process.stderr.close()


def arguments(policy, env_file=None, source_policy=None, source_env_file=None):
    if not policy: raise ValueError('server backup requires an explicit local policy')
    out = []
    for flag, value in (('--policy', policy), ('--env-file', env_file), ('--source-policy', source_policy), ('--source-env-file', source_env_file)):
        if value is not None:
            if not isinstance(value, (str, Path)) or not str(value): raise ValueError('local inputs must be nonempty paths')
            out.extend((flag, str(Path(value).absolute())))
    return out


def context_directory(api, folder, result=None):
    api.private_directory(folder)
    raw = private_bytes(api, folder / 'CONTEXT.json', api.MANAGED_MAX_FILE)
    context = api.strict_json(raw)
    if (not isinstance(context, dict) or set(context) != CONTEXT_KEYS or context['version'] != 1
            or context['kind'] != 'frp-server-context' or not absolute(context['cwd'])
            or not absolute(context['config_file']) or not absolute(context['database_file'])
            or type(context['history_enabled']) is not bool
            or any(context[key] and not absolute(context[key]) for key in ('history_path', 'log_path'))
            or context['history_enabled'] != bool(context['history_path'])
            or any(not isinstance(context[key], str) or not api.HEX_DIGEST.fullmatch(context[key]) for key in ('context_revision', 'policy_digest'))
            or not isinstance(context['template_requirements'], list)):
        raise ValueError('invalid native server context')
    policy = private_bytes(api, folder / 'POLICY.json', api.MANAGED_MAX_FILE)
    if sha(policy) != context['policy_digest']: raise ValueError('native server policy digest mismatch')
    values = context['files']; seen = set(); total = 0
    if not isinstance(values, list) or not 1 <= len(values) <= 128: raise ValueError('invalid native server context files')
    payload = {'CONTEXT.json': raw, 'POLICY.json': policy}
    for index, entry in enumerate(values, 1):
        if (not isinstance(entry, dict) or set(entry) != {'path', 'payload', 'size', 'sha256', 'mtime_ns'}
                or not absolute(entry['path']) or entry['path'] in seen or entry['payload'] != f'context/f{index:06d}'
                or type(entry['size']) is not int or not 0 <= entry['size'] <= api.MANAGED_MAX_FILE
                or type(entry['mtime_ns']) is not int or not 0 < entry['mtime_ns'] < 2**63
                or not isinstance(entry['sha256'], str) or not api.HEX_DIGEST.fullmatch(entry['sha256'])):
            raise ValueError('invalid native server dependency')
        data = private_bytes(api, folder / entry['payload'], api.MANAGED_MAX_FILE)
        if len(data) != entry['size'] or sha(data) != entry['sha256']: raise ValueError('native server dependency changed')
        seen.add(entry['path']); total += len(data); payload[entry['payload']] = data
        if total > api.MANAGED_MAX_BYTES: raise ValueError('native server context exceeds byte budget')
    if context['config_file'] not in seen: raise ValueError('native server context lacks its main configuration')
    actual = set()
    for top, dirs, files in os.walk(folder, followlinks=False):
        for name in dirs:
            path = Path(top) / name
            if path.relative_to(folder).as_posix() != 'context': raise ValueError('unexpected native context directory')
            api.private_directory(path)
        actual.update((Path(top) / name).relative_to(folder).as_posix() for name in files)
    if actual != set(payload): raise ValueError('native server context inventory mismatch')
    if result is not None and (result.get('version') != 1 or result.get('manifest_digest') != sha(raw)
            or result.get('context_revision') != context['context_revision'] or result.get('file_count') != len(values)
            or result.get('total_bytes') != total):
        raise ValueError('native server context reply mismatch')
    return context, payload


def capture(api, folder, output, binary, policy, env_file):
    result = maintenance(api, ['capture', '--config', str(folder / 'server.toml'), '--output', str(output),
                              *arguments(policy, env_file)], folder, binary)
    context, payload = context_directory(api, output, result)
    if context['cwd'] != str(folder) or context['config_file'] != str(folder / 'server.toml'):
        raise ValueError('server context differs from the selected installation')
    return context, payload


def metadata(api, folder):
    path = folder / 'installation.json'
    if not path.exists() and not path.is_symlink(): return None
    raw = private_bytes(api, path, api.MANAGED_MAX_FILE)
    value = api.strict_json(raw)
    if value != {'format': 2, 'roles': ['server']}: raise ValueError('explicit server checkpoints require single-server installation metadata')
    return raw, path.stat().st_mtime_ns


def database_sources(api, path):
    api.path_without_links(path)
    if Path(str(path) + '-journal').exists() or Path(str(path) + '-journal').is_symlink():
        raise ValueError('rollback-journal database requires prior clean shutdown')
    values = {}
    for suffix in ('', '-wal'):
        source = Path(str(path) + suffix)
        if source.exists() or source.is_symlink(): values[suffix] = ops_history.disk_digest(api, source)
    if '' not in values: raise ValueError('control database is missing')
    return values


def snapshot_database(api, path, output, temporary):
    original = database_sources(api, path)
    raw = temporary / 'sqlite-source'; raw.mkdir(mode=0o700)
    for suffix, digest in original.items():
        if ops_history.disk_digest(api, Path(str(path) + suffix), raw / ('control.sqlite' + suffix)) != digest:
            raise ValueError('stopped database changed during capture')
    api.database_snapshot(raw / 'control.sqlite', output); ops_history.database_valid(api, output)
    return original


@contextmanager
def history_lease(api, path):
    if path is None:
        yield None, lambda: None; return
    api.path_without_links(path)
    if not path.exists():
        def check():
            if path.exists() or path.is_symlink(): raise ValueError('history appeared during stopped capture')
        yield None, check; return
    api.private_directory(path)
    fd = os.open(path, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW); lock = None
    before = os.fstat(fd)
    try:
        ops_history._directory(before)
        lock = os.open('flock.lock', os.O_RDWR | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=fd)
        lock_before = os.fstat(lock); ops_history._file(lock_before, 1024)
        try: fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError as error: raise ValueError('history is still in use') from error
        def check():
            api.path_without_links(path)
            if not ops_history._same(before, path.stat()) or not ops_history._same(lock_before, os.stat('flock.lock', dir_fd=fd, follow_symlinks=False)):
                raise ValueError('history directory or lock changed')
        check(); yield fd, check; check()
    finally:
        if lock is not None: os.close(lock)
        os.close(fd)


def file_entry(path, name):
    info = path.stat()
    return {'path': name, 'size': info.st_size, 'mtime_ns': info.st_mtime_ns, 'sha256': sha(path.read_bytes())}


def backup(api, folder, output, *, offline, server_binary, policy, env_file=None):
    if not offline: raise ValueError('complete server capture requires explicit --offline acknowledgement')
    folder = api.private_directory(folder); output = api.path_without_links(output); api.private_directory(output.parent)
    if output.exists() or within(output, folder): raise ValueError('backup output must be new and outside the installation')
    original_metadata = metadata(api, folder)
    with tempfile.TemporaryDirectory(prefix='.frp-server-backup-', dir=output.parent) as temp:
        temporary = Path(temp); checkpoint = temporary / 'checkpoint'; checkpoint.mkdir(mode=0o700)
        context, payload = capture(api, folder, temporary / 'captured', server_binary, policy, env_file)
        history_path = Path(context['history_path']) if context['history_enabled'] else None
        if history_path is not None and within(output, history_path): raise ValueError('backup output must be outside history')
        database = Path(context['database_file'])
        with history_lease(api, history_path) as (history_fd, check_history):
            values = []
            for name, data in payload.items():
                (checkpoint / name).parent.mkdir(mode=0o700, parents=True, exist_ok=True)
                _write_file(api, checkpoint / name, data)
                entry = file_entry(checkpoint / name, name)
                source = next((v for v in context['files'] if v['payload'] == name), None)
                if source is not None: entry['mtime_ns'] = source['mtime_ns']
                values.append(entry)
            if original_metadata is not None:
                (checkpoint / 'aux').mkdir(mode=0o700); api.private(checkpoint / 'aux/installation.json', original_metadata[0].decode())
                entry = file_entry(checkpoint / 'aux/installation.json', 'aux/installation.json'); entry['mtime_ns'] = original_metadata[1]; values.append(entry)
            (checkpoint / 'control').mkdir(mode=0o700); db = checkpoint / 'control/control.sqlite'
            original_db = snapshot_database(api, database, db, temporary)
            info = db.stat(); values.append({'path': 'control/control.sqlite', 'size': info.st_size, 'mtime_ns': info.st_mtime_ns, 'sha256': ops_history.disk_digest(api, db)[-1]})
            (checkpoint / 'history').mkdir(mode=0o700)
            history_files, directories = ops_history.tree(api, history_fd, checkpoint / 'history') if history_fd is not None else ([], [])
            values += history_files
            manifest = {'version': 2, 'kind': 'frp-offline-server-checkpoint', 'checkpoint_id': str(uuid.uuid4()),
                'original_directory': str(folder), 'created_at_ms': time.time_ns() // 1_000_000, 'state': 'persisted_offline',
                'context_sha256': sha(payload['CONTEXT.json']), 'history_exists': history_fd is not None,
                'directories': directories, 'files': values}
            entries(api, manifest, folder)
            raw = encoded(manifest)
            if len(raw) > api.MANAGED_MAX_FILE: raise ValueError('server checkpoint manifest exceeds budget')
            def before_publish():
                check_history()
                again_context, again_payload = capture(api, folder, temporary / 'rechecked', server_binary, policy, env_file)
                if context != again_context or payload != again_payload or metadata(api, folder) != original_metadata or database_sources(api, database) != original_db:
                    raise ValueError('server sources changed during stopped capture')
                if history_fd is not None and ops_history.tree(api, history_fd) != (history_files, directories):
                    raise ValueError('history changed during stopped capture')
            return write_archive(api, output, temporary, SERVER_KIND, folder, raw,
                                 [(v['path'], checkpoint / v['path']) for v in values], before_publish)


def entries(api, manifest, folder):
    keys = {'version', 'kind', 'checkpoint_id', 'original_directory', 'created_at_ms', 'state', 'context_sha256', 'history_exists', 'directories', 'files'}
    if (not isinstance(manifest, dict) or set(manifest) != keys or manifest['version'] != 2
            or manifest['kind'] != 'frp-offline-server-checkpoint' or manifest['original_directory'] != str(folder)
            or not isinstance(manifest['checkpoint_id'], str) or not api.UUID.fullmatch(manifest['checkpoint_id'])
            or type(manifest['created_at_ms']) is not int or manifest['created_at_ms'] <= 0
            or manifest['state'] != 'persisted_offline' or type(manifest['history_exists']) is not bool
            or not isinstance(manifest['context_sha256'], str) or not api.HEX_DIGEST.fullmatch(manifest['context_sha256'])):
        raise ValueError('invalid version2 server checkpoint')
    def allowed(name):
        return (name in ('CONTEXT.json', 'POLICY.json', 'control/control.sqlite', 'aux/installation.json')
                or re.fullmatch(r'context/f[0-9]{6}', name) is not None
                or manifest['history_exists'] and name.startswith('history/') and relative(name[8:]) and name != 'history/flock.lock')
    values = validate_entries(api, manifest['files'], api.MAX_TOTAL, api.MAX_FILE, allowed)
    names = {v['path'] for v in values}
    if not {'CONTEXT.json', 'POLICY.json', 'control/control.sqlite'} <= names: raise ValueError('incomplete server checkpoint')
    if any(v['size'] > api.MANAGED_MAX_FILE for v in values if not v['path'].startswith(('control/', 'history/'))):
        raise ValueError('server context payload exceeds budget')
    directories = manifest['directories']
    if (not isinstance(directories, list) or len(values) + len(directories) > api.MANAGED_MAX_FILES
            or any(not isinstance(d, str) or not relative(d) for d in directories) or len(set(directories)) != len(directories)
            or directories and not manifest['history_exists']): raise ValueError('invalid server history directory inventory')
    for name in [v['path'][8:] for v in values if v['path'].startswith('history/')] + directories:
        if any(str(parent) not in directories for parent in Path(name).parents if str(parent) != '.'):
            raise ValueError('incomplete server history directory inventory')
    if any('history/' + name in names for name in directories): raise ValueError('server history path collision')
    return values


def normalized_policy(api, data):
    value = api.strict_json(data)
    if (not isinstance(value, dict) or set(value) != POLICY_KEYS or value['version'] != 1
            or not absolute(value['config_file']) or not absolute(value['working_dir'])
            or not isinstance(value['roots'], list) or not isinstance(value['files'], list)):
        raise ValueError('invalid local server policy')
    seen_ids, seen_paths = set(), []
    for key in ('roots', 'files'):
        for entry in value[key]:
            if (not isinstance(entry, dict) or set(entry) != {'id', 'path'}
                    or not isinstance(entry['id'], str) or re.fullmatch(r'[a-z][a-z0-9_-]{0,31}', entry['id']) is None
                    or entry['id'] in seen_ids or not absolute(entry['path']) or entry['path'] == '/'):
                raise ValueError('invalid local server policy authorization')
            path = Path(entry['path'])
            if any(within(path, other) or within(other, path) for other in seen_paths):
                raise ValueError('overlapping local server policy authorization')
            seen_ids.add(entry['id']); seen_paths.append(path)
        value[key].sort(key=lambda entry: entry['id'])
    if not 1 <= len(value['roots']) <= 32 or len(value['files']) > 128: raise ValueError('server policy exceeds authorization budget')
    return value


def authorized(path, policy, directory=False):
    return any(within(path, Path(entry['path'])) for entry in policy['roots']) or (
        not directory and any(path == Path(entry['path']) for entry in policy['files']))


def check_input_files(api, *paths):
    return {str(Path(path).absolute()): private_bytes(api, Path(path).absolute(), api.MANAGED_MAX_FILE) for path in paths if path is not None}


def _write_file(api, path, data, stamp=None):
    temporary = path.parent / ('.frp-proof-' + str(uuid.uuid4()))
    try:
        fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
        with os.fdopen(fd, 'wb') as output:
            output.write(data); output.flush(); os.fsync(output.fileno())
        if stamp is not None: os.utime(temporary, ns=(stamp, stamp))
        api.sync_file(temporary)
        os.link(temporary, path, follow_symlinks=False)
        temporary.unlink(); api.sync_directory(path.parent)
    finally:
        if temporary.exists(): temporary.unlink()


def check_archive_context(api, checkpoint, manifest, temporary):
    # Reconstitute just the helper's sealed namespace. DB/TSDB and installation
    # metadata are not native configuration inputs and cannot widen its policy.
    source = temporary / 'sealed-source'; source.mkdir(mode=0o700)
    for entry in manifest['files']:
        if entry['path'] in ('CONTEXT.json', 'POLICY.json') or entry['path'].startswith('context/'):
            raw = private_bytes(api, checkpoint / entry['path'], api.MANAGED_MAX_FILE)
            if sha(raw) != entry['sha256'] or len(raw) != entry['size']: raise ValueError('sealed server context changed')
            (source / entry['path']).parent.mkdir(mode=0o700, parents=True, exist_ok=True)
            _write_file(api, source / entry['path'], raw)
    context, payload = context_directory(api, source)
    if sha(payload['CONTEXT.json']) != manifest['context_sha256'] or context['cwd'] != manifest['original_directory']:
        raise ValueError('server context identity mismatch')
    if manifest['history_exists'] and not context['history_enabled']: raise ValueError('server history presence mismatch')
    return source, context


def transform(api, checkpoint, manifest, folder, temporary, binary, policy, source_policy, env_file, source_env_file):
    if not policy or not source_policy: raise ValueError('server restoration requires both local policies')
    local_inputs = check_input_files(api, policy, source_policy, env_file, source_env_file)
    source, original = check_archive_context(api, checkpoint, manifest, temporary)
    output = temporary / 'target-context'
    result = maintenance(api, ['transform', '--checkpoint', str(source), '--manifest-digest', manifest['context_sha256'],
        '--output', str(output), *arguments(policy, env_file, source_policy, source_env_file)], temporary, binary)
    context, payload = context_directory(api, output, result)
    if check_input_files(api, policy, source_policy, env_file, source_env_file) != local_inputs:
        raise ValueError('local server authorization inputs changed')
    local = normalized_policy(api, local_inputs[str(Path(policy).absolute())])
    if normalized_policy(api, payload['POLICY.json']) != local or context['cwd'] != str(folder) or context['config_file'] != str(folder / 'server.toml'):
        raise ValueError('transformed server context differs from local authorization')
    for entry in context['files']:
        if not authorized(Path(entry['path']), local): raise ValueError('native target dependency is unauthorized')
    if not authorized(Path(context['database_file']), local): raise ValueError('native target database is unauthorized')
    if context['history_enabled'] and not authorized(Path(context['history_path']), local, True):
        raise ValueError('native target history is unauthorized')
    if original['history_enabled'] != context['history_enabled']: raise ValueError('native target history behavior changed')
    return context, payload, local


def owned_directory(api, path, private=True):
    api.path_without_links(path)
    info = path.stat()
    if (not stat.S_ISDIR(info.st_mode) or info.st_uid != os.geteuid()
            or (stat.S_IMODE(info.st_mode) != 0o700 if private else bool(info.st_mode & 0o022))):
        raise ValueError('server installation directory is not private')
    return info


class Pins:
    """Keep every used parent inode alive and verify its fixed pathname."""
    def __init__(self, api): self.api, self.values = api, {}
    def pin(self, path, trusted=False):
        path = self.api.path_without_links(path)
        if path in self.values: self.check(); return
        fd = os.open(path, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
        info = os.fstat(fd)
        if (not stat.S_ISDIR(info.st_mode) or info.st_uid not in ((0, os.geteuid()) if trusted else (os.geteuid(),))
                or (bool(info.st_mode & 0o022) if trusted else stat.S_IMODE(info.st_mode) != 0o700)):
            os.close(fd); raise ValueError('unsafe server target parent')
        self.values[path] = (fd, info)
    def check(self):
        for path, (fd, before) in self.values.items():
            self.api.path_without_links(path); after = path.stat(); pinned = os.fstat(fd)
            ident = lambda value: (value.st_dev, value.st_ino, value.st_uid, stat.S_IMODE(value.st_mode))
            if ident(before) != ident(after) or ident(before) != ident(pinned): raise ValueError('server target parent changed')
    def close(self):
        for fd, _ in self.values.values(): os.close(fd)
        self.values.clear()


def mkdir_authorized(api, path, policy, pins):
    if path.exists(): pins.pin(path, trusted=True); return
    if not authorized(path, policy, True): raise ValueError('creating target directory is not authorized')
    parent = path.parent
    if parent.exists(): pins.pin(parent, trusted=True)
    else: mkdir_authorized(api, parent, policy, pins)
    pins.check(); path.mkdir(mode=0o700); api.sync_directory(parent); pins.pin(path)


def file_state(api, path, maximum):
    api.path_without_links(path)
    if not path.exists():
        if path.is_symlink(): raise ValueError('server target is a symbolic link')
        return None
    before = path.stat()
    if before.st_uid != os.geteuid() or before.st_nlink != 1 or stat.S_IMODE(before.st_mode) != 0o600 or not stat.S_ISREG(before.st_mode):
        raise ValueError('server target file is not private')
    result = ops_history.disk_digest(api, path)
    if result[2] > maximum: raise ValueError('server target file exceeds budget')
    return {'size': result[2], 'sha256': result[4], 'mtime_ns': result[3]}


def desired(entry): return {key: entry[key] for key in ('size', 'sha256', 'mtime_ns')}


def atomic_copy(api, source, target, entry, pins):
    pins.pin(target.parent, trusted=True); pins.check()
    current = file_state(api, target, api.MAX_FILE)
    if current is not None:
        if current != desired(entry): raise ValueError('server target content or modification time changed')
        return
    temporary = target.parent / ('.frp-write-' + str(uuid.uuid4()))
    source_fd = os.open(source, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    destination_fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    try:
        digest, size = hashlib.sha256(), 0; deadline = time.monotonic() + 60
        while chunk := os.read(source_fd, 65536):
            size += len(chunk); digest.update(chunk)
            if size > entry['size'] or time.monotonic() > deadline: raise ValueError('server staged payload exceeded bounds')
            view = memoryview(chunk)
            while view: view = view[os.write(destination_fd, view):]
        if size != entry['size'] or digest.hexdigest() != entry['sha256']: raise ValueError('server staged payload changed')
        os.utime(destination_fd, ns=(entry['mtime_ns'], entry['mtime_ns'])); os.fsync(destination_fd)
        pins.check(); os.link(temporary, target, follow_symlinks=False); temporary.unlink(); api.sync_directory(target.parent)
        if file_state(api, target, api.MAX_FILE) != desired(entry): raise ValueError('server publication changed')
    finally:
        os.close(source_fd); os.close(destination_fd)
        if temporary.exists(): temporary.unlink()


def plan_lock(api, folder, checkpoint_id, pins):
    path = folder.parent / ('.frp-server-restore-' + checkpoint_id)
    pins.pin(folder.parent, trusted=True)
    if not path.exists(): pins.check(); path.mkdir(mode=0o700); api.sync_directory(path.parent)
    pins.pin(path)
    lock = os.open(path / '.lock', os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600)
    info = os.fstat(lock)
    if info.st_uid != os.geteuid() or info.st_nlink != 1 or stat.S_IMODE(info.st_mode) != 0o600 or not stat.S_ISREG(info.st_mode):
        os.close(lock); raise ValueError('unsafe server restore lease')
    try: fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError as error: os.close(lock); raise ValueError('server restore is already running') from error
    return path, lock


def publish_directory(source, target):
    """Atomic directory publication with no replacement, including empty dirs."""
    import ctypes
    import sys
    library = ctypes.CDLL(None, use_errno=True)
    if sys.platform == 'darwin':
        call = library.renamex_np; call.argtypes = [ctypes.c_char_p, ctypes.c_char_p, ctypes.c_uint]
        status = call(os.fsencode(source), os.fsencode(target), 4)  # RENAME_EXCL
    elif sys.platform.startswith('linux') and hasattr(library, 'renameat2'):
        call = library.renameat2; call.argtypes = [ctypes.c_int, ctypes.c_char_p, ctypes.c_int, ctypes.c_char_p, ctypes.c_uint]
        status = call(-100, os.fsencode(source), -100, os.fsencode(target), 1)  # AT_FDCWD, RENAME_NOREPLACE
    else: raise ValueError('atomic no-replace directory publication is unavailable')
    if status != 0: raise OSError(ctypes.get_errno(), 'atomic server publication failed')


def verify_tree(api, root, files, directories, *, complete=False, allow_lock=False):
    expected = {str(Path(value['target']).relative_to(root)): desired(value) for value in files if within(Path(value['target']), root)}
    allowed_dirs = set(directories)
    for name in expected:
        allowed_dirs.update(str(p) for p in Path(name).parents if str(p) != '.')
    observed = set()
    if not root.exists():
        if complete: raise ValueError('server target tree is missing')
        return
    owned_directory(api, root)
    for top, dirs, names in os.walk(root, followlinks=False):
        for name in dirs:
            path = Path(top) / name; rel = path.relative_to(root).as_posix()
            if rel not in allowed_dirs: raise ValueError('unexpected server target directory')
            owned_directory(api, path)
        for name in names:
            path = Path(top) / name; rel = path.relative_to(root).as_posix()
            if allow_lock and rel == 'flock.lock':
                if file_state(api, path, 1024) is None: raise ValueError('server target lock is missing')
                continue
            if rel not in expected or file_state(api, path, api.MAX_FILE) != expected[rel]:
                raise ValueError('unexpected or changed server target file')
            observed.add(rel)
    if complete and observed != set(expected): raise ValueError('server target tree is incomplete')
    if complete and any(not (root / name).is_dir() for name in allowed_dirs): raise ValueError('server target directories are incomplete')


def restore(api, checkpoint, manifest, folder, *, offline, server_binary, policy, source_policy,
            env_file=None, source_env_file=None, hook=None):
    if not offline: raise ValueError('server restoration requires explicit --offline acknowledgement')
    folder = api.path_without_links(folder)
    entries(api, manifest, Path(manifest['original_directory']))
    raw_manifest = private_bytes(api, checkpoint / 'SERVER.json', api.MANAGED_MAX_FILE)
    if api.strict_json(raw_manifest) != manifest: raise ValueError('staged server manifest changed')
    archive_digest = sha(raw_manifest)
    pins = Pins(api); lock = None
    try:
        with restore_parent(api, folder.parent), tempfile.TemporaryDirectory(prefix='.frp-server-transform-', dir=folder.parent) as temporary:
            temporary = Path(temporary)
            target, payload, authorization = transform(api, checkpoint, manifest, folder, temporary, server_binary,
                                                       policy, source_policy, env_file, source_env_file)
            database = Path(target['database_file']); history = Path(target['history_path']) if target['history_enabled'] else None
            gate = Path(str(database) + '.restore-gate.json')
            plan_path = folder.parent / ('.frp-server-restore-' + manifest['checkpoint_id'])
            if folder.exists() and not (plan_path / 'PLAN.json').exists(): raise ValueError('move the stopped master aside before restoration')
            plan_path, lock = plan_lock(api, folder, manifest['checkpoint_id'], pins)
            lock_info = os.fstat(lock)
            def lease_check():
                pins.check()
                named = os.stat(plan_path / '.lock', follow_symlinks=False)
                if (named.st_dev, named.st_ino) != (lock_info.st_dev, lock_info.st_ino): raise ValueError('server restore lease was replaced')
            existing = api.strict_json(private_bytes(api, plan_path / 'PLAN.json', MAX_PLAN)) if (plan_path / 'PLAN.json').exists() else None
            if existing is not None and (not isinstance(existing, dict) or set(existing) != {'version', 'checkpoint_id', 'manifest_digest', 'target', 'context_revision', 'policy_sha256', 'gate', 'entries', 'directories', 'history_path', 'history_exists'}):
                raise ValueError('invalid durable server restore plan')
            marker = {'version': 1, 'kind': 'frp-server-restore-gate', 'checkpoint_id': manifest['checkpoint_id'],
                'context_revision': target['context_revision'], 'policy_sha256': target['policy_digest'],
                'created_at_ms': existing['gate']['created_at_ms'] if existing else time.time_ns() // 1_000_000}
            marker_bytes = encoded(marker); stamp = marker['created_at_ms'] * 1_000_000
            files = []; sources = {}
            def add(path, source, entry, kind):
                if any(value['target'] == str(path) for value in files): raise ValueError('server target path collision')
                files.append({'target': str(path), 'kind': kind, **desired(entry)}); sources[str(path)] = source
            for entry in target['files']:
                source = temporary / 'target-context' / entry['payload']
                add(Path(entry['path']), source, entry, 'context')
            manifest_entries = {value['path']: value for value in manifest['files']}
            if 'aux/installation.json' in manifest_entries:
                meta = private_bytes(api, checkpoint / 'aux/installation.json', api.MANAGED_MAX_FILE)
                if api.strict_json(meta) != {'format': 2, 'roles': ['server']}: raise ValueError('invalid archived server installation metadata')
                add(folder / 'installation.json', checkpoint / 'aux/installation.json', manifest_entries['aux/installation.json'], 'context')
            add(database, checkpoint / 'control/control.sqlite', manifest_entries['control/control.sqlite'], 'database')
            if not authorized(database, authorization) or (history is not None and not authorized(history, authorization, True)):
                raise ValueError('server data destination is unauthorized')
            if any(Path(value['target']) == gate or (history is not None and within(Path(value['target']), history)) for value in files):
                raise ValueError('server data paths overlap ordinary dependencies')
            if history is not None and (within(database, history) or within(history, database)):
                raise ValueError('server data paths overlap')
            for name, entry in manifest_entries.items():
                if name.startswith('history/'):
                    if history is None: raise ValueError('archived history lacks an authorized target')
                    add(history / name[8:], checkpoint / name, entry, 'history')
            marker_source = temporary / 'gate.json'; api.private(marker_source, marker_bytes.decode())
            add(gate, marker_source, {'size': len(marker_bytes), 'sha256': sha(marker_bytes), 'mtime_ns': stamp}, 'gate')
            files.sort(key=lambda value: value['target'])
            directory_paths = {str(folder)}
            if history is not None and manifest['history_exists']:
                directory_paths.add(str(history)); directory_paths.update(str(history / name) for name in manifest['directories'])
            for value in files:
                parent = Path(value['target']).parent
                while parent != folder.parent and (within(parent, folder) or authorized(parent, authorization, True)):
                    directory_paths.add(str(parent)); parent = parent.parent
            plan = {'version': 1, 'checkpoint_id': manifest['checkpoint_id'], 'manifest_digest': archive_digest,
                'target': str(folder), 'context_revision': target['context_revision'], 'policy_sha256': target['policy_digest'],
                'gate': marker, 'entries': files, 'directories': sorted(directory_paths),
                'history_path': str(history) if history is not None else '', 'history_exists': manifest['history_exists']}
            if len(encoded(plan)) > MAX_PLAN: raise ValueError('server restore plan exceeds budget')
            if existing is not None and existing != plan: raise ValueError('server restore plan or local authorization changed')
            if existing is None:
                if folder.exists(): raise ValueError('server target installation already exists')
                for path in (database, gate, *(Path(str(database) + suffix) for suffix in ('-wal', '-shm', '-journal'))):
                    if path.exists() or path.is_symlink(): raise ValueError('server target database or sidecar already exists')
                if history is not None and (history.exists() or history.is_symlink()): raise ValueError('server target history already exists')
                for value in files:
                    path = Path(value['target'])
                    if within(path, folder) or value['kind'] != 'context': continue
                    current = file_state(api, path, api.MAX_FILE)
                    if current is not None and current != desired(value): raise ValueError('shared dependency differs from archived bytes, mode or mtime')
                _write_file(api, plan_path / 'PLAN.json', encoded(plan)); existing = plan
            if hook: hook('plan_persisted')
            stage = folder if folder.exists() else plan_path / 'installation'
            if not stage.exists(): stage.mkdir(mode=0o700); api.sync_directory(stage.parent)
            pins.pin(stage)
            def staged(path): return stage / path.relative_to(folder) if within(path, folder) else path
            # DB sidecars are never supplied by an archive. They must stay absent
            # throughout publication; a new native startup would violate offline.
            def check_data_sidecars():
                for suffix in ('-wal', '-shm', '-journal'):
                    value = staged(Path(str(database) + suffix))
                    if value.exists() or value.is_symlink(): raise ValueError('database sidecar appeared during offline restoration')
            check_data_sidecars()
            stage_files = [{**value, 'target': str(staged(Path(value['target'])))} for value in files]
            main_dirs = [str(staged(Path(name)).relative_to(stage)) for name in plan['directories'] if Path(name) != folder and within(Path(name), folder)]
            verify_tree(api, stage, stage_files, main_dirs)
            if history is not None and not within(history, folder) and history.exists():
                verify_tree(api, history, files, manifest['directories'], allow_lock=True)
            for name in sorted(directory_paths, key=lambda name: (len(Path(name).parts), name)):
                path = Path(name); destination = staged(path)
                if within(path, folder):
                    if not destination.exists(): pins.check(); destination.mkdir(mode=0o700); api.sync_directory(destination.parent)
                    pins.pin(destination)
                else: mkdir_authorized(api, destination, authorization, pins)
            # Exact-file authorization never grants mkdir to its parent.
            for value in files:
                parent = staged(Path(value['target'])).parent
                if not parent.exists(): mkdir_authorized(api, parent, authorization, pins)
                pins.pin(parent, trusted=True)
            ordered = sorted(files, key=lambda value: (value['kind'] != 'gate', value['kind'] == 'database', value['target']))
            for value in ordered:
                lease_check(); check_data_sidecars()
                destination = staged(Path(value['target']))
                atomic_copy(api, sources[value['target']], destination, value, pins)
                if hook: hook('target:' + value['kind'])
            verify_tree(api, stage, stage_files, main_dirs, complete=True)
            if history is not None and not within(history, folder) and manifest['history_exists']:
                verify_tree(api, history, files, manifest['directories'], complete=True, allow_lock=True)
            ops_history.database_valid(api, staged(database))
            lease_check(); check_data_sidecars()
            if hook: hook('before_publish')
            if stage != folder:
                old = stage.stat()
                # Child descriptors remain valid across rename; remove their old
                # path identities immediately before the no-replace publication.
                for path in list(pins.values):
                    if within(path, stage): os.close(pins.values.pop(path)[0])
                publish_directory(stage, folder); api.sync_directory(folder.parent)
                new = folder.stat()
                if (old.st_dev, old.st_ino) != (new.st_dev, new.st_ino): raise ValueError('published server installation changed')
                pins.pin(folder)
            lease_check()
            completed = {'version': 1, 'checkpoint_id': manifest['checkpoint_id'], 'manifest_digest': archive_digest, 'state': 'installed_gated'}
            if not (plan_path / 'PUBLISHED.json').exists(): _write_file(api, plan_path / 'PUBLISHED.json', encoded(completed))
            elif api.strict_json(private_bytes(api, plan_path / 'PUBLISHED.json', 8192)) != completed: raise ValueError('server publication receipt changed')
            return {'code': 'ok', 'checkpoint_id': manifest['checkpoint_id'], 'state': 'installed_gated', 'restart_required': True}
    finally:
        if lock is not None: os.close(lock)
        pins.close()


def evaluate_confirmation(api, states, receipts):
    """Mirror control.EvaluateServerRestoreConfirmation; never repair DB facts."""
    import unicodedata
    out = {'ready': False, 'code': 'invalid_operation_state', 'active_operations': 0,
           'receipts': len(receipts), 'completed': 0, 'superseded': 0}
    valid_states = set(ACTIVE_STATES) | {'confirmed', 'rejected', 'conflict', 'failed', 'cancelled', 'rolled_back'}
    for state, count in states.items():
        if state not in valid_states or type(count) is not int or count < 0 or count > 2**63 - 1: return out
        if state in ACTIVE_STATES:
            out['active_operations'] += count
            if out['active_operations'] > 2**63 - 1: return out
    def fail(code): out['code'] = code; return out
    if out['active_operations']: return fail('active_operations')
    if len(receipts) > 10000: return fail('receipt_limit')
    identities, epochs, services, children, complete = set(), set(), set(), {}, set()
    def identity(value): return isinstance(value, str) and api.UUID.fullmatch(value) is not None
    def digest(value): return isinstance(value, str) and api.HEX_DIGEST.fullmatch(value) is not None
    for index, item in enumerate(receipts):
        node = str(item['node_id']); creator = item['creator']
        valid = (re.fullmatch(r'[1-9][0-9]*', node) is not None and int(node) <= 2**63 - 1
            and all(identity(item[key]) for key in ('id', 'service_id', 'epoch', 'backup_service_id'))
            and (item['replaced_service_id'] == '' or identity(item['replaced_service_id']))
            and item['service_id'] not in (item['backup_service_id'], item['replaced_service_id'])
            and all(digest(item[key]) for key in ('manifest_digest', 'context_revision', 'store_digest', 'token_sha256'))
            and item['state'] in ('pending', 'acknowledged')
            and isinstance(creator, str) and creator != '' and creator.strip() == creator and len(creator.encode()) <= 128
            and not any(unicodedata.category(char) == 'Cc' for char in creator)
            and all(type(item[key]) is int and 0 < item[key] <= 2**63 - 1 for key in ('created_at_ms', 'updated_at_ms', 'version'))
            and item['updated_at_ms'] >= item['created_at_ms'])
        if not valid or item['id'] in identities or (node, item['epoch']) in epochs or (node, item['service_id']) in services:
            return fail('invalid_receipt')
        identities.add(item['id']); epochs.add((node, item['epoch'])); services.add((node, item['service_id']))
        at = item['confirmed_at_ms']
        if at is not None:
            if type(at) is not int or not 0 < at <= 2**63 - 1 or at < item['created_at_ms']:
                return fail('invalid_completion')
            if item['state'] == 'acknowledged': complete.add(index); out['completed'] += 1
        if item['replaced_service_id']:
            children.setdefault((node, item['replaced_service_id']), []).append(index)
    for index, item in enumerate(receipts):
        if index in complete: continue
        token = item['token_sha256']; seen = set(); at = index; depth = 0
        while True:
            if at in seen: return fail('receipt_chain_cycle')
            seen.add(at); current = receipts[at]
            if current['token_sha256'] != token or current['current_token_sha256'] != token: return fail('receipt_binding')
            if at in complete: out['superseded'] += 1; break
            if depth >= 128: return fail('receipt_chain_limit')
            next_values = children.get((str(current['node_id']), current['service_id']), [])
            if not next_values:
                return fail('receipt_acknowledgement_required' if current['confirmed_at_ms'] is not None else 'receipt_unresolved')
            if len(next_values) != 1: return fail('receipt_chain_ambiguous')
            next_index = next_values[0]
            if receipts[next_index]['created_at_ms'] < current['created_at_ms']: return fail('receipt_chain_order')
            at = next_index; depth += 1
    out['ready'], out['code'] = True, 'ok'
    return out


def database_confirmation(api, path):
    with closing(sqlite3.connect(path.as_uri() + '?mode=ro', uri=True, timeout=3)) as db:
        db.execute('PRAGMA query_only=ON'); db.execute('BEGIN')
        api.integrity_check(db, time.monotonic() + 30)
        if db.execute('PRAGMA application_id').fetchone() != (1179798836,) or db.execute('PRAGMA user_version').fetchone() != (12,):
            raise ValueError('server confirmation requires the current database schema after native startup migration')
        states = dict(db.execute('SELECT state,COUNT(*) FROM config_operations GROUP BY state LIMIT 16'))
        orphan = db.execute('SELECT EXISTS(SELECT 1 FROM config_restore_completions c LEFT JOIN config_restores r ON r.id=c.receipt_id WHERE r.id IS NULL)').fetchone()[0]
        if orphan: raise ValueError('server ledger confirmation refused: invalid_completion')
        columns = ['id', 'node_id', 'service_id', 'epoch', 'backup_service_id', 'replaced_service_id', 'manifest_digest', 'context_revision', 'store_digest', 'state', 'creator', 'created_at_ms', 'updated_at_ms', 'version', 'token_sha256']
        sql = ('SELECT ' + ','.join('r.' + name for name in columns) + ',c.confirmed_at_ms,n.token_sha256 '
            'FROM config_restores r LEFT JOIN config_restore_completions c ON c.receipt_id=r.id '
            'LEFT JOIN nodes n ON n.id=r.node_id ORDER BY r.id LIMIT 10001')
        receipts = [dict(zip(columns + ['confirmed_at_ms', 'current_token_sha256'], row)) for row in db.execute(sql)]
        result = evaluate_confirmation(api, states, receipts)
        if not result['ready']: raise ValueError('server ledger confirmation refused: ' + result['code'])
        return result


def confirm(api, folder, checkpoint_id, *, offline, agents_reviewed, server_binary, policy, env_file=None):
    if not offline or not agents_reviewed: raise ValueError('server confirmation requires --offline and --agents-reviewed')
    if not isinstance(checkpoint_id, str) or api.UUID.fullmatch(checkpoint_id) is None: raise ValueError('invalid server checkpoint identity')
    folder = api.private_directory(folder); pins = Pins(api)
    try:
        with restore_parent(api, folder.parent), tempfile.TemporaryDirectory(prefix='.frp-server-confirm-', dir=folder.parent) as temp:
            temporary = Path(temp); inputs = check_input_files(api, policy, env_file)
            context, payload = capture(api, folder, temporary / 'context', server_binary, policy, env_file)
            local = normalized_policy(api, inputs[str(Path(policy).absolute())])
            if normalized_policy(api, payload['POLICY.json']) != local: raise ValueError('server confirmation policy mismatch')
            database = Path(context['database_file']); gate = Path(str(database) + '.restore-gate.json')
            receipt = Path(str(database) + '.restore-completed-' + checkpoint_id + '.json')
            pins.pin(database.parent, trusted=True)
            history = Path(context['history_path']) if context['history_enabled'] else None
            with history_lease(api, history) as (history_fd, check_history):
                marker_bytes = private_bytes(api, gate, 8192) if gate.exists() or gate.is_symlink() else None
                marker = api.strict_json(marker_bytes) if marker_bytes is not None else None
                keys = {'version', 'kind', 'checkpoint_id', 'context_revision', 'policy_sha256', 'created_at_ms'}
                if marker_bytes is not None and (not isinstance(marker, dict) or set(marker) != keys or marker['version'] != 1
                        or marker['kind'] != 'frp-server-restore-gate' or marker['checkpoint_id'] != checkpoint_id
                        or marker['context_revision'] != context['context_revision'] or marker['policy_sha256'] != context['policy_digest']
                        or type(marker['created_at_ms']) is not int or marker['created_at_ms'] <= 0):
                    raise ValueError('server restore gate or current context differs')
                prior = api.strict_json(private_bytes(api, receipt, 8192)) if receipt.exists() or receipt.is_symlink() else None
                if marker is None and prior is None: raise ValueError('server restore gate is missing')
                if prior is not None and (not isinstance(prior, dict) or set(prior) != {'version', 'kind', 'checkpoint_id', 'context_revision', 'policy_sha256', 'gate_sha256', 'confirmed_at_ms', 'state'}
                        or prior['version'] != 1 or prior['kind'] != 'frp-server-restore-completion' or prior['checkpoint_id'] != checkpoint_id
                        or prior['context_revision'] != context['context_revision'] or prior['policy_sha256'] != context['policy_digest']
                        or prior['state'] != 'confirmed_requires_restart' or type(prior['confirmed_at_ms']) is not int or prior['confirmed_at_ms'] <= 0
                        or not isinstance(prior['gate_sha256'], str) or not api.HEX_DIGEST.fullmatch(prior['gate_sha256'])
                        or marker_bytes is not None and prior['gate_sha256'] != sha(marker_bytes)):
                    raise ValueError('server completion receipt differs from this restore')
                original_db = snapshot_database(api, database, temporary / 'verified.sqlite', temporary)
                database_confirmation(api, temporary / 'verified.sqlite')
                again, again_payload = capture(api, folder, temporary / 'rechecked', server_binary, policy, env_file)
                if context != again or payload != again_payload or inputs != check_input_files(api, policy, env_file) or original_db != database_sources(api, database):
                    raise ValueError('server context or ledger changed during offline confirmation')
                check_history(); pins.check()
                if marker_bytes is not None and private_bytes(api, gate, 8192) != marker_bytes: raise ValueError('server restore gate changed')
                if prior is None:
                    completed = {'version': 1, 'kind': 'frp-server-restore-completion', 'checkpoint_id': checkpoint_id,
                        'context_revision': context['context_revision'], 'policy_sha256': context['policy_digest'],
                        'gate_sha256': sha(marker_bytes), 'confirmed_at_ms': time.time_ns() // 1_000_000, 'state': 'confirmed_requires_restart'}
                    _write_file(api, receipt, encoded(completed))
                if marker_bytes is not None:
                    pins.check(); check_history()
                    if private_bytes(api, gate, 8192) != marker_bytes: raise ValueError('server restore gate changed')
                    gate.unlink(); api.sync_directory(gate.parent)
                return {'code': 'ok', 'checkpoint_id': checkpoint_id, 'state': 'confirmed', 'restart_required': True}
    finally: pins.close()
