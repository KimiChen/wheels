/*
 * SPDX-License-Identifier: GPL-3.0-or-later
 * Adapted from wedecode v0.10.6 (commit 9bf626224511a42f77e4b682756472e06953854e).
 */

import colors from "picocolors";
import { saveLocalFile } from "../utils/fs-process.js";
import { createVM, runVmCode } from "../utils/create-vm.js";
import { printLog, sleep } from "../utils/common.js";
import { BaseDecompilation } from "./base-decompilation.js";
import { getGamePackCodeInfo } from "../utils/get-pack-code-info.js";
import { GameJsonExcludeKeys } from "../constant/index.js";
/**
 * 反编译工具类入口
 * */
export class GameDecompilation extends BaseDecompilation {
    codeInfo;
    wxsList;
    allRefComponentList = [];
    allSubPackagePages = [];
    /**
     * 初始化小游戏所需环境和变量
     * */
    async initGame() {
        this.codeInfo = getGamePackCodeInfo(this.pathInfo);
        const loadInfo = {};
        for (const name in this.codeInfo) {
            loadInfo[name] = this.codeInfo[name].length;
        }
        console.log(loadInfo);
    }
    /**
     * 反编译 game.json 文件， 只有主包需要处理
     * */
    async decompileGameJSON() {
        if (this.packType !== 'main')
            return;
        await sleep(200);
        const gameConfigString = this.codeInfo.appConfigJson;
        const gameConfig = JSON.parse(gameConfigString);
        Object.assign(gameConfig, gameConfig.global);
        GameJsonExcludeKeys.forEach(key => delete gameConfig[key]);
        const outputFileName = 'game.json';
        const gameConfigSaveString = JSON.stringify(gameConfig, null, 2);
        saveLocalFile(this.pathInfo.outputResolve(outputFileName), gameConfigSaveString, { force: true });
        printLog(" Completed " + ` (${gameConfigSaveString.length}) \t` + colors.bold(colors.gray(this.pathInfo.outputResolve(outputFileName))));
        printLog(` \u25B6 反编译 ${outputFileName} 文件成功. \n`, { isStart: true });
    }
    /**
     * 反编译小游戏的js文件
     * */
    async decompileGameJS() {
        const _this = this;
        let cont = 0;
        const evalCodeList = [
            this.codeInfo.subContextJs,
            this.codeInfo.gameJs
        ].filter(Boolean);
        const allJsList = [];
        const sandbox = {
            define(name, func) {
                allJsList.push(name);
                _this._parseJsDefine(name, func);
                cont++;
            },
            require() {
            },
        };
        evalCodeList.forEach(code => {
            const vm = createVM({ sandbox });
            if (!code.includes('define(') || !code.includes('function(require, module, exports)'))
                return;
            runVmCode(vm, code);
        });
        // console.log(allJsList)
        if (cont) {
            printLog(` \u25B6 反编译所有 js 文件成功. \n`);
        }
    }
    async decompileAll() {
        super.decompileAll();
        /* 开始编译 */
        await this.initGame();
        await this.decompileGameJSON();
        await this.decompileGameJS();
        await this.decompileAppWorkers();
    }
}
