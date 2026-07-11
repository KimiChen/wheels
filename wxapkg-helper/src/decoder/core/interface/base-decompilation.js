/*
 * SPDX-License-Identifier: GPL-3.0-or-later
 * Adapted from wedecode v0.10.6 (commit 9bf626224511a42f77e4b682756472e06953854e).
 */

import colors from "picocolors";
import path from "node:path";
import { PolyfillCover } from "./polyfill-cover.js";
import { createVM, runVmCode } from "../utils/create-vm.js";
import { localFileExists, readLocalFile, saveLocalFile } from "../utils/fs-process.js";
import { AppTypeMapping, PackTypeMapping } from "../typings/index.js";
import { commonDir, jsBeautify, printLog, removeVM2ExceptionLine, sleep } from "../utils/common.js";
export class BaseDecompilation {
    pathInfo;
    packPath;
    packType;
    appType;
    polyfillCover;
    constructor(packInfo) {
        this.pathInfo = packInfo.pathInfo;
        this.packPath = packInfo.inputPath;
        this.packType = packInfo.packType;
        this.appType = packInfo.appType;
        this.polyfillCover = new PolyfillCover(this.packPath, packInfo.customPolyfillRoot);
    }
    async decompileAppWorker() {
        await sleep(200);
        if (!localFileExists(this.pathInfo.workersPath)) {
            return;
        }
        const appConfigString = readLocalFile(this.pathInfo.appJsonPath);
        if (!appConfigString)
            return;
        const appConfig = JSON.parse(appConfigString);
        let code = readLocalFile(this.pathInfo.workersPath);
        let commPath = '';
        let vm = createVM({
            sandbox: {
                define(name) {
                    name = path.dirname(name) + '/';
                    if (!commPath)
                        commPath = name;
                    commPath = commonDir(commPath, name);
                }
            }
        });
        runVmCode(vm, code.slice(code.indexOf("define(")));
        if (commPath.length > 0)
            commPath = commPath.slice(0, -1);
        printLog(`Worker path:  ${commPath}`);
        appConfig.workers = commPath;
        saveLocalFile(this.pathInfo.appJsonPath, JSON.stringify(appConfig, null, 2));
        printLog(` \u25B6 反编译 Worker 文件成功. \n`, { isStart: true });
    }
    /**
     * 反编译 Worker 文件
     * */
    async decompileAppWorkers() {
        await sleep(200);
        if (!localFileExists(this.pathInfo.workersPath)) {
            return;
        }
        const _this = this;
        let commPath = '';
        let code = readLocalFile(this.pathInfo.workersPath);
        let vm = createVM({
            sandbox: {
                define(name, func) {
                    _this._parseJsDefine(name, func);
                    const workerPath = path.dirname(name) + '/';
                    if (!commPath)
                        commPath = workerPath;
                    commPath = commonDir(commPath, workerPath);
                }
            }
        });
        runVmCode(vm, code);
        printLog(`Worker path:  ${commPath}`);
        if (commPath) {
            const configFileName = this.appType === 'game' ? this.pathInfo.gameJsonPath : this.pathInfo.appJsonPath;
            const appConfig = JSON.parse(readLocalFile(configFileName));
            appConfig.workers = commPath;
            saveLocalFile(configFileName, JSON.stringify(appConfig, null, 2), { force: true });
        }
        printLog(` \u25B6 反编译 Worker 文件成功. \n`, { isStart: true });
    }
    decompileAll() {
        printLog(` \u25B6 当前反编译目标[ ${AppTypeMapping[this.appType]} ] (${colors.yellow(PackTypeMapping[this.packType])}) : ` + colors.blue(this.packPath));
        printLog(` \u25B6 当前输出目录:  ${colors.blue(this.pathInfo.outputPath)}\n`, {
            isEnd: true,
        });
    }
    _parseJsDefine(name, func) {
        if (path.extname(name) !== '.js')
            return;
        // console.log(name, func);
        /* 看看是否有 polyfill,  有的话直接使用注入 polyfill */
        const foundPolyfill = this.polyfillCover.findPolyfill(name);
        let resultCode = '';
        if (foundPolyfill) {
            resultCode = readLocalFile(foundPolyfill.fullPath);
        }
        else {
            let code = func.toString();
            code = code.slice(code.indexOf("{") + 1, code.lastIndexOf("}") - 1).trim();
            if (code.startsWith('"use strict";')) {
                code = code.replaceAll('"use strict";', '');
            }
            else if (code.startsWith("'use strict';")) {
                code = code.replaceAll(`'use strict';`, '');
            }
            else if ((code.startsWith('(function(){"use strict";') || code.startsWith("(function(){'use strict';")) && code.endsWith("})();")) {
                code = code.slice(25, -5);
            }
            code = code.replaceAll('require("@babel', 'require("./@babel');
            resultCode = jsBeautify(code.trim());
        }
        if (!resultCode.trim()) {
            return;
        }
        saveLocalFile(this.pathInfo.outputResolve(name), removeVM2ExceptionLine(resultCode.trim()), { force: true });
        printLog(" Completed " + ` (${resultCode.length}) \t` + colors.bold(colors.gray(name)));
    }
}
