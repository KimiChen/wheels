// SPDX-License-Identifier: GPL-3.0-or-later

import { fork } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { constants as fsConstants } from 'node:fs';
import fs from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const DECODER_DIR = fileURLToPath(new URL('./', import.meta.url));
const PROJECT_ROOT = path.resolve(DECODER_DIR, '../..');
const WORKER_PATH = fileURLToPath(new URL('./worker.js', import.meta.url));
const DEFAULT_TIMEOUT_MS = 15 * 60 * 1000;
const POLYFILL_STAGE_PREFIX = `wxapkg-helper-polyfill-${process.pid}-`;
const ACTIVE_PREPARATIONS = new WeakMap();

export async function runDecoderWorker(config, options = {}) {
  const ownsPreparation = !options.preparation;
  const preparation = options.preparation || await prepareDecoderWorker(config);
  const preparationState = ACTIVE_PREPARATIONS.get(preparation);
  if (!preparationState) {
    const error = new Error('反编译 worker 准备状态无效或已释放。');
    error.code = 'WXAPKG_INVALID_WORKER_PREPARATION';
    throw error;
  }
  if (preparationState.configKey !== workerPreparationConfigKey(config)) {
    const error = new Error('反编译 worker 准备状态与当前输入或输出目录不匹配。');
    error.code = 'WXAPKG_INVALID_WORKER_PREPARATION';
    throw error;
  }
  const workerConfig = {
    ...config,
    customPolyfillRoots: preparationState.customPolyfillRoots
  };

  try {
    return await runWorkerProcess(workerConfig, options, preparationState.isolation);
  } finally {
    if (ownsPreparation) {
      await disposeDecoderWorkerPreparation(preparation);
    }
  }
}

export async function prepareDecoderWorker(config) {
  const isolation = describeWorkerIsolation();
  assertWorkerIsolationSupported(isolation);
  const polyfillSnapshot = await snapshotCustomPolyfills(config.packageInfos);
  const preparation = Object.freeze({});
  ACTIVE_PREPARATIONS.set(preparation, {
    isolation,
    customPolyfillRoots: polyfillSnapshot.mappings,
    stageRoot: polyfillSnapshot.stageRoot,
    configKey: workerPreparationConfigKey(config)
  });

  try {
    assertSafeWorkerReadAllowlist({
      ...config,
      customPolyfillRoots: polyfillSnapshot.mappings
    });
    return preparation;
  } catch (error) {
    await disposeDecoderWorkerPreparation(preparation);
    throw error;
  }
}

export async function disposeDecoderWorkerPreparation(preparation) {
  const preparationState = preparation && ACTIVE_PREPARATIONS.get(preparation);
  if (!preparationState) {
    return;
  }

  ACTIVE_PREPARATIONS.delete(preparation);
  if (preparationState.stageRoot) {
    await fs.rm(preparationState.stageRoot, { recursive: true, force: true });
  }
}

async function runWorkerProcess(config, options, isolation) {
  const execArgv = ['--max-old-space-size=1024'];

  execArgv.push(isolation.permissionFlag);

  for (const readPath of buildReadAllowlist(config)) {
    assertSafeReadPermissionPath(readPath);
    execArgv.push(`--allow-fs-read=${readPath}`);
  }

  const child = fork(WORKER_PATH, [], {
    cwd: path.dirname(config.outputPath),
    env: sanitizedEnvironment(),
    execArgv,
    stdio: ['ignore', 'inherit', 'inherit', 'ipc']
  });

  const timeoutMs = normalizeTimeout(options.timeoutMs);
  const requestId = randomBytes(32).toString('hex');

  return await new Promise((resolve, reject) => {
    let response = null;
    let protocolError = null;
    let settled = false;
    const timer = setTimeout(() => {
      if (settled) {
        return;
      }

      settled = true;
      child.kill('SIGKILL');
      const error = new Error(`反编译超时（${Math.round(timeoutMs / 1000)} 秒）。`);
      error.code = 'WXAPKG_DECODE_TIMEOUT';
      reject(error);
    }, timeoutMs);

    child.on('message', (message) => {
      if (!isWorkerResponse(message, requestId)) {
        return;
      }
      if (response) {
        protocolError = workerProtocolError('反编译 worker 发送了多个最终响应。');
        child.kill('SIGKILL');
        return;
      }
      response = message;
    });

    child.on('error', (error) => {
      if (settled) {
        return;
      }

      settled = true;
      clearTimeout(timer);
      reject(error);
    });

    child.on('exit', (code, signal) => {
      if (settled) {
        return;
      }

      settled = true;
      clearTimeout(timer);

      if (protocolError) {
        reject(protocolError);
        return;
      }

      if (response?.type === 'result' && code === 0) {
        resolve({ ...response.result, isolation });
        return;
      }

      if (response?.type === 'error') {
        reject(deserializeError(response.error));
        return;
      }

      const error = new Error(
        signal
          ? `反编译 worker 被信号 ${signal} 终止。`
          : `反编译 worker 退出，代码 ${code ?? 'unknown'}。`
      );
      error.code = 'WXAPKG_WORKER_FAILED';
      reject(error);
    });

    child.send({ type: 'decode', requestId, config }, (error) => {
      if (!error || settled) {
        return;
      }
      settled = true;
      clearTimeout(timer);
      child.kill('SIGKILL');
      reject(error);
    });
  });
}

export function assertWorkerIsolationSupported(isolation) {
  if (isolation?.supported) {
    return;
  }

  const error = new Error('完整反编译需要支持 --permission 和 --allow-net 权限隔离的 Node.js 25+。');
  error.code = 'WXAPKG_UNSUPPORTED_NODE';
  throw error;
}

export function describeWorkerIsolation() {
  const permissionFlag = getPermissionFlag();
  const networkPermission = Boolean(process.allowedNodeEnvironmentFlags?.has('--allow-net'));

  return {
    permissionModel: Boolean(permissionFlag),
    networkPermission,
    supported: Boolean(permissionFlag && networkPermission),
    permissionFlag,
    memoryLimitMb: 1024,
    workerFsWrite: false
  };
}

function buildReadAllowlist(config) {
  const paths = new Set([
    DECODER_DIR,
    path.join(PROJECT_ROOT, 'src'),
    path.join(PROJECT_ROOT, 'node_modules'),
    path.join(PROJECT_ROOT, 'package.json'),
    config.outputPath
  ]);

  for (const packageInfo of config.packageInfos) {
    const packagePath = packageInfo.inputPath;
    paths.add(packagePath);
  }

  for (const item of config.customPolyfillRoots || []) {
    if (item.root) {
      paths.add(item.root);
    }
  }

  return [...paths];
}

function assertSafeWorkerReadAllowlist(config) {
  for (const readPath of buildReadAllowlist(config)) {
    assertSafeReadPermissionPath(readPath);
  }
}

function workerPreparationConfigKey(config) {
  return JSON.stringify({
    outputPath: path.resolve(config.outputPath),
    packagePaths: config.packageInfos.map((item) => path.resolve(item.inputPath))
  });
}

export async function snapshotCustomPolyfills(packageInfos) {
  const sourceRoots = new Map();
  const mappings = [];
  let stageRoot = null;

  try {
    for (const packageInfo of packageInfos) {
      const sourceRoot = path.resolve(path.dirname(packageInfo.inputPath), 'polyfill');
      let stagedRoot = sourceRoots.get(sourceRoot);

      if (stagedRoot === undefined) {
        const stats = await lstatIfExists(sourceRoot);
        if (!stats) {
          stagedRoot = null;
        } else {
          if (stats.isSymbolicLink() || !stats.isDirectory()) {
            throw unsafePolyfillError(sourceRoot);
          }
          stageRoot ||= await fs.mkdtemp(path.join(os.tmpdir(), POLYFILL_STAGE_PREFIX));
          stagedRoot = path.join(stageRoot, String(sourceRoots.size));
          await fs.mkdir(stagedRoot, { recursive: true, mode: 0o700 });
          await copySafePolyfillTree(sourceRoot, stagedRoot, await fs.realpath(sourceRoot));
        }
        sourceRoots.set(sourceRoot, stagedRoot);
      }

      mappings.push({ inputPath: packageInfo.inputPath, root: stagedRoot });
    }

    return { stageRoot, mappings };
  } catch (error) {
    if (stageRoot) {
      await fs.rm(stageRoot, { recursive: true, force: true });
    }
    throw error;
  }
}

export async function findSafeCustomPolyfillRoots(packageInfos) {
  const roots = new Set();

  for (const packageInfo of packageInfos) {
    const candidate = path.resolve(path.dirname(packageInfo.inputPath), 'polyfill');
    if (roots.has(candidate)) {
      continue;
    }

    const stats = await lstatIfExists(candidate);
    if (!stats) {
      continue;
    }
    if (stats.isSymbolicLink() || !stats.isDirectory()) {
      throw unsafePolyfillError(candidate);
    }

    const realRoot = await fs.realpath(candidate);
    await assertSafePolyfillTree(candidate, realRoot);
    roots.add(candidate);
  }

  return [...roots];
}

export function assertSafeReadPermissionPath(readPath) {
  if (typeof readPath !== 'string' || readPath.length === 0 || readPath.includes('\0') || readPath.includes('*')) {
    const error = new Error(`文件读取授权路径包含 Node 权限模型元字符：${readPath}`);
    error.code = 'WXAPKG_UNSAFE_PERMISSION_PATH';
    throw error;
  }
}

async function assertSafePolyfillTree(directory, realRoot) {
  const realDirectory = await fs.realpath(directory);
  if (!isInside(realRoot, realDirectory)) {
    throw unsafePolyfillError(directory);
  }

  const entries = await fs.readdir(directory, { withFileTypes: true });
  for (const entry of entries) {
    const target = path.join(directory, entry.name);
    const stats = await fs.lstat(target);
    if (stats.isSymbolicLink()) {
      throw unsafePolyfillError(target);
    }

    const realTarget = await fs.realpath(target);
    if (!isInside(realRoot, realTarget)) {
      throw unsafePolyfillError(target);
    }

    if (stats.isDirectory()) {
      await assertSafePolyfillTree(target, realRoot);
    }
    else if (!stats.isFile()) {
      throw unsafePolyfillError(target);
    }
  }
}

async function copySafePolyfillTree(sourceDirectory, targetDirectory, realRoot) {
  const realDirectory = await fs.realpath(sourceDirectory);
  if (!isInside(realRoot, realDirectory)) {
    throw unsafePolyfillError(sourceDirectory);
  }

  const entries = await fs.readdir(sourceDirectory, { withFileTypes: true });
  for (const entry of entries) {
    const source = path.join(sourceDirectory, entry.name);
    const stats = await fs.lstat(source);
    if (stats.isSymbolicLink()) {
      throw unsafePolyfillError(source);
    }

    const realSource = await fs.realpath(source);
    if (!isInside(realRoot, realSource)) {
      throw unsafePolyfillError(source);
    }

    const target = path.join(targetDirectory, entry.name);
    if (stats.isDirectory()) {
      await fs.mkdir(target, { mode: 0o700 });
      await copySafePolyfillTree(source, target, realRoot);
      continue;
    }
    if (!stats.isFile()) {
      throw unsafePolyfillError(source);
    }
    if (!entry.name.endsWith('.js')) {
      continue;
    }

    const flags = fsConstants.O_RDONLY | (fsConstants.O_NOFOLLOW || 0);
    const handle = await fs.open(source, flags);
    try {
      const openedStats = await handle.stat();
      const currentStats = await fs.lstat(source);
      if (
        !openedStats.isFile()
        || openedStats.dev !== stats.dev
        || openedStats.ino !== stats.ino
        || openedStats.dev !== currentStats.dev
        || openedStats.ino !== currentStats.ino
      ) {
        throw unsafePolyfillError(source);
      }
      await fs.writeFile(target, await handle.readFile(), { flag: 'wx', mode: 0o400 });
    } finally {
      await handle.close();
    }
  }
}

async function lstatIfExists(target) {
  try {
    return await fs.lstat(target);
  }
  catch (error) {
    if (error?.code === 'ENOENT') {
      return null;
    }
    throw error;
  }
}

function unsafePolyfillError(target) {
  const error = new Error(`自定义 polyfill 目录包含不安全的路径：${target}`);
  error.code = 'WXAPKG_UNSAFE_POLYFILL';
  return error;
}

function isInside(root, target) {
  const relative = path.relative(root, target);
  return relative === ''
    || (relative !== '..' && !relative.startsWith(`..${path.sep}`) && !path.isAbsolute(relative));
}

function getPermissionFlag() {
  if (process.allowedNodeEnvironmentFlags?.has('--permission')) {
    return '--permission';
  }

  if (process.allowedNodeEnvironmentFlags?.has('--experimental-permission')) {
    return '--experimental-permission';
  }

  return null;
}

function sanitizedEnvironment() {
  const env = {
    LANG: process.env.LANG || 'C.UTF-8',
    LC_ALL: process.env.LC_ALL || '',
    NO_COLOR: '1',
    NODE_NO_WARNINGS: '1',
    WXAPKG_VM_TIMEOUT_MS: '15000'
  };

  for (const key of ['SystemRoot', 'WINDIR', 'TEMP', 'TMP', 'TMPDIR']) {
    if (process.env[key]) {
      env[key] = process.env[key];
    }
  }

  return env;
}

function normalizeTimeout(value) {
  const timeout = Number(value ?? DEFAULT_TIMEOUT_MS);

  if (!Number.isFinite(timeout) || timeout <= 0) {
    return DEFAULT_TIMEOUT_MS;
  }

  return Math.floor(timeout);
}

function deserializeError(serialized = {}) {
  const error = new Error(serialized.message || '反编译失败。');
  error.name = serialized.name || 'Error';
  error.code = serialized.code;

  if (serialized.stack) {
    error.stack = serialized.stack;
  }

  return error;
}

function isWorkerResponse(message, requestId) {
  if (!isRecord(message) || message.requestId !== requestId) {
    return false;
  }

  if (message.type === 'result') {
    return hasOnlyKeys(message, ['type', 'requestId', 'result'])
      && isRecord(message.result)
      && hasOnlyKeys(message.result, ['manifest'])
      && Array.isArray(message.result.manifest);
  }

  if (message.type === 'error') {
    return hasOnlyKeys(message, ['type', 'requestId', 'error'])
      && isRecord(message.error)
      && hasOnlyKeys(message.error, ['name', 'message', 'code', 'stack'], ['message']);
  }

  return false;
}

function hasOnlyKeys(value, allowedKeys, requiredKeys = allowedKeys) {
  const keys = Object.keys(value);
  return requiredKeys.every((key) => keys.includes(key))
    && keys.every((key) => allowedKeys.includes(key));
}

function isRecord(value) {
  return value !== null && typeof value === 'object' && !Array.isArray(value);
}

function workerProtocolError(message) {
  const error = new Error(message);
  error.code = 'WXAPKG_WORKER_PROTOCOL';
  return error;
}
