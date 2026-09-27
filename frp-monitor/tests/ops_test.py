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
        unit = ops.systemd_unit('agent', '/var/lib/frp-monitor', '/opt/frp-monitor')
        self.assertIn('User=frp-monitor', unit)
        self.assertIn('UMask=0077', unit)
        self.assertIn('verify -c /var/lib/frp-monitor/agent.toml', unit)
        # Mount/network isolation would silently change host resource scope.
        for option in ('PrivateNetwork=', 'PrivateMounts=', 'PrivateTmp=', 'ProtectProc='):
            self.assertNotIn(option, unit)
        for path in ('/tmp/%n', '/tmp/$HOME', '/tmp/a\nExecStart=/bin/sh', '/tmp/a b', '/tmp/../escape'):
            with self.subTest(path=path), self.assertRaises(ValueError):
                ops.systemd_unit('server', path, '/opt/frp-monitor')

    def test_server_init_uses_empty_control_store_and_github_secret_file(self):
        # Reuse a generated local certificate to test exact installation bytes.
        certificate_source = self.root / 'tls-source'
        local.initialize(certificate_source, config=local.settings(self.root, {}))
        secret = self.root / 'github-input'
        local.private(secret, 'example-oauth-secret\n')
        config = local.settings(self.root, {'FRP_GITHUB_CLIENT_ID': 'example-id', 'FRP_GITHUB_CLIENT_SECRET_FILE': str(secret),
            'FRP_GITHUB_CALLBACK_URL': 'https://monitor.example.invalid/api/admin/v1/auth/github/callback', 'FRP_GITHUB_ADMIN_USERS': 'ExampleAdmin'})
        args = argparse.Namespace(directory=str(self.root / 'server'), server_id='primary', bind='127.0.0.1',
            tls_cert=str(certificate_source / 'local.crt'), tls_key=str(certificate_source / 'local.key'))
        with mock.patch.object(ops, 'settings', return_value=config):
            folder = ops.server_init(args)
        text = (folder / 'server.toml').read_text()
        monitor = tomllib.loads(text)['monitor']
        self.assertEqual(monitor['databaseFile'], str(folder / 'control.sqlite'))
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
        restored = ops.restore(archive, self.root / 'github-restored')
        self.assertEqual((restored / 'github.secret').read_text(), 'example-oauth-secret\n')
        restored_config = tomllib.loads((restored / 'server.toml').read_text())['monitor']
        self.assertEqual(restored_config['githubClientSecretFile'], str(restored / 'github.secret'))

    def test_agent_init_private_config_and_no_plaintext_tokens_in_toml(self):
        args = argparse.Namespace(directory=str(self.root / 'agent'), monitor_url='wss://monitor.example.invalid:7401/agent/v1/ws',
              server_addr='frp.example.invalid', server_id='test', client_id='stable-client', user='tenant',
              agent_token=str(self.runtime / 'agent.token'), frp_token=str(self.runtime / 'frp.token'),
              ca=None, probes=False, allow_private_probes=False)
        with mock.patch.object(ops, 'settings', return_value=local.settings(self.root, {})):
            ops.agent_init(args)
        folder = Path(args.directory)
        content = (folder / 'agent.toml').read_text()
        config = tomllib.loads(content)
        self.assertEqual(config['clientID'], 'stable-client')
        self.assertFalse(config['telemetry']['probeEnabled'])
        self.assertNotIn((folder / 'agent.token').read_text().strip(), content)
        self.assertEqual(config['auth']['tokenSource']['file']['path'], str(folder / 'frp.token'))
        self.assertEqual(folder.stat().st_mode & 0o777, 0o700)
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
        for action in (lambda: ops.database_snapshot(fifo, self.backups / 'pipe.sqlite'),
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


if __name__ == '__main__':
    unittest.main()
