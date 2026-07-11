import os from 'node:os';
import path from 'node:path';

export function expandHome(inputPath) {
  if (!inputPath) {
    return inputPath;
  }

  if (inputPath === '~') {
    return os.homedir();
  }

  if (inputPath.startsWith(`~${path.sep}`) || inputPath.startsWith('~/')) {
    return path.join(os.homedir(), inputPath.slice(2));
  }

  return inputPath;
}

export function normalizePath(inputPath) {
  return path.resolve(expandHome(inputPath));
}

export function uniquePaths(paths) {
  const seen = new Set();
  const result = [];

  for (const rawPath of paths.filter(Boolean)) {
    const fullPath = normalizePath(rawPath);
    const key = process.platform === 'win32' ? fullPath.toLowerCase() : fullPath;

    if (!seen.has(key)) {
      seen.add(key);
      result.push(fullPath);
    }
  }

  return result;
}

export function getDefaultSearchRoots(env = process.env, platform = process.platform) {
  const home = os.homedir();
  const roots = [];

  if (platform === 'win32') {
    roots.push(
      env.APPDATA && path.join(env.APPDATA, 'Tencent', 'xwechat'),
      env.APPDATA && path.join(env.APPDATA, 'Tencent', 'WeChat'),
      env.LOCALAPPDATA && path.join(env.LOCALAPPDATA, 'Tencent', 'WeChat'),
      env.USERPROFILE && path.join(env.USERPROFILE, 'Documents', 'WeChat Files', 'Applet'),
      env.USERPROFILE && path.join(env.USERPROFILE, 'Documents', 'WeChat Files')
    );
  } else if (platform === 'darwin') {
    roots.push(
      path.join(home, 'Library', 'Containers', 'com.tencent.xinWeChat', 'Data', 'Library', 'Application Support', 'com.tencent.xinWeChat'),
      path.join(home, 'Library', 'Containers', 'com.tencent.xinWeChat', 'Data', 'Documents', 'app_data', 'radium', 'users'),
      path.join(home, 'Library', 'Containers', 'com.tencent.xinWeChat', 'Data', 'Documents', 'xwechat_files'),
      path.join(home, 'Library', 'Containers', 'com.tencent.xinWeChat', 'Data', 'Documents', 'WeChat Files'),
      path.join(home, 'Library', 'Containers', 'com.tencent.WeChat', 'Data', 'Library', 'Application Support'),
      path.join(home, 'Library', 'Application Support', 'com.tencent.xinWeChat'),
      path.join(home, 'Library', 'Application Support', 'WeChat')
    );
  } else {
    roots.push(
      path.join(home, '.config', 'WeChat'),
      path.join(home, '.config', 'Tencent', 'WeChat'),
      path.join(home, 'Documents', 'WeChat Files')
    );
  }

  return uniquePaths(roots);
}
