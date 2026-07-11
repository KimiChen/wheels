import fs from 'node:fs/promises';
import path from 'node:path';
import { formatBytes, shortenPath } from './report.js';

const MAX_WALK_FILES = 5000;
const ASSET_EXTENSIONS = new Set([
  '.png',
  '.jpg',
  '.jpeg',
  '.gif',
  '.webp',
  '.svg',
  '.mp3',
  '.wav',
  '.ogg',
  '.mp4',
  '.json',
  '.atlas',
  '.sk',
  '.lh',
  '.ls',
  '.ttf',
  '.fnt',
  '.wasm'
]);

export async function summarizeOutputDir(outDir) {
  const root = path.resolve(outDir);
  const stats = await fs.stat(root).catch(() => null);

  if (!stats?.isDirectory()) {
    return {
      outDir: root,
      exists: false
    };
  }

  const summary = {
    outDir: root,
    exists: true,
    totalFiles: 0,
    totalDirs: 0,
    totalSize: 0,
    truncated: false,
    keyFiles: [],
    counts: {
      js: 0,
      json: 0,
      wxml: 0,
      wxss: 0,
      assets: 0
    }
  };

  await walkOutputDir(root, root, summary);
  summary.keyFiles = sortKeyFiles(summary.keyFiles);

  return summary;
}

export function printOutputSummary(summary) {
  if (!summary?.exists) {
    console.log(`\n输出统计：未找到输出目录 ${shortenPath(summary?.outDir || '')}`);
    return;
  }

  console.log('\n输出统计：');
  console.log(`  目录：${shortenPath(summary.outDir)}`);
  console.log(`  文件：${summary.totalFiles}${summary.truncated ? '+' : ''}，目录：${summary.totalDirs}，大小：${formatBytes(summary.totalSize)}`);
  console.log(`  JS：${summary.counts.js}，JSON：${summary.counts.json}，WXML：${summary.counts.wxml}，WXSS：${summary.counts.wxss}，资源：${summary.counts.assets}`);

  if (summary.keyFiles.length > 0) {
    console.log('  关键文件：');
    for (const filePath of summary.keyFiles) {
      console.log(`    - ${filePath}`);
    }
  } else {
    console.log('  关键文件：未发现 app.json、game.json、app-config.json、game.js 等常见入口文件');
  }
}

async function walkOutputDir(root, dir, summary) {
  if (summary.truncated) {
    return;
  }

  let entries;
  try {
    entries = await fs.readdir(dir, { withFileTypes: true });
  } catch {
    return;
  }

  for (const entry of entries) {
    if (summary.totalFiles >= MAX_WALK_FILES) {
      summary.truncated = true;
      return;
    }

    const fullPath = path.join(dir, entry.name);

    if (entry.isDirectory()) {
      summary.totalDirs += 1;
      await walkOutputDir(root, fullPath, summary);
      continue;
    }

    if (!entry.isFile()) {
      continue;
    }

    const stats = await fs.stat(fullPath).catch(() => null);
    summary.totalFiles += 1;
    summary.totalSize += stats?.size || 0;
    collectFileInfo(root, fullPath, summary);
  }
}

function collectFileInfo(root, fullPath, summary) {
  const relativePath = path.relative(root, fullPath).split(path.sep).join('/');
  const ext = path.extname(fullPath).toLowerCase();

  if (ext === '.js') {
    summary.counts.js += 1;
  } else if (ext === '.json') {
    summary.counts.json += 1;
  } else if (ext === '.wxml') {
    summary.counts.wxml += 1;
  } else if (ext === '.wxss') {
    summary.counts.wxss += 1;
  }

  if (ASSET_EXTENSIONS.has(ext) && !['.js', '.wxml', '.wxss'].includes(ext)) {
    summary.counts.assets += 1;
  }

  if (isKeyFile(relativePath)) {
    summary.keyFiles.push(relativePath);
  }
}

function isKeyFile(relativePath) {
  const baseName = path.posix.basename(relativePath);
  return baseName === 'app.json'
    || baseName === 'app-config.json'
    || baseName === 'game.json'
    || baseName === 'game.js'
    || baseName === 'app.js'
    || baseName === 'project.config.json'
    || baseName === 'project.private.config.json';
}

function sortKeyFiles(files) {
  const priority = new Map([
    ['app.json', 1],
    ['app-config.json', 2],
    ['game.json', 3],
    ['game.js', 4],
    ['app.js', 5],
    ['project.config.json', 6],
    ['project.private.config.json', 7]
  ]);

  return [...files].sort((a, b) => {
    const scoreA = priority.get(path.posix.basename(a)) || 99;
    const scoreB = priority.get(path.posix.basename(b)) || 99;

    if (scoreA !== scoreB) {
      return scoreA - scoreB;
    }

    return a.localeCompare(b);
  });
}
