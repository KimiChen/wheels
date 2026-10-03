"""Format4 uses closed, numbered USTAR payloads for native dependency snapshots."""
import hashlib
import io
import json
import os
from pathlib import Path
import sys
import tarfile
import tempfile
import unittest
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / 'scripts'))
import local
import ops
import ops_checkpoint

ID = '12345678-1234-4234-8234-123456789abc'


class IndexedCheckpointTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(); self.root = Path(self.temp.name).resolve(); self.root.chmod(0o700)
        self.folder = self.root / 'agent'; self.folder.mkdir(mode=0o700)
        (self.folder / 'managed').mkdir(mode=0o700)
        self.backups = self.root / 'backups'; self.backups.mkdir(mode=0o700)
        local.private(self.folder / 'installation.json', local.installation_metadata(['agent'], True))
        local.private(self.folder / 'agent.toml', '# synthetic private configuration\n')
        self.long = 'context/' + '/'.join(('中文' * 30, 'nested' * 20, 'tls.key'))
        self.payload = {'context/installation.json': (self.folder / 'installation.json').read_bytes(),
                        'context/agent.toml': b'# synthetic private configuration\n', self.long: b'private-key-fixture',
                        'GRAPH.json': b'{}', 'managed/store.json': b'{"proxies":[],"visitors":[]}',
                        'managed/identity.json': b'{}'}
        self.calls = []

    def tearDown(self): self.temp.cleanup()

    def helper(self, args, folder, binary):
        self.calls.append(args)
        if args[0] == 'checkpoint':
            self.assertIn('--dependency-graph', args)
            out = Path(args[args.index('--output') + 1]); out.mkdir(mode=0o700)
            entries = []
            for name, data in sorted(self.payload.items()):
                current = out
                for part in Path(name).parts[:-1]: current /= part; current.mkdir(mode=0o700, exist_ok=True)
                local.private(out / name, data.decode())
                entries.append({'path': name, 'size': len(data), 'sha256': hashlib.sha256(data).hexdigest(), 'mtime_ns': 1700000000123456789})
            manifest = {'version': 2, 'kind': 'frp-managed-checkpoint', 'id': ID, 'created_at_ms': 1700000000000,
                        'root': str(folder / 'managed'), 'store_name': 'store.json', 'config_file': str(folder / 'agent.toml'),
                        'cwd': str(folder), 'context_revision': 'a'*64, 'service_id': ID, 'store_exists': True,
                        'store_digest': 'b'*64, 'graph_sha256': hashlib.sha256(b'{}').hexdigest(), 'files': entries}
            raw = json.dumps(manifest).encode(); local.private(out / 'CHECKPOINT.json', raw.decode())
            return {'code': 'ok', 'checkpoint_id': ID, 'manifest_digest': hashlib.sha256(raw).hexdigest(),
                    'file_count': len(entries), 'total_bytes': sum(e['size'] for e in entries)}
        out = Path(args[args.index('--checkpoint') + 1])
        self.assertEqual((out / self.long).read_bytes(), self.payload[self.long])
        self.assertEqual(set(p.relative_to(out).as_posix() for p in out.rglob('*') if p.is_file()), set(self.payload) | {'CHECKPOINT.json'})
        return {'code': 'ok', 'state': 'pending', 'epoch': ID, 'service_id': ID,
                'manifest_digest': args[args.index('--manifest-digest') + 1]}

    def backup(self):
        with mock.patch.object(ops, 'maintenance', side_effect=self.helper):
            return ops.backup(self.folder, self.backups / 'snapshot.tar.gz', offline=True, dependency_graph=True)

    def members(self, archive):
        with tarfile.open(archive, 'r:gz') as source:
            return [(member.name, source.extractfile(member).read()) for member in source]

    def test_indexed_archive_preserves_long_unicode_paths_and_native_install_boundary(self):
        archive = self.backup(); members = self.members(archive)
        self.assertEqual([n for n, _ in members], ['BACKUP.json'] + [f'payload/{i:06d}' for i in range(len(self.payload)+1)])
        self.assertEqual(json.loads(members[0][1])['format'], 4)
        before = (self.folder / 'agent.toml').read_bytes()
        with mock.patch.object(ops, 'maintenance', side_effect=self.helper):
            result = ops.restore(archive, self.folder, offline=True)
        self.assertEqual(result['state'], 'pending'); self.assertEqual((self.folder / 'agent.toml').read_bytes(), before)
        self.assertEqual([x[0] for x in self.calls], ['checkpoint', 'restore-install'])

    def test_offline_and_original_path_are_mandatory(self):
        with mock.patch.object(ops, 'maintenance') as helper:
            with self.assertRaises(ValueError): ops.backup(self.folder, self.backups / 'bad.tar.gz', dependency_graph=True)
            helper.assert_not_called()
        archive = self.backup()
        with mock.patch.object(ops, 'maintenance') as helper:
            for folder, offline in ((self.folder, False), (self.root / 'other', True)):
                with self.assertRaises(ValueError): ops.restore(archive, folder, offline=offline)
            helper.assert_not_called()

    def test_malformed_numbering_checksum_paths_and_extensions_rejected_before_native(self):
        archive = self.backup(); original = self.members(archive)
        for kind in ('duplicate', 'reorder', 'extra', 'tamper', 'pax', 'traversal'):
            members = list(original)
            if kind == 'duplicate': members.append(members[-1])
            if kind == 'reorder': members[2], members[3] = members[3], members[2]
            if kind == 'extra': members.append(('payload/999999', b'x'))
            if kind == 'tamper': members[-1] = (members[-1][0], b'x' * len(members[-1][1]))
            if kind == 'traversal':
                manifest = json.loads(members[1][1]); manifest['files'][0]['path'] = 'context/../../escape'
                raw = json.dumps(manifest).encode(); envelope = json.loads(members[0][1]); envelope['manifest_sha256'] = hashlib.sha256(raw).hexdigest()
                members[:2] = [('BACKUP.json', json.dumps(envelope).encode()), ('payload/000000', raw)]
            path = self.backups / (kind + '.tar.gz')
            with tarfile.open(path, 'w:gz', format=tarfile.PAX_FORMAT if kind == 'pax' else tarfile.USTAR_FORMAT) as out:
                for index, (name, data) in enumerate(members):
                    info = tarfile.TarInfo(name); info.mode = 0o600; info.size = len(data)
                    if kind == 'pax' and index == 0: info.pax_headers = {'comment': 'x' * (1024 * 1024)}
                    out.addfile(info, io.BytesIO(data))
            path.chmod(0o600)
            with self.subTest(kind=kind), mock.patch.object(ops, 'maintenance') as helper:
                with self.assertRaises((ValueError, OSError)): ops.restore(path, self.folder, offline=True)
                helper.assert_not_called()
        self.assertFalse((self.root / 'escape').exists())

    def test_limits_symlinks_and_incomplete_closure_never_publish(self):
        for kind in ('budget', 'missing', 'link'):
            def helper(args, folder, binary):
                result = self.helper(args, folder, binary); out = Path(args[args.index('--output') + 1])
                if kind == 'missing': (out / self.long).unlink()
                if kind == 'link':
                    (out / self.long).unlink(); (out / self.long).symlink_to(self.folder / 'agent.toml')
                return result
            destination = self.backups / (kind + '.tar.gz')
            with mock.patch.object(ops, 'maintenance', side_effect=helper), mock.patch.object(ops, 'MANAGED_MAX_BYTES', 1 if kind == 'budget' else ops.MANAGED_MAX_BYTES):
                with self.assertRaises((ValueError, OSError)): ops.backup(self.folder, destination, offline=True, dependency_graph=True)
            self.assertFalse(destination.exists())
