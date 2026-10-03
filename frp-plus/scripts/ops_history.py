"""Stopped-process SQLite + VictoriaMetrics archive, restored at its original path.

The operator stops the master before stopping Agents. The TSDB lock is not an
online database snapshot or a claim that samples queued before shutdown survived.
"""
from __future__ import annotations

from contextlib import contextmanager, closing
import fcntl
import hashlib
import json
import os
from pathlib import Path
import sqlite3
import stat
import tempfile
import time
import tomllib

from ops_checkpoint import SERVER_KIND, relative, validate_entries, write_archive


def _same(a, b):
    return (a.st_dev, a.st_ino, a.st_size, a.st_mtime_ns, stat.S_IMODE(a.st_mode)) == (b.st_dev, b.st_ino, b.st_size, b.st_mtime_ns, stat.S_IMODE(b.st_mode))


def _directory(info):
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.geteuid() or info.st_mode & 0o022:
        raise ValueError('unsafe stopped history directory')


def _file(info, maximum):
    if (not stat.S_ISREG(info.st_mode) or info.st_uid != os.geteuid() or info.st_nlink != 1
            or info.st_mode & 0o022 or not 0 <= info.st_size <= maximum):
        raise ValueError('unsafe stopped history file')


@contextmanager
def history_lease(api, folder, configured):
    if not configured:
        yield None, None, lambda: None
        return
    path = Path(api._history_directory(configured, folder))
    if not path.exists():
        def check_absent():
            if path.exists() or path.is_symlink(): raise ValueError('history appeared during stopped backup')
        yield path, None, check_absent
        return
    api.private_directory(path)
    root = os.open(path, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    lock = None
    before = os.fstat(root)
    try:
        _directory(before)
        lock = os.open('flock.lock', os.O_RDWR | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=root)
        original_lock = os.fstat(lock); _file(original_lock, 1024)
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError as exc:
            raise ValueError('history is still in use; stop the master before backup') from exc
        def check():
            named = os.stat('flock.lock', dir_fd=root, follow_symlinks=False)
            if not _same(before, os.stat(path, follow_symlinks=False)) or not _same(original_lock, named):
                raise ValueError('history root or lock changed during backup')
        check()
        yield path, root, check
    finally:
        if lock is not None: os.close(lock)
        os.close(root)


def _names(fd, maximum):
    names = []
    with os.scandir(fd) as source:
        for entry in source:
            names.append(entry.name)
            if len(names) > maximum: raise ValueError('history exceeds directory budget')
    return sorted(names)


def tree(api, root_fd, output=None):
    """Pinned, no-follow traversal, copied and hashed with bounded streaming reads."""
    files, directories = [], []
    total, deadline = 0, time.monotonic() + 60
    def walk(fd, prefix):
        nonlocal total
        parent_info = os.fstat(fd); _directory(parent_info)
        names = _names(fd, api.MANAGED_MAX_FILES)
        if len(names) > api.MANAGED_MAX_FILES or len(files) + len(directories) + len(names) > api.MANAGED_MAX_FILES:
            raise ValueError('history exceeds file budget')
        for name in names:
            if time.monotonic() > deadline: raise ValueError('history capture exceeded time budget')
            if not prefix and name == 'flock.lock': continue
            path = prefix + name
            if not relative(path): raise ValueError('unsupported history path')
            before = os.stat(name, dir_fd=fd, follow_symlinks=False)
            if stat.S_ISDIR(before.st_mode):
                _directory(before)
                child = os.open(name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=fd)
                try:
                    if not _same(before, os.fstat(child)): raise ValueError('history directory changed')
                    directories.append(path)
                    if output is not None: (output / path).mkdir(mode=0o700)
                    walk(child, path + '/')
                    if not _same(before, os.fstat(child)): raise ValueError('history directory changed')
                finally: os.close(child)
            else:
                _file(before, api.MAX_FILE)
                total += before.st_size
                if total > api.MAX_TOTAL: raise ValueError('history exceeds byte budget')
                source_fd = os.open(name, os.O_RDONLY | os.O_NONBLOCK | os.O_NOFOLLOW, dir_fd=fd)
                destination = None
                try:
                    if not _same(before, os.fstat(source_fd)): raise ValueError('history file changed')
                    if output is not None:
                        destination = open(os.open(output / path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600), 'wb')
                    digest, size = hashlib.sha256(), 0
                    while chunk := os.read(source_fd, 65536):
                        size += len(chunk)
                        if size > before.st_size or time.monotonic() > deadline: raise ValueError('history changed or exceeded time budget')
                        digest.update(chunk)
                        if destination: destination.write(chunk)
                    if size != before.st_size or not _same(before, os.fstat(source_fd)): raise ValueError('history file changed')
                    if destination:
                        destination.flush(); os.fsync(destination.fileno())
                finally:
                    if destination: destination.close()
                    os.close(source_fd)
                files.append({'path': 'history/' + path, 'size': size, 'mtime_ns': before.st_mtime_ns, 'sha256': digest.hexdigest()})
            if not _same(before, os.stat(name, dir_fd=fd, follow_symlinks=False)):
                raise ValueError('history entry changed during capture')
        if names != _names(fd, api.MANAGED_MAX_FILES) or not _same(parent_info, os.fstat(fd)):
            raise ValueError('history directory changed during capture')
    walk(root_fd, '')
    return files, directories


def database_valid(api, path):
    with closing(sqlite3.connect(path.as_uri() + '?mode=ro', uri=True)) as db:
        api.integrity_check(db, time.monotonic() + 30)
        if db.execute('PRAGMA application_id').fetchone() != (1179798836,) or db.execute('PRAGMA user_version').fetchone()[0] not in range(4, 13):
            raise ValueError('unsupported control database schema')


def disk_digest(api, path, target=None):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    output = None
    try:
        if target is not None: output = open(os.open(target, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600), 'wb')
        before = os.fstat(fd); _file(before, api.MAX_FILE)
        digest, total = hashlib.sha256(), 0
        deadline = time.monotonic() + 30
        while chunk := os.read(fd, 65536):
            total += len(chunk)
            if total > before.st_size or time.monotonic() > deadline: raise ValueError('database changed or exceeded time budget')
            digest.update(chunk)
            if output: output.write(chunk)
        if output: output.flush(); os.fsync(output.fileno())
        if total != before.st_size or not _same(before, os.fstat(fd)) or not _same(before, os.stat(path, follow_symlinks=False)):
            raise ValueError('database changed during capture')
        return (before.st_dev, before.st_ino, before.st_size, before.st_mtime_ns, digest.hexdigest())
    finally:
        if output: output.close()
        os.close(fd)


def database_sources(api, folder):
    values = {}
    for name in ('control.sqlite', 'control.sqlite-wal'):
        path = folder / name
        if path.exists() or path.is_symlink(): values[name] = disk_digest(api, path)
    if 'control.sqlite' not in values: raise ValueError('control database is missing')
    return values


def backup(api, folder, output, *, offline):
    if not offline: raise ValueError('complete backup requires explicit stopped-process acknowledgement')
    context = api.managed_files(folder)
    metadata = api.strict_json(context['installation.json'])
    if 'server' not in metadata['roles']:
        # Original generated non-managed Agents have no history or Store. Their
        # closed top-level file set remains representable by the legacy format.
        return api.backup(folder, output, offline=True)
    if metadata.get('managed') is not None: raise ValueError('combined managed roles are unsupported')
    if any(b'{{' in context[name] or b'}}' in context[name] for name in ('server.toml', 'agent.toml') if name in context):
        raise ValueError('dynamic configuration dependencies are unsupported')
    config = tomllib.loads(context['server.toml'].decode())
    if config.get('monitor', {}).get('databaseFile') != str(folder / 'control.sqlite'):
        raise ValueError('complete master backup requires the generated local database')
    history = config['monitor'].get('historyDataPath', '')
    before_root = folder.stat(); _directory(before_root)
    for name in context: _file((folder / name).stat(), api.MANAGED_MAX_FILE)
    context_stamps = {name: (folder / name).stat().st_mtime_ns for name in context}
    with history_lease(api, folder, history) as (history_path, history_fd, check_lease), tempfile.TemporaryDirectory(prefix='.frp-complete-', dir=output.parent) as temp:
        temporary = Path(temp); checkpoint = temporary / 'checkpoint'; checkpoint.mkdir(mode=0o700)
        for name in ('context', 'control', 'history'): (checkpoint / name).mkdir(mode=0o700)
        original_database = database_sources(api, folder)
        database = checkpoint / 'control/control.sqlite'
        # Copy the stopped DB and any committed WAL first. SQLite reads only
        # these private copies, so it cannot create/checkpoint source sidecars.
        raw_database = temporary / 'sqlite-source'; raw_database.mkdir(mode=0o700)
        for name, expected in original_database.items():
            if disk_digest(api, folder / name, raw_database / name) != expected:
                raise ValueError('stopped database changed during capture')
        api.database_snapshot(raw_database / 'control.sqlite', database); database_valid(api, database)
        values = []
        for name, data in sorted(context.items()):
            target = checkpoint / 'context' / name; api.private(target, data.decode())
            values.append({'path': 'context/' + name, 'size': len(data), 'mtime_ns': context_stamps[name], 'sha256': hashlib.sha256(data).hexdigest()})
        db_info = database.stat()
        values.append({'path': 'control/control.sqlite', 'size': db_info.st_size, 'mtime_ns': db_info.st_mtime_ns,
                       'sha256': disk_digest(api, database)[-1]})
        history_files, directories = tree(api, history_fd, checkpoint / 'history') if history_fd is not None else ([], [])
        values += history_files
        manifest = {'version': 1, 'kind': 'frp-offline-server-checkpoint', 'original_directory': str(folder),
                    'created_at_ms': time.time_ns() // 1_000_000, 'history_directory': str(history_path.relative_to(folder)) if history_path else '',
                    'history_exists': history_fd is not None, 'directories': directories, 'files': values,
                    'state': 'persisted_offline'}
        entries(api, manifest, folder)
        raw = json.dumps(manifest, sort_keys=True, separators=(',', ':')).encode()
        if len(raw) > api.MANAGED_MAX_FILE: raise ValueError('master manifest exceeds size budget')
        payload = [(entry['path'], checkpoint / entry['path']) for entry in values]
        def before_publish():
            check_lease()
            if (context != api.managed_files(folder) or context_stamps != {name: (folder / name).stat().st_mtime_ns for name in context}
                    or database_sources(api, folder) != original_database or not _same(before_root, folder.stat())):
                raise ValueError('master sources changed during stopped backup')
            if history_fd is not None and tree(api, history_fd) != (history_files, directories):
                raise ValueError('history changed during stopped backup')
            if history_path is not None and history_fd is None and history_path.exists():
                raise ValueError('history appeared during stopped backup')
        return write_archive(api, output, temporary, SERVER_KIND, folder, raw, payload, before_publish)


def entries(api, manifest, folder):
    keys = {'version', 'kind', 'original_directory', 'created_at_ms', 'history_directory', 'history_exists', 'directories', 'files', 'state'}
    if (not isinstance(manifest, dict) or set(manifest) != keys or manifest['version'] != 1
            or manifest['kind'] != 'frp-offline-server-checkpoint' or manifest['original_directory'] != str(folder)
            or type(manifest['created_at_ms']) is not int or manifest['created_at_ms'] <= 0
            or type(manifest['history_exists']) is not bool or manifest['state'] != 'persisted_offline'):
        raise ValueError('unsupported master checkpoint')
    history = manifest['history_directory']
    if not isinstance(history, str) or history and (not relative(history) or Path(api._history_directory(history, folder, check_files=False)).relative_to(folder).as_posix() != history):
        raise ValueError('invalid master history path')
    if manifest['history_exists'] and not history: raise ValueError('history presence mismatch')
    def allowed(path):
        return (path == 'control/control.sqlite' or path.startswith('context/') and path.removeprefix('context/') in api.FILES - {'control.sqlite'}
                or manifest['history_exists'] and path.startswith('history/') and relative(path[8:]) and path != 'history/flock.lock')
    values = validate_entries(api, manifest['files'], api.MAX_TOTAL, api.MAX_FILE, allowed)
    names = {v['path'] for v in values}
    if not {'context/server.toml', 'context/installation.json', 'control/control.sqlite'} <= names:
        raise ValueError('incomplete master checkpoint')
    if any(v['size'] > api.MANAGED_MAX_FILE for v in values if v['path'].startswith('context/')):
        raise ValueError('master configuration exceeds budget')
    dirs = manifest['directories']
    if (not isinstance(dirs, list) or len(dirs) > api.MANAGED_MAX_FILES or len(dirs) + len(values) > api.MANAGED_MAX_FILES
            or len(set(dirs)) != len(dirs) or any(not relative(d) for d in dirs)
            or dirs and not manifest['history_exists']):
        raise ValueError('invalid master directory inventory')
    for path in [v['path'][8:] for v in values if v['path'].startswith('history/')] + dirs:
        if any(str(parent) not in dirs for parent in Path(path).parents if str(parent) != '.'):
            raise ValueError('incomplete master directory inventory')
    if any('history/' + d in names for d in dirs): raise ValueError('master path collision')
    return values


def restore(api, checkpoint, manifest, folder):
    # No in-place overwrite: the stopped old installation must be moved aside
    # and retained by the maintenance workflow before this atomic publication.
    if folder.exists(): raise ValueError('move the stopped master aside before same-path restoration')
    values = entries(api, manifest, folder)
    def write(stage, final):
        history = manifest['history_directory']
        if manifest['history_exists']:
            target = stage / history; target.mkdir(mode=0o700, parents=True)
            for directory in manifest['directories']: (target / directory).mkdir(mode=0o700, parents=True, exist_ok=True)
        for entry in values:
            name = entry['path']
            target = stage / (history + '/' + name[8:] if name.startswith('history/') else 'control.sqlite' if name == 'control/control.sqlite' else name[8:])
            source = checkpoint / name
            target.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
            source_fd = os.open(source, os.O_RDONLY | os.O_NOFOLLOW)
            target_fd = os.open(target, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
            try:
                digest, size = hashlib.sha256(), 0
                while chunk := os.read(source_fd, 65536):
                    size += len(chunk); digest.update(chunk)
                    if size > entry['size']: raise ValueError('staged master payload changed')
                    with memoryview(chunk) as view:
                        while view:
                            written = os.write(target_fd, view); view = view[written:]
                if size != entry['size'] or digest.hexdigest() != entry['sha256']: raise ValueError('staged master payload changed')
                os.utime(target_fd, ns=(entry['mtime_ns'], entry['mtime_ns'])); os.fsync(target_fd)
            finally: os.close(source_fd); os.close(target_fd)
        config = api.managed_files(stage, final)
        metadata = api.strict_json(config['installation.json'])
        if 'server' not in metadata['roles'] or metadata.get('managed') is not None: raise ValueError('unsupported master profile')
        live = tomllib.loads(config['server.toml'].decode())
        expected_history = str(final / history) if history else ''
        if (live.get('monitor', {}).get('databaseFile') != str(final / 'control.sqlite')
                or api._history_directory(live['monitor'].get('historyDataPath', ''), final, check_files=False) != expected_history):
            raise ValueError('master dependency closure mismatch')
        database_valid(api, stage / 'control.sqlite')
        for top, _, _ in os.walk(stage, topdown=False): api.sync_directory(Path(top))
    result = api.new_directory(folder, write)
    api.sync_directory(folder.parent)
    return result
