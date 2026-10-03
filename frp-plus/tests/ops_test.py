"""Exercise WAL-aware backups, hostile archives and installation configuration."""
import argparse
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import sqlite3
import sys
import tarfile
import tempfile
import threading
import tomllib
import unittest
from contextlib import closing
from unittest import mock

SCRIPTS = Path(__file__).resolve().parents[1] / 'scripts'
sys.path.insert(0, str(SCRIPTS))
import ops
import local


class OpsTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.root = Path(self.temporary.name).resolve()
        self.runtime = self.root / 'runtime'
        local.initialize(self.runtime, plain_http=True, config=local.settings(self.root, {}))
        self.backups = self.root / 'backups'
        self.backups.mkdir(mode=0o700)

    def tearDown(self):
        self.temporary.cleanup()

    def test_backup_recovers_committed_wal_and_remaps_private_paths(self):
        database = self.runtime / 'control.sqlite'
        connection = sqlite3.connect(database)
        database.chmod(0o600)
        try:
            connection.execute('pragma journal_mode=wal')
            connection.execute('pragma wal_autocheckpoint=0')
            connection.execute('create table samples(value text)')
            connection.execute('insert into samples values (?)', ('18446744073709551616',))
            connection.commit()
            self.assertGreater(Path(str(database) + '-wal').stat().st_size, 0)
            archive = ops.backup(self.runtime, self.backups / 'snapshot.tar.gz')
            connection.execute('insert into samples values (?)', ('later',))
            connection.commit()
            (self.runtime / 'server.log').write_text('not backed up')
            destination = self.root / '恢复目录'
            ops.restore(archive, destination)
            restored = sqlite3.connect(destination / 'control.sqlite')
            try:
                self.assertEqual(restored.execute('select value from samples').fetchall(), [('18446744073709551616',)])
                self.assertEqual(restored.execute('pragma integrity_check').fetchone(), ('ok',))
            finally:
                restored.close()
            config = tomllib.loads((destination / 'server.toml').read_text())
            self.assertEqual(config['monitor']['databaseFile'], str(destination / 'control.sqlite'))
            self.assertEqual(config['monitor']['historyDataPath'], '')
            self.assertNotIn('adminCredentialsFile', config['monitor'])
            self.assertEqual(json.loads((destination / 'installation.json').read_text())['format'], 2)
            self.assertFalse((destination / 'server.log').exists())
            self.assertEqual(archive.stat().st_mode & 0o777, 0o600)
            for path in destination.iterdir():
                self.assertEqual(path.stat().st_mode & 0o777, 0o600)
            # A restored installation can itself be backed up.
            ops.backup(destination, self.backups / 'again.tar.gz')
            with self.assertRaises(ValueError):
                ops.restore(archive, destination)
        finally:
            connection.close()

    def test_backup_empty_new_install_and_refuses_existing_output(self):
        path = self.backups / 'initial.tar.gz'
        ops.backup(self.runtime, path)
        ops.restore(path, self.root / 'initial')
        with self.assertRaises(ValueError):
            ops.backup(self.runtime, path)

    def test_rejects_external_config_reference_permissions_and_symlinks(self):
        target = self.runtime / 'agent.token'
        target.chmod(0o644)
        with self.assertRaises(ValueError):
            ops.backup(self.runtime, self.backups / 'bad.tar.gz')
        target.chmod(0o600)
        target.unlink()
        target.symlink_to(self.runtime / 'frp.token')
        with self.assertRaises((ValueError, OSError)):
            ops.backup(self.runtime, self.backups / 'bad.tar.gz')
        target.unlink()
        local.private(target, 'x' * 43)
        config = self.runtime / 'server.toml'
        config.write_text(config.read_text().replace(str(self.runtime / 'frp.token'), '/tmp/outside.token'))
        with self.assertRaises(ValueError):
            ops.backup(self.runtime, self.backups / 'bad.tar.gz')

    def test_backup_rejects_all_nested_runtime_destinations(self):
        child = self.runtime / 'backups'
        child.mkdir(mode=0o700)
        nested = child / 'deeper'
        nested.mkdir(mode=0o700)
        for parent in (self.runtime, child, nested):
            output = parent / 'snapshot.tar.gz'
            with self.subTest(parent=parent), self.assertRaisesRegex(ValueError, 'outside the runtime'):
                ops.backup(self.runtime, output)
            self.assertFalse(output.exists())
            self.assertEqual(list(parent.glob('.frp-backup-*')), [])
        # A sibling sharing the runtime's name prefix is still outside it.
        sibling = self.root / (self.runtime.name + '-backups')
        sibling.mkdir(mode=0o700)
        output = sibling / 'snapshot.tar.gz'
        self.assertEqual(ops.backup(self.runtime, output), output)

    def test_restore_rejects_links_traversal_duplicate_and_corrupt_hash(self):
        for kind in ('link', 'traversal', 'duplicate', 'hash'):
            archive = self.backups / (kind + '.tar.gz')
            with tarfile.open(archive, 'w:gz') as out:
                data = b'{}'
                member = tarfile.TarInfo('../escape' if kind == 'traversal' else 'installation.json')
                member.size = len(data)
                if kind == 'link':
                    member.type, member.linkname = tarfile.SYMTYPE, '/tmp/escape'
                    out.addfile(member)
                else:
                    out.addfile(member, io.BytesIO(data))
                if kind == 'duplicate':
                    out.addfile(member, io.BytesIO(data))
                if kind == 'hash':
                    data = json.dumps({'format': 2, 'original_directory': '/old', 'sha256': {'installation.json': '0' * 64}}).encode()
                    member = tarfile.TarInfo('BACKUP.json')
                    member.size = len(data)
                    out.addfile(member, io.BytesIO(data))
            archive.chmod(0o600)
            with self.subTest(kind=kind), self.assertRaises((ValueError, FileNotFoundError)):
                ops.restore(archive, self.root / kind)
            self.assertFalse((self.root / kind).exists())
        self.assertFalse((self.root / 'escape').exists())

    def test_unit_generation_keeps_host_metrics_and_blocks_injection(self):
        unit = ops.systemd_unit('agent', '/var/lib/frp-plus', '/opt/frp-plus')
        self.assertIn('User=frp-plus', unit)
        self.assertIn('UMask=0077', unit)
        self.assertIn('verify -c /var/lib/frp-plus/agent.toml', unit)
        # Mount/network isolation would silently change host resource scope.
        for option in ('PrivateNetwork=', 'PrivateMounts=', 'PrivateTmp=', 'ProtectProc='):
            self.assertNotIn(option, unit)
        for path in ('/tmp/%n', '/tmp/$HOME', '/tmp/a\nExecStart=/bin/sh', '/tmp/a b', '/tmp/../escape'):
            with self.subTest(path=path), self.assertRaises(ValueError):
                ops.systemd_unit('server', path, '/opt/frp-plus')

    def test_server_init_uses_empty_control_store_and_github_secret_file(self):
        # Reuse a generated local certificate to test exact installation bytes.
        certificate_source = self.root / 'tls-source'
        local.initialize(certificate_source, config=local.settings(self.root, {}))
        secret = self.root / 'github-input'
        local.private(secret, 'example-oauth-secret\n')
        config = local.settings(self.root, {'FRP_GITHUB_CLIENT_ID': 'example-id', 'FRP_GITHUB_CLIENT_SECRET_FILE': str(secret),
            'FRP_GITHUB_CALLBACK_URL': 'https://monitor.example.invalid/api/admin/v1/auth/github/callback', 'FRP_GITHUB_ADMIN_USERS': 'ExampleAdmin'})
        args = argparse.Namespace(directory=str(self.root / 'server🛰'), server_id='primary🛰', bind='127.0.0.1',
            tls_cert=str(certificate_source / 'local.crt'), tls_key=str(certificate_source / 'local.key'))
        with mock.patch.object(ops, 'settings', return_value=config):
            folder = ops.server_init(args)
        text = (folder / 'server.toml').read_text()
        monitor = tomllib.loads(text)['monitor']
        self.assertEqual(monitor['serverID'], 'primary🛰')
        self.assertEqual(monitor['databaseFile'], str(folder / 'control.sqlite'))
        self.assertEqual(monitor['certFile'], str(folder / 'tls.crt'))
        self.assertEqual(monitor['keyFile'], str(folder / 'tls.key'))
        self.assertEqual(monitor['historyDataPath'], '')
        self.assertNotIn('historyEnabled', monitor)
        self.assertEqual(monitor['githubAdminUsers'], ['exampleadmin'])
        self.assertEqual(monitor['githubClientSecretFile'], str(folder / 'github.secret'))
        self.assertNotIn('example-oauth-secret', text)
        self.assertFalse((folder / 'admin.token').exists())
        connection = sqlite3.connect(folder / 'control.sqlite')
        self.assertEqual(connection.execute('SELECT count(*) FROM nodes').fetchone(), (0,))
        connection.close()
        archive = ops.backup(folder, self.backups / 'github.tar.gz')
        restored = ops.restore(archive, self.root / 'github-restored🛰')
        self.assertEqual((restored / 'github.secret').read_text(), 'example-oauth-secret\n')
        restored_config = tomllib.loads((restored / 'server.toml').read_text())['monitor']
        self.assertEqual(restored_config['githubClientSecretFile'], str(restored / 'github.secret'))

    def test_agent_init_private_config_and_no_plaintext_tokens_in_toml(self):
        args = argparse.Namespace(directory=str(self.root / 'agent🛰'), monitor_url='wss://monitor.example.invalid:7401/agent/v1/ws',
              server_addr='frp.example.invalid', server_id='test', client_id='stable-client🛰', user='tenant🛰',
              agent_token=str(self.runtime / 'agent.token'), frp_token=str(self.runtime / 'frp.token'),
              ca=None, probes=False, allow_private_probes=False)
        with mock.patch.object(ops, 'settings', return_value=local.settings(self.root, {})):
            ops.agent_init(args)
        folder = Path(args.directory)
        content = (folder / 'agent.toml').read_text()
        config = tomllib.loads(content)
        self.assertEqual(config['clientID'], 'stable-client🛰')
        self.assertEqual(config['user'], 'tenant🛰')
        self.assertFalse(config['telemetry']['probeEnabled'])
        self.assertFalse(config['telemetry']['allowInsecureLoopback'])
        self.assertFalse(config['telemetry']['probeAllowPrivate'])
        self.assertEqual(config['telemetry']['endpoint'], args.monitor_url)
        self.assertNotIn((folder / 'agent.token').read_text().strip(), content)
        self.assertEqual(config['auth']['tokenSource']['file']['path'], str(folder / 'frp.token'))
        self.assertEqual(folder.stat().st_mode & 0o777, 0o700)
        self.assertNotIn('configManagement', config['telemetry'])
        args.directory = str(self.root / 'managed-agent')
        args.manage_config = True
        with mock.patch.object(ops, 'settings', return_value=local.settings(self.root, {})):
            ops.agent_init(args)
        managed_folder = Path(args.directory)
        managed = tomllib.loads((managed_folder / 'agent.toml').read_text())
        self.assertEqual(managed['store']['path'], str(managed_folder / 'managed/store.json'))
        self.assertTrue(managed['telemetry']['configManagement']['enabled'])
        self.assertEqual((managed_folder / 'managed').stat().st_mode & 0o777, 0o700)
        self.assertEqual((managed_folder / 'managed/store.json').stat().st_mode & 0o777, 0o600)
        self.assertEqual(json.loads((managed_folder / 'installation.json').read_text())['managed'],
                         {'version': 1, 'root': 'managed', 'store': 'store.json'})
        # Old format 2 must reject rather than silently omit managed recovery state.
        with self.assertRaises(ValueError):
            ops.backup(managed_folder, self.backups / 'incomplete-managed.tar.gz')
        args.directory = str(self.root / 'unsafe')
        args.monitor_url = 'ws://127.0.0.1:7401/agent/v1/ws'
        with self.assertRaises(ValueError):
            ops.agent_init(args)

    def test_restore_preserves_non_path_values_equal_to_an_old_path(self):
        config = self.runtime / 'agent.toml'
        before = config.read_text()
        before += '\n[metadatas]\noperator_note = ' + json.dumps(str(self.runtime / 'agent.token')) + '\n'
        config.write_text(before)
        archive = ops.backup(self.runtime, self.backups / 'paths.tar.gz')
        restored = self.root / 'restored-paths'
        ops.restore(archive, restored)
        data = tomllib.loads((restored / 'agent.toml').read_text())
        self.assertEqual(data['metadatas']['operator_note'], str(self.runtime / 'agent.token'))
        self.assertEqual(data['telemetry']['tokenFile'], str(restored / 'agent.token'))

    def test_remap_rejects_changes_inside_manual_multiline_string(self):
        text = 'note = """\npath = "/old/agent.token"\n"""\n'
        with self.assertRaises(ValueError):
            ops.remap_paths(text, '/old', self.root / 'new')

    def test_database_budget_uses_actual_sqlite_page_size(self):
        source, target = self.runtime / 'large-pages.sqlite', self.backups / 'snapshot.sqlite'
        connection = sqlite3.connect(source)
        source.chmod(0o600)
        try:
            connection.execute('PRAGMA page_size=65536')
            connection.execute('CREATE TABLE value(text TEXT)')
            connection.commit()
            self.assertEqual(connection.execute('PRAGMA page_size').fetchone()[0], 65536)
            with mock.patch.object(ops, 'MAX_FILE', 100000), self.assertRaises(ValueError):
                ops.database_snapshot(source, target)
            self.assertEqual(target.stat().st_size, 0, 'oversized snapshot must fail before allocating its pages')
        finally:
            connection.close()

    @unittest.skipUnless(hasattr(os, 'mkfifo'), 'requires Unix FIFO')
    def test_nonregular_archive_and_database_do_not_block(self):
        fifo = self.root / 'pipe'
        os.mkfifo(fifo, 0o600)
        for action in (lambda: ops.read_private(fifo),
                       lambda: ops.database_snapshot(fifo, self.backups / 'pipe.sqlite'),
                       lambda: ops.restore(fifo, self.root / 'pipe-restore')):
            errors = []
            def invoke():
                try:
                    action()
                except BaseException as exc:
                    errors.append(exc)
            thread = threading.Thread(target=invoke, daemon=True)
            thread.start()
            thread.join(timeout=1)
            blocked = thread.is_alive()
            if blocked:
                # Unblock a regressed blocking open so a failing test also cleans up.
                descriptor = os.open(fifo, os.O_WRONLY | os.O_NONBLOCK)
                os.close(descriptor)
                thread.join(timeout=1)
            self.assertFalse(blocked, 'regular-file validation blocked on FIFO open')
            self.assertEqual(len(errors), 1)
            self.assertIsInstance(errors[0], ValueError)

    def test_backup_excludes_tsdb_but_restores_control_configuration_and_counters(self):
        history = self.runtime / 'metrics'
        config = self.runtime / 'server.toml'
        config.write_text(config.read_text().replace('historyDataPath = ""', 'historyDataPath = ' + json.dumps(str(history))))
        history.mkdir(mode=0o700)
        (history / 'tsdb-sample').write_text('not included')
        db = sqlite3.connect(self.runtime / 'control.sqlite')
        with db:
            db.execute("UPDATE nodes SET traffic_period_rx_bytes='9007199254740993',traffic_adjustment_bytes='-3',private_note='private'")
        db.close()
        archive = ops.backup(self.runtime, self.backups / 'control.tar.gz')
        restored = self.root / 'control-restored'
        ops.restore(archive, restored)
        self.assertFalse((restored / 'metrics').exists())
        config = tomllib.loads((restored / 'server.toml').read_text())
        self.assertEqual(config['monitor']['historyDataPath'], str(restored / 'metrics'))
        db = sqlite3.connect(restored / 'control.sqlite')
        self.assertEqual(db.execute('SELECT traffic_period_rx_bytes,traffic_adjustment_bytes,private_note FROM nodes').fetchone(), ('9007199254740993', '-3', 'private'))
        db.close()

    def test_backup_preserves_groups_memberships_and_revisions(self):
        with closing(sqlite3.connect(self.runtime / 'control.sqlite')) as database:
            with database:
                database.execute("INSERT INTO node_groups(id,name,config_revision) VALUES(7,'生产🛰',9)")
                database.execute("INSERT INTO node_groups(id,name,config_revision) VALUES(8,'empty',2)")
                database.execute("INSERT INTO node_group_members(group_id,node_id) VALUES(7,1)")
                database.execute("INSERT INTO node_groups(id,name) VALUES(20,'deleted')")
                database.execute("DELETE FROM node_groups WHERE id=20")
            expected_node = database.execute('SELECT * FROM nodes').fetchall()
        archive = ops.backup(self.runtime, self.backups / 'groups.tar.gz')
        restored = ops.restore(archive, self.root / 'restored-groups')
        with closing(sqlite3.connect(restored / 'control.sqlite')) as database:
            self.assertEqual(database.execute('PRAGMA user_version').fetchone(), (9,))
            self.assertEqual(database.execute('SELECT * FROM nodes').fetchall(), expected_node)
            self.assertEqual(database.execute('SELECT id,name,config_revision FROM node_groups ORDER BY id').fetchall(),
                             [(7, '生产🛰', 9), (8, 'empty', 2)])
            self.assertEqual(database.execute('SELECT group_id,node_id FROM node_group_members').fetchall(), [(7, 1)])
            cursor = database.execute("INSERT INTO node_groups(name) VALUES('next')")
            self.assertEqual(cursor.lastrowid, 21)
            self.assertEqual(database.execute('PRAGMA foreign_key_check').fetchall(), [])

    def test_restore_keeps_supported_v4_for_startup_migration_and_rejects_other_versions(self):
        with closing(sqlite3.connect(self.runtime / 'control.sqlite')) as database:
            database.executescript('DROP TABLE config_operation_events; DROP TABLE config_operations; '
                                  'DROP TABLE node_group_members; DROP TABLE node_groups; PRAGMA user_version=4;')
            expected_node = database.execute('SELECT * FROM nodes').fetchall()
            expected_settings = database.execute('SELECT * FROM settings').fetchall()
        archive = ops.backup(self.runtime, self.backups / 'legacy.tar.gz')
        restored = ops.restore(archive, self.root / 'legacy-restored')
        with closing(sqlite3.connect(restored / 'control.sqlite')) as database:
            self.assertEqual(database.execute('PRAGMA user_version').fetchone(), (4,))
            self.assertEqual(database.execute('SELECT * FROM nodes').fetchall(), expected_node)
            self.assertEqual(database.execute('SELECT * FROM settings').fetchall(), expected_settings)
            self.assertEqual(database.execute("SELECT count(*) FROM sqlite_master WHERE name='node_groups'").fetchone(), (0,))
        for version, application in ((3, 1179798836), (10, 1179798836), (4, 123), (5, 123)):
            with self.subTest(version=version, application=application):
                with closing(sqlite3.connect(self.runtime / 'control.sqlite')) as database:
                    database.executescript(f'PRAGMA user_version={version}; PRAGMA application_id={application};')
                archive = ops.backup(self.runtime, self.backups / f'unsupported-{version}-{application}.tar.gz')
                target = self.root / f'unsupported-{version}-{application}'
                with self.assertRaisesRegex(ValueError, 'unsupported control database'):
                    ops.restore(archive, target)
                self.assertFalse(target.exists())

    def test_backup_preserves_configuration_operations_and_audit(self):
        operation = '12345678-1234-4234-8234-123456789abc'
        digest = 'a' * 64
        agent = json.dumps({'operation_id': operation, 'base_revision': digest, 'context_revision': digest,
                            'old_digest': digest, 'candidate_digest': digest, 'state': 'outcome_unknown',
                            'error_code': 'connection_lost', 'created_at_ms': 1000, 'updated_at_ms': 2000,
                            'deadline_at_ms': 2000000000000, 'store_persisted': True, 'runtime_applied': True,
                            'runtime_loaded': False, 'resources_ready': False, 'business_checked': False})
        with closing(sqlite3.connect(self.runtime / 'control.sqlite')) as database:
            with database:
                database.execute('INSERT INTO config_operations VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)',
                                 (operation, 1, '10000000-0000-4000-8000-000000000001', digest, digest, 'administrator',
                                  2000000000000, operation, digest, 'outcome_unknown', 2, 1000, 2000, agent, 2000))
                database.execute('INSERT INTO config_operation_events(operation_id,version,state,code,changes_json,created_at_ms,actor) '
                                 'VALUES(?,?,?,?,?,?,?)', (operation, 2, 'outcome_unknown', 'connection_lost', '[]', 2000, 'system:configuration-reconcile'))
            expected_operations = database.execute('SELECT * FROM config_operations').fetchall()
            expected_events = database.execute('SELECT * FROM config_operation_events').fetchall()
        archive = ops.backup(self.runtime, self.backups / 'operations.tar.gz')
        restored = ops.restore(archive, self.root / 'restored-operations')
        with closing(sqlite3.connect(restored / 'control.sqlite')) as database:
            self.assertEqual(database.execute('PRAGMA user_version').fetchone(), (9,))
            self.assertEqual(database.execute('SELECT * FROM config_operations').fetchall(), expected_operations)
            self.assertEqual(database.execute('SELECT * FROM config_operation_events').fetchall(), expected_events)
            self.assertEqual(database.execute('PRAGMA foreign_key_check').fetchall(), [])
            # Restoration cannot accidentally release the unresolved node lease.
            with self.assertRaises(sqlite3.IntegrityError):
                database.execute('INSERT INTO config_operations VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)',
                                 ('87654321-1234-4234-8234-123456789abc', 1, 'managed-service', digest, '',
                                  'administrator', 2000000000000, 'other', digest, 'draft', 1, 2000, 2000, None, None))

    def test_backup_preserves_schema_nine_restore_takeover_receipts(self):
        with closing(sqlite3.connect(self.runtime / 'control.sqlite')) as database:
            token = database.execute('SELECT token_sha256 FROM nodes WHERE id=1').fetchone()[0]
            row = ('12345678-1234-4234-8234-123456789abc', 1, token,
                   '23456789-1234-4234-8234-123456789abc', '34567890-1234-4234-8234-123456789abc',
                   '45678901-1234-4234-8234-123456789abc', '', 'a' * 64, 'b' * 64, 'c' * 64,
                   'pending', 'first-admin', 1000, 1000, 1)
            database.execute('INSERT INTO config_restores VALUES(' + ','.join('?' for _ in row) + ')', row)
            database.commit()
        archive = ops.backup(self.runtime, self.backups / 'restore-receipts.tar.gz')
        restored = ops.restore(archive, self.root / 'restored-receipts')
        with closing(sqlite3.connect(restored / 'control.sqlite')) as database:
            self.assertEqual(database.execute('PRAGMA user_version').fetchone(), (9,))
            self.assertEqual(database.execute('SELECT * FROM config_restores').fetchall(), [row])

    def test_old_installation_format_and_missing_control_database_are_rejected(self):
        metadata = self.runtime / 'installation.json'
        original = metadata.read_text()
        metadata.write_text('{"format":1,"roles":["server"]}')
        with self.assertRaises(ValueError):
            ops.backup(self.runtime, self.backups / 'old.tar.gz')
        metadata.write_text(original)
        (self.runtime / 'control.sqlite').unlink()
        with self.assertRaises(ValueError):
            ops.backup(self.runtime, self.backups / 'missing.tar.gz')

    def test_bad_installation_metadata_is_rejected_cleanly(self):
        (self.runtime / 'installation.json').write_text('[]')
        with self.assertRaises(ValueError):
            ops.backup(self.runtime, self.backups / 'bad-metadata.tar.gz')

    def test_literal_rejects_whitespace_in_shared_operational_fields(self):
        for value in ('has space', 'tab\tseparated', 'line\nbreak', ' lead', 'trail '):
            with self.subTest(value=value), self.assertRaises(ValueError):
                ops.literal(value, 'field')
        self.assertEqual(ops.literal('plain-value', 'field'), '"plain-value"')
        args = argparse.Namespace(directory=str(self.root / 'agent-ws'), monitor_url='wss://monitor.example.invalid:7401/agent/v1/ws',
              server_addr='frp example.invalid', server_id='test', client_id='stable-client', user='',
              agent_token=str(self.runtime / 'agent.token'), frp_token=str(self.runtime / 'frp.token'),
              ca=None, probes=False, allow_private_probes=False)
        config = local.settings(self.root, {})
        with mock.patch.object(ops, 'settings', return_value=config), self.assertRaises(ValueError):
            ops.agent_init(args)
        args.directory = str(self.root / 'agent-ws2')
        args.server_addr = 'frp.example.invalid'
        args.monitor_url = 'wss://monitor example.invalid:7401/agent/v1/ws'
        with mock.patch.object(ops, 'settings', return_value=config), self.assertRaises(ValueError):
            ops.agent_init(args)


if __name__ == '__main__':
    unittest.main()
