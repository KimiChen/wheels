"""Exercise managed checkpoint packaging without opening/recovering the live engine."""
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

SCRIPTS = Path(__file__).resolve().parents[1] / 'scripts'
sys.path.insert(0, str(SCRIPTS))
import local
import ops

CHECKPOINT_ID = '12345678-1234-4234-8234-123456789abc'
SERVICE_ID = '23456789-1234-4234-8234-123456789abc'
RESTORE_EPOCH = '34567890-1234-4234-8234-123456789abc'
RESTORED_ID = '45678901-1234-4234-8234-123456789abc'


class ManagedOpsTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.root = Path(self.temporary.name).resolve()
        self.root.chmod(0o700)
        self.runtime = self.root / 'agent'
        self.runtime.mkdir(mode=0o700)
        self.backups = self.root / 'backups'
        self.backups.mkdir(mode=0o700)
        local.private(self.runtime / 'installation.json', local.installation_metadata(['agent'], True))
        local.private(self.runtime / 'agent.toml', '[telemetry.configManagement]\nenabled=true\nroot=' + json.dumps(str(self.runtime / 'managed')) + '\n')
        local.private(self.runtime / 'agent.token', 'private-controller-secret\n')
        local.private(self.runtime / 'frp.token', 'private-native-secret\n')
        (self.runtime / 'managed').mkdir(mode=0o700)
        local.private(self.runtime / 'managed/store.json', '{"proxies":[],"visitors":[]}\n')
        local.private(self.runtime / 'managed/identity.json', json.dumps({'service_id': SERVICE_ID}))
        local.private(self.runtime / 'managed/.lock', '')
        self.calls = []

    def tearDown(self):
        self.temporary.cleanup()

    def helper(self, arguments, folder, binary):
        self.calls.append((list(arguments), folder, binary))
        self.assertEqual(folder, self.runtime)
        if arguments[0] == 'checkpoint':
            output = Path(arguments[arguments.index('--output') + 1])
            output.mkdir(mode=0o700)
            (output / 'context').mkdir(mode=0o700)
            (output / 'managed').mkdir(mode=0o700)
            entries = []
            payload = {'context/' + name: (self.runtime / name).read_bytes()
                       for name in ('installation.json', 'agent.toml', 'agent.token', 'frp.token')}
            payload.update({'managed/' + name: (self.runtime / 'managed' / name).read_bytes()
                            for name in ('store.json', 'identity.json')})
            if (self.runtime / 'managed/restore.json').exists():
                payload['managed/restore.json'] = (self.runtime / 'managed/restore.json').read_bytes()
            for name, data in sorted(payload.items()):
                local.private(output / name, data.decode())
                entries.append({'path': name, 'size': len(data), 'sha256': hashlib.sha256(data).hexdigest(),
                                'mtime_ns': 1700000000123456789})
            manifest = {'version': 1, 'kind': 'frp-managed-checkpoint', 'id': CHECKPOINT_ID,
                        'created_at_ms': 1700000000000, 'root': str(self.runtime / 'managed'),
                        'store_name': 'store.json', 'config_file': str(self.runtime / 'agent.toml'),
                        'cwd': str(self.runtime), 'context_revision': 'a' * 64, 'service_id': SERVICE_ID,
                        'store_exists': True, 'store_digest': 'b' * 64, 'files': entries}
            raw = json.dumps(manifest, separators=(',', ':')).encode()
            local.private(output / 'CHECKPOINT.json', raw.decode())
            return {'code': 'ok', 'checkpoint_id': CHECKPOINT_ID, 'manifest_digest': hashlib.sha256(raw).hexdigest(),
                    'file_count': len(entries), 'total_bytes': sum(entry['size'] for entry in entries)}
        digest = arguments[arguments.index('--manifest-digest') + 1]
        return {'code': 'ok', 'epoch': RESTORE_EPOCH, 'service_id': RESTORED_ID, 'manifest_digest': digest,
                'state': 'pending' if arguments[0] == 'restore-install' else 'confirmed'}

    def backup(self, name='snapshot.tar.gz'):
        with mock.patch.object(ops, 'maintenance', side_effect=self.helper):
            return ops.backup(self.runtime, self.backups / name, offline=True, agent_binary='/native-agent')

    def archive_members(self, archive):
        with tarfile.open(archive, 'r:gz') as source:
            return [(member, source.extractfile(member).read()) for member in source]

    def rewrite(self, archive, change, filename='altered.tar.gz'):
        output = self.backups / filename
        with tarfile.open(output, 'w:gz') as destination:
            for member, data in change(self.archive_members(archive)):
                if member.isfile():
                    member.size = len(data)
                    destination.addfile(member, io.BytesIO(data))
                else:
                    destination.addfile(member)
        output.chmod(0o600)
        return output

    def test_complete_checkpoint_archive_and_restore_delegate_without_live_writes(self):
        root_inode = (self.runtime / 'managed').stat().st_ino
        lock_inode = (self.runtime / 'managed/.lock').stat().st_ino
        archive = self.backup()
        members = dict((member.name, data) for member, data in self.archive_members(archive))
        manifest = json.loads(members['BACKUP.json'])
        checkpoint = json.loads(members['checkpoint/CHECKPOINT.json'])
        self.assertEqual(manifest['format'], 3)
        self.assertEqual(checkpoint['files'][0]['mtime_ns'], 1700000000123456789)
        self.assertEqual(manifest['checkpoint_sha256'], hashlib.sha256(members['checkpoint/CHECKPOINT.json']).hexdigest())
        self.assertIn(b'private-controller-secret', members['checkpoint/context/agent.token'])
        self.assertNotIn('checkpoint/managed/.lock', members)
        self.assertEqual(archive.stat().st_mode & 0o777, 0o600)
        (self.runtime / 'managed/store.json').write_text('changed-disk-by-fixture')
        with mock.patch.object(ops, 'maintenance', side_effect=self.helper):
            result = ops.restore(archive, self.runtime, offline=True, agent_binary='/native-agent')
        self.assertEqual(result['state'], 'pending')
        self.assertEqual(result['manifest_digest'], manifest['checkpoint_sha256'])
        self.assertEqual(self.calls[-1][0][0], 'restore-install')
        self.assertEqual((self.runtime / 'managed/store.json').read_text(), 'changed-disk-by-fixture', 'only native helper may install under its lease')
        self.assertEqual((self.runtime / 'managed').stat().st_ino, root_inode)
        self.assertEqual((self.runtime / 'managed/.lock').stat().st_ino, lock_inode)
        self.assertEqual(list(self.root.glob('.frp-restore-*')), [])
        self.assertEqual(list(self.backups.glob('.frp-checkpoint-*')), [])
        with mock.patch.object(ops, 'maintenance', side_effect=self.helper):
            confirmed = ops.restore_confirm(self.runtime, RESTORE_EPOCH, result['manifest_digest'], offline=True)
        self.assertEqual(confirmed['state'], 'confirmed')
        self.assertNotIn('active', json.dumps(confirmed))

    def test_offline_same_path_and_generated_single_agent_are_required(self):
        with mock.patch.object(ops, 'maintenance') as helper:
            with self.assertRaisesRegex(ValueError, 'offline'):
                ops.backup(self.runtime, self.backups / 'missing-offline.tar.gz')
            helper.assert_not_called()
        archive = self.backup()
        target = self.root / 'other'
        target.mkdir(mode=0o700)
        with mock.patch.object(ops, 'maintenance') as helper:
            for directory, offline in ((self.runtime, False), (target, True)):
                with self.subTest(directory=directory, offline=offline), self.assertRaises(ValueError):
                    ops.restore(archive, directory, offline=offline)
            with self.assertRaises(ValueError):
                ops.restore_confirm(self.runtime, RESTORE_EPOCH, 'a' * 64)
            helper.assert_not_called()
        (self.runtime / 'installation.json').write_text(local.installation_metadata(['server', 'agent'], True))
        with self.assertRaisesRegex(ValueError, 'single-Agent'):
            self.backup('dual.tar.gz')

    def test_new_backup_preserves_prior_activated_restore_material_for_native_validation(self):
        raw = '{"state":"confirmed","activated":true}\n'
        local.private(self.runtime / 'managed/restore.json', raw)
        archive = self.backup()
        members = {member.name: data for member, data in self.archive_members(archive)}
        self.assertEqual(members['checkpoint/managed/restore.json'], raw.encode())
        with mock.patch.object(ops, 'maintenance', side_effect=self.helper):
            restored = ops.restore(archive, self.runtime, offline=True)
        self.assertEqual(restored['state'], 'pending')
        self.assertEqual((self.runtime / 'managed/restore.json').read_text(), raw, 'Python must never install a saved old marker')

    def test_failed_or_inconsistent_checkpoint_never_publishes_archive(self):
        for kind in ('busy', 'digest', 'count', 'permission', 'hardlink', 'extra', 'missing'):
            path = self.backups / (kind + '.tar.gz')
            def helper(arguments, folder, binary):
                if kind == 'busy':
                    raise ValueError('managed checkpoint is locked')
                result = self.helper(arguments, folder, binary)
                output = Path(arguments[arguments.index('--output') + 1])
                if kind == 'digest':
                    result['manifest_digest'] = 'c' * 64
                elif kind == 'count':
                    result['file_count'] += 1
                elif kind == 'permission':
                    (output / 'context/agent.token').chmod(0o644)
                elif kind == 'hardlink':
                    os.link(output / 'context/agent.token', output / 'extra')
                elif kind == 'extra':
                    local.private(output / 'unknown', 'extra')
                elif kind == 'missing':
                    (output / 'managed/identity.json').unlink()
                return result
            with self.subTest(kind=kind), mock.patch.object(ops, 'maintenance', side_effect=helper):
                with self.assertRaises((ValueError, FileNotFoundError)):
                    ops.backup(self.runtime, path, offline=True)
            self.assertFalse(path.exists())
        self.assertEqual(list(self.backups.glob('.frp-checkpoint-*')), [])

    def test_restore_rejects_hostile_or_incomplete_archives_before_native_helper(self):
        archive = self.backup()
        for kind in ('traversal', 'symlink', 'duplicate', 'hash', 'missing', 'extra', 'mode', 'format', 'duplicate-json', 'checkpoint-digest'):
            def alter(members):
                members = list(members)
                if kind in ('traversal', 'extra'):
                    member = tarfile.TarInfo('../escape' if kind == 'traversal' else 'checkpoint/managed/.lock')
                    member.mode = 0o600
                    members.append((member, b'bad'))
                elif kind == 'symlink':
                    member = tarfile.TarInfo('checkpoint/context/agent.token')
                    member.type, member.linkname = tarfile.SYMTYPE, '/tmp/escape'
                    members.append((member, b''))
                elif kind == 'duplicate':
                    members.append(members[-1])
                elif kind == 'missing':
                    members = [(member, data) for member, data in members if member.name != 'checkpoint/context/agent.token']
                else:
                    for index, (member, data) in enumerate(members):
                        if kind == 'hash' and member.name == 'checkpoint/context/agent.token':
                            members[index] = member, b'changed'
                        elif kind == 'mode' and member.name == 'checkpoint/context/agent.token':
                            member.mode = 0o644
                        elif member.name == 'BACKUP.json':
                            value = json.loads(data)
                            if kind == 'format':
                                value['kind'] = 'unknown'
                            elif kind == 'checkpoint-digest':
                                value['checkpoint_sha256'] = '0' * 64
                            if kind == 'duplicate-json':
                                data = b'{"format":3,' + data[1:]
                            else:
                                data = json.dumps(value).encode()
                            members[index] = member, data
                return members
            bad = self.rewrite(archive, alter, kind + '.tar.gz')
            with self.subTest(kind=kind), mock.patch.object(ops, 'maintenance') as helper:
                with self.assertRaises((ValueError, FileNotFoundError)):
                    ops.restore(bad, self.runtime, offline=True)
                helper.assert_not_called()
        self.assertFalse((self.root / 'escape').exists())

    def test_checkpoint_size_and_count_budgets_and_exact_response_state(self):
        with mock.patch.object(ops, 'MANAGED_MAX_BYTES', 8), self.assertRaises(ValueError):
            self.backup('bytes.tar.gz')
        with mock.patch.object(ops, 'MANAGED_MAX_FILES', 2), self.assertRaises(ValueError):
            self.backup('count.tar.gz')
        archive = self.backup()
        for result in ({'code': 'ok', 'state': 'active'}, {'code': 'ok', 'state': 'pending', 'epoch': 'not-uuid'}, {'code': 'ok'}):
            with self.subTest(result=result), mock.patch.object(ops, 'maintenance', return_value=result):
                with self.assertRaisesRegex(ValueError, 'gate state'):
                    ops.restore(archive, self.runtime, offline=True)
        self.assertEqual(list(self.root.glob('.frp-restore-*')), [])

    def test_native_subprocess_has_bounded_output_timeout_and_private_errors(self):
        executable = self.root / 'native-helper'
        def script(content):
            executable.write_text('#!' + sys.executable + '\n' + content)
            executable.chmod(0o700)
        script('import json,sys\nassert sys.argv[-1]=="--offline"\nprint(json.dumps({"code":"ok","value":1}))\n')
        self.assertEqual(ops.maintenance(['checkpoint'], self.runtime, executable)['value'], 1)
        for content in ('import sys\nprint("private-controller-secret",file=sys.stderr)\nsys.exit(1)\n',
                        'print("x"*70000)\n', 'print("not JSON")\n'):
            script(content)
            with self.assertRaises(ValueError) as failure:
                ops.maintenance(['checkpoint'], self.runtime, executable)
            self.assertNotIn('private-controller-secret', str(failure.exception))
        script('import time\ntime.sleep(2)\n')
        with mock.patch.object(ops, 'MAINTENANCE_TIMEOUT', 0.05), self.assertRaisesRegex(ValueError, 'time budget'):
            ops.maintenance(['checkpoint'], self.runtime, executable)
        executable.chmod(0o722)
        with self.assertRaisesRegex(ValueError, 'trusted'):
            ops.maintenance(['checkpoint'], self.runtime, executable)

    def test_manifest_json_rejects_duplicate_deep_and_non_json_values(self):
        for raw in ('{"format":3,"format":3}', '[' * 1000 + '0' + ']' * 1000,
                    '{"value":NaN}', '{"value":Infinity}'):
            with self.subTest(raw=raw[:32]), self.assertRaises(ValueError):
                ops.strict_json(raw)

    def test_extension_metadata_is_rejected_before_unbounded_tar_processing(self):
        archive = self.backup()
        members = self.archive_members(archive)
        for kind in ('pax', 'gnu-longname'):
            output = self.backups / (kind + '.tar.gz')
            with tarfile.open(output, 'w:gz', format=tarfile.PAX_FORMAT if kind == 'pax' else tarfile.GNU_FORMAT) as destination:
                for index, (member, data) in enumerate(members):
                    copied = tarfile.TarInfo(member.name)
                    copied.size, copied.mode = len(data), 0o600
                    if index == 0:
                        if kind == 'pax':
                            copied.pax_headers = {'comment': 'x' * (2 * 1024 * 1024)}
                        else:
                            copied.name = 'x' * (2 * 1024 * 1024)
                    destination.addfile(copied, io.BytesIO(data))
            output.chmod(0o600)
            with self.subTest(kind=kind), mock.patch.object(ops, 'MANAGED_MAX_BYTES', 8192), mock.patch.object(ops, 'maintenance') as helper:
                with self.assertRaisesRegex(ValueError, 'USTAR'):
                    ops.restore(output, self.runtime, offline=True)
                helper.assert_not_called()
        reader = ops.BackupReader(io.BytesIO(b'x' * 129), 128)
        with self.assertRaisesRegex(ValueError, 'size budget'):
            reader.read(1024 * 1024)
        with mock.patch.object(ops.time, 'monotonic', side_effect=[0, 31]):
            reader = ops.BackupReader(io.BytesIO(b'x'), 128)
            with self.assertRaisesRegex(ValueError, 'time budget'):
                reader.read(1)

    def test_operations_cli_never_prints_private_paths_or_helper_errors(self):
        argv = ['ops.py', 'backup', '--directory', str(self.runtime), '--output', str(self.backups / 'private.tar.gz'), '--offline']
        output, errors = io.StringIO(), io.StringIO()
        with mock.patch.object(sys, 'argv', argv), mock.patch.object(sys, 'stdout', output), mock.patch.object(sys, 'stderr', errors), mock.patch.object(ops, 'backup', return_value=self.backups / 'private.tar.gz'):
            self.assertEqual(ops.main(), 0)
        self.assertNotIn(str(self.root), output.getvalue() + errors.getvalue())
        output, errors = io.StringIO(), io.StringIO()
        with mock.patch.object(sys, 'argv', argv), mock.patch.object(sys, 'stdout', output), mock.patch.object(sys, 'stderr', errors), mock.patch.object(ops, 'backup', side_effect=ValueError('private-controller-secret ' + str(self.runtime))):
            self.assertEqual(ops.main(), 1)
        self.assertNotIn(str(self.root), output.getvalue() + errors.getvalue())
        self.assertNotIn('private-controller-secret', output.getvalue() + errors.getvalue())


if __name__ == '__main__':
    unittest.main()
