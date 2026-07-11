// SPDX-License-Identifier: GPL-3.0-or-later

import path from 'node:path';
import { TextDecoder } from 'node:util';
import { WxapkgFormatError, WxapkgPathError } from './errors.js';

export const WXAPKG_HEADER_SIZE = 14;
export const WXAPKG_INDEX_COUNT_SIZE = 4;
export const WXAPKG_FIRST_MARK = 0xbe;
export const WXAPKG_LAST_MARK = 0xed;

const MIN_ENTRY_SIZE = 12;
const WINDOWS_RESERVED_DEVICE_NAME = /^(?:con|prn|aux|nul|com[1-9]|lpt[1-9])(?:\.|$)/i;
const utf8Decoder = new TextDecoder('utf-8', { fatal: true });

export function parseWxapkg(input) {
  const buffer = toBuffer(input);
  const header = parseHeader(buffer);
  const files = parseIndex(buffer, header);
  validateFileRanges(files, header);

  for (const file of files) {
    file.data = buffer.subarray(file.offset, file.offset + file.size);
  }

  return {
    buffer,
    header,
    files,
    fileList: files
  };
}

export function normalizeWxapkgPath(rawPath) {
  if (typeof rawPath !== 'string' || rawPath.length === 0) {
    throw new WxapkgPathError('wxapkg entry path must not be empty');
  }

  if (rawPath.includes('\0')) {
    throw new WxapkgPathError('wxapkg entry path contains a NUL byte', {
      details: { path: rawPath }
    });
  }

  if (rawPath.startsWith('//') || rawPath.startsWith('\\\\') || /^[a-zA-Z]:[\\/]/.test(rawPath)) {
    throw new WxapkgPathError('wxapkg entry path must not be an absolute or UNC path', {
      details: { path: rawPath }
    });
  }

  let candidate = rawPath.startsWith('/') ? rawPath.slice(1) : rawPath;
  candidate = candidate.replaceAll('\\', '/');

  if (!candidate || path.posix.isAbsolute(candidate) || path.win32.isAbsolute(candidate)) {
    throw new WxapkgPathError('wxapkg entry path must resolve relative to the package root', {
      details: { path: rawPath }
    });
  }

  const segments = candidate.split('/');
  if (segments.some((segment) => !segment || segment === '.' || segment === '..')) {
    throw new WxapkgPathError('wxapkg entry path contains an unsafe path segment', {
      details: { path: rawPath }
    });
  }

  for (const segment of segments) {
    if (segment.includes(':')) {
      throw new WxapkgPathError('wxapkg entry path contains a Windows alternate data stream', {
        details: { path: rawPath, segment }
      });
    }

    if (WINDOWS_RESERVED_DEVICE_NAME.test(segment)) {
      throw new WxapkgPathError('wxapkg entry path contains a Windows reserved device name', {
        details: { path: rawPath, segment }
      });
    }

    if (/[ .]$/.test(segment)) {
      throw new WxapkgPathError('wxapkg entry path contains a segment ending in a space or dot', {
        details: { path: rawPath, segment }
      });
    }
  }

  return segments.join('/');
}

function parseHeader(buffer) {
  if (buffer.length < WXAPKG_HEADER_SIZE) {
    throw new WxapkgFormatError('wxapkg header is truncated', {
      code: 'ERR_WXAPKG_TRUNCATED_HEADER',
      details: { actual: buffer.length, minimum: WXAPKG_HEADER_SIZE }
    });
  }

  const firstMark = buffer.readUInt8(0);
  const unknown = buffer.readUInt32BE(1);
  const indexLength = buffer.readUInt32BE(5);
  const bodyLength = buffer.readUInt32BE(9);
  const lastMark = buffer.readUInt8(13);

  if (firstMark !== WXAPKG_FIRST_MARK || lastMark !== WXAPKG_LAST_MARK) {
    throw new WxapkgFormatError('wxapkg magic bytes are invalid', {
      code: 'ERR_WXAPKG_MAGIC',
      details: { firstMark, lastMark }
    });
  }

  if (indexLength < WXAPKG_INDEX_COUNT_SIZE) {
    throw new WxapkgFormatError('wxapkg index is too short to contain a file count', {
      code: 'ERR_WXAPKG_INDEX_LENGTH',
      details: { indexLength }
    });
  }

  const indexOffset = WXAPKG_HEADER_SIZE;
  const indexEnd = indexOffset + indexLength;
  if (indexEnd > buffer.length) {
    throw new WxapkgFormatError('wxapkg index is truncated', {
      code: 'ERR_WXAPKG_TRUNCATED_INDEX',
      details: { indexEnd, actual: buffer.length }
    });
  }

  const bodyOffset = indexEnd;
  const bodyEnd = bodyOffset + bodyLength;
  if (bodyEnd > buffer.length) {
    throw new WxapkgFormatError('wxapkg body is truncated', {
      code: 'ERR_WXAPKG_TRUNCATED_BODY',
      details: { bodyEnd, actual: buffer.length }
    });
  }

  if (bodyEnd !== buffer.length) {
    throw new WxapkgFormatError('wxapkg contains data beyond its declared body', {
      code: 'ERR_WXAPKG_TRAILING_DATA',
      details: { bodyEnd, actual: buffer.length }
    });
  }

  return {
    firstMark,
    unknown,
    indexLength,
    bodyLength,
    lastMark,
    indexOffset,
    indexEnd,
    bodyOffset,
    bodyEnd
  };
}

function parseIndex(buffer, header) {
  const fileCount = buffer.readUInt32BE(header.indexOffset);
  const maximumCount = Math.floor((header.indexLength - WXAPKG_INDEX_COUNT_SIZE) / MIN_ENTRY_SIZE);

  if (fileCount === 0) {
    throw new WxapkgFormatError('wxapkg index does not contain any files', {
      code: 'ERR_WXAPKG_EMPTY'
    });
  }

  if (fileCount > maximumCount) {
    throw new WxapkgFormatError('wxapkg file count exceeds the declared index size', {
      code: 'ERR_WXAPKG_FILE_COUNT',
      details: { fileCount, maximumCount }
    });
  }

  const files = [];
  const seenPaths = new Set();
  let cursor = header.indexOffset + WXAPKG_INDEX_COUNT_SIZE;

  for (let index = 0; index < fileCount; index += 1) {
    ensureIndexBytes(header, cursor, 4, 'file name length');
    const nameLength = buffer.readUInt32BE(cursor);
    cursor += 4;

    if (nameLength === 0) {
      throw new WxapkgFormatError('wxapkg entry path must not be empty', {
        code: 'ERR_WXAPKG_ENTRY_NAME',
        details: { index }
      });
    }

    ensureIndexBytes(header, cursor, nameLength + 8, 'file entry');
    const nameBytes = buffer.subarray(cursor, cursor + nameLength);
    let rawPath;
    try {
      rawPath = utf8Decoder.decode(nameBytes);
    } catch (cause) {
      throw new WxapkgFormatError('wxapkg entry path is not valid UTF-8', {
        code: 'ERR_WXAPKG_ENTRY_UTF8',
        cause,
        details: { index }
      });
    }
    cursor += nameLength;

    const offset = buffer.readUInt32BE(cursor);
    cursor += 4;
    const size = buffer.readUInt32BE(cursor);
    cursor += 4;

    const normalizedPath = normalizeWxapkgPath(rawPath);
    const outputPathKey = normalizedPath.toLowerCase();
    if (seenPaths.has(outputPathKey)) {
      throw new WxapkgFormatError('wxapkg contains duplicate output paths', {
        code: 'ERR_WXAPKG_DUPLICATE_PATH',
        details: { path: normalizedPath }
      });
    }
    seenPaths.add(outputPathKey);

    files.push({
      index,
      name: normalizedPath,
      path: normalizedPath,
      rawPath,
      off: offset,
      offset,
      size
    });
  }

  if (cursor !== header.indexEnd) {
    throw new WxapkgFormatError('wxapkg index length does not match its file entries', {
      code: 'ERR_WXAPKG_INDEX_LENGTH',
      details: { parsedEnd: cursor, declaredEnd: header.indexEnd }
    });
  }

  header.fileCount = fileCount;
  return files;
}

function validateFileRanges(files, header) {
  for (const file of files) {
    if (file.offset < header.bodyOffset || file.offset > header.bodyEnd) {
      throw new WxapkgFormatError('wxapkg entry offset is outside the declared body', {
        code: 'ERR_WXAPKG_ENTRY_OFFSET',
        details: { path: file.path, offset: file.offset, bodyOffset: header.bodyOffset, bodyEnd: header.bodyEnd }
      });
    }

    if (file.size > header.bodyEnd - file.offset) {
      throw new WxapkgFormatError('wxapkg entry size exceeds the declared body', {
        code: 'ERR_WXAPKG_ENTRY_SIZE',
        details: { path: file.path, offset: file.offset, size: file.size, bodyEnd: header.bodyEnd }
      });
    }
  }

  const populatedRanges = files
    .filter((file) => file.size > 0)
    .sort((left, right) => left.offset - right.offset || left.size - right.size);

  for (let index = 1; index < populatedRanges.length; index += 1) {
    const previous = populatedRanges[index - 1];
    const current = populatedRanges[index];
    if (current.offset < previous.offset + previous.size) {
      throw new WxapkgFormatError('wxapkg file data ranges overlap', {
        code: 'ERR_WXAPKG_OVERLAPPING_ENTRIES',
        details: { previous: previous.path, current: current.path }
      });
    }
  }
}

function ensureIndexBytes(header, offset, length, label) {
  if (length > header.indexEnd - offset) {
    throw new WxapkgFormatError(`wxapkg index is truncated while reading ${label}`, {
      code: 'ERR_WXAPKG_TRUNCATED_INDEX',
      details: { offset, length, indexEnd: header.indexEnd }
    });
  }
}

function toBuffer(input) {
  if (Buffer.isBuffer(input)) {
    return input;
  }

  if (input instanceof Uint8Array) {
    return Buffer.from(input.buffer, input.byteOffset, input.byteLength);
  }

  throw new TypeError('wxapkg data must be a Buffer or Uint8Array');
}
