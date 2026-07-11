/*
 * SPDX-License-Identifier: GPL-3.0-or-later
 * Adapted from wedecode v0.10.6 (commit 9bf626224511a42f77e4b682756472e06953854e).
 */

import path from 'node:path';
import colors from 'picocolors';
import { globSync } from 'glob';
import { deleteLocalFile, listLocalFiles, localFileExists, readLocalFile } from './fs-process.js';
import { printLog, sleep } from './common.js';
import { removeAppFileList, removeGameFileList } from '../constant/index.js';
/**
 * 文件清理工具类
 * 负责清理反编译过程中产生的中间文件和缓存
 */
export class FileCleanerUtils {
    /**
     * 移除反编译过程中产生的缓存文件
     * @param outputPath 输出路径
     */
    static async removeCache(outputPath) {
        await sleep(500);
        let cont = 0;
        const removeFileList = removeGameFileList.concat(removeAppFileList);
        const allFile = listLocalFiles(outputPath)
            .filter(filepath => ['.js', '.html', '.json'].includes(path.extname(filepath)));
        allFile.forEach(filepath => {
            const fileName = path.basename(filepath).trim();
            const extname = path.extname(filepath);
            if (!localFileExists(filepath))
                return;
            let _deleteLocalFile = () => {
                cont++;
                deleteLocalFile(filepath, { catch: true, force: true });
            };
            if (removeFileList.includes(fileName)) {
                _deleteLocalFile();
            }
            else if (extname === '.html') {
                const feature = 'var __setCssStartTime__ = Date.now()';
                const data = readLocalFile(filepath);
                if (data.includes(feature))
                    _deleteLocalFile();
            }
            else if (filepath.endsWith('.appservice.js')) {
                _deleteLocalFile();
            }
            else if (filepath.endsWith('.webview.js')) {
                _deleteLocalFile();
            }
        });
        if (cont) {
            printLog(`\n ▶ 移除中间缓存产物成功, 总计 ${colors.yellow(cont)} 个`, { isStart: true });
        }
    }
    /**
     * 清理指定类型的文件
     * @param outputPath 输出路径
     * @param filePatterns 文件匹配模式数组
     * @param description 清理描述
     */
    static async cleanFilesByPattern(outputPath, filePatterns, description = '文件') {
        let count = 0;
        for (const pattern of filePatterns) {
            const files = globSync(path.join(outputPath, pattern));
            files.forEach(filepath => {
                if (localFileExists(filepath)) {
                    deleteLocalFile(filepath, { catch: true, force: true });
                    count++;
                }
            });
        }
        if (count > 0) {
            printLog(`\n ▶ 清理${description}成功, 总计 ${colors.yellow(count)} 个`, { isStart: true });
        }
        return count;
    }
    /**
     * 清理空目录
     * @param outputPath 输出路径
     */
    static async cleanEmptyDirectories(outputPath) {
        // 空目录由可信父进程在应用 manifest 后统一处理；只读 worker 不直接修改目录。
        return 0;
    }
}
