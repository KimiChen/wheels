/*
 * SPDX-License-Identifier: GPL-3.0-or-later
 * Adapted from wedecode v0.10.6 (commit 9bf626224511a42f77e4b682756472e06953854e).
 */

export { AppDecompilation } from './interface/app-decompilation.js';
export { BaseDecompilation } from './interface/base-decompilation.js';
export { GameDecompilation } from './interface/game-decompilation.js';
export { DefaultFilesGeneratorUtils } from './utils/default-files-generator.js';
export { FileCleanerUtils } from './utils/file-cleaner.js';
export { ProjectConfigUtils } from './utils/project-config.js';
export { findCommonRoot, getPathResolveInfo, isWxAppid, printLog } from './utils/common.js';
export {
    configureFileAccess,
    getFileManifest,
    listLocalFiles,
    localFileExists,
    readLocalFile,
    readLocalJsonFile,
    saveLocalFile
} from './utils/fs-process.js';
