/*
 * SPDX-License-Identifier: GPL-3.0-or-later
 * Adapted from wedecode v0.10.6 (commit 9bf626224511a42f77e4b682756472e06953854e).
 */

import * as cheerio from "cheerio";
import { readLocalFile } from "./fs-process.js";
/**
 * 获取 APP 包中主要的一些代码文件
 * @param pathInfo
 * @param opt
 * @param opt.adaptLen 小于该长度的内容认为空
 * */
export function getAppPackCodeInfo(pathInfo, opt = {}) {
    const { adaptLen = 100 } = opt || {};
    function __readFile(path) {
        if (!path)
            return '';
        const content = readLocalFile(path);
        return content.length > adaptLen ? content : '';
    }
    let pageFrameHtmlCode = __readFile(pathInfo.pageFrameHtmlPath);
    if (pageFrameHtmlCode) {
        const $ = cheerio.load(pageFrameHtmlCode);
        pageFrameHtmlCode = $('script').text();
    }
    const appServiceCode = __readFile(pathInfo.appServicePath);
    const appServiceAppCode = __readFile(pathInfo.appServiceAppPath);
    return {
        appConfigJson: __readFile(pathInfo.appConfigJsonPath),
        appWxss: __readFile(pathInfo.appWxssPath),
        appService: appServiceCode,
        appServiceApp: appServiceAppCode,
        pageFrame: __readFile(pathInfo.pageFramePath),
        workers: __readFile(pathInfo.workersPath),
        pageFrameHtml: pageFrameHtmlCode,
    };
}
/**
 * 获取 GAME 包中主要的一些代码文件
 * */
export function getGamePackCodeInfo(pathInfo, opt = {}) {
    const { adaptLen = 100 } = opt || {};
    function __readFile(path) {
        if (!path)
            return '';
        const content = readLocalFile(path);
        return content.length > adaptLen ? content : '';
    }
    return {
        workers: __readFile(pathInfo.workersPath),
        gameJs: __readFile(pathInfo.gameJsPath),
        appConfigJson: __readFile(pathInfo.appConfigJsonPath),
        subContextJs: __readFile(pathInfo.subContextJsPath),
    };
}
