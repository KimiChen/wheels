import fs from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { after, before, describe, it } from 'node:test';
import assert from 'node:assert/strict';
import { inspectWxapkgTarget, scanWxapkg } from '../src/scan.js';

const appid = 'wx0123456789abcdef';
let root;

describe('scanWxapkg', () => {
  before(async () => {
    root = await fs.mkdtemp(path.join(os.tmpdir(), 'wxapkg-helper-scan-'));

    await makePackageDir({
      dir: path.join(root, 'Applet', '测试小游戏', appid, 'pkg'),
      appInfoDir: path.join(root, 'Applet', '测试小游戏', appid),
      appName: '测试小游戏',
      files: [
        ['__APP__.wxapkg', 'main', new Date('2026-07-10T08:00:00')],
        ['stage-one.wxapkg', 'sub', new Date('2026-07-10T08:05:00')]
      ]
    });

    await makePackageDir({
      dir: path.join(root, 'Applet', 'wx2222222222222222', 'pkg'),
      files: [
        ['__APP__.wxapkg', 'main', new Date('2026-07-09T08:00:00')]
      ]
    });

    await makePackageDir({
      dir: path.join(root, 'app_data', 'radium', 'users', 'user-a', 'wx4444444444444444', 'pkg'),
      appInfoDir: path.join(root, 'app_data', 'radium', 'users', 'user-a', 'wx4444444444444444', 'profile'),
      appidValue: 'wx4444444444444444',
      appName: '目录文件里的游戏名',
      appInfoFile: 'game.info',
      files: [
        ['__APP__.wxapkg', 'main', new Date('2026-07-08T08:00:00')]
      ]
    });

    await makePackageDir({
      dir: path.join(root, 'app_data', 'radium', 'users', 'user-a', 'wx5555555555555555', 'pkg'),
      files: [
        ['__APP__.wxapkg', 'main', new Date('2026-07-07T08:00:00')]
      ]
    });
    await fs.mkdir(path.join(root, 'app_data', 'radium', 'users', 'user-a', 'wx5555555555555555', 'OUTPUT'), { recursive: true });
    await fs.writeFile(
      path.join(root, 'app_data', 'radium', 'users', 'user-a', 'wx5555555555555555', 'OUTPUT', 'app-config.json'),
      JSON.stringify({ subPackages: [{ name: 'scene', root: 'scene/' }] }),
      'utf8'
    );

    await makePackageDir({
      dir: path.join(root, 'LibOnly', 'wx3333333333333333'),
      files: [
        ['publicLib.wxapkg', 'public', new Date('2026-07-10T09:00:00')]
      ]
    });
  });

  after(async () => {
    await fs.rm(root, { recursive: true, force: true });
  });

  it('groups packages, identifies appid and reads local game name metadata', async () => {
    const result = await scanWxapkg({ roots: [root] });
    const candidate = result.candidates.find((item) => item.identity.appid === appid);

    assert.equal(result.limit, null);
    assert.equal(result.candidates.length, result.filteredCandidates);
    assert.equal(result.candidates[0].identity.appid, appid);
    assert.equal(candidate.packageCount, 2);
    assert.equal(candidate.mainCount, 1);
    assert.equal(candidate.publicLibCount, 0);
    assert.equal(candidate.identity.gameId, appid);
    assert.equal(candidate.identity.name, '测试小游戏');
    assert.equal(candidate.identity.displayName, '测试小游戏');
  });

  it('reads game name from files under the app directory instead of using radium path', async () => {
    const result = await scanWxapkg({ roots: [root], appid: 'wx4444444444444444' });
    const candidate = result.candidates[0];

    assert.equal(candidate.identity.name, '目录文件里的游戏名');
    assert.equal(candidate.identity.displayName, '目录文件里的游戏名');
  });

  it('does not use app_data or other cache path parts as the game name', async () => {
    const result = await scanWxapkg({ roots: [root], appid: 'wx5555555555555555' });
    const candidate = result.candidates[0];

    assert.equal(candidate.identity.name, null);
    assert.equal(candidate.identity.gameId, 'wx5555555555555555');
    assert.equal(candidate.identity.displayName, null);
  });

  it('hides publicLib-only candidates unless requested', async () => {
    const hidden = await scanWxapkg({ roots: [root], limit: 0 });
    assert.equal(hidden.candidates.some((item) => item.publicLibCount === item.packageCount), false);

    const shown = await scanWxapkg({ roots: [root], includePublicLib: true, limit: 0 });
    assert.equal(shown.candidates.some((item) => item.publicLibCount === item.packageCount), true);
  });

  it('filters by appid and since, and limits candidate count', async () => {
    const byAppid = await scanWxapkg({ roots: [root], appid, limit: 0 });
    assert.equal(byAppid.candidates.length, 1);
    assert.equal(byAppid.candidates[0].identity.appid, appid);

    const since = await scanWxapkg({ roots: [root], since: '2026-07-10', limit: 0 });
    assert.equal(since.candidates.every((item) => item.latestMtimeMs >= new Date(2026, 6, 10).getTime()), true);

    const limited = await scanWxapkg({ roots: [root], includePublicLib: true, limit: 1 });
    assert.equal(limited.candidates.length, 1);
    assert.equal(limited.filteredCandidates > limited.candidates.length, true);
  });

  it('lists direct packages in a target directory', async () => {
    const candidate = await inspectWxapkgTarget(path.join(root, 'Applet', '测试小游戏', appid, 'pkg'));

    assert.equal(candidate.packageCount, 2);
    assert.deepEqual(candidate.packages.map((item) => item.name), ['__APP__.wxapkg', 'stage-one.wxapkg']);
  });
});

async function makePackageDir({ dir, appInfoDir, appidValue = appid, appName, appInfoFile = 'appinfo.json', files }) {
  await fs.mkdir(dir, { recursive: true });

  if (appInfoDir && appName) {
    await fs.mkdir(appInfoDir, { recursive: true });
    await fs.writeFile(
      path.join(appInfoDir, appInfoFile),
      JSON.stringify({ appid: appidValue, miniGameName: appName }),
      'utf8'
    );
  }

  for (const [name, content, mtime] of files) {
    const filePath = path.join(dir, name);
    await fs.writeFile(filePath, content);
    await fs.utimes(filePath, mtime, mtime);
  }
}
