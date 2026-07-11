import path from 'node:path';

export function formatBytes(bytes) {
  if (!Number.isFinite(bytes) || bytes < 0) {
    return '-';
  }

  const units = ['B', 'KB', 'MB', 'GB'];
  let value = bytes;
  let unitIndex = 0;

  while (value >= 1024 && unitIndex < units.length - 1) {
    value /= 1024;
    unitIndex += 1;
  }

  const digits = value >= 10 || unitIndex === 0 ? 0 : 1;
  return `${value.toFixed(digits)}${units[unitIndex]}`;
}

export function formatDate(timestamp) {
  if (!timestamp) {
    return '-';
  }

  const date = new Date(timestamp);
  const pad = (value) => String(value).padStart(2, '0');

  return [
    date.getFullYear(),
    pad(date.getMonth() + 1),
    pad(date.getDate())
  ].join('-') + ` ${pad(date.getHours())}:${pad(date.getMinutes())}`;
}

export function shortenPath(fullPath) {
  const home = process.env.HOME || process.env.USERPROFILE;

  if (home && fullPath.startsWith(home)) {
    return `~${fullPath.slice(home.length)}`;
  }

  return fullPath;
}

export function formatCandidateLabel(candidate, index) {
  const countLabel = `${candidate.packageCount} pkg${candidate.packageCount > 1 ? 's' : ''}`;
  const mainLabel = candidate.mainCount > 0 ? 'main' : 'no-main';
  const publicLibLabel = candidate.publicLibCount > 0 ? ` publicLib:${candidate.publicLibCount}` : '';
  const gameNameLabel = formatGameName(candidate).padEnd(30);
  const gameIdLabel = (candidate.identity?.gameId || candidate.identity?.appid || '-').padEnd(18);

  return [
    `[${String(index + 1).padStart(2, '0')}]`,
    formatDate(candidate.latestMtimeMs).padEnd(16),
    gameNameLabel,
    gameIdLabel,
    formatBytes(candidate.totalSize).padStart(7),
    countLabel.padStart(6),
    mainLabel.padEnd(7),
    `${publicLibLabel}`.padEnd(13),
    shortenPath(candidate.dir)
  ].join(' ');
}

export function formatPackageLabel(packageInfo) {
  return [
    packageInfo.kind.padEnd(9),
    formatDate(packageInfo.mtimeMs).padEnd(16),
    formatBytes(packageInfo.size).padStart(7),
    packageInfo.name
  ].join(' ');
}

export function formatPackageList(candidate, options = {}) {
  const maxItems = Number.isFinite(options.maxItems) ? options.maxItems : 8;
  const packages = candidate.packages.slice(0, maxItems);
  const lines = packages.map((packageInfo) => `     - ${formatPackageLabel(packageInfo)}`);
  const hiddenCount = candidate.packages.length - packages.length;

  if (hiddenCount > 0) {
    lines.push(`     - ... 还有 ${hiddenCount} 个包`);
  }

  return lines;
}

export function formatIdentityLabel(candidate) {
  const identity = candidate?.identity;
  if (!identity) {
    return '未识别';
  }

  if (identity.name && identity.appid) {
    return `${identity.name} (标识 ID: ${identity.appid})`;
  }

  if (identity.name) {
    return identity.name;
  }

  if (identity.appid) {
    return `未识别名称 (标识 ID: ${identity.appid})`;
  }

  return '未识别';
}

export function formatGameName(candidate) {
  return candidate?.identity?.name || '未识别';
}

export function formatIdentitySources(identity) {
  if (!identity) {
    return '';
  }

  const parts = [];
  if (identity.nameSource) {
    parts.push(`名称：${identity.nameSource}`);
  }
  if (identity.appidSource) {
    parts.push(`标识 ID：${identity.appidSource}`);
  }
  if (identity.latestPackage) {
    parts.push(`修改时间：${identity.latestPackage.name}`);
  }

  return parts.join('；');
}

export function printRoots(roots, rootErrors) {
  console.log('扫描根目录：');

  for (const root of roots) {
    console.log(`  - ${shortenPath(root)}`);
  }

  if (rootErrors.length > 0) {
    console.log('\n跳过的根目录：');
    for (const error of rootErrors) {
      console.log(`  - ${shortenPath(error.root)} (${error.message})`);
    }
  }
}

export function printScanResults(result) {
  printRoots(result.roots, result.rootErrors);
  console.log(`\n扫描完成：访问 ${result.visitedDirs} 个目录，发现 ${result.packages.length} 个 wxapkg。`);

  const filters = formatScanFilters(result);
  if (filters) {
    console.log(`过滤条件：${filters}`);
  }

  if (result.candidates.length === 0) {
    console.log('没有找到可用候选目录。可以用 --include-public-lib 显示只有 publicLib.wxapkg 的目录。');
    return;
  }

  console.log('\n小游戏列表（按修改时间倒序）：');
  console.log('     修改时间          游戏名称                       标识 ID(appid)       大小    包数 主包     公共库          路径');
  result.candidates.forEach((candidate, index) => {
    console.log(formatCandidateLabel(candidate, index));
    for (const line of formatPackageList(candidate)) {
      console.log(line);
    }
  });

  if (result.filteredCandidates > result.candidates.length) {
    console.log(`\n已显示最近 ${result.candidates.length} 个候选；共有 ${result.filteredCandidates} 个匹配候选。使用 --limit 0 可显示全部。`);
  }
}

export function printPackageList(candidate) {
  console.log(`目标目录：${shortenPath(candidate.dir)}`);
  console.log(`识别线索：${formatIdentityLabel(candidate)}`);
  console.log(`包数量：${candidate.packageCount}，总大小：${formatBytes(candidate.totalSize)}，最新修改：${formatDate(candidate.latestMtimeMs)}`);
  console.log('\n包清单：');
  console.log('  类型      修改时间          大小    文件名');

  for (const packageInfo of candidate.packages) {
    console.log(`  ${formatPackageLabel(packageInfo)}`);
  }
}

function formatScanFilters(result) {
  const filters = [];

  if (result.filters?.appid) {
    filters.push(`标识ID=${result.filters.appid}`);
  }

  if (result.filters?.sinceMs) {
    filters.push(`since=${formatDate(result.filters.sinceMs)}`);
  }

  if (result.limit) {
    filters.push(`limit=${result.limit}`);
  }

  return filters.join('，');
}

export function toJsonSafeResult(result) {
  return {
    roots: result.roots,
    rootErrors: result.rootErrors,
    visitedDirs: result.visitedDirs,
    packages: result.packages,
    candidates: result.candidates,
    totalCandidates: result.totalCandidates,
    filteredCandidates: result.filteredCandidates,
    limit: result.limit,
    filters: result.filters
  };
}

export function makeOutputSlug(targetPath) {
  const cleanParts = targetPath
    .split(path.sep)
    .filter(Boolean)
    .slice(-3)
    .join('-')
    .replace(/[^\p{L}\p{N}._-]+/gu, '-')
    .replace(/^-+|-+$/g, '');

  return cleanParts || 'wxapkg';
}
