import { describe, it } from 'node:test';
import assert from 'node:assert/strict';
import path from 'node:path';
import { defaultOutputDir } from '../src/decode.js';
import { toPosixPath } from '../src/decoder/core/utils/common.js';
import { expandHome } from '../src/paths.js';
import { formatBytes, formatDate, makeOutputSlug } from '../src/report.js';

describe('report utilities', () => {
  it('formats bytes and dates', () => {
    assert.equal(formatBytes(0), '0B');
    assert.equal(formatBytes(1536), '1.5KB');
    assert.match(formatDate(new Date('2026-07-10T08:05:00').getTime()), /^2026-07-10 /);
  });

  it('keeps readable unicode names in output slugs', () => {
    assert.equal(makeOutputSlug('/tmp/wx0123456789abcdef/pkg'), 'tmp-wx0123456789abcdef-pkg');
    assert.equal(makeOutputSlug('测试小游戏'), '测试小游戏');
  });
});

describe('path utilities', () => {
  it('expands home paths', () => {
    assert.equal(expandHome('~'), process.env.HOME);
    assert.equal(expandHome('~/cache'), path.join(process.env.HOME, 'cache'));
  });

  it('normalizes generated source paths to POSIX separators', () => {
    assert.equal(toPosixPath('..\\assets\\icon.png'), '../assets/icon.png');
  });
});

describe('defaultOutputDir', () => {
  it('uses identity name before target path', () => {
    const outDir = defaultOutputDir('/tmp/wx0123456789abcdef/pkg', {
      name: '测试小游戏',
      appid: 'wx0123456789abcdef'
    });

    assert.equal(path.basename(outDir).startsWith('测试小游戏-'), true);
    assert.equal(path.basename(path.dirname(outDir)), 'decoded');
  });
});
