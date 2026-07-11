/*
 * SPDX-License-Identifier: GPL-3.0-or-later
 * Adapted from wedecode v0.10.6 (commit 9bf626224511a42f77e4b682756472e06953854e).
 */

import path from "node:path";
import { fileURLToPath } from "node:url";
import { globSync } from "glob";

export class PolyfillCover {
    allPolyfills = [];

    constructor(_packPath, customRoot = null) {
        const customPolyfills = customRoot
            ? globSync(path.resolve(customRoot, './**/*.js')).map(filePath => ({
                fullPath: filePath,
                polyfillPath: toPackagePath(path.relative(customRoot, filePath))
            }))
            : [];

        const builtinRoot = fileURLToPath(new URL('../polyfill/', import.meta.url));
        const builtinGlob = path.resolve(builtinRoot, './**/*.js');
        const builtinPolyfills = globSync(builtinGlob).map(filePath => ({
            fullPath: filePath,
            polyfillPath: toPackagePath(path.relative(builtinRoot, filePath))
        }));

        this.allPolyfills = [...customPolyfills, ...builtinPolyfills];
    }

    findPolyfill(targetPath) {
        return this.allPolyfills.find(item => targetPath.endsWith(item.polyfillPath));
    }
}

function toPackagePath(filePath) {
    return filePath.split(path.sep).join('/');
}
