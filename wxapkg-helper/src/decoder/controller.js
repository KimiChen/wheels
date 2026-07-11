// SPDX-License-Identifier: GPL-3.0-or-later

import path from 'node:path';
import { fileURLToPath } from 'node:url';
import {
  AppDecompilation,
  DefaultFilesGeneratorUtils,
  FileCleanerUtils,
  GameDecompilation,
  ProjectConfigUtils,
  configureFileAccess,
  getFileManifest,
  getPathResolveInfo,
  printLog
} from './core/index.js';

const PACK_TYPE_NAMES = {
  main: '主包',
  sub: '分包',
  independent: '独立分包'
};
const CORE_DIR = fileURLToPath(new URL('./core/', import.meta.url));

export class DecompilationController {
  constructor(options) {
    if (!Array.isArray(options?.packageInfos) || options.packageInfos.length === 0) {
      throw new Error('至少需要一个已解包的 wxapkg 文件。');
    }

    if (!options.outputPath) {
      throw new Error('outputPath 是必需的。');
    }

    this.packageInfos = options.packageInfos.map((packageInfo) => ({ ...packageInfo }));
    this.packages = this.packageInfos.map((packageInfo) => packageInfo.inputPath);
    this.customPolyfillRoots = new Map((options.customPolyfillRoots || []).map((item) => (
      [item.inputPath, item.root]
    )));
    this.applicationType = options.applicationType || this.packageInfos[0].appType || 'app';
    this.outputPath = options.outputPath;
    this.usePx = Boolean(options.usePx);

    configureFileAccess({
      readRoots: [
        this.outputPath,
        CORE_DIR,
        ...this.packages.map((packagePath) => path.dirname(packagePath)),
        ...[...this.customPolyfillRoots.values()].filter(Boolean)
      ],
      writeRoot: this.outputPath,
      captureWrites: true
    });
  }

  async run() {
    for (const packageInfo of this.packageInfos) {
      const packagePath = packageInfo.inputPath;
      const packInfo = createPackInfo(packageInfo, packagePath, this.outputPath, {
        appType: this.applicationType,
        customPolyfillRoot: this.customPolyfillRoots.get(packagePath) || null
      });

      if (packInfo.appType === 'game') {
        const decompiler = new GameDecompilation(packInfo);
        await decompiler.decompileAll();
      } else {
        const decompiler = new AppDecompilation(packInfo);
        decompiler.convertPlugin = true;
        await decompiler.decompileAll({ usePx: this.usePx });
      }

      printLog(`\n完成：${PACK_TYPE_NAMES[packInfo.packType] || packInfo.packType}反编译。`);
    }

    await DefaultFilesGeneratorUtils.generateDefaultAppFiles(this.outputPath);
    await ProjectConfigUtils.generateProjectConfigFiles(this.outputPath);
    await FileCleanerUtils.removeCache(this.outputPath);
    printLog('\n反编译流程结束。');

    return { manifest: getFileManifest() };
  }
}

function createPackInfo(unpacked, inputPath, outputPath, overrides = {}) {
  const pathInfo = getPathResolveInfo(outputPath);

  if (unpacked.subPackRootPath) {
    pathInfo.setPackRootPath(unpacked.subPackRootPath);
  }

  return {
    appType: overrides.appType || unpacked.appType || 'app',
    packType: unpacked.packType || 'sub',
    subPackRootPath: unpacked.subPackRootPath || '',
    customPolyfillRoot: overrides.customPolyfillRoot || null,
    pathInfo,
    inputPath,
    outputPath
  };
}
