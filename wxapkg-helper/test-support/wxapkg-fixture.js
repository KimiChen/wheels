import assert from 'node:assert/strict';
import crypto from 'node:crypto';

export function buildWxapkg(entries) {
  const normalizedEntries = entries.map(([name, data]) => ({
    name: Buffer.from(name, 'utf8'),
    data: Buffer.from(data)
  }));
  const indexLength = 4 + normalizedEntries.reduce((total, entry) => total + 12 + entry.name.length, 0);
  const bodyLength = normalizedEntries.reduce((total, entry) => total + entry.data.length, 0);
  const bodyOffset = 14 + indexLength;
  const header = Buffer.alloc(14);
  header[0] = 0xbe;
  header.writeUInt32BE(0, 1);
  header.writeUInt32BE(indexLength, 5);
  header.writeUInt32BE(bodyLength, 9);
  header[13] = 0xed;

  const index = Buffer.alloc(indexLength);
  index.writeUInt32BE(normalizedEntries.length, 0);
  let indexCursor = 4;
  let bodyCursor = bodyOffset;
  for (const entry of normalizedEntries) {
    index.writeUInt32BE(entry.name.length, indexCursor);
    indexCursor += 4;
    entry.name.copy(index, indexCursor);
    indexCursor += entry.name.length;
    index.writeUInt32BE(bodyCursor, indexCursor);
    indexCursor += 4;
    index.writeUInt32BE(entry.data.length, indexCursor);
    indexCursor += 4;
    bodyCursor += entry.data.length;
  }

  return Buffer.concat([header, index, ...normalizedEntries.map((entry) => entry.data)]);
}

export function encryptWxapkg(plain, appid) {
  assert.ok(plain.length >= 1023);
  const key = crypto.pbkdf2Sync(appid, 'saltiest', 1000, 32, 'sha1');
  const prefix = Buffer.alloc(1024);
  plain.copy(prefix, 0, 0, 1023);
  const cipher = crypto.createCipheriv('aes-256-cbc', key, Buffer.from('the iv: 16 bytes'));
  cipher.setAutoPadding(false);
  const encryptedPrefix = Buffer.concat([cipher.update(prefix), cipher.final()]);
  const xorKey = appid.charCodeAt(appid.length - 2);
  const plainRemainder = plain.subarray(1023);
  const remainder = Buffer.allocUnsafe(plainRemainder.length);
  for (let index = 0; index < plainRemainder.length; index += 1) {
    remainder[index] = plainRemainder[index] ^ xorKey;
  }
  return Buffer.concat([Buffer.from('V1MMWX'), encryptedPrefix, remainder]);
}
