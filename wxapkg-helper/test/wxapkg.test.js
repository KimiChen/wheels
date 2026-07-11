// SPDX-License-Identifier: GPL-3.0-or-later

import crypto from 'node:crypto';
import fs from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { afterEach, describe, it } from 'node:test';
import assert from 'node:assert/strict';
import { decryptWxapkg, extractWxid, needsDecryption } from '../src/decoder/wxapkg/decrypt.js';
import { WxapkgDecryptionError, WxapkgPathError } from '../src/decoder/wxapkg/errors.js';
import { parseWxapkg } from '../src/decoder/wxapkg/format.js';
import { unpackWxapkg } from '../src/decoder/wxapkg/unpack.js';
import { buildWxapkg, encryptWxapkg } from '../test-support/wxapkg-fixture.js';

const temporaryRoots = [];
const wxid = 'wx1234567890abcdef';

afterEach(async () => {
  await Promise.all(temporaryRoots.splice(0).map((root) => fs.rm(root, { recursive: true, force: true })));
});

describe('parseWxapkg', () => {
  it('parses root-prefixed Unicode paths and binary data', () => {
    const binary = Buffer.from([0x00, 0xff, 0x80, 0x41]);
    const packageData = buildWxapkg([
      ['/app-config.json', Buffer.from('{"pages":[]}')],
      ['/assets/测试.bin', binary]
    ]);

    const parsed = parseWxapkg(packageData);

    assert.equal(parsed.header.fileCount, 2);
    assert.equal(parsed.header.bodyEnd, packageData.length);
    assert.deepEqual(parsed.files.map((file) => file.path), ['app-config.json', 'assets/测试.bin']);
    assert.deepEqual(parsed.files[1].data, binary);
  });

  it('rejects invalid magic, truncated sections, and impossible counts', () => {
    const valid = buildWxapkg([['file.txt', Buffer.from('data')]]);
    const badMagic = Buffer.from(valid);
    badMagic[0] = 0;
    assert.throws(() => parseWxapkg(badMagic), hasCode('ERR_WXAPKG_MAGIC'));
    assert.throws(() => parseWxapkg(valid.subarray(0, 10)), hasCode('ERR_WXAPKG_TRUNCATED_HEADER'));
    assert.throws(() => parseWxapkg(valid.subarray(0, valid.length - 1)), hasCode('ERR_WXAPKG_TRUNCATED_BODY'));

    const badCount = Buffer.from(valid);
    badCount.writeUInt32BE(10, 14);
    assert.throws(() => parseWxapkg(badCount), hasCode('ERR_WXAPKG_FILE_COUNT'));
  });

  it('rejects invalid UTF-8, NUL bytes, and unsafe paths', () => {
    const invalidUtf8 = buildWxapkg([['ab', Buffer.from('data')]]);
    invalidUtf8[22] = 0xc3;
    invalidUtf8[23] = 0x28;
    assert.throws(() => parseWxapkg(invalidUtf8), hasCode('ERR_WXAPKG_ENTRY_UTF8'));

    const unsafePaths = [
      'bad\0name.js',
      '../escape.js',
      'safe/../../escape.js',
      'C:\\temp\\escape.js',
      '//server/share.js',
      '\\\\server\\share.js',
      'safe\\..\\escape.js'
    ];

    for (const unsafePath of unsafePaths) {
      assert.throws(() => parseWxapkg(buildWxapkg([[unsafePath, Buffer.from('x')]])), WxapkgPathError);
    }
  });

  it('rejects Windows alternate data streams, device names, and trailing spaces or dots', () => {
    const unsafePaths = [
      'file.js:payload',
      'assets:cache/file.js',
      'CON',
      'con.txt',
      'safe/PRN.json',
      'safe/aux',
      'safe/NUL.config.json',
      'safe/COM1.js',
      'safe/lpt9.log',
      'safe/trailing-space ',
      'safe/trailing-dot.'
    ];

    for (const unsafePath of unsafePaths) {
      assert.throws(
        () => parseWxapkg(buildWxapkg([[unsafePath, Buffer.from('x')]])),
        (error) => error instanceof WxapkgPathError && error.details?.path === unsafePath
      );
    }
  });

  it('allows names adjacent to Windows reserved device names', () => {
    const validPaths = [
      'console.js',
      'com10.js',
      'lpt0.log',
      'auxiliary.json',
      'nulled.txt',
      'safe/name .txt',
      'safe/middle..dot'
    ];
    const parsed = parseWxapkg(buildWxapkg(validPaths.map((filePath) => [filePath, Buffer.from('x')])));

    assert.deepEqual(parsed.files.map((file) => file.path), validPaths);
  });

  it('rejects normalized output paths that differ only by Windows casing', () => {
    const packageData = buildWxapkg([
      ['Assets/Icon.png', Buffer.from('first')],
      ['assets\\icon.PNG', Buffer.from('second')]
    ]);

    assert.throws(() => parseWxapkg(packageData), hasCode('ERR_WXAPKG_DUPLICATE_PATH'));
  });

  it('rejects offsets, sizes, and overlapping ranges outside the declared body', () => {
    const valid = buildWxapkg([
      ['a.bin', Buffer.from('aaaa')],
      ['b.bin', Buffer.from('bbbb')]
    ]);
    const firstOffsetPosition = entryOffsetPosition(valid, 0);

    const badOffset = Buffer.from(valid);
    badOffset.writeUInt32BE(0, firstOffsetPosition);
    assert.throws(() => parseWxapkg(badOffset), hasCode('ERR_WXAPKG_ENTRY_OFFSET'));

    const badSize = Buffer.from(valid);
    badSize.writeUInt32BE(valid.length, firstOffsetPosition + 4);
    assert.throws(() => parseWxapkg(badSize), hasCode('ERR_WXAPKG_ENTRY_SIZE'));

    const overlap = Buffer.from(valid);
    const secondOffsetPosition = entryOffsetPosition(overlap, 1);
    overlap.writeUInt32BE(overlap.readUInt32BE(firstOffsetPosition) + 1, secondOffsetPosition);
    assert.throws(() => parseWxapkg(overlap), hasCode('ERR_WXAPKG_OVERLAPPING_ENTRIES'));
  });
});

describe('V1MMWX decryption', () => {
  it('decrypts with an explicit wxid and supports the buffer-first compatibility form', () => {
    const plain = buildWxapkg([['large.bin', crypto.randomBytes(1400)]]);
    const encrypted = encryptWxapkg(plain, wxid);

    assert.equal(needsDecryption(encrypted), true);
    assert.equal(needsDecryption(plain), false);
    assert.deepEqual(decryptWxapkg(wxid, encrypted), plain);
    assert.deepEqual(decryptWxapkg(encrypted, wxid), plain);
  });

  it('rejects missing data and incorrect wxids without exiting the process', () => {
    const plain = buildWxapkg([['large.bin', crypto.randomBytes(1400)]]);
    const encrypted = encryptWxapkg(plain, wxid);

    assert.throws(() => decryptWxapkg('not-an-appid', encrypted), WxapkgDecryptionError);
    assert.throws(() => decryptWxapkg('wx0000000000000000', encrypted), hasCode('ERR_WXAPKG_DECRYPTED_MAGIC'));
    assert.throws(() => decryptWxapkg(wxid, Buffer.from('V1MMWX')), hasCode('ERR_WXAPKG_ENCRYPTED_TRUNCATED'));
  });

  it('extracts wxids from POSIX and Windows paths', () => {
    assert.equal(extractWxid(`/cache/${wxid}/__APP__.wxapkg`), wxid);
    assert.equal(extractWxid(`C:\\cache\\${wxid}\\__APP__.wxapkg`), wxid);
    assert.equal(extractWxid('/cache/no-appid/__APP__.wxapkg'), null);
  });
});

describe('unpackWxapkg', () => {
  it('writes every file under the output directory and returns compatible metadata', async () => {
    const root = await makeTemporaryRoot();
    const inputPath = path.join(root, '__APP__.wxapkg');
    const outputPath = path.join(root, 'output');
    const binary = Buffer.from([0, 1, 2, 255]);
    await fs.writeFile(inputPath, buildWxapkg([
      ['/app-config.json', Buffer.from('{"pages":[]}')],
      ['/assets/测试.bin', binary],
      ['/game.js', Buffer.from('Game({})')]
    ]));

    const result = await unpackWxapkg(inputPath, outputPath);

    assert.equal(result.inputPath, inputPath);
    assert.equal(result.outputPath, outputPath);
    assert.equal(result.files, result.fileList);
    assert.equal(result.subPackRootPath, '');
    assert.equal(result.appType, 'game');
    assert.equal(result.packType, 'main');
    assert.deepEqual(await fs.readFile(path.join(outputPath, 'assets', '测试.bin')), binary);
  });

  it('uses a wxid from the input path for encrypted packages', async () => {
    const root = await makeTemporaryRoot();
    const inputDir = path.join(root, wxid);
    const inputPath = path.join(inputDir, '__APP__.wxapkg');
    const outputPath = path.join(root, 'output');
    const binary = crypto.randomBytes(1400);
    const plain = buildWxapkg([['large.bin', binary]]);
    await fs.mkdir(inputDir);
    await fs.writeFile(inputPath, encryptWxapkg(plain, wxid));

    const result = await unpackWxapkg(inputPath, outputPath);

    assert.equal(result.encrypted, true);
    assert.equal(result.wxid, wxid);
    assert.deepEqual(await fs.readFile(path.join(outputPath, 'large.bin')), binary);
  });

  it('accepts an explicit wxid and rejects encrypted input when no wxid is available', async () => {
    const root = await makeTemporaryRoot();
    const inputPath = path.join(root, '__APP__.wxapkg');
    const outputPath = path.join(root, 'output');
    const binary = crypto.randomBytes(1400);
    const plain = buildWxapkg([['large.bin', binary]]);
    await fs.writeFile(inputPath, encryptWxapkg(plain, wxid));

    await assert.rejects(unpackWxapkg(inputPath, outputPath), hasCode('ERR_WXAPKG_WXID_REQUIRED'));
    const result = await unpackWxapkg(inputPath, outputPath, { wxid });

    assert.equal(result.wxid, wxid);
    assert.deepEqual(await fs.readFile(path.join(outputPath, 'large.bin')), binary);
  });

  it('reports subpackage roots and independent package types', async () => {
    const root = await makeTemporaryRoot();

    for (const independent of [false, true]) {
      const suffix = independent ? 'independent' : 'sub';
      const inputPath = path.join(root, `${suffix}.wxapkg`);
      const outputPath = path.join(root, `${suffix}-output`);
      const config = JSON.stringify({ subPackages: [{ root: independent ? 'scene/' : 'scene', independent }] });
      await fs.writeFile(inputPath, buildWxapkg([
        ['scene/app-config.json', Buffer.from(config)],
        ['scene/index.js', Buffer.from('Page({})')]
      ]));

      const result = await unpackWxapkg(inputPath, outputPath);
      assert.equal(result.subPackRootPath, 'scene');
      assert.equal(result.packType, independent ? 'independent' : 'sub');
    }
  });

  it('rejects traversal before creating the output directory', async () => {
    const root = await makeTemporaryRoot();
    const inputPath = path.join(root, 'bad.wxapkg');
    const outputPath = path.join(root, 'output');
    const escapedPath = path.join(root, 'escape.txt');
    await fs.writeFile(inputPath, buildWxapkg([['../escape.txt', Buffer.from('escape')]]));

    await assert.rejects(unpackWxapkg(inputPath, outputPath), WxapkgPathError);
    await assert.rejects(fs.stat(outputPath), { code: 'ENOENT' });
    await assert.rejects(fs.stat(escapedPath), { code: 'ENOENT' });
  });

  it('preserves existing non-empty files unless overwrite is enabled', async () => {
    const root = await makeTemporaryRoot();
    const inputPath = path.join(root, '__APP__.wxapkg');
    const outputPath = path.join(root, 'output');
    await fs.mkdir(outputPath);
    await fs.writeFile(path.join(outputPath, 'kept.txt'), 'old');
    await fs.writeFile(path.join(outputPath, 'empty.txt'), '');
    await fs.writeFile(inputPath, buildWxapkg([
      ['kept.txt', Buffer.from('new')],
      ['empty.txt', Buffer.from('filled')]
    ]));

    const firstResult = await unpackWxapkg(inputPath, outputPath);
    assert.equal(await fs.readFile(path.join(outputPath, 'kept.txt'), 'utf8'), 'old');
    assert.equal(await fs.readFile(path.join(outputPath, 'empty.txt'), 'utf8'), 'filled');
    assert.deepEqual(firstResult.skippedFiles.map((file) => file.path), ['kept.txt']);
    assert.deepEqual(firstResult.writtenFiles.map((file) => file.path), ['empty.txt']);

    const overwriteResult = await unpackWxapkg(inputPath, outputPath, { overwrite: true });
    assert.equal(await fs.readFile(path.join(outputPath, 'kept.txt'), 'utf8'), 'new');
    assert.equal(overwriteResult.skippedFiles.length, 0);
    assert.equal(overwriteResult.writtenFiles.length, 2);
  });

  it('does not follow symlinked directories inside the output root', { skip: process.platform === 'win32' }, async () => {
    const root = await makeTemporaryRoot();
    const inputPath = path.join(root, 'bad-symlink.wxapkg');
    const outputPath = path.join(root, 'output');
    const outsidePath = path.join(root, 'outside');
    await fs.mkdir(outputPath);
    await fs.mkdir(outsidePath);
    await fs.symlink(outsidePath, path.join(outputPath, 'linked'));
    await fs.writeFile(inputPath, buildWxapkg([['linked/escape.txt', Buffer.from('escape')]]));

    await assert.rejects(unpackWxapkg(inputPath, outputPath), hasCode('ERR_WXAPKG_OUTPUT_SYMLINK'));
    await assert.rejects(fs.stat(path.join(outsidePath, 'escape.txt')), { code: 'ENOENT' });
  });
});

function entryOffsetPosition(packageData, wantedIndex) {
  const fileCount = packageData.readUInt32BE(14);
  assert.ok(wantedIndex < fileCount);
  let cursor = 18;
  for (let index = 0; index < fileCount; index += 1) {
    const nameLength = packageData.readUInt32BE(cursor);
    cursor += 4 + nameLength;
    if (index === wantedIndex) {
      return cursor;
    }
    cursor += 8;
  }
  throw new Error('entry not found');
}

function hasCode(code) {
  return (error) => error instanceof Error && error.code === code;
}

async function makeTemporaryRoot() {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'wxapkg-format-'));
  temporaryRoots.push(root);
  return root;
}
