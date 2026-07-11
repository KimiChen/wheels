import assert from 'node:assert/strict';
import { execFile } from 'node:child_process';
import fs from 'node:fs/promises';
import path from 'node:path';
import { describe, it } from 'node:test';
import { fileURLToPath } from 'node:url';
import { promisify } from 'node:util';

const execFileAsync = promisify(execFile);
const projectRoot = fileURLToPath(new URL('../', import.meta.url));
const cliPath = path.join(projectRoot, 'bin', 'wxapkg-helper.js');
const packageJson = JSON.parse(await fs.readFile(path.join(projectRoot, 'package.json'), 'utf8'));

describe('wxapkg-helper CLI', () => {
  it('reads the version from package.json', async () => {
    const { stdout } = await execFileAsync(process.execPath, [cliPath, '--version']);

    assert.equal(stdout.trim(), packageJson.version);
  });

  it('documents the internal scan and decode commands', async () => {
    const { stdout } = await execFileAsync(process.execPath, [cliPath, '--help']);

    assert.match(stdout, /使用项目内置引擎反解/);
    assert.match(stdout, /scan \[options\]/);
    assert.match(stdout, /decode \[options\] \[target\]/);
  });

  it('does not expose the removed wedecode executable option', async () => {
    const { stdout } = await execFileAsync(process.execPath, [cliPath, 'decode', '--help']);

    assert.match(stdout, /--unpack-only/);
    assert.doesNotMatch(stdout, /--wedecode-bin/);
  });
});
