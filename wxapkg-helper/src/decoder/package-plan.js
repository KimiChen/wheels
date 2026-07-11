// SPDX-License-Identifier: GPL-3.0-or-later

import { inspectWxapkg, unpackWxapkg } from './wxapkg/unpack.js';
import { toDecompilerOutputPath } from './output-paths.js';

export async function inspectPackagePlan(packagePaths, options = {}) {
  const inspected = [];

  for (let index = 0; index < packagePaths.length; index += 1) {
    const packageInfo = await inspectWxapkg(packagePaths[index], options);
    inspected.push({
      inputPath: packageInfo.inputPath,
      appType: packageInfo.appType,
      packType: packageInfo.packType,
      subPackRootPath: packageInfo.subPackRootPath,
      fileCount: packageInfo.files.length,
      contentSha256: packageInfo.contentSha256,
      originalIndex: index
    });
  }

  inspected.sort((left, right) => {
    const score = packageTypeScore(left.packType) - packageTypeScore(right.packType);
    return score || left.originalIndex - right.originalIndex;
  });

  const packages = inspected.map(({ originalIndex: _originalIndex, ...packageInfo }) => packageInfo);
  return {
    applicationType: inferApplicationType(packages),
    packages
  };
}

export async function unpackPackagePlan(packagePlan, outputPath, options = {}) {
  const unpackedPackages = [];
  const useDecompilerPaths = Boolean(options.decompilerPaths);

  for (const expected of packagePlan.packages) {
    const unpacked = await unpackWxapkg(expected.inputPath, outputPath, {
      ...options,
      expectedContentSha256: expected.contentSha256,
      mapEntryPath: useDecompilerPaths ? toDecompilerOutputPath : undefined
    });
    assertPackageIdentity(expected, unpacked);
    unpackedPackages.push({
      inputPath: unpacked.inputPath,
      appType: unpacked.appType,
      packType: unpacked.packType,
      subPackRootPath: useDecompilerPaths
        ? toDecompilerOutputPath(unpacked.subPackRootPath)
        : unpacked.subPackRootPath,
      fileCount: unpacked.files.length
    });
  }

  return {
    applicationType: packagePlan.applicationType,
    packages: unpackedPackages
  };
}

export function createProcessedPackageList(packagePlan) {
  return packagePlan.packages.map((packageInfo) => ({
    path: packageInfo.inputPath,
    appType: packagePlan.applicationType,
    packType: packageInfo.packType,
    fileCount: packageInfo.fileCount
  }));
}

function inferApplicationType(packages) {
  return packages.find((item) => item.packType === 'main')?.appType
    || packages.find((item) => item.appType === 'game')?.appType
    || packages[0]?.appType
    || 'app';
}

function packageTypeScore(packType) {
  if (packType === 'main') {
    return 0;
  }
  if (packType === 'independent') {
    return 1;
  }
  return 2;
}

function assertPackageIdentity(expected, actual) {
  if (
    expected.appType !== actual.appType
    || expected.packType !== actual.packType
    || expected.subPackRootPath !== actual.subPackRootPath
    || expected.fileCount !== actual.files.length
    || expected.contentSha256 !== actual.contentSha256
  ) {
    const error = new Error(`输入包在校验后发生变化：${expected.inputPath}`);
    error.code = 'WXAPKG_INPUT_CHANGED';
    throw error;
  }
}
