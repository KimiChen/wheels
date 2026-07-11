// SPDX-License-Identifier: GPL-3.0-or-later

import crypto from 'node:crypto';
import { WxapkgDecryptionError } from './errors.js';
import { WXAPKG_FIRST_MARK, WXAPKG_LAST_MARK } from './format.js';

export const V1MMWX_MAGIC = Buffer.from('V1MMWX', 'ascii');

const AES_BLOCK_LENGTH = 1024;
const DECRYPTED_PREFIX_LENGTH = 1023;
const WXID_PATTERN = /^wx[0-9a-f]{16}$/i;

export function needsDecryption(input) {
  const buffer = toBuffer(input);
  return buffer.length >= V1MMWX_MAGIC.length
    && buffer.subarray(0, V1MMWX_MAGIC.length).equals(V1MMWX_MAGIC);
}

export function extractWxid(filePath) {
  if (typeof filePath !== 'string') {
    return null;
  }

  return filePath
    .split(/[\\/]+/)
    .map((part) => part.trim())
    .find((part) => WXID_PATTERN.test(part)) || null;
}

export function decryptWxapkg(wxid, input) {
  let resolvedWxid = wxid;
  let encryptedData = input;

  if (Buffer.isBuffer(wxid) || wxid instanceof Uint8Array) {
    encryptedData = wxid;
    resolvedWxid = input;
  }

  const buffer = toBuffer(encryptedData);
  resolvedWxid = validateWxid(resolvedWxid);

  if (!needsDecryption(buffer)) {
    throw new WxapkgDecryptionError('encrypted wxapkg does not start with the V1MMWX signature', {
      code: 'ERR_WXAPKG_ENCRYPTED_MAGIC'
    });
  }

  const encryptedPrefixEnd = V1MMWX_MAGIC.length + AES_BLOCK_LENGTH;
  if (buffer.length < encryptedPrefixEnd) {
    throw new WxapkgDecryptionError('V1MMWX encrypted wxapkg is truncated', {
      code: 'ERR_WXAPKG_ENCRYPTED_TRUNCATED',
      details: { actual: buffer.length, minimum: encryptedPrefixEnd }
    });
  }

  let decryptedPrefix;
  try {
    const key = crypto.pbkdf2Sync(resolvedWxid, 'saltiest', 1000, 32, 'sha1');
    const decipher = crypto.createDecipheriv('aes-256-cbc', key, Buffer.from('the iv: 16 bytes'));
    decipher.setAutoPadding(false);
    decryptedPrefix = Buffer.concat([
      decipher.update(buffer.subarray(V1MMWX_MAGIC.length, encryptedPrefixEnd)),
      decipher.final()
    ]);
  } catch (cause) {
    throw new WxapkgDecryptionError('failed to decrypt the V1MMWX AES prefix', {
      code: 'ERR_WXAPKG_DECRYPTION_FAILED',
      cause
    });
  }

  const xorKey = resolvedWxid.charCodeAt(resolvedWxid.length - 2);
  const encryptedRemainder = buffer.subarray(encryptedPrefixEnd);
  const decryptedRemainder = Buffer.allocUnsafe(encryptedRemainder.length);
  for (let index = 0; index < encryptedRemainder.length; index += 1) {
    decryptedRemainder[index] = encryptedRemainder[index] ^ xorKey;
  }

  const result = Buffer.concat([
    decryptedPrefix.subarray(0, DECRYPTED_PREFIX_LENGTH),
    decryptedRemainder
  ]);

  if (result.length < 14 || result[0] !== WXAPKG_FIRST_MARK || result[13] !== WXAPKG_LAST_MARK) {
    throw new WxapkgDecryptionError('decrypted data is not a valid wxapkg; the wxid may be incorrect', {
      code: 'ERR_WXAPKG_DECRYPTED_MAGIC'
    });
  }

  return result;
}

export function validateWxid(wxid) {
  if (typeof wxid !== 'string' || !WXID_PATTERN.test(wxid.trim())) {
    throw new WxapkgDecryptionError('wxid must match wx followed by 16 hexadecimal characters', {
      code: 'ERR_WXAPKG_WXID',
      details: { wxid: typeof wxid === 'string' ? wxid : null }
    });
  }

  return wxid.trim();
}

function toBuffer(input) {
  if (Buffer.isBuffer(input)) {
    return input;
  }

  if (input instanceof Uint8Array) {
    return Buffer.from(input.buffer, input.byteOffset, input.byteLength);
  }

  throw new TypeError('encrypted wxapkg data must be a Buffer or Uint8Array');
}
