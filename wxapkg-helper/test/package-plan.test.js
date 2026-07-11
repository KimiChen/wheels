import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { afterEach, describe, it } from 'node:test';
import { inspectPackagePlan, unpackPackagePlan } from '../src/decoder/package-plan.js';
import { toDecompilerOutputPath } from '../src/decoder/output-paths.js';
import { unpackWxapkg } from '../src/decoder/wxapkg/unpack.js';
import { buildWxapkg } from '../test-support/wxapkg-fixture.js';

const temporaryRoots = [];

afterEach(async () => {
  await Promise.all(temporaryRoots.splice(0).map((root) => (
    fs.rm(root, { recursive: true, force: true })
  )));
});

describe('package decode plan', () => {
  it('orders a content-identified main package before subpackages', async () => {
    const root = await makeTemporaryRoot();
    const subpackage = path.join(root, 'a-subpackage.wxapkg');
    const mainPackage = path.join(root, 'z-main-package.wxapkg');
    await fs.writeFile(subpackage, buildWxapkg([
      ['/sub/app-config.json', Buffer.from('{"subPackages":[{"root":"sub"}]}')],
      ['/sub/page.js', Buffer.from('Page({})')]
    ]));
    await fs.writeFile(mainPackage, appPackage('main'));

    const plan = await inspectPackagePlan([subpackage, mainPackage]);

    assert.deepEqual(
      plan.packages.map((item) => path.basename(item.inputPath)),
      ['z-main-package.wxapkg', 'a-subpackage.wxapkg']
    );
    assert.deepEqual(plan.packages.map((item) => item.packType), ['main', 'sub']);
  });

  it('rejects an input whose contents changed after inspection', async () => {
    const root = await makeTemporaryRoot();
    const input = path.join(root, '__APP__.wxapkg');
    const output = path.join(root, 'output');
    await fs.writeFile(input, appPackage('old'));
    const plan = await inspectPackagePlan([input]);

    await fs.writeFile(input, appPackage('new'));

    await assert.rejects(
      unpackPackagePlan(plan, output),
      (error) => error?.code === 'WXAPKG_INPUT_CHANGED'
    );
    await assert.rejects(fs.stat(output), { code: 'ENOENT' });
  });

  it('rejects plugin path mappings that collide with existing output paths', async () => {
    const root = await makeTemporaryRoot();
    const input = path.join(root, '__APP__.wxapkg');
    const output = path.join(root, 'output');
    await fs.writeFile(input, buildWxapkg([
      ['/__plugin__/wx1111111111111111/file.js', Buffer.from('source')],
      ['/plugin_/wx1111111111111111/file.js', Buffer.from('collision')]
    ]));

    await assert.rejects(
      unpackWxapkg(input, output, { mapEntryPath: toDecompilerOutputPath }),
      (error) => error?.code === 'ERR_WXAPKG_DUPLICATE_OUTPUT_PATH'
    );
    await assert.rejects(fs.stat(output), { code: 'ENOENT' });
  });
});

function appPackage(value) {
  return buildWxapkg([
    ['/app-config.json', Buffer.from('{"pages":[],"global":{},"window":{}}')],
    ['/same.txt', Buffer.from(value)]
  ]);
}

async function makeTemporaryRoot() {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'wxapkg-package-plan-'));
  temporaryRoots.push(root);
  return root;
}
