import fs from 'node:fs/promises';
import { Command } from 'commander';
import packageJson from '../package.json' with { type: 'json' };
import { decodeWxapkg, defaultOutputDir } from './decode.js';
import { printDecodeErrorHelp } from './error-help.js';
import { printOutputSummary, summarizeOutputDir } from './output-summary.js';
import { normalizePath } from './paths.js';
import { printPackageList, printScanResults, shortenPath, toJsonSafeResult } from './report.js';
import { inspectWxapkgTarget, scanWxapkg } from './scan.js';
import {
  chooseCandidate,
  chooseDecodeMode,
  chooseExistingOutputAction,
  confirmClearOutput,
  confirmDecode,
  inputOutputDir
} from './select.js';

export async function run(argv = process.argv) {
  const program = new Command();

  program
    .name('wxapkg-helper')
    .description('扫描微信小游戏/小程序缓存中的 wxapkg，并使用项目内置引擎反解。')
    .version(packageJson.version);

  program
    .command('scan')
    .description('扫描缓存目录并列出 wxapkg 候选目录')
    .option('-r, --root <path>', '指定扫描根目录，可重复传入', collect, [])
    .option('--max-depth <n>', '最大递归深度', '14')
    .option('--include-public-lib', '显示只有 publicLib.wxapkg 的目录')
    .option('--appid <wxid>', '只显示指定游戏标识 ID/appid 的候选目录')
    .option('--since <date>', '只显示指定日期之后修改过的候选目录')
    .option('--limit <n>', '最多显示最近的候选数量，默认显示全部')
    .option('--json', '输出 JSON')
    .action(async (options) => {
      const result = await scanWxapkg({
        roots: options.root,
        maxDepth: Number(options.maxDepth),
        includePublicLib: options.includePublicLib,
        appid: options.appid,
        since: options.since,
        limit: options.limit
      });

      if (options.json) {
        console.log(JSON.stringify(toJsonSafeResult(result), null, 2));
      } else {
        printScanResults(result);
      }
    });

  program
    .command('decode')
    .description('选择或指定 wxapkg 文件/目录，并使用内置引擎反解')
    .argument('[target]', 'wxapkg 文件或含 wxapkg 的目录')
    .option('-r, --root <path>', '未指定 target 时，用此根目录扫描，可重复传入', collect, [])
    .option('-o, --out <path>', '输出目录')
    .option('--max-depth <n>', '最大递归深度', '14')
    .option('--include-public-lib', '选择列表中显示，并在目录反解时包含 publicLib.wxapkg')
    .option('--appid <wxid>', '未指定 target 时，只显示指定游戏标识 ID/appid 的候选目录')
    .option('--since <date>', '未指定 target 时，只显示指定日期之后修改过的候选目录')
    .option('--limit <n>', '未指定 target 时，最多显示最近的候选数量，默认显示全部')
    .option('--list-packages', '只预览目标目录包清单，不执行反解')
    .option('--clear', '清空旧产物')
    .option('--open-dir', '完成后打开输出目录')
    .option('--px', '使用 px 而不是 rpx 解析 css')
    .option('--unpack-only', '只解包不反编译')
    .option('--wxid <wxid>', '指定加密包的微信小程序 WXID')
    .option('--dry-run', '只打印内置反编译执行计划')
    .option('-y, --yes', '跳过确认')
    .action(async (target, options) => {
      await runDecode({ ...options, target });
    });

  if (argv.length <= 2) {
    await runDecode({});
    return;
  }

  await program.parseAsync(argv);
}

async function runDecode(options) {
  printDisclaimer();

  const candidate = options.target ? null : await selectTargetFromScan(options);
  const target = options.target ? normalizePath(options.target) : candidate.dir;
  let outDir = options.out ? normalizePath(options.out) : defaultOutputDir(target, candidate?.identity);
  let unpackOnly = Boolean(options.unpackOnly);

  if (options.listPackages) {
    const packageCandidate = candidate ?? await inspectWxapkgTarget(target);
    printPackageList(packageCandidate);
    return;
  }

  if (candidate && !options.yes && !options.unpackOnly) {
    unpackOnly = await chooseDecodeMode();
  }

  outDir = await resolveOutputDir({
    outDir,
    clear: Boolean(options.clear),
    yes: Boolean(options.yes),
    dryRun: Boolean(options.dryRun)
  });

  if (!options.yes) {
    const ok = await confirmDecode({
      target,
      outDir,
      clear: Boolean(options.clear),
      unpackOnly,
      candidate
    });

    if (!ok) {
      console.log('已取消。');
      return;
    }
  }

  let result;
  try {
    result = await decodeWxapkg({
      target,
      outDir,
      clear: Boolean(options.clear),
      openDir: Boolean(options.openDir),
      px: Boolean(options.px),
      unpackOnly,
      wxid: options.wxid,
      identity: candidate?.identity,
      includePublicLib: Boolean(options.includePublicLib),
      dryRun: Boolean(options.dryRun)
    });
  } catch (error) {
    printDecodeErrorHelp(error, {
      target,
      outDir,
      clear: Boolean(options.clear),
      unpackOnly
    });
    throw error;
  }

  if (!result.skipped) {
    console.log(`\n完成，输出目录：${shortenPath(result.outDir)}`);
    printOutputSummary(await summarizeOutputDir(result.outDir));
  }
}

async function selectTargetFromScan(options) {
  const result = await scanWxapkg({
    roots: options.root,
    maxDepth: Number(options.maxDepth ?? 14),
    includePublicLib: Boolean(options.includePublicLib),
    appid: options.appid,
    since: options.since,
    limit: options.limit
  });

  printScanResults(result);

  if (result.candidates.length === 0) {
    throw new Error('没有找到可反解的 wxapkg 候选目录。请先打开目标小游戏让微信缓存包，或用 --root 指定缓存目录。');
  }

  const candidate = await chooseCandidate(result.candidates);
  return candidate;
}

async function resolveOutputDir({ outDir, clear, yes, dryRun }) {
  if (dryRun) {
    return outDir;
  }

  let currentOutDir = outDir;

  while (await pathExists(currentOutDir)) {
    if (clear) {
      if (yes) {
        console.log(`警告：输出目录已存在，--clear 将清空：${shortenPath(currentOutDir)}`);
        return currentOutDir;
      }

      const ok = await confirmClearOutput(currentOutDir);
      if (!ok) {
        throw new Error('已取消。');
      }

      return currentOutDir;
    }

    if (yes) {
      console.log(`警告：输出目录已存在且未传 --clear，将继续写入：${shortenPath(currentOutDir)}`);
      return currentOutDir;
    }

    const action = await chooseExistingOutputAction(currentOutDir);
    if (action === 'continue') {
      return currentOutDir;
    }

    if (action === 'cancel') {
      throw new Error('已取消。');
    }

    currentOutDir = normalizePath(await inputOutputDir(currentOutDir));
  }

  return currentOutDir;
}

async function pathExists(targetPath) {
  return fs.stat(targetPath).then(() => true, () => false);
}

function collect(value, previous) {
  previous.push(value);
  return previous;
}

function printDisclaimer() {
  console.log('仅用于你拥有权利或已获授权的代码审计、恢复和学习场景。请遵守相关法律与平台协议。\n');
}
