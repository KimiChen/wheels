// SPDX-License-Identifier: GPL-3.0-or-later

export class WxapkgError extends Error {
  constructor(message, options = {}) {
    super(message, options.cause === undefined ? undefined : { cause: options.cause });
    this.name = new.target.name;
    this.code = options.code || 'ERR_WXAPKG';

    if (options.details !== undefined) {
      this.details = options.details;
    }
  }
}

export class WxapkgFormatError extends WxapkgError {
  constructor(message, options = {}) {
    super(message, { ...options, code: options.code || 'ERR_WXAPKG_FORMAT' });
  }
}

export class WxapkgDecryptionError extends WxapkgError {
  constructor(message, options = {}) {
    super(message, { ...options, code: options.code || 'ERR_WXAPKG_DECRYPTION' });
  }
}

export class WxapkgPathError extends WxapkgError {
  constructor(message, options = {}) {
    super(message, { ...options, code: options.code || 'ERR_WXAPKG_PATH' });
  }
}

export class WxapkgIoError extends WxapkgError {
  constructor(message, options = {}) {
    super(message, { ...options, code: options.code || 'ERR_WXAPKG_IO' });
  }
}
