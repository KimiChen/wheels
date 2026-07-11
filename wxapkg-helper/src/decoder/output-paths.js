// SPDX-License-Identifier: GPL-3.0-or-later

export const PLUGIN_SOURCE_DIRECTORY = '__plugin__';
export const PLUGIN_OUTPUT_DIRECTORY = 'plugin_';

export function toDecompilerOutputPath(filePath) {
  if (!filePath) {
    return filePath;
  }

  return String(filePath)
    .split('/')
    .map((segment) => segment === PLUGIN_SOURCE_DIRECTORY ? PLUGIN_OUTPUT_DIRECTORY : segment)
    .join('/');
}
