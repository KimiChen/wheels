"""Indexed format4 packaging; native code remains the managed Store installer."""
from __future__ import annotations

import hashlib
import io
import json
import os
from pathlib import Path, PurePosixPath
import re
import stat
import tarfile
import tempfile
import time

INDEX = re.compile(r'payload/[0-9]{6}')
AGENT_KIND = 'frp-managed-agent'
SERVER_KIND = 'frp-offline-server'


def relative(name):
    return (isinstance(name, str) and bool(name) and len(name.encode()) <= 4096
            and not name.startswith('/') and str(PurePosixPath(name)) == name
            and '\\' not in name and not any(ord(c) < 32 for c in name)
            and len(name.split('/')) <= 32
            and all(p not in ('', '.', '..') and not p.startswith('.txn-') and len(p.encode()) <= 255
                    for p in name.split('/')))


def checkpoint_path(api, name):
    if name == 'GRAPH.json' or name == 'managed/restore-install.json':
        return True
    if isinstance(name, str) and name.startswith('context/'):
        value = name.removeprefix('context/')
        return relative(value) and value != 'managed' and not value.startswith('managed/')
    return api.checkpoint_path(name)


def entries(api, manifest, folder):
    required = {'version', 'kind', 'id', 'created_at_ms', 'root', 'store_name', 'config_file', 'cwd',
                'context_revision', 'service_id', 'store_exists', 'store_digest', 'files', 'graph_sha256'}
    if (not isinstance(manifest, dict) or set(manifest) != required or manifest['version'] != 2
            or manifest['kind'] != 'frp-managed-checkpoint' or manifest['root'] != str(folder / 'managed')
            or manifest['store_name'] != 'store.json' or manifest['config_file'] != str(folder / 'agent.toml')
            or manifest['cwd'] != str(folder) or type(manifest['created_at_ms']) is not int
            or manifest['created_at_ms'] <= 0 or type(manifest['store_exists']) is not bool):
        raise ValueError('unsupported dependency checkpoint identity')
    for key in ('id', 'service_id'):
        if not isinstance(manifest[key], str) or not api.UUID.fullmatch(manifest[key]):
            raise ValueError('invalid dependency checkpoint identity')
    for key in ('context_revision', 'store_digest', 'graph_sha256'):
        if not isinstance(manifest[key], str) or not api.HEX_DIGEST.fullmatch(manifest[key]):
            raise ValueError('invalid dependency checkpoint digest')
    result = validate_entries(api, manifest['files'], api.MANAGED_MAX_BYTES, api.MANAGED_MAX_FILE,
                              lambda name: checkpoint_path(api, name))
    names = {entry['path'] for entry in result}
    if (not {'GRAPH.json', 'context/agent.toml', 'context/installation.json', 'managed/identity.json'} <= names
            or ('managed/store.json' in names) != manifest['store_exists']):
        raise ValueError('incomplete dependency checkpoint')
    return result


def validate_entries(api, values, maximum, maximum_file, allowed):
    if not isinstance(values, list) or not 1 <= len(values) <= api.MANAGED_MAX_FILES:
        raise ValueError('invalid checkpoint inventory')
    seen, total = set(), 0
    for value in values:
        if (not isinstance(value, dict) or set(value) != {'path', 'size', 'sha256', 'mtime_ns'}
                or not relative(value['path']) or not allowed(value['path']) or value['path'] in seen
                or type(value['size']) is not int or not 0 <= value['size'] <= maximum_file
                or type(value['mtime_ns']) is not int or not 0 < value['mtime_ns'] < 2**63
                or not isinstance(value['sha256'], str) or not api.HEX_DIGEST.fullmatch(value['sha256'])):
            raise ValueError('invalid checkpoint file')
        total += value['size']
        if total > maximum:
            raise ValueError('checkpoint exceeds byte budget')
        seen.add(value['path'])
    return values


def private_bytes(api, path, maximum):
    data = api.read_private(path, maximum)
    info = path.stat()
    if info.st_nlink != 1 or info.st_uid != os.geteuid():
        raise ValueError('checkpoint file is not private')
    return data


def directory(api, checkpoint, folder):
    api.private_directory(checkpoint)
    raw = private_bytes(api, checkpoint / 'CHECKPOINT.json', api.MANAGED_MAX_FILE)
    manifest = api.strict_json(raw)
    values = entries(api, manifest, folder)
    payload = [('CHECKPOINT.json', raw)]
    expected = {'CHECKPOINT.json'}
    parents = set()
    for entry in values:
        name = entry['path']
        data = private_bytes(api, checkpoint / name, api.MANAGED_MAX_FILE)
        if len(data) != entry['size'] or hashlib.sha256(data).hexdigest() != entry['sha256']:
            raise ValueError('checkpoint payload mismatch')
        if name == 'GRAPH.json' and hashlib.sha256(data).hexdigest() != manifest['graph_sha256']:
            raise ValueError('dependency graph mismatch')
        payload.append((name, data)); expected.add(name)
        parents.update(str(p) for p in PurePosixPath(name).parents if str(p) != '.')
    actual = set()
    for top, dirs, files in os.walk(checkpoint, followlinks=False):
        for name in dirs:
            path = Path(top) / name
            if path.relative_to(checkpoint).as_posix() not in parents:
                raise ValueError('unexpected checkpoint directory')
            api.private_directory(path)
            if path.stat().st_uid != os.geteuid():
                raise ValueError('checkpoint owner mismatch')
        actual.update((Path(top) / name).relative_to(checkpoint).as_posix() for name in files)
        if len(actual) > api.MANAGED_MAX_FILES + 1:
            raise ValueError('checkpoint exceeds file budget')
    if actual != expected:
        raise ValueError('checkpoint inventory mismatch')
    metadata = api.strict_json(dict(payload)['context/installation.json'])
    if metadata != {'format': 2, 'roles': ['agent'], 'managed': {'version': 1, 'root': 'managed', 'store': 'store.json'}}:
        raise ValueError('unsupported dependency checkpoint profile')
    return manifest, payload, hashlib.sha256(raw).hexdigest()


def write_archive(api, output, temporary, kind, folder, manifest, payload, before_publish=None):
    envelope = {'format': 4, 'kind': kind, 'original_directory': str(folder),
                'manifest_sha256': hashlib.sha256(manifest).hexdigest(), 'created_at_ms': time.time_ns() // 1_000_000,
                'payload_count': len(payload) + 1}
    staged = temporary / 'backup.tar.gz'
    api.private(staged, '')
    with tarfile.open(staged, 'w:gz', format=tarfile.USTAR_FORMAT) as archive:
        header = json.dumps(envelope, sort_keys=True, separators=(',', ':')).encode()
        def add(name, data):
            info = tarfile.TarInfo(name); info.mode = 0o600
            if isinstance(data, Path):
                info.size = data.stat().st_size
                with data.open('rb') as source: archive.addfile(info, source)
            else:
                info.size = len(data); archive.addfile(info, io.BytesIO(data))
        add('BACKUP.json', header)
        add('payload/000000', manifest)
        for index, (_, data) in enumerate(payload, 1): add(f'payload/{index:06d}', data)
    api.sync_file(staged)
    if before_publish is not None: before_publish()
    # O_EXCL publication; no overwrite of an existing completed archive.
    os.link(staged, output); api.sync_directory(output.parent)
    return output


def backup(api, folder, output, *, offline, agent_binary):
    if not offline:
        raise ValueError('dependency checkpoint requires stopped Agent acknowledgement')
    with tempfile.TemporaryDirectory(prefix='.frp-checkpoint-', dir=output.parent) as temporary:
        temporary = Path(temporary); checkpoint = temporary / 'checkpoint'
        result = api.maintenance(['checkpoint', '--config', str(folder / 'agent.toml'), '--output', str(checkpoint),
                                  '--dependency-graph'], folder, agent_binary)
        manifest, payload, digest = directory(api, checkpoint, folder)
        if (result.get('manifest_digest') != digest or result.get('checkpoint_id') != manifest['id']
                or result.get('file_count') != len(manifest['files'])
                or result.get('total_bytes') != sum(value['size'] for value in manifest['files'])):
            raise ValueError('native checkpoint response does not match durable files')
        return write_archive(api, output, temporary, AGENT_KIND, folder, payload[0][1], payload[1:])


def extract(api, fd, folder, destination, resolve):
    """Read strict sequential numbered payloads; source names never enter tar headers."""
    with api.read_archive(fd, api.MAX_TOTAL + 2 * api.MANAGED_MAX_FILE) as archive:
        def next_member(name, limit):
            member = archive.next()
            if (member is None or member.name != name or not member.isfile() or member.mode != 0o600
                    or not 0 <= member.size <= limit):
                raise ValueError('invalid indexed backup inventory')
            return member
        first = next_member('BACKUP.json', api.MANAGED_MAX_FILE)
        with archive.extractfile(first) as source: envelope = api.strict_json(source.read(api.MANAGED_MAX_FILE + 1))
        if (not isinstance(envelope, dict) or set(envelope) != {'format', 'kind', 'original_directory', 'manifest_sha256', 'created_at_ms', 'payload_count'}
                or envelope['format'] != 4 or envelope['kind'] not in (AGENT_KIND, SERVER_KIND)
                or envelope['original_directory'] != str(folder) or type(envelope['created_at_ms']) is not int
                or envelope['created_at_ms'] <= 0 or type(envelope['payload_count']) is not int
                or not 2 <= envelope['payload_count'] <= api.MANAGED_MAX_FILES + 1
                or not isinstance(envelope['manifest_sha256'], str) or not api.HEX_DIGEST.fullmatch(envelope['manifest_sha256'])):
            raise ValueError('indexed restore requires the original installation directory')
        first = next_member('payload/000000', api.MANAGED_MAX_FILE)
        with archive.extractfile(first) as source: raw = source.read(api.MANAGED_MAX_FILE + 1)
        if hashlib.sha256(raw).hexdigest() != envelope['manifest_sha256']:
            raise ValueError('indexed manifest mismatch')
        manifest = api.strict_json(raw)
        name, values = resolve(envelope['kind'], manifest)
        if len(values) + 1 != envelope['payload_count']:
            raise ValueError('indexed payload count mismatch')
        api.private(destination / name, raw.decode('utf-8'))
        for index, entry in enumerate(values, 1):
            member = next_member(f'payload/{index:06d}', entry['size'])
            if member.size != entry['size']:
                raise ValueError('indexed payload size mismatch')
            target = destination / entry['path']
            cursor = destination
            for part in PurePosixPath(entry['path']).parts[:-1]:
                cursor /= part; cursor.mkdir(mode=0o700, exist_ok=True)
            digest = hashlib.sha256()
            output_fd = os.open(target, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
            with os.fdopen(output_fd, 'wb') as output, archive.extractfile(member) as source:
                while chunk := source.read(65536): digest.update(chunk); output.write(chunk)
                output.flush(); os.fsync(output.fileno())
            if digest.hexdigest() != entry['sha256']:
                raise ValueError('indexed payload checksum mismatch')
        if archive.next() is not None:
            raise ValueError('unexpected indexed payload')
    return envelope, manifest


def restore(api, fd, folder, *, offline, agent_binary):
    if not offline:
        raise ValueError('indexed restoration requires offline acknowledgement')
    folder = api.path_without_links(folder); api.private_directory(folder.parent)
    with tempfile.TemporaryDirectory(prefix='.frp-indexed-', dir=folder.parent) as temporary:
        checkpoint = Path(temporary)
        def resolve(kind, manifest):
            if kind == AGENT_KIND:
                api.private_directory(folder)
                return 'CHECKPOINT.json', entries(api, manifest, folder)
            from ops_history import entries as server_entries
            return 'SERVER.json', server_entries(api, manifest, folder)
        envelope, manifest = extract(api, fd, folder, checkpoint, resolve)
        if envelope['kind'] == SERVER_KIND:
            from ops_history import restore as server_restore
            return server_restore(api, checkpoint, manifest, folder)
        _, _, digest = directory(api, checkpoint, folder)
        if digest != envelope['manifest_sha256']:
            raise ValueError('checkpoint changed after indexed extraction')
        result = api.maintenance(['restore-install', '--checkpoint', str(checkpoint), '--root', str(folder / 'managed'),
                                  '--manifest-digest', digest], folder, agent_binary)
        return api.restore_result(result, 'pending', digest)
