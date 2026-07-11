import { afterEach, describe, it } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import {
  MAX_MANIFEST_OPERATIONS,
  MAX_MANIFEST_SINGLE_WRITE_BYTES,
  MAX_MANIFEST_WRITE_BYTES,
  applyWriteManifest,
  validateWriteManifest
} from '../src/decoder/write-manifest.js';

const temporaryRoots = [];

afterEach(async () => {
  await Promise.all(temporaryRoots.splice(0).map((root) => (
    fs.rm(root, { recursive: true, force: true })
  )));
});

describe('write manifest validation', () => {
  it('strictly decodes write data and returns normalized operations', () => {
    const operations = validateWriteManifest([
      writeOperation('nested/file.txt', Buffer.from('hello')),
      { op: 'delete', path: 'old.txt' }
    ]);

    assert.equal(Object.isFrozen(operations), true);
    assert.deepEqual(operations[0], {
      op: 'write',
      path: 'nested/file.txt',
      data: Buffer.from('hello'),
      size: 5
    });
    assert.deepEqual(operations[1], { op: 'delete', path: 'old.txt' });
  });

  it('rejects unknown operations, missing fields, accessors, and injected fields', () => {
    const accessor = { op: 'delete', path: 'file.txt' };
    Object.defineProperty(accessor, 'path', { get: () => 'other.txt', enumerable: true });

    const cases = [
      [{ op: 'move', path: 'a', destination: 'b' }],
      [{ op: 'write', path: 'a', data: '', size: 0, mode: 0o777 }],
      [{ op: 'delete' }],
      [accessor],
      [Object.assign(Object.create({ injected: true }), { op: 'delete', path: 'a' })]
    ];

    for (const manifest of cases) {
      assert.throws(
        () => validateWriteManifest(manifest),
        hasCode('WXAPKG_MANIFEST_INVALID')
      );
    }

    const injectedArray = [{ op: 'delete', path: 'a' }];
    injectedArray.extra = true;
    assert.throws(
      () => validateWriteManifest(injectedArray),
      hasCode('WXAPKG_MANIFEST_INVALID')
    );
  });

  it('rejects non-canonical, absolute, escaping, and duplicate target paths', () => {
    const invalidPaths = [
      '/absolute.txt',
      '../escape.txt',
      'a/../escape.txt',
      './file.txt',
      'a//file.txt',
      'a/',
      'C:/escape.txt',
      'directory/file:stream',
      'directory/NUL.txt',
      'directory/trailing. ',
      'a\\escape.txt',
      'nul\0byte.txt'
    ];

    for (const invalidPath of invalidPaths) {
      assert.throws(
        () => validateWriteManifest([{ op: 'delete', path: invalidPath }]),
        hasCode('WXAPKG_MANIFEST_PATH')
      );
    }

    assert.throws(
      () => validateWriteManifest([
        { op: 'delete', path: 'same.txt' },
        writeOperation('same.txt', Buffer.from('replacement'))
      ]),
      hasCode('WXAPKG_MANIFEST_CONFLICT')
    );

    assert.throws(
      () => validateWriteManifest([
        { op: 'delete', path: 'Case.txt' },
        { op: 'delete', path: 'case.txt' }
      ]),
      hasCode('WXAPKG_MANIFEST_CONFLICT')
    );

    assert.throws(
      () => validateWriteManifest([
        writeOperation('parent', Buffer.from('file')),
        writeOperation('parent/child', Buffer.from('child'))
      ]),
      hasCode('WXAPKG_MANIFEST_CONFLICT')
    );
  });

  it('rejects malformed, non-canonical, and size-mismatched base64', () => {
    const cases = [
      { op: 'write', path: 'a', data: 'aGVsbG8', size: 5 },
      { op: 'write', path: 'a', data: 'aGVsbG8*', size: 6 },
      { op: 'write', path: 'a', data: 'AB==', size: 1 },
      { op: 'write', path: 'a', data: 'aGVsbG8=', size: 4 },
      { op: 'write', path: 'a', data: Buffer.from('hello'), size: 5 }
    ];

    for (const operation of cases) {
      assert.throws(
        () => validateWriteManifest([operation]),
        hasCode('WXAPKG_MANIFEST_INVALID')
      );
    }
  });

  it('enforces operation and write-size limits before allocating data buffers', () => {
    assert.equal(MAX_MANIFEST_OPERATIONS, 100_000);
    assert.equal(MAX_MANIFEST_WRITE_BYTES, 512 * 1024 * 1024);
    assert.equal(MAX_MANIFEST_SINGLE_WRITE_BYTES, 128 * 1024 * 1024);

    assert.throws(
      () => validateWriteManifest(new Array(MAX_MANIFEST_OPERATIONS + 1)),
      hasCode('WXAPKG_MANIFEST_LIMIT')
    );
    assert.throws(
      () => validateWriteManifest([{
        op: 'write',
        path: 'large.bin',
        data: '',
        size: MAX_MANIFEST_SINGLE_WRITE_BYTES + 1
      }]),
      hasCode('WXAPKG_MANIFEST_LIMIT')
    );
  });
});

describe('write manifest application', () => {
  it('writes files, creates real parents, and deletes files and empty directories', async () => {
    const root = await makeTemporaryRoot();
    await fs.writeFile(path.join(root, 'old.txt'), 'old');
    await fs.mkdir(path.join(root, 'empty'));

    const result = await applyWriteManifest(root, [
      writeOperation('nested/file.txt', Buffer.from('hello')),
      writeOperation('empty.bin', Buffer.alloc(0)),
      { op: 'delete', path: 'old.txt' },
      { op: 'delete', path: 'empty' }
    ]);

    assert.deepEqual(result, {
      operationCount: 4,
      written: 2,
      deleted: 2,
      bytesWritten: 5
    });
    assert.equal(await fs.readFile(path.join(root, 'nested/file.txt'), 'utf8'), 'hello');
    assert.equal((await fs.stat(path.join(root, 'empty.bin'))).size, 0);
    await assert.rejects(fs.stat(path.join(root, 'old.txt')), { code: 'ENOENT' });
    await assert.rejects(fs.stat(path.join(root, 'empty')), { code: 'ENOENT' });
  });

  it('validates the entire manifest before changing the output tree', async () => {
    const root = await makeTemporaryRoot();

    await assert.rejects(
      applyWriteManifest(root, [
        writeOperation('would-be-created.txt', Buffer.from('data')),
        { op: 'delete', path: '../invalid' }
      ]),
      hasCode('WXAPKG_MANIFEST_PATH')
    );
    await assert.rejects(fs.stat(path.join(root, 'would-be-created.txt')), { code: 'ENOENT' });

    await fs.mkdir(path.join(root, 'not-empty'));
    await fs.writeFile(path.join(root, 'not-empty/child.txt'), 'keep');
    await assert.rejects(
      applyWriteManifest(root, [
        writeOperation('also-not-created.txt', Buffer.from('data')),
        { op: 'delete', path: 'not-empty' }
      ]),
      hasCode('WXAPKG_MANIFEST_UNSAFE_OUTPUT')
    );
    await assert.rejects(fs.stat(path.join(root, 'also-not-created.txt')), { code: 'ENOENT' });
  });

  it('rejects directory write targets and existing hard-linked files', async () => {
    const root = await makeTemporaryRoot();
    await fs.mkdir(path.join(root, 'directory'));

    await assert.rejects(
      applyWriteManifest(root, [writeOperation('directory', Buffer.from('data'))]),
      hasCode('WXAPKG_MANIFEST_UNSAFE_OUTPUT')
    );

    const original = path.join(root, 'original.txt');
    const linked = path.join(root, 'linked.txt');
    await fs.writeFile(original, 'original');
    await fs.link(original, linked);
    await assert.rejects(
      applyWriteManifest(root, [writeOperation('linked.txt', Buffer.from('changed'))]),
      hasCode('WXAPKG_MANIFEST_UNSAFE_OUTPUT')
    );
    assert.equal(await fs.readFile(original, 'utf8'), 'original');
  });

  it('does not follow symlinked roots, parents, write targets, or delete targets', {
    skip: process.platform === 'win32'
  }, async () => {
    const base = await makeTemporaryRoot();
    const root = path.join(base, 'output');
    const outside = path.join(base, 'outside');
    await fs.mkdir(root);
    await fs.mkdir(outside);
    await fs.writeFile(path.join(outside, 'target.txt'), 'outside');

    const rootLink = path.join(base, 'root-link');
    await fs.symlink(root, rootLink);
    await assert.rejects(
      applyWriteManifest(rootLink, [writeOperation('file.txt', Buffer.from('data'))]),
      hasCode('WXAPKG_MANIFEST_UNSAFE_OUTPUT')
    );

    await fs.symlink(outside, path.join(root, 'linked-parent'));
    await assert.rejects(
      applyWriteManifest(root, [writeOperation('linked-parent/escape.txt', Buffer.from('escape'))]),
      hasCode('WXAPKG_MANIFEST_UNSAFE_OUTPUT')
    );
    await assert.rejects(fs.stat(path.join(outside, 'escape.txt')), { code: 'ENOENT' });

    await fs.symlink(path.join(outside, 'target.txt'), path.join(root, 'write-link'));
    await assert.rejects(
      applyWriteManifest(root, [writeOperation('write-link', Buffer.from('changed'))]),
      hasCode('WXAPKG_MANIFEST_UNSAFE_OUTPUT')
    );
    assert.equal(await fs.readFile(path.join(outside, 'target.txt'), 'utf8'), 'outside');

    await fs.symlink(path.join(outside, 'target.txt'), path.join(root, 'delete-link'));
    await assert.rejects(
      applyWriteManifest(root, [{ op: 'delete', path: 'delete-link' }]),
      hasCode('WXAPKG_MANIFEST_UNSAFE_OUTPUT')
    );
    assert.equal(await fs.readFile(path.join(outside, 'target.txt'), 'utf8'), 'outside');
  });
});

function writeOperation(filePath, data) {
  return {
    op: 'write',
    path: filePath,
    data: data.toString('base64'),
    size: data.length
  };
}

async function makeTemporaryRoot() {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'wxapkg-manifest-'));
  temporaryRoots.push(root);
  return root;
}

function hasCode(code) {
  return (error) => error?.code === code;
}
