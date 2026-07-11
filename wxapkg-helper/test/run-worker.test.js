import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { afterEach, describe, it } from 'node:test';
import {
  assertSafeReadPermissionPath,
  disposeDecoderWorkerPreparation,
  findSafeCustomPolyfillRoots,
  prepareDecoderWorker,
  runDecoderWorker,
  snapshotCustomPolyfills
} from '../src/decoder/run-worker.js';

const temporaryRoots = [];

afterEach(async () => {
  await Promise.all(temporaryRoots.splice(0).map((root) => (
    fs.rm(root, { recursive: true, force: true })
  )));
});

describe('custom polyfill read roots', () => {
  it('rejects Node permission wildcard expansion in user-controlled paths', () => {
    assert.throws(
      () => assertSafeReadPermissionPath('/tmp/package*'),
      (error) => error?.code === 'WXAPKG_UNSAFE_PERMISSION_PATH'
    );
    assert.doesNotThrow(() => assertSafeReadPermissionPath('/tmp/package.wxapkg'));
  });

  it('allows an existing tree containing only real directories and regular files', async () => {
    const root = await makeTemporaryRoot();
    const packagePath = path.join(root, '__APP__.wxapkg');
    const polyfillRoot = path.join(root, 'polyfill');
    await fs.writeFile(packagePath, 'package');
    await fs.mkdir(path.join(polyfillRoot, 'nested'), { recursive: true });
    await fs.writeFile(path.join(polyfillRoot, 'nested/helper.js'), 'module.exports = {};');

    assert.deepEqual(
      await findSafeCustomPolyfillRoots([{ inputPath: packagePath }]),
      [polyfillRoot]
    );
  });

  it('does not authorize missing roots and rejects symlinks to external files', {
    skip: process.platform === 'win32'
  }, async () => {
    const root = await makeTemporaryRoot();
    const packagePath = path.join(root, '__APP__.wxapkg');
    const outside = path.join(root, 'secret.txt');
    await fs.writeFile(packagePath, 'package');
    await fs.writeFile(outside, 'secret');

    assert.deepEqual(await findSafeCustomPolyfillRoots([{ inputPath: packagePath }]), []);

    const polyfillRoot = path.join(root, 'polyfill');
    await fs.mkdir(polyfillRoot);
    await fs.symlink(outside, path.join(polyfillRoot, 'helper.js'));

    await assert.rejects(
      findSafeCustomPolyfillRoots([{ inputPath: packagePath }]),
      (error) => error?.code === 'WXAPKG_UNSAFE_POLYFILL'
    );
  });

  it('uses a private snapshot after the source directory changes', {
    skip: process.platform === 'win32'
  }, async () => {
    const root = await makeTemporaryRoot();
    const packagePath = path.join(root, '__APP__.wxapkg');
    const polyfillRoot = path.join(root, 'polyfill');
    const replacement = path.join(root, 'replacement');
    await fs.writeFile(packagePath, 'package');
    await fs.mkdir(polyfillRoot);
    await fs.writeFile(path.join(polyfillRoot, 'helper.js'), 'module.exports = "safe";');
    await fs.mkdir(replacement);
    await fs.writeFile(path.join(replacement, 'helper.js'), 'module.exports = "changed";');

    const snapshot = await snapshotCustomPolyfills([{ inputPath: packagePath }]);
    temporaryRoots.push(snapshot.stageRoot);
    await fs.rm(polyfillRoot, { recursive: true });
    await fs.symlink(replacement, polyfillRoot);

    assert.equal(
      await fs.readFile(path.join(snapshot.mappings[0].root, 'helper.js'), 'utf8'),
      'module.exports = "safe";'
    );
    assert.notEqual(snapshot.mappings[0].root, polyfillRoot);
  });

  it('rejects forged, released, and cross-task worker preparations', async () => {
    const root = await makeTemporaryRoot();
    const packagePath = path.join(root, '__APP__.wxapkg');
    const config = {
      packageInfos: [{ inputPath: packagePath }],
      outputPath: path.join(root, 'output')
    };
    await fs.writeFile(packagePath, 'package');

    await assert.rejects(
      runDecoderWorker(config, { preparation: Object.freeze({}) }),
      (error) => error?.code === 'WXAPKG_INVALID_WORKER_PREPARATION'
    );

    const preparation = await prepareDecoderWorker(config);
    await assert.rejects(
      runDecoderWorker({ ...config, outputPath: path.join(root, 'other-output') }, { preparation }),
      (error) => error?.code === 'WXAPKG_INVALID_WORKER_PREPARATION'
    );

    await disposeDecoderWorkerPreparation(preparation);
    await assert.rejects(
      runDecoderWorker(config, { preparation }),
      (error) => error?.code === 'WXAPKG_INVALID_WORKER_PREPARATION'
    );
  });
});

async function makeTemporaryRoot() {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'wxapkg-polyfill-'));
  temporaryRoots.push(root);
  return root;
}
