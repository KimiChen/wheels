// SPDX-License-Identifier: GPL-3.0-or-later

import { constants as fsConstants } from 'node:fs';
import crypto from 'node:crypto';
import fs from 'node:fs/promises';
import path from 'node:path';
import { decryptWxapkg, extractWxid, needsDecryption, validateWxid } from './decrypt.js';
import { WxapkgDecryptionError, WxapkgIoError, WxapkgPathError } from './errors.js';
import { normalizeWxapkgPath, parseWxapkg } from './format.js';

export async function unpackWxapkg(inputPath, outputPath, options = {}) {
  const normalizedOptions = typeof options === 'string' ? { wxid: options } : options || {};
  const resolvedOutputPath = path.resolve(outputPath);
  const inspected = await inspectWxapkg(inputPath, normalizedOptions);

  if (
    normalizedOptions.expectedContentSha256
    && inspected.contentSha256 !== normalizedOptions.expectedContentSha256
  ) {
    const error = new WxapkgIoError(`wxapkg input changed after inspection: ${inspected.inputPath}`, {
      code: 'WXAPKG_INPUT_CHANGED',
      details: { inputPath: inspected.inputPath }
    });
    throw error;
  }

  const outputFiles = inspected.files.map((file) => ({
    file,
    outputEntryPath: mapOutputEntryPath(file.path, normalizedOptions.mapEntryPath)
  }));
  const outputPaths = new Set();

  for (const { outputEntryPath } of outputFiles) {
    const outputPathKey = outputEntryPath.toLowerCase();
    if (outputPaths.has(outputPathKey)) {
      throw new WxapkgPathError('wxapkg entries map to the same output path', {
        code: 'ERR_WXAPKG_DUPLICATE_OUTPUT_PATH',
        details: { path: outputEntryPath }
      });
    }
    outputPaths.add(outputPathKey);
  }

  await ensureOutputRoot(resolvedOutputPath);
  const writtenFiles = [];
  const skippedFiles = [];

  for (const { file, outputEntryPath } of outputFiles) {
    const destination = resolveOutputPath(resolvedOutputPath, outputEntryPath);
    await ensureSafeParent(resolvedOutputPath, path.dirname(destination));
    const written = await writeFileWithoutFollowingSymlinks(destination, file.data, {
      overwrite: Boolean(normalizedOptions.overwrite)
    });
    (written ? writtenFiles : skippedFiles).push(file);
  }

  return {
    ...inspected,
    outputPath: resolvedOutputPath,
    writtenFiles,
    skippedFiles
  };
}

export async function inspectWxapkg(inputPath, options = {}) {
  const normalizedOptions = typeof options === 'string' ? { wxid: options } : options || {};
  const resolvedInputPath = path.resolve(inputPath);

  let packageData;
  try {
    packageData = await fs.readFile(resolvedInputPath);
  } catch (cause) {
    throw new WxapkgIoError(`unable to read wxapkg input: ${resolvedInputPath}`, {
      code: 'ERR_WXAPKG_INPUT_READ',
      cause,
      details: { inputPath: resolvedInputPath }
    });
  }

  const encrypted = needsDecryption(packageData);
  let wxid = null;
  if (encrypted) {
    wxid = normalizedOptions.wxid || extractWxid(resolvedInputPath);
    if (!wxid) {
      throw new WxapkgDecryptionError('encrypted wxapkg requires --wxid or a wxid path segment', {
        code: 'ERR_WXAPKG_WXID_REQUIRED',
        details: { inputPath: resolvedInputPath }
      });
    }
    wxid = validateWxid(wxid);
    packageData = decryptWxapkg(wxid, packageData);
  }

  const parsed = parseWxapkg(packageData);
  const contentSha256 = crypto.createHash('sha256').update(packageData).digest('hex');
  const subPackRootPath = findCommonDirectory(parsed.files.map((file) => file.path));
  const appType = parsed.files.some((file) => file.path === 'game.js') ? 'game' : 'app';
  const packType = inferPackType(parsed.files, subPackRootPath);

  return {
    inputPath: resolvedInputPath,
    encrypted,
    wxid,
    contentSha256,
    header: parsed.header,
    files: parsed.files,
    fileList: parsed.files,
    subPackRootPath,
    appType,
    packType
  };
}

function mapOutputEntryPath(entryPath, mapper) {
  if (mapper === undefined) {
    return entryPath;
  }
  if (typeof mapper !== 'function') {
    throw new TypeError('mapEntryPath must be a function');
  }
  return normalizeWxapkgPath(mapper(entryPath));
}

export function resolveOutputPath(outputPath, entryPath) {
  const outputRoot = path.resolve(outputPath);
  const destination = path.resolve(outputRoot, ...entryPath.split('/'));
  const relative = path.relative(outputRoot, destination);

  if (!relative || relative.startsWith(`..${path.sep}`) || relative === '..' || path.isAbsolute(relative)) {
    throw new WxapkgPathError('wxapkg entry resolves outside the output directory', {
      code: 'ERR_WXAPKG_OUTPUT_ESCAPE',
      details: { outputPath: outputRoot, entryPath }
    });
  }

  return destination;
}

function findCommonDirectory(filePaths) {
  if (filePaths.length === 0) {
    return '';
  }

  const directoryParts = filePaths.map((filePath) => filePath.split('/').slice(0, -1));
  const common = [];
  const shortestLength = Math.min(...directoryParts.map((parts) => parts.length));

  for (let index = 0; index < shortestLength; index += 1) {
    const part = directoryParts[0][index];
    if (!directoryParts.every((parts) => parts[index] === part)) {
      break;
    }
    common.push(part);
  }

  return common.join('/');
}

function inferPackType(files, subPackRootPath) {
  const configPath = subPackRootPath
    ? `${subPackRootPath}/app-config.json`
    : 'app-config.json';
  const configFile = files.find((file) => file.path === configPath);

  if (!configFile) {
    return 'sub';
  }

  try {
    const config = JSON.parse(configFile.data.toString('utf8'));
    if (!subPackRootPath) {
      return 'main';
    }
    const subPackages = config.subPackages || config.subpackages || [];
    const current = subPackages.find((item) => normalizePackageRoot(item?.root) === subPackRootPath);
    if (!current) {
      return 'main';
    }
    return current.independent ? 'independent' : 'sub';
  } catch {
    return 'main';
  }
}

function normalizePackageRoot(root) {
  return String(root || '')
    .replaceAll('\\', '/')
    .replace(/^\/+|\/+$/g, '');
}

async function ensureOutputRoot(outputPath) {
  try {
    await fs.mkdir(outputPath, { recursive: true });
    const stats = await fs.lstat(outputPath);
    if (stats.isSymbolicLink() || !stats.isDirectory()) {
      throw new WxapkgPathError('wxapkg output path must be a real directory', {
        code: 'ERR_WXAPKG_OUTPUT_SYMLINK',
        details: { outputPath }
      });
    }
  } catch (cause) {
    if (cause instanceof WxapkgPathError) {
      throw cause;
    }
    throw new WxapkgIoError(`unable to create wxapkg output directory: ${outputPath}`, {
      code: 'ERR_WXAPKG_OUTPUT_CREATE',
      cause,
      details: { outputPath }
    });
  }
}

async function ensureSafeParent(outputRoot, parentPath) {
  const relative = path.relative(outputRoot, parentPath);
  const parts = relative ? relative.split(path.sep) : [];
  let current = outputRoot;

  for (const part of parts) {
    current = path.join(current, part);
    let stats;
    try {
      stats = await fs.lstat(current);
    } catch (cause) {
      if (cause.code !== 'ENOENT') {
        throw cause;
      }
      await fs.mkdir(current);
      stats = await fs.lstat(current);
    }

    if (stats.isSymbolicLink() || !stats.isDirectory()) {
      throw new WxapkgPathError('wxapkg output path contains an unsafe directory component', {
        code: 'ERR_WXAPKG_OUTPUT_SYMLINK',
        details: { path: current }
      });
    }
  }

  const [realRoot, realParent] = await Promise.all([fs.realpath(outputRoot), fs.realpath(parentPath)]);
  const realRelative = path.relative(realRoot, realParent);
  if (realRelative === '..' || realRelative.startsWith(`..${path.sep}`) || path.isAbsolute(realRelative)) {
    throw new WxapkgPathError('wxapkg output parent resolves outside the output directory', {
      code: 'ERR_WXAPKG_OUTPUT_ESCAPE',
      details: { outputRoot, parentPath }
    });
  }
}

async function writeFileWithoutFollowingSymlinks(destination, data, options) {
  try {
    const existing = await fs.lstat(destination).catch((cause) => {
      if (cause.code === 'ENOENT') {
        return null;
      }
      throw cause;
    });

    if (existing?.isSymbolicLink() || existing?.isDirectory()) {
      throw new WxapkgPathError('wxapkg output file path is a symlink or directory', {
        code: 'ERR_WXAPKG_OUTPUT_SYMLINK',
        details: { destination }
      });
    }

    if (existing && existing.size > 0 && !options.overwrite) {
      return false;
    }

    const flags = fsConstants.O_WRONLY
      | fsConstants.O_CREAT
      | fsConstants.O_TRUNC
      | (fsConstants.O_NOFOLLOW || 0);
    const handle = await fs.open(destination, flags, 0o666);
    try {
      await handle.writeFile(data);
    } finally {
      await handle.close();
    }
    return true;
  } catch (cause) {
    if (cause instanceof WxapkgPathError) {
      throw cause;
    }
    throw new WxapkgIoError(`unable to write wxapkg output file: ${destination}`, {
      code: 'ERR_WXAPKG_OUTPUT_WRITE',
      cause,
      details: { destination }
    });
  }
}
