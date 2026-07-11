import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { afterEach, beforeEach, describe, it } from 'node:test';
import { decodeWxapkg } from '../src/decode.js';
import { buildWxapkg } from '../test-support/wxapkg-fixture.js';

let root;

describe('decodeWxapkg execution plan', () => {
  beforeEach(async () => {
    root = await fs.mkdtemp(path.join(os.tmpdir(), 'wxapkg-helper-decode-'));
  });

  afterEach(async () => {
    await fs.rm(root, { recursive: true, force: true });
  });

  it('builds an internal dry-run plan without an external command', async () => {
    const input = path.join(root, '__APP__.wxapkg');
    const output = path.join(root, 'output');
    await fs.writeFile(input, 'dry-run does not parse package contents');

    const result = await decodeWxapkg({
      target: input,
      outDir: output,
      dryRun: true,
      wxid: 'wx1234567890abcdef',
      unpackOnly: true
    });

    assert.equal(result.engine, 'internal');
    assert.equal(result.skipped, true);
    assert.equal(result.unpackOnly, true);
    assert.equal(result.wxid, 'wx1234567890abcdef');
    assert.equal(result.workDir, process.cwd());
    assert.deepEqual(result.packages, [input]);
    assert.equal('command' in result, false);
    assert.equal('args' in result, false);
  });

  it('rejects clearing an output directory that contains the input package', async () => {
    const inputDir = path.join(root, 'unsafe-output');
    const input = path.join(inputDir, '__APP__.wxapkg');
    await fs.mkdir(inputDir);
    await fs.writeFile(input, 'package');

    await assert.rejects(
      decodeWxapkg({
        target: input,
        outDir: inputDir,
        clear: true,
        dryRun: true
      }),
      (error) => error.code === 'WXAPKG_UNSAFE_OUTPUT'
    );
  });

  it('rejects --clear containment hidden by an ancestor symlink', { skip: process.platform === 'win32' }, async () => {
    const sourceDir = path.join(root, 'source');
    const input = path.join(sourceDir, '__APP__.wxapkg');
    const sentinel = path.join(sourceDir, 'keep.txt');
    const alias = path.join(root, 'alias');
    await fs.mkdir(sourceDir);
    await fs.writeFile(input, 'package');
    await fs.writeFile(sentinel, 'keep');
    await fs.symlink(root, alias);

    await assert.rejects(
      decodeWxapkg({
        target: input,
        outDir: path.join(alias, 'source'),
        clear: true
      }),
      (error) => error.code === 'WXAPKG_UNSAFE_OUTPUT'
    );
    assert.equal(await fs.readFile(sentinel, 'utf8'), 'keep');
  });

  it('excludes publicLib from directory decoding by default', async () => {
    const inputDir = path.join(root, 'packages');
    await fs.mkdir(inputDir);
    await fs.writeFile(path.join(inputDir, 'publicLib.wxapkg'), 'public library');

    await assert.rejects(
      decodeWxapkg({
        target: inputDir,
        outDir: path.join(root, 'output'),
        dryRun: true
      }),
      /publicLib\.wxapkg/
    );
  });

  it('validates every input package before clearing old output', async () => {
    const input = path.join(root, '__APP__.wxapkg');
    const output = path.join(root, 'output');
    const sentinel = path.join(output, 'keep.txt');
    await fs.writeFile(input, 'not a wxapkg');
    await fs.mkdir(output);
    await fs.writeFile(sentinel, 'keep');

    await assert.rejects(
      decodeWxapkg({
        target: input,
        outDir: output,
        clear: true
      }),
      (error) => error.code === 'ERR_WXAPKG_TRUNCATED_HEADER'
    );
    assert.equal(await fs.readFile(sentinel, 'utf8'), 'keep');
  });

  it('rejects unsupported worker isolation before changing the output directory', async () => {
    const input = path.join(root, '__APP__.wxapkg');
    const output = path.join(root, 'output');
    const sentinel = path.join(output, 'keep.txt');
    await fs.writeFile(input, buildWxapkg([
      ['/app-config.json', Buffer.from('{"pages":[],"global":{},"window":{}}')]
    ]));
    await fs.mkdir(output);
    await fs.writeFile(sentinel, 'keep');

    await assert.rejects(
      decodeWxapkg({
        target: input,
        outDir: output,
        clear: true,
        workerIsolation: { supported: false }
      }),
      (error) => error.code === 'WXAPKG_UNSUPPORTED_NODE'
    );
    assert.equal(await fs.readFile(sentinel, 'utf8'), 'keep');
  });

  it('does not trust a caller-supplied permissive worker isolation configuration', async () => {
    const input = path.join(root, '__APP__.wxapkg');
    const output = path.join(root, 'output');
    await fs.writeFile(input, buildWxapkg([
      ['/app-config.json', Buffer.from(JSON.stringify({
        pages: [],
        global: {},
        window: {},
        fixturePadding: 'isolation'.repeat(30)
      }))]
    ]));

    const result = await decodeWxapkg({
      target: input,
      outDir: output,
      clear: true,
      workerIsolation: {
        supported: true,
        permissionFlag: '--definitely-invalid',
        networkPermission: true
      }
    });

    assert.notEqual(result.isolation.permissionFlag, '--definitely-invalid');
    assert.equal(result.isolation.supported, true);
    assert.equal(JSON.parse(await fs.readFile(path.join(output, 'app.json'), 'utf8')).pages.length, 0);
  });

  it('rejects an unsafe custom polyfill before clearing old output', {
    skip: process.platform === 'win32'
  }, async () => {
    const input = path.join(root, '__APP__.wxapkg');
    const output = path.join(root, 'output');
    const sentinel = path.join(output, 'keep.txt');
    const externalPolyfill = path.join(root, 'external-polyfill');
    await fs.writeFile(input, buildWxapkg([
      ['/app-config.json', Buffer.from('{"pages":[],"global":{},"window":{}}')]
    ]));
    await fs.mkdir(output);
    await fs.writeFile(sentinel, 'keep');
    await fs.mkdir(externalPolyfill);
    await fs.symlink(externalPolyfill, path.join(root, 'polyfill'));

    await assert.rejects(
      decodeWxapkg({
        target: input,
        outDir: output,
        clear: true
      }),
      (error) => error.code === 'WXAPKG_UNSAFE_POLYFILL'
    );
    assert.equal(await fs.readFile(sentinel, 'utf8'), 'keep');
  });

  it('rejects an unsafe worker read path before clearing old output', {
    skip: process.platform === 'win32'
  }, async () => {
    const input = path.join(root, '__APP__.wxapkg');
    const output = path.join(root, 'output*');
    const sentinel = path.join(output, 'keep.txt');
    await fs.writeFile(input, buildWxapkg([
      ['/app-config.json', Buffer.from('{"pages":[],"global":{},"window":{}}')]
    ]));
    await fs.mkdir(output);
    await fs.writeFile(sentinel, 'keep');

    await assert.rejects(
      decodeWxapkg({
        target: input,
        outDir: output,
        clear: true
      }),
      (error) => error.code === 'WXAPKG_UNSAFE_PERMISSION_PATH'
    );
    assert.equal(await fs.readFile(sentinel, 'utf8'), 'keep');
  });

  it('performs unpack-only work in the trusted parent without starting the decompiler', async () => {
    const input = path.join(root, '__APP__.wxapkg');
    const output = path.join(root, 'output');
    const appConfig = Buffer.from(JSON.stringify({ pages: [], global: {}, window: {} }));
    await fs.writeFile(input, buildWxapkg([
      ['/app-config.json', appConfig],
      ['/asset.txt', Buffer.from('raw-asset')]
    ]));

    const result = await decodeWxapkg({
      target: input,
      outDir: output,
      clear: true,
      unpackOnly: true
    });

    assert.equal(result.processed.length, 1);
    assert.equal(result.processed[0].fileCount, 2);
    assert.equal(await fs.readFile(path.join(output, 'asset.txt'), 'utf8'), 'raw-asset');
    await assert.rejects(fs.stat(path.join(output, 'project.private.config.json')), { code: 'ENOENT' });
  });

  it('removes a prepared polyfill snapshot when output setup fails', async () => {
    const input = path.join(root, '__APP__.wxapkg');
    const output = path.join(root, 'output-file');
    const polyfillRoot = path.join(root, 'polyfill');
    await fs.writeFile(input, buildWxapkg([
      ['/app-config.json', Buffer.from('{"pages":[],"global":{},"window":{}}')]
    ]));
    await fs.writeFile(output, 'keep');
    await fs.mkdir(polyfillRoot);
    await fs.writeFile(path.join(polyfillRoot, 'helper.js'), 'module.exports = {};');
    const stagesBefore = await listProcessPolyfillStages();

    await assert.rejects(
      decodeWxapkg({ target: input, outDir: output }),
      (error) => error?.code === 'EEXIST'
    );

    assert.equal(await fs.readFile(output, 'utf8'), 'keep');
    assert.deepEqual(await listProcessPolyfillStages(), stagesBefore);
  });

  it('runs the internal worker through a complete synthetic game decode', async () => {
    const input = path.join(root, '__APP__.wxapkg');
    const output = path.join(root, 'output');
    const directWorkerWrite = path.join(output, 'worker-direct-write.txt');
    const outsideDirectory = path.join(root, 'outside');
    const workerSymlink = path.join(output, 'worker-link');
    const symlinkEscape = path.join(outsideDirectory, 'symlink-escape.txt');
    await fs.mkdir(outsideDirectory);
    const appConfig = JSON.stringify({
      pages: [],
      global: {
        deviceOrientation: 'portrait',
        networkTimeout: { request: 10000 }
      },
      window: { backgroundTextStyle: 'light' },
      fixturePadding: 'x'.repeat(160)
    });
    const gameCode = [
      'var isolationStatus = "blocked";',
      'try {',
      '  var hostProcess = define.constructor("return process")();',
      '  try {',
      `    hostProcess.getBuiltinModule("node:fs").writeFileSync(${JSON.stringify(directWorkerWrite)}, "escape");`,
      '    isolationStatus = "write";',
      '  } catch (error) {',
      '    isolationStatus = "denied";',
      '  }',
      '} catch (error) {}',
      'define("isolation-" + isolationStatus + ".js", function(require, module, exports) {',
      '  module.exports = isolationStatus;',
      '});',
      'var symlinkStatus = "blocked";',
      'try {',
      '  var symlinkProcess = define.constructor("return process")();',
      '  var hostFs = symlinkProcess.getBuiltinModule("node:fs");',
      `  hostFs.symlinkSync(${JSON.stringify(outsideDirectory)}, ${JSON.stringify(workerSymlink)}, "dir");`,
      `  hostFs.writeFileSync(${JSON.stringify(path.join(workerSymlink, 'symlink-escape.txt'))}, "escape");`,
      '  symlinkStatus = "write";',
      '} catch (error) {',
      '  symlinkStatus = "denied";',
      '}',
      'define("symlink-" + symlinkStatus + ".js", function(require, module, exports) {',
      '  module.exports = symlinkStatus;',
      '});',
      'var networkStatus = "blocked";',
      'try {',
      '  var networkProcess = define.constructor("return process")();',
      '  networkStatus = networkProcess.permission && networkProcess.permission.has("net") ? "allowed" : "denied";',
      '} catch (error) {}',
      'define("network-" + networkStatus + ".js", function(require, module, exports) {',
      '  module.exports = networkStatus;',
      '});',
      'var signalStatus = "blocked";',
      'try {',
      '  var signalProcess = define.constructor("return process")();',
      '  signalStatus = typeof signalProcess.kill === "function" || typeof signalProcess._debugProcess === "function"',
      '    ? "available"',
      '    : "denied";',
      '} catch (error) {}',
      'define("signal-" + signalStatus + ".js", function(require, module, exports) {',
      '  module.exports = signalStatus;',
      '});',
      'var ipcStatus = "blocked";',
      'try {',
      '  var ipcProcess = define.constructor("return process")();',
      '  Object.defineProperty(ipcProcess, "_send", {',
      '    configurable: true,',
      '    value: function(message) {',
      '      ipcStatus = message && message.requestId ? "captured" : "forged";',
      '      return true;',
      '    }',
      '  });',
      '  Object.defineProperty(ipcProcess, "_handleQueue", { value: [] });',
      '  ipcStatus = "tampered";',
      '} catch (error) {',
      '  ipcStatus = "denied";',
      '}',
      'define("ipc-" + ipcStatus + ".js", function(require, module, exports) {',
      '  module.exports = ipcStatus;',
      '});',
      'define("hello.js", function(require, module, exports) {',
      '  module.exports = "internal-worker-ok";',
      '});',
      `/* ${'fixture'.repeat(30)} */`
    ].join('\n');
    await fs.writeFile(input, buildWxapkg([
      ['/app-config.json', Buffer.from(appConfig)],
      ['/game.js', Buffer.from(gameCode)]
    ]));
    await fs.mkdir(path.join(root, 'polyfill'));
    await fs.writeFile(
      path.join(root, 'polyfill/hello.js'),
      'module.exports = "validated-custom-polyfill";'
    );
    const stagesBefore = await listProcessPolyfillStages();

    const result = await decodeWxapkg({
      target: input,
      outDir: output,
      clear: true
    });

    assert.equal(result.engine, 'internal');
    assert.equal(result.skipped, false);
    assert.equal(result.processed.length, 1);
    assert.equal(result.processed[0].appType, 'game');
    assert.match(await fs.readFile(path.join(output, 'hello.js'), 'utf8'), /validated-custom-polyfill/);
    assert.equal(JSON.parse(await fs.readFile(path.join(output, 'game.json'), 'utf8')).deviceOrientation, 'portrait');
    assert.equal(JSON.parse(await fs.readFile(path.join(output, 'project.private.config.json'), 'utf8')).setting.urlCheck, false);
    await assert.rejects(fs.stat(directWorkerWrite), { code: 'ENOENT' });
    await assert.rejects(fs.lstat(workerSymlink), { code: 'ENOENT' });
    await assert.rejects(fs.stat(symlinkEscape), { code: 'ENOENT' });
    const isolationFiles = (await fs.readdir(output)).filter((name) => name.startsWith('isolation-'));
    assert.deepEqual(isolationFiles, ['isolation-denied.js']);
    const symlinkFiles = (await fs.readdir(output)).filter((name) => name.startsWith('symlink-'));
    assert.deepEqual(symlinkFiles, ['symlink-denied.js']);
    const networkFiles = (await fs.readdir(output)).filter((name) => name.startsWith('network-'));
    assert.deepEqual(networkFiles, ['network-denied.js']);
    const signalFiles = (await fs.readdir(output)).filter((name) => name.startsWith('signal-'));
    assert.deepEqual(signalFiles, ['signal-denied.js']);
    const ipcFiles = (await fs.readdir(output)).filter((name) => name.startsWith('ipc-'));
    assert.deepEqual(ipcFiles, ['ipc-denied.js']);
    assert.deepEqual(await listProcessPolyfillStages(), stagesBefore);
  });

  it('runs the internal app pipeline without wedecode runtime state', async () => {
    const input = path.join(root, '__APP__.wxapkg');
    const output = path.join(root, 'output');
    const appConfig = JSON.stringify({
      pages: ['pages/index/index'],
      global: {},
      window: {
        navigationBarTitleText: '内置引擎测试',
        backgroundTextStyle: 'light'
      },
      fixturePadding: 'app'.repeat(80)
    });
    await fs.writeFile(input, buildWxapkg([
      ['/app-config.json', Buffer.from(appConfig)]
    ]));

    const result = await decodeWxapkg({
      target: input,
      outDir: output,
      clear: true
    });

    assert.equal(result.processed[0].appType, 'app');
    assert.equal(result.processed[0].packType, 'main');
    const appJson = JSON.parse(await fs.readFile(path.join(output, 'app.json'), 'utf8'));
    assert.deepEqual(appJson.pages, ['pages/index/index']);
    assert.equal(appJson.window.navigationBarTitleText, '内置引擎测试');
    assert.equal(await fs.readFile(path.join(output, 'pages/index/index.js'), 'utf8'), 'Page({ data: {} })');
  });

  it('terminates a decompiler worker that exceeds the total timeout', async () => {
    const input = path.join(root, '__APP__.wxapkg');
    const output = path.join(root, 'output');
    const appConfig = JSON.stringify({
      pages: [],
      global: {},
      window: {},
      fixturePadding: 'timeout'.repeat(30)
    });
    const gameCode = [
      'var marker = "define(function(require, module, exports)";',
      'while (true) {}'
    ].join('\n');
    await fs.writeFile(input, buildWxapkg([
      ['/app-config.json', Buffer.from(appConfig)],
      ['/game.js', Buffer.from(gameCode)]
    ]));
    await fs.mkdir(path.join(root, 'polyfill'));
    await fs.writeFile(path.join(root, 'polyfill/helper.js'), 'module.exports = {};');
    const stagesBefore = await listProcessPolyfillStages();

    await assert.rejects(
      decodeWxapkg({ target: input, outDir: output, clear: true, timeoutMs: 50 }),
      (error) => error?.code === 'WXAPKG_DECODE_TIMEOUT'
    );
    await assert.rejects(fs.stat(path.join(output, 'project.private.config.json')), { code: 'ENOENT' });
    assert.deepEqual(await listProcessPolyfillStages(), stagesBefore);
  });

  it('maps plugin directories only for full decompilation', async () => {
    const input = path.join(root, '__APP__.wxapkg');
    const fullOutput = path.join(root, 'full-output');
    const unpackOutput = path.join(root, 'unpack-output');
    const pluginAsset = '__plugin__/wx1111111111111111/asset.txt';
    const appConfig = JSON.stringify({
      pages: [],
      global: {},
      window: {},
      fixturePadding: 'plugin'.repeat(20)
    });
    await fs.writeFile(input, buildWxapkg([
      ['/app-config.json', Buffer.from(appConfig)],
      [`/${pluginAsset}`, Buffer.from('plugin-asset')]
    ]));

    await decodeWxapkg({ target: input, outDir: fullOutput, clear: true });
    assert.equal(
      await fs.readFile(path.join(fullOutput, 'plugin_/wx1111111111111111/asset.txt'), 'utf8'),
      'plugin-asset'
    );
    await assert.rejects(fs.stat(path.join(fullOutput, pluginAsset)), { code: 'ENOENT' });

    await decodeWxapkg({ target: input, outDir: unpackOutput, clear: true, unpackOnly: true });
    assert.equal(await fs.readFile(path.join(unpackOutput, pluginAsset), 'utf8'), 'plugin-asset');
    await assert.rejects(fs.stat(path.join(unpackOutput, 'plugin_')), { code: 'ENOENT' });
  });
});

async function listProcessPolyfillStages() {
  const prefix = `wxapkg-helper-polyfill-${process.pid}-`;
  return (await fs.readdir(os.tmpdir()))
    .filter((name) => name.startsWith(prefix))
    .sort();
}
