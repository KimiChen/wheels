"""Version3 packaging boundaries; native code owns policy semantics and writes.

Synthetic helper replies exercise the Python envelope, not native relocation.
Native checkpoint/CLI tests separately validate authorization and FRP equivalence.
"""
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
ENV_VALUE = 'synthetic-template-value-never-in-backup'


class RelocationWrapperTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.root = Path(self.temporary.name).resolve(); self.root.chmod(0o700)
        self.source = self.root / 'source'; self.target = self.root / 'target'
        for folder in (self.source, self.target):
            folder.mkdir(mode=0o700); (folder / 'managed').mkdir(mode=0o700)
            local.private(folder / 'installation.json', local.installation_metadata(['agent'], True))
            local.private(folder / 'agent.toml', '# preserved {{ .Envs.LOCAL_VALUE }}\n')
        self.backups = self.root / 'backups'; self.backups.mkdir(mode=0o700)
        self.source_policy = self.root / 'source-policy.json'
        self.target_policy = self.root / 'target-policy.json'
        for folder, path in ((self.source, self.source_policy), (self.target, self.target_policy)):
            local.private(path, json.dumps({'version': 1, 'config_file': str(folder / 'agent.toml'),
                'working_dir': str(folder), 'store_file': str(folder / 'managed/store.json'),
                'roots': [{'id': 'installation', 'path': str(folder)}], 'files': []}))
        self.env = self.root / 'local-values.json'; local.private(self.env, json.dumps({'LOCAL_VALUE': ENV_VALUE}))
        self.source_env = self.root / 'source-values.json'; local.private(self.source_env, '{"LOCAL_VALUE":"synthetic-source-value"}')
        self.payload = {'GRAPH.json': b'{}', 'POLICY.json': self.source_policy.read_bytes(),
            'context/f000001': (self.source / 'agent.toml').read_bytes(),
            'context/f000002': (self.source / 'installation.json').read_bytes(),
            'managed/identity.json': b'{}', 'managed/store.json': b'{"proxies":[],"visitors":[]}'}
        self.version = 3; self.calls = []

    def tearDown(self): self.temporary.cleanup()

    def helper(self, arguments, folder, binary):
        self.calls.append((arguments, folder, binary))
        if arguments[0] == 'checkpoint':
            output = Path(arguments[arguments.index('--output') + 1]); output.mkdir(mode=0o700)
            entries = []
            for name, data in sorted(self.payload.items()):
                (output / name).parent.mkdir(mode=0o700, parents=True, exist_ok=True)
                local.private(output / name, data.decode())
                entries.append({'path': name, 'size': len(data), 'sha256': hashlib.sha256(data).hexdigest(), 'mtime_ns': 1700000000123456789})
            manifest = {'version': self.version, 'kind': 'frp-managed-checkpoint', 'id': ID, 'created_at_ms': 1700000000000,
                'root': str(self.source / 'managed'), 'store_name': 'store.json', 'config_file': str(self.source / 'agent.toml'),
                'cwd': str(self.source), 'context_revision': 'a' * 64, 'service_id': ID, 'store_exists': True,
                'store_digest': 'b' * 64, 'graph_sha256': hashlib.sha256(self.payload['GRAPH.json']).hexdigest(), 'files': entries}
            if self.version == 3:
                manifest['policy_sha256'] = hashlib.sha256(self.payload['POLICY.json']).hexdigest()
                manifest['context_sources'] = [{'path': str(self.source / name), 'payload': f'context/f{index:06d}'}
                    for index, name in enumerate(('agent.toml', 'installation.json'), 1)]
            raw = json.dumps(manifest).encode(); local.private(output / 'CHECKPOINT.json', raw.decode())
            return {'code': 'ok', 'checkpoint_id': ID, 'manifest_digest': hashlib.sha256(raw).hexdigest(),
                'file_count': len(entries), 'total_bytes': sum(entry['size'] for entry in entries)}
        if arguments[0] == 'restore-install':
            checkpoint = Path(arguments[arguments.index('--checkpoint') + 1])
            self.assertEqual(checkpoint.stat().st_mode & 0o777, 0o700)
            self.assertNotEqual(checkpoint, folder)
            self.assertEqual(set(path.relative_to(checkpoint).as_posix() for path in checkpoint.rglob('*') if path.is_file()),
                set(self.payload) | {'CHECKPOINT.json'})
            for name, data in self.payload.items():
                self.assertEqual((checkpoint / name).read_bytes(), data)
                self.assertEqual((checkpoint / name).stat().st_mode & 0o777, 0o600)
            self.assertEqual(arguments[arguments.index('--root') + 1], str(folder / 'managed'))
        return {'code': 'ok', 'state': 'confirmed' if arguments[0] == 'restore-confirm' else 'pending',
            'epoch': ID, 'service_id': ID, 'manifest_digest': arguments[arguments.index('--manifest-digest') + 1]}

    def backup(self, name='snapshot.tar.gz'):
        with mock.patch.object(ops, 'maintenance', side_effect=self.helper):
            return ops.backup(self.source, self.backups / name, offline=True, policy=self.source_policy, env_file=self.env)

    def read_archive(self, path):
        with tarfile.open(path, 'r:gz') as source:
            return [(member.name, source.extractfile(member).read()) for member in source]

    def write_archive(self, name, members):
        path = self.backups / name
        with tarfile.open(path, 'w:gz', format=tarfile.USTAR_FORMAT) as output:
            for name, data in members:
                member = tarfile.TarInfo(name); member.mode = 0o600; member.size = len(data)
                output.addfile(member, io.BytesIO(data))
        path.chmod(0o600); return path

    def change_manifest(self, members, change):
        members = list(members); manifest = json.loads(members[1][1]); change(manifest)
        raw = json.dumps(manifest).encode(); envelope = json.loads(members[0][1]); envelope['manifest_sha256'] = hashlib.sha256(raw).hexdigest()
        members[:2] = [('BACKUP.json', json.dumps(envelope).encode()), ('payload/000000', raw)]
        return members

    def test_backup_v3_keeps_opaque_payloads_and_never_packages_local_environment(self):
        archive = self.backup(); members = self.read_archive(archive)
        self.assertEqual([name for name, _ in members], ['BACKUP.json'] + [f'payload/{index:06d}' for index in range(len(self.payload) + 1)])
        self.assertEqual(json.loads(members[1][1])['version'], 3)
        combined = b'\n'.join(data for _, data in members)
        self.assertNotIn(ENV_VALUE.encode(), combined); self.assertNotIn(str(self.env).encode(), combined)
        arguments = self.calls[0][0]
        self.assertEqual(arguments[arguments.index('--env-file') + 1], str(self.env))
        self.assertEqual(arguments[arguments.index('--policy') + 1], str(self.source_policy))

    def test_relocation_requires_both_local_policies_even_when_archive_has_policy(self):
        archive = self.backup()
        with mock.patch.object(ops, 'maintenance') as helper:
            for folder in (self.source, self.target):
                for arguments in ({}, {'policy': self.target_policy}, {'source_policy': self.source_policy}):
                    with self.subTest(same_path=folder == self.source, arguments=tuple(arguments)), self.assertRaises(ValueError):
                        ops.restore(archive, folder, offline=True, **arguments)
            helper.assert_not_called()

    def test_native_installer_alone_receives_local_policy_and_can_write_runtime(self):
        archive = self.backup(); before = (self.target / 'agent.toml').read_bytes()
        with mock.patch.object(ops, 'maintenance', side_effect=self.helper):
            result = ops.restore(archive, self.target, offline=True, policy=self.target_policy,
                source_policy=self.source_policy, env_file=self.env, source_env_file=self.source_env)
        self.assertEqual(result['state'], 'pending'); self.assertEqual((self.target / 'agent.toml').read_bytes(), before)
        self.assertEqual(list((self.target / 'managed').iterdir()), [])
        arguments = self.calls[-1][0]
        for flag, path in (('--policy', self.target_policy), ('--source-policy', self.source_policy),
                ('--env-file', self.env), ('--source-env-file', self.source_env)):
            self.assertEqual(arguments[arguments.index(flag) + 1], str(path))
        self.assertEqual(list(self.root.glob('.frp-indexed-*')), [])

    def test_native_policy_denial_leaves_runtime_and_external_files_untouched(self):
        outside = self.root / 'not-authorized'; local.private(outside, 'do not replace')
        archive = self.backup(); before = (self.target / 'agent.toml').read_bytes()
        def deny(arguments, folder, binary):
            self.assertEqual(arguments[arguments.index('--policy') + 1], str(self.target_policy))
            self.assertEqual(arguments[arguments.index('--source-policy') + 1], str(self.source_policy))
            raise ValueError('synthetic native policy denied')
        with mock.patch.object(ops, 'maintenance', side_effect=deny), self.assertRaisesRegex(ValueError, 'policy denied'):
            ops.restore(archive, self.target, offline=True, policy=self.target_policy, source_policy=self.source_policy)
        self.assertEqual(outside.read_text(), 'do not replace'); self.assertEqual((self.target / 'agent.toml').read_bytes(), before)
        self.assertEqual(list((self.target / 'managed').iterdir()), [])

    def test_sidecar_four_mib_is_preserved_and_larger_is_rejected(self):
        # Contents here are synthetic: the native helper owns sidecar semantics.
        self.payload['managed/context-history.json'] = b'x' * (4 * 1024 * 1024)
        archive = self.backup()
        with mock.patch.object(ops, 'maintenance', side_effect=self.helper):
            self.assertEqual(ops.restore(archive, self.target, offline=True, policy=self.target_policy,
                source_policy=self.source_policy)['state'], 'pending')
        members = self.read_archive(archive)
        def oversized(manifest):
            next(entry for entry in manifest['files'] if entry['path'] == 'managed/context-history.json')['size'] += 1
        path = self.write_archive('oversized.tar.gz', self.change_manifest(members, oversized))
        with mock.patch.object(ops, 'maintenance') as helper, self.assertRaises(ValueError):
            ops.restore(path, self.target, offline=True, policy=self.target_policy, source_policy=self.source_policy)
        helper.assert_not_called()
        self.payload['managed/context-history.json'] += b'x'
        with self.assertRaises(ValueError): self.backup('oversized-output.tar.gz')
        self.assertFalse((self.backups / 'oversized-output.tar.gz').exists())

    def test_ordinary_dependency_cannot_use_sidecar_size_exception(self):
        self.payload['context/f000001'] = b'x' * (1024 * 1024 + 1)
        with self.assertRaises(ValueError): self.backup()
        self.assertFalse((self.backups / 'snapshot.tar.gz').exists())

    def test_malformed_source_maps_are_rejected_before_native(self):
        original = self.read_archive(self.backup())
        mutations = {
            'duplicate': lambda m: m['context_sources'][1].update(path=m['context_sources'][0]['path']),
            'relative': lambda m: m['context_sources'][0].update(path='relative/agent.toml'),
            'double_slash': lambda m: m['context_sources'][0].update(path='/' + m['context_sources'][0]['path']),
            'missing': lambda m: m['context_sources'].pop(),
            'opaque_mismatch': lambda m: m['context_sources'][0].update(payload='context/agent.toml'),
            'traversal': lambda m: m['files'][0].update(path='../outside'),
            'source_identity': lambda m: m.update(cwd=str(self.target)),
            'policy_digest': lambda m: m.update(policy_sha256='0' * 64),
        }
        for name, mutate in mutations.items():
            path = self.write_archive(name + '.tar.gz', self.change_manifest(original, mutate))
            with self.subTest(name=name), mock.patch.object(ops, 'maintenance') as helper, self.assertRaises((ValueError, OSError)):
                ops.restore(path, self.target, offline=True, policy=self.target_policy, source_policy=self.source_policy)
            helper.assert_not_called()

    def test_policy_mode_rejects_version2_and_non_object_manifests(self):
        original = self.read_archive(self.backup())
        def downgrade(manifest):
            manifest['version'] = 2
            manifest.pop('policy_sha256'); manifest.pop('context_sources')
        cases = [self.change_manifest(original, downgrade)]
        raw = b'[]'; envelope = json.loads(original[0][1]); envelope['manifest_sha256'] = hashlib.sha256(raw).hexdigest()
        cases.append([('BACKUP.json', json.dumps(envelope).encode()), ('payload/000000', raw)] + original[2:])
        for index, members in enumerate(cases):
            path = self.write_archive(str(index) + '.tar.gz', members)
            with mock.patch.object(ops, 'maintenance') as helper, self.assertRaises(ValueError):
                ops.restore(path, self.target, offline=True, policy=self.target_policy, source_policy=self.source_policy)
            helper.assert_not_called()

    def test_policy_checkpoint_reply_must_match_requested_version(self):
        self.version = 2
        self.payload['context/agent.toml'] = self.payload.pop('context/f000001')
        self.payload['context/installation.json'] = self.payload.pop('context/f000002')
        self.payload.pop('POLICY.json')
        with self.assertRaisesRegex(ValueError, 'native checkpoint response'):
            self.backup()
        self.assertFalse((self.backups / 'snapshot.tar.gz').exists())

    def test_offline_and_environment_policy_requirements_fail_before_helper(self):
        with mock.patch.object(ops, 'maintenance') as helper:
            with self.assertRaises(ValueError): ops.backup(self.source, self.backups / 'bad.tar.gz', policy=self.source_policy)
            with self.assertRaises(ValueError): ops.backup(self.source, self.backups / 'bad.tar.gz', offline=True, env_file=self.env)
            with self.assertRaises(ValueError): ops.restore_confirm(self.source, ID, 'a' * 64, offline=True, env_file=self.env)
            for arguments in ({'policy': ''}, {'policy': self.source_policy, 'env_file': ''}):
                with self.assertRaises(ValueError): ops_checkpoint.policy_arguments(**arguments)
            helper.assert_not_called()

    def test_restore_confirmation_passes_local_materials_without_returning_them(self):
        with mock.patch.object(ops, 'maintenance', side_effect=self.helper):
            result = ops.restore_confirm(self.target, ID, 'a' * 64, offline=True, policy=self.target_policy, env_file=self.env)
        self.assertEqual(result['state'], 'confirmed'); self.assertNotIn(str(self.env), json.dumps(result))
        self.assertEqual(self.calls[-1][0][-4:], ['--policy', str(self.target_policy), '--env-file', str(self.env)])


# Opt in to the actual assembled CLI; ordinary Python runs stay independent of
# Go build products. This is offline wrapper/native acceptance, not forwarding.
NATIVE_AGENT = os.environ.get('FRP_PLUS_TEST_AGENT')


@unittest.skipUnless(NATIVE_AGENT, 'set FRP_PLUS_TEST_AGENT for the actual native maintenance CLI')
class NativeRelocationWrapperTests(unittest.TestCase):
    def setUp(self):
        import uuid
        self.temporary = tempfile.TemporaryDirectory()
        self.root = Path(self.temporary.name).resolve(); self.root.chmod(0o700)
        self.binary = Path(NATIVE_AGENT).resolve()
        self.source = self.root / 'source'; self.target = self.root / 'target'
        for folder in (self.source, self.target):
            folder.mkdir(mode=0o700); (folder / 'managed').mkdir(mode=0o700)
        (self.source / 'managed/operations').mkdir(mode=0o700)
        local.private(self.source / 'managed/identity.json', json.dumps({'version': 1, 'service_id': str(uuid.uuid4()), 'store_name': 'store.json'}))
        self.ca_source = self.root / 'external-source'; self.ca_target = self.root / 'external-target'
        self.ca_source.mkdir(mode=0o700); self.ca_target.mkdir(mode=0o700)
        local.private(self.ca_source / 'ca.pem', 'synthetic certificate bytes retained exactly')
        (self.source / 'includes').mkdir(mode=0o700); (self.source / 'plugin').mkdir(mode=0o700)
        local.private(self.source / 'includes/site.toml', "[[proxies]]\nname='included'\ntype='tcp'\nlocalPort=8080\nremotePort=18080\nenabled=false\n")
        local.private(self.source / 'plugin/site.crt', 'synthetic plugin certificate')
        local.private(self.source / 'plugin/site.key', 'synthetic plugin private key')
        local.private(self.source / 'agent.token', 'synthetic-monitor-token')
        local.private(self.source / 'installation.json', local.installation_metadata(['agent'], True))
        main = "serverAddr='127.0.0.1'\nincludes=['includes/*.toml']\n[auth]\nmethod='token'\ntoken='{{ .Envs.LOCAL_TOKEN }}'\n"
        main += '[transport.tls]\ntrustedCaFile=' + json.dumps(str(self.ca_source / 'ca.pem')) + '\n'
        main += '[store]\npath=' + json.dumps(str(self.source / 'managed/store.json')) + '\n'
        main += "[telemetry]\nenabled=true\nendpoint='ws://127.0.0.1:17401/agent/v1/ws'\ntokenFile='{{ .Envs.ROOT }}/agent.token'\nallowInsecureLoopback=true\n"
        main += '[telemetry.configManagement]\nenabled=true\nroot=' + json.dumps(str(self.source / 'managed')) + '\n'
        local.private(self.source / 'agent.toml', main)
        local.private(self.source / 'managed/store.json', json.dumps({'proxies': [{'name': 'stored-cert', 'type': 'tcp', 'enabled': False,
            'plugin': {'type': 'https2http', 'localAddr': '127.0.0.1:8080', 'crtPath': str(self.source / 'plugin/site.crt'), 'keyPath': str(self.source / 'plugin/site.key')}}], 'visitors': []}))
        self.source_policy = self.root / 'source-policy.json'; self.target_policy = self.root / 'target-policy.json'
        self.source_env = self.root / 'source-environment.json'; self.target_env = self.root / 'target-environment.json'
        for folder, ca, policy, env in ((self.source, self.ca_source, self.source_policy, self.source_env),
                (self.target, self.ca_target, self.target_policy, self.target_env)):
            local.private(policy, json.dumps({'version': 1, 'config_file': str(folder / 'agent.toml'), 'working_dir': str(folder),
                'store_file': str(folder / 'managed/store.json'), 'roots': [{'id': 'installation', 'path': str(folder)}],
                'files': [{'id': 'external-ca', 'path': str(ca / 'ca.pem')}]}))
            local.private(env, json.dumps({'ROOT': str(folder), 'LOCAL_TOKEN': ENV_VALUE}))

    def tearDown(self): self.temporary.cleanup()

    def backup(self):
        return ops.backup(self.source, self.root / 'native.tar.gz', offline=True, agent_binary=self.binary,
            policy=self.source_policy, env_file=self.source_env)

    def restore(self, archive):
        return ops.restore(archive, self.target, offline=True, agent_binary=self.binary, policy=self.target_policy,
            source_policy=self.source_policy, env_file=self.target_env, source_env_file=self.source_env)

    def test_actual_cli_relocates_sealed_dependencies_without_loading_source_disk(self):
        import shutil
        archive = self.backup()
        with tarfile.open(archive, 'r:gz') as bundle:
            data = b'\n'.join(bundle.extractfile(member).read() for member in bundle)
        self.assertNotIn(ENV_VALUE.encode(), data); self.assertNotIn(str(self.source_env).encode(), data)
        shutil.rmtree(self.source); shutil.rmtree(self.ca_source)
        result = self.restore(archive)
        self.assertEqual(result['state'], 'pending')
        self.assertEqual((self.ca_target / 'ca.pem').read_text(), 'synthetic certificate bytes retained exactly')
        self.assertEqual((self.target / 'plugin/site.key').read_text(), 'synthetic plugin private key')
        self.assertTrue((self.target / 'includes/site.toml').exists())
        main = (self.target / 'agent.toml').read_text()
        self.assertIn('{{ .Envs.LOCAL_TOKEN }}', main); self.assertNotIn(str(self.source), main)
        self.assertNotIn(ENV_VALUE, main)
        store = json.loads((self.target / 'managed/store.json').read_text())
        self.assertEqual(store['proxies'][0]['plugin']['keyPath'], str(self.target / 'plugin/site.key'))
        self.assertTrue((self.target / 'managed/context-history.json').exists())
        original = {str(path): (path.read_bytes(), path.stat().st_mtime_ns) for path in self.target.rglob('*') if path.is_file()}
        self.assertEqual(self.restore(archive), result)
        self.assertEqual(original, {str(path): (path.read_bytes(), path.stat().st_mtime_ns) for path in self.target.rglob('*') if path.is_file()})
        with self.assertRaisesRegex(ValueError, 'maintenance failed'):
            ops.restore_confirm(self.target, result['epoch'], result['manifest_digest'], offline=True,
                agent_binary=self.binary, policy=self.target_policy, env_file=self.target_env)

    def test_actual_cli_rejects_changed_nonpath_environment_before_writes(self):
        archive = self.backup()
        self.target_env.write_text(json.dumps({'ROOT': str(self.target), 'LOCAL_TOKEN': 'different-business-token'}))
        with self.assertRaisesRegex(ValueError, 'maintenance failed'): self.restore(archive)
        self.assertEqual(list(self.target.rglob('*')), [self.target / 'managed'])
        self.assertEqual(list(self.ca_target.iterdir()), [])

    def test_actual_cli_does_not_archive_an_environment_file_used_as_dependency(self):
        # The local values file is also the rendered telemetry tokenFile here;
        # explicit path collision must fail even though the policy permits it.
        (self.source / 'agent.token').write_text(self.source_env.read_text())
        with self.assertRaisesRegex(ValueError, 'maintenance failed'):
            ops.backup(self.source, self.root / 'collision.tar.gz', offline=True, agent_binary=self.binary,
                policy=self.source_policy, env_file=self.source / 'agent.token')
        self.assertFalse((self.root / 'collision.tar.gz').exists())


if __name__ == '__main__': unittest.main()
