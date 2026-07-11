import fs from 'node:fs/promises';
import path from 'node:path';

const APPID_PATTERN = /^wx[0-9a-f]{16}$/i;
const APPID_SCAN_PATTERN = /wx[0-9a-f]{16}/ig;
const MAX_METADATA_FILE_SIZE = 256 * 1024;
const MAX_METADATA_ANCESTORS = 8;
const MAX_METADATA_SCAN_DEPTH = 4;
const MAX_METADATA_FILES = 120;
const MAX_JSON_DEPTH = 8;

const GENERIC_PATH_PARTS = new Set([
  'app',
  'app data',
  'app_data',
  'appdata',
  'applet',
  'appbrand',
  'application support',
  'cache',
  'caches',
  'data',
  'documents',
  'games',
  'library',
  'minigame',
  'miniapp',
  'miniprogram',
  'package',
  'packages',
  'pkg',
  'program',
  'publiclib',
  'radium',
  'storage',
  'tencent',
  'tmp',
  'user',
  'users',
  'wechat',
  'wechat files',
  'weixin',
  'wxapkg',
  'xwechat',
  'xwechat_files',
  '__app__'
]);

const NAME_KEY_SCORES = new Map([
  ['minigamename', 98],
  ['gamename', 96],
  ['nickname', 94],
  ['appname', 92],
  ['miniprogramname', 90],
  ['displayname', 88],
  ['appdisplayname', 88],
  ['apptitle', 84],
  ['title', 80],
  ['projectname', 72],
  ['name', 64]
]);

const APPID_KEYS = new Set([
  'appid',
  'app_id',
  'extappid',
  'wxid',
  'wx_id'
]);

export async function identifyCandidate(candidate) {
  const pathAppidHints = collectAppidHints(candidate);
  const preliminaryAppid = bestHint(pathAppidHints)?.value ?? null;
  const metadataHints = await collectMetadataHints(candidate.dir, preliminaryAppid);
  const appidHints = [
    ...pathAppidHints,
    ...metadataHints.appids
  ];
  const fallbackNameHints = [
    ...collectPathNameHints(candidate),
    ...collectPackageNameHints(candidate)
  ];

  const appidHint = bestHint(appidHints);
  const nameHint = bestHint(metadataHints.names.filter((hint) => hint.value !== appidHint?.value));
  const fallbackNameHint = bestHint(fallbackNameHints.filter((hint) => hint.value !== appidHint?.value));
  const pathHint = makePathHint(candidate.dir, appidHint?.value);
  const latestPackage = findLatestPackage(candidate.packages);

  return {
    appid: appidHint?.value ?? null,
    appidSource: appidHint?.source ?? null,
    gameId: appidHint?.value ?? null,
    gameIdSource: appidHint?.source ?? null,
    name: nameHint?.value ?? null,
    nameSource: nameHint?.source ?? null,
    fallbackName: fallbackNameHint?.value ?? null,
    fallbackNameSource: fallbackNameHint?.source ?? null,
    displayName: nameHint?.value ?? null,
    confidence: nameHint?.score ?? appidHint?.score ?? 20,
    pathHint,
    latestMtimeMs: candidate.latestMtimeMs,
    latestPackage: latestPackage
      ? {
          name: latestPackage.name,
          path: latestPackage.path,
          mtimeMs: latestPackage.mtimeMs
        }
      : null,
    clues: buildClues({ appidHint, nameHint, fallbackNameHint, pathHint, latestPackage })
  };
}

export function isWxAppid(value) {
  return APPID_PATTERN.test(String(value || '').trim());
}

function collectAppidHints(candidate) {
  const hints = [];
  const pathInputs = [
    { value: candidate.dir, source: 'path:directory', score: 100 },
    ...candidate.packages.map((packageInfo) => ({
      value: packageInfo.path,
      source: `path:${packageInfo.name}`,
      score: 96
    }))
  ];

  for (const input of pathInputs) {
    const parts = splitPathParts(input.value);

    for (let index = parts.length - 1; index >= 0; index -= 1) {
      const part = parts[index];
      if (isWxAppid(part)) {
        hints.push({
          value: part,
          source: input.source,
          score: input.score + Math.max(0, 8 - (parts.length - 1 - index))
        });
      }
    }

    for (const value of extractAppids(input.value)) {
      hints.push({
        value,
        source: input.source,
        score: input.score - 12
      });
    }
  }

  return hints;
}

async function collectMetadataHints(dir, appid) {
  const result = { appids: [], names: [] };
  const metadataFiles = await getMetadataSearchFiles(dir, appid);

  for (const filePath of metadataFiles) {
    const rawText = await readMetadataText(filePath);
    if (!rawText) {
      continue;
    }

    const source = `metadata:${filePath}`;
    for (const foundAppid of extractAppids(rawText)) {
      result.appids.push({ value: foundAppid, source, score: 60 });
    }

    const json = parseJson(rawText);
    if (json) {
      collectJsonHints(json, source, result);
    } else {
      collectLooseTextHints(rawText, source, result);
    }
  }

  return result;
}

async function getMetadataSearchFiles(dir, appid) {
  const files = [];
  const directDirs = getMetadataSearchDirs(dir, appid);

  for (const metadataDir of directDirs) {
    await collectMetadataFilesInDir(metadataDir, files);
  }

  const appRoot = findAppRootDir(dir, appid);
  if (appRoot) {
    await collectMetadataFilesRecursive(appRoot, files, new Set(), 0);
  }

  return Array.from(new Set(files)).slice(0, MAX_METADATA_FILES);
}

function getMetadataSearchDirs(dir, appid) {
  const dirs = new Set();
  let current = dir;

  for (let index = 0; index < MAX_METADATA_ANCESTORS; index += 1) {
    dirs.add(current);
    if (appid && path.basename(current).toLowerCase() === appid.toLowerCase()) {
      break;
    }

    const parent = path.dirname(current);
    if (parent === current) {
      break;
    }
    current = parent;
  }

  const appRoot = findAppRootDir(dir, appid);
  if (appRoot) {
    dirs.add(appRoot);
  }

  return Array.from(dirs);
}

function findAppRootDir(dir, appid) {
  if (!appid) {
    return null;
  }

  let current = dir;

  while (true) {
    if (path.basename(current).toLowerCase() === appid.toLowerCase()) {
      return current;
    }

    const parent = path.dirname(current);
    if (parent === current) {
      return null;
    }

    current = parent;
  }
}

async function collectMetadataFilesInDir(dir, files) {
  let entries;
  try {
    entries = await fs.readdir(dir, { withFileTypes: true });
  } catch {
    return;
  }

  for (const entry of entries) {
    if (files.length >= MAX_METADATA_FILES) {
      return;
    }

    if (!entry.isFile() || !shouldReadMetadataFile(entry.name)) {
      continue;
    }

    const filePath = path.join(dir, entry.name);
    if (await isReadableMetadataFile(filePath)) {
      files.push(filePath);
    }
  }
}

async function collectMetadataFilesRecursive(dir, files, seenDirs, depth) {
  if (files.length >= MAX_METADATA_FILES || depth > MAX_METADATA_SCAN_DEPTH) {
    return;
  }

  const normalizedDir = path.normalize(dir);
  if (seenDirs.has(normalizedDir)) {
    return;
  }
  seenDirs.add(normalizedDir);

  let entries;
  try {
    entries = await fs.readdir(dir, { withFileTypes: true });
  } catch {
    return;
  }

  const sortedEntries = entries.sort((a, b) => metadataEntryScore(b.name) - metadataEntryScore(a.name));

  for (const entry of sortedEntries) {
    if (files.length >= MAX_METADATA_FILES) {
      return;
    }

    const fullPath = path.join(dir, entry.name);

    if (entry.isFile()) {
      if (shouldReadMetadataFile(entry.name) && await isReadableMetadataFile(fullPath)) {
        files.push(fullPath);
      }
      continue;
    }

    if (entry.isDirectory() && !shouldSkipMetadataDir(entry.name)) {
      await collectMetadataFilesRecursive(fullPath, files, seenDirs, depth + 1);
    }
  }
}

function shouldReadMetadataFile(fileName) {
  const lowerName = fileName.toLowerCase();
  const extension = path.extname(lowerName);

  if (lowerName === 'package.json' || lowerName === 'package-lock.json') {
    return false;
  }

  if (extension && !['.json', '.txt', '.info', '.config', '.conf', '.dat'].includes(extension)) {
    return false;
  }

  return /(account|app|config|game|info|manifest|metadata|name|profile|project|weapp|wx)/.test(lowerName);
}

function shouldSkipMetadataDir(dirName) {
  const lowerName = dirName.toLowerCase();
  return lowerName === 'node_modules'
    || lowerName === '.git'
    || lowerName === 'decoded'
    || lowerName === 'output'
    || lowerName === 'outputs'
    || lowerName === 'tmp'
    || lowerName === 'temp';
}

async function isReadableMetadataFile(filePath) {
  const stats = await fs.stat(filePath).catch(() => null);
  return Boolean(stats?.isFile() && stats.size > 0 && stats.size <= MAX_METADATA_FILE_SIZE);
}

async function readMetadataText(filePath) {
  const buffer = await fs.readFile(filePath).catch(() => null);
  if (!buffer || buffer.length === 0) {
    return null;
  }

  const sampleLength = Math.min(buffer.length, 512);
  let nulCount = 0;
  for (let index = 0; index < sampleLength; index += 1) {
    if (buffer[index] === 0) {
      nulCount += 1;
    }
  }

  if (nulCount > sampleLength * 0.05) {
    return null;
  }

  return buffer.toString('utf8');
}

function metadataEntryScore(fileName) {
  const lowerName = fileName.toLowerCase();

  if (/app.?info|game.?info|account.?info/.test(lowerName)) {
    return 50;
  }
  if (/manifest|config|profile/.test(lowerName)) {
    return 40;
  }
  if (/name|project|weapp|wx/.test(lowerName)) {
    return 30;
  }
  if (lowerName.endsWith('.json')) {
    return 20;
  }

  return 0;
}

function parseJson(rawText) {
  try {
    return JSON.parse(rawText.replace(/^\uFEFF/, ''));
  } catch {
    return null;
  }
}

function collectJsonHints(value, source, result, keyPath = [], depth = 0) {
  if (!value || depth > MAX_JSON_DEPTH) {
    return;
  }

  if (Array.isArray(value)) {
    for (const item of value.slice(0, 50)) {
      collectJsonHints(item, source, result, keyPath, depth + 1);
    }
    return;
  }

  if (typeof value !== 'object') {
    return;
  }

  for (const [key, child] of Object.entries(value).slice(0, 200)) {
    const childPath = [...keyPath, key];
    const normalizedKey = normalizeKey(key);

    if (typeof child === 'string') {
      if (isAppidKey(key, normalizedKey)) {
        for (const appid of extractAppids(child)) {
          result.appids.push({
            value: appid,
            source: `${source}:${childPath.join('.')}`,
            score: 90
          });
        }
      }

      const nameScore = getNameScore(normalizedKey, childPath);
      if (nameScore) {
        const name = cleanNameCandidate(child);
        if (name) {
          result.names.push({
            value: name,
            source: `${source}:${childPath.join('.')}`,
            score: nameScore
          });
        }
      }
    }

    collectJsonHints(child, source, result, childPath, depth + 1);
  }
}

function getNameScore(normalizedKey, keyPath) {
  const score = NAME_KEY_SCORES.get(normalizedKey);
  if (!score) {
    return null;
  }

  const normalizedPath = keyPath.map(normalizeKey);
  if (normalizedKey === 'name' && normalizedPath.some(isNonAppNameContext)) {
    return null;
  }

  if (normalizedKey === 'title' && normalizedPath.some(isNonAppNameContext)) {
    return null;
  }

  return score;
}

function isNonAppNameContext(normalizedKey) {
  return normalizedKey === 'subpackages'
    || normalizedKey === 'subpackagesv2'
    || normalizedKey === 'packages'
    || normalizedKey === 'pages'
    || normalizedKey === 'plugins'
    || normalizedKey === 'preloadrule'
    || normalizedKey === 'tabbar'
    || normalizedKey === 'usingcomponents'
    || normalizedKey === 'window'
    || normalizedKey === 'workers';
}

function collectLooseTextHints(rawText, source, result) {
  const keyPattern = '(miniGameName|gameName|nickname|appName|miniProgramName|displayName|appDisplayName|appTitle|title|projectName|name)';
  const quotedPattern = new RegExp(`["']${keyPattern}["']\\s*[:=]\\s*["']([^"'\\n]{2,80})["']`, 'ig');
  const plainPattern = new RegExp(`\\b${keyPattern}\\b\\s*[:=]\\s*([^\\n\\r]{2,80})`, 'ig');

  for (const match of rawText.matchAll(quotedPattern)) {
    const normalizedKey = normalizeKey(match[1]);
    const name = cleanNameCandidate(match[2]);
    const score = NAME_KEY_SCORES.get(normalizedKey);
    if (name && score) {
      result.names.push({
        value: name,
        source: `${source}:${match[1]}`,
        score: score - 4
      });
    }
  }

  for (const match of rawText.matchAll(plainPattern)) {
    const normalizedKey = normalizeKey(match[1]);
    const name = cleanNameCandidate(match[2]);
    const score = NAME_KEY_SCORES.get(normalizedKey);
    if (name && score) {
      result.names.push({
        value: name,
        source: `${source}:${match[1]}`,
        score: score - 8
      });
    }
  }
}

function isAppidKey(key, normalizedKey) {
  return APPID_KEYS.has(key) || APPID_KEYS.has(normalizedKey) || normalizedKey.endsWith('appid');
}

function normalizeKey(key) {
  return String(key || '').replace(/[-_\s]/g, '').toLowerCase();
}

function collectPathNameHints(candidate) {
  const parts = splitPathParts(candidate.dir);
  const hints = [];
  const lastAppidIndex = findLastIndex(parts, isWxAppid);
  const minIndex = Math.max(0, parts.length - 8);

  for (let index = parts.length - 1; index >= minIndex; index -= 1) {
    const name = cleanPathNamePart(parts[index]);
    if (!name) {
      continue;
    }

    let score = 42;
    if (containsCjk(name)) {
      score += 20;
    }
    if (index === parts.length - 1) {
      score += 8;
    }
    if (lastAppidIndex !== -1 && Math.abs(index - lastAppidIndex) === 1) {
      score += 10;
    }

    hints.push({
      value: name,
      source: `path:${parts.slice(Math.max(0, index - 1), index + 2).join('/')}`,
      score
    });
  }

  return hints;
}

function collectPackageNameHints(candidate) {
  return candidate.packages
    .filter((packageInfo) => packageInfo.kind === 'package')
    .map((packageInfo) => ({
      value: cleanNameCandidate(path.basename(packageInfo.name, path.extname(packageInfo.name))),
      source: `package:${packageInfo.name}`,
      score: 34
    }))
    .filter((hint) => hint.value);
}

function cleanPathNamePart(part) {
  const clean = cleanNameCandidate(part);
  if (!clean) {
    return null;
  }

  const normalized = clean.toLowerCase();
  if (GENERIC_PATH_PARTS.has(normalized)) {
    return null;
  }
  if (/^v?\d+(?:[._-]\d+)*$/i.test(clean)) {
    return null;
  }
  if (/^[0-9a-f]{12,}$/i.test(clean)) {
    return null;
  }
  if (/^[-_.]+$/.test(clean)) {
    return null;
  }

  return clean;
}

function cleanNameCandidate(value) {
  if (typeof value !== 'string') {
    return null;
  }

  let clean = value.trim().replace(/\s+/g, ' ');
  if (!clean) {
    return null;
  }

  try {
    clean = decodeURIComponent(clean);
  } catch {
    // Keep the original path part if it is not URI-encoded.
  }

  clean = clean.trim().replace(/^["'`]+|["'`]+$/g, '');
  clean = clean.replace(/[",;}\]]+$/g, '').trim();
  if (clean.length < 2 || clean.length > 80) {
    return null;
  }
  if (GENERIC_PATH_PARTS.has(clean.toLowerCase())) {
    return null;
  }
  if (isWxAppid(clean)) {
    return null;
  }
  if (/[\\/]/.test(clean)) {
    return null;
  }
  if (/\.(?:wxapkg|json|js|wxss|wxml)$/i.test(clean)) {
    return null;
  }

  return clean;
}

function makePathHint(dir, appid) {
  const parts = splitPathParts(dir);
  const appidIndex = appid ? parts.findIndex((part) => part.toLowerCase() === appid.toLowerCase()) : -1;

  if (appidIndex !== -1) {
    return parts.slice(Math.max(0, appidIndex - 1), Math.min(parts.length, appidIndex + 3)).join('/');
  }

  return parts.slice(-4).join('/');
}

function buildClues({ appidHint, nameHint, fallbackNameHint, pathHint, latestPackage }) {
  const clues = [];

  if (appidHint) {
    clues.push({ kind: 'appid', value: appidHint.value, source: appidHint.source });
  }

  if (nameHint) {
    clues.push({ kind: 'name', value: nameHint.value, source: nameHint.source });
  }

  if (fallbackNameHint) {
    clues.push({ kind: 'fallbackName', value: fallbackNameHint.value, source: fallbackNameHint.source });
  }

  if (pathHint) {
    clues.push({ kind: 'path', value: pathHint, source: 'path:directory' });
  }

  if (latestPackage) {
    clues.push({
      kind: 'mtime',
      value: latestPackage.mtimeMs,
      source: `package:${latestPackage.name}`
    });
  }

  return clues;
}

function findLatestPackage(packages) {
  return packages.reduce((latest, packageInfo) => {
    if (!latest || packageInfo.mtimeMs > latest.mtimeMs) {
      return packageInfo;
    }

    return latest;
  }, null);
}

function bestHint(hints) {
  const deduped = new Map();

  for (const hint of hints) {
    if (!hint?.value) {
      continue;
    }

    const key = hint.value.toLowerCase();
    const previous = deduped.get(key);
    if (!previous || hint.score > previous.score) {
      deduped.set(key, hint);
    }
  }

  return Array.from(deduped.values()).sort((a, b) => {
    if (b.score !== a.score) {
      return b.score - a.score;
    }

    return a.value.length - b.value.length;
  })[0] ?? null;
}

function extractAppids(input) {
  const seen = new Set();
  const appids = [];
  const matches = String(input || '').match(APPID_SCAN_PATTERN) ?? [];

  for (const match of matches) {
    const value = match.toLowerCase();
    if (!seen.has(value) && isWxAppid(value)) {
      seen.add(value);
      appids.push(value);
    }
  }

  return appids;
}

function splitPathParts(input) {
  return path.normalize(String(input || ''))
    .split(/[\\/]+/)
    .filter(Boolean);
}

function findLastIndex(items, predicate) {
  for (let index = items.length - 1; index >= 0; index -= 1) {
    if (predicate(items[index])) {
      return index;
    }
  }

  return -1;
}

function containsCjk(value) {
  return /[\u3400-\u9FFF]/.test(value);
}
