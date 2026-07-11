import fs from 'node:fs/promises';
import path from 'node:path';
import { identifyCandidate } from './identity.js';
import { getDefaultSearchRoots, normalizePath, uniquePaths } from './paths.js';

const DEFAULT_MAX_DEPTH = 14;

const IGNORED_DIRS = new Set([
  '.git',
  '.hg',
  '.svn',
  'node_modules',
  'Library',
  'Applications',
  'Movies',
  'Music',
  'Pictures',
  '.Trash'
]);

export async function scanWxapkg(options = {}) {
  const roots = uniquePaths(options.roots?.length ? options.roots : getDefaultSearchRoots());
  const maxDepth = Number.isFinite(Number(options.maxDepth)) ? Number(options.maxDepth) : DEFAULT_MAX_DEPTH;
  const filters = normalizeScanFilters(options);
  const packages = [];
  const rootErrors = [];
  const state = { visitedDirs: 0 };

  for (const root of roots) {
    try {
      const stats = await fs.stat(root);

      if (stats.isFile()) {
        if (isWxapkgFile(root)) {
          packages.push(await packageFromFile(root));
        }
        continue;
      }

      if (stats.isDirectory()) {
        await walkDir(root, 0, maxDepth, packages, state);
      }
    } catch (error) {
      rootErrors.push({ root, message: error.message });
    }
  }

  const allCandidates = await groupPackages(packages, Boolean(options.includePublicLib));
  const filteredCandidates = applyCandidateFilters(allCandidates, filters);
  const candidates = filters.limit ? filteredCandidates.slice(0, filters.limit) : filteredCandidates;

  return {
    roots,
    rootErrors,
    visitedDirs: state.visitedDirs,
    packages,
    candidates,
    totalCandidates: allCandidates.length,
    filteredCandidates: filteredCandidates.length,
    limit: filters.limit,
    filters: {
      appid: filters.appid,
      sinceMs: filters.sinceMs
    }
  };
}

export function isWxapkgFile(filePath) {
  return path.basename(filePath).toLowerCase().endsWith('.wxapkg');
}

export async function inspectWxapkgTarget(target) {
  const fullPath = normalizePath(target);
  const stats = await fs.stat(fullPath).catch(() => null);

  if (!stats) {
    throw new Error(`输入路径不存在：${fullPath}`);
  }

  if (stats.isFile()) {
    if (!isWxapkgFile(fullPath)) {
      throw new Error(`输入文件不是 wxapkg：${fullPath}`);
    }

    return buildCandidate(path.dirname(fullPath), [await packageFromFile(fullPath)]);
  }

  if (!stats.isDirectory()) {
    throw new Error(`输入路径必须是 wxapkg 文件或目录：${fullPath}`);
  }

  const entries = await fs.readdir(fullPath, { withFileTypes: true });
  const packages = [];

  for (const entry of entries) {
    if (entry.isFile() && isWxapkgFile(entry.name)) {
      packages.push(await packageFromFile(path.join(fullPath, entry.name)));
    }
  }

  if (packages.length === 0) {
    throw new Error(`目录下没有找到 wxapkg 文件：${fullPath}`);
  }

  return buildCandidate(fullPath, packages);
}

async function walkDir(dir, depth, maxDepth, packages, state) {
  if (depth > maxDepth) {
    return;
  }

  state.visitedDirs += 1;

  let entries;
  try {
    entries = await fs.readdir(dir, { withFileTypes: true });
  } catch {
    return;
  }

  for (const entry of entries) {
    const fullPath = path.join(dir, entry.name);

    if (entry.isDirectory()) {
      if (!shouldSkipDir(entry.name)) {
        await walkDir(fullPath, depth + 1, maxDepth, packages, state);
      }
      continue;
    }

    if (entry.isFile() && isWxapkgFile(entry.name)) {
      try {
        packages.push(await packageFromFile(fullPath));
      } catch {
        // The file may disappear while WeChat is updating its cache.
      }
    }
  }
}

function shouldSkipDir(dirName) {
  return IGNORED_DIRS.has(dirName);
}

async function packageFromFile(filePath) {
  const fullPath = normalizePath(filePath);
  const stats = await fs.stat(fullPath);
  const name = path.basename(fullPath);

  return {
    path: fullPath,
    dir: path.dirname(fullPath),
    name,
    kind: classifyPackageName(name),
    size: stats.size,
    mtimeMs: stats.mtimeMs
  };
}

function classifyPackageName(name) {
  const lowerName = name.toLowerCase();

  if (lowerName === 'publiclib.wxapkg') {
    return 'publicLib';
  }

  if (
    lowerName === 'app.wxapkg'
    || lowerName === '__app__.wxapkg'
    || lowerName === '__without_multi_plugincode__.wxapkg'
  ) {
    return 'main';
  }

  return 'package';
}

async function groupPackages(packages, includePublicLib) {
  const groups = new Map();

  for (const packageInfo of packages) {
    if (!groups.has(packageInfo.dir)) {
      groups.set(packageInfo.dir, []);
    }

    groups.get(packageInfo.dir).push(packageInfo);
  }

  const candidates = await Promise.all(
    Array.from(groups.entries()).map(([dir, packageList]) => buildCandidate(dir, packageList))
  );

  return candidates
    .filter((candidate) => includePublicLib || candidate.packageCount > candidate.publicLibCount)
    .sort((a, b) => {
      if (b.latestMtimeMs !== a.latestMtimeMs) {
        return b.latestMtimeMs - a.latestMtimeMs;
      }

      if (b.mainCount !== a.mainCount) {
        return b.mainCount - a.mainCount;
      }

      return b.totalSize - a.totalSize;
    });
}

function normalizeScanFilters(options) {
  return {
    appid: normalizeAppid(options.appid),
    sinceMs: parseSince(options.since),
    limit: parseLimit(options.limit)
  };
}

function applyCandidateFilters(candidates, filters) {
  return candidates.filter((candidate) => {
    if (filters.appid && candidate.identity?.appid?.toLowerCase() !== filters.appid) {
      return false;
    }

    if (filters.sinceMs && candidate.latestMtimeMs < filters.sinceMs) {
      return false;
    }

    return true;
  });
}

function normalizeAppid(appid) {
  if (!appid) {
    return null;
  }

  const value = String(appid).trim().toLowerCase();
  if (!/^wx[0-9a-f]{16}$/i.test(value)) {
    throw new Error(`无效 appid：${appid}`);
  }

  return value;
}

function parseSince(since) {
  if (!since) {
    return null;
  }

  const rawValue = String(since).trim();
  const dateOnlyMatch = rawValue.match(/^(\d{4})-(\d{2})-(\d{2})$/);
  const timestamp = dateOnlyMatch
    ? new Date(Number(dateOnlyMatch[1]), Number(dateOnlyMatch[2]) - 1, Number(dateOnlyMatch[3])).getTime()
    : Date.parse(rawValue);
  if (!Number.isFinite(timestamp)) {
    throw new Error(`无法解析 --since 日期：${since}`);
  }

  return timestamp;
}

function parseLimit(limit) {
  if (limit === undefined || limit === null || limit === '') {
    return null;
  }

  const value = Number(limit);
  if (!Number.isFinite(value) || value < 0) {
    throw new Error(`无效 --limit：${limit}`);
  }

  if (value === 0) {
    return null;
  }

  return Math.floor(value);
}

async function buildCandidate(dir, packageList) {
  const sortedPackages = [...packageList].sort((a, b) => {
    if (a.kind === b.kind) {
      return a.name.localeCompare(b.name);
    }

    return packageKindScore(b.kind) - packageKindScore(a.kind);
  });

  const candidate = {
    dir,
    packages: sortedPackages,
    packageCount: sortedPackages.length,
    mainCount: sortedPackages.filter((item) => item.kind === 'main').length,
    publicLibCount: sortedPackages.filter((item) => item.kind === 'publicLib').length,
    totalSize: sortedPackages.reduce((sum, item) => sum + item.size, 0),
    latestMtimeMs: Math.max(...sortedPackages.map((item) => item.mtimeMs))
  };

  candidate.identity = await identifyCandidate(candidate);

  return candidate;
}

function packageKindScore(kind) {
  if (kind === 'main') {
    return 3;
  }

  if (kind === 'package') {
    return 2;
  }

  return 1;
}
