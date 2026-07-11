import fs from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { after, before, describe, it } from 'node:test';
import assert from 'node:assert/strict';
import { summarizeOutputDir } from '../src/output-summary.js';

let root;

describe('summarizeOutputDir', () => {
  before(async () => {
    root = await fs.mkdtemp(path.join(os.tmpdir(), 'wxapkg-helper-output-'));
    await fs.mkdir(path.join(root, 'pages', 'index'), { recursive: true });
    await fs.mkdir(path.join(root, 'assets'), { recursive: true });

    await fs.writeFile(path.join(root, 'game.js'), 'console.log("game");');
    await fs.writeFile(path.join(root, 'game.json'), '{"deviceOrientation":"portrait"}');
    await fs.writeFile(path.join(root, 'app-config.json'), '{"pages":[]}');
    await fs.writeFile(path.join(root, 'pages', 'index', 'index.wxml'), '<view />');
    await fs.writeFile(path.join(root, 'pages', 'index', 'index.wxss'), 'view {}');
    await fs.writeFile(path.join(root, 'assets', 'logo.png'), 'fake');
  });

  after(async () => {
    await fs.rm(root, { recursive: true, force: true });
  });

  it('counts output files and key entry files', async () => {
    const summary = await summarizeOutputDir(root);

    assert.equal(summary.exists, true);
    assert.equal(summary.totalFiles, 6);
    assert.equal(summary.counts.js, 1);
    assert.equal(summary.counts.json, 2);
    assert.equal(summary.counts.wxml, 1);
    assert.equal(summary.counts.wxss, 1);
    assert.equal(summary.counts.assets, 3);
    assert.deepEqual(summary.keyFiles, ['app-config.json', 'game.json', 'game.js']);
  });

  it('handles missing output directories', async () => {
    const summary = await summarizeOutputDir(path.join(root, 'missing'));

    assert.equal(summary.exists, false);
  });
});
