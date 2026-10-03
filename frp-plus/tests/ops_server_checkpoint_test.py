"""Explicit server wrapper safety; mocked helper covers the Python IO boundary."""
from contextlib import closing
import copy
import fcntl
import hashlib
import io
import json
import os
from pathlib import Path
import shutil
import sqlite3
import sys
import tarfile
import tempfile
import unittest
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / 'scripts'))
import local
import ops
import ops_server_checkpoint as server


class ServerCheckpointTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(); self.root = Path(self.temp.name).resolve(); self.root.chmod(0o700)
        self.source = self.root / 'source'; self.source.mkdir(mode=0o700)
        self.target = self.root / 'target'
        self.data = self.root / 'source-data'; self.data.mkdir(mode=0o700)
        self.next_data = self.root / 'target-data'
        self.certs = self.root / 'source-certs'; self.certs.mkdir(mode=0o700)
        self.next_certs = self.root / 'target-certs'; self.next_certs.mkdir(mode=0o700)
        local.private(self.certs / 'ca.pem', 'synthetic private CA')
        local.private(self.source / 'token', 'synthetic token')
        local.private(self.source / 'installation.json', local.installation_metadata(['server']))
        self.database = self.data / 'control.sqlite'; local.control_database(self.database)
        self.history = self.data / 'history'; self.history.mkdir(mode=0o700)
        (self.history / 'data').mkdir(mode=0o700); (self.history / 'empty').mkdir(mode=0o700)
        local.private(self.history / 'flock.lock', ''); local.private(self.history / 'data/samples', 'synthetic persisted series')
        self.config = {'database': str(self.database), 'history': str(self.history),
            'dependencies': [str(self.source / 'token'), str(self.certs / 'ca.pem')], 'template': '{{ .Envs.TOKEN }}'}
        local.private(self.source / 'server.toml', json.dumps(self.config))
        self.source_policy = self.root / 'source-policy.json'; self.target_policy = self.root / 'target-policy.json'
        self.env = self.root / 'local-environment.json'; local.private(self.env, '{"TOKEN":"unarchived-template-secret"}')
        for folder, data, certs, output in ((self.source, self.data, self.certs, self.source_policy), (self.target, self.next_data, self.next_certs, self.target_policy)):
            local.private(output, json.dumps({'version': 1, 'config_file': str(folder / 'server.toml'), 'working_dir': str(folder),
                'roots': [{'id': 'install', 'path': str(folder)}, {'id': 'data', 'path': str(data)}],
                'files': [{'id': 'ca', 'path': str(certs / 'ca.pem')}]}))
        self.archive = self.root / 'backup.tar.gz'
        self.helper_calls = []
        self.helper_patch = mock.patch.object(server, 'maintenance', side_effect=self.helper); self.helper_patch.start()
        self.addCleanup(self.helper_patch.stop)

    def tearDown(self): self.temp.cleanup()

    def emit(self, policy, output, files, database, history):
        output.mkdir(mode=0o700); (output / 'context').mkdir(mode=0o700)
        policy_raw = server.encoded(policy); local.private(output / 'POLICY.json', policy_raw.decode())
        entries = []
        for index, (path, data, stamp) in enumerate(sorted(files), 1):
            payload = f'context/f{index:06d}'; server._write_file(ops, output / payload, data)
            entries.append({'path': path, 'payload': payload, 'size': len(data), 'sha256': server.sha(data), 'mtime_ns': stamp})
        context = {'kind': 'frp-server-context', 'version': 1, 'config_file': policy['config_file'], 'cwd': policy['working_dir'],
            'policy_digest': server.sha(policy_raw), 'database_file': database, 'history_path': history, 'history_enabled': bool(history),
            'log_path': '', 'template_requirements': [], 'files': entries}
        context['context_revision'] = server.sha(server.encoded(context))
        raw = server.encoded(context); local.private(output / 'CONTEXT.json', raw.decode())
        return {'code': 'ok', 'version': 1, 'manifest_digest': server.sha(raw), 'context_revision': context['context_revision'],
            'file_count': len(entries), 'total_bytes': sum(entry['size'] for entry in entries)}

    def helper(self, api, args, folder, binary):
        self.helper_calls.append(args)
        get = lambda flag: args[args.index(flag) + 1]
        policy = json.loads(Path(get('--policy')).read_text()); output = Path(get('--output'))
        if args[0] == 'capture':
            path = Path(policy['config_file']); config = json.loads(path.read_text())
            paths = [path, *(Path(value) for value in config['dependencies'])]
            files = [(str(value), value.read_bytes(), value.stat().st_mtime_ns) for value in paths]
            return self.emit(policy, output, files, config['database'], config['history'])
        source = Path(get('--checkpoint')); raw = (source / 'CONTEXT.json').read_bytes()
        self.assertEqual(server.sha(raw), get('--manifest-digest'))
        original = json.loads(raw); source_policy = json.loads(Path(get('--source-policy')).read_text())
        self.assertEqual(json.loads((source / 'POLICY.json').read_text()), source_policy)
        pairs = []
        for kind in ('roots', 'files'):
            left = {value['id']: value['path'] for value in source_policy[kind]}; right = {value['id']: value['path'] for value in policy[kind]}
            if set(left) != set(right): raise ValueError('synthetic authorization mismatch')
            pairs += [(value, right[key]) for key, value in left.items()]
        def mapped(value):
            for old, new in pairs:
                if value == old or value.startswith(old + '/'): return new + value[len(old):]
            raise ValueError('synthetic unmapped source')
        files = []
        for entry in original['files']:
            data = (source / entry['payload']).read_bytes()
            if entry['path'] == original['config_file']:
                for old, new in pairs: data = data.replace(old.encode(), new.encode())
            files.append((mapped(entry['path']), data, entry['mtime_ns']))
        return self.emit(policy, output, files, mapped(original['database_file']), mapped(original['history_path']) if original['history_path'] else '')

    def backup(self):
        return ops.backup(self.source, self.archive, offline=True, complete=True, server_binary='synthetic-helper', policy=self.source_policy, env_file=self.env)

    def restore(self, *, hook=None):
        original = server.restore
        def call(*args, **kwargs): return original(*args, **kwargs, hook=hook)
        with mock.patch.object(server, 'restore', side_effect=call):
            return ops.restore(self.archive, self.target, offline=True, server_binary='synthetic-helper', policy=self.target_policy,
                source_policy=self.source_policy, env_file=self.env, source_env_file=self.env)

    def test_backup_restore_external_data_sealed_source_and_gate_before_database(self):
        before = server.database_sources(ops, self.database); self.backup()
        self.assertEqual(before, server.database_sources(ops, self.database))
        with tarfile.open(self.archive, 'r:gz') as archive:
            values = [archive.extractfile(entry).read() for entry in archive]
        self.assertNotIn(b'unarchived-template-secret', b'\n'.join(values))
        shutil.rmtree(self.source); shutil.rmtree(self.data); shutil.rmtree(self.certs)
        def hook(stage):
            if stage == 'target:database': self.assertTrue(Path(str(self.next_data / 'control.sqlite') + '.restore-gate.json').exists())
        result = self.restore(hook=hook)
        self.assertEqual(result['state'], 'installed_gated'); self.assertTrue(result['restart_required'])
        self.assertTrue((self.next_data / 'history/empty').is_dir())
        self.assertEqual((self.next_data / 'history/data/samples').read_text(), 'synthetic persisted series')
        self.assertFalse((self.next_data / 'history/flock.lock').exists())
        with closing(sqlite3.connect(self.next_data / 'control.sqlite')) as db:
            self.assertEqual(db.execute('PRAGMA user_version').fetchone(), (12,))
        gate = json.loads(Path(str(self.next_data / 'control.sqlite') + '.restore-gate.json').read_text())
        self.assertEqual(gate['checkpoint_id'], result['checkpoint_id'])
        self.assertEqual(gate['kind'], 'frp-server-restore-gate')
        self.assertNotIn(str(self.root), json.dumps(result))
        self.assertEqual(self.restore(), result)

    def test_wal_commits_preserved_without_touching_source(self):
        with closing(sqlite3.connect(self.database)) as db:
            db.execute('PRAGMA journal_mode=WAL'); db.execute('PRAGMA wal_autocheckpoint=0')
            db.execute('CREATE TABLE fixture(value TEXT)'); db.execute("INSERT INTO fixture VALUES('committed')"); db.commit()
            before = server.database_sources(ops, self.database); self.backup()
            self.assertEqual(before, server.database_sources(ops, self.database))
        self.restore()
        with closing(sqlite3.connect(self.next_data / 'control.sqlite')) as db:
            self.assertEqual(db.execute('SELECT value FROM fixture').fetchall(), [('committed',)])

    def test_running_history_missing_offline_and_missing_authority_rejected(self):
        with (self.history / 'flock.lock').open('rb') as lock:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            with self.assertRaisesRegex(ValueError, 'still in use'): self.backup()
        self.assertFalse(self.archive.exists())
        with self.assertRaises(ValueError): ops.backup(self.source, self.archive, policy=self.source_policy, server_binary='synthetic-helper')
        self.backup()
        for kwargs in ({}, {'policy': self.target_policy}, {'source_policy': self.source_policy}):
            with self.subTest(arguments=tuple(kwargs)), self.assertRaises(ValueError):
                ops.restore(self.archive, self.target, offline=True, server_binary='synthetic-helper', **kwargs)
        self.assertFalse(self.target.exists())

    def test_partial_publication_retries_only_same_plan_and_never_overwrites_drift(self):
        self.backup()
        def crash(stage):
            if stage == 'target:database': raise ValueError('synthetic interruption')
        with self.assertRaisesRegex(ValueError, 'synthetic interruption'): self.restore(hook=crash)
        self.assertFalse(self.target.exists()); self.assertTrue(Path(str(self.next_data / 'control.sqlite') + '.restore-gate.json').exists())
        (self.next_data / 'history/data/samples').write_text('external drift')
        with self.assertRaisesRegex(ValueError, 'changed server target'): self.restore()
        self.assertEqual((self.next_data / 'history/data/samples').read_text(), 'external drift')
        self.assertFalse(self.target.exists())

    def test_plan_only_and_after_database_interruption_resume(self):
        self.backup()
        for stop in ('plan_persisted', 'target:database', 'before_publish'):
            def crash(stage):
                if stage == stop: raise ValueError('synthetic stop')
            with self.subTest(stop=stop), self.assertRaisesRegex(ValueError, 'synthetic stop'): self.restore(hook=crash)
            self.assertFalse(self.target.exists())
        self.assertEqual(self.restore()['state'], 'installed_gated')

    def test_existing_db_history_and_shared_dependency_mtime_refused(self):
        self.backup()
        self.next_data.mkdir(mode=0o700); local.private(self.next_data / 'control.sqlite-wal', '')
        with self.assertRaisesRegex(ValueError, 'sidecar already exists'): self.restore()
        (self.next_data / 'control.sqlite-wal').unlink(); (self.next_data / 'history').mkdir(mode=0o700)
        with self.assertRaisesRegex(ValueError, 'history already exists'): self.restore()
        (self.next_data / 'history').rmdir(); local.private(self.next_certs / 'ca.pem', (self.certs / 'ca.pem').read_text())
        with self.assertRaisesRegex(ValueError, 'shared dependency differs'): self.restore()
        stamp = (self.certs / 'ca.pem').stat().st_mtime_ns; os.utime(self.next_certs / 'ca.pem', ns=(stamp, stamp))
        self.assertEqual(self.restore()['state'], 'installed_gated')

    def test_archive_omission_checksum_and_target_parent_drift_rejected(self):
        self.backup()
        original = server.atomic_copy
        def drift(api, source, target, entry, pins):
            if not getattr(drift, 'done', False):
                drift.done = True; self.next_certs.rename(self.root / 'moved-certs'); self.next_certs.mkdir(mode=0o700)
            return original(api, source, target, entry, pins)
        with mock.patch.object(server, 'atomic_copy', side_effect=drift), self.assertRaisesRegex(ValueError, 'parent changed'):
            self.restore()
        self.assertFalse(self.target.exists())

    def test_binary_dependency_and_existing_trusted_0755_exact_parent(self):
        binary = b"\x00\xff\xfeopaque-dependency\x80"
        (self.certs / 'ca.pem').write_bytes(binary)
        self.backup(); self.next_certs.chmod(0o755)
        self.assertEqual(self.restore()['state'], 'installed_gated')
        self.assertEqual((self.next_certs / 'ca.pem').read_bytes(), binary)

    def test_world_writable_exact_parent_is_refused_without_publication(self):
        self.backup(); self.next_certs.chmod(0o777)
        with self.assertRaisesRegex(ValueError, 'unsafe server target parent'): self.restore()
        self.assertFalse(self.target.exists()); self.assertFalse((self.next_certs / 'ca.pem').exists())

    def test_atomic_directory_publish_refuses_even_existing_empty_target(self):
        first = self.root / 'first'; second = self.root / 'second'; first.mkdir(mode=0o700); second.mkdir(mode=0o700)
        local.private(first / 'value', 'source')
        with self.assertRaises(OSError): server.publish_directory(first, second)
        self.assertTrue((first / 'value').exists()); self.assertEqual(list(second.iterdir()), [])



    def test_confirmation_requires_review_stoppage_context_and_durable_ledger(self):
        self.backup(); restored = self.restore(); history = self.next_data / 'history'
        local.private(history / 'flock.lock', '')
        kwargs = {'server_binary': 'synthetic-helper', 'policy': self.target_policy, 'env_file': self.env}
        for offline, reviewed in ((False, True), (True, False)):
            with self.assertRaises(ValueError):
                server.confirm(ops, self.target, restored['checkpoint_id'], offline=offline, agents_reviewed=reviewed, **kwargs)
        with (history / 'flock.lock').open('rb') as lock:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            with self.assertRaisesRegex(ValueError, 'still in use'):
                server.confirm(ops, self.target, restored['checkpoint_id'], offline=True, agents_reviewed=True, **kwargs)
        token = self.target / 'token'; original = token.read_bytes(); stamp = token.stat().st_mtime_ns
        token.write_text('drift')
        with self.assertRaisesRegex(ValueError, 'context differs'):
            server.confirm(ops, self.target, restored['checkpoint_id'], offline=True, agents_reviewed=True, **kwargs)
        token.write_bytes(original); os.utime(token, ns=(stamp, stamp))
        database = self.next_data / 'control.sqlite'
        with closing(sqlite3.connect(database)) as db, db:
            db.execute('INSERT INTO config_operations(operation_id,node_id,service_id,base_revision,candidate_digest,creator,deadline_at_ms,idempotency_key,request_digest,state,version,created_at_ms,updated_at_ms) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)',
                ('synthetic', 1, 'synthetic', 'a'*64, '', 'operator', 1, 'synthetic', 'b'*64, 'outcome_unknown', 1, 1, 1))
        with self.assertRaisesRegex(ValueError, 'active_operations'):
            server.confirm(ops, self.target, restored['checkpoint_id'], offline=True, agents_reviewed=True, **kwargs)
        # Simulate a separately established terminal fact; confirmation itself
        # must never write or infer this transition.
        with closing(sqlite3.connect(database)) as db, db: db.execute("UPDATE config_operations SET state='cancelled'")
        before = server.database_sources(ops, database)
        result = server.confirm(ops, self.target, restored['checkpoint_id'], offline=True, agents_reviewed=True, **kwargs)
        self.assertEqual(result['state'], 'confirmed'); self.assertTrue(result['restart_required'])
        self.assertEqual(before, server.database_sources(ops, database))
        self.assertFalse(Path(str(database) + '.restore-gate.json').exists())
        self.assertTrue(Path(str(database) + '.restore-completed-' + restored['checkpoint_id'] + '.json').exists())
        self.assertEqual(server.confirm(ops, self.target, restored['checkpoint_id'], offline=True, agents_reviewed=True, **kwargs), result)


class ConfirmationLedgerTests(unittest.TestCase):
    def receipt(self, index, *, completed=False, replaced='', current=True):
        import uuid
        identity = lambda n: str(uuid.UUID(int=n, version=4))
        value = {'id': identity(1000 + index), 'node_id': 1, 'service_id': identity(2000 + index), 'epoch': identity(3000 + index),
            'backup_service_id': identity(4000 + index), 'replaced_service_id': replaced, 'manifest_digest': 'a'*64,
            'context_revision': 'b'*64, 'store_digest': 'c'*64, 'state': 'acknowledged' if completed else 'pending', 'creator': 'operator',
            'created_at_ms': index+1, 'updated_at_ms': index+100, 'version': 1, 'token_sha256': 'd'*64,
            'confirmed_at_ms': index+2 if completed else None, 'current_token_sha256': 'd'*64 if current else None}
        return value
    def evaluate(self, rows, states=None): return server.evaluate_confirmation(ops, states or {}, rows)
    def test_lost_ack_time_and_completed_history_with_rotated_or_deleted_node(self):
        for current in (None, 'e'*64):
            row = self.receipt(1, completed=True); row['current_token_sha256'] = current
            self.assertTrue(self.evaluate([row])['ready'])
        row['current_token_sha256'] = row['token_sha256']
        row['state'] = 'pending'; self.assertEqual(self.evaluate([row])['code'], 'receipt_acknowledgement_required')
        row['state'] = 'acknowledged'; row['confirmed_at_ms'] = 0
        self.assertEqual(self.evaluate([row])['code'], 'invalid_completion')
    def test_unique_replaced_chain_only_and_current_binding(self):
        old = self.receipt(1); new = self.receipt(2, completed=True, replaced=old['service_id'])
        self.assertEqual(self.evaluate([old,new])['superseded'], 1)
        old['confirmed_at_ms'] = old['created_at_ms'] + 1
        self.assertEqual(self.evaluate([old,new])['superseded'], 1)
        old['confirmed_at_ms'] = None
        new['replaced_service_id'] = ''; new['backup_service_id'] = old['service_id']
        self.assertEqual(self.evaluate([old,new])['code'], 'receipt_unresolved')
        new['replaced_service_id'] = old['service_id']; new['current_token_sha256'] = 'e'*64
        self.assertEqual(self.evaluate([old,new])['code'], 'receipt_binding')
    def test_fork_cycle_order_depth_and_active_unknown_states(self):
        old = self.receipt(1); one = self.receipt(2, completed=True, replaced=old['service_id']); two = self.receipt(3, completed=True, replaced=old['service_id'])
        self.assertEqual(self.evaluate([old,one,two])['code'], 'receipt_chain_ambiguous')
        one['state'] = 'pending'; one['confirmed_at_ms'] = None; old['replaced_service_id'] = one['service_id']
        old['created_at_ms'] = one['created_at_ms']; self.assertEqual(self.evaluate([old,one])['code'], 'receipt_chain_cycle')
        old['replaced_service_id'] = ''; one['created_at_ms'] = 1
        self.assertEqual(self.evaluate([old,one])['code'], 'receipt_chain_order')
        for length, code in ((129, 'ok'), (130, 'receipt_chain_limit')):
            rows = []
            for i in range(length): rows.append(self.receipt(i, completed=i==length-1, replaced=rows[-1]['service_id'] if rows else ''))
            self.assertEqual(self.evaluate(rows)['code'], code)
        self.assertEqual(self.evaluate([], {'unknown': 1})['code'], 'invalid_operation_state')
        self.assertEqual(self.evaluate([], {'outcome_unknown': 1})['code'], 'active_operations')


NATIVE_SERVER = os.environ.get('FRP_PLUS_TEST_SERVER')


@unittest.skipUnless(NATIVE_SERVER, 'set FRP_PLUS_TEST_SERVER for actual native helper acceptance')
class NativeServerCheckpointTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(); self.root = Path(self.temp.name).resolve(); self.root.chmod(0o700)
        self.source = self.root / 'source'; self.source.mkdir(mode=0o700)
        self.data = self.root / 'source-data'; self.data.mkdir(mode=0o700)
        self.target = self.root / 'target'; self.target_data = self.root / 'target-data'
        self.source_files = self.root / 'source-files'; self.source_files.mkdir(mode=0o700)
        self.target_files = self.root / 'target-files'; self.target_files.mkdir(mode=0o700)
        local.private(self.source_files / '404.html', '<html>retained page</html>')
        local.private(self.source / 'github.secret', 'synthetic-private-oauth-secret')
        local.private(self.source / 'installation.json', local.installation_metadata(['server']))
        local.control_database(self.data / 'control.sqlite')
        history = self.data / 'history'; history.mkdir(mode=0o700); (history / 'empty').mkdir(mode=0o700)
        local.private(history / 'flock.lock', ''); local.private(history / 'persisted', 'offline fixture')
        config = "bindAddr='127.0.0.1'\nbindPort=7000\ncustom404Page='{{ .Envs.PAGE }}'\n[auth]\nmethod='token'\ntoken='{{ .Envs.TOKEN }}'\n"
        config += "[monitor]\nenabled=true\nbindAddr='127.0.0.1'\nbindPort=7401\nserverID='unchanged-test-server'\ndatabaseFile='{{ .Envs.DATA }}/control.sqlite'\nhistoryDataPath='{{ .Envs.DATA }}/history'\n"
        config += "githubClientID='test-client'\ngithubClientSecretFile='{{ .Envs.ROOT }}/github.secret'\ngithubCallbackURL='https://admin.invalid/api/admin/v1/auth/github/callback'\ngithubAdminUsers=['test-admin']\n"
        local.private(self.source / 'server.toml', config)
        self.source_policy = self.root / 'source-policy.json'; self.target_policy = self.root / 'target-policy.json'
        self.source_env = self.root / 'source-environment.json'; self.target_env = self.root / 'target-environment.json'
        for folder, data, external, policy, env in ((self.source, self.data, self.source_files, self.source_policy, self.source_env),
                (self.target, self.target_data, self.target_files, self.target_policy, self.target_env)):
            local.private(policy, json.dumps({'version': 1, 'config_file': str(folder / 'server.toml'), 'working_dir': str(folder),
                'roots': [{'id': 'install', 'path': str(folder)}, {'id': 'data', 'path': str(data)}],
                'files': [{'id': 'page', 'path': str(external / '404.html')}]}))
            local.private(env, json.dumps({'ROOT': str(folder), 'DATA': str(data), 'PAGE': str(external / '404.html'), 'TOKEN': 'synthetic-unarchived-environment'}))
        self.archive = self.root / 'native.tar.gz'; self.binary = Path(NATIVE_SERVER).resolve()

    def tearDown(self): self.temp.cleanup()

    def test_actual_native_context_external_data_relocation_and_retry(self):
        ops.backup(self.source, self.archive, offline=True, server_binary=self.binary, policy=self.source_policy, env_file=self.source_env)
        with tarfile.open(self.archive, 'r:gz') as archive:
            data = b'\n'.join(archive.extractfile(entry).read() for entry in archive)
        self.assertNotIn(b'synthetic-unarchived-environment', data)
        shutil.rmtree(self.source); shutil.rmtree(self.data); shutil.rmtree(self.source_files)
        kwargs = {'offline': True, 'server_binary': self.binary, 'source_policy': self.source_policy, 'policy': self.target_policy,
                  'env_file': self.target_env, 'source_env_file': self.source_env}
        result = ops.restore(self.archive, self.target, **kwargs)
        self.assertEqual(result['state'], 'installed_gated')
        self.assertTrue((self.target_data / 'control.sqlite.restore-gate.json').exists())
        self.assertEqual((self.target_files / '404.html').read_text(), '<html>retained page</html>')
        self.assertTrue((self.target_data / 'history/empty').is_dir())
        self.assertEqual((self.target_data / 'history/persisted').read_text(), 'offline fixture')
        self.assertNotIn('synthetic-unarchived-environment', (self.target / 'server.toml').read_text())
        self.assertEqual(ops.restore(self.archive, self.target, **kwargs), result)


if __name__ == '__main__': unittest.main()
