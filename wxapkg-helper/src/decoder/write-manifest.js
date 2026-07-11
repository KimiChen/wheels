// SPDX-License-Identifier: GPL-3.0-or-later

import { constants as fsConstants } from 'node:fs';
import fs from 'node:fs/promises';
import path from 'node:path';

export const MAX_MANIFEST_OPERATIONS = 100_000;
export const MAX_MANIFEST_WRITE_BYTES = 512 * 1024 * 1024;
export const MAX_MANIFEST_SINGLE_WRITE_BYTES = 128 * 1024 * 1024;
export const MAX_MANIFEST_PATH_BYTES = 4096;

const BASE64_PATTERN = /^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/;
const WRITE_FIELDS = new Set(['op', 'path', 'data', 'size']);
const DELETE_FIELDS = new Set(['op', 'path']);

export class WriteManifestError extends Error {
  constructor(message, options = {}) {
    super(message, options.cause === undefined ? undefined : { cause: options.cause });
    this.name = 'WriteManifestError';
    this.code = options.code || 'WXAPKG_MANIFEST_INVALID';

    if (options.details !== undefined) {
      this.details = options.details;
    }
  }
}

export function validateWriteManifest(manifest) {
  const entries = readDenseManifestArray(manifest);
  if (entries.length > MAX_MANIFEST_OPERATIONS) {
    throw manifestError(
      'WXAPKG_MANIFEST_LIMIT',
      `worker manifest exceeds the ${MAX_MANIFEST_OPERATIONS} operation limit`,
      { actual: entries.length, maximum: MAX_MANIFEST_OPERATIONS }
    );
  }

  const operations = [];
  const targets = new Set();
  let totalWriteBytes = 0;

  for (let index = 0; index < entries.length; index += 1) {
    const fields = readOperationFields(entries[index], index);
    const operationPath = validateManifestPath(fields.path, index);
    const targetKey = operationPath.toLowerCase();

    if (targets.has(targetKey)) {
      throw manifestError(
        'WXAPKG_MANIFEST_CONFLICT',
        `worker manifest contains a duplicate target: ${operationPath}`,
        { index, path: operationPath }
      );
    }
    targets.add(targetKey);

    if (fields.op === 'delete') {
      operations.push(Object.freeze({ op: 'delete', path: operationPath }));
      continue;
    }

    const data = decodeWriteData(fields, index, operationPath, totalWriteBytes);
    totalWriteBytes += data.length;
    if (totalWriteBytes > MAX_MANIFEST_WRITE_BYTES) {
      throw manifestError(
        'WXAPKG_MANIFEST_LIMIT',
        `worker manifest exceeds the ${MAX_MANIFEST_WRITE_BYTES} byte write limit`,
        { actual: totalWriteBytes, maximum: MAX_MANIFEST_WRITE_BYTES }
      );
    }

    operations.push(Object.freeze({
      op: 'write',
      path: operationPath,
      data,
      size: data.length
    }));
  }

  assertNoTargetHierarchyConflicts(operations);
  return Object.freeze(operations);
}

export async function applyWriteManifest(outputRoot, manifest) {
  const operations = validateWriteManifest(manifest);
  const root = await inspectOutputRoot(outputRoot);

  // Complete both schema and filesystem validation before the first mutation.
  for (const operation of operations) {
    await preflightOperation(root, operation);
  }

  let written = 0;
  let deleted = 0;
  let bytesWritten = 0;

  for (const operation of operations) {
    try {
      if (operation.op === 'write') {
        await applyWrite(root, operation);
        written += 1;
        bytesWritten += operation.size;
      } else if (await applyDelete(root, operation)) {
        deleted += 1;
      }
    } catch (cause) {
      if (cause instanceof WriteManifestError) {
        throw cause;
      }
      throw manifestError(
        'WXAPKG_MANIFEST_IO',
        `unable to apply worker manifest operation: ${operation.path}`,
        { op: operation.op, path: operation.path },
        cause
      );
    }
  }

  return {
    operationCount: operations.length,
    written,
    deleted,
    bytesWritten
  };
}

function readDenseManifestArray(manifest) {
  if (!Array.isArray(manifest) || Object.getPrototypeOf(manifest) !== Array.prototype) {
    throw manifestError('WXAPKG_MANIFEST_INVALID', 'worker manifest must be a plain array');
  }

  const lengthDescriptor = Object.getOwnPropertyDescriptor(manifest, 'length');
  const length = lengthDescriptor?.value;

  if (!Number.isSafeInteger(length) || length < 0) {
    throw manifestError('WXAPKG_MANIFEST_INVALID', 'worker manifest has an invalid array length');
  }

  if (length > MAX_MANIFEST_OPERATIONS) {
    return { length };
  }

  const descriptors = Object.getOwnPropertyDescriptors(manifest);
  const ownKeys = Reflect.ownKeys(descriptors);
  if (ownKeys.some((key) => typeof key !== 'string') || ownKeys.length !== length + 1) {
    throw manifestError('WXAPKG_MANIFEST_INVALID', 'worker manifest array contains injected fields');
  }

  const entries = new Array(length);
  for (let index = 0; index < length; index += 1) {
    const descriptor = descriptors[String(index)];
    if (!descriptor || !Object.hasOwn(descriptor, 'value')) {
      throw manifestError(
        'WXAPKG_MANIFEST_INVALID',
        `worker manifest entry ${index} is missing or uses an accessor`,
        { index }
      );
    }
    entries[index] = descriptor.value;
  }

  return entries;
}

function readOperationFields(operation, index) {
  if (
    operation === null
    || typeof operation !== 'object'
    || Array.isArray(operation)
    || ![Object.prototype, null].includes(Object.getPrototypeOf(operation))
  ) {
    throw manifestError(
      'WXAPKG_MANIFEST_INVALID',
      `worker manifest operation ${index} must be a plain object`,
      { index }
    );
  }

  const descriptors = Object.getOwnPropertyDescriptors(operation);
  const keys = Reflect.ownKeys(descriptors);
  if (keys.some((key) => typeof key !== 'string')) {
    throw manifestError(
      'WXAPKG_MANIFEST_INVALID',
      `worker manifest operation ${index} contains a symbol field`,
      { index }
    );
  }

  const opDescriptor = descriptors.op;
  if (!opDescriptor || !Object.hasOwn(opDescriptor, 'value')) {
    throw manifestError(
      'WXAPKG_MANIFEST_INVALID',
      `worker manifest operation ${index} has no data op field`,
      { index }
    );
  }

  const op = opDescriptor.value;
  const expectedFields = op === 'write'
    ? WRITE_FIELDS
    : op === 'delete'
      ? DELETE_FIELDS
      : null;

  if (!expectedFields) {
    throw manifestError(
      'WXAPKG_MANIFEST_INVALID',
      `worker manifest operation ${index} has an unsupported op`,
      { index }
    );
  }

  if (keys.length !== expectedFields.size || keys.some((key) => !expectedFields.has(key))) {
    throw manifestError(
      'WXAPKG_MANIFEST_INVALID',
      `worker manifest operation ${index} has unexpected or missing fields`,
      { index, fields: keys }
    );
  }

  const values = { op };
  for (const field of expectedFields) {
    const descriptor = descriptors[field];
    if (!descriptor || !Object.hasOwn(descriptor, 'value')) {
      throw manifestError(
        'WXAPKG_MANIFEST_INVALID',
        `worker manifest operation ${index} field ${field} must not be an accessor`,
        { index, field }
      );
    }
    values[field] = descriptor.value;
  }

  return values;
}

function validateManifestPath(value, index) {
  if (typeof value !== 'string' || value.length === 0) {
    throw invalidPath('manifest path must be a non-empty string', value, index);
  }

  if (value.includes('\0') || value.includes('\\')) {
    throw invalidPath('manifest path must not contain NUL or backslash characters', value, index);
  }

  if (
    path.posix.isAbsolute(value)
    || /^[A-Za-z]:/.test(value)
    || path.posix.normalize(value) !== value
  ) {
    throw invalidPath('manifest path must be a canonical relative POSIX path', value, index);
  }

  const segments = value.split('/');
  if (segments.some((segment) => segment === '' || segment === '.' || segment === '..')) {
    throw invalidPath('manifest path contains an unsafe path segment', value, index);
  }

  if (Buffer.byteLength(value, 'utf8') > MAX_MANIFEST_PATH_BYTES) {
    throw invalidPath(`manifest path exceeds ${MAX_MANIFEST_PATH_BYTES} UTF-8 bytes`, value, index);
  }

  if (segments.some(isUnsafePortableSegment)) {
    throw invalidPath('manifest path is not safe on supported filesystems', value, index);
  }

  return value;
}

function isUnsafePortableSegment(segment) {
  return segment.includes(':')
    || /[ .]$/.test(segment)
    || /^(?:con|prn|aux|nul|com[1-9]|lpt[1-9])(?:\..*)?$/i.test(segment);
}

function decodeWriteData(fields, index, operationPath, currentTotal) {
  if (!Number.isSafeInteger(fields.size) || fields.size < 0) {
    throw manifestError(
      'WXAPKG_MANIFEST_INVALID',
      `worker manifest write ${index} has an invalid size`,
      { index, path: operationPath }
    );
  }

  if (fields.size > MAX_MANIFEST_SINGLE_WRITE_BYTES) {
    throw manifestError(
      'WXAPKG_MANIFEST_LIMIT',
      `worker manifest write exceeds the ${MAX_MANIFEST_SINGLE_WRITE_BYTES} byte per-file limit`,
      { index, path: operationPath, actual: fields.size, maximum: MAX_MANIFEST_SINGLE_WRITE_BYTES }
    );
  }

  if (currentTotal + fields.size > MAX_MANIFEST_WRITE_BYTES) {
    throw manifestError(
      'WXAPKG_MANIFEST_LIMIT',
      `worker manifest exceeds the ${MAX_MANIFEST_WRITE_BYTES} byte write limit`,
      {
        actual: currentTotal + fields.size,
        maximum: MAX_MANIFEST_WRITE_BYTES
      }
    );
  }

  if (typeof fields.data !== 'string') {
    throw manifestError(
      'WXAPKG_MANIFEST_INVALID',
      `worker manifest write ${index} data must be base64 text`,
      { index, path: operationPath }
    );
  }

  const expectedEncodedLength = Math.ceil(fields.size / 3) * 4;
  if (fields.data.length !== expectedEncodedLength || !BASE64_PATTERN.test(fields.data)) {
    throw manifestError(
      'WXAPKG_MANIFEST_INVALID',
      `worker manifest write ${index} contains invalid base64 data`,
      { index, path: operationPath }
    );
  }

  const data = Buffer.from(fields.data, 'base64');
  if (data.length !== fields.size || data.toString('base64') !== fields.data) {
    throw manifestError(
      'WXAPKG_MANIFEST_INVALID',
      `worker manifest write ${index} base64 data does not match its declared size`,
      { index, path: operationPath, actual: data.length, declared: fields.size }
    );
  }

  return data;
}

function assertNoTargetHierarchyConflicts(operations) {
  const root = { children: new Map(), operation: null };

  for (const operation of operations) {
    let node = root;
    for (const segment of operation.path.toLowerCase().split('/')) {
      if (node.operation) {
        throw hierarchyConflict(node.operation, operation);
      }
      if (!node.children.has(segment)) {
        node.children.set(segment, { children: new Map(), operation: null });
      }
      node = node.children.get(segment);
    }

    if (node.children.size > 0) {
      const descendant = findDescendantOperation(node);
      throw hierarchyConflict(operation, descendant);
    }
    node.operation = operation;
  }
}

function findDescendantOperation(node) {
  const pending = [node];
  while (pending.length > 0) {
    const current = pending.pop();
    if (current.operation) {
      return current.operation;
    }
    pending.push(...current.children.values());
  }
  return null;
}

function hierarchyConflict(ancestor, descendant) {
  return manifestError(
    'WXAPKG_MANIFEST_CONFLICT',
    `worker manifest targets conflict hierarchically: ${ancestor.path} and ${descendant.path}`,
    { ancestor: ancestor.path, descendant: descendant.path }
  );
}

async function inspectOutputRoot(outputRoot) {
  if (typeof outputRoot !== 'string' || outputRoot.length === 0) {
    throw manifestError('WXAPKG_MANIFEST_UNSAFE_OUTPUT', 'manifest output root must be a path string');
  }

  const resolvedPath = path.resolve(outputRoot);
  let stats;
  try {
    stats = await fs.lstat(resolvedPath);
  } catch (cause) {
    throw manifestError(
      'WXAPKG_MANIFEST_UNSAFE_OUTPUT',
      `manifest output root is not accessible: ${resolvedPath}`,
      { outputRoot: resolvedPath },
      cause
    );
  }

  if (stats.isSymbolicLink() || !stats.isDirectory()) {
    throw manifestError(
      'WXAPKG_MANIFEST_UNSAFE_OUTPUT',
      `manifest output root must be a real directory: ${resolvedPath}`,
      { outputRoot: resolvedPath }
    );
  }

  return {
    resolvedPath,
    realPath: await fs.realpath(resolvedPath),
    dev: stats.dev,
    ino: stats.ino
  };
}

async function preflightOperation(root, operation) {
  const parent = await inspectParentChain(root, operation.path, false);
  if (!parent.exists) {
    return;
  }

  const destination = resolveDestination(root, operation.path);
  const stats = await lstatIfExists(destination);
  if (!stats) {
    return;
  }

  if (operation.op === 'write') {
    assertSafeWriteTarget(stats, operation.path);
  } else {
    await assertSafeDeleteTarget(destination, stats, operation.path);
  }
}

async function applyWrite(root, operation) {
  await inspectParentChain(root, operation.path, true);
  const destination = resolveDestination(root, operation.path);
  const existing = await lstatIfExists(destination);
  if (existing) {
    assertSafeWriteTarget(existing, operation.path);
  }

  const flags = fsConstants.O_WRONLY
    | fsConstants.O_CREAT
    | (fsConstants.O_NOFOLLOW || 0);
  const handle = await fs.open(destination, flags, 0o666);
  try {
    const [openedStats, pathStats] = await Promise.all([
      handle.stat(),
      fs.lstat(destination)
    ]);
    assertSafeWriteTarget(openedStats, operation.path);
    if (openedStats.dev !== pathStats.dev || openedStats.ino !== pathStats.ino) {
      throw manifestError(
        'WXAPKG_MANIFEST_UNSAFE_OUTPUT',
        `manifest write target changed while it was being opened: ${operation.path}`,
        { path: operation.path }
      );
    }

    await handle.truncate(0);
    await handle.writeFile(operation.data);
  } finally {
    await handle.close();
  }
}

async function applyDelete(root, operation) {
  const parent = await inspectParentChain(root, operation.path, false);
  if (!parent.exists) {
    return false;
  }

  const destination = resolveDestination(root, operation.path);
  const stats = await lstatIfExists(destination);
  if (!stats) {
    return false;
  }

  await assertSafeDeleteTarget(destination, stats, operation.path);
  if (stats.isDirectory()) {
    await fs.rmdir(destination);
  } else {
    await fs.unlink(destination);
  }
  return true;
}

async function inspectParentChain(root, operationPath, createMissing) {
  await assertRootUnchanged(root);
  const parts = operationPath.split('/').slice(0, -1);
  let current = root.resolvedPath;

  for (const part of parts) {
    current = path.join(current, part);
    let stats = await lstatIfExists(current);

    if (!stats && createMissing) {
      try {
        await fs.mkdir(current);
      } catch (cause) {
        if (cause?.code !== 'EEXIST') {
          throw cause;
        }
      }
      stats = await fs.lstat(current);
    }

    if (!stats) {
      return { exists: false };
    }

    if (stats.isSymbolicLink() || !stats.isDirectory()) {
      throw manifestError(
        'WXAPKG_MANIFEST_UNSAFE_OUTPUT',
        `manifest path contains a non-directory or symlink component: ${operationPath}`,
        { path: operationPath, component: current }
      );
    }
  }

  const realParent = await fs.realpath(current);
  if (!isInside(root.realPath, realParent)) {
    throw manifestError(
      'WXAPKG_MANIFEST_UNSAFE_OUTPUT',
      `manifest parent resolves outside the output root: ${operationPath}`,
      { path: operationPath }
    );
  }

  return { exists: true, path: current };
}

async function assertRootUnchanged(root) {
  const stats = await fs.lstat(root.resolvedPath);
  if (
    stats.isSymbolicLink()
    || !stats.isDirectory()
    || stats.dev !== root.dev
    || stats.ino !== root.ino
    || await fs.realpath(root.resolvedPath) !== root.realPath
  ) {
    throw manifestError(
      'WXAPKG_MANIFEST_UNSAFE_OUTPUT',
      `manifest output root changed during application: ${root.resolvedPath}`,
      { outputRoot: root.resolvedPath }
    );
  }
}

function resolveDestination(root, operationPath) {
  const destination = path.resolve(root.resolvedPath, ...operationPath.split('/'));
  if (!isInside(root.resolvedPath, destination) || destination === root.resolvedPath) {
    throw manifestError(
      'WXAPKG_MANIFEST_PATH',
      `manifest path resolves outside the output root: ${operationPath}`,
      { path: operationPath }
    );
  }
  return destination;
}

function assertSafeWriteTarget(stats, operationPath) {
  if (stats.isSymbolicLink() || !stats.isFile() || stats.nlink > 1) {
    throw manifestError(
      'WXAPKG_MANIFEST_UNSAFE_OUTPUT',
      `manifest write target is not a safe regular file: ${operationPath}`,
      { path: operationPath }
    );
  }
}

async function assertSafeDeleteTarget(destination, stats, operationPath) {
  if (stats.isSymbolicLink() || (!stats.isFile() && !stats.isDirectory())) {
    throw manifestError(
      'WXAPKG_MANIFEST_UNSAFE_OUTPUT',
      `manifest delete target is not a safe file or directory: ${operationPath}`,
      { path: operationPath }
    );
  }

  if (stats.isDirectory() && (await fs.readdir(destination)).length > 0) {
    throw manifestError(
      'WXAPKG_MANIFEST_UNSAFE_OUTPUT',
      `manifest may only delete empty real directories: ${operationPath}`,
      { path: operationPath }
    );
  }
}

async function lstatIfExists(targetPath) {
  try {
    return await fs.lstat(targetPath);
  } catch (cause) {
    if (cause?.code === 'ENOENT') {
      return null;
    }
    throw cause;
  }
}

function isInside(root, candidate) {
  const relative = path.relative(root, candidate);
  return relative === ''
    || (relative !== '..' && !relative.startsWith(`..${path.sep}`) && !path.isAbsolute(relative));
}

function invalidPath(message, value, index) {
  return manifestError(
    'WXAPKG_MANIFEST_PATH',
    `${message} at operation ${index}`,
    { index, path: typeof value === 'string' ? value : undefined }
  );
}

function manifestError(code, message, details, cause) {
  return new WriteManifestError(message, { code, details, cause });
}
