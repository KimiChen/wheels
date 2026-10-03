"""Offline complete master backups preserve SQLite plus the entire stopped TSDB."""
from contextlib import closing
import fcntl
import hashlib
import io
import json
import os
from pathlib import Path
import sqlite3
import sys
import tarfile
import tempfile
import unittest
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / 'scripts'))
import local
import ops
import ops_checkpoint
import ops_history


class CompleteHistoryTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(); self.root = Path(self.temp.name).resolve(); self.root.chmod(0o700)
        self.folder = self.root / 'runtime'
        local.initialize(self.folder, plain_http=True, config=local.settings(self.root, {'FRP_MONITOR_HISTORY_DATA_PATH': 'history'}))
        self.history = self.folder / 'history'; self.history.mkdir(mode=0o700)
        for name in ('data', 'data/small', 'indexdb', 'cache', 'metadata', 'empty'):
            (self.history / name).mkdir(mode=0o700)
        self.payload = {'data/small/part': b'private-timeseries-data\x00', 'indexdb/index': b'private-index',
                        'cache/state': b'private-cache', 'metadata/state': b'private-metadata'}
        for name, data in self.payload.items(): local.private(self.history / name, data.decode())
        local.private(self.history / 'flock.lock', '')
        self.backups = self.root / 'backups'; self.backups.mkdir(mode=0o700)

    def tearDown(self): self.temp.cleanup()

    def backup(self, name='complete.tar.gz'):
        return ops.backup(self.folder, self.backups / name, complete=True, offline=True)

    def test_complete_same_path_atomic_restore_keeps_history_and_sqlite_schema_twelve(self):
        before = {p.relative_to(self.folder).as_posix(): (p.read_bytes(), p.stat().st_mtime_ns) for p in self.folder.rglob('*') if p.is_file()}
        archive = self.backup()
        after = {p.relative_to(self.folder).as_posix(): (p.read_bytes(), p.stat().st_mtime_ns) for p in self.folder.rglob('*') if p.is_file()}
        self.assertEqual(before, after, 'offline capture must not mutate the source')
        with self.assertRaisesRegex(ValueError, 'move'): ops.restore(archive, self.folder, offline=True)
        previous = self.root / 'kept-original'; self.folder.rename(previous)
        result = ops.restore(archive, self.folder, offline=True)
        self.assertEqual(result, self.folder)
        for name, data in self.payload.items():
            self.assertEqual((self.history / name).read_bytes(), data)
            self.assertEqual((self.history / name).stat().st_mtime_ns, before['history/' + name][1])
        self.assertTrue((self.history / 'empty').is_dir())
        self.assertFalse((self.history / 'flock.lock').exists())
        self.assertTrue((previous / 'history/flock.lock').exists())
        for name in ('server.toml', 'agent.toml', 'agent.token', 'frp.token'):
            self.assertEqual((self.folder / name).read_bytes(), before[name][0])
            self.assertEqual((self.folder / name).stat().st_mtime_ns, before[name][1])
        with closing(sqlite3.connect(self.folder / 'control.sqlite')) as db:
            self.assertEqual(db.execute('PRAGMA user_version').fetchone(), (12,))
            self.assertEqual(db.execute('PRAGMA integrity_check').fetchone(), ('ok',))
            self.assertEqual(db.execute('SELECT count(*) FROM config_audit_retention').fetchone(), (1,))

    def test_wal_commits_are_snapshotted_without_touching_source(self):
        connection = sqlite3.connect(self.folder / 'control.sqlite')
        try:
            connection.execute('PRAGMA journal_mode=wal'); connection.execute('PRAGMA wal_autocheckpoint=0')
            connection.execute('CREATE TABLE backup_probe(value TEXT)'); connection.execute("INSERT INTO backup_probe VALUES('durable')"); connection.commit()
            original = ops_history.database_sources(ops, self.folder)
            archive = self.backup('wal.tar.gz')
            self.assertEqual(ops_history.database_sources(ops, self.folder), original)
        finally: connection.close()
        self.folder.rename(self.root / 'old-wal')
        ops.restore(archive, self.folder, offline=True)
        with closing(sqlite3.connect(self.folder / 'control.sqlite')) as db:
            self.assertEqual(db.execute('SELECT value FROM backup_probe').fetchall(), [('durable',)])

    def test_explicit_stop_and_existing_vm_lock_enforced(self):
        with self.assertRaises(ValueError): ops.backup(self.folder, self.backups / 'not-offline.tar.gz', complete=True)
        with (self.history / 'flock.lock').open('rb') as lock:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            with self.assertRaisesRegex(ValueError, 'still in use'): self.backup('locked.tar.gz')
        self.assertFalse((self.backups / 'locked.tar.gz').exists())

    def test_source_drift_symlink_hardlink_and_budgets_fail_without_output(self):
        def mutate(*args, **kwargs):
            result = original(*args, **kwargs)
            (self.history / 'cache/state').write_text('outside-maintenance-change')
            return result
        original = ops_history.tree
        with mock.patch.object(ops_history, 'tree', side_effect=mutate):
            with self.assertRaises(ValueError): self.backup('drift.tar.gz')
        self.assertFalse((self.backups / 'drift.tar.gz').exists())
        for kind in ('symlink', 'hardlink', 'budget'):
            target = self.history / 'unsafe'
            if kind == 'symlink': target.symlink_to(self.folder / 'agent.token')
            if kind == 'hardlink': os.link(self.folder / 'agent.token', target)
            with self.subTest(kind=kind), mock.patch.object(ops, 'MAX_TOTAL', 1 if kind == 'budget' else ops.MAX_TOTAL):
                with self.assertRaises((ValueError, OSError)): self.backup(kind + '.tar.gz')
            if target.exists() or target.is_symlink(): target.unlink()
            self.assertFalse((self.backups / (kind + '.tar.gz')).exists())

    def test_empty_history_and_disabled_history_have_explicit_presence(self):
        for name in self.payload: (self.history / name).unlink()
        archive = self.backup('empty.tar.gz')
        self.folder.rename(self.root / 'empty-old')
        ops.restore(archive, self.folder, offline=True)
        self.assertTrue((self.history / 'empty').is_dir())
        self.assertEqual([p for p in self.history.rglob('*') if p.is_file()], [])
        # Before native startup there is no lock: copying a configured existing
        # tree without its lifecycle lock must not silently claim offline safety.
        with self.assertRaises(OSError): self.backup('no-lock.tar.gz')
        server = self.folder / 'server.toml'
        server.write_text(server.read_text().replace('historyDataPath = ' + json.dumps(str(self.history)), 'historyDataPath = ""'))
        archive = self.backup('disabled.tar.gz')
        self.folder.rename(self.root / 'disabled-old')
        ops.restore(archive, self.folder, offline=True)
        self.assertFalse(self.history.exists())

    def test_lock_replacement_detected_before_archive_publication(self):
        original = ops_history.tree
        def replace_lock(*args, **kwargs):
            result = original(*args, **kwargs)
            lock = self.history / 'flock.lock'
            lock.rename(self.history / 'old-lock')
            local.private(lock, '')
            return result
        with mock.patch.object(ops_history, 'tree', side_effect=replace_lock):
            with self.assertRaises(ValueError): self.backup('replaced-lock.tar.gz')
        self.assertFalse((self.backups / 'replaced-lock.tar.gz').exists())

    def test_rehashed_archive_missing_referenced_token_is_rejected_without_publication(self):
        archive = self.backup()
        with tarfile.open(archive, 'r:gz') as source:
            members = [(member.name, source.extractfile(member).read()) for member in source]
        envelope, manifest = json.loads(members[0][1]), json.loads(members[1][1])
        omitted = next(i for i, value in enumerate(manifest['files']) if value['path'] == 'context/frp.token')
        del manifest['files'][omitted]
        payload = [data for _, data in members[2:]]; del payload[omitted]
        raw = json.dumps(manifest).encode()
        envelope['manifest_sha256'] = hashlib.sha256(raw).hexdigest()
        envelope['payload_count'] = len(payload) + 1
        damaged = self.backups / 'missing-reference.tar.gz'
        with tarfile.open(damaged, 'w:gz', format=tarfile.USTAR_FORMAT) as output:
            for name, data in [('BACKUP.json', json.dumps(envelope).encode())] + [(f'payload/{i:06d}', value) for i, value in enumerate([raw] + payload)]:
                member = tarfile.TarInfo(name); member.mode = 0o600; member.size = len(data)
                output.addfile(member, io.BytesIO(data))
        damaged.chmod(0o600)
        self.folder.rename(self.root / 'retained-original')
        with self.assertRaisesRegex(ValueError, 'referenced private file is missing'):
            ops.restore(damaged, self.folder, offline=True)
        self.assertFalse(self.folder.exists())

    def test_old_unmanaged_agent_complete_uses_format_two_without_helper(self):
        (self.folder / 'installation.json').write_text(local.installation_metadata(['agent']))
        (self.folder / 'server.toml').unlink(); (self.folder / 'control.sqlite').unlink()
        before = ops.managed_files(self.folder)
        with mock.patch.object(ops, 'maintenance') as helper:
            archive = self.backup('old-agent.tar.gz')
            with archive.open('rb') as source:
                self.assertEqual(ops.archive_format(source.fileno()), 2)
            self.folder.rename(self.root / 'old-agent-original')
            ops.restore(archive, self.folder, offline=True)
            helper.assert_not_called()
        self.assertEqual(ops.managed_files(self.folder), before)
        self.assertFalse((self.folder / 'managed').exists())

    def test_same_path_master_restore_accepts_safe_0755_parent_and_keeps_private_target(self):
        archive = self.backup('public-parent.tar.gz')
        self.folder.rename(self.root / 'retained-parent-layout')
        self.root.chmod(0o755)
        try:
            ops.restore(archive, self.folder, offline=True)
            self.assertEqual(self.folder.stat().st_mode & 0o777, 0o700)
            self.assertTrue(all(p.stat().st_mode & 0o777 == (0o700 if p.is_dir() else 0o600) for p in self.folder.rglob('*')))
            self.assertEqual((self.history / 'data/small/part').read_bytes(), self.payload['data/small/part'])
        finally:
            self.root.chmod(0o700)

    def test_unsafe_restore_parent_rejected_before_extraction(self):
        archive = self.backup('unsafe-parent.tar.gz')
        self.folder.rename(self.root / 'retained-unsafe-layout')
        for mode in (0o770, 0o777):
            self.root.chmod(mode)
            with self.subTest(mode=mode), self.assertRaisesRegex(ValueError, 'trusted owner'):
                ops.restore(archive, self.folder, offline=True)
            self.assertFalse(self.folder.exists())
        self.root.chmod(0o700)
        with mock.patch.object(ops_checkpoint, 'trusted_restore_parent', return_value=False):
            with self.assertRaisesRegex(ValueError, 'trusted owner'):
                ops.restore(archive, self.folder, offline=True)
        self.assertFalse(self.folder.exists())
