"""Opt-in real frps/Agent cross-directory recovery, loopback and private files only."""
from contextlib import ExitStack, closing
import json
import os
from pathlib import Path
import shutil
import sqlite3
import sys
import tarfile
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[1]
sys.path[:0] = [str(ROOT / 'scripts'), str(ROOT / 'tests')]
import local
import ops
import smoke
from monitor_smoke import CountedTarget
from smoke import PublicAPI, child, echo_matches, echo_server, port_open, reserve_port, wait_for


def private(path, data):
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    path.write_bytes(data.encode() if isinstance(data, str) else data)
    path.chmod(0o600)


def document(path, value): private(path, json.dumps(value, sort_keys=True))
def samples(history): return sum(point.get('samples', 0) for point in history['points'])


@unittest.skipUnless(os.environ.get('FRP_SERVER_BACKUP_E2E_SERVER') and os.environ.get('FRP_SERVER_BACKUP_E2E_AGENT'),
                     'requires explicit native server and Agent binaries')
class ServerBackupNativeTest(unittest.TestCase):
    def test_external_data_and_template_relocation(self):
        server = Path(os.environ['FRP_SERVER_BACKUP_E2E_SERVER']).resolve()
        agent = Path(os.environ['FRP_SERVER_BACKUP_E2E_AGENT']).resolve()
        with tempfile.TemporaryDirectory(prefix='server-backup-native-', dir=ROOT / '.cache') as temp, ExitStack() as stack:
            base = Path(temp).resolve(); base.chmod(0o700)
            held = [stack.enter_context(reserve_port()) for _ in range(4)]
            control, monitor, remote, dashboard = [sock.getsockname()[1] for sock in held]
            echo = stack.enter_context(echo_server())
            target = stack.enter_context(CountedTarget())
            cfg = local.settings(base, {'FRP_SERVER_PORT': str(control), 'FRP_MONITOR_PORT': str(monitor),
                                       'FRP_MONITOR_INTERVAL_SECONDS': '1', 'FRP_MONITOR_HISTORY_DATA_PATH': 'history'})
            source = local.initialize(base / 'source', probes=True, config=cfg)
            destination, client = base / 'destination', base / 'client'
            client.mkdir(mode=0o700)
            source_deps, target_deps = base / 'source-deps', base / 'target-deps'
            source_data, target_data = base / 'source-data', base / 'target-data'
            source_history, target_history = base / 'source-history', base / 'target-history'
            for folder in (source_deps, source_data): folder.mkdir(mode=0o700)
            for name in ('local.crt', 'frp.token', 'agent.token'):
                private(client / name, (source / name).read_bytes())
            agent_text = (source / 'agent.toml').read_text().replace(str(source), str(client))
            agent_text += ('\n[[proxies]]\nname = "server-restore-echo"\ntype = "tcp"\n'
                           f'localIP = "127.0.0.1"\nlocalPort = {echo}\nremotePort = {remote}\n')
            private(client / 'agent.toml', agent_text)
            for name in ('local.crt', 'local.key', 'frp.token'):
                (source / name).rename(source_deps / name)
            (source / 'control.sqlite').rename(source_data / 'control.sqlite')
            database = source_data / 'control.sqlite'
            with closing(sqlite3.connect(database)) as db, db:
                db.execute("UPDATE nodes SET traffic_adjustment_bytes='161061273600',traffic_reset_mode='manual' WHERE id=1")
                probes = {'version': 1, 'nodes': [{'agent_id': '1', 'tasks': [{'id': 'recovery-probe', 'name': 'Loopback probe', 'target': f'127.0.0.1:{target.port}', 'interval': 5}]}]}
                db.execute('UPDATE settings SET probe_json=? WHERE id=1', (json.dumps(probes),))
            raw = (source / 'server.toml').read_text()
            for name in ('local.crt', 'local.key', 'frp.token'):
                raw = raw.replace(str(source / name), str(source_deps / name))
            raw = raw.replace(str(source / 'control.sqlite'), str(database)).replace(str(source / 'history'), str(source_history))
            raw = (f'webServer.addr = "127.0.0.1"\nwebServer.port = {dashboard}\nwebServer.user = "fixture"\n'
                   'webServer.password = "{{ .Envs.FRP_RESTORE_TEST_PASSWORD }}"\n'
                   f'transport.tls.certFile = {json.dumps(str(source_deps / "local.crt"))}\n'
                   f'transport.tls.keyFile = {json.dumps(str(source_deps / "local.key"))}\n' + raw)
            private(source / 'server.toml', raw)
            document(source / 'installation.json', {'format': 2, 'roles': ['server']})
            # Remove unrelated Agent setup files. Only the controller's explicit closure is backed up.
            for name in ('agent.toml', 'agent.token', 'local.json'): (source / name).unlink()
            password = 'isolated-template-password-never-archived'
            env = {'FRP_RESTORE_TEST_PASSWORD': password}
            env_file = base / 'environment.json'; document(env_file, env)
            def policy(root, deps, data, history):
                return {'version': 1, 'config_file': str(root / 'server.toml'), 'working_dir': str(root),
                        'roots': [{'id': 'install', 'path': str(root)}, {'id': 'tls', 'path': str(deps)},
                                  {'id': 'data', 'path': str(data)}, {'id': 'history', 'path': str(history)}], 'files': []}
            source_policy, target_policy = base / 'source-policy.json', base / 'target-policy.json'
            document(source_policy, policy(source, source_deps, source_data, source_history))
            document(target_policy, policy(destination, target_deps, target_data, target_history))
            backups = base / 'backups'; backups.mkdir(mode=0o700)
            api = PublicAPI(monitor, client / 'local.crt')
            for sock in held: sock.close()
            process_env = {**smoke.child_environment(), **env}
            def launch_server(folder):
                with patch('smoke.child_environment', return_value=process_env):
                    process = stack.enter_context(child('relocated server', [str(server), '-c', str(folder / 'server.toml')], folder))
                wait_for('controller listeners', lambda: port_open(control) and port_open(monitor), (process,), 25)
                return process
            def launch_agent(service):
                process = stack.enter_context(child('relocation peer', [str(agent), '-c', str(client / 'agent.toml')], client))
                wait_for('peer monitoring and actual forwarding', lambda: api.node()['session'] == 'online' and echo_matches(remote), (service, process), 25)
                return process
            service = launch_server(source); peer = launch_agent(service)
            wait_for('persisted host and probe samples', lambda: samples(api.history()) > 0 and bool(api.history()['probes']) and api.history()['probes'][0]['samples'] > 0, (service, peer), 30)
            before = api.history()
            with self.assertRaises(ValueError):
                ops.backup(source, backups / 'running.tar.gz', offline=True, server_binary=server, policy=source_policy, env_file=env_file)
            self.assertFalse((backups / 'running.tar.gz').exists())
            service.stop(); peer.stop()
            self.assertFalse(service.forced_stop or peer.forced_stop)
            # Materialize a stopped main+WAL pair containing an uncheckpointed committed update.
            with closing(sqlite3.connect(database)) as db:
                db.execute('PRAGMA journal_mode=WAL'); db.execute('PRAGMA wal_autocheckpoint=0')
                db.execute("UPDATE nodes SET traffic_adjustment_bytes='171798691840' WHERE id=1"); db.commit()
                image, wal = database.read_bytes(), Path(str(database) + '-wal').read_bytes()
            private(database, image); private(Path(str(database) + '-wal'), wal)
            before_files = {p: (p.read_bytes(), p.stat().st_mtime_ns) for folder in (source, source_deps, source_data) for p in folder.iterdir() if p.is_file()}
            archive = ops.backup(source, backups / 'complete.tar.gz', offline=True, server_binary=server, policy=source_policy, env_file=env_file)
            for path, (contents, mtime) in before_files.items():
                self.assertEqual(path.read_bytes(), contents); self.assertEqual(path.stat().st_mtime_ns, mtime)
            with tarfile.open(archive, 'r:gz') as package:
                for member in package.getmembers():
                    if member.isfile(): self.assertNotIn(password.encode(), package.extractfile(member).read())
            # No recovery reads can fall back to the original main, TLS, token, database or TSDB.
            for folder in (source, source_deps, source_data, source_history): shutil.rmtree(folder)
            restored = ops.restore(archive, destination, offline=True, server_binary=server, source_policy=source_policy,
                                   policy=target_policy, source_env_file=env_file, env_file=env_file)
            self.assertEqual(restored['state'], 'installed_gated')
            target_database = target_data / 'control.sqlite'
            gate = Path(str(target_database) + '.restore-gate.json')
            self.assertTrue(gate.is_file())
            self.assertEqual(json.loads(gate.read_bytes())['checkpoint_id'], restored['checkpoint_id'])
            self.assertNotIn(str(source), (destination / 'server.toml').read_text())
            self.assertIn('{{ .Envs.FRP_RESTORE_TEST_PASSWORD }}', (destination / 'server.toml').read_text())
            with closing(sqlite3.connect(target_database)) as db:
                self.assertEqual(db.execute('PRAGMA integrity_check').fetchone(), ('ok',))
                self.assertEqual(db.execute('PRAGMA user_version').fetchone(), (12,))
                self.assertEqual(db.execute('SELECT traffic_adjustment_bytes FROM nodes WHERE id=1').fetchone(), ('171798691840',))
            service = launch_server(destination)
            recovered = api.history()
            self.assertGreaterEqual(samples(recovered), samples(before))
            self.assertGreaterEqual(recovered['probes'][0]['samples'], before['probes'][0]['samples'])
            self.assertEqual(api.node()['session'], 'waiting')
            peer = launch_agent(service)
            wait_for('new persisted samples after relocation', lambda: samples(api.history()) > samples(recovered), (service, peer), 30)
            # A running TSDB and a changed template context cannot be falsely confirmed offline.
            import ops_server_checkpoint as server_ops
            def confirm(**changes):
                options = dict(offline=True, agents_reviewed=True, server_binary=server, policy=target_policy, env_file=env_file)
                options.update(changes)
                return server_ops.confirm(ops, destination, restored['checkpoint_id'], **options)
            with self.assertRaises(ValueError): confirm()
            service.stop(); peer.stop()
            with self.assertRaises(ValueError): confirm(agents_reviewed=False)
            wrong_env = base / 'wrong-environment.json'; document(wrong_env, {**env, 'FRP_RESTORE_TEST_PASSWORD': password + '-changed'})
            with self.assertRaises(ValueError): confirm(env_file=wrong_env)
            confirmed = confirm()
            self.assertEqual(confirmed['state'], 'confirmed')
            self.assertFalse(gate.exists())
            service = launch_server(destination); peer = launch_agent(service)
            self.assertTrue(echo_matches(remote))
            service.stop(); peer.stop()
            # Restored dependencies, template requirements and history remain re-backupable.
            second = ops.backup(destination, backups / 'second.tar.gz', offline=True, server_binary=server, policy=target_policy, env_file=env_file)
            self.assertTrue(Path(second).is_file())


if __name__ == '__main__': unittest.main()
