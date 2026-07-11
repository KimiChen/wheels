/*
 * SPDX-License-Identifier: GPL-3.0-or-later
 * Adapted from wedecode v0.10.6 (commit 9bf626224511a42f77e4b682756472e06953854e).
 */

import path from 'node:path';
import { deepmerge } from '@biggerstar/deepmerge';
import { readLocalFile, saveLocalFile } from './fs-process.js';
import { isDev } from '../environment.js';
import { printLog } from './common.js';
/**
 * 项目配置生成工具类
 * 负责生成小程序项目的配置文件
 */
export class ProjectConfigUtils {
    /**
     * 生成小程序的项目配置文件
     * @param outputPath 输出路径
     */
    static async generateProjectConfigFiles(outputPath) {
        const projectPrivateConfigJsonPath = path.join(outputPath, 'project.private.config.json');
        const DEV_defaultConfigData = {
            "setting": {
                "ignoreDevUnusedFiles": false,
                "ignoreUploadUnusedFiles": false,
            }
        };
        const defaultConfigData = {
            "setting": {
                "es6": false,
                "urlCheck": false,
            }
        };
        if (isDev) {
            Object.assign(defaultConfigData.setting, DEV_defaultConfigData.setting);
        }
        let finallyConfig = {};
        const projectPrivateConfigString = readLocalFile(projectPrivateConfigJsonPath);
        if (projectPrivateConfigString) {
            const projectPrivateConfigData = JSON.parse(projectPrivateConfigString);
            deepmerge(projectPrivateConfigData, defaultConfigData);
            finallyConfig = projectPrivateConfigData;
        }
        else {
            finallyConfig = defaultConfigData;
        }
        saveLocalFile(projectPrivateConfigJsonPath, JSON.stringify(finallyConfig, null, 2), { force: true });
        printLog(` ▶ 生成项目配置文件成功. \n`, { isStart: true });
    }
}
