import fs from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { spawn } from 'node:child_process';
import { makeOutputSlug } from './report.js';
import { normalizePath } from './paths.js';
import { inspectWxapkgTarget } from './scan.js';
import {
  createProcessedPackageList,
  inspectPackagePlan,
  unpackPackagePlan
} from './decoder/package-plan.js';
import {
  assertWorkerIsolationSupported,
  describeWorkerIsolation,
  disposeDecoderWorkerPreparation,
  prepareDecoderWorker,
  runDecoderWorker
} from './decoder/run-worker.js';
import { applyWriteManifest } from './decoder/write-manifest.js';

export async function decodeWxapkg(options) {
  const target = normalizePath(options.target);
  const outDir = normalizePath(options.outDir || defaultOutputDir(target, options.identity));
  const targetStats = await fs.stat(target).catch(() => null);

  if (!targetStats) {
    throw new Error(`输入路径不存在：${target}`);
  }

  const candidate = await inspectWxapkgTarget(target);
  const packages = selectPackages(candidate.packages, {
    explicitFile: targetStats.isFile(),
    includePublicLib: Boolean(options.includePublicLib)
  });

  if (packages.length === 0) {
    throw new Error('目录中只有 publicLib.wxapkg；默认不会将公共库合并到目标代码。');
  }

  const wxid = options.wxid || options.identity?.appid || null;
  const isolation = describeWorkerIsolation();
  if (options.workerIsolation?.supported === false) {
    isolation.supported = false;
  }
  const plan = {
    target,
    outDir,
    workDir: process.cwd(),
    packages: packages.map((item) => item.path),
    clear: Boolean(options.clear),
    openDir: Boolean(options.openDir),
    usePx: Boolean(options.px),
    unpackOnly: Boolean(options.unpackOnly),
    wxid,
    isolation
  };

  await assertSafeOutput({ outDir, packagePaths: plan.packages, clear: plan.clear });

  if (options.dryRun) {
    printDryRun(plan);
    return { ...plan, engine: 'internal', skipped: true };
  }

  if (!plan.unpackOnly) {
    assertWorkerIsolationSupported(plan.isolation);
  }

  const inspectedPlan = await inspectPackagePlan(plan.packages, { wxid: plan.wxid });
  const workerPreparation = plan.unpackOnly
    ? null
    : await prepareDecoderWorker({
      packageInfos: inspectedPlan.packages,
      outputPath: outDir
    });

  try {
    if (plan.clear) {
      await fs.rm(outDir, { recursive: true, force: true });
    }

    await fs.mkdir(outDir, { recursive: true });
    const packagePlan = await unpackPackagePlan(inspectedPlan, outDir, {
      wxid: plan.wxid,
      decompilerPaths: !plan.unpackOnly
    });
    const processed = createProcessedPackageList(packagePlan);

    let workerResult = {};
    if (!plan.unpackOnly) {
      const { manifest, ...result } = await runDecoderWorker({
        packageInfos: packagePlan.packages,
        applicationType: packagePlan.applicationType,
        outputPath: outDir,
        usePx: plan.usePx
      }, {
        timeoutMs: options.timeoutMs,
        preparation: workerPreparation
      });
      await applyWriteManifest(outDir, manifest);
      workerResult = result;
    }

    if (plan.openDir) {
      await openDirectory(outDir);
    }

    return {
      ...plan,
      ...workerResult,
      processed,
      engine: 'internal',
      skipped: false
    };
  } finally {
    await disposeDecoderWorkerPreparation(workerPreparation);
  }
}

function selectPackages(packages, options) {
  if (options.explicitFile || options.includePublicLib) {
    return packages;
  }

  return packages.filter((item) => item.kind !== 'publicLib');
}

function printDryRun(plan) {
  console.log('dry-run：不会执行项目内置反编译器。');
  console.log(`输入路径：${plan.target}`);
  console.log(`输出目录：${plan.outDir}`);
  console.log(`工作目录：${plan.workDir}`);
  console.log(`处理模式：${plan.unpackOnly ? '只解包' : '完整反编译'}`);
  console.log(`像素单位：${plan.usePx ? 'px' : 'rpx'}`);
  console.log(`清空旧产物：${plan.clear ? '是' : '否'}`);
  console.log(`wxid：${plan.wxid || '未指定（加密包将尝试从路径识别）'}`);
  console.log(`worker 权限隔离：${plan.isolation.supported ? '已启用（只读文件/网络/子进程）' : '当前 Node 版本不完整支持'}`);
  console.log('包清单：');

  for (const packagePath of plan.packages) {
    console.log(`  - ${packagePath}`);
  }
}

async function assertSafeOutput({ outDir, packagePaths, clear }) {
  const outputRoot = path.resolve(outDir);
  const outputStats = await fs.lstat(outputRoot).catch(() => null);
  if (outputStats?.isSymbolicLink()) {
    const error = new Error(`输出目录不能是符号链接：${outputRoot}`);
    error.code = 'WXAPKG_UNSAFE_OUTPUT';
    throw error;
  }

  const canonicalOutput = await canonicalizePath(outputRoot);
  const protectedPaths = await Promise.all([
    path.parse(outputRoot).root,
    process.cwd(),
    os.homedir()
  ].map(canonicalizePath));

  if (clear && protectedPaths.some((protectedPath) => isInside(canonicalOutput, protectedPath))) {
    const error = new Error(`拒绝清空受保护目录：${outputRoot}`);
    error.code = 'WXAPKG_UNSAFE_OUTPUT';
    throw error;
  }

  for (const packagePath of packagePaths) {
    const canonicalPackage = await canonicalizePath(packagePath);
    if (clear && isInside(canonicalOutput, canonicalPackage)) {
      const error = new Error(`输出目录包含输入包，--clear 可能删除源文件：${outputRoot}`);
      error.code = 'WXAPKG_UNSAFE_OUTPUT';
      throw error;
    }
  }

  if (outputStats?.isDirectory() && !clear) {
    await assertTreeHasNoSymlinks(outputRoot);
  }
}

async function canonicalizePath(targetPath) {
  let current = path.resolve(targetPath);
  const missingParts = [];

  while (true) {
    try {
      const realPath = await fs.realpath(current);
      return path.resolve(realPath, ...missingParts);
    } catch (error) {
      if (error?.code !== 'ENOENT') {
        const unsafeError = new Error(`无法校验路径的真实位置：${targetPath}`);
        unsafeError.code = 'WXAPKG_UNSAFE_OUTPUT';
        unsafeError.cause = error;
        throw unsafeError;
      }

      const parent = path.dirname(current);
      if (parent === current) {
        return path.resolve(targetPath);
      }

      missingParts.unshift(path.basename(current));
      current = parent;
    }
  }
}

async function assertTreeHasNoSymlinks(root) {
  const entries = await fs.readdir(root, { withFileTypes: true });

  for (const entry of entries) {
    const fullPath = path.join(root, entry.name);

    if (entry.isSymbolicLink()) {
      const error = new Error(`输出目录中存在符号链接，拒绝继续写入：${fullPath}`);
      error.code = 'WXAPKG_UNSAFE_OUTPUT';
      throw error;
    }

    if (entry.isDirectory()) {
      await assertTreeHasNoSymlinks(fullPath);
    }
  }
}

function isInside(parent, child) {
  const relative = path.relative(parent, child);
  return relative === '' || (!relative.startsWith(`..${path.sep}`) && relative !== '..' && !path.isAbsolute(relative));
}

async function openDirectory(directory) {
  const command = process.platform === 'win32'
    ? 'explorer.exe'
    : process.platform === 'darwin'
      ? 'open'
      : 'xdg-open';
  const child = spawn(command, [directory], {
    detached: true,
    stdio: 'ignore'
  });

  await new Promise((resolve, reject) => {
    child.once('spawn', resolve);
    child.once('error', reject);
  });
  child.unref();
}

export function defaultOutputDir(target, identity) {
  const stamp = new Date()
    .toISOString()
    .replace(/[-:]/g, '')
    .replace(/\..+$/, '')
    .replace('T', '-');

  const slugSource = identity?.name || identity?.appid || target;

  return path.resolve(process.cwd(), 'decoded', `${makeOutputSlug(slugSource)}-${stamp}`);
}
